package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/purplevoid/harbor-factory/internal/app"
)

// RunNewTUIAdapter matches the lifecycleTUIRunner signature used by cmd/tui_v2.go.
func RunNewTUIAdapter(ctx context.Context, services *app.LifecycleServices) error {
	return RunNewTUI(ctx, services)
}

// RunNewTUI launches the task-board TUI backed by the application-layer task
// board gateway.
func RunNewTUI(ctx context.Context, services *app.LifecycleServices) error {
	if ctx == nil {
		ctx = context.Background()
	}
	model := newAppModel(ctx, services)
	program := tea.NewProgram(model, tea.WithAltScreen(), tea.WithContext(ctx))
	_, err := program.Run()
	return err
}

// appModel is the top-level model for the task-board TUI.
type appModel struct {
	ctx     context.Context
	gateway taskBoardGateway

	board  TaskBoardModel
	input  TaskInputModel
	detail *detailModel
	logs   *logModel
	review *reviewPrompt
	action *runActionPrompt

	width              int
	height             int
	err                error
	notice             string
	authoringAvailable bool

	pendingStart   *pendingTaskBoardStart
	pendingReview  *pendingTaskBoardReview
	pendingAction  *pendingTaskBoardRunAction
	activeMutation taskBoardMutationKind
	logEpoch       uint64

	refreshInFlight  bool
	refreshRequested bool
	refreshEpoch     uint64
	exitInFlight     bool
	exitFlushFailed  bool
}

type taskBoardGateway = app.TaskBoardGateway

type taskBoardLoadedMsg struct {
	snapshot app.TaskBoardSnapshot
	epoch    uint64
	err      error
}

type taskBoardMutationMsg struct {
	kind     taskBoardMutationKind
	mutation app.TaskBoardMutation
	err      error
}

type taskBoardLogMsg struct {
	log   app.TaskBoardLog
	epoch uint64
	err   error
}

type taskBoardExitMsg struct{ err error }

type taskBoardMutationKind string

const (
	taskBoardStartMutation  taskBoardMutationKind = "start_authoring"
	taskBoardReviewMutation taskBoardMutationKind = "review"
	taskBoardRetryMutation  taskBoardMutationKind = "retry_run"
	taskBoardCancelMutation taskBoardMutationKind = "cancel_run"
)

type taskBoardRunActionKind string

const (
	taskBoardRetryAction  taskBoardRunActionKind = "retry"
	taskBoardCancelAction taskBoardRunActionKind = "cancel"
)

type pendingTaskBoardStart struct {
	message TaskSubmitMsg
	key     string
}

type pendingTaskBoardReview struct {
	taskID   string
	review   app.TaskBoardReview
	decision app.TaskBoardReviewDecision
	reason   string
	key      string
}

type pendingTaskBoardRunAction struct {
	kind   taskBoardRunActionKind
	taskID string
	runID  string
	reason string
	key    string
}

type reviewPrompt struct {
	decision      app.TaskBoardReviewDecision
	reasonInput   textinput.Model
	validationErr string
}

func newReviewPrompt(decision app.TaskBoardReviewDecision) *reviewPrompt {
	input := textinput.New()
	input.Prompt = "原因 "
	input.Placeholder = "说明审核决定的原因"
	input.CharLimit = 240
	input.Width = 52
	input.Focus()
	return &reviewPrompt{decision: decision, reasonInput: input}
}

func (prompt *reviewPrompt) View(width int) string {
	content := prompt.reasonInput.View()
	if prompt.validationErr != "" {
		content += "\n" + failStyleV2.Render(prompt.validationErr)
	}
	return inputStyle.Width(max(1, width)).Render(content)
}

type runActionPrompt struct {
	kind          taskBoardRunActionKind
	strategy      app.TaskBoardRetryStrategy
	reasonInput   textinput.Model
	validationErr string
}

