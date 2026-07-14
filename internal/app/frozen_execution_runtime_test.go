package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	runtimeFixtureSourceStage = workflowkit.StageKey("runtime-source")
	runtimeFixtureVerifyStage = workflowkit.StageKey("runtime-verify")
	runtimeFixtureActor       = "runtime-test-actor"
)

type frozenRuntimeFixture struct {
	store    *store.Store
	services *LifecycleServices
	task     store.TaskV2
	revision store.TaskRevision
	run      store.WorkflowRun
	frozen   frozenRunDefinition
}

func TestFrozenExecutionRuntimeCompletesFrozenRunWithArtifactLineageAndQuotaSettlement(t *testing.T) {
	ctx := context.Background()
	fixture := newFrozenRuntimeFixture(t)
	defer fixture.store.Close()

	var charged int
	runtime := newFrozenRuntime(t, fixture.services, frozenRuntimeRegistry(t, fixture.frozen.Workflow, func(ctx context.Context, request workflowkit.StageExecutionRequest) (workflowkit.StageExecutionResult, error) {
		if request.Stage.Key == runtimeFixtureSourceStage {
			if request.ReadInput == nil {
				return workflowkit.StageExecutionResult{}, fmt.Errorf("source stage did not receive frozen input reader")
			}
			if err := request.Charge(ctx, workflowkit.StageUsage{
				Dimension: "agent_turn", Units: 1, OperationKey: "source-turn-1", OccurredAt: time.Now().UTC(),
			}); err != nil {
				return workflowkit.StageExecutionResult{}, err
			}
			charged++
		}
		return completedFixtureStage(ctx, request)
	}))
	worker := newFrozenRuntimeWorker(t, fixture.store, runtime, "runtime-complete-worker")

	run := driveFrozenRuntimeToTerminal(t, ctx, worker, fixture.store, fixture.run.ID)
	if run.Status != store.WorkflowRunSucceeded {
		t.Fatalf("run status = %s, want %s", run.Status, store.WorkflowRunSucceeded)
	}
	if charged != 1 {
		t.Fatalf("executor agent-turn charges = %d, want 1", charged)
	}

	attempts, err := fixture.store.ListStageAttemptsForRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 {
		t.Fatalf("stage attempts = %+v, want exactly two", attempts)
	}
	for _, attempt := range attempts {
		if attempt.ExecutionStatus != store.StageExecutionCompleted || attempt.Verdict != store.VerdictPass || attempt.ArtifactManifestID == "" {
			t.Fatalf("stage attempt terminal projection = %+v", attempt)
		}
		references, err := fixture.store.ListArtifactRefs(ctx, attempt.ArtifactManifestID)
		if err != nil {
			t.Fatal(err)
		}
		if len(references) != 1 || references[0].RunID != run.ID || references[0].AttemptID != attempt.ID {
			t.Fatalf("artifact lineage for stage %s = %+v", attempt.StageKey, references)
		}
	}

	assertRuntimeQuotaAccount(t, ctx, fixture.store, store.QuotaScopeTask, fixture.task.ID, "stage_attempt", 2, 0)
	assertRuntimeQuotaAccount(t, ctx, fixture.store, store.QuotaScopeActor, runtimeFixtureActor, "stage_attempt", 2, 0)
	assertRuntimeQuotaAccount(t, ctx, fixture.store, store.QuotaScopeTask, fixture.task.ID, "agent_turn", 1, 0)
	assertRuntimeQuotaAccount(t, ctx, fixture.store, store.QuotaScopeActor, runtimeFixtureActor, "agent_turn", 1, 0)

	for _, scope := range []struct {
		kind store.QuotaScopeKind
		id   string
	}{{store.QuotaScopeTask, fixture.task.ID}, {store.QuotaScopeActor, runtimeFixtureActor}} {
		leases, err := fixture.store.ListDurableQuotaLeasesForScope(ctx, scope.kind, scope.id)
		if err != nil {
			t.Fatal(err)
		}
		if len(leases) != 3 {
			t.Fatalf("%s quota leases = %+v, want source stage_attempt + agent_turn and verify stage_attempt", scope.kind, leases)
		}
		for _, lease := range leases {
			if lease.State != store.DurableQuotaLeaseSettled || lease.RemainingUnits() != 0 {
				t.Fatalf("unsettled quota lease = %+v", lease)
			}
		}
	}
}

