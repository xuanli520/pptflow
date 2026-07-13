package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

type layoutMode int

const (
	layoutMinimal layoutMode = iota
	layoutStacked
	layoutMedium
	layoutWide
)

type appLayout struct {
	Mode                        layoutMode
	ContentWidth, ContentHeight int
	SidebarWidth, MainWidth     int
}

func layoutFor(width, height int) appLayout {
	contentW := maxInt(1, width-2)
	contentH := maxInt(6, height-7)
	l := appLayout{ContentWidth: contentW, ContentHeight: contentH, MainWidth: contentW}
	switch {
	case width >= 120:
		l.Mode = layoutWide
		l.SidebarWidth = minInt(36, contentW/3)
		l.MainWidth = contentW - l.SidebarWidth - 1
	case width >= 90:
		l.Mode = layoutMedium
		l.SidebarWidth = minInt(28, contentW/3)
		l.MainWidth = contentW - l.SidebarWidth - 1
	case width >= 72:
		l.Mode = layoutStacked
	default:
		l.Mode = layoutMinimal
	}
	return l
}

func padRightDisplay(s string, width int) string {
	if width <= 0 {
		return ""
	}
	s = truncateDisplay(s, width)
	w := ansi.StringWidth(s)
	if w >= width {
		return s
	}
	return s + strings.Repeat(" ", width-w)
}

func truncateDisplay(s string, width int) string {
	if width <= 0 {
		return ""
	}
	return ansi.Truncate(s, width, "…")
}

func clipDisplay(s string, width int) string {
	if width <= 0 {
		return ""
	}
	return ansi.Truncate(s, width, "")
}

func fitDisplay(s string, width int) string {
	if width <= 0 {
		return ""
	}
	s = clipDisplay(s, width)
	return s + strings.Repeat(" ", maxInt(0, width-ansi.StringWidth(s)))
}

// styleContentWidth returns the cells available to content before lipgloss
// applies horizontal padding. Borders are outside Style.Width and therefore
// are accounted for by callers choosing the block width.
func styleContentWidth(blockWidth int, style lipgloss.Style) int {
	return maxInt(1, blockWidth-style.GetHorizontalPadding())
}

// boundedPanelWidth chooses a panel Style.Width that always fits the terminal.
// Lipgloss adds borders outside Style.Width, so the border cells are reserved
// before applying the preferred minimum.
func boundedPanelWidth(terminalWidth, preferredMin, maximum int) int {
	maxBlock := maxInt(1, terminalWidth-panelStyle.GetHorizontalBorderSize())
	desired := terminalWidth - 8
	if desired < preferredMin {
		if maxBlock >= preferredMin {
			desired = preferredMin
		} else {
			desired = maxBlock
		}
	}
	if maximum > 0 {
		desired = minInt(desired, maximum)
	}
	return clampInt(desired, 1, maxBlock)
}

func fitOverlayRows(rows []string, terminalHeight, selectedRow int) []string {
	available := maxInt(1, terminalHeight-panelStyle.GetVerticalFrameSize())
	if len(rows) <= available {
		return rows
	}
	if available == 1 {
		return rows[:1]
	}
	selectedRow = clampInt(selectedRow, 1, len(rows)-1)
	if available == 2 {
		return []string{rows[0], rows[selectedRow]}
	}
	windowSize := available - 2
	start := clampInt(selectedRow-windowSize/2, 1, len(rows)-windowSize)
	end := minInt(len(rows), start+windowSize)
	out := make([]string, 0, available)
	out = append(out, rows[0])
	out = append(out, rows[start:end]...)
	out = append(out, subtleStyle.Render("…"))
	return out
}

func clipOverlayRows(rows []string, width int) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = clipDisplay(row, width)
	}
	return out
}

// textInputView renders a bubbles textinput within an exact cell budget.
// bubbles/textinput v0.21 renders one cursor cell beyond Model.Width, including
// when the cursor is hidden. Reserving that cell here keeps outer decorations
// such as "[ " and " ]" on the same physical terminal line.
func textInputView(input textinput.Model, width int) string {
	if width <= 0 {
		return ""
	}
	input.Width = maxInt(1, width-1)
	input.SetCursor(input.Position()) // recompute the horizontal viewport
	return fitDisplay(input.View(), width)
}

func boxedText(value string, width int) string {
	if width <= 0 {
		return ""
	}
	if width < 4 {
		return clipDisplay(value, width)
	}
	return "[ " + clipDisplay(value, width-4) + " ]"
}

func boxedTextInput(input textinput.Model, width int) string {
	if width <= 0 {
		return ""
	}
	if width < 4 {
		return textInputView(input, width)
	}
	return "[ " + textInputView(input, width-4) + " ]"
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func clampInt(v, low, high int) int {
	if v < low {
		return low
	}
	if v > high {
		return high
	}
	return v
}