func newRunActionPrompt(kind taskBoardRunActionKind, strategy app.TaskBoardRetryStrategy) *runActionPrompt {
	input := textinput.New()
	input.Prompt = "原因 "
	input.Placeholder = "记录本次操作的原因"
	input.CharLimit = 240
	input.Width = 52
	input.Focus()
	return &runActionPrompt{kind: kind, strategy: strategy, reasonInput: input}
}

func (prompt *runActionPrompt) View(width int) string {
	label := "重试当前 Run"
	switch prompt.kind {
	case taskBoardRetryAction:
		if prompt.strategy == app.TaskBoardRetryStrategyAuthoringRecovery {
			label = "恢复/重试创题 Run"
		}
	case taskBoardCancelAction:
		label = "取消当前 Run"
	}
	content := detailSectionTitleStyle.Render(label) + "\n" + prompt.reasonInput.View()
	if prompt.validationErr != "" {
		content += "\n" + failStyleV2.Render(prompt.validationErr)
	}
	return inputStyle.Width(max(1, width)).Render(content)
}

func newAppModel(ctx context.Context, services *app.LifecycleServices) appModel {
	var gateway taskBoardGateway
	if services != nil {
		gateway = services.TaskBoard
	}
	return newAppModelWithGateway(ctx, gateway)
}

func newAppModelWithGateway(ctx context.Context, gateway taskBoardGateway) appModel {
	return appModel{
		ctx:             ctx,
		gateway:         gateway,
		board:           NewTaskBoardModel(),
		input:           NewTaskInputModel(),
		refreshInFlight: true,
		refreshEpoch:    1,
	}
}

func (m appModel) Init() tea.Cmd {
	return tea.Batch(
		m.refreshTasks(m.refreshEpoch),
		m.pollTick(),
		textinput.Blink,
	)
}

func (m appModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Let visible inputs process cursor blink messages without changing board
	// selection or lifecycle state.
	var inputCmd tea.Cmd
	if _, ok := msg.(tea.KeyMsg); !ok {
		var commands []tea.Cmd
		if m.input.Visible() {
			command, _ := m.input.Update(msg)
			commands = append(commands, command)
		}
		if m.review != nil {
			var command tea.Cmd
			m.review.reasonInput, command = m.review.reasonInput.Update(msg)
			commands = append(commands, command)
		}
		if m.action != nil {
			var command tea.Cmd
			m.action.reasonInput, command = m.action.reasonInput.Update(msg)
			commands = append(commands, command)
		}
		inputCmd = tea.Batch(commands...)
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, inputCmd

	case tea.KeyMsg:
		return m.handleKey(msg, inputCmd)

	case TaskSubmitMsg:
		return m.beginAuthoring(msg, inputCmd)

	case tickMsg:
		if m.exitInFlight || m.mutationInFlight() {
			return m, tea.Batch(inputCmd, m.pollTick())
		}
		var refreshCmd tea.Cmd
		m, refreshCmd = m.requestRefresh()
		return m, tea.Batch(inputCmd, refreshCmd, m.pollTick())

	case taskBoardLoadedMsg:
		if msg.epoch != 0 && msg.epoch != m.refreshEpoch {
			return m, inputCmd
		}
		m.refreshInFlight = false
		if msg.err != nil {
			m.err = msg.err
		} else {
			m.board.SetTasks(taskItemsForSnapshot(msg.snapshot))
			m.authoringAvailable = msg.snapshot.AuthoringAvailable
			if m.pendingStart == nil && m.pendingReview == nil && m.pendingAction == nil {
				m.err = nil
			}
		}
		if m.refreshRequested && !m.exitInFlight {
			m.refreshRequested = false
			var refreshCmd tea.Cmd
			m, refreshCmd = m.requestRefresh()
			return m, tea.Batch(inputCmd, refreshCmd)
		}
		return m, inputCmd

	case taskBoardMutationMsg:
		m.activeMutation = ""
		if msg.err != nil {
			m.err = msg.err
			return m, inputCmd
		}
		m.err = nil
		m.notice = msg.mutation.Summary
		switch msg.kind {
		case taskBoardStartMutation:
			m.pendingStart = nil
			m.input.Reset()
			m.input.Hide()
		case taskBoardReviewMutation:
			m.pendingReview = nil
			m.review = nil
			m.detail = nil
		case taskBoardRetryMutation, taskBoardCancelMutation:
			m.pendingAction = nil
			m.action = nil
			m.logs = nil
			m.detail = nil
		}
		var refreshCmd tea.Cmd
		m, refreshCmd = m.requestRefresh()
		return m, tea.Batch(inputCmd, refreshCmd)

	case taskBoardLogMsg:
		if msg.epoch != m.logEpoch || m.logs == nil {
			return m, inputCmd
		}
		if msg.err != nil {
			m.logs = newLogModel(m.detailTask(), app.TaskBoardLog{RunID: m.logs.runID, Path: m.logs.path, Message: msg.err.Error()})
			return m, inputCmd
		}
		m.logs = newLogModel(m.detailTask(), msg.log)
		return m, inputCmd

	case taskBoardExitMsg:
		m.exitInFlight = false
		if msg.err != nil {
			m.err = msg.err
			m.exitFlushFailed = true
			return m, inputCmd
		}
		return m, tea.Quit

	case error:
		m.err = msg
		return m, inputCmd
	}

	return m, inputCmd
}

