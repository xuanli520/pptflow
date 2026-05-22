package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
)

type taskCardLine struct {
	text   string
	style  lipgloss.Style
	styled bool
}

const taskCardMinLineCount = 4

func renderTaskCard(task TaskProject, width int, now time.Time, isSelected bool) string {
	width = max(12, width)
	bodyWidth := max(8, width-2)
	var lines []taskCardLine
	lines = append(lines, taskCardLine{text: truncateMiddleDisplay(task.ID, bodyWidth)})
	switch task.TaskState {
	case model.TaskWaitingManual:
		lines = append(lines, waitingTaskLines(task, bodyWidth)...)
		lines = append(lines, waitingDurationLine(task.EnteredWaitingAt, now, bodyWidth))
	case model.TaskCompleted:
		if taskHasGitSyncStatus(task) {
			lines = append(lines, inspectingTaskLines(task, bodyWidth)...)
			if strings.TrimSpace(task.SyncError) != "" {
				lines = append(lines, taskCardLine{text: "Ctrl+W 重试", style: errorStyle, styled: true})
			}
		} else {
			line := fmt.Sprintf("累计完成: %d 次", task.CompletionCount)
			if task.LastCompletedAt != "" {
				line += " · 最后: " + shortTime(task.LastCompletedAt)
			}
			lines = append(lines, taskCardLine{text: line})
		}
	default:
		lines = append(lines, inspectingTaskLines(task, bodyWidth)...)
		if strings.TrimSpace(task.SyncError) != "" {
			lines = append(lines, taskCardLine{text: "Ctrl+W 重试", style: errorStyle, styled: true})
		}
	}
	if len(lines) < taskCardMinLineCount {
		if isSelected {
			lines = append(lines, taskCardLine{text: renderGradientBar(bodyWidth)})
		} else {
			lines = append(lines, taskCardLine{text: strings.Repeat("─", min(bodyWidth, 28)), style: mutedStyle, styled: true})
		}
	}
	for len(lines) < taskCardMinLineCount {
		lines = append(lines, taskCardLine{})
	}
	rendered := make([]string, 0, len(lines))
	for _, line := range lines {
		text := truncateDisplay(taskCardLineText(line.text), bodyWidth)
		if line.styled {
			text = line.style.Render(text)
		}
		rendered = append(rendered, text)
	}
	return strings.Join(rendered, "\n")
}

func renderSelectedIndicator(width int) string {
	return lipgloss.NewStyle().
		Foreground(lipgloss.Color("#4488FF")).
		Width(width).
		Align(lipgloss.Left).
		Render("▼")
}

