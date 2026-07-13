// Package workflowruntime provides durable, storage-backed implementations
// for the domain-neutral contracts in pkg/workflowkit.
package workflowruntime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	// ObjectAlgorithm is the only content-addressing algorithm accepted by the
	// local immutable object store.
	ObjectAlgorithm = "sha256"

	// ArtifactManifestFormat identifies the canonical JSON representation of
	// ArtifactManifest. A new incompatible representation must use a new value.
	ArtifactManifestFormat = "workflowruntime.artifact-manifest.v1"
)

var (
	// ErrInvalidObjectRef marks an object reference that cannot be mapped to a
	// safe, canonical content-addressed path.
	ErrInvalidObjectRef = errors.New("workflowruntime: invalid object reference")
	// ErrInvalidArtifactManifest marks incomplete or inconsistent immutable
	// artifact lineage metadata.
	ErrInvalidArtifactManifest = errors.New("workflowruntime: invalid artifact manifest")
)

// ArtifactState is a lifecycle projection for immutable artifact metadata.
// The underlying content object is never modified for any state.
type ArtifactState = workflowkit.ArtifactState

const (
	ArtifactActive      = workflowkit.ArtifactActive
	ArtifactSuperseded  = workflowkit.ArtifactSuperseded
	ArtifactQuarantined = workflowkit.ArtifactQuarantined
)

// ObjectRef identifies one immutable byte object. Digest is always a
// canonical sha256 fingerprint; SizeBytes lets callers reject accidental
// truncation before accepting a reference.
type ObjectRef struct {
	Digest    workflowkit.Fingerprint `json:"digest"`
	SizeBytes int64                   `json:"size_bytes"`
}

// Validate confirms that this reference is safe to resolve and has a
// canonical SHA-256 digest.
func (reference ObjectRef) Validate() error {
	if err := reference.Digest.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidObjectRef, err)
	}
	if reference.SizeBytes < 0 {
		return fmt.Errorf("%w: object size cannot be negative", ErrInvalidObjectRef)
	}
	return nil
}

// Clone returns an independent object reference value.
func (reference ObjectRef) Clone() ObjectRef { return reference }

// ArtifactManifest is immutable, reference-friendly metadata for one
// produced artifact. It intentionally contains no content bytes or mutable
// filesystem paths: Object identifies the immutable payload and Artifact
// carries the workflow lineage needed for reuse and audit.
//
// Construct manifests with NewArtifactManifest. Callers that need to project a
// different lifecycle state must derive a new manifest with WithState rather
// than mutate a previously stored value.
type ArtifactManifest struct {
	Format   string                  `json:"format"`
	Artifact workflowkit.ArtifactRef `json:"artifact"`
	Object   ObjectRef               `json:"object"`
}

// NewArtifactManifest returns a canonical, validated immutable manifest.
// Input bindings are sorted by name and timestamps are normalized to UTC so
// semantically equivalent manifests serialize identically.
func NewArtifactManifest(artifact workflowkit.ArtifactRef, object ObjectRef) (ArtifactManifest, error) {
	manifest := ArtifactManifest{
		Format:   ArtifactManifestFormat,
		Artifact: canonicalArtifactRef(artifact),
		Object:   object.Clone(),
	}
	if err := manifest.Validate(); err != nil {
		return ArtifactManifest{}, err
	}
	return manifest, nil
}

// Clone returns an independent manifest snapshot. It is safe to retain the
// returned value while a caller derives another manifest state.
func (manifest ArtifactManifest) Clone() ArtifactManifest {
	manifest.Artifact = manifest.Artifact.Clone()
	manifest.Object = manifest.Object.Clone()
	return manifest
}

// Validate verifies the manifest's fixed format, workflow lineage, and the
// one-to-one relationship between its content object and artifact digest.
func (manifest ArtifactManifest) Validate() error {
	if manifest.Format != ArtifactManifestFormat {
		return fmt.Errorf("%w: unsupported format %q", ErrInvalidArtifactManifest, manifest.Format)
	}
	if err := manifest.Artifact.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidArtifactManifest, err)
	}
	if err := manifest.Object.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidArtifactManifest, err)
	}
	if manifest.Artifact.ContentDigest != manifest.Object.Digest {
		return fmt.Errorf("%w: artifact content digest and object digest differ", ErrInvalidArtifactManifest)
	}
	return nil
}

