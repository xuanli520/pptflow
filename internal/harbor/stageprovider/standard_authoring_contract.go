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
// standard_authoring_contract in a deployment operation lock. Prompt and
// Schema map named deployment assets to the pre-existing
// PromptContentFingerprint and SchemaContentFingerprint of the enclosing
// record respectively. Duplicating hashes here would create two authorities;
// the lock record remains the one authoritative content binding.
type StandardAuthoringContractLock struct {
	Format  string                                  `json:"format"`
	Version string                                  `json:"version"`
	Prompt  StandardAuthoringContractAssetReference `json:"prompt"`
	Schema  StandardAuthoringContractAssetReference `json:"schema"`
}

// StandardAuthoringContractAssetManifestEntry selects the two immutable
// deployment assets for one exact Standard authoring stage. It contains no
// arbitrary configuration and no workspace path; entries are expanded into
// typed lock extensions only by the deterministic lock generator.
type StandardAuthoringContractAssetManifestEntry struct {
	StageKey workflowkit.StageKey                    `json:"stage_key"`
	Prompt   StandardAuthoringContractAssetReference `json:"prompt"`
	Schema   StandardAuthoringContractAssetReference `json:"schema"`
}

// Clone returns independently owned manifest entry values.
func (entry StandardAuthoringContractAssetManifestEntry) Clone() StandardAuthoringContractAssetManifestEntry {
	entry.Prompt = entry.Prompt.Clone()
	entry.Schema = entry.Schema.Clone()
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
	if !manifest.Template.Equal(workflowadapter.StandardAuthoringTemplateReference()) {
		return fmt.Errorf("%w: Standard authoring contract asset manifest must bind %s@%s", ErrInvalidDeploymentOperationCatalogLock, workflowadapter.StandardAuthoringWorkflowTemplateID, workflowadapter.StandardAuthoringWorkflowTemplateVersion)
	}
	if manifest.Operations == nil {
		return fmt.Errorf("%w: Standard authoring contract asset manifest operations must be an explicit array", ErrInvalidDeploymentOperationCatalogLock)
	}
	expected := make(map[workflowkit.StageKey]struct{}, len(workflowadapter.StandardAuthoringStageOrder()))
	for _, stageKey := range workflowadapter.StandardAuthoringStageOrder() {
		expected[stageKey] = struct{}{}
	}
	if len(manifest.Operations) != len(expected) {
		return fmt.Errorf("%w: Standard authoring contract asset manifest has %d operations, want %d", ErrInvalidDeploymentOperationCatalogLock, len(manifest.Operations), len(expected))
	}
	seenStages := make(map[workflowkit.StageKey]struct{}, len(manifest.Operations))
	assetIdentities := make(map[standardAuthoringContractAssetIdentity]string, len(manifest.Operations)*2)
	assetPaths := make(map[string]standardAuthoringContractAssetIdentity, len(manifest.Operations)*2)
	for index, entry := range manifest.Operations {
		if _, present := expected[entry.StageKey]; !present {
			return fmt.Errorf("%w: Standard authoring contract asset manifest operation %d has unknown stage %q", ErrInvalidDeploymentOperationCatalogLock, index, entry.StageKey)
		}
		if _, duplicate := seenStages[entry.StageKey]; duplicate {
			return fmt.Errorf("%w: Standard authoring contract asset manifest has duplicate stage %q", ErrInvalidDeploymentOperationCatalogLock, entry.StageKey)
		}
		seenStages[entry.StageKey] = struct{}{}
		contract := StandardAuthoringContractLock{Format: StandardAuthoringContractLockFormat, Version: StandardAuthoringContractLockVersion, Prompt: entry.Prompt, Schema: entry.Schema}
		if err := contract.Validate(); err != nil {
			return fmt.Errorf("%w: Standard authoring contract asset manifest stage %q: %v", ErrInvalidDeploymentOperationCatalogLock, entry.StageKey, err)
		}
		for _, reference := range []StandardAuthoringContractAssetReference{entry.Prompt, entry.Schema} {
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
	if err := lock.Prompt.Validate(); err != nil {
		return fmt.Errorf("%w: Standard authoring prompt asset: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	if err := lock.Schema.Validate(); err != nil {
		return fmt.Errorf("%w: Standard authoring schema asset: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	if lock.Prompt.ID == lock.Schema.ID && lock.Prompt.Version == lock.Schema.Version {
		return fmt.Errorf("%w: Standard authoring prompt and schema must use distinct asset identities", ErrInvalidDeploymentOperationCatalogLock)
	}
	if lock.Prompt.RelativePath == lock.Schema.RelativePath {
		return fmt.Errorf("%w: Standard authoring prompt and schema must use distinct asset paths", ErrInvalidDeploymentOperationCatalogLock)
	}
	return nil
}

type standardAuthoringContractAssetIdentity struct {
	id      string
	version string
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
	isStandardAuthoring := lock.CatalogReceipt.Template.Equal(workflowadapter.StandardAuthoringTemplateReference())
	if !isStandardAuthoring {
		for index, record := range lock.Operations {
			if record.StandardAuthoringContract != nil {
				return fmt.Errorf("%w: operation %d carries a Standard authoring contract for non-Standard-authoring template", ErrInvalidDeploymentOperationCatalogLock, index)
			}
		}
		return nil
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
			identity := standardAuthoringContractAssetIdentity{id: asset.reference.ID, version: asset.reference.Version}
			binding := standardAuthoringContractAssetBinding{kind: asset.kind, relativePath: asset.reference.RelativePath, fingerprint: asset.fingerprint}
			if existing, present := identities[identity]; present {
				if existing != binding {
					return fmt.Errorf("%w: Standard authoring %s asset %q@%q has conflicting path or SHA-256 binding", ErrInvalidDeploymentOperationCatalogLock, asset.kind, asset.reference.ID, asset.reference.Version)
				}
			} else {
				identities[identity] = binding
			}
			pathKey := asset.kind + ":" + asset.reference.RelativePath
			if existing, present := paths[pathKey]; present && existing != identity {
				return fmt.Errorf("%w: Standard authoring %s asset path has conflicting identity", ErrInvalidDeploymentOperationCatalogLock, asset.kind)
			}
			paths[pathKey] = identity
		}
	}
	return nil
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
