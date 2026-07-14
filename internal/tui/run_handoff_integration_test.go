package tui

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

type recordingTaskHubRunWorkerLauncher struct {
	mu       sync.Mutex
	requests []app.RunWorkerHandoffLaunchRequest
	err      error
}

func (launcher *recordingTaskHubRunWorkerLauncher) LaunchRunWorker(_ context.Context, request app.RunWorkerHandoffLaunchRequest) (app.RunWorkerHandoffLaunchReceipt, error) {
	launcher.mu.Lock()
	defer launcher.mu.Unlock()
	launcher.requests = append(launcher.requests, request)
	if launcher.err != nil {
		return app.RunWorkerHandoffLaunchReceipt{}, launcher.err
	}
	return app.RunWorkerHandoffLaunchReceipt{
		RunID:     request.RunID,
		Owner:     request.Owner,
		ProcessID: 7000 + len(launcher.requests),
		LogPath:   filepath.Join("/tmp", "task-hub-worker-"+request.HandoffOperationID+".log"),
	}, nil
}

func (launcher *recordingTaskHubRunWorkerLauncher) launchRequests() []app.RunWorkerHandoffLaunchRequest {
	launcher.mu.Lock()
	defer launcher.mu.Unlock()
	return append([]app.RunWorkerHandoffLaunchRequest(nil), launcher.requests...)
}

type lostReplyTaskHubHandoffAdapter struct {
	*AppTaskHubLifecycleAdapter
	failFirst bool
}

func (adapter *lostReplyTaskHubHandoffAdapter) ExecuteTaskHubRunHandoff(ctx context.Context, request TaskHubRunHandoffRequest) (TaskHubRunHandoffResult, error) {
	result, err := adapter.AppTaskHubLifecycleAdapter.ExecuteTaskHubRunHandoff(ctx, request)
	if err != nil {
		return result, err
	}
	if adapter.failFirst {
		adapter.failFirst = false
		return result, errors.New("simulated response loss after durable handoff receipt")
	}
	return result, nil
}

func TestTaskHubExitHandoffSQLiteHonorsPerRunSelectionAndLaunchesThroughApplicationService(t *testing.T) {
	ctx, root, services, runs := newTaskHubExitHandoffFixture(t, 2)
	launcher := &recordingTaskHubRunWorkerLauncher{}
	adapter := NewAppTaskHubLifecycleAdapterWithRunWorkerHandoffLauncher(services, launcher)
	canceled := false
	m := newTaskHubExitHandoffModel(t, ctx, func() { canceled = true }, adapter)

	updated, _ := m.Update(runeKey("q"))
	m = updated.(model)
	if m.exitHandoff == nil || len(m.exitHandoff.Items) != 2 || m.exitHandoff.selectedCount() != 2 {
		t.Fatalf("exit handoff did not enumerate and default-select active Runs: %+v", m.exitHandoff)
	}

	deselected := runs[0].ID
	selected := runs[1].ID
	selectTaskHubExitHandoffItem(t, &m, deselected)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeySpace})
	m = updated.(model)
	if item := taskHubExitHandoffItemForRun(m.exitHandoff, deselected); item.Selected {
		t.Fatalf("selected Run %s was not deselected: %+v", deselected, item)
	}
	selectedItem := taskHubExitHandoffItemForRun(m.exitHandoff, selected)
	if !selectedItem.Selected {
		t.Fatalf("other active Run was changed while deselecting one: %+v", selectedItem)
	}

	updated, command := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if command == nil {
		t.Fatal("selected Run handoff did not create deferred command")
	}
	updated, quitCommand := m.Update(command())
	m = updated.(model)
	if quitCommand == nil {
		t.Fatal("completed selected Run handoff did not exit")
	}
	if _, okay := quitCommand().(tea.QuitMsg); !okay {
		t.Fatalf("handoff completion command = %T, want tea.QuitMsg", quitCommand())
	}
	if canceled {
		t.Fatal("TUI exit handoff canceled the shared root context")
	}

	deselectedHandoffs, err := services.Store().ListRunWorkerHandoffsForRun(ctx, deselected)
	if err != nil {
		t.Fatal(err)
	}
	if len(deselectedHandoffs) != 0 {
		t.Fatalf("deselected Run received durable handoff: %+v", deselectedHandoffs)
	}
	handoffs, err := services.Store().ListRunWorkerHandoffsForRun(ctx, selected)
	if err != nil {
		t.Fatal(err)
	}
	if len(handoffs) != 1 {
		t.Fatalf("selected Run durable handoffs = %+v, want exactly one", handoffs)
	}
	stored := handoffs[0]
	if stored.ID != selectedItem.Request.HandoffOperationID || stored.IdempotencyKey != selectedItem.Request.IdempotencyKey ||
		stored.ExpectedRunVersion != selectedItem.Request.Expected.RunVersion || stored.ExpectedRunExecutionEpoch != selectedItem.Request.Expected.ExecutionEpoch ||
		stored.ExpectedRunDefinitionHash != selectedItem.Request.Expected.DefinitionHash || stored.Owner != selectedItem.Request.Owner ||
		stored.State != store.RunWorkerHandoffLaunching {
		t.Fatalf("durable handoff did not preserve frozen TUI command: %+v request=%+v", stored, selectedItem.Request)
	}
	launches := launcher.launchRequests()
	if len(launches) != 1 || launches[0].ManagedRoot != root || launches[0].RunID != selected || launches[0].Owner != selectedItem.Request.Owner ||
		launches[0].HandoffOperationID != selectedItem.Request.HandoffOperationID {
		t.Fatalf("application launcher request = %+v, want selected frozen Run handoff", launches)
	}
}

