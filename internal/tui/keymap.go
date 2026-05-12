package tui

import tea "github.com/charmbracelet/bubbletea"

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
	m.overview.SetFocus(area)
}

func (m app) handleKey(msg tea.KeyMsg) (app, []tea.Cmd) {
	var cmds []tea.Cmd
	key := msg.String()

	if m.runConfig.active {
		if key == "ctrl+x" {
			m.message = "请先关闭运行配置再终止作业"
			return m, cmds
		}
		return m.handleRunConfigKey(msg)
	}
	if m.confirmCancelTaskID != "" {
		switch key {
		case "y", "Y", "enter":
			taskID := m.confirmCancelTaskID
			jobID := m.confirmCancelJobID
			m.confirmCancelTaskID = ""
			m.confirmCancelJobID = ""
			m.message = "正在发送终止请求 " + taskID
			return m, append(cmds, cancelTaskCmd(m.scheduler, taskID, jobID))
		case "n", "N", "esc":
			m.confirmCancelTaskID = ""
			m.confirmCancelJobID = ""
			m.message = "已取消终止请求"
			return m, cmds
		default:
			return m, cmds
		}
	}

	switch key {
	case "ctrl+c", "ctrl+q":
		return m, []tea.Cmd{tea.Batch(m.shutdownScheduler(), tea.Quit)}
	case "ctrl+x":
		m.openCancelConfirm()
		return m, cmds
	case "ctrl+r":
		m.openRunConfig()
		return m, cmds
	case "tab":
		m.switchPanel(1)
		m.detailFollowTail = true
		return m, append(cmds, m.reloadDetail())
	case "shift+tab":
		m.switchPanel(-1)
		m.detailFollowTail = true
		return m, append(cmds, m.reloadDetail())
	case "esc":
		m.handleEscape()
		return m, cmds
	}

	switch m.focus {
	case focusSearch:
		switch key {
		case "enter":
			m.setFocus(focusOverviewTable)
			if cmd := m.overview.confirmSearch(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		case "down":
			m.setFocus(focusOverviewTable)
		default:
			var cmd tea.Cmd
			m.overview, cmd = m.overview.Update(msg)
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
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
		case "up", "down", "pgup", "pgdown", "home", "end", "s", "S", "z":
			before := m.selectedTaskID()
			beforeKey := m.selectedOverviewDetailKey()
			var cmd tea.Cmd
			m.overview, cmd = m.overview.Update(msg)
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
			after := m.selectedTaskID()
			afterKey := m.selectedOverviewDetailKey()
			if after != "" && (after != before || afterKey != beforeKey) {
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
			m.detailFollowTail = false
		case "down":
			m.detail.LineDown(1)
			if m.detail.AtBottom() {
				m.detailFollowTail = true
			}
		case "pgup":
			m.detail.PageUp()
			m.detailFollowTail = false
		case "pgdown":
			m.detail.PageDown()
			if m.detail.AtBottom() {
				m.detailFollowTail = true
			}
		case "home":
			m.detail.GotoTop()
			m.detailFollowTail = false
		case "end":
			m.detail.GotoBottom()
			m.detailFollowTail = true
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
	m.detailFollowTail = true
	if m.qaMode == "recheck" && len(m.detailVM.RefRuns) > 0 {
		m.setFocus(focusRefRunList)
		return
	}
	m.setFocus(focusStageList)
}

func (m *app) handleEscape() {
	switch {
	case m.runConfig.active:
		m.runConfig = runConfig{}
		m.message = "已取消重跑"
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

func (m *app) openRunConfig() {
	if m.selectedTaskID() == "" {
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
	m.runConfig = newRunConfig(m.selectedTaskID(), m.qaMode, m.selectedRefRun(), m.rerunStageKey(), m.cfg.Docker.KeepRuntime, m.detailVM.DocsSummary.Count)
}

func (m *app) openCancelConfirm() {
	taskID := m.selectedTaskID()
	if taskID == "" {
		m.message = "没有选中的任务"
		return
	}
	job, ok := m.activeJobForTask(taskID)
	if !ok {
		m.message = "该任务没有排队或运行中的作业"
		return
	}
	m.confirmCancelTaskID = taskID
	m.confirmCancelJobID = job.JobID
}

func footerFor(m app) string {
	if m.runConfig.active {
		return "Tab 切换  Space 选择  Enter 确认  Esc 取消"
	}
	if m.confirmCancelTaskID != "" {
		return "y/Enter 确认终止  n/Esc 取消"
	}
	switch m.focus {
	case focusSearch:
		return "Ctrl+C 退出  Tab 执行详情  ↓ 表格  Ctrl+R 重跑  Ctrl+X 终止"
	case focusOverviewTable:
		return "↑↓选择 Enter详情 /搜索 s排序 S反向 PgUp/PgDn翻页 z条数 Ctrl+R重跑 Ctrl+X终止 m模式"
	case focusStageList:
		return "↑↓ 阶段  Enter 详情  Ctrl+R 重跑  Ctrl+X 终止  m 模式"
	case focusRefRunList:
		return "↑↓ 参考运行  Enter 选择  Esc 返回阶段  Ctrl+R 重跑  Ctrl+X 终止"
	case focusDetailViewport:
		return "↑↓ 滚动  PgUp/PgDn 翻页  Esc 返回阶段  Ctrl+X 终止"
	default:
		return "Ctrl+C 退出"
	}
}
