package stageprovider

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"unicode"

	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	// StandardAuthoringContractLockFormat identifies the typed, deployment
	// owned prompt/schema binding for one operation in the closed Standard
	// authoring template. It intentionally contains only stable asset
	// identities and deployment-relative paths: the existing lock-record
	// fingerprints remain the sole content identities.
	StandardAuthoringContractLockFormat  = "harbor.standard-authoring-contract.v1"
	StandardAuthoringContractLockVersion = "1"

	// StandardAuthoringContractAssetManifestFormat identifies the source
	// controlled deployment asset inventory used by the deterministic lock
	// generator. It is deliberately separate from the generated lock: the
	// manifest names assets and paths while the generated lock records their
	// raw content SHA-256 values together with concrete host runtime facts.
	StandardAuthoringContractAssetManifestFormat  = "harbor.standard-authoring-contract-assets.v1"
	StandardAuthoringContractAssetManifestVersion = "1"
)

// StandardAuthoringContractAssetReference identifies one immutable prompt or
// output-schema asset below the deployment-owned Standard authoring contract
// root. RelativePath is deliberately never a caller workspace path and is
// checked again at execution time before the file is opened.
type StandardAuthoringContractAssetReference struct {
	ID           string `json:"id"`
	Version      string `json:"version"`
	RelativePath string `json:"relative_path"`
}

// Clone returns an independently owned asset reference. The type is scalar
// today, but this boundary makes future additions explicit rather than
// accidentally sharing mutable data through a lock-record copy.
func (reference StandardAuthoringContractAssetReference) Clone() StandardAuthoringContractAssetReference {
	return reference
}

// Validate proves that an asset coordinate is canonical and cannot express a
// path traversal, absolute path, platform-specific alternate separator, or
// a mutable/unversioned identity. The filesystem root itself is intentionally
// not part of the lock: deployment composition supplies it separately and the
// runtime attestor proves its containment/no-symlink invariant per effect.
func (reference StandardAuthoringContractAssetReference) Validate() error {
	if err := validateStandardAuthoringContractAssetID(reference.ID); err != nil {
		return err
	}
	if err := validateStandardAuthoringContractAssetVersion(reference.Version); err != nil {
		return err
	}
	return validateStandardAuthoringContractRelativePath(reference.RelativePath)
}

// StandardAuthoringContractLock is the typed record extension named
// standard_authoring_contract in a deployment operation lock. Prompt and the
// primary Schema map named deployment assets to the pre-existing
// PromptContentFingerprint and SchemaContentFingerprint of the enclosing
// record respectively. AdditionalSchemas are secondary output contracts whose
// content hashes live directly beside their deployment-relative identity.
type StandardAuthoringContractLock struct {
	Format            string                                          `json:"format"`
	Version           string                                          `json:"version"`
	Prompt            StandardAuthoringContractAssetReference         `json:"prompt"`
	Schema            StandardAuthoringContractAssetReference         `json:"schema"`
	AdditionalSchemas []StandardAuthoringContractAdditionalSchemaLock `json:"additional_schemas,omitempty"`
}

// StandardAuthoringContractAdditionalSchemaLock pins a secondary schema asset
// that is not represented by the generic lock record's single
// SchemaContentFingerprint field.
type StandardAuthoringContractAdditionalSchemaLock struct {
	StandardAuthoringContractAssetReference
	ContentSHA256 workflowkit.Fingerprint `json:"content_sha256"`
}

// StandardAuthoringContractAssetManifestEntry selects the two immutable
// primary deployment assets for one exact Standard authoring stage, plus any
// secondary schemas owned by built-in stage outputs. It contains no arbitrary
// configuration and no workspace path; entries are expanded into typed lock
// extensions only by the deterministic lock generator.
type StandardAuthoringContractAssetManifestEntry struct {
	StageKey          workflowkit.StageKey                      `json:"stage_key"`
	Prompt            StandardAuthoringContractAssetReference   `json:"prompt"`
	Schema            StandardAuthoringContractAssetReference   `json:"schema"`
	AdditionalSchemas []StandardAuthoringContractAssetReference `json:"additional_schemas,omitempty"`
}

