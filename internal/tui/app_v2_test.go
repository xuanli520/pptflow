package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/purplevoid/harbor-factory/internal/app"
)

type taskBoardGatewayStub struct {
	snapshot         app.TaskBoardSnapshot
	startRequests    []app.TaskBoardStartAuthoringRequest
	decisionRequests []app.TaskBoardDecideReviewRequest
	retryRequests    []app.TaskBoardRetryRunRequest
	cancelRequests   []app.TaskBoardCancelRunRequest
	log              app.TaskBoardLog
	startErr         error
	decisionErr      error
	retryErr         error
	cancelErr        error
	logErr           error
	flushErr         error
	listCalls        int
	flushCalls       int
	keys             int
}

func (stub *taskBoardGatewayStub) NewIdempotencyKey() (string, error) {
	stub.keys++
	return "019f65fb-7270-74f8-8a04-1a50c12c7cae", nil
}

func (stub *taskBoardGatewayStub) List(context.Context) (app.TaskBoardSnapshot, error) {
	stub.listCalls++
	return stub.snapshot, nil
}

func (stub *taskBoardGatewayStub) StartAuthoring(_ context.Context, request app.TaskBoardStartAuthoringRequest) (app.TaskBoardMutation, error) {
	stub.startRequests = append(stub.startRequests, request)
	return app.TaskBoardMutation{TaskID: "task-1", Summary: "started"}, stub.startErr
}

func (stub *taskBoardGatewayStub) DecideReview(_ context.Context, request app.TaskBoardDecideReviewRequest) (app.TaskBoardMutation, error) {
	stub.decisionRequests = append(stub.decisionRequests, request)
	return app.TaskBoardMutation{TaskID: request.TaskID, Summary: "decided"}, stub.decisionErr
}

func (stub *taskBoardGatewayStub) ReadRunLog(context.Context, app.TaskBoardReadRunLogRequest) (app.TaskBoardLog, error) {
	return stub.log, stub.logErr
}

func (stub *taskBoardGatewayStub) RetryRun(_ context.Context, request app.TaskBoardRetryRunRequest) (app.TaskBoardMutation, error) {
	stub.retryRequests = append(stub.retryRequests, request)
	return app.TaskBoardMutation{TaskID: request.TaskID, RunID: request.RunID, Summary: "retried"}, stub.retryErr
}

func (stub *taskBoardGatewayStub) CancelRun(_ context.Context, request app.TaskBoardCancelRunRequest) (app.TaskBoardMutation, error) {
	stub.cancelRequests = append(stub.cancelRequests, request)
	return app.TaskBoardMutation{TaskID: request.TaskID, RunID: request.RunID, Summary: "canceled"}, stub.cancelErr
}

func (stub *taskBoardGatewayStub) FlushQueuedRuns(context.Context) error {
	stub.flushCalls++
	return stub.flushErr
}

func taskBoardTestSnapshot(authoringAvailable bool) app.TaskBoardSnapshot {
	return app.TaskBoardSnapshot{
		AuthoringAvailable: authoringAvailable,
		Tasks: []app.TaskBoardTask{{
			ID: "task-1", Title: "Task one", RepositoryURL: "https://example.invalid/repo.git", CommitSHA: "abcdef0123456789", Column: app.TaskBoardPending,
			Review: &app.TaskBoardReview{Kind: app.TaskBoardAuthoringReview, RequestID: "review-1"},
			RunID:  "run-1", RunStatus: "failed_recoverable", Runs: []app.TaskBoardRun{{
				ID: "run-1", Status: "failed_recoverable", CurrentStage: "repo_prepare", LogPath: "/managed/logs/run-1.log", CanRetry: true,
			}},
		}},
	}
}

func loadedTaskBoardModel(t *testing.T, stub *taskBoardGatewayStub) appModel {
	t.Helper()
	model := newAppModelWithGateway(context.Background(), stub)
	updated, command := model.Update(taskBoardLoadedMsg{snapshot: stub.snapshot, epoch: 1})
	if command != nil {
		t.Fatal("load update unexpectedly returned a command")
	}
	return updated.(appModel)
}

func keyRune(value rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{value}}
}

