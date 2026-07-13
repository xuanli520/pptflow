package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

type WorkspaceResumeOverlay struct {
	Item     store.RunWithTask
	Selected int
	Complete int
	Total    int
}

func NewWorkspaceResumeOverlay(item store.RunWithTask) *WorkspaceResumeOverlay {
	_, events := loadWorkspaceState(item.Run.WorkspacePath)
	completed := map[string]bool{}
	for _, event := range events {
		if event.NodeID != "" && terminalRunnerEvent(event) {
			completed[event.NodeID] = true
		}
	}
	return &WorkspaceResumeOverlay{Item: item, Complete: len(completed), Total: len(nodeOrder())}
}

func (o *WorkspaceResumeOverlay) Init() tea.Cmd                  { return nil }
func (o *WorkspaceResumeOverlay) Update(tea.Msg) (bool, tea.Cmd) { return true, nil }
func (o *WorkspaceResumeOverlay) Focus()                         {}
func (o *WorkspaceResumeOverlay) Blur()                          {}
func (o *WorkspaceResumeOverlay) HandleKey(tea.KeyMsg) tea.Cmd   { return nil }
func (o *WorkspaceResumeOverlay) ZIndex() int                    { return 60 }
func (o *WorkspaceResumeOverlay) InterceptsAllKeys() bool        { return true }

func (o *WorkspaceResumeOverlay) View(width, height int) string {
	if o == nil {
		return ""
	}
	name := emptyDash(o.Item.Task.TaskName)
	boxWidth := boundedPanelWidth(width, 42, 72)
	lineWidth := styleContentWidth(boxWidth, panelStyle)
	rows := []string{
		sectionStyle.Render("工作区可恢复"),
		"",
		fmt.Sprintf("%s 上次运行于 %s", redactSingleLineUI(name), hubTime(o.Item.Run)),
		fmt.Sprintf("已完成 %d/%d 个节点", o.Complete, o.Total),
		"",
		resumeChoice(o.Selected == 0, "R", "恢复运行", "从断点继续", lineWidth),
		resumeChoice(o.Selected == 1, "N", "新建运行", "复制配置到新工作区", lineWidth),
		resumeChoice(o.Selected == 2, "V", "只读查看", "不修改任何文件", lineWidth),
		"",
		subtleStyle.Render("↑↓ 选择  Enter 确认  Esc 取消"),
	}
	rows = clipOverlayRows(rows, lineWidth)
	rows = fitOverlayRows(rows, height, 5+o.Selected)
	box := panelStyle.Width(boxWidth).Render(strings.Join(rows, "\n"))
	return lipgloss.Place(maxInt(1, width), maxInt(1, height), lipgloss.Center, lipgloss.Center, box)
}

func resumeChoice(selected bool, key, title, detail string, width int) string {
	prefix := "  "
	if selected {
		prefix = selectedStyle.Render("> ")
	}
	return clipDisplay(prefix+"["+key+"] "+padRightDisplay(title, 12)+subtleStyle.Render(detail), width)
}

func (m *model) updateResumeKey(msg tea.KeyMsg) tea.Cmd {
	o := m.resumeOverlay
	if o == nil {
		return nil
	}
	switch msg.String() {
	case "esc":
		m.closeResumeOverlay()
		return nil
	case "up", "k":
		o.Selected = (o.Selected + 2) % 3
		return nil
	case "down", "j", "tab":
		o.Selected = (o.Selected + 1) % 3
		return nil
	case "r", "R":
		o.Selected = 0
	case "n", "N":
		o.Selected = 1
	case "v", "V":
		o.Selected = 2
	case "enter":
	default:
		return nil
	}
	path := o.Item.Run.WorkspacePath
	taskName := o.Item.Task.TaskName
	selection := o.Selected
	m.closeResumeOverlay()
	switch selection {
	case 0:
		opts, _, err := app.LoadRunnerOptions(path)
		if err != nil {
			m.err = err
			return nil
		}
		opts = app.MergeRuntimeOptions(opts, m.runtimeOpts)
		opts.AutoApprove = false
		next := m.startRunner(opts)
		*m = m.attachHubContext(next)
		m.notice = "正在从 run_options.json 恢复工作区。"
		return tea.Batch(m.runWorkflow(), m.waitEvent(), m.refreshWorkspace(), m.spinner.Tick)
	case 1:
		return m.openRunConfig(path, taskName)
	case 2:
		m.openWorkspaceSnapshot(path)
	}
	return nil
}

func (m *model) closeResumeOverlay() {
	m.resumeOverlay = nil
	if m.router != nil {
		m.router.PopOverlay()
	}
	m.focusMgr.Pop()
}
