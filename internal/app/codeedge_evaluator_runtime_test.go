package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/codeedge"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestCodeEdgeEvaluatorRuntimePreallocatesTrialsBeforeInvocationAndReconcilesUnknownOutcome(t *testing.T) {
	ctx := context.Background()
	fixture := newCodeEdgeComplianceFixture(t, codeEdgeComplianceFixtureOptions{})
	defer fixture.database.Close()
	run := resumeCodeEdgeRuntimeFixtureRun(t, ctx, fixture)
	frozen := codeEdgeRuntimeFrozenDefinition(t, run)
	stage, job, payload := createCodeEdgeEvaluatorRuntimeStageJob(t, ctx, fixture, run, workflowadapter.HarborRunQwen)

	invoked := false
	trialsWereReady := false
	runtime := newFrozenRuntime(t, fixture.services, codeEdgeRuntimeRegistry(t, frozen.Workflow, func(callCtx context.Context, request workflowkit.StageExecutionRequest) (workflowkit.StageExecutionResult, error) {
		if request.Stage.Key != workflowkit.StageKey(workflowadapter.HarborRunQwen) {
			return completedFixtureStage(callCtx, request)
		}
		invoked = true
		trials, err := fixture.database.ListTrialExecutionsForStageAttempt(callCtx, stage.ID)
		if err != nil {
			return workflowkit.StageExecutionResult{}, err
		}
		if len(trials) != codeEdgeEvaluatorTrialCount {
			return workflowkit.StageExecutionResult{}, fmt.Errorf("evaluator invocation saw %d TrialExecutions, want four", len(trials))
		}
		for ordinal, trial := range trials {
			attempts, listErr := fixture.database.ListTrialAttemptsForTrialExecution(callCtx, trial.ID)
			if listErr != nil || trial.Ordinal != ordinal+1 || trial.Status != store.TrialExecutionRunning || len(attempts) != 1 || attempts[0].Ordinal != 1 || attempts[0].Status != store.TrialAttemptRunning {
				return workflowkit.StageExecutionResult{}, fmt.Errorf("evaluator invocation saw incomplete preallocation: trial=%+v attempts=%+v err=%v", trial, attempts, listErr)
			}
		}
		trialsWereReady = true
		return workflowkit.StageExecutionResult{Outcome: workflowkit.Outcome{Status: workflowkit.StatusInterrupted}, ErrorText: "simulated evaluator transport interruption"}, nil
	}))
	claim := claimCodeEdgeRuntimeStageJob(t, ctx, fixture.database, run.ID, job.ID)

	result, err := runtime.HandleDurableJob(ctx, DurableJobExecution{Claim: claim})
	state := result.State
	if err != nil || state != store.JobInDoubt || result.Failure == nil || !invoked || !trialsWereReady {
		t.Fatalf("CodeEdge evaluator in_doubt invocation = result=%+v err=%v invoked=%t trialsWereReady=%t", result, err, invoked, trialsWereReady)
	}
	assertCodeEdgeEvaluatorInDoubt(t, ctx, fixture.database, run.ID, stage, payload.StageKey)
	reconciliationJob := requireCodeEdgeEvaluatorReconciliationJob(t, ctx, fixture.database, job, run, stage, payload.StageKey)
	if reconciliationJob.State != store.JobQueued {
		t.Fatalf("CodeEdge evaluator reconciliation job = %+v, want queued", reconciliationJob)
	}
}

