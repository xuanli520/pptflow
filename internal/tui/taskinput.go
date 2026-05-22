package tui

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const maxTaskIDLength = 64

var taskIDPattern = regexp.MustCompile(`^TASK-\d{8}-[A-F0-9]{6}$`)

type TaskInputSubmitMsg struct {
	TaskID string
}

type TaskInputModel struct {
	input textinput.Model
	err   string
}

func newTaskInputModel() TaskInputModel {
	input := textinput.New()
	input.Prompt = "输入 TASK ID: "
	input.Placeholder = "TASK-YYYYMMDD-XXXXXX"
	input.CharLimit = maxTaskIDLength
	input.Width = 34
	return TaskInputModel{input: input}
}

func ValidateTaskID(raw string) (string, error) {
	cleaned := strings.TrimSpace(raw)
	if len(cleaned) > maxTaskIDLength {
		return "", fmt.Errorf("TASK ID exceeds max length")
	}
	if !taskIDPattern.MatchString(cleaned) {
		return "", fmt.Errorf("invalid TASK ID format, expected TASK-YYYYMMDD-XXXXXX")
	}
	return cleaned, nil
}

func (m *TaskInputModel) Focus() {
	m.input.Focus()
}

func (m *TaskInputModel) Blur() {
	m.input.Blur()
}

func (m TaskInputModel) Focused() bool {
	return m.input.Focused()
}

func (m TaskInputModel) Value() string {
	return m.input.Value()
}

func (m *TaskInputModel) SetError(err string) {
	m.err = err
}

func (m *TaskInputModel) Clear() {
	m.input.SetValue("")
	m.err = ""
}

func (m *TaskInputModel) SetWidth(width int) {
	m.input.Width = max(18, min(width, 48))
}

func (m *TaskInputModel) Update(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "enter":
		taskID, err := ValidateTaskID(m.input.Value())
		if err != nil {
			m.err = err.Error()
			return nil
		}
		m.err = ""
		m.input.SetValue("")
		return func() tea.Msg { return TaskInputSubmitMsg{TaskID: taskID} }
	case "esc":
		m.Clear()
		m.Blur()
		return nil
	default:
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		if len(m.input.Value()) > maxTaskIDLength {
			m.input.SetValue(m.input.Value()[:maxTaskIDLength])
		}
		m.err = ""
		return cmd
	}
}

func (m TaskInputModel) View(width int) string {
	view := m.input.View()
	if m.err != "" {
		return view + "  " + errorStyle.Render(truncateDisplay(m.err, max(8, width-lipglossWidth(view)-4)))
	}
	return view
}

func lipglossWidth(value string) int {
	return lipgloss.Width(value)
}
