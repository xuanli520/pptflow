package tui

import (
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// TaskSubmitMsg is emitted when the user submits a new task.
type TaskSubmitMsg struct {
	RepoURL   string
	CommitSHA string
}

// TaskInputModel handles the repo URL and commit SHA input bar.
type TaskInputModel struct {
	repoInput   textinput.Model
	commitInput textinput.Model
	focusIndex  int // 0=repo, 1=commit
	visible     bool
}

func NewTaskInputModel() TaskInputModel {
	repo := textinput.New()
	repo.Prompt = "URL "
	repo.Placeholder = "https://github.com/owner/repo"
	repo.CharLimit = 256
	repo.Width = 50

	commit := textinput.New()
	commit.Prompt = "SHA "
	commit.Placeholder = "abc1234..."
	commit.CharLimit = 40
	commit.Width = 20

	repo.Focus()
	return TaskInputModel{
		repoInput:   repo,
		commitInput: commit,
		focusIndex:  0,
		visible:     true,
	}
}

func (m *TaskInputModel) Show() {
	m.visible = true
	m.repoInput.Focus()
	m.focusIndex = 0
	m.commitInput.Blur()
}

func (m *TaskInputModel) Hide() {
	m.visible = false
	m.repoInput.Blur()
	m.commitInput.Blur()
}

func (m *TaskInputModel) Focused() bool {
	return m.visible && (m.repoInput.Focused() || m.commitInput.Focused())
}

func (m *TaskInputModel) toggleFocus() {
	if m.focusIndex == 0 {
		m.repoInput.Blur()
		m.commitInput.Focus()
		m.focusIndex = 1
	} else {
		m.commitInput.Blur()
		m.repoInput.Focus()
		m.focusIndex = 0
	}
}

func (m *TaskInputModel) Update(msg tea.Msg) (tea.Cmd, bool) {
	if !m.visible {
		return nil, false
	}

	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		// Forward non-key messages to both inputs for cursor blink etc.
		var repoCmd, commitCmd tea.Cmd
		m.repoInput, repoCmd = m.repoInput.Update(msg)
		m.commitInput, commitCmd = m.commitInput.Update(msg)
		return tea.Batch(repoCmd, commitCmd), false
	}

	switch keyMsg.String() {
	case "enter":
		repoURL := m.repoInput.Value()
		commitSHA := m.commitInput.Value()
		if repoURL != "" && commitSHA != "" {
			m.repoInput.SetValue("")
			m.commitInput.SetValue("")
			return func() tea.Msg { return TaskSubmitMsg{RepoURL: repoURL, CommitSHA: commitSHA} }, true
		}
		return nil, false

	case "tab":
		m.toggleFocus()
		return nil, false

	case "esc":
		m.Hide()
		return nil, false
	}

	// Forward to focused input
	var cmd tea.Cmd
	if m.focusIndex == 0 {
		m.repoInput, cmd = m.repoInput.Update(msg)
	} else {
		m.commitInput, cmd = m.commitInput.Update(msg)
	}
	return cmd, false
}

func (m TaskInputModel) View(width int) string {
	if !m.visible {
		return ""
	}
	return inputStyle.Width(width).Render(m.repoInput.View() + "  " + m.commitInput.View())
}
