package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/purplevoid/harbor-factory/internal/app"
)

type evaluatorActionKind string

const (
	evaluatorLaunchAction evaluatorActionKind = "launch"
	evaluatorAdoptAction  evaluatorActionKind = "adopt"
)

type evaluatorActionPhase string

const (
	evaluatorActionInput    evaluatorActionPhase = "input"
	evaluatorActionPreview  evaluatorActionPhase = "preview"
	evaluatorActionPrepared evaluatorActionPhase = "prepared"
)

type evaluatorActionPrompt struct {
	kind          evaluatorActionKind
	phase         evaluatorActionPhase
	status        app.TaskBoardEvaluatorStatus
	reason        textinput.Model
	validationErr string

	launchPreview   *app.TaskBoardEvaluatorLaunchPreview
	handoffPreview  *app.TaskBoardEvaluatorEvidenceHandoffPreview
	launchPrepared  *app.TaskBoardPreparedEvaluatorLaunch
	handoffPrepared *app.TaskBoardPreparedEvaluatorEvidenceHandoff
}

type pendingTaskBoardEvaluatorAction struct {
	kind        evaluatorActionKind
	taskID      string
	parentRunID string
	childRunID  string
	reason      string
	key         string
}

type taskBoardEvaluatorPreviewMsg struct {
	kind    evaluatorActionKind
	epoch   uint64
	preview app.TaskBoardEvaluatorLaunchPreview
	handoff app.TaskBoardEvaluatorEvidenceHandoffPreview
	err     error
}

type taskBoardEvaluatorPreparedMsg struct {
	kind     evaluatorActionKind
	epoch    uint64
	prepared app.TaskBoardPreparedEvaluatorLaunch
	handoff  app.TaskBoardPreparedEvaluatorEvidenceHandoff
	err      error
}

func newEvaluatorActionPrompt(kind evaluatorActionKind, status app.TaskBoardEvaluatorStatus) *evaluatorActionPrompt {
	input := textinput.New()
	input.Prompt = "原因 "
	input.Placeholder = "记录本次外部评测操作的原因"
	input.CharLimit = 240
	input.Width = 52
	input.Focus()
	return &evaluatorActionPrompt{kind: kind, phase: evaluatorActionInput, status: status, reason: input}
}

func (prompt *evaluatorActionPrompt) View(width int) string {
	if prompt == nil {
		return ""
	}
	label := "启动 CodeEdge Qwen/Opus 评测"
	if prompt.kind == evaluatorAdoptAction {
		label = "采用 CodeEdge 评测证据"
	}
	content := detailSectionTitleStyle.Render(label)
	switch prompt.phase {
	case evaluatorActionInput:
		content += "\n" + prompt.reason.View()
	case evaluatorActionPreview:
		content += "\n" + prompt.previewView(max(1, width-4))
	case evaluatorActionPrepared:
		content += "\n" + prompt.preparedView(max(1, width-4))
	}
	if prompt.validationErr != "" {
		content += "\n" + failStyleV2.Render(prompt.validationErr)
	}
	return inputStyle.Width(max(1, width)).Render(content)
}

func (prompt *evaluatorActionPrompt) previewView(width int) string {
	if prompt.kind == evaluatorLaunchAction && prompt.launchPreview != nil {
		preview := prompt.launchPreview
		return renderDetailSection("评测计划", detailFields(width,
			detailField("Phase-1 Run", preview.ParentRunID, width),
			detailField("TaskRevision", preview.RevisionID, width),
			detailField("子工作流", preview.TemplateID+"@"+preview.TemplateVersion, width),
			detailField("Profile 指纹", preview.ExecutionProfileFingerprint, width),
			detailField("Spec 指纹", preview.ExecutionSpecFingerprint, width),
		), width)
	}
	if prompt.kind == evaluatorAdoptAction && prompt.handoffPreview != nil {
		preview := prompt.handoffPreview
		return renderDetailSection("证据采用计划", detailFields(width,
			detailField("Phase-1 Run", preview.ParentRunID, width),
			detailField("评测子 Run", preview.ChildRunID, width),
			detailField("TaskRevision", preview.RevisionID, width),
			detailField("Handoff 指纹", preview.HandoffFingerprint, width),
			detailField("Qwen trial-set", preview.QwenTrialFingerprint, width),
			detailField("Opus trial-set", preview.OpusTrialFingerprint, width),
		), width)
	}
	return failStyleV2.Render("评测计划不可用")
}

