package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/xuanli520/p2r_tui/internal/config"
	dockermgr "github.com/xuanli520/p2r_tui/internal/docker"
	"github.com/xuanli520/p2r_tui/internal/executor"
	"github.com/xuanli520/p2r_tui/internal/maintenance"
)

type dockerMirrorFocus int

const (
	dockerMirrorFocusEnabled dockerMirrorFocus = iota
	dockerMirrorFocusDaemonJSON
	dockerMirrorFocusBackupDir
	dockerMirrorFocusMirrors
	dockerMirrorFocusManualApply
	dockerMirrorFocusActions
)

type dockerMirrorPanel struct {
	focus       dockerMirrorFocus
	actionIndex int
	values      config.DockerDaemonMirrorsConfig
	input       textinput.Model
	status      dockermgr.DaemonMirrorSummary
	statusText  string
	confirm     string
	backup      string
	backups     []string
	backupIndex int
}

type dockerMirrorMsg struct {
	operation string
	summary   dockermgr.DaemonMirrorSummary
	err       error
}

type dockerGCMsg struct {
	summary dockermgr.GCSummary
	err     error
}

func newDockerMirrorPanel(cfg config.Config) dockerMirrorPanel {
	input := textinput.New()
	input.Prompt = ""
	input.Width = 72
	panel := dockerMirrorPanel{values: cfg.Docker.DaemonMirrors, input: input}
	panel.refreshBackups()
	panel.syncInputFromFocus()
	return panel
}

func (p *dockerMirrorPanel) syncInputFromFocus() {
	switch p.focus {
	case dockerMirrorFocusDaemonJSON:
		p.input.SetValue(p.values.DaemonJSON)
		p.input.Focus()
	case dockerMirrorFocusBackupDir:
		p.input.SetValue(p.values.BackupDir)
		p.input.Focus()
	case dockerMirrorFocusMirrors:
		p.input.SetValue(strings.Join(p.values.RegistryMirrors, ","))
		p.input.Focus()
	default:
		p.input.Blur()
	}
}

func (p *dockerMirrorPanel) saveInputToFocus() {
	value := strings.TrimSpace(p.input.Value())
	switch p.focus {
	case dockerMirrorFocusDaemonJSON:
		p.values.DaemonJSON = value
	case dockerMirrorFocusBackupDir:
		p.values.BackupDir = value
		p.refreshBackups()
	case dockerMirrorFocusMirrors:
		var mirrors []string
		for _, item := range strings.Split(value, ",") {
			if item = strings.TrimSpace(item); item != "" {
				mirrors = append(mirrors, item)
			}
		}
		p.values.RegistryMirrors = mirrors
	}
}

func (p *dockerMirrorPanel) refreshBackups() {
	entries, err := os.ReadDir(strings.TrimSpace(p.values.BackupDir))
	if err != nil {
		p.backups = nil
		p.backupIndex = 0
		return
	}
	var backups []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}
		backups = append(backups, filepath.Join(p.values.BackupDir, entry.Name()))
	}
	sort.Sort(sort.Reverse(sort.StringSlice(backups)))
	p.backups = backups
	if p.backupIndex >= len(p.backups) {
		p.backupIndex = max(0, len(p.backups)-1)
	}
}

func (p dockerMirrorPanel) selectedBackup() string {
	if strings.TrimSpace(p.backup) != "" {
		return strings.TrimSpace(p.backup)
	}
	if len(p.backups) == 0 {
		return ""
	}
	return p.backups[max(0, min(p.backupIndex, len(p.backups)-1))]
}