func TestCodeEdgeEvaluatorRecoveryProjectsObservedCompletionWithoutAnotherModelInvocation(t *testing.T) {
	ctx := context.Background()
	fixture := newCodeEdgeComplianceFixture(t, codeEdgeComplianceFixtureOptions{})
	defer fixture.database.Close()
	run := resumeCodeEdgeRuntimeFixtureRun(t, ctx, fixture)
	frozen := codeEdgeRuntimeFrozenDefinition(t, run)
	stage, job, payload := createCodeEdgeEvaluatorRuntimeStageJob(t, ctx, fixture, run, workflowadapter.HarborRunQwen)
	descriptor, found := frozen.Workflow.Stage(payload.StageKey)
	if !found {
		t.Fatalf("frozen CodeEdge workflow omits evaluator %q", payload.StageKey)
	}

	providerCalls := 0
	runtime := newFrozenRuntime(t, fixture.services, codeEdgeRuntimeRegistry(t, frozen.Workflow, func(callCtx context.Context, request workflowkit.StageExecutionRequest) (workflowkit.StageExecutionResult, error) {
		if request.Stage.Key != payload.StageKey {
			return completedFixtureStage(callCtx, request)
		}
		providerCalls++
		return workflowkit.StageExecutionResult{
			Outcome: workflowkit.Outcome{Status: workflowkit.StatusInterrupted}, ErrorText: "simulate worker loss after Harbor invocation",
		}, nil
	}))
	claim := claimCodeEdgeRuntimeStageJob(t, ctx, fixture.database, run.ID, job.ID)
	result, err := runtime.HandleDurableJob(ctx, DurableJobExecution{Claim: claim})
	state := result.State
	if err != nil || state != store.JobInDoubt || result.Failure == nil || providerCalls != 1 {
		t.Fatalf("initial CodeEdge evaluator in_doubt result = %+v err=%v providerCalls=%d", result, err, providerCalls)
	}

	admission, err := fixture.database.GetDurableAdmissionDecisionByIdempotencyKey(ctx, "stage-admission:"+job.ID)
	if err != nil || admission == nil || !admission.Accepted || len(admission.Leases) == 0 {
		t.Fatalf("initial CodeEdge evaluator quota admission = %+v, %v", admission, err)
	}
	for _, lease := range admission.Leases {
		if lease.State != store.DurableQuotaLeaseUncertain {
			t.Fatalf("interrupted CodeEdge evaluator lease = %+v, want uncertain", lease)
		}
	}

	taskRoot := fixture.services.core.layout.snapshotDirectory(fixture.revision.TaskID, fixture.revision.ID)
	bundle := codeEdgeHarborRunBundleBytes(t, taskRoot, fixture.childFrozen.Binding.TaskSnapshotDigest, fixture.frozen.Policy.QwenPolicy, "observed-recovered-qwen")
	observer := &codeEdgeEvaluatorTestObserver{result: observedCodeEdgeEvaluatorResult(t, descriptor, bundle), observed: true, requireReadableInputs: true}
	fixture.services.core.evaluatorObserver = observer
	if err := runtime.reconcileRecoveredStageJob(ctx, job); err != nil {
		t.Fatalf("enqueue recovered CodeEdge evaluator reconciliation = %v", err)
	}
	if observer.calls != 0 || providerCalls != 1 {
		t.Fatalf("source recovery calls observer=%d provider=%d; want no observer and no second provider call", observer.calls, providerCalls)
	}
	reconciliationJob := requireCodeEdgeEvaluatorReconciliationJob(t, ctx, fixture.database, job, run, stage, payload.StageKey)
	worker := newFrozenRuntimeWorker(t, fixture.database, runtime, "codeedge-evaluator-reconciliation-worker")
	workerResult, workerErr := worker.RunOnce(ctx)
	if workerErr != nil || workerResult.Job == nil || workerResult.Job.ID != reconciliationJob.ID || workerResult.FinalState != store.JobSucceeded {
		t.Fatalf("execute CodeEdge evaluator reconciliation job = %+v, %v", workerResult, workerErr)
	}
	if observer.calls != 1 || providerCalls != 1 {
		t.Fatalf("CodeEdge completion reconciliation calls observer=%d provider=%d; want observer once and no second provider call", observer.calls, providerCalls)
	}
	if observer.request.Execution.Stage.Key != payload.StageKey || len(observer.request.Execution.Inputs) == 0 {
		t.Fatalf("CodeEdge observer request did not retain frozen stage/inputs: %+v", observer.request)
	}

	updatedStage, err := fixture.database.GetStageAttempt(ctx, stage.ID)
	if err != nil || updatedStage == nil || updatedStage.ExecutionStatus != store.StageExecutionCompleted || updatedStage.Verdict != store.VerdictPass || updatedStage.ArtifactManifestID == "" {
		t.Fatalf("observed CodeEdge evaluator stage = %+v, %v", updatedStage, err)
	}
	effect, err := fixture.database.GetSideEffectOperationByOperationKey(ctx, codeEdgeEvaluatorOperationKey(run.ID, stage.ID))
	if err != nil || effect == nil || effect.State != store.SideEffectSucceeded || effect.ReceiptRef != updatedStage.ArtifactManifestID || effect.DestinationDigest == "" {
		t.Fatalf("observed CodeEdge evaluator side effect = %+v, %v", effect, err)
	}
	reconciliation, err := fixture.database.GetReconciliationAttemptByOperationKey(ctx, codeEdgeEvaluatorReconciliationKey(effect.OperationKey))
	if err != nil || reconciliation == nil || reconciliation.State != store.ReconciliationCompleted {
		t.Fatalf("observed CodeEdge evaluator reconciliation = %+v, %v", reconciliation, err)
	}
	var observed codeEdgeEvaluatorObservedCompletion
	if err := json.Unmarshal([]byte(reconciliation.ObservedJSON), &observed); err != nil || observed.StageKey != payload.StageKey || observed.ArtifactManifestID != updatedStage.ArtifactManifestID {
		t.Fatalf("observed CodeEdge evaluator reconciliation evidence = %+v, %v", observed, err)
	}
	settledAdmission, err := fixture.database.GetDurableAdmissionDecisionByIdempotencyKey(ctx, "stage-admission:"+job.ID)
	if err != nil || settledAdmission == nil {
		t.Fatalf("read reconciled CodeEdge evaluator quota admission = %+v, %v", settledAdmission, err)
	}
	for _, lease := range settledAdmission.Leases {
		if lease.State != store.DurableQuotaLeaseSettled {
			t.Fatalf("observed CodeEdge evaluator lease = %+v, want settled", lease)
		}
	}
	trials, err := fixture.database.ListTrialExecutionsForStageAttempt(ctx, stage.ID)
	if err != nil || len(trials) != codeEdgeEvaluatorTrialCount {
		t.Fatalf("observed CodeEdge evaluator trials = %+v, %v", trials, err)
	}
	for _, trial := range trials {
		attempts, listErr := fixture.database.ListTrialAttemptsForTrialExecution(ctx, trial.ID)
		if listErr != nil || trial.Status != store.TrialExecutionCompleted || len(attempts) != 1 || attempts[0].Status != store.TrialAttemptCompleted {
			t.Fatalf("observed CodeEdge logical trial = %+v technical=%+v err=%v", trial, attempts, listErr)
		}
	}
	updatedRun, err := fixture.database.GetWorkflowRun(ctx, run.ID)
	if err != nil || updatedRun == nil || updatedRun.Status == store.WorkflowRunInDoubt {
		t.Fatalf("observed CodeEdge evaluator Run remained in_doubt: %+v, %v", updatedRun, err)
	}
}

func TestCodeEdgeEvaluatorReconciliationFinalizesCommittedReceiptAfterCrash(t *testing.T) {
	ctx := context.Background()
	scenario := newCodeEdgeEvaluatorCommittedReceiptCrashScenario(t, ctx)
	worker := newFrozenRuntimeWorker(t, scenario.fixture.database, scenario.runtime, "codeedge-evaluator-committed-receipt-worker")
	result, workerErr := worker.RunOnce(ctx)
	if workerErr != nil || result.Job == nil || result.Job.ID != scenario.reconciliationJob.ID || result.FinalState != store.JobSucceeded {
		t.Fatalf("recover committed CodeEdge evaluator receipt = %+v, %v", result, workerErr)
	}
	if scenario.observer.calls != 0 || *scenario.providerCalls != 1 {
		t.Fatalf("committed receipt recovery calls observer=%d provider=%d; want neither", scenario.observer.calls, *scenario.providerCalls)
	}
	completedReconciliation, err := scenario.fixture.database.GetReconciliationAttemptByOperationKey(ctx, codeEdgeEvaluatorReconciliationKey(scenario.effect.OperationKey))
	if err != nil || completedReconciliation == nil || completedReconciliation.State != store.ReconciliationCompleted {
		t.Fatalf("completed receipt reconciliation = %+v, %v", completedReconciliation, err)
	}
	updatedRun, err := scenario.fixture.database.GetWorkflowRun(ctx, scenario.run.ID)
	if err != nil || updatedRun == nil || updatedRun.Status == store.WorkflowRunInDoubt {
		t.Fatalf("committed receipt recovery Run = %+v, %v", updatedRun, err)
	}
}

