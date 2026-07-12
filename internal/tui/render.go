package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

func (m *model) bindPages() {
	if m.router == nil {
		m.router = newPageRouter(m.view)
	}
	m.router.Register(viewStart, &startPage{pageBase{m: m}})
	m.router.Register(viewOverview, &overviewPage{pageBase{m: m}})
	m.router.Register(viewGate, &gatePage{pageBase{m: m}})
	m.router.Register(viewNodeDetail, &detailPage{pageBase{m: m}})
	m.router.Register(viewLogs, &logsPage{pageBase{m: m}})
	m.router.Register(viewDone, &donePage{pageBase{m: m}})
	m.router.SwitchTo(m.view)
}

func (m *model) currentPage() Page {
	m.bindPages()
	return m.router.Page(m.view)
}

func renderFrame(m model, body string) string {
	if m.helpVisible {
		frame := lipgloss.JoinVertical(lipgloss.Left, m.header(), (&helpOverlay{view: m.view}).View(m.width, maxInt(8, m.height-3)), m.footer())
		return strings.TrimRight(frame, "\n") + "\n"
	}
	if m.confirm != nil {
		frame := lipgloss.JoinVertical(lipgloss.Left, m.header(), m.confirm.View(m.width, maxInt(8, m.height-3)), m.footer())
		return strings.TrimRight(frame, "\n") + "\n"
	}
	parts := []string{m.header(), body}
	if status := m.statusBar(); status != "" {
		parts = append(parts, status)
	}
	if toast := m.renderToast(); toast != "" {
		parts = append(parts, toast)
	}
	parts = append(parts, m.footer())
	frame := lipgloss.JoinVertical(lipgloss.Left, parts...)
	return strings.TrimRight(frame, "\n") + "\n"
}

func joinResponsiveColumns(layout appLayout, left, right string) string {
	if layout.Mode != layoutWide && layout.Mode != layoutMedium {
		return lipgloss.JoinVertical(lipgloss.Left, left, "", right)
	}
	leftStyle := lipgloss.NewStyle().Width(maxInt(20, layout.SidebarWidth-2)).MaxWidth(maxInt(20, layout.SidebarWidth-2))
	rightStyle := lipgloss.NewStyle().Width(maxInt(24, layout.MainWidth-3)).MaxWidth(maxInt(24, layout.MainWidth-3)).BorderLeft(true).BorderForeground(defaultTheme.Border).PaddingLeft(2)
	return lipgloss.JoinHorizontal(lipgloss.Top, leftStyle.Render(left), rightStyle.Render(right))
}
