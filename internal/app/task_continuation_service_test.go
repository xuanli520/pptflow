package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/workflowruntime"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

type continuationFixture struct {
	services  *LifecycleServices
	dataStore *store.Store
	task      store.TaskV2
	revision  store.TaskRevision
	run       store.WorkflowRun
}

func TestSameArtifactBindingsTreatsNilAndEmptyAsEqual(t *testing.T) {
	if !sameArtifactBindings(nil, []workflowkit.ArtifactBinding{}) {
		t.Fatal("nil and empty artifact bindings must represent the same empty input set")
	}
}

func TestTaskContinuationPlanIsFrozenIdempotentAndCoversEveryStage(t *testing.T) {
	ctx := context.Background()
	fixture := newContinuationFixture(t, store.WorkflowRunFailedRecoverable)
	// The app clock is injected below while the Store retains its real clock.
	// Anchor the fixture at the current instant so the frozen 24-hour TTL is
	// valid for both clocks regardless of the calendar date on which tests run.
	plannedAt := time.Now().UTC()
	fixture.services.core.now = func() time.Time { return plannedAt }
	command := continuationCommand(t, ctx, fixture, "continue-idempotent", []workflowkit.NodeID{workflowadapter.QualityCheck}, false)

	plan, err := fixture.services.Continuations.PlanTaskContinuation(ctx, command)
	if err != nil {
		t.Fatalf("plan continuation: %v", err)
	}
	snapshot := plan.Snapshot()
	if snapshot.Strategy != workflowkit.StrategyRetryAttempt || snapshot.SourceRunID != fixture.run.ID || snapshot.NextExecutionEpoch != fixture.run.ExecutionEpoch+1 {
		t.Fatalf("unexpected frozen continuation plan: %+v", snapshot)
	}
	if snapshot.PlanID == snapshot.CommandID {
		t.Fatalf("frozen plan reused continuation command UUID %q", snapshot.PlanID)
	}
	if want := plannedAt.Add(workflowadapter.RequiredContinuationPlanTTL); !snapshot.ExpiresAt.Equal(want) {
		t.Fatalf("plan expiration = %s, want frozen profile TTL expiry %s", snapshot.ExpiresAt, want)
	}
	commandRecord, err := fixture.dataStore.GetContinuationCommand(ctx, snapshot.CommandID)
	if err != nil {
		t.Fatal(err)
	}
	if commandRecord == nil || commandRecord.ID == plan.ID() {
		t.Fatalf("command and plan identities must be distinct: command=%+v plan=%q", commandRecord, plan.ID())
	}
	workflow, err := decodeFrozenWorkflow(fixture.run)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Nodes) != len(workflow.Stages) || len(snapshot.Schedule) == 0 {
		t.Fatalf("plan did not expand a complete actionable no-change closure: nodes=%d schedule=%d stages=%d", len(snapshot.Nodes), len(snapshot.Schedule), len(workflow.Stages))
	}
	for _, transition := range snapshot.Nodes {
		stage, exists := workflow.Stage(transition.NodeID)
		if !exists {
			t.Fatalf("plan contains unknown stage %q", transition.NodeID)
		}
		if isContentChangingStage(stage) && transition.Disposition != workflowkit.DispositionPreserve {
			t.Fatalf("no-content plan scheduled content-changing stage: %+v", transition)
		}
		if transition.NodeID == workflowkit.NodeID(workflowadapter.Package) && transition.Disposition != workflowkit.DispositionOperatorOnly {
			t.Fatalf("no-content plan did not retain package as operator-only: %+v", transition)
		}
	}
	for _, batch := range snapshot.Schedule {
		for _, nodeID := range batch.NodeIDs {
			if nodeID == workflowkit.NodeID(workflowadapter.Package) {
				t.Fatalf("no-content continuation scheduled package: %#v", snapshot.Schedule)
			}
		}
	}

	fixture.services.core.now = func() time.Time { return plannedAt.Add(time.Hour) }
	replayed, err := fixture.services.Continuations.PlanTaskContinuation(ctx, command)
	if err != nil || replayed.ID() != plan.ID() || replayed.Fingerprint() != plan.Fingerprint() || !replayed.Snapshot().ExpiresAt.Equal(snapshot.ExpiresAt) {
		t.Fatalf("idempotent plan replay = id=%q fingerprint=%q err=%v; want id=%q fingerprint=%q", replayed.ID(), replayed.Fingerprint(), err, plan.ID(), plan.Fingerprint())
	}
	conflict := command
	conflict.TargetNodeIDs = []workflowkit.NodeID{workflowadapter.SimilarityCheck}
	if _, err := fixture.services.Continuations.PlanTaskContinuation(ctx, conflict); !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Fatalf("changed command payload error = %v, want ErrIdempotencyConflict", err)
	}
}