func (m app) handleDockerSettingsKey(msg tea.KeyMsg) (app, []tea.Cmd) {
	key := msg.String()
	var cmds []tea.Cmd
	if m.dockerMirror.confirm != "" {
		switch key {
		case "y", "Y", "enter":
			action := m.dockerMirror.confirm
			m.dockerMirror.confirm = ""
			if action == "apply" {
				m.message = "正在写入 Docker daemon mirror 配置..."
				return m, append(cmds, dockerMirrorApplyCmd(m.cfg, m.dockerMirror.values))
			}
			if action == "restore" {
				m.message = "正在恢复 Docker daemon 配置..."
				return m, append(cmds, dockerMirrorRestoreCmd(m.cfg, m.dockerMirror.values, m.dockerMirror.selectedBackup()))
			}
		case "n", "N", "esc":
			m.dockerMirror.confirm = ""
			m.message = "已取消 Docker mirror 操作"
		}
		return m, cmds
	}
	if m.dockerMirror.focus == dockerMirrorFocusDaemonJSON || m.dockerMirror.focus == dockerMirrorFocusBackupDir || m.dockerMirror.focus == dockerMirrorFocusMirrors {
		switch key {
		case "tab", "shift+tab", "enter", "esc", "ctrl+s":
		default:
			var cmd tea.Cmd
			m.dockerMirror.input, cmd = m.dockerMirror.input.Update(msg)
			return m, append(cmds, cmd)
		}
	}
	switch key {
	case "tab":
		m.dockerMirror.saveInputToFocus()
		m.dockerMirror.focus = (m.dockerMirror.focus + 1) % 6
		m.dockerMirror.syncInputFromFocus()
	case "shift+tab":
		m.dockerMirror.saveInputToFocus()
		m.dockerMirror.focus = (m.dockerMirror.focus + 5) % 6
		m.dockerMirror.syncInputFromFocus()
	case " ":
		switch m.dockerMirror.focus {
		case dockerMirrorFocusEnabled:
			m.dockerMirror.values.Enabled = !m.dockerMirror.values.Enabled
		case dockerMirrorFocusManualApply:
			m.dockerMirror.values.RequireManualApply = !m.dockerMirror.values.RequireManualApply
		}
	case "left":
		if m.dockerMirror.focus == dockerMirrorFocusActions {
			m.dockerMirror.actionIndex = max(0, m.dockerMirror.actionIndex-1)
		}
	case "right":
		if m.dockerMirror.focus == dockerMirrorFocusActions {
			m.dockerMirror.actionIndex = min(3, m.dockerMirror.actionIndex+1)
		}
	case "up":
		if m.dockerMirror.focus == dockerMirrorFocusActions && m.dockerMirror.actionIndex == 3 {
			m.dockerMirror.backupIndex = max(0, m.dockerMirror.backupIndex-1)
		}
	case "down":
		if m.dockerMirror.focus == dockerMirrorFocusActions && m.dockerMirror.actionIndex == 3 && len(m.dockerMirror.backups) > 0 {
			m.dockerMirror.backupIndex = min(len(m.dockerMirror.backups)-1, m.dockerMirror.backupIndex+1)
		}
	case "ctrl+s", "s":
		m.dockerMirror.saveInputToFocus()
		if err := config.SaveProjectDaemonMirrors(m.cfg.ProjectConfigPath, m.dockerMirror.values); err != nil {
			m.message = "保存 Docker mirror 配置失败: " + err.Error()
		} else {
			m.message = "已保存 Docker mirror 配置到 " + m.cfg.ProjectConfigPath
			m.cfg.Docker.DaemonMirrors = m.dockerMirror.values
		}
	case "r":
		m.dockerMirror.saveInputToFocus()
		return m, append(cmds, dockerMirrorStatusCmd(m.cfg, m.dockerMirror.values))
	case "a":
		m.dockerMirror.saveInputToFocus()
		m.dockerMirror.confirm = "apply"
	case "b":
		m.dockerMirror.saveInputToFocus()
		m.dockerMirror.refreshBackups()
		m.dockerMirror.backup = m.dockerMirror.selectedBackup()
		m.dockerMirror.confirm = "restore"
	case "enter":
		if m.dockerMirror.focus == dockerMirrorFocusActions {
			switch m.dockerMirror.actionIndex {
			case 0:
				return m, append(cmds, dockerMirrorStatusCmd(m.cfg, m.dockerMirror.values))
			case 1:
				if err := config.SaveProjectDaemonMirrors(m.cfg.ProjectConfigPath, m.dockerMirror.values); err != nil {
					m.message = "保存 Docker mirror 配置失败: " + err.Error()
				} else {
					m.message = "已保存 Docker mirror 配置到 " + m.cfg.ProjectConfigPath
					m.cfg.Docker.DaemonMirrors = m.dockerMirror.values
				}
			case 2:
				m.dockerMirror.confirm = "apply"
			case 3:
				m.dockerMirror.refreshBackups()
				m.dockerMirror.backup = m.dockerMirror.selectedBackup()
				m.dockerMirror.confirm = "restore"
			}
		}
	case "esc":
		m.switchPanel(-1)
	}
	return m, cmds
}

func dockerMirrorStatusCmd(cfg config.Config, values config.DockerDaemonMirrorsConfig) tea.Cmd {
	cfg.Docker.DaemonMirrors = values
	return func() tea.Msg {
		return dockerMirrorMsg{operation: "status", summary: dockermgr.DaemonMirrorStatus(cfg)}
	}
}

func dockerMirrorApplyCmd(cfg config.Config, values config.DockerDaemonMirrorsConfig) tea.Cmd {
	cfg.Docker.DaemonMirrors = values
	return func() tea.Msg {
		summary, err := dockermgr.ApplyDaemonMirrors(cfg, true)
		return dockerMirrorMsg{operation: "apply", summary: summary, err: err}
	}
}

