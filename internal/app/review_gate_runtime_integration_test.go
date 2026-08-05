package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/testsupport"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	runtimeReviewGateStage      = workflowkit.StageKey("runtime-review-gate")
	runtimeReviewSuccessorStage = workflowkit.StageKey("runtime-review-successor")
	runtimeReviewDecisionKey    = "runtime_review_decision"
)

type reviewGateRuntimeFixture struct {
	store       *store.Store
	services    *LifecycleServices
	task        store.TaskV2
	revision    store.TaskRevision
	run         store.WorkflowRun
	frozen      frozenRunDefinition
	runtime     *FrozenExecutionRuntime
	worker      *DurableWorker
	reviewStage workflowkit.StageKey
}

type reviewGateRuntimeFixtureOptions struct {
	newServices       func(string, *store.Store) (*LifecycleServices, error)
	freezeCatalogLock bool
	reviewStage       workflowkit.StageKey
}

func TestFrozenExecutionRuntimeReviewGateWaitsWithoutQuotaAdmission(t *testing.T) {
	ctx := context.Background()
	fixture := newReviewGateRuntimeFixture(t)
	defer fixture.store.Close()

	gateJob := queueRuntimeReviewGate(t, ctx, fixture)
	beforeTaskLeases := reviewGateQuotaLeases(t, ctx, fixture.store, store.QuotaScopeTask, fixture.task.ID)
	beforeActorLeases := reviewGateQuotaLeases(t, ctx, fixture.store, store.QuotaScopeActor, runtimeFixtureActor)
	if len(beforeTaskLeases) != 1 || len(beforeActorLeases) != 1 {
		t.Fatalf("source-stage quota baseline = task %+v actor %+v; want one settled source lease per scope", beforeTaskLeases, beforeActorLeases)
	}

	result, err := fixture.worker.RunOnce(ctx)
	if err != nil || result.FinalState != store.JobSucceeded || result.Job == nil || result.Job.ID != gateJob.ID {
		t.Fatalf("review gate worker result = %+v, %v", result, err)
	}

	gateAttempt, err := fixture.store.GetStageAttempt(ctx, gateJob.StageAttemptID)
	if err != nil || gateAttempt == nil || gateAttempt.ExecutionStatus != store.StageExecutionWaiting {
		t.Fatalf("review gate stage projection = %+v, %v", gateAttempt, err)
	}
	run, err := fixture.store.GetWorkflowRun(ctx, fixture.run.ID)
	if err != nil || run == nil || run.Status != store.WorkflowRunWaitingReview {
		t.Fatalf("review gate run projection = %+v, %v", run, err)
	}
	binding, err := fixture.store.GetReviewGateBindingByStageAttempt(ctx, gateAttempt.ID)
	if err != nil || binding == nil || binding.RunID != fixture.run.ID || binding.RevisionID != fixture.revision.ID || binding.StageKey != string(fixture.reviewStage) {
		t.Fatalf("review gate binding = %+v, %v", binding, err)
	}
	nodes, err := fixture.store.ListNodeAttempts(ctx, gateAttempt.ID)
	if err != nil || len(nodes) != 1 || nodes[0].ID != binding.NodeAttemptID || nodes[0].Status != store.NodeAttemptWaiting {
		t.Fatalf("review gate waiting node = %+v, %v", nodes, err)
	}

	afterTaskLeases := reviewGateQuotaLeases(t, ctx, fixture.store, store.QuotaScopeTask, fixture.task.ID)
	afterActorLeases := reviewGateQuotaLeases(t, ctx, fixture.store, store.QuotaScopeActor, runtimeFixtureActor)
	if len(afterTaskLeases) != len(beforeTaskLeases) || len(afterActorLeases) != len(beforeActorLeases) {
		t.Fatalf("review gate admitted quota: task %d -> %d, actor %d -> %d", len(beforeTaskLeases), len(afterTaskLeases), len(beforeActorLeases), len(afterActorLeases))
	}
}

