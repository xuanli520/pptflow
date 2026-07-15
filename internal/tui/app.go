package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

func (m model) Init() tea.Cmd {
	if m.lifecycle == nil {
		return nil
	}
	m.taskHub.Loading = true
	return tea.Batch(m.initialTaskHubLoadV2(), taskHubPollCmd())
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m = m.cloneForUpdate()
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.refreshComponentSizes()
		m.toast.Offset = 0
		if m.toastCanScroll() {
			return m, toastScrollCmd(m.toast.ID)
		}
		return m, nil
	case toastExpiredMsg:
		if msg.id == m.toast.ID {
			m.toast.Message = ""
			m.toast.Offset = 0
		}
		return m, nil
	case toastScrollMsg:
		if msg.id != m.toast.ID || !m.toastCanScroll() {
			return m, nil
		}
		cycleLength := toastCycleLength(redactSingleLineUI(m.toast.Message))
		if cycleLength < 1 {
			return m, nil
		}
		m.toast.Offset = (m.toast.Offset + 1) % cycleLength
		return m, toastScrollCmd(msg.id)
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
	case taskHubPollMsg:
		if m.lifecycle != nil {
			m.taskHub.Loading = true
			return m, tea.Batch(m.loadTaskHubV2(), taskHubPollCmd())
		}
		return m, nil
	case taskHubLoadedMsg:
		if msg.sequence != m.taskHubLoadSequence || !sameTaskHubQuery(m.taskHub.Query, msg.query) {
			return m, nil
		}
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
		// A CodeEdge package plan chooses the exact approved frozen Run at the
		// application boundary. Keep the confirmation target aligned with that
		// checkpoint so a later selection change cannot make the form appear to
		// authorize a different Run.
		if command.Action == TaskHubActionPackageRevision && strings.TrimSpace(preview.Expected.RunID) != "" {
			command.Target.TaskID = strings.TrimSpace(preview.Expected.TaskID)
			command.Target.RevisionID = strings.TrimSpace(preview.Expected.RevisionID)
			command.Target.RunID = strings.TrimSpace(preview.Expected.RunID)
			m.taskHub.SelectedTaskID = command.Target.TaskID
			m.taskHub.SelectedRunID = command.Target.RunID
		}
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
		if msg.result.Action == TaskHubActionStartStandardAuthoring {
			// Standard authoring is a global launch, so its request target is
			// intentionally empty. Its completed receipt supplies the new draft
			// Task and source/session Run that the next refresh must preserve.
			m.taskHub.SelectedTaskID = strings.TrimSpace(msg.result.Target.TaskID)
			m.taskHub.SelectedRunID = strings.TrimSpace(msg.result.Target.RunID)
			if m.taskHub.SelectedRunID == "" {
				m.taskHub.SelectedRunID = strings.TrimSpace(msg.result.ExecutionID)
			}
			// A completed global launch must remain visible so the selected output
			// is not immediately replaced by a row from a stale search result.
			m.taskHub.Query.Filter = ""
			m.hubSearch.SetValue("")
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
	case taskHubRunHandoffExecutedMsg:
		return m, m.applyTaskHubExitHandoffResult(msg)
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
