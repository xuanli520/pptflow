package app

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/testsupport"
)

type recordingRunActivationLauncher struct {
	mu       sync.Mutex
	requests []RunWorkerHandoffLaunchRequest
}

func (launcher *recordingRunActivationLauncher) LaunchRunWorker(_ context.Context, request RunWorkerHandoffLaunchRequest) (RunWorkerHandoffLaunchReceipt, error) {
	launcher.mu.Lock()
	defer launcher.mu.Unlock()
	launcher.requests = append(launcher.requests, request)
	return RunWorkerHandoffLaunchReceipt{
		RunID: request.RunID, Owner: request.Owner, ProcessID: 4300 + len(launcher.requests),
		LogPath: filepath.Join("/tmp", "run-activation-"+request.HandoffOperationID+".log"),
	}, nil
}

func (launcher *recordingRunActivationLauncher) snapshot() []RunWorkerHandoffLaunchRequest {
	launcher.mu.Lock()
	defer launcher.mu.Unlock()
	return append([]RunWorkerHandoffLaunchRequest(nil), launcher.requests...)
}

func TestRunActivationDrainsQueuedRunOutboxThroughOneControlledHandoff(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	launcher := &recordingRunActivationLauncher{}
	services, err := NewLifecycleServicesWithOptions(root, database, LifecycleServicesOptions{
		OperationResolver:        testsupport.AcceptAllStageOperationResolver(),
		RunWorkerHandoffLauncher: launcher,
	})
	if err != nil {
		t.Fatal(err)
	}
	task, revision, err := services.Tasks.ImportTask(ctx, ImportTaskRequest{
		CreateDraftTaskRequest: CreateDraftTaskRequest{Slug: "automatic-run-activation", Title: "Automatic Run Activation", Actor: "tester", Reason: "activation fixture"},
		SourceDirectory:        writeLifecycleSnapshot(t, "activate queued Run\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := services.Runs.StartRun(ctx, StartRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, Profile: lifecycleCompleteProfile(t),
		ExecutionSpec: lifecycleExecutionSpec(task.ID, revision.ID, revision.TaskDigest), Trigger: "activation-test", Actor: "tester", Reason: "queue activation fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := database.ListPendingOutboxEvents(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Topic != workflowRunQueuedOutboxTopic || pending[0].EntityID != run.ID {
		t.Fatalf("queued Run activation event = %+v", pending)
	}
	eventID := pending[0].ID

	if err := services.RunActivations.Drain(ctx); err != nil {
		t.Fatalf("drain queued Run activation: %v", err)
	}
	launches := launcher.snapshot()
	if len(launches) != 1 || launches[0].RunID != run.ID || launches[0].ManagedRoot != root || launches[0].Owner != "automatic-run-worker:"+run.ID {
		t.Fatalf("automatic controlled worker launch = %+v", launches)
	}
	handoffs, err := database.ListRunWorkerHandoffsForRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(handoffs) != 1 || handoffs[0].State != store.RunWorkerHandoffLaunching || handoffs[0].IdempotencyKey == run.ID {
		t.Fatalf("automatic durable handoff = %+v", handoffs)
	}
	event, err := database.GetOutboxEvent(ctx, eventID)
	if err != nil {
		t.Fatal(err)
	}
	if event == nil || event.State != store.OutboxPublished {
		t.Fatalf("activation event after drain = %+v", event)
	}

	if err := services.RunActivations.Drain(ctx); err != nil {
		t.Fatalf("repeat drain queued Run activation: %v", err)
	}
	if launches := launcher.snapshot(); len(launches) != 1 {
		t.Fatalf("repeat activation launched another worker: %+v", launches)
	}
}

func TestRunActivationSweepReplacesExpiredUnclaimedHandoff(t *testing.T) {
	ctx := context.Background()
	const launchTTL = 5 * time.Second
	root := t.TempDir()
	database, err := store.OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	launcher := &recordingRunActivationLauncher{}
	services, err := NewLifecycleServicesWithOptions(root, database, LifecycleServicesOptions{
		OperationResolver:        testsupport.AcceptAllStageOperationResolver(),
		RunWorkerHandoffLauncher: launcher,
	})
	if err != nil {
		t.Fatal(err)
	}
	task, revision, err := services.Tasks.ImportTask(ctx, ImportTaskRequest{
		CreateDraftTaskRequest: CreateDraftTaskRequest{Slug: "expired-run-activation", Title: "Expired Run Activation", Actor: "tester", Reason: "activation recovery fixture"},
		SourceDirectory:        writeLifecycleSnapshot(t, "recover expired handoff\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := services.Runs.StartRun(ctx, StartRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, Profile: lifecycleCompleteProfile(t),
		ExecutionSpec: lifecycleExecutionSpec(task.ID, revision.ID, revision.TaskDigest), Trigger: "activation-recovery-test", Actor: "tester", Reason: "queue activation recovery fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	key, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := services.WorkerHandoffs.ReserveRunWorkerHandoff(ctx, ReserveRunWorkerHandoffCommand{
		IdempotencyKey: key, RunID: run.ID,
		Expected: RunWorkerHandoffCheckpoint{RunVersion: run.Version, ExecutionEpoch: run.ExecutionEpoch, DefinitionHash: run.DefinitionHash},
		Owner:    "expired-activation-child", Actor: "tester", Reason: "simulate child spawned before claim", LaunchTTL: launchTTL,
	})
	if err != nil || !reserved.Launch {
		t.Fatalf("reserve unclaimed activation handoff = %+v, %v", reserved, err)
	}
	if _, err := services.WorkerHandoffs.RecordRunWorkerHandoffSpawned(ctx, reserved.Handoff.ID, 4242, filepath.Join(root, "runs", run.ID, "dead-worker.log"), "tester", "simulate child exited before claim"); err != nil {
		t.Fatal(err)
	}
	if err := services.RunActivations.Drain(ctx); err != nil {
		t.Fatalf("ack initial activation while child remains launchable: %v", err)
	}
	if launches := launcher.snapshot(); len(launches) != 0 {
		t.Fatalf("active handoff triggered duplicate launch: %+v", launches)
	}

	// Race instrumentation can make the initial durable-outbox drain exceed a
	// millisecond-scale lease. Use a realistic launch window, then wait until
	// this exact handoff is provably expired before exercising recovery.
	if wait := time.Until(reserved.Handoff.LaunchDeadlineAt.Add(50 * time.Millisecond)); wait > 0 {
		time.Sleep(wait)
	}
	if err := services.RunActivations.Drain(ctx); err != nil {
		t.Fatalf("sweep expired unclaimed handoff: %v", err)
	}
	launches := launcher.snapshot()
	if len(launches) != 1 || launches[0].RunID != run.ID {
		t.Fatalf("replacement worker launch = %+v", launches)
	}
	handoffs, err := database.ListRunWorkerHandoffsForRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(handoffs) != 2 || handoffs[0].ID != reserved.Handoff.ID || handoffs[0].State != store.RunWorkerHandoffExpired || handoffs[1].State != store.RunWorkerHandoffLaunching {
		t.Fatalf("expired/replacement handoffs = %+v", handoffs)
	}
	if err := services.RunActivations.Drain(ctx); err != nil {
		t.Fatalf("repeat expired handoff sweep: %v", err)
	}
	if launches := launcher.snapshot(); len(launches) != 1 {
		t.Fatalf("repeated sweep launched another replacement: %+v", launches)
	}
}

func TestRunActivationStartsWaitingReviewResolution(t *testing.T) {
	ctx := context.Background()
	fixture := newReviewGateRuntimeFixture(t)
	defer fixture.store.Close()
	launcher := &recordingRunActivationLauncher{}
	fixture.services.RunActivations = &RunActivationService{core: fixture.services.core, launcher: launcher}

	gateJob := queueRuntimeReviewGate(t, ctx, fixture)
	openRuntimeReviewGate(t, ctx, fixture, gateJob)
	binding := requireRuntimeReviewGateBinding(t, ctx, fixture.store, gateJob.StageAttemptID)
	decideRuntimeReviewGate(t, ctx, fixture, binding, store.ReviewDecisionApprove)

	if err := fixture.services.RunActivations.Drain(ctx); err != nil {
		t.Fatalf("activate waiting review resolution: %v", err)
	}
	launches := launcher.snapshot()
	if len(launches) != 1 || launches[0].RunID != fixture.run.ID {
		t.Fatalf("waiting review controlled launch = %+v", launches)
	}
}

func TestRunActivationStartsWaitingAuthoringReviewResolution(t *testing.T) {
	ctx := context.Background()
	services, database := newAuthoringReviewServiceFixture(t)
	defer database.Close()
	launcher := &recordingRunActivationLauncher{}
	services.RunActivations = &RunActivationService{core: services.core, launcher: launcher}
	opened := openAuthoringReviewServiceGate(t, ctx, database)
	checkpoint, err := services.AuthoringReviews.CaptureCheckpoint(ctx, opened.Request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := services.AuthoringReviews.Decide(ctx, DecideAuthoringReviewRequest{
		IdempotencyKey: newAuthoringReviewServiceUUID(t), Action: store.ReviewDecisionApprove,
		Actor: "operator", Reason: "approve source/session review", Expected: checkpoint,
	}); err != nil {
		t.Fatal(err)
	}

	if err := services.RunActivations.Drain(ctx); err != nil {
		t.Fatalf("activate waiting authoring review resolution: %v", err)
	}
	launches := launcher.snapshot()
	if len(launches) != 1 || launches[0].RunID != opened.Run.ID {
		t.Fatalf("waiting authoring review controlled launch = %+v", launches)
	}
}

func TestRunWorkerEligibilityRejectsStaleCommandsOutsideActiveRun(t *testing.T) {
	queued := func(command string) store.DurableJob {
		return store.DurableJob{State: store.JobQueued, CommandType: command}
	}
	for _, scenario := range []struct {
		name    string
		status  store.WorkflowRunStatus
		allowed string
		stale   string
	}{
		{name: "review", status: store.WorkflowRunWaitingReview, allowed: store.ReviewGateResolutionCommandType, stale: "stage_attempt.execute"},
		{name: "continuation", status: store.WorkflowRunWaitingContinuation, allowed: "task_continuation.execute", stale: "stage_attempt.execute"},
		{name: "in doubt", status: store.WorkflowRunInDoubt, allowed: codeEdgeEvaluatorReconciliationCommandType, stale: "stage_attempt.execute"},
		{name: "terminal repair", status: store.WorkflowRunSucceeded, allowed: repairSessionAdvanceCommandType, stale: "workflow_run.execute"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			if !runWorkerJobIsEligible(scenario.status, queued(scenario.allowed)) {
				t.Fatalf("%s command was not eligible", scenario.allowed)
			}
			if runWorkerJobIsEligible(scenario.status, queued(scenario.stale)) {
				t.Fatalf("stale command %s was eligible for %s", scenario.stale, scenario.status)
			}
		})
	}
}
