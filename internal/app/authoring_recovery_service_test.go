package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

type authoringRecoveryFixture struct {
	root     string
	services *LifecycleServices
	store    *store.Store
	catalog  *stageprovider.DeploymentOperationCatalogResolver
	source   store.AuthoringSource
	session  store.AuthoringSession
	task     store.TaskV2
	run      store.WorkflowRun
	workflow workflowkit.WorkflowDescriptor
	failed   store.StageAttempt
}

func TestAuthoringRecoveryFreezesRetryPlanAndQueuesOneExecution(t *testing.T) {
	ctx := context.Background()
	fixture := newAuthoringRecoveryFixture(t, workflowkit.FailureNetwork)

	available, reason, err := fixture.services.AuthoringRecovery.CanRecover(ctx, fixture.run.ID)
	if err != nil || !available || reason != "" {
		t.Fatalf("authoring recovery availability = %t, %q, %v", available, reason, err)
	}
	checkpoint, err := fixture.services.AuthoringRecovery.CurrentCheckpoint(ctx, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	previewKey := authoringRecoveryUUID(t)
	beforePreview := snapshotAuthoringRecoveryManagedFiles(t, fixture.root)
	preview, err := fixture.services.AuthoringRecovery.PreviewAuthoringRecovery(ctx, AuthoringRecoveryCommand{
		CommandKey: previewKey, RunID: fixture.run.ID, Expected: checkpoint, Actor: "operator", Reason: "inspect frozen authoring recovery",
	})
	if err != nil {
		t.Fatalf("preview authoring recovery: %v", err)
	}
	if persisted, err := fixture.store.GetContinuationCommandByKey(ctx, previewKey); err != nil || persisted != nil {
		t.Fatalf("preview persisted command = %+v, %v", persisted, err)
	}
	if afterPreview := snapshotAuthoringRecoveryManagedFiles(t, fixture.root); !reflect.DeepEqual(afterPreview, beforePreview) {
		t.Fatal("preview authoring recovery changed managed state")
	}
	assertAuthoringRecoveryPlan(t, preview, fixture)

	key := authoringRecoveryUUID(t)
	plan, err := fixture.services.AuthoringRecovery.PlanAuthoringRecovery(ctx, AuthoringRecoveryCommand{
		CommandKey: key, RunID: fixture.run.ID, Expected: checkpoint, Actor: "operator", Reason: "recover network-failed authoring stage",
	})
	if err != nil {
		t.Fatalf("plan authoring recovery: %v", err)
	}
	assertAuthoringRecoveryPlan(t, plan, fixture)
	command, err := fixture.store.GetContinuationCommandByKey(ctx, key)
	if err != nil || command == nil {
		t.Fatalf("stored authoring recovery command = %+v, %v", command, err)
	}
	persisted, err := decodeAuthoringRecoveryCommand(*command)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Format != authoringRecoveryCommandFormat || len(persisted.FailureStageAttemptIDs) != 1 || persisted.FailureStageAttemptIDs[0] != fixture.failed.ID ||
		persisted.Expected.SubjectID != fixture.source.ID || persisted.Expected.SubjectRevisionID != fixture.session.ID ||
		persisted.Expected.SubjectDigest != workflowkit.SubjectDigest(fixture.source.SnapshotContentDigest) ||
		persisted.Expected.WorkflowFingerprint != workflowkit.Fingerprint(fixture.run.DefinitionHash) ||
		persisted.TargetTaskID != fixture.task.ID || persisted.TargetTaskVersion != fixture.task.Version {
		t.Fatalf("persisted authoring recovery command = %+v", persisted)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(command.PayloadJSON), &payload); err != nil {
		t.Fatalf("decode v2 authoring recovery command payload: %v", err)
	}
	for _, redundant := range []string{"authoring_source_id", "authoring_session_id", "source_digest", "definition_fingerprint"} {
		if _, found := payload[redundant]; found {
			t.Fatalf("v2 authoring recovery command retained redundant %q: %s", redundant, command.PayloadJSON)
		}
	}

	execution, err := fixture.services.AuthoringRecovery.ExecuteAuthoringRecovery(ctx, plan.ID())
	if err != nil {
		t.Fatalf("execute authoring recovery: %v", err)
	}
	replayed, err := fixture.services.AuthoringRecovery.ExecuteAuthoringRecovery(ctx, plan.ID())
	if err != nil || replayed.ID != execution.ID || replayed.State != store.ContinuationExecutionQueued {
		t.Fatalf("replayed recovery execution = %+v, %v; first=%+v", replayed, err, execution)
	}
	updated, err := fixture.store.GetWorkflowRun(ctx, fixture.run.ID)
	if err != nil || updated == nil || updated.ExecutionEpoch != fixture.run.ExecutionEpoch+1 || updated.Version != fixture.run.Version+1 {
		t.Fatalf("recovery commit run = %+v, %v", updated, err)
	}
	refreshed, err := fixture.services.AuthoringRecovery.CurrentCheckpoint(ctx, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	replayedPlan, err := fixture.services.AuthoringRecovery.PlanAuthoringRecovery(ctx, AuthoringRecoveryCommand{
		CommandKey: key, RunID: fixture.run.ID, Expected: refreshed, Actor: "operator", Reason: "recover network-failed authoring stage",
	})
	if err != nil || replayedPlan.ID() != plan.ID() {
		t.Fatalf("replayed plan after epoch advance = %s, %v; first=%s", replayedPlan.ID(), err, plan.ID())
	}
	if _, err := fixture.services.AuthoringRecovery.PlanAuthoringRecovery(ctx, AuthoringRecoveryCommand{
		CommandKey: authoringRecoveryUUID(t), RunID: fixture.run.ID, Expected: checkpoint, Actor: "operator", Reason: "stale new recovery request",
	}); !errors.Is(err, store.ErrOptimisticLock) {
		t.Fatalf("new key with stale checkpoint error = %v, want ErrOptimisticLock", err)
	}
	if _, err := fixture.services.AuthoringRecovery.PlanAuthoringRecovery(ctx, AuthoringRecoveryCommand{
		CommandKey: authoringRecoveryUUID(t), RunID: fixture.run.ID, Expected: refreshed, Actor: "operator", Reason: "overlapping recovery request",
	}); !errors.Is(err, ErrAuthoringRecoveryUnavailable) {
		t.Fatalf("new key while recovery is active error = %v, want unavailable", err)
	}
	active, err := fixture.store.HasActiveContinuationExecutionForRun(ctx, fixture.run.ID)
	if err != nil || !active {
		t.Fatalf("active authoring recovery = %t, %v", active, err)
	}
	if available, reason, err := fixture.services.AuthoringRecovery.CanRecover(ctx, fixture.run.ID); err != nil || available || !strings.Contains(reason, "active recovery") {
		t.Fatalf("availability while execution is queued = %t, %q, %v", available, reason, err)
	}
}

func TestAuthoringRecoveryRejectsWaitingContinuationWithoutWritingRecoveryState(t *testing.T) {
	ctx := context.Background()
	fixture := newAuthoringRecoveryFixture(t, workflowkit.FailureNetwork)
	running, err := fixture.store.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: fixture.run.ID, ExpectedVersion: fixture.run.Version, Status: store.WorkflowRunRunning,
		Actor: "worker", Reason: "reach a legal historical continuation transition",
	})
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := fixture.store.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: running.ID, ExpectedVersion: running.Version, Status: store.WorkflowRunWaitingContinuation,
		Actor: "worker", Reason: "preserve historical authoring continuation state",
	})
	if err != nil {
		t.Fatal(err)
	}
	available, reason, err := fixture.services.AuthoringRecovery.CanRecover(ctx, waiting.ID)
	if err != nil || available || !strings.Contains(reason, string(store.WorkflowRunWaitingContinuation)) {
		t.Fatalf("waiting continuation recovery availability = %t, %q, %v", available, reason, err)
	}
	checkpoint, err := fixture.services.AuthoringRecovery.CurrentCheckpoint(ctx, waiting.ID)
	if err != nil {
		t.Fatal(err)
	}
	key := authoringRecoveryUUID(t)
	_, err = fixture.services.AuthoringRecovery.PlanAuthoringRecovery(ctx, AuthoringRecoveryCommand{
		CommandKey: key, RunID: waiting.ID, Expected: checkpoint, Actor: "operator", Reason: "do not mutate historical waiting continuation",
	})
	if !errors.Is(err, ErrAuthoringRecoveryUnavailable) {
		t.Fatalf("waiting continuation recovery plan = %v, want unavailable", err)
	}
	if command, lookupErr := fixture.store.GetContinuationCommandByKey(ctx, key); lookupErr != nil || command != nil {
		t.Fatalf("waiting continuation created recovery command=%+v err=%v", command, lookupErr)
	}
	updated, err := fixture.store.GetWorkflowRun(ctx, waiting.ID)
	if err != nil || updated == nil || updated.Version != waiting.Version || updated.ExecutionEpoch != waiting.ExecutionEpoch || updated.Status != store.WorkflowRunWaitingContinuation {
		t.Fatalf("waiting continuation Run mutated by rejected recovery: %+v, %v", updated, err)
	}
}

