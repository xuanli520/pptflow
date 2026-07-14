package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

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

	state, err := runtime.HandleDurableJob(ctx, DurableJobExecution{Claim: claim})
	if err != nil || state != store.JobInterrupted || !invoked || !trialsWereReady {
		t.Fatalf("CodeEdge evaluator interrupted invocation = state=%s err=%v invoked=%t trialsWereReady=%t", state, err, invoked, trialsWereReady)
	}
	assertCodeEdgeEvaluatorInDoubt(t, ctx, fixture.database, run.ID, stage, payload.StageKey)
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

	state, err := runtime.HandleDurableJob(ctx, DurableJobExecution{Claim: claim})
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

func TestCodeEdgeEvaluatorRuntimeLeavesSuccessfulTrialsForTrustedReceiptProjection(t *testing.T) {
	ctx := context.Background()
	fixture := newCodeEdgeComplianceFixture(t, codeEdgeComplianceFixtureOptions{})
	defer fixture.database.Close()
	run := resumeCodeEdgeRuntimeFixtureRun(t, ctx, fixture)
	frozen := codeEdgeRuntimeFrozenDefinition(t, run)
	stage, job, _ := createCodeEdgeEvaluatorRuntimeStageJob(t, ctx, fixture, run, workflowadapter.HarborRunQwen)
	runtime := newFrozenRuntime(t, fixture.services, codeEdgeRuntimeRegistry(t, frozen.Workflow, completedFixtureStage))
	claim := claimCodeEdgeRuntimeStageJob(t, ctx, fixture.database, run.ID, job.ID)

	state, err := runtime.HandleDurableJob(ctx, DurableJobExecution{Claim: claim})
	if err != nil || state != store.JobSucceeded {
		t.Fatalf("successful CodeEdge evaluator invocation = state=%s err=%v", state, err)
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
	for _, trial := range trials {
		attempts, listErr := fixture.database.ListTrialAttemptsForTrialExecution(ctx, trial.ID)
		if listErr != nil || trial.Status != store.TrialExecutionRunning || len(attempts) != 1 || attempts[0].Status != store.TrialAttemptRunning {
			t.Fatalf("provider success prematurely trusted TrialExecution=%+v attempts=%+v err=%v", trial, attempts, listErr)
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
	if err := runtime.reconcileRecoveredStageJob(ctx, job); err != nil {
		t.Fatalf("replay expired CodeEdge evaluator stage reconciliation = %v", err)
	}
	replayedReconciliation, err := fixture.database.GetReconciliationAttemptByOperationKey(ctx, codeEdgeEvaluatorReconciliationKey(fence.Operation.OperationKey))
	if err != nil || replayedReconciliation == nil || replayedReconciliation.ID != firstReconciliation.ID {
		t.Fatalf("CodeEdge evaluator reconciliation replay = %+v, %v; want %s", replayedReconciliation, err, firstReconciliation.ID)
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
		ID: workflowadapter.CodeEdgePhase1WorkflowTemplateID, Version: workflowadapter.CodeEdgePhase1WorkflowTemplateVersion,
		Stages: []workflowkit.StageDescriptor{{Key: workflowkit.StageKey(workflowadapter.HarborRunQwen), Group: "evaluation"}},
	}
	err := rejectContentContinuationTargets(workflow, []workflowkit.NodeID{workflowkit.NodeID(workflowadapter.HarborRunQwen)})
	if !errors.Is(err, ErrTaskContinuationTarget) {
		t.Fatalf("CodeEdge evaluator ordinary continuation retry error = %v, want target rejection", err)
	}
}

func resumeCodeEdgeRuntimeFixtureRun(t *testing.T, ctx context.Context, fixture *codeEdgeComplianceFixture) store.WorkflowRun {
	t.Helper()
	run, err := fixture.database.GetWorkflowRun(ctx, fixture.run.ID)
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
	seedCodeEdgeRuntimeTaskSnapshot(t, ctx, fixture, run, descriptor)
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

// The final-compliance fixture intentionally seeds only the receipt evidence
// it validates. Runtime execution additionally needs a managed immutable task
// snapshot binding, so this test-local producer creates that lineage through
// the same object/manifest APIs used by real stage outputs.
func seedCodeEdgeRuntimeTaskSnapshot(t *testing.T, ctx context.Context, fixture *codeEdgeComplianceFixture, run store.WorkflowRun, evaluator workflowkit.StageDescriptor) {
	t.Helper()
	var snapshot workflowkit.ArtifactSpec
	found := false
	for _, input := range evaluator.Inputs {
		if input.Name == "task_snapshot" {
			snapshot = input
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("CodeEdge evaluator %q has no task_snapshot input", evaluator.Key)
	}
	emptyInputs, err := workflowkit.FingerprintArtifactBindings(nil)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := fixture.database.CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: run.ID, StageKey: "codeedge-runtime-task-snapshot-fixture", StageGroup: "fixture", Ordinal: 1,
		InputFingerprint: string(emptyInputs), BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`,
		Actor: runtimeFixtureActor, Reason: "seed immutable CodeEdge runtime task snapshot input",
	})
	if err != nil {
		t.Fatal(err)
	}
	stage, err = fixture.database.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: stage.ID, ExpectedVersion: stage.Version, ExecutionStatus: store.StageExecutionRunning,
		Actor: runtimeFixtureActor, Reason: "materialize immutable CodeEdge runtime task snapshot input",
	})
	if err != nil {
		t.Fatal(err)
	}
	node, err := fixture.database.CreateNodeAttempt(ctx, store.CreateNodeAttemptRequest{
		StageAttemptID: stage.ID, NodeID: stage.StageKey, Generation: 0, Attempt: 1,
		IdempotencyKey: "codeedge-runtime-task-snapshot-node:" + stage.ID, Actor: runtimeFixtureActor,
		Reason: "materialize immutable CodeEdge runtime task snapshot input",
	})
	if err != nil {
		t.Fatal(err)
	}
	node, err = fixture.database.TransitionNodeAttempt(ctx, store.TransitionNodeAttemptRequest{
		NodeAttemptID: node.ID, ExpectedVersion: node.Version, Status: store.NodeAttemptRunning,
		Actor: runtimeFixtureActor, Reason: "materialize immutable CodeEdge runtime task snapshot input",
	})
	if err != nil {
		t.Fatal(err)
	}
	producer := workflowkit.StageDescriptor{
		Key: workflowkit.StageKey(stage.StageKey), Version: "fixture", Outputs: []workflowkit.ArtifactSpec{{
			Name: snapshot.Name, SchemaVersion: snapshot.SchemaVersion, Required: true,
		}},
	}
	manifest, _, err := persistStageArtifacts(ctx, fixture.services.core, run, fixture.revision, stage, node, producer, nil, []StageArtifact{{
		Key: snapshot.Name, SchemaVersion: snapshot.SchemaVersion, Content: []byte("immutable CodeEdge task snapshot fixture\n"), TurnOrdinal: 1,
	}}, runtimeFixtureActor, "persist immutable CodeEdge runtime task snapshot input")
	if err != nil {
		t.Fatal(err)
	}
	stage, err = fixture.database.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: stage.ID, ExpectedVersion: stage.Version, ExecutionStatus: store.StageExecutionCompleted, Verdict: store.VerdictPass,
		ArtifactManifestID: manifest.ID, Actor: runtimeFixtureActor, Reason: "complete immutable CodeEdge runtime task snapshot input",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.database.TransitionNodeAttempt(ctx, store.TransitionNodeAttemptRequest{
		NodeAttemptID: node.ID, ExpectedVersion: node.Version, Status: store.NodeAttemptCompleted,
		Actor: runtimeFixtureActor, Reason: "complete immutable CodeEdge runtime task snapshot input",
	}); err != nil {
		t.Fatal(err)
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