func (m appModel) handleKey(msg tea.KeyMsg, inputCmd tea.Cmd) (tea.Model, tea.Cmd) {
	key := msg.String()

	if m.logs != nil {
		return m.handleLogKey(msg, inputCmd)
	}

	if m.action != nil {
		return m.handleRunActionPromptKey(msg, inputCmd)
	}

	if m.review != nil {
		return m.handleReviewPromptKey(msg, inputCmd)
	}

	// A visible form owns every key. In particular, typing d/a/r must never
	// leak into detail or review commands behind the form.
	if m.input.Visible() {
		if m.mutationInFlight() {
			m.notice = "操作仍在提交，请等待结果"
			return m, inputCmd
		}
		command, _ := m.input.Update(msg)
		return m, tea.Batch(inputCmd, command)
	}

	if m.detail != nil {
		return m.handleDetailKey(msg, inputCmd)
	}

	switch key {
	case "esc":
		return m, inputCmd

	case "q", "ctrl+c":
		if m.exitInFlight {
			return m, inputCmd
		}
		if m.mutationInFlight() || m.refreshInFlight {
			m.notice = "操作仍在进行，请等待结果后再退出"
			return m, inputCmd
		}
		if key == "ctrl+c" && m.exitFlushFailed {
			return m, tea.Quit
		}
		return m.beginExit()

	case "n":
		if !m.authoringAvailable {
			m.err = app.ErrStandardAuthoringLaunchUnavailable
			return m, inputCmd
		}
		if m.refreshInFlight {
			m.notice = "正在刷新任务状态"
			return m, inputCmd
		}
		m.input.Show()
		return m, textinput.Blink

	case "tab":
		m.board.MoveRight()
		return m, inputCmd

	case "shift+tab":
		m.board.MoveLeft()
		return m, inputCmd

	case "up", "k":
		m.board.MoveUp()
		return m, inputCmd

	case "down", "j":
		m.board.MoveDown()
		return m, inputCmd

	case "left", "h":
		m.board.MoveLeft()
		return m, inputCmd

	case "right", "l":
		m.board.MoveRight()
		return m, inputCmd

	case "d":
		if t := m.board.SelectedTask(); t != nil {
			m.detail = newDetailModel(t)
		}
		return m, inputCmd

	}

	return m, inputCmd
}

