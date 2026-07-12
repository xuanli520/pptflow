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
	text := redactUI(m.toast.Message)
	switch m.toast.Level {
	case toastSuccess:
		return passStyle.Render("✓ " + text)
	case toastWarning:
		return warnStyle.Render("⚠ " + text)
	case toastError:
		return failStyle.Render("✗ " + text)
	default:
		return defaultTheme.Focused.Render("● " + text)
	}
}
