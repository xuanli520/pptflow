package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

func (m *model) setView(view viewMode) {
	// The lifecycle TUI has exactly one page. All secondary experiences are
	// overlays, so a stale legacy key can never navigate to a workspace page.
	if view != viewHub {
		view = viewHub
	}
	m.view = view
	if m.router == nil {
		m.router = newPageRouter(view)
	} else {
		m.router.SwitchTo(view)
	}
	m.focusMgr.SetCurrent(focusPage)
}

func (m *model) handleGlobalKey(msg tea.KeyMsg) (bool, tea.Cmd) {
	key := msg.String()
	if m.exitHandoff != nil {
		return true, m.updateTaskHubExitHandoffKey(msg)
	}
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
	if key == "ctrl+x" {
		m.openRunControl()
		return true, nil
	}
	if m.hubSearching {
		return true, m.updateTaskHubV2Search(msg)
	}
	if handled, cmd := m.handleTaskHubPrefixKey(msg); handled {
		return true, cmd
	}
	switch key {
	case "?":
		m.taskHubHelpVisible = true
		m.focusMgr.Push(focusOverlay)
		return true, nil
	case "ctrl+q", "ctrl+c", "q":
		return true, m.requestQuit()
	}
	return false, nil
}

func (m *model) requestQuit() tea.Cmd {
	if m.hasActiveRun() {
		return m.requestTaskHubExitHandoff()
	}
	return tea.Quit
}

func (m *model) openRunControl() {
	m.runControl = newLifecycleRunControlOverlay(m.taskHubRunByID(m.taskHub.SelectedRunID))
	if m.router != nil {
		m.router.PushOverlay(m.runControl)
	}
	m.focusMgr.Push(focusOverlay)
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
	case "r":
		// Reconcile is deliberately absent from ordinary Runs. Do not turn its
		// mnemonic into a generic recovery shortcut when the authoritative
		// capability projection did not expose it.
		if m.runControl.declaresAction(TaskHubRunControlReconcile) {
			m.runControl.selectAction(TaskHubRunControlReconcile)
		}
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
	choices := []TaskHubRunControlAction{"", TaskHubRunControlPause, TaskHubRunControlCancelStage, TaskHubRunControlTerminate}
	if m.runControl.declaresAction(TaskHubRunControlReconcile) {
		choices = append(choices, TaskHubRunControlReconcile)
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
	for _, run := range m.taskHub.Snapshot.Runs {
		if run.Active || run.QueuePosition > 0 {
			return true
		}
	}
	return false
}

func (m model) footer() string {
	if m.exitHandoff != nil {
		return subtleStyle.Render("[↑↓/j k] 选择  [Space] 勾选/取消  [Enter] 交接并退出  [Esc] 返回")
	}
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
			return subtleStyle.Render(m.runControl.actionKeyHint())
		}
		return subtleStyle.Render("[Enter/Esc] 返回")
	}
	text := "[Tab Tasks/Runs/Queue] [↑↓ 选择] [Enter/d 详情] [/ 搜索] " + m.taskHubPrefixHint() + " [q 退出] [? 帮助]"
	if m.taskHubPlan != nil {
		text = "[Esc 关闭预览] " + m.taskHubPrefixHint()
	}
	if m.width > 0 {
		text = clipDisplay(text, m.width)
	}
	return subtleStyle.Render(strings.TrimSpace(text))
}
