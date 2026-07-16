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
	review *reviewPrompt

	width              int
	height             int
	err                error
	notice             string
	authoringAvailable bool

	pendingStart   *pendingTaskBoardStart
	pendingReview  *pendingTaskBoardReview
	activeMutation taskBoardMutationKind

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

type taskBoardExitMsg struct{ err error }

type taskBoardMutationKind string

const (
	taskBoardStartMutation  taskBoardMutationKind = "start_authoring"
	taskBoardReviewMutation taskBoardMutationKind = "review"
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

type reviewPrompt struct {
	decision      app.TaskBoardReviewDecision
	reasonInput   textinput.Model
	validationErr string
}

func newReviewPrompt(decision app.TaskBoardReviewDecision) *reviewPrompt {
	input := textinput.New()
	input.Prompt = "Reason "
	input.Placeholder = "Why this review decision is appropriate"
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
			if m.pendingStart == nil && m.pendingReview == nil {
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
		}
		var refreshCmd tea.Cmd
		m, refreshCmd = m.requestRefresh()
		return m, tea.Batch(inputCmd, refreshCmd)

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

	switch key {
	case "esc":
		if m.detail != nil {
			m.detail = nil
			return m, nil
		}
		return m, inputCmd

	case "q", "ctrl+c":
		if m.exitInFlight {
			return m, inputCmd
		}
		if m.mutationInFlight() || m.refreshInFlight {
			m.notice = "操作仍在进行，请等待结果后再退出"
			return m, inputCmd
		}
		if m.detail != nil {
			m.detail = nil
			return m, inputCmd
		}
		if key == "ctrl+c" && m.exitFlushFailed {
			return m, tea.Quit
		}
		return m.beginExit()

	case "n":
		if m.detail != nil {
			return m, inputCmd
		}
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

	case "a":
		if m.detail != nil && m.detail.task.Review != nil {
			return m.openReviewPrompt(app.TaskBoardApprove, inputCmd)
		}
		return m, inputCmd

	case "r":
		if m.detail != nil && m.detail.task.Review != nil {
			return m.openReviewPrompt(app.TaskBoardRequestChanges, inputCmd)
		}
		return m, inputCmd
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

	if m.detail != nil {
		footer := footerStyle.Render("[q] 返回")
		if m.detail.task.Review != nil {
			footer = footerStyle.Render("[a] 通过  [r] 返修  [q] 返回")
		}
		prompt := ""
		detailHeight := m.height - 4
		if m.review != nil {
			prompt = m.review.View(m.width)
			detailHeight -= 4
			footer = footerStyle.Render("[enter] 提交审核  [esc] 取消")
		}
		return appStyle.Render(lipgloss.JoinVertical(lipgloss.Top,
			headerStyle.Width(m.width).Render("Harbor Task Factory"),
			m.detail.View(m.width, max(1, detailHeight)),
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
		headerStyle.Width(m.width).Render("Harbor Task Factory"),
		m.board.View(m.width, max(1, boardHeight)),
		m.input.View(m.width),
		status,
		footerStyle.Render(footer),
	))
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

// detailModel renders task detail content for gate review.
type detailModel struct {
	task *TaskItem
}

func newDetailModel(task *TaskItem) *detailModel {
	return &detailModel{task: task}
}

func (d *detailModel) View(width, height int) string {
	lines := []string{
		detailTitleStyle.Width(width).Render(d.task.Name),
		mutedStyle.Render("slug:" + d.task.Slug),
		mutedStyle.Render(d.task.RepoURL),
		mutedStyle.Render("sha:" + d.task.CommitSHA),
		mutedStyle.Render("状态: " + d.task.Lifecycle),
	}
	if d.task.RunStatus != "" {
		run := "Run: " + d.task.RunStatus
		if d.task.RunID != "" {
			run = "Run: " + truncateMiddle(d.task.RunID, 16) + " (" + d.task.RunStatus + ")"
		}
		lines = append(lines, mutedStyle.Render(run))
	}
	if d.task.CurrentStage != "" {
		lines = append(lines, mutedStyle.Render("阶段: "+d.task.CurrentStage))
	}
	if d.task.Review != nil {
		lines = append(lines, highlightStyle.Render("待审核: "+string(d.task.Review.Kind)))
	} else if d.task.OpenReviews > 1 {
		lines = append(lines, failStyleV2.Render("存在多个待处理审核，请使用 CLI 选择明确的审核请求"))
	}
	return lipgloss.NewStyle().Width(max(1, width-2)).Height(max(1, height-2)).Render(lipgloss.JoinVertical(lipgloss.Left, lines...))
}
