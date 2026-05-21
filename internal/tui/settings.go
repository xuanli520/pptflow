package tui

import (
	"strings"
)

type settingsItem int

const (
	settingsItemDocker settingsItem = iota
)

type settingsPanel struct {
	selected settingsItem
}

// settingsPanel keeps the Settings overlay's minimal navigation state.
// Full-screen settings tabs were removed; rendering is delegated to
// settings_overlay.go.
func newSettingsPanel() settingsPanel {
	return settingsPanel{selected: settingsItemDocker}
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
