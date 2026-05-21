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
	dockerMirrorFocusCount
)

const dockerMirrorActionCount = 4

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

func (p *dockerMirrorPanel) moveFocus(delta int) {
	p.saveInputToFocus()
	if p.focus == dockerMirrorFocusActions {
		nextAction := p.actionIndex + delta
		if nextAction >= 0 && nextAction < dockerMirrorActionCount {
			p.actionIndex = nextAction
			return
		}
	}
	next := (int(p.focus) + delta) % int(dockerMirrorFocusCount)
	if next < 0 {
		next += int(dockerMirrorFocusCount)
	}
	p.focus = dockerMirrorFocus(next)
	if p.focus == dockerMirrorFocusActions {
		if delta < 0 {
			p.actionIndex = dockerMirrorActionCount - 1
		} else if p.actionIndex >= dockerMirrorActionCount {
			p.actionIndex = 0
		}
	}
	p.syncInputFromFocus()
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

func (m app) requestDockerMirrorApply(cmds []tea.Cmd) (app, []tea.Cmd) {
	m.dockerMirror.saveInputToFocus()
	if !m.dockerMirror.values.Enabled {
		m.message = "请先启用 Docker 镜像源"
		return m, cmds
	}
	if m.dockerMirror.values.RequireManualApply {
		m.message = "正在生成 Docker 镜像源手动应用文件..."
		return m, append(cmds, dockerMirrorApplyCmd(m.cfg, m.dockerMirror.values))
	}
	m.dockerMirror.confirm = "apply"
	return m, cmds
}

func (m app) requestDockerMirrorRestore(cmds []tea.Cmd) (app, []tea.Cmd) {
	m.dockerMirror.saveInputToFocus()
	m.dockerMirror.refreshBackups()
	m.dockerMirror.backup = m.dockerMirror.selectedBackup()
	if m.dockerMirror.backup == "" {
		m.message = "没有可恢复的 Docker 备份"
		return m, cmds
	}
	if m.dockerMirror.values.RequireManualApply {
		m.message = "正在生成 Docker 配置手动恢复文件..."
		return m, append(cmds, dockerMirrorRestoreCmd(m.cfg, m.dockerMirror.values, m.dockerMirror.backup))
	}
	m.dockerMirror.confirm = "restore"
	return m, cmds
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
				m.message = "正在写入 Docker 守护进程镜像源配置..."
				return m, append(cmds, dockerMirrorApplyCmd(m.cfg, m.dockerMirror.values))
			}
			if action == "restore" {
				m.message = "正在恢复 Docker 守护进程配置..."
				return m, append(cmds, dockerMirrorRestoreCmd(m.cfg, m.dockerMirror.values, m.dockerMirror.selectedBackup()))
			}
		case "n", "N", "esc":
			m.dockerMirror.confirm = ""
			m.message = "已取消 Docker 镜像源操作"
		}
		return m, cmds
	}
	if m.dockerMirror.focus == dockerMirrorFocusDaemonJSON || m.dockerMirror.focus == dockerMirrorFocusBackupDir || m.dockerMirror.focus == dockerMirrorFocusMirrors {
		switch key {
		case "tab", "shift+tab", "enter", "esc", "ctrl+s", "up", "down", "pgup", "pgdown":
		default:
			var cmd tea.Cmd
			m.dockerMirror.input, cmd = m.dockerMirror.input.Update(msg)
			return m, append(cmds, cmd)
		}
	}
	switch key {
	case "tab":
		m.dockerMirror.moveFocus(1)
	case "shift+tab":
		m.dockerMirror.moveFocus(-1)
	case "up":
		m.dockerMirror.moveFocus(-1)
	case "down":
		m.dockerMirror.moveFocus(1)
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
			m.dockerMirror.actionIndex = min(dockerMirrorActionCount-1, m.dockerMirror.actionIndex+1)
		}
	case "pgup":
		if m.dockerMirror.focus == dockerMirrorFocusActions && m.dockerMirror.actionIndex == 3 {
			m.dockerMirror.backupIndex = max(0, m.dockerMirror.backupIndex-1)
		}
	case "pgdown":
		if m.dockerMirror.focus == dockerMirrorFocusActions && m.dockerMirror.actionIndex == 3 && len(m.dockerMirror.backups) > 0 {
			m.dockerMirror.backupIndex = min(len(m.dockerMirror.backups)-1, m.dockerMirror.backupIndex+1)
		}
	case "ctrl+s", "s":
		m.dockerMirror.saveInputToFocus()
		if err := config.SaveProjectDaemonMirrors(m.cfg.ProjectConfigPath, m.dockerMirror.values); err != nil {
			m.message = "保存 Docker 镜像源配置失败: " + err.Error()
		} else {
			m.message = "已保存 Docker 镜像源配置到 " + m.cfg.ProjectConfigPath
			m.cfg.Docker.DaemonMirrors = m.dockerMirror.values
		}
	case "r":
		m.dockerMirror.saveInputToFocus()
		return m, append(cmds, dockerMirrorStatusCmd(m.cfg, m.dockerMirror.values))
	case "a":
		return m.requestDockerMirrorApply(cmds)
	case "b":
		return m.requestDockerMirrorRestore(cmds)
	case "enter":
		if m.dockerMirror.focus == dockerMirrorFocusActions {
			switch m.dockerMirror.actionIndex {
			case 0:
				return m, append(cmds, dockerMirrorStatusCmd(m.cfg, m.dockerMirror.values))
			case 1:
				if err := config.SaveProjectDaemonMirrors(m.cfg.ProjectConfigPath, m.dockerMirror.values); err != nil {
					m.message = "保存 Docker 镜像源配置失败: " + err.Error()
				} else {
					m.message = "已保存 Docker 镜像源配置到 " + m.cfg.ProjectConfigPath
					m.cfg.Docker.DaemonMirrors = m.dockerMirror.values
				}
			case 2:
				return m.requestDockerMirrorApply(cmds)
			case 3:
				return m.requestDockerMirrorRestore(cmds)
			}
		}
	case "esc":
		m.dockerMirror.saveInputToFocus()
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
		var summary dockermgr.DaemonMirrorSummary
		var err error
		if values.RequireManualApply {
			summary, err = dockermgr.PlanDaemonMirrors(cfg)
		} else {
			summary, err = dockermgr.ApplyDaemonMirrors(cfg, true)
		}
		return dockerMirrorMsg{operation: summary.Operation, summary: summary, err: err}
	}
}