func (m appModel) handleDetailKey(msg tea.KeyMsg, inputCmd tea.Cmd) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q", "ctrl+c":
		m.detail = nil
		return m, inputCmd
	case "l":
		return m.openLog(inputCmd)
	case "t":
		return m.openRunActionPrompt(taskBoardRetryAction, inputCmd)
	case "x":
		return m.openRunActionPrompt(taskBoardCancelAction, inputCmd)
	case "a":
		if m.detail.task.Review != nil {
			return m.openReviewPrompt(app.TaskBoardApprove, inputCmd)
		}
	case "r":
		if m.detail.task.Review != nil {
			return m.openReviewPrompt(app.TaskBoardRequestChanges, inputCmd)
		}
	}
	return m, inputCmd
}

func (m appModel) handleLogKey(msg tea.KeyMsg, inputCmd tea.Cmd) (tea.Model, tea.Cmd) {
	if m.logs == nil {
		return m, inputCmd
	}
	width, height := m.logDimensions()
	switch msg.String() {
	case "esc", "q", "ctrl+c":
		m.logs = nil
		m.logEpoch++
		return m, inputCmd
	case "up", "k":
		m.logs.MoveUp(width, height)
	case "down", "j":
		m.logs.MoveDown(width, height)
	case "pgup", "ctrl+u":
		m.logs.PageUp(width, height)
	case "pgdown", "ctrl+d":
		m.logs.PageDown(width, height)
	case "home", "g":
		m.logs.GoToStart()
	case "end", "G":
		m.logs.GoToEnd(width, height)
	case "r":
		return m.openLog(inputCmd)
	}
	return m, inputCmd
}

func (m appModel) handleReviewPromptKey(msg tea.KeyMsg, inputCmd tea.Cmd) (tea.Model, tea.Cmd) {
	if m.mutationInFlight() {
		m.notice = "操作仍在提交，请等待结果"
		return m, inputCmd
	}
	switch msg.String() {
	case "esc":
		m.review = nil
		return m, inputCmd
	case "enter":
		reason := strings.TrimSpace(m.review.reasonInput.Value())
		if reason == "" {
			m.review.validationErr = "审核原因不能为空"
			return m, inputCmd
		}
		return m.beginReview(m.review.decision, reason, inputCmd)
	}
	var command tea.Cmd
	m.review.reasonInput, command = m.review.reasonInput.Update(msg)
	return m, tea.Batch(inputCmd, command)
}

func (m appModel) openReviewPrompt(decision app.TaskBoardReviewDecision, inputCmd tea.Cmd) (tea.Model, tea.Cmd) {
	if m.mutationInFlight() || m.detail == nil || m.detail.task.Review == nil {
		return m, inputCmd
	}
	m.review = newReviewPrompt(decision)
	return m, tea.Batch(inputCmd, textinput.Blink)
}

func (m appModel) handleRunActionPromptKey(msg tea.KeyMsg, inputCmd tea.Cmd) (tea.Model, tea.Cmd) {
	if m.action == nil {
		return m, inputCmd
	}
	if m.mutationInFlight() {
		m.notice = "操作仍在提交，请等待结果"
		return m, inputCmd
	}
	switch msg.String() {
	case "esc":
		m.action = nil
		return m, inputCmd
	case "enter":
		reason := strings.TrimSpace(m.action.reasonInput.Value())
		if reason == "" {
			m.action.validationErr = "操作原因不能为空"
			return m, inputCmd
		}
		return m.beginRunAction(m.action.kind, reason, inputCmd)
	}
	var command tea.Cmd
	m.action.reasonInput, command = m.action.reasonInput.Update(msg)
	return m, tea.Batch(inputCmd, command)
}

