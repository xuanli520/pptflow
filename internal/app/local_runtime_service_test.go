package app

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

func TestLocalRuntimeAttachRunProjectsOnlyValidLocalLeaseFacts(t *testing.T) {
	ctx := context.Background()
	root, services, task, _, run := newLocalRuntimeServiceFixture(t, "runtime-owner")
	claim := claimLocalRuntimeRunJob(t, ctx, services, run.ID, time.Minute)
	taskQuota, actorQuota := reserveLocalRuntimeQuota(t, ctx, services, task.ID, run.CreatedBy, time.Minute)

	beforeFiles := snapshotLocalRuntimeFiles(t, root)
	beforeJob, err := services.Store().GetDurableJob(ctx, claim.Job.ID)
	if err != nil || beforeJob == nil {
		t.Fatalf("read fixture job before attach: %+v, %v", beforeJob, err)
	}
	beforeLease, err := services.Store().GetLease(ctx, claim.DispatchLease.ID)
	if err != nil || beforeLease == nil {
		t.Fatalf("read fixture dispatch lease before attach: %+v, %v", beforeLease, err)
	}
	beforeTaskQuota, err := services.Store().GetDurableQuotaLease(ctx, taskQuota.ID)
	if err != nil || beforeTaskQuota == nil {
		t.Fatalf("read fixture task quota before attach: %+v, %v", beforeTaskQuota, err)
	}
	beforeActorQuota, err := services.Store().GetDurableQuotaLease(ctx, actorQuota.ID)
	if err != nil || beforeActorQuota == nil {
		t.Fatalf("read fixture actor quota before attach: %+v, %v", beforeActorQuota, err)
	}

	attachment, err := services.LocalRuntime.AttachRun(ctx, AttachRunRequest{RunID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	if attachment.Run.ID != run.ID || attachment.AttachableJobs != 1 || len(attachment.Jobs) != 1 || !attachment.Jobs[0].Attachable {
		t.Fatalf("attach projection = %+v", attachment)
	}
	if len(attachment.Jobs[0].Leases) != 1 || !attachment.Jobs[0].Leases[0].Valid ||
		attachment.Jobs[0].Leases[0].Lease.ResourceType != "job_dispatch" || attachment.Jobs[0].Leases[0].Lease.ResourceID != claim.Job.ID {
		t.Fatalf("attach lease projection = %+v", attachment.Jobs[0].Leases)
	}
	if attachment.TaskQuota.ScopeKind != store.QuotaScopeTask || attachment.TaskQuota.ScopeID != task.ID ||
		len(attachment.TaskQuota.Leases) != 1 || !attachment.TaskQuota.Leases[0].Valid {
		t.Fatalf("task quota attachment = %+v", attachment.TaskQuota)
	}
	if attachment.ActorQuota.ScopeKind != store.QuotaScopeActor || attachment.ActorQuota.ScopeID != run.CreatedBy ||
		len(attachment.ActorQuota.Leases) != 1 || !attachment.ActorQuota.Leases[0].Valid {
		t.Fatalf("actor quota attachment = %+v", attachment.ActorQuota)
	}

	afterJob, _ := services.Store().GetDurableJob(ctx, claim.Job.ID)
	afterLease, _ := services.Store().GetLease(ctx, claim.DispatchLease.ID)
	afterTaskQuota, _ := services.Store().GetDurableQuotaLease(ctx, taskQuota.ID)
	afterActorQuota, _ := services.Store().GetDurableQuotaLease(ctx, actorQuota.ID)
	if afterJob.Version != beforeJob.Version || afterLease.Version != beforeLease.Version ||
		afterTaskQuota.Version != beforeTaskQuota.Version || afterActorQuota.Version != beforeActorQuota.Version {
		t.Fatalf("attach mutated durable state: job %d/%d lease %d/%d task quota %d/%d actor quota %d/%d",
			beforeJob.Version, afterJob.Version, beforeLease.Version, afterLease.Version,
			beforeTaskQuota.Version, afterTaskQuota.Version, beforeActorQuota.Version, afterActorQuota.Version)
	}
	if afterFiles := snapshotLocalRuntimeFiles(t, root); !reflect.DeepEqual(afterFiles, beforeFiles) {
		t.Fatal("attach changed managed local files")
	}
}

func TestLocalRuntimeReconcileRunRecoversOnlyLocalDurableFacts(t *testing.T) {
	ctx := context.Background()
	_, services, task, _, run := newLocalRuntimeServiceFixture(t, "runtime-reconciler")
	claim := claimLocalRuntimeRunJob(t, ctx, services, run.ID, 10*time.Millisecond)
	taskQuota, actorQuota := reserveLocalRuntimeQuota(t, ctx, services, task.ID, run.CreatedBy, 10*time.Millisecond)

	checkpoint, err := services.Control.CurrentCheckpoint(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	control, err := services.Control.Request(ctx, RequestExecutionControlRequest{
		OperationKey: "local-runtime-reconcile-control", Action: store.ControlActionPause, RunID: run.ID,
		Expected: checkpoint, Actor: run.CreatedBy, Reason: "fixture unresolved pause",
	})
	if err != nil || control.Status != store.ControlOperationRequested {
		t.Fatalf("create unresolved control = %+v, %v", control, err)
	}

	sideEffect, err := services.Store().CreateSideEffectOperation(ctx, store.CreateSideEffectOperationRequest{
		OperationKey: "local-runtime-side-effect", IdempotencyKey: "local-runtime-side-effect-key", RunID: run.ID,
		EffectKind: "external_provider_fixture", SourceDigest: "sha256:fixture", PayloadJSON: `{}`,
		Actor: run.CreatedBy, Reason: "fixture unknown external operation",
	})
	if err != nil {
		t.Fatal(err)
	}
	sideEffect, err = services.Store().TransitionSideEffectOperation(ctx, store.TransitionSideEffectOperationRequest{
		OperationID: sideEffect.ID, ExpectedVersion: sideEffect.Version, State: store.SideEffectStarted,
		Actor: run.CreatedBy, Reason: "fixture started external operation",
	})
	if err != nil {
		t.Fatal(err)
	}
	sideEffect, err = services.Store().TransitionSideEffectOperation(ctx, store.TransitionSideEffectOperationRequest{
		OperationID: sideEffect.ID, ExpectedVersion: sideEffect.Version, State: store.SideEffectUnknown,
		Actor: run.CreatedBy, Reason: "fixture lost external operation receipt",
	})
	if err != nil {
		t.Fatal(err)
	}
	sideEffectVersion := sideEffect.Version

	// Store time is not injectable through the application API. A bounded wait
	// makes all three short local leases stale before the recovery transaction.
	time.Sleep(40 * time.Millisecond)
	result, err := services.LocalRuntime.ReconcileRun(ctx, ReconcileRunRequest{
		RunID: run.ID, Actor: "runtime-reconciler", Reason: "recover selected local durable Run",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.RecoveredJobs) != 1 || result.RecoveredJobs[0].Job.ID != claim.Job.ID || result.RecoveredJobs[0].Job.State != store.JobInterrupted {
		t.Fatalf("local job recovery = %+v", result.RecoveredJobs)
	}
	if result.ExpiredTaskQuotas != 1 || result.ExpiredActorQuotas != 1 {
		t.Fatalf("scoped quota recovery = task:%d actor:%d", result.ExpiredTaskQuotas, result.ExpiredActorQuotas)
	}
	if !containsLocalRuntimeControl(result.UnresolvedControls, control.ID) {
		t.Fatalf("reconcile result omitted unresolved control %s: %+v", control.ID, result.UnresolvedControls)
	}
	if !containsLocalRuntimeQuotaLease(result.UnresolvedQuotaLeases, taskQuota.ID) || !containsLocalRuntimeQuotaLease(result.UnresolvedQuotaLeases, actorQuota.ID) {
		t.Fatalf("reconcile result omitted expired local quotas: %+v", result.UnresolvedQuotaLeases)
	}
	if result.Attachment.AttachableJobs != 0 || len(result.Attachment.Jobs) != 1 || result.Attachment.Jobs[0].Attachable {
		t.Fatalf("reconciled attachment retained attachable stale work: %+v", result.Attachment)
	}

	job, err := services.Store().GetDurableJob(ctx, claim.Job.ID)
	if err != nil || job == nil || job.State != store.JobInterrupted {
		t.Fatalf("reconciled job = %+v, %v", job, err)
	}
	dispatchLease, err := services.Store().GetLease(ctx, claim.DispatchLease.ID)
	if err != nil || dispatchLease == nil || dispatchLease.State != store.LeaseExpired {
		t.Fatalf("reconciled dispatch lease = %+v, %v", dispatchLease, err)
	}
	updatedControl, err := services.Control.Get(ctx, control.ID)
	if err != nil || updatedControl.Status != store.ControlOperationReconcileRequired {
		t.Fatalf("reconciled control = %+v, %v", updatedControl, err)
	}
	for _, leaseID := range []string{taskQuota.ID, actorQuota.ID} {
		lease, err := services.Store().GetDurableQuotaLease(ctx, leaseID)
		if err != nil || lease == nil || lease.State != store.DurableQuotaLeaseExpired {
			t.Fatalf("reconciled quota %s = %+v, %v", leaseID, lease, err)
		}
	}
	unchangedSideEffect, err := services.Store().GetSideEffectOperation(ctx, sideEffect.ID)
	if err != nil || unchangedSideEffect == nil || unchangedSideEffect.State != store.SideEffectUnknown || unchangedSideEffect.Version != sideEffectVersion {
		t.Fatalf("generic Run reconciliation altered external side effect: %+v, %v", unchangedSideEffect, err)
	}
}

func TestLocalRuntimeReconcileRunDrainsEveryExpiredJobBatch(t *testing.T) {
	ctx := context.Background()
	_, services, _, _, run := newLocalRuntimeServiceFixture(t, "runtime-batch-reconciler")
	const expectedJobs = localRuntimeReconcileBatchSize + 1
	claims := make([]store.DurableJobDispatchClaim, 0, expectedJobs)
	claims = append(claims, claimLocalRuntimeRunJob(t, ctx, services, run.ID, 10*time.Millisecond))
	for ordinal := 1; ordinal < expectedJobs; ordinal++ {
		job, err := services.Store().CreateDurableJob(ctx, store.CreateDurableJobRequest{
			CommandType: "workflow_run.execute", EntityType: "workflow_run", EntityID: run.ID, RunID: run.ID,
			PayloadJSON: `{}`, IdempotencyKey: "local-runtime-batch-job-" + run.ID + "-" + strconv.Itoa(ordinal),
			Actor: "runtime-batch-reconciler", Reason: "create batch reconcile fixture job",
		})
		if err != nil {
			t.Fatal(err)
		}
		claim, err := services.Store().ClaimNextDurableJob(ctx, store.ClaimNextDurableJobRequest{
			IdempotencyKey: "local-runtime-batch-claim-" + run.ID + "-" + strconv.Itoa(ordinal), Owner: "runtime-batch-worker", LeaseTTL: 10 * time.Millisecond,
			Actor: "runtime-batch-reconciler", Reason: "claim batch reconcile fixture job",
		})
		if err != nil || claim.Job == nil || claim.DispatchLease == nil || claim.Job.ID != job.ID {
			t.Fatalf("claim batch job %d = %+v, %v", ordinal, claim, err)
		}
		claims = append(claims, claim)
	}

	time.Sleep(40 * time.Millisecond)
	result, err := services.LocalRuntime.ReconcileRun(ctx, ReconcileRunRequest{
		RunID: run.ID, Actor: "runtime-batch-reconciler", Reason: "drain every selected Run recovery batch",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.RecoveredJobs) != expectedJobs {
		t.Fatalf("recovered %d jobs, want all %d", len(result.RecoveredJobs), expectedJobs)
	}
	recovered := make(map[string]store.JobState, len(result.RecoveredJobs))
	for _, recovery := range result.RecoveredJobs {
		recovered[recovery.Job.ID] = recovery.Job.State
	}
	for _, claim := range claims {
		if recovered[claim.Job.ID] != store.JobInterrupted {
			t.Fatalf("batch job %s was not recovered: %+v", claim.Job.ID, recovered)
		}
	}
}

func TestLocalRuntimeAttachAndReconcileProjectScopedWorkerHandoffs(t *testing.T) {
	ctx := context.Background()
	_, services, _, _, run := newLocalRuntimeServiceFixture(t, "runtime-handoff")
	operationID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	key, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := services.Store().ReserveRunWorkerHandoff(ctx, store.ReserveRunWorkerHandoffRequest{
		ID: operationID, IdempotencyKey: key, RequestFingerprint: "sha256:runtime-handoff-attach",
		RunID: run.ID, ExpectedRunVersion: run.Version, ExpectedRunExecutionEpoch: run.ExecutionEpoch,
		ExpectedRunDefinitionHash: run.DefinitionHash, Owner: "runtime-handoff-owner", Actor: "runtime-handoff",
		Reason: "create local runtime handoff fixture", LaunchTTL: 10 * time.Millisecond,
	})
	if err != nil || !reserved.Launch || reserved.Handoff.State != store.RunWorkerHandoffLaunching {
		t.Fatalf("reserve controlled worker handoff = %+v, %v", reserved, err)
	}

	attachment, err := services.LocalRuntime.AttachRun(ctx, AttachRunRequest{RunID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(attachment.WorkerHandoffs) != 1 || attachment.WorkerHandoffs[0].ID != operationID || attachment.WorkerHandoffs[0].State != store.RunWorkerHandoffLaunching {
		t.Fatalf("attached worker handoffs = %+v", attachment.WorkerHandoffs)
	}

	time.Sleep(40 * time.Millisecond)
	reconciled, err := services.LocalRuntime.ReconcileRun(ctx, ReconcileRunRequest{
		RunID: run.ID, Actor: "runtime-handoff", Reason: "reconcile expired controlled worker handoff",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(reconciled.ExpiredWorkerHandoffs) != 1 || reconciled.ExpiredWorkerHandoffs[0].ID != operationID || reconciled.ExpiredWorkerHandoffs[0].State != store.RunWorkerHandoffExpired {
		t.Fatalf("expired worker handoffs = %+v", reconciled.ExpiredWorkerHandoffs)
	}
	if len(reconciled.Attachment.WorkerHandoffs) != 1 || reconciled.Attachment.WorkerHandoffs[0].ID != operationID || reconciled.Attachment.WorkerHandoffs[0].State != store.RunWorkerHandoffExpired {
		t.Fatalf("reconciled worker handoff attachment = %+v", reconciled.Attachment.WorkerHandoffs)
	}
}

func TestLocalRuntimeReconcileExpiresClaimedWorkerHandoffWithItsSupervisorLease(t *testing.T) {
	ctx := context.Background()
	_, services, _, _, run := newLocalRuntimeServiceFixture(t, "runtime-claimed-handoff")
	operationID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	key, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := services.Store().ReserveRunWorkerHandoff(ctx, store.ReserveRunWorkerHandoffRequest{
		ID: operationID, IdempotencyKey: key, RequestFingerprint: "sha256:runtime-claimed-handoff",
		RunID: run.ID, ExpectedRunVersion: run.Version, ExpectedRunExecutionEpoch: run.ExecutionEpoch,
		ExpectedRunDefinitionHash: run.DefinitionHash, Owner: "runtime-claimed-handoff-owner", Actor: "runtime-claimed-handoff",
		Reason: "create claimed local runtime handoff fixture", LaunchTTL: time.Minute,
	})
	if err != nil || !reserved.Launch {
		t.Fatalf("reserve claimed worker handoff = %+v, %v", reserved, err)
	}
	claim, err := services.Store().ClaimRunWorkerHandoff(ctx, store.ClaimRunWorkerHandoffRequest{
		OperationID: operationID, RunID: run.ID, Owner: "runtime-claimed-handoff-owner", ProcessID: 8181,
		LogPath: "/managed/runtime-claimed-handoff.log", LeaseTTL: time.Second,
		Actor: "runtime-claimed-handoff", Reason: "claim local runtime handoff fixture",
	})
	if err != nil || claim.Handoff.State != store.RunWorkerHandoffHandedOff || claim.WorkerLease.State != store.LeaseActive {
		t.Fatalf("claim controlled worker handoff = %+v, %v", claim, err)
	}

	attachment, err := services.LocalRuntime.AttachRun(ctx, AttachRunRequest{RunID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(attachment.WorkerHandoffs) != 1 || attachment.WorkerHandoffs[0].State != store.RunWorkerHandoffHandedOff || len(attachment.WorkerLeases) != 1 || !attachment.WorkerLeases[0].Valid {
		t.Fatalf("claimed worker attachment = %+v", attachment)
	}

	time.Sleep(1500 * time.Millisecond)
	reconciled, err := services.LocalRuntime.ReconcileRun(ctx, ReconcileRunRequest{
		RunID: run.ID, Actor: "runtime-claimed-handoff", Reason: "reconcile claimed worker lease loss",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(reconciled.ExpiredWorkerHandoffs) != 1 || reconciled.ExpiredWorkerHandoffs[0].ID != operationID || reconciled.ExpiredWorkerHandoffs[0].State != store.RunWorkerHandoffExpired {
		t.Fatalf("expired claimed worker handoffs = %+v", reconciled.ExpiredWorkerHandoffs)
	}
	if len(reconciled.Attachment.WorkerHandoffs) != 1 || reconciled.Attachment.WorkerHandoffs[0].State != store.RunWorkerHandoffExpired || len(reconciled.Attachment.WorkerLeases) != 1 || reconciled.Attachment.WorkerLeases[0].Lease.State != store.LeaseExpired {
		t.Fatalf("claimed worker reconcile attachment = %+v", reconciled.Attachment)
	}
}

func newLocalRuntimeServiceFixture(t *testing.T, actor string) (string, *LifecycleServices, store.TaskV2, store.TaskRevision, store.WorkflowRun) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	services, err := newLifecycleServicesForTest(root, database)
	if err != nil {
		t.Fatal(err)
	}
	task, revision, err := services.Tasks.ImportTask(ctx, ImportTaskRequest{
		CreateDraftTaskRequest: CreateDraftTaskRequest{Slug: "local-runtime-" + actor, Actor: actor, Reason: "local runtime fixture"},
		SourceDirectory:        writeLifecycleSnapshot(t, "local runtime fixture\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := services.Runs.StartRun(ctx, StartRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, Profile: lifecycleCompleteProfile(t), ExecutionSpec: lifecycleExecutionSpec(task.ID, revision.ID, revision.TaskDigest), Trigger: "local-runtime-fixture", Actor: actor, Reason: "start local runtime fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err = services.Store().TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunRunning, Actor: actor, Reason: "start local runtime worker",
	})
	if err != nil {
		t.Fatal(err)
	}
	return root, services, task, revision, run
}

func claimLocalRuntimeRunJob(t *testing.T, ctx context.Context, services *LifecycleServices, runID string, ttl time.Duration) store.DurableJobDispatchClaim {
	t.Helper()
	jobs, err := services.Store().ListDurableJobsForRun(ctx, runID)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("list initial durable job = %+v, %v", jobs, err)
	}
	claim, err := services.Store().ClaimNextDurableJob(ctx, store.ClaimNextDurableJobRequest{
		IdempotencyKey: "local-runtime-claim-" + runID, Owner: "local-runtime-worker", LeaseTTL: ttl,
		Actor: "local-runtime-worker", Reason: "claim local runtime fixture",
	})
	if err != nil || claim.Job == nil || claim.DispatchLease == nil || claim.Job.ID != jobs[0].ID {
		t.Fatalf("claim local runtime job = %+v, %v", claim, err)
	}
	return claim
}

func reserveLocalRuntimeQuota(t *testing.T, ctx context.Context, services *LifecycleServices, taskID, actor string, ttl time.Duration) (store.DurableQuotaLease, store.DurableQuotaLease) {
	t.Helper()
	for _, account := range []store.CreateQuotaAccountRequest{
		{ScopeKind: store.QuotaScopeTask, ScopeID: taskID, Dimension: "local_runtime", LimitUnits: 10, Actor: actor, Reason: "local runtime fixture"},
		{ScopeKind: store.QuotaScopeActor, ScopeID: actor, Dimension: "local_runtime", LimitUnits: 10, Actor: actor, Reason: "local runtime fixture"},
	} {
		if _, err := services.Store().CreateQuotaAccount(ctx, account); err != nil {
			t.Fatal(err)
		}
	}
	reserve := func(scopeKind store.QuotaScopeKind, scopeID, key string) store.DurableQuotaLease {
		t.Helper()
		lease, err := services.Store().ReserveQuota(ctx, store.QuotaLeaseRequest{
			IdempotencyKey: key, Owner: "local-runtime-worker", ScopeKind: scopeKind, ScopeID: scopeID, Dimension: "local_runtime", Units: 1,
			ReclaimPolicy: store.QuotaReclaimUnused, TTL: ttl, Actor: actor, Reason: "reserve local runtime quota",
		})
		if err != nil {
			t.Fatal(err)
		}
		return lease
	}
	return reserve(store.QuotaScopeTask, taskID, "local-runtime-task-quota-"+taskID), reserve(store.QuotaScopeActor, actor, "local-runtime-actor-quota-"+actor+"-"+taskID)
}

func containsLocalRuntimeControl(operations []store.DurableControlOperation, operationID string) bool {
	for _, operation := range operations {
		if operation.ID == operationID && operation.Status == store.ControlOperationReconcileRequired {
			return true
		}
	}
	return false
}

func containsLocalRuntimeQuotaLease(leases []store.DurableQuotaLease, leaseID string) bool {
	for _, lease := range leases {
		if lease.ID == leaseID && lease.State == store.DurableQuotaLeaseExpired {
			return true
		}
	}
	return false
}

func snapshotLocalRuntimeFiles(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files := make(map[string][]byte)
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		if path == filepath.Join(root, "harbor.db-wal") || path == filepath.Join(root, "harbor.db-shm") {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(relative)] = content
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
