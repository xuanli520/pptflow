package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

func (m *model) bindPages() {
	if m.router == nil {
		m.router = newPageRouter(viewHub)
	}
	m.router.Register(viewHub, &hubPage{pageBase{m: m}})
	m.router.SwitchTo(viewHub)
}

func (m *model) currentPage() Page {
	m.bindPages()
	return m.router.Page(viewHub)
}

func renderFrame(m model, body string) string {
	if m.exitHandoff != nil {
		frame := lipgloss.JoinVertical(lipgloss.Left, m.header(), m.exitHandoff.View(m.width, maxInt(8, m.height-3)), m.footer())
		return strings.TrimRight(frame, "\n") + "\n"
	}
	if m.taskHubDetail != nil {
		frame := lipgloss.JoinVertical(lipgloss.Left, m.header(), m.taskHubDetail.View(m.width, maxInt(8, m.height-3)), m.footer())
		return strings.TrimRight(frame, "\n") + "\n"
	}
	if m.taskHubHelpVisible {
		frame := lipgloss.JoinVertical(lipgloss.Left, m.header(), (taskHubHelpOverlay{}).View(m.width, maxInt(8, m.height-3)), m.footer())
		return strings.TrimRight(frame, "\n") + "\n"
	}
	if m.taskHubMutation != nil {
		frame := lipgloss.JoinVertical(lipgloss.Left, m.header(), m.taskHubMutation.View(m.width, maxInt(8, m.height-3)), m.footer())
		return strings.TrimRight(frame, "\n") + "\n"
	}
	if m.runControl != nil {
		frame := lipgloss.JoinVertical(lipgloss.Left, m.header(), m.runControl.View(m.width, maxInt(8, m.height-3)), m.footer())
		return strings.TrimRight(frame, "\n") + "\n"
	}
	parts := []string{m.header(), body}
	if toast := m.renderToast(); toast != "" {
		parts = append(parts, toast)
	}
	parts = append(parts, m.footer())
	return strings.TrimRight(lipgloss.JoinVertical(lipgloss.Left, parts...), "\n") + "\n"
}