// WithState derives a new immutable manifest with the requested lifecycle
// projection. The payload object and all lineage remain unchanged.
func (manifest ArtifactManifest) WithState(state ArtifactState) (ArtifactManifest, error) {
	updated := manifest.Clone()
	updated.Artifact.State = state
	return NewArtifactManifest(updated.Artifact, updated.Object)
}

// MarshalCanonicalJSON serializes a validated manifest into deterministic,
// reference-only JSON suitable for storage as another immutable object.
func (manifest ArtifactManifest) MarshalCanonicalJSON() ([]byte, error) {
	canonical, err := NewArtifactManifest(manifest.Artifact, manifest.Object)
	if err != nil {
		return nil, err
	}
	return json.Marshal(canonical)
}

// ParseArtifactManifest accepts only the exact canonical JSON representation
// emitted by MarshalCanonicalJSON. Rejecting alternate ordering, whitespace,
// and unknown fields prevents a manifest reference from acquiring ambiguous
// serialized meaning over time.
func ParseArtifactManifest(encoded []byte) (ArtifactManifest, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var decoded ArtifactManifest
	if err := decoder.Decode(&decoded); err != nil {
		return ArtifactManifest{}, fmt.Errorf("%w: decode JSON: %v", ErrInvalidArtifactManifest, err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return ArtifactManifest{}, err
	}
	canonical, err := NewArtifactManifest(decoded.Artifact, decoded.Object)
	if err != nil {
		return ArtifactManifest{}, err
	}
	if decoded.Format != canonical.Format {
		return ArtifactManifest{}, fmt.Errorf("%w: unsupported format %q", ErrInvalidArtifactManifest, decoded.Format)
	}
	canonicalJSON, err := canonical.MarshalCanonicalJSON()
	if err != nil {
		return ArtifactManifest{}, err
	}
	if !bytes.Equal(encoded, canonicalJSON) {
		return ArtifactManifest{}, fmt.Errorf("%w: JSON is not canonical", ErrInvalidArtifactManifest)
	}
	return canonical, nil
}

// ManifestRef points to a serialized ArtifactManifest object without copying
// its content. It is the value a control plane can persist in rows, plans, or
// append-only events.
type ManifestRef struct {
	ArtifactID workflowkit.ArtifactID `json:"artifact_id"`
	Manifest   ObjectRef              `json:"manifest"`
}

// Validate confirms that a manifest reference can be followed safely.
func (reference ManifestRef) Validate() error {
	if err := validateIdentifier("artifact id", string(reference.ArtifactID)); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidArtifactManifest, err)
	}
	if err := reference.Manifest.Validate(); err != nil {
		return fmt.Errorf("%w: manifest object: %v", ErrInvalidArtifactManifest, err)
	}
	return nil
}

// Clone returns an independent manifest reference value.
func (reference ManifestRef) Clone() ManifestRef {
	reference.Manifest = reference.Manifest.Clone()
	return reference
}

// Reference creates a durable pointer to a serialized version of this
// manifest. The supplied object must be the canonical manifest JSON object.
func (manifest ArtifactManifest) Reference(serialized ObjectRef) (ManifestRef, error) {
	if err := manifest.Validate(); err != nil {
		return ManifestRef{}, err
	}
	reference := ManifestRef{ArtifactID: manifest.Artifact.ID, Manifest: serialized.Clone()}
	if err := reference.Validate(); err != nil {
		return ManifestRef{}, err
	}
	return reference, nil
}

func canonicalArtifactRef(reference workflowkit.ArtifactRef) workflowkit.ArtifactRef {
	canonical := reference.Clone()
	canonical.CreatedAt = canonical.CreatedAt.UTC()
	sort.Slice(canonical.InputBindings, func(left, right int) bool {
		return canonical.InputBindings[left].Name < canonical.InputBindings[right].Name
	})
	return canonical
}

func validateIdentifier(label, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", label)
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return fmt.Errorf("%s contains a control character", label)
		}
	}
	return nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: decode trailing JSON: %v", ErrInvalidArtifactManifest, err)
	}
	return fmt.Errorf("%w: unexpected trailing JSON value", ErrInvalidArtifactManifest)
}
