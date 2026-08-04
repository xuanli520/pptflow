package tui

import (
	"context"
	"errors"
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

	board           TaskBoardModel
	input           TaskInputModel
	detail          *detailModel
	logs            *logModel
	transcript      *agentTranscriptModel
	review          *reviewPrompt
	action          *runActionPrompt
	evaluatorAction *evaluatorActionPrompt

	width              int
	height             int
	err                error
	notice             string
	authoringAvailable bool

	pendingStart           *pendingTaskBoardStart
	pendingReview          *pendingTaskBoardReview
	pendingAction          *pendingTaskBoardRunAction
	deferredAction         *pendingTaskBoardRunAction
	pendingEvaluatorAction *pendingTaskBoardEvaluatorAction
	activeMutation         taskBoardMutationKind
	logEpoch               uint64

	refreshInFlight         bool
	refreshRequested        bool
	refreshEpoch            uint64
	activationInFlight      bool
	recoveryPreviewInFlight bool
	recoveryPreviewEpoch    uint64
	protocolPreviewInFlight bool
	protocolPreviewEpoch    uint64
	protocolPrepareInFlight bool
	protocolPrepareEpoch    uint64
	evaluatorActionEpoch    uint64
	exitInFlight            bool
	exitFlushFailed         bool
}

type taskBoardGateway = app.TaskBoardGateway

type taskBoardLoadedMsg struct {
	snapshot app.TaskBoardSnapshot
	epoch    uint64
	err      error
}

// taskBoardActivationMsg keeps queued-run activation off the board refresh
// path so a busy durable outbox cannot prevent the operator from seeing or
// deciding an already-open review.
type taskBoardActivationMsg struct {
	err error
}

type taskBoardMutationMsg struct {
	kind     taskBoardMutationKind
	mutation app.TaskBoardMutation
	err      error
}

type taskBoardRecoveryPreviewMsg struct {
	preview app.TaskBoardRecoveryPreview
	epoch   uint64
	taskID  string
	runID   string
	reason  string
	err     error
}

type taskBoardProtocolRetryPreviewMsg struct {
	preview app.TaskBoardStandardProtocolRetryPreview
	epoch   uint64
	taskID  string
	runID   string
	stageID string
	reason  string
	err     error
}

type taskBoardProtocolRetryPreparedMsg struct {
	prepared app.TaskBoardPreparedStandardProtocolRetry
	epoch    uint64
	taskID   string
	runID    string
	stageID  string
	reason   string
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
	taskBoardStartMutation                taskBoardMutationKind = "start_authoring"
	taskBoardReviewMutation               taskBoardMutationKind = "review"
	taskBoardRetryMutation                taskBoardMutationKind = "retry_run"
	taskBoardRetryAuthoringLaunchMutation taskBoardMutationKind = "retry_authoring_launch"
	taskBoardCancelMutation               taskBoardMutationKind = "cancel_run"
	taskBoardEvaluatorPreviewMutation     taskBoardMutationKind = "evaluator_preview"
	taskBoardEvaluatorPrepareMutation     taskBoardMutationKind = "evaluator_prepare"
	taskBoardEvaluatorConfirmMutation     taskBoardMutationKind = "evaluator_confirm"
	taskBoardEvaluatorAdoptMutation       taskBoardMutationKind = "evaluator_adopt"
)

type taskBoardRunActionKind string

const (
	taskBoardRetryAction                taskBoardRunActionKind = "retry"
	taskBoardRetryAuthoringLaunchAction taskBoardRunActionKind = "retry_authoring_launch"
	taskBoardCancelAction               taskBoardRunActionKind = "cancel"
	recoveryPreviewTimeout                                     = 15 * time.Second
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
	kind            taskBoardRunActionKind
	operationID     string
	taskID          string
	runID           string
	reason          string
	key             string
	recoveryPreview *app.TaskBoardRecoveryPreview
	protocolRetry   *app.TaskBoardPreparedStandardProtocolRetry
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
	kind             taskBoardRunActionKind
	strategy         app.TaskBoardRetryStrategy
	reasonInput      textinput.Model
	validationErr    string
	requiresReason   bool
	recoveryPreview  *app.TaskBoardRecoveryPreview
	protocolPreview  *app.TaskBoardStandardProtocolRetryPreview
	protocolPrepared *app.TaskBoardPreparedStandardProtocolRetry
}

