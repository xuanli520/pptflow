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

func buildOverviewColumns(width int) []table.Column {
	specs := overviewColumnSpecs(width)
	columns := make([]table.Column, 0, len(specs))
	for _, spec := range specs {
		columns = append(columns, table.Column{Title: spec.Title, Width: spec.Width})
	}
	return columns
}

func overviewColumnSpecs(width int) []overviewColumnSpec {
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
			specs = append(specs, overviewColumnSpec{Key: def.key, Title: def.title, Width: width})
		}
	}
	return specs
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
	var builder strings.Builder
	for _, r := range value {
		next := builder.String() + string(r)
		if lipgloss.Width(next)+1 > width {
			break
		}
		builder.WriteRune(r)
	}
	return builder.String() + "…"
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
		for _, r := range line {
			next := builder.String() + string(r)
			if lipgloss.Width(next) > width && builder.Len() > 0 {
				result = append(result, builder.String())
				builder.Reset()
			}
			builder.WriteRune(r)
		}
		if builder.Len() > 0 {
			result = append(result, builder.String())
		}
	}
	return result
}

func displayPrefix(value string, width int) string {
	var builder strings.Builder
	for _, r := range value {
		next := builder.String() + string(r)
		if lipgloss.Width(next) > width {
			break
		}
		builder.WriteRune(r)
	}
	return builder.String()
}

func displaySuffix(value string, width int) string {
	runes := []rune(value)
	var parts []string
	for i := len(runes) - 1; i >= 0; i-- {
		candidate := string(runes[i]) + strings.Join(parts, "")
		if lipgloss.Width(candidate) > width {
			break
		}
		parts = append([]string{string(runes[i])}, parts...)
	}
	return strings.Join(parts, "")
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
