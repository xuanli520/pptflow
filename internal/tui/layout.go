package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/lipgloss"
)

type layoutMode int

const (
	layoutWide layoutMode = iota
	layoutMedium
	layoutStacked
	layoutMinimal
)

type appLayout struct {
	mode                layoutMode
	contentWidth        int
	contentHeight       int
	overviewTableHeight int
	leftWidth           int
	rightWidth          int
	stageHeight         int
	detailWidth         int
	detailHeight        int
}

type overviewColumnSpec struct {
	Key   string
	Title string
	Width int
}

func layoutFor(width, height int, execution bool) appLayout {
	if width <= 0 {
		width = 100
	}
	if height <= 0 {
		height = 30
	}
	contentWidth := max(24, width-2)
	contentHeight := max(6, height-7)
	panelFrameWidth := panelStyle.GetHorizontalFrameSize()
	panelFrameHeight := panelStyle.GetVerticalFrameSize()
	mode := layoutWide
	switch {
	case width < 72:
		mode = layoutMinimal
	case width < 90:
		mode = layoutStacked
	case width < 120:
		mode = layoutMedium
	}
	layout := appLayout{
		mode:                mode,
		contentWidth:        contentWidth,
		contentHeight:       contentHeight,
		overviewTableHeight: max(6, contentHeight-2),
		detailWidth:         contentWidth,
		detailHeight:        contentHeight,
	}
	if !execution {
		return layout
	}
	switch mode {
	case layoutWide:
		layout.leftWidth = max(26, contentWidth*35/100)
		layout.rightWidth = max(40, contentWidth-layout.leftWidth)
		if layout.leftWidth+layout.rightWidth > contentWidth {
			layout.rightWidth = max(20, contentWidth-layout.leftWidth)
		}
		layout.detailWidth = max(12, layout.rightWidth-panelFrameWidth)
		layout.detailHeight = max(4, contentHeight-panelFrameHeight)
	case layoutMedium:
		layout.leftWidth = max(22, contentWidth*32/100)
		layout.rightWidth = max(32, contentWidth-layout.leftWidth)
		if layout.leftWidth+layout.rightWidth > contentWidth {
			layout.rightWidth = max(20, contentWidth-layout.leftWidth)
		}
		layout.detailWidth = max(12, layout.rightWidth-panelFrameWidth)
		layout.detailHeight = max(4, contentHeight-panelFrameHeight)
	case layoutStacked, layoutMinimal:
		layout.stageHeight = min(max(5, contentHeight/3), 10)
		layout.detailWidth = max(12, contentWidth-panelFrameWidth)
		layout.detailHeight = max(4, contentHeight-layout.stageHeight-panelFrameHeight)
	}
	return layout
}

func verticalChromeHeight(m app) int {
	height := 1
	if m.message != "" {
		height++
	}
	if m.confirmCancelTaskID != "" {
		height++
	}
	height += pipelineBarHeight(m)
	height++
	return height
}

func buildOverviewColumns(width int, sort overviewSortMode, asc bool) []table.Column {
	return columnsFromSpecs(overviewColumnSpecs(width, sort, asc))
}

func overviewColumnSpecs(width int, sort overviewSortMode, asc bool) []overviewColumnSpec {
	breakpoint := 0
	switch {
	case width >= 120:
		breakpoint = 0
	case width >= 90:
		breakpoint = 1
	case width >= 72:
		breakpoint = 2
	default:
		breakpoint = 3
	}
	defs := []struct {
		key    string
		title  string
		widths [4]int
	}{
		{"task_id", "任务ID", [4]int{24, 22, 18, 14}},
		{"run_status", "状态", [4]int{12, 10, 8, 6}},
		{"failed_stage", "失败", [4]int{8, 6, 5, 4}},
		{"blocker", "阻断", [4]int{6, 5, 4, 3}},
		{"high", "严重", [4]int{6, 5, 4, 3}},
		{"manual_verdict", "判定", [4]int{8, 6, 0, 0}},
		{"docs", "文档", [4]int{6, 5, 4, 0}},
		{"cleanup", "清理", [4]int{10, 8, 0, 0}},
		{"batch", "批次", [4]int{12, 8, 0, 0}},
		{"last_run", "最后运行", [4]int{16, 0, 0, 0}},
		{"mode", "模式", [4]int{10, 0, 0, 0}},
	}
	specs := make([]overviewColumnSpec, 0, len(defs))
	for _, def := range defs {
		if width := def.widths[breakpoint]; width > 0 {
			title := def.title
			if overviewSortKey(sort) == def.key {
				if asc {
					title += "↑"
				} else {
					title += "↓"
				}
			}
			specs = append(specs, overviewColumnSpec{Key: def.key, Title: title, Width: width})
		}
	}
	return specs
}

func overviewSortKey(sort overviewSortMode) string {
	switch sort {
	case sortByStatus:
		return "run_status"
	case sortBySeverity:
		return "blocker"
	case sortByLastRun:
		return "last_run"
	case sortByVerdict:
		return "manual_verdict"
	default:
		return "task_id"
	}
}

func truncateDisplay(value string, width int) string {
	if width <= 0 {
		return ""
	}
	value = strings.TrimSpace(value)
	if lipgloss.Width(value) <= width {
		return value
	}
	if width == 1 {
		return "…"
	}
	return displayPrefix(value, width-1) + "…"
}

func truncateMiddleDisplay(value string, width int) string {
	if width <= 0 {
		return ""
	}
	value = strings.TrimSpace(value)
	if lipgloss.Width(value) <= width {
		return value
	}
	if width <= 3 {
		return truncateDisplay(value, width)
	}
	leftWidth := (width - 1) / 2
	rightWidth := width - 1 - leftWidth
	return displayPrefix(value, leftWidth) + "…" + displaySuffix(value, rightWidth)
}

func wrapDisplay(value string, width int) []string {
	if width <= 0 {
		return []string{value}
	}
	value = strings.ReplaceAll(value, "\r\n", "\n")
	var result []string
	for _, raw := range strings.Split(value, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			result = append(result, "")
			continue
		}
		var builder strings.Builder
		currentWidth := 0
		for _, r := range line {
			runeWidth := lipgloss.Width(string(r))
			if currentWidth+runeWidth > width && builder.Len() > 0 {
				result = append(result, builder.String())
				builder.Reset()
				currentWidth = 0
			}
			builder.WriteRune(r)
			currentWidth += runeWidth
		}
		if builder.Len() > 0 {
			result = append(result, builder.String())
		}
	}
	return result
}

func displayPrefix(value string, width int) string {
	var builder strings.Builder
	currentWidth := 0
	for _, r := range value {
		runeWidth := lipgloss.Width(string(r))
		if currentWidth+runeWidth > width {
			break
		}
		builder.WriteRune(r)
		currentWidth += runeWidth
	}
	return builder.String()
}

func displaySuffix(value string, width int) string {
	runes := []rune(value)
	currentWidth := 0
	start := len(runes)
	for start > 0 {
		runeWidth := lipgloss.Width(string(runes[start-1]))
		if currentWidth+runeWidth > width {
			break
		}
		currentWidth += runeWidth
		start--
	}
	return string(runes[start:])
}

func clamp(value, low, high int) int {
	if high < low {
		return low
	}
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
