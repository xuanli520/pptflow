package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type SettingsOverlay struct {
	content string
}

func (SettingsOverlay) Init() tea.Cmd                  { return nil }
func (SettingsOverlay) Update(tea.Msg) (bool, tea.Cmd) { return false, nil }
func (SettingsOverlay) ZIndex() int                    { return 100 }
func (SettingsOverlay) InterceptsAllKeys() bool        { return true }
func (SettingsOverlay) Destroy() tea.Cmd               { return nil }

func (o SettingsOverlay) WithContent(content string) SettingsOverlay {
	o.content = content
	return o
}

func (o SettingsOverlay) View(width, height int) string {
	content := strings.TrimRight(o.content, "\n")
	if content == "" {
		return ""
	}
	overlayWidth := clamp(width*35/100, 40, 60)
	if width > 0 {
		overlayWidth = min(overlayWidth, max(20, width-4))
	}
	lines := strings.Split(content, "\n")
	overlayHeight := min(len(lines)+panelStyle.GetVerticalFrameSize(), max(10, height*6/10))
	if overlayHeight <= 0 {
		overlayHeight = min(len(lines)+panelStyle.GetVerticalFrameSize(), 20)
	}
	bodyHeight := max(1, overlayHeight-panelStyle.GetVerticalFrameSize())
	if len(lines) > bodyHeight {
		lines = lines[:bodyHeight]
	}
	panel := renderPanel(overlayWidth, overlayHeight, strings.Join(lines, "\n"))
	leftPad := max(0, width-overlayWidth-2)
	topPad := max(0, height-overlayHeight-3)
	return strings.Repeat("\n", topPad) + lipgloss.NewStyle().MarginLeft(leftPad).Render(panel)
}

func renderSettingsOverlay(m app) string {
	return m.settingsUI.WithContent(renderSettings(m)).View(m.width, m.height)
}
