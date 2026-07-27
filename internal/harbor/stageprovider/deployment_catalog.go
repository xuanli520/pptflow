package stageprovider

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"unicode"

	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	// DeploymentOperationCatalogFormat identifies the strict, source-controlled
	// production operation whitelist. It is deliberately separate from the
	// RunExecutionSpec format: a catalog says what one deployment is able to
	// run, while a spec says what one Run selected from that deployment.
	DeploymentOperationCatalogFormat = "harbor.deployment-operation-catalog.v1"
	// DeploymentOperationCatalogVersion is the schema revision for the
	// deployment catalog document. Incompatible additions require a new value
	// rather than a permissive decoder branch.
	DeploymentOperationCatalogVersion = "1"
	// DeploymentOperationCatalogReceiptFormat identifies the small immutable
	// receipt persisted with a Run. It binds the source-controlled catalog
	// identity without copying any secrets or mutable deployment paths.
	DeploymentOperationCatalogReceiptFormat = "harbor.deployment-operation-catalog-receipt.v1"
	// DeploymentOperationCatalogReceiptVersion is deliberately independent
	// from the catalog schema version.
	DeploymentOperationCatalogReceiptVersion = "1"
	// DeploymentOperationCatalogReceiptFingerprintDomain separates a complete
	// receipt identity from the catalog fingerprint it carries. A production
	// binary uses it to bind the exact receipt, rather than only its catalog
	// content hash, into its linker metadata.
	DeploymentOperationCatalogReceiptFingerprintDomain = "harbor.stageprovider.deployment-operation-catalog-receipt.v1"
)

var (
	// ErrInvalidDeploymentOperationCatalog marks an invalid static production
	// whitelist. The catalog is rejected before StartRun can create durable
	// work, so an invalid deployment cannot become an implicit allow-all.
	ErrInvalidDeploymentOperationCatalog = errors.New("harbor stage provider: invalid deployment operation catalog")
	// ErrDeploymentOperationCatalogUnavailable marks a missing immutable
	// catalog resolver. It is intentionally distinct from an empty catalog,
	// which is a valid deny-all deployment configuration.
	ErrDeploymentOperationCatalogUnavailable = errors.New("harbor stage provider: deployment operation catalog is unavailable")
	// ErrDeploymentOperationCatalogDrift marks a structurally valid frozen
	// resolution whose stage/plugin/runtime/checkout/secret contract is not the
	// exact contract installed in this deployment catalog.
	ErrDeploymentOperationCatalogDrift = errors.New("harbor stage provider: deployment operation catalog drift")
)

// DeploymentStageContract pins the Harbor workflow stage identity that one
// production operation is allowed to serve. Group, type, and plugin are all
// included so a stage-name match alone can never select a different plugin or
// semantic stage after a catalog/template change.
type DeploymentStageContract struct {
	Key    workflowkit.StageKey             `json:"key"`
	Type   workflowadapter.StageBindingType `json:"type"`
	Group  workflowadapter.StageGroup       `json:"group"`
	Plugin workflowkit.PluginBinding        `json:"plugin"`
}

// DeploymentCheckoutContract identifies the controlled logical checkout that
// an operation may use. Purpose is deployment-owned policy (for example,
// "oracle-isolated" or "evaluator-clean"); a Run freezes only the checkout
// handle and its selected revision, never a host path.
type DeploymentCheckoutContract struct {
	ID      string `json:"id"`
	Purpose string `json:"purpose"`
}

// DeploymentOperationRegistration is one exact production allow-list entry.
// It has no free-form configuration bag: every caller-visible choice comes
// from the sealed RunExecutionSpec resolution and must match this static
// registration byte-for-byte where relevant.
//
// Secrets contain references only. Secret values, paths, environment values,
// and provider defaults must never enter this catalog or a Run manifest.
type DeploymentOperationRegistration struct {
	Stage           DeploymentStageContract               `json:"stage"`
	Provider        workflowadapter.ProviderReference     `json:"provider"`
	Operation       workflowadapter.StageOperationBinding `json:"operation"`
	Runtime         workflowadapter.RuntimeReference      `json:"runtime"`
	Checkout        DeploymentCheckoutContract            `json:"checkout"`
	Secrets         []workflowadapter.SecretReference     `json:"secrets"`
	HarborEvaluator *HarborEvaluatorOperationContract     `json:"harbor_evaluator,omitempty"`
}

// Clone returns an independently owned registration. It is used both at the
// catalog boundary and when a caller inspects a resolver, preventing later
// caller mutation from changing an installed whitelist.
func (registration DeploymentOperationRegistration) Clone() DeploymentOperationRegistration {
	registration.Operation = registration.Operation.Clone()
	registration.Secrets = cloneDeploymentSecrets(registration.Secrets)
	if registration.HarborEvaluator != nil {
		contract := registration.HarborEvaluator.Clone()
		registration.HarborEvaluator = &contract
	}
	return registration
}

// DeploymentOperationCatalog is the versioned, immutable source of truth for
// production executable operations. It intentionally supports an explicit
// empty Operations array: an incompletely configured deployment is safely
// deny-all and returns provider/operation unavailable during StartRun
// preflight rather than falling back to PATH, a latest image, or a model
// default.
type DeploymentOperationCatalog struct {
	Format         string `json:"format"`
	Version        string `json:"version"`
	CatalogID      string `json:"catalog_id"`
	CatalogVersion string `json:"catalog_version"`
	// Template is the exact closed Harbor workflow template this deployment
	// catalog serves. It is required; a catalog cannot silently mean Standard
	// merely because a caller omitted a template selector.
	Template   workflowadapter.TemplateReference `json:"template"`
	Operations []DeploymentOperationRegistration `json:"operations"`
}

// Clone returns a deep copy suitable for caller inspection or canonicalizing
// without mutating the installed catalog.
func (catalog DeploymentOperationCatalog) Clone() DeploymentOperationCatalog {
	operations := catalog.Operations
	catalog.Operations = make([]DeploymentOperationRegistration, len(operations))
	for index, operation := range operations {
		catalog.Operations[index] = operation.Clone()
	}
	return catalog
}