func TestTaskContinuationKeepsImmutableCommandProvenanceAcrossPlanAndExecution(t *testing.T) {
	ctx := context.Background()
	fixture := newContinuationFixture(t, store.WorkflowRunFailedRecoverable)
	command := continuationCommand(t, ctx, fixture, "immutable-provenance", []workflowkit.NodeID{workflowadapter.QualityCheck}, false)
	command.Actor = "provenance-actor"
	command.Reason = "freeze this exact continuation intent"

	plan, err := fixture.services.Continuations.PlanTaskContinuation(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	commandRecord, err := fixture.dataStore.GetContinuationCommand(ctx, plan.Snapshot().CommandID)
	if err != nil || commandRecord == nil || commandRecord.Actor != command.Actor || commandRecord.Reason != command.Reason {
		t.Fatalf("immutable continuation command = %+v, err=%v", commandRecord, err)
	}
	frozenEvents, err := fixture.dataStore.ListAuditEvents(ctx, store.ListAuditEventsRequest{EntityType: "frozen_plan", EntityID: plan.ID()})
	if err != nil || len(frozenEvents) != 1 || frozenEvents[0].Actor != command.Actor || frozenEvents[0].Reason != command.Reason {
		t.Fatalf("frozen plan audit = %+v, err=%v", frozenEvents, err)
	}

	changedProvenance := command
	changedProvenance.Actor = "other-actor"
	changedProvenance.Reason = "a different reason"
	if _, err := fixture.services.Continuations.PlanTaskContinuation(ctx, changedProvenance); !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Fatalf("changed command provenance error = %v, want ErrIdempotencyConflict", err)
	}

	execution, err := fixture.services.Continuations.ExecuteTaskContinuation(ctx, plan.ID())
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []struct {
		entityType string
		entityID   string
		action     string
	}{
		{entityType: "workflow_run", entityID: fixture.run.ID, action: "workflow_run.continuation_committed"},
		{entityType: "continuation_execution", entityID: execution.ID, action: "continuation_execution.committed"},
	} {
		events, err := fixture.dataStore.ListAuditEvents(ctx, store.ListAuditEventsRequest{EntityType: target.entityType, EntityID: target.entityID})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, event := range events {
			if event.Action != target.action {
				continue
			}
			found = true
			if event.Actor != command.Actor || event.Reason != command.Reason {
				t.Fatalf("%s audit provenance = %+v, want actor=%q reason=%q", target.action, event, command.Actor, command.Reason)
			}
		}
		if !found {
			t.Fatalf("missing %s audit in %+v", target.action, events)
		}
	}
}

func TestTaskContinuationForceRecomputeAdvancesGeneration(t *testing.T) {
	ctx := context.Background()
	fixture := newContinuationFixture(t, store.WorkflowRunFailedRecoverable)
	plan, err := fixture.services.Continuations.PlanTaskContinuation(ctx, continuationCommand(t, ctx, fixture, "force-recompute", []workflowkit.NodeID{workflowadapter.QualityCheck}, true))
	if err != nil {
		t.Fatalf("plan force recompute: %v", err)
	}
	snapshot := plan.Snapshot()
	if snapshot.Strategy != workflowkit.StrategyRecompute {
		t.Fatalf("strategy = %q, want recompute", snapshot.Strategy)
	}
	for _, transition := range snapshot.Nodes {
		if transition.NodeID == workflowadapter.QualityCheck {
			if transition.Disposition != workflowkit.DispositionSchedule || transition.ToGeneration != transition.FromGeneration+1 {
				t.Fatalf("force-selected node transition = %+v", transition)
			}
			return
		}
	}
	t.Fatalf("force-selected node %q missing from plan", workflowadapter.QualityCheck)
}

func TestTaskContinuationExpandsFrozenStageGroupsWithoutPersistingNodeSelectors(t *testing.T) {
	ctx := context.Background()
	fixture := newContinuationFixture(t, store.WorkflowRunFailedRecoverable)
	command := continuationCommand(t, ctx, fixture, "quality-stage-group", nil, true)
	command.TargetStageGroups = []string{"quality"}

	plan, err := fixture.services.Continuations.PlanTaskContinuation(ctx, command)
	if err != nil {
		t.Fatalf("plan stage group continuation: %v", err)
	}
	if plan.Snapshot().Strategy != workflowkit.StrategyRecompute {
		t.Fatalf("group continuation strategy = %q, want recompute", plan.Snapshot().Strategy)
	}
	transitions := make(map[workflowkit.NodeID]workflowkit.NodeTransition, len(plan.Snapshot().Nodes))
	for _, transition := range plan.Snapshot().Nodes {
		transitions[transition.NodeID] = transition
	}
	for _, nodeID := range []workflowkit.NodeID{workflowadapter.CodeEdgeLint, workflowadapter.QualityCheck} {
		transition, found := transitions[nodeID]
		if !found || transition.Disposition != workflowkit.DispositionSchedule || transition.ToGeneration != transition.FromGeneration+1 {
			t.Fatalf("quality group node %q transition = %+v", nodeID, transition)
		}
	}

	storedPlan, err := fixture.dataStore.GetFrozenPlan(ctx, plan.ID())
	if err != nil || storedPlan == nil {
		t.Fatalf("load frozen group plan = %+v, %v", storedPlan, err)
	}
	commandRecord, err := fixture.dataStore.GetContinuationCommand(ctx, storedPlan.CommandID)
	if err != nil || commandRecord == nil {
		t.Fatalf("load group command = %+v, %v", commandRecord, err)
	}
	var persisted normalizedContinuationCommand
	if err := decodeStrictJSON(commandRecord.PayloadJSON, &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted.TargetStageGroups) != 1 || persisted.TargetStageGroups[0] != "quality" || len(persisted.TargetNodeIDs) != 0 {
		t.Fatalf("persisted command must retain only the group selector: %+v", persisted)
	}
}

