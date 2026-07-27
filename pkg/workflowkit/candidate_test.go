package workflowkit

import (
	"errors"
	"testing"
	"time"
)

func TestCandidateSnapshotCanonicalizesManifestAndRejectsTampering(t *testing.T) {
	first := candidateTestFile("solution/solve.sh", "solve")
	second := candidateTestFile("tests/test.sh", "tests")
	snapshot, err := NewCandidateSnapshot([]CandidateFile{second, first})
	if err != nil {
		t.Fatalf("create candidate snapshot: %v", err)
	}
	if got, want := snapshot.Files[0].Path, first.Path; got != want {
		t.Fatalf("canonical candidate file order = %q, want %q", got, want)
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("validate candidate snapshot: %v", err)
	}

	tampered := snapshot.Clone()
	tampered.Files[0].ContentDigest = SHA256Fingerprint([]byte("substituted"))
	if err := tampered.Validate(); !errors.Is(err, ErrInvalidCandidateSnapshot) {
		t.Fatalf("tampered candidate error = %v, want ErrInvalidCandidateSnapshot", err)
	}
	if _, err := NewCandidateSnapshot([]CandidateFile{candidateTestFile("../escape", "bad")}); !errors.Is(err, ErrInvalidCandidateSnapshot) {
		t.Fatalf("unsafe candidate path error = %v, want ErrInvalidCandidateSnapshot", err)
	}
}

func TestValidationReceiptBindsSnapshotContractAndFreshness(t *testing.T) {
	now := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	snapshot, err := NewCandidateSnapshot([]CandidateFile{candidateTestFile("solution/solve.sh", "solve")})
	if err != nil {
		t.Fatalf("create candidate snapshot: %v", err)
	}
	contract, err := NewCandidateValidationContract(SHA256Fingerprint([]byte("runtime")), SHA256Fingerprint([]byte("verification")))
	if err != nil {
		t.Fatalf("create validation contract: %v", err)
	}
	contractDigest, err := contract.Fingerprint()
	if err != nil {
		t.Fatalf("fingerprint validation contract: %v", err)
	}
	receipt, err := NewValidationReceipt(ValidationReceipt{
		SnapshotDigest: snapshot.Digest, ContractDigest: contractDigest, Verdict: ValidationPass,
		Diagnostics: []AgentCommandReport{{CommandID: "oracle_verify", ExitCode: 0, TestStarted: true}},
		IssuedAt:    now, ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("create validation receipt: %v", err)
	}
	if err := ValidateCandidateReceipt(snapshot, contract, receipt, now.Add(time.Minute)); err != nil {
		t.Fatalf("validate matching receipt: %v", err)
	}
	if err := ValidateCandidateReceipt(snapshot, contract, receipt, now.Add(2*time.Hour)); !errors.Is(err, ErrInvalidCandidateSnapshot) {
		t.Fatalf("expired receipt error = %v, want ErrInvalidCandidateSnapshot", err)
	}

	other, err := NewCandidateSnapshot([]CandidateFile{candidateTestFile("solution/solve.sh", "different")})
	if err != nil {
		t.Fatalf("create substituted snapshot: %v", err)
	}
	if err := ValidateCandidateReceipt(other, contract, receipt, now.Add(time.Minute)); !errors.Is(err, ErrInvalidCandidateSnapshot) {
		t.Fatalf("substituted snapshot receipt error = %v, want ErrInvalidCandidateSnapshot", err)
	}

	rejected, err := NewValidationReceipt(ValidationReceipt{
		SnapshotDigest: snapshot.Digest, ContractDigest: contractDigest, Verdict: ValidationReject,
		FailureCode: AgentFailureValidatorReject, IssuedAt: now, ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("create rejected receipt: %v", err)
	}
	if rejected.FailureCode != AgentFailureValidatorReject || rejected.Digest == "" {
		t.Fatalf("rejected receipt = %#v, want bound failure receipt", rejected)
	}
}

func candidateTestFile(name, content string) CandidateFile {
	return CandidateFile{
		Path: name, SchemaVersion: "task-file/v1", ContentDigest: SHA256Fingerprint([]byte(content)), SizeBytes: int64(len(content)),
	}
}