// Validate proves that a catalog is a strict, code-version-compatible
// whitelist. It rejects unknown Harbor stages, stage/plugin/type/group drift,
// unversioned provider/runtime/operation/secret identities, duplicate
// operation coordinates, and ambiguous operation payload variants.
func (catalog DeploymentOperationCatalog) Validate() error {
	if catalog.Format != DeploymentOperationCatalogFormat {
		return fmt.Errorf("%w: unsupported format %q", ErrInvalidDeploymentOperationCatalog, catalog.Format)
	}
	if catalog.Version != DeploymentOperationCatalogVersion {
		return fmt.Errorf("%w: unsupported version %q", ErrInvalidDeploymentOperationCatalog, catalog.Version)
	}
	if err := validateDeploymentCatalogString("catalog id", catalog.CatalogID); err != nil {
		return err
	}
	if err := validateDeploymentCatalogString("catalog version", catalog.CatalogVersion); err != nil {
		return err
	}
	template, err := workflowadapter.ResolveWorkflowTemplate(catalog.Template)
	if err != nil {
		return fmt.Errorf("%w: workflow template: %v", ErrInvalidDeploymentOperationCatalog, err)
	}
	if catalog.Operations == nil {
		return fmt.Errorf("%w: operations must be an explicit array", ErrInvalidDeploymentOperationCatalog)
	}

	seen := make(map[deploymentOperationCoordinate]struct{}, len(catalog.Operations))
	for index, registration := range catalog.Operations {
		if err := validateDeploymentOperationRegistration(registration, template.Catalog); err != nil {
			return fmt.Errorf("%w: operation %d: %v", ErrInvalidDeploymentOperationCatalog, index, err)
		}
		coordinate := deploymentCoordinateForRegistration(registration)
		if _, duplicate := seen[coordinate]; duplicate {
			return fmt.Errorf("%w: duplicate operation %s", ErrInvalidDeploymentOperationCatalog, coordinate)
		}
		seen[coordinate] = struct{}{}
	}
	return nil
}

// CanonicalJSON returns a validated canonical catalog document. Semantically
// unordered registrations and secret-reference sets are sorted, while every
// identity and payload field remains fingerprint-significant.
func (catalog DeploymentOperationCatalog) CanonicalJSON() ([]byte, error) {
	if err := catalog.Validate(); err != nil {
		return nil, err
	}
	canonical := catalog.Clone()
	for index := range canonical.Operations {
		sort.Slice(canonical.Operations[index].Secrets, func(left, right int) bool {
			return deploymentSecretLess(canonical.Operations[index].Secrets[left], canonical.Operations[index].Secrets[right])
		})
		if canonical.Operations[index].HarborEvaluator != nil {
			contract := canonical.Operations[index].HarborEvaluator.canonicalized()
			canonical.Operations[index].HarborEvaluator = &contract
		}
	}
	sort.Slice(canonical.Operations, func(left, right int) bool {
		return deploymentCoordinateForRegistration(canonical.Operations[left]).less(deploymentCoordinateForRegistration(canonical.Operations[right]))
	})
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return nil, fmt.Errorf("%w: encode canonical catalog: %v", ErrInvalidDeploymentOperationCatalog, err)
	}
	return encoded, nil
}

// Fingerprint returns a domain-separated identity for the entire static
// deployment whitelist. CLI, TUI, foreground worker, and detached worker can
// persist and compare this value without accepting a mutable catalog file as
// runtime authority.
func (catalog DeploymentOperationCatalog) Fingerprint() (workflowkit.Fingerprint, error) {
	canonical, err := catalog.CanonicalJSON()
	if err != nil {
		return "", err
	}
	return workflowkit.FingerprintBytes("harbor.stageprovider.deployment-operation-catalog.v1", canonical)
}

// ParseDeploymentOperationCatalogJSON strictly decodes a catalog document.
// Unknown fields, duplicate keys at every nesting level, null/missing
// operation arrays, and trailing values are rejected before validation.
func ParseDeploymentOperationCatalogJSON(raw []byte) (DeploymentOperationCatalog, error) {
	if err := rejectDuplicateDeploymentCatalogJSONKeys(raw); err != nil {
		return DeploymentOperationCatalog{}, fmt.Errorf("%w: decode catalog: %v", ErrInvalidDeploymentOperationCatalog, err)
	}
	var document deploymentOperationCatalogDocument
	if err := decodeDeploymentCatalogJSON(raw, &document); err != nil {
		return DeploymentOperationCatalog{}, fmt.Errorf("%w: decode catalog: %v", ErrInvalidDeploymentOperationCatalog, err)
	}
	if document.Operations == nil {
		return DeploymentOperationCatalog{}, fmt.Errorf("%w: operations must be an explicit array", ErrInvalidDeploymentOperationCatalog)
	}
	catalog := DeploymentOperationCatalog{
		Format:         document.Format,
		Version:        document.Version,
		CatalogID:      document.CatalogID,
		CatalogVersion: document.CatalogVersion,
		Template:       document.Template,
		Operations:     document.Operations,
	}
	if err := catalog.Validate(); err != nil {
		return DeploymentOperationCatalog{}, err
	}
	return catalog.Clone(), nil
}

// UnmarshalJSON keeps direct encoding/json use as strict as the named parser.
// A catalog cannot accidentally become permissive merely because a caller used
// json.Unmarshal instead of ParseDeploymentOperationCatalogJSON.
func (catalog *DeploymentOperationCatalog) UnmarshalJSON(raw []byte) error {
	if catalog == nil {
		return fmt.Errorf("%w: nil catalog", ErrInvalidDeploymentOperationCatalog)
	}
	parsed, err := ParseDeploymentOperationCatalogJSON(raw)
	if err != nil {
		return err
	}
	*catalog = parsed
	return nil
}

type deploymentOperationCatalogDocument struct {
	Format         string                            `json:"format"`
	Version        string                            `json:"version"`
	CatalogID      string                            `json:"catalog_id"`
	CatalogVersion string                            `json:"catalog_version"`
	Template       workflowadapter.TemplateReference `json:"template"`
	Operations     []DeploymentOperationRegistration `json:"operations"`
}

// DeploymentOperationCatalogResolver is a read-only preflight resolver for a
// single installed catalog. It never learns new registrations from a caller's
// RunExecutionSpec: every accepted resolution was already present when the
// resolver was constructed.
type DeploymentOperationCatalogResolver struct {
	catalog     DeploymentOperationCatalog
	fingerprint workflowkit.Fingerprint
	canonical   []byte
	operations  map[deploymentOperationCoordinate]DeploymentOperationRegistration
	providers   map[string]map[string]workflowadapter.ProviderReference
}

