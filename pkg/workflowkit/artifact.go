package workflowkit

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

const fingerprintPrefix = "sha256:"

// SHA256Fingerprint returns the canonical fingerprint of raw content.
func SHA256Fingerprint(content []byte) Fingerprint {
	sum := sha256.Sum256(content)
	return Fingerprint(fingerprintPrefix + hex.EncodeToString(sum[:]))
}

// Validate verifies the canonical sha256:<lowercase-hex> representation.
func (fingerprint Fingerprint) Validate() error {
	value := string(fingerprint)
	if !strings.HasPrefix(value, fingerprintPrefix) {
		return fmt.Errorf("%w: fingerprint %q must use sha256", ErrInvalidArtifact, value)
	}
	hexValue := strings.TrimPrefix(value, fingerprintPrefix)
	if len(hexValue) != sha256.Size*2 {
		return fmt.Errorf("%w: fingerprint %q has an invalid SHA-256 length", ErrInvalidArtifact, value)
	}
	decoded, err := hex.DecodeString(hexValue)
	if err != nil || len(decoded) != sha256.Size || strings.ToLower(hexValue) != hexValue {
		return fmt.Errorf("%w: fingerprint %q is not canonical lowercase hexadecimal", ErrInvalidArtifact, value)
	}
	return nil
}

// FingerprintBytes derives a domain-separated fingerprint from raw bytes. The
// domain is length-prefixed so no concatenation ambiguity can affect a frozen
// manifest or plan fingerprint.
func FingerprintBytes(domain string, content []byte) (Fingerprint, error) {
	if err := validateRequired("fingerprint domain", domain, ErrInvalidArtifact); err != nil {
		return "", err
	}
	var encoded bytes.Buffer
	writeFingerprintPart(&encoded, []byte(domain))
	writeFingerprintPart(&encoded, content)
	return SHA256Fingerprint(encoded.Bytes()), nil
}

// FingerprintPart is a named raw component in a canonical fingerprint.
type FingerprintPart struct {
	Name  string
	Value []byte
}

// Validate verifies either a bare SHA-256 digest or a versioned domain digest
// in the canonical <lowercase-domain>:sha256:<lowercase-hex> form. Subject
// identity belongs to a domain adapter, so it must not be forced into the
// object-store fingerprint namespace.
func (digest SubjectDigest) Validate() error {
	value := string(digest)
	if strings.HasPrefix(value, fingerprintPrefix) {
		return Fingerprint(value).Validate()
	}
	separator := ":sha256:"
	position := strings.Index(value, separator)
	if position <= 0 || position+len(separator) >= len(value) || strings.Count(value, separator) != 1 {
		return fmt.Errorf("%w: subject digest %q must use sha256 or a versioned sha256 scheme", ErrInvalidArtifact, value)
	}
	namespace := value[:position]
	for _, character := range namespace {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '.' && character != '_' && character != '-' {
			return fmt.Errorf("%w: subject digest namespace %q is not canonical", ErrInvalidArtifact, namespace)
		}
	}
	return Fingerprint(fingerprintPrefix + value[position+len(separator):]).Validate()
}

// FingerprintParts creates a domain-separated fingerprint for named fields.
// Field order is ignored but duplicate names are rejected.
func FingerprintParts(domain string, parts []FingerprintPart) (Fingerprint, error) {
	if err := validateRequired("fingerprint domain", domain, ErrInvalidArtifact); err != nil {
		return "", err
	}
	copyParts := make([]FingerprintPart, len(parts))
	for index, part := range parts {
		if err := validateRequired("fingerprint part name", part.Name, ErrInvalidArtifact); err != nil {
			return "", err
		}
		copyParts[index] = FingerprintPart{Name: part.Name, Value: append([]byte(nil), part.Value...)}
	}
	sort.Slice(copyParts, func(left, right int) bool { return copyParts[left].Name < copyParts[right].Name })
	for index := 1; index < len(copyParts); index++ {
		if copyParts[index-1].Name == copyParts[index].Name {
			return "", fmt.Errorf("%w: duplicate fingerprint part %q", ErrInvalidArtifact, copyParts[index].Name)
		}
	}
	var encoded bytes.Buffer
	writeFingerprintPart(&encoded, []byte(domain))
	writeUint64(&encoded, uint64(len(copyParts)))
	for _, part := range copyParts {
		writeFingerprintPart(&encoded, []byte(part.Name))
		writeFingerprintPart(&encoded, part.Value)
	}
	return SHA256Fingerprint(encoded.Bytes()), nil
}

func writeFingerprintPart(buffer *bytes.Buffer, value []byte) {
	writeUint64(buffer, uint64(len(value)))
	_, _ = buffer.Write(value)
}

func writeUint64(buffer *bytes.Buffer, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = buffer.Write(encoded[:])
}

// ArtifactBinding ties a typed input name to immutable artifact content. The
// artifact ID remains available for audit and retrieval; its content digest is
// what contributes to an input fingerprint.
type ArtifactBinding struct {
	Name          string      `json:"name"`
	ArtifactID    ArtifactID  `json:"artifact_id"`
	ContentDigest Fingerprint `json:"content_digest"`
	SchemaVersion string      `json:"schema_version"`
}

// Clone returns an independent binding value.
func (binding ArtifactBinding) Clone() ArtifactBinding { return binding }