func (prompt *evaluatorActionPrompt) preparedView(width int) string {
	if prompt.kind == evaluatorLaunchAction && prompt.launchPrepared != nil {
		prepared := prompt.launchPrepared
		return renderDetailSection("已冻结评测输入", detailFields(width,
			detailField("Phase-1 Run", prepared.ParentRunID, width),
			detailField("Input bundle", prepared.InputBundleID, width),
			detailField("Profile 指纹", prepared.ExecutionProfileFingerprint, width),
			detailField("Spec 指纹", prepared.ExecutionSpecFingerprint, width),
		), width)
	}
	if prompt.kind == evaluatorAdoptAction && prompt.handoffPrepared != nil {
		prepared := prompt.handoffPrepared
		return renderDetailSection("已冻结证据采用", detailFields(width,
			detailField("操作 ID", prepared.OperationID, width),
			detailField("Phase-1 Run", prepared.ParentRunID, width),
			detailField("评测子 Run", prepared.ChildRunID, width),
			detailField("Handoff 指纹", prepared.HandoffFingerprint, width),
		), width)
	}
	return failStyleV2.Render("冻结确认不可用")
}

func (m appModel) openEvaluatorAction(kind evaluatorActionKind, inputCmd tea.Cmd) (tea.Model, tea.Cmd) {
	if m.mutationInFlight() || m.refreshInFlight || m.detail == nil {
		m.notice = "请等待当前操作完成"
		return m, inputCmd
	}
	status := m.detail.evaluatorStatus()
	if status == nil {
		m.notice = "当前任务没有可操作的 CodeEdge evaluator"
		return m, inputCmd
	}
	if kind == evaluatorLaunchAction && !status.CanLaunch {
		m.notice = "当前 Phase-1 Run 不可启动 evaluator"
		return m, inputCmd
	}
	if kind == evaluatorAdoptAction && !status.CanAdopt {
		m.notice = "当前 evaluator 证据不可采用"
		return m, inputCmd
	}
	copy := *status
	m.evaluatorAction = newEvaluatorActionPrompt(kind, copy)
	m.pendingEvaluatorAction = nil
	return m, tea.Batch(inputCmd, textinput.Blink)
}

func (m appModel) handleEvaluatorActionKey(msg tea.KeyMsg, inputCmd tea.Cmd) (tea.Model, tea.Cmd) {
	if m.evaluatorAction == nil {
		return m, inputCmd
	}
	if m.mutationInFlight() {
		m.notice = "操作仍在提交，请等待结果"
		return m, inputCmd
	}
	switch msg.String() {
	case "esc":
		m.evaluatorAction = nil
		m.pendingEvaluatorAction = nil
		m.evaluatorActionEpoch++
		return m, inputCmd
	case "enter":
		switch m.evaluatorAction.phase {
		case evaluatorActionInput:
			reason := strings.TrimSpace(m.evaluatorAction.reason.Value())
			if reason == "" {
				m.evaluatorAction.validationErr = "操作原因不能为空"
				return m, inputCmd
			}
			return m.beginEvaluatorPreview(reason, inputCmd)
		case evaluatorActionPreview:
			return m.beginEvaluatorPrepare(inputCmd)
		case evaluatorActionPrepared:
			return m.beginEvaluatorConfirm(inputCmd)
		}
	}
	var command tea.Cmd
	m.evaluatorAction.reason, command = m.evaluatorAction.reason.Update(msg)
	return m, tea.Batch(inputCmd, command)
}

func (m appModel) beginEvaluatorPreview(reason string, inputCmd tea.Cmd) (tea.Model, tea.Cmd) {
	if m.evaluatorAction == nil || m.detail == nil {
		return m, inputCmd
	}
	m.evaluatorAction.validationErr = ""
	m.evaluatorActionEpoch++
	epoch := m.evaluatorActionEpoch
	m.activeMutation = taskBoardEvaluatorPreviewMutation
	return m, tea.Batch(inputCmd, m.previewEvaluatorAction(m.evaluatorAction.kind, m.detail.task.ID, m.evaluatorAction.status, reason, epoch))
}