// DeploymentOperationCatalogIdentity is the compact immutable catalog binding
// that belongs in a frozen Run manifest and provider receipt. The canonical
// catalog bytes remain addressable separately; this identity makes an
// accidental catalog/version substitution observable across CLI, TUI, and all
// worker modes.
type DeploymentOperationCatalogIdentity struct {
	CatalogID      string                            `json:"catalog_id"`
	CatalogVersion string                            `json:"catalog_version"`
	Template       workflowadapter.TemplateReference `json:"template"`
	Fingerprint    workflowkit.Fingerprint           `json:"fingerprint"`
}

// Validate proves an ID/version/fingerprint tuple can safely be compared to a
// loaded catalog. It is intentionally usable without loading the catalog so a
// Run manifest reader can reject a malformed frozen identity early.
func (identity DeploymentOperationCatalogIdentity) Validate() error {
	if err := validateDeploymentCatalogString("catalog id", identity.CatalogID); err != nil {
		return err
	}
	if err := validateDeploymentCatalogString("catalog version", identity.CatalogVersion); err != nil {
		return err
	}
	if _, err := workflowadapter.ResolveWorkflowTemplate(identity.Template); err != nil {
		return fmt.Errorf("%w: catalog template: %v", ErrInvalidDeploymentOperationCatalog, err)
	}
	if err := identity.Fingerprint.Validate(); err != nil {
		return fmt.Errorf("%w: catalog fingerprint: %v", ErrInvalidDeploymentOperationCatalog, err)
	}
	return nil
}

// DeploymentOperationCatalogReceipt is the stable, serializable catalog
// binding intended for frozen Run manifests and runtime receipts. A complete
// deployment lock may add binary/image attestations around this receipt; this
// package deliberately keeps the operation catalog model independent from a
// host-specific lock implementation.
type DeploymentOperationCatalogReceipt struct {
	Format               string                            `json:"format"`
	Version              string                            `json:"version"`
	CatalogFormat        string                            `json:"catalog_format"`
	CatalogSchemaVersion string                            `json:"catalog_schema_version"`
	CatalogID            string                            `json:"catalog_id"`
	CatalogVersion       string                            `json:"catalog_version"`
	Template             workflowadapter.TemplateReference `json:"template"`
	CatalogFingerprint   workflowkit.Fingerprint           `json:"catalog_fingerprint"`
}

// Validate proves a receipt contains one complete immutable catalog identity.
func (receipt DeploymentOperationCatalogReceipt) Validate() error {
	if receipt.Format != DeploymentOperationCatalogReceiptFormat {
		return fmt.Errorf("%w: unsupported receipt format %q", ErrInvalidDeploymentOperationCatalog, receipt.Format)
	}
	if receipt.Version != DeploymentOperationCatalogReceiptVersion {
		return fmt.Errorf("%w: unsupported receipt version %q", ErrInvalidDeploymentOperationCatalog, receipt.Version)
	}
	if receipt.CatalogFormat != DeploymentOperationCatalogFormat {
		return fmt.Errorf("%w: unsupported catalog format %q", ErrInvalidDeploymentOperationCatalog, receipt.CatalogFormat)
	}
	if receipt.CatalogSchemaVersion != DeploymentOperationCatalogVersion {
		return fmt.Errorf("%w: unsupported catalog schema version %q", ErrInvalidDeploymentOperationCatalog, receipt.CatalogSchemaVersion)
	}
	return DeploymentOperationCatalogIdentity{
		CatalogID: receipt.CatalogID, CatalogVersion: receipt.CatalogVersion, Template: receipt.Template, Fingerprint: receipt.CatalogFingerprint,
	}.Validate()
}

// CanonicalJSON returns a validated deterministic receipt document that can be
// written directly into a frozen Run input bundle.
func (receipt DeploymentOperationCatalogReceipt) CanonicalJSON() ([]byte, error) {
	if err := receipt.Validate(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return nil, fmt.Errorf("%w: encode catalog receipt: %v", ErrInvalidDeploymentOperationCatalog, err)
	}
	return encoded, nil
}

// Fingerprint returns the domain-separated identity of the complete canonical
// receipt. It is distinct from CatalogFingerprint, which identifies only the
// referenced catalog document.
func (receipt DeploymentOperationCatalogReceipt) Fingerprint() (workflowkit.Fingerprint, error) {
	canonical, err := receipt.CanonicalJSON()
	if err != nil {
		return "", err
	}
	return workflowkit.FingerprintBytes(DeploymentOperationCatalogReceiptFingerprintDomain, canonical)
}

// ParseDeploymentOperationCatalogReceiptJSON strictly decodes a frozen
// receipt. It is provided for worker startup validation and rejects the same
// duplicate/unknown/trailing JSON cases as catalog parsing.
func ParseDeploymentOperationCatalogReceiptJSON(raw []byte) (DeploymentOperationCatalogReceipt, error) {
	if err := rejectDuplicateDeploymentCatalogJSONKeys(raw); err != nil {
		return DeploymentOperationCatalogReceipt{}, fmt.Errorf("%w: decode catalog receipt: %v", ErrInvalidDeploymentOperationCatalog, err)
	}
	var document deploymentOperationCatalogReceiptDocument
	if err := decodeDeploymentCatalogJSON(raw, &document); err != nil {
		return DeploymentOperationCatalogReceipt{}, fmt.Errorf("%w: decode catalog receipt: %v", ErrInvalidDeploymentOperationCatalog, err)
	}
	receipt := DeploymentOperationCatalogReceipt{
		Format:               document.Format,
		Version:              document.Version,
		CatalogFormat:        document.CatalogFormat,
		CatalogSchemaVersion: document.CatalogSchemaVersion,
		CatalogID:            document.CatalogID,
		CatalogVersion:       document.CatalogVersion,
		Template:             document.Template,
		CatalogFingerprint:   document.CatalogFingerprint,
	}
	if err := receipt.Validate(); err != nil {
		return DeploymentOperationCatalogReceipt{}, err
	}
	return receipt, nil
}