func dockerMirrorRestoreCmd(cfg config.Config, values config.DockerDaemonMirrorsConfig, backup string) tea.Cmd {
	cfg.Docker.DaemonMirrors = values
	return func() tea.Msg {
		summary, err := dockermgr.RestoreDaemonMirrors(cfg, backup, true)
		return dockerMirrorMsg{operation: "restore", summary: summary, err: err}
	}
}

func dockerStartupGCCmd(cfg config.Config, activeJobs int) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		summary, err := maintenance.TryRunOnTUIStart(ctx, cfg, executor.New(), activeJobs)
		return dockerGCMsg{summary: summary, err: err}
	}
}

func renderDockerSettings(m app) string {
	p := m.dockerMirror
	focusMark := func(f dockerMirrorFocus) string {
		if p.focus == f {
			return "> "
		}
		return "  "
	}
	checked := func(value bool) string {
		if value {
			return "[x]"
		}
		return "[ ]"
	}
	lines := []string{
		"Docker",
		"",
		"Desired config",
		focusMark(dockerMirrorFocusEnabled) + "Enabled: " + checked(p.values.Enabled),
		focusMark(dockerMirrorFocusDaemonJSON) + "daemon.json: " + dockerMirrorField(p, dockerMirrorFocusDaemonJSON, p.values.DaemonJSON),
		focusMark(dockerMirrorFocusBackupDir) + "backup dir: " + dockerMirrorField(p, dockerMirrorFocusBackupDir, p.values.BackupDir),
		focusMark(dockerMirrorFocusMirrors) + "mirrors: " + dockerMirrorField(p, dockerMirrorFocusMirrors, strings.Join(p.values.RegistryMirrors, ", ")),
		focusMark(dockerMirrorFocusManualApply) + "Require manual apply: " + checked(p.values.RequireManualApply),
		"  backups: " + dockerMirrorBackupLine(p),
		"",
		"Current daemon",
		"  daemon.json: " + p.values.DaemonJSON,
		"  status: " + dockerMirrorStatusLine(p.status),
		"  current mirrors: " + strings.Join(p.status.CurrentRegistryMirrors, ", "),
		"  desired mirrors: " + strings.Join(p.values.RegistryMirrors, ", "),
		"",
		"Actions",
		"  " + renderDockerMirrorActions(p),
	}
	if p.confirm != "" {
		lines = append(lines, "", "Confirm "+p.confirm+": daemon="+p.values.DaemonJSON+" backup="+p.selectedBackup(), "backup dir="+p.values.BackupDir, "Docker restart required after daemon changes. y/Enter confirm, n/Esc cancel")
	}
	if p.status.Error != "" {
		lines = append(lines, "", "Error: "+p.status.Error)
	}
	return strings.Join(lines, "\n")
}

func dockerMirrorBackupLine(p dockerMirrorPanel) string {
	if len(p.backups) == 0 {
		return "none"
	}
	return fmt.Sprintf("%d found, selected %s", len(p.backups), p.backups[max(0, min(p.backupIndex, len(p.backups)-1))])
}

func dockerMirrorField(p dockerMirrorPanel, focus dockerMirrorFocus, fallback string) string {
	if p.focus == focus {
		return p.input.View()
	}
	return fallback
}

func renderDockerMirrorActions(p dockerMirrorPanel) string {
	actions := []string{"Refresh Status", "Save Config", "Apply to daemon", "Restore Backup"}
	for index, action := range actions {
		if p.focus == dockerMirrorFocusActions && p.actionIndex == index {
			actions[index] = selectedStyle.Render(action)
		}
	}
	return strings.Join(actions, "  ")
}

func dockerMirrorStatusLine(summary dockermgr.DaemonMirrorSummary) string {
	if summary.DaemonJSON == "" {
		return "not loaded"
	}
	if !summary.OK {
		return "error"
	}
	if summary.Changed {
		return "drift"
	}
	return "consistent"
}

func (m *app) handleDockerMirrorMsg(msg tea.Msg, cmds *[]tea.Cmd) bool {
	switch value := msg.(type) {
	case dockerMirrorMsg:
		m.dockerMirror.status = value.summary
		if value.err != nil {
			m.message = fmt.Sprintf("Docker mirror %s 失败: %s", value.operation, value.err)
		} else {
			m.message = fmt.Sprintf("Docker mirror %s 完成", value.operation)
		}
		return true
	case dockerGCMsg:
		if value.err != nil {
			m.message = "Docker GC 失败: " + value.err.Error()
		} else if value.summary.Skipped {
			m.message = "Docker GC 已跳过: " + value.summary.SkipReason
		}
		return true
	default:
		return false
	}
}
