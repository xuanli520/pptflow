package app

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	parallelCoordinatorLeftStage  = workflowkit.StageKey("parallel-coordinator-left")
	parallelCoordinatorRightStage = workflowkit.StageKey("parallel-coordinator-right")
	parallelCoordinatorJoinStage  = workflowkit.StageKey("parallel-coordinator-join")
)

// TestFrozenExecutionRuntimeCoordinatorOnlyEnqueuesMissingPeerFromActiveBatch
// is deliberately an app/store integration test rather than a direct
// workflowkit unit test.  It proves the runtime projects durable state into
// the public coordinator and obeys its partial-batch decision: a queued peer
// is not duplicated, the missing peer is admitted, and the dependent batch is
// not brought forward.
func TestFrozenExecutionRuntimeCoordinatorOnlyEnqueuesMissingPeerFromActiveBatch(t *testing.T) {
	ctx := context.Background()
	fixture := newParallelCoordinatorRuntimeFixture(t)
	defer fixture.store.Close()

	runtime := newFrozenRuntime(t, fixture.services, frozenRuntimeRegistry(t, fixture.frozen.Workflow, completedFixtureStage))
	plan, err := initialRuntimeExecutionPlan(fixture.frozen)
	if err != nil {
		t.Fatalf("initial runtime plan: %v", err)
	}
	parent := requireParallelCoordinatorJob(t, ctx, fixture.store, fixture.run.ID)
	left, found := plan.Workflow.Stage(parallelCoordinatorLeftStage)
	if !found {
		t.Fatal("parallel workflow omitted left stage")
	}
	leftTransition, found := plan.stageTransition(parallelCoordinatorLeftStage)
	if !found {
		t.Fatal("parallel runtime plan omitted left transition")
	}
	if err := runtime.enqueueStageAttempt(ctx, parent, fixture.run, fixture.frozen, plan, left, leftTransition); err != nil {
		t.Fatalf("seed queued left stage: %v", err)
	}
	queuedLeft, queuedLeftPayload := requireRuntimeStageJob(t, ctx, fixture.store, fixture.run.ID, parallelCoordinatorLeftStage)
	if queuedLeft.State != store.JobQueued {
		t.Fatalf("seed left job = %+v, want queued", queuedLeft)
	}

	// Drive the real leased workflow_run.execute path.  handleWorkflowRun
	// invokes executeWorkflowkitCoordinator, so this proves the public Engine
	// coordinator callback observes the same durable partial-batch facts.
	worker := newFrozenRuntimeWorker(t, fixture.store, runtime, "parallel-coordinator-worker")
	result, err := worker.RunOnce(ctx)
	if err != nil || result.FinalState != store.JobSucceeded || result.Job == nil || result.Job.ID != parent.ID {
		t.Fatalf("run public Engine coordinator for partial batch = %+v, %v", result, err)
	}

	stageJobs, err := runtime.stageJobsForPlan(ctx, fixture.run.ID, plan.ExecutionKey)
	if err != nil {
		t.Fatalf("read planned stage jobs: %v", err)
	}
	if len(stageJobs) != 2 {
		t.Fatalf("planned stage jobs = %#v, want exactly queued left and newly queued right", stageJobs)
	}
	leftAfter, found := stageJobs[parallelCoordinatorLeftStage]
	if !found || leftAfter.Job.ID != queuedLeft.ID || leftAfter.Payload.StageAttemptID != queuedLeftPayload.StageAttemptID {
		t.Fatalf("left peer was duplicated or replaced: before=%+v/%+v after=%+v", queuedLeft, queuedLeftPayload, leftAfter)
	}
	right, found := stageJobs[parallelCoordinatorRightStage]
	if !found || right.Job.State != store.JobQueued || right.Payload.StageAttemptID == "" {
		t.Fatalf("missing peer was not durably queued: %+v", right)
	}
	if _, premature := stageJobs[parallelCoordinatorJoinStage]; premature {
		t.Fatalf("dependent join stage was queued before both same-batch peers completed: %#v", stageJobs)
	}
	rightAttempt, err := fixture.store.GetStageAttempt(ctx, right.Payload.StageAttemptID)
	if err != nil || rightAttempt == nil || rightAttempt.ExecutionStatus != store.StageExecutionQueued {
		t.Fatalf("right stage attempt = %+v, %v; want queued", rightAttempt, err)
	}
}

