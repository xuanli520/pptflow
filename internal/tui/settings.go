package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

type settingsItem int

const (
	settingsItemDocker settingsItem = iota
)

type settingsPanel struct {
	selected settingsItem
}

func newSettingsPanel() settingsPanel {
	return settingsPanel{selected: settingsItemDocker}
}

func (m app) handleSettingsKey(msg tea.KeyMsg) (app, []tea.Cmd) {
	key := msg.String()
	var cmds []tea.Cmd
	switch key {
	case "ctrl+c", "ctrl+q":
		return m, []tea.Cmd{tea.Batch(m.shutdownScheduler(), tea.Quit)}
	}
	if m.settings.selected == settingsItemDocker && m.dockerMirror.confirm != "" {
		return m.handleDockerSettingsKey(msg)
	}
	switch key {
	case "tab", "ctrl+right":
		m.saveSettingsInput()
		m.switchPanel(1)
		return m, append(cmds, m.reloadDetail())
	case "shift+tab", "ctrl+left":
		m.saveSettingsInput()
		m.switchPanel(-1)
		return m, append(cmds, m.reloadDetail())
	}
	switch m.settings.selected {
	case settingsItemDocker:
		return m.handleDockerSettingsKey(msg)
	default:
		return m, cmds
	}
}

func (m *app) saveSettingsInput() {
	switch m.settings.selected {
	case settingsItemDocker:
		m.dockerMirror.saveInputToFocus()
	}
}

func renderSettings(m app) string {
	var sections []string
	sections = append(sections, renderSettingsNav(m))
	switch m.settings.selected {
	case settingsItemDocker:
		sections = append(sections, renderDockerSettings(m))
	}
	return strings.Join(sections, "\n\n")
}

func renderSettingsNav(m app) string {
	item := "  Docker 镜像源"
	if m.settings.selected == settingsItemDocker {
		item = selectedStyle.Render("> Docker 镜像源")
	}
	return strings.Join([]string{
		titleStyle.Render("设置"),
		item,
	}, "\n")
}

func settingsFooter(m app) string {
	if m.settings.selected == settingsItemDocker && m.dockerMirror.confirm != "" {
		return "y/回车 确认  n/Esc 取消"
	}
	return "Tab/Shift+Tab 顶层页  ↑↓ 字段和按钮  Space 开关  Enter 执行  PgUp/PgDn 备份  Esc 返回"
}
