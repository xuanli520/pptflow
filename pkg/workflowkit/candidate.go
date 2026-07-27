package workflowkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
)

const (
	CandidateSnapshotFormat  = "workflowkit.candidate-snapshot.v1"
	CandidateSnapshotVersion = "1"

	CandidateValidationContractFormat  = "workflowkit.candidate-validation-contract.v1"
	CandidateValidationContractVersion = "1"

	ValidationReceiptFormat  = "workflowkit.validation-receipt.v1"
	ValidationReceiptVersion = "1"
)

var (
	// ErrInvalidCandidateSnapshot marks a malformed snapshot, validation
	// contract, or receipt. The three values form one immutable candidate
	// validation chain and are deliberately validated together at admission.
	ErrInvalidCandidateSnapshot = errors.New("workflowkit: invalid candidate snapshot")
)

// CandidateFile is one host-captured file in an immutable candidate manifest.
// Path is a logical slash-separated name, never an absolute workspace path.
// The content itself remains an existing content-addressed artifact object.
type CandidateFile struct {
	Path          string      `json:"path"`
	SchemaVersion string      `json:"schema_version"`
	ContentDigest Fingerprint `json:"content_digest"`
	SizeBytes     int64       `json:"size_bytes"`
}

func (file CandidateFile) validate() error {
	if err := ValidateCandidateFilePath(file.Path); err != nil {
		return err
	}
	if err := validateRequired("candidate file schema version", file.SchemaVersion, ErrInvalidCandidateSnapshot); err != nil {
		return err
	}
	if err := file.ContentDigest.Validate(); err != nil {
		return fmt.Errorf("%w: candidate file %q digest: %v", ErrInvalidCandidateSnapshot, file.Path, err)
	}
	if file.SizeBytes < 0 {
		return fmt.Errorf("%w: candidate file %q size cannot be negative", ErrInvalidCandidateSnapshot, file.Path)
	}
	return nil
}

// CandidateSnapshot v1 is a content-addressed manifest of fixed candidate
// files. It is the only candidate representation handed to critics or
// validators; neither receives the author's writable workspace path.
type CandidateSnapshot struct {
	Format  string          `json:"format"`
	Version string          `json:"version"`
	Files   []CandidateFile `json:"files"`
	Digest  Fingerprint     `json:"digest"`
}

// NewCandidateSnapshot canonicalizes a host-captured file manifest and
// attaches its aggregate content digest. It does not write a directory or
// duplicate file bytes; files continue to live in the normal artifact store.
func NewCandidateSnapshot(files []CandidateFile) (CandidateSnapshot, error) {
	snapshot := CandidateSnapshot{
		Format: CandidateSnapshotFormat, Version: CandidateSnapshotVersion,
		Files: append([]CandidateFile(nil), files...),
	}
	if snapshot.Files == nil {
		snapshot.Files = []CandidateFile{}
	}
	sort.Slice(snapshot.Files, func(left, right int) bool { return snapshot.Files[left].Path < snapshot.Files[right].Path })
	digest, err := candidateSnapshotDigest(snapshot)
	if err != nil {
		return CandidateSnapshot{}, err
	}
	snapshot.Digest = digest
	if err := snapshot.Validate(); err != nil {
		return CandidateSnapshot{}, err
	}
	return snapshot, nil
}

// Clone returns an independently owned immutable manifest value.
func (snapshot CandidateSnapshot) Clone() CandidateSnapshot {
	snapshot.Files = append([]CandidateFile(nil), snapshot.Files...)
	return snapshot
}