type deploymentOperationCatalogReceiptDocument struct {
	Format               string                            `json:"format"`
	Version              string                            `json:"version"`
	CatalogFormat        string                            `json:"catalog_format"`
	CatalogSchemaVersion string                            `json:"catalog_schema_version"`
	CatalogID            string                            `json:"catalog_id"`
	CatalogVersion       string                            `json:"catalog_version"`
	Template             workflowadapter.TemplateReference `json:"template"`
	CatalogFingerprint   workflowkit.Fingerprint           `json:"catalog_fingerprint"`
}

// UnmarshalJSON keeps direct JSON decoding of a receipt strict.
func (receipt *DeploymentOperationCatalogReceipt) UnmarshalJSON(raw []byte) error {
	if receipt == nil {
		return fmt.Errorf("%w: nil catalog receipt", ErrInvalidDeploymentOperationCatalog)
	}
	parsed, err := ParseDeploymentOperationCatalogReceiptJSON(raw)
	if err != nil {
		return err
	}
	*receipt = parsed
	return nil
}

// NewDeploymentOperationCatalogResolver freezes a validated catalog in memory
// and indexes exact coordinates for side-effect-free StartRun preflight.
func NewDeploymentOperationCatalogResolver(catalog DeploymentOperationCatalog) (*DeploymentOperationCatalogResolver, error) {
	if err := catalog.Validate(); err != nil {
		return nil, err
	}
	canonical, err := catalog.CanonicalJSON()
	if err != nil {
		return nil, err
	}
	fingerprint, err := workflowkit.FingerprintBytes("harbor.stageprovider.deployment-operation-catalog.v1", canonical)
	if err != nil {
		return nil, err
	}
	installed := catalog.Clone()
	resolver := &DeploymentOperationCatalogResolver{
		catalog:     installed,
		fingerprint: fingerprint,
		canonical:   append([]byte(nil), canonical...),
		operations:  make(map[deploymentOperationCoordinate]DeploymentOperationRegistration, len(installed.Operations)),
		providers:   make(map[string]map[string]workflowadapter.ProviderReference),
	}
	for _, registration := range installed.Operations {
		coordinate := deploymentCoordinateForRegistration(registration)
		resolver.operations[coordinate] = registration.Clone()
		versions := resolver.providers[registration.Provider.ID]
		if versions == nil {
			versions = make(map[string]workflowadapter.ProviderReference)
			resolver.providers[registration.Provider.ID] = versions
		}
		versions[registration.Provider.Version] = registration.Provider
	}
	return resolver, nil
}

// LoadDeploymentOperationCatalogFile loads one read-only deployment catalog
// file, strictly parses it, and freezes its canonical bytes in a resolver. It
// does not watch the path or retain a file handle, so later file edits cannot
// mutate a running process's allow-list.
func LoadDeploymentOperationCatalogFile(path string) (*DeploymentOperationCatalogResolver, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("%w: catalog path is required", ErrInvalidDeploymentOperationCatalog)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read deployment operation catalog %q: %w", path, err)
	}
	catalog, err := ParseDeploymentOperationCatalogJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("load deployment operation catalog %q: %w", path, err)
	}
	return NewDeploymentOperationCatalogResolver(catalog)
}

// Catalog returns a defensive copy of the static catalog that was installed
// into this resolver.
func (resolver *DeploymentOperationCatalogResolver) Catalog() DeploymentOperationCatalog {
	if resolver == nil {
		return DeploymentOperationCatalog{}
	}
	return resolver.catalog.Clone()
}

// CatalogFingerprint returns the fingerprint of the catalog that was frozen
// when this resolver was created.
func (resolver *DeploymentOperationCatalogResolver) CatalogFingerprint() workflowkit.Fingerprint {
	if resolver == nil {
		return ""
	}
	return resolver.fingerprint
}

// Template returns the exact closed Harbor workflow template installed with
// this catalog. It is a value copy and never falls back to Standard.
func (resolver *DeploymentOperationCatalogResolver) Template() workflowadapter.TemplateReference {
	if resolver == nil {
		return workflowadapter.TemplateReference{}
	}
	return resolver.catalog.Template
}

// CanonicalCatalogJSON returns a defensive copy of the canonical static
// catalog bytes frozen when this resolver was constructed. Application code
// can persist these bytes before scheduling a Run without rereading a mutable
// deployment file.
func (resolver *DeploymentOperationCatalogResolver) CanonicalCatalogJSON() []byte {
	if resolver == nil {
		return nil
	}
	return append([]byte(nil), resolver.canonical...)
}

// CatalogIdentity returns the immutable catalog ID/version/fingerprint tuple
// suitable for embedding in a Run manifest before any external work starts.
func (resolver *DeploymentOperationCatalogResolver) CatalogIdentity() DeploymentOperationCatalogIdentity {
	if resolver == nil {
		return DeploymentOperationCatalogIdentity{}
	}
	return DeploymentOperationCatalogIdentity{
		CatalogID:      resolver.catalog.CatalogID,
		CatalogVersion: resolver.catalog.CatalogVersion,
		Template:       resolver.catalog.Template,
		Fingerprint:    resolver.fingerprint,
	}
}

// Receipt returns the compact immutable catalog binding for a frozen Run
// manifest. It contains only public catalog identity, never secret values or
// host-specific paths.
func (resolver *DeploymentOperationCatalogResolver) Receipt() DeploymentOperationCatalogReceipt {
	if resolver == nil {
		return DeploymentOperationCatalogReceipt{}
	}
	return DeploymentOperationCatalogReceipt{
		Format:               DeploymentOperationCatalogReceiptFormat,
		Version:              DeploymentOperationCatalogReceiptVersion,
		CatalogFormat:        DeploymentOperationCatalogFormat,
		CatalogSchemaVersion: DeploymentOperationCatalogVersion,
		CatalogID:            resolver.catalog.CatalogID,
		CatalogVersion:       resolver.catalog.CatalogVersion,
		Template:             resolver.catalog.Template,
		CatalogFingerprint:   resolver.fingerprint,
	}
}

// CanonicalReceiptJSON returns the canonical bytes of Receipt for durable Run
// manifest storage.
func (resolver *DeploymentOperationCatalogResolver) CanonicalReceiptJSON() ([]byte, error) {
	if resolver == nil {
		return nil, ErrDeploymentOperationCatalogUnavailable
	}
	return resolver.Receipt().CanonicalJSON()
}

