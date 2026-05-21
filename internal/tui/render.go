package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/lipgloss"

	"github.com/xuanli520/p2r_tui/internal/pipeline"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
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

func taskStateStyle(state string) lipgloss.Style {
	switch strings.TrimSpace(state) {
	case model.TaskInspecting:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#00DDDD"))
	case model.TaskWaitingManual:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#DDAA00"))
	case model.TaskCompleted:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#00CC66"))
	default:
		return mutedStyle
	}
}

func renderHeader(m app) string {
	taskBoard := "[题目管理]"
	overview := "[总览]"
	if m.tab == panelTaskBoard {
		taskBoard = activeStyle.Render(taskBoard)
	} else if m.tab == panelOverview {
		overview = activeStyle.Render(overview)
	}
	mode := "模式: " + localizeMode(m.qaMode)
	settings := "[设置 Ctrl+/]"
	if m.settingsOpen {
		settings = activeStyle.Render(settings)
	} else {
		settings = mutedStyle.Render(settings)
	}
	return titleStyle.Render("p2r QA 工作台") + "  " + taskBoard + "  " + overview + "  " + settings + "  " + mutedStyle.Render(mode)
}

func renderTaskBoard(m app) string {
	layout := layoutFor(m.width, max(8, m.height-verticalChromeHeight(m)), false)
	if m.taskBoard == nil {
		return ""
	}
	m.taskBoard.prepareLayout(layout.contentWidth, layout.contentHeight)
	return m.taskBoard.WithJobs(m.activeJobs).View(layout.contentWidth, layout.contentHeight)
}

func renderOverview(m app) string {
	if m.overview == nil {
		return ""
	}
	return m.overview.Render()
}

func renderExecution(m app) string {
	taskID := m.selectedTaskID()
	if taskID == "" {
		return mutedStyle.Render("未选择已索引的项目\n请先执行 `p2r scan --path <projects-qa>`")
	}
	layout := layoutFor(m.width, max(8, m.height-verticalChromeHeight(m)), true)
	if layout.mode == layoutWide || layout.mode == layoutMedium {
		leftContentHeight := max(1, layout.contentHeight-panelStyle.GetVerticalFrameSize())
		left := renderPanel(layout.leftWidth, layout.contentHeight, renderExecutionLeft(m, max(8, layout.leftWidth-panelStyle.GetHorizontalFrameSize()), leftContentHeight))
		rightWidth := max(8, layout.rightWidth-panelStyle.GetHorizontalFrameSize())
		right := renderPanel(layout.rightWidth, layout.contentHeight, renderDetailContext(m, rightWidth)+"\n"+m.detail.View())
		return lipgloss.JoinHorizontal(lipgloss.Top, left, right)
	}
	stageSummary := renderPanel(layout.contentWidth, layout.stageHeight, renderStageSummary(m, max(8, layout.contentWidth-panelStyle.GetHorizontalFrameSize())))
	detailWidth := max(8, layout.contentWidth-panelStyle.GetHorizontalFrameSize())
	detail := renderPanel(layout.contentWidth, max(6, layout.contentHeight-layout.stageHeight), renderDetailContext(m, detailWidth)+"\n"+m.detail.View())
	return lipgloss.JoinVertical(lipgloss.Left, stageSummary, detail)
}

func renderPanel(width, height int, content string) string {
	contentWidth := max(1, width-panelStyle.GetHorizontalFrameSize())
	contentHeight := max(1, height-panelStyle.GetVerticalFrameSize())
	return panelStyle.Width(contentWidth).Height(contentHeight).Render(content)
}