// Validate verifies the canonical manifest identity, file entries, ordering,
// and aggregate digest. It rejects a reordered or substituted manifest even
// when its individual file entries remain well formed.
func (snapshot CandidateSnapshot) Validate() error {
	if snapshot.Format != CandidateSnapshotFormat || snapshot.Version != CandidateSnapshotVersion {
		return fmt.Errorf("%w: unsupported candidate snapshot identity", ErrInvalidCandidateSnapshot)
	}
	if len(snapshot.Files) == 0 {
		return fmt.Errorf("%w: candidate snapshot has no files", ErrInvalidCandidateSnapshot)
	}
	previous := ""
	for _, file := range snapshot.Files {
		if err := file.validate(); err != nil {
			return err
		}
		if previous != "" && file.Path <= previous {
			return fmt.Errorf("%w: candidate files are not strictly sorted and unique", ErrInvalidCandidateSnapshot)
		}
		previous = file.Path
	}
	if err := snapshot.Digest.Validate(); err != nil {
		return fmt.Errorf("%w: candidate snapshot digest: %v", ErrInvalidCandidateSnapshot, err)
	}
	expected, err := candidateSnapshotDigest(snapshot)
	if err != nil {
		return err
	}
	if snapshot.Digest != expected {
		return fmt.Errorf("%w: candidate snapshot digest does not match its manifest", ErrInvalidCandidateSnapshot)
	}
	return nil
}

func candidateSnapshotDigest(snapshot CandidateSnapshot) (Fingerprint, error) {
	canonical := struct {
		Format  string          `json:"format"`
		Version string          `json:"version"`
		Files   []CandidateFile `json:"files"`
	}{Format: snapshot.Format, Version: snapshot.Version, Files: append([]CandidateFile(nil), snapshot.Files...)}
	sort.Slice(canonical.Files, func(left, right int) bool { return canonical.Files[left].Path < canonical.Files[right].Path })
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("%w: encode candidate snapshot: %v", ErrInvalidCandidateSnapshot, err)
	}
	return FingerprintBytes("workflowkit.candidate-snapshot.v1", encoded)
}

// ValidateCandidateFilePath accepts only a normalized logical candidate file
// name. It deliberately has no host filesystem semantics and can therefore
// be used by a host capture implementation before it resolves a workspace.
func ValidateCandidateFilePath(value string) error {
	if strings.TrimSpace(value) == "" || strings.Contains(value, "\\") || strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("%w: candidate file path %q is not a normalized relative path", ErrInvalidCandidateSnapshot, value)
	}
	if strings.HasPrefix(value, "/") || path.Clean(value) != value || value == "." || strings.HasPrefix(value, "../") {
		return fmt.Errorf("%w: candidate file path %q is not a normalized relative path", ErrInvalidCandidateSnapshot, value)
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("%w: candidate file path %q is not a normalized relative path", ErrInvalidCandidateSnapshot, value)
		}
	}
	return nil
}

// CandidateValidationContract binds host candidate verification to frozen
// runtime and verification documents. It intentionally contains only their
// content digests; directory paths and commands are host implementation
// details, not agent-controlled arguments.
type CandidateValidationContract struct {
	Format                     string      `json:"format"`
	Version                    string      `json:"version"`
	RuntimeContractDigest      Fingerprint `json:"runtime_contract_digest"`
	VerificationContractDigest Fingerprint `json:"verification_contract_digest"`
}

func NewCandidateValidationContract(runtimeDigest, verificationDigest Fingerprint) (CandidateValidationContract, error) {
	contract := CandidateValidationContract{
		Format: CandidateValidationContractFormat, Version: CandidateValidationContractVersion,
		RuntimeContractDigest: runtimeDigest, VerificationContractDigest: verificationDigest,
	}
	if err := contract.Validate(); err != nil {
		return CandidateValidationContract{}, err
	}
	return contract, nil
}

func (contract CandidateValidationContract) Validate() error {
	if contract.Format != CandidateValidationContractFormat || contract.Version != CandidateValidationContractVersion {
		return fmt.Errorf("%w: unsupported candidate validation contract identity", ErrInvalidCandidateSnapshot)
	}
	for label, digest := range map[string]Fingerprint{
		"runtime contract":      contract.RuntimeContractDigest,
		"verification contract": contract.VerificationContractDigest,
	} {
		if err := digest.Validate(); err != nil {
			return fmt.Errorf("%w: %s digest: %v", ErrInvalidCandidateSnapshot, label, err)
		}
	}
	return nil
}

// Fingerprint returns the immutable identity that a validation receipt binds.
func (contract CandidateValidationContract) Fingerprint() (Fingerprint, error) {
	if err := contract.Validate(); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(contract)
	if err != nil {
		return "", fmt.Errorf("%w: encode candidate validation contract: %v", ErrInvalidCandidateSnapshot, err)
	}
	return FingerprintBytes("workflowkit.candidate-validation-contract.v1", encoded)
}

