package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/testsupport"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestRunAttachCommandEmitsReadOnlyAttachmentWithoutDurableEffects(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	services := openCommandLifecycle(t, root)
	task, revision, err := services.Tasks.ImportTask(ctx, app.ImportTaskRequest{
		CreateDraftTaskRequest: app.CreateDraftTaskRequest{Slug: "attach-command", Actor: "tester", Reason: "run attach command fixture"},
		SourceDirectory:        writeCommandTaskSnapshot(t, "attach command fixture\n"),
	})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	run, err := services.Runs.StartRun(ctx, app.StartRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, Profile: commandCompleteProfile(t), ExecutionSpec: commandExecutionSpec(task.ID, revision.ID, revision.TaskDigest), Trigger: "attach-command", Actor: "tester", Reason: "start attach command fixture",
	})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	run, err = services.Store().TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunRunning, Actor: "tester", Reason: "start attach command worker",
	})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	jobs, err := services.Store().ListDurableJobsForRun(ctx, run.ID)
	if err != nil || len(jobs) != 1 {
		services.Store().Close()
		t.Fatalf("list attach command durable job = %+v, %v", jobs, err)
	}
	claim, err := services.Store().ClaimNextDurableJob(ctx, store.ClaimNextDurableJobRequest{
		IdempotencyKey: "attach-command-claim", Owner: "attach-command-worker", LeaseTTL: time.Minute, Actor: "tester", Reason: "claim attach command job",
	})
	if err != nil || claim.Job == nil || claim.DispatchLease == nil || claim.Job.ID != jobs[0].ID {
		services.Store().Close()
		t.Fatalf("claim attach command durable job = %+v, %v", claim, err)
	}
	if err := services.Store().Close(); err != nil {
		t.Fatal(err)
	}

	before := snapshotCommandControlPlane(t, root)
	command := newRunCommandV2(&lifecycleCLIConfig{root: root})
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{"attach", "--run", run.ID})
	if err := command.ExecuteContext(ctx); err != nil {
		t.Fatalf("run attach command: %v\n%s", err, output.String())
	}
	var attachment app.RunAttachment
	if err := json.Unmarshal(output.Bytes(), &attachment); err != nil {
		t.Fatalf("decode run attach output: %v\n%s", err, output.String())
	}
	if attachment.Run.ID != run.ID || attachment.AttachableJobs != 1 || len(attachment.Jobs) != 1 || !attachment.Jobs[0].Attachable {
		t.Fatalf("run attach output = %+v", attachment)
	}
	if after := snapshotCommandControlPlane(t, root); !reflect.DeepEqual(after, before) {
		t.Fatal("run attach changed the control plane or managed files")
	}
}

func TestRunReconcileCommandRequiresReasonAndScopesRecoveryToNamedRun(t *testing.T) {
	ctx := context.Background()
	actor := defaultLifecycleActor()
	if actor == "" {
		t.Skip("local OS actor is unavailable in this test environment")
	}
	root := t.TempDir()
	services := openCommandLifecycle(t, root)
	task, revision, err := services.Tasks.ImportTask(ctx, app.ImportTaskRequest{
		CreateDraftTaskRequest: app.CreateDraftTaskRequest{Slug: "reconcile-command", Actor: actor, Reason: "run reconcile command fixture"},
		SourceDirectory:        writeCommandTaskSnapshot(t, "reconcile command fixture\n"),
	})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	runA := startCommandLocalRuntimeRun(t, ctx, services, task.ID, revision.ID, actor, "reconcile-command-a")
	runB := startCommandLocalRuntimeRun(t, ctx, services, task.ID, revision.ID, actor, "reconcile-command-b")
	claimA := claimCommandLocalRuntimeRunJob(t, ctx, services, runA.ID, "reconcile-command-claim-a")
	claimB := claimCommandLocalRuntimeRunJob(t, ctx, services, runB.ID, "reconcile-command-claim-b")
	time.Sleep(40 * time.Millisecond)
	if err := services.Store().Close(); err != nil {
		t.Fatal(err)
	}

	config := &lifecycleCLIConfig{root: root}
	missingReason := newRunCommandV2(config)
	missingReason.SetArgs([]string{"reconcile", "--run", runA.ID})
	if err := missingReason.ExecuteContext(ctx); err == nil {
		t.Fatal("run reconcile accepted a missing audit reason")
	}

	command := newRunCommandV2(config)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{"reconcile", "--run", runA.ID, "--reason", "recover selected local worker"})
	if err := command.ExecuteContext(ctx); err != nil {
		t.Fatalf("run reconcile command: %v\n%s", err, output.String())
	}
	var result app.RunReconciliationResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("decode run reconcile output: %v\n%s", err, output.String())
	}
	if len(result.RecoveredJobs) != 1 || result.RecoveredJobs[0].Job.ID != claimA.Job.ID || result.RecoveredJobs[0].Job.State != store.JobInDoubt || result.RecoveredJobs[0].Job.Failure == nil || result.RecoveredJobs[0].Job.Failure.Code != "job.lease_lost" {
		t.Fatalf("run reconcile output = %+v", result)
	}

	check := openCommandLifecycle(t, root)
	defer check.Store().Close()
	selected, err := check.Store().GetDurableJob(ctx, claimA.Job.ID)
	if err != nil || selected == nil || selected.State != store.JobInDoubt || selected.Failure == nil || selected.Failure.Code != "job.lease_lost" {
		t.Fatalf("selected Run job after reconcile = %+v, %v", selected, err)
	}
	unselected, err := check.Store().GetDurableJob(ctx, claimB.Job.ID)
	if err != nil || unselected == nil || unselected.State != store.JobRunning {
		t.Fatalf("unselected Run job changed by reconcile = %+v, %v", unselected, err)
	}
	unselectedLease, err := check.Store().GetLease(ctx, claimB.DispatchLease.ID)
	if err != nil || unselectedLease == nil || unselectedLease.State != store.LeaseActive {
		t.Fatalf("unselected Run lease changed by reconcile = %+v, %v", unselectedLease, err)
	}
}

