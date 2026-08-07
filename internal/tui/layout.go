package tui

import "github.com/charmbracelet/lipgloss"

// appTitle is the banner rendered on every screen's header row.
const appTitle = "Harbor Task Factory"

// Terminal chrome occupies a fixed number of rows on every screen. Naming the
// budget here keeps the row arithmetic in one place instead of repeating
// height-3 / height-6 / height-7 offsets across each view.
const (
	// headerRows is the single-row application banner.
	headerRows = 1
	// footerRows is the single-row key hint line.
	footerRows = 1
	// framedPaneRows counts the top and bottom border of a bordered pane.
	framedPaneRows = 2
	// minimumBodyRows keeps a body pane usable on a very short terminal.
	minimumBodyRows = 3
	// appHorizontalPadding matches appStyle's Padding(0, 1) on both sides.
	appHorizontalPadding = 2
	// detailLabelColumns is the fixed label gutter width in a field row.
	detailLabelColumns = 11
	// detailFieldGutter is the space a field row spends on padding and border.
	detailFieldGutter = 6
)

// chrome converts a terminal window size into the row and column budget each
// screen may render into. Every screen derives its body size from this type so
// no view can silently exceed the window.
type chrome struct {
	width  int
	height int
	// statusRows is 1 when a notice or error line is present, otherwise 0.
	statusRows int
	// promptRows is the measured height of an open prompt pane, otherwise 0.
	promptRows int
}

// newChrome builds a budget for a window, recording whether a status line and
// prompt pane are currently visible.
func newChrome(width, height int, status string, prompt string) chrome {
	budget := chrome{width: width, height: height}
	if status != "" {
		budget.statusRows = lipgloss.Height(status)
	}
	if prompt != "" {
		budget.promptRows = lipgloss.Height(prompt)
	}
	return budget
}

// contentWidth is the column budget inside the application padding.
func (budget chrome) contentWidth() int {
	return max(1, budget.width-appHorizontalPadding)
}

// bodyRows is the row budget a screen's scrollable body may occupy.
//
// The floor is deliberately not applied against the whole window: a screen that
// claimed minimumBodyRows while an open prompt already consumed the remaining
// rows would render taller than the terminal, which is the overflow this type
// exists to prevent. The body therefore yields to chrome, and only what is
// genuinely left over is floored to one row.
func (budget chrome) bodyRows() int {
	used := headerRows + footerRows + budget.statusRows + budget.promptRows
	available := budget.height - used
	if available >= minimumBodyRows {
		return available
	}
	return max(1, available)
}

// framedBodyRows is the row budget for content inside a bordered pane that
// itself must fit within bodyRows.
func (budget chrome) framedBodyRows() int {
	return max(1, budget.bodyRows()-framedPaneRows)
}
