package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/purplevoid/harbor-factory/internal/harbor/nodes"
)

func (m *model) setView(view viewMode) {
	m.view = view
	if m.router == nil {
		m.router = newPageRouter(view)
	} else {
		m.router.SwitchTo(view)
	}
	switch view {
	case viewHub:
		m.focusMgr.SetCurrent(focusPage)
	case viewStart:
		m.focusMgr.SetCurrent(focusStartField)
	case viewOverview:
		m.focusMgr.SetCurrent(focusOverviewTable)
	case viewGate:
		m.focusMgr.SetCurrent(focusGateChecklist)
	case viewNodeDetail:
		m.focusMgr.SetCurrent(focusDetailViewport)
	case viewLogs:
		m.focusMgr.SetCurrent(focusLogsViewport)
	default:
		m.focusMgr.SetCurrent(focusPage)
	}
}

func (m *model) cyclePage(delta int) {
	pages := []viewMode{viewOverview}
	if m.activeGate != nil {
		pages = append(pages, viewGate)
	}
	pages = append(pages, viewNodeDetail, viewLogs)
	if m.done {
		pages = append(pages, viewDone)
	}
	if m.store != nil || m.lifecycle != nil {
		pages = append(pages, viewHub)
	}
	idx := 0
	for i, p := range pages {
		if p == m.view {
			idx = i
			break
		}
	}
	m.setView(pages[(idx+delta+len(pages))%len(pages)])
}

func (m *model) handleGlobalKey(msg tea.KeyMsg) (bool, tea.Cmd) {
	key := msg.String()
	if m.taskHubDetail != nil {
		return true, m.updateTaskHubDetailKey(msg)
	}
	if m.taskHubHelpVisible {
		if key == "?" || key == "esc" || key == "q" {
			m.taskHubHelpVisible = false
			m.focusMgr.Pop()
		}
		return true, nil
	}
	if m.taskHubMutation != nil {
		return true, m.updateTaskHubMutationKey(msg)
	}
	if m.runControl != nil {
		return true, m.updateRunControlKey(msg)
	}
	if m.confirm != nil {
		return true, m.updateConfirmKey(msg)
	}
	if key == "ctrl+x" {
		m.openRunControl()
		return true, nil
	}
	if m.helpVisible {
		if key == "?" || key == "esc" || key == "q" {
			m.helpVisible = false
			if m.router != nil {
				m.router.PopOverlay()
			}
			m.focusMgr.Pop()
		}
		return true, nil
	}
	if m.gateEditingNote {
		return false, nil
	}
	if m.searching {
		return true, m.updateSearchKey(msg)
	}
	if m.hubSearching {
		if m.lifecycle != nil {
			return true, m.updateTaskHubV2Search(msg)
		}
		return true, nil
	}
	if m.lifecycle != nil && m.view == viewHub {
		if handled, cmd := m.handleTaskHubPrefixKey(msg); handled {
			return true, cmd
		}
	}
	if m.view == viewStart {
		// Printable runes belong to the focused form input. In particular,
		// numbers and "?" must not leak into navigation/help shortcuts.
		if isTextStartField(m.startField) && msg.Type == tea.KeyRunes {
			return false, nil
		}
		if key == "?" {
			m.helpVisible = true
			if m.router != nil {
				m.router.PushOverlay(&helpOverlay{view: m.view})
			}
			m.focusMgr.Push(focusOverlay)
			return true, nil
		}
		if key == "esc" && m.startStep == startStepBasic && m.store != nil {
			return true, m.returnToHub()
		}
		return false, nil
	}
	switch key {
	case "?":
		if m.lifecycle != nil && m.view == viewHub {
			m.taskHubHelpVisible = true
			m.focusMgr.Push(focusOverlay)
			return true, nil
		}
		m.helpVisible = true
		if m.router != nil {
			m.router.PushOverlay(&helpOverlay{view: m.view, readOnly: m.readOnly})
		}
		m.focusMgr.Push(focusOverlay)
		return true, nil
	case "/":
		if m.view == viewOverview || m.view == viewGate || m.view == viewLogs || m.view == viewNodeDetail {
			m.beginSearch()
			return true, nil
		}
	case "1":
		if m.store != nil || m.lifecycle != nil {
			return true, m.returnToHub()
		}
	case "ctrl+o", "2":
		m.setView(viewOverview)
		return true, nil
	case "ctrl+g", "3":
		if m.activeGate != nil {
			m.setView(viewGate)
		} else {
			return true, m.showToast("当前没有活跃审查关卡", toastWarning)
		}
		return true, nil
	case "ctrl+d", "4":
		m.setView(viewNodeDetail)
		return true, nil
	case "ctrl+l", "5":
		m.setView(viewLogs)
		return true, nil
	case "ctrl+e":
		if m.done {
			m.setView(viewDone)
		} else {
			return true, m.showToast("运行尚未完成", toastWarning)
		}
		return true, nil
	case "ctrl+q", "ctrl+c":
		return true, m.requestQuit()
	case "q":
		if m.view != viewStart {
			return true, m.requestQuit()
		}
	case "esc":
		if m.view == viewDone && (m.store != nil || m.lifecycle != nil) {
			return true, m.returnToHub()
		}
		if m.view != viewOverview && m.view != viewStart && m.view != viewHub {
			m.setView(viewOverview)
			return true, nil
		}
		if m.view == viewOverview && (m.store != nil || m.lifecycle != nil) {
			return true, m.returnToHub()
		}
	case "shift+tab":
		if m.view == viewOverview || m.view == viewDone {
			m.cyclePage(-1)
			return true, nil
		}
	}
	return false, nil
}