func newRunActionPrompt(kind taskBoardRunActionKind, strategy app.TaskBoardRetryStrategy) *runActionPrompt {
	input := textinput.New()
	input.Prompt = "原因 "
	input.Placeholder = "记录本次操作的原因"
	input.CharLimit = 240
	input.Width = 52
	input.Focus()
	return &runActionPrompt{kind: kind, strategy: strategy, reasonInput: input, requiresReason: kind != taskBoardRetryAuthoringLaunchAction}
}

func (prompt *runActionPrompt) View(width int) string {
	label := "重试当前 Run"
	switch prompt.kind {
	case taskBoardRetryAction:
		if prompt.strategy == app.TaskBoardRetryStrategyTaskContinuation {
			label = "断点恢复创题 Run"
		} else if prompt.strategy == app.TaskBoardRetryStrategyStandardProtocolStage {
			label = "重试当前 Standard 阶段"
		}
	case taskBoardRetryAuthoringLaunchAction:
		label = "重试源码捕获"
	case taskBoardCancelAction:
		label = "取消当前 Run"
	}
	content := detailSectionTitleStyle.Render(label)
	if prompt.recoveryPreview != nil {
		content += "\n" + recoveryPreviewView(*prompt.recoveryPreview, max(1, width-4))
	} else if prompt.protocolPrepared != nil {
		content += "\n" + standardProtocolRetryPreviewView(prompt.protocolPrepared.TaskBoardStandardProtocolRetryPreview, "重试准备", max(1, width-4))
	} else if prompt.protocolPreview != nil {
		content += "\n" + standardProtocolRetryPreviewView(*prompt.protocolPreview, "协议重试预览", max(1, width-4))
	} else if prompt.requiresReason {
		content += "\n" + prompt.reasonInput.View()
	}
	if prompt.validationErr != "" {
		content += "\n" + failStyleV2.Render(prompt.validationErr)
	}
	return inputStyle.Width(max(1, width)).Render(content)
}

func standardProtocolRetryPreviewView(preview app.TaskBoardStandardProtocolRetryPreview, title string, width int) string {
	fields := []string{
		detailField("目标阶段", displayStageName(preview.StageKey), width),
		detailField("失败 attempt", preview.Source.StageAttemptID, width),
		detailField("Transcript", preview.Source.TranscriptID, width),
		detailField("协议状态", preview.Status, width),
		detailField("失败码", preview.Source.FailureCode, width),
		detailField("模型", preview.ModelID, width),
		detailField("响应摘要", fmt.Sprintf("%d bytes · %s", preview.ResponseSize, preview.ResponseSHA), width),
	}
	return renderDetailSection(title, detailFields(width, fields...), width)
}

func recoveryPreviewView(preview app.TaskBoardRecoveryPreview, width int) string {
	fields := []string{
		detailField("目标阶段", recoveryPreviewStageList(preview.TargetStages), width),
		detailField("复用阶段", recoveryPreviewStageList(preview.ReusedStages), width),
		detailField("重新调度", recoveryPreviewStageList(preview.ScheduledStages), width),
		detailField("执行 Epoch", fmt.Sprintf("%d -> %d", preview.CurrentExecutionEpoch, preview.NextExecutionEpoch), width),
		detailField("断点序列", fmt.Sprintf("%d", preview.CheckpointSequence), width),
		detailField("输入校验", "复用阶段的产物与输入指纹已核验", width),
		detailField("工作流指纹", preview.WorkflowFingerprint, width),
	}
	if reasons := recoveryPreviewReasonList(preview); reasons != "" {
		fields = append(fields, detailField("计划原因", reasons, width))
	}
	if len(preview.InvalidatedStages) > 0 {
		fields = append(fields, detailField("未调度阶段", recoveryPreviewStageList(preview.InvalidatedStages), width))
	}
	if len(preview.OperatorOnlyStages) > 0 {
		fields = append(fields, detailField("人工阶段", recoveryPreviewStageList(preview.OperatorOnlyStages), width))
	}
	return renderDetailSection("断点恢复计划", detailFields(width, fields...), width)
}