func TestAppModelRoutesBoardCommandsThroughGateway(t *testing.T) {
	stub := &taskBoardGatewayStub{snapshot: taskBoardTestSnapshot(true)}
	model := loadedTaskBoardModel(t, stub)
	if selected := model.board.SelectedTask(); selected == nil || selected.ID != "task-1" || selected.Review == nil {
		t.Fatalf("loaded board selection = %+v", selected)
	}

	message := TaskSubmitMsg{RepoURL: "https://example.invalid/repo.git", CommitSHA: "abcdef0123456789", Slug: "task-one", Title: "Task one", Reason: "create task"}
	_, startCommand := model.beginAuthoring(message, nil)
	mutationMessage := startCommand()
	if _, ok := mutationMessage.(taskBoardMutationMsg); !ok || len(stub.startRequests) != 1 || stub.startRequests[0].Reason != message.Reason || stub.startRequests[0].Slug != message.Slug || stub.startRequests[0].IdempotencyKey == "" {
		t.Fatalf("start command = %#v, request=%+v", mutationMessage, stub.startRequests)
	}

	reviewModel := loadedTaskBoardModel(t, stub)
	reviewModel.detail = newDetailModel(reviewModel.board.SelectedTask())
	updated, _ := reviewModel.handleKey(keyRune('a'), nil)
	reviewModel = updated.(appModel)
	if reviewModel.review == nil || reviewModel.review.decision != app.TaskBoardApprove {
		t.Fatalf("review confirmation = %+v", reviewModel.review)
	}
	reviewModel.review.reasonInput.SetValue("approve reviewed task")
	updated, reviewCommand := reviewModel.handleKey(tea.KeyMsg{Type: tea.KeyEnter}, nil)
	decisionMessage := reviewCommand()
	if _, ok := decisionMessage.(taskBoardMutationMsg); !ok || len(stub.decisionRequests) != 1 || stub.decisionRequests[0].Decision != app.TaskBoardApprove || stub.decisionRequests[0].Reason != "approve reviewed task" || stub.decisionRequests[0].IdempotencyKey == "" {
		t.Fatalf("review command = %#v, requests=%+v", decisionMessage, stub.decisionRequests)
	}
	if updated.(appModel).activeMutation != taskBoardReviewMutation {
		t.Fatal("review mutation was not marked in flight")
	}
}

func TestVisibleTaskInputConsumesBoardAndReviewKeys(t *testing.T) {
	stub := &taskBoardGatewayStub{snapshot: taskBoardTestSnapshot(true)}
	model := loadedTaskBoardModel(t, stub)
	model.input.Show()
	for _, value := range []rune{'d', 'a', 'r', 'h', 'j', 'k', 'l'} {
		updated, _ := model.handleKey(keyRune(value), nil)
		model = updated.(appModel)
	}
	updated, _ := model.handleKey(tea.KeyMsg{Type: tea.KeyTab}, nil)
	model = updated.(appModel)
	if model.detail != nil || model.review != nil || len(stub.decisionRequests) != 0 || model.board.cursor != 0 || model.board.selected != 0 {
		t.Fatalf("input key leaked into board state: detail=%+v review=%+v decisions=%+v cursor=%d selected=%d", model.detail, model.review, stub.decisionRequests, model.board.cursor, model.board.selected)
	}
	if model.input.repoInput.Value() != "darhjkl" || model.input.focusIndex != 1 {
		t.Fatalf("input did not retain typed keys or tab focus: value=%q focus=%d", model.input.repoInput.Value(), model.input.focusIndex)
	}
}

