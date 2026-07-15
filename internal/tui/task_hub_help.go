package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// taskHubHelpOverlay documents only V2 lifecycle navigation. It deliberately
// omits legacy workspace and runner shortcuts so the hard CLI/TUI cutover is
// not obscured by stale instructions.
type taskHubHelpOverlay struct{}

func (taskHubHelpOverlay) View(width, height int) string {
	panelWidth := boundedPanelWidth(width, 40, 88)
	contentWidth := styleContentWidth(panelWidth, panelStyle)
	rows := []string{
		sectionStyle.Render("Task Hub 帮助"),
		"",
		"[Tab/←→] 在 Tasks、Runs、Queue 间切换",
		"[↑↓/j k] 选择 Task 或 Run  [Enter/d] 查看只读详情",
		"[/] 搜索当前生命周期快照  [r] 在详情中刷新事实",
		"",
		sectionStyle.Render("两键计划与确认"),
		"[t n/i/s/e/f/a/d/u] Task 生命周期计划",
		"[x c/n/e/h/a/k] 继续、启动、评测、采用证据、附着或运行控制",
		"[v a/c/r] 审核  [p p/w] 本地 package/撤回",
		"[Ctrl+X] 运行控制；仅 interrupted/in_doubt Run 显示 [R] 本地 reconcile",
		"",
		warnStyle.Render("计划确认时必须填写原因；操作员与 UUIDv7 幂等键由本机生成。"),
		"",
		subtleStyle.Render("[? / Esc / q] 关闭帮助"),
	}
	rows = clipOverlayRows(rows, contentWidth)
	rows = fitOverlayRows(rows, height, 1)
	body := lipgloss.JoinVertical(lipgloss.Left, rows...)
	box := panelStyle.Width(panelWidth).Align(lipgloss.Left).Render(body)
	return lipgloss.Place(maxInt(1, width), maxInt(1, height), lipgloss.Center, lipgloss.Center, box)
}

func taskHubHelpText() string {
	return strings.Join([]string{
		"Tasks", "Runs", "Queue", "两键计划与确认", "UUIDv7 幂等键",
	}, " ")
}