func (m appModel) openRunActionPrompt(kind taskBoardRunActionKind, inputCmd tea.Cmd) (tea.Model, tea.Cmd) {
	if m.mutationInFlight() || m.detail == nil || !m.detail.hasCurrentRun() {
		return m, inputCmd
	}
	if isRetryAction(kind) && !m.detail.canRetryCurrentRun() {
		m.notice = m.detail.currentRun().RetryReason
		if m.notice == "" {
			m.notice = "当前 Run 不可重试"
		}
		return m, inputCmd
	}
	if kind == taskBoardCancelAction && !m.detail.canCancelCurrentRun() {
		m.notice = "当前 Run 已是终态，无法取消"
		return m, inputCmd
	}
	m.action = newRunActionPrompt(kind, m.detail.currentRun().RetryStrategy)
	return m, tea.Batch(inputCmd, textinput.Blink)
}

func isRetryAction(kind taskBoardRunActionKind) bool {
	return kind == taskBoardRetryAction
}

func (m appModel) openLog(inputCmd tea.Cmd) (tea.Model, tea.Cmd) {
	if m.detail == nil || !m.detail.hasCurrentRun() {
		m.notice = "当前题目尚无 Run 日志"
		return m, inputCmd
	}
	run := m.detail.currentRun()
	m.logEpoch++
	m.logs = newLogModel(m.detail.task, app.TaskBoardLog{
		RunID:   run.ID,
		Path:    run.LogPath,
		Message: "正在读取日志...",
	})
	return m, tea.Batch(inputCmd, m.readRunLog(m.detail.task.ID, run.ID, m.logEpoch))
}

func (m appModel) detailTask() *TaskItem {
	if m.detail == nil {
		return nil
	}
	return m.detail.task
}

func (m appModel) logDimensions() (int, int) {
	return m.contentWidth(), max(8, m.height-3)
}

func (m appModel) contentWidth() int {
	return max(1, m.width-2)
}

func (m appModel) mutationInFlight() bool {
	return m.activeMutation != ""
}

func (m appModel) requestRefresh() (appModel, tea.Cmd) {
	if m.exitInFlight {
		return m, nil
	}
	if m.refreshInFlight {
		m.refreshRequested = true
		return m, nil
	}
	m.refreshInFlight = true
	m.refreshEpoch++
	return m, m.refreshTasks(m.refreshEpoch)
}

func (m appModel) refreshTasks(epoch uint64) tea.Cmd {
	return func() tea.Msg {
		if m.gateway == nil {
			return taskBoardLoadedMsg{epoch: epoch, err: fmt.Errorf("task board service is not configured")}
		}
		if err := m.gateway.FlushQueuedRuns(m.ctx); err != nil {
			return taskBoardLoadedMsg{epoch: epoch, err: err}
		}
		snapshot, err := m.gateway.List(m.ctx)
		return taskBoardLoadedMsg{snapshot: snapshot, epoch: epoch, err: err}
	}
}

func (m appModel) beginAuthoring(message TaskSubmitMsg, inputCmd tea.Cmd) (tea.Model, tea.Cmd) {
	if m.mutationInFlight() || m.refreshInFlight {
		m.notice = "请等待当前操作完成"
		return m, inputCmd
	}
	message.RepoURL = strings.TrimSpace(message.RepoURL)
	message.CommitSHA = strings.TrimSpace(message.CommitSHA)
	message.BaseImage = strings.TrimSpace(message.BaseImage)
	message.Slug = strings.TrimSpace(message.Slug)
	message.Title = strings.TrimSpace(message.Title)
	message.Reason = strings.TrimSpace(message.Reason)
	if m.pendingStart == nil || m.pendingStart.message != message {
		key, err := m.newIdempotencyKey()
		if err != nil {
			m.err = err
			return m, inputCmd
		}
		m.pendingStart = &pendingTaskBoardStart{message: message, key: key}
	}
	m.activeMutation = taskBoardStartMutation
	return m, tea.Batch(inputCmd, m.startAuthoring(*m.pendingStart))
}