// Clone returns independently owned manifest entry values.
func (entry StandardAuthoringContractAssetManifestEntry) Clone() StandardAuthoringContractAssetManifestEntry {
	entry.Prompt = entry.Prompt.Clone()
	entry.Schema = entry.Schema.Clone()
	entry.AdditionalSchemas = cloneStandardAuthoringContractAssetReferences(entry.AdditionalSchemas)
	return entry
}

// StandardAuthoringContractAssetManifest is the complete source-controlled
// mapping from the closed authoring stage catalog to prompt/schema asset
// paths. It intentionally has exact stage coverage; a catalog generator never
// discovers an asset through naming convention or a directory scan.
type StandardAuthoringContractAssetManifest struct {
	Format     string                                        `json:"format"`
	Version    string                                        `json:"version"`
	Template   workflowadapter.TemplateReference             `json:"template"`
	Operations []StandardAuthoringContractAssetManifestEntry `json:"operations"`
}

// Clone returns an independently owned manifest.
func (manifest StandardAuthoringContractAssetManifest) Clone() StandardAuthoringContractAssetManifest {
	operations := manifest.Operations
	manifest.Operations = make([]StandardAuthoringContractAssetManifestEntry, len(operations))
	for index, operation := range operations {
		manifest.Operations[index] = operation.Clone()
	}
	return manifest
}

