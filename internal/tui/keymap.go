package tui

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
)

type focusArea int

const (
	focusSearch focusArea = iota
	focusOverviewTable
	focusStageList
	focusRefRunList
	focusDetailViewport
)

func (f focusArea) String() string {
	switch f {
	case focusSearch:
		return "search"
	case focusOverviewTable:
		return "overview-table"
	case focusStageList:
		return "stage-list"
	case focusRefRunList:
		return "ref-run-list"
	case focusDetailViewport:
		return "detail-viewport"
	default:
		return "unknown"
	}
}

func (m *app) setFocus(area focusArea) {
	m.focus = area
	if area == focusSearch {
		m.search.Focus()
	} else {
		m.search.Blur()
	}
	if area == focusOverviewTable {
		m.table.Focus()
	} else {
		m.table.Blur()
	}
}

func (m app) handleKey(msg tea.KeyMsg) (app, []tea.Cmd) {
	var cmds []tea.Cmd
	key := msg.String()

	if m.confirm {
		switch key {
		case "y", "Y", "enter":
			m.confirm = false
			m.running = true
			m.message = "流水线运行中..."
			cmds = append(cmds, m.runSelected())
		case "n", "N", "esc":
			m.confirm = false
			m.message = "已取消重跑"
		}
		return m, cmds
	}

	switch key {
	case "ctrl+c", "ctrl+q":
		return m, []tea.Cmd{tea.Quit}
	case "ctrl+r":
		m.openRerunConfirm()
		return m, cmds
	case "ctrl+a":
		m.showAttachHint()
		return m, cmds
	case "ctrl+m":
		m.toggleMode()
		return m, cmds
	case "tab":
		m.switchPanel(1)
		return m, append(cmds, m.reloadDetail())
	case "shift+tab":
		m.switchPanel(-1)
		return m, append(cmds, m.reloadDetail())
	case "esc":
		m.handleEscape()
		return m, cmds
	}

	switch m.focus {
	case focusSearch:
		before := m.selectedTaskID()
		switch key {
		case "enter", "down":
			m.setFocus(focusOverviewTable)
		default:
			var cmd tea.Cmd
			m.search, cmd = m.search.Update(msg)
			cmds = append(cmds, cmd)
			m.refreshRows()
		}
		if m.selectedTaskID() != before {
			cmds = append(cmds, m.reloadDetail())
		}
	case focusOverviewTable:
		switch key {
		case "enter":
			m.enterExecution()
			cmds = append(cmds, m.reloadDetail())
		case "/":
			m.setFocus(focusSearch)
		case "m":
			m.toggleMode()
		case "q":
			m.setFocus(focusSearch)
		case "up", "down", "pgup", "pgdown", "home", "end":
			before := m.selectedTaskID()
			var cmd tea.Cmd
			m.table, cmd = m.table.Update(msg)
			cmds = append(cmds, cmd)
			if m.syncSelectedTaskFromCursor() || m.selectedTaskID() != before {
				cmds = append(cmds, m.reloadDetail())
			}
		}
	case focusStageList:
		switch key {
		case "up":
			m.moveStage(-1)
		case "down":
			m.moveStage(1)
		case "right", "enter":
			m.setFocus(focusDetailViewport)
		case "left":
			if m.qaMode == "recheck" && len(m.detailVM.RefRuns) > 0 {
				m.setFocus(focusRefRunList)
			}
		case "m":
			m.toggleMode()
		case "q":
			m.tab = panelOverview
			m.setFocus(focusOverviewTable)
		}
	case focusRefRunList:
		switch key {
		case "up":
			m.moveRef(-1)
		case "down":
			m.moveRef(1)
		case "pgup":
			m.moveRef(-max(1, m.detail.Height))
		case "pgdown":
			m.moveRef(max(1, m.detail.Height))
		case "left", "esc":
			m.setFocus(focusStageList)
		case "right":
			m.setFocus(focusDetailViewport)
		case "enter":
			if ref := m.selectedRefRun(); ref != "" {
				m.message = "已选择参考运行: " + ref
			}
		case "m":
			m.toggleMode()
		}
	case focusDetailViewport:
		switch key {
		case "up":
			m.detail.LineUp(1)
		case "down":
			m.detail.LineDown(1)
		case "pgup":
			m.detail.PageUp()
		case "pgdown":
			m.detail.PageDown()
		case "home":
			m.detail.GotoTop()
		case "end":
			m.detail.GotoBottom()
		case "left", "esc":
			m.setFocus(focusStageList)
		case "m":
			m.toggleMode()
		case "q":
			m.setFocus(focusStageList)
		}
	}

	return m, cmds
}

func (m *app) switchPanel(delta int) {
	m.tab = (m.tab + delta + 2) % 2
	if m.tab == panelOverview {
		if m.selectedTaskID() == "" {
			m.setFocus(focusSearch)
			return
		}
		m.setFocus(focusOverviewTable)
		return
	}
	m.enterExecution()
}

func (m *app) enterExecution() {
	m.tab = panelExecution
	if m.qaMode == "recheck" && len(m.detailVM.RefRuns) > 0 {
		m.setFocus(focusRefRunList)
		return
	}
	m.setFocus(focusStageList)
}

func (m *app) handleEscape() {
	switch {
	case m.focus == focusSearch:
		m.setFocus(focusOverviewTable)
	case m.tab == panelExecution && m.focus != focusStageList:
		m.setFocus(focusStageList)
	case m.tab == panelExecution:
		m.tab = panelOverview
		m.setFocus(focusOverviewTable)
	default:
		m.setFocus(focusSearch)
	}
}

func (m *app) toggleMode() {
	if m.qaMode == "recheck" {
		m.qaMode = "initial"
		if m.focus == focusRefRunList {
			m.setFocus(focusStageList)
		}
		return
	}
	m.qaMode = "recheck"
	m.syncRefSelection()
	if m.tab == panelExecution && len(m.detailVM.RefRuns) > 0 {
		m.setFocus(focusRefRunList)
	}
}

func (m *app) showAttachHint() {
	if taskID := m.selectedTaskID(); taskID != "" {
		m.message = fmt.Sprintf("附加文档命令: p2r attach %s --file <path>", taskID)
	}
}

func (m *app) openRerunConfirm() {
	if m.selectedTaskID() == "" || m.running {
		return
	}
	m.syncRefSelection()
	if plan := m.rerunStagePlan(); plan.blockedReason != "" {
		m.message = plan.blockedReason
		return
	}
	if m.qaMode == "recheck" && m.selectedRefRun() == "" {
		m.message = "打回重检模式需要选择一个参考运行"
		if m.tab == panelExecution {
			m.setFocus(focusRefRunList)
		}
		return
	}
	m.confirm = true
}

func footerFor(m app) string {
	if m.confirm {
		return "Enter/y 确认  Esc/n 取消"
	}
	switch m.focus {
	case focusSearch:
		return "Ctrl+C 退出  Tab 执行详情  ↓ 表格  Ctrl+R 重跑"
	case focusOverviewTable:
		return "↑↓ 选择  Enter 执行详情  / 搜索  Ctrl+R 重跑  m 模式  Ctrl+C 退出"
	case focusStageList:
		return "↑↓ 阶段  Enter 详情  Ctrl+R 重跑  Ctrl+A 文档  m 模式"
	case focusRefRunList:
		return "↑↓ 参考运行  Enter 选择  Esc 返回阶段  Ctrl+R 重跑"
	case focusDetailViewport:
		return "↑↓ 滚动  PgUp/PgDn 翻页  Esc 返回阶段  Ctrl+A 文档"
	default:
		return "Ctrl+C 退出"
	}
}