func TestAppModelRetriesFailedCommandsWithTheSameIdempotencyKey(t *testing.T) {
	stub := &taskBoardGatewayStub{
		snapshot:    taskBoardTestSnapshot(true),
		startErr:    errors.New("activation unavailable"),
		decisionErr: errors.New("activation unavailable"),
	}
	message := TaskSubmitMsg{RepoURL: "https://example.invalid/repo.git", CommitSHA: "abcdef0123456789", Slug: "task-one", Title: "Task one", Reason: "create task"}

	model := loadedTaskBoardModel(t, stub)
	updated, command := model.beginAuthoring(message, nil)
	model = updated.(appModel)
	first := command().(taskBoardMutationMsg)
	updated, _ = model.Update(first)
	model = updated.(appModel)
	updated, command = model.beginAuthoring(TaskSubmitMsg{
		RepoURL: "  " + message.RepoURL + "  ", CommitSHA: "  " + message.CommitSHA + "  ", Slug: "  " + message.Slug + "  ",
		Title: "  " + message.Title + "  ", Reason: "  " + message.Reason + "  ",
	}, nil)
	model = updated.(appModel)
	_ = model
	_ = command().(taskBoardMutationMsg)
	if len(stub.startRequests) != 2 || stub.startRequests[0].IdempotencyKey != stub.startRequests[1].IdempotencyKey {
		t.Fatalf("authoring retry keys = %+v", stub.startRequests)
	}
	if stub.startRequests[1].RepositoryURL != message.RepoURL || stub.startRequests[1].CommitSHA != message.CommitSHA || stub.startRequests[1].Slug != message.Slug || stub.startRequests[1].Title != message.Title || stub.startRequests[1].Reason != message.Reason {
		t.Fatalf("authoring retry request = %+v, want normalized %+v", stub.startRequests[1], message)
	}

	reviewModel := loadedTaskBoardModel(t, stub)
	reviewModel.detail = newDetailModel(reviewModel.board.SelectedTask())
	updated, _ = reviewModel.handleKey(keyRune('a'), nil)
	reviewModel = updated.(appModel)
	reviewModel.review.reasonInput.SetValue("approve reviewed task")
	updated, command = reviewModel.handleKey(tea.KeyMsg{Type: tea.KeyEnter}, nil)
	reviewModel = updated.(appModel)
	first = command().(taskBoardMutationMsg)
	updated, _ = reviewModel.Update(first)
	reviewModel = updated.(appModel)
	updated, _ = reviewModel.handleKey(tea.KeyMsg{Type: tea.KeyEsc}, nil)
	reviewModel = updated.(appModel)
	updated, _ = reviewModel.handleKey(keyRune('q'), nil)
	reviewModel = updated.(appModel)
	updated, _ = reviewModel.handleKey(keyRune('d'), nil)
	reviewModel = updated.(appModel)
	updated, _ = reviewModel.handleKey(keyRune('a'), nil)
	reviewModel = updated.(appModel)
	reviewModel.review.reasonInput.SetValue("approve reviewed task")
	updated, command = reviewModel.handleKey(tea.KeyMsg{Type: tea.KeyEnter}, nil)
	_ = updated.(appModel)
	_ = command().(taskBoardMutationMsg)
	if len(stub.decisionRequests) != 2 || stub.decisionRequests[0].IdempotencyKey != stub.decisionRequests[1].IdempotencyKey {
		t.Fatalf("review retry keys = %+v", stub.decisionRequests)
	}
}

func TestAppModelRefreshesQueuedRunsAndGatesUnavailableAuthoring(t *testing.T) {
	stub := &taskBoardGatewayStub{snapshot: taskBoardTestSnapshot(false)}
	model := newAppModelWithGateway(context.Background(), stub)
	message := model.refreshTasks(1)()
	loaded, ok := message.(taskBoardLoadedMsg)
	if !ok || stub.flushCalls != 1 || stub.listCalls != 1 {
		t.Fatalf("startup refresh = %#v, flushes=%d lists=%d", message, stub.flushCalls, stub.listCalls)
	}
	updated, _ := model.Update(loaded)
	model = updated.(appModel)
	updated, command := model.handleKey(keyRune('n'), nil)
	if command != nil {
		t.Fatal("unavailable authoring unexpectedly opened a form command")
	}
	model = updated.(appModel)
	if model.input.Visible() || !errors.Is(model.err, app.ErrStandardAuthoringLaunchUnavailable) {
		t.Fatalf("unavailable authoring state = visible:%t err:%v", model.input.Visible(), model.err)
	}
}

func TestAppModelDoesNotExitWhileAMutationIsInFlight(t *testing.T) {
	stub := &taskBoardGatewayStub{snapshot: taskBoardTestSnapshot(true)}
	model := loadedTaskBoardModel(t, stub)
	message := TaskSubmitMsg{RepoURL: "https://example.invalid/repo.git", CommitSHA: "abcdef0123456789", Slug: "task-one", Title: "Task one", Reason: "create task"}
	updated, _ := model.beginAuthoring(message, nil)
	model = updated.(appModel)
	updated, exitCommand := model.handleKey(keyRune('q'), nil)
	model = updated.(appModel)
	if exitCommand != nil || stub.flushCalls != 0 || !model.mutationInFlight() {
		t.Fatalf("exit while mutation in flight = command:%v flushes:%d active:%q", exitCommand, stub.flushCalls, model.activeMutation)
	}
}

func TestAppModelCanForceExitAfterExitFlushFails(t *testing.T) {
	stub := &taskBoardGatewayStub{snapshot: taskBoardTestSnapshot(true), flushErr: errors.New("activation unavailable")}
	model := loadedTaskBoardModel(t, stub)
	updated, exitCommand := model.handleKey(keyRune('q'), nil)
	model = updated.(appModel)
	if exitCommand == nil {
		t.Fatal("q did not attempt the exit flush")
	}
	message := exitCommand()
	updated, _ = model.Update(message)
	model = updated.(appModel)
	if !model.exitFlushFailed || stub.flushCalls != 1 {
		t.Fatalf("failed exit flush state = failed:%t calls:%d", model.exitFlushFailed, stub.flushCalls)
	}
	updated, forceCommand := model.handleKey(tea.KeyMsg{Type: tea.KeyCtrlC}, nil)
	_ = updated.(appModel)
	if _, ok := forceCommand().(tea.QuitMsg); !ok {
		t.Fatalf("forced exit command = %#v, want QuitMsg", forceCommand())
	}
}