// ValidationVerdict is the closed result of host-owned candidate validation.
type ValidationVerdict string

const (
	ValidationPass   ValidationVerdict = "pass"
	ValidationReject ValidationVerdict = "reject"
)

func (verdict ValidationVerdict) valid() bool {
	return verdict == ValidationPass || verdict == ValidationReject
}

// ValidationReceipt v1 records a validator's bounded, structured result.
// Diagnostics use the same restricted, pre-redacted command report contract
// as AgentAttemptReport, so a receipt cannot become a raw-log persistence
// channel.
type ValidationReceipt struct {
	Format         string               `json:"format"`
	Version        string               `json:"version"`
	SnapshotDigest Fingerprint          `json:"snapshot_digest"`
	ContractDigest Fingerprint          `json:"contract_digest"`
	Verdict        ValidationVerdict    `json:"verdict"`
	FailureCode    AgentFailureCode     `json:"failure_code,omitempty"`
	Diagnostics    []AgentCommandReport `json:"diagnostics"`
	IssuedAt       time.Time            `json:"issued_at"`
	ExpiresAt      time.Time            `json:"expires_at"`
	Digest         Fingerprint          `json:"digest"`
}

// NewValidationReceipt canonicalizes and signs a host-generated receipt.
func NewValidationReceipt(receipt ValidationReceipt) (ValidationReceipt, error) {
	receipt.Format = ValidationReceiptFormat
	receipt.Version = ValidationReceiptVersion
	receipt.Diagnostics = append([]AgentCommandReport(nil), receipt.Diagnostics...)
	if receipt.Diagnostics == nil {
		receipt.Diagnostics = []AgentCommandReport{}
	}
	receipt.IssuedAt = receipt.IssuedAt.UTC()
	receipt.ExpiresAt = receipt.ExpiresAt.UTC()
	digest, err := validationReceiptDigest(receipt)
	if err != nil {
		return ValidationReceipt{}, err
	}
	receipt.Digest = digest
	if err := receipt.Validate(); err != nil {
		return ValidationReceipt{}, err
	}
	return receipt, nil
}

// Clone returns an independently owned receipt.
func (receipt ValidationReceipt) Clone() ValidationReceipt {
	receipt.Diagnostics = append([]AgentCommandReport(nil), receipt.Diagnostics...)
	return receipt
}

// Validate verifies receipt identity and its binding to a particular snapshot
// and frozen verification contract. Use ValidateAt to additionally reject an
// expired or future-dated receipt at a scheduling/admission boundary.
func (receipt ValidationReceipt) Validate() error {
	if receipt.Format != ValidationReceiptFormat || receipt.Version != ValidationReceiptVersion {
		return fmt.Errorf("%w: unsupported validation receipt identity", ErrInvalidCandidateSnapshot)
	}
	for label, digest := range map[string]Fingerprint{"snapshot": receipt.SnapshotDigest, "contract": receipt.ContractDigest, "receipt": receipt.Digest} {
		if err := digest.Validate(); err != nil {
			return fmt.Errorf("%w: validation receipt %s digest: %v", ErrInvalidCandidateSnapshot, label, err)
		}
	}
	if !receipt.Verdict.valid() {
		return fmt.Errorf("%w: unsupported validation verdict %q", ErrInvalidCandidateSnapshot, receipt.Verdict)
	}
	if !receipt.FailureCode.valid() {
		return fmt.Errorf("%w: unsupported validation failure code %q", ErrInvalidCandidateSnapshot, receipt.FailureCode)
	}
	if receipt.Verdict == ValidationPass && receipt.FailureCode != AgentFailureNone {
		return fmt.Errorf("%w: passing validation receipt cannot have a failure code", ErrInvalidCandidateSnapshot)
	}
	if receipt.Verdict == ValidationReject && receipt.FailureCode == AgentFailureNone {
		return fmt.Errorf("%w: rejected validation receipt requires a failure code", ErrInvalidCandidateSnapshot)
	}
	if receipt.IssuedAt.IsZero() || receipt.ExpiresAt.IsZero() || !receipt.ExpiresAt.After(receipt.IssuedAt) {
		return fmt.Errorf("%w: validation receipt lifetime is invalid", ErrInvalidCandidateSnapshot)
	}
	commandIDs := make(map[string]struct{}, len(receipt.Diagnostics))
	for _, diagnostic := range receipt.Diagnostics {
		if err := diagnostic.validate(); err != nil {
			return err
		}
		if _, exists := commandIDs[diagnostic.CommandID]; exists {
			return fmt.Errorf("%w: duplicate validation diagnostic %q", ErrInvalidCandidateSnapshot, diagnostic.CommandID)
		}
		commandIDs[diagnostic.CommandID] = struct{}{}
	}
	expected, err := validationReceiptDigest(receipt)
	if err != nil {
		return err
	}
	if receipt.Digest != expected {
		return fmt.Errorf("%w: validation receipt digest does not match its content", ErrInvalidCandidateSnapshot)
	}
	return nil
}