// VerifyCatalogIdentity proves that a compact frozen manifest binding belongs
// to this loaded catalog. A mismatch is a deployment/catalog drift and must
// be rejected before any external effect starts.
func (resolver *DeploymentOperationCatalogResolver) VerifyCatalogIdentity(identity DeploymentOperationCatalogIdentity) error {
	if resolver == nil {
		return ErrDeploymentOperationCatalogUnavailable
	}
	if err := identity.Validate(); err != nil {
		return err
	}
	if identity != resolver.CatalogIdentity() {
		return fmt.Errorf("%w: frozen catalog %q@%q fingerprint %q does not match loaded catalog", ErrDeploymentOperationCatalogDrift, identity.CatalogID, identity.CatalogVersion, identity.Fingerprint)
	}
	return nil
}

// VerifyReceipt proves that a frozen catalog receipt belongs to this loaded
// catalog. It is the worker-side counterpart to freezing Receipt at StartRun.
func (resolver *DeploymentOperationCatalogResolver) VerifyReceipt(receipt DeploymentOperationCatalogReceipt) error {
	if resolver == nil {
		return ErrDeploymentOperationCatalogUnavailable
	}
	if err := receipt.Validate(); err != nil {
		return err
	}
	if receipt != resolver.Receipt() {
		return fmt.Errorf("%w: frozen catalog receipt does not match loaded catalog", ErrDeploymentOperationCatalogDrift)
	}
	return nil
}

// ValidateExecutionSpec proves that a complete frozen RunExecutionSpec is
// bound to this catalog's exact closed template before validating every stage
// operation. StageOperationResolution intentionally contains only a single
// stage's stable references, so callers admitting a full Run must use this
// method rather than infer a template from a stage key or plugin binding.
func (resolver *DeploymentOperationCatalogResolver) ValidateExecutionSpec(specification workflowadapter.RunExecutionSpec) error {
	if resolver == nil {
		return ErrDeploymentOperationCatalogUnavailable
	}
	if err := resolver.verifyExecutionSpecTemplate(specification); err != nil {
		return err
	}
	if err := specification.ValidateWithOperationResolver(resolver); err != nil {
		return fmt.Errorf("validate frozen execution specification against deployment catalog: %w", err)
	}
	return nil
}

// ResolveExecutionSpecStageOperation resolves one operation only after the
// enclosing frozen specification has been proven to use this exact catalog
// template. It is the template-safe alternative to passing a bare
// StageOperationResolution when a caller has the full Run document.
func (resolver *DeploymentOperationCatalogResolver) ResolveExecutionSpecStageOperation(specification workflowadapter.RunExecutionSpec, key workflowkit.StageKey) (DeploymentOperationRegistration, error) {
	if resolver == nil {
		return DeploymentOperationRegistration{}, ErrDeploymentOperationCatalogUnavailable
	}
	if err := resolver.verifyExecutionSpecTemplate(specification); err != nil {
		return DeploymentOperationRegistration{}, err
	}
	resolution, err := specification.ResolveStageOperation(key)
	if err != nil {
		return DeploymentOperationRegistration{}, err
	}
	return resolver.ResolveStageOperation(resolution)
}

func (resolver *DeploymentOperationCatalogResolver) verifyExecutionSpecTemplate(specification workflowadapter.RunExecutionSpec) error {
	if resolver == nil {
		return ErrDeploymentOperationCatalogUnavailable
	}
	if _, err := workflowadapter.ResolveWorkflowTemplate(specification.Template); err != nil {
		return fmt.Errorf("%w: frozen execution specification template: %v", ErrDeploymentOperationCatalogDrift, err)
	}
	if !specification.Template.Equal(resolver.catalog.Template) {
		return fmt.Errorf("%w: frozen execution specification template %s@%s does not match deployment catalog template %s@%s", ErrDeploymentOperationCatalogDrift, specification.Template.ID, specification.Template.Version, resolver.catalog.Template.ID, resolver.catalog.Template.Version)
	}
	return nil
}

// ResolveStageOperation validates and returns the exact static registration
// selected by a frozen RunExecutionSpec resolution. It does not instantiate an
// executor or perform a provider action; callers can compose it with typed
// provider handlers after this preflight succeeds.
func (resolver *DeploymentOperationCatalogResolver) ResolveStageOperation(resolution workflowadapter.StageOperationResolution) (DeploymentOperationRegistration, error) {
	if resolver == nil {
		return DeploymentOperationRegistration{}, ErrDeploymentOperationCatalogUnavailable
	}
	if err := validateDeploymentOperationResolution(resolution); err != nil {
		return DeploymentOperationRegistration{}, err
	}
	coordinate := deploymentCoordinateForResolution(resolution)
	registration, present := resolver.operations[coordinate]
	if !present {
		versions, knownProvider := resolver.providers[resolution.Provider.ID]
		if !knownProvider {
			return DeploymentOperationRegistration{}, fmt.Errorf("%w: provider %q", ErrProviderUnavailable, resolution.Provider.ID)
		}
		installed, knownVersion := versions[resolution.Provider.Version]
		if !knownVersion || installed.Kind != resolution.Provider.Kind {
			return DeploymentOperationRegistration{}, fmt.Errorf("%w: provider %q version %q", ErrProviderVersionMismatch, resolution.Provider.ID, resolution.Provider.Version)
		}
		return DeploymentOperationRegistration{}, fmt.Errorf("%w: provider %q operation %q@%q for stage %q", ErrStageOperationUnavailable, resolution.Provider.ID, resolution.Operation.OperationID, resolution.Operation.Version, resolution.StageKey)
	}
	if registration.Provider != resolution.Provider {
		return DeploymentOperationRegistration{}, fmt.Errorf("%w: provider %q version %q", ErrProviderVersionMismatch, resolution.Provider.ID, resolution.Provider.Version)
	}

	if registration.Stage.Type != resolution.StageType || registration.Stage.Plugin != resolution.Plugin {
		return DeploymentOperationRegistration{}, fmt.Errorf("%w: stage %q plugin/type differs from installed catalog", ErrDeploymentOperationCatalogDrift, resolution.StageKey)
	}
	installedPayload, err := canonicalOperationBindingPayload(registration.Operation)
	if err != nil {
		return DeploymentOperationRegistration{}, fmt.Errorf("%w: installed operation payload: %v", ErrInvalidDeploymentOperationCatalog, err)
	}
	requestedPayload, err := canonicalOperationBindingPayload(resolution.Operation)
	if err != nil {
		return DeploymentOperationRegistration{}, err
	}
	if !bytes.Equal(installedPayload, requestedPayload) {
		return DeploymentOperationRegistration{}, fmt.Errorf("%w: operation %q@%q for stage %q", ErrFrozenOperationPayloadMismatch, resolution.Operation.OperationID, resolution.Operation.Version, resolution.StageKey)
	}
	if registration.Runtime != resolution.Runtime {
		return DeploymentOperationRegistration{}, fmt.Errorf("%w: stage %q runtime %q@%q", ErrDeploymentOperationCatalogDrift, resolution.StageKey, resolution.Runtime.ID, resolution.Runtime.Version)
	}
	if registration.Checkout.ID != resolution.Checkout.ID {
		return DeploymentOperationRegistration{}, fmt.Errorf("%w: stage %q checkout %q", ErrDeploymentOperationCatalogDrift, resolution.StageKey, resolution.Checkout.ID)
	}
	if !sameDeploymentSecrets(registration.Secrets, resolution.Secrets) {
		return DeploymentOperationRegistration{}, fmt.Errorf("%w: stage %q secret references", ErrDeploymentOperationCatalogDrift, resolution.StageKey)
	}
	return registration.Clone(), nil
}

