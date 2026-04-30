package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/lipgloss"
)

var (
	appStyle      = lipgloss.NewStyle().Padding(0, 1)
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#00DDDD"))
	activeStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#00DDDD"))
	mutedStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#888888"))
	errorStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF4444"))
	footerStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#888888"))
	panelStyle    = lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color("#555555")).Padding(0, 1)
	selectedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFFFFF")).Background(lipgloss.Color("#005FD7"))
)

func tableStyles() table.Styles {
	styles := table.DefaultStyles()
	styles.Header = styles.Header.Bold(true).Foreground(lipgloss.Color("#00DDDD"))
	styles.Selected = styles.Selected.Foreground(lipgloss.Color("#FFFFFF")).Background(lipgloss.Color("#005FD7")).Bold(false)
	return styles
}

func renderHeader(m app) string {
	overview := "[项目总览]"
	execution := "[执行详情]"
	if m.tab == panelOverview {
		overview = activeStyle.Render(overview)
	} else {
		execution = activeStyle.Render(execution)
	}
	mode := "模式: " + localizeMode(m.qaMode)
	return titleStyle.Render("p2r QA 工作台") + "  " + overview + "  " + execution + "  " + mutedStyle.Render(mode)
}

func renderOverview(m app) string {
	if len(m.overviewItems) == 0 {
		return m.search.View() + "\n\n" + mutedStyle.Render("未选择已索引的项目\n请先执行 `p2r scan --path <projects-qa>`")
	}
	return m.search.View() + "\n\n" + m.table.View()
}

func renderExecution(m app) string {
	taskID := m.selectedTaskID()
	if taskID == "" {
		return mutedStyle.Render("未选择已索引的项目\n请先执行 `p2r scan --path <projects-qa>`")
	}
	layout := layoutFor(m.width, m.height, true)
	if layout.mode == layoutWide || layout.mode == layoutMedium {
		left := renderPanel(layout.leftWidth, layout.contentHeight, renderExecutionLeft(m, max(8, layout.leftWidth-panelStyle.GetHorizontalFrameSize())))
		right := renderPanel(layout.rightWidth, layout.contentHeight, m.detail.View())
		return lipgloss.JoinHorizontal(lipgloss.Top, left, right)
	}
	stageSummary := renderPanel(layout.contentWidth, layout.stageHeight, renderStageSummary(m, max(8, layout.contentWidth-panelStyle.GetHorizontalFrameSize())))
	detail := renderPanel(layout.contentWidth, max(6, layout.contentHeight-layout.stageHeight), m.detail.View())
	return lipgloss.JoinVertical(lipgloss.Left, stageSummary, detail)
}

func renderPanel(width, height int, content string) string {
	contentWidth := max(1, width-panelStyle.GetHorizontalFrameSize())
	contentHeight := max(1, height-panelStyle.GetVerticalFrameSize())
	return panelStyle.Width(contentWidth).Height(contentHeight).Render(content)
}

func renderExecutionLeft(m app, width int) string {
	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("任务: %s\n", truncateMiddleDisplay(m.detailVM.TaskID, max(8, width-4))))
	if m.detailVM.HasRun {
		builder.WriteString(fmt.Sprintf("运行: %s\n", truncateMiddleDisplay(m.detailVM.Run.RunID, max(8, width-4))))
		builder.WriteString(fmt.Sprintf("状态: %s\n", localizeRunStatus(m.detailVM.Run.Status)))
	} else {
		builder.WriteString("运行: 未生成\n")
	}
	builder.WriteString("模式: " + localizeMode(m.qaMode) + "\n")
	if m.qaMode == "recheck" {
		builder.WriteString("参考运行: " + empty(m.selectedRefRun(), "-") + "\n")
	}
	builder.WriteString("\n阶段:\n")
	for index, stage := range m.detailVM.Stages {
		builder.WriteString(renderStageLine(stage, index == m.stageIndex, width))
		builder.WriteString("\n")
	}
	if m.qaMode == "recheck" {
		builder.WriteString("\n参考运行列表:\n")
		if len(m.detailVM.RefRuns) == 0 {
			builder.WriteString("  无可用参考运行\n")
		}
		for index, run := range m.detailVM.RefRuns {
			prefix := "  "
			line := fmt.Sprintf("%s %s", run.RunID, localizeRunStatus(run.Status))
			if index == m.refIndex {
				prefix = "> "
				if m.focus == focusRefRunList {
					line = selectedStyle.Render(truncateDisplay(line, max(8, width-2)))
					builder.WriteString(prefix + line + "\n")
					continue
				}
			}
			builder.WriteString(prefix + truncateDisplay(line, max(8, width-2)) + "\n")
		}
	}
	return builder.String()
}