// ValidateAt rejects receipts that are not current. This makes a receipt from
// an earlier snapshot, contract, or expired validator window unusable at a
// repair or downstream critic admission boundary.
func (receipt ValidationReceipt) ValidateAt(now time.Time) error {
	if err := receipt.Validate(); err != nil {
		return err
	}
	now = now.UTC()
	if now.Before(receipt.IssuedAt) || !now.Before(receipt.ExpiresAt) {
		return fmt.Errorf("%w: validation receipt is not current", ErrInvalidCandidateSnapshot)
	}
	return nil
}

func validationReceiptDigest(receipt ValidationReceipt) (Fingerprint, error) {
	canonical := struct {
		Format         string               `json:"format"`
		Version        string               `json:"version"`
		SnapshotDigest Fingerprint          `json:"snapshot_digest"`
		ContractDigest Fingerprint          `json:"contract_digest"`
		Verdict        ValidationVerdict    `json:"verdict"`
		FailureCode    AgentFailureCode     `json:"failure_code,omitempty"`
		Diagnostics    []AgentCommandReport `json:"diagnostics"`
		IssuedAt       time.Time            `json:"issued_at"`
		ExpiresAt      time.Time            `json:"expires_at"`
	}{
		Format: receipt.Format, Version: receipt.Version, SnapshotDigest: receipt.SnapshotDigest,
		ContractDigest: receipt.ContractDigest, Verdict: receipt.Verdict, FailureCode: receipt.FailureCode,
		Diagnostics: append([]AgentCommandReport(nil), receipt.Diagnostics...), IssuedAt: receipt.IssuedAt.UTC(), ExpiresAt: receipt.ExpiresAt.UTC(),
	}
	sort.Slice(canonical.Diagnostics, func(left, right int) bool {
		return canonical.Diagnostics[left].CommandID < canonical.Diagnostics[right].CommandID
	})
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("%w: encode validation receipt: %v", ErrInvalidCandidateSnapshot, err)
	}
	return FingerprintBytes("workflowkit.validation-receipt.v1", encoded)
}

// CandidateValidator is a host-owned validation boundary. A validator accepts
// only an immutable snapshot and frozen contract, never agent-supplied paths,
// commands, environment values, or workspace handles.
type CandidateValidator interface {
	ValidateCandidate(context.Context, CandidateSnapshot, CandidateValidationContract) (ValidationReceipt, error)
}

// ValidateCandidateReceipt verifies that a validator response binds exactly
// the supplied snapshot and contract and is current at the caller's admission
// time. A critic or repair planner must call this before trusting a snapshot.
func ValidateCandidateReceipt(snapshot CandidateSnapshot, contract CandidateValidationContract, receipt ValidationReceipt, now time.Time) error {
	if err := snapshot.Validate(); err != nil {
		return err
	}
	if err := contract.Validate(); err != nil {
		return err
	}
	if err := receipt.ValidateAt(now); err != nil {
		return err
	}
	contractDigest, err := contract.Fingerprint()
	if err != nil {
		return err
	}
	if receipt.SnapshotDigest != snapshot.Digest || receipt.ContractDigest != contractDigest {
		return fmt.Errorf("%w: validation receipt does not bind the supplied snapshot and contract", ErrInvalidCandidateSnapshot)
	}
	return nil
}