// ValidateStageOperation implements workflowadapter.StageOperationResolver.
// It only proves the frozen operation is in this immutable catalog and never
// starts a process, resolves a path, reads a secret, or contacts a provider.
func (resolver *DeploymentOperationCatalogResolver) ValidateStageOperation(resolution workflowadapter.StageOperationResolution) error {
	_, err := resolver.ResolveStageOperation(resolution)
	return err
}

func validateDeploymentOperationRegistration(registration DeploymentOperationRegistration, catalog workflowadapter.StageCatalog) error {
	if err := validateDeploymentStageContract(registration.Stage, catalog); err != nil {
		return err
	}
	if err := validateProviderReference(registration.Provider); err != nil {
		return err
	}
	if err := validateDeploymentVersionedReference("provider", registration.Provider.ID, registration.Provider.Version); err != nil {
		return err
	}
	if err := validateOperationBinding(registration.Operation); err != nil {
		return err
	}
	if registration.Operation.ProviderID != registration.Provider.ID {
		return fmt.Errorf("operation provider %q does not match provider %q", registration.Operation.ProviderID, registration.Provider.ID)
	}
	if err := validateDeploymentVersionedReference("operation", registration.Operation.OperationID, registration.Operation.Version); err != nil {
		return err
	}
	if err := validateDeploymentRuntimeReference(registration.Runtime); err != nil {
		return err
	}
	if err := validateDeploymentCheckoutContract(registration.Checkout); err != nil {
		return err
	}
	if registration.Secrets == nil {
		return errors.New("secret references must be an explicit array")
	}
	seenSecrets := make(map[string]workflowadapter.SecretReference, len(registration.Secrets))
	for _, secret := range registration.Secrets {
		if err := validateDeploymentSecretReference(secret); err != nil {
			return err
		}
		if existing, duplicate := seenSecrets[secret.ID]; duplicate {
			if existing != secret {
				return fmt.Errorf("secret %q has conflicting provider/version", secret.ID)
			}
			return fmt.Errorf("duplicate secret %q", secret.ID)
		}
		seenSecrets[secret.ID] = secret
	}
	payload, localCommand := registration.Operation.Payload.(workflowadapter.LocalCommandOperationPayload)
	if registration.HarborEvaluator != nil {
		if err := validateHarborEvaluatorCatalogRegistration(*registration.HarborEvaluator, registration, catalog); err != nil {
			return err
		}
	} else if localCommand && isHarborEvaluatorCommandID(payload.CommandID) {
		return fmt.Errorf("Harbor evaluator command %q requires a typed Harbor evaluator contract", payload.CommandID)
	}
	return nil
}

// validateDeploymentStageContract proves that one catalog registration belongs
// to the exact closed StageCatalog selected by the deployment catalog. It
// never consults StandardStageCatalog as a fallback.
func validateDeploymentStageContract(contract DeploymentStageContract, catalog workflowadapter.StageCatalog) error {
	if err := catalog.Validate(); err != nil {
		return fmt.Errorf("workflow template catalog: %w", err)
	}
	if err := validateDeploymentStageContractShape(contract); err != nil {
		return err
	}
	definition, present := catalog.Stage(contract.Key)
	if !present {
		return fmt.Errorf("stage %q is not in Harbor template %s@%s", contract.Key, catalog.Template.ID, catalog.Template.Version)
	}
	if contract.Group != definition.Group {
		return fmt.Errorf("stage %q group %q does not match Harbor template %s@%s group %q", contract.Key, contract.Group, catalog.Template.ID, catalog.Template.Version, definition.Group)
	}
	if contract.Plugin.ID != definition.Plugin.ID || contract.Plugin.Version != definition.Plugin.Version {
		return fmt.Errorf("stage %q plugin %s@%s does not match Harbor template %s@%s plugin %s@%s", contract.Key, contract.Plugin.ID, contract.Plugin.Version, catalog.Template.ID, catalog.Template.Version, definition.Plugin.ID, definition.Plugin.Version)
	}
	return nil
}

// validateDeploymentStageContractShape validates the closed stage-binding
// union without selecting a template. It is used by the independent lock
// parser; a catalog/lock resolver later compares the record against the exact
// template-bound catalog registration.
func validateDeploymentStageContractShape(contract DeploymentStageContract) error {
	if err := validateDeploymentCatalogString("stage key", string(contract.Key)); err != nil {
		return err
	}
	if err := validateDeploymentCatalogString("stage type", string(contract.Type)); err != nil {
		return err
	}
	if err := validateDeploymentCatalogString("stage group", string(contract.Group)); err != nil {
		return err
	}
	if err := contract.Plugin.Validate(); err != nil {
		return fmt.Errorf("stage plugin: %w", err)
	}
	if expectedType, present := deploymentStageBindingType(contract.Key); !present || contract.Type != expectedType {
		return fmt.Errorf("stage %q type %q is not the sealed Harbor binding type", contract.Key, contract.Type)
	}
	return nil
}

