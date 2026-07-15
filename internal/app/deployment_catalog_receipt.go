package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
)

// deploymentCatalogReceiptFileName is deliberately separate from the run
// manifest. It gives a worker one small, canonical, independently-addressable
// receipt to compare before it starts an external stage effect.
const (
	deploymentCatalogReceiptFileName      = "deployment-catalog.receipt.json"
	deploymentCatalogLockIdentityFileName = "deployment-catalog.lock-identity.json"
)

// DeploymentCatalogReceiptResolver is the production allow-list boundary that
// can freeze and later verify a DeploymentOperationCatalogReceipt. The
// stageprovider DeploymentOperationCatalogResolver implements it directly.
//
// Keeping this interface at the application boundary lets a composition use a
// catalog-aware combined provider resolver once it exposes the same receipt
// contract, without teaching lifecycle code about provider handler types.
// Merely installing an OperationResolver does not opt a deployment into this
// receipt contract; callers must supply one here (or use an OperationResolver
// that implements this interface) and may require it explicitly.
type DeploymentCatalogReceiptResolver interface {
	workflowadapter.StageOperationResolver
	Receipt() stageprovider.DeploymentOperationCatalogReceipt
	CanonicalReceiptJSON() ([]byte, error)
	VerifyReceipt(stageprovider.DeploymentOperationCatalogReceipt) error
}

// DeploymentCatalogLockIdentityResolver is an optional strengthening of the
// catalog receipt boundary. The stageprovider
// CatalogLockAttestedWorkflowkitProviderOperationResolver implements this
// contract, allowing a Run to pin both the approved operation catalog and the
// concrete immutable lock that attests its implementation identities.
//
// It deliberately remains separate from DeploymentCatalogReceiptResolver so
// catalog-only and non-production compositions do not acquire a fabricated
// production lock identity.
type DeploymentCatalogLockIdentityResolver interface {
	LockIdentity() stageprovider.DeploymentOperationCatalogLockIdentity
	VerifyLockIdentity(stageprovider.DeploymentOperationCatalogLockIdentity) error
}

// TemplateDeploymentCatalogResolver installs one immutable deployment catalog
// (and, when supported by Resolver, its immutable operation lock) for exactly
// one closed workflow template. The explicit template key prevents a catalog
// for a CodeEdge parent Run from authorizing its evaluator child, or vice
// versa.
//
// Resolver's receipt must name exactly Template. NewLifecycleServicesWithOptions
// rejects a mismatch during composition rather than deferring it to a later
// StartRun or worker claim.
type TemplateDeploymentCatalogResolver struct {
	Template workflowadapter.TemplateReference
	Resolver DeploymentCatalogReceiptResolver
}

type deploymentCatalogBinding struct {
	template              workflowadapter.TemplateReference
	resolver              DeploymentCatalogReceiptResolver
	canonical             []byte
	lockResolver          DeploymentCatalogLockIdentityResolver
	lockIdentity          *stageprovider.DeploymentOperationCatalogLockIdentity
	canonicalLockIdentity []byte
}

// deploymentCatalogRegistry is an immutable, template-keyed deployment
// capability snapshot. It intentionally has no fallback binding: once a
// deployment opts into catalog enforcement, every admitted Run must resolve
// the catalog matching its own frozen template.
type deploymentCatalogRegistry struct {
	bindings map[workflowadapter.TemplateReference]*deploymentCatalogBinding
}

func newDeploymentCatalogRegistry(configured []TemplateDeploymentCatalogResolver) (*deploymentCatalogRegistry, error) {
	if len(configured) == 0 {
		return nil, nil
	}
	registry := &deploymentCatalogRegistry{
		bindings: make(map[workflowadapter.TemplateReference]*deploymentCatalogBinding, len(configured)),
	}
	for index, entry := range configured {
		if err := entry.Template.Validate(); err != nil {
			return nil, fmt.Errorf("validate deployment catalog binding %d template: %w", index, err)
		}
		if entry.Resolver == nil {
			return nil, fmt.Errorf("%w: deployment catalog binding %s@%s has no resolver", stageprovider.ErrDeploymentOperationCatalogUnavailable, entry.Template.ID, entry.Template.Version)
		}
		if _, duplicate := registry.bindings[entry.Template]; duplicate {
			return nil, fmt.Errorf("%w: duplicate deployment catalog binding for workflow template %s@%s", stageprovider.ErrDeploymentOperationCatalogDrift, entry.Template.ID, entry.Template.Version)
		}
		binding, err := newDeploymentCatalogBinding(entry.Resolver)
		if err != nil {
			return nil, fmt.Errorf("configure deployment catalog binding %s@%s: %w", entry.Template.ID, entry.Template.Version, err)
		}
		if binding == nil || !binding.template.Equal(entry.Template) {
			return nil, fmt.Errorf("%w: deployment catalog binding %s@%s receipt names another workflow template", stageprovider.ErrDeploymentOperationCatalogDrift, entry.Template.ID, entry.Template.Version)
		}
		registry.bindings[entry.Template] = binding
	}
	return registry, nil
}