func (m appModel) previewEvaluatorAction(kind evaluatorActionKind, taskID string, status app.TaskBoardEvaluatorStatus, reason string, epoch uint64) tea.Cmd {
	return func() tea.Msg {
		if m.gateway == nil {
			return taskBoardEvaluatorPreviewMsg{kind: kind, epoch: epoch, err: fmt.Errorf("task board service is not configured")}
		}
		ctx := m.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		if kind == evaluatorLaunchAction {
			preview, err := m.gateway.PreviewEvaluatorLaunch(ctx, app.TaskBoardEvaluatorLaunchPreviewRequest{TaskID: taskID, ParentRunID: status.ParentRunID})
			return taskBoardEvaluatorPreviewMsg{kind: kind, epoch: epoch, preview: preview, err: err}
		}
		preview, err := m.gateway.PreviewEvaluatorEvidenceHandoff(ctx, app.TaskBoardEvaluatorEvidenceHandoffPreviewRequest{TaskID: taskID, ParentRunID: status.ParentRunID, ChildRunID: status.ChildRunID})
		return taskBoardEvaluatorPreviewMsg{kind: kind, epoch: epoch, handoff: preview, err: err}
	}
}

func (m appModel) beginEvaluatorPrepare(inputCmd tea.Cmd) (tea.Model, tea.Cmd) {
	if m.evaluatorAction == nil || m.detail == nil {
		return m, inputCmd
	}
	reason := strings.TrimSpace(m.evaluatorAction.reason.Value())
	current := pendingTaskBoardEvaluatorAction{
		kind: m.evaluatorAction.kind, taskID: m.detail.task.ID, parentRunID: m.evaluatorAction.status.ParentRunID,
		childRunID: m.evaluatorAction.status.ChildRunID, reason: reason,
	}
	if previous := m.pendingEvaluatorAction; previous != nil && previous.kind == current.kind && previous.taskID == current.taskID && previous.parentRunID == current.parentRunID && previous.childRunID == current.childRunID && previous.reason == current.reason {
		current.key = previous.key
	} else {
		key, err := m.newIdempotencyKey()
		if err != nil {
			m.err = err
			return m, inputCmd
		}
		current.key = key
	}
	m.pendingEvaluatorAction = &current
	m.evaluatorActionEpoch++
	epoch := m.evaluatorActionEpoch
	m.activeMutation = taskBoardEvaluatorPrepareMutation
	return m, tea.Batch(inputCmd, m.prepareEvaluatorAction(current, epoch))
}

func (m appModel) prepareEvaluatorAction(pending pendingTaskBoardEvaluatorAction, epoch uint64) tea.Cmd {
	return func() tea.Msg {
		if m.gateway == nil {
			return taskBoardEvaluatorPreparedMsg{kind: pending.kind, epoch: epoch, err: fmt.Errorf("task board service is not configured")}
		}
		if pending.kind == evaluatorLaunchAction {
			prepared, err := m.gateway.PrepareEvaluatorLaunch(m.ctx, app.TaskBoardEvaluatorLaunchRequest{IdempotencyKey: pending.key, TaskID: pending.taskID, ParentRunID: pending.parentRunID, Reason: pending.reason})
			return taskBoardEvaluatorPreparedMsg{kind: pending.kind, epoch: epoch, prepared: prepared, err: err}
		}
		prepared, err := m.gateway.PrepareEvaluatorEvidenceHandoff(m.ctx, app.TaskBoardEvaluatorEvidenceHandoffRequest{IdempotencyKey: pending.key, TaskID: pending.taskID, ParentRunID: pending.parentRunID, ChildRunID: pending.childRunID, Reason: pending.reason})
		return taskBoardEvaluatorPreparedMsg{kind: pending.kind, epoch: epoch, handoff: prepared, err: err}
	}
}

func (m appModel) beginEvaluatorConfirm(inputCmd tea.Cmd) (tea.Model, tea.Cmd) {
	if m.evaluatorAction == nil || m.pendingEvaluatorAction == nil {
		return m, inputCmd
	}
	pending := *m.pendingEvaluatorAction
	if pending.kind != m.evaluatorAction.kind {
		m.evaluatorAction.validationErr = "评测确认已过期，请重新核验"
		m.pendingEvaluatorAction = nil
		return m, inputCmd
	}
	if pending.kind == evaluatorLaunchAction {
		m.activeMutation = taskBoardEvaluatorConfirmMutation
	} else {
		m.activeMutation = taskBoardEvaluatorAdoptMutation
	}
	return m, tea.Batch(inputCmd, m.confirmEvaluatorAction(pending))
}