func (m *model) requestQuit() tea.Cmd {
	if m.hasActiveRun() {
		dialog := newConfirmDialog(confirmQuit, "确认退出", "退出控制台不会停止当前运行，是否继续？")
		dialog.FocusedYes = false
		m.openConfirm(dialog)
		return func() tea.Msg { return confirmOpenedMsg{} }
	}
	return tea.Quit
}

func (m *model) updateConfirmKey(msg tea.KeyMsg) tea.Cmd {
	d := m.confirm
	if d == nil {
		return nil
	}
	switch msg.String() {
	case "left", "right", "tab", "shift+tab":
		d.FocusedYes = !d.FocusedYes
		return nil
	case "n", "N", "esc":
		m.cancelConfirm()
		return nil
	case "y", "Y":
		d.FocusedYes = true
	case "enter":
		if !d.FocusedYes {
			m.cancelConfirm()
			return nil
		}
	default:
		return nil
	}
	m.confirm = nil
	if m.router != nil {
		m.router.PopOverlay()
	}
	m.focusMgr.Pop()
	switch d.Action {
	case confirmApprove, confirmReject:
		gate := d.Gate
		m.activeGate = gate
		approved := d.Action == confirmApprove
		decision := m.makeGateDecision(approved)
		m.activeGate = nil
		m.setView(viewOverview)
		return m.submitDecision(decision, gate)
	case confirmQuit:
		return tea.Quit
	}
	return nil
}

func (m *model) cancelConfirm() {
	if m.confirm != nil && m.confirm.Gate != nil && m.activeGate == nil {
		m.activeGate = m.confirm.Gate
	}
	m.confirm = nil
	if m.router != nil {
		m.router.PopOverlay()
	}
	m.focusMgr.Pop()
}

func (m *model) openConfirm(dialog *ConfirmDialog) {
	m.confirm = dialog
	if m.router != nil {
		m.router.PushOverlay(dialog)
	}
	m.focusMgr.Push(focusOverlay)
}

func (m *model) openRunControl() {
	if m.lifecycle != nil {
		m.runControl = newLifecycleRunControlOverlay(m.taskHubRunByID(m.controlRunID()))
	} else {
		m.runControl = newRunControlOverlay(m.controlRunID(), m.opts.Workspace, m.done, m.readOnly)
	}
	if m.router != nil {
		m.router.PushOverlay(m.runControl)
	}
	m.focusMgr.Push(focusOverlay)
}