func (registry *deploymentCatalogRegistry) bindingFor(template workflowadapter.TemplateReference) (*deploymentCatalogBinding, bool) {
	if registry == nil {
		return nil, false
	}
	binding, present := registry.bindings[template]
	return binding, present
}

func (registry *deploymentCatalogRegistry) allBindingsHaveLocks() bool {
	if registry == nil || len(registry.bindings) == 0 {
		return false
	}
	for _, binding := range registry.bindings {
		if binding == nil || binding.lockResolver == nil || binding.lockIdentity == nil {
			return false
		}
	}
	return true
}

// soleBinding preserves the legacy single-resolver composition only for
// cross-Run CodeEdge evidence inspection. It must never be used by StartRun,
// replay, or worker admission, where a catalog from another template would be
// an unsafe fallback.
func (registry *deploymentCatalogRegistry) soleBinding() (*deploymentCatalogBinding, bool) {
	if registry == nil || len(registry.bindings) != 1 {
		return nil, false
	}
	for _, binding := range registry.bindings {
		return binding, binding != nil
	}
	return nil, false
}

func newDeploymentCatalogBinding(resolver DeploymentCatalogReceiptResolver) (*deploymentCatalogBinding, error) {
	if resolver == nil {
		return nil, nil
	}
	receipt := resolver.Receipt()
	if err := receipt.Validate(); err != nil {
		return nil, fmt.Errorf("validate configured deployment catalog receipt: %w", err)
	}
	canonical, err := resolver.CanonicalReceiptJSON()
	if err != nil {
		return nil, fmt.Errorf("canonicalize configured deployment catalog receipt: %w", err)
	}
	_, normalized, err := canonicalDeploymentCatalogReceipt(canonical)
	if err != nil {
		return nil, fmt.Errorf("validate configured deployment catalog receipt bytes: %w", err)
	}
	receiptCanonical, err := receipt.CanonicalJSON()
	if err != nil {
		return nil, fmt.Errorf("canonicalize configured deployment catalog receipt value: %w", err)
	}
	if !bytes.Equal(canonical, normalized) || !bytes.Equal(normalized, receiptCanonical) {
		return nil, fmt.Errorf("%w: configured deployment catalog receipt is not canonical", stageprovider.ErrDeploymentOperationCatalogDrift)
	}
	if err := resolver.VerifyReceipt(receipt); err != nil {
		return nil, fmt.Errorf("verify configured deployment catalog receipt: %w", err)
	}
	binding := &deploymentCatalogBinding{
		template: receipt.Template, resolver: resolver, canonical: append([]byte(nil), canonical...),
	}
	if lockResolver, ok := resolver.(DeploymentCatalogLockIdentityResolver); ok {
		identity := lockResolver.LockIdentity()
		if err := identity.Validate(); err != nil {
			return nil, fmt.Errorf("validate configured deployment catalog lock identity: %w", err)
		}
		if err := lockResolver.VerifyLockIdentity(identity); err != nil {
			return nil, fmt.Errorf("verify configured deployment catalog lock identity: %w", err)
		}
		canonicalIdentity, err := canonicalDeploymentCatalogLockIdentity(identity)
		if err != nil {
			return nil, fmt.Errorf("canonicalize configured deployment catalog lock identity: %w", err)
		}
		binding.lockResolver = lockResolver
		binding.lockIdentity = &identity
		binding.canonicalLockIdentity = canonicalIdentity
	}
	return binding, nil
}

