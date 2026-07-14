package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func tempV5DB(t *testing.T) *Store {
	t.Helper()
	s := tempDB(t)
	if _, err := s.db.Exec(migrationV5); err != nil {
		t.Fatalf("apply v5 schema fixture: %v", err)
	}
	return s
}

func TestMigrateExistingV2ThroughV4StoresToCurrentSchema(t *testing.T) {
	for startingVersion := 2; startingVersion <= 4; startingVersion++ {
		t.Run("from_v"+string(rune('0'+startingVersion)), func(t *testing.T) {
			root := t.TempDir()
			db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(root, dbFileName)))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`CREATE TABLE schema_version (version INTEGER NOT NULL)`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(migrationV2); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO schema_version (version) VALUES (2)`); err != nil {
				t.Fatal(err)
			}
			migrations := []string{migrationV3, migrationV4}
			for version := 3; version <= startingVersion; version++ {
				if _, err := db.Exec(migrations[version-3]); err != nil {
					t.Fatalf("apply v%d fixture: %v", version, err)
				}
				if _, err := db.Exec(`INSERT INTO schema_version (version) VALUES (?)`, version); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			s, err := Open(root)
			if err != nil {
				t.Fatalf("migrate v%d fixture to current schema: %v", startingVersion, err)
			}
			defer s.Close()
			var version int
			if err := s.db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
				t.Fatal(err)
			}
			if version != schemaVersion {
				t.Fatalf("schema version = %d, want %d", version, schemaVersion)
			}
			for _, table := range []string{"quota_accounts_v5", "quota_account_policy_bindings_v11", "control_operations_v5", "side_effect_operations_v5", "capacity_pools_v5", "job_dispatch_claims_v5", "entity_id_registry", "task_purge_operations_v7", "outbox_delivery_operations_v9"} {
				var count int
				if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count != 1 {
					t.Fatalf("migration from v%d did not create %s", startingVersion, table)
				}
			}
		})
	}
}

func TestV5QuotaLedgerAdmissionAndReconcile(t *testing.T) {
	ctx := context.Background()
	s := tempV5DB(t)
	task, _ := createValidatedTaskAndRevision(t, s)
	actor := "local-owner"

	taskToken, err := s.CreateQuotaAccount(ctx, CreateQuotaAccountRequest{
		ScopeKind: QuotaScopeTask, ScopeID: task.ID, Dimension: "token", LimitUnits: 10, Actor: actor, Reason: "test setup",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateQuotaAccount(ctx, CreateQuotaAccountRequest{
		ScopeKind: QuotaScopeActor, ScopeID: actor, Dimension: "token", LimitUnits: 10, Actor: actor, Reason: "test setup",
	}); err != nil {
		t.Fatal(err)
	}
	lease, err := s.ReserveQuota(ctx, QuotaLeaseRequest{
		IdempotencyKey: "reserve-token", Owner: "worker-a", ScopeKind: QuotaScopeTask, ScopeID: task.ID,
		Dimension: "token", Units: 6, ReclaimPolicy: QuotaReclaimUnused, TTL: time.Hour, Actor: actor, Reason: "generation",
	})
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := s.ReserveQuota(ctx, QuotaLeaseRequest{
		IdempotencyKey: "reserve-token", Owner: "worker-a", ScopeKind: QuotaScopeTask, ScopeID: task.ID,
		Dimension: "token", Units: 6, ReclaimPolicy: QuotaReclaimUnused, TTL: time.Hour, Actor: actor, Reason: "generation",
	})
	if err != nil || duplicate.ID != lease.ID {
		t.Fatalf("idempotent reservation = %+v, %v", duplicate, err)
	}
	if _, err := s.RecordQuotaUsage(ctx, RecordQuotaUsageRequest{
		OperationKey: "turn-1", LeaseID: lease.ID, FencingToken: lease.FencingToken, Units: 2,
		OccurredAt: time.Now().UTC(), Actor: actor, Reason: "completed turn",
	}); err != nil {
		t.Fatal(err)
	}
	settlement, err := s.SettleQuotaLease(ctx, SettleQuotaLeaseRequest{
		IdempotencyKey: "settle-token", LeaseID: lease.ID, Owner: "worker-a", FencingToken: lease.FencingToken,
		Outcome: QuotaSettlementCompleted, Actor: actor, Reason: "completed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if settlement.Lease.State != DurableQuotaLeaseSettled || settlement.ConsumedUnits != 2 || settlement.ReleasedUnits != 4 {
		t.Fatalf("unexpected settled lease: %+v", settlement)
	}
	account, err := s.GetQuotaAccount(ctx, taskToken.ID)
	if err != nil || account == nil || account.ConsumedUnits != 2 || account.ReservedUnits != 0 || account.AvailableUnits() != 8 {
		t.Fatalf("quota account after settlement = %+v, %v", account, err)
	}

	uncertain, err := s.ReserveQuota(ctx, QuotaLeaseRequest{
		IdempotencyKey: "reserve-uncertain", Owner: "worker-b", ScopeKind: QuotaScopeTask, ScopeID: task.ID,
		Dimension: "token", Units: 3, ReclaimPolicy: QuotaReclaimUnused, TTL: time.Hour, Actor: actor, Reason: "provider call",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SettleQuotaLease(ctx, SettleQuotaLeaseRequest{
		IdempotencyKey: "settle-uncertain", LeaseID: uncertain.ID, Owner: "worker-b", FencingToken: uncertain.FencingToken,
		Outcome: QuotaSettlementUncertain, Actor: actor, Reason: "outcome unknown",
	}); err != nil {
		t.Fatal(err)
	}
	blocked, err := s.ReserveQuota(ctx, QuotaLeaseRequest{
		IdempotencyKey: "reserve-blocked", Owner: "worker-c", ScopeKind: QuotaScopeTask, ScopeID: task.ID,
		Dimension: "token", Units: 6, ReclaimPolicy: QuotaReclaimUnused, TTL: time.Hour, Actor: actor, Reason: "must block",
	})
	if err == nil || blocked.ID != "" || !errors.Is(err, ErrQuotaExhausted) {
		t.Fatalf("unknown outcome did not retain capacity: lease=%+v err=%v", blocked, err)
	}
	if _, err := s.ReconcileQuotaLease(ctx, ReconcileQuotaLeaseRequest{
		IdempotencyKey: "reconcile-uncertain", LeaseID: uncertain.ID, Owner: "worker-b", FencingToken: uncertain.FencingToken,
		Outcome: QuotaSettlementCanceled, Actor: actor, Reason: "provider confirmed no use",
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.CreateQuotaAccount(ctx, CreateQuotaAccountRequest{
		ScopeKind: QuotaScopeTask, ScopeID: task.ID, Dimension: "trial", LimitUnits: 2, Actor: actor, Reason: "test setup",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateQuotaAccount(ctx, CreateQuotaAccountRequest{
		ScopeKind: QuotaScopeActor, ScopeID: actor, Dimension: "trial", LimitUnits: 2, Actor: actor, Reason: "test setup",
	}); err != nil {
		t.Fatal(err)
	}
	admission, err := s.AdmitTaskActorQuota(ctx, AdmitTaskActorQuotaRequest{
		IdempotencyKey: "admit-trial", TaskID: task.ID, Actor: actor, LeaseOwner: "worker-d", LeaseTTL: time.Hour,
		Policy:            QuotaPolicyBinding{PolicyID: "test.quota", PolicyVersion: "1", PolicyFingerprint: "sha256:test-quota-v1"},
		BootstrapAccounts: []QuotaAccountBootstrap{{Dimension: "trial", TaskLimitUnits: 2, ActorLimitUnits: 2}},
		Claims:            []TaskActorQuotaClaim{{Dimension: "trial", Units: 1, ReclaimPolicy: QuotaReclaimUnused}}, Reason: "evaluate",
	})
	if err != nil || !admission.Accepted || len(admission.Leases) != 2 {
		t.Fatalf("task+actor admission = %+v, %v", admission, err)
	}
	replayed, err := s.AdmitTaskActorQuota(ctx, AdmitTaskActorQuotaRequest{
		IdempotencyKey: "admit-trial", TaskID: task.ID, Actor: actor, LeaseOwner: "worker-d", LeaseTTL: time.Hour,
		Policy:            QuotaPolicyBinding{PolicyID: "test.quota", PolicyVersion: "1", PolicyFingerprint: "sha256:test-quota-v1"},
		BootstrapAccounts: []QuotaAccountBootstrap{{Dimension: "trial", TaskLimitUnits: 2, ActorLimitUnits: 2}},
		Claims:            []TaskActorQuotaClaim{{Dimension: "trial", Units: 1, ReclaimPolicy: QuotaReclaimUnused}}, Reason: "evaluate",
	})
	if err != nil || replayed.ID != admission.ID || len(replayed.Leases) != 2 {
		t.Fatalf("idempotent admission = %+v, %v", replayed, err)
	}
}

func TestV5ExecutionControlAndReconciliationFacts(t *testing.T) {
	ctx := context.Background()
	s := tempV5DB(t)
	task, revision := createValidatedTaskAndRevision(t, s)
	run, err := s.CreateWorkflowRun(ctx, CreateWorkflowRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, WorkflowTemplateID: "harbor.standard", WorkflowTemplateVersion: "v5",
		ResolvedProfileHash: "profile", DefinitionHash: "workflow-fingerprint", RunManifestJSON: `{}`, Trigger: "create", Actor: "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err = s.TransitionWorkflowRun(ctx, TransitionWorkflowRunRequest{RunID: run.ID, ExpectedVersion: run.Version, Status: WorkflowRunRunning, Actor: "tester", Reason: "start"})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := ControlCheckpointRef{Sequence: uint64(run.Version), ExecutionEpoch: run.ExecutionEpoch, SubjectVersion: task.Version,
		SubjectID: task.ID, SubjectRevisionID: revision.ID, SubjectDigest: revision.TaskDigest, WorkflowFingerprint: run.DefinitionHash}
	operation, err := s.CreateExecutionControlOperation(ctx, ExecutionControlCommand{
		OperationKey: "pause-run", Action: ControlActionPause, RunID: run.ID, Expected: checkpoint, Actor: "tester", Reason: "safe pause", GracePeriod: time.Second,
	})
	if err != nil || operation.Status != ControlOperationRequested {
		t.Fatalf("create control operation = %+v, %v", operation, err)
	}
	updatedRun, err := s.GetWorkflowRun(ctx, run.ID)
	if err != nil || updatedRun == nil || updatedRun.Status != WorkflowRunPauseRequested {
		t.Fatalf("control did not atomically request run pause: %+v, %v", updatedRun, err)
	}
	duplicate, err := s.CreateExecutionControlOperation(ctx, ExecutionControlCommand{
		OperationKey: "pause-run", Action: ControlActionPause, RunID: run.ID, Expected: checkpoint, Actor: "tester", Reason: "safe pause", GracePeriod: time.Second,
	})
	if err != nil || duplicate.ID != operation.ID {
		t.Fatalf("idempotent control command = %+v, %v", duplicate, err)
	}
	receiptID, err := NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	transitionID, err := NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	operation, err = s.TransitionExecutionControlOperation(ctx, TransitionControlOperationRequest{
		ID: transitionID, OperationID: operation.ID, ExpectedVersion: operation.Version, Status: ControlOperationPropagating,
		RuntimeReceipts: []RuntimeTerminationReceipt{{ID: receiptID, RuntimeScopeID: "stage-scope", ObservedAt: time.Now().UTC(), Graceful: true, PayloadJSON: `{"checkpoint":"saved"}`}},
		Actor:           "tester", Reason: "runtime acknowledged",
	})
	if err != nil || operation.Status != ControlOperationPropagating || len(operation.RuntimeReceipts) != 1 {
		t.Fatalf("propagate control = %+v, %v", operation, err)
	}
	ackID, err := NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	operation, err = s.TransitionExecutionControlOperation(ctx, TransitionControlOperationRequest{
		ID: ackID, OperationID: operation.ID, ExpectedVersion: operation.Version, Status: ControlOperationAcknowledged,
		CheckpointID: "checkpoint-1", QuotaSettlementID: "settlement-1", Actor: "tester", Reason: "settled",
	})
	if err != nil || operation.Status != ControlOperationAcknowledged || operation.CheckpointID != "checkpoint-1" {
		t.Fatalf("acknowledge control = %+v, %v", operation, err)
	}

	sideEffect, err := s.CreateSideEffectOperation(ctx, CreateSideEffectOperationRequest{
		OperationKey: "local-package", IdempotencyKey: "local-package-idempotency", RunID: run.ID, EffectKind: "local_package",
		SourceDigest: "sha256:source", PayloadJSON: `{}`, Actor: "tester", Reason: "package",
	})
	if err != nil {
		t.Fatal(err)
	}
	sideEffect, err = s.TransitionSideEffectOperation(ctx, TransitionSideEffectOperationRequest{OperationID: sideEffect.ID, ExpectedVersion: sideEffect.Version, State: SideEffectStarted, Actor: "tester", Reason: "write package"})
	if err != nil {
		t.Fatal(err)
	}
	sideEffect, err = s.TransitionSideEffectOperation(ctx, TransitionSideEffectOperationRequest{OperationID: sideEffect.ID, ExpectedVersion: sideEffect.Version, State: SideEffectUnknown, Actor: "tester", Reason: "process lost"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := s.StartReconciliationAttempt(ctx, StartReconciliationAttemptRequest{
		OperationKey: "reconcile-package", SubjectType: "run", SubjectID: run.ID, SideEffectOperationID: sideEffect.ID,
		Ordinal: 1, ObservedJSON: `{"receipt":"found"}`, Actor: "tester", Reason: "inspect local package",
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err = s.CompleteReconciliationAttempt(ctx, CompleteReconciliationAttemptRequest{
		AttemptID: attempt.ID, ExpectedVersion: attempt.Version, State: ReconciliationCompleted, ObservedJSON: `{"receipt":"found"}`,
		Resolution: "package receipt verified", Actor: "tester", Reason: "resolved",
	})
	if err != nil || attempt.State != ReconciliationCompleted || attempt.FinishedAt == nil {
		t.Fatalf("complete reconciliation = %+v, %v", attempt, err)
	}
}

func TestV5DurableDispatcherClaimsAndRecoversExpiredWorkers(t *testing.T) {
	ctx := context.Background()
	s := tempV5DB(t)
	clock := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	pool, err := s.ConfigureCapacityPool(ctx, ConfigureCapacityPoolRequest{PoolKey: "local-workers", Capacity: 1, Actor: "tester", Reason: "test setup"})
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.CreateDurableJob(ctx, CreateDurableJobRequest{CommandType: "execute", EntityType: "test", EntityID: "one", IdempotencyKey: "job-one", Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := s.ClaimNextDurableJob(ctx, ClaimNextDurableJobRequest{IdempotencyKey: "claim-one", Owner: "worker-one", LeaseTTL: time.Minute, CapacityPoolKey: pool.PoolKey, Actor: "tester"})
	if err != nil || claim.Job == nil || claim.Job.ID != job.ID || claim.DispatchLease == nil || claim.CapacityLease == nil {
		t.Fatalf("claim queued job = %+v, %v", claim, err)
	}
	completed, err := s.TransitionDurableJob(ctx, TransitionDurableJobRequest{JobID: claim.Job.ID, ExpectedVersion: claim.Job.Version, State: JobSucceeded, Actor: "tester", Reason: "done"})
	if err != nil || completed.State != JobSucceeded {
		t.Fatalf("complete claimed job = %+v, %v", completed, err)
	}
	capacityLease, err := s.GetLease(ctx, claim.CapacityLease.ID)
	if err != nil || capacityLease == nil || capacityLease.State != LeaseReleased {
		t.Fatalf("terminal job did not release capacity lease: %+v, %v", capacityLease, err)
	}

	job, err = s.CreateDurableJob(ctx, CreateDurableJobRequest{CommandType: "execute", EntityType: "test", EntityID: "two", IdempotencyKey: "job-two", Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	expiring, err := s.ClaimNextDurableJob(ctx, ClaimNextDurableJobRequest{IdempotencyKey: "claim-two", Owner: "worker-two", LeaseTTL: time.Second, CapacityPoolKey: pool.PoolKey, Actor: "tester"})
	if err != nil || expiring.Job == nil || expiring.Job.ID != job.ID {
		t.Fatalf("claim expiring job = %+v, %v", expiring, err)
	}
	clock = clock.Add(2 * time.Second)
	recoveries, err := s.ScanExpiredDurableJobsForReconcile(ctx, ScanExpiredDurableJobsRequest{Actor: "recovery", Reason: "worker lease expired"})
	if err != nil || len(recoveries) != 1 || recoveries[0].Job.ID != job.ID || recoveries[0].Job.State != JobInterrupted {
		t.Fatalf("expired dispatch recovery = %+v, %v", recoveries, err)
	}
	capacityLease, err = s.GetLease(ctx, expiring.CapacityLease.ID)
	if err != nil || capacityLease == nil || capacityLease.State != LeaseReleased {
		t.Fatalf("recovery did not release capacity lease: %+v, %v", capacityLease, err)
	}
}

func TestV5ScopedDurableClaimFencesSelectionAndEmptyReplay(t *testing.T) {
	ctx := context.Background()
	s := tempV5DB(t)
	task, revision := createValidatedTaskAndRevision(t, s)
	runA := createV5ReconcileRun(t, ctx, s, task, revision, "scoped-dispatch-a")
	runB := createV5ReconcileRun(t, ctx, s, task, revision, "scoped-dispatch-b")

	jobA, err := s.CreateDurableJob(ctx, CreateDurableJobRequest{
		CommandType: "execute", EntityType: "fixture", EntityID: "scoped-dispatch-a", RunID: runA.ID,
		Priority: 1, PayloadJSON: `{}`, IdempotencyKey: "scoped-dispatch-job-a", Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	jobB, err := s.CreateDurableJob(ctx, CreateDurableJobRequest{
		CommandType: "execute", EntityType: "fixture", EntityID: "scoped-dispatch-b", RunID: runB.ID,
		Priority: 100, PayloadJSON: `{}`, IdempotencyKey: "scoped-dispatch-job-b", Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}

	request := ClaimNextDurableJobRequest{
		IdempotencyKey: "scoped-dispatch-claim-a", Owner: "worker-a", RunID: runA.ID,
		LeaseTTL: time.Minute, Actor: "tester", Reason: "claim only Run A",
	}
	claim, err := s.ClaimNextDurableJob(ctx, request)
	if err != nil || claim.Job == nil || claim.Job.ID != jobA.ID || claim.RunID != runA.ID {
		t.Fatalf("scoped claim = %+v, %v; want Run A job %s", claim, err, jobA.ID)
	}
	other, err := s.GetDurableJob(ctx, jobB.ID)
	if err != nil || other == nil || other.State != JobQueued {
		t.Fatalf("Run B job after Run A scoped claim = %+v, %v; want queued", other, err)
	}

	replayed, err := s.ClaimNextDurableJob(ctx, request)
	if err != nil || replayed.ID != claim.ID || replayed.RunID != runA.ID || replayed.Job == nil || replayed.Job.ID != jobA.ID {
		t.Fatalf("scoped active claim replay = %+v, %v; want %+v", replayed, err, claim)
	}
	conflict := request
	conflict.RunID = runB.ID
	if _, err := s.ClaimNextDurableJob(ctx, conflict); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("claim replay under another Run = %v, want ErrIdempotencyConflict", err)
	}

	emptyRequest := ClaimNextDurableJobRequest{
		IdempotencyKey: "scoped-dispatch-empty-a", Owner: "worker-a", RunID: runA.ID,
		LeaseTTL: time.Minute, Actor: "tester", Reason: "prove empty claim scope",
	}
	empty, err := s.ClaimNextDurableJob(ctx, emptyRequest)
	if err != nil || empty.State != "empty" || empty.Job != nil || empty.RunID != runA.ID {
		t.Fatalf("scoped empty claim = %+v, %v", empty, err)
	}
	emptyReplay, err := s.ClaimNextDurableJob(ctx, emptyRequest)
	if err != nil || emptyReplay.ID != empty.ID || emptyReplay.State != "empty" || emptyReplay.RunID != runA.ID {
		t.Fatalf("scoped empty replay = %+v, %v; want %+v", emptyReplay, err, empty)
	}
	emptyConflict := emptyRequest
	emptyConflict.RunID = runB.ID
	if _, err := s.ClaimNextDurableJob(ctx, emptyConflict); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("empty claim replay under another Run = %v, want ErrIdempotencyConflict", err)
	}
}

func TestV5ScopedExpiredJobReconciliationLeavesOtherRunUntouched(t *testing.T) {
	ctx := context.Background()
	s := tempV5DB(t)
	clock := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	task, revision := createValidatedTaskAndRevision(t, s)
	runA := createV5ReconcileRun(t, ctx, s, task, revision, "scoped-reconcile-a")
	runB := createV5ReconcileRun(t, ctx, s, task, revision, "scoped-reconcile-b")

	jobA, err := s.CreateDurableJob(ctx, CreateDurableJobRequest{
		CommandType: "execute", EntityType: "fixture", EntityID: "scoped-a", RunID: runA.ID,
		PayloadJSON: `{}`, IdempotencyKey: "scoped-reconcile-job-a", Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimA, err := s.ClaimNextDurableJob(ctx, ClaimNextDurableJobRequest{
		IdempotencyKey: "scoped-reconcile-claim-a", Owner: "worker-a", LeaseTTL: time.Second, Actor: "tester", Reason: "fixture",
	})
	if err != nil || claimA.Job == nil || claimA.DispatchLease == nil || claimA.Job.ID != jobA.ID {
		t.Fatalf("claim Run A job = %+v, %v", claimA, err)
	}

	jobB, err := s.CreateDurableJob(ctx, CreateDurableJobRequest{
		CommandType: "execute", EntityType: "fixture", EntityID: "scoped-b", RunID: runB.ID,
		PayloadJSON: `{}`, IdempotencyKey: "scoped-reconcile-job-b", Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimB, err := s.ClaimNextDurableJob(ctx, ClaimNextDurableJobRequest{
		IdempotencyKey: "scoped-reconcile-claim-b", Owner: "worker-b", LeaseTTL: time.Second, Actor: "tester", Reason: "fixture",
	})
	if err != nil || claimB.Job == nil || claimB.DispatchLease == nil || claimB.Job.ID != jobB.ID {
		t.Fatalf("claim Run B job = %+v, %v", claimB, err)
	}

	clock = clock.Add(2 * time.Second)
	recoveries, err := s.ScanExpiredDurableJobsForReconcile(ctx, ScanExpiredDurableJobsRequest{
		RunID: runA.ID, Actor: "recovery", Reason: "recover only selected Run",
	})
	if err != nil || len(recoveries) != 1 || recoveries[0].Job.ID != jobA.ID || recoveries[0].Job.State != JobInterrupted {
		t.Fatalf("scoped recovery = %+v, %v", recoveries, err)
	}
	otherJob, err := s.GetDurableJob(ctx, jobB.ID)
	if err != nil || otherJob == nil || otherJob.State != JobRunning {
		t.Fatalf("unselected Run job changed by scoped recovery: %+v, %v", otherJob, err)
	}
	otherLease, err := s.GetLease(ctx, claimB.DispatchLease.ID)
	if err != nil || otherLease == nil || otherLease.State != LeaseActive {
		t.Fatalf("unselected Run dispatch lease changed by scoped recovery: %+v, %v", otherLease, err)
	}
}

func TestV5ExpireLeasesForRunLeavesOtherRunLeaseUntouched(t *testing.T) {
	ctx := context.Background()
	s := tempV5DB(t)
	clock := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	task, revision := createValidatedTaskAndRevision(t, s)
	runA := createV5ReconcileRun(t, ctx, s, task, revision, "scoped-lease-a")
	runB := createV5ReconcileRun(t, ctx, s, task, revision, "scoped-lease-b")

	jobA, err := s.CreateDurableJob(ctx, CreateDurableJobRequest{
		CommandType: "execute", EntityType: "fixture", EntityID: "lease-a", RunID: runA.ID,
		PayloadJSON: `{}`, IdempotencyKey: "scoped-lease-job-a", Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimA, err := s.ClaimNextDurableJob(ctx, ClaimNextDurableJobRequest{
		IdempotencyKey: "scoped-lease-claim-a", Owner: "worker-a", LeaseTTL: time.Second, Actor: "tester", Reason: "fixture",
	})
	if err != nil || claimA.Job == nil || claimA.DispatchLease == nil || claimA.Job.ID != jobA.ID {
		t.Fatalf("claim Run A job = %+v, %v", claimA, err)
	}

	jobB, err := s.CreateDurableJob(ctx, CreateDurableJobRequest{
		CommandType: "execute", EntityType: "fixture", EntityID: "lease-b", RunID: runB.ID,
		PayloadJSON: `{}`, IdempotencyKey: "scoped-lease-job-b", Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimB, err := s.ClaimNextDurableJob(ctx, ClaimNextDurableJobRequest{
		IdempotencyKey: "scoped-lease-claim-b", Owner: "worker-b", LeaseTTL: time.Second, Actor: "tester", Reason: "fixture",
	})
	if err != nil || claimB.Job == nil || claimB.DispatchLease == nil || claimB.Job.ID != jobB.ID {
		t.Fatalf("claim Run B job = %+v, %v", claimB, err)
	}

	clock = clock.Add(2 * time.Second)
	expired, err := s.ExpireLeasesForRun(ctx, runA.ID, "recovery", "expire only selected Run leases")
	if err != nil || expired != 1 {
		t.Fatalf("expire selected Run leases = %d, %v", expired, err)
	}
	selectedLease, err := s.GetLease(ctx, claimA.DispatchLease.ID)
	if err != nil || selectedLease == nil || selectedLease.State != LeaseExpired {
		t.Fatalf("selected Run lease = %+v, %v", selectedLease, err)
	}
	otherLease, err := s.GetLease(ctx, claimB.DispatchLease.ID)
	if err != nil || otherLease == nil || otherLease.State != LeaseActive {
		t.Fatalf("unselected Run lease changed by scoped expiration: %+v, %v", otherLease, err)
	}
}

func TestV5ExpireQuotaLeasesForScopeLeavesOtherScopesUntouched(t *testing.T) {
	ctx := context.Background()
	s := tempV5DB(t)
	clock := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	taskA, _ := createValidatedTaskAndRevision(t, s)
	taskB, _ := createValidatedTaskAndRevision(t, s)
	const actorA = "scope-owner-a"
	const actorB = "scope-owner-b"

	for _, account := range []CreateQuotaAccountRequest{
		{ScopeKind: QuotaScopeTask, ScopeID: taskA.ID, Dimension: "token", LimitUnits: 10, Actor: actorA, Reason: "fixture"},
		{ScopeKind: QuotaScopeTask, ScopeID: taskB.ID, Dimension: "token", LimitUnits: 10, Actor: actorA, Reason: "fixture"},
		{ScopeKind: QuotaScopeActor, ScopeID: actorA, Dimension: "token", LimitUnits: 10, Actor: actorA, Reason: "fixture"},
		{ScopeKind: QuotaScopeActor, ScopeID: actorB, Dimension: "token", LimitUnits: 10, Actor: actorB, Reason: "fixture"},
	} {
		if _, err := s.CreateQuotaAccount(ctx, account); err != nil {
			t.Fatal(err)
		}
	}
	reserve := func(key, owner string, scopeKind QuotaScopeKind, scopeID string) DurableQuotaLease {
		t.Helper()
		lease, err := s.ReserveQuota(ctx, QuotaLeaseRequest{
			IdempotencyKey: key, Owner: owner, ScopeKind: scopeKind, ScopeID: scopeID, Dimension: "token", Units: 1,
			ReclaimPolicy: QuotaReclaimUnused, TTL: time.Second, Actor: owner, Reason: "fixture",
		})
		if err != nil {
			t.Fatal(err)
		}
		return lease
	}
	taskLeaseA := reserve("scoped-task-lease-a", actorA, QuotaScopeTask, taskA.ID)
	taskLeaseB := reserve("scoped-task-lease-b", actorA, QuotaScopeTask, taskB.ID)
	actorLeaseA := reserve("scoped-actor-lease-a", actorA, QuotaScopeActor, actorA)
	actorLeaseB := reserve("scoped-actor-lease-b", actorB, QuotaScopeActor, actorB)

	clock = clock.Add(2 * time.Second)
	expiredTask, err := s.ExpireQuotaLeasesForScope(ctx, QuotaScopeTask, taskA.ID, actorA, "recover selected Task quota")
	if err != nil || expiredTask != 1 {
		t.Fatalf("expire selected Task quota lease = %d, %v", expiredTask, err)
	}
	expiredActor, err := s.ExpireQuotaLeasesForScope(ctx, QuotaScopeActor, actorA, actorA, "recover selected actor quota")
	if err != nil || expiredActor != 1 {
		t.Fatalf("expire selected actor quota lease = %d, %v", expiredActor, err)
	}
	for _, expectation := range []struct {
		name  string
		lease DurableQuotaLease
		state DurableQuotaLeaseState
	}{
		{name: "selected task", lease: taskLeaseA, state: DurableQuotaLeaseExpired},
		{name: "other task", lease: taskLeaseB, state: DurableQuotaLeaseActive},
		{name: "selected actor", lease: actorLeaseA, state: DurableQuotaLeaseExpired},
		{name: "other actor", lease: actorLeaseB, state: DurableQuotaLeaseActive},
	} {
		t.Run(expectation.name, func(t *testing.T) {
			lease, err := s.GetDurableQuotaLease(ctx, expectation.lease.ID)
			if err != nil || lease == nil || lease.State != expectation.state {
				t.Fatalf("quota lease = %+v, %v; want state %s", lease, err, expectation.state)
			}
		})
	}
}

func createV5ReconcileRun(t *testing.T, ctx context.Context, s *Store, task TaskV2, revision TaskRevision, trigger string) WorkflowRun {
	t.Helper()
	run, err := s.CreateWorkflowRun(ctx, CreateWorkflowRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, WorkflowTemplateID: "harbor.standard", WorkflowTemplateVersion: "v5",
		ResolvedProfileHash: "profile", DefinitionHash: "workflow-fingerprint", RunManifestJSON: `{}`,
		Trigger: trigger, Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	return run
}
