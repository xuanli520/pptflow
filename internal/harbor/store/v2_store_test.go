package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrateV1ToV2PreservesLegacyTables(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, dbFileName)
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(migrationV1); err != nil {
		t.Fatalf("create v1 schema: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_version (version INTEGER NOT NULL)`); err != nil {
		t.Fatalf("create version table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO schema_version (version) VALUES (1)`); err != nil {
		t.Fatalf("record v1 schema: %v", err)
	}
	result, err := db.Exec(`INSERT INTO tasks (task_dir, task_name) VALUES (?, ?)`, "/tmp/legacy", "legacy")
	if err != nil {
		t.Fatalf("insert legacy task: %v", err)
	}
	legacyTaskID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO runs (task_id, workspace_path, run_id, status) VALUES (?, ?, ?, ?)`, legacyTaskID, "/tmp/legacy-workspace", "legacy-run", "succeeded"); err != nil {
		t.Fatalf("insert legacy run: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(root)
	if err != nil {
		t.Fatalf("migrate v1 store: %v", err)
	}
	defer s.Close()
	var version, taskCount, runCount int
	if err := s.db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("schema version = %d, want %d", version, schemaVersion)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM tasks`).Scan(&taskCount); err != nil || taskCount != 1 {
		t.Fatalf("v1 tasks not preserved: count=%d err=%v", taskCount, err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&runCount); err != nil || runCount != 1 {
		t.Fatalf("v1 runs not preserved: count=%d err=%v", runCount, err)
	}
	var tableName string
	if err := s.db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'tasks_v2'`).Scan(&tableName); err != nil || tableName != "tasks_v2" {
		t.Fatalf("tasks_v2 missing: %q err=%v", tableName, err)
	}
	backups, err := s.ListVerifiedBackups()
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) == 0 || backups[0].Reason != "critical:schema_migration" {
		t.Fatalf("expected verified pre-migration backup, got %+v", backups)
	}
}

func TestV2TaskRevisionReviewPromotionAndAudit(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	task, err := s.CreateTaskV2(ctx, CreateTaskV2Request{
		Slug:         "stable-task",
		Title:        "Stable task",
		MetadataJSON: `{"language":"go"}`,
		Actor:        "tester",
		Reason:       "create fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !isUUIDv7(task.ID) || task.Version != 1 || task.IdentityState != TaskIdentityCanonical {
		t.Fatalf("unexpected task: %+v", task)
	}
	_, err = s.CreateTaskV2(ctx, CreateTaskV2Request{ID: task.ID, Slug: "duplicate", Actor: "tester"})
	if !errors.Is(err, ErrIdentityCollision) {
		t.Fatalf("duplicate task err=%v, want identity collision", err)
	}

	if _, err := s.CreateTaskRevision(ctx, CreateTaskRevisionRequest{
		TaskID: task.ID, Origin: RevisionOriginManual, TaskDigest: "legacy:sha256:" + strings.Repeat("a", 64), Actor: "tester",
	}); err == nil {
		t.Fatal("V1/non-V2 digest was accepted for a V2 revision")
	}
	revision, err := s.CreateTaskRevision(ctx, CreateTaskRevisionRequest{
		TaskID:       task.ID,
		Origin:       RevisionOriginManual,
		TaskDigest:   validTaskDigest("a"),
		MetadataJSON: `{}`,
		Actor:        "tester",
		Reason:       "validated fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !isUUIDv7(revision.ID) || revision.VersionNumber != 1 {
		t.Fatalf("unexpected revision: %+v", revision)
	}
	if _, err := s.PromoteTaskCurrentRevision(ctx, PromoteCurrentRevisionRequest{TaskID: task.ID, RevisionID: revision.ID, ExpectedVersion: task.Version, Actor: "tester"}); !errors.Is(err, ErrRevisionNotValidated) {
		t.Fatalf("promotion before validation err=%v, want validation requirement", err)
	}
	revision, err = s.TransitionTaskRevisionState(ctx, TransitionTaskRevisionStateRequest{
		RevisionID: revision.ID, ExpectedStateVersion: revision.StateVersion, State: RevisionStateValidated,
		ValidationEvidenceManifest: "validation-manifest-sha256", Actor: "tester", Reason: "blocking checks passed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if revision.State != RevisionStateValidated || revision.ValidationEvidenceManifest == "" || revision.StateVersion != 2 {
		t.Fatalf("safe revision validation was not persisted: %+v", revision)
	}
	if _, err := s.PromoteTaskCurrentRevision(ctx, PromoteCurrentRevisionRequest{TaskID: task.ID, RevisionID: revision.ID, ExpectedVersion: task.Version, Actor: "tester"}); !errors.Is(err, ErrReviewApprovalNeeded) {
		t.Fatalf("promotion without review err=%v, want review requirement", err)
	}
	review, err := s.CreateReviewRequest(ctx, CreateReviewRequest{
		RevisionID:             revision.ID,
		EvidenceManifestDigest: "sha256:evidence",
		Actor:                  "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := s.RecordReviewDecision(ctx, RecordReviewDecisionRequest{
		ReviewRequestID:        review.ID,
		RevisionID:             revision.ID,
		Action:                 ReviewDecisionApprove,
		ExpectedRevisionDigest: revision.TaskDigest,
		Actor:                  "tester",
		Reason:                 "approved",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !isUUIDv7(decision.ID) {
		t.Fatalf("review decision is not UUIDv7: %+v", decision)
	}
	promoted, err := s.PromoteTaskCurrentRevision(ctx, PromoteCurrentRevisionRequest{
		TaskID: task.ID, RevisionID: revision.ID, ExpectedVersion: task.Version, Actor: "tester", Reason: "review approval",
	})
	if err != nil {
		t.Fatal(err)
	}
	if promoted.CurrentRevisionID != revision.ID || promoted.Version != task.Version+1 {
		t.Fatalf("promotion did not update current revision: %+v", promoted)
	}
	if _, err := s.UpdateTaskV2(ctx, UpdateTaskV2Request{TaskID: task.ID, ExpectedVersion: task.Version, Title: "stale", Actor: "tester"}); !errors.Is(err, ErrOptimisticLock) {
		t.Fatalf("stale task update err=%v, want optimistic lock", err)
	}
	if _, err := s.db.Exec(`UPDATE task_revisions SET task_digest = 'harbor.task.v2:sha256:`+strings.Repeat("b", 64)+`' WHERE id = ?`, revision.ID); err == nil {
		t.Fatal("immutable task revision accepted a direct update")
	}
	events, err := s.ListAuditEvents(ctx, ListAuditEventsRequest{EntityID: task.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 2 || events[0].Actor != "tester" {
		t.Fatalf("task audit events missing or actor was not persisted: %+v", events)
	}
	if _, err := s.db.Exec(`DELETE FROM audit_events WHERE id = ?`, events[0].ID); err == nil {
		t.Fatal("append-only audit event accepted a delete")
	}
}

func TestLegacyOrphanDoesNotMerge(t *testing.T) {
	s := tempDB(t)
	orphan, err := s.CreateLegacyOrphan(context.Background(), CreateTaskV2Request{
		Slug:           "orphan-workspace",
		Title:          "Unresolved legacy workspace",
		LegacyIdentity: "workspace=/tmp/unknown; no canonical digest",
		Actor:          "importer",
	})
	if err != nil {
		t.Fatal(err)
	}
	if orphan.IdentityState != TaskIdentityLegacyOrphan || orphan.LegacyIdentity == "" {
		t.Fatalf("legacy orphan was not retained distinctly: %+v", orphan)
	}
}

func TestWorkflowRunAndStageAttemptTransitions(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	task, revision := createValidatedTaskAndRevision(t, s)
	run, err := s.CreateWorkflowRun(ctx, CreateWorkflowRunRequest{
		TaskID:                  task.ID,
		RevisionID:              revision.ID,
		WorkflowTemplateID:      "harbor.standard",
		WorkflowTemplateVersion: "v1",
		ResolvedProfileHash:     "profile-sha256",
		DefinitionHash:          "definition-sha256",
		RunManifestJSON:         `{"template":"harbor.standard"}`,
		Trigger:                 "verify",
		Actor:                   "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err = s.TransitionWorkflowRun(ctx, TransitionWorkflowRunRequest{RunID: run.ID, ExpectedVersion: run.Version, Status: WorkflowRunRunning, Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	if run.StartedAt == nil || run.Version != 2 {
		t.Fatalf("run start not persisted: %+v", run)
	}
	stage, err := s.CreateStageAttempt(ctx, CreateStageAttemptRequest{
		RunID:              run.ID,
		StageKey:           "quality",
		StageGroup:         "quality",
		Ordinal:            1,
		InputFingerprint:   "input-sha256",
		BudgetSnapshotJSON: `{}`,
		RetrySnapshotJSON:  `{}`,
		Actor:              "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	stage, err = s.TransitionStageAttempt(ctx, TransitionStageAttemptRequest{StageAttemptID: stage.ID, ExpectedVersion: stage.Version, ExecutionStatus: StageExecutionRunning, Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	stage, err = s.TransitionStageAttempt(ctx, TransitionStageAttemptRequest{
		StageAttemptID: stage.ID, ExpectedVersion: stage.Version, ExecutionStatus: StageExecutionCompleted,
		Verdict: VerdictNeedsRepair, ArtifactManifestID: "manifest-sha256", Actor: "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	if stage.FinishedAt == nil || stage.Verdict != VerdictNeedsRepair {
		t.Fatalf("completed repair verdict not durable: %+v", stage)
	}
	if _, err := s.TransitionStageAttempt(ctx, TransitionStageAttemptRequest{StageAttemptID: stage.ID, ExpectedVersion: stage.Version, ExecutionStatus: StageExecutionRunning, Actor: "tester"}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("terminal stage transition err=%v, want invalid transition", err)
	}
}

func TestDurableJobIdempotencyAndLeaseRelease(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	job, err := s.CreateDurableJob(ctx, CreateDurableJobRequest{
		CommandType:    "run.execute",
		EntityType:     "task",
		EntityID:       "logical-task",
		PayloadJSON:    `{"scope":"full"}`,
		IdempotencyKey: "job-key-1",
		Actor:          "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := s.CreateDurableJob(ctx, CreateDurableJobRequest{
		CommandType: "run.execute", EntityType: "task", EntityID: "logical-task", PayloadJSON: `{"scope":"full"}`,
		IdempotencyKey: "job-key-1", Actor: "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ID != job.ID {
		t.Fatalf("idempotent job returned %s, want %s", replayed.ID, job.ID)
	}
	if _, err := s.CreateDurableJob(ctx, CreateDurableJobRequest{
		CommandType: "run.execute", EntityType: "task", EntityID: "logical-task", PayloadJSON: `{"scope":"different"}`,
		IdempotencyKey: "job-key-1", Actor: "tester",
	}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflicting idempotency err=%v", err)
	}
	lease, err := s.AcquireLease(ctx, AcquireLeaseRequest{ResourceType: "task", ResourceID: "logical-task", Owner: "worker-a", JobID: job.ID, Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	if lease.FencingToken != 1 || lease.ExpiresAt.Sub(lease.CreatedAt) != DefaultLeaseTTL {
		t.Fatalf("default lease policy not persisted: %+v", lease)
	}
	if _, err := s.AcquireLease(ctx, AcquireLeaseRequest{ResourceType: "task", ResourceID: "logical-task", Owner: "worker-b", Actor: "tester"}); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("competing lease err=%v, want held", err)
	}
	lease, err = s.HeartbeatLease(ctx, HeartbeatLeaseRequest{LeaseID: lease.ID, Owner: lease.Owner, FencingToken: lease.FencingToken, ExpectedVersion: lease.Version, Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Version != 2 {
		t.Fatalf("lease heartbeat did not CAS version: %+v", lease)
	}
	job, err = s.TransitionDurableJob(ctx, TransitionDurableJobRequest{JobID: job.ID, ExpectedVersion: job.Version, State: JobRunning, Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	job, err = s.TransitionDurableJob(ctx, TransitionDurableJobRequest{JobID: job.ID, ExpectedVersion: job.Version, State: JobSucceeded, Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	if job.FinishedAt == nil {
		t.Fatalf("terminal job has no finish timestamp: %+v", job)
	}
	storedLease, err := s.GetLease(ctx, lease.ID)
	if err != nil || storedLease == nil || storedLease.State != LeaseReleased {
		t.Fatalf("terminal job did not release lease: %+v err=%v", storedLease, err)
	}
	next, err := s.AcquireLease(ctx, AcquireLeaseRequest{ResourceType: "task", ResourceID: "logical-task", Owner: "worker-b", Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	if next.FencingToken <= lease.FencingToken {
		t.Fatalf("fencing token did not advance: old=%d new=%d", lease.FencingToken, next.FencingToken)
	}
}

func TestListDurableJobsAndLeasesForAttachProjection(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	task, revision := createValidatedTaskAndRevision(t, s)
	run, err := s.CreateWorkflowRun(ctx, CreateWorkflowRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, WorkflowTemplateID: "harbor.standard", WorkflowTemplateVersion: "v2",
		ResolvedProfileHash: "profile", DefinitionHash: "definition", RunManifestJSON: `{}`, Trigger: "verify", Actor: "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.CreateDurableJob(ctx, CreateDurableJobRequest{
		CommandType: "run.execute", EntityType: "workflow_run", EntityID: run.ID, RunID: run.ID,
		PayloadJSON: `{}`, IdempotencyKey: "attach-projection-job", Actor: "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := s.AcquireLease(ctx, AcquireLeaseRequest{
		ResourceType: "job_dispatch", ResourceID: job.ID, Owner: "worker", JobID: job.ID, Actor: "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := s.ListDurableJobsForRun(ctx, run.ID)
	if err != nil || len(jobs) != 1 || jobs[0].ID != job.ID {
		t.Fatalf("jobs for run = %+v, %v", jobs, err)
	}
	leases, err := s.ListLeasesForJob(ctx, job.ID)
	if err != nil || len(leases) != 1 || leases[0].ID != lease.ID || leases[0].State != LeaseActive {
		t.Fatalf("leases for job = %+v, %v", leases, err)
	}
	if _, err := s.ListDurableJobsForRun(ctx, "not-a-uuid"); !errors.Is(err, ErrInvalidUUIDv7Identity) {
		t.Fatalf("invalid run lookup error = %v", err)
	}
}

func TestCreateWorkflowRunDispatchIsAtomicWithRunRecord(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	task, revision := createValidatedTaskAndRevision(t, s)
	if _, err := s.CreateWorkflowRun(ctx, CreateWorkflowRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, WorkflowTemplateID: "harbor.standard", WorkflowTemplateVersion: "v2",
		ResolvedProfileHash: "profile", DefinitionHash: "definition", RunManifestJSON: `{}`, Trigger: "verify", Actor: "tester",
		Dispatch: &WorkflowRunDispatchRequest{CommandType: "workflow_run.execute", PayloadJSON: "not-json", IdempotencyKey: "bad-dispatch"},
	}); err == nil {
		t.Fatal("run with invalid dispatch payload was accepted")
	}
	runs, err := s.ListWorkflowRunsForTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("invalid dispatch left a visible workflow run: %+v", runs)
	}

	run, err := s.CreateWorkflowRun(ctx, CreateWorkflowRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, WorkflowTemplateID: "harbor.standard", WorkflowTemplateVersion: "v2",
		ResolvedProfileHash: "profile", DefinitionHash: "definition", RunManifestJSON: `{}`, Trigger: "verify", Actor: "tester",
		Dispatch: &WorkflowRunDispatchRequest{CommandType: "workflow_run.execute", PayloadJSON: `{"run":"queued"}`, IdempotencyKey: "good-dispatch"},
	})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := s.ListDurableJobsForRun(ctx, run.ID)
	if err != nil || len(jobs) != 1 || jobs[0].EntityID != run.ID || jobs[0].State != JobQueued {
		t.Fatalf("atomic initial dispatch = %+v, %v", jobs, err)
	}
	pending, err := s.ListPendingOutboxEvents(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Topic != "workflow_run.queued" || pending[0].EntityID != run.ID {
		t.Fatalf("atomic initial dispatch outbox = %+v", pending)
	}
}

func createValidatedTaskAndRevision(t *testing.T, s *Store) (TaskV2, TaskRevision) {
	t.Helper()
	ctx := context.Background()
	task, err := s.CreateTaskV2(ctx, CreateTaskV2Request{Slug: "task-" + t.Name(), Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	revision, err := s.CreateTaskRevision(ctx, CreateTaskRevisionRequest{
		TaskID: task.ID, Origin: RevisionOriginGenerated, TaskDigest: validTaskDigest("c"), Actor: "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	revision, err = s.TransitionTaskRevisionState(ctx, TransitionTaskRevisionStateRequest{
		RevisionID: revision.ID, ExpectedStateVersion: revision.StateVersion, State: RevisionStateValidated,
		ValidationEvidenceManifest: "validation-" + t.Name(), Actor: "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	return task, revision
}

func validTaskDigest(character string) string {
	return taskDigestV2Prefix + strings.Repeat(character, 64)
}
