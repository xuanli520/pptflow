package tui

import "github.com/charmbracelet/lipgloss"

var (
	colorPrimary   = lipgloss.Color("#5B8DEE")
	colorSuccess   = lipgloss.Color("#6BBF6B")
	colorDanger    = lipgloss.Color("#E05555")
	colorMuted     = lipgloss.Color("#6B7280")
	colorHighlight = lipgloss.Color("#F5F5F5")

	appStyle = lipgloss.NewStyle().Padding(0, 1)

	headerStyle = lipgloss.NewStyle().
			Foreground(colorHighlight).
			Background(colorPrimary).
			Padding(0, 1).
			Bold(true)

	columnTitleStyle = lipgloss.NewStyle().
				Foreground(colorHighlight).
				Bold(true)

	columnTitleMutedStyle = lipgloss.NewStyle().
				Foreground(colorMuted)

	cardStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorMuted).
			Padding(0, 1)

	cardSelectedStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(colorPrimary).
				Padding(0, 1)

	cardTitleStyle = lipgloss.NewStyle().
			Foreground(colorHighlight).
			Bold(true)

	statusRunningStyle = lipgloss.NewStyle().
				Foreground(colorSuccess).
				Bold(true)

	inputStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorMuted).
			Padding(0, 1)

	footerStyle = lipgloss.NewStyle().
			Foreground(colorMuted)

	detailTitleStyle = lipgloss.NewStyle().
				Foreground(colorHighlight).
				Background(colorPrimary).
				Padding(0, 1).
				Bold(true)

	detailBreadcrumbStyle = lipgloss.NewStyle().
				Foreground(colorMuted)

	detailSubtitleStyle = lipgloss.NewStyle().
				Foreground(colorHighlight).
				Bold(true)

	detailSectionStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(colorMuted).
				Padding(0, 1)

	detailSectionTitleStyle = lipgloss.NewStyle().
				Foreground(colorPrimary).
				Bold(true)

	detailLabelStyle = lipgloss.NewStyle().
				Foreground(colorMuted)

	detailValueStyle = lipgloss.NewStyle().
				Foreground(colorHighlight)

	detailHistoryCurrentStyle = lipgloss.NewStyle().
					Foreground(colorSuccess).
					Bold(true)

	logContentStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorMuted).
			Padding(0, 1)

	mutedStyle = lipgloss.NewStyle().
			Foreground(colorMuted)

	highlightStyle = lipgloss.NewStyle().
			Foreground(colorPrimary).
			Bold(true)

	passStyleV2 = lipgloss.NewStyle().
			Foreground(colorSuccess)

	failStyleV2 = lipgloss.NewStyle().
			Foreground(colorDanger)
)