func canonicalDeploymentCatalogReceipt(raw []byte) (stageprovider.DeploymentOperationCatalogReceipt, []byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return stageprovider.DeploymentOperationCatalogReceipt{}, nil, fmt.Errorf("%w: deployment catalog receipt is required", stageprovider.ErrInvalidDeploymentOperationCatalog)
	}
	receipt, err := stageprovider.ParseDeploymentOperationCatalogReceiptJSON(raw)
	if err != nil {
		return stageprovider.DeploymentOperationCatalogReceipt{}, nil, err
	}
	canonical, err := receipt.CanonicalJSON()
	if err != nil {
		return stageprovider.DeploymentOperationCatalogReceipt{}, nil, err
	}
	return receipt, canonical, nil
}

// canonicalDeploymentCatalogLockIdentity returns the stable compact JSON
// representation used by the separately managed identity file. The manifest
// stores the typed value so it remains readable when the outer manifest is
// formatted for operators; the companion file stays byte-addressable for a
// worker before it starts a provider operation.
func canonicalDeploymentCatalogLockIdentity(identity stageprovider.DeploymentOperationCatalogLockIdentity) ([]byte, error) {
	if err := identity.Validate(); err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(identity)
	if err != nil {
		return nil, fmt.Errorf("encode deployment catalog lock identity: %w", err)
	}
	return canonical, nil
}

func parseDeploymentCatalogLockIdentityJSON(raw []byte) (stageprovider.DeploymentOperationCatalogLockIdentity, []byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return stageprovider.DeploymentOperationCatalogLockIdentity{}, nil, fmt.Errorf("%w: deployment catalog lock identity is required", stageprovider.ErrInvalidDeploymentOperationCatalogLock)
	}
	var identity stageprovider.DeploymentOperationCatalogLockIdentity
	if err := decodeStrictJSON(string(raw), &identity); err != nil {
		return stageprovider.DeploymentOperationCatalogLockIdentity{}, nil, fmt.Errorf("%w: decode deployment catalog lock identity: %v", stageprovider.ErrInvalidDeploymentOperationCatalogLock, err)
	}
	canonical, err := canonicalDeploymentCatalogLockIdentity(identity)
	if err != nil {
		return stageprovider.DeploymentOperationCatalogLockIdentity{}, nil, err
	}
	return identity, canonical, nil
}

func cloneDeploymentCatalogLockIdentity(identity *stageprovider.DeploymentOperationCatalogLockIdentity) *stageprovider.DeploymentOperationCatalogLockIdentity {
	if identity == nil {
		return nil
	}
	copy := *identity
	return &copy
}

// deploymentCatalogBindingForTemplate returns the one immutable catalog
// binding that may authorize a Run for template. A configured registry never
// falls back to another template's catalog: a missing key is a fail-closed
// deployment error, not a request to reuse the first installed resolver.
func (core *lifecycleServiceCore) deploymentCatalogBindingForTemplate(template workflowadapter.TemplateReference) (*deploymentCatalogBinding, error) {
	if core == nil || core.deploymentCatalogs == nil {
		return nil, nil
	}
	if err := template.Validate(); err != nil {
		return nil, fmt.Errorf("validate deployment catalog template: %w", err)
	}
	binding, present := core.deploymentCatalogs.bindingFor(template)
	if !present || binding == nil {
		return nil, fmt.Errorf("%w: no configured deployment catalog binding for workflow template %s@%s", stageprovider.ErrDeploymentOperationCatalogUnavailable, template.ID, template.Version)
	}
	return binding, nil
}

// configuredDeploymentCatalogBindingForTemplate is the non-failing lookup
// used only by cross-Run CodeEdge evidence inspection. Normal StartRun,
// replay, and worker paths must use deploymentCatalogBindingForTemplate so an
// unbound executable Run remains fail-closed.
func (core *lifecycleServiceCore) configuredDeploymentCatalogBindingForTemplate(template workflowadapter.TemplateReference) (*deploymentCatalogBinding, bool) {
	if core == nil || core.deploymentCatalogs == nil {
		return nil, false
	}
	return core.deploymentCatalogs.bindingFor(template)
}

