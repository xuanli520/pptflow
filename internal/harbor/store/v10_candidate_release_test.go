package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCandidateGarbageCollectionTombstonesOnlyExpiredTerminalMaterial(t *testing.T) {
	ctx := context.Background()
	fixture := prepareRevisionCandidatePlanFixture(t)
	clock := time.Date(2043, 2, 3, 4, 5, 6, 0, time.UTC)
	fixture.store.now = func() time.Time { return clock }
	candidate, err := fixture.store.DiscardRevisionCandidate(ctx, DiscardRevisionCandidateRequest{
		CandidateID: fixture.candidate.ID, ExpectedVersion: fixture.candidate.Version, Actor: "retention-worker", Reason: "discard fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	if candidate.RetainUntil == nil || !candidate.RetainUntil.Equal(clock.Add(RevisionCandidateRetention)) {
		t.Fatalf("discarded candidate retention = %v, want %v", candidate.RetainUntil, clock.Add(RevisionCandidateRetention))
	}
	request := PrepareCandidateGarbageCollectionRequest{
		CandidateID: candidate.ID, ExpectedCandidateVersion: candidate.Version, IdempotencyKey: mustUUIDv7(t),
		Actor: "retention-worker", Reason: "seven day retention elapsed",
	}
	if _, err := fixture.store.PrepareCandidateGarbageCollection(ctx, request); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("early candidate garbage collection = %v, want invalid transition", err)
	}
	clock = clock.Add(RevisionCandidateRetention)
	prepared, err := fixture.store.PrepareCandidateGarbageCollection(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Operation.State != CandidateGarbageCollectionInProgress || prepared.Operation.CandidateID != candidate.ID {
		t.Fatalf("prepared candidate cleanup = %+v", prepared)
	}
	if _, err := fixture.store.FinalizeCandidateGarbageCollection(ctx, FinalizeCandidateGarbageCollectionRequest{
		OperationID: prepared.Operation.ID, ExpectedVersion: prepared.Operation.Version + 1, Actor: "retention-worker", Reason: request.Reason,
		RemoveDirectory: func() error { return nil },
	}); !errors.Is(err, ErrOptimisticLock) {
		t.Fatalf("stale candidate cleanup finalization = %v, want optimistic lock", err)
	}
	removals := 0
	finalized, err := fixture.store.FinalizeCandidateGarbageCollection(ctx, FinalizeCandidateGarbageCollectionRequest{
		OperationID: prepared.Operation.ID, ExpectedVersion: prepared.Operation.Version, Actor: "retention-worker", Reason: request.Reason,
		RemoveDirectory: func() error {
			removals++
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !finalized.Collected || removals != 1 || finalized.Operation.State != CandidateGarbageCollectionCompleted || finalized.Candidate.CheckoutTombstonedAt == nil || finalized.Candidate.CheckoutTombstonedBy != "retention-worker" {
		t.Fatalf("candidate cleanup finalization = %+v removals=%d", finalized, removals)
	}
	stored, err := fixture.store.GetRevisionCandidate(ctx, candidate.ID)
	if err != nil || stored == nil || stored.State != RevisionCandidateDiscarded || stored.CheckoutTombstonedAt == nil {
		t.Fatalf("candidate tombstone projection = %+v err=%v", stored, err)
	}
	if stored.MutationReceiptID == "" || stored.PreparedChangeID == "" {
		t.Fatalf("candidate immutable evidence was removed from record: %+v", stored)
	}
	if receipt, err := fixture.store.GetMutationReceipt(ctx, stored.MutationReceiptID); err != nil || receipt == nil {
		t.Fatalf("candidate immutable mutation receipt missing after cleanup: %+v err=%v", receipt, err)
	}
	for _, identity := range []string{candidate.ID, candidate.TargetRevisionID, candidate.TargetRunID} {
		var count int
		if err := fixture.store.db.QueryRow(`SELECT COUNT(*) FROM entity_id_registry WHERE id = ?`, identity).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("candidate cleanup released global UUID identity %s", identity)
		}
	}
	replayed, err := fixture.store.PrepareCandidateGarbageCollection(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Operation.ID != finalized.Operation.ID || replayed.Operation.Version != finalized.Operation.Version {
		t.Fatalf("candidate cleanup replay = %+v initial=%+v", replayed, finalized)
	}
	if _, err := fixture.store.PrepareCandidateGarbageCollection(ctx, PrepareCandidateGarbageCollectionRequest{
		CandidateID: candidate.ID, ExpectedCandidateVersion: stored.Version, IdempotencyKey: mustUUIDv7(t),
		Actor: "retention-worker", Reason: request.Reason,
	}); !errors.Is(err, ErrImmutable) {
		t.Fatalf("second candidate cleanup key = %v, want immutable tombstone", err)
	}
}

func TestCandidateGarbageCollectionRecordsFilesystemFailureAndReplays(t *testing.T) {
	ctx := context.Background()
	fixture := prepareRevisionCandidatePlanFixture(t)
	clock := time.Date(2044, 3, 4, 5, 6, 7, 0, time.UTC)
	fixture.store.now = func() time.Time { return clock }
	candidate, err := fixture.store.DiscardRevisionCandidate(ctx, DiscardRevisionCandidateRequest{
		CandidateID: fixture.candidate.ID, ExpectedVersion: fixture.candidate.Version, Actor: "retention-worker", Reason: "discard fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(RevisionCandidateRetention)
	request := PrepareCandidateGarbageCollectionRequest{
		CandidateID: candidate.ID, ExpectedCandidateVersion: candidate.Version, IdempotencyKey: mustUUIDv7(t),
		Actor: "retention-worker", Reason: "seven day retention elapsed",
	}
	prepared, err := fixture.store.PrepareCandidateGarbageCollection(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.FinalizeCandidateGarbageCollection(ctx, FinalizeCandidateGarbageCollectionRequest{
		OperationID: prepared.Operation.ID, ExpectedVersion: prepared.Operation.Version, Actor: "retention-worker", Reason: request.Reason,
		RemoveDirectory: func() error { return errors.New("permission denied") },
	}); !errors.Is(err, ErrCandidateGCFilesystem) {
		t.Fatalf("candidate cleanup filesystem failure = %v, want ErrCandidateGCFilesystem", err)
	}
	failed, err := fixture.store.RecordCandidateGarbageCollectionFailure(ctx, RecordCandidateGarbageCollectionFailureRequest{
		OperationID: prepared.Operation.ID, ExpectedVersion: prepared.Operation.Version, Actor: "retention-worker", Reason: request.Reason, ErrorText: "permission denied",
	})
	if err != nil {
		t.Fatal(err)
	}
	if failed.LastError == "" || failed.Version != prepared.Operation.Version+1 {
		t.Fatalf("candidate cleanup failure projection = %+v", failed)
	}
	recovered, err := fixture.store.PrepareCandidateGarbageCollection(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Operation.ID != failed.ID || recovered.Operation.Version != failed.Version || recovered.Operation.LastError == "" {
		t.Fatalf("candidate cleanup recovery = %+v failed=%+v", recovered, failed)
	}
	finalized, err := fixture.store.FinalizeCandidateGarbageCollection(ctx, FinalizeCandidateGarbageCollectionRequest{
		OperationID: recovered.Operation.ID, ExpectedVersion: recovered.Operation.Version, Actor: "retention-worker", Reason: request.Reason,
		RemoveDirectory: func() error { return nil },
	})
	if err != nil || !finalized.Collected || finalized.Operation.LastError != "" {
		t.Fatalf("candidate cleanup recovered finalization = %+v err=%v", finalized, err)
	}
}

func TestReleaseWithdrawUsesUUIDv7IdempotencyCASAndImmutableReceipt(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	task, revision := createValidatedTaskAndRevision(t, s)
	revision, err := s.TransitionTaskRevisionState(ctx, TransitionTaskRevisionStateRequest{
		RevisionID: revision.ID, ExpectedStateVersion: revision.StateVersion, State: RevisionStateReleased, Actor: "tester", Reason: "release fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	release, err := s.CreateLocalPackageRelease(ctx, CreateLocalPackageReleaseRequest{
		ReleaseVersion: "v10-withdraw", TaskID: task.ID, RevisionID: revision.ID, TaskDigest: revision.TaskDigest,
		PackageRef: "objects/sha256/package", EvidenceRef: "objects/sha256/evidence", Actor: "tester", Reason: "release fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	channel, err := s.SetReleaseChannel(ctx, SetReleaseChannelRequest{Channel: "stable", ReleaseID: release.ID, ExpectedVersion: 0, Actor: "tester", Reason: "channel fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExecuteReleaseWithdraw(ctx, ExecuteReleaseWithdrawRequest{
		ReleaseID: release.ID, ExpectedReleaseVersion: release.RecordVersion, IdempotencyKey: "not-a-uuid", Actor: "tester", Reason: "withdraw fixture",
	}); !errors.Is(err, ErrInvalidUUIDv7Identity) {
		t.Fatalf("non-UUIDv7 withdrawal key = %v, want identity error", err)
	}
	if _, err := s.ExecuteReleaseWithdraw(ctx, ExecuteReleaseWithdrawRequest{
		ReleaseID: release.ID, ExpectedReleaseVersion: release.RecordVersion + 1, IdempotencyKey: mustUUIDv7(t), Actor: "tester", Reason: "stale withdrawal",
	}); !errors.Is(err, ErrOptimisticLock) {
		t.Fatalf("stale withdrawal CAS = %v, want optimistic lock", err)
	}
	key := mustUUIDv7(t)
	request := ExecuteReleaseWithdrawRequest{
		ReleaseID: release.ID, ExpectedReleaseVersion: release.RecordVersion, IdempotencyKey: key, Actor: "tester", Reason: "withdraw fixture",
	}
	withdrawn, err := s.ExecuteReleaseWithdraw(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if withdrawn.Release.WithdrawnAt == nil || withdrawn.Release.RecordVersion != release.RecordVersion+1 || withdrawn.Operation.State != ReleaseWithdrawCompleted || withdrawn.Receipt.ID == "" || withdrawn.Receipt.ReceiptDigest == "" {
		t.Fatalf("release withdrawal = %+v", withdrawn)
	}
	if withdrawalOperation, err := s.GetReleaseWithdrawOperation(ctx, withdrawn.Operation.ID); err != nil || withdrawalOperation == nil || withdrawalOperation.ReceiptID != withdrawn.Receipt.ID {
		t.Fatalf("stored release withdrawal operation = %+v err=%v", withdrawalOperation, err)
	}
	if receipt, err := s.GetReleaseWithdrawReceipt(ctx, withdrawn.Receipt.ID); err != nil || receipt == nil || receipt.ReceiptDigest != withdrawn.Receipt.ReceiptDigest {
		t.Fatalf("stored release withdrawal receipt = %+v err=%v", receipt, err)
	}
	for _, identity := range []string{withdrawn.Operation.ID, withdrawn.Receipt.ID} {
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM entity_id_registry WHERE id = ?`, identity).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("release withdrawal identity %s is not globally registered", identity)
		}
	}
	replayed, err := s.ExecuteReleaseWithdraw(ctx, request)
	if err != nil || replayed.Operation.ID != withdrawn.Operation.ID || replayed.Receipt.ID != withdrawn.Receipt.ID || replayed.Release.RecordVersion != withdrawn.Release.RecordVersion {
		t.Fatalf("release withdrawal replay = %+v err=%v", replayed, err)
	}
	conflict := request
	conflict.Reason = "a different command"
	if _, err := s.ExecuteReleaseWithdraw(ctx, conflict); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflicting release withdrawal replay = %v, want idempotency conflict", err)
	}
	if _, err := s.ExecuteReleaseWithdraw(ctx, ExecuteReleaseWithdrawRequest{
		ReleaseID: release.ID, ExpectedReleaseVersion: withdrawn.Release.RecordVersion, IdempotencyKey: mustUUIDv7(t), Actor: "tester", Reason: "second withdrawal",
	}); !errors.Is(err, ErrImmutable) {
		t.Fatalf("second release withdrawal = %v, want immutable", err)
	}
	pointer, err := s.GetReleaseChannel(ctx, channel.Channel)
	if err != nil || pointer == nil || pointer.ReleaseID != release.ID || pointer.Version != channel.Version {
		t.Fatalf("withdrawal changed channel pointer without a channel CAS: %+v err=%v", pointer, err)
	}
}