func TestTaskContinuationPreviewDoesNotPersistDurableState(t *testing.T) {
	ctx := context.Background()
	fixture := newContinuationFixture(t, store.WorkflowRunFailedRecoverable)
	command := continuationCommand(t, ctx, fixture, "preview-only", nil, true)
	command.TargetStageGroups = []string{"quality"}
	beforeEvents, err := fixture.dataStore.ListAuditEvents(ctx, store.ListAuditEventsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	beforeRun, err := fixture.services.Runs.Get(ctx, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}

	preview, err := fixture.services.Continuations.PreviewTaskContinuation(ctx, command)
	if err != nil {
		t.Fatalf("preview continuation: %v", err)
	}
	if preview.ID() == "" || preview.Snapshot().ExpiresAt.IsZero() {
		t.Fatalf("preview did not return a fully frozen result: %+v", preview.Snapshot())
	}
	afterEvents, err := fixture.dataStore.ListAuditEvents(ctx, store.ListAuditEventsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	afterRun, err := fixture.services.Runs.Get(ctx, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterEvents) != len(beforeEvents) || afterRun.Version != beforeRun.Version || afterRun.ExecutionEpoch != beforeRun.ExecutionEpoch {
		t.Fatalf("preview mutated durable state: events %d->%d run %+v->%+v", len(beforeEvents), len(afterEvents), beforeRun, afterRun)
	}
	if _, err := fixture.services.Continuations.ExecuteTaskContinuation(ctx, preview.ID()); !errors.Is(err, ErrTaskContinuationNotFound) {
		t.Fatalf("ephemeral preview was executable: %v", err)
	}
}

func TestTaskContinuationRejectsContentChangingStagesWithoutCandidate(t *testing.T) {
	ctx := context.Background()
	fixture := newContinuationFixture(t, store.WorkflowRunFailedRecoverable)
	for _, target := range []workflowkit.NodeID{workflowadapter.TaskDesign, workflowadapter.MaterializeTask, workflowadapter.TaskRepair} {
		t.Run(string(target), func(t *testing.T) {
			_, err := fixture.services.Continuations.PlanTaskContinuation(ctx, continuationCommand(t, ctx, fixture, "content-stage-"+string(target), []workflowkit.NodeID{target}, true))
			if !errors.Is(err, ErrTaskContinuationTarget) {
				t.Fatalf("content-changing target error = %v, want ErrTaskContinuationTarget", err)
			}
		})
	}
}

func TestTaskContinuationRejectsOperatorOnlyPackageStage(t *testing.T) {
	ctx := context.Background()
	fixture := newContinuationFixture(t, store.WorkflowRunFailedRecoverable)
	_, err := fixture.services.Continuations.PlanTaskContinuation(ctx, continuationCommand(t, ctx, fixture, "operator-only-package", []workflowkit.NodeID{workflowadapter.Package}, true))
	if !errors.Is(err, ErrTaskContinuationTarget) {
		t.Fatalf("operator-only package continuation error = %v, want ErrTaskContinuationTarget", err)
	}
}

func TestTaskContinuationRejectsAutomaticallySelectedContentStage(t *testing.T) {
	ctx := context.Background()
	fixture := newContinuationFixture(t, store.WorkflowRunFailedRecoverable)
	fixture.services.Continuations.observer = hybridNoContentFixtureObserver(t, fixture.dataStore, fixture.services.core.objects)
	emptyInputs, err := workflowkit.FingerprintArtifactBindings(nil)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := fixture.dataStore.CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: fixture.run.ID, StageKey: workflowadapter.TaskDesign, StageGroup: "task_design", Ordinal: 2,
		InputFingerprint: string(emptyInputs), BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`, Actor: "tester", Reason: "content-stage fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	stage, err = fixture.dataStore.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: stage.ID, ExpectedVersion: stage.Version, ExecutionStatus: store.StageExecutionRunning, Actor: "tester", Reason: "content-stage fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.dataStore.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: stage.ID, ExpectedVersion: stage.Version, ExecutionStatus: store.StageExecutionInfraFailed, Actor: "tester", Reason: "content-stage fixture",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.services.Continuations.PlanTaskContinuation(ctx, continuationCommand(t, ctx, fixture, "automatic-content-stage", nil, false)); !errors.Is(err, ErrTaskContinuationTarget) {
		t.Fatalf("automatic content-stage error = %v, want ErrTaskContinuationTarget", err)
	}
}

func TestTaskContinuationRejectsInvalidationClosureThatReachesContentStage(t *testing.T) {
	ctx := context.Background()
	fixture := newContinuationFixture(t, store.WorkflowRunFailedRecoverable)
	if _, err := fixture.services.Continuations.PlanTaskContinuation(ctx, continuationCommand(t, ctx, fixture, "upstream-content-closure", []workflowkit.NodeID{workflowadapter.RepoPrepare}, true)); !errors.Is(err, ErrTaskContinuationTarget) {
		t.Fatalf("content-reaching invalidation closure error = %v, want ErrTaskContinuationTarget", err)
	}
}

func TestTaskContinuationStandardCatalogBlocksUnresolvedTaskRepairFindings(t *testing.T) {
	ctx := context.Background()
	fixture := newContinuationFixture(t, store.WorkflowRunFailedRecoverable)
	workflow, err := decodeFrozenWorkflow(fixture.run)
	if err != nil {
		t.Fatal(err)
	}
	seedStandardLineageBeforeStage(t, ctx, fixture.dataStore, fixture.services.core.objects, fixture.run, fixture.revision, workflow, workflowkit.StageKey(workflowadapter.TaskRepair))
	fixture.services.Continuations.observer = storeContinuationStateObserver{dataStore: fixture.dataStore, objects: fixture.services.core.objects}
	if _, err := fixture.services.Continuations.PlanTaskContinuation(ctx, continuationCommand(t, ctx, fixture, "unresolved-task-repair-findings", []workflowkit.NodeID{workflowadapter.QualityCheck}, true)); !errors.Is(err, ErrTaskContinuationTarget) {
		t.Fatalf("unresolved task-repair findings error = %v, want ErrTaskContinuationTarget", err)
	}
}

func TestTaskContinuationReusesVerifiedV4ArtifactLineage(t *testing.T) {
	ctx := context.Background()
	fixture := newContinuationFixture(t, store.WorkflowRunFailedRecoverable)
	workflow, err := decodeFrozenWorkflow(fixture.run)
	if err != nil {
		t.Fatal(err)
	}
	repoPrepare, exists := workflow.Stage(workflowkit.StageKey(workflowadapter.RepoPrepare))
	if !exists {
		t.Fatal("frozen workflow is missing repo_prepare")
	}
	bindings := fixtureInputBindings(repoPrepare)
	fingerprint, err := workflowkit.FingerprintArtifactBindings(bindings)
	if err != nil {
		t.Fatal(err)
	}
	seedReusableContinuationStage(t, ctx, fixture.dataStore, fixture.services.core.objects, fixture.run, fixture.revision, repoPrepare, bindings, fingerprint)
	fixture.services.Continuations.observer = hybridNoContentFixtureObserver(t, fixture.dataStore, fixture.services.core.objects)
	emptyInputs, err := workflowkit.FingerprintArtifactBindings(nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := fixture.services.Continuations.PlanTaskContinuation(ctx, continuationCommand(t, ctx, fixture, "reuse-v4-lineage", []workflowkit.NodeID{workflowadapter.QualityCheck}, true))
	if err != nil {
		t.Fatal(err)
	}
	for _, transition := range plan.Snapshot().Nodes {
		if transition.NodeID == workflowadapter.RepoPrepare {
			if transition.Disposition != workflowkit.DispositionPreserve || transition.ExpectedInputFingerprint != emptyInputs {
				t.Fatalf("V4 lineage was not reused: %+v", transition)
			}
			return
		}
	}
	t.Fatalf("missing %q transition", workflowadapter.RepoPrepare)
}

func TestTaskContinuationTreatsMissingAndCorruptArtifactObjectsAsUnavailable(t *testing.T) {
	ctx := context.Background()
	for _, scenario := range []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{
			name: "corrupt",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("tampered continuation artifact\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "missing",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newContinuationFixture(t, store.WorkflowRunFailedRecoverable)
			workflow, err := decodeFrozenWorkflow(fixture.run)
			if err != nil {
				t.Fatal(err)
			}
			repoPrepare, exists := workflow.Stage(workflowkit.StageKey(workflowadapter.RepoPrepare))
			if !exists {
				t.Fatal("frozen workflow is missing repo_prepare")
			}
			bindings := fixtureInputBindings(repoPrepare)
			fingerprint, err := workflowkit.FingerprintArtifactBindings(bindings)
			if err != nil {
				t.Fatal(err)
			}
			references := seedReusableContinuationStage(t, ctx, fixture.dataStore, fixture.services.core.objects, fixture.run, fixture.revision, repoPrepare, bindings, fingerprint)
			if len(references) == 0 {
				t.Fatal("fixture did not create an artifact reference")
			}
			manifest, err := loadStageArtifactManifestIndex(ctx, fixture.dataStore, references[0].ManifestID)
			if err != nil {
				t.Fatal(err)
			}
			object, err := manifest.objectFor(references[0])
			if err != nil {
				t.Fatal(err)
			}
			path, err := fixture.services.core.objects.ObjectPath(object)
			if err != nil {
				t.Fatal(err)
			}
			scenario.mutate(t, path)

			state, err := (storeContinuationStateObserver{dataStore: fixture.dataStore, objects: fixture.services.core.objects}).Observe(ctx, fixture.run, fixture.revision, workflow)
			if err != nil {
				t.Fatal(err)
			}
			if state.Latest[repoPrepare.Key].ID == "" {
				t.Fatal("continuation observer did not see the completed stage attempt")
			}
			for _, reuse := range state.ReuseStates {
				if reuse.NodeID != repoPrepare.Key {
					continue
				}
				if reuse.Present || reuse.ArtifactsIntact {
					t.Fatalf("%s object was accepted for continuation reuse: %+v", scenario.name, reuse)
				}
				return
			}
			t.Fatalf("missing reuse state for %q", repoPrepare.Key)
		})
	}
}

func TestTaskContinuationRejectsStaleAndExpiredPlans(t *testing.T) {
	ctx := context.Background()
	t.Run("stale checkpoint", func(t *testing.T) {
		fixture := newContinuationFixture(t, store.WorkflowRunFailedRecoverable)
		command := continuationCommand(t, ctx, fixture, "stale-checkpoint", []workflowkit.NodeID{workflowadapter.QualityCheck}, false)
		command.Expected.Sequence++
		if _, err := fixture.services.Continuations.PlanTaskContinuation(ctx, command); !errors.Is(err, store.ErrOptimisticLock) {
			t.Fatalf("stale plan error = %v, want ErrOptimisticLock", err)
		}
	})
	t.Run("expired frozen plan", func(t *testing.T) {
		fixture := newContinuationFixture(t, store.WorkflowRunFailedRecoverable)
		command := continuationCommand(t, ctx, fixture, "expired-plan", []workflowkit.NodeID{workflowadapter.QualityCheck}, false)
		plan, err := fixture.services.Continuations.PlanTaskContinuation(ctx, command)
		if err != nil {
			t.Fatal(err)
		}
		fixture.services.core.now = func() time.Time { return plan.Snapshot().ExpiresAt.Add(time.Second) }
		if _, err := fixture.services.Continuations.ExecuteTaskContinuation(ctx, plan.ID()); !errors.Is(err, store.ErrContinuationPlanExpired) {
			t.Fatalf("expired execute error = %v, want ErrContinuationPlanExpired", err)
		}
	})
}

func TestTaskContinuationSupportsPausedAndCanceledRuns(t *testing.T) {
	ctx := context.Background()
	for _, status := range []store.WorkflowRunStatus{store.WorkflowRunPaused, store.WorkflowRunCanceled} {
		t.Run(string(status), func(t *testing.T) {
			fixture := newContinuationFixture(t, status)
			command := continuationCommand(t, ctx, fixture, "continue-"+string(status), nil, false)
			plan, err := fixture.services.Continuations.PlanTaskContinuation(ctx, command)
			if err != nil {
				t.Fatalf("plan %s continuation: %v", status, err)
			}
			if plan.Snapshot().Strategy != workflowkit.StrategyRetryAttempt {
				t.Fatalf("%s continuation strategy = %q, want retry_attempt", status, plan.Snapshot().Strategy)
			}
		})
	}
}

func TestTaskContinuationExecuteUsesOneCASAndReplaysOneExecution(t *testing.T) {
	ctx := context.Background()
	fixture := newContinuationFixture(t, store.WorkflowRunFailedRecoverable)
	first, err := fixture.services.Continuations.PlanTaskContinuation(ctx, continuationCommand(t, ctx, fixture, "parallel-first", []workflowkit.NodeID{workflowadapter.QualityCheck}, false))
	if err != nil {
		t.Fatal(err)
	}
	second, err := fixture.services.Continuations.PlanTaskContinuation(ctx, continuationCommand(t, ctx, fixture, "parallel-second", []workflowkit.NodeID{workflowadapter.SimilarityCheck}, false))
	if err != nil {
		t.Fatal(err)
	}

	type executeResult struct {
		plan      workflowkit.ContinuationPlan
		execution store.ContinuationExecution
		err       error
	}
	results := make(chan executeResult, 2)
	start := make(chan struct{})
	var group sync.WaitGroup
	for _, plan := range []workflowkit.ContinuationPlan{first, second} {
		group.Add(1)
		go func(plan workflowkit.ContinuationPlan) {
			defer group.Done()
			<-start
			execution, err := fixture.services.Continuations.ExecuteTaskContinuation(ctx, plan.ID())
			results <- executeResult{plan: plan, execution: execution, err: err}
		}(plan)
	}
	close(start)
	group.Wait()
	close(results)

	var winner executeResult
	var stale int
	for result := range results {
		if result.err == nil {
			if winner.execution.ID != "" {
				t.Fatalf("two plans committed from the same checkpoint: first=%+v second=%+v", winner, result)
			}
			winner = result
			continue
		}
		if !errors.Is(result.err, store.ErrOptimisticLock) {
			t.Fatalf("parallel execute error = %v, want ErrOptimisticLock", result.err)
		}
		stale++
	}
	if winner.execution.ID == "" || stale != 1 {
		t.Fatalf("CAS result winner=%+v stale=%d", winner, stale)
	}
	fixture.services.core.now = func() time.Time { return winner.plan.Snapshot().ExpiresAt.Add(time.Second) }
	replayed, err := fixture.services.Continuations.ExecuteTaskContinuation(ctx, winner.plan.ID())
	if err != nil || replayed.ID != winner.execution.ID || replayed.State != store.ContinuationExecutionQueued {
		t.Fatalf("execution replay = %+v, %v; want %+v", replayed, err, winner.execution)
	}
	job, err := fixture.dataStore.GetDurableJobByIdempotency(ctx, continuationExecutionKey(winner.plan.ID())+":job")
	if err != nil || job == nil || job.EntityID != winner.execution.ID || job.State != store.JobQueued {
		t.Fatalf("atomic continuation job = %+v, %v", job, err)
	}
	updated, err := fixture.services.Runs.Get(ctx, fixture.run.ID)
	if err != nil || updated.ExecutionEpoch != fixture.run.ExecutionEpoch+1 {
		t.Fatalf("run epoch was not atomically advanced: %+v, %v", updated, err)
	}
}

func TestTaskContinuationBlocksInDoubtAndReconcilingEvidence(t *testing.T) {
	ctx := context.Background()
	t.Run("run in doubt", func(t *testing.T) {
		fixture := newContinuationFixture(t, store.WorkflowRunInDoubt)
		if _, err := fixture.services.Continuations.PlanTaskContinuation(ctx, continuationCommand(t, ctx, fixture, "run-in-doubt", []workflowkit.NodeID{workflowadapter.QualityCheck}, false)); !errors.Is(err, store.ErrContinuationReconciliationRequired) {
			t.Fatalf("in_doubt run plan error = %v, want ErrContinuationReconciliationRequired", err)
		}
	})
	t.Run("stage reconciling before execute", func(t *testing.T) {
		fixture := newContinuationFixture(t, store.WorkflowRunRunning)
		plan, err := fixture.services.Continuations.PlanTaskContinuation(ctx, continuationCommand(t, ctx, fixture, "stage-reconciling", []workflowkit.NodeID{workflowadapter.QualityCheck}, false))
		if err != nil {
			t.Fatal(err)
		}
		stage := createReconcilingStage(t, ctx, fixture)
		if stage.ExecutionStatus != store.StageExecutionReconciling {
			t.Fatalf("stage state = %q, want reconciling", stage.ExecutionStatus)
		}
		if _, err := fixture.services.Continuations.ExecuteTaskContinuation(ctx, plan.ID()); !errors.Is(err, store.ErrContinuationReconciliationRequired) {
			t.Fatalf("reconciling execute error = %v, want ErrContinuationReconciliationRequired", err)
		}
	})
	t.Run("unknown side effect before execute", func(t *testing.T) {
		fixture := newContinuationFixture(t, store.WorkflowRunRunning)
		plan, err := fixture.services.Continuations.PlanTaskContinuation(ctx, continuationCommand(t, ctx, fixture, "side-effect-unknown", []workflowkit.NodeID{workflowadapter.QualityCheck}, false))
		if err != nil {
			t.Fatal(err)
		}
		effect, err := fixture.dataStore.CreateSideEffectOperation(ctx, store.CreateSideEffectOperationRequest{
			OperationKey: "unknown-side-effect", IdempotencyKey: "unknown-side-effect-key", RunID: fixture.run.ID,
			EffectKind: "local_package", SourceDigest: "sha256:source", PayloadJSON: `{}`, Actor: "tester", Reason: "fixture",
		})
		if err != nil {
			t.Fatal(err)
		}
		effect, err = fixture.dataStore.TransitionSideEffectOperation(ctx, store.TransitionSideEffectOperationRequest{OperationID: effect.ID, ExpectedVersion: effect.Version, State: store.SideEffectStarted, Actor: "tester", Reason: "fixture"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.dataStore.TransitionSideEffectOperation(ctx, store.TransitionSideEffectOperationRequest{OperationID: effect.ID, ExpectedVersion: effect.Version, State: store.SideEffectUnknown, Actor: "tester", Reason: "fixture"}); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.services.Continuations.ExecuteTaskContinuation(ctx, plan.ID()); !errors.Is(err, store.ErrContinuationReconciliationRequired) {
			t.Fatalf("unknown side effect execute error = %v, want ErrContinuationReconciliationRequired", err)
		}
	})
}

func newContinuationFixture(t *testing.T, terminal store.WorkflowRunStatus) continuationFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	dataStore, err := store.OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	services, err := newLifecycleServicesForTest(root, dataStore)
	if err != nil {
		t.Fatal(err)
	}
	if services.Continuations == nil {
		t.Fatal("LifecycleServices did not wire TaskContinuationService")
	}
	source := writeLifecycleSnapshot(t, "continuation fixture\n")
	task, revision, err := services.Tasks.ImportTask(ctx, ImportTaskRequest{
		CreateDraftTaskRequest: CreateDraftTaskRequest{Slug: "continuation-" + t.Name(), Actor: "tester", Reason: "fixture"},
		SourceDirectory:        source,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := services.Runs.StartRun(ctx, StartRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, Profile: lifecycleCompleteProfile(t), ExecutionSpec: lifecycleExecutionSpec(task.ID, revision.ID, revision.TaskDigest), Trigger: "continue-fixture", Actor: "tester", Reason: "freeze fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	services.Continuations.observer = controlledNoContentFixtureObserver(t)
	run = transitionContinuationRun(t, ctx, dataStore, run, terminal)
	return continuationFixture{services: services, dataStore: dataStore, task: task, revision: revision, run: run}
}

type continuationStateObserverFunc func(context.Context, store.WorkflowRun, store.TaskRevision, workflowkit.WorkflowDescriptor) (continuationRunState, error)

func (observer continuationStateObserverFunc) Observe(ctx context.Context, run store.WorkflowRun, revision store.TaskRevision, workflow workflowkit.WorkflowDescriptor) (continuationRunState, error) {
	return observer(ctx, run, revision, workflow)
}

func (observer continuationStateObserverFunc) ObserveSubject(ctx context.Context, run store.WorkflowRun, subject workflowRunSubject, workflow workflowkit.WorkflowDescriptor) (continuationRunState, error) {
	if !subject.isTaskRevision() || subject.Revision == nil {
		return continuationRunState{}, fmt.Errorf("fixture observer only supports task-revision subjects")
	}
	return observer(ctx, run, *subject.Revision, workflow)
}

func controlledNoContentFixtureObserver(t *testing.T) continuationSubjectStateObserver {
	t.Helper()
	return continuationStateObserverFunc(func(_ context.Context, _ store.WorkflowRun, _ store.TaskRevision, workflow workflowkit.WorkflowDescriptor) (continuationRunState, error) {
		return controlledNoContentFixtureState(workflow)
	})
}

func hybridNoContentFixtureObserver(t *testing.T, dataStore *store.Store, objects *workflowruntime.ArtifactObjectStore) continuationSubjectStateObserver {
	t.Helper()
	return continuationStateObserverFunc(func(ctx context.Context, run store.WorkflowRun, revision store.TaskRevision, workflow workflowkit.WorkflowDescriptor) (continuationRunState, error) {
		state, err := controlledNoContentFixtureState(workflow)
		if err != nil {
			return continuationRunState{}, err
		}
		observed, err := (storeContinuationStateObserver{dataStore: dataStore, objects: objects}).Observe(ctx, run, revision, workflow)
		if err != nil {
			return continuationRunState{}, err
		}
		observedReuse := make(map[workflowkit.NodeID]workflowkit.StageReuseState, len(observed.ReuseStates))
		for _, reuse := range observed.ReuseStates {
			observedReuse[reuse.NodeID] = reuse
		}
		for index, reuse := range state.ReuseStates {
			if actual, exists := observedReuse[reuse.NodeID]; exists && actual.Present && actual.ArtifactsIntact {
				state.ReuseStates[index] = actual
			}
		}
		for nodeID, generation := range observed.Generations {
			if generation > state.Generations[nodeID] {
				state.Generations[nodeID] = generation
			}
		}
		for nodeID, latest := range observed.Latest {
			state.Latest[nodeID] = latest
		}
		state.InDoubt = observed.InDoubt
		return state, nil
	})
}

func controlledNoContentFixtureState(workflow workflowkit.WorkflowDescriptor) (continuationRunState, error) {
	state := continuationRunState{
		ReuseStates: make([]workflowkit.StageReuseState, 0, len(workflow.Stages)),
		Generations: make(map[workflowkit.NodeID]int, len(workflow.Stages)),
		Latest:      make(map[workflowkit.NodeID]store.StageAttempt, len(workflow.Stages)),
	}
	for _, stage := range workflow.Stages {
		bindings := fixtureInputBindings(stage)
		fingerprint, err := workflowkit.FingerprintArtifactBindings(bindings)
		if err != nil {
			return continuationRunState{}, err
		}
		state.ReuseStates = append(state.ReuseStates, workflowkit.StageReuseState{
			NodeID:                   stage.Key,
			Present:                  true,
			ArtifactsIntact:          true,
			ExpectedInputFingerprint: fingerprint,
			CurrentInputs:            bindings,
		})
	}
	state.Latest[workflowkit.NodeID(workflowadapter.QualityCheck)] = store.StageAttempt{StageKey: workflowadapter.QualityCheck, ExecutionStatus: store.StageExecutionInfraFailed}
	return state, nil
}

func fixtureInputBindings(descriptor workflowkit.StageDescriptor) []workflowkit.ArtifactBinding {
	bindings := make([]workflowkit.ArtifactBinding, 0, len(descriptor.Inputs))
	for _, input := range descriptor.Inputs {
		bindings = append(bindings, workflowkit.ArtifactBinding{
			Name:          input.Name,
			ArtifactID:    workflowkit.ArtifactID("fixture-input:" + string(descriptor.Key) + ":" + input.Name),
			ContentDigest: workflowkit.SHA256Fingerprint([]byte(string(descriptor.Key) + ":" + input.Name)),
			SchemaVersion: input.SchemaVersion,
		})
	}
	return bindings
}

func seedStandardLineageBeforeStage(t *testing.T, ctx context.Context, dataStore *store.Store, objects *workflowruntime.ArtifactObjectStore, run store.WorkflowRun, revision store.TaskRevision, workflow workflowkit.WorkflowDescriptor, stop workflowkit.StageKey) {
	t.Helper()
	for _, stage := range workflow.Stages {
		if stage.Key == stop {
			return
		}
		bindings := fixtureInputBindings(stage)
		fingerprint, err := workflowkit.FingerprintArtifactBindings(bindings)
		if err != nil {
			t.Fatal(err)
		}
		seedReusableContinuationStage(t, ctx, dataStore, objects, run, revision, stage, bindings, fingerprint)
	}
	t.Fatalf("frozen workflow is missing stop stage %q", stop)
}

func seedReusableContinuationStage(t *testing.T, ctx context.Context, dataStore *store.Store, objects *workflowruntime.ArtifactObjectStore, run store.WorkflowRun, revision store.TaskRevision, descriptor workflowkit.StageDescriptor, bindings []workflowkit.ArtifactBinding, inputFingerprint workflowkit.Fingerprint) []store.ArtifactRef {
	t.Helper()
	if objects == nil {
		t.Fatal("artifact object store is required for continuation lineage fixture")
	}
	encodedBindings, err := json.Marshal(bindings)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := dataStore.CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: run.ID, StageKey: string(descriptor.Key), StageGroup: descriptor.Group, Ordinal: 1,
		InputFingerprint: string(inputFingerprint), BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`, Actor: "tester", Reason: "continuation lineage fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	nodeAttemptID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	artifacts := make([]stageArtifactObject, 0, len(descriptor.Outputs))
	for _, output := range descriptor.Outputs {
		content := []byte("continuation-lineage:" + string(descriptor.Key) + ":" + output.Name)
		object, err := objects.PutBytes(ctx, content)
		if err != nil {
			t.Fatal(err)
		}
		artifacts = append(artifacts, stageArtifactObject{
			Key: output.Name, SchemaVersion: output.SchemaVersion, Digest: object.Digest, SizeBytes: object.SizeBytes,
		})
	}
	manifestPayload := stageArtifactManifestPayload{
		Format: stageArtifactManifestFormat, RunID: run.ID, StageAttemptID: stage.ID, NodeAttemptID: nodeAttemptID, StageKey: descriptor.Key, Artifacts: artifacts,
	}
	encodedManifest, err := json.Marshal(manifestPayload)
	if err != nil {
		t.Fatal(err)
	}
	manifestFingerprint, err := workflowkit.FingerprintBytes(stageArtifactManifestFormat, encodedManifest)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := dataStore.CreateArtifactManifest(ctx, store.CreateArtifactManifestRequest{
		SubjectRevisionID: revision.ID, SubjectDigest: revision.TaskDigest, WorkflowFingerprint: run.DefinitionHash,
		ManifestJSON: string(encodedManifest), ManifestFingerprint: string(manifestFingerprint),
		IdempotencyKey: "continuation-manifest:" + string(descriptor.Key), Actor: "tester", Reason: "continuation lineage fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	references := make([]store.ArtifactRef, 0, len(artifacts))
	for _, artifact := range artifacts {
		reference, err := dataStore.CreateArtifactRef(ctx, store.CreateArtifactRefRequest{
			ManifestID: manifest.ID, ArtifactKey: artifact.Key, ContentDigest: string(artifact.Digest),
			SchemaVersion: artifact.SchemaVersion, RunID: run.ID, StageKey: string(descriptor.Key), AttemptID: stage.ID, TurnOrdinal: artifact.TurnOrdinal,
			SubjectRevisionID: revision.ID, SubjectDigest: revision.TaskDigest, WorkflowFingerprint: run.DefinitionHash,
			InputBindingsJSON: string(encodedBindings), InputFingerprint: string(inputFingerprint), ProducerVersion: "continuation-fixture.v1", IdempotencyKey: "continuation-artifact:" + string(descriptor.Key) + ":" + artifact.Key, Actor: "tester", Reason: "continuation lineage fixture",
		})
		if err != nil {
			t.Fatal(err)
		}
		references = append(references, reference)
	}
	stage, err = dataStore.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: stage.ID, ExpectedVersion: stage.Version, ExecutionStatus: store.StageExecutionRunning, Actor: "tester", Reason: "continuation lineage fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dataStore.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: stage.ID, ExpectedVersion: stage.Version, ExecutionStatus: store.StageExecutionCompleted, Verdict: store.VerdictPass, ArtifactManifestID: manifest.ID, Actor: "tester", Reason: "continuation lineage fixture",
	}); err != nil {
		t.Fatal(err)
	}
	return references
}

func transitionContinuationRun(t *testing.T, ctx context.Context, dataStore *store.Store, run store.WorkflowRun, target store.WorkflowRunStatus) store.WorkflowRun {
	t.Helper()
	transition := func(status store.WorkflowRunStatus) {
		var err error
		run, err = dataStore.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{RunID: run.ID, ExpectedVersion: run.Version, Status: status, Actor: "tester", Reason: "continuation fixture state"})
		if err != nil {
			t.Fatal(err)
		}
	}
	switch target {
	case store.WorkflowRunCanceled:
		transition(store.WorkflowRunCanceled)
	case store.WorkflowRunRunning:
		transition(store.WorkflowRunRunning)
	case store.WorkflowRunFailedRecoverable:
		transition(store.WorkflowRunRunning)
		transition(store.WorkflowRunFailedRecoverable)
	case store.WorkflowRunPaused:
		transition(store.WorkflowRunRunning)
		transition(store.WorkflowRunPauseRequested)
		transition(store.WorkflowRunPausing)
		transition(store.WorkflowRunPaused)
	case store.WorkflowRunInDoubt:
		transition(store.WorkflowRunRunning)
		transition(store.WorkflowRunInDoubt)
	default:
		t.Fatalf("unsupported continuation fixture status %q", target)
	}
	return run
}

func continuationCommand(t *testing.T, ctx context.Context, fixture continuationFixture, key string, targets []workflowkit.NodeID, force bool) ContinueTaskCommand {
	t.Helper()
	checkpoint, err := fixture.services.Continuations.CurrentCheckpoint(ctx, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return ContinueTaskCommand{
		CommandKey:    key,
		TaskID:        fixture.task.ID,
		RunID:         fixture.run.ID,
		Expected:      checkpoint,
		TargetNodeIDs: append([]workflowkit.NodeID(nil), targets...),
		ForceSelected: force,
		Actor:         "tester",
		Reason:        fmt.Sprintf("continue fixture %s", key),
	}
}

func createReconcilingStage(t *testing.T, ctx context.Context, fixture continuationFixture) store.StageAttempt {
	t.Helper()
	emptyInputs, err := workflowkit.FingerprintArtifactBindings(nil)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := fixture.dataStore.CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: fixture.run.ID, StageKey: workflowadapter.QualityCheck, StageGroup: "quality", Ordinal: 2,
		InputFingerprint: string(emptyInputs), BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`, Actor: "tester", Reason: "reconcile fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []store.StageExecutionStatus{store.StageExecutionRunning, store.StageExecutionInDoubt, store.StageExecutionReconciling} {
		stage, err = fixture.dataStore.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
			StageAttemptID: stage.ID, ExpectedVersion: stage.Version, ExecutionStatus: status, Actor: "tester", Reason: "reconcile fixture",
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return stage
}