func (m appModel) confirmEvaluatorAction(pending pendingTaskBoardEvaluatorAction) tea.Cmd {
	return func() tea.Msg {
		if m.gateway == nil {
			kind := taskBoardEvaluatorConfirmMutation
			if pending.kind == evaluatorAdoptAction {
				kind = taskBoardEvaluatorAdoptMutation
			}
			return taskBoardMutationMsg{kind: kind, err: fmt.Errorf("task board service is not configured")}
		}
		if pending.kind == evaluatorLaunchAction {
			mutation, err := m.gateway.ConfirmEvaluatorLaunch(m.ctx, app.TaskBoardEvaluatorLaunchRequest{IdempotencyKey: pending.key, TaskID: pending.taskID, ParentRunID: pending.parentRunID, Reason: pending.reason})
			return taskBoardMutationMsg{kind: taskBoardEvaluatorConfirmMutation, mutation: mutation, err: err}
		}
		mutation, err := m.gateway.AdoptEvaluatorEvidenceHandoff(m.ctx, app.TaskBoardEvaluatorEvidenceHandoffRequest{IdempotencyKey: pending.key, TaskID: pending.taskID, ParentRunID: pending.parentRunID, ChildRunID: pending.childRunID, Reason: pending.reason})
		return taskBoardMutationMsg{kind: taskBoardEvaluatorAdoptMutation, mutation: mutation, err: err}
	}
}

func (m appModel) applyEvaluatorPreview(msg taskBoardEvaluatorPreviewMsg, inputCmd tea.Cmd) (tea.Model, tea.Cmd) {
	if msg.epoch != m.evaluatorActionEpoch || m.evaluatorAction == nil || msg.kind != m.evaluatorAction.kind {
		return m, inputCmd
	}
	m.activeMutation = ""
	if msg.err != nil {
		m.evaluatorAction.validationErr = "无法核验评测计划；请刷新任务状态后重试"
		return m, inputCmd
	}
	if msg.kind == evaluatorLaunchAction {
		if msg.preview.ParentRunID != m.evaluatorAction.status.ParentRunID {
			m.evaluatorAction.validationErr = "评测计划未绑定当前 Phase-1 Run"
			return m, inputCmd
		}
		m.evaluatorAction.launchPreview = &msg.preview
	} else {
		if msg.handoff.ParentRunID != m.evaluatorAction.status.ParentRunID || msg.handoff.ChildRunID != m.evaluatorAction.status.ChildRunID {
			m.evaluatorAction.validationErr = "证据采用计划未绑定当前 parent/child"
			return m, inputCmd
		}
		m.evaluatorAction.handoffPreview = &msg.handoff
	}
	m.evaluatorAction.phase = evaluatorActionPreview
	m.evaluatorAction.validationErr = ""
	return m, inputCmd
}

func (m appModel) applyEvaluatorPrepared(msg taskBoardEvaluatorPreparedMsg, inputCmd tea.Cmd) (tea.Model, tea.Cmd) {
	if msg.epoch != m.evaluatorActionEpoch || m.evaluatorAction == nil || msg.kind != m.evaluatorAction.kind || m.pendingEvaluatorAction == nil {
		return m, inputCmd
	}
	m.activeMutation = ""
	if msg.err != nil {
		m.evaluatorAction.validationErr = "无法冻结评测操作；请检查运行状态后重试"
		return m, inputCmd
	}
	if msg.kind == evaluatorLaunchAction {
		if msg.prepared.ParentRunID != m.pendingEvaluatorAction.parentRunID || msg.prepared.InputBundleID == "" {
			m.evaluatorAction.validationErr = "冻结评测输入未绑定当前 Phase-1 Run"
			return m, inputCmd
		}
		m.evaluatorAction.launchPrepared = &msg.prepared
	} else {
		if msg.handoff.ParentRunID != m.pendingEvaluatorAction.parentRunID || msg.handoff.ChildRunID != m.pendingEvaluatorAction.childRunID || msg.handoff.OperationID == "" {
			m.evaluatorAction.validationErr = "冻结证据采用未绑定当前 parent/child"
			return m, inputCmd
		}
		m.evaluatorAction.handoffPrepared = &msg.handoff
	}
	m.evaluatorAction.phase = evaluatorActionPrepared
	m.evaluatorAction.validationErr = ""
	return m, inputCmd
}

func evaluatorActionFooter(prompt *evaluatorActionPrompt) string {
	if prompt == nil {
		return ""
	}
	switch prompt.phase {
	case evaluatorActionInput:
		return "[enter] 核验计划  [esc] 取消"
	case evaluatorActionPreview:
		return "[enter] 冻结确认  [esc] 取消"
	default:
		return "[enter] 最终确认  [esc] 取消"
	}
}
