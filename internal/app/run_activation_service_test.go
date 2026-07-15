package app

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

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