func TestLifecycleMutationDecidesReviewGateAndQueuesResolution(t *testing.T) {
	ctx := context.Background()
	fixture := newReviewGateRuntimeFixture(t)
	defer fixture.store.Close()

	gateJob := queueRuntimeReviewGate(t, ctx, fixture)
	openRuntimeReviewGate(t, ctx, fixture, gateJob)
	binding := requireRuntimeReviewGateBinding(t, ctx, fixture.store, gateJob.StageAttemptID)

	receipt, command := decideRuntimeReviewGate(t, ctx, fixture, binding, store.ReviewDecisionApprove)
	if receipt.Action != LifecycleMutationReview || receipt.ReviewRequestID != binding.ReviewRequestID || receipt.ReviewDecisionID == "" || receipt.ReviewDecision != string(store.ReviewDecisionApprove) {
		t.Fatalf("gate lifecycle receipt = %+v", receipt)
	}
	replayed, err := fixture.services.Mutations.DecideReview(ctx, command)
	if err != nil || replayed != receipt {
		t.Fatalf("gate lifecycle decision replay = %+v, %v; want %+v", replayed, err, receipt)
	}

	decisions, err := fixture.store.ListReviewDecisionsForRequest(ctx, binding.ReviewRequestID)
	if err != nil || len(decisions) != 1 || decisions[0].ID != receipt.ReviewDecisionID || decisions[0].Action != store.ReviewDecisionApprove {
		t.Fatalf("gate decisions = %+v, %v", decisions, err)
	}
	jobs, err := fixture.store.ListDurableJobsForRun(ctx, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	resolutionCount := 0
	for _, job := range jobs {
		if job.CommandType != store.ReviewGateResolutionCommandType {
			continue
		}
		resolutionCount++
		if job.State != store.JobQueued || job.EntityID != binding.StageAttemptID || job.StageAttemptID != binding.StageAttemptID || job.IdempotencyKey != store.ReviewGateResolutionJobKey(binding.StageAttemptID, receipt.ReviewDecisionID) {
			t.Fatalf("gate resolution job = %+v", job)
		}
		payload, err := decodeReviewGateResolutionPayload(job.PayloadJSON)
		if err != nil || payload.ReviewRequestID != binding.ReviewRequestID || payload.ReviewDecisionID != receipt.ReviewDecisionID || payload.RunID != fixture.run.ID || payload.StageAttemptID != binding.StageAttemptID {
			t.Fatalf("gate resolution payload = %+v, %v", payload, err)
		}
	}
	if resolutionCount != 1 {
		t.Fatalf("gate resolution jobs = %d, want 1 in %+v", resolutionCount, jobs)
	}
}

func TestFrozenExecutionRuntimeApprovedReviewGateMaterializesArtifactAndSchedulesSuccessor(t *testing.T) {
	ctx := context.Background()
	fixture := newReviewGateRuntimeFixture(t)
	defer fixture.store.Close()

	gateJob := queueRuntimeReviewGate(t, ctx, fixture)
	openRuntimeReviewGate(t, ctx, fixture, gateJob)
	binding := requireRuntimeReviewGateBinding(t, ctx, fixture.store, gateJob.StageAttemptID)
	receipt, _ := decideRuntimeReviewGate(t, ctx, fixture, binding, store.ReviewDecisionApprove)

	resolution, err := fixture.worker.RunOnce(ctx)
	if err != nil || resolution.FinalState != store.JobSucceeded || resolution.Job == nil || resolution.Job.CommandType != store.ReviewGateResolutionCommandType {
		t.Fatalf("approved review gate resolution = %+v, %v", resolution, err)
	}
	gateAttempt, err := fixture.store.GetStageAttempt(ctx, binding.StageAttemptID)
	if err != nil || gateAttempt == nil || gateAttempt.ExecutionStatus != store.StageExecutionCompleted || gateAttempt.Verdict != store.VerdictPass || gateAttempt.ArtifactManifestID == "" {
		t.Fatalf("approved gate stage = %+v, %v", gateAttempt, err)
	}
	assertRuntimeReviewDecisionArtifact(t, ctx, fixture, *gateAttempt, binding, receipt.ReviewDecisionID, store.ReviewDecisionApprove)

	run, err := fixture.store.GetWorkflowRun(ctx, fixture.run.ID)
	if err != nil || run == nil || run.Status != store.WorkflowRunRunning {
		t.Fatalf("approved gate run = %+v, %v", run, err)
	}
	if result, err := fixture.worker.RunOnce(ctx); err != nil || result.FinalState != store.JobSucceeded || result.Job == nil || result.Job.CommandType != "workflow_run.execute" {
		t.Fatalf("approved gate successor coordinator = %+v, %v", result, err)
	}
	successorJob, _ := requireRuntimeStageJob(t, ctx, fixture.store, fixture.run.ID, runtimeReviewSuccessorStage)
	if successorJob.State != store.JobQueued {
		t.Fatalf("approved gate successor stage job = %+v", successorJob)
	}
}

// This follows the same durable path as a CodeEdge final_review approval:
// ResolveReview -> projectResolvedReviewGate -> enqueueNextCoordinator ->
// workflow_run.execute. The predecessor Run is frozen under an older catalog
// lock version, and the resolution and successor coordinator continue under a
// new lock version bound to the same catalog receipt.
func TestFrozenExecutionRuntimeApprovedFinalReviewContinuesAfterCatalogLockVersionChange(t *testing.T) {
	ctx := context.Background()
	selection := testsupport.CompleteRunExecutionSpec(
		"018f0a73-3b49-7000-8000-000000000071",
		"018f0a73-3b49-7000-8000-000000000072",
		"harbor.task.v2:sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
	)
	selection.Template = workflowadapter.StandardTemplateReference()
	oldResolver := catalogLockAttestedResolverForSpecWithBuild(t, selection, "review-gate-build-compat", "v1", "lock-v1", stageprovider.HarborFlowBuildIdentity{
		Module: "github.com/purplevoid/harbor-factory", Version: "v2.0.0", Commit: strings.Repeat("a", 40),
		ContentSHA256: workflowkit.SHA256Fingerprint([]byte("review-gate-old-build")),
	})
	currentResolver := catalogLockAttestedResolverForSpecWithBuild(t, selection, "review-gate-build-compat", "v1", "lock-v2", stageprovider.HarborFlowBuildIdentity{
		Module: "github.com/purplevoid/harbor-factory", Version: "v2.1.0", Commit: strings.Repeat("b", 40),
		ContentSHA256: workflowkit.SHA256Fingerprint([]byte("review-gate-current-build")),
	})
	fixture := newReviewGateRuntimeFixtureWithOptions(t, reviewGateRuntimeFixtureOptions{
		freezeCatalogLock: true,
		reviewStage:       workflowkit.StageKey(workflowadapter.FinalReview),
		newServices: func(root string, database *store.Store) (*LifecycleServices, error) {
			return NewLifecycleServicesWithOptions(root, database, LifecycleServicesOptions{
				OperationResolver: oldResolver,
				DeploymentCatalogResolvers: []TemplateDeploymentCatalogResolver{{
					Template: oldResolver.Receipt().Template, Resolver: oldResolver,
				}},
				RequireDeploymentCatalog: true,
			})
		},
	})
	defer fixture.store.Close()

	gateJob := queueRuntimeReviewGate(t, ctx, fixture)
	openRuntimeReviewGate(t, ctx, fixture, gateJob)
	binding := requireRuntimeReviewGateBinding(t, ctx, fixture.store, gateJob.StageAttemptID)
	decideRuntimeReviewGate(t, ctx, fixture, binding, store.ReviewDecisionApprove)

	currentServices := catalogLockLifecycleServices(t, fixture.services.core.layout.root, fixture.store, currentResolver)
	currentRuntime := newFrozenRuntime(t, currentServices, fixture.runtime.workflowkitRegistry)
	if _, _, err := currentRuntime.loadFrozenRun(ctx, fixture.run.ID, fixture.run.DefinitionHash, fixture.frozen.ExecutionSpecFingerprint, fixture.frozen.QuotaPolicy); err != nil {
		t.Fatalf("frozen final_review predecessor load after lock version change = %v", err)
	}
	currentWorker := newFrozenRuntimeWorker(t, fixture.store, currentRuntime, "review-gate-build-compatible-worker")
	resolution, err := currentWorker.RunOnce(ctx)
	if err != nil || resolution.FinalState != store.JobSucceeded || resolution.Job == nil || resolution.Job.CommandType != store.ReviewGateResolutionCommandType {
		t.Fatalf("approved final_review resolution under changed lock version = %+v, %v", resolution, err)
	}
	successor, err := currentWorker.RunOnce(ctx)
	if err != nil || successor.FinalState != store.JobSucceeded || successor.Job == nil || successor.Job.CommandType != "workflow_run.execute" {
		t.Fatalf("approved final_review successor coordinator under changed lock version = %+v, %v", successor, err)
	}
	stageJob, _ := requireRuntimeStageJob(t, ctx, fixture.store, fixture.run.ID, runtimeReviewSuccessorStage)
	if stageJob.State != store.JobQueued {
		t.Fatalf("final_review successor stage after lock version change = %+v, want queued", stageJob)
	}
}

func TestFrozenExecutionRuntimeReviewGateNonApprovalDecisionsProjectRunOutcome(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		action  store.ReviewDecisionAction
		verdict store.Verdict
		status  store.WorkflowRunStatus
	}{
		{name: "request changes", action: store.ReviewDecisionRequestChanges, verdict: store.VerdictNeedsRepair, status: store.WorkflowRunWaitingContinuation},
		{name: "reject", action: store.ReviewDecisionRejectTerminal, verdict: store.VerdictReject, status: store.WorkflowRunFailedTerminal},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx := context.Background()
			fixture := newReviewGateRuntimeFixture(t)
			defer fixture.store.Close()

			gateJob := queueRuntimeReviewGate(t, ctx, fixture)
			openRuntimeReviewGate(t, ctx, fixture, gateJob)
			binding := requireRuntimeReviewGateBinding(t, ctx, fixture.store, gateJob.StageAttemptID)
			receipt, _ := decideRuntimeReviewGate(t, ctx, fixture, binding, scenario.action)
			result, err := fixture.worker.RunOnce(ctx)
			if err != nil || result.FinalState != store.JobSucceeded || result.Job == nil || result.Job.CommandType != store.ReviewGateResolutionCommandType {
				t.Fatalf("%s resolution = %+v, %v", scenario.name, result, err)
			}

			gateAttempt, err := fixture.store.GetStageAttempt(ctx, binding.StageAttemptID)
			if err != nil || gateAttempt == nil || gateAttempt.ExecutionStatus != store.StageExecutionCompleted || gateAttempt.Verdict != scenario.verdict || gateAttempt.ArtifactManifestID == "" {
				t.Fatalf("%s gate stage = %+v, %v", scenario.name, gateAttempt, err)
			}
			assertRuntimeReviewDecisionArtifact(t, ctx, fixture, *gateAttempt, binding, receipt.ReviewDecisionID, scenario.action)
			run, err := fixture.store.GetWorkflowRun(ctx, fixture.run.ID)
			if err != nil || run == nil || run.Status != scenario.status {
				t.Fatalf("%s run = %+v, %v; want %s", scenario.name, run, err, scenario.status)
			}
			assertNoQueuedRuntimeReviewSuccessor(t, ctx, fixture.store, fixture.run.ID)
		})
	}
}

