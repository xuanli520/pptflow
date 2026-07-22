package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const taskBoardTestBaseImage = "docker.io/library/rust:1.65.0-bullseye@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

const (
	taskBoardTestTaskType    = "feature"
	taskBoardTestApplication = "backend"
	taskBoardTestObjective   = "Add a bounded Tower HTTP backend feature"
)

type taskBoardGatewayStub struct {
	snapshot                app.TaskBoardSnapshot
	startRequests           []app.TaskBoardStartAuthoringRequest
	decisionRequests        []app.TaskBoardDecideReviewRequest
	recoveryPreviews        []app.TaskBoardPreviewRunRecoveryRequest
	retryRequests           []app.TaskBoardRetryRunRequest
	launchRetryRequests     []app.TaskBoardRetryAuthoringLaunchRequest
	cancelRequests          []app.TaskBoardCancelRunRequest
	log                     app.TaskBoardLog
	startErr                error
	decisionErr             error
	recoveryPreview         app.TaskBoardRecoveryPreview
	recoveryPreviewErr      error
	recoveryPreviewDeadline time.Time
	retryErr                error
	launchRetryErr          error
	cancelErr               error
	logErr                  error
	flushErr                error
	listCalls               int
	flushCalls              int
	keys                    int
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

func (stub *taskBoardGatewayStub) PreviewRunRecovery(ctx context.Context, request app.TaskBoardPreviewRunRecoveryRequest) (app.TaskBoardRecoveryPreview, error) {
	stub.recoveryPreviews = append(stub.recoveryPreviews, request)
	if deadline, ok := ctx.Deadline(); ok {
		stub.recoveryPreviewDeadline = deadline
	}
	preview := stub.recoveryPreview
	if preview.TaskID == "" {
		preview.TaskID = request.TaskID
	}
	if preview.RunID == "" {
		preview.RunID = request.RunID
	}
	if preview.Strategy == "" {
		preview.Strategy = app.TaskBoardRetryStrategyAuthoringRecovery
		for _, task := range stub.snapshot.Tasks {
			for _, run := range task.Runs {
				if run.ID == request.RunID && run.RetryStrategy != "" {
					preview.Strategy = run.RetryStrategy
				}
			}
		}
	}
	if preview.Checkpoint.Sequence == 0 {
		preview.Checkpoint = workflowkit.CheckpointRef{Sequence: 1}
	}
	if preview.SemanticPlanFingerprint == "" {
		preview.SemanticPlanFingerprint = workflowkit.SHA256Fingerprint([]byte("task-board-recovery-preview"))
	}
	return preview, stub.recoveryPreviewErr
}

func (stub *taskBoardGatewayStub) RetryRun(_ context.Context, request app.TaskBoardRetryRunRequest) (app.TaskBoardMutation, error) {
	stub.retryRequests = append(stub.retryRequests, request)
	return app.TaskBoardMutation{TaskID: request.TaskID, RunID: request.RunID, Summary: "retried"}, stub.retryErr
}

func (stub *taskBoardGatewayStub) RetryAuthoringLaunch(_ context.Context, request app.TaskBoardRetryAuthoringLaunchRequest) (app.TaskBoardMutation, error) {
	stub.launchRetryRequests = append(stub.launchRetryRequests, request)
	return app.TaskBoardMutation{OperationID: request.OperationID, TaskID: "task-recovered", RunID: "run-recovered", Summary: "retried source capture"}, stub.launchRetryErr
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

func TestAppModelRoutesDurablePreTaskCaptureRetryWithoutNewIdempotencyKey(t *testing.T) {
	snapshot := taskBoardTestSnapshot(true)
	snapshot.PendingAuthoringLaunches = []app.TaskBoardAuthoringLaunch{{
		OperationID:    "019f65fb-7270-74f8-8a04-1a50c12c7cae",
		RepositoryURL:  "https://example.invalid/repo.git",
		CommitSHA:      "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		Slug:           "failed-source-capture",
		Title:          "Failed source capture",
		Status:         "source_capture_failed",
		FailureCode:    "authoring.source_capture_failed",
		FailureSummary: "Standard 创题源码捕获失败，尚未创建 Task。",
		CanRetry:       true,
	}}
	stub := &taskBoardGatewayStub{snapshot: snapshot}
	model := loadedTaskBoardModel(t, stub)
	if selected := model.board.SelectedTask(); selected == nil || selected.AuthoringLaunch == nil {
		t.Fatalf("pre-Task launch board selection = %+v", selected)
	}
	updated, _ := model.handleKey(keyRune('d'), nil)
	model = updated.(appModel)
	if model.detail == nil || !model.detail.hasAuthoringLaunch() || !strings.Contains(detailFooterText(model.detail), "[t] 重试源码捕获") {
		t.Fatalf("pre-Task launch detail = %+v footer=%q", model.detail, detailFooterText(model.detail))
	}
	updated, _ = model.handleKey(keyRune('t'), nil)
	model = updated.(appModel)
	if model.action == nil || model.action.kind != taskBoardRetryAuthoringLaunchAction || model.action.requiresReason {
		t.Fatalf("pre-Task capture retry prompt = %+v", model.action)
	}
	updated, command := model.handleKey(tea.KeyMsg{Type: tea.KeyEnter}, nil)
	model = updated.(appModel)
	if model.activeMutation != taskBoardRetryAuthoringLaunchMutation || command == nil {
		t.Fatalf("pre-Task capture retry start = active:%q command:%v", model.activeMutation, command)
	}
	message := command().(taskBoardMutationMsg)
	if message.kind != taskBoardRetryAuthoringLaunchMutation || len(stub.launchRetryRequests) != 1 ||
		stub.launchRetryRequests[0].OperationID != snapshot.PendingAuthoringLaunches[0].OperationID || stub.keys != 0 {
		t.Fatalf("pre-Task capture retry dispatch = message:%+v requests:%+v keys:%d", message, stub.launchRetryRequests, stub.keys)
	}
}

func TestAppModelPreviewsAuthoringRecoveryBeforeRetryAndRetainsItsIdempotencyKey(t *testing.T) {
	snapshot := taskBoardTestSnapshot(true)
	snapshot.Tasks[0].Runs[0].RetryStrategy = app.TaskBoardRetryStrategyAuthoringRecovery
	stub := &taskBoardGatewayStub{
		snapshot: snapshot,
		retryErr: errors.New("activation unavailable"),
		recoveryPreview: app.TaskBoardRecoveryPreview{
			CheckpointSequence: 9, CurrentExecutionEpoch: 1, NextExecutionEpoch: 2,
			Checkpoint:   workflowkit.CheckpointRef{Sequence: 9},
			TargetStages: []string{"authoring_harness"}, ReusedStages: []string{"repo_prepare", "repo_analyze"},
			ScheduledStages: []string{"authoring_harness", "tests_analysis"}, WorkflowFingerprint: "sha256:preview",
		},
	}
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
	if !model.recoveryPreviewInFlight || model.activeMutation != "" || command == nil {
		t.Fatalf("authoring recovery preview start = preview:%t active:%q command:%v", model.recoveryPreviewInFlight, model.activeMutation, command)
	}
	preview := command().(taskBoardRecoveryPreviewMsg)
	if len(stub.recoveryPreviews) != 1 || len(stub.retryRequests) != 0 || stub.recoveryPreviews[0].TaskID != "task-1" ||
		stub.recoveryPreviews[0].RunID != "run-1" || stub.recoveryPreviews[0].Reason != "recover transient provider failure" || stub.keys != 0 {
		t.Fatalf("authoring recovery preview dispatch = previews:%+v retries:%+v keys:%d", stub.recoveryPreviews, stub.retryRequests, stub.keys)
	}
	if stub.recoveryPreviewDeadline.IsZero() || time.Until(stub.recoveryPreviewDeadline) <= 0 || time.Until(stub.recoveryPreviewDeadline) > recoveryPreviewTimeout {
		t.Fatalf("authoring recovery preview deadline = %s", stub.recoveryPreviewDeadline)
	}
	updated, _ = model.Update(preview)
	model = updated.(appModel)
	if model.recoveryPreviewInFlight || model.action == nil || model.action.recoveryPreview == nil ||
		!strings.Contains(model.action.View(100), "断点恢复计划") || !strings.Contains(model.action.View(100), "Authoring harness 修复验证") {
		t.Fatalf("authoring recovery preview result = action:%+v", model.action)
	}

	updated, command = model.handleKey(tea.KeyMsg{Type: tea.KeyEnter}, nil)
	model = updated.(appModel)
	if model.activeMutation != taskBoardRetryMutation || command == nil || stub.keys != 1 {
		t.Fatalf("authoring recovery confirmation = active:%q command:%v keys:%d", model.activeMutation, command, stub.keys)
	}
	first := command().(taskBoardMutationMsg)
	if first.kind != taskBoardRetryMutation {
		t.Fatalf("authoring recovery message = %#v", first)
	}
	updated, _ = model.Update(first)
	model = updated.(appModel)
	if model.action == nil || model.action.recoveryPreview == nil || model.pendingAction == nil {
		t.Fatalf("failed recovery did not preserve approved recovery action: action=%+v pending=%+v", model.action, model.pendingAction)
	}

	updated, command = model.handleKey(tea.KeyMsg{Type: tea.KeyEnter}, nil)
	model = updated.(appModel)
	_ = command().(taskBoardMutationMsg)
	if len(stub.retryRequests) != 2 {
		t.Fatalf("authoring recovery dispatches = %+v", stub.retryRequests)
	}
	if first, replay := stub.retryRequests[0], stub.retryRequests[1]; first.IdempotencyKey == "" || first.IdempotencyKey != replay.IdempotencyKey || first.TaskID != "task-1" || first.RunID != "run-1" || first.Reason != "recover transient provider failure" || first.ExpectedRecoveryCheckpoint == nil || first.ExpectedRecoveryCheckpoint.Sequence != 9 || first.ExpectedRecoveryPlanFingerprint == "" || replay.ExpectedRecoveryCheckpoint == nil || *first.ExpectedRecoveryCheckpoint != *replay.ExpectedRecoveryCheckpoint || first.ExpectedRecoveryPlanFingerprint != replay.ExpectedRecoveryPlanFingerprint {
		t.Fatalf("authoring recovery idempotency/replay = first:%+v replay:%+v", first, replay)
	}
}

func TestAppModelPreviewsAuthoringAdmissionRepairBeforeRetry(t *testing.T) {
	snapshot := taskBoardTestSnapshot(true)
	snapshot.Tasks[0].Runs[0].Status = "waiting_continuation"
	snapshot.Tasks[0].Runs[0].RetryStrategy = app.TaskBoardRetryStrategyAuthoringAdmissionRepair
	stub := &taskBoardGatewayStub{
		snapshot: snapshot,
		recoveryPreview: app.TaskBoardRecoveryPreview{
			CheckpointSequence: 14, CurrentExecutionEpoch: 2, NextExecutionEpoch: 3,
			Checkpoint:          workflowkit.CheckpointRef{Sequence: 14},
			Strategy:            app.TaskBoardRetryStrategyAuthoringAdmissionRepair,
			TargetStages:        []string{"instruction_generate", "task_toml_generate", "dockerfile_generate"},
			ReusedStages:        []string{"repo_prepare", "repo_analyze", "task_design"},
			ScheduledStages:     []string{"instruction_generate", "task_toml_generate", "dockerfile_generate", "content_review"},
			WorkflowFingerprint: "sha256:preview",
		},
	}
	model := loadedTaskBoardModel(t, stub)
	model.detail = newDetailModel(model.board.SelectedTask())

	updated, _ := model.handleKey(keyRune('t'), nil)
	model = updated.(appModel)
	if model.action == nil || model.action.strategy != app.TaskBoardRetryStrategyAuthoringAdmissionRepair {
		t.Fatalf("authoring admission repair prompt = %+v", model.action)
	}
	model.action.reasonInput.SetValue("apply content-review correction")
	updated, command := model.handleKey(tea.KeyMsg{Type: tea.KeyEnter}, nil)
	model = updated.(appModel)
	if !model.recoveryPreviewInFlight || command == nil || len(stub.retryRequests) != 0 {
		t.Fatalf("authoring admission repair preview start = preview:%t command:%v retries:%+v", model.recoveryPreviewInFlight, command, stub.retryRequests)
	}
	preview := command().(taskBoardRecoveryPreviewMsg)
	if len(stub.recoveryPreviews) != 1 || stub.recoveryPreviews[0].Reason != "apply content-review correction" {
		t.Fatalf("authoring admission repair preview request = %+v", stub.recoveryPreviews)
	}
	updated, _ = model.Update(preview)
	model = updated.(appModel)
	if model.recoveryPreviewInFlight || model.action == nil || model.action.recoveryPreview == nil {
		t.Fatalf("authoring admission repair preview result = %+v", model.action)
	}
	updated, command = model.handleKey(tea.KeyMsg{Type: tea.KeyEnter}, nil)
	model = updated.(appModel)
	if model.activeMutation != taskBoardRetryMutation || command == nil || stub.keys != 1 {
		t.Fatalf("authoring admission repair confirmation = active:%q command:%v keys:%d", model.activeMutation, command, stub.keys)
	}
	_ = command().(taskBoardMutationMsg)
	if len(stub.retryRequests) != 1 || stub.retryRequests[0].Reason != "apply content-review correction" || stub.retryRequests[0].ExpectedRecoveryCheckpoint == nil || stub.retryRequests[0].ExpectedRecoveryCheckpoint.Sequence != 14 || stub.retryRequests[0].ExpectedRecoveryPlanFingerprint == "" {
		t.Fatalf("authoring admission repair retry request = %+v", stub.retryRequests)
	}
}

func TestAppModelClearsInvalidRecoveryPreviewState(t *testing.T) {
	snapshot := taskBoardTestSnapshot(true)
	snapshot.Tasks[0].Runs[0].RetryStrategy = app.TaskBoardRetryStrategyAuthoringRecovery
	stub := &taskBoardGatewayStub{snapshot: snapshot}
	model := loadedTaskBoardModel(t, stub)
	model.detail = newDetailModel(model.board.SelectedTask())

	updated, _ := model.handleKey(keyRune('t'), nil)
	model = updated.(appModel)
	model.action.reasonInput.SetValue("recover after stale preview")
	updated, _ = model.handleKey(tea.KeyMsg{Type: tea.KeyEnter}, nil)
	model = updated.(appModel)
	if !model.recoveryPreviewInFlight {
		t.Fatal("recovery preview was not started")
	}

	updated, _ = model.Update(taskBoardRecoveryPreviewMsg{
		epoch: model.recoveryPreviewEpoch, taskID: "other-task", runID: "run-1", reason: "recover after stale preview",
	})
	model = updated.(appModel)
	if model.recoveryPreviewInFlight || model.action == nil || !strings.Contains(model.action.validationErr, "已过期") {
		t.Fatalf("invalid recovery preview left the action blocked: preview:%t action:%+v", model.recoveryPreviewInFlight, model.action)
	}
}

func TestAppModelClearsStaleRecoveryPreviewBindingBeforeRetrying(t *testing.T) {
	snapshot := taskBoardTestSnapshot(true)
	snapshot.Tasks[0].Runs[0].RetryStrategy = app.TaskBoardRetryStrategyAuthoringRecovery
	stub := &taskBoardGatewayStub{snapshot: snapshot}
	model := loadedTaskBoardModel(t, stub)
	model.detail = newDetailModel(model.board.SelectedTask())

	updated, _ := model.handleKey(keyRune('t'), nil)
	model = updated.(appModel)
	model.action.reasonInput.SetValue("recover after a stale preview")
	updated, command := model.handleKey(tea.KeyMsg{Type: tea.KeyEnter}, nil)
	model = updated.(appModel)
	preview := command().(taskBoardRecoveryPreviewMsg)
	updated, _ = model.Update(preview)
	model = updated.(appModel)
	updated, command = model.handleKey(tea.KeyMsg{Type: tea.KeyEnter}, nil)
	model = updated.(appModel)
	if model.pendingAction == nil || model.pendingAction.key == "" || command == nil {
		t.Fatalf("confirmed recovery action = pending:%+v command:%v", model.pendingAction, command)
	}
	firstKey := model.pendingAction.key
	updated, _ = model.Update(taskBoardMutationMsg{kind: taskBoardRetryMutation, err: fmt.Errorf("wrapped: %w", app.ErrTaskBoardRecoveryPreviewStale)})
	model = updated.(appModel)
	if model.pendingAction != nil || model.action == nil || model.action.recoveryPreview != nil || !strings.Contains(model.action.validationErr, "已变化") || model.err != nil {
		t.Fatalf("stale recovery cleanup = pending:%+v action:%+v err:%v", model.pendingAction, model.action, model.err)
	}

	updated, command = model.handleKey(tea.KeyMsg{Type: tea.KeyEnter}, nil)
	model = updated.(appModel)
	if !model.recoveryPreviewInFlight || command == nil {
		t.Fatalf("stale recovery did not require a new preview: preview:%t command:%v", model.recoveryPreviewInFlight, command)
	}
	preview = command().(taskBoardRecoveryPreviewMsg)
	updated, _ = model.Update(preview)
	model = updated.(appModel)
	updated, command = model.handleKey(tea.KeyMsg{Type: tea.KeyEnter}, nil)
	model = updated.(appModel)
	if model.pendingAction == nil || stub.keys != 2 || command == nil {
		t.Fatalf("stale recovery did not allocate a new confirmation key: pending:%+v first:%q keys:%d command:%v", model.pendingAction, firstKey, stub.keys, command)
	}
}

func TestAppModelQueuesConfirmedRunActionsUntilRefreshCompletes(t *testing.T) {
	tests := []struct {
		name         string
		actionKey    rune
		configureRun func(*app.TaskBoardRun)
		wantMutation taskBoardMutationKind
	}{
		{
			name:      "authoring admission repair",
			actionKey: 't',
			configureRun: func(run *app.TaskBoardRun) {
				run.Status = "waiting_continuation"
				run.CanRetry = true
				run.RetryStrategy = app.TaskBoardRetryStrategyAuthoringAdmissionRepair
			},
			wantMutation: taskBoardRetryMutation,
		},
		{
			name:      "cancel",
			actionKey: 'x',
			configureRun: func(run *app.TaskBoardRun) {
				run.Status = "running"
			},
			wantMutation: taskBoardCancelMutation,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := taskBoardTestSnapshot(true)
			test.configureRun(&snapshot.Tasks[0].Runs[0])
			stub := &taskBoardGatewayStub{snapshot: snapshot}
			model := loadedTaskBoardModel(t, stub)
			model.detail = newDetailModel(model.board.SelectedTask())

			updated, _ := model.handleKey(keyRune(test.actionKey), nil)
			model = updated.(appModel)
			if model.action == nil {
				t.Fatal("run action prompt was not opened")
			}
			model.action.reasonInput.SetValue("confirm while the board is refreshing")
			if test.wantMutation == taskBoardRetryMutation {
				updated, previewCommand := model.handleKey(tea.KeyMsg{Type: tea.KeyEnter}, nil)
				model = updated.(appModel)
				if !model.recoveryPreviewInFlight || previewCommand == nil {
					t.Fatalf("authoring repair preview was not started: preview:%t command:%v", model.recoveryPreviewInFlight, previewCommand)
				}
				preview := previewCommand().(taskBoardRecoveryPreviewMsg)
				updated, _ = model.Update(preview)
				model = updated.(appModel)
				if model.action == nil || model.action.recoveryPreview == nil {
					t.Fatalf("authoring repair preview was not accepted: %+v", model.action)
				}
			}
			model.refreshInFlight = true

			updated, command := model.handleKey(tea.KeyMsg{Type: tea.KeyEnter}, nil)
			model = updated.(appModel)
			if command != nil || model.activeMutation != "" || model.deferredAction == nil {
				t.Fatalf("first confirmation was not deferred: command:%v active:%q deferred:%+v", command, model.activeMutation, model.deferredAction)
			}
			queuedKey := model.deferredAction.key
			if queuedKey == "" || stub.keys != 1 {
				t.Fatalf("first confirmation key = %q, generated keys=%d", queuedKey, stub.keys)
			}

			// Repeated Enter while the same refresh is in flight must not create a
			// second action or a second idempotency key.
			updated, command = model.handleKey(tea.KeyMsg{Type: tea.KeyEnter}, nil)
			model = updated.(appModel)
			if command != nil || model.deferredAction == nil || model.deferredAction.key != queuedKey || stub.keys != 1 {
				t.Fatalf("repeat confirmation changed the queued action: command:%v deferred:%+v keys:%d", command, model.deferredAction, stub.keys)
			}

			updated, command = model.Update(taskBoardLoadedMsg{snapshot: snapshot, epoch: model.refreshEpoch})
			model = updated.(appModel)
			if command == nil || model.activeMutation != test.wantMutation || model.deferredAction != nil {
				t.Fatalf("refresh completion did not dispatch the queued action: command:%v active:%q deferred:%+v", command, model.activeMutation, model.deferredAction)
			}
			message, ok := command().(taskBoardMutationMsg)
			if !ok || message.kind != test.wantMutation {
				t.Fatalf("queued action result = %#v, want %q", message, test.wantMutation)
			}
			switch test.wantMutation {
			case taskBoardRetryMutation:
				if len(stub.retryRequests) != 1 || stub.retryRequests[0].IdempotencyKey != queuedKey || stub.retryRequests[0].Reason != "confirm while the board is refreshing" {
					t.Fatalf("deferred retry request = %+v, queued key=%q", stub.retryRequests, queuedKey)
				}
			case taskBoardCancelMutation:
				if len(stub.cancelRequests) != 1 || stub.cancelRequests[0].IdempotencyKey != queuedKey || stub.cancelRequests[0].Reason != "confirm while the board is refreshing" {
					t.Fatalf("deferred cancel request = %+v, queued key=%q", stub.cancelRequests, queuedKey)
				}
			}
		})
	}
}

func TestAppModelCancelsDeferredRunActionBeforeRefreshCompletes(t *testing.T) {
	stub := &taskBoardGatewayStub{snapshot: taskBoardTestSnapshot(true)}
	model := loadedTaskBoardModel(t, stub)
	model.detail = newDetailModel(model.board.SelectedTask())

	updated, _ := model.handleKey(keyRune('t'), nil)
	model = updated.(appModel)
	model.action.reasonInput.SetValue("do not send this retry")
	model.refreshInFlight = true
	updated, _ = model.handleKey(tea.KeyMsg{Type: tea.KeyEnter}, nil)
	model = updated.(appModel)
	if model.deferredAction == nil {
		t.Fatal("retry confirmation was not deferred")
	}

	updated, command := model.handleKey(tea.KeyMsg{Type: tea.KeyEsc}, nil)
	model = updated.(appModel)
	if command != nil || model.action != nil || model.deferredAction != nil {
		t.Fatalf("escape did not cancel the deferred action: command:%v action:%+v deferred:%+v", command, model.action, model.deferredAction)
	}
	updated, command = model.Update(taskBoardLoadedMsg{snapshot: stub.snapshot, epoch: model.refreshEpoch})
	model = updated.(appModel)
	if command != nil || model.activeMutation != "" || len(stub.retryRequests) != 0 {
		t.Fatalf("canceled deferred action was dispatched: command:%v active:%q requests:%+v", command, model.activeMutation, stub.retryRequests)
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
	if !model.recoveryPreviewInFlight || command == nil {
		t.Fatalf("repair continuation preview start = preview:%t command:%v", model.recoveryPreviewInFlight, command)
	}
	preview := command().(taskBoardRecoveryPreviewMsg)
	updated, _ = model.Update(preview)
	model = updated.(appModel)
	updated, command = model.handleKey(tea.KeyMsg{Type: tea.KeyEnter}, nil)
	model = updated.(appModel)
	if model.activeMutation != taskBoardRetryMutation || command == nil {
		t.Fatalf("repair continuation confirmation = active:%q command:%v", model.activeMutation, command)
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
