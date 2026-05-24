package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
)

type focusArea int

const (
	focusSearch focusArea = iota
	focusOverviewTable
	focusTaskBoard
	focusTaskInput
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
	case focusTaskBoard:
		return "task-board"
	case focusTaskInput:
		return "task-input"
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
	target := focusTargetPage
	if area == focusTaskInput {
		target = focusTargetInputBox
	}
	if m.settingsOpen {
		target = focusTargetOverlay
	}
	m.focusManager.SetCurrent(target)
	m.overview.SetFocus(area)
	if m.taskInput != nil {
		if area == focusTaskInput {
			m.taskInput.Focus()
		} else {
			m.taskInput.Blur()
		}
	}
}

func (m app) handleKey(msg tea.KeyMsg) (app, []tea.Cmd) {
	var cmds []tea.Cmd
	key := msg.String()

	if m.settingsOpen {
		if handled, overlayCmd := m.settingsUI.Update(msg); handled {
			m.closeSettingsOverlay()
			if overlayCmd != nil {
				cmds = append(cmds, overlayCmd)
			}
			return m, cmds
		}
		switch key {
		case "ctrl+c", "ctrl+q":
			m.message = "设置已打开，按 Esc、Q 或 Ctrl+/ 关闭设置"
			return m, cmds
		default:
			next, settingsCmds := m.handleDockerSettingsKey(msg)
			next.settingsOpen = true
			return next, append(cmds, settingsCmds...)
		}
	}
	switch key {
	case settingsShortcutKey, "ctrl+/":
		m.settingsOpen = true
		m.settingsFocusBeforeOpen = m.focus
		m.settingsFocusCaptured = true
		m.focusManager.Push(focusTargetOverlay)
		if m.router != nil {
			cmds = append(cmds, m.router.PushOverlay(m.settingsUI))
		}
		return m, cmds
	case "/":
		if m.focus != focusTaskInput {
			m.taskInputFocusBeforeOpen = m.focus
			m.taskInputFocusCaptured = true
			m.focusManager.Push(focusTargetInputBox)
			m.setFocus(focusTaskInput)
			return m, cmds
		}
	}
	if m.focus == focusTaskInput && !globalKeyWhileInput(key) {
		if m.taskInput != nil {
			if cmd := m.taskInput.Update(msg); cmd != nil {
				cmds = append(cmds, cmd)
			}
			if !m.taskInput.Focused() {
				m.focusManager.Pop()
				if m.taskInputFocusCaptured {
					m.taskInputFocusCaptured = false
					m.setFocus(m.taskInputFocusBeforeOpen)
				} else {
					m.setFocus(m.defaultPageFocus())
				}
			}
		}
		return m, cmds
	}
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
	if m.confirmQuit {
		switch key {
		case "y", "Y", "enter":
			force := m.confirmQuitDocker
			tasks := append([]TaskProject(nil), m.confirmQuitTasks...)
			m.confirmQuit = false
			m.confirmQuitDocker = false
			m.confirmQuitTasks = nil
			m.message = "正在清理并退出..."
			return m, append(cmds, m.quitCleanupCmd(force, tasks))
		case "n", "N", "esc":
			m.confirmQuit = false
			m.confirmQuitDocker = false
			m.confirmQuitTasks = nil
			m.message = "已取消退出"
			return m, cmds
		default:
			return m, cmds
		}
	}
	if m.confirmStartupDockerCleanup {
		switch key {
		case "y", "Y", "enter":
			m.confirmStartupDockerCleanup = false
			m.startupDockerCleanupCount = 0
			m.message = "正在清理遗留 Docker 资源..."
			return m, append(cmds, startupDockerCleanupCmd(m.cfg))
		case "n", "N", "esc", "q":
			m.confirmStartupDockerCleanup = false
			m.startupDockerCleanupCount = 0
			m.message = "已跳过遗留 Docker 清理"
			return m, cmds
		default:
			return m, cmds
		}
	}
	if m.verdictPrompt.taskID != "" {
		return m.handleVerdictPromptKey(key, cmds)
	}

	switch key {
	case "ctrl+c", "ctrl+q":
		m.message = "正在检查 Docker 运行状态..."
		return m, append(cmds, m.prepareQuitCmd())
	case "q":
		if m.focus == focusSearch {
			break
		}
		m.message = "正在检查 Docker 运行状态..."
		return m, append(cmds, m.prepareQuitCmd())
	case "ctrl+o":
		m.setTab(panelOverview)
		m.setFocus(focusOverviewTable)
		return m, append(cmds, m.reloadOverview())
	case "ctrl+e":
		if m.tab == panelTaskBoard {
			if task, ok := m.taskBoard.SelectedTask(); ok && task.TaskState == model.TaskWaitingManual {
				m.openVerdictPrompt(task)
				return m, cmds
			}
			m.message = "请选择待处理题目"
			return m, cmds
		}
	case "ctrl+w":
		if m.tab == panelTaskBoard {
			if task, ok := m.taskBoard.SelectedTask(); ok && canRetryGitSyncTask(task) {
				m.message = "正在重试 Git 同步 " + task.ID
				return m, append(cmds, m.taskActionCmd("retry-git", task.ID))
			}
			m.message = "请选择 Git 同步失败的题目"
			return m, cmds
		}
	case "ctrl+s":
		if m.tab == panelTaskBoard {
			if task, ok := m.taskBoard.SelectedTask(); ok && task.TaskState == model.TaskWaitingManual {
				if task.DockerRunning {
					m.message = "待处理服务已启动 " + task.ID
					return m, cmds
				}
				m.message = "正在启动待处理服务 " + task.ID
				return m, append(cmds, m.taskActionCmd("start-docker", task.ID))
			}
			m.message = "请选择待处理题目"
			return m, cmds
		}
	case "ctrl+x":
		m.openCancelConfirm()
		return m, cmds
	case "ctrl+r":
		if m.focus == focusTaskInput && m.openRunConfigForTaskInput() {
			return m, cmds
		}
		if m.tab == panelTaskBoard {
			if task, ok := m.taskBoard.SelectedTask(); ok && canOpenInspectionRunConfig(task.TaskState) {
				m.openRunConfigForTask(task.ID, runConfigActionInspection)
				return m, cmds
			}
			m.message = "请选择可重跑质检任务"
			return m, cmds
		}
		if m.tab == panelOverview {
			if item, ok := m.overview.SelectedItem(); ok && item.HasTask {
				if canOpenInspectionRunConfig(item.TaskState) {
					m.openRunConfigForTask(item.TaskID, runConfigActionInspection)
					return m, cmds
				}
				m.message = "请选择可重跑质检任务"
				return m, cmds
			}
		}
		m.openRunConfigForSelected(runConfigActionPipeline)
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
	case focusTaskBoard:
		switch key {
		case "enter":
			if task, ok := m.taskBoard.SelectedTask(); ok {
				m.setTab(panelExecution)
				m.openExecutionForTask(task.ID)
				m.setFocus(focusStageList)
				cmds = append(cmds, m.reloadDetail())
			}
		case "left", "right", "up", "down":
			if m.router != nil && m.router.Active() == pageTaskBoard {
				if page := m.router.ActivePage(); page != nil {
					if cmd := page.HandleKey(msg); cmd != nil {
						cmds = append(cmds, cmd)
					}
					break
				}
			}
			if cmd := m.taskBoard.HandleKey(msg); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
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
			_, cmd = m.overview.Update(msg)
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
			_, cmd = m.overview.Update(msg)
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
			m.setTab(panelOverview)
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

func canRetryGitSyncTask(task TaskProject) bool {
	if strings.TrimSpace(task.SyncError) == "" {
		return false
	}
	return task.TaskState == model.TaskInspecting || task.TaskState == model.TaskCompleted
}

func canOpenInspectionRunConfig(state string) bool {
	return state != model.TaskInspecting && state != model.TaskWaitingManual
}

func (m *app) switchPanel(delta int) {
	if m.tab == panelTaskBoard {
		m.setTab(panelOverview)
		m.setFocus(focusOverviewTable)
		return
	}
	if m.tab == panelOverview {
		m.setTab(panelTaskBoard)
		m.setFocus(focusTaskBoard)
		return
	}
	if delta < 0 {
		m.setTab(panelOverview)
		m.setFocus(focusOverviewTable)
		return
	}
	m.setTab(panelTaskBoard)
	m.setFocus(focusTaskBoard)
}

func (m *app) setTab(tab int) {
	m.tab = tab
	if m.router == nil {
		return
	}
	switch tab {
	case panelTaskBoard:
		m.router.SwitchTo(pageTaskBoard)
	case panelOverview:
		m.router.SwitchTo(pageOverview)
	case panelExecution:
		m.router.SwitchTo(pageExecution)
	}
}

func globalKeyWhileInput(key string) bool {
	if isSettingsShortcutKey(key) {
		return true
	}
	switch key {
	case "ctrl+c", "ctrl+q", "ctrl+o", "ctrl+e", "ctrl+w", "ctrl+x", "ctrl+r", "ctrl+s", "q":
		return true
	default:
		return false
	}
}

const settingsShortcutKey = "ctrl+_"

func isSettingsShortcutKey(key string) bool {
	return key == settingsShortcutKey || key == "ctrl+/"
}

func (m *app) closeSettingsOverlay() {
	m.dockerMirror.saveInputToFocus()
	m.settingsOpen = false
	if m.router != nil {
		_ = m.router.PopOverlay()
	}
	m.focusManager.Pop()
	if m.settingsFocusCaptured {
		m.settingsFocusCaptured = false
		m.setFocus(m.settingsFocusBeforeOpen)
		return
	}
	m.setFocus(m.defaultPageFocus())
}

func (m app) defaultPageFocus() focusArea {
	switch m.tab {
	case panelTaskBoard:
		return focusTaskBoard
	case panelExecution:
		return focusStageList
	case panelOverview:
		if m.selectedTaskID() == "" {
			return focusSearch
		}
		return focusOverviewTable
	default:
		return focusTaskBoard
	}
}

func (m *app) enterExecution() {
	m.openExecutionForTask(m.selectedTaskID())
	m.setTab(panelExecution)
	m.detailFollowTail = true
	if m.qaMode == "recheck" && len(m.detailVM.RefRuns) > 0 {
		m.setFocus(focusRefRunList)
		return
	}
	m.setFocus(focusStageList)
}

func (m *app) openExecutionForTask(taskID string) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return
	}
	m.executionState.taskID = taskID
	if m.overview != nil {
		m.overview.selectedID = taskID
	}
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
		m.setTab(panelOverview)
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

func (m *app) openVerdictPrompt(task TaskProject) {
	m.verdictPrompt = verdictPrompt{taskID: task.ID, index: verdictIndex(task.ManualVerdict)}
	if m.verdictPrompt.index < 0 {
		m.verdictPrompt.index = 0
	}
	m.message = "请选择判定 " + task.ID
}

func (m app) handleVerdictPromptKey(key string, cmds []tea.Cmd) (app, []tea.Cmd) {
	switch key {
	case "left", "up":
		m.verdictPrompt.index = clamp(m.verdictPrompt.index-1, 0, len(manualVerdictOptions())-1)
	case "right", "down", "tab":
		m.verdictPrompt.index = clamp(m.verdictPrompt.index+1, 0, len(manualVerdictOptions())-1)
	case "1":
		m.verdictPrompt.index = 0
	case "2":
		m.verdictPrompt.index = 1
	case "3":
		m.verdictPrompt.index = 2
	case "enter", " ":
		taskID := m.verdictPrompt.taskID
		verdict := manualVerdictOptions()[m.verdictPrompt.index].value
		m.verdictPrompt = verdictPrompt{}
		m.message = "正在结束质检 " + taskID + "，判定: " + localizeManualVerdict(verdict)
		return m, append(cmds, m.taskActionCmdWithVerdict("complete", taskID, verdict))
	case "esc", "q":
		m.verdictPrompt = verdictPrompt{}
		m.message = "已取消结束质检"
	default:
		return m, cmds
	}
	return m, cmds
}

type manualVerdictOption struct {
	value string
	label string
}

func manualVerdictOptions() []manualVerdictOption {
	return []manualVerdictOption{
		{value: model.ManualPass, label: "通过"},
		{value: model.ManualRework, label: "返工"},
		{value: model.ManualFail, label: "不通过"},
	}
}

func verdictIndex(verdict string) int {
	for index, option := range manualVerdictOptions() {
		if option.value == strings.TrimSpace(verdict) {
			return index
		}
	}
	return -1
}

func (m *app) openRunConfig() {
	m.openRunConfigForSelected(runConfigActionPipeline)
}

func (m *app) openRunConfigForSelected(action runConfigAction) {
	m.openRunConfigForTask(m.selectedTaskID(), action)
}

func (m *app) openRunConfigForTask(taskID string, action runConfigAction) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
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
	m.runConfig = newRunConfig(taskID, m.qaMode, m.selectedRefRun(), m.rerunStageKey(), m.cfg.Docker.KeepRuntime, m.detailVM.DocsSummary.Count, action, m.cfg.Pipeline.DefaultStages)
}

func (m *app) openRunConfigForTaskInput() bool {
	if m.taskInput == nil {
		return false
	}
	taskID, err := ValidateTaskID(m.taskInput.Value())
	if err != nil {
		m.taskInput.SetError(err.Error())
		return true
	}
	m.overview.selectedID = taskID
	m.taskInput.Clear()
	m.taskInput.Blur()
	m.focusManager.Pop()
	m.taskInputFocusCaptured = false
	m.setFocus(focusTaskBoard)
	m.openRunConfigForTask(taskID, runConfigActionInspection)
	return true
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
	if m.confirmStartupDockerCleanup {
		return "y/Enter 清理遗留 Docker 资源  n/Esc 跳过"
	}
	if m.verdictPrompt.taskID != "" {
		return "←→ 选择判定  1/2/3 快选  Enter 结束质检  Esc 取消"
	}
	if m.settingsOpen {
		if m.settings.selected == settingsItemDocker && m.dockerMirror.confirm != "" {
			return "y/Enter 确认  n/Esc 取消  Ctrl+/ 关闭设置"
		}
		return "Esc/Q/Ctrl+/ 关闭设置  Tab/Shift+Tab 字段  ↑↓ 字段  Space 开关  Enter 执行"
	}
	switch m.focus {
	case focusTaskBoard:
		return "/ 输入题目  Ctrl+S 启动服务  Ctrl+E 判定完成  Ctrl+R 重检  Ctrl+W 重试Git  Ctrl+O 总览  Ctrl+/ 设置  Q 退出"
	case focusTaskInput:
		return "Enter 开始质检  Esc 清空  ←→ 光标  Ctrl+E/Ctrl+R/Ctrl+O 全局  Ctrl+/ 设置  Q 退出"
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