// Validate verifies a complete immutable artifact binding.
func (binding ArtifactBinding) Validate() error {
	if err := validateRequired("artifact binding name", binding.Name, ErrInvalidArtifact); err != nil {
		return err
	}
	if err := validateRequired("artifact binding id", string(binding.ArtifactID), ErrInvalidArtifact); err != nil {
		return err
	}
	if err := binding.ContentDigest.Validate(); err != nil {
		return err
	}
	if err := validateRequired("artifact binding schema version", binding.SchemaVersion, ErrInvalidArtifact); err != nil {
		return err
	}
	return nil
}

// FingerprintArtifactBindings computes a stable input fingerprint. Binding
// order and artifact IDs do not affect the result; input name, schema, and
// immutable content do.
func FingerprintArtifactBindings(bindings []ArtifactBinding) (Fingerprint, error) {
	copyBindings := append([]ArtifactBinding(nil), bindings...)
	sort.Slice(copyBindings, func(left, right int) bool { return copyBindings[left].Name < copyBindings[right].Name })
	parts := make([]FingerprintPart, 0, len(copyBindings))
	for _, binding := range copyBindings {
		if err := binding.Validate(); err != nil {
			return "", err
		}
		parts = append(parts, FingerprintPart{
			Name:  binding.Name,
			Value: []byte(binding.SchemaVersion + "\x00" + string(binding.ContentDigest)),
		})
	}
	return FingerprintParts("workflowkit.artifact-input.v1", parts)
}

// ArtifactState is an immutable artifact's lifecycle projection. Immutable
// bytes are never overwritten; superseded and quarantined artifacts remain
// auditable but cannot be reused as active evidence.
type ArtifactState string

const (
	ArtifactActive      ArtifactState = "active"
	ArtifactSuperseded  ArtifactState = "superseded"
	ArtifactQuarantined ArtifactState = "quarantined"
)

func (state ArtifactState) valid() bool {
	switch state {
	case ArtifactActive, ArtifactSuperseded, ArtifactQuarantined:
		return true
	default:
		return false
	}
}

// ArtifactRef is immutable metadata and lineage for a produced artifact.
type ArtifactRef struct {
	ID                  ArtifactID        `json:"id"`
	ContentDigest       Fingerprint       `json:"content_digest"`
	SchemaVersion       string            `json:"schema_version"`
	RunID               string            `json:"run_id"`
	StageKey            StageKey          `json:"stage_key"`
	AttemptID           AttemptID         `json:"attempt_id"`
	TurnOrdinal         int               `json:"turn_ordinal"`
	WorkflowFingerprint Fingerprint       `json:"workflow_fingerprint"`
	SubjectRevisionID   string            `json:"subject_revision_id"`
	SubjectDigest       SubjectDigest     `json:"subject_digest"`
	InputBindings       []ArtifactBinding `json:"input_bindings"`
	InputFingerprint    Fingerprint       `json:"input_fingerprint"`
	ProducerVersion     string            `json:"producer_version"`
	CreatedAt           time.Time         `json:"created_at"`
	State               ArtifactState     `json:"state"`
}

// Clone returns an independent immutable-metadata snapshot.
func (reference ArtifactRef) Clone() ArtifactRef {
	reference.InputBindings = append([]ArtifactBinding(nil), reference.InputBindings...)
	return reference
}

// Reusable reports whether this artifact can participate in a preserve plan.
func (reference ArtifactRef) Reusable() bool {
	return reference.State == ArtifactActive
}

// Validate verifies complete V2 artifact lineage. The input fingerprint must
// correspond exactly to the typed immutable input bindings, including the
// valid empty-input fingerprint for source stages.
func (reference ArtifactRef) Validate() error {
	if err := validateRequired("artifact id", string(reference.ID), ErrInvalidArtifact); err != nil {
		return err
	}
	if err := reference.ContentDigest.Validate(); err != nil {
		return err
	}
	if err := validateRequired("artifact schema version", reference.SchemaVersion, ErrInvalidArtifact); err != nil {
		return err
	}
	if err := validateRequired("artifact run id", reference.RunID, ErrInvalidArtifact); err != nil {
		return err
	}
	if err := validateRequired("artifact stage key", string(reference.StageKey), ErrInvalidArtifact); err != nil {
		return err
	}
	if err := validateRequired("artifact attempt id", string(reference.AttemptID), ErrInvalidArtifact); err != nil {
		return err
	}
	if reference.TurnOrdinal < 0 {
		return fmt.Errorf("%w: artifact turn ordinal cannot be negative", ErrInvalidArtifact)
	}
	if err := reference.WorkflowFingerprint.Validate(); err != nil {
		return err
	}
	if err := validateRequired("artifact subject revision id", reference.SubjectRevisionID, ErrInvalidArtifact); err != nil {
		return err
	}
	if err := reference.SubjectDigest.Validate(); err != nil {
		return err
	}
	if err := reference.InputFingerprint.Validate(); err != nil {
		return err
	}
	if err := validateRequired("artifact producer version", reference.ProducerVersion, ErrInvalidArtifact); err != nil {
		return err
	}
	if reference.CreatedAt.IsZero() {
		return fmt.Errorf("%w: artifact created at is required", ErrInvalidArtifact)
	}
	if !reference.State.valid() {
		return fmt.Errorf("%w: invalid artifact state %q", ErrInvalidArtifact, reference.State)
	}
	expected, err := FingerprintArtifactBindings(reference.InputBindings)
	if err != nil {
		return err
	}
	if expected != reference.InputFingerprint {
		return fmt.Errorf("%w: artifact input fingerprint does not match its bindings", ErrInvalidArtifact)
	}
	return nil
}
