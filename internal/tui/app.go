package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
)

func (m model) Init() tea.Cmd {
	if m.lifecycle == nil {
		return nil
	}
	m.taskHub.Loading = true
	return tea.Batch(m.loadTaskHubV2(), taskHubPollCmd())
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m = m.cloneForUpdate()
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.refreshComponentSizes()
		return m, nil
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		if m.done || m.view == viewStart || m.view == viewHub {
			return m, nil
		}
		return m, cmd
	case toastExpiredMsg:
		if msg.id == m.toast.ID {
			m.toast.Message = ""
		}
		return m, nil
	case confirmOpenedMsg:
		return m, nil
	case tea.MouseMsg:
		return m, m.handleMouse(msg)
	case tea.FocusMsg:
		m.bindPages()
		if page := m.router.Page(m.view); page != nil {
			page.Focus()
		}
		return m, nil
	case tea.BlurMsg:
		m.bindPages()
		if page := m.router.Page(m.view); page != nil {
			page.Blur()
		}
		return m, nil
	case tea.KeyMsg:
		if handled, cmd := m.handleGlobalKey(msg); handled {
			return m, cmd
		}
		m.bindPages()
		if handled, cmd := m.router.Dispatch(msg); handled {
			return m, cmd
		}
	case runnerEventMsg:
		event := domain.RunnerEvent(msg)
		m.applyRunnerEvent(event)
		return m, m.waitEvent()
	case editorDoneMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		if msg.before.changed(msg.after) {
			if m.editedFiles == nil {
				m.editedFiles = map[string]string{}
			}
			m.editedFiles[msg.path] = editSummary(msg.before, msg.after)
		}
		return m, m.showToast("工件编辑已完成", toastSuccess)
	case gateDecisionWrittenMsg:
		if msg.err != nil {
			m.err = msg.err
			m.notice = ""
			m.activeGate = msg.gate
			m.view = viewGate
			return m, nil
		}
		m.err = nil
		if msg.path != "" {
			m.summary.GateDecisions = mergeGateDecisions(m.summary.GateDecisions, []domain.GateDecision{msg.decision})
			m.notice = fmt.Sprintf("决定已写入 %s。快照模式只写决定文件，需由外部运行器消费。", msg.path)
		} else {
			m.notice = ""
		}
		m.resetGateLocalState()
		return m, m.showToast("审查决定已提交", toastSuccess)
	case runnerDoneMsg:
		m.summary = msg.summary
		m.err = msg.err
		m.done = true
		m.view = viewDone
		return m, nil
	case workspaceRefreshMsg:
		m.applyWorkspaceSnapshot(msg.summary, msg.events)
		if m.done {
			return m, nil
		}
		return m, m.refreshWorkspace()
	case taskHubPollMsg:
		if m.view == viewHub && m.lifecycle != nil {
			m.taskHub.Loading = true
			return m, tea.Batch(m.loadTaskHubV2(), taskHubPollCmd())
		}
		return m, nil
	case taskHubLoadedMsg:
		if msg.err != nil {
			m.taskHub.Loading = false
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.applyTaskHubSnapshot(msg.snapshot)
		return m, nil
	case taskHubSearchMsg:
		if m.lifecycle != nil && m.hubSearching && strings.TrimSpace(m.hubSearch.Value()) == msg.query {
			m.taskHub.Query.Filter = msg.query
			m.hubFilter = msg.query
			m.taskHub.Loading = true
			return m, m.loadTaskHubV2()
		}
	case taskHubPrefixTimeoutMsg:
		if m.taskHubPrefix.Prefix != 0 && m.taskHubPrefix.Sequence == msg.sequence {
			m.clearTaskHubPrefix()
		}
		return m, nil
	case taskHubPlanMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, m.showToast("生命周期计划生成失败", toastError)
		}
		preview := msg.preview.Clone()
		m.taskHubPlan = &preview
		command := msg.command
		m.taskHubPlanCommand = &command
		m.notice = "已生成 " + taskHubActionLabel(msg.command.Action) + " 的计划预览。"
		return m, m.showToast("计划预览已更新", toastSuccess)
	case taskHubMutationPreparedMsg:
		if m.taskHubMutation == nil || m.taskHubMutation.IdempotencyKey != msg.idempotencyKey || m.taskHubMutation.Phase != taskHubMutationPreparing {
			return m, nil
		}
		m.taskHubMutation.Phase = taskHubMutationReady
		if msg.err != nil {
			m.taskHubMutation.Error = msg.err.Error()
			m.err = msg.err
			return m, m.showToast("冻结生命周期计划失败", toastError)
		}
		preview := msg.prepared.Preview.Clone()
		m.taskHubMutation.Preview = preview
		if err := m.taskHubMutation.lockFrozenProvenance(msg.prepared.Actor, msg.prepared.Reason); err != nil {
			m.taskHubMutation.Error = err.Error()
			m.err = err
			return m, m.showToast("冻结计划缺少权威操作来源", toastError)
		}
		m.taskHubPlan = &preview
		m.err = nil
		m.notice = "已冻结计划；请再次确认后提交执行。"
		return m, m.showToast("生命周期计划已冻结", toastSuccess)
	case taskHubMutationExecutedMsg:
		if m.taskHubMutation == nil || m.taskHubMutation.IdempotencyKey != msg.idempotencyKey || m.taskHubMutation.Phase != taskHubMutationExecuting {
			return m, nil
		}
		m.taskHubMutation.Phase = taskHubMutationReady
		if msg.err != nil {
			m.taskHubMutation.Error = msg.err.Error()
			m.err = msg.err
			return m, m.showToast("生命周期操作未完成；可使用相同幂等键重试", toastError)
		}
		summary := strings.TrimSpace(msg.result.Summary)
		if summary == "" {
			summary = taskHubActionLabel(msg.result.Action) + "已提交"
		}
		m.closeTaskHubMutation()
		m.taskHubPlan = nil
		m.taskHubPlanCommand = nil
		m.taskHub.Loading = true
		m.err = nil
		m.notice = summary
		return m, tea.Batch(m.showToast(summary, toastSuccess), m.loadTaskHubV2())
	case taskHubRunControlMutationExecutedMsg:
		if m.taskHubMutation == nil || m.taskHubMutation.IdempotencyKey != msg.idempotencyKey || m.taskHubMutation.Phase != taskHubMutationExecuting {
			return m, nil
		}
		m.taskHubMutation.Phase = taskHubMutationReady
		if msg.err != nil {
			m.taskHubMutation.Error = msg.err.Error()
			m.err = msg.err
			return m, m.showToast("运行控制未完成；可使用相同幂等键重试", toastError)
		}
		summary := strings.TrimSpace(msg.result.Summary)
		if summary == "" {
			summary = taskHubRunControlActionLabel(msg.result.Action) + "请求已提交"
		}
		m.closeTaskHubMutation()
		m.taskHub.Loading = true
		m.err = nil
		m.notice = summary
		return m, tea.Batch(m.showToast(summary, toastSuccess), m.loadTaskHubV2())
	case taskHubDetailLoadedMsg:
		if m.taskHubDetail == nil || !sameTaskHubDetailQuery(m.taskHubDetail.Query, msg.query) {
			return m, nil
		}
		m.taskHubDetail.Loading = false
		if msg.err != nil {
			m.taskHubDetail.Error = msg.err.Error()
			m.err = msg.err
			return m, m.showToast("生命周期详情读取失败", toastError)
		}
		detail := msg.detail.Clone()
		m.taskHubDetail.Detail = detail
		m.taskHubDetail.Error = ""
		m.taskHubDetail.Scroll = 0
		m.syncTaskHubDetailSelections()
		m.err = nil
		m.notice = "已刷新只读生命周期详情。"
		return m, nil
	case taskHubRunControlPlanMsg:
		// A late read-only preview must not reopen an overlay that the operator
		// already dismissed with Esc, nor replace a newer selection's preview.
		if m.runControl == nil || m.runControl.RunID != msg.runID || m.runControl.SelectedAction != msg.action {
			return m, nil
		}
		if msg.err != nil {
			m.err = msg.err
			return m, m.showToast("运行控制预览生成失败", toastError)
		}
		preview := msg.preview.Clone()
		m.runControl.Preview = &preview
		m.notice = "已生成 " + taskHubRunControlActionLabel(msg.action) + " 的只读影响预览。"
		return m, m.showToast("运行控制预览已更新", toastSuccess)
	}
	return m, nil
}

func (m model) View() string {
	m = m.cloneForUpdate()
	if m.width == 0 {
		return "正在启动 Harbor 出题工坊...\n"
	}
	body := m.currentPage().View(m.width, m.height)
	return renderFrame(m, body)
}