func TestFrozenExecutionRuntimeRecoversExpiredReviewGateResolution(t *testing.T) {
	for _, scenario := range []struct {
		name                   string
		materializeBeforeCrash bool
	}{
		{name: "before decision artifact materialization"},
		{name: "after decision artifact materialization", materializeBeforeCrash: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx := context.Background()
			fixture := newReviewGateRuntimeFixture(t)
			defer fixture.store.Close()

			gateJob := queueRuntimeReviewGate(t, ctx, fixture)
			openRuntimeReviewGate(t, ctx, fixture, gateJob)
			binding := requireRuntimeReviewGateBinding(t, ctx, fixture.store, gateJob.StageAttemptID)
			receipt, _ := decideRuntimeReviewGate(t, ctx, fixture, binding, store.ReviewDecisionApprove)
			if scenario.materializeBeforeCrash {
				materializeRuntimeReviewGateForRecovery(t, ctx, fixture, binding, receipt.ReviewDecisionID)
			}
			resolutionJob := requireRuntimeReviewGateResolutionJob(t, ctx, fixture.store, fixture.run.ID, receipt.ReviewDecisionID)
			claim, err := fixture.store.ClaimNextDurableJob(ctx, store.ClaimNextDurableJobRequest{
				IdempotencyKey: "review-gate-recovery-" + receipt.ReviewDecisionID, Owner: "review-gate-crashed-worker", RunID: fixture.run.ID,
				LeaseTTL: 10 * time.Millisecond, Actor: runtimeFixtureActor, Reason: "simulate review gate resolver crash",
			})
			if err != nil || claim.Job == nil || claim.Job.ID != resolutionJob.ID || claim.DispatchLease == nil {
				t.Fatalf("claim review gate resolution for crash = %+v, %v", claim, err)
			}
			time.Sleep(40 * time.Millisecond)

			recovered, err := fixture.worker.RunOnce(ctx)
			if err != nil {
				t.Fatalf("recover expired review gate resolution = %+v, %v", recovered, err)
			}
			if len(recovered.Recoveries) != 1 || recovered.Recoveries[0].Job.ID != resolutionJob.ID || recovered.Recoveries[0].Job.State != store.JobInDoubt || recovered.Recoveries[0].Job.Failure == nil || recovered.Recoveries[0].Job.Failure.Code != "job.lease_lost" {
				t.Fatalf("review gate recovery facts = %+v", recovered.Recoveries)
			}
			gateAttempt, err := fixture.store.GetStageAttempt(ctx, binding.StageAttemptID)
			if err != nil || gateAttempt == nil || gateAttempt.ExecutionStatus != store.StageExecutionCompleted || gateAttempt.Verdict != store.VerdictPass || gateAttempt.ArtifactManifestID == "" {
				t.Fatalf("recovered review gate stage = %+v, %v", gateAttempt, err)
			}
			assertRuntimeReviewDecisionArtifact(t, ctx, fixture, *gateAttempt, binding, receipt.ReviewDecisionID, store.ReviewDecisionApprove)
			references, err := fixture.store.ListArtifactRefs(ctx, gateAttempt.ArtifactManifestID)
			if err != nil || len(references) != 1 {
				t.Fatalf("recovered review gate artifacts = %+v, %v", references, err)
			}
			run, err := fixture.store.GetWorkflowRun(ctx, fixture.run.ID)
			if err != nil || run == nil || run.Status != store.WorkflowRunRunning {
				t.Fatalf("recovered review gate run = %+v, %v", run, err)
			}
			successor, _ := requireRuntimeStageJob(t, ctx, fixture.store, fixture.run.ID, runtimeReviewSuccessorStage)
			if successor.State != store.JobQueued {
				t.Fatalf("recovery did not enqueue successor stage: %+v", successor)
			}
			persistedResolution, err := fixture.store.GetDurableJob(ctx, resolutionJob.ID)
			if err != nil || persistedResolution == nil || persistedResolution.State != store.JobInDoubt || persistedResolution.Failure == nil || persistedResolution.Failure.Code != "job.lease_lost" {
				t.Fatalf("expired resolution job projection = %+v, %v", persistedResolution, err)
			}
		})
	}
}

