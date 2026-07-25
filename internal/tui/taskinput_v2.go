package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
)

// TaskSubmitMsg is emitted when the user submits a new task.
type TaskSubmitMsg struct {
	RepoURL     string
	CommitSHA   string
	BaseImage   string
	Slug        string
	Title       string
	TaskType    string
	Application string
	CodeLang    string
	Is0To1      bool
	Objective   string
	Reason      string
}

// TaskInputModel collects the caller-selected immutable source coordinate and
// task identity and environment required by the Standard authoring lifecycle
// command.
type TaskInputModel struct {
	repoInput        textinput.Model
	commitInput      textinput.Model
	baseImageInput   textinput.Model
	slugInput        textinput.Model
	titleInput       textinput.Model
	taskTypeInput    textinput.Model
	applicationInput textinput.Model
	codeLangInput    textinput.Model
	is0To1           bool
	objectiveInput   textinput.Model
	reasonInput      textinput.Model
	focusIndex       int
	visible          bool
	validationErr    string
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

	taskType := textinput.New()
	taskType.Prompt = "Task type "
	taskType.Placeholder = "feature"
	taskType.CharLimit = 64
	taskType.Width = 24

	application := textinput.New()
	application.Prompt = "Application "
	application.Placeholder = "backend"
	application.CharLimit = 64
	application.Width = 36

	codeLang := textinput.New()
	codeLang.Prompt = "Code language "
	codeLang.Placeholder = "rust"
	codeLang.CharLimit = 64
	codeLang.Width = 24

	objective := textinput.New()
	objective.Prompt = "Objective "
	objective.Placeholder = "Add the requested bounded behavior"
	objective.CharLimit = 512
	objective.Width = 64

	reason := textinput.New()
	reason.Prompt = "Reason "
	reason.Placeholder = "Why this task is being created"
	reason.CharLimit = 240
	reason.Width = 44

	return TaskInputModel{
		repoInput:        repo,
		commitInput:      commit,
		baseImageInput:   baseImage,
		slugInput:        slug,
		titleInput:       title,
		taskTypeInput:    taskType,
		applicationInput: application,
		codeLangInput:    codeLang,
		objectiveInput:   objective,
		reasonInput:      reason,
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
	m.taskTypeInput.Blur()
	m.applicationInput.Blur()
	m.codeLangInput.Blur()
	m.objectiveInput.Blur()
	m.reasonInput.Blur()
}

func (m *TaskInputModel) Hide() {
	m.visible = false
	m.repoInput.Blur()
	m.commitInput.Blur()
	m.baseImageInput.Blur()
	m.slugInput.Blur()
	m.titleInput.Blur()
	m.taskTypeInput.Blur()
	m.applicationInput.Blur()
	m.codeLangInput.Blur()
	m.objectiveInput.Blur()
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
		m.taskTypeInput.Focus()
		m.focusIndex = 5
	case 5:
		m.taskTypeInput.Blur()
		m.applicationInput.Focus()
		m.focusIndex = 6
	case 6:
		m.applicationInput.Blur()
		m.codeLangInput.Focus()
		m.focusIndex = 7
	case 7:
		m.codeLangInput.Blur()
		m.focusIndex = 8
	case 8:
		m.objectiveInput.Focus()
		m.focusIndex = 9
	case 9:
		m.objectiveInput.Blur()
		m.reasonInput.Focus()
		m.focusIndex = 10
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
	m.taskTypeInput.SetValue("")
	m.applicationInput.SetValue("")
	m.codeLangInput.SetValue("")
	m.is0To1 = false
	m.objectiveInput.SetValue("")
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
		var repoCmd, commitCmd, baseImageCmd, slugCmd, titleCmd, taskTypeCmd, applicationCmd, codeLangCmd, objectiveCmd, reasonCmd tea.Cmd
		m.repoInput, repoCmd = m.repoInput.Update(msg)
		m.commitInput, commitCmd = m.commitInput.Update(msg)
		m.baseImageInput, baseImageCmd = m.baseImageInput.Update(msg)
		m.slugInput, slugCmd = m.slugInput.Update(msg)
		m.titleInput, titleCmd = m.titleInput.Update(msg)
		m.taskTypeInput, taskTypeCmd = m.taskTypeInput.Update(msg)
		m.applicationInput, applicationCmd = m.applicationInput.Update(msg)
		m.codeLangInput, codeLangCmd = m.codeLangInput.Update(msg)
		m.objectiveInput, objectiveCmd = m.objectiveInput.Update(msg)
		m.reasonInput, reasonCmd = m.reasonInput.Update(msg)
		return tea.Batch(repoCmd, commitCmd, baseImageCmd, slugCmd, titleCmd, taskTypeCmd, applicationCmd, codeLangCmd, objectiveCmd, reasonCmd), false
	}
	if keyMsg.String() == " " && m.focusIndex == 8 {
		m.is0To1 = !m.is0To1
		return nil, true
	}

	switch keyMsg.String() {
	case "enter":
		request := TaskSubmitMsg{
			RepoURL:     strings.TrimSpace(m.repoInput.Value()),
			CommitSHA:   strings.TrimSpace(m.commitInput.Value()),
			BaseImage:   strings.TrimSpace(m.baseImageInput.Value()),
			Slug:        strings.TrimSpace(m.slugInput.Value()),
			Title:       strings.TrimSpace(m.titleInput.Value()),
			TaskType:    strings.TrimSpace(m.taskTypeInput.Value()),
			Application: strings.TrimSpace(m.applicationInput.Value()),
			CodeLang:    strings.TrimSpace(m.codeLangInput.Value()),
			Is0To1:      m.is0To1,
			Objective:   strings.TrimSpace(m.objectiveInput.Value()),
			Reason:      strings.TrimSpace(m.reasonInput.Value()),
		}
		if request.RepoURL != "" && request.CommitSHA != "" && request.BaseImage != "" && request.Slug != "" && request.Title != "" && request.TaskType != "" && request.Application != "" && request.CodeLang != "" && request.Objective != "" && request.Reason != "" {
			if len(request.Objective) > workflowadapter.AuthoringContractObjectiveMaxBytes {
				m.validationErr = "Objective must be at most 512 UTF-8 bytes"
				return nil, true
			}
			m.validationErr = ""
			return func() tea.Msg { return request }, true
		}
		m.validationErr = "URL, SHA, base image, slug, title, task type, application, code language, objective, and reason are required"
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
	} else if m.focusIndex == 5 {
		m.taskTypeInput, cmd = m.taskTypeInput.Update(msg)
	} else if m.focusIndex == 6 {
		m.applicationInput, cmd = m.applicationInput.Update(msg)
	} else if m.focusIndex == 7 {
		m.codeLangInput, cmd = m.codeLangInput.Update(msg)
	} else if m.focusIndex == 8 {
		return nil, true
	} else if m.focusIndex == 9 {
		m.objectiveInput, cmd = m.objectiveInput.Update(msg)
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
		m.taskTypeInput.View() + "  " + m.applicationInput.View() + "\n" +
		m.codeLangInput.View() + "  0-to-1 [" + map[bool]string{true: "x", false: " "}[m.is0To1] + "]" + "\n" +
		m.objectiveInput.View() + "\n" +
		m.reasonInput.View()
	if m.validationErr != "" {
		content += "\n" + failStyleV2.Render(m.validationErr)
	}
	return inputStyle.Width(max(1, width-inputStyle.GetHorizontalFrameSize())).Render(content)
}