func taskCardLineText(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func taskHasGitSyncStatus(task TaskProject) bool {
	return strings.TrimSpace(task.SyncError) != "" || (strings.TrimSpace(task.SyncPhase) != "" && task.SyncPhase != "done")
}

func inspectingTaskLines(task TaskProject, width int) []taskCardLine {
	if strings.TrimSpace(task.SyncError) != "" {
		summary, logPath := splitSyncErrorLog(task.SyncError)
		lines := []taskCardLine{{text: "[Git 同步失败] " + summary, style: errorStyle, styled: true}}
		if logPath != "" {
			lines = append(lines, taskCardLine{text: "日志: " + logPath, style: mutedStyle, styled: true})
		}
		return lines
	}
	if strings.TrimSpace(task.SyncPhase) != "" && task.SyncPhase != "done" {
		phase := task.SyncPhase
		if task.SyncPercent >= 0 {
			phase = fmt.Sprintf("%s: %d%%", phase, task.SyncPercent)
		}
		return []taskCardLine{{text: "[Git 同步中] " + phase}}
	}
	if task.RunStatus == model.RunRunning && (task.CurrentStatus == model.StageRunning || strings.TrimSpace(task.CurrentStage) != "") {
		stage := firstNonEmpty(task.CurrentStage, "运行中")
		return []taskCardLine{
			{text: compactStageLabel(stage)},
			{text: progressBar(stageProgressPercent(stage), width)},
		}
	}
	if task.CurrentStatus == model.StageFailed || task.CurrentStatus == model.StageBlocked || strings.TrimSpace(task.FailedStage) != "" {
		stage := firstNonEmpty(task.FailedStage, task.CurrentStage)
		lines := []taskCardLine{{text: compactStageLabel(stage)}}
		reason := localizeSummary(task.FailedSummary)
		if reason == "" {
			reason = "阶段失败"
		}
		lines = append(lines, taskCardLine{text: "✗ 失败: " + reason, style: errorStyle, styled: true})
		return lines
	}
	if task.RunStatus == model.RunRunning {
		return []taskCardLine{
			{text: "运行中"},
			{text: progressBar(5, width)},
		}
	}
	return []taskCardLine{{text: "[Git 同步中]", style: mutedStyle, styled: true}}
}

func splitSyncErrorLog(value string) (string, string) {
	value = strings.TrimSpace(value)
	marker := "; 日志:"
	index := strings.LastIndex(value, marker)
	if index < 0 {
		return value, ""
	}
	summary := strings.TrimSpace(value[:index])
	logPath := strings.TrimSpace(value[index+len(marker):])
	return summary, logPath
}

func waitingTaskLines(task TaskProject, width int) []taskCardLine {
	if task.DockerRunning {
		ports := waitingServicePorts(task)
		if len(ports) > 0 {
			return styledTaskLines(wrapTaskCardText("端口: "+strings.Join(ports, "  "), width), lipgloss.NewStyle().Foreground(lipgloss.Color("#4488FF")))
		}
	}
	switch {
	case task.DockerRunning && task.FrontendURL != "":
		return []taskCardLine{{text: task.FrontendURL, style: lipgloss.NewStyle().Foreground(lipgloss.Color("#4488FF")), styled: true}}
	case task.DockerRunning:
		return []taskCardLine{{text: "! Docker 已启动，端口检测失败", style: lipgloss.NewStyle().Foreground(lipgloss.Color("#DDAA00")), styled: true}}
	case strings.TrimSpace(task.ComposeMeta.Project) != "":
		return []taskCardLine{{text: "Docker 已停止，Ctrl+S 启动服务", style: mutedStyle, styled: true}}
	default:
		return []taskCardLine{{text: "✗ Docker 启动失败", style: errorStyle, styled: true}}
	}
}

func waitingServicePorts(task TaskProject) []string {
	var result []string
	seen := map[string]bool{}
	for _, port := range task.ComposeMeta.Ports {
		label := servicePortLabel(port)
		if label == "" || seen[label] {
			continue
		}
		seen[label] = true
		result = append(result, label)
	}
	if len(result) == 0 && strings.TrimSpace(task.FrontendURL) != "" {
		result = append(result, strings.TrimSpace(task.FrontendURL))
	}
	return result
}

func servicePortLabel(port model.ServicePort) string {
	url := strings.TrimSpace(port.URL)
	if url == "" && port.Host > 0 {
		scheme := "http"
		if port.Container == 443 || port.Host == 443 {
			scheme = "https"
		}
		url = fmt.Sprintf("%s://localhost:%d", scheme, port.Host)
	}
	if url == "" {
		return ""
	}
	service := strings.TrimSpace(port.Service)
	if service == "" {
		return url
	}
	return service + " " + url
}

func styledTaskLines(values []string, style lipgloss.Style) []taskCardLine {
	lines := make([]taskCardLine, 0, len(values))
	for _, value := range values {
		lines = append(lines, taskCardLine{text: value, style: style, styled: true})
	}
	return lines
}

func wrapTaskCardText(value string, width int) []string {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return nil
	}
	var lines []string
	line := fields[0]
	for _, field := range fields[1:] {
		next := line + " " + field
		if lipgloss.Width(next) <= width {
			line = next
			continue
		}
		lines = append(lines, line)
		line = field
	}
	lines = append(lines, line)
	return lines
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

func waitingDurationLine(value string, now time.Time, width int) taskCardLine {
	duration := waitingDurationParts(value, now)
	line := "等待: " + duration.text
	if duration.late {
		line = "⏱ " + line
		return taskCardLine{text: line, style: errorStyle, styled: true}
	}
	return taskCardLine{text: line}
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

var gradientColors = []string{
	"#c1ff72", "#b1f186", "#a2e49a", "#92d6ae",
	"#83c9c2", "#73bbd7", "#64aeeb", "#54a0ff",
}

func renderGradientBar(width int) string {
	if width <= 0 {
		return ""
	}
	segCount := len(gradientColors)
	lineWidth := min(width, segCount*2)
	segWidth := lineWidth / segCount
	remainder := lineWidth % segCount
	var bar strings.Builder
	for i := 0; i < segCount; i++ {
		w := segWidth
		if i < remainder {
			w++
		}
		if w <= 0 {
			continue
		}
		bar.WriteString(lipgloss.NewStyle().
			Foreground(lipgloss.Color(gradientColors[i])).
			Render(strings.Repeat("─", w)))
	}
	return bar.String()
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