func TestCodeEdgeEvaluatorExpiredReconciliationLeaseRestoresCommittedReceiptWithoutObserver(t *testing.T) {
	ctx := context.Background()
	scenario := newCodeEdgeEvaluatorCommittedReceiptCrashScenario(t, ctx)

	claim, err := scenario.fixture.database.ClaimNextDurableJob(ctx, store.ClaimNextDurableJobRequest{
		IdempotencyKey: "claim-expired-codeedge-evaluator-reconciliation:" + scenario.reconciliationJob.ID,
		RunID:          scenario.run.ID,
		Owner:          "expired-codeedge-evaluator-reconciliation-worker",
		LeaseTTL:       time.Minute,
		Actor:          runtimeFixtureActor,
		Reason:         "simulate lost CodeEdge evaluator reconciliation dispatch lease",
	})
	if err != nil || claim.Job == nil || claim.Job.ID != scenario.reconciliationJob.ID {
		t.Fatalf("claim CodeEdge evaluator reconciliation job = %+v, %v", claim, err)
	}
	recovered, err := scenario.fixture.database.TransitionDurableJob(ctx, store.TransitionDurableJobRequest{
		JobID: claim.Job.ID, ExpectedVersion: claim.Job.Version, State: store.JobInDoubt,
		Failure: &store.DurableJobFailure{Code: "job.lease_lost", Message: "The worker lease expired before the job outcome was recorded.", DetailsJSON: `{}`},
		Actor:   runtimeFixtureActor, Reason: "simulate expired CodeEdge evaluator reconciliation dispatch lease",
	})
	if err != nil || recovered.State != store.JobInDoubt || recovered.Failure == nil || recovered.Failure.Code != "job.lease_lost" {
		t.Fatalf("recover expired CodeEdge evaluator reconciliation job = %+v, %v", recovered, err)
	}
	if err := scenario.runtime.ReconcileDurableJobRecoveries(ctx, []store.ExpiredDurableJobRecovery{{Job: recovered}}); err != nil {
		t.Fatalf("reconcile expired CodeEdge evaluator reconciliation job = %v", err)
	}
	if scenario.observer.calls != 0 || *scenario.providerCalls != 1 {
		t.Fatalf("expired reconciliation recovery calls observer=%d provider=%d; want neither", scenario.observer.calls, *scenario.providerCalls)
	}
	reconciliation, err := scenario.fixture.database.GetReconciliationAttemptByOperationKey(ctx, codeEdgeEvaluatorReconciliationKey(scenario.effect.OperationKey))
	if err != nil || reconciliation == nil || reconciliation.State != store.ReconciliationCompleted {
		t.Fatalf("expired reconciliation durable receipt = %+v, %v", reconciliation, err)
	}
	updatedRun, err := scenario.fixture.database.GetWorkflowRun(ctx, scenario.run.ID)
	if err != nil || updatedRun == nil || updatedRun.Status == store.WorkflowRunInDoubt {
		t.Fatalf("expired reconciliation Run = %+v, %v", updatedRun, err)
	}
	successor, err := scenario.fixture.database.GetDurableJobByIdempotency(ctx, "workflow-run-next:"+scenario.run.ID+":"+scenario.sourceJob.ID)
	if err != nil || successor == nil || successor.CommandType != "workflow_run.execute" || successor.State != store.JobQueued {
		t.Fatalf("expired reconciliation successor coordinator = %+v, %v", successor, err)
	}
}

func TestCodeEdgeEvaluatorRecoveryLeavesMissingOrMalformedObservationInDoubt(t *testing.T) {
	for _, test := range []struct {
		name     string
		observer codeEdgeEvaluatorTestObserver
	}{
		{name: "missing", observer: codeEdgeEvaluatorTestObserver{observed: false}},
		{name: "malformed", observer: codeEdgeEvaluatorTestObserver{
			observed: true,
			result: workflowkit.StageExecutionResult{
				Outcome:   workflowkit.Outcome{Status: workflowkit.StatusCompleted, Verdict: workflowkit.VerdictPass},
				Artifacts: []workflowkit.StageArtifact{{Name: workflowadapter.CodeEdgeEvaluatorQwenBundleArtifact, SchemaVersion: codeedge.HarborRunBundleV018Format, Content: []byte("not a Harbor bundle")}, {Name: workflowadapter.CodeEdgeEvaluatorQwenScreenshotArtifact, SchemaVersion: workflowadapter.CodeEdgeEvaluatorScreenshotSchemaVersion, Content: codeEdgePNG(t)}},
			},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			fixture := newCodeEdgeComplianceFixture(t, codeEdgeComplianceFixtureOptions{})
			defer fixture.database.Close()
			run := resumeCodeEdgeRuntimeFixtureRun(t, ctx, fixture)
			frozen := codeEdgeRuntimeFrozenDefinition(t, run)
			stage, job, payload := createCodeEdgeEvaluatorRuntimeStageJob(t, ctx, fixture, run, workflowadapter.HarborRunQwen)
			providerCalls := 0
			runtime := newFrozenRuntime(t, fixture.services, codeEdgeRuntimeRegistry(t, frozen.Workflow, func(callCtx context.Context, request workflowkit.StageExecutionRequest) (workflowkit.StageExecutionResult, error) {
				if request.Stage.Key != payload.StageKey {
					return completedFixtureStage(callCtx, request)
				}
				providerCalls++
				return workflowkit.StageExecutionResult{Outcome: workflowkit.Outcome{Status: workflowkit.StatusInterrupted}, ErrorText: "simulate missing observer evidence"}, nil
			}))
			claim := claimCodeEdgeRuntimeStageJob(t, ctx, fixture.database, run.ID, job.ID)
			result, err := runtime.HandleDurableJob(ctx, DurableJobExecution{Claim: claim})
			state := result.State
			if err != nil || state != store.JobInDoubt || result.Failure == nil || providerCalls != 1 {
				t.Fatalf("initial CodeEdge evaluator in_doubt result = %+v err=%v providerCalls=%d", result, err, providerCalls)
			}
			fixture.services.core.evaluatorObserver = &test.observer
			if err := runtime.reconcileRecoveredStageJob(ctx, job); err != nil {
				t.Fatalf("enqueue %s CodeEdge evaluator observation = %v", test.name, err)
			}
			if test.observer.calls != 0 || providerCalls != 1 {
				t.Fatalf("%s source recovery calls observer=%d provider=%d", test.name, test.observer.calls, providerCalls)
			}
			reconciliationJob := requireCodeEdgeEvaluatorReconciliationJob(t, ctx, fixture.database, job, run, stage, payload.StageKey)
			worker := newFrozenRuntimeWorker(t, fixture.database, runtime, "codeedge-evaluator-"+test.name+"-reconciliation-worker")
			workerResult, workerErr := worker.RunOnce(ctx)
			if workerErr != nil || workerResult.Job == nil || workerResult.Job.ID != reconciliationJob.ID || workerResult.FinalState != store.JobSucceeded {
				t.Fatalf("execute %s CodeEdge evaluator reconciliation job = %+v, %v", test.name, workerResult, workerErr)
			}
			if test.observer.calls != 1 || providerCalls != 1 {
				t.Fatalf("%s observation calls observer=%d provider=%d", test.name, test.observer.calls, providerCalls)
			}
			assertCodeEdgeEvaluatorInDoubt(t, ctx, fixture.database, run.ID, stage, payload.StageKey)
		})
	}
}