// frozenDeploymentCatalogReceipt returns the immutable receipt configured for
// a new Run's exact template. A nil byte slice means this is an explicitly
// non-production lifecycle composition; it never fabricates a catalog
// identity for an accept-all fixture resolver.
func (core *lifecycleServiceCore) frozenDeploymentCatalogReceipt(template workflowadapter.TemplateReference) ([]byte, error) {
	binding, err := core.deploymentCatalogBindingForTemplate(template)
	if err != nil || binding == nil {
		return nil, err
	}
	return append([]byte(nil), binding.canonical...), nil
}

// frozenDeploymentCatalogLockIdentity returns the immutable configured lock
// identity for a new Run's exact template. A nil identity preserves the
// explicit catalog-only and non-production composition modes.
func (core *lifecycleServiceCore) frozenDeploymentCatalogLockIdentity(template workflowadapter.TemplateReference) (*stageprovider.DeploymentOperationCatalogLockIdentity, error) {
	binding, err := core.deploymentCatalogBindingForTemplate(template)
	if err != nil || binding == nil || binding.lockResolver == nil || binding.lockIdentity == nil {
		return nil, err
	}
	identity := *binding.lockIdentity
	if err := core.verifyDeploymentCatalogLockIdentity(template, &identity); err != nil {
		return nil, err
	}
	return &identity, nil
}

// resolveStartRunDeploymentCatalogReceipt either verifies the receipt already
// sealed in an input bundle or obtains the one immutable receipt owned by this
// process's binding for template. It never replaces a bundle receipt with a
// new current catalog value: that substitution would turn a retry into a
// different Run definition.
func (core *lifecycleServiceCore) resolveStartRunDeploymentCatalogReceipt(template workflowadapter.TemplateReference, raw []byte) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		frozen, err := core.frozenDeploymentCatalogReceipt(template)
		if err != nil {
			return nil, err
		}
		if err := core.verifyDeploymentCatalogReceipt(template, frozen); err != nil {
			return nil, err
		}
		return frozen, nil
	}
	_, canonical, err := canonicalDeploymentCatalogReceipt(raw)
	if err != nil {
		return nil, err
	}
	if err := core.verifyDeploymentCatalogReceipt(template, canonical); err != nil {
		return nil, err
	}
	return canonical, nil
}

// resolveStartRunDeploymentCatalogLockIdentity either verifies the identity
// sealed in a StartRun input bundle or freezes this process's template-bound
// lock. It never substitutes a newly loaded lock for a bundle identity on
// replay.
func (core *lifecycleServiceCore) resolveStartRunDeploymentCatalogLockIdentity(template workflowadapter.TemplateReference, identity *stageprovider.DeploymentOperationCatalogLockIdentity) (*stageprovider.DeploymentOperationCatalogLockIdentity, error) {
	if identity == nil {
		return core.frozenDeploymentCatalogLockIdentity(template)
	}
	copy := *identity
	if err := core.verifyDeploymentCatalogLockIdentity(template, &copy); err != nil {
		return nil, err
	}
	return &copy, nil
}

// verifyDeploymentCatalogReceipt checks both canonical receipt bytes frozen
// into a bundle/manifest and the catalog bound to template. The second check
// is what makes a separately started worker reject a catalog substitution
// before it can start an external side effect.
func (core *lifecycleServiceCore) verifyDeploymentCatalogReceipt(template workflowadapter.TemplateReference, raw []byte) error {
	binding, err := core.deploymentCatalogBindingForTemplate(template)
	if err != nil {
		return err
	}
	if binding == nil {
		if len(bytes.TrimSpace(raw)) != 0 {
			return fmt.Errorf("%w: frozen deployment catalog receipt has no configured verifier", stageprovider.ErrDeploymentOperationCatalogUnavailable)
		}
		return nil
	}
	receipt, canonical, err := canonicalDeploymentCatalogReceipt(raw)
	if err != nil {
		return fmt.Errorf("validate frozen deployment catalog receipt: %w", err)
	}
	if !receipt.Template.Equal(template) {
		return fmt.Errorf("%w: frozen deployment catalog receipt template %s@%s does not match Run template %s@%s", stageprovider.ErrDeploymentOperationCatalogDrift, receipt.Template.ID, receipt.Template.Version, template.ID, template.Version)
	}
	if !bytes.Equal(raw, canonical) {
		return fmt.Errorf("%w: frozen deployment catalog receipt is not canonical", stageprovider.ErrDeploymentOperationCatalogDrift)
	}
	if !bytes.Equal(canonical, binding.canonical) {
		return fmt.Errorf("%w: frozen deployment catalog receipt differs from this deployment", stageprovider.ErrDeploymentOperationCatalogDrift)
	}
	if err := binding.resolver.VerifyReceipt(receipt); err != nil {
		return fmt.Errorf("verify frozen deployment catalog receipt: %w", err)
	}
	return nil
}

