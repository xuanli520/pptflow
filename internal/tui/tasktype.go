package tui

import (
	"fmt"
	"net/url"
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

func nextProjectType(current string) string {
	options := taskTypeOptions()
	normalized := config.NormalizeProjectType(current)
	for index, option := range options {
		if option.value == normalized {
			return options[(index+1)%len(options)].value
		}
	}
	return options[0].value
}

func nextExistingTaskProjectTypeReset(current string) string {
	options := taskTypeOptions()
	normalized := config.NormalizeProjectType(current)
	if normalized == "" {
		return options[0].value
	}
	for index, option := range options {
		if option.value == normalized {
			if index == len(options)-1 {
				return ""
			}
			return options[index+1].value
		}
	}
	return ""
}

func runConfigProjectTypeText(c runConfig) string {
	if c.existingTask {
		if c.projectType == "" {
			if label := taskTypeLabel(c.currentType); label != "" {
				return "重置题型: 保持当前 (" + label + ")"
			}
			return "重置题型: 保持当前"
		}
		return "重置题型: " + taskTypeLabel(c.projectType)
	}
	label := taskTypeLabel(c.projectType)
	if label == "" {
		label = taskTypeLabel(config.ProjectTypeFullstack)
	}
	return "题型: " + label
}

func taskTypeFromGitURL(git config.GitConfig, gitURL string) string {
	target := normalizedGitURLPrefix(gitURL)
	if target == "" {
		return ""
	}
	for _, projectType := range config.ProjectTypes() {
		base, err := config.GitBaseURLForProjectType(git, projectType)
		if err != nil {
			continue
		}
		base = normalizedGitURLPrefix(base)
		if base != "" && (target == base || strings.HasPrefix(target, base+"/")) {
			return projectType
		}
	}
	return taskTypeFromGitURLPath(gitURL)
}

func normalizedGitURLPrefix(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimRight(value, "/")
	value = strings.TrimSuffix(value, ".git")
	value = strings.TrimRight(value, "/")
	return value
}

func taskTypeFromGitURLPath(gitURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(gitURL))
	if err != nil {
		return ""
	}
	path := strings.Trim(parsed.Path, "/")
	path = strings.TrimSuffix(path, ".git")
	path = strings.Trim(path, "/")
	if path == "" {
		return ""
	}
	segments := strings.Split(path, "/")
	if len(segments) < 2 {
		return ""
	}
	switch strings.ToLower(segments[len(segments)-2]) {
	case "fullstack":
		return config.ProjectTypeFullstack
	case "server":
		return config.ProjectTypePureBackend
	case "web":
		return config.ProjectTypePureFrontend
	default:
		return ""
	}
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
