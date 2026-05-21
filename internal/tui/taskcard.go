package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
)

func renderTaskCard(task TaskProject, selected bool, width int, now time.Time) string {
	width = max(12, width)
	bodyWidth := max(8, width-2)
	var lines []string
	lines = append(lines, truncateMiddleDisplay(task.ID, bodyWidth))
	switch task.TaskState {
	case model.TaskWaitingManual:
		lines = append(lines, waitingTaskLine(task, bodyWidth))
		lines = append(lines, waitingDurationLine(task.EnteredWaitingAt, now, bodyWidth))
	case model.TaskCompleted:
		if taskHasGitSyncStatus(task) {
			lines = append(lines, inspectingTaskLines(task, bodyWidth)...)
			if strings.TrimSpace(task.SyncError) != "" {
				lines = append(lines, errorStyle.Render(truncateDisplay("Ctrl+W 重试", bodyWidth)))
			}
		} else {
			line := fmt.Sprintf("累计完成: %d 次", task.CompletionCount)
			if task.LastCompletedAt != "" {
				line += " · 最后: " + shortTime(task.LastCompletedAt)
			}
			lines = append(lines, truncateDisplay(line, bodyWidth))
		}
	default:
		lines = append(lines, inspectingTaskLines(task, bodyWidth)...)
		if strings.TrimSpace(task.SyncError) != "" {
			lines = append(lines, errorStyle.Render(truncateDisplay("Ctrl+W 重试", bodyWidth)))
		}
	}
	lines = append(lines, mutedStyle.Render(strings.Repeat("─", min(bodyWidth, 28))))
	rendered := strings.Join(lines, "\n")
	if selected {
		selectedLines := make([]string, 0, len(lines))
		for _, line := range lines {
			selectedLines = append(selectedLines, selectedStyle.Render(padDisplay(line, bodyWidth)))
		}
		return strings.Join(selectedLines, "\n")
	}
	return rendered
}

func padDisplay(value string, width int) string {
	value = truncateDisplay(value, width)
	if extra := width - lipgloss.Width(value); extra > 0 {
		return value + strings.Repeat(" ", extra)
	}
	return value
}

func taskHasGitSyncStatus(task TaskProject) bool {
	return strings.TrimSpace(task.SyncError) != "" || (strings.TrimSpace(task.SyncPhase) != "" && task.SyncPhase != "done")
}

func inspectingTaskLines(task TaskProject, width int) []string {
	if strings.TrimSpace(task.SyncError) != "" {
		return []string{errorStyle.Render(truncateDisplay("[Git 同步失败] "+task.SyncError, width))}
	}
	if strings.TrimSpace(task.SyncPhase) != "" && task.SyncPhase != "done" {
		phase := task.SyncPhase
		if task.SyncPercent >= 0 {
			phase = fmt.Sprintf("%s: %d%%", phase, task.SyncPercent)
		}
		return []string{truncateDisplay("[Git 同步中] "+phase, width)}
	}
	if task.CurrentStatus == model.StageFailed || task.CurrentStatus == model.StageBlocked || strings.TrimSpace(task.FailedStage) != "" {
		stage := firstNonEmpty(task.FailedStage, task.CurrentStage)
		lines := []string{truncateDisplay(compactStageLabel(stage), width)}
		reason := localizeSummary(task.FailedSummary)
		if reason == "" {
			reason = "阶段失败"
		}
		lines = append(lines, errorStyle.Render(truncateDisplay("✗ 失败: "+reason, width)))
		return lines
	}
	if task.RunStatus == model.RunRunning {
		stage := firstNonEmpty(task.CurrentStage, "运行中")
		return []string{
			truncateDisplay(compactStageLabel(stage), width),
			progressBar(stageProgressPercent(stage), width),
		}
	}
	return []string{mutedStyle.Render(truncateDisplay("[Git 同步中]", width))}
}

func waitingTaskLine(task TaskProject, width int) string {
	switch {
	case task.DockerRunning && task.FrontendURL != "":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#4488FF")).Render(truncateDisplay(task.FrontendURL, width))
	case task.DockerRunning:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#DDAA00")).Render(truncateDisplay("! Docker 已启动，端口检测失败", width))
	default:
		return errorStyle.Render(truncateDisplay("✗ Docker 启动失败", width))
	}
}

func waitingDuration(value string, now time.Time) string {
	return waitingDurationParts(value, now).text
}

type waitingDurationView struct {
	text string
	late bool
}

func waitingDurationParts(value string, now time.Time) waitingDurationView {
	start, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
	if err != nil {
		return waitingDurationView{text: "--:--"}
	}
	if now.IsZero() {
		now = time.Now()
	}
	elapsed := now.Sub(start)
	if elapsed < 0 {
		elapsed = 0
	}
	hours := int(elapsed.Hours())
	minutes := int(elapsed.Minutes()) % 60
	seconds := int(elapsed.Seconds()) % 60
	view := waitingDurationView{late: elapsed >= 30*time.Minute}
	if hours > 0 {
		view.text = fmt.Sprintf("%d:%02d:%02d", hours, minutes, seconds)
		return view
	}
	view.text = fmt.Sprintf("%02d:%02d", minutes, seconds)
	return view
}

func waitingDurationLine(value string, now time.Time, width int) string {
	duration := waitingDurationParts(value, now)
	line := "等待: " + duration.text
	if duration.late {
		line = "⏱ " + line
		return errorStyle.Render(truncateDisplay(line, width))
	}
	return truncateDisplay(line, width)
}

func compactStageLabel(stage string) string {
	stage = strings.TrimSpace(stage)
	if stage == "" || stage == "运行中" {
		return "运行中"
	}
	return stage + ": " + localizeStageName(stage, "")
}

func progressBar(percent int, width int) string {
	percent = clamp(percent, 0, 100)
	label := fmt.Sprintf(" %d%%", percent)
	barWidth := max(4, min(18, width-lipgloss.Width(label)-2))
	filled := barWidth * percent / 100
	bar := "[" + strings.Repeat("▓", filled) + strings.Repeat("░", barWidth-filled) + "]"
	return truncateDisplay(bar+label, width)
}

func stageProgressPercent(stage string) int {
	stages := model.AllStages()
	for index, candidate := range stages {
		if candidate == stage {
			return (index*100 + 50) / len(stages)
		}
	}
	return 5
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
