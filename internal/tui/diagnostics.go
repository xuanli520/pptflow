package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xuanli520/p2r_tui/internal/tasklifecycle"
)

type taskDiagnosticsState struct {
	active   bool
	pending  bool
	snapshot tasklifecycle.TaskDiagnosticsSnapshot
}

func isDiagnosticsKey(key string) bool {
	return key == "ctrl+d"
}

func (m app) openTaskDiagnosticsForCurrentContext(cmds []tea.Cmd) (app, []tea.Cmd) {
	taskID := strings.TrimSpace(m.selectedTaskID())
	if taskID == "" {
		m.message = "没有选中的任务"
		return m, cmds
	}
	m.message = "正在生成任务诊断报告 " + taskID
	return m, append(cmds, m.taskDiagnosticsCmd(taskID))
}

func (m app) handleTaskDiagnosticsKey(key string, cmds []tea.Cmd) (app, []tea.Cmd) {
	switch key {
	case "esc", "q", "n", "N":
		m.diagnostics = taskDiagnosticsState{}
		m.message = "已关闭任务诊断"
		return m, cmds
	case "enter", "y", "Y":
		if m.diagnostics.pending {
			return m, cmds
		}
		if !diagnosticsCanRepair(m.diagnostics.snapshot) {
			m.message = "任务状态诊断未发现可修复问题"
			return m, cmds
		}
		m.diagnostics.pending = true
		m.message = "正在修复任务状态 " + m.diagnostics.snapshot.Task.ID
		return m, append(cmds, m.taskDiagnosticsRepairCmd(m.diagnostics.snapshot))
	default:
		return m, cmds
	}
}

func (m *app) handleDiagnosticsMsg(msg tea.Msg, cmds *[]tea.Cmd) bool {
	switch value := msg.(type) {
	case taskDiagnosticsMsg:
		if value.err != nil {
			m.message = "任务诊断失败: " + value.err.Error()
			return true
		}
		m.diagnostics = taskDiagnosticsState{active: true, snapshot: value.snapshot}
		if diagnosticsCanRepair(value.snapshot) {
			m.message = fmt.Sprintf("任务状态诊断发现 %d 个可修复问题", countRepairableIssues(value.snapshot))
		} else if len(value.snapshot.Issues) > 0 {
			m.message = fmt.Sprintf("任务状态诊断发现 %d 个问题，无需自动修复", len(value.snapshot.Issues))
		} else {
			m.message = "任务状态诊断未发现可修复问题"
		}
		return true
	case taskDiagnosticsRepairMsg:
		m.diagnostics = taskDiagnosticsState{}
		if value.err != nil {
			m.message = "任务状态修复失败: " + value.err.Error()
			*cmds = append(*cmds, m.reloadOverview(), m.taskBoard.Reload(), m.reloadSchedulerJobs())
			return true
		}
		m.message = fmt.Sprintf("已修复 %d 个任务状态问题", len(value.result.FixedIssues))
		if value.result.CleanupError != "" {
			m.message += "，Docker 清理失败: " + value.result.CleanupError
		}
		if value.result.LogPath != "" {
			m.message += "，日志: " + value.result.LogPath
		}
		*cmds = append(*cmds, m.reloadOverview(), m.taskBoard.Reload(), m.reloadSchedulerJobs(), m.reloadDetail())
		return true
	default:
		return false
	}
}

func diagnosticsCanRepair(snapshot tasklifecycle.TaskDiagnosticsSnapshot) bool {
	for _, issue := range snapshot.Issues {
		if issue.Policy == tasklifecycle.FixTerminalReset || issue.Policy == tasklifecycle.FixStopLeakedDocker {
			return true
		}
	}
	return false
}

func countRepairableIssues(snapshot tasklifecycle.TaskDiagnosticsSnapshot) int {
	count := 0
	for _, issue := range snapshot.Issues {
		if issue.Policy == tasklifecycle.FixTerminalReset || issue.Policy == tasklifecycle.FixStopLeakedDocker {
			count++
		}
	}
	return count
}

func renderTaskDiagnostics(m app) string {
	width := max(20, m.width-4)
	snapshot := m.diagnostics.snapshot
	lines := []string{
		titleStyle.Render("任务诊断: " + snapshot.Task.ID),
		fmt.Sprintf("状态: %s  当前运行: %s", localizeTaskState(snapshot.Task.State), empty(snapshot.Task.CurrentRunID, "-")),
		fmt.Sprintf("运行: %d  事件: %d  失败文件: %d", len(snapshot.Runs), len(snapshot.Events), len(snapshot.FailureFiles)),
	}
	if snapshot.Lock.Exists {
		lines = append(lines, fmt.Sprintf("锁: pid=%d stale=%t path=%s", snapshot.Lock.PID, snapshot.Lock.Stale, truncateMiddleDisplay(snapshot.Lock.Path, max(8, width-16))))
	}
	if snapshot.Docker.Checked {
		dockerLine := fmt.Sprintf("Docker: running=%t", snapshot.Docker.Running)
		if snapshot.Docker.Error != "" {
			dockerLine += " error=" + snapshot.Docker.Error
		}
		lines = append(lines, truncateDisplay(dockerLine, width))
	}
	lines = append(lines, "")
	if len(snapshot.Issues) == 0 {
		lines = append(lines, "未发现诊断问题")
	} else {
		lines = append(lines, "问题:")
		for _, issue := range snapshot.Issues {
			line := fmt.Sprintf("%s [%s] %s -> %s", issue.Code, issue.Severity, issue.Message, issue.Policy)
			lines = append(lines, truncateDisplay(line, width))
			if issue.Detail != "" {
				lines = append(lines, mutedStyle.Render(truncateDisplay("  "+issue.Detail, width)))
			}
		}
	}
	lines = append(lines, "")
	if m.diagnostics.pending {
		lines = append(lines, "正在修复...")
	} else if diagnosticsCanRepair(snapshot) {
		lines = append(lines, "Enter 修复  Esc/q 关闭")
	} else {
		lines = append(lines, "Esc/q 关闭")
	}
	return panelStyle.Width(width).Render(strings.Join(lines, "\n"))
}
