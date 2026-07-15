package tui

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

type toastLevel int

const (
	toastInfo toastLevel = iota
	toastSuccess
	toastWarning
	toastError

	toastMinimumLifetime = 4 * time.Second
	toastMaximumLifetime = 14 * time.Second
	toastScrollInterval  = 250 * time.Millisecond
	toastScrollGap       = "   "
)

type toastState struct {
	ID      uint64
	Message string
	Level   toastLevel
	Offset  int
}

func (m *model) showToast(message string, level toastLevel) tea.Cmd {
	m.toast.ID++
	m.toast.Message = message
	m.toast.Level = level
	m.toast.Offset = 0
	id := m.toast.ID
	commands := []tea.Cmd{tea.Tick(m.toastLifetime(), func(time.Time) tea.Msg { return toastExpiredMsg{id: id} })}
	if m.toastCanScroll() {
		commands = append(commands, toastScrollCmd(id))
	}
	return tea.Batch(commands...)
}

func (m model) renderToast() string {
	if m.toast.Message == "" {
		return ""
	}
	text := redactSingleLineUI(m.toast.Message)
	prefix := toastPrefix(m.toast.Level)
	style := defaultTheme.Focused
	switch m.toast.Level {
	case toastSuccess:
		style = passStyle
	case toastWarning:
		style = warnStyle
	case toastError:
		style = failStyle
	}
	viewport := maxInt(1, m.width-ansi.StringWidth(prefix))
	visible, _ := toastCircularWindow(text, viewport, m.toast.Offset)
	rendered := style.Render(prefix + visible)
	if m.width > 0 {
		rendered = clipDisplay(rendered, m.width)
	}
	return rendered
}

func (m model) toastLifetime() time.Duration {
	text := redactSingleLineUI(m.toast.Message)
	if !m.toastCanScroll() {
		return toastMinimumLifetime
	}
	// Long failures need enough time for the circular viewport to expose every
	// character at least once. The cap keeps transient UI feedback bounded.
	estimated := time.Duration(len([]rune(text+toastScrollGap))) * 140 * time.Millisecond
	return clampDuration(estimated, toastMinimumLifetime, toastMaximumLifetime)
}

func (m model) toastCanScroll() bool {
	if strings.TrimSpace(m.toast.Message) == "" || m.width <= 0 {
		return false
	}
	prefix := toastPrefix(m.toast.Level)
	return ansi.StringWidth(redactSingleLineUI(m.toast.Message)) > maxInt(1, m.width-ansi.StringWidth(prefix))
}

func toastPrefix(level toastLevel) string {
	switch level {
	case toastSuccess:
		return "✓ "
	case toastWarning:
		return "⚠ "
	case toastError:
		return "✗ "
	default:
		return "● "
	}
}

func toastScrollCmd(id uint64) tea.Cmd {
	return tea.Tick(toastScrollInterval, func(time.Time) tea.Msg { return toastScrollMsg{id: id} })
}

// toastCircularWindow chooses a cell-bounded viewport from a repeated message
// and gap. The source text is already redacted and has no terminal escapes.
func toastCircularWindow(text string, width, offset int) (string, bool) {
	if width <= 0 {
		return "", false
	}
	if ansi.StringWidth(text) <= width {
		return text, false
	}
	cycle := []rune(text + toastScrollGap)
	if len(cycle) == 0 {
		return "", false
	}
	offset %= len(cycle)
	if offset < 0 {
		offset += len(cycle)
	}
	var output strings.Builder
	for index := 0; index < len(cycle); index++ {
		candidate := string(cycle[(offset+index)%len(cycle)])
		if ansi.StringWidth(output.String()+candidate) > width {
			break
		}
		output.WriteString(candidate)
	}
	if output.Len() == 0 {
		return ansi.Truncate(text, width, ""), true
	}
	return output.String(), true
}

func toastCycleLength(text string) int {
	return len([]rune(text + toastScrollGap))
}

func clampDuration(value, minimum, maximum time.Duration) time.Duration {
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}