func TestCodeEdgeEvaluatorRuntimeHandlesQueuedTerminationBeforeTrialAllocation(t *testing.T) {
	ctx := context.Background()
	fixture := newCodeEdgeComplianceFixture(t, codeEdgeComplianceFixtureOptions{})
	defer fixture.database.Close()
	run := resumeCodeEdgeRuntimeFixtureRun(t, ctx, fixture)
	frozen := codeEdgeRuntimeFrozenDefinition(t, run)
	stage, job, _ := createCodeEdgeEvaluatorRuntimeStageJob(t, ctx, fixture, run, workflowadapter.HarborRunQwen)
	checkpoint, err := fixture.services.Control.CurrentCheckpoint(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.services.Control.Request(ctx, RequestExecutionControlRequest{
		OperationKey: "codeedge-queued-termination:" + stage.ID, Action: store.ControlActionTerminate, RunID: run.ID,
		Expected: checkpoint, Actor: runtimeFixtureActor, Reason: "terminate evaluator before worker invocation",
	}); err != nil {
		t.Fatal(err)
	}
	runtime := newFrozenRuntime(t, fixture.services, codeEdgeRuntimeRegistry(t, frozen.Workflow, func(context.Context, workflowkit.StageExecutionRequest) (workflowkit.StageExecutionResult, error) {
		return workflowkit.StageExecutionResult{}, errors.New("queued CodeEdge evaluator must not invoke its provider")
	}))
	claim := claimCodeEdgeRuntimeStageJob(t, ctx, fixture.database, run.ID, job.ID)

	result, err := runtime.HandleDurableJob(ctx, DurableJobExecution{Claim: claim})
	state := result.State
	if err != nil || state != store.JobCanceled {
		t.Fatalf("queued CodeEdge evaluator termination = state=%s err=%v", state, err)
	}
	trials, err := fixture.database.ListTrialExecutionsForStageAttempt(ctx, stage.ID)
	if err != nil || len(trials) != 0 {
		t.Fatalf("queued evaluator termination created trials=%+v err=%v", trials, err)
	}
	effect, err := fixture.database.GetSideEffectOperationByOperationKey(ctx, codeEdgeEvaluatorOperationKey(run.ID, stage.ID))
	if err != nil || effect != nil {
		t.Fatalf("queued evaluator termination created side effect=%+v err=%v", effect, err)
	}
}

func TestCodeEdgeEvaluatorRuntimeCompletesTrustedTrialsAfterDirectSuccessAndReplay(t *testing.T) {
	ctx := context.Background()
	fixture := newCodeEdgeComplianceFixture(t, codeEdgeComplianceFixtureOptions{})
	defer fixture.database.Close()
	run := resumeCodeEdgeRuntimeFixtureRun(t, ctx, fixture)
	frozen := codeEdgeRuntimeFrozenDefinition(t, run)
	stage, job, _ := createCodeEdgeEvaluatorRuntimeStageJob(t, ctx, fixture, run, workflowadapter.HarborRunQwen)
	providerCalls := 0
	runtime := newFrozenRuntime(t, fixture.services, codeEdgeRuntimeRegistry(t, frozen.Workflow, func(callCtx context.Context, request workflowkit.StageExecutionRequest) (workflowkit.StageExecutionResult, error) {
		if request.Stage.Key == workflowkit.StageKey(workflowadapter.HarborRunQwen) {
			providerCalls++
		}
		return completedFixtureStage(callCtx, request)
	}))
	claim := claimCodeEdgeRuntimeStageJob(t, ctx, fixture.database, run.ID, job.ID)

	result, err := runtime.HandleDurableJob(ctx, DurableJobExecution{Claim: claim})
	state := result.State
	if err != nil || state != store.JobSucceeded || providerCalls != 1 {
		t.Fatalf("successful CodeEdge evaluator invocation = state=%s err=%v providerCalls=%d", state, err, providerCalls)
	}
	updatedStage, err := fixture.database.GetStageAttempt(ctx, stage.ID)
	if err != nil || updatedStage == nil || updatedStage.ExecutionStatus != store.StageExecutionCompleted || updatedStage.ArtifactManifestID == "" {
		t.Fatalf("successful CodeEdge evaluator stage = %+v, %v", updatedStage, err)
	}
	effect, err := fixture.database.GetSideEffectOperationByOperationKey(ctx, codeEdgeEvaluatorOperationKey(run.ID, stage.ID))
	if err != nil || effect == nil || effect.State != store.SideEffectSucceeded || effect.ReceiptRef != updatedStage.ArtifactManifestID || effect.DestinationDigest == "" {
		t.Fatalf("successful CodeEdge evaluator side effect = %+v, %v", effect, err)
	}
	trials, err := fixture.database.ListTrialExecutionsForStageAttempt(ctx, stage.ID)
	if err != nil || len(trials) != codeEdgeEvaluatorTrialCount {
		t.Fatalf("successful CodeEdge evaluator trials = %+v, %v", trials, err)
	}
	trialIDs := make([]string, 0, len(trials))
	trialAttemptIDs := make([]string, 0, len(trials))
	for _, trial := range trials {
		attempts, listErr := fixture.database.ListTrialAttemptsForTrialExecution(ctx, trial.ID)
		if listErr != nil || trial.Status != store.TrialExecutionCompleted || len(attempts) != 1 || attempts[0].Ordinal != 1 || attempts[0].Status != store.TrialAttemptCompleted {
			t.Fatalf("direct CodeEdge evaluator completion did not finalize TrialExecution=%+v attempts=%+v err=%v", trial, attempts, listErr)
		}
		trialIDs = append(trialIDs, trial.ID)
		trialAttemptIDs = append(trialAttemptIDs, attempts[0].ID)
	}

	// Simulate a source worker lease loss after the direct stage/effect receipt
	// commits. Recovery must reuse the four original logical identities and must
	// not manufacture an observer reconciliation record or another provider run.
	if err := runtime.reconcileRecoveredStageJob(ctx, job); err != nil {
		t.Fatalf("replay committed direct CodeEdge evaluator completion = %v", err)
	}
	if providerCalls != 1 {
		t.Fatalf("direct CodeEdge evaluator replay invoked provider %d times, want once", providerCalls)
	}
	if reconciliation, reconcileErr := fixture.database.GetReconciliationAttemptByOperationKey(ctx, codeEdgeEvaluatorReconciliationKey(effect.OperationKey)); reconcileErr != nil || reconciliation != nil {
		t.Fatalf("direct CodeEdge evaluator replay created reconciliation=%+v err=%v", reconciliation, reconcileErr)
	}
	replayedTrials, err := fixture.database.ListTrialExecutionsForStageAttempt(ctx, stage.ID)
	if err != nil || len(replayedTrials) != codeEdgeEvaluatorTrialCount {
		t.Fatalf("replayed CodeEdge evaluator trials = %+v, %v", replayedTrials, err)
	}
	for index, trial := range replayedTrials {
		attempts, listErr := fixture.database.ListTrialAttemptsForTrialExecution(ctx, trial.ID)
		if listErr != nil || trial.ID != trialIDs[index] || trial.Status != store.TrialExecutionCompleted || len(attempts) != 1 || attempts[0].ID != trialAttemptIDs[index] || attempts[0].Status != store.TrialAttemptCompleted {
			t.Fatalf("direct CodeEdge evaluator replay changed TrialExecution=%+v attempts=%+v err=%v", trial, attempts, listErr)
		}
	}
}

func TestCodeEdgeEvaluatorRecoveredStartedFenceDoesNotReinvokeOrAllocateMoreTrials(t *testing.T) {
	ctx := context.Background()
	fixture := newCodeEdgeComplianceFixture(t, codeEdgeComplianceFixtureOptions{})
	defer fixture.database.Close()
	run := resumeCodeEdgeRuntimeFixtureRun(t, ctx, fixture)
	frozen := codeEdgeRuntimeFrozenDefinition(t, run)
	stage, job, _ := createCodeEdgeEvaluatorRuntimeStageJob(t, ctx, fixture, run, workflowadapter.HarborRunQwen)
	descriptor, found := frozen.Workflow.Stage(workflowkit.StageKey(workflowadapter.HarborRunQwen))
	if !found {
		t.Fatal("frozen CodeEdge workflow omits qwen evaluator")
	}
	stage, err := fixture.database.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: stage.ID, ExpectedVersion: stage.Version, ExecutionStatus: store.StageExecutionRunning,
		Actor: runtimeFixtureActor, Reason: "simulate worker state immediately before evaluator invocation fence",
	})
	if err != nil {
		t.Fatal(err)
	}
	invoked := false
	runtime := newFrozenRuntime(t, fixture.services, codeEdgeRuntimeRegistry(t, frozen.Workflow, func(context.Context, workflowkit.StageExecutionRequest) (workflowkit.StageExecutionResult, error) {
		invoked = true
		return workflowkit.StageExecutionResult{}, errors.New("recovery must not invoke evaluator")
	}))
	fence, err := runtime.prepareCodeEdgeEvaluatorEffect(ctx, job, run, stage, descriptor)
	if err != nil || !fence.Invoke || fence.Operation.State != store.SideEffectStarted {
		t.Fatalf("prepare started CodeEdge evaluator fence = %+v, %v", fence, err)
	}
	trialsBefore, err := fixture.database.ListTrialExecutionsForStageAttempt(ctx, stage.ID)
	if err != nil || len(trialsBefore) != codeEdgeEvaluatorTrialCount {
		t.Fatalf("started evaluator trials before recovery = %+v, %v", trialsBefore, err)
	}

	if err := runtime.reconcileRecoveredStageJob(ctx, job); err != nil {
		t.Fatalf("reconcile expired CodeEdge evaluator stage = %v", err)
	}
	firstReconciliation, err := fixture.database.GetReconciliationAttemptByOperationKey(ctx, codeEdgeEvaluatorReconciliationKey(fence.Operation.OperationKey))
	if err != nil || firstReconciliation == nil {
		t.Fatalf("read first CodeEdge evaluator reconciliation = %+v, %v", firstReconciliation, err)
	}
	firstReconciliationJob := requireCodeEdgeEvaluatorReconciliationJob(t, ctx, fixture.database, job, run, stage, workflowkit.StageKey(workflowadapter.HarborRunQwen))
	if err := runtime.reconcileRecoveredStageJob(ctx, job); err != nil {
		t.Fatalf("replay expired CodeEdge evaluator stage reconciliation = %v", err)
	}
	replayedReconciliation, err := fixture.database.GetReconciliationAttemptByOperationKey(ctx, codeEdgeEvaluatorReconciliationKey(fence.Operation.OperationKey))
	if err != nil || replayedReconciliation == nil || replayedReconciliation.ID != firstReconciliation.ID {
		t.Fatalf("CodeEdge evaluator reconciliation replay = %+v, %v; want %s", replayedReconciliation, err, firstReconciliation.ID)
	}
	replayedReconciliationJob := requireCodeEdgeEvaluatorReconciliationJob(t, ctx, fixture.database, job, run, stage, workflowkit.StageKey(workflowadapter.HarborRunQwen))
	if replayedReconciliationJob.ID != firstReconciliationJob.ID {
		t.Fatalf("CodeEdge evaluator reconciliation delivery replay = %+v, want %s", replayedReconciliationJob, firstReconciliationJob.ID)
	}
	if invoked {
		t.Fatal("recovered started CodeEdge evaluator invoked its provider")
	}
	trialsAfter, err := fixture.database.ListTrialExecutionsForStageAttempt(ctx, stage.ID)
	if err != nil || len(trialsAfter) != codeEdgeEvaluatorTrialCount {
		t.Fatalf("recovered evaluator trials = %+v, %v; want no new sample", trialsAfter, err)
	}
	assertCodeEdgeEvaluatorInDoubt(t, ctx, fixture.database, run.ID, stage, workflowkit.StageKey(workflowadapter.HarborRunQwen))
}

