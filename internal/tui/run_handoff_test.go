package tui

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

type recordingTaskHubHandoffLifecycle struct {
	*fakeTaskHubLifecycle

	mu        sync.Mutex
	requests  []TaskHubRunHandoffRequest
	failFirst bool
}

func (service *recordingTaskHubHandoffLifecycle) ExecuteTaskHubRunHandoff(_ context.Context, request TaskHubRunHandoffRequest) (TaskHubRunHandoffResult, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.requests = append(service.requests, request)
	if service.failFirst && len(service.requests) == 1 {
		return TaskHubRunHandoffResult{}, errors.New("simulated lost Task Hub handoff response")
	}
	return TaskHubRunHandoffResult{
		RunID:       request.RunID,
		OperationID: request.HandoffOperationID,
		State:       "launching",
		Summary:     "controlled child worker started",
	}, nil
}

func (service *recordingTaskHubHandoffLifecycle) handoffRequests() []TaskHubRunHandoffRequest {
	service.mu.Lock()
	defer service.mu.Unlock()
	return append([]TaskHubRunHandoffRequest(nil), service.requests...)
}

func TestTaskHubExitHandoffDefaultsAllActiveRunsAndAllowsIndividualDeselection(t *testing.T) {
	service := &recordingTaskHubHandoffLifecycle{fakeTaskHubLifecycle: &fakeTaskHubLifecycle{snapshot: taskHubExitHandoffSnapshot(t, 2)}}
	ctx := context.Background()
	canceled := false
	m := initialLifecycleHubModel(ctx, func() { canceled = true }, service)
	m.width, m.height = 86, 28
	loaded := m.loadTaskHubV2()().(taskHubLoadedMsg)
	if loaded.err != nil {
		t.Fatal(loaded.err)
	}
	m.applyTaskHubSnapshot(loaded.snapshot)

	updated, command := m.Update(runeKey("q"))
	m = updated.(model)
	if command != nil || m.exitHandoff == nil || len(m.exitHandoff.Items) != 2 {
		t.Fatalf("q did not open two-Run exit handoff panel: command=%v overlay=%+v", command, m.exitHandoff)
	}
	for _, item := range m.exitHandoff.Items {
		if !item.Selected || item.State != taskHubRunHandoffReady {
			t.Fatalf("active Run did not default to selected ready handoff: %+v", item)
		}
		if err := store.ValidateUUIDv7(item.Request.HandoffOperationID); err != nil {
			t.Fatalf("handoff operation ID is not UUIDv7: %v", err)
		}
		if err := store.ValidateUUIDv7(item.Request.IdempotencyKey); err != nil {
			t.Fatalf("handoff idempotency key is not UUIDv7: %v", err)
		}
	}
	if rendered := ansi.Strip(m.View()); !containsAll(rendered, "退出前交接", "[x]", "[Space] 勾选/取消") {
		t.Fatalf("exit handoff panel did not render decisions:\n%s", rendered)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = updated.(model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeySpace})
	m = updated.(model)
	if !m.exitHandoff.Items[0].Selected || m.exitHandoff.Items[1].Selected {
		t.Fatalf("per-Run deselection changed the wrong decisions: %+v", m.exitHandoff.Items)
	}

	updated, command = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if command == nil {
		t.Fatal("selected exit handoff did not return an asynchronous command")
	}
	updated, quitCommand := m.Update(command())
	m = updated.(model)
	if quitCommand == nil {
		t.Fatal("completed selected handoff did not request exit")
	}
	if _, okay := quitCommand().(tea.QuitMsg); !okay {
		t.Fatalf("completed selected handoff command = %T, want tea.QuitMsg", quitCommand())
	}
	requests := service.handoffRequests()
	if len(requests) != 1 || requests[0].RunID != m.exitHandoff.Items[0].Run.RunID {
		t.Fatalf("individual selection did not isolate its handoff request: %+v", requests)
	}
	if canceled {
		t.Fatal("TUI exit handoff invoked the shared root cancel callback")
	}
}

func TestTaskHubExitHandoffAllDeselectedExitsWithoutHandoff(t *testing.T) {
	service := &recordingTaskHubHandoffLifecycle{fakeTaskHubLifecycle: &fakeTaskHubLifecycle{snapshot: taskHubExitHandoffSnapshot(t, 2)}}
	m, cleanup := newTestTaskHubV2Model(t, service)
	defer cleanup()

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	m = updated.(model)
	if m.exitHandoff == nil || m.exitHandoff.selectedCount() != 2 {
		t.Fatalf("Ctrl+C did not default all active Runs to selected: %+v", m.exitHandoff)
	}
	for index := range m.exitHandoff.Items {
		m.exitHandoff.Selected = index
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeySpace})
		m = updated.(model)
	}
	if got := m.exitHandoff.selectedCount(); got != 0 {
		t.Fatalf("all active Runs were not deselected: %d", got)
	}
	updated, command := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if command == nil {
		t.Fatal("zero-selected handoff did not return direct exit command")
	}
	if _, okay := command().(tea.QuitMsg); !okay {
		t.Fatalf("zero-selected handoff command = %T, want tea.QuitMsg", command())
	}
	if requests := service.handoffRequests(); len(requests) != 0 {
		t.Fatalf("zero-selected exit launched child workers: %+v", requests)
	}
}