func TestFrozenExecutionRuntimeUsesPublicWorkflowkitEngineAndRetainsFailedEvidence(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dataStore, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	services, err := newLifecycleServicesForTest(root, dataStore)
	if err != nil {
		t.Fatal(err)
	}
	task, revision, err := services.Tasks.ImportTask(ctx, ImportTaskRequest{
		CreateDraftTaskRequest: CreateDraftTaskRequest{Slug: "public-engine-bridge", Title: "Public Engine Bridge", Actor: runtimeFixtureActor, Reason: "create public Engine bridge fixture"},
		SourceDirectory:        writeLifecycleSnapshot(t, "public engine bridge fixture\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := services.Runs.StartRun(ctx, StartRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, Profile: lifecycleCompleteProfile(t), ExecutionSpec: lifecycleExecutionSpec(task.ID, revision.ID, revision.TaskDigest),
		Trigger: "public-engine-bridge", Actor: runtimeFixtureActor, Reason: "start public Engine bridge fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := decodeFrozenRunDefinition(run)
	if err != nil {
		t.Fatal(err)
	}
	stage, found := frozen.Workflow.Stage(workflowkit.StageKey(workflowadapter.RepoPrepare))
	if !found {
		t.Fatal("standard workflow omitted repo_prepare")
	}
	var observed workflowkit.StageExecutionRequest
	registry, err := workflowkit.NewControlledPluginRegistry([]workflowkit.PluginRegistration[workflowkit.StageExecutor]{
		{Binding: stage.Plugin, Implementation: workflowkit.StageExecutorFunc(func(_ context.Context, request workflowkit.StageExecutionRequest) (workflowkit.StageExecutionResult, error) {
			observed = request
			return workflowkit.StageExecutionResult{
				Outcome:   workflowkit.Outcome{Status: workflowkit.StatusInfraFailed, Failure: workflowkit.FailurePermanent},
				Artifacts: []workflowkit.StageArtifact{{Name: stage.Outputs[0].Name, SchemaVersion: stage.Outputs[0].SchemaVersion, Content: []byte("provider diagnostic evidence\n"), TurnOrdinal: 1}},
				ErrorText: "controlled provider reported a permanent diagnostic failure",
			}, nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := NewFrozenExecutionRuntime(FrozenExecutionRuntimeConfig{
		Services: services, WorkflowkitRegistry: registry, QuotaLeaseTTL: time.Second, ControlPollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	worker := newFrozenRuntimeWorker(t, dataStore, runtime, "public-engine-bridge-worker")
	if result, err := worker.RunOnce(ctx); err != nil || result.FinalState != store.JobSucceeded {
		t.Fatalf("initial public Engine coordinator = %+v, %v", result, err)
	}
	result, err := worker.RunOnce(ctx)
	if err != nil || result.FinalState != store.JobSucceeded {
		t.Fatalf("public Engine stage execution = %+v, %v", result, err)
	}
	if observed.Execution.ID != run.ID || observed.Execution.Binding.Format != workflowadapter.RunExecutionSpecFormat || observed.Execution.Binding.Version != workflowadapter.RunExecutionSpecVersion || observed.Claim.Stage == nil || observed.Stage.Key != stage.Key {
		t.Fatalf("public Engine received an incomplete frozen stage claim: %+v", observed)
	}
	attempts, err := dataStore.ListStageAttemptsForRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].ExecutionStatus != store.StageExecutionInfraFailed || attempts[0].ArtifactManifestID == "" {
		t.Fatalf("failed public Engine stage projection = %+v", attempts)
	}
	references, err := dataStore.ListArtifactRefs(ctx, attempts[0].ArtifactManifestID)
	if err != nil || len(references) != 1 || references[0].ArtifactKey != stage.Outputs[0].Name {
		t.Fatalf("failed public Engine diagnostic evidence = %+v, %v", references, err)
	}
	updated, err := dataStore.GetWorkflowRun(ctx, run.ID)
	if err != nil || updated == nil || updated.Status != store.WorkflowRunFailedRecoverable {
		t.Fatalf("public Engine failed run status = %+v, %v", updated, err)
	}
}

func TestFrozenExecutionRuntimeMarksAdmittedQuotaUncertainOnPostAdmissionIntegrityFailure(t *testing.T) {
	ctx := context.Background()
	fixture := newFrozenRuntimeFixture(t)
	defer fixture.store.Close()

	runtime := newFrozenRuntime(t, fixture.services, frozenRuntimeRegistry(t, fixture.frozen.Workflow, func(ctx context.Context, request workflowkit.StageExecutionRequest) (workflowkit.StageExecutionResult, error) {
		// Simulate a concurrent integrity writer after the node has started.
		// The runtime's later terminal transition must detect the stale node
		// version and retain the already-admitted quota for reconciliation.
		if request.Claim.Stage == nil {
			return workflowkit.StageExecutionResult{}, errors.New("public Engine stage claim is absent")
		}
		nodes, err := fixture.store.ListNodeAttempts(ctx, string(request.Claim.Stage.StageAttempt.ID))
		if err != nil || len(nodes) != 1 {
			return workflowkit.StageExecutionResult{}, fmt.Errorf("read runtime node attempt: nodes=%+v err=%w", nodes, err)
		}
		node := nodes[0]
		if _, err := fixture.store.TransitionNodeAttempt(ctx, store.TransitionNodeAttemptRequest{
			NodeAttemptID: node.ID, ExpectedVersion: node.Version, Status: store.NodeAttemptInDoubt,
			ErrorText: "simulate concurrent node integrity failure", Actor: runtimeFixtureActor, Reason: "runtime integrity fixture",
		}); err != nil {
			return workflowkit.StageExecutionResult{}, err
		}
		return completedFixtureStage(ctx, request)
	}))
	worker := newFrozenRuntimeWorker(t, fixture.store, runtime, "runtime-post-admission-integrity")

	if result, err := worker.RunOnce(ctx); err != nil || result.FinalState != store.JobSucceeded {
		t.Fatalf("initial coordinator result = %+v, %v", result, err)
	}
	result, err := worker.RunOnce(ctx)
	if !errors.Is(err, store.ErrOptimisticLock) || result.FinalState != store.JobFailed {
		t.Fatalf("post-admission integrity result = %+v, %v", result, err)
	}

	stageJob, payload := requireRuntimeStageJob(t, ctx, fixture.store, fixture.run.ID, runtimeFixtureSourceStage)
	if result.Job == nil || result.Job.ID != stageJob.ID || payload.StageAttemptID == "" {
		t.Fatalf("failed stage job projection = %+v", result)
	}
	attempt, err := fixture.store.GetStageAttempt(ctx, payload.StageAttemptID)
	if err != nil || attempt == nil || attempt.ExecutionStatus != store.StageExecutionInDoubt {
		t.Fatalf("stage integrity projection = %+v, %v", attempt, err)
	}
	run, err := fixture.store.GetWorkflowRun(ctx, fixture.run.ID)
	if err != nil || run == nil || run.Status != store.WorkflowRunInDoubt {
		t.Fatalf("run integrity projection = %+v, %v", run, err)
	}
	for _, scope := range []struct {
		kind store.QuotaScopeKind
		id   string
	}{{store.QuotaScopeTask, fixture.task.ID}, {store.QuotaScopeActor, runtimeFixtureActor}} {
		leases, err := fixture.store.ListDurableQuotaLeasesForScope(ctx, scope.kind, scope.id)
		if err != nil || len(leases) != 2 {
			t.Fatalf("admitted quota leases for %s/%s = %+v, %v", scope.kind, scope.id, leases, err)
		}
		for _, lease := range leases {
			if lease.State != store.DurableQuotaLeaseUncertain {
				t.Fatalf("post-integrity lease = %+v, want uncertain", lease)
			}
		}
	}
}

func TestFrozenExecutionRuntimeStageAttemptGateSurvivesTwoWorkerCoordinatorRace(t *testing.T) {
	ctx := context.Background()
	fixture := newFrozenRuntimeFixture(t)
	defer fixture.store.Close()
	runtime := newFrozenRuntime(t, fixture.services, frozenRuntimeRegistry(t, fixture.frozen.Workflow, completedFixtureStage))

	firstWorker := newFrozenRuntimeWorker(t, fixture.store, runtime, "runtime-race-coordinator")
	if result, err := firstWorker.RunOnce(ctx); err != nil || result.FinalState != store.JobSucceeded {
		t.Fatalf("initial coordinator result = %+v, %v", result, err)
	}
	sourceJob, sourcePayload := requireRuntimeStageJob(t, ctx, fixture.store, fixture.run.ID, runtimeFixtureSourceStage)
	claim, err := fixture.store.ClaimNextDurableJob(ctx, store.ClaimNextDurableJobRequest{
		IdempotencyKey: "runtime-race-stage-claim", Owner: "runtime-race-stage-owner", LeaseTTL: time.Minute,
		Actor: runtimeFixtureActor, Reason: "hold predecessor stage job open for handoff race",
	})
	if err != nil || claim.Job == nil || claim.Job.ID != sourceJob.ID || claim.Job.State != store.JobRunning {
		t.Fatalf("claim predecessor stage job = %+v, %v", claim, err)
	}
	markRuntimeStageCompleted(t, ctx, fixture.store, sourcePayload.StageAttemptID)

	if err := runtime.enqueueNextCoordinator(ctx, *claim.Job, fixture.run, fixture.frozen, sourcePayload); err != nil {
		t.Fatal(err)
	}
	secondWorker := newFrozenRuntimeWorker(t, fixture.store, runtime, "runtime-race-next-coordinator")
	result, err := secondWorker.RunOnce(ctx)
	if err != nil || result.FinalState != store.JobSucceeded || result.Job == nil || result.Job.CommandType != "workflow_run.execute" {
		t.Fatalf("second worker coordinator result = %+v, %v", result, err)
	}

	persistedPredecessor, err := fixture.store.GetDurableJob(ctx, sourceJob.ID)
	if err != nil || persistedPredecessor == nil || persistedPredecessor.State != store.JobRunning {
		t.Fatalf("predecessor job was not held running during coordinator race: %+v, %v", persistedPredecessor, err)
	}
	verifyJob, _ := requireRuntimeStageJob(t, ctx, fixture.store, fixture.run.ID, runtimeFixtureVerifyStage)
	if verifyJob.State != store.JobQueued {
		t.Fatalf("next stage was not durably queued by racing coordinator: %+v", verifyJob)
	}
}

func TestFrozenExecutionRuntimeRecoveryRestoresMissingStageHandoffAfterExpiredLease(t *testing.T) {
	ctx := context.Background()
	fixture := newFrozenRuntimeFixture(t)
	defer fixture.store.Close()
	runtime := newFrozenRuntime(t, fixture.services, frozenRuntimeRegistry(t, fixture.frozen.Workflow, completedFixtureStage))

	initial := newFrozenRuntimeWorker(t, fixture.store, runtime, "runtime-recovery-initial")
	if result, err := initial.RunOnce(ctx); err != nil || result.FinalState != store.JobSucceeded {
		t.Fatalf("initial coordinator result = %+v, %v", result, err)
	}
	sourceJob, sourcePayload := requireRuntimeStageJob(t, ctx, fixture.store, fixture.run.ID, runtimeFixtureSourceStage)
	claim, err := fixture.store.ClaimNextDurableJob(ctx, store.ClaimNextDurableJobRequest{
		IdempotencyKey: "runtime-recovery-stage-claim", Owner: "runtime-recovery-lost-owner", LeaseTTL: 10 * time.Millisecond,
		Actor: runtimeFixtureActor, Reason: "simulate crash after durable stage terminal projection",
	})
	if err != nil || claim.Job == nil || claim.Job.ID != sourceJob.ID {
		t.Fatalf("claim stage for recovery fixture = %+v, %v", claim, err)
	}
	markRuntimeStageCompleted(t, ctx, fixture.store, sourcePayload.StageAttemptID)
	time.Sleep(40 * time.Millisecond)

	recoveryWorker := newFrozenRuntimeWorker(t, fixture.store, runtime, "runtime-recovery-worker")
	result, err := recoveryWorker.RunOnce(ctx)
	if err != nil {
		t.Fatalf("recovery worker result = %+v, %v", result, err)
	}
	if len(result.Recoveries) != 1 || result.Recoveries[0].Job.ID != sourceJob.ID || result.Recoveries[0].Job.State != store.JobInterrupted {
		t.Fatalf("recovery facts = %+v", result.Recoveries)
	}
	if result.FinalState != store.JobSucceeded || result.Job == nil || result.Job.CommandType != "workflow_run.execute" {
		t.Fatalf("recovery did not claim restored coordinator = %+v", result)
	}
	verifyJob, _ := requireRuntimeStageJob(t, ctx, fixture.store, fixture.run.ID, runtimeFixtureVerifyStage)
	if verifyJob.State != store.JobQueued {
		t.Fatalf("recovery did not queue successor stage: %+v", verifyJob)
	}

	// The same recovery facts are safe to replay. The handoff idempotency key
	// is tied to the recovered predecessor job and cannot make a second stage.
	if err := runtime.ReconcileDurableJobRecoveries(ctx, result.Recoveries); err != nil {
		t.Fatal(err)
	}
	jobs, err := fixture.store.ListDurableJobsForRun(ctx, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	matching := 0
	for _, job := range jobs {
		if job.IdempotencyKey == "workflow-run-next:"+fixture.run.ID+":"+sourceJob.ID {
			matching++
		}
	}
	if matching != 1 {
		t.Fatalf("recovered handoff jobs = %d, want 1", matching)
	}
}

func TestFrozenExecutionRuntimeRecoveryProjectsNonProgressingTerminalStageOutcomes(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		status    store.StageExecutionStatus
		verdict   store.Verdict
		wantRun   store.WorkflowRunStatus
		wantEmpty bool
	}{
		{name: "needs repair", status: store.StageExecutionCompleted, verdict: store.VerdictNeedsRepair, wantRun: store.WorkflowRunWaitingContinuation, wantEmpty: true},
		{name: "infrastructure failure", status: store.StageExecutionInfraFailed, wantRun: store.WorkflowRunFailedRecoverable, wantEmpty: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx := context.Background()
			fixture := newFrozenRuntimeFixture(t)
			defer fixture.store.Close()
			runtime := newFrozenRuntime(t, fixture.services, frozenRuntimeRegistry(t, fixture.frozen.Workflow, completedFixtureStage))
			initial := newFrozenRuntimeWorker(t, fixture.store, runtime, "runtime-recovery-outcome-initial")
			if result, err := initial.RunOnce(ctx); err != nil || result.FinalState != store.JobSucceeded {
				t.Fatalf("initial coordinator result = %+v, %v", result, err)
			}
			sourceJob, sourcePayload := requireRuntimeStageJob(t, ctx, fixture.store, fixture.run.ID, runtimeFixtureSourceStage)
			claim, err := fixture.store.ClaimNextDurableJob(ctx, store.ClaimNextDurableJobRequest{
				IdempotencyKey: "runtime-recovery-outcome-" + strings.ReplaceAll(scenario.name, " ", "-"), Owner: "runtime-recovery-outcome-owner", LeaseTTL: 10 * time.Millisecond,
				Actor: runtimeFixtureActor, Reason: "simulate crash after stage terminal outcome",
			})
			if err != nil || claim.Job == nil || claim.Job.ID != sourceJob.ID {
				t.Fatalf("claim stage for outcome recovery fixture = %+v, %v", claim, err)
			}
			markRuntimeStageTerminal(t, ctx, fixture.store, sourcePayload.StageAttemptID, scenario.status, scenario.verdict)
			time.Sleep(40 * time.Millisecond)
			recovery := newFrozenRuntimeWorker(t, fixture.store, runtime, "runtime-recovery-outcome-worker")
			result, err := recovery.RunOnce(ctx)
			if err != nil {
				t.Fatalf("outcome recovery result = %+v, %v", result, err)
			}
			if len(result.Recoveries) != 1 || result.Recoveries[0].Job.ID != sourceJob.ID || result.Empty != scenario.wantEmpty {
				t.Fatalf("outcome recovery facts = %+v", result)
			}
			run, err := fixture.store.GetWorkflowRun(ctx, fixture.run.ID)
			if err != nil || run == nil || run.Status != scenario.wantRun {
				t.Fatalf("recovered run outcome = %+v, %v", run, err)
			}
			jobs, err := fixture.store.ListDurableJobsForRun(ctx, fixture.run.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, job := range jobs {
				if job.CommandType != "stage_attempt.execute" {
					continue
				}
				var payload frozenStageExecutionPayload
				if err := json.Unmarshal([]byte(job.PayloadJSON), &payload); err != nil {
					t.Fatal(err)
				}
				if payload.StageKey == runtimeFixtureVerifyStage {
					t.Fatalf("non-progressing recovered stage queued successor job %+v", job)
				}
			}
		})
	}
}

func TestFrozenExecutionRuntimeRejectsFrozenPluginVersionDrift(t *testing.T) {
	ctx := context.Background()
	fixture := newFrozenRuntimeFixture(t)
	defer fixture.store.Close()
	invoked := false
	registry, err := workflowkit.NewControlledPluginRegistry([]workflowkit.PluginRegistration[workflowkit.StageExecutor]{
		{Binding: workflowkit.PluginBinding{ID: "runtime.source", Version: "2"}, Implementation: workflowkit.StageExecutorFunc(func(context.Context, workflowkit.StageExecutionRequest) (workflowkit.StageExecutionResult, error) {
			invoked = true
			return workflowkit.StageExecutionResult{}, errors.New("wrong-version executor must never run")
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime := newFrozenRuntime(t, fixture.services, registry)
	worker := newFrozenRuntimeWorker(t, fixture.store, runtime, "runtime-version-drift-worker")
	if result, err := worker.RunOnce(ctx); err != nil || result.FinalState != store.JobSucceeded {
		t.Fatalf("initial coordinator result = %+v, %v", result, err)
	}
	if result, err := worker.RunOnce(ctx); err != nil || result.FinalState != store.JobSucceeded {
		t.Fatalf("version-drift stage result = %+v, %v", result, err)
	}
	if invoked {
		t.Fatal("runtime fell back to an incompatible plugin version")
	}
	_, payload := requireRuntimeStageJob(t, ctx, fixture.store, fixture.run.ID, runtimeFixtureSourceStage)
	attempt, err := fixture.store.GetStageAttempt(ctx, payload.StageAttemptID)
	if err != nil || attempt == nil {
		t.Fatalf("read drifted stage attempt: %+v, %v", attempt, err)
	}
	if attempt.ExecutionStatus != store.StageExecutionInfraFailed || !strings.Contains(attempt.ErrorText, "version") {
		t.Fatalf("version drift stage projection = %+v", attempt)
	}
	run, err := fixture.store.GetWorkflowRun(ctx, fixture.run.ID)
	if err != nil || run == nil || run.Status != store.WorkflowRunFailedRecoverable {
		t.Fatalf("version drift run projection = %+v, %v", run, err)
	}
}

func TestFrozenExecutionRuntimeMalformedPayloadProjectsRunInDoubt(t *testing.T) {
	ctx := context.Background()
	fixture := newFrozenRuntimeFixture(t)
	defer fixture.store.Close()
	runtime := newFrozenRuntime(t, fixture.services, frozenRuntimeRegistry(t, fixture.frozen.Workflow, completedFixtureStage))
	malformed, err := fixture.store.CreateDurableJob(ctx, store.CreateDurableJobRequest{
		CommandType: "workflow_run.execute", EntityType: "workflow_run", EntityID: fixture.run.ID, RunID: fixture.run.ID,
		Priority: 100, PayloadJSON: `{"unexpected":true}`, IdempotencyKey: "runtime-malformed-payload", Actor: runtimeFixtureActor, Reason: "malformed payload fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	worker := newFrozenRuntimeWorker(t, fixture.store, runtime, "runtime-malformed-worker")
	result, err := worker.RunOnce(ctx)
	if err == nil || result.FinalState != store.JobFailed || result.Job == nil || result.Job.ID != malformed.ID {
		t.Fatalf("malformed job result = %+v, %v", result, err)
	}
	run, getErr := fixture.store.GetWorkflowRun(ctx, fixture.run.ID)
	if getErr != nil || run == nil || run.Status != store.WorkflowRunInDoubt {
		t.Fatalf("malformed job run projection = %+v, %v", run, getErr)
	}
}

func TestFrozenExecutionRuntimeTreatsLostStageFenceAsInDoubt(t *testing.T) {
	ctx := context.Background()
	fixture := newFrozenRuntimeFixture(t)
	defer fixture.store.Close()
	runtime := newFrozenRuntime(t, fixture.services, frozenRuntimeRegistry(t, fixture.frozen.Workflow, completedFixtureStage))
	initial := newFrozenRuntimeWorker(t, fixture.store, runtime, "runtime-fence-initial")
	if result, err := initial.RunOnce(ctx); err != nil || result.FinalState != store.JobSucceeded {
		t.Fatalf("initial coordinator result = %+v, %v", result, err)
	}
	sourceJob, sourcePayload := requireRuntimeStageJob(t, ctx, fixture.store, fixture.run.ID, runtimeFixtureSourceStage)
	claim, err := fixture.store.ClaimNextDurableJob(ctx, store.ClaimNextDurableJobRequest{
		IdempotencyKey: "runtime-fence-stage-claim", Owner: "runtime-fence-owner", LeaseTTL: time.Minute,
		Actor: runtimeFixtureActor, Reason: "claim stage for stale fence fixture",
	})
	if err != nil || claim.Job == nil || claim.Job.ID != sourceJob.ID {
		t.Fatalf("claim stale-fence stage = %+v, %v", claim, err)
	}
	lost := make(chan struct{})
	close(lost)
	state, err := runtime.HandleDurableJob(ctx, DurableJobExecution{Claim: claim, LeaseLost: lost})
	if err != nil || state != store.JobInterrupted {
		t.Fatalf("stale-fence runtime result = %s, %v", state, err)
	}
	attempt, err := fixture.store.GetStageAttempt(ctx, sourcePayload.StageAttemptID)
	if err != nil || attempt == nil || attempt.ExecutionStatus != store.StageExecutionInDoubt {
		t.Fatalf("stale-fence stage projection = %+v, %v", attempt, err)
	}
	run, err := fixture.store.GetWorkflowRun(ctx, fixture.run.ID)
	if err != nil || run == nil || run.Status != store.WorkflowRunInDoubt {
		t.Fatalf("stale-fence run projection = %+v, %v", run, err)
	}
}

func TestFrozenExecutionRuntimeAppliesQueuedPauseTerminateAndStageCancel(t *testing.T) {
	for _, scenario := range []struct {
		name        string
		action      store.ControlAction
		wantRun     store.WorkflowRunStatus
		wantStage   store.StageExecutionStatus
		wantJob     store.JobState
		stageScoped bool
	}{
		{name: "pause", action: store.ControlActionPause, wantRun: store.WorkflowRunPaused, wantStage: store.StageExecutionInterrupted, wantJob: store.JobInterrupted},
		{name: "terminate", action: store.ControlActionTerminate, wantRun: store.WorkflowRunCanceled, wantStage: store.StageExecutionCanceled, wantJob: store.JobCanceled},
		{name: "cancel stage", action: store.ControlActionCancelStage, wantRun: store.WorkflowRunFailedRecoverable, wantStage: store.StageExecutionCanceled, wantJob: store.JobCanceled, stageScoped: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx := context.Background()
			fixture := newFrozenRuntimeFixture(t)
			defer fixture.store.Close()
			runtime := newFrozenRuntime(t, fixture.services, frozenRuntimeRegistry(t, fixture.frozen.Workflow, completedFixtureStage))
			worker := newFrozenRuntimeWorker(t, fixture.store, runtime, "runtime-control-"+strings.ReplaceAll(scenario.name, " ", "-"))
			if result, err := worker.RunOnce(ctx); err != nil || result.FinalState != store.JobSucceeded {
				t.Fatalf("initial coordinator result = %+v, %v", result, err)
			}
			_, payload := requireRuntimeStageJob(t, ctx, fixture.store, fixture.run.ID, runtimeFixtureSourceStage)
			checkpoint, err := fixture.services.Control.CurrentCheckpoint(ctx, fixture.run.ID)
			if err != nil {
				t.Fatal(err)
			}
			request := RequestExecutionControlRequest{
				OperationKey: "runtime-control-" + strings.ReplaceAll(scenario.name, " ", "-"), Action: scenario.action,
				RunID: fixture.run.ID, Expected: checkpoint, Actor: runtimeFixtureActor, Reason: "runtime control fixture",
			}
			if scenario.stageScoped {
				request.StageAttemptID = payload.StageAttemptID
			}
			operation, err := fixture.services.Control.Request(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			result, err := worker.RunOnce(ctx)
			if err != nil || result.FinalState != scenario.wantJob {
				t.Fatalf("control worker result = %+v, %v; want %s", result, err, scenario.wantJob)
			}
			attempt, err := fixture.store.GetStageAttempt(ctx, payload.StageAttemptID)
			if err != nil || attempt == nil || attempt.ExecutionStatus != scenario.wantStage {
				t.Fatalf("control stage projection = %+v, %v", attempt, err)
			}
			run, err := fixture.store.GetWorkflowRun(ctx, fixture.run.ID)
			if err != nil || run == nil || run.Status != scenario.wantRun {
				t.Fatalf("control run projection = %+v, %v", run, err)
			}
			acknowledged, err := fixture.services.Control.Get(ctx, operation.ID)
			if err != nil || acknowledged.Status != store.ControlOperationAcknowledged {
				t.Fatalf("control acknowledgement = %+v, %v", acknowledged, err)
			}
		})
	}

}

func newFrozenRuntimeFixture(t *testing.T) frozenRuntimeFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	dataStore, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	services, err := newLifecycleServicesForTest(root, dataStore)
	if err != nil {
		_ = dataStore.Close()
		t.Fatal(err)
	}
	task, revision, err := services.Tasks.ImportTask(ctx, ImportTaskRequest{
		CreateDraftTaskRequest: CreateDraftTaskRequest{Slug: "frozen-runtime", Title: "Frozen Runtime", Actor: runtimeFixtureActor, Reason: "runtime integration fixture"},
		SourceDirectory:        writeLifecycleSnapshot(t, "frozen runtime fixture\n"),
		ChangeSummary:          "import immutable runtime fixture",
	})
	if err != nil {
		_ = dataStore.Close()
		t.Fatal(err)
	}
	specification := lifecycleExecutionSpec(task.ID, revision.ID, revision.TaskDigest)
	specificationCanonical, err := specification.CanonicalJSON()
	if err != nil {
		_ = dataStore.Close()
		t.Fatal(err)
	}
	specificationFingerprint, err := specification.Fingerprint()
	if err != nil {
		_ = dataStore.Close()
		t.Fatal(err)
	}
	profileFingerprint, err := workflowkit.FingerprintBytes("app.runtime-fixture-profile.v1", []byte("runtime-fixture"))
	if err != nil {
		_ = dataStore.Close()
		t.Fatal(err)
	}
	workflow := runtimeFixtureWorkflow()
	policy := runtimeFixtureQuotaPolicy(t)
	definition, err := workflow.Fingerprint()
	if err != nil {
		_ = dataStore.Close()
		t.Fatal(err)
	}
	initialExecutionPlan, err := workflowkit.CompileDependencyExecutionPlan(workflow)
	if err != nil {
		_ = dataStore.Close()
		t.Fatal(err)
	}
	runID, err := store.NewUUIDv7()
	if err != nil {
		_ = dataStore.Close()
		t.Fatal(err)
	}
	resolved := workflowadapter.ResolvedWorkflow{
		TemplateID: "runtime-fixture", TemplateVersion: "1", ExecutionProfileID: "runtime-fixture", ExecutionProfileVersion: "1",
		ContinuationPlanTTL: workflowadapter.RequiredContinuationPlanTTL, ExecutionProfileFingerprint: profileFingerprint, DefinitionFingerprint: definition,
		Descriptor: workflow, QuotaPolicy: policy,
	}
	manifest, err := json.Marshal(runManifest{
		Format: "harbor.workflow-run-manifest.v2", RunID: runID, TaskID: task.ID, Revision: revision.ID, Resolved: resolved, InitialExecutionPlan: initialExecutionPlan,
		Inputs: &runManifestInputs{
			Format:                   runManifestInputsFormat,
			ProfileFingerprint:       profileFingerprint,
			ExecutionSpecFingerprint: specificationFingerprint,
		},
		ExecutionSpec: append(json.RawMessage(nil), specificationCanonical...),
	})
	if err != nil {
		_ = dataStore.Close()
		t.Fatal(err)
	}
	payload, err := json.Marshal(workflowRunExecutionPayload{
		Format: workflowRunExecutionPayloadFormat, RunID: runID, DefinitionHash: string(definition),
		ExecutionSpecFingerprint: specificationFingerprint, QuotaPolicy: policy.Clone(),
	})
	if err != nil {
		_ = dataStore.Close()
		t.Fatal(err)
	}
	run, err := dataStore.CreateWorkflowRun(ctx, store.CreateWorkflowRunRequest{
		ID: runID, TaskID: task.ID, RevisionID: revision.ID, WorkflowTemplateID: resolved.TemplateID, WorkflowTemplateVersion: resolved.TemplateVersion,
		ResolvedProfileHash: string(profileFingerprint), DefinitionHash: string(definition), RunManifestJSON: string(manifest),
		Trigger: "runtime integration", Actor: runtimeFixtureActor, Reason: "create frozen runtime fixture",
		Dispatch: &store.WorkflowRunDispatchRequest{CommandType: "workflow_run.execute", PayloadJSON: string(payload), IdempotencyKey: "workflow-run-execution:" + runID},
	})
	if err != nil {
		_ = dataStore.Close()
		t.Fatal(err)
	}
	frozen, err := decodeFrozenRunDefinition(run)
	if err != nil {
		_ = dataStore.Close()
		t.Fatal(err)
	}
	return frozenRuntimeFixture{store: dataStore, services: services, task: task, revision: revision, run: run, frozen: frozen}
}

func runtimeFixtureWorkflow() workflowkit.WorkflowDescriptor {
	claim := func(dimension string, units int64) workflowkit.QuotaClaim {
		return workflowkit.QuotaClaim{Dimension: dimension, Units: units, ReclaimPolicy: workflowkit.ReclaimUnused}
	}
	budget := workflowkit.ExecutionBudget{
		TurnTimeout: time.Second, MaxTurns: 1, AttemptTimeout: time.Second, MaxAttempts: 1, MaxElapsed: time.Second,
	}
	return workflowkit.WorkflowDescriptor{
		ID: "runtime-fixture", Version: "1",
		Stages: []workflowkit.StageDescriptor{
			{
				Key: runtimeFixtureSourceStage, Version: "1", Plugin: workflowkit.PluginBinding{ID: "runtime.source", Version: "1"}, Group: "runtime",
				Outputs: []workflowkit.ArtifactSpec{{Name: "source_report", SchemaVersion: "runtime.v1", Required: true}}, Effect: workflowkit.EffectEvidenceOnly,
				Budget: budget, QuotaClaims: []workflowkit.QuotaClaim{claim("stage_attempt", 1), claim("agent_turn", 2)}, Retry: workflowkit.RetryPolicy{},
				Verdicts: workflowkit.VerdictPolicy{Allowed: []workflowkit.Verdict{workflowkit.VerdictPass}}, Reuse: workflowkit.ReuseWhenInputsMatch,
				Capabilities: workflowkit.CapabilitySet{workflowkit.CapabilityCancel, workflowkit.CapabilityContinue},
			},
			{
				Key: runtimeFixtureVerifyStage, Version: "1", Plugin: workflowkit.PluginBinding{ID: "runtime.verify", Version: "1"}, Group: "runtime",
				Dependencies: []workflowkit.StageKey{runtimeFixtureSourceStage},
				Outputs:      []workflowkit.ArtifactSpec{{Name: "verify_report", SchemaVersion: "runtime.v1", Required: true}}, Effect: workflowkit.EffectEvidenceOnly,
				Budget: budget, QuotaClaims: []workflowkit.QuotaClaim{claim("stage_attempt", 1)}, Retry: workflowkit.RetryPolicy{},
				Verdicts: workflowkit.VerdictPolicy{Allowed: []workflowkit.Verdict{workflowkit.VerdictPass}}, Reuse: workflowkit.ReuseWhenInputsMatch,
				Capabilities: workflowkit.CapabilitySet{workflowkit.CapabilityCancel, workflowkit.CapabilityContinue},
			},
		},
	}
}

func runtimeFixtureQuotaPolicy(t *testing.T) workflowadapter.ResolvedQuotaPolicy {
	t.Helper()
	policy := workflowadapter.QuotaPolicy{
		ID: "runtime-fixture-policy", Version: "1",
		AccountLimits: []workflowadapter.QuotaAccountLimit{
			{Dimension: "stage_attempt", TaskLimitUnits: 10, ActorLimitUnits: 10},
			{Dimension: "agent_turn", TaskLimitUnits: 10, ActorLimitUnits: 10},
		},
		Stages: []workflowadapter.StageQuotaPolicy{
			{StageKey: runtimeFixtureSourceStage, Claims: []workflowkit.QuotaClaim{{Dimension: "stage_attempt", Units: 1, ReclaimPolicy: workflowkit.ReclaimUnused}, {Dimension: "agent_turn", Units: 2, ReclaimPolicy: workflowkit.ReclaimUnused}}},
			{StageKey: runtimeFixtureVerifyStage, Claims: []workflowkit.QuotaClaim{{Dimension: "stage_attempt", Units: 1, ReclaimPolicy: workflowkit.ReclaimUnused}}},
		},
	}
	fingerprint, err := policy.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	return workflowadapter.ResolvedQuotaPolicy{ID: policy.ID, Version: policy.Version, Fingerprint: fingerprint, AccountLimits: policy.AccountLimits, Stages: policy.Stages}
}

func frozenRuntimeRegistry(t *testing.T, workflow workflowkit.WorkflowDescriptor, executor workflowkit.StageExecutorFunc) *workflowkit.ControlledPluginRegistry[workflowkit.StageExecutor] {
	t.Helper()
	registrations := make([]workflowkit.PluginRegistration[workflowkit.StageExecutor], 0, len(workflow.Stages))
	for _, stage := range workflow.Stages {
		registrations = append(registrations, workflowkit.PluginRegistration[workflowkit.StageExecutor]{Binding: stage.Plugin, Implementation: executor})
	}
	registry, err := workflowkit.NewControlledPluginRegistry(registrations)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func newFrozenRuntime(t *testing.T, services *LifecycleServices, registry *workflowkit.ControlledPluginRegistry[workflowkit.StageExecutor]) *FrozenExecutionRuntime {
	t.Helper()
	runtime, err := NewFrozenExecutionRuntime(FrozenExecutionRuntimeConfig{
		Services: services, WorkflowkitRegistry: registry, QuotaLeaseTTL: time.Second, ControlPollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func newFrozenRuntimeWorker(t *testing.T, dataStore *store.Store, runtime *FrozenExecutionRuntime, owner string) *DurableWorker {
	t.Helper()
	worker, err := NewDurableWorker(DurableWorkerConfig{
		Store: dataStore, Owner: owner, Actor: runtimeFixtureActor, Reason: "frozen runtime integration test",
		LeaseTTL: time.Second, HeartbeatEvery: 100 * time.Millisecond, PollInterval: time.Millisecond, Handler: runtime,
	})
	if err != nil {
		t.Fatal(err)
	}
	return worker
}

func completedFixtureStage(_ context.Context, request workflowkit.StageExecutionRequest) (workflowkit.StageExecutionResult, error) {
	artifacts := make([]workflowkit.StageArtifact, 0, len(request.Stage.Outputs))
	for _, output := range request.Stage.Outputs {
		artifacts = append(artifacts, workflowkit.StageArtifact{
			Name: output.Name, SchemaVersion: output.SchemaVersion, Content: []byte(fmt.Sprintf("%s:%s\n", request.Stage.Key, output.Name)), TurnOrdinal: 1,
		})
	}
	return workflowkit.StageExecutionResult{Outcome: workflowkit.Outcome{Status: workflowkit.StatusCompleted, Verdict: workflowkit.VerdictPass}, Artifacts: artifacts}, nil
}

func driveFrozenRuntimeToTerminal(t *testing.T, ctx context.Context, worker *DurableWorker, dataStore *store.Store, runID string) store.WorkflowRun {
	t.Helper()
	for cycle := 0; cycle < 12; cycle++ {
		result, err := worker.RunOnce(ctx)
		if err != nil {
			t.Fatalf("durable worker cycle %d: %+v, %v", cycle, result, err)
		}
		run, err := dataStore.GetWorkflowRun(ctx, runID)
		if err != nil || run == nil {
			t.Fatalf("read run after cycle %d: %+v, %v", cycle, run, err)
		}
		if run.Status == store.WorkflowRunSucceeded || run.Status == store.WorkflowRunFailedRecoverable || run.Status == store.WorkflowRunFailedTerminal || run.Status == store.WorkflowRunCanceled || run.Status == store.WorkflowRunInterrupted || run.Status == store.WorkflowRunInDoubt {
			return *run
		}
		if result.Empty {
			t.Fatalf("durable worker became empty before run %s reached a terminal state", runID)
		}
	}
	t.Fatalf("run %s did not reach a terminal state", runID)
	return store.WorkflowRun{}
}

func requireRuntimeStageJob(t *testing.T, ctx context.Context, dataStore *store.Store, runID string, key workflowkit.StageKey) (store.DurableJob, frozenStageExecutionPayload) {
	t.Helper()
	jobs, err := dataStore.ListDurableJobsForRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range jobs {
		if job.CommandType != "stage_attempt.execute" {
			continue
		}
		var payload frozenStageExecutionPayload
		if err := json.Unmarshal([]byte(job.PayloadJSON), &payload); err != nil {
			t.Fatal(err)
		}
		if payload.StageKey == key {
			return job, payload
		}
	}
	t.Fatalf("no durable stage job for %q in %+v", key, jobs)
	return store.DurableJob{}, frozenStageExecutionPayload{}
}

func markRuntimeStageCompleted(t *testing.T, ctx context.Context, dataStore *store.Store, attemptID string) {
	markRuntimeStageTerminal(t, ctx, dataStore, attemptID, store.StageExecutionCompleted, store.VerdictPass)
}

func markRuntimeStageTerminal(t *testing.T, ctx context.Context, dataStore *store.Store, attemptID string, status store.StageExecutionStatus, verdict store.Verdict) {
	t.Helper()
	attempt, err := dataStore.GetStageAttempt(ctx, attemptID)
	if err != nil || attempt == nil {
		t.Fatalf("read stage attempt %s: %+v, %v", attemptID, attempt, err)
	}
	updated, err := dataStore.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: attempt.ID, ExpectedVersion: attempt.Version, ExecutionStatus: store.StageExecutionRunning, Actor: runtimeFixtureActor, Reason: "race fixture starts stage",
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt = &updated
	if _, err := dataStore.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: attempt.ID, ExpectedVersion: attempt.Version, ExecutionStatus: status, Verdict: verdict,
		Actor: runtimeFixtureActor, Reason: "recovery fixture commits stage terminal projection",
	}); err != nil {
		t.Fatal(err)
	}
}

func assertRuntimeQuotaAccount(t *testing.T, ctx context.Context, dataStore *store.Store, kind store.QuotaScopeKind, scopeID, dimension string, consumed, reserved int64) {
	t.Helper()
	accounts, err := dataStore.ListQuotaAccountsForScope(ctx, kind, scopeID)
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range accounts {
		if account.Dimension == dimension {
			if account.ConsumedUnits != consumed || account.ReservedUnits != reserved {
				t.Fatalf("%s/%s quota account = %+v, want consumed=%d reserved=%d", kind, dimension, account, consumed, reserved)
			}
			return
		}
	}
	t.Fatalf("missing %s quota account for %s/%s", dimension, kind, scopeID)
}