// verifyDeploymentCatalogLockIdentity proves that a frozen lock identity is
// exactly the one installed for template. A configured catalog-only resolver
// remains valid with no identity; a lock-aware resolver rejects an absent
// identity so an older or tampered manifest cannot silently bypass the
// stronger production boundary.
func (core *lifecycleServiceCore) verifyDeploymentCatalogLockIdentity(template workflowadapter.TemplateReference, identity *stageprovider.DeploymentOperationCatalogLockIdentity) error {
	binding, err := core.deploymentCatalogBindingForTemplate(template)
	if err != nil {
		return err
	}
	if binding == nil || binding.lockResolver == nil || binding.lockIdentity == nil {
		if identity != nil {
			return fmt.Errorf("%w: frozen deployment catalog lock identity has no configured verifier", stageprovider.ErrDeploymentOperationCatalogLockUnavailable)
		}
		return nil
	}
	if identity == nil {
		return fmt.Errorf("%w: frozen deployment catalog lock identity is required", stageprovider.ErrDeploymentOperationCatalogLockDrift)
	}
	if err := identity.Validate(); err != nil {
		return fmt.Errorf("validate frozen deployment catalog lock identity: %w", err)
	}
	canonical, err := canonicalDeploymentCatalogLockIdentity(*identity)
	if err != nil {
		return fmt.Errorf("canonicalize frozen deployment catalog lock identity: %w", err)
	}
	if !bytes.Equal(canonical, binding.canonicalLockIdentity) {
		return fmt.Errorf("%w: frozen deployment catalog lock identity is not this deployment's canonical identity", stageprovider.ErrDeploymentOperationCatalogLockDrift)
	}
	if *identity != *binding.lockIdentity {
		return fmt.Errorf("%w: frozen deployment catalog lock identity differs from this deployment", stageprovider.ErrDeploymentOperationCatalogLockDrift)
	}
	if err := binding.lockResolver.VerifyLockIdentity(*identity); err != nil {
		return fmt.Errorf("verify frozen deployment catalog lock identity: %w", err)
	}
	return nil
}

func (core *lifecycleServiceCore) validateDeploymentCatalogExecutionSpec(specification workflowadapter.RunExecutionSpec) error {
	binding, err := core.deploymentCatalogBindingForTemplate(specification.Template)
	if err != nil || binding == nil {
		return err
	}
	if err := specification.ValidateWithOperationResolver(binding.resolver); err != nil {
		return fmt.Errorf("validate explicit execution specification against deployment catalog: %w", err)
	}
	return nil
}

// canonicalManifestDeploymentCatalogReceipt returns the compact canonical
// receipt embedded in a pretty-printed run manifest. json.RawMessage retains
// source whitespace on decode, so callers compare the normalized bytes rather
// than assuming the manifest's lexical representation itself is compact.
func canonicalManifestDeploymentCatalogReceipt(manifest runManifest) ([]byte, error) {
	if len(bytes.TrimSpace(manifest.DeploymentCatalogReceipt)) == 0 {
		return nil, nil
	}
	_, canonical, err := canonicalDeploymentCatalogReceipt(manifest.DeploymentCatalogReceipt)
	if err != nil {
		return nil, err
	}
	return canonical, nil
}

func canonicalManifestDeploymentCatalogLockIdentity(manifest runManifest) (*stageprovider.DeploymentOperationCatalogLockIdentity, error) {
	if manifest.DeploymentCatalogLockIdentity == nil {
		return nil, nil
	}
	identity := *manifest.DeploymentCatalogLockIdentity
	if _, err := canonicalDeploymentCatalogLockIdentity(identity); err != nil {
		return nil, err
	}
	return &identity, nil
}

