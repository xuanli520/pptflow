package tui

import (
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// mouseTarget binds mouse behavior to text that is actually present in the
// rendered frame. This keeps hit testing aligned with responsive layouts,
// filtered rows, optional notices, and overlays without duplicating Y offsets.
type mouseTarget struct {
	x, y, width int
	activate    func() tea.Cmd
}

func (t mouseTarget) contains(x, y int) bool {
	return y == t.y && x >= t.x && x < t.x+maxInt(1, t.width)
}

type renderedFrame struct{ lines []string }

func newRenderedFrame(view string) renderedFrame {
	return renderedFrame{lines: strings.Split(ansi.Strip(view), "\n")}
}

func (f renderedFrame) targets(marker string, activate func() tea.Cmd) []mouseTarget {
	if marker == "" || activate == nil {
		return nil
	}
	var targets []mouseTarget
	for y, line := range f.lines {
		searchFrom := 0
		for searchFrom <= len(line) {
			index := strings.Index(line[searchFrom:], marker)
			if index < 0 {
				break
			}
			index += searchFrom
			targets = append(targets, mouseTarget{
				x:        ansi.StringWidth(line[:index]),
				y:        y,
				width:    ansi.StringWidth(marker),
				activate: activate,
			})
			searchFrom = index + len(marker)
		}
	}
	return targets
}

func (m *model) handleMouse(msg tea.MouseMsg) tea.Cmd {
	event := tea.MouseEvent(msg)
	if event.Button == tea.MouseButtonWheelUp || event.Button == tea.MouseButtonWheelDown {
		if m.taskHubDetail != nil {
			delta := 3
			if event.Button == tea.MouseButtonWheelUp {
				delta = -delta
			}
			m.taskHubDetail.scroll(delta, m.taskHubDetailHeight())
			return nil
		}
		if m.mouseOverlayVisible() {
			return nil
		}
		delta := 3
		if event.Button == tea.MouseButtonWheelUp {
			delta = -delta
		}
		m.handleMouseWheel(delta)
		return nil
	}
	if event.Button != tea.MouseButtonLeft || event.Action != tea.MouseActionPress {
		return nil
	}
	for _, target := range m.mouseTargets() {
		if target.contains(event.X, event.Y) {
			return target.activate()
		}
	}
	return nil
}

func (m *model) mouseOverlayVisible() bool {
	return m.taskHubDetail != nil || m.taskHubHelpVisible || m.helpVisible || m.confirm != nil || m.taskHubMutation != nil || m.runControl != nil
}

func (m *model) handleMouseWheel(delta int) {
	switch m.view {
	case viewHub:
		if m.lifecycle != nil {
			m.moveTaskHubSelection(delta)
		} else if delta < 0 {
			m.hubTable.MoveUp(-delta)
		} else {
			m.hubTable.MoveDown(delta)
		}
	case viewLogs:
		m.scrollLogFile(delta)
	case viewGate:
		if m.syncGateViewport(m.activeGate) {
			if delta < 0 {
				m.gateViewport.LineUp(-delta)
			} else {
				m.gateViewport.LineDown(delta)
			}
			m.gateScroll = m.gateViewport.YOffset
		}
	case viewNodeDetail:
		if m.syncDetailViewport() {
			if delta < 0 {
				m.detailViewport.LineUp(-delta)
			} else {
				m.detailViewport.LineDown(delta)
			}
			m.detailScroll = m.detailViewport.YOffset
		}
	case viewOverview:
		m.syncOverviewTable()
		if delta < 0 {
			m.overviewTable.MoveUp(-delta)
		} else {
			m.overviewTable.MoveDown(delta)
		}
		m.syncSelectedOverviewRow()
	case viewStart:
		if delta < 0 {
			m.selectPrevStartField()
		} else {
			m.selectNextStartField()
		}
		m.focusStartInput(m.startField)
	}
}

func (m *model) mouseTargets() []mouseTarget {
	frame := newRenderedFrame(m.View())
	var targets []mouseTarget
	add := func(marker string, activate func() tea.Cmd) {
		targets = append(targets, frame.targets(marker, activate)...)
	}

	if m.taskHubMutation != nil {
		add("[Enter] 确认提交", func() tea.Cmd { return m.updateTaskHubMutationKey(mouseKey("enter")) })
		add("[Esc] 取消", func() tea.Cmd { return m.updateTaskHubMutationKey(mouseKey("esc")) })
		return targets
	}
	if m.runControl != nil {
		add("  返回并保持运行  ", func() tea.Cmd {
			m.runControl.selectReturn()
			return m.updateRunControlKey(mouseKey("enter"))
		})
		if m.runControl.lifecycleControlAvailable() {
			for _, choice := range []struct {
				marker string
				action TaskHubRunControlAction
			}{
				{"[P] 暂停运行", TaskHubRunControlPause},
				{"[K] 取消选中阶段", TaskHubRunControlCancelStage},
				{"[S] 终止本次运行", TaskHubRunControlTerminate},
			} {
				choice := choice
				add(choice.marker, func() tea.Cmd {
					m.runControl.selectAction(choice.action)
					return nil
				})
			}
			add("[Enter] 查看影响预览", func() tea.Cmd {
				return m.updateRunControlKey(mouseKey("enter"))
			})
		}
		return targets
	}
	if m.taskHubDetail != nil {
		for _, tab := range taskHubDetailTabs() {
			tab := tab
			add(tab.label(), func() tea.Cmd {
				m.taskHubDetail.Tab = tab
				m.taskHubDetail.Scroll = 0
				return nil
			})
		}
		add("[r] 刷新", func() tea.Cmd { return m.updateTaskHubDetailKey(runeMouseKey("r")) })
		add("[Esc] 返回", func() tea.Cmd { return m.updateTaskHubDetailKey(mouseKey("esc")) })
		return targets
	}
	if m.taskHubHelpVisible {
		add("[? / Esc / q] 关闭帮助", func() tea.Cmd { return m.dispatchMouseKey("esc") })
		return targets
	}
	if m.helpVisible {
		add("按 ?、Esc 或 q 关闭", func() tea.Cmd { return m.dispatchMouseKey("?") })
		return targets
	}
	if m.confirm != nil {
		add("  是  ", func() tea.Cmd {
			m.confirm.FocusedYes = true
			return m.updateConfirmKey(tea.KeyMsg{Type: tea.KeyEnter})
		})
		add("  否  ", func() tea.Cmd {
			m.confirm.FocusedYes = false
			return m.updateConfirmKey(tea.KeyMsg{Type: tea.KeyEnter})
		})
		return targets
	}
	switch m.view {
	case viewHub:
		if m.lifecycle != nil {
			for _, tab := range []struct {
				marker string
				tab    TaskHubTab
			}{
				{marker: "Tasks", tab: TaskHubTasksTab},
				{marker: "Runs", tab: TaskHubRunsTab},
				{marker: "Queue", tab: TaskHubQueueTab},
			} {
				tab := tab
				add(tab.marker, func() tea.Cmd {
					m.selectTaskHubTab(tab.tab)
					return nil
				})
			}
			switch m.taskHub.Query.Tab {
			case TaskHubTasksTab:
				for _, row := range m.taskHub.Rows {
					row := row
					add(shortTaskHubID(row.Task.TaskID), func() tea.Cmd {
						m.taskHub.SelectedTaskID = row.Task.TaskID
						if row.HasLatestRun {
							m.taskHub.SelectedRunID = row.LatestRun.RunID
						}
						m.focusMgr.SetCurrent(focusPage)
						return nil
					})
				}
			case TaskHubRunsTab:
				for _, run := range sortTaskHubRuns(m.taskHub.Snapshot.Runs) {
					run := run
					add(shortTaskHubID(run.RunID), func() tea.Cmd {
						m.taskHub.SelectedRunID = run.RunID
						m.taskHub.SelectedTaskID = run.TaskID
						m.focusMgr.SetCurrent(focusPage)
						return nil
					})
				}
			case TaskHubQueueTab:
				for _, run := range m.taskHub.Snapshot.Runs {
					if run.QueuePosition <= 0 {
						continue
					}
					run := run
					add("#"+strconv.Itoa(run.QueuePosition), func() tea.Cmd {
						m.taskHub.SelectedRunID = run.RunID
						m.taskHub.SelectedTaskID = run.TaskID
						m.focusMgr.SetCurrent(focusPage)
						return nil
					})
				}
			}
			return appendTaskHubMouseFooterTargets(m, frame, targets)
		}
		for index, row := range m.hubTable.Rows() {
			if len(row) == 0 {
				continue
			}
			index := index
			add(row[0], func() tea.Cmd {
				m.hubTable.SetCursor(index)
				m.focusMgr.SetCurrent(focusPage)
				return nil
			})
		}
	case viewStart:
		m.addStartMouseTargets(frame, &targets)
	case viewOverview:
		m.syncOverviewTable()
		for index, id := range m.overviewRowIDs {
			index, id := index, id
			add(localizeNode(id)+" ("+id+")", func() tea.Cmd {
				m.overviewTable.SetCursor(index)
				m.selectedNode = id
				m.selectedArtifact = 0
				m.focusMgr.SetCurrent(focusOverviewTable)
				return nil
			})
		}
	case viewNodeDetail:
		for _, id := range nodeOrder() {
			event, ok := m.nodes[id]
			if !ok || !matchesFilter(m.filter, id, localizeNode(id), event.Message, event.Status) {
				continue
			}
			id := id
			add(localizeNode(id)+" ("+id+")", func() tea.Cmd {
				m.selectedNode = id
				m.selectedArtifact = 0
				m.detailScroll = 0
				m.focusMgr.SetCurrent(focusDetailViewport)
				return nil
			})
		}
	}

	for marker, key := range map[string]string{
		"[Ctrl+O 总览]": "ctrl+o", "[Ctrl+G 审查]": "ctrl+g", "[Ctrl+D 详情]": "ctrl+d",
		"[Ctrl+L 日志]": "ctrl+l", "[Ctrl+E 完成]": "ctrl+e", "[Ctrl+A/a 批准]": "a",
		"[Ctrl+R/r 拒绝]": "r", "[v Codex指导返修]": "v", "[c Codex自动循环]": "c",
		"[u 人工编辑后重跑]": "u", "[Ctrl+V/v 刷新证据]": "v", "[Ctrl+N 备注/指导]": "ctrl+n",
		"[t 跟踪]": "t", "[1 工作区]": "1", "[2 总览]": "2",
		"[4 详情]": "4", "[5 日志]": "5", "[? 帮助]": "?", "Ctrl+B 入队": "ctrl+b",
		"[Enter 打开]": "enter", "[Del 删除]": "delete", "[s/S 排序]": "s", "[/ 搜索]": "/",
		"[q 退出]": "q", "[Enter 下一步]": "enter", "[Enter 启动]": "enter", "[Ctrl+Q 退出]": "ctrl+q",
		"[Esc 返回]": "esc", "[Tab 下一工件]": "tab", "[Tab/Shift+Tab 切换文件]": "tab", "[/ 过滤]": "/",
	} {
		key := key
		add(marker, func() tea.Cmd { return m.dispatchMouseKey(key) })
	}
	return targets
}

func appendTaskHubMouseFooterTargets(m *model, frame renderedFrame, targets []mouseTarget) []mouseTarget {
	add := func(marker string, activate func() tea.Cmd) {
		targets = append(targets, frame.targets(marker, activate)...)
	}
	add("[Enter/d 详情]", func() tea.Cmd { return m.dispatchMouseKey("enter") })
	add("[/ 搜索]", func() tea.Cmd { return m.dispatchMouseKey("/") })
	add("[? 帮助]", func() tea.Cmd { return m.dispatchMouseKey("?") })
	add("[q 退出]", func() tea.Cmd { return m.dispatchMouseKey("q") })
	return targets
}

func (m *model) addStartMouseTargets(frame renderedFrame, targets *[]mouseTarget) {
	add := func(marker string, activate func() tea.Cmd) {
		*targets = append(*targets, frame.targets(marker, activate)...)
	}
	if m.startStep == startStepBasic {
		add("2 高级选项", func() tea.Cmd { return m.dispatchMouseKey("enter") })
		add("运行已有任务", func() tea.Cmd { return m.selectStartMode(startExistingTask) })
		add("从仓库生成", func() tea.Cmd { return m.selectStartMode(startGenerateTask) })
	} else {
		add("1 基本配置", func() tea.Cmd {
			m.startStep = startStepBasic
			m.startField = startFieldMode
			return m.focusStartInput(m.startField)
		})
		for index, group := range advancedGroups() {
			index, group := index, group
			add(startGroupName(group), func() tea.Cmd {
				m.selectAdvancedGroupByNumber(index + 1)
				return nil
			})
		}
	}
	for _, field := range m.activeStartFields() {
		if field == startFieldMode {
			continue
		}
		field := field
		add(localizeField(field), func() tea.Cmd {
			m.startField = field
			cmd := m.focusStartInput(field)
			if isBoolStartField(field) {
				m.toggleStartBool()
			}
			return cmd
		})
	}
}

func (m *model) selectStartMode(mode startMode) tea.Cmd {
	m.startMode = mode
	m.opts.Generate = mode == startGenerateTask
	m.startField = startFieldMode
	return m.focusStartInput(m.startField)
}

func (m *model) dispatchMouseKey(key string) tea.Cmd {
	msg := mouseKey(key)
	if handled, cmd := m.handleGlobalKey(msg); handled {
		return cmd
	}
	m.bindPages()
	_, cmd := m.router.Dispatch(msg)
	return cmd
}

func runeMouseKey(key string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
}

func mouseKey(key string) tea.KeyMsg {
	switch key {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "ctrl+o":
		return tea.KeyMsg{Type: tea.KeyCtrlO}
	case "ctrl+g":
		return tea.KeyMsg{Type: tea.KeyCtrlG}
	case "ctrl+d":
		return tea.KeyMsg{Type: tea.KeyCtrlD}
	case "ctrl+l":
		return tea.KeyMsg{Type: tea.KeyCtrlL}
	case "ctrl+e":
		return tea.KeyMsg{Type: tea.KeyCtrlE}
	case "ctrl+n":
		return tea.KeyMsg{Type: tea.KeyCtrlN}
	case "ctrl+r":
		return tea.KeyMsg{Type: tea.KeyCtrlR}
	case "ctrl+b":
		return tea.KeyMsg{Type: tea.KeyCtrlB}
	case "ctrl+s":
		return tea.KeyMsg{Type: tea.KeyCtrlS}
	case "ctrl+q":
		return tea.KeyMsg{Type: tea.KeyCtrlQ}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "delete":
		return tea.KeyMsg{Type: tea.KeyDelete}
	default:
		return runeMouseKey(key)
	}
}
