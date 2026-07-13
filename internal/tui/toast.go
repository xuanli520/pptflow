package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

type toastLevel int

const (
	toastInfo toastLevel = iota
	toastSuccess
	toastWarning
	toastError
)

type toastState struct {
	ID      uint64
	Message string
	Level   toastLevel
}

func (m *model) showToast(message string, level toastLevel) tea.Cmd {
	m.toast.ID++
	m.toast.Message = message
	m.toast.Level = level
	id := m.toast.ID
	return tea.Tick(4*time.Second, func(time.Time) tea.Msg { return toastExpiredMsg{id: id} })
}

func (m model) renderToast() string {
	if m.toast.Message == "" {
		return ""
	}
	text := redactSingleLineUI(m.toast.Message)
	var rendered string
	switch m.toast.Level {
	case toastSuccess:
		rendered = passStyle.Render("✓ " + text)
	case toastWarning:
		rendered = warnStyle.Render("⚠ " + text)
	case toastError:
		rendered = failStyle.Render("✗ " + text)
	default:
		rendered = defaultTheme.Focused.Render("● " + text)
	}
	if m.width > 0 {
		rendered = clipDisplay(rendered, m.width)
	}
	return rendered
}