func recoveryPreviewStageList(stages []string) string {
	if len(stages) == 0 {
		return "无"
	}
	names := make([]string, 0, len(stages))
	for _, stage := range stages {
		names = append(names, displayStageName(stage))
	}
	return strings.Join(names, ", ")
}

func recoveryPreviewReasonList(preview app.TaskBoardRecoveryPreview) string {
	seen := make(map[string]struct{})
	stages := make([]string, 0, len(preview.TargetStages)+len(preview.ReusedStages)+len(preview.ScheduledStages)+len(preview.InvalidatedStages)+len(preview.OperatorOnlyStages))
	stages = append(stages, preview.TargetStages...)
	stages = append(stages, preview.ReusedStages...)
	stages = append(stages, preview.ScheduledStages...)
	stages = append(stages, preview.InvalidatedStages...)
	stages = append(stages, preview.OperatorOnlyStages...)
	parts := make([]string, 0, len(stages))
	for _, stage := range stages {
		if _, duplicate := seen[stage]; duplicate {
			continue
		}
		seen[stage] = struct{}{}
		reasons := preview.StageReasons[stage]
		if len(reasons) == 0 {
			continue
		}
		labels := make([]string, 0, len(reasons))
		for _, reason := range reasons {
			labels = append(labels, recoveryPreviewReasonLabel(reason))
		}
		parts = append(parts, displayStageName(stage)+": "+strings.Join(labels, ", "))
	}
	return strings.Join(parts, "；")
}

