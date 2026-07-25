package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestAuthoringRepairLedgerReplaysAppendOnlyOpenRequirement(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	_, _, run, _ := createAuthoringReviewGateFixture(t, ctx, s)
	repairID, err := NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	request := OpenAuthoringRepairLedgerEntryRequest{
		ID: repairID, RunID: run.ID, ContractDigest: authoringDigest("a"),
		TargetProducer: "repo_analyze", FindingKind: AuthoringRepairFindingSourceAnalysisInvalid,
		Reason: "review evidence identifies a missing source fact", EvidenceDigest: authoringDigest("b"), Actor: "reviewer",
	}
	opened, err := s.OpenAuthoringRepairLedgerEntry(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if opened.ID != repairID || opened.RunID != run.ID || opened.State != AuthoringRepairLedgerOpen ||
		opened.FindingKind != AuthoringRepairFindingSourceAnalysisInvalid || opened.Resolution != nil {
		t.Fatalf("opened repair ledger entry = %+v", opened)
	}
	replayed, err := s.OpenAuthoringRepairLedgerEntry(ctx, request)
	if err != nil || replayed != opened {
		t.Fatalf("replayed repair ledger entry = %+v, %v; first=%+v", replayed, err, opened)
	}
	changed := request
	changed.Reason = "different repair requirement"
	if _, err := s.OpenAuthoringRepairLedgerEntry(ctx, changed); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed repair ledger replay error = %v, want idempotency conflict", err)
	}
	entries, err := s.ListOpenAuthoringRepairLedgerEntries(ctx, run.ID)
	if err != nil || len(entries) != 1 || entries[0] != opened {
		t.Fatalf("open repair ledger entries = %+v, %v", entries, err)
	}
	if _, err := s.db.Exec(`UPDATE audit_events SET reason = 'mutated' WHERE id = ?`, opened.ID); err == nil {
		t.Fatal("repair ledger audit event accepted a direct mutation")
	}
	artifactID, err := NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	attemptID, err := NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveAuthoringRepairLedgerEntry(ctx, ResolveAuthoringRepairLedgerEntryRequest{
		ID: mustRepairLedgerUUID(t), RepairID: opened.ID, RunID: run.ID, ContractDigest: opened.ContractDigest,
		Producer: opened.TargetProducer, SupersedingArtifactID: artifactID, SupersedingAttemptID: attemptID,
		Reason: "attempt to resolve without validated evidence", Actor: "reviewer",
	}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("unvalidated repair resolution error = %v, want invalid transition", err)
	}
	entries, err = s.ListOpenAuthoringRepairLedgerEntries(ctx, run.ID)
	if err != nil || len(entries) != 1 || entries[0].ID != opened.ID {
		t.Fatalf("failed resolution changed open ledger = %+v, %v", entries, err)
	}
}

func TestAuthoringRepairLedgerRejectsUnknownFindingKind(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	_, _, run, _ := createAuthoringReviewGateFixture(t, ctx, s)
	_, err := s.OpenAuthoringRepairLedgerEntry(ctx, OpenAuthoringRepairLedgerEntryRequest{
		ID: mustRepairLedgerUUID(t), RunID: run.ID, ContractDigest: authoringDigest("a"), TargetProducer: "task_design",
		FindingKind: "model_says_so", Reason: "untyped repair", EvidenceDigest: authoringDigest("b"), Actor: "reviewer",
	})
	if err == nil || !strings.Contains(err.Error(), "finding kind") {
		t.Fatalf("unknown repair finding kind error = %v", err)
	}
}

func mustRepairLedgerUUID(t *testing.T) string {
	t.Helper()
	id, err := NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
