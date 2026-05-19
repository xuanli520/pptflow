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
	case "tab":
		m.switchPanel(1)
		return m, append(cmds, m.reloadDetail())
	case "shift+tab":
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
	prefix := "  "
	if m.settings.selected == settingsItemDocker {
		prefix = "> "
	}
	return strings.Join([]string{
		"设置",
		prefix + "Docker",
	}, "\n")
}

func settingsFooter(m app) string {
	if m.settings.selected == settingsItemDocker && m.dockerMirror.confirm != "" {
		return "y/Enter 确认  n/Esc 取消"
	}
	return "Tab 顶层页  ↑↓ 设置项  Space 切换  r刷新  s保存  a应用  b恢复  Enter动作  Esc返回"
}