func validateDeploymentRuntimeReference(reference workflowadapter.RuntimeReference) error {
	if err := validateDeploymentCatalogString("runtime id", reference.ID); err != nil {
		return err
	}
	if err := validateDeploymentCatalogString("runtime kind", reference.Kind); err != nil {
		return err
	}
	return validateDeploymentVersionedReference("runtime", reference.ID, reference.Version)
}

func validateDeploymentCheckoutContract(contract DeploymentCheckoutContract) error {
	if err := validateDeploymentCatalogString("checkout id", contract.ID); err != nil {
		return err
	}
	return validateDeploymentCatalogString("checkout purpose", contract.Purpose)
}

func validateDeploymentSecretReference(reference workflowadapter.SecretReference) error {
	if err := validateDeploymentCatalogString("secret id", reference.ID); err != nil {
		return err
	}
	if err := validateDeploymentCatalogString("secret provider", reference.Provider); err != nil {
		return err
	}
	return validateDeploymentVersionedReference("secret", reference.ID, reference.Version)
}

func validateDeploymentVersionedReference(label, id, version string) error {
	if err := validateDeploymentCatalogString(label+" id", id); err != nil {
		return err
	}
	return validateDeploymentCatalogString(label+" version", version)
}

func validateDeploymentCatalogString(label, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%w: %s is required", ErrInvalidDeploymentOperationCatalog, label)
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return fmt.Errorf("%w: %s contains a control character", ErrInvalidDeploymentOperationCatalog, label)
		}
	}
	return nil
}

func validateDeploymentOperationResolution(resolution workflowadapter.StageOperationResolution) error {
	if err := validateStageOperationResolution(resolution); err != nil {
		return err
	}
	if err := validateDeploymentRuntimeReference(resolution.Runtime); err != nil {
		return fmt.Errorf("%w: resolution runtime: %v", ErrInvalidStageOperation, err)
	}
	if err := validateDeploymentCatalogString("resolution checkout id", resolution.Checkout.ID); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidStageOperation, err)
	}
	if err := validateDeploymentCatalogString("resolution checkout revision id", resolution.Checkout.RevisionID); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidStageOperation, err)
	}
	if err := resolution.Checkout.RevisionDigest.Validate(); err != nil {
		return fmt.Errorf("%w: resolution checkout revision digest: %v", ErrInvalidStageOperation, err)
	}
	seenInputs := make(map[string]struct{}, len(resolution.ArtifactInputs))
	for _, input := range resolution.ArtifactInputs {
		if err := validateDeploymentCatalogString("resolution artifact input port", input.Port); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidStageOperation, err)
		}
		if err := validateDeploymentCatalogString("resolution artifact input id", string(input.ArtifactID)); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidStageOperation, err)
		}
		if _, duplicate := seenInputs[input.Port]; duplicate {
			return fmt.Errorf("%w: duplicate resolution artifact input port %q", ErrInvalidStageOperation, input.Port)
		}
		seenInputs[input.Port] = struct{}{}
	}
	seenSecrets := make(map[string]workflowadapter.SecretReference, len(resolution.Secrets))
	for _, secret := range resolution.Secrets {
		if err := validateDeploymentSecretReference(secret); err != nil {
			return fmt.Errorf("%w: resolution %v", ErrInvalidStageOperation, err)
		}
		if existing, duplicate := seenSecrets[secret.ID]; duplicate {
			if existing != secret {
				return fmt.Errorf("%w: resolution secret %q has conflicting provider/version", ErrInvalidStageOperation, secret.ID)
			}
			return fmt.Errorf("%w: duplicate resolution secret %q", ErrInvalidStageOperation, secret.ID)
		}
		seenSecrets[secret.ID] = secret
	}
	return nil
}

type deploymentOperationCoordinate struct {
	stageKey         workflowkit.StageKey
	providerID       string
	providerVersion  string
	operationID      string
	operationVersion string
}

func deploymentCoordinateForRegistration(registration DeploymentOperationRegistration) deploymentOperationCoordinate {
	return deploymentOperationCoordinate{
		stageKey:         registration.Stage.Key,
		providerID:       registration.Provider.ID,
		providerVersion:  registration.Provider.Version,
		operationID:      registration.Operation.OperationID,
		operationVersion: registration.Operation.Version,
	}
}

func deploymentCoordinateForResolution(resolution workflowadapter.StageOperationResolution) deploymentOperationCoordinate {
	return deploymentOperationCoordinate{
		stageKey:         resolution.StageKey,
		providerID:       resolution.Provider.ID,
		providerVersion:  resolution.Provider.Version,
		operationID:      resolution.Operation.OperationID,
		operationVersion: resolution.Operation.Version,
	}
}

func (coordinate deploymentOperationCoordinate) less(other deploymentOperationCoordinate) bool {
	if coordinate.stageKey != other.stageKey {
		return coordinate.stageKey < other.stageKey
	}
	if coordinate.providerID != other.providerID {
		return coordinate.providerID < other.providerID
	}
	if coordinate.providerVersion != other.providerVersion {
		return coordinate.providerVersion < other.providerVersion
	}
	if coordinate.operationID != other.operationID {
		return coordinate.operationID < other.operationID
	}
	return coordinate.operationVersion < other.operationVersion
}

func (coordinate deploymentOperationCoordinate) String() string {
	return fmt.Sprintf("stage=%q provider=%q@%q operation=%q@%q", coordinate.stageKey, coordinate.providerID, coordinate.providerVersion, coordinate.operationID, coordinate.operationVersion)
}

