package tui

import "github.com/charmbracelet/lipgloss"

var (
	titleStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	subtleStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	sectionStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	passStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	warnStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	failStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	panelStyle   = lipgloss.NewStyle().
			Border(lipgloss.NormalBorder()).
			BorderForeground(lipgloss.Color("240")).
			Padding(1, 2)
)
