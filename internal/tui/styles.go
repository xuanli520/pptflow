package tui

import "github.com/charmbracelet/lipgloss"

// Theme centralises every colour and reusable style used by the TUI. Keeping
// the palette here makes the views consistent and lets a future configurable
// theme replace the defaults without touching page code.
type Theme struct {
	Primary  lipgloss.Color
	Success  lipgloss.Color
	Warning  lipgloss.Color
	Error    lipgloss.Color
	Muted    lipgloss.Color
	Border   lipgloss.Color
	Title    lipgloss.Style
	Section  lipgloss.Style
	Panel    lipgloss.Style
	Help     lipgloss.Style
	Selected lipgloss.Style
	Focused  lipgloss.Style
}

func newTheme() Theme {
	primary := lipgloss.Color("#00afff")
	success := lipgloss.Color("#00d700")
	warning := lipgloss.Color("#ffaf00")
	failure := lipgloss.Color("#ff5f5f")
	muted := lipgloss.Color("#808080")
	border := lipgloss.Color("#585858")
	return Theme{
		Primary: primary, Success: success, Warning: warning, Error: failure,
		Muted: muted, Border: border,
		Title:    lipgloss.NewStyle().Bold(true).Foreground(primary),
		Section:  lipgloss.NewStyle().Bold(true).Foreground(warning),
		Panel:    lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(border).Padding(1, 2),
		Help:     lipgloss.NewStyle().Foreground(muted),
		Selected: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#ffffff")).Background(primary),
		Focused:  lipgloss.NewStyle().Foreground(primary).Bold(true),
	}
}

var defaultTheme = newTheme()

// Compatibility aliases keep business/render helpers small while all style
// definitions still come from the theme.
var (
	titleStyle    = defaultTheme.Title
	subtleStyle   = defaultTheme.Help
	sectionStyle  = defaultTheme.Section
	passStyle     = lipgloss.NewStyle().Foreground(defaultTheme.Success)
	warnStyle     = lipgloss.NewStyle().Foreground(defaultTheme.Warning)
	failStyle     = lipgloss.NewStyle().Foreground(defaultTheme.Error)
	panelStyle    = defaultTheme.Panel
	selectedStyle = defaultTheme.Selected
)