func TestAuthoringRecoveryResumesLegacyV1Command(t *testing.T) {
	ctx := context.Background()
	fixture := newAuthoringRecoveryFixture(t, workflowkit.FailureNetwork)
	checkpoint, err := fixture.services.AuthoringRecovery.CurrentCheckpoint(ctx, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	key := authoringRecoveryUUID(t)
	legacy := authoringRecoveryCommandV1{
		Format:                 authoringRecoveryCommandFormatV1,
		CommandKey:             key,
		RunID:                  fixture.run.ID,
		AuthoringSourceID:      fixture.source.ID,
		AuthoringSessionID:     fixture.session.ID,
		TargetTaskID:           fixture.task.ID,
		TargetTaskVersion:      fixture.task.Version,
		SourceDigest:           workflowkit.SubjectDigest(fixture.source.SnapshotContentDigest),
		DefinitionFingerprint:  workflowkit.Fingerprint(fixture.run.DefinitionHash),
		Expected:               checkpoint,
		FailureStageAttemptIDs: []string{fixture.failed.ID},
		TargetNodeIDs:          []workflowkit.NodeID{workflowkit.NodeID(workflowadapter.RepoAnalyze)},
	}
	payload, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	record, err := fixture.store.CreateContinuationCommand(ctx, store.CreateContinuationCommandRequest{
		CommandKey: key, SubjectID: fixture.source.ID, RunID: fixture.run.ID, PayloadJSON: string(payload),
		Actor: "operator", Reason: "resume persisted v1 recovery command",
	})
	if err != nil {
		t.Fatal(err)
	}

	plan, err := fixture.services.AuthoringRecovery.PlanAuthoringRecovery(ctx, AuthoringRecoveryCommand{
		CommandKey: key, RunID: fixture.run.ID, Expected: checkpoint, Actor: "operator", Reason: "resume persisted v1 recovery command",
	})
	if err != nil {
		t.Fatalf("resume legacy v1 authoring recovery command: %v", err)
	}
	assertAuthoringRecoveryPlan(t, plan, fixture)
	decoded, err := decodeAuthoringRecoveryCommand(record)
	if err != nil || decoded.Format != authoringRecoveryCommandFormat || decoded.Expected != checkpoint {
		t.Fatalf("normalize legacy v1 command = %+v, %v", decoded, err)
	}
	if _, err := fixture.services.AuthoringRecovery.ExecuteAuthoringRecovery(ctx, plan.ID()); err != nil {
		t.Fatalf("execute resumed legacy v1 authoring recovery: %v", err)
	}
}

func TestAuthoringRecoveryRevalidatesTargetsAfterCommandPersistence(t *testing.T) {
	ctx := context.Background()
	fixture := newAuthoringRecoveryFixture(t, workflowkit.FailureNetwork)
	checkpoint, err := fixture.services.AuthoringRecovery.CurrentCheckpoint(ctx, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	key := authoringRecoveryUUID(t)
	baseObserver := fixture.services.AuthoringRecovery.observer
	observations := 0
	fixture.services.AuthoringRecovery.observer = authoringRecoverySubjectObserverFunc(func(ctx context.Context, run store.WorkflowRun, subject workflowRunSubject, workflow workflowkit.WorkflowDescriptor) (continuationRunState, error) {
		state, err := baseObserver.ObserveSubject(ctx, run, subject, workflow)
		if err != nil {
			return continuationRunState{}, err
		}
		observations++
		if observations != 2 {
			return state, nil
		}
		persisted, lookupErr := fixture.store.GetContinuationCommandByKey(ctx, key)
		if lookupErr != nil || persisted == nil {
			t.Fatalf("command was not durable before revalidation: %+v, %v", persisted, lookupErr)
		}
		stageID := workflowkit.NodeID(workflowadapter.RepoAnalyze)
		latest := state.Latest[stageID]
		latest.ID = authoringRecoveryUUID(t)
		state.Latest[stageID] = latest
		return state, nil
	})

	_, err = fixture.services.AuthoringRecovery.PlanAuthoringRecovery(ctx, AuthoringRecoveryCommand{
		CommandKey: key, RunID: fixture.run.ID, Expected: checkpoint, Actor: "operator", Reason: "reject target drift after command persistence",
	})
	if !errors.Is(err, store.ErrOptimisticLock) {
		t.Fatalf("target drift recovery plan error = %v, want ErrOptimisticLock", err)
	}
	if observations != 2 {
		t.Fatalf("authoring recovery observations = %d, want post-persistence revalidation", observations)
	}
	record, err := fixture.store.GetContinuationCommandByKey(ctx, key)
	if err != nil || record == nil {
		t.Fatalf("persisted command after target drift = %+v, %v", record, err)
	}
	if frozen, err := fixture.store.GetFrozenPlanByCommand(ctx, record.ID); err != nil || frozen != nil {
		t.Fatalf("target-drifted command froze plan = %+v, %v", frozen, err)
	}
}

func TestAuthoringAdmissionRecoveryRegeneratesOnlyIndependentPackageProducers(t *testing.T) {
	template := workflowadapter.StandardAuthoringTaskAdmissionWorkflowTemplate()
	workflow, err := template.Compile(lifecycleCompleteProfileForTemplate(t, template))
	if err != nil {
		t.Fatal(err)
	}
	state := continuationRunState{Latest: map[workflowkit.NodeID]store.StageAttempt{
		workflowkit.NodeID(workflowadapter.CodeEdgePackageAdmission): {
			ID: "admission-attempt", StageKey: workflowadapter.CodeEdgePackageAdmission,
			ExecutionStatus: store.StageExecutionCompleted, Verdict: store.VerdictNeedsRepair,
		},
	}}
	targets, failures, err := authoringRecoveryTargets(store.WorkflowRun{Status: store.WorkflowRunWaitingContinuation}, workflow.Descriptor, state)
	if err != nil {
		t.Fatal(err)
	}
	want := []workflowkit.NodeID{
		workflowkit.NodeID(workflowadapter.TaskTOMLGen), workflowkit.NodeID(workflowadapter.DockerfileGen), workflowkit.NodeID(workflowadapter.TestsAnalysis),
	}
	if !reflect.DeepEqual(targets, want) || !reflect.DeepEqual(failures, []string{"admission-attempt"}) {
		t.Fatalf("admission recovery targets=%+v failures=%+v", targets, failures)
	}
}

func TestAuthoringRecoveryReusesVerifiedSourceSessionArtifacts(t *testing.T) {
	ctx := context.Background()
	fixture := newAuthoringRecoveryFixture(t, workflowkit.FailureNetwork)
	seedAuthoringRecoveryRepoPrepare(t, ctx, fixture)
	checkpoint, err := fixture.services.AuthoringRecovery.CurrentCheckpoint(ctx, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := fixture.services.AuthoringRecovery.PlanAuthoringRecovery(ctx, AuthoringRecoveryCommand{
		CommandKey: authoringRecoveryUUID(t), RunID: fixture.run.ID, Expected: checkpoint, Actor: "operator", Reason: "reuse verified source preparation",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, transition := range plan.Snapshot().Nodes {
		if transition.NodeID != workflowkit.NodeID(workflowadapter.RepoPrepare) {
			continue
		}
		if transition.Disposition != workflowkit.DispositionPreserve || transition.ExpectedInputFingerprint == "" {
			t.Fatalf("repo_prepare transition = %+v, want verified preservation", transition)
		}
		return
	}
	t.Fatal("authoring recovery plan omitted repo_prepare")
}

func TestAuthoringRecoveryResumesPausedRunBeforeFirstStage(t *testing.T) {
	ctx := context.Background()
	fixture := newPausedAuthoringRecoveryFixture(t)

	available, reason, err := fixture.services.AuthoringRecovery.CanRecover(ctx, fixture.run.ID)
	if err != nil || !available || reason != "" {
		t.Fatalf("paused authoring recovery availability = %t, %q, %v", available, reason, err)
	}
	checkpoint, err := fixture.services.AuthoringRecovery.CurrentCheckpoint(ctx, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := fixture.services.AuthoringRecovery.PlanAuthoringRecovery(ctx, AuthoringRecoveryCommand{
		CommandKey: authoringRecoveryUUID(t), RunID: fixture.run.ID, Expected: checkpoint, Actor: "operator", Reason: "resume before first stage",
	})
	if err != nil {
		t.Fatalf("plan paused authoring recovery: %v", err)
	}
	for _, transition := range plan.Snapshot().Nodes {
		if transition.NodeID == workflowkit.NodeID(workflowadapter.RepoPrepare) {
			if transition.Disposition != workflowkit.DispositionSchedule {
				t.Fatalf("paused source-root transition = %+v", transition)
			}
			if _, err := fixture.services.AuthoringRecovery.ExecuteAuthoringRecovery(ctx, plan.ID()); err != nil {
				t.Fatalf("execute paused authoring recovery: %v", err)
			}
			return
		}
	}
	t.Fatal("paused authoring recovery omitted repo_prepare")
}

func TestAuthoringRecoveryFailsClosedOnDeploymentCatalogDrift(t *testing.T) {
	ctx := context.Background()
	fixture := newAuthoringRecoveryFixture(t, workflowkit.FailureNetwork)

	driftedDocument := fixture.catalog.Catalog()
	driftedDocument.CatalogVersion = "authoring-recovery-drift"
	driftedCatalog, err := stageprovider.NewDeploymentOperationCatalogResolver(driftedDocument)
	if err != nil {
		t.Fatalf("build drifted deployment catalog: %v", err)
	}
	driftedServices, err := NewLifecycleServicesWithOptions(fixture.root, fixture.store, standardAuthoringLaunchTestOptions(nil, nil, driftedCatalog))
	if err != nil {
		t.Fatalf("reopen lifecycle services with drifted catalog: %v", err)
	}

	available, reason, err := driftedServices.AuthoringRecovery.CanRecover(ctx, fixture.run.ID)
	if err != nil || available || !strings.Contains(reason, stageprovider.ErrDeploymentOperationCatalogDrift.Error()) {
		t.Fatalf("catalog-drifted recovery availability = %t, %q, %v", available, reason, err)
	}
	checkpoint, err := driftedServices.AuthoringRecovery.CurrentCheckpoint(ctx, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	commandKey := authoringRecoveryUUID(t)
	if _, err := driftedServices.AuthoringRecovery.PlanAuthoringRecovery(ctx, AuthoringRecoveryCommand{
		CommandKey: commandKey, RunID: fixture.run.ID, Expected: checkpoint, Actor: "operator", Reason: "reject catalog-drifted recovery",
	}); !errors.Is(err, ErrAuthoringRecoveryUnavailable) {
		t.Fatalf("catalog-drifted recovery plan error = %v, want unavailable", err)
	}
	if command, err := fixture.store.GetContinuationCommandByKey(ctx, commandKey); err != nil || command != nil {
		t.Fatalf("catalog-drifted recovery persisted command = %+v, %v", command, err)
	}
	updated, err := fixture.store.GetWorkflowRun(ctx, fixture.run.ID)
	if err != nil || updated == nil || updated.Version != fixture.run.Version || updated.ExecutionEpoch != fixture.run.ExecutionEpoch {
		t.Fatalf("catalog-drifted recovery changed run = %+v, %v", updated, err)
	}
}

func TestAuthoringRecoveryFailsClosedOnDeploymentCatalogLockDrift(t *testing.T) {
	ctx := context.Background()
	fixture := newLockedAuthoringRecoveryFixture(t, workflowkit.FailureNetwork)
	driftedResolver := &authoringRecoveryCatalogLockResolver{
		DeploymentOperationCatalogResolver: fixture.catalog,
		identity:                           authoringRecoveryCatalogLockIdentity("v2"),
	}
	driftedServices, err := NewLifecycleServicesWithOptions(fixture.root, fixture.store, LifecycleServicesOptions{
		OperationResolver: driftedResolver,
		DeploymentCatalogResolvers: []TemplateDeploymentCatalogResolver{{
			Template: workflowadapter.StandardAuthoringTemplateReference(), Resolver: driftedResolver,
		}},
		RequireDeploymentCatalog: true,
		RequireDeploymentLock:    true,
	})
	if err != nil {
		t.Fatalf("reopen lifecycle services with drifted catalog lock: %v", err)
	}

	available, reason, err := driftedServices.AuthoringRecovery.CanRecover(ctx, fixture.run.ID)
	if err != nil || available || !strings.Contains(reason, stageprovider.ErrDeploymentOperationCatalogLockDrift.Error()) {
		t.Fatalf("catalog-lock-drifted recovery availability = %t, %q, %v", available, reason, err)
	}
	checkpoint, err := driftedServices.AuthoringRecovery.CurrentCheckpoint(ctx, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	commandKey := authoringRecoveryUUID(t)
	if _, err := driftedServices.AuthoringRecovery.PlanAuthoringRecovery(ctx, AuthoringRecoveryCommand{
		CommandKey: commandKey, RunID: fixture.run.ID, Expected: checkpoint, Actor: "operator", Reason: "reject catalog-lock-drifted recovery",
	}); !errors.Is(err, ErrAuthoringRecoveryUnavailable) {
		t.Fatalf("catalog-lock-drifted recovery plan error = %v, want unavailable", err)
	}
	if command, err := fixture.store.GetContinuationCommandByKey(ctx, commandKey); err != nil || command != nil {
		t.Fatalf("catalog-lock-drifted recovery persisted command = %+v, %v", command, err)
	}
	updated, err := fixture.store.GetWorkflowRun(ctx, fixture.run.ID)
	if err != nil || updated == nil || updated.Version != fixture.run.Version || updated.ExecutionEpoch != fixture.run.ExecutionEpoch {
		t.Fatalf("catalog-lock-drifted recovery changed run = %+v, %v", updated, err)
	}
}

func TestAuthoringRecoveryRejectsInDoubtAndFrozenNonRetryableFailure(t *testing.T) {
	ctx := context.Background()
	t.Run("in doubt requires reconciliation", func(t *testing.T) {
		fixture := newAuthoringRecoveryFixture(t, workflowkit.FailureNetwork)
		run := transitionAuthoringRecoveryRun(t, ctx, fixture.store, fixture.run, store.WorkflowRunInDoubt)
		fixture.run = run
		available, reason, err := fixture.services.AuthoringRecovery.CanRecover(ctx, run.ID)
		if err != nil || available || !strings.Contains(reason, store.ErrContinuationReconciliationRequired.Error()) {
			t.Fatalf("in_doubt availability = %t, %q, %v", available, reason, err)
		}
		checkpoint, checkpointErr := fixture.services.AuthoringRecovery.CurrentCheckpoint(ctx, run.ID)
		if checkpointErr != nil {
			t.Fatal(checkpointErr)
		}
		_, err = fixture.services.AuthoringRecovery.PlanAuthoringRecovery(ctx, AuthoringRecoveryCommand{
			CommandKey: authoringRecoveryUUID(t), RunID: run.ID, Expected: checkpoint, Actor: "operator", Reason: "attempt unreconciled recovery",
		})
		if !errors.Is(err, store.ErrContinuationReconciliationRequired) {
			t.Fatalf("in_doubt plan error = %v, want reconciliation requirement", err)
		}
	})
	t.Run("retry policy rejects permanent failure", func(t *testing.T) {
		fixture := newAuthoringRecoveryFixture(t, workflowkit.FailurePermanent)
		available, reason, err := fixture.services.AuthoringRecovery.CanRecover(ctx, fixture.run.ID)
		if err != nil || available || !strings.Contains(reason, "retry policy") {
			t.Fatalf("permanent failure availability = %t, %q, %v", available, reason, err)
		}
		checkpoint, checkpointErr := fixture.services.AuthoringRecovery.CurrentCheckpoint(ctx, fixture.run.ID)
		if checkpointErr != nil {
			t.Fatal(checkpointErr)
		}
		_, err = fixture.services.AuthoringRecovery.PlanAuthoringRecovery(ctx, AuthoringRecoveryCommand{
			CommandKey: authoringRecoveryUUID(t), RunID: fixture.run.ID, Expected: checkpoint, Actor: "operator", Reason: "attempt permanent failure recovery",
		})
		if !errors.Is(err, ErrAuthoringRecoveryUnavailable) {
			t.Fatalf("permanent failure plan error = %v, want authoring recovery unavailable", err)
		}
	})
}

func TestAuthoringRecoveryRejectsMaterializedRunWithoutSecondRevision(t *testing.T) {
	ctx := context.Background()
	fixture := newAuthoringRecoveryFixture(t, workflowkit.FailureNetwork)
	checkpoint, err := fixture.services.AuthoringRecovery.CurrentCheckpoint(ctx, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := fixture.services.AuthoringRecovery.PlanAuthoringRecovery(ctx, AuthoringRecoveryCommand{
		CommandKey: authoringRecoveryUUID(t), RunID: fixture.run.ID, Expected: checkpoint, Actor: "operator", Reason: "freeze plan before materialization race",
	})
	if err != nil {
		t.Fatal(err)
	}
	run := transitionAuthoringRecoveryRun(t, ctx, fixture.store, fixture.run, store.WorkflowRunRunning)
	task, err := fixture.store.GetTaskV2(ctx, fixture.task.ID)
	if err != nil || task == nil {
		t.Fatalf("load draft task = %+v, %v", task, err)
	}
	materialized, err := fixture.store.MaterializeAuthoringTask(ctx, store.MaterializeAuthoringTaskRequest{
		IdempotencyKey: authoringRecoveryUUID(t), AuthoringSessionID: fixture.session.ID, AuthoringRunID: run.ID,
		ExpectedTaskVersion: task.Version, ExpectedRunVersion: run.Version,
		TaskDigest: "harbor.task.v2:sha256:" + strings.Repeat("d", 64), ProposalDigest: "proposal",
		ManifestID: "authoring-recovery-materialized-manifest", ChangeSummary: "race fixture", MetadataJSON: `{}`,
		Actor: "worker", Reason: "materialize before recovery commit",
	})
	if err != nil {
		t.Fatalf("materialize authoring fixture: %v", err)
	}
	loadedRun, err := fixture.store.GetWorkflowRun(ctx, run.ID)
	if err != nil || loadedRun == nil {
		t.Fatalf("load materialized Run = %+v, %v", loadedRun, err)
	}
	runValue := transitionAuthoringRecoveryRun(t, ctx, fixture.store, *loadedRun, store.WorkflowRunFailedRecoverable)
	fixture.run = runValue
	if _, err := fixture.services.AuthoringRecovery.ExecuteAuthoringRecovery(ctx, plan.ID()); !errors.Is(err, ErrAuthoringRecoveryUnavailable) {
		t.Fatalf("materialized recovery execution error = %v, want unavailable", err)
	}
	finalTask, err := fixture.store.GetTaskV2(ctx, fixture.task.ID)
	if err != nil || finalTask == nil || finalTask.CurrentRevisionID != materialized.Revision.ID {
		t.Fatalf("materialized task = %+v, %v", finalTask, err)
	}
	if duplicate, err := fixture.store.GetAuthoringTaskMaterializationForRun(ctx, fixture.run.ID); err != nil || duplicate == nil || duplicate.RevisionID != materialized.Revision.ID {
		t.Fatalf("materialization receipt after rejected recovery = %+v, %v", duplicate, err)
	}
}

func newAuthoringRecoveryFixture(t *testing.T, failure workflowkit.FailureClass) authoringRecoveryFixture {
	t.Helper()
	ctx := context.Background()
	fixture := newAuthoringRecoveryLaunchFixture(t)
	return failAuthoringRecoveryLaunchFixture(t, ctx, fixture, failure)
}

func newLockedAuthoringRecoveryFixture(t *testing.T, failure workflowkit.FailureClass) authoringRecoveryFixture {
	t.Helper()
	ctx := context.Background()
	fixture := newLockedAuthoringRecoveryLaunchFixture(t)
	return failAuthoringRecoveryLaunchFixture(t, ctx, fixture, failure)
}

func failAuthoringRecoveryLaunchFixture(t *testing.T, ctx context.Context, fixture authoringRecoveryFixture, failure workflowkit.FailureClass) authoringRecoveryFixture {
	t.Helper()
	runValue := transitionAuthoringRecoveryRun(t, ctx, fixture.store, fixture.run, store.WorkflowRunRunning)
	stage, found := fixture.workflow.Stage(workflowkit.NodeID(workflowadapter.RepoAnalyze))
	if !found {
		t.Fatal("frozen authoring workflow has no repo_analyze")
	}
	if failure != workflowkit.FailurePermanent && !stage.Retry.Allows(failure) {
		t.Fatalf("fixture repo_analyze retry policy does not allow %s", failure)
	}
	emptyInputs, err := workflowkit.FingerprintArtifactBindings(nil)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := fixture.store.CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: runValue.ID, StageKey: string(stage.Key), StageGroup: stage.Group, Ordinal: 1, InputFingerprint: string(emptyInputs),
		BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`, Actor: "worker", Reason: "create failed authoring stage",
	})
	if err != nil {
		t.Fatal(err)
	}
	failed, err = fixture.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: failed.ID, ExpectedVersion: failed.Version, ExecutionStatus: store.StageExecutionRunning, Actor: "worker", Reason: "run failed authoring stage",
	})
	if err != nil {
		t.Fatal(err)
	}
	failed, err = fixture.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: failed.ID, ExpectedVersion: failed.Version, ExecutionStatus: store.StageExecutionInfraFailed,
		FailureClass: string(failure), ErrorText: "simulated authoring provider failure", Actor: "worker", Reason: "fail authoring stage",
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.run = transitionAuthoringRecoveryRun(t, ctx, fixture.store, runValue, store.WorkflowRunFailedRecoverable)
	fixture.failed = failed
	return fixture
}

func newPausedAuthoringRecoveryFixture(t *testing.T) authoringRecoveryFixture {
	t.Helper()
	ctx := context.Background()
	fixture := newAuthoringRecoveryLaunchFixture(t)
	run := transitionAuthoringRecoveryRun(t, ctx, fixture.store, fixture.run, store.WorkflowRunRunning)
	run = transitionAuthoringRecoveryRun(t, ctx, fixture.store, run, store.WorkflowRunPauseRequested)
	run = transitionAuthoringRecoveryRun(t, ctx, fixture.store, run, store.WorkflowRunPausing)
	fixture.run = transitionAuthoringRecoveryRun(t, ctx, fixture.store, run, store.WorkflowRunPaused)
	return fixture
}

func newAuthoringRecoveryLaunchFixture(t *testing.T) authoringRecoveryFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	capturer := &standardAuthoringSourceCapturerFixture{
		coordinate: standardAuthoringLaunchTestCoordinate,
		snapshot:   standardAuthoringLaunchTestSnapshot(t, standardAuthoringLaunchTestCoordinate),
	}
	definitions := standardAuthoringLaunchTestDefinitionProvider(t)
	services, err := NewLifecycleServicesWithOptions(root, database, standardAuthoringLaunchTestOptions(capturer, definitions, definitions.catalog))
	if err != nil {
		t.Fatal(err)
	}
	return startAuthoringRecoveryLaunchFixture(t, ctx, root, database, services, definitions.catalog)
}

func newLockedAuthoringRecoveryLaunchFixture(t *testing.T) authoringRecoveryFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	capturer := &standardAuthoringSourceCapturerFixture{
		coordinate: standardAuthoringLaunchTestCoordinate,
		snapshot:   standardAuthoringLaunchTestSnapshot(t, standardAuthoringLaunchTestCoordinate),
	}
	definitions := standardAuthoringLaunchTestDefinitionProvider(t)
	resolver := &authoringRecoveryCatalogLockResolver{
		DeploymentOperationCatalogResolver: definitions.catalog,
		identity:                           authoringRecoveryCatalogLockIdentity("v1"),
	}
	services, err := NewLifecycleServicesWithOptions(root, database, LifecycleServicesOptions{
		OperationResolver: resolver,
		DeploymentCatalogResolvers: []TemplateDeploymentCatalogResolver{{
			Template: workflowadapter.StandardAuthoringTemplateReference(), Resolver: resolver,
		}},
		RequireDeploymentCatalog:               true,
		RequireDeploymentLock:                  true,
		StandardAuthoringSourceCapturer:        capturer,
		StandardAuthoringRunDefinitionProvider: definitions,
	})
	if err != nil {
		t.Fatal(err)
	}
	return startAuthoringRecoveryLaunchFixture(t, ctx, root, database, services, definitions.catalog)
}

func startAuthoringRecoveryLaunchFixture(t *testing.T, ctx context.Context, root string, database *store.Store, services *LifecycleServices, catalog *stageprovider.DeploymentOperationCatalogResolver) authoringRecoveryFixture {
	t.Helper()
	receipt, err := services.AuthoringLaunches.Start(ctx, StandardAuthoringLaunchCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{IdempotencyKey: authoringRecoveryUUID(t), Actor: "author", Reason: "create authoring recovery fixture"},
		RepositoryURL:                standardAuthoringLaunchTestCoordinate.RepositoryURL, CommitSHA: standardAuthoringLaunchTestCoordinate.CommitSHA,
		BaseImage: standardAuthoringLaunchTestBaseImage,
		Slug:      "authoring-recovery-fixture", Title: "Authoring recovery fixture", MetadataJSON: `{}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.GetWorkflowRun(ctx, receipt.RunID)
	if err != nil || run == nil {
		t.Fatalf("load authoring fixture Run = %+v, %v", run, err)
	}
	workflow, err := decodeFrozenWorkflow(*run)
	if err != nil {
		t.Fatal(err)
	}
	source, err := database.GetAuthoringSource(ctx, receipt.AuthoringSourceID)
	if err != nil || source == nil {
		t.Fatalf("load authoring fixture source = %+v, %v", source, err)
	}
	session, err := database.GetAuthoringSession(ctx, receipt.AuthoringSessionID)
	if err != nil || session == nil {
		t.Fatalf("load authoring fixture session = %+v, %v", session, err)
	}
	task, err := database.GetTaskV2(ctx, receipt.TaskID)
	if err != nil || task == nil {
		t.Fatalf("load authoring fixture task = %+v, %v", task, err)
	}
	return authoringRecoveryFixture{root: root, services: services, store: database, catalog: catalog, source: *source, session: *session, task: *task, run: *run, workflow: workflow}
}

type authoringRecoveryCatalogLockResolver struct {
	*stageprovider.DeploymentOperationCatalogResolver
	identity stageprovider.DeploymentOperationCatalogLockIdentity
}

func (resolver *authoringRecoveryCatalogLockResolver) LockIdentity() stageprovider.DeploymentOperationCatalogLockIdentity {
	return resolver.identity
}

func (resolver *authoringRecoveryCatalogLockResolver) VerifyLockIdentity(identity stageprovider.DeploymentOperationCatalogLockIdentity) error {
	if identity != resolver.identity {
		return stageprovider.ErrDeploymentOperationCatalogLockDrift
	}
	return nil
}

func authoringRecoveryCatalogLockIdentity(version string) stageprovider.DeploymentOperationCatalogLockIdentity {
	return stageprovider.DeploymentOperationCatalogLockIdentity{
		LockID: "authoring-recovery-test-lock", LockVersion: version,
		Fingerprint: workflowkit.SHA256Fingerprint([]byte("authoring-recovery-test-lock:" + version)),
	}
}

func seedAuthoringRecoveryRepoPrepare(t *testing.T, ctx context.Context, fixture authoringRecoveryFixture) {
	t.Helper()
	stage, found := fixture.workflow.Stage(workflowkit.NodeID(workflowadapter.RepoPrepare))
	if !found || len(stage.Outputs) != 1 {
		t.Fatalf("repo_prepare descriptor = %+v", stage)
	}
	emptyInputs, err := workflowkit.FingerprintArtifactBindings(nil)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := fixture.store.CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: fixture.run.ID, StageKey: string(stage.Key), StageGroup: stage.Group, Ordinal: 1, InputFingerprint: string(emptyInputs),
		BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`, Actor: "worker", Reason: "seed reusable authoring source stage",
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err = fixture.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: attempt.ID, ExpectedVersion: attempt.Version, ExecutionStatus: store.StageExecutionRunning, Actor: "worker", Reason: "run reusable authoring source stage",
	})
	if err != nil {
		t.Fatal(err)
	}
	node, err := fixture.store.CreateNodeAttempt(ctx, store.CreateNodeAttemptRequest{
		StageAttemptID: attempt.ID, NodeID: string(stage.Key), Generation: 0, Attempt: 1, IdempotencyKey: authoringRecoveryUUID(t), Actor: "worker", Reason: "seed authoring source node",
	})
	if err != nil {
		t.Fatal(err)
	}
	subject, err := fixture.services.core.resolveWorkflowRunSubject(ctx, fixture.run)
	if err != nil {
		t.Fatal(err)
	}
	manifest, _, err := persistStageArtifactsForSubject(ctx, fixture.services.core, fixture.run, subject, attempt, node, stage, nil, []StageArtifact{{
		ID: authoringRecoveryUUID(t), Key: stage.Outputs[0].Name, SchemaVersion: stage.Outputs[0].SchemaVersion, Content: []byte("verified authoring repo preparation"), TurnOrdinal: 1,
	}}, "worker", "persist reusable authoring source artifact")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: attempt.ID, ExpectedVersion: attempt.Version, ExecutionStatus: store.StageExecutionCompleted, Verdict: store.VerdictPass,
		ArtifactManifestID: manifest.ID, Actor: "worker", Reason: "complete reusable authoring source stage",
	}); err != nil {
		t.Fatal(err)
	}
}

func assertAuthoringRecoveryPlan(t *testing.T, plan workflowkit.ContinuationPlan, fixture authoringRecoveryFixture) {
	t.Helper()
	snapshot := plan.Snapshot()
	if snapshot.Strategy != workflowkit.StrategyRetryAttempt || snapshot.SourceRunID != fixture.run.ID ||
		snapshot.SubjectRevisionID != fixture.session.ID || snapshot.SubjectDigest != workflowkit.SubjectDigest(fixture.source.SnapshotContentDigest) ||
		snapshot.BaseCheckpoint.SubjectID != fixture.source.ID || snapshot.BaseCheckpoint.SubjectVersion != store.AuthoringSessionControlSubjectVersion {
		t.Fatalf("authoring recovery plan binding = %+v", snapshot)
	}
	transitions := make(map[workflowkit.NodeID]workflowkit.NodeTransition, len(snapshot.Nodes))
	for _, transition := range snapshot.Nodes {
		transitions[transition.NodeID] = transition
	}
	for _, nodeID := range []workflowkit.NodeID{workflowkit.NodeID(workflowadapter.RepoAnalyze), workflowkit.NodeID(workflowadapter.TaskDesign)} {
		transition, found := transitions[nodeID]
		if !found || transition.Disposition != workflowkit.DispositionSchedule {
			t.Fatalf("authoring recovery transition %s = %+v, found=%t", nodeID, transition, found)
		}
	}
}

func transitionAuthoringRecoveryRun(t *testing.T, ctx context.Context, dataStore *store.Store, run store.WorkflowRun, target store.WorkflowRunStatus) store.WorkflowRun {
	t.Helper()
	if run.Status == target {
		return run
	}
	updated, err := dataStore.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: target, Actor: "worker", Reason: "authoring recovery fixture transition",
	})
	if err != nil {
		t.Fatalf("transition authoring fixture Run %s from %s to %s: %v", run.ID, run.Status, target, err)
	}
	return updated
}

type authoringRecoverySubjectObserverFunc func(context.Context, store.WorkflowRun, workflowRunSubject, workflowkit.WorkflowDescriptor) (continuationRunState, error)

func (observer authoringRecoverySubjectObserverFunc) ObserveSubject(ctx context.Context, run store.WorkflowRun, subject workflowRunSubject, workflow workflowkit.WorkflowDescriptor) (continuationRunState, error) {
	return observer(ctx, run, subject, workflow)
}

func snapshotAuthoringRecoveryManagedFiles(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files := make(map[string][]byte)
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() || path == filepath.Join(root, "harbor.db-wal") || path == filepath.Join(root, "harbor.db-shm") {
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

func authoringRecoveryUUID(t *testing.T) string {
	t.Helper()
	id, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