func TestTaskHubExitHandoffRetainsFrozenIdsAcrossLostReply(t *testing.T) {
	service := &recordingTaskHubHandoffLifecycle{
		fakeTaskHubLifecycle: &fakeTaskHubLifecycle{snapshot: taskHubExitHandoffSnapshot(t, 1)},
		failFirst:            true,
	}
	m, cleanup := newTestTaskHubV2Model(t, service)
	defer cleanup()

	updated, _ := m.Update(runeKey("q"))
	m = updated.(model)
	if m.exitHandoff == nil {
		t.Fatal("exit handoff panel did not open")
	}
	frozen := m.exitHandoff.Items[0].Request
	updated, command := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if command == nil {
		t.Fatal("first handoff did not create command")
	}
	updated, _ = m.Update(command())
	m = updated.(model)
	if m.exitHandoff == nil || m.exitHandoff.Items[0].State != taskHubRunHandoffFailed {
		t.Fatalf("lost reply did not leave retryable handoff panel: %+v", m.exitHandoff)
	}
	updated, command = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if command == nil {
		t.Fatal("retry did not create handoff command")
	}
	updated, quitCommand := m.Update(command())
	m = updated.(model)
	if quitCommand == nil {
		t.Fatal("idempotent retry did not complete exit")
	}
	if _, okay := quitCommand().(tea.QuitMsg); !okay {
		t.Fatalf("retry exit command = %T, want tea.QuitMsg", quitCommand())
	}
	requests := service.handoffRequests()
	if len(requests) != 2 || requests[0].HandoffOperationID != frozen.HandoffOperationID || requests[1].HandoffOperationID != frozen.HandoffOperationID ||
		requests[0].IdempotencyKey != frozen.IdempotencyKey || requests[1].IdempotencyKey != frozen.IdempotencyKey {
		t.Fatalf("lost reply retry changed frozen handoff identity: %+v", requests)
	}
}

func TestTaskHubExitHandoffPanelFitsNarrowTerminal(t *testing.T) {
	service := &recordingTaskHubHandoffLifecycle{fakeTaskHubLifecycle: &fakeTaskHubLifecycle{snapshot: taskHubExitHandoffSnapshot(t, 3)}}
	m, cleanup := newTestTaskHubV2Model(t, service)
	defer cleanup()
	m.width, m.height = 34, 14

	updated, _ := m.Update(runeKey("q"))
	m = updated.(model)
	if m.exitHandoff == nil {
		t.Fatal("exit handoff panel did not open")
	}
	assertRenderedWidth(t, "exit handoff overlay", ansi.Strip(m.exitHandoff.View(m.width, m.height)), m.width)
}

func taskHubExitHandoffSnapshot(t *testing.T, count int) TaskHubSnapshot {
	t.Helper()
	snapshot := TaskHubSnapshot{}
	for index := 0; index < count; index++ {
		taskID, err := store.NewUUIDv7()
		if err != nil {
			t.Fatal(err)
		}
		runID, err := store.NewUUIDv7()
		if err != nil {
			t.Fatal(err)
		}
		revisionID, err := store.NewUUIDv7()
		if err != nil {
			t.Fatal(err)
		}
		snapshot.Tasks = append(snapshot.Tasks, TaskHubTask{TaskID: taskID, Name: "handoff-task"})
		snapshot.Runs = append(snapshot.Runs, TaskHubRun{
			RunID:          runID,
			TaskID:         taskID,
			RevisionID:     revisionID,
			ExecutionState: "running",
			Active:         true,
			Handoff: TaskHubRunHandoffCapability{
				Enabled: true,
				Expected: TaskHubRunHandoffCheckpoint{
					RunVersion:     1,
					ExecutionEpoch: index,
					DefinitionHash: "sha256:handoff-test-definition-" + string(rune('a'+index)),
				},
			},
		})
	}
	return snapshot
}

func containsAll(text string, values ...string) bool {
	for _, value := range values {
		if !strings.Contains(text, value) {
			return false
		}
	}
	return true
}