// verifyRunDeploymentCatalogReceipt proves the database manifest and its
// separately managed receipt file are the same canonical receipt and that the
// current worker's catalog still authorizes it. It is intentionally a no-op
// for a lifecycle composition that did not opt into catalog enforcement, so
// existing non-production test resolvers do not gain an invented identity.
func (core *lifecycleServiceCore) verifyRunDeploymentCatalogReceipt(run store.WorkflowRun) error {
	if core == nil || core.deploymentCatalogs == nil {
		return nil
	}
	var manifest runManifest
	if err := decodeStrictJSON(run.RunManifestJSON, &manifest); err != nil {
		return fmt.Errorf("decode run manifest deployment catalog receipt: %w", err)
	}
	specification, _, _, err := canonicalFrozenRunExecutionSpec(manifest, run)
	if err != nil {
		return fmt.Errorf("decode run manifest execution specification for deployment catalog receipt: %w", err)
	}
	template := specification.Template
	if run.WorkflowTemplateID != template.ID || run.WorkflowTemplateVersion != template.Version || !manifest.Resolved.Template.Equal(template) {
		return fmt.Errorf("%w: Run template identity does not match its frozen execution specification", stageprovider.ErrDeploymentOperationCatalogDrift)
	}
	canonical, err := canonicalManifestDeploymentCatalogReceipt(manifest)
	if err != nil {
		return fmt.Errorf("decode run manifest deployment catalog receipt: %w", err)
	}
	if err := core.verifyDeploymentCatalogReceipt(template, canonical); err != nil {
		return fmt.Errorf("verify run manifest deployment catalog receipt: %w", err)
	}
	lockIdentity, err := canonicalManifestDeploymentCatalogLockIdentity(manifest)
	if err != nil {
		return fmt.Errorf("decode run manifest deployment catalog lock identity: %w", err)
	}
	if err := core.verifyDeploymentCatalogLockIdentity(template, lockIdentity); err != nil {
		return fmt.Errorf("verify run manifest deployment catalog lock identity: %w", err)
	}

	path := filepath.Join(core.layout.runDirectory(run.ID), deploymentCatalogReceiptFileName)
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect managed deployment catalog receipt: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%w: managed deployment catalog receipt is not a regular file", stageprovider.ErrDeploymentOperationCatalogDrift)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read managed deployment catalog receipt: %w", err)
	}
	if err := core.verifyDeploymentCatalogReceipt(template, raw); err != nil {
		return fmt.Errorf("verify managed deployment catalog receipt: %w", err)
	}
	if !bytes.Equal(raw, canonical) {
		return fmt.Errorf("%w: run manifest and managed deployment catalog receipt differ", stageprovider.ErrDeploymentOperationCatalogDrift)
	}
	if lockIdentity != nil {
		lockPath := filepath.Join(core.layout.runDirectory(run.ID), deploymentCatalogLockIdentityFileName)
		lockRaw, lockErr := readManagedRunLockIdentityFile(lockPath)
		if lockErr != nil {
			return lockErr
		}
		parsedLockIdentity, canonicalLockIdentity, lockErr := parseDeploymentCatalogLockIdentityJSON(lockRaw)
		if lockErr != nil {
			return fmt.Errorf("decode managed deployment catalog lock identity: %w", lockErr)
		}
		if !bytes.Equal(lockRaw, canonicalLockIdentity) {
			return fmt.Errorf("%w: managed deployment catalog lock identity is not canonical", stageprovider.ErrDeploymentOperationCatalogLockDrift)
		}
		if err := core.verifyDeploymentCatalogLockIdentity(template, &parsedLockIdentity); err != nil {
			return fmt.Errorf("verify managed deployment catalog lock identity: %w", err)
		}
		if parsedLockIdentity != *lockIdentity {
			return fmt.Errorf("%w: run manifest and managed deployment catalog lock identity differ", stageprovider.ErrDeploymentOperationCatalogLockDrift)
		}
	}
	return nil
}

func readManagedRunLockIdentityFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect managed deployment catalog lock identity: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: managed deployment catalog lock identity is not a regular file", stageprovider.ErrDeploymentOperationCatalogLockDrift)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read managed deployment catalog lock identity: %w", err)
	}
	return raw, nil
}