func (m appModel) startAuthoring(pending pendingTaskBoardStart) tea.Cmd {
	return func() tea.Msg {
		if m.gateway == nil {
			return taskBoardMutationMsg{kind: taskBoardStartMutation, err: fmt.Errorf("task board service is not configured")}
		}
		mutation, err := m.gateway.StartAuthoring(m.ctx, app.TaskBoardStartAuthoringRequest{
			IdempotencyKey: pending.key,
			RepositoryURL:  pending.message.RepoURL,
			CommitSHA:      pending.message.CommitSHA,
			BaseImage:      pending.message.BaseImage,
			Slug:           pending.message.Slug,
			Title:          pending.message.Title,
			MetadataJSON:   "{}",
			Reason:         pending.message.Reason,
		})
		return taskBoardMutationMsg{kind: taskBoardStartMutation, mutation: mutation, err: err}
	}
}

func (m appModel) beginReview(decision app.TaskBoardReviewDecision, reason string, inputCmd tea.Cmd) (tea.Model, tea.Cmd) {
	if m.mutationInFlight() || m.refreshInFlight || m.detail == nil || m.detail.task.Review == nil {
		m.notice = "请等待当前操作完成"
		return m, inputCmd
	}
	current := pendingTaskBoardReview{taskID: m.detail.task.ID, review: *m.detail.task.Review, decision: decision, reason: reason}
	if m.pendingReview == nil || m.pendingReview.taskID != current.taskID || m.pendingReview.review != current.review || m.pendingReview.decision != current.decision || m.pendingReview.reason != current.reason {
		key, err := m.newIdempotencyKey()
		if err != nil {
			m.err = err
			return m, inputCmd
		}
		current.key = key
		m.pendingReview = &current
	}
	m.activeMutation = taskBoardReviewMutation
	return m, tea.Batch(inputCmd, m.decideReview(*m.pendingReview))
}

func (m appModel) decideReview(pending pendingTaskBoardReview) tea.Cmd {
	return func() tea.Msg {
		if m.gateway == nil {
			return taskBoardMutationMsg{kind: taskBoardReviewMutation, err: fmt.Errorf("task board service is not configured")}
		}
		mutation, err := m.gateway.DecideReview(m.ctx, app.TaskBoardDecideReviewRequest{
			IdempotencyKey: pending.key,
			TaskID:         pending.taskID,
			Review:         pending.review,
			Decision:       pending.decision,
			Reason:         pending.reason,
		})
		return taskBoardMutationMsg{kind: taskBoardReviewMutation, mutation: mutation, err: err}
	}
}

func (m appModel) beginRunAction(kind taskBoardRunActionKind, reason string, inputCmd tea.Cmd) (tea.Model, tea.Cmd) {
	if m.mutationInFlight() || m.refreshInFlight || m.detail == nil || !m.detail.hasCurrentRun() {
		m.notice = "请等待当前操作完成"
		return m, inputCmd
	}
	run := m.detail.currentRun()
	current := pendingTaskBoardRunAction{
		kind: kind, taskID: m.detail.task.ID, runID: run.ID, reason: strings.TrimSpace(reason),
	}
	if m.pendingAction == nil || m.pendingAction.kind != current.kind || m.pendingAction.taskID != current.taskID || m.pendingAction.runID != current.runID || m.pendingAction.reason != current.reason {
		key, err := m.newIdempotencyKey()
		if err != nil {
			m.err = err
			return m, inputCmd
		}
		current.key = key
		m.pendingAction = &current
	}
	switch kind {
	case taskBoardRetryAction:
		m.activeMutation = taskBoardRetryMutation
	case taskBoardCancelAction:
		m.activeMutation = taskBoardCancelMutation
	default:
		m.err = fmt.Errorf("unsupported task board Run action %q", kind)
		return m, inputCmd
	}
	return m, tea.Batch(inputCmd, m.runAction(*m.pendingAction))
}