func renderExecutionLeft(m app, width int, maxHeight int) string {
	if maxHeight <= 0 {
		return ""
	}
	width = max(1, width)
	info := []string{
		fmt.Sprintf("任务: %s", truncateMiddleDisplay(m.detailVM.TaskID, max(8, width-4))),
	}
	info = append(info, "模式: "+localizeMode(m.qaMode))
	if m.qaMode == "recheck" {
		info = append(info, "参考运行: "+empty(m.selectedRefRun(), "-"))
	}
	if m.detailVM.HasRun {
		info = append(info,
			fmt.Sprintf("运行: %s", truncateMiddleDisplay(m.detailVM.Run.RunID, max(8, width-4))),
			fmt.Sprintf("状态: %s", localizeRunStatus(m.detailVM.Run.Status)),
		)
	} else {
		info = append(info, "运行: 未生成")
	}

	listBudget := 0
	if len(m.detailVM.Stages) > 0 || m.qaMode == "recheck" {
		listBudget = 1
		if maxHeight >= 4 {
			listBudget = max(2, maxHeight/2)
		}
	}
	infoBudget := maxHeight
	if listBudget > 0 {
		infoBudget = max(1, maxHeight-listBudget)
	}
	lines := append([]string{}, info[:min(len(info), infoBudget)]...)
	remaining := maxHeight - len(lines)
	if remaining <= 0 {
		return joinLimitedLines(lines, maxHeight)
	}
	if m.qaMode != "recheck" {
		lines = append(lines, renderStageSection(m, width, remaining)...)
		return joinLimitedLines(lines, maxHeight)
	}

	if m.focus == focusRefRunList {
		if remaining >= 9 {
			stageCap := min(len(m.detailVM.Stages)+2, remaining/2)
			lines = append(lines, renderStageSection(m, width, stageCap)...)
			remaining = maxHeight - len(lines)
		}
		lines = append(lines, renderRefRunSection(m, width, remaining)...)
		return joinLimitedLines(lines, maxHeight)
	}

	stageCap := remaining
	if len(m.detailVM.RefRuns) > 0 && remaining >= 10 {
		stageCap = min(len(m.detailVM.Stages)+2, remaining/2)
	}
	lines = append(lines, renderStageSection(m, width, stageCap)...)
	remaining = maxHeight - len(lines)
	if remaining > 0 && len(m.detailVM.RefRuns) > 0 {
		lines = append(lines, renderRefRunSection(m, width, remaining)...)
	}
	return joinLimitedLines(lines, maxHeight)
}

func renderStageSection(m app, width int, maxLines int) []string {
	if maxLines <= 0 {
		return nil
	}
	var items []string
	for index, stage := range m.detailVM.Stages {
		items = append(items, renderStageLine(stage, index == m.stageIndex, width))
	}
	if len(items) == 0 {
		items = []string{"阶段: 未生成"}
	}
	if maxLines >= 3 {
		return append([]string{"阶段:"}, visibleWindowLines(items, m.stageIndex, maxLines-1)...)
	}
	return visibleWindowLines(items, m.stageIndex, maxLines)
}

func renderRefRunSection(m app, width int, maxLines int) []string {
	if maxLines <= 0 {
		return nil
	}
	items := []string{"  无可用参考运行"}
	if len(m.detailVM.RefRuns) > 0 {
		items = nil
		for index, run := range m.detailVM.RefRuns {
			prefix := "  "
			line := fmt.Sprintf("%s %s", run.RunID, localizeRunStatus(run.Status))
			if index == m.refIndex {
				prefix = "> "
				if m.focus == focusRefRunList {
					line = selectedStyle.Render(truncateDisplay(line, max(8, width-2)))
					items = append(items, prefix+line)
					continue
				}
			}
			items = append(items, prefix+truncateDisplay(line, max(8, width-2)))
		}
	}
	if maxLines >= 3 {
		return append([]string{"参考运行列表:"}, visibleWindowLines(items, m.refIndex, maxLines-1)...)
	}
	return visibleWindowLines(items, m.refIndex, maxLines)
}