func recoveryPreviewReasonLabel(reason string) string {
	switch reason {
	case "artifact_unavailable":
		return "上游产物缺失或损坏"
	case "input_fingerprint_drift":
		return "输入指纹不一致"
	case "dependency_invalidated":
		return "依赖阶段需重跑"
	case "retry_requested":
		return "失败阶段重试"
	case "force_recompute":
		return "请求重新生成"
	default:
		return reason
	}
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
		if m.evaluatorAction != nil {
			var command tea.Cmd
			m.evaluatorAction.reason, command = m.evaluatorAction.reason.Update(msg)
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
			// The detail pane is a projection of the same snapshot: refresh
			// its TaskItem so a failed Run's status, failure record, and
			// recovery hint stay live without closing and reopening the view.
			if m.detail != nil {
				if updated := taskItemForID(pending, running, completed, m.detail.task.ID); updated != nil {
					m.detail.task = updated
				}
			}
			if m.pendingStart == nil && m.pendingReview == nil && m.pendingAction == nil && m.pendingEvaluatorAction == nil {
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

	case taskBoardActivationMsg:
		m.activationInFlight = false
		if msg.err != nil {
			m.notice = "queued Run 交接暂未完成；看板将在下次刷新重试"
		}
		return m, inputCmd

	case taskBoardMutationMsg:
		m.activeMutation = ""
		if msg.err != nil {
			if msg.kind == taskBoardEvaluatorConfirmMutation || msg.kind == taskBoardEvaluatorAdoptMutation {
				// Evaluator failures can originate in a deployment or evidence
				// verifier. Keep their raw diagnostics in durable records rather
				// than rendering them in the local terminal.
				m.err = errors.New("CodeEdge 评测操作未完成；请刷新运行状态后重试")
				return m, inputCmd
			}
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
			m.detail = nil
		case taskBoardRetryMutation, taskBoardRetryAuthoringLaunchMutation, taskBoardCancelMutation:
			m.pendingAction = nil
			m.deferredAction = nil
			m.action = nil
			m.logs = nil
			m.detail = nil
		case taskBoardEvaluatorConfirmMutation, taskBoardEvaluatorAdoptMutation:
			m.pendingEvaluatorAction = nil
			m.evaluatorAction = nil
			m.logs = nil
			m.detail = nil
		}
		var refreshCmd tea.Cmd
		m, refreshCmd = m.requestRefresh()
		return m, tea.Batch(inputCmd, refreshCmd)

	case taskBoardRecoveryPreviewMsg:
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

	case taskBoardProtocolRetryPreviewMsg:
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

	case taskBoardProtocolRetryPreparedMsg:
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

	case taskBoardEvaluatorPreviewMsg:
		return m.applyEvaluatorPreview(msg, inputCmd)

	case taskBoardEvaluatorPreparedMsg:
		return m.applyEvaluatorPrepared(msg, inputCmd)

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
	if m.transcript != nil {
		return m.handleAgentTranscriptKey(msg, inputCmd)
	}

	if m.evaluatorAction != nil {
		return m.handleEvaluatorActionKey(msg, inputCmd)
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
	case "e":
		return m.openEvaluatorAction(evaluatorLaunchAction, inputCmd)
	case "v":
		return m.openEvaluatorAction(evaluatorAdoptAction, inputCmd)
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

func (m appModel) handleAgentTranscriptKey(msg tea.KeyMsg, inputCmd tea.Cmd) (tea.Model, tea.Cmd) {
	if m.transcript == nil {
		return m, inputCmd
	}
	width, height := m.logDimensions()
	switch msg.String() {
	case "esc", "q", "ctrl+c":
		m.transcript = nil
	case "up", "k":
		m.transcript.MoveUp(width, height)
	case "down", "j":
		m.transcript.MoveDown(width, height)
	case "pgup", "ctrl+u":
		m.transcript.PageUp(width, height)
	case "pgdown", "ctrl+d":
		m.transcript.PageDown(width, height)
	case "left", "h":
		m.transcript.MovePrevious()
	case "right", "l":
		m.transcript.MoveNext()
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

func isRetryAction(kind taskBoardRunActionKind) bool {
	return kind == taskBoardRetryAction || kind == taskBoardRetryAuthoringLaunchAction
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

func (m appModel) logDimensions() (int, int) {
	return m.contentWidth(), max(8, m.height-3)
}

func (m appModel) contentWidth() int {
	return max(1, m.width-2)
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

func (m appModel) refreshTasks(epoch uint64) tea.Cmd {
	return func() tea.Msg {
		if m.gateway == nil {
			return taskBoardLoadedMsg{epoch: epoch, err: fmt.Errorf("task board service is not configured")}
		}
		snapshot, err := m.gateway.List(m.ctx)
		return taskBoardLoadedMsg{snapshot: snapshot, epoch: epoch, err: err}
	}
}

func (m appModel) activateQueuedRuns() tea.Cmd {
	return func() tea.Msg {
		if m.gateway == nil {
			return taskBoardActivationMsg{err: fmt.Errorf("task board service is not configured")}
		}
		return taskBoardActivationMsg{err: m.gateway.FlushQueuedRuns(m.ctx)}
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
			TaskType:       pending.message.TaskType,
			Application:    pending.message.Application,
			CodeLang:       pending.message.CodeLang,
			Is0To1:         pending.message.Is0To1,
			Objective:      pending.message.Objective,
			MetadataJSON:   "{}",
			Reason:         pending.message.Reason,
		})
		return taskBoardMutationMsg{kind: taskBoardStartMutation, mutation: mutation, err: err}
	}
}

func (m appModel) loadTaskConfig(path string) tea.Cmd {
	return func() tea.Msg {
		config, err := readTaskInputConfigFile(path)
		return TaskConfigLoadedMsg{Path: path, Config: config, Err: err}
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

func (m appModel) previewStandardProtocolRetry(taskID, runID, stageID, reason string, epoch uint64) tea.Cmd {
	return func() tea.Msg {
		if m.gateway == nil {
			return taskBoardProtocolRetryPreviewMsg{epoch: epoch, taskID: taskID, runID: runID, stageID: stageID, reason: reason, err: fmt.Errorf("task board service is not configured")}
		}
		ctx := m.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		ctx, cancel := context.WithTimeout(ctx, recoveryPreviewTimeout)
		defer cancel()
		preview, err := m.gateway.PreviewStandardProtocolRetry(ctx, app.TaskBoardPreviewStandardProtocolRetryRequest{
			TaskID: taskID, RunID: runID, StageAttemptID: stageID, Reason: reason,
		})
		return taskBoardProtocolRetryPreviewMsg{preview: preview, epoch: epoch, taskID: taskID, runID: runID, stageID: stageID, reason: reason, err: err}
	}
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

func (m appModel) prepareStandardProtocolRetry(preview app.TaskBoardStandardProtocolRetryPreview, reason string, epoch uint64) tea.Cmd {
	return func() tea.Msg {
		if m.gateway == nil {
			return taskBoardProtocolRetryPreparedMsg{epoch: epoch, taskID: preview.TaskID, runID: preview.RunID, stageID: preview.Source.StageAttemptID, reason: reason, err: fmt.Errorf("task board service is not configured")}
		}
		prepared, err := m.gateway.PrepareStandardProtocolRetry(m.ctx, app.TaskBoardPrepareStandardProtocolRetryRequest{
			TaskBoardPreviewStandardProtocolRetryRequest: app.TaskBoardPreviewStandardProtocolRetryRequest{
				TaskID: preview.TaskID, RunID: preview.RunID, StageAttemptID: preview.Source.StageAttemptID, Reason: reason,
			},
			Expected: preview.Checkpoint,
		})
		return taskBoardProtocolRetryPreparedMsg{prepared: prepared, epoch: epoch, taskID: preview.TaskID, runID: preview.RunID, stageID: preview.Source.StageAttemptID, reason: reason, err: err}
	}
}

func (m appModel) previewRunRecovery(taskID, runID, reason string, epoch uint64) tea.Cmd {
	return func() tea.Msg {
		if m.gateway == nil {
			return taskBoardRecoveryPreviewMsg{epoch: epoch, taskID: taskID, runID: runID, reason: reason, err: fmt.Errorf("task board service is not configured")}
		}
		ctx := m.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		ctx, cancel := context.WithTimeout(ctx, recoveryPreviewTimeout)
		defer cancel()
		preview, err := m.gateway.PreviewRunRecovery(ctx, app.TaskBoardPreviewRunRecoveryRequest{
			TaskID: taskID,
			RunID:  runID,
			Reason: reason,
		})
		return taskBoardRecoveryPreviewMsg{preview: preview, epoch: epoch, taskID: taskID, runID: runID, reason: reason, err: err}
	}
}

func requiresRecoveryPreview(strategy app.TaskBoardRetryStrategy) bool {
	return strategy == app.TaskBoardRetryStrategyTaskContinuation
}

func requiresProtocolRetryPreview(strategy app.TaskBoardRetryStrategy) bool {
	return strategy == app.TaskBoardRetryStrategyStandardProtocolStage
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

func (m appModel) runAction(pending pendingTaskBoardRunAction) tea.Cmd {
	return func() tea.Msg {
		if m.gateway == nil {
			kind := taskBoardRetryMutation
			if pending.kind == taskBoardCancelAction {
				kind = taskBoardCancelMutation
			} else if pending.kind == taskBoardRetryAuthoringLaunchAction {
				kind = taskBoardRetryAuthoringLaunchMutation
			}
			return taskBoardMutationMsg{kind: kind, err: fmt.Errorf("task board service is not configured")}
		}
		switch pending.kind {
		case taskBoardRetryAction:
			request := app.TaskBoardRetryRunRequest{
				IdempotencyKey: pending.key, TaskID: pending.taskID, RunID: pending.runID, Reason: pending.reason,
			}
			if pending.recoveryPreview != nil {
				checkpoint := pending.recoveryPreview.Checkpoint
				request.ExpectedRecoveryCheckpoint = &checkpoint
				request.ExpectedRecoveryPlanFingerprint = pending.recoveryPreview.SemanticPlanFingerprint
			}
			if pending.protocolRetry != nil {
				checkpoint := pending.protocolRetry.Checkpoint
				request.ExpectedStandardProtocolRetry = &checkpoint
			}
			mutation, err := m.gateway.RetryRun(m.ctx, request)
			return taskBoardMutationMsg{kind: taskBoardRetryMutation, mutation: mutation, err: err}
		case taskBoardRetryAuthoringLaunchAction:
			mutation, err := m.gateway.RetryAuthoringLaunch(m.ctx, app.TaskBoardRetryAuthoringLaunchRequest{OperationID: pending.operationID})
			return taskBoardMutationMsg{kind: taskBoardRetryAuthoringLaunchMutation, mutation: mutation, err: err}
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

// taskItemForID finds the board projection of one Task across all columns.
// It is used to refresh the open detail pane in place after a snapshot load.
func taskItemForID(pending, running, completed []TaskItem, id string) *TaskItem {
	for _, column := range [][]TaskItem{pending, running, completed} {
		for index := range column {
			if column[index].ID == id {
				item := column[index]
				return &item
			}
		}
	}
	return nil
}

func taskItemsForSnapshot(snapshot app.TaskBoardSnapshot) (pending, running, completed []TaskItem) {
	for _, launch := range snapshot.PendingAuthoringLaunches {
		copy := launch
		pending = append(pending, TaskItem{
			ID:              launch.OperationID,
			Slug:            launch.Slug,
			Name:            launch.Title,
			RepoURL:         launch.RepositoryURL,
			CommitSHA:       launch.CommitSHA,
			State:           TaskPending,
			RunStatus:       launch.Status,
			Lifecycle:       "source_capture_failed",
			AuthoringLaunch: &copy,
		})
	}
	for _, task := range snapshot.Tasks {
		var evaluator *app.TaskBoardEvaluatorStatus
		if task.Evaluator != nil {
			copy := *task.Evaluator
			evaluator = &copy
		}
		item := TaskItem{
			ID:           task.ID,
			Slug:         task.Slug,
			Name:         task.Title,
			RepoURL:      task.RepositoryURL,
			CommitSHA:    task.CommitSHA,
			RunID:        task.RunID,
			CurrentStage: task.CurrentStage,
			OperatorSummary: cloneTaskBoardOperatorSummaryForTUI(
				task.OperatorSummary,
			),
			RunStatus:   task.RunStatus,
			Lifecycle:   task.LifecycleState,
			Review:      task.Review,
			OpenReviews: task.OpenReviewCount,
			Evaluator:   evaluator,
			Runs:        make([]TaskRunItem, 0, len(task.Runs)),
		}
		for _, run := range task.Runs {
			var authoringEvidence *app.TaskBoardAuthoringEvidence
			if run.AuthoringEvidence != nil {
				copy := *run.AuthoringEvidence
				copy.Claims = append([]app.TaskBoardAuthoringClaim(nil), run.AuthoringEvidence.Claims...)
				copy.Lineage = append([]app.TaskBoardAuthoringArtifact(nil), run.AuthoringEvidence.Lineage...)
				authoringEvidence = &copy
			}
			var standardProtocolRetry *app.TaskBoardStandardProtocolRetry
			if run.StandardProtocolRetry != nil {
				copy := *run.StandardProtocolRetry
				standardProtocolRetry = &copy
			}
			item.Runs = append(item.Runs, TaskRunItem{
				ID:                    run.ID,
				ParentRunID:           run.ParentRunID,
				AuthoringEvidence:     authoringEvidence,
				AgentTurnTranscripts:  append([]app.TaskBoardAgentTranscript(nil), run.AgentTurnTranscripts...),
				Status:                run.Status,
				CurrentStage:          run.CurrentStage,
				OperatorSummary:       cloneTaskBoardOperatorSummaryForTUI(run.OperatorSummary),
				FailureStage:          run.FailureStage,
				FailureClass:          run.FailureClass,
				FailureReason:         run.FailureReason,
				FailureCode:           run.FailureCode,
				FailureSummary:        run.FailureSummary,
				FailureJobID:          run.FailureJobID,
				FailureArtifactID:     run.FailureArtifactID,
				FailureRecordedAt:     run.FailureRecordedAt,
				FailureRecoveryAction: run.FailureRecoveryAction,
				CanRedrive:            run.CanRedrive,
				CreatedAt:             run.CreatedAt,
				StartedAt:             run.StartedAt,
				FinishedAt:            run.FinishedAt,
				LogPath:               run.LogPath,
				HasLog:                run.HasLog,
				CanRetry:              run.CanRetry,
				RetryReason:           run.RetryReason,
				RetryStrategy:         run.RetryStrategy,
				StandardProtocolRetry: standardProtocolRetry,
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

func cloneTaskBoardOperatorSummaryForTUI(summary *app.TaskBoardOperatorSummary) *app.TaskBoardOperatorSummary {
	if summary == nil {
		return nil
	}
	copy := *summary
	if summary.LatestValidation != nil {
		validation := *summary.LatestValidation
		if summary.LatestValidation.RecordedAt != nil {
			recordedAt := summary.LatestValidation.RecordedAt.UTC()
			validation.RecordedAt = &recordedAt
		}
		copy.LatestValidation = &validation
	}
	return &copy
}

func (m appModel) View() string {
	if m.width == 0 {
		return "loading..."
	}
	contentWidth := m.contentWidth()
	if m.transcript != nil {
		width, height := m.logDimensions()
		return appStyle.Render(lipgloss.JoinVertical(lipgloss.Top,
			headerStyle.Width(contentWidth).Render("Harbor Task Factory"),
			m.transcript.View(width, height),
			m.statusView(),
			footerStyle.Render("[↑↓/jk] 滚动  [←→/hl] 切换回合  [PgUp/PgDn] 翻页  [q] 返回详情"),
		))
	}

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
			detailHeight -= lipgloss.Height(prompt) + 1
			footerText := "[enter] 确认操作  [esc] 取消"
			if m.recoveryPreviewInFlight {
				footerText = "正在核验断点恢复计划..."
			} else if m.protocolPreviewInFlight {
				footerText = "正在核验协议重试来源..."
			} else if m.protocolPrepareInFlight {
				footerText = "正在准备协议重试..."
			} else if m.action.protocolPrepared != nil {
				footerText = "[enter] 确认重试当前阶段  [esc] 取消"
			} else if m.action.protocolPreview != nil {
				footerText = "[enter] 准备重试当前阶段  [esc] 取消"
			} else if m.action.recoveryPreview != nil {
				footerText = "[enter] 确认从此断点恢复  [esc] 取消"
			} else if m.action.kind == taskBoardRetryAction && requiresRecoveryPreview(m.action.strategy) {
				footerText = "[enter] 查看断点恢复计划  [esc] 取消"
			} else if m.action.kind == taskBoardRetryAction && requiresProtocolRetryPreview(m.action.strategy) {
				footerText = "[enter] 查看协议重试来源  [esc] 取消"
			}
			footer = footerStyle.Render(footerText)
		}
		if m.evaluatorAction != nil {
			prompt = m.evaluatorAction.View(contentWidth)
			detailHeight -= lipgloss.Height(prompt) + 1
			footer = footerStyle.Render(evaluatorActionFooter(m.evaluatorAction))
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
	input := m.input.View(contentWidth)

	boardHeight := m.height - 5
	if input != "" {
		boardHeight -= lipgloss.Height(input)
	}
	if status != "" {
		boardHeight--
	}
	footer := "[tab] 切换面板  [hjkl/↑↓←→] 导航  [d] 详情  [q] 退出"
	if m.authoringAvailable {
		footer = "[n] 从配置新建  " + footer
	}
	return appStyle.Render(lipgloss.JoinVertical(lipgloss.Top,
		headerStyle.Width(contentWidth).Render("Harbor Task Factory"),
		m.board.View(contentWidth, max(1, boardHeight)),
		input,
		status,
		footerStyle.Render(footer),
	))
}

func detailFooter(detail *detailModel) string {
	return footerStyle.Render(detailFooterText(detail))
}

func detailFooterText(detail *detailModel) string {
	actions := make([]string, 0, 4)
	if detail != nil && detail.canRetryAuthoringLaunch() {
		actions = append(actions, "[t] 重试源码捕获")
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
		if evaluator := detail.evaluatorStatus(); evaluator != nil {
			if evaluator.CanLaunch {
				actions = append(actions, "[e] 启动评测")
			}
			if evaluator.CanAdopt {
				actions = append(actions, "[v] 采用评测证据")
			}
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
