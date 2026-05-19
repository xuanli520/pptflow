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
	case "ctrl+right":
		m.switchPanel(1)
		return m, append(cmds, m.reloadDetail())
	case "ctrl+left":
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
	return "Tab/Shift+Tab 字段和按钮  空格切换  ↑↓ 选择备份  回车执行  Ctrl←/→ 顶层页  Esc返回"
}