func TestLifecycleMutationGateApprovalCannotPromoteCurrentRevision(t *testing.T) {
	ctx := context.Background()
	fixture := newReviewGateRuntimeFixture(t)
	defer fixture.store.Close()

	gateJob := queueRuntimeReviewGate(t, ctx, fixture)
	openRuntimeReviewGate(t, ctx, fixture, gateJob)
	binding := requireRuntimeReviewGateBinding(t, ctx, fixture.store, gateJob.StageAttemptID)
	if receipt, _ := decideRuntimeReviewGate(t, ctx, fixture, binding, store.ReviewDecisionApprove); receipt.ReviewDecisionID == "" {
		t.Fatalf("gate approval receipt lacks review decision ID: %+v", receipt)
	}
	validated, err := fixture.services.Revisions.MarkValidated(ctx, fixture.revision.ID, fixture.revision.StateVersion, "sha256:runtime-gate-validation", runtimeFixtureActor, "validate review gate fixture")
	if err != nil {
		t.Fatal(err)
	}
	if validated.State != store.RevisionStateValidated {
		t.Fatalf("validated revision = %+v", validated)
	}
	if _, err := fixture.services.Reviews.PromoteCurrent(ctx, fixture.task.ID, fixture.revision.ID, fixture.task.Version, runtimeFixtureActor, "gate approval cannot promote current revision"); !errors.Is(err, store.ErrReviewApprovalNeeded) {
		t.Fatalf("gate approval promoted current revision: %v", err)
	}
	task, err := fixture.store.GetTaskV2(ctx, fixture.task.ID)
	if err != nil || task == nil || task.CurrentRevisionID != "" {
		t.Fatalf("task after rejected promotion = %+v, %v", task, err)
	}
}