// newParallelCoordinatorRuntimeFixture mirrors the existing frozen-runtime
// fixture's real managed-input/run-manifest admission path, but uses a small
// three-node DAG: left and right are an independent first batch and join
// depends on both.  It intentionally has no production catalog dependency.
func newParallelCoordinatorRuntimeFixture(t *testing.T) frozenRuntimeFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	dataStore, err := store.OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	services, err := newLifecycleServicesForTest(root, dataStore)
	if err != nil {
		_ = dataStore.Close()
		t.Fatal(err)
	}
	task, revision, err := services.Tasks.ImportTask(ctx, ImportTaskRequest{
		CreateDraftTaskRequest: CreateDraftTaskRequest{
			Slug: "parallel-runtime-coordinator", Title: "Parallel Runtime Coordinator", Actor: runtimeFixtureActor,
			Reason: "create parallel coordinator runtime fixture",
		},
		SourceDirectory: writeLifecycleSnapshot(t, "parallel coordinator fixture\n"),
		ChangeSummary:   "import immutable parallel coordinator fixture",
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
	profile := lifecycleCompleteProfile(t)
	profileCanonical, err := profile.CanonicalJSON()
	if err != nil {
		_ = dataStore.Close()
		t.Fatal(err)
	}
	profileFingerprint, err := profile.Fingerprint()
	if err != nil {
		_ = dataStore.Close()
		t.Fatal(err)
	}
	workflow := parallelCoordinatorWorkflow()
	if err := workflow.Validate(); err != nil {
		_ = dataStore.Close()
		t.Fatal(err)
	}
	policy := parallelCoordinatorQuotaPolicy(t)
	definition, err := workflow.Fingerprint()
	if err != nil {
		_ = dataStore.Close()
		t.Fatal(err)
	}
	initialPlan, err := workflowkit.CompileDependencyExecutionPlan(workflow)
	if err != nil {
		_ = dataStore.Close()
		t.Fatal(err)
	}
	runID, err := store.NewUUIDv7()
	if err != nil {
		_ = dataStore.Close()
		t.Fatal(err)
	}
	writeFrozenRuntimeFixtureManagedInputs(t, services, runID, profileCanonical, specificationCanonical)
	resolved := workflowadapter.ResolvedWorkflow{
		TemplateID: "parallel-runtime-coordinator", TemplateVersion: "1", ExecutionProfileID: "parallel-runtime-coordinator", ExecutionProfileVersion: "1",
		ContinuationPlanTTL: workflowadapter.RequiredContinuationPlanTTL, CandidateProviderBudget: profile.CandidateProviderBudget,
		ExecutionProfileFingerprint: profileFingerprint, DefinitionFingerprint: definition, Descriptor: workflow, QuotaPolicy: policy,
	}
	manifest, err := json.Marshal(runManifest{
		Format: "harbor.workflow-run-manifest.v2", RunID: runID, TaskID: task.ID, Revision: revision.ID, Resolved: resolved,
		InitialExecutionPlan: initialPlan,
		Inputs: &runManifestInputs{
			Format: runManifestInputsFormat, ProfileFingerprint: profileFingerprint, ExecutionSpecFingerprint: specificationFingerprint,
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
		Trigger: "parallel coordinator integration", Actor: runtimeFixtureActor, Reason: "create parallel coordinator frozen run",
		Dispatch: &store.WorkflowRunDispatchRequest{
			CommandType: "workflow_run.execute", PayloadJSON: string(payload), IdempotencyKey: "workflow-run-execution:" + runID,
		},
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

func parallelCoordinatorWorkflow() workflowkit.WorkflowDescriptor {
	claim := func() workflowkit.QuotaClaim {
		return workflowkit.QuotaClaim{Dimension: "stage_attempt", Units: 1, ReclaimPolicy: workflowkit.ReclaimUnused}
	}
	stage := func(key workflowkit.StageKey, dependencies []workflowkit.StageKey) workflowkit.StageDescriptor {
		return workflowkit.StageDescriptor{
			Key: key, Version: "1", Plugin: workflowkit.PluginBinding{ID: "parallel." + string(key), Version: "1"}, Group: "parallel",
			Dependencies: dependencies,
			Outputs:      []workflowkit.ArtifactSpec{{Name: string(key) + "_report", SchemaVersion: "parallel.v1", Required: true}},
			Effect:       workflowkit.EffectEvidenceOnly,
			Dispatch:     workflowkit.StageDispatchAutomatic,
			Budget: workflowkit.ExecutionBudget{
				TurnTimeout: runtimeFixtureStageTimeout, MaxTurns: 1, AttemptTimeout: runtimeFixtureStageTimeout, MaxAttempts: 1, MaxElapsed: runtimeFixtureStageTimeout,
			},
			QuotaClaims:  []workflowkit.QuotaClaim{claim()},
			Retry:        workflowkit.RetryPolicy{},
			Verdicts:     workflowkit.VerdictPolicy{Allowed: []workflowkit.Verdict{workflowkit.VerdictPass}},
			Reuse:        workflowkit.ReuseWhenInputsMatch,
			Capabilities: workflowkit.CapabilitySet{workflowkit.CapabilityCancel, workflowkit.CapabilityContinue},
		}
	}
	return workflowkit.WorkflowDescriptor{
		ID: "parallel-runtime-coordinator", Version: "1",
		Stages: []workflowkit.StageDescriptor{
			stage(parallelCoordinatorLeftStage, nil),
			stage(parallelCoordinatorRightStage, nil),
			stage(parallelCoordinatorJoinStage, []workflowkit.StageKey{parallelCoordinatorLeftStage, parallelCoordinatorRightStage}),
		},
	}
}

func parallelCoordinatorQuotaPolicy(t *testing.T) workflowadapter.ResolvedQuotaPolicy {
	t.Helper()
	claim := workflowkit.QuotaClaim{Dimension: "stage_attempt", Units: 1, ReclaimPolicy: workflowkit.ReclaimUnused}
	policy := workflowadapter.QuotaPolicy{
		ID: "parallel-runtime-coordinator-policy", Version: "1",
		AccountLimits: []workflowadapter.QuotaAccountLimit{{Dimension: "stage_attempt", TaskLimitUnits: 10, ActorLimitUnits: 10}},
		Stages: []workflowadapter.StageQuotaPolicy{
			{StageKey: parallelCoordinatorLeftStage, Claims: []workflowkit.QuotaClaim{claim}},
			{StageKey: parallelCoordinatorRightStage, Claims: []workflowkit.QuotaClaim{claim}},
			{StageKey: parallelCoordinatorJoinStage, Claims: []workflowkit.QuotaClaim{claim}},
		},
	}
	fingerprint, err := policy.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	return workflowadapter.ResolvedQuotaPolicy{
		ID: policy.ID, Version: policy.Version, Fingerprint: fingerprint,
		AccountLimits: append([]workflowadapter.QuotaAccountLimit(nil), policy.AccountLimits...),
		Stages:        append([]workflowadapter.StageQuotaPolicy(nil), policy.Stages...),
	}
}

func requireParallelCoordinatorJob(t *testing.T, ctx context.Context, dataStore *store.Store, runID string) store.DurableJob {
	t.Helper()
	jobs, err := dataStore.ListDurableJobsForRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range jobs {
		if job.CommandType == "workflow_run.execute" && job.RunID == runID {
			return job
		}
	}
	t.Fatalf("parallel coordinator Run %s has no initial coordinator job: %+v", runID, jobs)
	return store.DurableJob{}
}