func TestAppTaskHubExitHandoffRequiresCompositionSuppliedLauncher(t *testing.T) {
	ctx, _, services, runs := newTaskHubExitHandoffFixture(t, 1)
	adapter := NewAppTaskHubLifecycleAdapter(services)
	snapshot, err := adapter.QueryTaskHub(ctx, TaskHubQuery{Tab: TaskHubRunsTab})
	if err != nil || len(snapshot.Runs) != 1 {
		t.Fatalf("query handoff capability without launcher = %+v, %v", snapshot, err)
	}
	capability := snapshot.Runs[0].Handoff
	if capability.Enabled || capability.DisabledReason == "" {
		t.Fatalf("uncomposed Task Hub exposed a launchable worker handoff: %+v", capability)
	}
	operationID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	key, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	_, err = adapter.ExecuteTaskHubRunHandoff(ctx, TaskHubRunHandoffRequest{
		RunID: runs[0].ID, Expected: capability.Expected, HandoffOperationID: operationID, IdempotencyKey: key,
		Owner: "fixture-owner", Actor: "tester", Reason: "verify missing composition launcher",
	})
	if err == nil {
		t.Fatal("handoff without composition launcher unexpectedly succeeded")
	}
	handoffs, listErr := services.Store().ListRunWorkerHandoffsForRun(ctx, runs[0].ID)
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(handoffs) != 0 {
		t.Fatalf("handoff without launcher wrote durable operation: %+v", handoffs)
	}
}

func TestTaskHubExitHandoffSQLiteRejectsStaleCheckpointBeforeLaunching(t *testing.T) {
	ctx, _, services, runs := newTaskHubExitHandoffFixture(t, 1)
	launcher := &recordingTaskHubRunWorkerLauncher{}
	adapter := NewAppTaskHubLifecycleAdapterWithRunWorkerHandoffLauncher(services, launcher)
	m := newTaskHubExitHandoffModel(t, ctx, func() {}, adapter)

	updated, _ := m.Update(runeKey("q"))
	m = updated.(model)
	if m.exitHandoff == nil {
		t.Fatal("exit handoff panel did not open")
	}
	if _, err := services.Store().TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: runs[0].ID, ExpectedVersion: runs[0].Version, Status: store.WorkflowRunRunning,
		Actor: "tester", Reason: "invalidate frozen Task Hub exit handoff checkpoint",
	}); err != nil {
		t.Fatal(err)
	}

	updated, command := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if command == nil {
		t.Fatal("stale handoff did not create deferred command")
	}
	updated, _ = m.Update(command())
	m = updated.(model)
	if !errors.Is(m.err, store.ErrOptimisticLock) || m.exitHandoff == nil || m.exitHandoff.Items[0].State != taskHubRunHandoffFailed {
		t.Fatalf("stale handoff did not remain retryable with CAS error: err=%v overlay=%+v", m.err, m.exitHandoff)
	}
	if launches := launcher.launchRequests(); len(launches) != 0 {
		t.Fatalf("stale checkpoint launched child worker: %+v", launches)
	}
	handoffs, err := services.Store().ListRunWorkerHandoffsForRun(ctx, runs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(handoffs) != 0 {
		t.Fatalf("stale checkpoint created durable handoff: %+v", handoffs)
	}
}

