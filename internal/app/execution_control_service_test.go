package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
)

func TestExecutionControlServicePersistsTargetedControlWithCAS(t *testing.T) {
	ctx := context.Background()
	services, task, revision := newControlLifecycleFixture(t, "control-owner")
	run, err := services.Runs.StartRun(ctx, StartRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, Profile: lifecycleCompleteProfile(t), ExecutionSpec: lifecycleExecutionSpec(task.ID, revision.ID, revision.TaskDigest), Trigger: "verify",
		Actor: "control-owner", Reason: "start control fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err = services.Store().TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunRunning,
		Actor: "control-owner", Reason: "worker started",
	})
	if err != nil {
		t.Fatal(err)
	}

	checkpoint, err := services.Control.CurrentCheckpoint(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.Sequence != uint64(run.Version) || checkpoint.SubjectID != task.ID || checkpoint.SubjectRevisionID != revision.ID {
		t.Fatalf("control checkpoint = %+v, want authoritative run/task/revision identity", checkpoint)
	}

	pause, err := services.Control.Request(ctx, RequestExecutionControlRequest{
		OperationKey: "pause-control-fixture", Action: store.ControlActionPause, RunID: run.ID,
		Expected: checkpoint, Actor: "control-owner", Reason: "pause before maintenance",
	})
	if err != nil {
		t.Fatal(err)
	}
	if pause.Status != store.ControlOperationRequested || pause.Action != store.ControlActionPause || pause.Expected != checkpoint || pause.GracePeriod != 30*time.Second {
		t.Fatalf("pause operation = %+v", pause)
	}
	updatedRun, err := services.Runs.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updatedRun.Status != store.WorkflowRunPauseRequested || updatedRun.Version != run.Version+1 {
		t.Fatalf("pause request did not atomically update run: %+v", updatedRun)
	}

	replayedPause, err := services.Control.Request(ctx, RequestExecutionControlRequest{
		OperationKey: "pause-control-fixture", Action: store.ControlActionPause, RunID: run.ID,
		Expected: checkpoint, Actor: "control-owner", Reason: "pause before maintenance",
	})
	if err != nil {
		t.Fatalf("replay pause operation: %v", err)
	}
	if replayedPause.ID != pause.ID || replayedPause.Version != pause.Version {
		t.Fatalf("replayed pause = %+v, want %+v", replayedPause, pause)
	}
	if _, err := services.Control.Request(ctx, RequestExecutionControlRequest{
		OperationKey: "stale-control-fixture", Action: store.ControlActionTerminate, RunID: run.ID,
		Expected: checkpoint, Actor: "control-owner", Reason: "late terminate request",
	}); !errors.Is(err, store.ErrOptimisticLock) {
		t.Fatalf("stale control checkpoint error = %v, want %v", err, store.ErrOptimisticLock)
	}

	propagationID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	pause, err = services.Control.Transition(ctx, TransitionExecutionControlRequest{
		ID: propagationID, OperationID: pause.ID, ExpectedVersion: pause.Version,
		Status: store.ControlOperationPropagating, Actor: "control-owner", Reason: "worker received pause",
	})
	if err != nil {
		t.Fatal(err)
	}
	if pause.Status != store.ControlOperationPropagating {
		t.Fatalf("propagated pause = %+v", pause)
	}
	ackID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := services.Control.Transition(ctx, TransitionExecutionControlRequest{
		ID: ackID, OperationID: pause.ID, ExpectedVersion: pause.Version - 1,
		Status: store.ControlOperationAcknowledged, Actor: "control-owner", Reason: "late acknowledgement",
	}); !errors.Is(err, store.ErrOptimisticLock) {
		t.Fatalf("stale control transition error = %v, want %v", err, store.ErrOptimisticLock)
	}
	pause, err = services.Control.Transition(ctx, TransitionExecutionControlRequest{
		ID: ackID, OperationID: pause.ID, ExpectedVersion: pause.Version,
		Status: store.ControlOperationAcknowledged, CheckpointID: "checkpoint-pause", QuotaSettlementID: "quota-settlement-pause",
		Actor: "control-owner", Reason: "pause checkpoint persisted",
	})
	if err != nil {
		t.Fatal(err)
	}
	if pause.Status != store.ControlOperationAcknowledged || pause.CheckpointID != "checkpoint-pause" {
		t.Fatalf("acknowledged pause = %+v", pause)
	}

	stage, err := services.Store().CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: run.ID, StageKey: workflowadapter.QualityCheck, StageGroup: "quality", Ordinal: 1,
		InputFingerprint: "sha256:control-stage-input", BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`,
		Actor: "control-owner", Reason: "control fixture stage",
	})
	if err != nil {
		t.Fatal(err)
	}
	current, err := services.Control.CurrentCheckpoint(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	cancelStage, err := services.Control.Request(ctx, RequestExecutionControlRequest{
		OperationKey: "cancel-stage-control-fixture", Action: store.ControlActionCancelStage, RunID: run.ID, StageAttemptID: stage.ID,
		Expected: current, Actor: "control-owner", Reason: "cancel selected stage",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cancelStage.StageAttemptID != stage.ID || cancelStage.Action != store.ControlActionCancelStage {
		t.Fatalf("stage cancel operation = %+v", cancelStage)
	}
	gate, err := services.Store().CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: run.ID, StageKey: workflowadapter.FinalReview, StageGroup: "review", Ordinal: 2,
		InputFingerprint: "sha256:control-gate-input", BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`,
		Actor: "control-owner", Reason: "control fixture review gate",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := services.Control.Request(ctx, RequestExecutionControlRequest{
		OperationKey: "cancel-gate-control-fixture", Action: store.ControlActionCancelStage, RunID: run.ID, StageAttemptID: gate.ID,
		Expected: current, Actor: "control-owner", Reason: "attempt unsupported gate cancellation",
	}); err == nil || !strings.Contains(err.Error(), "cancel capability") {
		t.Fatalf("unsupported stage cancellation error = %v, want frozen capability rejection", err)
	}

	terminate, err := services.Control.Request(ctx, RequestExecutionControlRequest{
		OperationKey: "terminate-control-fixture", Action: store.ControlActionTerminate, RunID: run.ID,
		Expected: current, Actor: "control-owner", Reason: "stop paused run",
	})
	if err != nil {
		t.Fatal(err)
	}
	if terminate.Action != store.ControlActionTerminate {
		t.Fatalf("terminate operation = %+v", terminate)
	}
	updatedRun, err = services.Runs.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updatedRun.Status != store.WorkflowRunCancelRequested {
		t.Fatalf("terminate request did not transition paused run: %+v", updatedRun)
	}
}

func newControlLifecycleFixture(t *testing.T, actor string) (*LifecycleServices, store.TaskV2, store.TaskRevision) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	services, err := newLifecycleServicesForTest(root, database)
	if err != nil {
		t.Fatal(err)
	}
	task, revision, err := services.Tasks.ImportTask(ctx, ImportTaskRequest{
		CreateDraftTaskRequest: CreateDraftTaskRequest{Slug: "control-fixture", Actor: actor, Reason: "create control fixture"},
		SourceDirectory:        writeLifecycleSnapshot(t, "control fixture\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return services, task, revision
}
