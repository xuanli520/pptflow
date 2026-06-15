package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/taskdocs"
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
		if m.tab == panelOverview && m.focus == focusOverviewTable {
			break
		}
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
		if isDiagnosticsKey(key) {
			m.message = "请先关闭运行配置再诊断"
			return m, cmds
		}
		return m.handleRunConfigKey(msg)
	}
	if m.diagnostics.active {
		return m.handleTaskDiagnosticsKey(key, cmds)
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
	if m.taskTypePrompt.taskID != "" {
		return m.handleTaskTypePromptKey(key, cmds)
	}
	if m.verdictPrompt.taskID != "" {
		return m.handleVerdictPromptKey(key, cmds)
	}

	switch key {
	case "ctrl+c", "ctrl+q":
		m.message = "正在检查 Docker 运行状态..."
		return m, append(cmds, m.prepareQuitCmd())
	case "q":
		if m.focus == focusSearch || (m.tab == panelOverview && m.focus == focusOverviewTable) {
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
				return m, append(cmds, m.retryGitSyncCmd(task.ID))
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
				return m, append(cmds, m.startDockerCmd(task.ID))
			}
			m.message = "请选择待处理题目"
			return m, cmds
		}
	case "ctrl+x":
		if cmd := m.openCancelConfirm(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		return m, cmds
	case "ctrl+d":
		return m.openTaskDiagnosticsForCurrentContext(cmds)
	case "ctrl+r":
		return m.openInspectionRunConfigForCurrentContext(cmds)
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

type inspectionAdmission struct {
	Allowed bool
	Message string
}

func (m app) evaluateInspectionAdmission(task TaskProject) inspectionAdmission {
	taskID := strings.TrimSpace(task.ID)
	if taskID == "" {
		return inspectionAdmission{Message: "请选择可重跑质检任务"}
	}
	if job, ok := m.activeJobForTask(taskID); ok {
		return inspectionAdmission{Message: "该任务已有排队或运行中的作业: " + job.JobID}
	}
	if task.TaskState == model.TaskWaitingManual {
		return inspectionAdmission{Message: "请先完成待处理判定，再重跑质检"}
	}
	if task.TaskState == model.TaskInspecting && strings.TrimSpace(task.SyncError) != "" {
		return inspectionAdmission{Message: "Git 同步失败，请按 Ctrl+W 重试；需要修复状态请按 Ctrl+D 诊断"}
	}
	if strings.TrimSpace(task.CurrentRunID) != "" || task.RunStatus == model.RunRunning || task.CurrentStatus == model.StageRunning {
		return inspectionAdmission{Message: "该任务已有运行记录未结束，请按 Ctrl+X 终止或 Ctrl+D 诊断"}
	}
	if task.TaskState == model.TaskInspecting {
		return inspectionAdmission{Message: "题目检查状态异常，请按 Ctrl+D 打开诊断后重跑"}
	}
	return inspectionAdmission{Allowed: true}
}

func taskProjectFromOverviewItem(item overviewItem) TaskProject {
	return TaskProject{
		ID:            item.TaskID,
		TaskState:     item.TaskState,
		CurrentRunID:  item.CurrentRunID,
		LastRunID:     item.LastRunID,
		RunStatus:     item.RunStatus,
		ManualVerdict: item.ManualVerdict,
		FailedStage:   item.FailedStage,
	}
}

func (m app) openInspectionRunConfigForCurrentContext(cmds []tea.Cmd) (app, []tea.Cmd) {
	if m.focus == focusTaskInput {
		m.openInspectionFlowForTaskInput()
		return m, cmds
	}
	switch m.tab {
	case panelTaskBoard:
		task, ok := m.taskBoard.SelectedTask()
		if !ok {
			m.message = "请选择可重跑质检任务"
			return m, cmds
		}
		if admission := m.evaluateInspectionAdmission(task); !admission.Allowed {
			m.message = admission.Message
			return m, cmds
		}
		m.openRunConfigForTask(task.ID)
		return m, cmds
	case panelOverview:
		item, ok := m.overview.SelectedItem()
		if !ok {
			m.message = "请选择一个已创建任务"
			return m, cmds
		}
		if !item.HasTask {
			m.message = "该项目尚未创建任务，请从任务输入框开始质检"
			return m, cmds
		}
		if admission := m.evaluateInspectionAdmission(taskProjectFromOverviewItem(item)); !admission.Allowed {
			m.message = admission.Message
			return m, cmds
		}
		m.openRunConfigForTask(item.TaskID)
		return m, cmds
	case panelExecution:
		taskID := m.selectedTaskID()
		if strings.TrimSpace(taskID) == "" {
			m.message = "当前执行详情没有选中的任务"
			return m, cmds
		}
		project, err := m.lookupTaskProject(taskID)
		if err != nil {
			m.message = err.Error()
			return m, cmds
		}
		if project == nil {
			m.message = "当前执行详情没有已创建任务"
			return m, cmds
		}
		if admission := m.evaluateInspectionAdmission(*project); !admission.Allowed {
			m.message = admission.Message
			return m, cmds
		}
		m.openRunConfigForTask(taskID)
		return m, cmds
	default:
		m.message = "当前页面不支持重跑"
		return m, cmds
	}
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
	case "ctrl+c", "ctrl+q", "ctrl+o", "ctrl+e", "ctrl+w", "ctrl+x", "ctrl+r", "ctrl+d", "ctrl+s", "q":
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
	case m.taskTypePrompt.taskID != "":
		m.taskTypePrompt = taskTypePrompt{}
		m.message = "已取消新题质检"
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
		return m, append(cmds, m.completeManualCmd(taskID, verdict))
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
	m.openRunConfigForSelected()
}

func (m *app) openRunConfigForSelected() {
	m.openRunConfigForTask(m.selectedTaskID())
}

func (m *app) openRunConfigForTask(taskID string) {
	m.openRunConfigForTaskWithProjectType(taskID, "")
}

func (m *app) openRunConfigForTaskWithProjectType(taskID string, projectType string) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return
	}
	if project, err := m.lookupTaskProject(taskID); err == nil && project != nil {
		if admission := m.evaluateInspectionAdmission(*project); !admission.Allowed {
			m.message = admission.Message
			return
		}
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
	availableDocs := taskdocs.AvailableCount(m.cfg.ScanPath, taskID)
	m.runConfig = newRunConfig(taskID, m.qaMode, m.selectedRefRun(), m.rerunStageKey(), m.cfg.Docker.KeepRuntime, availableDocs, m.cfg.Pipeline.DefaultStages)
	if project, err := m.lookupTaskProject(taskID); err == nil && project != nil {
		m.runConfig.existingTask = true
		m.runConfig.currentType = taskTypeFromGitURL(m.cfg.Git, project.GitURL)
	}
	m.runConfig.projectType = config.NormalizeProjectType(projectType)
}

func (m *app) openInspectionFlowForTaskInput() bool {
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
	existingTask, err := m.taskInputExistingTask(taskID)
	if err != nil {
		m.message = err.Error()
		m.taskInput.SetValue(taskID)
		m.taskInput.SetError(err.Error())
		m.taskInput.Focus()
		m.setFocus(focusTaskInput)
		return true
	}
	if existingTask {
		m.openRunConfigForTask(taskID)
		return true
	}
	m.openTaskTypePrompt(taskID)
	return true
}

func (m *app) openCancelConfirm() tea.Cmd {
	taskID := m.selectedTaskID()
	if taskID == "" {
		m.message = "没有选中的任务"
		return nil
	}
	job, ok := m.activeJobForTask(taskID)
	if !ok {
		if m.selectedTaskHasPersistedRunningRun(taskID) && m.lifecycle != nil {
			m.message = "正在检查失联运行 " + taskID
			return m.recoverOrphanRunCmd(taskID)
		}
		m.message = "该任务没有排队或运行中的作业"
		return nil
	}
	m.confirmCancelTaskID = taskID
	m.confirmCancelJobID = job.JobID
	return nil
}

func (m app) selectedTaskHasPersistedRunningRun(taskID string) bool {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return false
	}
	if m.detailVM.TaskID == taskID && m.detailVM.Run.Status == model.RunRunning {
		return true
	}
	if item, ok := m.overview.ItemByTaskID(taskID); ok && item.RunStatus == model.RunRunning {
		return true
	}
	if m.taskBoard != nil {
		if task, ok := m.taskBoard.SelectedTask(); ok && task.ID == taskID {
			return task.RunStatus == model.RunRunning || task.CurrentStatus == model.StageRunning
		}
	}
	return false
}