// Validate proves the manifest is an exact, canonical asset inventory for the
// closed Standard authoring template.
func (manifest StandardAuthoringContractAssetManifest) Validate() error {
	if manifest.Format != StandardAuthoringContractAssetManifestFormat {
		return fmt.Errorf("%w: unsupported Standard authoring contract asset manifest format %q", ErrInvalidDeploymentOperationCatalogLock, manifest.Format)
	}
	if manifest.Version != StandardAuthoringContractAssetManifestVersion {
		return fmt.Errorf("%w: unsupported Standard authoring contract asset manifest version %q", ErrInvalidDeploymentOperationCatalogLock, manifest.Version)
	}
	if err := manifest.Template.Validate(); err != nil {
		return fmt.Errorf("%w: Standard authoring contract asset manifest template: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	if !workflowadapter.IsStandardAuthoringWorkflowTemplate(manifest.Template) {
		return fmt.Errorf("%w: Standard authoring contract asset manifest must bind an installed Standard authoring template", ErrInvalidDeploymentOperationCatalogLock)
	}
	if manifest.Operations == nil {
		return fmt.Errorf("%w: Standard authoring contract asset manifest operations must be an explicit array", ErrInvalidDeploymentOperationCatalogLock)
	}
	stageOrder, err := workflowadapter.StandardAuthoringStageOrderForTemplate(manifest.Template)
	if err != nil {
		return fmt.Errorf("%w: Standard authoring contract asset manifest template: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	expected := make(map[workflowkit.StageKey]struct{}, len(stageOrder))
	for _, stageKey := range stageOrder {
		expected[stageKey] = struct{}{}
	}
	if len(manifest.Operations) != len(expected) {
		return fmt.Errorf("%w: Standard authoring contract asset manifest has %d operations, want %d", ErrInvalidDeploymentOperationCatalogLock, len(manifest.Operations), len(expected))
	}
	seenStages := make(map[workflowkit.StageKey]struct{}, len(manifest.Operations))
	assetIdentities := make(map[standardAuthoringContractAssetIdentity]string, len(manifest.Operations)*3)
	assetPaths := make(map[string]standardAuthoringContractAssetIdentity, len(manifest.Operations)*3)
	for index, entry := range manifest.Operations {
		if _, present := expected[entry.StageKey]; !present {
			return fmt.Errorf("%w: Standard authoring contract asset manifest operation %d has unknown stage %q", ErrInvalidDeploymentOperationCatalogLock, index, entry.StageKey)
		}
		if _, duplicate := seenStages[entry.StageKey]; duplicate {
			return fmt.Errorf("%w: Standard authoring contract asset manifest has duplicate stage %q", ErrInvalidDeploymentOperationCatalogLock, entry.StageKey)
		}
		seenStages[entry.StageKey] = struct{}{}
		if err := validateStandardAuthoringPrimaryContractAssets(entry.Prompt, entry.Schema); err != nil {
			return fmt.Errorf("%w: Standard authoring contract asset manifest stage %q: %v", ErrInvalidDeploymentOperationCatalogLock, entry.StageKey, err)
		}
		stageReferences := []StandardAuthoringContractAssetReference{entry.Prompt, entry.Schema}
		seenAdditional := make(map[standardAuthoringContractAssetIdentity]struct{}, len(entry.AdditionalSchemas))
		for additionalIndex, reference := range entry.AdditionalSchemas {
			if err := reference.Validate(); err != nil {
				return fmt.Errorf("%w: Standard authoring contract asset manifest stage %q additional schema %d: %v", ErrInvalidDeploymentOperationCatalogLock, entry.StageKey, additionalIndex, err)
			}
			identity := standardAuthoringContractAssetIdentity{id: reference.ID, version: reference.Version}
			if identity == (standardAuthoringContractAssetIdentity{id: entry.Prompt.ID, version: entry.Prompt.Version}) || identity == (standardAuthoringContractAssetIdentity{id: entry.Schema.ID, version: entry.Schema.Version}) {
				return fmt.Errorf("%w: Standard authoring contract asset manifest stage %q repeats an additional schema identity", ErrInvalidDeploymentOperationCatalogLock, entry.StageKey)
			}
			if _, duplicate := seenAdditional[identity]; duplicate {
				return fmt.Errorf("%w: Standard authoring contract asset manifest stage %q repeats an additional schema identity", ErrInvalidDeploymentOperationCatalogLock, entry.StageKey)
			}
			seenAdditional[identity] = struct{}{}
			if reference.RelativePath == entry.Prompt.RelativePath || reference.RelativePath == entry.Schema.RelativePath {
				return fmt.Errorf("%w: Standard authoring contract asset manifest stage %q repeats an additional schema path", ErrInvalidDeploymentOperationCatalogLock, entry.StageKey)
			}
			stageReferences = append(stageReferences, reference)
		}
		for _, reference := range stageReferences {
			identity := standardAuthoringContractAssetIdentity{id: reference.ID, version: reference.Version}
			if existing, present := assetIdentities[identity]; present && existing != reference.RelativePath {
				return fmt.Errorf("%w: Standard authoring asset %q@%q maps to conflicting paths", ErrInvalidDeploymentOperationCatalogLock, reference.ID, reference.Version)
			}
			assetIdentities[identity] = reference.RelativePath
			if existing, present := assetPaths[reference.RelativePath]; present && existing != identity {
				return fmt.Errorf("%w: Standard authoring asset path %q maps to conflicting identities", ErrInvalidDeploymentOperationCatalogLock, reference.RelativePath)
			}
			assetPaths[reference.RelativePath] = identity
		}
	}
	return nil
}

// CanonicalJSON returns a stable manifest representation. Manifest entry
// order is a semantic set order so source formatting cannot change a generated
// deployment lock's content identity.
func (manifest StandardAuthoringContractAssetManifest) CanonicalJSON() ([]byte, error) {
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	canonical := manifest.Clone()
	sort.Slice(canonical.Operations, func(left, right int) bool {
		return canonical.Operations[left].StageKey < canonical.Operations[right].StageKey
	})
	for index := range canonical.Operations {
		sort.Slice(canonical.Operations[index].AdditionalSchemas, func(left, right int) bool {
			return standardAuthoringContractAssetReferenceLess(canonical.Operations[index].AdditionalSchemas[left], canonical.Operations[index].AdditionalSchemas[right])
		})
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return nil, fmt.Errorf("%w: encode Standard authoring contract asset manifest: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	return encoded, nil
}

// ParseStandardAuthoringContractAssetManifestJSON strictly decodes a complete
// manifest. Unknown fields, duplicates, trailing JSON, and incomplete stage
// coverage are all rejected before a lock generator can observe asset paths.
func ParseStandardAuthoringContractAssetManifestJSON(raw []byte) (StandardAuthoringContractAssetManifest, error) {
	if err := rejectDuplicateDeploymentCatalogJSONKeys(raw); err != nil {
		return StandardAuthoringContractAssetManifest{}, fmt.Errorf("%w: decode Standard authoring contract asset manifest: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	var document standardAuthoringContractAssetManifestDocument
	if err := decodeDeploymentCatalogJSON(raw, &document); err != nil {
		return StandardAuthoringContractAssetManifest{}, fmt.Errorf("%w: decode Standard authoring contract asset manifest: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	manifest := StandardAuthoringContractAssetManifest{Format: document.Format, Version: document.Version, Template: document.Template, Operations: document.Operations}
	if err := manifest.Validate(); err != nil {
		return StandardAuthoringContractAssetManifest{}, err
	}
	return manifest.Clone(), nil
}

// UnmarshalJSON keeps direct encoding/json decoding strict.
func (manifest *StandardAuthoringContractAssetManifest) UnmarshalJSON(raw []byte) error {
	if manifest == nil {
		return fmt.Errorf("%w: nil Standard authoring contract asset manifest", ErrInvalidDeploymentOperationCatalogLock)
	}
	parsed, err := ParseStandardAuthoringContractAssetManifestJSON(raw)
	if err != nil {
		return err
	}
	*manifest = parsed
	return nil
}

type standardAuthoringContractAssetManifestDocument struct {
	Format     string                                        `json:"format"`
	Version    string                                        `json:"version"`
	Template   workflowadapter.TemplateReference             `json:"template"`
	Operations []StandardAuthoringContractAssetManifestEntry `json:"operations"`
}

// Clone returns an independently owned typed contract lock.
func (lock StandardAuthoringContractLock) Clone() StandardAuthoringContractLock {
	lock.Prompt = lock.Prompt.Clone()
	lock.Schema = lock.Schema.Clone()
	lock.AdditionalSchemas = cloneStandardAuthoringContractAdditionalSchemaLocks(lock.AdditionalSchemas)
	return lock
}

// Clone returns independently owned additional-schema lock values.
func (lock StandardAuthoringContractAdditionalSchemaLock) Clone() StandardAuthoringContractAdditionalSchemaLock {
	lock.StandardAuthoringContractAssetReference = lock.StandardAuthoringContractAssetReference.Clone()
	return lock
}

// Validate proves the extension has one exact version and two distinct,
// canonical deployment assets. Their raw SHA-256 identities live in the
// enclosing lock record and are checked by validateStandardAuthoringLock.
func (lock StandardAuthoringContractLock) Validate() error {
	if lock.Format != StandardAuthoringContractLockFormat {
		return fmt.Errorf("%w: unsupported Standard authoring contract lock format %q", ErrInvalidDeploymentOperationCatalogLock, lock.Format)
	}
	if lock.Version != StandardAuthoringContractLockVersion {
		return fmt.Errorf("%w: unsupported Standard authoring contract lock version %q", ErrInvalidDeploymentOperationCatalogLock, lock.Version)
	}
	if err := validateStandardAuthoringPrimaryContractAssets(lock.Prompt, lock.Schema); err != nil {
		return err
	}
	seenAdditional := make(map[standardAuthoringContractAssetIdentity]struct{}, len(lock.AdditionalSchemas))
	seenAdditionalPaths := make(map[string]struct{}, len(lock.AdditionalSchemas))
	for index, schema := range lock.AdditionalSchemas {
		if err := schema.Validate(); err != nil {
			return fmt.Errorf("%w: Standard authoring additional schema asset %d: %v", ErrInvalidDeploymentOperationCatalogLock, index, err)
		}
		identity := standardAuthoringContractAssetIdentity{id: schema.ID, version: schema.Version}
		if identity == (standardAuthoringContractAssetIdentity{id: lock.Prompt.ID, version: lock.Prompt.Version}) || identity == (standardAuthoringContractAssetIdentity{id: lock.Schema.ID, version: lock.Schema.Version}) {
			return fmt.Errorf("%w: Standard authoring additional schema must use a distinct asset identity", ErrInvalidDeploymentOperationCatalogLock)
		}
		if _, duplicate := seenAdditional[identity]; duplicate {
			return fmt.Errorf("%w: Standard authoring additional schema must not repeat an asset identity", ErrInvalidDeploymentOperationCatalogLock)
		}
		seenAdditional[identity] = struct{}{}
		if schema.RelativePath == lock.Prompt.RelativePath || schema.RelativePath == lock.Schema.RelativePath {
			return fmt.Errorf("%w: Standard authoring additional schema must use a distinct asset path", ErrInvalidDeploymentOperationCatalogLock)
		}
		if _, duplicate := seenAdditionalPaths[schema.RelativePath]; duplicate {
			return fmt.Errorf("%w: Standard authoring additional schema must not repeat an asset path", ErrInvalidDeploymentOperationCatalogLock)
		}
		seenAdditionalPaths[schema.RelativePath] = struct{}{}
	}
	return nil
}

// Validate proves an additional schema asset reference and its content hash
// are fully pinned.
func (lock StandardAuthoringContractAdditionalSchemaLock) Validate() error {
	if err := lock.StandardAuthoringContractAssetReference.Validate(); err != nil {
		return err
	}
	if err := lock.ContentSHA256.Validate(); err != nil {
		return fmt.Errorf("%w: Standard authoring additional schema content SHA-256: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	return nil
}

type standardAuthoringContractAssetIdentity struct {
	id      string
	version string
}

func cloneStandardAuthoringContractAssetReferences(references []StandardAuthoringContractAssetReference) []StandardAuthoringContractAssetReference {
	if references == nil {
		return nil
	}
	copied := make([]StandardAuthoringContractAssetReference, len(references))
	for index, reference := range references {
		copied[index] = reference.Clone()
	}
	return copied
}

func cloneStandardAuthoringContractAdditionalSchemaLocks(locks []StandardAuthoringContractAdditionalSchemaLock) []StandardAuthoringContractAdditionalSchemaLock {
	if locks == nil {
		return nil
	}
	copied := make([]StandardAuthoringContractAdditionalSchemaLock, len(locks))
	for index, lock := range locks {
		copied[index] = lock.Clone()
	}
	return copied
}

func standardAuthoringContractAssetReferenceLess(left, right StandardAuthoringContractAssetReference) bool {
	if left.ID != right.ID {
		return left.ID < right.ID
	}
	if left.Version != right.Version {
		return left.Version < right.Version
	}
	return left.RelativePath < right.RelativePath
}

func validateStandardAuthoringPrimaryContractAssets(prompt, schema StandardAuthoringContractAssetReference) error {
	if err := prompt.Validate(); err != nil {
		return fmt.Errorf("%w: Standard authoring prompt asset: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	if err := schema.Validate(); err != nil {
		return fmt.Errorf("%w: Standard authoring schema asset: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	if prompt.ID == schema.ID && prompt.Version == schema.Version {
		return fmt.Errorf("%w: Standard authoring prompt and schema must use distinct asset identities", ErrInvalidDeploymentOperationCatalogLock)
	}
	if prompt.RelativePath == schema.RelativePath {
		return fmt.Errorf("%w: Standard authoring prompt and schema must use distinct asset paths", ErrInvalidDeploymentOperationCatalogLock)
	}
	return nil
}

type standardAuthoringContractAssetBinding struct {
	kind         string
	relativePath string
	fingerprint  workflowkit.Fingerprint
}

// validateStandardAuthoringLockContract applies the top-level invariant for
// the closed authoring template: every operation has exactly one typed
// prompt/schema contract extension, and an asset identity/path always maps to
// exactly one raw SHA-256. Other templates must not carry this specialized
// extension, so a generic lock cannot accidentally gain Standard's execution
// semantics.
func validateStandardAuthoringLockContract(lock DeploymentOperationCatalogLock) error {
	isStandardAuthoring := workflowadapter.IsStandardAuthoringWorkflowTemplate(lock.CatalogReceipt.Template)
	if !isStandardAuthoring {
		for index, record := range lock.Operations {
			if record.StandardAuthoringContract != nil {
				return fmt.Errorf("%w: operation %d carries a Standard authoring contract for non-Standard-authoring template", ErrInvalidDeploymentOperationCatalogLock, index)
			}
		}
		return nil
	}
	if err := validateStandardAuthoringLockedTurnBudgets(lock); err != nil {
		return err
	}

	identities := make(map[standardAuthoringContractAssetIdentity]standardAuthoringContractAssetBinding, len(lock.Operations)*2)
	paths := make(map[string]standardAuthoringContractAssetIdentity, len(lock.Operations)*2)
	for index, record := range lock.Operations {
		if record.StandardAuthoringContract == nil {
			return fmt.Errorf("%w: Standard authoring operation %d is missing standard_authoring_contract", ErrInvalidDeploymentOperationCatalogLock, index)
		}
		contract := record.StandardAuthoringContract.Clone()
		if err := contract.Validate(); err != nil {
			return fmt.Errorf("%w: Standard authoring operation %d contract: %v", ErrInvalidDeploymentOperationCatalogLock, index, err)
		}
		for _, asset := range []struct {
			kind        string
			reference   StandardAuthoringContractAssetReference
			fingerprint workflowkit.Fingerprint
		}{
			{kind: "prompt", reference: contract.Prompt, fingerprint: record.PromptContentFingerprint},
			{kind: "schema", reference: contract.Schema, fingerprint: record.SchemaContentFingerprint},
		} {
			if err := recordStandardAuthoringContractAssetBinding(identities, paths, asset.kind, asset.reference, asset.fingerprint); err != nil {
				return err
			}
		}
		for _, asset := range contract.AdditionalSchemas {
			if err := recordStandardAuthoringContractAssetBinding(identities, paths, "schema", asset.StandardAuthoringContractAssetReference, asset.ContentSHA256); err != nil {
				return err
			}
		}
	}
	return nil
}

func recordStandardAuthoringContractAssetBinding(identities map[standardAuthoringContractAssetIdentity]standardAuthoringContractAssetBinding, paths map[string]standardAuthoringContractAssetIdentity, kind string, reference StandardAuthoringContractAssetReference, fingerprint workflowkit.Fingerprint) error {
	identity := standardAuthoringContractAssetIdentity{id: reference.ID, version: reference.Version}
	binding := standardAuthoringContractAssetBinding{kind: kind, relativePath: reference.RelativePath, fingerprint: fingerprint}
	if existing, present := identities[identity]; present {
		if existing != binding {
			return fmt.Errorf("%w: Standard authoring %s asset %q@%q has conflicting path or SHA-256 binding", ErrInvalidDeploymentOperationCatalogLock, kind, reference.ID, reference.Version)
		}
	} else {
		identities[identity] = binding
	}
	pathKey := kind + ":" + reference.RelativePath
	if existing, present := paths[pathKey]; present && existing != identity {
		return fmt.Errorf("%w: Standard authoring %s asset path has conflicting identity", ErrInvalidDeploymentOperationCatalogLock, kind)
	}
	paths[pathKey] = identity
	return nil
}

// validateStandardAuthoringLockedTurnBudgets keeps the deployment's three
// independent turn declarations from drifting: the catalog operation payload,
// the lock-owned profile, and the compiled template quota must all agree.
// The executor can then treat the program length as the exact bounded work,
// rather than silently running fewer turns than a newly enlarged profile.
func validateStandardAuthoringLockedTurnBudgets(lock DeploymentOperationCatalogLock) error {
	profile, err := lock.StandardAuthoringProfile()
	if err != nil {
		return err
	}
	template, err := workflowadapter.ResolveWorkflowTemplate(profile.Template)
	if err != nil {
		return fmt.Errorf("%w: resolve Standard authoring template for turn budget: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	compiled, err := template.Compile(profile)
	if err != nil {
		return fmt.Errorf("%w: compile Standard authoring profile for turn budget: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	for _, record := range lock.Operations {
		payload, isAgentTurn := record.Operation.Payload.(workflowadapter.AgentTurnOperationPayload)
		if !isAgentTurn {
			continue
		}
		definition, found := template.Catalog.Stage(record.Stage.Key)
		if !found {
			return fmt.Errorf("%w: Standard authoring agent stage %q is absent from the template", ErrInvalidDeploymentOperationCatalogLock, record.Stage.Key)
		}
		stage, found := compiled.Descriptor.Stage(record.Stage.Key)
		if !found {
			return fmt.Errorf("%w: Standard authoring agent stage %q is absent from the compiled descriptor", ErrInvalidDeploymentOperationCatalogLock, record.Stage.Key)
		}
		claimedTurns, hasClaim := standardAuthoringLockedAgentTurnClaim(stage)
		if !hasClaim || payload.MaxTurns != definition.RequiredTurns || payload.MaxTurns != stage.Budget.MaxTurns || int64(payload.MaxTurns) != claimedTurns {
			return fmt.Errorf("%w: Standard authoring agent stage %q turn declarations disagree", ErrInvalidDeploymentOperationCatalogLock, record.Stage.Key)
		}
	}
	return nil
}

func standardAuthoringLockedAgentTurnClaim(stage workflowkit.StageDescriptor) (int64, bool) {
	var units int64
	found := false
	for _, claim := range stage.QuotaClaims {
		if claim.Dimension != "agent_turn" {
			continue
		}
		if found || claim.Units <= 0 {
			return 0, false
		}
		units = claim.Units
		found = true
	}
	return units, found
}

func validateStandardAuthoringContractAssetID(value string) error {
	if err := validateOperationCatalogLockToken("Standard authoring contract asset id", value); err != nil {
		return err
	}
	first := value[0]
	if !((first >= 'a' && first <= 'z') || (first >= '0' && first <= '9')) || value != strings.ToLower(value) || strings.HasPrefix(value, ".") || strings.HasSuffix(value, ".") || strings.Contains(value, "..") {
		return fmt.Errorf("%w: Standard authoring contract asset id must be canonical lowercase token", ErrInvalidDeploymentOperationCatalogLock)
	}
	return nil
}

func validateStandardAuthoringContractAssetVersion(value string) error {
	if value != strings.TrimSpace(value) {
		return fmt.Errorf("%w: Standard authoring contract asset version must not contain leading or trailing whitespace", ErrInvalidDeploymentOperationCatalogLock)
	}
	if err := validateOperationCatalogLockVersion("Standard authoring contract asset", value); err != nil {
		return err
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '.' || character == '_' || character == '-' || character == '+' {
			continue
		}
		return fmt.Errorf("%w: Standard authoring contract asset version contains unsupported character %q", ErrInvalidDeploymentOperationCatalogLock, character)
	}
	return nil
}

func validateStandardAuthoringContractRelativePath(value string) error {
	if value == "" || value != strings.TrimSpace(value) || strings.Contains(value, "\\") || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || path.Clean(value) != value || value == "." {
		return fmt.Errorf("%w: Standard authoring contract asset path must be a clean non-root slash-relative path", ErrInvalidDeploymentOperationCatalogLock)
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || component == "." || component == ".." {
			return fmt.Errorf("%w: Standard authoring contract asset path contains traversal component", ErrInvalidDeploymentOperationCatalogLock)
		}
		for _, character := range component {
			if unicode.IsControl(character) {
				return fmt.Errorf("%w: Standard authoring contract asset path contains a control character", ErrInvalidDeploymentOperationCatalogLock)
			}
		}
	}
	return nil
}