func renderStageSummary(m app, width int) string {
	if len(m.detailVM.Stages) == 0 {
		return "阶段: 未生成"
	}
	var parts []string
	for index, stage := range m.detailVM.Stages {
		icon, color := stageStatusIcon(stage.Status)
		text := icon + stage.Stage
		if index == m.stageIndex {
			text = ">" + text
		}
		parts = append(parts, lipgloss.NewStyle().Foreground(color).Render(text))
	}
	line := "阶段: " + strings.Join(parts, " ")
	if m.qaMode == "recheck" {
		line += "  参考运行: " + empty(m.selectedRefRun(), "-")
	}
	return truncateDisplay(line, width)
}

func renderStageLine(stage stageView, selected bool, width int) string {
	icon, color := stageStatusIcon(stage.Status)
	status := lipgloss.NewStyle().Foreground(color).Render(icon)
	reason := stage.ErrorSummary
	if reason != "" {
		reason = " " + reason
	}
	line := fmt.Sprintf("%s %s %-18s %s %dms%s", status, stage.Stage, stage.DisplayName, localizeStageStatus(stage.Status), stage.DurationMS, reason)
	line = truncateDisplay(line, max(8, width-2))
	if selected {
		line = "> " + line
		if width > 2 && selected {
			line = selectedStyle.Render(truncateDisplay(line, width))
		}
		return line
	}
	return "  " + line
}

func renderConfirm(m app) string {
	stage := m.selectedStage()
	stageKey := stage.Stage
	if stageKey == "" {
		stageKey = m.selectedStageKey
	}
	if stageKey == "" {
		stageKey = "A"
	}
	stages := affectedStages(stageKey)
	docs := m.detailVM.DocsSummary
	var updates []string
	for _, affected := range stages {
		updates = append(updates, reportTypeForStage(affected))
	}
	lines := []string{
		fmt.Sprintf("确认重新运行 %s？", m.selectedTaskID()),
		"模式: " + localizeMode(m.qaMode),
		"参考运行: " + empty(m.selectedRefRun(), "-"),
		"阶段: " + strings.Join(stages, ", "),
		fmt.Sprintf("补充文档: %d 个，manifest: %s", docs.Count, docs.ManifestPath),
		"预检: 将在新运行中重新生成 preflight.json",
		"清理: " + cleanupImpactText(m.cfg.Docker.KeepRuntime),
		fmt.Sprintf("keep-runtime: %s", yesNo(m.cfg.Docker.KeepRuntime)),
		"将更新: " + strings.Join(updates, ", "),
		"",
		"Enter/y 确认，Esc/n 取消",
	}
	if m.qaMode != "recheck" {
		lines = append(lines[:2], lines[3:]...)
	}
	return errorStyle.Render(strings.Join(lines, "\n"))
}

func messageStyle(message string) lipgloss.Style {
	lower := strings.ToLower(message)
	if strings.Contains(lower, "error") || strings.Contains(lower, "failed") || strings.Contains(message, "失败") || strings.Contains(message, "崩溃") {
		return errorStyle
	}
	return mutedStyle
}

func reportTypeForStage(stage string) string {
	switch stage {
	case "A":
		return "结构与规则证据"
	case "B":
		return "Docker运行时证据"
	case "C":
		return "测试运行证据"
	case "D":
		return "测试有效性报告"
	case "E":
		return "静态验收报告"
	case "F":
		return "标注员修复报告"
	default:
		return stage
	}
}

func cleanupImpactText(keepRuntime bool) string {
	if keepRuntime {
		return "按 keep-runtime 保留当前运行资源，并保留手动清理命令"
	}
	return "运行后清理 p2r 管理的 Docker 资源"
}

func yesNo(value bool) string {
	if value {
		return "是"
	}
	return "否"
}