func sameDeploymentSecrets(left, right []workflowadapter.SecretReference) bool {
	if len(left) != len(right) {
		return false
	}
	left = cloneDeploymentSecrets(left)
	right = cloneDeploymentSecrets(right)
	sort.Slice(left, func(first, second int) bool { return deploymentSecretLess(left[first], left[second]) })
	sort.Slice(right, func(first, second int) bool { return deploymentSecretLess(right[first], right[second]) })
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func cloneDeploymentSecrets(secrets []workflowadapter.SecretReference) []workflowadapter.SecretReference {
	if secrets == nil {
		return nil
	}
	cloned := make([]workflowadapter.SecretReference, len(secrets))
	copy(cloned, secrets)
	return cloned
}

func deploymentSecretLess(left, right workflowadapter.SecretReference) bool {
	if left.ID != right.ID {
		return left.ID < right.ID
	}
	if left.Provider != right.Provider {
		return left.Provider < right.Provider
	}
	return left.Version < right.Version
}

// deploymentStageBindingType keeps the catalog coupled to the sealed Harbor
// binding union. Adding a new standard stage must add its explicit binding
// type here; otherwise the deployment catalog refuses the new stage instead of
// silently treating a string stage name as executable.
func deploymentStageBindingType(key workflowkit.StageKey) (workflowadapter.StageBindingType, bool) {
	switch key {
	case workflowadapter.RepoPrepare:
		return workflowadapter.StageBindingRepoPrepare, true
	case workflowadapter.RepoStructureResearch:
		return workflowadapter.StageBindingRepoStructureResearch, true
	case workflowadapter.TestRuntimeResearch:
		return workflowadapter.StageBindingTestRuntimeResearch, true
	case workflowadapter.VerifierThreatResearch:
		return workflowadapter.StageBindingVerifierThreatResearch, true
	case workflowadapter.TaskSynthesis:
		return workflowadapter.StageBindingTaskSynthesis, true
	case workflowadapter.AuthoringLoop:
		return workflowadapter.StageBindingAuthoringLoop, true
	case workflowadapter.HostCandidateVerify:
		return workflowadapter.StageBindingHostCandidateVerify, true
	case workflowadapter.TestQualityCritic:
		return workflowadapter.StageBindingTestQualityCritic, true
	case workflowadapter.SolutionIntegrityCritic:
		return workflowadapter.StageBindingSolutionIntegrityCritic, true
	case workflowadapter.AuthoringRepair:
		return workflowadapter.StageBindingAuthoringRepair, true
	case workflowadapter.FinalAttestation:
		return workflowadapter.StageBindingFinalAttestation, true
	case workflowadapter.RepoAnalyze:
		return workflowadapter.StageBindingRepoAnalyze, true
	case workflowadapter.TaskDesign:
		return workflowadapter.StageBindingTaskDesign, true
	case workflowadapter.TaskReview:
		return workflowadapter.StageBindingTaskReview, true
	case workflowadapter.GenerateTaskFiles:
		return workflowadapter.StageBindingGenerateTaskFiles, true
	case workflowadapter.InstructionGen:
		return workflowadapter.StageBindingInstructionGen, true
	case workflowadapter.TaskTOMLGen:
		return workflowadapter.StageBindingTaskTOMLGen, true
	case workflowadapter.DockerfileGen:
		return workflowadapter.StageBindingDockerfileGen, true
	case workflowadapter.DockerfileBuildValidate:
		return workflowadapter.StageBindingDockerfileBuildValidate, true
	case workflowadapter.ContentReview:
		return workflowadapter.StageBindingContentReview, true
	case workflowadapter.SolveGen:
		return workflowadapter.StageBindingSolveGen, true
	case workflowadapter.TestGen:
		return workflowadapter.StageBindingTestGen, true
	case workflowadapter.AuthoringHarness:
		return workflowadapter.StageBindingAuthoringHarness, true
	case workflowadapter.TestsAnalysis:
		return workflowadapter.StageBindingTestsAnalysis, true
	case workflowadapter.CodeEdgePackageAdmission:
		return workflowadapter.StageBindingCodeEdgePackageAdmission, true
	case workflowadapter.SolutionReview:
		return workflowadapter.StageBindingSolutionReview, true
	case workflowadapter.MaterializeTask:
		return workflowadapter.StageBindingMaterializeTask, true
	case workflowadapter.TaskRepair:
		return workflowadapter.StageBindingTaskRepair, true
	case workflowadapter.RuntimeSelfCheck:
		return workflowadapter.StageBindingRuntimeSelfCheck, true
	case workflowadapter.HarborVerify:
		return workflowadapter.StageBindingHarborVerify, true
	case workflowadapter.DockerBuild:
		return workflowadapter.StageBindingDockerBuild, true
	case workflowadapter.InitialVerify:
		return workflowadapter.StageBindingInitialVerify, true
	case workflowadapter.OracleVerify:
		return workflowadapter.StageBindingOracleVerify, true
	case workflowadapter.CodeEdgeLint:
		return workflowadapter.StageBindingCodeEdgeLint, true
	case workflowadapter.QualityCheck:
		return workflowadapter.StageBindingQualityCheck, true
	case workflowadapter.SimilarityCheck:
		return workflowadapter.StageBindingSimilarityCheck, true
	case workflowadapter.FinalReview:
		return workflowadapter.StageBindingFinalReview, true
	case workflowadapter.HarborRunQwen:
		return workflowadapter.StageBindingHarborRunQwen, true
	case workflowadapter.HarborRunOpus:
		return workflowadapter.StageBindingHarborRunOpus, true
	case workflowadapter.EvaluatorEvidenceHandoff:
		return workflowadapter.StageBindingEvaluatorEvidenceHandoff, true
	case workflowadapter.ResultReview:
		return workflowadapter.StageBindingResultReview, true
	case workflowadapter.SubmissionLint:
		return workflowadapter.StageBindingSubmissionLint, true
	case workflowadapter.Package:
		return workflowadapter.StageBindingPackage, true
	default:
		return "", false
	}
}

func decodeDeploymentCatalogJSON(raw []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return fmt.Errorf("decode trailing JSON value: %w", err)
	}
	return nil
}

func rejectDuplicateDeploymentCatalogJSONKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := walkDeploymentCatalogJSONValue(decoder, "$"); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return fmt.Errorf("decode trailing JSON value: %w", err)
	}
	return nil
}

func walkDeploymentCatalogJSONValue(decoder *json.Decoder, location string) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("object key at %s is not a string", location)
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate object key %q at %s", key, location)
			}
			seen[key] = struct{}{}
			if err := walkDeploymentCatalogJSONValue(decoder, location+"."+key); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return fmt.Errorf("object at %s is not closed", location)
		}
	case '[':
		index := 0
		for decoder.More() {
			if err := walkDeploymentCatalogJSONValue(decoder, fmt.Sprintf("%s[%d]", location, index)); err != nil {
				return err
			}
			index++
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return fmt.Errorf("array at %s is not closed", location)
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q at %s", delimiter, location)
	}
	return nil
}

var _ workflowadapter.StageOperationResolver = (*DeploymentOperationCatalogResolver)(nil)