func footerFor(m app) string {
	if m.runConfig.active {
		return "Tab 切换  Space 选择  Enter 确认  Esc 取消"
	}
	if m.diagnostics.active {
		if diagnosticsCanRepair(m.diagnostics.snapshot) {
			return "Enter 修复  Esc/q 关闭诊断"
		}
		return "Esc/q 关闭诊断"
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
	if m.taskTypePrompt.taskID != "" {
		return "←→ 选择题型  1/2/3 快选  Enter 确认  Esc 取消"
	}
	if m.settingsOpen {
		if m.settings.selected == settingsItemDocker && m.dockerMirror.confirm != "" {
			return "y/Enter 确认  n/Esc 取消  Ctrl+/ 关闭设置"
		}
		return "Esc/Q/Ctrl+/ 关闭设置  Tab/Shift+Tab 字段  ↑↓ 字段  Space 开关  Enter 执行"
	}
	switch m.focus {
	case focusTaskBoard:
		return "/ 输入题目  Ctrl+S 启动服务  Ctrl+E 判定完成  Ctrl+R 重检  Ctrl+W 重试Git  Ctrl+D 诊断  Ctrl+O 总览  Ctrl+/ 设置  Q 退出"
	case focusTaskInput:
		return "Enter 开始质检  Esc 清空  ←→ 光标  Ctrl+E/Ctrl+R/Ctrl+D/Ctrl+O 全局  Ctrl+/ 设置  Q 退出"
	case focusSearch:
		return "Ctrl+C 退出  Tab 执行详情  ↓ 表格  Ctrl+R 重跑  Ctrl+D 诊断  Ctrl+X 终止"
	case focusOverviewTable:
		return "↑↓选择 Enter详情 /搜索 s排序 S反向 PgUp/PgDn翻页 z条数 Ctrl+R重跑 Ctrl+D诊断 Ctrl+X终止 m模式"
	case focusStageList:
		return "↑↓ 阶段  Enter 详情  Ctrl+R 重跑  Ctrl+D 诊断  Ctrl+X 终止  m 模式"
	case focusRefRunList:
		return "↑↓ 参考运行  Enter 选择  Esc 返回阶段  Ctrl+R 重跑  Ctrl+D 诊断  Ctrl+X 终止"
	case focusDetailViewport:
		return "↑↓ 滚动  PgUp/PgDn 翻页  Esc 返回阶段  Ctrl+D 诊断  Ctrl+X 终止"
	default:
		return "Ctrl+C 退出"
	}
}