func visibleWindowLines(items []string, selected int, maxLines int) []string {
	if maxLines <= 0 || len(items) == 0 {
		return nil
	}
	selected = clamp(selected, 0, len(items)-1)
	if len(items) <= maxLines {
		return append([]string{}, items...)
	}
	if maxLines == 1 {
		return []string{items[selected]}
	}
	itemSlots := maxLines - 1
	if selected < len(items)-1 && selected > 0 && maxLines >= 3 {
		itemSlots = maxLines - 2
	}
	itemSlots = max(1, itemSlots)
	start := clamp(selected-itemSlots+1, 0, max(0, len(items)-itemSlots))
	end := min(len(items), start+itemSlots)
	above := start > 0
	below := end < len(items)
	lines := make([]string, 0, maxLines)
	if above {
		lines = append(lines, mutedStyle.Render("↑"))
	}
	lines = append(lines, items[start:end]...)
	if below && len(lines) < maxLines {
		lines = append(lines, mutedStyle.Render("↓"))
	}
	return lines
}

func joinLimitedLines(lines []string, maxLines int) string {
	if maxLines <= 0 {
		return ""
	}
	if len(lines) > maxLines {
		lines = lines[:maxLines]
	}
	rendered := strings.Join(lines, "\n")
	parts := strings.Split(rendered, "\n")
	if len(parts) > maxLines {
		parts = parts[:maxLines]
	}
	return strings.Join(parts, "\n")
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

func renderDetailContext(m app, width int) string {
	stage := m.selectedStage()
	if stage.Stage == "" {
		return mutedStyle.Render(truncateDisplay("当前: 未选择阶段", width))
	}
	icon, _ := stageStatusIcon(stage.Status)
	jobID := "-"
	mode := "-"
	if job, ok := m.streamJobForTask(m.selectedTaskID()); ok {
		jobID = truncateMiddleDisplay(job.JobID, 18)
		if stream, ok := m.detailVM.StreamByStage[stage.Stage]; ok {
			mode = streamModeLabel(stream.Mode)
		} else if stream, ok := job.StreamByStage[stage.Stage]; ok {
			mode = streamModeLabel(stream.Mode)
		}
	}
	line := fmt.Sprintf("当前: %s %s  %s %s  耗时: %dms  run: %s  job: %s  模式: %s", stage.Stage, stage.DisplayName, icon, localizeStageStatus(stage.Status), stage.DurationMS, empty(m.detailVM.Run.RunID, "未生成"), jobID, mode)
	return mutedStyle.Render(truncateDisplay(line, width))
}

func streamModeLabel(mode pipeline.StreamMode) string {
	switch mode {
	case pipeline.StreamModeCumulative:
		return "Codex"
	case pipeline.StreamModeAppend:
		return "进程输出"
	default:
		return "-"
	}
}

func renderStageLine(stage stageView, selected bool, width int) string {
	icon, color := stageStatusIcon(stage.Status)
	status := lipgloss.NewStyle().Foreground(color).Render(icon)
	reason := localizeSummary(stage.ErrorSummary)
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
	plan := m.rerunStagePlan()
	stages := plan.displayStages
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

func renderRunConfig(m app) string {
	c := m.runConfig
	plan := m.rerunStagePlan()
	width := clamp(m.width-4, 48, 88)
	if width <= 0 {
		width = 72
	}
	var lines []string
	focusIndex := 1
	lines = append(lines, titleStyle.Render("运行配置: "+truncateMiddleDisplay(c.taskID, max(12, width-16))))
	lines = append(lines, focusLine(c.focus == runConfigFocusMode, "模式: "+localizeMode(c.mode)))
	if c.mode == "recheck" {
		lines = append(lines, "  参考运行: "+empty(c.refRun, "-"))
	}
	lines = append(lines, "")
	stageHeaderIndex := len(lines)
	lines = append(lines, focusLine(c.focus == runConfigFocusStages, "阶段:"))
	for i, stage := range model.AllStages() {
		checked := runConfigStageChecked(c, stage, plan)
		mark := "[ ]"
		if checked {
			mark = "[✓]"
		}
		text := fmt.Sprintf("  %s %s - %s", mark, stage, localizeStageName(stage, ""))
		if c.focus == runConfigFocusStages && c.stageIndex == i {
			focusIndex = len(lines)
			text = selectedStyle.Render(truncateDisplay("> "+text, width-4))
		}
		lines = append(lines, text)
	}
	if c.focus == runConfigFocusStages && focusIndex == 1 {
		focusIndex = stageHeaderIndex
	}
	if c.fromStage != "" {
		lines = append(lines, mutedStyle.Render("使用起始阶段时不能多选阶段"))
	}
	if len(c.stages) > 0 {
		lines = append(lines, mutedStyle.Render("多选阶段时不能使用起始阶段"))
	}
	if c.focus == runConfigFocusFrom {
		focusIndex = len(lines)
	}
	lines = append(lines, focusLine(c.focus == runConfigFocusFrom, "起始阶段: "+empty(c.fromStage, "未设置")))
	if c.focus == runConfigFocusKeepRuntime {
		focusIndex = len(lines)
	}
	lines = append(lines, focusLine(c.focus == runConfigFocusKeepRuntime, "保留运行时: "+yesNo(c.keepRuntime)))
	lines = append(lines, fmt.Sprintf("  补充文档: 已托管附件 %d 个", c.attachedCount))
	input := c.input.View()
	if c.focus == runConfigFocusExtraDocs {
		focusIndex = len(lines)
		input = selectedStyle.Render(truncateDisplay(input, width-4))
	}
	lines = append(lines, input)
	if c.err != "" {
		lines = append(lines, errorStyle.Render(truncateDisplay(c.err, width-4)))
	}
	if plan.blockedReason != "" && c.err == "" {
		lines = append(lines, errorStyle.Render(truncateDisplay(plan.blockedReason, width-4)))
	}
	stageText := strings.Join(plan.displayStages, ", ")
	if stageText == "" {
		stageText = "-"
	}
	lines = append(lines, mutedStyle.Render("将运行阶段: "+stageText))
	lines = append(lines, "")
	if c.focus == runConfigFocusSubmit {
		focusIndex = len(lines)
	}
	submit := focusLine(c.focus == runConfigFocusSubmit, "[Enter] 确认")
	if c.focus == runConfigFocusCancel {
		focusIndex = len(lines)
	}
	cancel := focusLine(c.focus == runConfigFocusCancel, "[Esc] 取消")
	lines = append(lines, submit+"  "+cancel+"  "+mutedStyle.Render("[Tab] 切换"))

	panelHeight := len(lines) + panelStyle.GetVerticalFrameSize()
	if m.height > 0 {
		reserved := verticalChromeHeight(m)
		panelHeight = min(panelHeight, max(3, m.height-reserved))
	}
	contentHeight := max(1, panelHeight-panelStyle.GetVerticalFrameSize())
	lines = visibleRunConfigLines(lines, focusIndex, contentHeight)
	return renderPanel(width, panelHeight, strings.Join(lines, "\n"))
}

func visibleRunConfigLines(lines []string, focusIndex int, maxLines int) []string {
	if maxLines <= 0 {
		return nil
	}
	if len(lines) <= maxLines {
		return append([]string{}, lines...)
	}
	if len(lines) == 0 {
		return nil
	}
	body := visibleWindowLines(lines[1:], max(0, focusIndex-1), max(1, maxLines-1))
	return append([]string{lines[0]}, body...)
}

func focusLine(focused bool, line string) string {
	if focused {
		return selectedStyle.Render("> " + line)
	}
	return "  " + line
}

func runConfigStageChecked(c runConfig, stage string, plan stagePlan) bool {
	if len(c.stages) > 0 {
		return c.stages[stage]
	}
	for _, selected := range plan.displayStages {
		if selected == stage {
			return true
		}
	}
	return false
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