func newReviewGateRuntimeFixture(t *testing.T) reviewGateRuntimeFixture {
	return newReviewGateRuntimeFixtureWithOptions(t, reviewGateRuntimeFixtureOptions{})
}

func newReviewGateRuntimeFixtureWithOptions(t *testing.T, options reviewGateRuntimeFixtureOptions) reviewGateRuntimeFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	dataStore, err := store.OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	newServices := options.newServices
	if newServices == nil {
		newServices = newLifecycleServicesForTest
	}
	services, err := newServices(root, dataStore)
	if err != nil {
		_ = dataStore.Close()
		t.Fatal(err)
	}
	task, revision, err := services.Tasks.ImportTask(ctx, ImportTaskRequest{
		CreateDraftTaskRequest: CreateDraftTaskRequest{Slug: "runtime-review-gate", Title: "Runtime Review Gate", Actor: runtimeFixtureActor, Reason: "review gate runtime integration fixture"},
		SourceDirectory:        writeLifecycleSnapshot(t, "review gate runtime fixture\n"),
		ChangeSummary:          "import immutable review gate runtime fixture",
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
	reviewStage := options.reviewStage
	if reviewStage == "" {
		reviewStage = runtimeReviewGateStage
	}
	workflow := runtimeReviewGateWorkflow(reviewStage)
	policy := runtimeReviewGateQuotaPolicy(t, reviewStage)
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
	writeFrozenRuntimeFixtureManagedInputs(t, services, runID, profileCanonical, specificationCanonical)
	var catalogReceipt []byte
	if options.freezeCatalogLock {
		catalogReceipt, err = services.core.frozenDeploymentCatalogReceipt(specification.Template)
		if err != nil {
			_ = dataStore.Close()
			t.Fatal(err)
		}
		if err := writeNewBytes(filepath.Join(services.core.layout.runDirectory(runID), deploymentCatalogReceiptFileName), catalogReceipt); err != nil {
			_ = dataStore.Close()
			t.Fatal(err)
		}
	}
	decisionArtifact := workflowkit.ArtifactSpec{Name: runtimeReviewDecisionKey, SchemaVersion: "harbor.review-decision.v1", Required: true}
	resolved := workflowadapter.ResolvedWorkflow{
		TemplateID: "runtime-review-gate", TemplateVersion: "1", ExecutionProfileID: "runtime-review-gate", ExecutionProfileVersion: "1",
		ContinuationPlanTTL: workflowadapter.RequiredContinuationPlanTTL, CandidateProviderBudget: profile.CandidateProviderBudget, ExecutionProfileFingerprint: profileFingerprint, DefinitionFingerprint: definition,
		Descriptor: workflow, QuotaPolicy: policy,
		ReviewStages: []workflowadapter.ReviewStage{{StageKey: reviewStage, ReviewKind: workflowadapter.ReviewTaskDirection, DecisionArtifact: decisionArtifact}},
	}
	if options.freezeCatalogLock {
		resolved.Template = specification.Template
		resolved.TemplateID = specification.Template.ID
		resolved.TemplateVersion = specification.Template.Version
	}
	manifest, err := json.Marshal(runManifest{
		Format: "harbor.workflow-run-manifest.v2", RunID: runID, TaskID: task.ID, Revision: revision.ID, Resolved: resolved, InitialExecutionPlan: initialExecutionPlan,
		Inputs: &runManifestInputs{
			Format:                   runManifestInputsFormat,
			ProfileFingerprint:       profileFingerprint,
			ExecutionSpecFingerprint: specificationFingerprint,
		},
		ExecutionSpec:            append(json.RawMessage(nil), specificationCanonical...),
		DeploymentCatalogReceipt: append(json.RawMessage(nil), catalogReceipt...),
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
		Trigger: "runtime review gate integration", Actor: runtimeFixtureActor, Reason: "create frozen review gate runtime fixture",
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
	registry, err := workflowkit.NewControlledPluginRegistry([]workflowkit.PluginRegistration[workflowkit.StageExecutor]{
		{Binding: workflowkit.PluginBinding{ID: "runtime.source", Version: "1"}, Implementation: workflowkit.StageExecutorFunc(completedFixtureStage)},
		{Binding: workflowkit.PluginBinding{ID: "runtime.review-gate", Version: "1"}, Implementation: workflowkit.StageExecutorFunc(func(_ context.Context, request workflowkit.StageExecutionRequest) (workflowkit.StageExecutionResult, error) {
			if request.Claim.Stage == nil || request.Claim.Stage.StageAttempt.ID == "" || request.Execution.ID == "" {
				return workflowkit.StageExecutionResult{}, errors.New("review gate did not receive a public Engine claim")
			}
			binding, err := workflowkit.NewOpaqueExecutionBinding("runtime.review-gate.wait", "1", []byte(`{"fixture":"review-gate"}`))
			if err != nil {
				return workflowkit.StageExecutionResult{}, err
			}
			return workflowkit.StageExecutionResult{Wait: &workflowkit.StageWait{
				Kind: workflowkit.StageWaitExternalDecision, OperationKey: "review-gate:" + request.Execution.ID + ":" + string(request.Stage.Key), DecisionBinding: binding,
			}}, nil
		})},
		{Binding: workflowkit.PluginBinding{ID: "runtime.review-successor", Version: "1"}, Implementation: workflowkit.StageExecutorFunc(completedFixtureStage)},
	})
	if err != nil {
		_ = dataStore.Close()
		t.Fatal(err)
	}
	runtime := newFrozenRuntime(t, services, registry)
	worker := newFrozenRuntimeWorker(t, dataStore, runtime, "runtime-review-gate-worker")
	return reviewGateRuntimeFixture{store: dataStore, services: services, task: task, revision: revision, run: run, frozen: frozen, runtime: runtime, worker: worker, reviewStage: reviewStage}
}

func runtimeReviewGateWorkflow(reviewStage workflowkit.StageKey) workflowkit.WorkflowDescriptor {
	budget := workflowkit.ExecutionBudget{
		TurnTimeout: time.Second, MaxTurns: 1, AttemptTimeout: time.Second, MaxAttempts: 1, MaxElapsed: time.Second,
	}
	claim := workflowkit.QuotaClaim{Dimension: "stage_attempt", Units: 1, ReclaimPolicy: workflowkit.ReclaimUnused}
	decisionArtifact := workflowkit.ArtifactSpec{Name: runtimeReviewDecisionKey, SchemaVersion: "harbor.review-decision.v1", Required: true}
	return workflowkit.WorkflowDescriptor{
		ID: "runtime-review-gate", Version: "1",
		Stages: []workflowkit.StageDescriptor{
			{
				Key: runtimeFixtureSourceStage, Version: "1", Plugin: workflowkit.PluginBinding{ID: "runtime.source", Version: "1"}, Group: "runtime",
				Outputs: []workflowkit.ArtifactSpec{{Name: "source_report", SchemaVersion: "runtime.v1", Required: true}}, Effect: workflowkit.EffectEvidenceOnly,
				Dispatch: workflowkit.StageDispatchAutomatic,
				Budget:   budget, QuotaClaims: []workflowkit.QuotaClaim{claim}, Retry: workflowkit.RetryPolicy{},
				Verdicts: workflowkit.VerdictPolicy{Allowed: []workflowkit.Verdict{workflowkit.VerdictPass}}, Reuse: workflowkit.ReuseWhenInputsMatch,
				Capabilities: workflowkit.CapabilitySet{workflowkit.CapabilityCancel, workflowkit.CapabilityContinue},
			},
			{
				Key: reviewStage, Version: "1", Plugin: workflowkit.PluginBinding{ID: "runtime.review-gate", Version: "1"}, Group: "runtime",
				Dependencies: []workflowkit.StageKey{runtimeFixtureSourceStage}, Inputs: []workflowkit.ArtifactSpec{{Name: "source_report", SchemaVersion: "runtime.v1", Required: true}}, Outputs: []workflowkit.ArtifactSpec{decisionArtifact}, Effect: workflowkit.EffectEvidenceOnly,
				Dispatch: workflowkit.StageDispatchAutomatic,
				Budget:   budget, QuotaClaims: []workflowkit.QuotaClaim{}, Retry: workflowkit.RetryPolicy{},
				Verdicts: workflowkit.VerdictPolicy{Allowed: []workflowkit.Verdict{workflowkit.VerdictPass, workflowkit.VerdictNeedsRepair, workflowkit.VerdictReject}}, Reuse: workflowkit.ReuseWhenInputsMatch,
				Capabilities: workflowkit.CapabilitySet{workflowkit.CapabilityApprove},
			},
			{
				Key: runtimeReviewSuccessorStage, Version: "1", Plugin: workflowkit.PluginBinding{ID: "runtime.review-successor", Version: "1"}, Group: "runtime",
				Dependencies: []workflowkit.StageKey{reviewStage}, Inputs: []workflowkit.ArtifactSpec{decisionArtifact}, Outputs: []workflowkit.ArtifactSpec{{Name: "successor_report", SchemaVersion: "runtime.v1", Required: true}}, Effect: workflowkit.EffectEvidenceOnly,
				Dispatch: workflowkit.StageDispatchAutomatic,
				Budget:   budget, QuotaClaims: []workflowkit.QuotaClaim{claim}, Retry: workflowkit.RetryPolicy{},
				Verdicts: workflowkit.VerdictPolicy{Allowed: []workflowkit.Verdict{workflowkit.VerdictPass}}, Reuse: workflowkit.ReuseWhenInputsMatch,
				Capabilities: workflowkit.CapabilitySet{workflowkit.CapabilityCancel, workflowkit.CapabilityContinue},
			},
		},
	}
}

func runtimeReviewGateQuotaPolicy(t *testing.T, reviewStage workflowkit.StageKey) workflowadapter.ResolvedQuotaPolicy {
	t.Helper()
	policy := workflowadapter.QuotaPolicy{
		ID: "runtime-review-gate-policy", Version: "1",
		AccountLimits: []workflowadapter.QuotaAccountLimit{{Dimension: "stage_attempt", TaskLimitUnits: 10, ActorLimitUnits: 10}},
		Stages: []workflowadapter.StageQuotaPolicy{
			{StageKey: runtimeFixtureSourceStage, Claims: []workflowkit.QuotaClaim{{Dimension: "stage_attempt", Units: 1, ReclaimPolicy: workflowkit.ReclaimUnused}}},
			{StageKey: reviewStage, Claims: []workflowkit.QuotaClaim{}},
			{StageKey: runtimeReviewSuccessorStage, Claims: []workflowkit.QuotaClaim{{Dimension: "stage_attempt", Units: 1, ReclaimPolicy: workflowkit.ReclaimUnused}}},
		},
	}
	fingerprint, err := policy.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	return workflowadapter.ResolvedQuotaPolicy{ID: policy.ID, Version: policy.Version, Fingerprint: fingerprint, AccountLimits: policy.AccountLimits, Stages: policy.Stages}
}

func queueRuntimeReviewGate(t *testing.T, ctx context.Context, fixture reviewGateRuntimeFixture) store.DurableJob {
	t.Helper()
	for cycle := 0; cycle < 3; cycle++ {
		result, err := fixture.worker.RunOnce(ctx)
		if err != nil || result.FinalState != store.JobSucceeded || result.Job == nil {
			t.Fatalf("queue review gate cycle %d = %+v, %v", cycle, result, err)
		}
	}
	gateJob, _ := requireRuntimeStageJob(t, ctx, fixture.store, fixture.run.ID, fixture.reviewStage)
	if gateJob.State != store.JobQueued {
		t.Fatalf("queued review gate job = %+v", gateJob)
	}
	return gateJob
}

func openRuntimeReviewGate(t *testing.T, ctx context.Context, fixture reviewGateRuntimeFixture, gateJob store.DurableJob) {
	t.Helper()
	result, err := fixture.worker.RunOnce(ctx)
	if err != nil || result.FinalState != store.JobSucceeded || result.Job == nil || result.Job.ID != gateJob.ID {
		t.Fatalf("open review gate result = %+v, %v", result, err)
	}
}

func requireRuntimeReviewGateBinding(t *testing.T, ctx context.Context, dataStore *store.Store, stageAttemptID string) *store.ReviewGateBinding {
	t.Helper()
	binding, err := dataStore.GetReviewGateBindingByStageAttempt(ctx, stageAttemptID)
	if err != nil || binding == nil {
		t.Fatalf("read review gate binding for %s: %+v, %v", stageAttemptID, binding, err)
	}
	return binding
}

func decideRuntimeReviewGate(t *testing.T, ctx context.Context, fixture reviewGateRuntimeFixture, binding *store.ReviewGateBinding, action store.ReviewDecisionAction) (LifecycleMutationReceipt, DecideReviewLifecycleCommand) {
	t.Helper()
	checkpoint, err := fixture.services.Mutations.CaptureReviewCheckpoint(ctx, fixture.task.ID, fixture.revision.ID, binding.ReviewRequestID)
	if err != nil {
		t.Fatal(err)
	}
	idempotencyKey, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	command := DecideReviewLifecycleCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{IdempotencyKey: idempotencyKey, Actor: runtimeFixtureActor, Reason: "decide durable runtime review gate", Expected: checkpoint},
		Decision:                     action,
	}
	receipt, err := fixture.services.Mutations.DecideReview(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	return receipt, command
}

func reviewGateQuotaLeases(t *testing.T, ctx context.Context, dataStore *store.Store, scope store.QuotaScopeKind, scopeID string) []store.DurableQuotaLease {
	t.Helper()
	leases, err := dataStore.ListDurableQuotaLeasesForScope(ctx, scope, scopeID)
	if err != nil {
		t.Fatal(err)
	}
	return leases
}

func assertRuntimeReviewDecisionArtifact(t *testing.T, ctx context.Context, fixture reviewGateRuntimeFixture, gateAttempt store.StageAttempt, binding *store.ReviewGateBinding, decisionID string, action store.ReviewDecisionAction) {
	t.Helper()
	references, err := fixture.store.ListArtifactRefs(ctx, gateAttempt.ArtifactManifestID)
	if err != nil || len(references) != 1 || references[0].ArtifactKey != runtimeReviewDecisionKey || references[0].SchemaVersion != "harbor.review-decision.v1" || references[0].RunID != fixture.run.ID || references[0].AttemptID != gateAttempt.ID {
		t.Fatalf("review decision artifact refs = %+v, %v", references, err)
	}
	index, err := loadStageArtifactManifestIndex(ctx, fixture.store, gateAttempt.ArtifactManifestID)
	if err != nil {
		t.Fatal(err)
	}
	object, err := index.objectFor(references[0])
	if err != nil {
		t.Fatal(err)
	}
	content, err := fixture.services.core.objects.ReadAll(ctx, object)
	if err != nil {
		t.Fatal(err)
	}
	var artifact reviewGateDecisionArtifact
	if err := json.Unmarshal(content, &artifact); err != nil {
		t.Fatal(err)
	}
	if artifact.Format != reviewGateDecisionArtifactFormat || artifact.ReviewRequestID != binding.ReviewRequestID || artifact.ReviewDecisionID != decisionID || artifact.Action != action || artifact.RevisionID != fixture.revision.ID || artifact.RevisionDigest != fixture.revision.TaskDigest || artifact.ReviewKind != string(workflowadapter.ReviewTaskDirection) || artifact.EvidenceManifestDigest != binding.EvidenceManifestDigest || artifact.InputFingerprint != binding.InputFingerprint || artifact.DecisionActor != runtimeFixtureActor {
		t.Fatalf("review decision artifact = %+v", artifact)
	}
}

func assertNoQueuedRuntimeReviewSuccessor(t *testing.T, ctx context.Context, dataStore *store.Store, runID string) {
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
		if payload.StageKey == runtimeReviewSuccessorStage && job.State == store.JobQueued {
			t.Fatalf("non-approved review gate queued successor %+v", job)
		}
	}
}

func requireRuntimeReviewGateResolutionJob(t *testing.T, ctx context.Context, dataStore *store.Store, runID, decisionID string) store.DurableJob {
	t.Helper()
	jobs, err := dataStore.ListDurableJobsForRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range jobs {
		if job.CommandType != store.ReviewGateResolutionCommandType {
			continue
		}
		payload, err := decodeReviewGateResolutionPayload(job.PayloadJSON)
		if err != nil {
			t.Fatal(err)
		}
		if payload.ReviewDecisionID == decisionID {
			return job
		}
	}
	t.Fatalf("no review gate resolution job for decision %s in %+v", decisionID, jobs)
	return store.DurableJob{}
}

func materializeRuntimeReviewGateForRecovery(t *testing.T, ctx context.Context, fixture reviewGateRuntimeFixture, binding *store.ReviewGateBinding, decisionID string) {
	t.Helper()
	gateAttempt, err := fixture.store.GetStageAttempt(ctx, binding.StageAttemptID)
	if err != nil || gateAttempt == nil || gateAttempt.ExecutionStatus != store.StageExecutionWaiting {
		t.Fatalf("read waiting review gate attempt = %+v, %v", gateAttempt, err)
	}
	nodes, err := fixture.store.ListNodeAttempts(ctx, binding.StageAttemptID)
	if err != nil || len(nodes) != 1 || nodes[0].ID != binding.NodeAttemptID || nodes[0].Status != store.NodeAttemptWaiting {
		t.Fatalf("read waiting review gate node = %+v, %v", nodes, err)
	}
	decisions, err := fixture.store.ListReviewDecisionsForRequest(ctx, binding.ReviewRequestID)
	if err != nil || len(decisions) != 1 || decisions[0].ID != decisionID {
		t.Fatalf("read review decision to materialize = %+v, %v", decisions, err)
	}
	stage, found := fixture.frozen.Workflow.Stage(fixture.reviewStage)
	if !found {
		t.Fatalf("frozen review stage %q is missing", fixture.reviewStage)
	}
	inputs, err := decodeReviewGateInputs(binding)
	if err != nil {
		t.Fatal(err)
	}
	content, err := json.Marshal(reviewGateDecisionArtifact{
		Format: reviewGateDecisionArtifactFormat, ReviewRequestID: binding.ReviewRequestID, ReviewDecisionID: decisions[0].ID,
		Action: decisions[0].Action, RevisionID: binding.RevisionID, RevisionDigest: binding.RevisionDigest,
		ReviewKind: binding.ReviewKind, EvidenceManifestDigest: binding.EvidenceManifestDigest, InputFingerprint: binding.InputFingerprint,
		DecisionActor: decisions[0].Actor, DecisionReason: decisions[0].Reason,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, _, err := persistStageArtifacts(ctx, fixture.services.core, fixture.run, fixture.revision, *gateAttempt, nodes[0], stage, inputs, []StageArtifact{{
		Key: runtimeReviewDecisionKey, SchemaVersion: "harbor.review-decision.v1", Content: content,
	}}, runtimeFixtureActor, "simulate persisted review gate artifact before resolver crash")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := fixture.store.CompleteReviewGateResolution(ctx, store.CompleteReviewGateResolutionRequest{
		ReviewRequestID: binding.ReviewRequestID, ReviewDecisionID: decisionID, RunID: fixture.run.ID, StageAttemptID: gateAttempt.ID,
		ExpectedRunVersion: fixtureRunVersion(t, ctx, fixture.store, fixture.run.ID), ExpectedStageAttemptVersion: gateAttempt.Version, ExpectedNodeAttemptVersion: nodes[0].Version,
		ArtifactManifestID: manifest.ID, Actor: runtimeFixtureActor, Reason: "simulate review gate resolver crash after artifact materialization",
	})
	if err != nil || resolved.Run.Status != store.WorkflowRunWaitingReview || resolved.StageAttempt.ExecutionStatus != store.StageExecutionCompleted {
		t.Fatalf("materialize review gate before recovery = %+v, %v", resolved, err)
	}
}

func fixtureRunVersion(t *testing.T, ctx context.Context, dataStore *store.Store, runID string) int64 {
	t.Helper()
	run, err := dataStore.GetWorkflowRun(ctx, runID)
	if err != nil || run == nil {
		t.Fatalf("read workflow run %s: %+v, %v", runID, run, err)
	}
	return run.Version
}
