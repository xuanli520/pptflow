package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestTaskPurgeProtocolUsesCASIdempotencyAndPermanentMutationFence(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	task := createPurgeableTaskV7(t, s)
	request := PrepareTaskPurgeRequest{
		TaskID: task.ID, ExpectedTaskVersion: task.Version, IdempotencyKey: "purge-protocol-key",
		Actor: "tester", Reason: "retention elapsed",
	}

	prepared, err := s.PrepareTaskPurge(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.Acquired || prepared.Operation.State != TaskPurgeInProgress || prepared.Operation.LeaseID == "" {
		t.Fatalf("prepared purge = %+v", prepared)
	}
	lease, err := s.GetLease(ctx, prepared.Operation.LeaseID)
	if err != nil || lease == nil || lease.State != LeaseActive || lease.ResourceType != "task_purge" || lease.ResourceID != task.ID {
		t.Fatalf("purge lease = %+v err=%v", lease, err)
	}
	if _, err := s.UpdateTaskV2(ctx, UpdateTaskV2Request{
		TaskID: task.ID, ExpectedVersion: task.Version, LifecycleState: TaskLifecycleDraft, Actor: "tester",
	}); !errors.Is(err, ErrTaskPurgeInProgress) {
		t.Fatalf("mutation during purge err=%v, want ErrTaskPurgeInProgress", err)
	}

	stale := request
	stale.IdempotencyKey = "purge-stale-version"
	stale.ExpectedTaskVersion--
	if _, err := s.PrepareTaskPurge(ctx, stale); !errors.Is(err, ErrOptimisticLock) {
		t.Fatalf("stale purge prepare err=%v, want ErrOptimisticLock", err)
	}
	replay, err := s.PrepareTaskPurge(ctx, request)
	if err != nil {
		t.Fatalf("idempotent prepare replay: %v", err)
	}
	if replay.Acquired || replay.Operation.ID != prepared.Operation.ID || replay.Operation.Version != prepared.Operation.Version {
		t.Fatalf("prepared replay = %+v, initial=%+v", replay, prepared)
	}
	conflict := request
	conflict.Reason = "different command"
	if _, err := s.PrepareTaskPurge(ctx, conflict); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflicting idempotency err=%v, want ErrIdempotencyConflict", err)
	}
	if _, err := s.FinalizeTaskPurge(ctx, FinalizeTaskPurgeRequest{
		OperationID: prepared.Operation.ID, ExpectedVersion: prepared.Operation.Version + 1,
		Actor: "tester", Reason: "retention elapsed", RemoveDirectory: func() error { return nil },
	}); !errors.Is(err, ErrOptimisticLock) {
		t.Fatalf("stale finalization err=%v, want ErrOptimisticLock", err)
	}

	removals := 0
	finalized, err := s.FinalizeTaskPurge(ctx, FinalizeTaskPurgeRequest{
		OperationID: prepared.Operation.ID, ExpectedVersion: prepared.Operation.Version,
		Actor: "tester", Reason: "retention elapsed", RemoveDirectory: func() error {
			removals++
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if removals != 1 || !finalized.Purged || finalized.Operation.State != TaskPurgeCompleted || finalized.Operation.CompletedAt == nil {
		t.Fatalf("finalized purge = %+v removals=%d", finalized, removals)
	}
	lease, err = s.GetLease(ctx, prepared.Operation.LeaseID)
	if err != nil || lease == nil || lease.State != LeaseReleased {
		t.Fatalf("completed purge lease = %+v err=%v", lease, err)
	}
	events, err := s.ListAuditEvents(ctx, ListAuditEventsRequest{EntityType: "task_purge_operation", EntityID: finalized.Operation.ID})
	if err != nil {
		t.Fatal(err)
	}
	actions := make(map[string]bool, len(events))
	for _, event := range events {
		actions[event.Action] = true
	}
	if !actions["task_purge.prepared"] || !actions["task_purge.completed"] {
		t.Fatalf("purge audit history = %+v", events)
	}

	if _, err := s.UpdateTaskV2(ctx, UpdateTaskV2Request{
		TaskID: task.ID, ExpectedVersion: task.Version, LifecycleState: TaskLifecycleDraft, Actor: "tester",
	}); !errors.Is(err, ErrTaskPurged) {
		t.Fatalf("restore after completed purge err=%v, want ErrTaskPurged", err)
	}
	if _, err := s.CreateTaskRevision(ctx, CreateTaskRevisionRequest{
		TaskID: task.ID, Origin: RevisionOriginManual, TaskDigest: validTaskDigest("a"), Actor: "tester",
	}); !errors.Is(err, ErrTaskPurged) {
		t.Fatalf("revision after completed purge err=%v, want ErrTaskPurged", err)
	}
	newCommand := request
	newCommand.IdempotencyKey = "purge-after-complete"
	if _, err := s.PrepareTaskPurge(ctx, newCommand); !errors.Is(err, ErrTaskPurged) {
		t.Fatalf("new purge after completed operation err=%v, want ErrTaskPurged", err)
	}
	replay, err = s.PrepareTaskPurge(ctx, request)
	if err != nil {
		t.Fatalf("completed purge replay: %v", err)
	}
	if replay.Acquired || replay.Operation.ID != finalized.Operation.ID || replay.Operation.State != TaskPurgeCompleted {
		t.Fatalf("completed purge replay = %+v", replay)
	}
}

func TestTaskPurgeValidatesCommandBeforeCreatingCriticalBackup(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	before, err := s.ListVerifiedBackups()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareTaskPurge(ctx, PrepareTaskPurgeRequest{}); err == nil {
		t.Fatal("invalid purge command was accepted")
	}
	after, err := s.ListVerifiedBackups()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("invalid purge command created backup: before=%d after=%d", len(before), len(after))
	}
}

func TestTaskPurgeFilesystemFailureReleasesLeaseForIdempotentRecovery(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	task := createPurgeableTaskV7(t, s)
	request := PrepareTaskPurgeRequest{
		TaskID: task.ID, ExpectedTaskVersion: task.Version, IdempotencyKey: "purge-filesystem-recovery",
		Actor: "tester", Reason: "retention elapsed",
	}
	prepared, err := s.PrepareTaskPurge(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinalizeTaskPurge(ctx, FinalizeTaskPurgeRequest{
		OperationID: prepared.Operation.ID, ExpectedVersion: prepared.Operation.Version,
		Actor: "tester", Reason: "retention elapsed", RemoveDirectory: func() error { return errors.New("permission denied") },
	}); !errors.Is(err, ErrTaskPurgeFilesystem) {
		t.Fatalf("filesystem failure err=%v, want ErrTaskPurgeFilesystem", err)
	}
	failed, err := s.RecordTaskPurgeFailure(ctx, RecordTaskPurgeFailureRequest{
		OperationID: prepared.Operation.ID, ExpectedVersion: prepared.Operation.Version,
		Actor: "tester", Reason: "retention elapsed", ErrorText: "permission denied",
	})
	if err != nil {
		t.Fatal(err)
	}
	if failed.State != TaskPurgeInProgress || failed.LastError == "" || failed.Version != prepared.Operation.Version+1 {
		t.Fatalf("recorded failure = %+v", failed)
	}
	lease, err := s.GetLease(ctx, prepared.Operation.LeaseID)
	if err != nil || lease == nil || lease.State != LeaseReleased {
		t.Fatalf("failure did not release purge lease: %+v err=%v", lease, err)
	}

	recovered, err := s.PrepareTaskPurge(ctx, request)
	if err != nil {
		t.Fatalf("recover prepared purge: %v", err)
	}
	if !recovered.Acquired || recovered.Operation.ID != prepared.Operation.ID || recovered.Operation.Version != failed.Version+1 || recovered.Operation.LastError != "" || recovered.Operation.LeaseID == prepared.Operation.LeaseID {
		t.Fatalf("recovered purge = %+v", recovered)
	}
	finalized, err := s.FinalizeTaskPurge(ctx, FinalizeTaskPurgeRequest{
		OperationID: recovered.Operation.ID, ExpectedVersion: recovered.Operation.Version,
		Actor: "tester", Reason: "retention elapsed", RemoveDirectory: func() error { return nil },
	})
	if err != nil || !finalized.Purged || finalized.Operation.State != TaskPurgeCompleted {
		t.Fatalf("recovered finalization = %+v err=%v", finalized, err)
	}
}

func TestTaskPurgeReclaimsExpiredLeaseAfterCrashAtFilesystemBoundary(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	clock := time.Date(2042, 1, 2, 3, 4, 5, 0, time.UTC)
	s.now = func() time.Time { return clock }
	task := createPurgeableTaskV7(t, s)
	request := PrepareTaskPurgeRequest{
		TaskID: task.ID, ExpectedTaskVersion: task.Version, IdempotencyKey: "purge-crash-recovery",
		Actor: "tester", Reason: "retention elapsed", LeaseTTL: time.Minute,
	}
	prepared, err := s.PrepareTaskPurge(ctx, request)
	if err != nil {
		t.Fatal(err)
	}

	// Model a process death after RemoveDirectory succeeded but before the
	// SQLite transaction committed. The operation and its lease remain at the
	// prepared state; a later retry must reclaim it and accept an absent path.
	clock = clock.Add(2 * time.Minute)
	recovered, err := s.PrepareTaskPurge(ctx, request)
	if err != nil {
		t.Fatalf("reclaim expired purge: %v", err)
	}
	if !recovered.Acquired || recovered.Operation.ID != prepared.Operation.ID || recovered.Operation.LeaseID == prepared.Operation.LeaseID || recovered.Operation.LeaseFencingToken <= prepared.Operation.LeaseFencingToken {
		t.Fatalf("reclaimed purge = %+v initial=%+v", recovered, prepared)
	}
	finalized, err := s.FinalizeTaskPurge(ctx, FinalizeTaskPurgeRequest{
		OperationID: recovered.Operation.ID, ExpectedVersion: recovered.Operation.Version,
		Actor: "tester", Reason: "retention elapsed", RemoveDirectory: func() error { return nil },
	})
	if err != nil || !finalized.Purged {
		t.Fatalf("finalize reclaimed purge = %+v err=%v", finalized, err)
	}
}

func TestTaskPurgePersistsBlockedDependencySnapshotWithoutLease(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	task := createPurgeableTaskV7(t, s)
	pending, err := s.CreateOutboxEvent(ctx, CreateOutboxEventRequest{
		Topic: "task.purge.test", EntityType: "task", EntityID: task.ID, PayloadJSON: `{}`,
		IdempotencyKey: "purge-blocking-outbox", Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := PrepareTaskPurgeRequest{
		TaskID: task.ID, ExpectedTaskVersion: task.Version, IdempotencyKey: "purge-blocked-operation",
		Actor: "tester", Reason: "retention elapsed",
	}
	prepared, err := s.PrepareTaskPurge(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Acquired || prepared.Operation.State != TaskPurgeBlocked || prepared.Operation.LeaseID != "" || len(prepared.Operation.Dependencies.PendingOutboxIDs) != 1 || prepared.Operation.Dependencies.PendingOutboxIDs[0] != pending.ID {
		t.Fatalf("blocked purge = %+v", prepared)
	}
	replay, err := s.PrepareTaskPurge(ctx, request)
	if err != nil || replay.Acquired || replay.Operation.ID != prepared.Operation.ID || replay.Operation.State != TaskPurgeBlocked {
		t.Fatalf("blocked purge replay = %+v err=%v", replay, err)
	}
}

func TestTaskPurgeBlocksImmutableArtifactReferences(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	task, err := s.CreateTaskV2(ctx, CreateTaskV2Request{Slug: "purge-artifact-pin", Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	revision, err := s.CreateTaskRevision(ctx, CreateTaskRevisionRequest{
		TaskID: task.ID, Origin: RevisionOriginManual, TaskDigest: validTaskDigest("f"), Actor: "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := s.UpdateTaskV2(ctx, UpdateTaskV2Request{
		TaskID: task.ID, ExpectedVersion: task.Version, LifecycleState: TaskLifecycleDeleted, Actor: "tester", Reason: "retention fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := s.CreateArtifactManifest(ctx, CreateArtifactManifestRequest{
		SubjectRevisionID: revision.ID, SubjectDigest: revision.TaskDigest, WorkflowFingerprint: "sha256:workflow",
		ManifestJSON: `{}`, ManifestFingerprint: "sha256:manifest", IdempotencyKey: "purge-artifact-manifest", Actor: "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	reference, err := s.CreateArtifactRef(ctx, CreateArtifactRefRequest{
		ManifestID: manifest.ID, ArtifactKey: "report", ContentDigest: "sha256:content", SchemaVersion: "v1",
		RunID: "run", StageKey: "quality", AttemptID: "attempt", SubjectRevisionID: revision.ID,
		SubjectDigest: revision.TaskDigest, WorkflowFingerprint: "sha256:workflow", InputBindingsJSON: `{}`,
		InputFingerprint: "sha256:inputs", ProducerVersion: "v1", IdempotencyKey: "purge-artifact-ref", Actor: "tester",
	})
	if err != nil {
		t.Fatal(err)
	}

	prepared, err := s.PrepareTaskPurge(ctx, PrepareTaskPurgeRequest{
		TaskID: task.ID, ExpectedTaskVersion: deleted.Version, IdempotencyKey: "purge-artifact-operation",
		Actor: "tester", Reason: "retention elapsed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Acquired || prepared.Operation.State != TaskPurgeBlocked || len(prepared.Operation.Dependencies.ArtifactManifestIDs) != 1 || prepared.Operation.Dependencies.ArtifactManifestIDs[0] != manifest.ID || len(prepared.Operation.Dependencies.ArtifactRefIDs) != 1 || prepared.Operation.Dependencies.ArtifactRefIDs[0] != reference.ID {
		t.Fatalf("artifact-pinned purge = %+v", prepared)
	}
}

func TestMigrateV6ToCurrentInstallsTaskPurgeProtocol(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, dbFileName)
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range []string{migrationV2, migrationV3, migrationV4, migrationV5} {
		if _, err := db.Exec(migration); err != nil {
			_ = db.Close()
			t.Fatalf("build V5 migration fixture: %v", err)
		}
	}
	if _, err := db.Exec(`CREATE TABLE schema_version (version INTEGER NOT NULL)`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := applyMigrationV6(tx); err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		t.Fatalf("apply V6 fixture migration: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO schema_version (version) VALUES (6)`); err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(root)
	if err != nil {
		t.Fatalf("migrate V6 store to current schema: %v", err)
	}
	defer s.Close()
	var version, tableCount, triggerCount int
	if err := s.db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'task_purge_operations_v7'`).Scan(&tableCount); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name IN ('task_purge_v7_blocks_task_mutation', 'entity_id_registry_task_purge_operations_v7_insert', 'entity_id_registry_task_purge_operations_v7_id_immutable')`).Scan(&triggerCount); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion || tableCount != 1 || triggerCount != 3 {
		t.Fatalf("V7 migration result: version=%d table=%d triggers=%d", version, tableCount, triggerCount)
	}

	// The new table participates in the global lifetime identity namespace.
	// Using a real task as the foreign key proves the rejection comes from the
	// V7 global-ID trigger rather than a foreign-key validation failure.
	task, err := s.CreateTaskV2(context.Background(), CreateTaskV2Request{Slug: "migration-purge-identity", Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO task_purge_operations_v7 (id, task_id, idempotency_key, expected_task_version, actor, reason, state, dependencies_json, created_at, updated_at, version) VALUES (?, ?, 'migration-collision', 1, 'tester', 'fixture', 'blocked', '{}', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, 1)`, task.ID, task.ID); !isGlobalIdentityCollision(err) {
		t.Fatalf("V7 purge operation global-ID collision err=%v", err)
	}
}

func createPurgeableTaskV7(t *testing.T, s *Store) TaskV2 {
	t.Helper()
	ctx := context.Background()
	task, err := s.CreateTaskV2(ctx, CreateTaskV2Request{Slug: "purge-" + t.Name(), Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := s.UpdateTaskV2(ctx, UpdateTaskV2Request{
		TaskID: task.ID, ExpectedVersion: task.Version, LifecycleState: TaskLifecycleDeleted, Actor: "tester", Reason: "retention fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	return deleted
}
