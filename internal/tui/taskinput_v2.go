package tui

import (
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// TaskSubmitMsg is emitted when the user submits a new task.
type TaskSubmitMsg struct {
	RepoURL   string
	CommitSHA string
	BaseImage string
	Slug      string
	Title     string
	Reason    string
}

// TaskInputModel collects the caller-selected immutable source coordinate and
// task identity and environment required by the Standard authoring lifecycle
// command.
type TaskInputModel struct {
	repoInput      textinput.Model
	commitInput    textinput.Model
	baseImageInput textinput.Model
	slugInput      textinput.Model
	titleInput     textinput.Model
	reasonInput    textinput.Model
	focusIndex     int
	visible        bool
	validationErr  string
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
	commit.CharLimit = 64
	commit.Width = 20

	baseImage := textinput.New()
	baseImage.Prompt = "Base image "
	baseImage.Placeholder = "registry/repository:tag@sha256:<64 hex>"
	baseImage.CharLimit = 512
	baseImage.Width = 64

	slug := textinput.New()
	slug.Prompt = "Slug "
	slug.Placeholder = "my-task"
	slug.CharLimit = 80
	slug.Width = 28

	title := textinput.New()
	title.Prompt = "Title "
	title.Placeholder = "Task title"
	title.CharLimit = 160
	title.Width = 44

	reason := textinput.New()
	reason.Prompt = "Reason "
	reason.Placeholder = "Why this task is being created"
	reason.CharLimit = 240
	reason.Width = 44

	return TaskInputModel{
		repoInput:      repo,
		commitInput:    commit,
		baseImageInput: baseImage,
		slugInput:      slug,
		titleInput:     title,
		reasonInput:    reason,
	}
}

func (m *TaskInputModel) Show() {
	m.visible = true
	m.validationErr = ""
	m.repoInput.Focus()
	m.focusIndex = 0
	m.commitInput.Blur()
	m.baseImageInput.Blur()
	m.slugInput.Blur()
	m.titleInput.Blur()
	m.reasonInput.Blur()
}

func (m *TaskInputModel) Hide() {
	m.visible = false
	m.repoInput.Blur()
	m.commitInput.Blur()
	m.baseImageInput.Blur()
	m.slugInput.Blur()
	m.titleInput.Blur()
	m.reasonInput.Blur()
}

func (m *TaskInputModel) Visible() bool {
	return m.visible
}

func (m *TaskInputModel) toggleFocus() {
	switch m.focusIndex {
	case 0:
		m.repoInput.Blur()
		m.commitInput.Focus()
		m.focusIndex = 1
	case 1:
		m.commitInput.Blur()
		m.baseImageInput.Focus()
		m.focusIndex = 2
	case 2:
		m.baseImageInput.Blur()
		m.slugInput.Focus()
		m.focusIndex = 3
	case 3:
		m.slugInput.Blur()
		m.titleInput.Focus()
		m.focusIndex = 4
	case 4:
		m.titleInput.Blur()
		m.reasonInput.Focus()
		m.focusIndex = 5
	default:
		m.reasonInput.Blur()
		m.repoInput.Focus()
		m.focusIndex = 0
	}
}

// Reset clears a successfully submitted form. A failed submission retains all
// inputs so retrying does not manufacture a second user command.
func (m *TaskInputModel) Reset() {
	m.repoInput.SetValue("")
	m.commitInput.SetValue("")
	m.baseImageInput.SetValue("")
	m.slugInput.SetValue("")
	m.titleInput.SetValue("")
	m.reasonInput.SetValue("")
	m.validationErr = ""
}

func (m *TaskInputModel) Update(msg tea.Msg) (tea.Cmd, bool) {
	if !m.visible {
		return nil, false
	}

	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		// Forward non-key messages to every input for cursor blink events.
		var repoCmd, commitCmd, baseImageCmd, slugCmd, titleCmd, reasonCmd tea.Cmd
		m.repoInput, repoCmd = m.repoInput.Update(msg)
		m.commitInput, commitCmd = m.commitInput.Update(msg)
		m.baseImageInput, baseImageCmd = m.baseImageInput.Update(msg)
		m.slugInput, slugCmd = m.slugInput.Update(msg)
		m.titleInput, titleCmd = m.titleInput.Update(msg)
		m.reasonInput, reasonCmd = m.reasonInput.Update(msg)
		return tea.Batch(repoCmd, commitCmd, baseImageCmd, slugCmd, titleCmd, reasonCmd), false
	}

	switch keyMsg.String() {
	case "enter":
		request := TaskSubmitMsg{
			RepoURL:   m.repoInput.Value(),
			CommitSHA: m.commitInput.Value(),
			BaseImage: m.baseImageInput.Value(),
			Slug:      m.slugInput.Value(),
			Title:     m.titleInput.Value(),
			Reason:    m.reasonInput.Value(),
		}
		if request.RepoURL != "" && request.CommitSHA != "" && request.BaseImage != "" && request.Slug != "" && request.Title != "" && request.Reason != "" {
			m.validationErr = ""
			return func() tea.Msg { return request }, true
		}
		m.validationErr = "URL, SHA, base image, slug, title, and reason are required"
		return nil, true

	case "tab":
		m.toggleFocus()
		return nil, true

	case "esc":
		m.Hide()
		return nil, true
	}

	// Forward to focused input
	var cmd tea.Cmd
	if m.focusIndex == 0 {
		m.repoInput, cmd = m.repoInput.Update(msg)
	} else if m.focusIndex == 1 {
		m.commitInput, cmd = m.commitInput.Update(msg)
	} else if m.focusIndex == 2 {
		m.baseImageInput, cmd = m.baseImageInput.Update(msg)
	} else if m.focusIndex == 3 {
		m.slugInput, cmd = m.slugInput.Update(msg)
	} else if m.focusIndex == 4 {
		m.titleInput, cmd = m.titleInput.Update(msg)
	} else {
		m.reasonInput, cmd = m.reasonInput.Update(msg)
	}
	return cmd, true
}

func (m TaskInputModel) View(width int) string {
	if !m.visible {
		return ""
	}
	content := m.repoInput.View() + "  " + m.commitInput.View() + "\n" +
		m.baseImageInput.View() + "\n" +
		m.slugInput.View() + "  " + m.titleInput.View() + "\n" +
		m.reasonInput.View()
	if m.validationErr != "" {
		content += "\n" + failStyleV2.Render(m.validationErr)
	}
	return inputStyle.Width(max(1, width)).Render(content)
}