func (m model) controlRunID() string {
	if m.lifecycle != nil {
		if runID := strings.TrimSpace(m.taskHub.SelectedRunID); runID != "" {
			return runID
		}
	}
	if runID := strings.TrimSpace(m.summary.RunID); runID != "" {
		return runID
	}
	for index := len(m.events) - 1; index >= 0; index-- {
		if runID := strings.TrimSpace(m.events[index].RunID); runID != "" {
			return runID
		}
	}
	return ""
}

func (m *model) updateRunControlKey(msg tea.KeyMsg) tea.Cmd {
	if m.runControl == nil {
		return nil
	}
	switch msg.String() {
	case "enter", "esc", "q", "ctrl+x":
		if msg.String() != "enter" || m.runControl.selectedIsReturn() || !m.runControl.lifecycleControlAvailable() {
			m.closeRunControl()
			return nil
		}
		if m.runControl.Preview != nil {
			if !m.runControl.Preview.ConfirmationNeeded {
				return m.showToast("当前运行控制预览不可提交", toastWarning)
			}
			return m.openTaskHubRunControlConfirmation()
		}
		return m.previewTaskHubRunControl(m.runControl.SelectedAction)
	case "p":
		m.runControl.selectAction(TaskHubRunControlPause)
	case "k":
		m.runControl.selectAction(TaskHubRunControlCancelStage)
	case "s":
		m.runControl.selectAction(TaskHubRunControlTerminate)
	case "up":
		m.cycleRunControlSelection(-1)
	case "down", "j":
		m.cycleRunControlSelection(1)
	}
	return nil
}

func (m *model) cycleRunControlSelection(delta int) {
	if m.runControl == nil || !m.runControl.lifecycleControlAvailable() {
		return
	}
	choices := []TaskHubRunControlAction{
		"",
		TaskHubRunControlPause,
		TaskHubRunControlCancelStage,
		TaskHubRunControlTerminate,
	}
	current := 0
	for index, action := range choices {
		if action == m.runControl.SelectedAction {
			current = index
			break
		}
	}
	next := (current + delta + len(choices)) % len(choices)
	if choices[next] == "" {
		m.runControl.selectReturn()
		return
	}
	m.runControl.selectAction(choices[next])
}

func (m *model) closeRunControl() {
	m.runControl = nil
	if m.router != nil {
		m.router.PopOverlay()
	}
	m.focusMgr.Pop()
}

func (m model) hasActiveRun() bool {
	if m.lifecycle != nil {
		for _, run := range m.taskHub.Snapshot.Runs {
			if run.Active || run.QueuePosition > 0 {
				return true
			}
		}
	}
	if m.runner != nil && !m.done {
		return true
	}
	if m.scheduler != nil {
		tasks := m.scheduler.Snapshot()
		return tasks.Running+tasks.Queued > 0
	}
	return false
}

func (m *model) beginSearch() {
	m.searching = true
	m.searchInput.Focus()
	m.focusMgr.Push(focusSearch)
}
func (m *model) updateSearchKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		m.searching = false
		m.searchInput.Blur()
		m.focusMgr.Pop()
		return nil
	case "enter":
		m.searching = false
		m.searchInput.Blur()
		m.focusMgr.Pop()
		return nil
	}
	var cmd tea.Cmd
	m.searchInput, cmd = m.searchInput.Update(msg)
	m.filter = m.searchInput.Value()
	if m.view == viewGate {
		m.gateScroll = 0
	}
	return cmd
}