func (m appModel) runAction(pending pendingTaskBoardRunAction) tea.Cmd {
	return func() tea.Msg {
		if m.gateway == nil {
			kind := taskBoardRetryMutation
			if pending.kind == taskBoardCancelAction {
				kind = taskBoardCancelMutation
			}
			return taskBoardMutationMsg{kind: kind, err: fmt.Errorf("task board service is not configured")}
		}
		switch pending.kind {
		case taskBoardRetryAction:
			mutation, err := m.gateway.RetryRun(m.ctx, app.TaskBoardRetryRunRequest{
				IdempotencyKey: pending.key, TaskID: pending.taskID, RunID: pending.runID, Reason: pending.reason,
			})
			return taskBoardMutationMsg{kind: taskBoardRetryMutation, mutation: mutation, err: err}
		case taskBoardCancelAction:
			mutation, err := m.gateway.CancelRun(m.ctx, app.TaskBoardCancelRunRequest{
				IdempotencyKey: pending.key, TaskID: pending.taskID, RunID: pending.runID, Reason: pending.reason,
			})
			return taskBoardMutationMsg{kind: taskBoardCancelMutation, mutation: mutation, err: err}
		default:
			return taskBoardMutationMsg{err: fmt.Errorf("unsupported task board Run action %q", pending.kind)}
		}
	}
}

func (m appModel) readRunLog(taskID, runID string, epoch uint64) tea.Cmd {
	return func() tea.Msg {
		if m.gateway == nil {
			return taskBoardLogMsg{epoch: epoch, err: fmt.Errorf("task board service is not configured")}
		}
		log, err := m.gateway.ReadRunLog(m.ctx, app.TaskBoardReadRunLogRequest{TaskID: taskID, RunID: runID})
		return taskBoardLogMsg{log: log, epoch: epoch, err: err}
	}
}

func (m appModel) newIdempotencyKey() (string, error) {
	if m.gateway == nil {
		return "", fmt.Errorf("task board service is not configured")
	}
	return m.gateway.NewIdempotencyKey()
}

func (m appModel) flushBeforeExit() tea.Cmd {
	return func() tea.Msg {
		if m.gateway == nil {
			return taskBoardExitMsg{err: fmt.Errorf("task board service is not configured")}
		}
		return taskBoardExitMsg{err: m.gateway.FlushQueuedRuns(m.ctx)}
	}
}

func (m appModel) beginExit() (tea.Model, tea.Cmd) {
	if m.exitInFlight || m.mutationInFlight() || m.refreshInFlight {
		return m, nil
	}
	m.exitInFlight = true
	m.exitFlushFailed = false
	return m, m.flushBeforeExit()
}

func taskItemsForSnapshot(snapshot app.TaskBoardSnapshot) (pending, running, completed []TaskItem) {
	for _, task := range snapshot.Tasks {
		item := TaskItem{
			ID:           task.ID,
			Slug:         task.Slug,
			Name:         task.Title,
			RepoURL:      task.RepositoryURL,
			CommitSHA:    task.CommitSHA,
			RunID:        task.RunID,
			CurrentStage: task.CurrentStage,
			RunStatus:    task.RunStatus,
			Lifecycle:    task.LifecycleState,
			Review:       task.Review,
			OpenReviews:  task.OpenReviewCount,
			Runs:         make([]TaskRunItem, 0, len(task.Runs)),
		}
		for _, run := range task.Runs {
			item.Runs = append(item.Runs, TaskRunItem{
				ID:            run.ID,
				Status:        run.Status,
				CurrentStage:  run.CurrentStage,
				FailureStage:  run.FailureStage,
				FailureClass:  run.FailureClass,
				FailureReason: run.FailureReason,
				CreatedAt:     run.CreatedAt,
				StartedAt:     run.StartedAt,
				FinishedAt:    run.FinishedAt,
				LogPath:       run.LogPath,
				HasLog:        run.HasLog,
				CanRetry:      run.CanRetry,
				RetryReason:   run.RetryReason,
				RetryStrategy: run.RetryStrategy,
			})
		}
		switch task.Column {
		case app.TaskBoardRunning:
			item.State = TaskRunning
			running = append(running, item)
		case app.TaskBoardCompleted:
			item.State = TaskCompleted
			completed = append(completed, item)
		default:
			item.State = TaskPending
			pending = append(pending, item)
		}
	}
	return pending, running, completed
}