func dockerMirrorRestoreCmd(cfg config.Config, values config.DockerDaemonMirrorsConfig, backup string) tea.Cmd {
	cfg.Docker.DaemonMirrors = values
	return func() tea.Msg {
		var summary dockermgr.DaemonMirrorSummary
		var err error
		if values.RequireManualApply {
			summary, err = dockermgr.PlanRestoreDaemonMirrors(cfg, backup)
		} else {
			summary, err = dockermgr.RestoreDaemonMirrors(cfg, backup, true)
		}
		return dockerMirrorMsg{operation: summary.Operation, summary: summary, err: err}
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

func dockerStartupCheckCmd(cfg config.Config, activeJobs int) tea.Cmd {
	return func() tea.Msg {
		if activeJobs > 0 {
			return startupDockerCheckMsg{}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		dockerCfg := cfg.Docker
		dockerCfg.GC.Enabled = true
		dockerCfg.GC.P2ROnly = true
		dockerCfg.GC.PruneExitedContainers = true
		dockerCfg.GC.PruneNetworks = true
		dockerCfg.GC.PruneVolumes = false
		dockerCfg.GC.PruneImages = false
		dockerCfg.GC.PruneBuilderCache = false
		summary, err := dockermgr.RunGC(ctx, dockermgr.GCRunRequest{
			ScanPath:   cfg.ScanPath,
			Config:     dockerCfg,
			Exec:       executor.New(),
			DryRun:     true,
			Trigger:    "tui_start_legacy_check",
			SkipReason: "",
		})
		return startupDockerCheckMsg{count: dockerGCCandidateCount(summary), err: err}
	}
}

func startupDockerCleanupCmd(cfg config.Config) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		return dockerGCMsg{summary: dockermgr.GCSummary{OK: true, Trigger: "startup_confirmed_cleanup"}, err: LightExitCleanup(ctx, cfg)}
	}
}

func dockerGCCandidateCount(summary dockermgr.GCSummary) int {
	count := 0
	for _, action := range summary.Actions {
		count += len(action.Candidates)
	}
	return count
}

func renderDockerSettings(m app) string {
	p := m.dockerMirror
	lines := []string{
		titleStyle.Render("Docker 镜像源"),
		"",
		dockerMirrorSection("目标配置"),
		dockerMirrorSettingRow(p, dockerMirrorFocusEnabled, "启用", dockerMirrorSwitchValue(p.values.Enabled)),
		dockerMirrorSettingRow(p, dockerMirrorFocusDaemonJSON, "daemon.json 路径", dockerMirrorField(p, dockerMirrorFocusDaemonJSON, p.values.DaemonJSON)),
		dockerMirrorSettingRow(p, dockerMirrorFocusBackupDir, "备份目录", dockerMirrorField(p, dockerMirrorFocusBackupDir, p.values.BackupDir)),
		dockerMirrorSettingRow(p, dockerMirrorFocusMirrors, "镜像源", dockerMirrorField(p, dockerMirrorFocusMirrors, dockerMirrorListValue(p.values.RegistryMirrors))),
		dockerMirrorSettingRow(p, dockerMirrorFocusManualApply, "需要手动应用", dockerMirrorSwitchValue(p.values.RequireManualApply)),
		dockerMirrorReadonlyRow("备份", dockerMirrorBackupLine(p)),
		"",
		dockerMirrorReadonlySection("当前 Docker 守护进程（只读）"),
		dockerMirrorReadonlyRow("daemon.json 路径", p.values.DaemonJSON),
		dockerMirrorReadonlyRow("状态", dockerMirrorStatusLine(p.status)),
		dockerMirrorReadonlyRow("当前镜像源", dockerMirrorListValue(p.status.CurrentRegistryMirrors)),
		dockerMirrorReadonlyRow("目标镜像源", dockerMirrorListValue(p.values.RegistryMirrors)),
	}
	if p.status.ManualApplyPath != "" {
		lines = append(lines, dockerMirrorReadonlyRow("手动应用文件", p.status.ManualApplyPath))
	}
	if p.status.ManualApplyCommand != "" {
		lines = append(lines, dockerMirrorReadonlyRow("手动命令", p.status.ManualApplyCommand))
	}
	lines = append(lines,
		"",
		dockerMirrorSection("操作"),
		renderDockerMirrorActions(p),
	)
	if p.confirm != "" {
		lines = append(lines,
			"",
			activeStyle.Render("确认"+dockerMirrorOperationName(p.confirm)),
			dockerMirrorReadonlyRow("daemon.json 路径", p.values.DaemonJSON),
			dockerMirrorReadonlyRow("备份文件", p.selectedBackup()),
			dockerMirrorReadonlyRow("备份目录", p.values.BackupDir),
			"Docker 守护进程配置变更后需要重启。y/回车确认，n/Esc取消",
		)
	}
	if p.status.Error != "" {
		lines = append(lines, "", errorStyle.Render("错误: "+p.status.Error))
	}
	return strings.Join(lines, "\n")
}

func dockerMirrorBackupLine(p dockerMirrorPanel) string {
	if len(p.backups) == 0 {
		return "无可用备份"
	}
	return fmt.Sprintf("%d 个，可恢复 %s", len(p.backups), p.backups[max(0, min(p.backupIndex, len(p.backups)-1))])
}

func dockerMirrorField(p dockerMirrorPanel, focus dockerMirrorFocus, fallback string) string {
	if p.focus == focus {
		return p.input.View()
	}
	return dockerMirrorEmpty(fallback)
}

func dockerMirrorSection(title string) string {
	return activeStyle.Render(title)
}

func dockerMirrorReadonlySection(title string) string {
	return mutedStyle.Render(title)
}

func dockerMirrorSettingRow(p dockerMirrorPanel, focus dockerMirrorFocus, label string, value string) string {
	line := label + ": " + value
	if p.focus == focus {
		return selectedStyle.Render("> " + line)
	}
	return "  " + mutedStyle.Render(label+": ") + value
}

func dockerMirrorReadonlyRow(label string, value string) string {
	return "  " + mutedStyle.Render(label+": ") + dockerMirrorEmpty(value)
}

func dockerMirrorSwitchValue(value bool) string {
	if value {
		return "[✓] 是"
	}
	return "[ ] 否"
}

func dockerMirrorListValue(values []string) string {
	if len(values) == 0 {
		return "无"
	}
	return strings.Join(values, "，")
}

func dockerMirrorEmpty(value string) string {
	if strings.TrimSpace(value) == "" {
		return "未设置"
	}
	return value
}

func renderDockerMirrorActions(p dockerMirrorPanel) string {
	actions := dockerMirrorActionLabels(p)
	for index, action := range actions {
		action = "[" + action + "]"
		if p.focus == dockerMirrorFocusActions && p.actionIndex == index {
			actions[index] = selectedStyle.Render("> " + action)
			continue
		}
		actions[index] = "  " + action
	}
	return strings.Join(actions, "\n")
}

func dockerMirrorActionLabels(p dockerMirrorPanel) []string {
	if p.values.RequireManualApply {
		return []string{"刷新状态", "保存配置", "生成手动应用文件", "生成恢复文件"}
	}
	return []string{"刷新状态", "保存配置", "应用到守护进程", "恢复备份"}
}

func dockerMirrorStatusLine(summary dockermgr.DaemonMirrorSummary) string {
	if summary.DaemonJSON == "" {
		return "未加载"
	}
	if !summary.OK {
		return "错误"
	}
	if summary.Changed {
		return "不一致"
	}
	return "一致"
}

func dockerMirrorOperationName(operation string) string {
	switch operation {
	case "status":
		return "刷新状态"
	case "manual_apply":
		return "生成手动应用文件"
	case "apply":
		return "应用"
	case "manual_restore":
		return "生成恢复文件"
	case "restore":
		return "恢复"
	default:
		return operation
	}
}

func (m *app) handleDockerMirrorMsg(msg tea.Msg, cmds *[]tea.Cmd) bool {
	switch value := msg.(type) {
	case dockerMirrorMsg:
		m.dockerMirror.status = value.summary
		if value.err != nil {
			m.message = fmt.Sprintf("Docker 镜像源%s失败: %s", dockerMirrorOperationName(value.operation), value.err)
		} else if value.summary.ManualApplyPath != "" {
			m.message = fmt.Sprintf("Docker 镜像源%s完成: %s", dockerMirrorOperationName(value.operation), value.summary.ManualApplyPath)
		} else {
			m.message = fmt.Sprintf("Docker 镜像源%s完成", dockerMirrorOperationName(value.operation))
		}
		return true
	case dockerGCMsg:
		if value.err != nil {
			m.message = "Docker 清理失败: " + value.err.Error()
		} else if value.summary.Trigger == "startup_confirmed_cleanup" {
			m.message = "Docker 遗留资源清理完成"
		} else if value.summary.Skipped {
			m.message = "Docker 清理已跳过: " + value.summary.SkipReason
		}
		return true
	case startupDockerCheckMsg:
		if value.err != nil {
			m.message = "Docker 遗留资源检查失败: " + value.err.Error()
			return true
		}
		if value.count > 0 {
			m.confirmStartupDockerCleanup = true
			m.startupDockerCleanupCount = value.count
		}
		return true
	default:
		return false
	}
}