func (m model) footer() string {
	if m.taskHubDetail != nil {
		return subtleStyle.Render("[Tab/←→] 切换分类  [↑↓/j k] 浏览  [r] 刷新  [Esc] 返回")
	}
	if m.taskHubHelpVisible {
		return subtleStyle.Render("[? / Esc / q] 关闭 Task Hub 帮助")
	}
	if m.taskHubMutation != nil {
		return subtleStyle.Render("[Tab] 切换字段  [Enter] 确认提交  [Esc] 取消")
	}
	if m.runControl != nil {
		if m.runControl.lifecycleControlAvailable() {
			return subtleStyle.Render("[P/K/S] 选择  [Enter] 查看影响预览  [Esc] 返回")
		}
		return subtleStyle.Render("[Enter/Esc] 返回")
	}
	var text string
	switch m.view {
	case viewHub:
		if m.lifecycle != nil {
			text = "[Tab Tasks/Runs/Queue] [↑↓ 选择] [Enter/d 详情] [/ 搜索] " + m.taskHubPrefixHint() + " [q 退出] [? 帮助]"
			if m.taskHubPlan != nil {
				text = "[Esc 关闭预览] " + m.taskHubPrefixHint()
			}
		} else {
			text = "[↑↓ 选择] [Enter 打开] [s/S 排序] [/ 搜索] [q 退出] [? 帮助]"
		}
	case viewStart:
		if m.startStep == startStepBasic {
			text = "[Tab/↓ 下一字段] [Shift+Tab/↑ 上一字段] [Space 切换模式] [Ctrl+Space 路径补全] [Enter 下一步] [Ctrl+Q 退出]"
		} else {
			text = "[F1-F4 分组] [Ctrl+←→ 分组] [Tab/↓ 下项] [Shift+Tab/↑ 上项] [Space 切换] [Ctrl+Space 补全] [Enter 启动] [Ctrl+B 入队] [Esc 返回] [Ctrl+Q 退出]"
		}
	case viewGate:
		if m.readOnly {
			text = "[↑↓/j k 滚动] [PgUp/PgDn 翻页] [Home/End 首尾] [Tab 下一工件] [Esc 返回] [Ctrl+L 日志] [q 退出] （只读）"
		} else {
			text = "[↑↓/j k 滚动] [PgUp/PgDn 翻页] [Home/End 首尾] [Ctrl+A/a 批准] [Ctrl+R/r 拒绝]"
			gate := m.activeGate
			if gate == nil && m.confirm != nil {
				gate = m.confirm.Gate
			}
			if gate != nil && gate.GateID == nodes.FinalReview {
				text += " [v Codex指导返修] [c Codex自动循环] [u 人工编辑后重跑]"
			} else if gate != nil && gate.GateID == nodes.ResultReview {
				text += " [Ctrl+V/v 刷新证据]"
			}
			text += " [Ctrl+N 备注/指导] [e 编辑工件] [Tab 下一工件] [Esc 返回] [? 帮助]"
		}
	case viewLogs:
		text = "[↑↓/j k 滚动] [PgUp/PgDn 翻页] [Home/End 首尾] [t 跟踪] [Tab/Shift+Tab 切换文件] [Ctrl+O 总览] [? 帮助]"
	case viewNodeDetail:
		text = "[↑↓/j k 选择] [Tab/Shift+Tab 切换工件] [Ctrl+L 日志] [Ctrl+O 总览] [/ 过滤] [? 帮助]"
	case viewDone:
		text = "[Esc 返回工作区] [1 工作区] [2 总览] [4 详情] [5 日志] [q 退出] [? 帮助]"
	default:
		text = "[↑↓选择] [Enter详情] [PgUp/PgDn翻页] [Tab下一页] [Ctrl+G审查] [Ctrl+L日志] [Ctrl+E完成] [Ctrl+X运行控制] [q退出] [/过滤] [?帮助]"
	}
	if m.readOnly && m.view != viewGate && m.view != viewHub {
		text += "  （只读）"
	}
	if m.searching {
		text = "搜索：" + m.searchInput.View() + "  [Enter 应用] [Esc 关闭]"
	}
	style := subtleStyle
	if m.width > 0 {
		style = style.Width(maxInt(20, m.width))
	}
	return style.Render(text)
}