func TestTaskInputSubmissionRetainsValuesUntilTheBackendSucceeds(t *testing.T) {
	input := NewTaskInputModel()
	if input.commitInput.CharLimit != 64 {
		t.Fatalf("commit SHA limit = %d, want 64", input.commitInput.CharLimit)
	}
	input.Show()
	input.repoInput.SetValue("https://example.invalid/repo.git")
	input.commitInput.SetValue("abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789")
	input.slugInput.SetValue("retained-task")
	input.titleInput.SetValue("Retained task")
	input.reasonInput.SetValue("test retry")
	command, handled := input.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !handled || command == nil {
		t.Fatal("complete form did not emit a submit message")
	}
	message, ok := command().(TaskSubmitMsg)
	if !ok || message.Slug != "retained-task" || message.Reason != "test retry" || len(message.CommitSHA) != 64 {
		t.Fatalf("submit message = %#v", message)
	}
	if input.repoInput.Value() == "" || input.reasonInput.Value() == "" {
		t.Fatal("input values were cleared before a successful backend response")
	}
	input.Reset()
	if input.repoInput.Value() != "" || input.reasonInput.Value() != "" {
		t.Fatal("successful reset did not clear the form")
	}
}

func TestDetailRunActionsAndLogsTargetTheCurrentRun(t *testing.T) {
	stub := &taskBoardGatewayStub{
		snapshot: taskBoardTestSnapshot(true),
		log: app.TaskBoardLog{
			RunID: "run-1", Path: "/managed/logs/run-1.log", Content: "first line\nsecond line",
		},
	}
	model := loadedTaskBoardModel(t, stub)
	model.detail = newDetailModel(model.board.SelectedTask())

	updated, _ := model.handleKey(keyRune('t'), nil)
	model = updated.(appModel)
	if model.action == nil || model.action.kind != taskBoardRetryAction {
		t.Fatalf("retry prompt = %+v", model.action)
	}
	model.action.reasonInput.SetValue("retry recoverable run")
	updated, command := model.handleKey(tea.KeyMsg{Type: tea.KeyEnter}, nil)
	model = updated.(appModel)
	if model.activeMutation != taskBoardRetryMutation || command == nil {
		t.Fatalf("retry start = active:%q command:%v", model.activeMutation, command)
	}
	_ = command().(taskBoardMutationMsg)
	if len(stub.retryRequests) != 1 || stub.retryRequests[0].TaskID != "task-1" || stub.retryRequests[0].RunID != "run-1" || stub.retryRequests[0].Reason != "retry recoverable run" || stub.retryRequests[0].IdempotencyKey == "" {
		t.Fatalf("retry request = %+v", stub.retryRequests)
	}

	model = loadedTaskBoardModel(t, stub)
	model.detail = newDetailModel(model.board.SelectedTask())
	updated, logCommand := model.handleKey(keyRune('l'), nil)
	model = updated.(appModel)
	if model.logs == nil || logCommand == nil {
		t.Fatalf("log open = logs:%+v command:%v", model.logs, logCommand)
	}
	updated, _ = model.Update(logCommand())
	model = updated.(appModel)
	if model.logs == nil || model.logs.path != "/managed/logs/run-1.log" || !strings.Contains(model.logs.content, "second line") {
		t.Fatalf("loaded log = %+v", model.logs)
	}
}

func TestAppDetailAndLogViewsFitTheWindow(t *testing.T) {
	stub := &taskBoardGatewayStub{snapshot: taskBoardTestSnapshot(true)}
	model := loadedTaskBoardModel(t, stub)
	model.width = 120
	model.height = 40
	model.detail = newDetailModel(model.board.SelectedTask())
	assertAppWidth(t, model.View(), model.width)

	model.logs = newLogModel(model.detail.task, app.TaskBoardLog{
		RunID: "run-1", Path: "/managed/logs/run-1.log", Content: strings.Repeat("a log line\n", 24),
	})
	assertAppWidth(t, model.View(), model.width)
}

func assertAppWidth(t *testing.T, rendered string, width int) {
	t.Helper()
	for _, line := range strings.Split(ansi.Strip(rendered), "\n") {
		if actual := lipgloss.Width(line); actual > width {
			t.Fatalf("application line width %d exceeds window width %d: %q", actual, width, line)
		}
	}
}
