package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type SettingsOverlay struct {
	content string
}

func (SettingsOverlay) Init() tea.Cmd           { return nil }
func (SettingsOverlay) ZIndex() int             { return 100 }
func (SettingsOverlay) InterceptsAllKeys() bool { return true }
func (SettingsOverlay) Destroy() tea.Cmd        { return nil }

func (SettingsOverlay) Update(msg tea.Msg) (bool, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return false, nil
	}
	switch key.String() {
	case "esc", "q":
		return true, nil
	default:
		return isSettingsShortcutKey(key.String()), nil
	}
}

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
	return renderPanel(overlayWidth, overlayHeight, strings.Join(lines, "\n"))
}

func renderSettingsOverlay(m app) string {
	return m.settingsUI.WithContent(renderSettings(m)).View(max(20, m.width-2), m.height)
}

func renderOverlayBottomRight(base, overlay string, width, height int) string {
	overlay = strings.TrimRight(overlay, "\n")
	if overlay == "" {
		return base
	}
	lines := strings.Split(strings.TrimRight(base, "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}
	contentWidth := width
	if contentWidth <= 0 {
		for _, line := range lines {
			contentWidth = max(contentWidth, lipgloss.Width(line))
		}
	}
	contentWidth = max(1, contentWidth)
	overlayLines := strings.Split(overlay, "\n")
	canvasHeight := max(len(lines), height)
	for len(lines) < canvasHeight {
		lines = append(lines, "")
	}
	overlayWidth := 0
	for _, line := range overlayLines {
		overlayWidth = max(overlayWidth, lipgloss.Width(line))
	}
	leftPad := max(0, contentWidth-overlayWidth)
	top := max(0, canvasHeight-len(overlayLines)-1)
	for index, line := range overlayLines {
		row := top + index
		if row >= len(lines) {
			break
		}
		lines[row] = strings.Repeat(" ", leftPad) + line
	}
	return strings.Join(lines, "\n")
}
