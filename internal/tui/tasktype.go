package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xuanli520/p2r_tui/internal/config"
)

type taskTypePrompt struct {
	taskID string
	index  int
}

type taskTypeOption struct {
	value string
	label string
}

func taskTypeOptions() []taskTypeOption {
	return []taskTypeOption{
		{value: config.ProjectTypeFullstack, label: "全栈"},
		{value: config.ProjectTypePureBackend, label: "纯后端"},
		{value: config.ProjectTypePureFrontend, label: "纯前端"},
	}
}

func taskTypeLabel(value string) string {
	normalized := config.NormalizeProjectType(value)
	for _, option := range taskTypeOptions() {
		if option.value == normalized {
			return option.label
		}
	}
	return ""
}

func (m *app) openTaskTypePrompt(taskID string) {
	m.taskTypePrompt = taskTypePrompt{taskID: strings.TrimSpace(taskID)}
	m.message = "请选择题型 " + m.taskTypePrompt.taskID
}

func (m app) handleTaskTypePromptKey(key string, cmds []tea.Cmd) (app, []tea.Cmd) {
	options := taskTypeOptions()
	switch key {
	case "left", "up":
		m.taskTypePrompt.index = clamp(m.taskTypePrompt.index-1, 0, len(options)-1)
	case "right", "down", "tab":
		m.taskTypePrompt.index = clamp(m.taskTypePrompt.index+1, 0, len(options)-1)
	case "1":
		m.taskTypePrompt.index = 0
	case "2":
		m.taskTypePrompt.index = 1
	case "3":
		m.taskTypePrompt.index = 2
	case "enter", " ":
		taskID := m.taskTypePrompt.taskID
		projectType := options[clamp(m.taskTypePrompt.index, 0, len(options)-1)].value
		m.taskTypePrompt = taskTypePrompt{}
		m.openRunConfigForTaskWithProjectType(taskID, runConfigActionInspection, projectType)
	case "esc", "q":
		m.taskTypePrompt = taskTypePrompt{}
		m.message = "已取消新题质检"
	default:
		return m, cmds
	}
	return m, cmds
}

func renderTaskTypePrompt(m app, width int) string {
	options := taskTypeOptions()
	prefix := "确认题型 " + m.taskTypePrompt.taskID + ":"
	rawParts := []string{prefix}
	for index, option := range options {
		rawParts = append(rawParts, fmt.Sprintf("[%d %s]", index+1, option.label))
	}
	raw := strings.Join(rawParts, " ")
	if lipglossWidth(raw) > width {
		return truncateDisplay(raw, width)
	}
	parts := []string{prefix}
	for index, option := range options {
		label := fmt.Sprintf("%d %s", index+1, option.label)
		if index == m.taskTypePrompt.index {
			label = selectedStyle.Render("[" + label + "]")
		} else {
			label = mutedStyle.Render("[" + label + "]")
		}
		parts = append(parts, label)
	}
	return strings.Join(parts, " ")
}