func (m appModel) View() string {
	if m.width == 0 {
		return "loading..."
	}
	contentWidth := m.contentWidth()

	if m.logs != nil {
		width, height := m.logDimensions()
		return appStyle.Render(lipgloss.JoinVertical(lipgloss.Top,
			headerStyle.Width(contentWidth).Render("Harbor Task Factory"),
			m.logs.View(width, height),
			m.statusView(),
			footerStyle.Render("[↑↓/jk] 滚动  [PgUp/PgDn] 翻页  [r] 刷新  [q] 返回详情"),
		))
	}

	if m.detail != nil {
		footer := detailFooter(m.detail)
		if m.detail.task.Review != nil {
			footer = footerStyle.Render("[a] 通过  [r] 返修  " + detailFooterText(m.detail))
		}
		prompt := ""
		detailHeight := max(8, m.height-3)
		if m.review != nil {
			prompt = m.review.View(contentWidth)
			detailHeight -= 4
			footer = footerStyle.Render("[enter] 提交审核  [esc] 取消")
		}
		if m.action != nil {
			prompt = m.action.View(contentWidth)
			detailHeight -= 5
			footer = footerStyle.Render("[enter] 确认操作  [esc] 取消")
		}
		return appStyle.Render(lipgloss.JoinVertical(lipgloss.Top,
			headerStyle.Width(contentWidth).Render("Harbor Task Factory"),
			m.detail.View(contentWidth, max(1, detailHeight)),
			prompt,
			m.statusView(),
			footer,
		))
	}
	status := m.statusView()

	boardHeight := m.height - 5
	if m.input.Visible() {
		boardHeight -= 5
	}
	if status != "" {
		boardHeight--
	}
	footer := "[tab] 切换面板  [hjkl/↑↓←→] 导航  [d] 详情  [q] 退出"
	if m.authoringAvailable {
		footer = "[n] 新任务  " + footer
	}
	return appStyle.Render(lipgloss.JoinVertical(lipgloss.Top,
		headerStyle.Width(contentWidth).Render("Harbor Task Factory"),
		m.board.View(contentWidth, max(1, boardHeight)),
		m.input.View(contentWidth),
		status,
		footerStyle.Render(footer),
	))
}

func detailFooter(detail *detailModel) string {
	return footerStyle.Render(detailFooterText(detail))
}

func detailFooterText(detail *detailModel) string {
	actions := make([]string, 0, 4)
	if detail != nil && detail.hasCurrentRun() {
		actions = append(actions, "[l] 日志")
		if detail.canRetryCurrentRun() {
			label := "重试"
			if detail.currentRun().RetryStrategy == app.TaskBoardRetryStrategyAuthoringRecovery {
				label = "恢复/重试"
			}
			actions = append(actions, "[t] "+label)
		}
		if detail.canCancelCurrentRun() {
			actions = append(actions, "[x] 取消")
		}
	}
	actions = append(actions, "[q] 返回")
	return strings.Join(actions, "  ")
}

func (m appModel) statusView() string {
	if m.err != nil {
		return failStyleV2.Render(m.err.Error())
	}
	if m.notice != "" {
		return passStyleV2.Render(m.notice)
	}
	return ""
}

type tickMsg struct{}

func (m appModel) pollTick() tea.Cmd {
	return tea.Tick(2*time.Second, func(t time.Time) tea.Msg {
		return tickMsg{}
	})
}