func TestCodeEdgeEvaluatorBudgetAndContinuationRetryAreRejected(t *testing.T) {
	descriptor := workflowkit.StageDescriptor{
		Key:    workflowkit.StageKey(workflowadapter.HarborRunQwen),
		Budget: workflowkit.ExecutionBudget{MaxAttempts: 2},
	}
	if err := validateCodeEdgeEvaluatorBudget(descriptor); !errors.Is(err, ErrFrozenExecutionPayload) {
		t.Fatalf("CodeEdge evaluator max_attempts=2 error = %v, want frozen execution rejection", err)
	}
	workflow := workflowkit.WorkflowDescriptor{
		ID: workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateID, Version: workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateVersion,
		Stages: []workflowkit.StageDescriptor{{Key: workflowkit.StageKey(workflowadapter.HarborRunQwen), Group: "evaluation"}},
	}
	err := rejectContentContinuationTargets(workflow, []workflowkit.NodeID{workflowkit.NodeID(workflowadapter.HarborRunQwen)})
	if !errors.Is(err, ErrTaskContinuationTarget) {
		t.Fatalf("CodeEdge evaluator ordinary continuation retry error = %v, want target rejection", err)
	}
}

func TestCodeEdgeEvaluatorGuardsIncludeChildWithoutGrantingParentComplianceAuthority(t *testing.T) {
	parentRun := store.WorkflowRun{
		WorkflowTemplateID: workflowadapter.CodeEdgePhase1WorkflowTemplateID, WorkflowTemplateVersion: workflowadapter.CodeEdgePhase1WorkflowTemplateVersion,
	}
	childRun := store.WorkflowRun{
		WorkflowTemplateID: workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateID, WorkflowTemplateVersion: workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateVersion,
	}
	standardRun := store.WorkflowRun{
		WorkflowTemplateID: workflowadapter.StandardWorkflowTemplateID, WorkflowTemplateVersion: workflowadapter.StandardWorkflowTemplateVersion,
	}
	evaluator := workflowkit.StageDescriptor{Key: workflowkit.StageKey(workflowadapter.HarborRunQwen)}
	nonEvaluator := workflowkit.StageDescriptor{Key: workflowkit.StageKey(workflowadapter.RepoPrepare)}

	if isCodeEdgeEvaluatorStage(parentRun, evaluator) || !isCodeEdgeEvaluatorStage(childRun, evaluator) {
		t.Fatal("CodeEdge evaluator fence guard did not preserve child-only evaluator ownership")
	}
	if isCodeEdgeEvaluatorStage(childRun, nonEvaluator) || isCodeEdgeEvaluatorStage(standardRun, evaluator) {
		t.Fatal("CodeEdge evaluator fence guard accepted a non-evaluator stage or an unrelated template")
	}
	if !isCodeEdgePhase1Run(parentRun) || isCodeEdgePhase1Run(childRun) {
		t.Fatal("parent-only CodeEdge compliance predicate leaked to the evaluator child")
	}

	childWorkflow := workflowkit.WorkflowDescriptor{
		ID: workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateID, Version: workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateVersion,
		Stages: []workflowkit.StageDescriptor{{Key: workflowkit.StageKey(workflowadapter.HarborRunQwen)}},
	}
	if !isCodeEdgeEvaluatorNode(childWorkflow, workflowkit.NodeID(workflowadapter.HarborRunQwen)) {
		t.Fatal("evaluator child continuation guard did not select its Qwen node")
	}
	if isCodeEdgeEvaluatorNode(childWorkflow, workflowkit.NodeID(workflowadapter.RepoPrepare)) {
		t.Fatal("evaluator child continuation guard selected a non-evaluator node")
	}
}

