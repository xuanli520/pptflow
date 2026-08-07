package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	"github.com/charmbracelet/x/ansi"
)

// scrollPane is the single scrolling primitive shared by every read-only
// screen. It wraps bubbles/viewport so the TUI keeps one implementation of
// offset clamping, paging, and line accounting instead of one per screen.
type scrollPane struct {
	view  viewport.Model
	lines []string
}

// newScrollPane builds an empty pane. Content and size are applied separately
// because a screen learns its window budget only at render time.
func newScrollPane() *scrollPane {
	return &scrollPane{view: viewport.New(1, 1)}
}

// SetContent replaces the pane's text, wrapping it to the given column budget.
// Wrapping happens here so line accounting and scrolling agree on what one
// visible row is.
func (pane *scrollPane) SetContent(text string, width int) {
	if pane == nil {
		return
	}
	wrapped := ansi.WrapWc(ansi.Strip(text), max(1, width), "")
	pane.lines = strings.Split(wrapped, "\n")
	pane.view.SetContent(wrapped)
}

// Resize applies the current window budget to the pane.
func (pane *scrollPane) Resize(width, height int) {
	if pane == nil {
		return
	}
	pane.view.Width = max(1, width)
	pane.view.Height = max(1, height)
}

// LineCount is the number of wrapped content rows.
func (pane *scrollPane) LineCount() int {
	if pane == nil {
		return 0
	}
	return len(pane.lines)
}

// FirstVisibleLine is the 1-based index of the topmost rendered row. It
// returns 0 for empty content so callers can render an explicit empty state.
func (pane *scrollPane) FirstVisibleLine() int {
	if pane == nil || len(pane.lines) == 0 {
		return 0
	}
	return pane.view.YOffset + 1
}

// LastVisibleLine is the 1-based index of the bottommost rendered row.
func (pane *scrollPane) LastVisibleLine() int {
	if pane == nil || len(pane.lines) == 0 {
		return 0
	}
	return min(len(pane.lines), pane.view.YOffset+pane.view.Height)
}

func (pane *scrollPane) MoveUp() {
	if pane != nil {
		pane.view.LineUp(1)
	}
}

func (pane *scrollPane) MoveDown() {
	if pane != nil {
		pane.view.LineDown(1)
	}
}

func (pane *scrollPane) PageUp() {
	if pane != nil {
		pane.view.ViewUp()
	}
}

func (pane *scrollPane) PageDown() {
	if pane != nil {
		pane.view.ViewDown()
	}
}

func (pane *scrollPane) GoToStart() {
	if pane != nil {
		pane.view.GotoTop()
	}
}

func (pane *scrollPane) GoToEnd() {
	if pane != nil {
		pane.view.GotoBottom()
	}
}

// EnsureVisible scrolls the minimum amount needed to bring a 0-based content
// line into view. A screen with a selection uses it so moving the selection
// never leaves it rendered off-pane.
func (pane *scrollPane) EnsureVisible(line int) {
	if pane == nil || len(pane.lines) == 0 {
		return
	}
	line = min(max(0, line), len(pane.lines)-1)
	height := max(1, pane.view.Height)
	if line < pane.view.YOffset {
		pane.view.SetYOffset(line)
		return
	}
	if line >= pane.view.YOffset+height {
		pane.view.SetYOffset(line - height + 1)
	}
}

// View renders exactly the pane's height in rows.
func (pane *scrollPane) View() string {
	if pane == nil {
		return ""
	}
	return pane.view.View()
}