func TestTaskHubExitHandoffSQLiteReplaysLostReplyWithoutSecondChildLaunch(t *testing.T) {
	ctx, _, services, runs := newTaskHubExitHandoffFixture(t, 1)
	launcher := &recordingTaskHubRunWorkerLauncher{}
	adapter := &lostReplyTaskHubHandoffAdapter{
		AppTaskHubLifecycleAdapter: NewAppTaskHubLifecycleAdapterWithRunWorkerHandoffLauncher(services, launcher),
		failFirst:                  true,
	}
	m := newTaskHubExitHandoffModel(t, ctx, func() {}, adapter)

	updated, _ := m.Update(runeKey("q"))
	m = updated.(model)
	frozen := m.exitHandoff.Items[0].Request
	updated, command := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if command == nil {
		t.Fatal("first handoff did not create deferred command")
	}
	updated, _ = m.Update(command())
	m = updated.(model)
	if m.exitHandoff == nil || m.exitHandoff.Items[0].State != taskHubRunHandoffFailed {
		t.Fatalf("lost response did not retain retryable handoff: %+v", m.exitHandoff)
	}

	updated, command = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if command == nil {
		t.Fatal("lost-reply retry did not create deferred command")
	}
	updated, quitCommand := m.Update(command())
	m = updated.(model)
	if quitCommand == nil {
		t.Fatal("replayed durable handoff did not exit")
	}
	if _, okay := quitCommand().(tea.QuitMsg); !okay {
		t.Fatalf("replayed handoff completion command = %T, want tea.QuitMsg", quitCommand())
	}
	if frozen != m.exitHandoff.Items[0].Request {
		t.Fatalf("lost-reply retry changed frozen handoff request: before=%+v after=%+v", frozen, m.exitHandoff.Items[0].Request)
	}
	if launches := launcher.launchRequests(); len(launches) != 1 {
		t.Fatalf("lost-reply retry launched more than one child: %+v", launches)
	}
	handoffs, err := services.Store().ListRunWorkerHandoffsForRun(ctx, runs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(handoffs) != 1 || handoffs[0].ID != frozen.HandoffOperationID || handoffs[0].IdempotencyKey != frozen.IdempotencyKey {
		t.Fatalf("lost-reply retry did not replay one durable handoff: %+v", handoffs)
	}
}

func newTaskHubExitHandoffFixture(t *testing.T, count int) (context.Context, string, *app.LifecycleServices, []store.WorkflowRun) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	dataStore, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	services, err := newTaskHubAdapterLifecycleServices(root, dataStore)
	if err != nil {
		t.Fatal(err)
	}
	task, revision, err := services.Tasks.ImportTask(ctx, app.ImportTaskRequest{
		CreateDraftTaskRequest: app.CreateDraftTaskRequest{Slug: "tui-exit-handoff", Title: "TUI Exit Handoff", Actor: "tester", Reason: "create Task Hub handoff fixture"},
		SourceDirectory:        taskHubAdapterSnapshot(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	runs := make([]store.WorkflowRun, 0, count)
	for index := 0; index < count; index++ {
		run, startErr := services.Runs.StartRun(ctx, app.StartRunRequest{
			TaskID: task.ID, RevisionID: revision.ID,
			Profile: taskHubAdapterCompleteProfile(t), ExecutionSpec: taskHubExecutionSpec(task.ID, revision.ID, revision.TaskDigest),
			Trigger: "tui-exit-handoff", Actor: "tester", Reason: "start Task Hub exit handoff fixture",
		})
		if startErr != nil {
			t.Fatal(startErr)
		}
		runs = append(runs, run)
	}
	return ctx, root, services, runs
}

func newTaskHubExitHandoffModel(t *testing.T, ctx context.Context, cancel context.CancelFunc, lifecycle TaskHubLifecycleService) model {
	t.Helper()
	m := initialLifecycleHubModel(ctx, cancel, lifecycle)
	m.width, m.height = 100, 32
	loaded := m.loadTaskHubV2()().(taskHubLoadedMsg)
	if loaded.err != nil {
		t.Fatal(loaded.err)
	}
	m.applyTaskHubSnapshot(loaded.snapshot)
	return m
}

func selectTaskHubExitHandoffItem(t *testing.T, model *model, runID string) {
	t.Helper()
	if model == nil || model.exitHandoff == nil {
		t.Fatal("exit handoff panel is unavailable")
	}
	for index, item := range model.exitHandoff.Items {
		if item.Run.RunID == runID {
			model.exitHandoff.Selected = index
			return
		}
	}
	t.Fatalf("exit handoff panel does not list Run %s: %+v", runID, model.exitHandoff.Items)
}

func taskHubExitHandoffItemForRun(overlay *taskHubExitHandoffOverlay, runID string) taskHubExitHandoffItem {
	if overlay == nil {
		return taskHubExitHandoffItem{}
	}
	for _, item := range overlay.Items {
		if item.Run.RunID == runID {
			return item
		}
	}
	return taskHubExitHandoffItem{}
}
