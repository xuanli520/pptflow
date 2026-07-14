package tui

import (
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

type mouseTarget struct {
	x, y, width int
	activate    func() tea.Cmd
}

func (target mouseTarget) contains(x, y int) bool {
	return y == target.y && x >= target.x && x < target.x+maxInt(1, target.width)
}

type renderedFrame struct{ lines []string }

func newRenderedFrame(view string) renderedFrame {
	return renderedFrame{lines: strings.Split(ansi.Strip(view), "\n")}
}

func (frame renderedFrame) targets(marker string, activate func() tea.Cmd) []mouseTarget {
	if marker == "" || activate == nil {
		return nil
	}
	var targets []mouseTarget
	for y, line := range frame.lines {
		searchFrom := 0
		for searchFrom <= len(line) {
			index := strings.Index(line[searchFrom:], marker)
			if index < 0 {
				break
			}
			index += searchFrom
			targets = append(targets, mouseTarget{
				x: ansi.StringWidth(line[:index]), y: y, width: ansi.StringWidth(marker), activate: activate,
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
		if event.Button == tea.MouseButtonWheelUp {
			m.moveTaskHubSelection(-3)
		} else {
			m.moveTaskHubSelection(3)
		}
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
	return m.taskHubDetail != nil || m.taskHubHelpVisible || m.exitHandoff != nil || m.taskHubMutation != nil || m.runControl != nil
}

func (m *model) mouseTargets() []mouseTarget {
	frame := newRenderedFrame(m.View())
	var targets []mouseTarget
	add := func(marker string, activate func() tea.Cmd) {
		targets = append(targets, frame.targets(marker, activate)...)
	}
	if m.exitHandoff != nil {
		for index, item := range m.exitHandoff.Items {
			if !item.selectable() {
				continue
			}
			index := index
			add(shortTaskHubID(item.Run.RunID), func() tea.Cmd {
				m.exitHandoff.Selected = index
				m.exitHandoff.toggleSelected()
				return nil
			})
		}
		add("[Enter] 交接并退出", func() tea.Cmd { return m.updateTaskHubExitHandoffKey(mouseKey("enter")) })
		add("[Esc] 返回", func() tea.Cmd { return m.updateTaskHubExitHandoffKey(mouseKey("esc")) })
		return targets
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
			add("[Enter] 查看影响预览", func() tea.Cmd { return m.updateRunControlKey(mouseKey("enter")) })
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
			return m.selectTaskHubTab(tab.tab)
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

func (m *model) dispatchMouseKey(key string) tea.Cmd {
	msg := mouseKey(key)
	if handled, command := m.handleGlobalKey(msg); handled {
		return command
	}
	m.bindPages()
	_, command := m.router.Dispatch(msg)
	return command
}

func runeMouseKey(key string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
}

func mouseKey(key string) tea.KeyMsg {
	switch key {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	default:
		return runeMouseKey(key)
	}
}
