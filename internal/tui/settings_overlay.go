package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type SettingsOverlay struct{}

func (SettingsOverlay) Init() tea.Cmd                  { return nil }
func (SettingsOverlay) Update(tea.Msg) (bool, tea.Cmd) { return false, nil }
func (SettingsOverlay) ZIndex() int                    { return 100 }
func (SettingsOverlay) InterceptsAllKeys() bool        { return true }
func (SettingsOverlay) Destroy() tea.Cmd               { return nil }

func (SettingsOverlay) View(width, height int) string {
	return ""
}

func renderSettingsOverlay(m app) string {
	content := renderSettings(m)
	overlayWidth := clamp(m.width*35/100, 40, 60)
	if m.width > 0 {
		overlayWidth = min(overlayWidth, max(20, m.width-4))
	}
	lines := strings.Split(content, "\n")
	overlayHeight := min(len(lines)+panelStyle.GetVerticalFrameSize(), max(10, m.height*6/10))
	if overlayHeight <= 0 {
		overlayHeight = min(len(lines)+panelStyle.GetVerticalFrameSize(), 20)
	}
	bodyHeight := max(1, overlayHeight-panelStyle.GetVerticalFrameSize())
	if len(lines) > bodyHeight {
		lines = lines[:bodyHeight]
	}
	panel := renderPanel(overlayWidth, overlayHeight, strings.Join(lines, "\n"))
	leftPad := max(0, m.width-overlayWidth-2)
	topPad := max(0, m.height-overlayHeight-3)
	return strings.Repeat("\n", topPad) + lipgloss.NewStyle().MarginLeft(leftPad).Render(panel)
}
