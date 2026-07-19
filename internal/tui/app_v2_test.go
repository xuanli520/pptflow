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

const taskBoardTestBaseImage = "docker.io/library/rust:1.65.0-bullseye@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

const (
	taskBoardTestTaskType    = "feature"
	taskBoardTestApplication = "backend"
	taskBoardTestObjective   = "Add a bounded Tower HTTP backend feature"
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

	message := TaskSubmitMsg{RepoURL: "https://example.invalid/repo.git", CommitSHA: "abcdef0123456789", BaseImage: taskBoardTestBaseImage, Slug: "task-one", Title: "Task one", TaskType: taskBoardTestTaskType, Application: taskBoardTestApplication, Objective: taskBoardTestObjective, Reason: "create task"}
	_, startCommand := model.beginAuthoring(message, nil)
	mutationMessage := startCommand()
	if _, ok := mutationMessage.(taskBoardMutationMsg); !ok || len(stub.startRequests) != 1 || stub.startRequests[0].Reason != message.Reason || stub.startRequests[0].BaseImage != message.BaseImage || stub.startRequests[0].Slug != message.Slug || stub.startRequests[0].TaskType != message.TaskType || stub.startRequests[0].Application != message.Application || stub.startRequests[0].Objective != message.Objective || stub.startRequests[0].IdempotencyKey == "" {
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

func TestTaskItemsForSnapshotPreservesDurableFailureProjection(t *testing.T) {
	pending, _, _ := taskItemsForSnapshot(app.TaskBoardSnapshot{Tasks: []app.TaskBoardTask{{
		ID: "task-1", Title: "Task one", Column: app.TaskBoardPending, RunID: "run-1",
		Runs: []app.TaskBoardRun{{
			ID:                    "run-1",
			FailureCode:           "handoff.definition_invalid",
			FailureSummary:        "The approved child definition is invalid.",
			FailureJobID:          "job-1",
			FailureArtifactID:     "artifact-1",
			FailureRecoveryAction: app.TaskBoardFailureRecoveryRedriveAuthoringHandoff,
			CanRedrive:            true,
		}},
	}}})
	if len(pending) != 1 || len(pending[0].Runs) != 1 {
		t.Fatalf("task items = %+v", pending)
	}
	run := pending[0].Runs[0]
	if run.FailureCode != "handoff.definition_invalid" || run.FailureSummary != "The approved child definition is invalid." ||
		run.FailureJobID != "job-1" || run.FailureArtifactID != "artifact-1" ||
		run.FailureRecoveryAction != app.TaskBoardFailureRecoveryRedriveAuthoringHandoff || !run.CanRedrive {
		t.Fatalf("durable failure item = %+v", run)
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

func TestTaskInputRequiresFrozenInputsAndIncludesThemInTabOrder(t *testing.T) {
	input := NewTaskInputModel()
	input.Show()
	input.repoInput.SetValue("https://example.invalid/repo.git")
	input.commitInput.SetValue("abcdef0123456789abcdef0123456789abcdef01234567")
	input.slugInput.SetValue("immutable-environment")
	input.titleInput.SetValue("Immutable environment")
	input.reasonInput.SetValue("exercise required task contract input")

	for range 2 {
		_, handled := input.Update(tea.KeyMsg{Type: tea.KeyTab})
		if !handled {
			t.Fatal("tab was not handled by the task form")
		}
	}
	if input.focusIndex != 2 || !input.baseImageInput.Focused() {
		t.Fatalf("base image focus = index:%d focused:%t", input.focusIndex, input.baseImageInput.Focused())
	}

	command, handled := input.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !handled || command != nil || !strings.Contains(input.validationErr, "base image") {
		t.Fatalf("missing base image submission = command:%v handled:%t error:%q", command, handled, input.validationErr)
	}

	input.baseImageInput.SetValue(taskBoardTestBaseImage)
	for range 3 {
		input.Update(tea.KeyMsg{Type: tea.KeyTab})
	}
	if input.focusIndex != 5 || !input.taskTypeInput.Focused() {
		t.Fatalf("task type focus = index:%d focused:%t", input.focusIndex, input.taskTypeInput.Focused())
	}
	input.Update(tea.KeyMsg{Type: tea.KeyTab})
	if input.focusIndex != 6 || !input.applicationInput.Focused() {
		t.Fatalf("application focus = index:%d focused:%t", input.focusIndex, input.applicationInput.Focused())
	}
	input.Update(tea.KeyMsg{Type: tea.KeyTab})
	if input.focusIndex != 7 || !input.objectiveInput.Focused() {
		t.Fatalf("objective focus = index:%d focused:%t", input.focusIndex, input.objectiveInput.Focused())
	}
	command, handled = input.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !handled || command != nil || !strings.Contains(input.validationErr, "task type") || !strings.Contains(input.validationErr, "application") || !strings.Contains(input.validationErr, "objective") {
		t.Fatalf("missing authoring brief submission = command:%v handled:%t error:%q", command, handled, input.validationErr)
	}
}

func TestAppModelRetriesFailedCommandsWithTheSameIdempotencyKey(t *testing.T) {
	stub := &taskBoardGatewayStub{
		snapshot:    taskBoardTestSnapshot(true),
		startErr:    errors.New("activation unavailable"),
		decisionErr: errors.New("activation unavailable"),
	}
	message := TaskSubmitMsg{RepoURL: "https://example.invalid/repo.git", CommitSHA: "abcdef0123456789", BaseImage: taskBoardTestBaseImage, Slug: "task-one", Title: "Task one", TaskType: taskBoardTestTaskType, Application: taskBoardTestApplication, Objective: taskBoardTestObjective, Reason: "create task"}

	model := loadedTaskBoardModel(t, stub)
	updated, command := model.beginAuthoring(message, nil)
	model = updated.(appModel)
	first := command().(taskBoardMutationMsg)
	updated, _ = model.Update(first)
	model = updated.(appModel)
	updated, command = model.beginAuthoring(TaskSubmitMsg{
		RepoURL: "  " + message.RepoURL + "  ", CommitSHA: "  " + message.CommitSHA + "  ", BaseImage: "  " + message.BaseImage + "  ", Slug: "  " + message.Slug + "  ",
		Title: "  " + message.Title + "  ", TaskType: "  " + message.TaskType + "  ", Application: "  " + message.Application + "  ", Objective: "  " + message.Objective + "  ", Reason: "  " + message.Reason + "  ",
	}, nil)
	model = updated.(appModel)
	_ = model
	_ = command().(taskBoardMutationMsg)
	if len(stub.startRequests) != 2 || stub.startRequests[0].IdempotencyKey != stub.startRequests[1].IdempotencyKey {
		t.Fatalf("authoring retry keys = %+v", stub.startRequests)
	}
	if stub.startRequests[1].RepositoryURL != message.RepoURL || stub.startRequests[1].CommitSHA != message.CommitSHA || stub.startRequests[1].BaseImage != message.BaseImage || stub.startRequests[1].Slug != message.Slug || stub.startRequests[1].Title != message.Title || stub.startRequests[1].TaskType != message.TaskType || stub.startRequests[1].Application != message.Application || stub.startRequests[1].Objective != message.Objective || stub.startRequests[1].Reason != message.Reason {
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

func TestAppModelRetainsBaseImageAfterFailedStartAndClearsItAfterSuccess(t *testing.T) {
	stub := &taskBoardGatewayStub{
		snapshot: taskBoardTestSnapshot(true),
		startErr: errors.New("activation unavailable"),
	}
	model := loadedTaskBoardModel(t, stub)
	model.input.Show()
	model.input.baseImageInput.SetValue(taskBoardTestBaseImage)
	model.input.taskTypeInput.SetValue(taskBoardTestTaskType)
	model.input.applicationInput.SetValue(taskBoardTestApplication)
	model.input.objectiveInput.SetValue(taskBoardTestObjective)
	message := TaskSubmitMsg{
		RepoURL: "https://example.invalid/repo.git", CommitSHA: "abcdef0123456789", BaseImage: taskBoardTestBaseImage,
		Slug: "task-one", Title: "Task one", TaskType: taskBoardTestTaskType, Application: taskBoardTestApplication, Objective: taskBoardTestObjective, Reason: "create task",
	}

	updated, command := model.beginAuthoring(message, nil)
	model = updated.(appModel)
	failed := command().(taskBoardMutationMsg)
	updated, _ = model.Update(failed)
	model = updated.(appModel)
	if !model.input.Visible() || model.input.baseImageInput.Value() != taskBoardTestBaseImage || model.input.taskTypeInput.Value() != taskBoardTestTaskType || model.input.applicationInput.Value() != taskBoardTestApplication || model.input.objectiveInput.Value() != taskBoardTestObjective {
		t.Fatalf("failed start cleared task contract input: visible:%t baseImage:%q type:%q application:%q objective:%q", model.input.Visible(), model.input.baseImageInput.Value(), model.input.taskTypeInput.Value(), model.input.applicationInput.Value(), model.input.objectiveInput.Value())
	}

	stub.startErr = nil
	updated, command = model.beginAuthoring(message, nil)
	model = updated.(appModel)
	succeeded := command().(taskBoardMutationMsg)
	updated, _ = model.Update(succeeded)
	model = updated.(appModel)
	if model.input.Visible() || model.input.baseImageInput.Value() != "" || model.input.taskTypeInput.Value() != "" || model.input.applicationInput.Value() != "" || model.input.objectiveInput.Value() != "" {
		t.Fatalf("successful start retained task contract input: visible:%t baseImage:%q type:%q application:%q objective:%q", model.input.Visible(), model.input.baseImageInput.Value(), model.input.taskTypeInput.Value(), model.input.applicationInput.Value(), model.input.objectiveInput.Value())
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
	message := TaskSubmitMsg{RepoURL: "https://example.invalid/repo.git", CommitSHA: "abcdef0123456789", BaseImage: taskBoardTestBaseImage, Slug: "task-one", Title: "Task one", TaskType: taskBoardTestTaskType, Application: taskBoardTestApplication, Objective: taskBoardTestObjective, Reason: "create task"}
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
	if input.baseImageInput.CharLimit != 512 {
		t.Fatalf("base image limit = %d, want 512", input.baseImageInput.CharLimit)
	}
	if input.taskTypeInput.CharLimit != 64 || input.applicationInput.CharLimit != 64 || input.objectiveInput.CharLimit != 512 {
		t.Fatalf("authoring brief limits = type:%d application:%d objective:%d", input.taskTypeInput.CharLimit, input.applicationInput.CharLimit, input.objectiveInput.CharLimit)
	}
	input.Show()
	input.repoInput.SetValue("  https://example.invalid/repo.git  ")
	input.commitInput.SetValue("abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789")
	input.baseImageInput.SetValue(taskBoardTestBaseImage)
	input.slugInput.SetValue("retained-task")
	input.titleInput.SetValue("Retained task")
	input.taskTypeInput.SetValue("  " + taskBoardTestTaskType + "  ")
	input.applicationInput.SetValue("  " + taskBoardTestApplication + "  ")
	input.objectiveInput.SetValue("  " + taskBoardTestObjective + "  ")
	input.reasonInput.SetValue("test retry")
	command, handled := input.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !handled || command == nil {
		t.Fatal("complete form did not emit a submit message")
	}
	message, ok := command().(TaskSubmitMsg)
	if !ok || message.RepoURL != "https://example.invalid/repo.git" || message.BaseImage != taskBoardTestBaseImage || message.Slug != "retained-task" || message.TaskType != taskBoardTestTaskType || message.Application != taskBoardTestApplication || message.Objective != taskBoardTestObjective || message.Reason != "test retry" || len(message.CommitSHA) != 64 {
		t.Fatalf("submit message = %#v", message)
	}
	if input.repoInput.Value() == "" || input.baseImageInput.Value() == "" || input.taskTypeInput.Value() == "" || input.applicationInput.Value() == "" || input.objectiveInput.Value() == "" || input.reasonInput.Value() == "" {
		t.Fatal("input values were cleared before a successful backend response")
	}
	input.Reset()
	if input.repoInput.Value() != "" || input.baseImageInput.Value() != "" || input.taskTypeInput.Value() != "" || input.applicationInput.Value() != "" || input.objectiveInput.Value() != "" || input.reasonInput.Value() != "" {
		t.Fatal("successful reset did not clear the form")
	}
}

func TestTaskInputRejectsObjectiveOverUTF8ByteLimit(t *testing.T) {
	input := NewTaskInputModel()
	input.Show()
	input.repoInput.SetValue("https://example.invalid/repo.git")
	input.commitInput.SetValue("abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789")
	input.baseImageInput.SetValue(taskBoardTestBaseImage)
	input.slugInput.SetValue("byte-limited-task")
	input.titleInput.SetValue("Byte limited task")
	input.taskTypeInput.SetValue(taskBoardTestTaskType)
	input.applicationInput.SetValue(taskBoardTestApplication)
	input.objectiveInput.SetValue(strings.Repeat("界", 200))
	input.reasonInput.SetValue("verify byte limit")
	command, handled := input.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !handled || command != nil || !strings.Contains(input.validationErr, "512 UTF-8 bytes") {
		t.Fatalf("oversized UTF-8 objective = handled:%t command:%v error:%q", handled, command, input.validationErr)
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

func TestAppModelRoutesAuthoringRecoveryThroughRetryAndRetainsItsIdempotencyKey(t *testing.T) {
	snapshot := taskBoardTestSnapshot(true)
	snapshot.Tasks[0].Runs[0].RetryStrategy = app.TaskBoardRetryStrategyAuthoringRecovery
	stub := &taskBoardGatewayStub{snapshot: snapshot, retryErr: errors.New("activation unavailable")}
	model := loadedTaskBoardModel(t, stub)
	model.detail = newDetailModel(model.board.SelectedTask())

	updated, _ := model.handleKey(keyRune('t'), nil)
	model = updated.(appModel)
	if model.action == nil || model.action.kind != taskBoardRetryAction || model.action.strategy != app.TaskBoardRetryStrategyAuthoringRecovery {
		t.Fatalf("authoring recovery prompt = %+v", model.action)
	}
	model.action.reasonInput.SetValue("recover transient provider failure")
	updated, command := model.handleKey(tea.KeyMsg{Type: tea.KeyEnter}, nil)
	model = updated.(appModel)
	if model.activeMutation != taskBoardRetryMutation || command == nil {
		t.Fatalf("authoring recovery start = active:%q command:%v", model.activeMutation, command)
	}
	first := command().(taskBoardMutationMsg)
	if first.kind != taskBoardRetryMutation {
		t.Fatalf("authoring recovery message = %#v", first)
	}
	updated, _ = model.Update(first)
	model = updated.(appModel)
	if model.action == nil || model.pendingAction == nil {
		t.Fatalf("failed recovery did not preserve pending action: action=%+v pending=%+v", model.action, model.pendingAction)
	}

	updated, command = model.handleKey(tea.KeyMsg{Type: tea.KeyEnter}, nil)
	model = updated.(appModel)
	_ = model
	_ = command().(taskBoardMutationMsg)
	if len(stub.retryRequests) != 2 {
		t.Fatalf("authoring recovery dispatches = %+v", stub.retryRequests)
	}
	if first, replay := stub.retryRequests[0], stub.retryRequests[1]; first.IdempotencyKey == "" || first.IdempotencyKey != replay.IdempotencyKey || first.TaskID != "task-1" || first.RunID != "run-1" || first.Reason != "recover transient provider failure" {
		t.Fatalf("authoring recovery idempotency/replay = first:%+v replay:%+v", first, replay)
	}
}

func TestAppModelRequestsAuthoringChangesThenDispatchesRepairContinuation(t *testing.T) {
	snapshot := taskBoardTestSnapshot(true)
	snapshot.Tasks[0].RunStatus = "waiting_review"
	snapshot.Tasks[0].Runs[0].Status = "waiting_review"
	snapshot.Tasks[0].Runs[0].CurrentStage = "task_review"
	snapshot.Tasks[0].Runs[0].CanRetry = false
	stub := &taskBoardGatewayStub{snapshot: snapshot}
	model := loadedTaskBoardModel(t, stub)
	model.detail = newDetailModel(model.board.SelectedTask())

	updated, _ := model.handleKey(keyRune('r'), nil)
	model = updated.(appModel)
	if model.review == nil || model.review.decision != app.TaskBoardRequestChanges {
		t.Fatalf("request-changes prompt = %+v", model.review)
	}
	model.review.reasonInput.SetValue("correct the generated tower-http paths")
	updated, command := model.handleKey(tea.KeyMsg{Type: tea.KeyEnter}, nil)
	model = updated.(appModel)
	if model.activeMutation != taskBoardReviewMutation || command == nil {
		t.Fatalf("request-changes start = active:%q command:%v", model.activeMutation, command)
	}
	decision := command().(taskBoardMutationMsg)
	if len(stub.decisionRequests) != 1 || stub.decisionRequests[0].Decision != app.TaskBoardRequestChanges ||
		stub.decisionRequests[0].Reason != "correct the generated tower-http paths" {
		t.Fatalf("request-changes dispatch = %+v", stub.decisionRequests)
	}
	updated, _ = model.Update(decision)
	model = updated.(appModel)

	stub.snapshot.Tasks[0].Review = nil
	stub.snapshot.Tasks[0].Column = app.TaskBoardRunning
	stub.snapshot.Tasks[0].RunStatus = "waiting_continuation"
	stub.snapshot.Tasks[0].Runs[0].Status = "waiting_continuation"
	stub.snapshot.Tasks[0].Runs[0].CanRetry = true
	stub.snapshot.Tasks[0].Runs[0].RetryReason = ""
	stub.snapshot.Tasks[0].Runs[0].RetryStrategy = app.TaskBoardRetryStrategyAuthoringAdmissionRepair
	updated, _ = model.Update(taskBoardLoadedMsg{snapshot: stub.snapshot, epoch: model.refreshEpoch})
	model = updated.(appModel)
	model.board.MoveRight()
	selected := model.board.SelectedTask()
	if selected == nil {
		t.Fatal("waiting-continuation task was not projected into the running column")
	}
	model.detail = newDetailModel(selected)
	if footer := detailFooterText(model.detail); !strings.Contains(footer, "[t] 修复并继续") {
		t.Fatalf("repair continuation footer = %q", footer)
	}

	updated, _ = model.handleKey(keyRune('t'), nil)
	model = updated.(appModel)
	if model.action == nil || model.action.kind != taskBoardRetryAction || model.action.strategy != app.TaskBoardRetryStrategyAuthoringAdmissionRepair {
		t.Fatalf("repair continuation prompt = %+v", model.action)
	}
	model.action.reasonInput.SetValue("regenerate with the review feedback")
	updated, command = model.handleKey(tea.KeyMsg{Type: tea.KeyEnter}, nil)
	model = updated.(appModel)
	if model.activeMutation != taskBoardRetryMutation || command == nil {
		t.Fatalf("repair continuation start = active:%q command:%v", model.activeMutation, command)
	}
	_ = command().(taskBoardMutationMsg)
	if len(stub.retryRequests) != 1 || stub.retryRequests[0].TaskID != "task-1" || stub.retryRequests[0].RunID != "run-1" ||
		stub.retryRequests[0].Reason != "regenerate with the review feedback" {
		t.Fatalf("repair continuation dispatch = %+v", stub.retryRequests)
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

func TestAppTaskInputWithValidationErrorFitsTheWindow(t *testing.T) {
	stub := &taskBoardGatewayStub{snapshot: taskBoardTestSnapshot(true)}
	model := loadedTaskBoardModel(t, stub)
	model.width = 120
	model.height = 24
	model.input.Show()
	if _, handled := model.input.Update(tea.KeyMsg{Type: tea.KeyEnter}); !handled || model.input.validationErr == "" {
		t.Fatal("empty authoring form did not render its validation error")
	}
	rendered := model.View()
	assertAppWidth(t, rendered, model.width)
	assertAppHeight(t, rendered, model.height)
}

func assertAppWidth(t *testing.T, rendered string, width int) {
	t.Helper()
	for _, line := range strings.Split(ansi.Strip(rendered), "\n") {
		if actual := lipgloss.Width(line); actual > width {
			t.Fatalf("application line width %d exceeds window width %d: %q", actual, width, line)
		}
	}
}

func assertAppHeight(t *testing.T, rendered string, height int) {
	t.Helper()
	if actual := lipgloss.Height(ansi.Strip(rendered)); actual > height {
		t.Fatalf("application height %d exceeds window height %d", actual, height)
	}
}
