package tui

import (
	"fmt"
	"os"

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
	if m.store != nil {
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
	if m.runConfig != nil {
		return true, m.updateRunConfigKey(msg)
	}
	if m.resumeOverlay != nil {
		return true, m.updateResumeKey(msg)
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
	if m.confirm != nil {
		return true, m.updateConfirmKey(msg)
	}
	if m.gateEditingNote {
		return false, nil
	}
	if m.searching {
		return true, m.updateSearchKey(msg)
	}
	if m.view == viewStart {
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
		m.helpVisible = true
		if m.router != nil {
			m.router.PushOverlay(&helpOverlay{view: m.view})
		}
		m.focusMgr.Push(focusOverlay)
		return true, nil
	case "/":
		if m.view == viewOverview || m.view == viewGate || m.view == viewLogs || m.view == viewNodeDetail {
			m.beginSearch()
			return true, nil
		}
	case "1":
		if m.store != nil {
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
	case "ctrl+x", "x":
		if m.view == viewStart {
			return false, nil
		}
		if m.readOnly {
			m.err = fmt.Errorf("workspace snapshot is read-only while another Factory process owns the run")
			return true, nil
		}
		if m.done {
			return true, m.showToast("运行已经结束", toastInfo)
		}
		m.openConfirm(newConfirmDialog(confirmCancelRun, "确认取消运行", "取消后当前工作流将停止，是否继续？"))
		return true, func() tea.Msg { return confirmOpenedMsg{} }
	case "ctrl+q", "ctrl+c":
		return true, m.requestQuit()
	case "q":
		if m.view != viewStart {
			return true, m.requestQuit()
		}
	case "esc":
		if m.view == viewDone && m.store != nil {
			return true, m.returnToHub()
		}
		if m.view != viewOverview && m.view != viewStart && m.view != viewHub {
			m.setView(viewOverview)
			return true, nil
		}
		if m.view == viewOverview && m.store != nil {
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
	if m.view == viewHub && m.runner == nil {
		for _, item := range m.hubItems {
			if item.Run.IsActive {
				m.openConfirm(newConfirmDialog(confirmQuit, "确认退出", "仍有运行中的工作区。退出不会停止其他 Factory 进程，是否继续？"))
				return func() tea.Msg { return confirmOpenedMsg{} }
			}
		}
		m.cancelRun()
		return tea.Quit
	}
	if m.done || m.readOnly || m.view == viewStart {
		m.cancelRun()
		return tea.Quit
	}
	m.openConfirm(newConfirmDialog(confirmQuit, "确认退出", "工作流仍在运行。退出将取消当前运行，是否继续？"))
	return func() tea.Msg { return confirmOpenedMsg{} }
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
	case confirmCancelRun:
		m.cancelRun()
		m.notice = "已请求取消当前运行"
		return m.showToast(m.notice, toastWarning)
	case confirmQuit:
		m.cancelRun()
		return tea.Quit
	case confirmEditArtifact:
		return openEditorCmd(d.Path)
	case confirmDeleteWorkspace:
		path := d.Path
		dataStore := m.store
		return func() tea.Msg {
			if err := os.RemoveAll(path); err != nil {
				return workspaceDeletedMsg{path: path, err: err}
			}
			if dataStore != nil {
				if err := dataStore.DeleteRunByWorkspace(path); err != nil {
					return workspaceDeletedMsg{path: path, err: err}
				}
				if err := dataStore.CleanOrphanTasks(); err != nil {
					return workspaceDeletedMsg{path: path, err: err}
				}
			}
			return workspaceDeletedMsg{path: path}
		}
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

func (m *model) confirmSelectedNodeEdit() tea.Cmd {
	if m.readOnly {
		m.err = fmt.Errorf("工作区快照为只读，不能编辑工件")
		return nil
	}
	artifact, ok := m.selectedNodeArtifact()
	if !ok {
		return m.showToast("当前节点没有可编辑工件", toastWarning)
	}
	path, err := m.safeEditableArtifactPath(artifact.Path)
	if err != nil {
		m.err = err
		return nil
	}
	dialog := newConfirmDialog(confirmEditArtifact, "确认编辑工件", "将使用外部编辑器打开：\n"+path)
	dialog.Path = path
	m.openConfirm(dialog)
	return func() tea.Msg { return confirmOpenedMsg{} }
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
	if m.runConfig != nil {
		return subtleStyle.Render("[Tab/↑↓ 切换] [Space 开关] [Enter 开始重跑] [Esc 取消]")
	}
	if m.resumeOverlay != nil {
		return subtleStyle.Render("[R 恢复运行] [N 新建运行] [V 只读查看] [Enter 确认] [Esc 取消]")
	}
	var text string
	switch m.view {
	case viewHub:
		text = "[↑↓ 选择] [Enter 打开] [Ctrl+N 新建] [Ctrl+R 重跑] [Del 删除] [s/S 排序] [/ 搜索] [q 退出] [? 帮助]"
	case viewStart:
		if m.startStep == startStepBasic {
			text = "[Tab/↓ 下一字段] [Shift+Tab/↑ 上一字段] [Space 切换模式] [Ctrl+Space 路径补全] [Enter 下一步] [Ctrl+Q 退出]"
		} else {
			text = "[F1-F4 分组] [Ctrl+←→ 分组] [Tab/↓ 下项] [Shift+Tab/↑ 上项] [Space 切换] [Ctrl+Space 补全] [Enter 启动] [Esc 返回] [Ctrl+Q 退出]"
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
			if gate != nil && (gate.GateID == nodes.FinalReview || gate.GateID == nodes.ResultReview) {
				text += " [Ctrl+V/v 修订/刷新]"
			}
			text += " [Ctrl+N 备注] [e 编辑工件] [Tab 下一工件] [Esc 返回] [? 帮助]"
		}
	case viewLogs:
		text = "[↑↓/j k 滚动] [PgUp/PgDn 翻页] [Home/End 首尾] [t 跟踪] [Tab/Shift+Tab 切换文件] [Ctrl+O 总览] [? 帮助]"
	case viewNodeDetail:
		if m.readOnly {
			text = "[↑↓/j k 选择] [Tab/Shift+Tab 切换工件] [Ctrl+L 日志] [Ctrl+O 总览] [/ 过滤] [? 帮助]"
		} else {
			text = "[↑↓/j k 选择] [Tab/Shift+Tab 切换工件] [e 编辑] [Ctrl+L 日志] [Ctrl+O 总览] [/ 过滤] [? 帮助]"
		}
	case viewDone:
		text = "[Esc 返回工作区] [Ctrl+R 重跑] [Ctrl+N 新建] [1 工作区] [2 总览] [4 详情] [5 日志] [q 退出] [? 帮助]"
	default:
		if m.readOnly {
			text = "[↑↓选择] [Enter详情] [PgUp/PgDn翻页] [Tab下一页] [Ctrl+G审查] [Ctrl+L日志] [Ctrl+E完成] [q退出] [/过滤] [?帮助]"
		} else {
			text = "[↑↓选择] [Enter详情] [PgUp/PgDn翻页] [Tab下一页] [Ctrl+G审查] [Ctrl+L日志] [Ctrl+E完成] [Ctrl+X取消运行] [q退出] [/过滤] [?帮助]"
		}
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

func (m *model) handleMouse(msg tea.MouseMsg) tea.Cmd {
	event := tea.MouseEvent(msg)
	switch event.Button {
	case tea.MouseButtonWheelUp:
		switch m.view {
		case viewHub:
			m.hubTable.MoveUp(3)
		case viewLogs:
			m.scrollLogFile(-3)
		case viewGate:
			if m.syncGateViewport(m.activeGate) {
				m.gateViewport.LineUp(3)
				m.gateScroll = m.gateViewport.YOffset
			}
		case viewNodeDetail:
			if m.syncDetailViewport() {
				m.detailViewport.LineUp(3)
				m.detailScroll = m.detailViewport.YOffset
			}
		case viewOverview:
			m.syncOverviewTable()
			m.overviewTable.MoveUp(3)
			m.syncSelectedOverviewRow()
		case viewStart:
			m.selectPrevStartField()
			m.focusStartInput(m.startField)
		}
		return nil
	case tea.MouseButtonWheelDown:
		switch m.view {
		case viewHub:
			m.hubTable.MoveDown(3)
		case viewLogs:
			m.scrollLogFile(3)
		case viewGate:
			if m.syncGateViewport(m.activeGate) {
				m.gateViewport.LineDown(3)
				m.gateScroll = m.gateViewport.YOffset
			}
		case viewNodeDetail:
			if m.syncDetailViewport() {
				m.detailViewport.LineDown(3)
				m.detailScroll = m.detailViewport.YOffset
			}
		case viewOverview:
			m.syncOverviewTable()
			m.overviewTable.MoveDown(3)
			m.syncSelectedOverviewRow()
		case viewStart:
			m.selectNextStartField()
			m.focusStartInput(m.startField)
		}
		return nil
	case tea.MouseButtonLeft:
		if event.Action != tea.MouseActionPress {
			return nil
		}
	default:
		return nil
	}
	switch m.view {
	case viewStart:
		fields := m.activeStartFields()
		if m.startStep == startStepBasic {
			if event.Y == 9 || event.Y == 10 {
				m.startMode = startExistingTask
				if event.Y == 10 {
					m.startMode = startGenerateTask
				}
				m.opts.Generate = m.startMode == startGenerateTask
				m.startField = startFieldMode
				return nil
			}
			if event.Y >= 11 && len(fields) > 1 {
				idx := clampInt(1+event.Y-11, 1, len(fields)-1)
				m.startField = fields[idx]
				m.focusStartInput(m.startField)
			}
			return nil
		}
		layout := layoutFor(m.width, m.height)
		if event.Y >= 9 && event.Y < 9+len(advancedGroups()) && (layout.Mode == layoutStacked || layout.Mode == layoutMinimal || event.X < layout.SidebarWidth) {
			m.selectAdvancedGroupByNumber(event.Y - 8)
			return nil
		}
		fieldRow := 14
		if layout.Mode == layoutWide || layout.Mode == layoutMedium {
			fieldRow = 9
		}
		if event.Y >= fieldRow && len(fields) > 0 {
			idx := clampInt(event.Y-fieldRow, 0, len(fields)-1)
			m.startField = fields[idx]
			m.focusStartInput(m.startField)
		}
	case viewOverview, viewNodeDetail:
		var ids []string
		for _, id := range nodeOrder() {
			if ev, ok := m.nodes[id]; ok && matchesFilter(m.filter, id, localizeNode(id), ev.Message) {
				ids = append(ids, id)
			} else if !ok && m.view == viewOverview && matchesFilter(m.filter, id, localizeNode(id)) {
				ids = append(ids, id)
			}
		}
		if len(ids) > 0 {
			idx := clampInt(event.Y-5, 0, len(ids)-1)
			m.selectedNode = ids[idx]
			m.selectedArtifact = 0
		}
	case viewGate:
		if m.readOnly {
			return nil
		}
		if event.Y >= m.height-7 {
			key := "a"
			if event.X > m.width/3 {
				key = "r"
			}
			if event.X > 2*m.width/3 {
				key = "v"
			}
			updated, cmd := m.updateGateKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
			*m = updated.(model)
			return cmd
		}
	}
	return nil
}
