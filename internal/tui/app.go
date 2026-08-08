package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/purplevoid/harbor-factory/internal/app"
)

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

// appModel is the top-level model for the task-board TUI. Exactly one screen
// owns the body at a time; screens are ordered by which one has focus, and each
// one derives its own row budget from the shared chrome so no screen can render
// taller than the terminal it was given.
type appModel struct {
	ctx     context.Context
	gateway taskBoardGateway

	board      TaskBoardModel
	input      TaskInputModel
	detail     *detailModel
	logs       *logModel
	transcript *agentTranscriptModel
	reviewGate *reviewScreen
	review     *reviewPrompt
	action     *runActionPrompt

	width              int
	height             int
	err                error
	notice             string
	authoringAvailable bool

	pendingStart   *pendingTaskBoardStart
	pendingReview  *pendingTaskBoardReview
	pendingAction  *pendingTaskBoardRunAction
	deferredAction *pendingTaskBoardRunAction
	activeMutation taskBoardMutationKind
	logEpoch       uint64
	reviewEpoch    uint64

	refreshInFlight         bool
	refreshRequested        bool
	refreshEpoch            uint64
	activationInFlight      bool
	reviewInspectInFlight   bool
	recoveryPreviewInFlight bool
	recoveryPreviewEpoch    uint64
	protocolPreviewInFlight bool
	protocolPreviewEpoch    uint64
	protocolPrepareInFlight bool
	protocolPrepareEpoch    uint64
	exitInFlight            bool
	exitFlushFailed         bool
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

	case TaskConfigLoadRequestMsg:
		return m, tea.Batch(inputCmd, m.loadTaskConfig(msg.Path))

	case TaskConfigLoadedMsg:
		if msg.Err != nil {
			m.input.SetConfigLoadError(msg.Err)
			return m, inputCmd
		}
		m.input.ApplyConfig(msg.Config)
		m.err = nil
		m.notice = "配置已加载，可在提交前修改"
		return m, tea.Batch(inputCmd, textinput.Blink)

	case tickMsg:
		if m.exitInFlight || m.mutationInFlight() {
			return m, tea.Batch(inputCmd, m.pollTick())
		}
		var refreshCmd tea.Cmd
		m, refreshCmd = m.requestRefresh()
		return m, tea.Batch(inputCmd, refreshCmd, m.pollTick())

	case taskBoardLoadedMsg:
		return m.applyLoadedSnapshot(msg, inputCmd)

	case taskBoardActivationMsg:
		m.activationInFlight = false
		if msg.err != nil {
			m.notice = "queued Run 交接暂未完成；看板将在下次刷新重试"
		}
		return m, inputCmd

	case taskBoardMutationMsg:
		return m.applyMutationResult(msg, inputCmd)

	case taskBoardReviewInspectionMsg:
		if msg.epoch != m.reviewEpoch || m.reviewGate == nil {
			return m, inputCmd
		}
		m.reviewInspectInFlight = false
		if msg.err != nil {
			// A failed inspection must not look like an empty gate: the screen says
			// the read failed so the operator does not decide on missing evidence.
			m.reviewGate.SetMessage("无法读取门禁详情: " + msg.err.Error())
			return m, inputCmd
		}
		m.reviewGate.SetInspection(msg.inspection)
		return m, inputCmd

	case taskBoardRecoveryPreviewMsg:
		return m.applyRecoveryPreview(msg, inputCmd)

	case taskBoardProtocolRetryPreviewMsg:
		return m.applyProtocolRetryPreview(msg, inputCmd)

	case taskBoardProtocolRetryPreparedMsg:
		return m.applyProtocolRetryPrepared(msg, inputCmd)

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

// applyLoadedSnapshot refreshes every projection derived from a board snapshot.
// The open detail pane is re-projected in place so a failed Run's status and
// recovery hint stay live without the operator closing and reopening the view.
func (m appModel) applyLoadedSnapshot(msg taskBoardLoadedMsg, inputCmd tea.Cmd) (tea.Model, tea.Cmd) {
	if msg.epoch != 0 && msg.epoch != m.refreshEpoch {
		return m, inputCmd
	}
	m.refreshInFlight = false
	if msg.err != nil {
		m.err = msg.err
	} else {
		pending, running, completed := taskItemsForSnapshot(msg.snapshot)
		m.board.SetTasks(pending, running, completed)
		m.authoringAvailable = msg.snapshot.AuthoringAvailable
		if m.detail != nil {
			if updated := taskItemForID(pending, running, completed, m.detail.task.ID); updated != nil {
				m.detail.task = updated
			}
		}
		if m.pendingStart == nil && m.pendingReview == nil && m.pendingAction == nil {
			m.err = nil
		}
	}
	if m.deferredAction != nil {
		pending := *m.deferredAction
		m.deferredAction = nil
		// The mutation completion will request a fresh snapshot, so the
		// coalesced periodic refresh is no longer needed.
		m.refreshRequested = false
		return m.dispatchRunAction(pending, inputCmd)
	}
	if m.refreshRequested && !m.exitInFlight {
		m.refreshRequested = false
		var refreshCmd tea.Cmd
		m, refreshCmd = m.requestRefresh()
		return m, tea.Batch(inputCmd, refreshCmd)
	}
	// A queued-run activation can acquire the same durable write lock as a
	// board projection. Start it only after the projection has rendered so
	// the TUI never remains on its loading screen behind that activation.
	var activationCmd tea.Cmd
	if !m.exitInFlight && !m.activationInFlight {
		m.activationInFlight = true
		activationCmd = m.activateQueuedRuns()
	}
	return m, tea.Batch(inputCmd, activationCmd)
}

func (m appModel) applyMutationResult(msg taskBoardMutationMsg, inputCmd tea.Cmd) (tea.Model, tea.Cmd) {
	m.activeMutation = ""
	if msg.err != nil {
		if msg.kind == taskBoardRetryMutation && errors.Is(msg.err, app.ErrTaskBoardRecoveryPreviewStale) {
			m.err = nil
			m.pendingAction = nil
			m.deferredAction = nil
			if m.action != nil {
				m.action.recoveryPreview = nil
				m.action.validationErr = "断点恢复计划已变化，请重新核验"
			}
			return m, inputCmd
		}
		if msg.kind == taskBoardRetryMutation && errors.Is(msg.err, app.ErrStandardProtocolStageRetryStale) {
			m.err = nil
			m.pendingAction = nil
			m.deferredAction = nil
			if m.action != nil {
				m.action.protocolPreview = nil
				m.action.protocolPrepared = nil
				m.action.validationErr = "协议重试来源已变化，请重新核验"
			}
			return m, inputCmd
		}
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
		m.reviewGate = nil
		m.reviewEpoch++
		m.detail = nil
	case taskBoardRetryMutation, taskBoardRetryAuthoringLaunchMutation, taskBoardCancelMutation:
		m.pendingAction = nil
		m.deferredAction = nil
		m.action = nil
		m.logs = nil
		m.detail = nil
	}
	var refreshCmd tea.Cmd
	m, refreshCmd = m.requestRefresh()
	return m, tea.Batch(inputCmd, refreshCmd)
}

func (m appModel) applyRecoveryPreview(msg taskBoardRecoveryPreviewMsg, inputCmd tea.Cmd) (tea.Model, tea.Cmd) {
	if msg.epoch != m.recoveryPreviewEpoch {
		return m, inputCmd
	}
	m.recoveryPreviewInFlight = false
	if m.action == nil ||
		m.action.kind != taskBoardRetryAction || m.detail == nil || !m.detail.hasCurrentRun() ||
		m.detail.task.ID != msg.taskID || m.detail.currentRun().ID != msg.runID ||
		strings.TrimSpace(m.action.reasonInput.Value()) != msg.reason {
		if m.action != nil {
			m.action.validationErr = "断点恢复计划已过期，请重新核验"
		} else {
			m.notice = "断点恢复计划已过期，请重新核验"
		}
		return m, inputCmd
	}
	if msg.err != nil {
		m.action.validationErr = "无法生成断点恢复计划: " + msg.err.Error()
		return m, inputCmd
	}
	if msg.preview.TaskID != msg.taskID || msg.preview.RunID != msg.runID ||
		msg.preview.Strategy != m.action.strategy || msg.preview.Checkpoint.Sequence == 0 ||
		msg.preview.SemanticPlanFingerprint == "" {
		m.action.validationErr = "断点恢复计划无效，请重新核验"
		return m, inputCmd
	}
	m.err = nil
	m.notice = ""
	m.action.validationErr = ""
	m.action.recoveryPreview = &msg.preview
	return m, inputCmd
}

func (m appModel) applyProtocolRetryPreview(msg taskBoardProtocolRetryPreviewMsg, inputCmd tea.Cmd) (tea.Model, tea.Cmd) {
	if msg.epoch != m.protocolPreviewEpoch {
		return m, inputCmd
	}
	m.protocolPreviewInFlight = false
	if m.action == nil || m.action.kind != taskBoardRetryAction || m.detail == nil || !m.detail.hasCurrentRun() ||
		m.detail.task.ID != msg.taskID || m.detail.currentRun().ID != msg.runID ||
		strings.TrimSpace(m.action.reasonInput.Value()) != msg.reason {
		m.notice = "协议重试预览已过期，请重新核验"
		return m, inputCmd
	}
	if msg.err != nil {
		m.action.validationErr = "无法生成协议重试预览: " + msg.err.Error()
		return m, inputCmd
	}
	if msg.preview.TaskID != msg.taskID || msg.preview.RunID != msg.runID || msg.preview.Source.StageAttemptID != msg.stageID ||
		msg.preview.Checkpoint.RunID != msg.runID || msg.preview.Checkpoint.StageAttemptID != msg.stageID || msg.preview.Checkpoint.RetryFingerprint == "" {
		m.action.validationErr = "协议重试预览无效，请重新核验"
		return m, inputCmd
	}
	m.action.validationErr = ""
	m.action.protocolPreview = &msg.preview
	m.action.protocolPrepared = nil
	return m, inputCmd
}

func (m appModel) applyProtocolRetryPrepared(msg taskBoardProtocolRetryPreparedMsg, inputCmd tea.Cmd) (tea.Model, tea.Cmd) {
	if msg.epoch != m.protocolPrepareEpoch {
		return m, inputCmd
	}
	m.protocolPrepareInFlight = false
	if m.action == nil || m.action.protocolPreview == nil || m.detail == nil || !m.detail.hasCurrentRun() ||
		m.detail.task.ID != msg.taskID || m.detail.currentRun().ID != msg.runID ||
		strings.TrimSpace(m.action.reasonInput.Value()) != msg.reason {
		m.notice = "协议重试准备已过期，请重新核验"
		return m, inputCmd
	}
	if msg.err != nil {
		m.action.validationErr = "无法准备协议重试: " + msg.err.Error()
		return m, inputCmd
	}
	if msg.prepared.TaskID != msg.taskID || msg.prepared.RunID != msg.runID || msg.prepared.Source.StageAttemptID != msg.stageID ||
		msg.prepared.Checkpoint != m.action.protocolPreview.Checkpoint || msg.prepared.Reason != msg.reason {
		m.action.validationErr = "协议重试准备无效，请重新核验"
		return m, inputCmd
	}
	m.action.validationErr = ""
	m.action.protocolPrepared = &msg.prepared
	return m, inputCmd
}

// handleKey routes one key to the focused screen. Order is focus order: a
// read-only overlay, then a modal prompt, then a visible form, then detail,
// then the board. A screen that owns the key never lets it reach the one behind
// it, so typing a reason can never trigger a board command.
func (m appModel) handleKey(msg tea.KeyMsg, inputCmd tea.Cmd) (tea.Model, tea.Cmd) {
	key := msg.String()

	if m.logs != nil {
		return m.handleLogKey(msg, inputCmd)
	}
	if m.transcript != nil {
		return m.handleAgentTranscriptKey(msg, inputCmd)
	}

	if m.action != nil {
		return m.handleRunActionPromptKey(msg, inputCmd)
	}

	if m.review != nil {
		return m.handleReviewPromptKey(msg, inputCmd)
	}

	if m.reviewGate != nil {
		return m.handleReviewScreenKey(msg, inputCmd)
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
		// Returning to the config picker starts a new authoring command. A
		// retained pending start is only valid for retrying the visible form.
		m.pendingStart = nil
		m.input.BeginConfigLoad()
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
	case "up", "k":
		m.detail.MoveUp()
	case "down", "j":
		m.detail.MoveDown()
	case "pgup", "ctrl+u":
		m.detail.PageUp()
	case "pgdown", "ctrl+d":
		m.detail.PageDown()
	case "home", "g":
		m.detail.GoToStart()
	case "end", "G":
		m.detail.GoToEnd()
	case "e":
		// Evidence and the root contract are diagnostic, not decision data.
		// They stay collapsed by default so the decision-necessary sections fit
		// the first screen of a short terminal.
		m.detail.ToggleEvidence()
	case "l":
		return m.openLog(inputCmd)
	case "p":
		return m.openAgentTranscript(inputCmd)
	case "t":
		if m.detail != nil && m.detail.hasAuthoringLaunch() {
			return m.openRunActionPrompt(taskBoardRetryAuthoringLaunchAction, inputCmd)
		}
		return m.openRunActionPrompt(taskBoardRetryAction, inputCmd)
	case "x":
		return m.openRunActionPrompt(taskBoardCancelAction, inputCmd)
	case "v":
		if m.detail != nil && m.detail.task != nil && m.detail.task.Review != nil {
			return m.openReviewScreen(inputCmd)
		}
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
	switch msg.String() {
	case "esc", "q", "ctrl+c":
		m.logs = nil
		m.logEpoch++
		return m, inputCmd
	case "up", "k":
		m.logs.MoveUp()
	case "down", "j":
		m.logs.MoveDown()
	case "pgup", "ctrl+u":
		m.logs.PageUp()
	case "pgdown", "ctrl+d":
		m.logs.PageDown()
	case "home", "g":
		m.logs.GoToStart()
	case "end", "G":
		m.logs.GoToEnd()
	case "enter":
		m.logs.ToggleExpanded()
	case "R":
		// Raw mode is the diagnostic fallback: if record parsing ever drops
		// something the operator needs, the original bytes remain reachable.
		m.logs.ToggleRaw()
	case "r":
		return m.openLog(inputCmd)
	}
	return m, inputCmd
}

func (m appModel) handleAgentTranscriptKey(msg tea.KeyMsg, inputCmd tea.Cmd) (tea.Model, tea.Cmd) {
	if m.transcript == nil {
		return m, inputCmd
	}
	switch msg.String() {
	case "esc", "q", "ctrl+c":
		m.transcript = nil
	case "up", "k":
		m.transcript.MoveUp()
	case "down", "j":
		m.transcript.MoveDown()
	case "pgup", "ctrl+u":
		m.transcript.PageUp()
	case "pgdown", "ctrl+d":
		m.transcript.PageDown()
	case "left", "h":
		m.transcript.MovePrevious()
	case "right", "l":
		m.transcript.MoveNext()
	}
	return m, inputCmd
}

func (m appModel) handleReviewScreenKey(msg tea.KeyMsg, inputCmd tea.Cmd) (tea.Model, tea.Cmd) {
	if m.reviewGate == nil {
		return m, inputCmd
	}
	switch msg.String() {
	case "esc", "q", "ctrl+c":
		m.reviewGate = nil
		m.reviewEpoch++
		m.reviewInspectInFlight = false
		return m, inputCmd
	case "up", "k":
		m.reviewGate.MoveUp()
	case "down", "j":
		m.reviewGate.MoveDown()
	case "pgup", "ctrl+u":
		m.reviewGate.PageUp()
	case "pgdown", "ctrl+d":
		m.reviewGate.PageDown()
	case "home", "g":
		m.reviewGate.GoToStart()
	case "end", "G":
		m.reviewGate.GoToEnd()
	case "a":
		return m.openReviewPrompt(app.TaskBoardApprove, inputCmd)
	case "r":
		return m.openReviewPrompt(app.TaskBoardRequestChanges, inputCmd)
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
	// The reason is a multi-line textarea, so enter belongs to the text and the
	// decision is committed with an explicit chord instead. The footer states
	// that chord, because an operator cannot guess it.
	case "ctrl+s":
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

// openReviewScreen opens the dedicated gate screen and asks the application
// layer for the full gate inspection. The board projection alone carries only
// the gate's identity, which is why the previous detail section could not show
// what was actually being decided.
func (m appModel) openReviewScreen(inputCmd tea.Cmd) (tea.Model, tea.Cmd) {
	if m.detail == nil || m.detail.task == nil || m.detail.task.Review == nil {
		return m, inputCmd
	}
	review := *m.detail.task.Review
	m.reviewGate = newReviewScreen(m.detail.task, review)
	m.reviewEpoch++
	m.reviewInspectInFlight = true
	return m, tea.Batch(inputCmd, m.inspectReview(m.detail.task.ID, review, m.reviewEpoch))
}

func (m appModel) handleRunActionPromptKey(msg tea.KeyMsg, inputCmd tea.Cmd) (tea.Model, tea.Cmd) {
	if m.action == nil {
		return m, inputCmd
	}
	if m.mutationInFlight() {
		if m.recoveryPreviewInFlight || m.protocolPreviewInFlight || m.protocolPrepareInFlight {
			m.notice = "正在核验断点恢复计划，请等待结果"
		} else {
			m.notice = "操作仍在提交，请等待结果"
		}
		return m, inputCmd
	}
	switch msg.String() {
	case "esc":
		m.action = nil
		m.deferredAction = nil
		m.recoveryPreviewEpoch++
		m.recoveryPreviewInFlight = false
		m.protocolPreviewInFlight = false
		m.protocolPrepareInFlight = false
		m.protocolPreviewEpoch++
		m.protocolPrepareEpoch++
		return m, inputCmd
	case "enter":
		if m.action.protocolPrepared != nil {
			return m.beginRunAction(m.action.kind, strings.TrimSpace(m.action.reasonInput.Value()), inputCmd)
		}
		if m.action.protocolPreview != nil {
			return m.beginProtocolRetryPrepare(inputCmd)
		}
		if m.action.recoveryPreview != nil {
			return m.beginRunAction(m.action.kind, strings.TrimSpace(m.action.reasonInput.Value()), inputCmd)
		}
		if !m.action.requiresReason {
			return m.beginRunAction(m.action.kind, "", inputCmd)
		}
		reason := strings.TrimSpace(m.action.reasonInput.Value())
		if reason == "" {
			m.action.validationErr = "操作原因不能为空"
			return m, inputCmd
		}
		if m.action.kind == taskBoardRetryAction && requiresRecoveryPreview(m.action.strategy) {
			return m.beginRecoveryPreview(reason, inputCmd)
		}
		if m.action.kind == taskBoardRetryAction && requiresProtocolRetryPreview(m.action.strategy) {
			return m.beginProtocolRetryPreview(reason, inputCmd)
		}
		return m.beginRunAction(m.action.kind, reason, inputCmd)
	}
	var command tea.Cmd
	m.action.reasonInput, command = m.action.reasonInput.Update(msg)
	return m, tea.Batch(inputCmd, command)
}

func (m appModel) openRunActionPrompt(kind taskBoardRunActionKind, inputCmd tea.Cmd) (tea.Model, tea.Cmd) {
	if m.mutationInFlight() || m.detail == nil {
		return m, inputCmd
	}
	if kind == taskBoardRetryAuthoringLaunchAction {
		if !m.detail.canRetryAuthoringLaunch() {
			m.notice = "当前源码捕获启动不可重试"
			return m, inputCmd
		}
		m.action = newRunActionPrompt(kind, app.TaskBoardRetryStrategyNone)
		return m, tea.Batch(inputCmd, textinput.Blink)
	}
	if !m.detail.hasCurrentRun() {
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

func (m appModel) openAgentTranscript(inputCmd tea.Cmd) (tea.Model, tea.Cmd) {
	if m.detail == nil || !m.detail.hasAgentTurnTranscripts() {
		m.notice = "当前 Run 没有可查看的 Agent 回合"
		return m, inputCmd
	}
	m.transcript = newAgentTranscriptModel(m.detail.task)
	return m, inputCmd
}

func (m appModel) detailTask() *TaskItem {
	if m.detail == nil {
		return nil
	}
	return m.detail.task
}

func (m appModel) mutationInFlight() bool {
	return m.activeMutation != "" || m.recoveryPreviewInFlight || m.protocolPreviewInFlight || m.protocolPrepareInFlight
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
	message.TaskType = strings.TrimSpace(message.TaskType)
	message.Application = strings.TrimSpace(message.Application)
	message.CodeLang = strings.TrimSpace(message.CodeLang)
	message.Objective = strings.TrimSpace(message.Objective)
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

func (m appModel) beginRunAction(kind taskBoardRunActionKind, reason string, inputCmd tea.Cmd) (tea.Model, tea.Cmd) {
	if m.mutationInFlight() || m.detail == nil {
		m.notice = "请等待当前操作完成"
		return m, inputCmd
	}
	if kind == taskBoardRetryAuthoringLaunchAction {
		launch := m.detail.authoringLaunch()
		if launch == nil || !launch.CanRetry {
			m.notice = "当前源码捕获启动不可重试"
			return m, inputCmd
		}
		m.pendingAction = &pendingTaskBoardRunAction{kind: kind, operationID: launch.OperationID}
		return m.scheduleRunAction(*m.pendingAction, inputCmd)
	}
	if !m.detail.hasCurrentRun() {
		m.notice = "请等待当前操作完成"
		return m, inputCmd
	}
	run := m.detail.currentRun()
	current := pendingTaskBoardRunAction{
		kind: kind, taskID: m.detail.task.ID, runID: run.ID, reason: strings.TrimSpace(reason),
	}
	if kind == taskBoardRetryAction && m.action != nil && requiresRecoveryPreview(m.action.strategy) && m.action.recoveryPreview != nil {
		preview := *m.action.recoveryPreview
		if preview.TaskID != current.taskID || preview.RunID != current.runID || preview.Strategy != m.action.strategy ||
			preview.Checkpoint.Sequence == 0 || preview.SemanticPlanFingerprint == "" {
			m.action.recoveryPreview = nil
			m.action.validationErr = "断点恢复计划已过期，请重新核验"
			return m, inputCmd
		}
		current.recoveryPreview = &preview
	}
	if kind == taskBoardRetryAction && m.action != nil && requiresProtocolRetryPreview(m.action.strategy) && m.action.protocolPrepared != nil {
		prepared := *m.action.protocolPrepared
		if prepared.TaskID != current.taskID || prepared.RunID != current.runID || prepared.Checkpoint.RetryFingerprint == "" || prepared.Reason != current.reason {
			m.action.protocolPrepared = nil
			m.action.validationErr = "协议重试准备已过期，请重新核验"
			return m, inputCmd
		}
		current.protocolRetry = &prepared
	}
	if m.pendingAction != nil && m.pendingAction.kind == current.kind && m.pendingAction.taskID == current.taskID && m.pendingAction.runID == current.runID && m.pendingAction.reason == current.reason {
		current.key = m.pendingAction.key
	} else {
		key, err := m.newIdempotencyKey()
		if err != nil {
			m.err = err
			return m, inputCmd
		}
		current.key = key
	}
	m.pendingAction = &current
	return m.scheduleRunAction(*m.pendingAction, inputCmd)
}

// beginRecoveryPreview obtains a non-durable task-continuation plan before it
// can be confirmed. RetryRun still builds and commits a fresh plan.
func (m appModel) beginRecoveryPreview(reason string, inputCmd tea.Cmd) (tea.Model, tea.Cmd) {
	if m.mutationInFlight() || m.detail == nil || m.action == nil || !m.detail.hasCurrentRun() {
		m.notice = "请等待当前操作完成"
		return m, inputCmd
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		m.action.validationErr = "操作原因不能为空"
		return m, inputCmd
	}
	run := m.detail.currentRun()
	m.action.validationErr = ""
	m.action.recoveryPreview = nil
	m.recoveryPreviewEpoch++
	m.recoveryPreviewInFlight = true
	return m, tea.Batch(inputCmd, m.previewRunRecovery(m.detail.task.ID, run.ID, reason, m.recoveryPreviewEpoch))
}

func (m appModel) beginProtocolRetryPreview(reason string, inputCmd tea.Cmd) (tea.Model, tea.Cmd) {
	if m.mutationInFlight() || m.detail == nil || m.action == nil || !m.detail.hasCurrentRun() {
		m.notice = "请等待当前操作完成"
		return m, inputCmd
	}
	run := m.detail.currentRun()
	if run.StandardProtocolRetry == nil || run.StandardProtocolRetry.StageAttemptID == "" {
		m.action.validationErr = "当前 Run 没有可重试的协议失败阶段"
		return m, inputCmd
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		m.action.validationErr = "操作原因不能为空"
		return m, inputCmd
	}
	m.action.validationErr = ""
	m.action.protocolPreview = nil
	m.action.protocolPrepared = nil
	m.protocolPreviewEpoch++
	m.protocolPreviewInFlight = true
	return m, tea.Batch(inputCmd, m.previewStandardProtocolRetry(m.detail.task.ID, run.ID, run.StandardProtocolRetry.StageAttemptID, reason, m.protocolPreviewEpoch))
}

func (m appModel) beginProtocolRetryPrepare(inputCmd tea.Cmd) (tea.Model, tea.Cmd) {
	if m.mutationInFlight() || m.detail == nil || m.action == nil || m.action.protocolPreview == nil || !m.detail.hasCurrentRun() {
		m.notice = "请等待当前操作完成"
		return m, inputCmd
	}
	reason := strings.TrimSpace(m.action.reasonInput.Value())
	if reason == "" {
		m.action.validationErr = "操作原因不能为空"
		return m, inputCmd
	}
	preview := *m.action.protocolPreview
	m.action.validationErr = ""
	m.protocolPrepareEpoch++
	m.protocolPrepareInFlight = true
	return m, tea.Batch(inputCmd, m.prepareStandardProtocolRetry(preview, reason, m.protocolPrepareEpoch))
}

func (m appModel) scheduleRunAction(pending pendingTaskBoardRunAction, inputCmd tea.Cmd) (tea.Model, tea.Cmd) {
	if m.refreshInFlight {
		m.deferredAction = &pending
		m.notice = "正在刷新任务状态，完成后将执行操作"
		return m, inputCmd
	}
	return m.dispatchRunAction(pending, inputCmd)
}

func (m appModel) dispatchRunAction(pending pendingTaskBoardRunAction, inputCmd tea.Cmd) (tea.Model, tea.Cmd) {
	switch pending.kind {
	case taskBoardRetryAction:
		m.activeMutation = taskBoardRetryMutation
	case taskBoardRetryAuthoringLaunchAction:
		m.activeMutation = taskBoardRetryAuthoringLaunchMutation
	case taskBoardCancelAction:
		m.activeMutation = taskBoardCancelMutation
	default:
		m.err = fmt.Errorf("unsupported task board Run action %q", pending.kind)
		return m, inputCmd
	}
	return m, tea.Batch(inputCmd, m.runAction(pending))
}

func (m appModel) beginExit() (tea.Model, tea.Cmd) {
	if m.exitInFlight || m.mutationInFlight() || m.refreshInFlight {
		return m, nil
	}
	m.exitInFlight = true
	m.exitFlushFailed = false
	return m, m.flushBeforeExit()
}

// View composes the focused screen into the window. Every branch spends the
// same row budget: header, body sized by chrome.bodyRows, an optional prompt, a
// status line, and a footer. The body is the only part allowed to scroll, which
// is what keeps the header and breadcrumb on screen at any terminal size.
func (m appModel) View() string {
	if m.width == 0 {
		return "loading..."
	}

	status := m.statusView()
	prompt := ""
	if m.review != nil {
		prompt = m.review.View(newChrome(m.width, m.height, status, "").contentWidth())
	} else if m.action != nil {
		prompt = m.action.View(newChrome(m.width, m.height, status, "").contentWidth())
	} else if m.input.Visible() {
		prompt = m.input.View(newChrome(m.width, m.height, status, "").contentWidth())
	}
	budget := newChrome(m.width, m.height, status, prompt)
	contentWidth := budget.contentWidth()
	bodyRows := budget.bodyRows()

	var body, footer string
	switch {
	case m.transcript != nil:
		body = m.transcript.View(contentWidth, bodyRows)
		footer = "[↑↓/jk] 滚动  [←→/hl] 切换回合  [PgUp/PgDn] 翻页  [q] 返回详情"
	case m.logs != nil:
		body = m.logs.View(contentWidth, bodyRows)
		footer = m.logs.FooterText()
	case m.reviewGate != nil:
		body = m.reviewGate.View(contentWidth, bodyRows)
		footer = "[a] 通过  [r] 返修  [↑↓/jk] 滚动  [PgUp/PgDn] 翻页  [q] 返回详情"
	case m.detail != nil:
		body = m.detail.View(contentWidth, bodyRows)
		footer = detailFooterText(m.detail)
	default:
		body = m.board.View(contentWidth, bodyRows)
		footer = "[tab] 切换面板  [hjkl/↑↓←→] 导航  [d] 详情  [q] 退出"
		if m.authoringAvailable {
			footer = "[n] 从配置新建  " + footer
		}
	}

	// A prompt owns the footer while it is open: its keys are the only ones the
	// operator can act on.
	if m.review != nil {
		footer = "[ctrl+s] 提交审核  [esc] 取消"
	} else if m.action != nil {
		footer = m.runActionFooterText()
	} else if m.input.Visible() {
		footer = m.taskInputFooterText()
	}

	sections := []string{headerStyle.Width(contentWidth).Render(appTitle), body}
	if prompt != "" {
		sections = append(sections, prompt)
	}
	if status != "" {
		sections = append(sections, status)
	}
	sections = append(sections, footerStyle.Render(truncateDisplay(footer, contentWidth)))
	return appStyle.Render(lipgloss.JoinVertical(lipgloss.Top, sections...))
}

func (m appModel) taskInputFooterText() string {
	if m.mutationInFlight() {
		return "正在提交创题任务..."
	}
	if m.input.mode == taskInputLoadConfig {
		if m.input.loadingConfig {
			return "正在加载配置...  [esc] 取消"
		}
		return "[enter] 加载配置  [esc] 取消"
	}
	return "[tab] 下一项  [space] 切换 0-to-1  [enter] 提交  [esc] 取消"
}

func (m appModel) runActionFooterText() string {
	switch {
	case m.recoveryPreviewInFlight:
		return "正在核验断点恢复计划..."
	case m.protocolPreviewInFlight:
		return "正在核验协议重试来源..."
	case m.protocolPrepareInFlight:
		return "正在准备协议重试..."
	case m.action == nil:
		return "[enter] 确认操作  [esc] 取消"
	case m.action.protocolPrepared != nil:
		return "[enter] 确认重试当前阶段  [esc] 取消"
	case m.action.protocolPreview != nil:
		return "[enter] 准备重试当前阶段  [esc] 取消"
	case m.action.recoveryPreview != nil:
		return "[enter] 确认从此断点恢复  [esc] 取消"
	case m.action.kind == taskBoardRetryAction && requiresRecoveryPreview(m.action.strategy):
		return "[enter] 查看断点恢复计划  [esc] 取消"
	case m.action.kind == taskBoardRetryAction && requiresProtocolRetryPreview(m.action.strategy):
		return "[enter] 查看协议重试来源  [esc] 取消"
	default:
		return "[enter] 确认操作  [esc] 取消"
	}
}

func detailFooterText(detail *detailModel) string {
	actions := make([]string, 0, 8)
	if detail != nil && detail.canRetryAuthoringLaunch() {
		actions = append(actions, "[t] 重试源码捕获")
	}
	if detail != nil && detail.task != nil && detail.task.Review != nil {
		actions = append(actions, "[v] 审核门禁", "[a] 通过", "[r] 返修")
	}
	if detail != nil && detail.hasCurrentRun() {
		actions = append(actions, "[l] 日志")
		if detail.hasAgentTurnTranscripts() {
			actions = append(actions, "[p] Agent 回合")
		}
		if detail.canRetryCurrentRun() {
			label := "重试"
			if detail.currentRun().RetryStrategy == app.TaskBoardRetryStrategyTaskContinuation {
				label = "断点恢复"
			} else if detail.currentRun().RetryStrategy == app.TaskBoardRetryStrategyStandardProtocolStage {
				label = "重试当前阶段"
			}
			actions = append(actions, "[t] "+label)
		}
		if detail.canCancelCurrentRun() {
			actions = append(actions, "[x] 取消")
		}
	}
	if detail != nil {
		actions = append(actions, detail.evidenceToggleHint())
	}
	actions = append(actions, "[q] 返回")
	return strings.Join(actions, "  ")
}

func (m appModel) statusView() string {
	if m.err != nil {
		return styleFail.Render(truncateDisplay(m.err.Error(), newChrome(m.width, m.height, "", "").contentWidth()))
	}
	if m.notice != "" {
		return stylePass.Render(truncateDisplay(m.notice, newChrome(m.width, m.height, "", "").contentWidth()))
	}
	return ""
}