func resumeCodeEdgeRuntimeFixtureRun(t *testing.T, ctx context.Context, fixture *codeEdgeComplianceFixture) store.WorkflowRun {
	t.Helper()
	run, err := fixture.database.GetWorkflowRun(ctx, fixture.runtimeRun.ID)
	if err != nil || run == nil {
		t.Fatalf("read CodeEdge runtime fixture run = %+v, %v", run, err)
	}
	if run.Status == store.WorkflowRunRunning {
		return *run
	}
	updated, err := fixture.database.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunRunning,
		Actor: runtimeFixtureActor, Reason: "resume fixture for CodeEdge evaluator runtime",
	})
	if err != nil {
		t.Fatalf("resume CodeEdge runtime fixture run from %s: %v", run.Status, err)
	}
	return updated
}

func createCodeEdgeEvaluatorRuntimeStageJob(t *testing.T, ctx context.Context, fixture *codeEdgeComplianceFixture, run store.WorkflowRun, stageKey string) (store.StageAttempt, store.DurableJob, frozenStageExecutionPayload) {
	t.Helper()
	frozen := codeEdgeRuntimeFrozenDefinition(t, run)
	descriptor, found := frozen.Workflow.Stage(workflowkit.StageKey(stageKey))
	if !found {
		t.Fatalf("frozen CodeEdge workflow has no evaluator %q", stageKey)
	}
	inputs, err := resolveStageInputs(ctx, fixture.database, fixture.services.core.objects, run, fixture.revision, descriptor)
	if err != nil {
		t.Fatalf("resolve frozen evaluator inputs: %v", err)
	}
	inputFingerprint, err := workflowkit.FingerprintArtifactBindings(inputs)
	if err != nil {
		t.Fatal(err)
	}
	stageAttempts, err := fixture.database.ListStageAttemptsForRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	ordinal := 1
	retryOf := ""
	for _, existing := range stageAttempts {
		if existing.StageKey == stageKey && existing.Ordinal >= ordinal {
			ordinal = existing.Ordinal + 1
			retryOf = existing.ID
		}
	}
	budget, err := json.Marshal(descriptor.Budget)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := json.Marshal(runtimeStageAttemptSnapshot{
		Format: runtimeStageAttemptSnapshotFormat, ExecutionKey: "initial", Generation: 0, Retry: descriptor.Retry.Clone(),
	})
	if err != nil {
		t.Fatal(err)
	}
	stage, err := fixture.database.CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: run.ID, RetryOfStageAttemptID: retryOf, StageKey: stageKey, StageGroup: descriptor.Group, Ordinal: ordinal,
		InputFingerprint: string(inputFingerprint), BudgetSnapshotJSON: string(budget), RetrySnapshotJSON: string(retry),
		Actor: runtimeFixtureActor, Reason: "create CodeEdge evaluator runtime lifecycle fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := frozenStageExecutionPayload{
		Format: frozenStageExecutionPayloadFormat, RunID: run.ID, StageAttemptID: stage.ID, StageKey: workflowkit.StageKey(stageKey),
		DefinitionHash: run.DefinitionHash, Generation: 0, QuotaPolicy: frozen.QuotaPolicy.Clone(),
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	job, err := fixture.database.CreateDurableJob(ctx, store.CreateDurableJobRequest{
		CommandType: "stage_attempt.execute", EntityType: "stage_attempt", EntityID: stage.ID, RunID: run.ID, StageAttemptID: stage.ID,
		Priority: 1000, PayloadJSON: string(encoded), IdempotencyKey: "codeedge-evaluator-runtime-stage:" + stage.ID,
		Actor: runtimeFixtureActor, Reason: "run CodeEdge evaluator lifecycle fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	return stage, job, payload
}

type codeEdgeEvaluatorCommittedReceiptCrashScenario struct {
	fixture           *codeEdgeComplianceFixture
	runtime           *FrozenExecutionRuntime
	run               store.WorkflowRun
	sourceJob         store.DurableJob
	reconciliationJob store.DurableJob
	effect            store.SideEffectOperation
	observer          *codeEdgeEvaluatorTestObserver
	providerCalls     *int
}

// newCodeEdgeEvaluatorCommittedReceiptCrashScenario materializes the durable
// facts left by a crash after an observed completion commits its receipt but
// before the ReconciliationAttempt and successor coordinator are completed.
func newCodeEdgeEvaluatorCommittedReceiptCrashScenario(t *testing.T, ctx context.Context) codeEdgeEvaluatorCommittedReceiptCrashScenario {
	t.Helper()
	fixture := newCodeEdgeComplianceFixture(t, codeEdgeComplianceFixtureOptions{})
	t.Cleanup(func() { _ = fixture.database.Close() })
	run := resumeCodeEdgeRuntimeFixtureRun(t, ctx, fixture)
	frozen := codeEdgeRuntimeFrozenDefinition(t, run)
	stage, sourceJob, payload := createCodeEdgeEvaluatorRuntimeStageJob(t, ctx, fixture, run, workflowadapter.HarborRunQwen)
	descriptor, found := frozen.Workflow.Stage(payload.StageKey)
	if !found {
		t.Fatalf("frozen CodeEdge workflow omits evaluator %q", payload.StageKey)
	}

	providerCalls := 0
	runtime := newFrozenRuntime(t, fixture.services, codeEdgeRuntimeRegistry(t, frozen.Workflow, func(callCtx context.Context, request workflowkit.StageExecutionRequest) (workflowkit.StageExecutionResult, error) {
		if request.Stage.Key != payload.StageKey {
			return completedFixtureStage(callCtx, request)
		}
		providerCalls++
		return workflowkit.StageExecutionResult{Outcome: workflowkit.Outcome{Status: workflowkit.StatusInterrupted}, ErrorText: "simulate evaluator interruption before durable projection"}, nil
	}))
	claim := claimCodeEdgeRuntimeStageJob(t, ctx, fixture.database, run.ID, sourceJob.ID)
	result, err := runtime.HandleDurableJob(ctx, DurableJobExecution{Claim: claim})
	state := result.State
	if err != nil || state != store.JobInDoubt || result.Failure == nil || providerCalls != 1 {
		t.Fatalf("initial CodeEdge evaluator in_doubt result = %+v err=%v providerCalls=%d", result, err, providerCalls)
	}
	assertCodeEdgeEvaluatorInDoubt(t, ctx, fixture.database, run.ID, stage, payload.StageKey)

	currentStage, err := fixture.database.GetStageAttempt(ctx, stage.ID)
	if err != nil || currentStage == nil {
		t.Fatalf("load in_doubt CodeEdge evaluator stage = %+v, %v", currentStage, err)
	}
	currentStageValue, err := fixture.database.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: currentStage.ID, ExpectedVersion: currentStage.Version, ExecutionStatus: store.StageExecutionReconciling,
		Actor: runtimeFixtureActor, Reason: "simulate observer projection before crash",
	})
	if err != nil {
		t.Fatal(err)
	}
	node := latestNodeAttempt(ctx, fixture.database, stage.ID)
	if node.ID == "" || node.Status != store.NodeAttemptInDoubt {
		t.Fatalf("in_doubt CodeEdge evaluator node = %+v", node)
	}
	inputs, err := resolveStageInputs(ctx, fixture.database, fixture.services.core.objects, run, fixture.revision, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	taskRoot := fixture.services.core.layout.snapshotDirectory(fixture.revision.TaskID, fixture.revision.ID)
	bundle := codeEdgeHarborRunBundleBytes(t, taskRoot, fixture.childFrozen.Binding.TaskSnapshotDigest, fixture.frozen.Policy.QwenPolicy, "committed-receipt-qwen")
	converted := stageResultFromWorkflowkit(observedCodeEdgeEvaluatorResult(t, descriptor, bundle))
	manifest, _, err := persistStageArtifacts(ctx, fixture.services.core, run, fixture.revision, currentStageValue, node, descriptor, inputs, converted.Artifacts, runtimeFixtureActor, "simulate observed CodeEdge evaluator artifacts before crash")
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.services.CodeEdgeCompliance.completeTrustedTrialSet(ctx, run, currentStageValue, codeEdgeEvaluatorTrialCount, runtimeFixtureActor, "simulate trusted trials before reconciliation completion"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.reconcileObservedCodeEdgeEvaluatorQuota(ctx, run, sourceJob); err != nil {
		t.Fatal(err)
	}
	effect, err := fixture.database.GetSideEffectOperationByOperationKey(ctx, codeEdgeEvaluatorOperationKey(run.ID, stage.ID))
	if err != nil || effect == nil {
		t.Fatalf("load in_doubt CodeEdge evaluator effect = %+v, %v", effect, err)
	}
	effectValue, err := runtime.completeReconciledCodeEdgeEvaluatorEffect(ctx, *effect, manifest, runtimeFixtureActor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.database.TransitionNodeAttempt(ctx, store.TransitionNodeAttemptRequest{
		NodeAttemptID: node.ID, ExpectedVersion: node.Version, Status: store.NodeAttemptCompleted,
		Actor: runtimeFixtureActor, Reason: "simulate observed CodeEdge evaluator node before crash",
	}); err != nil {
		t.Fatal(err)
	}
	completedStage, err := fixture.database.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: currentStageValue.ID, ExpectedVersion: currentStageValue.Version, ExecutionStatus: store.StageExecutionCompleted,
		Verdict: store.VerdictPass, ArtifactManifestID: manifest.ID, Actor: runtimeFixtureActor,
		Reason: "simulate observed CodeEdge evaluator stage before reconciliation completion",
	})
	if err != nil {
		t.Fatal(err)
	}
	reconciliation, err := fixture.database.GetReconciliationAttemptByOperationKey(ctx, codeEdgeEvaluatorReconciliationKey(effectValue.OperationKey))
	if err != nil || reconciliation == nil || reconciliation.State != store.ReconciliationRunning {
		t.Fatalf("crash-window CodeEdge reconciliation = %+v, %v", reconciliation, err)
	}
	observer := &codeEdgeEvaluatorTestObserver{err: errors.New("committed receipt recovery must not re-read Harbor evidence")}
	fixture.services.core.evaluatorObserver = observer
	reconciliationJob := requireCodeEdgeEvaluatorReconciliationJob(t, ctx, fixture.database, sourceJob, run, completedStage, payload.StageKey)
	return codeEdgeEvaluatorCommittedReceiptCrashScenario{
		fixture: fixture, runtime: runtime, run: run, sourceJob: sourceJob,
		reconciliationJob: reconciliationJob, effect: effectValue, observer: observer, providerCalls: &providerCalls,
	}
}

func codeEdgeRuntimeFrozenDefinition(t *testing.T, run store.WorkflowRun) frozenRunDefinition {
	t.Helper()
	frozen, err := decodeFrozenRunDefinition(run)
	if err != nil {
		t.Fatal(err)
	}
	return frozen
}

func codeEdgeRuntimeRegistry(t *testing.T, workflow workflowkit.WorkflowDescriptor, executor workflowkit.StageExecutorFunc) *workflowkit.ControlledPluginRegistry[workflowkit.StageExecutor] {
	t.Helper()
	registrations := make([]workflowkit.PluginRegistration[workflowkit.StageExecutor], 0, len(workflow.Stages))
	seen := make(map[string]struct{}, len(workflow.Stages))
	for _, stage := range workflow.Stages {
		key := stage.Plugin.ID + "\x00" + stage.Plugin.Version
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		registrations = append(registrations, workflowkit.PluginRegistration[workflowkit.StageExecutor]{Binding: stage.Plugin, Implementation: executor})
	}
	registry, err := workflowkit.NewControlledPluginRegistry(registrations)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func claimCodeEdgeRuntimeStageJob(t *testing.T, ctx context.Context, database *store.Store, runID, jobID string) store.DurableJobDispatchClaim {
	t.Helper()
	claim, err := database.ClaimNextDurableJob(ctx, store.ClaimNextDurableJobRequest{
		IdempotencyKey: "claim-codeedge-evaluator-runtime:" + jobID, RunID: runID, Owner: "codeedge-evaluator-runtime-worker",
		LeaseTTL: time.Minute, Actor: runtimeFixtureActor, Reason: "claim CodeEdge evaluator lifecycle fixture",
	})
	if err != nil || claim.Job == nil || claim.Job.ID != jobID {
		t.Fatalf("claim CodeEdge evaluator stage job = %+v, %v", claim, err)
	}
	return claim
}

func requireCodeEdgeEvaluatorReconciliationJob(t *testing.T, ctx context.Context, database *store.Store, sourceJob store.DurableJob, run store.WorkflowRun, stage store.StageAttempt, stageKey workflowkit.StageKey) store.DurableJob {
	t.Helper()
	job, err := database.GetDurableJobByIdempotency(ctx, codeEdgeEvaluatorReconciliationJobKey(sourceJob.ID))
	if err != nil || job == nil {
		t.Fatalf("read CodeEdge evaluator reconciliation job = %+v, %v", job, err)
	}
	if job.CommandType != codeEdgeEvaluatorReconciliationCommandType || job.EntityType != codeEdgeEvaluatorReconciliationEntityType || job.EntityID != stage.ID || job.RunID != run.ID || job.StageAttemptID != stage.ID || job.CreatedBy != sourceJob.CreatedBy {
		t.Fatalf("CodeEdge evaluator reconciliation job binding = %+v", job)
	}
	var sourcePayload frozenStageExecutionPayload
	if err := decodeStrictJSON(sourceJob.PayloadJSON, &sourcePayload); err != nil {
		t.Fatalf("decode source stage job payload = %v", err)
	}
	var payload codeEdgeEvaluatorReconciliationPayload
	if err := decodeStrictJSON(job.PayloadJSON, &payload); err != nil {
		t.Fatalf("decode CodeEdge evaluator reconciliation job payload = %v", err)
	}
	if payload.Format != codeEdgeEvaluatorReconciliationPayloadFormat || payload.RunID != run.ID || payload.StageAttemptID != stage.ID || payload.StageKey != stageKey || payload.DefinitionHash != sourcePayload.DefinitionHash || payload.Generation != sourcePayload.Generation || payload.SourceStageJobID != sourceJob.ID || !reflect.DeepEqual(payload.QuotaPolicy, sourcePayload.QuotaPolicy) {
		t.Fatalf("CodeEdge evaluator reconciliation payload = %+v; source=%+v", payload, sourcePayload)
	}
	return *job
}

type codeEdgeEvaluatorTestObserver struct {
	result                workflowkit.StageExecutionResult
	observed              bool
	err                   error
	calls                 int
	request               CodeEdgeEvaluatorObservationRequest
	requireReadableInputs bool
}

func (observer *codeEdgeEvaluatorTestObserver) ObserveCompletedCodeEdgeEvaluator(ctx context.Context, request CodeEdgeEvaluatorObservationRequest) (workflowkit.StageExecutionResult, bool, error) {
	observer.calls++
	observer.request = request
	if observer.requireReadableInputs {
		if len(request.Execution.Inputs) == 0 {
			return workflowkit.StageExecutionResult{}, false, errors.New("observer received no immutable inputs")
		}
		for _, binding := range request.Execution.Inputs {
			if _, err := request.Execution.ReadInput(ctx, binding); err != nil {
				return workflowkit.StageExecutionResult{}, false, fmt.Errorf("read immutable observer input %q: %w", binding.Name, err)
			}
		}
	}
	return observer.result, observer.observed, observer.err
}

func observedCodeEdgeEvaluatorResult(t *testing.T, stage workflowkit.StageDescriptor, bundle []byte) workflowkit.StageExecutionResult {
	t.Helper()
	artifacts := make([]workflowkit.StageArtifact, 0, len(stage.Outputs))
	for _, output := range stage.Outputs {
		content := codeEdgePNG(t)
		if output.Name == evaluatorResultArtifactKey(string(stage.Key)) {
			content = bundle
		}
		artifacts = append(artifacts, workflowkit.StageArtifact{Name: output.Name, SchemaVersion: output.SchemaVersion, Content: content})
	}
	return workflowkit.StageExecutionResult{
		Outcome: workflowkit.Outcome{Status: workflowkit.StatusCompleted, Verdict: workflowkit.VerdictPass}, Artifacts: artifacts,
	}
}

func assertCodeEdgeEvaluatorInDoubt(t *testing.T, ctx context.Context, database *store.Store, runID string, stage store.StageAttempt, stageKey workflowkit.StageKey) {
	t.Helper()
	trials, err := database.ListTrialExecutionsForStageAttempt(ctx, stage.ID)
	if err != nil || len(trials) != codeEdgeEvaluatorTrialCount {
		t.Fatalf("CodeEdge evaluator trials = %+v, %v", trials, err)
	}
	for _, trial := range trials {
		attempts, listErr := database.ListTrialAttemptsForTrialExecution(ctx, trial.ID)
		if listErr != nil || trial.Status != store.TrialExecutionInDoubt || len(attempts) != 1 || attempts[0].Status != store.TrialAttemptInDoubt {
			t.Fatalf("CodeEdge evaluator unknown TrialExecution = %+v attempts=%+v err=%v", trial, attempts, listErr)
		}
	}
	effect, err := database.GetSideEffectOperationByOperationKey(ctx, codeEdgeEvaluatorOperationKey(runID, stage.ID))
	if err != nil || effect == nil || effect.State != store.SideEffectUnknown {
		t.Fatalf("CodeEdge evaluator side effect = %+v, %v; want unknown", effect, err)
	}
	reconciliation, err := database.GetReconciliationAttemptByOperationKey(ctx, codeEdgeEvaluatorReconciliationKey(effect.OperationKey))
	if err != nil || reconciliation == nil || reconciliation.State != store.ReconciliationRunning || reconciliation.SubjectID != stage.ID || reconciliation.SideEffectOperationID != effect.ID {
		t.Fatalf("CodeEdge evaluator reconciliation = %+v, %v", reconciliation, err)
	}
	updatedStage, err := database.GetStageAttempt(ctx, stage.ID)
	if err != nil || updatedStage == nil || updatedStage.ExecutionStatus != store.StageExecutionInDoubt {
		t.Fatalf("CodeEdge evaluator stage = %+v, %v; want in_doubt", updatedStage, err)
	}
	run, err := database.GetWorkflowRun(ctx, runID)
	if err != nil || run == nil || run.Status != store.WorkflowRunInDoubt {
		t.Fatalf("CodeEdge evaluator Run = %+v, %v; want in_doubt", run, err)
	}
	if updatedStage.StageKey != string(stageKey) {
		t.Fatalf("CodeEdge evaluator stage key = %q, want %q", updatedStage.StageKey, stageKey)
	}
}