// TestRunReconcileCommandLeavesCodeEdgeEvaluatorInDoubtLocalOnly protects the
// deliberately narrow CLI contract. A generic local recovery may fence an
// expired worker delivery, but it must not observe a Harbor evaluator, invoke
// a provider, append a technical TrialAttempt, or manufacture a remote
// receipt. Those actions belong solely to the separately dispatched,
// provider-specific reconciliation command.
func TestRunReconcileCommandLeavesCodeEdgeEvaluatorInDoubtLocalOnly(t *testing.T) {
	ctx := context.Background()
	actor := defaultLifecycleActor()
	if actor == "" {
		t.Skip("local OS actor is unavailable in this test environment")
	}
	root := t.TempDir()
	probe := &commandCodeEdgeEvaluatorReconcileProbe{}
	factory := func(factoryRoot string, database *store.Store) (*app.LifecycleServices, error) {
		if factoryRoot != root {
			t.Fatalf("CodeEdge reconcile factory root = %q, want %q", factoryRoot, root)
		}
		return app.NewLifecycleServicesWithOptions(factoryRoot, database, app.LifecycleServicesOptions{
			OperationResolver:         testsupport.AcceptAllStageOperationResolver(),
			CodeEdgeEvaluatorObserver: probe,
		})
	}
	services := openCommandEvaluatorLifecycle(t, root, factory)
	task, revision, err := services.Tasks.ImportTask(ctx, app.ImportTaskRequest{
		CreateDraftTaskRequest: app.CreateDraftTaskRequest{
			Slug: "codeedge-local-reconcile", Actor: actor, Reason: "create CodeEdge evaluator local-reconcile fixture",
		},
		SourceDirectory: writeCommandTaskSnapshot(t, "CodeEdge evaluator local reconcile fixture\n"),
	})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	run, err := services.Runs.StartRun(ctx, app.StartRunRequest{
		TaskID: task.ID, RevisionID: revision.ID,
		Profile:       commandCodeEdgeEvaluatorProfile(t),
		ExecutionSpec: testsupport.CompleteCodeEdgeEvaluatorChildRunExecutionSpec(task.ID, revision.ID, revision.TaskDigest),
		Trigger:       "codeedge-local-reconcile", Actor: actor, Reason: "start CodeEdge evaluator local-reconcile fixture",
	})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	run, err = services.Store().TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunRunning,
		Actor: actor, Reason: "begin CodeEdge evaluator local-reconcile fixture",
	})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	stage, err := services.Store().CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: run.ID, StageKey: workflowadapter.HarborRunQwen, StageGroup: string(workflowadapter.StageEvaluation), Ordinal: 1,
		InputFingerprint: "sha256:codeedge-local-reconcile-input", BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`,
		Actor: actor, Reason: "create CodeEdge evaluator attempt with an unknown external outcome",
	})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	stage, err = services.Store().TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: stage.ID, ExpectedVersion: stage.Version, ExecutionStatus: store.StageExecutionRunning,
		Actor: actor, Reason: "begin CodeEdge evaluator attempt",
	})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	for ordinal := 1; ordinal <= 4; ordinal++ {
		trial, createErr := services.Store().CreateTrialExecution(ctx, store.CreateTrialExecutionRequest{
			RunID: run.ID, StageAttemptID: stage.ID, StageKey: workflowadapter.HarborRunQwen, Ordinal: ordinal,
			Actor: actor, Reason: "preallocate CodeEdge logical evaluator trial",
		})
		if createErr != nil {
			services.Store().Close()
			t.Fatal(createErr)
		}
		attempt, createErr := services.Store().CreateTrialAttempt(ctx, store.CreateTrialAttemptRequest{
			TrialExecutionID: trial.ID, Ordinal: 1, Actor: actor, Reason: "start CodeEdge evaluator technical trial",
		})
		if createErr != nil {
			services.Store().Close()
			t.Fatal(createErr)
		}
		attempt, createErr = services.Store().TransitionTrialAttempt(ctx, store.TransitionTrialAttemptRequest{
			TrialAttemptID: attempt.ID, ExpectedVersion: attempt.Version, Status: store.TrialAttemptRunning,
			Actor: actor, Reason: "invoke CodeEdge evaluator technical trial",
		})
		if createErr != nil {
			services.Store().Close()
			t.Fatal(createErr)
		}
		if _, createErr = services.Store().TransitionTrialAttempt(ctx, store.TransitionTrialAttemptRequest{
			TrialAttemptID: attempt.ID, ExpectedVersion: attempt.Version, Status: store.TrialAttemptInDoubt,
			ErrorText: "external evaluator outcome is unknown", FailureClass: "external_outcome_unknown",
			Actor: actor, Reason: "fence CodeEdge evaluator technical trial pending provider reconciliation",
		}); createErr != nil {
			services.Store().Close()
			t.Fatal(createErr)
		}
	}
	effectKey := "codeedge-evaluator:v1:" + run.ID + ":" + stage.ID
	effect, err := services.Store().CreateSideEffectOperation(ctx, store.CreateSideEffectOperationRequest{
		OperationKey: effectKey, RunID: run.ID, StageAttemptID: stage.ID,
		EffectKind: "codeedge.phase1.evaluator.v1", IdempotencyKey: effectKey,
		SourceDigest: stage.InputFingerprint, PayloadJSON: `{"fixture":"CodeEdge evaluator external outcome unknown"}`,
		Actor: actor, Reason: "record unknown CodeEdge evaluator side effect",
	})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	effect, err = services.Store().TransitionSideEffectOperation(ctx, store.TransitionSideEffectOperationRequest{
		OperationID: effect.ID, ExpectedVersion: effect.Version, State: store.SideEffectStarted,
		Actor: actor, Reason: "record CodeEdge evaluator invocation fence",
	})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	effect, err = services.Store().TransitionSideEffectOperation(ctx, store.TransitionSideEffectOperationRequest{
		OperationID: effect.ID, ExpectedVersion: effect.Version, State: store.SideEffectUnknown,
		Actor: actor, Reason: "record CodeEdge evaluator unknown external outcome",
	})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	stage, err = services.Store().TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: stage.ID, ExpectedVersion: stage.Version, ExecutionStatus: store.StageExecutionInDoubt,
		ErrorText: "external evaluator outcome is unknown", FailureClass: "external_outcome_unknown",
		Actor: actor, Reason: "fence CodeEdge evaluator stage pending provider reconciliation",
	})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	run, err = services.Store().TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunInDoubt,
		Actor: actor, Reason: "fence CodeEdge evaluator Run pending provider reconciliation",
	})
	if err != nil {
		services.Store().Close()
		t.Fatal(err)
	}
	jobs, err := services.Store().ListDurableJobsForRun(ctx, run.ID)
	if err != nil || len(jobs) != 1 {
		services.Store().Close()
		t.Fatalf("list CodeEdge evaluator fixture jobs = %+v, %v", jobs, err)
	}
	claim, err := services.Store().ClaimNextDurableJob(ctx, store.ClaimNextDurableJobRequest{
		IdempotencyKey: "codeedge-local-reconcile-claim", RunID: run.ID, Owner: "codeedge-local-reconcile-worker",
		LeaseTTL: 10 * time.Millisecond, Actor: actor, Reason: "simulate lost local evaluator worker lease",
	})
	if err != nil || claim.Job == nil || claim.DispatchLease == nil || claim.Job.ID != jobs[0].ID {
		services.Store().Close()
		t.Fatalf("claim CodeEdge evaluator fixture job = %+v, %v", claim, err)
	}
	beforeTrials, beforeAttempts := snapshotCommandCodeEdgeEvaluatorTrials(t, ctx, services.Store(), stage.ID)
	beforeRun := run
	beforeStage := stage
	beforeEffect := effect
	beforeReceipt, err := services.Store().GetReconciliationAttemptByOperationKey(ctx, "codeedge-evaluator-reconcile:v1:"+effectKey)
	if err != nil || beforeReceipt != nil {
		services.Store().Close()
		t.Fatalf("unexpected CodeEdge evaluator provider receipt before local reconcile = %+v, %v", beforeReceipt, err)
	}
	time.Sleep(40 * time.Millisecond)
	if err := services.Store().Close(); err != nil {
		t.Fatal(err)
	}

	config := &lifecycleCLIConfig{root: root, newLifecycleService: factory}
	command := newRunCommandV2(config)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{"reconcile", "--run", run.ID, "--reason", "recover only the lost local worker lease"})
	if err := command.ExecuteContext(ctx); err != nil {
		t.Fatalf("run reconcile CodeEdge evaluator fixture: %v\n%s", err, output.String())
	}
	var result app.RunReconciliationResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("decode CodeEdge evaluator local reconcile output: %v\n%s", err, output.String())
	}
	if len(result.RecoveredJobs) != 1 || result.RecoveredJobs[0].Job.ID != claim.Job.ID || result.RecoveredJobs[0].Job.State != store.JobInDoubt || result.RecoveredJobs[0].Job.Failure == nil || result.RecoveredJobs[0].Job.Failure.Code != "job.lease_lost" {
		t.Fatalf("CodeEdge evaluator local reconcile result = %+v", result)
	}
	if probe.observerCalls != 0 || probe.providerCalls != 0 {
		t.Fatalf("local run reconcile called CodeEdge observer/provider: observer=%d provider=%d", probe.observerCalls, probe.providerCalls)
	}

	check := openCommandEvaluatorLifecycle(t, root, factory)
	defer check.Store().Close()
	afterRun, err := check.Store().GetWorkflowRun(ctx, run.ID)
	if err != nil || afterRun == nil || afterRun.Status != store.WorkflowRunInDoubt || !reflect.DeepEqual(*afterRun, beforeRun) {
		t.Fatalf("CodeEdge evaluator Run after local reconcile = %+v, %v; want unchanged in_doubt %+v", afterRun, err, beforeRun)
	}
	afterStage, err := check.Store().GetStageAttempt(ctx, stage.ID)
	if err != nil || afterStage == nil || afterStage.ExecutionStatus != store.StageExecutionInDoubt || !reflect.DeepEqual(*afterStage, beforeStage) {
		t.Fatalf("CodeEdge evaluator stage after local reconcile = %+v, %v; want unchanged in_doubt %+v", afterStage, err, beforeStage)
	}
	afterEffect, err := check.Store().GetSideEffectOperationByOperationKey(ctx, effectKey)
	if err != nil || afterEffect == nil || afterEffect.State != store.SideEffectUnknown || afterEffect.ReceiptRef != "" || !reflect.DeepEqual(*afterEffect, beforeEffect) {
		t.Fatalf("CodeEdge evaluator external receipt after local reconcile = %+v, %v; want unchanged unknown effect %+v", afterEffect, err, beforeEffect)
	}
	afterTrials, afterAttempts := snapshotCommandCodeEdgeEvaluatorTrials(t, ctx, check.Store(), stage.ID)
	if !reflect.DeepEqual(afterTrials, beforeTrials) || !reflect.DeepEqual(afterAttempts, beforeAttempts) {
		t.Fatalf("local reconcile changed CodeEdge evaluator trials: executions=%+v attempts=%+v; want executions=%+v attempts=%+v", afterTrials, afterAttempts, beforeTrials, beforeAttempts)
	}
	for _, trial := range afterTrials {
		if trial.Status != store.TrialExecutionInDoubt || len(afterAttempts[trial.ID]) != 1 || afterAttempts[trial.ID][0].Status != store.TrialAttemptInDoubt {
			t.Fatalf("CodeEdge evaluator trial after local reconcile = %+v attempts=%+v; want one in_doubt technical attempt", trial, afterAttempts[trial.ID])
		}
	}
	afterReceipt, err := check.Store().GetReconciliationAttemptByOperationKey(ctx, "codeedge-evaluator-reconcile:v1:"+effectKey)
	if err != nil || afterReceipt != nil {
		t.Fatalf("local run reconcile created a CodeEdge evaluator provider receipt = %+v, %v", afterReceipt, err)
	}
	afterJob, err := check.Store().GetDurableJob(ctx, claim.Job.ID)
	if err != nil || afterJob == nil || afterJob.State != store.JobInDoubt || afterJob.Failure == nil || afterJob.Failure.Code != "job.lease_lost" {
		t.Fatalf("local evaluator worker job after reconcile = %+v, %v", afterJob, err)
	}
}

type commandCodeEdgeEvaluatorReconcileProbe struct {
	observerCalls int
	providerCalls int
}

var _ app.CodeEdgeEvaluatorCompletedObserver = (*commandCodeEdgeEvaluatorReconcileProbe)(nil)

func (probe *commandCodeEdgeEvaluatorReconcileProbe) ObserveCompletedCodeEdgeEvaluator(context.Context, app.CodeEdgeEvaluatorObservationRequest) (workflowkit.StageExecutionResult, bool, error) {
	probe.observerCalls++
	// In production, a provider-specific observer reaches the controlled
	// read-only provider adapter only after it has accepted the observation.
	// Keep that boundary explicit in the probe so an accidental local CLI call
	// is visible as both an observer and provider-side effect.
	probe.providerCalls++
	return workflowkit.StageExecutionResult{}, false, nil
}

func snapshotCommandCodeEdgeEvaluatorTrials(t *testing.T, ctx context.Context, database *store.Store, stageAttemptID string) ([]store.TrialExecution, map[string][]store.TrialAttempt) {
	t.Helper()
	trials, err := database.ListTrialExecutionsForStageAttempt(ctx, stageAttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if len(trials) != 4 {
		t.Fatalf("CodeEdge evaluator logical trial count = %d, want 4", len(trials))
	}
	attempts := make(map[string][]store.TrialAttempt, len(trials))
	for _, trial := range trials {
		technical, listErr := database.ListTrialAttemptsForTrialExecution(ctx, trial.ID)
		if listErr != nil {
			t.Fatal(listErr)
		}
		attempts[trial.ID] = technical
	}
	return trials, attempts
}

func startCommandLocalRuntimeRun(t *testing.T, ctx context.Context, services *app.LifecycleServices, taskID, revisionID, actor, trigger string) store.WorkflowRun {
	t.Helper()
	revision, err := services.Store().GetTaskRevision(ctx, revisionID)
	if err != nil || revision == nil || revision.TaskID != taskID {
		t.Fatalf("load command local runtime revision = %+v, %v", revision, err)
	}
	run, err := services.Runs.StartRun(ctx, app.StartRunRequest{
		TaskID: taskID, RevisionID: revisionID, Profile: commandCompleteProfile(t), ExecutionSpec: commandExecutionSpec(taskID, revisionID, revision.TaskDigest), Trigger: trigger, Actor: actor, Reason: "start command local runtime fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err = services.Store().TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunRunning, Actor: actor, Reason: "start command local runtime worker",
	})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func claimCommandLocalRuntimeRunJob(t *testing.T, ctx context.Context, services *app.LifecycleServices, runID, key string) store.DurableJobDispatchClaim {
	t.Helper()
	jobs, err := services.Store().ListDurableJobsForRun(ctx, runID)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("list command local runtime jobs = %+v, %v", jobs, err)
	}
	claim, err := services.Store().ClaimNextDurableJob(ctx, store.ClaimNextDurableJobRequest{
		IdempotencyKey: key, Owner: "command-local-runtime-worker", LeaseTTL: 10 * time.Millisecond,
		Actor: "command-local-runtime-worker", Reason: "claim command local runtime job",
	})
	if err != nil || claim.Job == nil || claim.DispatchLease == nil || claim.Job.ID != jobs[0].ID {
		t.Fatalf("claim command local runtime job = %+v, %v", claim, err)
	}
	return claim
}
