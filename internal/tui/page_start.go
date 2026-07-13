package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type startPage struct{ pageBase }

func (p *startPage) Focus() {
	if p.m != nil {
		p.m.focusStartInput(p.m.startField)
	}
}

func (p *startPage) Blur() {
	if p.m == nil {
		return
	}
	for field, input := range p.m.startInputs {
		input.Blur()
		p.m.startInputs[field] = input
	}
}

func (p *startPage) Update(msg tea.Msg) (bool, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok || p.m == nil {
		return false, nil
	}
	updated, cmd := p.m.updateStartKey(key)
	p.apply(updated)
	return true, cmd
}

func (p *startPage) HandleKey(key tea.KeyMsg) tea.Cmd {
	_, cmd := p.Update(key)
	return cmd
}

func (p *startPage) View(width, height int) string {
	p.m.width = width
	p.m.height = height
	return p.m.startView()
}

func (m model) startView() string {
	lines := []string{sectionStyle.Render("启动工作流")}
	compact := layoutFor(m.width, m.height).Mode == layoutMinimal || (m.height > 0 && m.height < 20)
	if m.startStep == startStepBasic {
		lines = append(lines, defaultTheme.Focused.Render("● 1 基本配置")+subtleStyle.Render("  ○ 2 高级选项"))
		if !compact {
			lines = append(lines, "", "请选择运行模式并填写工作流来源。")
		}
		for _, field := range m.activeStartFields() {
			lines = append(lines, m.renderStartField(field))
		}
	} else {
		lines = append(lines,
			subtleStyle.Render("○ 1 基本配置  ")+defaultTheme.Focused.Render("● 2 高级选项"),
			"",
			"一次只展开一个配置分组；使用 F1～F4 或 Ctrl+←/→ 切换分组。",
			"",
		)
		var groupLines []string
		for index, group := range advancedGroups() {
			label := fmt.Sprintf("▸ %d %s", index+1, startGroupName(group))
			if group == m.selectedStartGroup {
				label = fmt.Sprintf("▾ %d %s", index+1, startGroupName(group))
				groupLines = append(groupLines, selectedStyle.Render(" "+label+" "))
			} else {
				groupLines = append(groupLines, subtleStyle.Render("  "+label))
			}
		}
		var fieldLines []string
		for _, field := range m.activeStartFields() {
			fieldLines = append(fieldLines, m.renderStartField(field))
		}
		lines = append(lines, joinResponsiveColumns(layoutFor(m.width, m.height), strings.Join(groupLines, "\n"), strings.Join(fieldLines, "\n")))
	}
	lines = flattenRenderedLines(lines)
	if m.height > 12 {
		available := m.height - 2 - 4 - lipgloss.Height(m.footer()) - 2
		maxLines := maxInt(4, available-1)
		if len(lines) > maxLines {
			selected := 0
			for i, line := range lines {
				if strings.Contains(line, "> ") {
					selected = i
					break
				}
			}
			start := clampInt(selected-maxLines/2, 0, len(lines)-maxLines)
			visible := append([]string{}, lines[start:start+maxLines]...)
			visible = append(visible, scrollIndicator(start, maxLines, len(lines)))
			lines = visible
		}
	}
	if m.err != nil {
		lines = append(lines, "", failStyle.Render(redactSingleLineUI(localizeRuntimeError(m.err))))
	}
	if len(m.pathSuggestions) > 1 {
		preview := m.pathSuggestions
		if len(preview) > 4 {
			preview = preview[:4]
		}
		lines = append(lines, subtleStyle.Render("路径候选："+redactSingleLineUI(strings.Join(preview, "  "))))
	}
	if m.startStep == startStepBasic {
		lines = append(lines, "", subtleStyle.Render("填写完成后按 Enter 进入高级选项"))
	} else {
		lines = append(lines, "", subtleStyle.Render("Enter 启动 · Ctrl+B 入队 · Esc 返回"))
	}
	return panelStyle.Width(contentWidth(m.width)).Render(strings.Join(lines, "\n"))
}

func flattenRenderedLines(blocks []string) []string {
	var lines []string
	for _, block := range blocks {
		lines = append(lines, strings.Split(block, "\n")...)
	}
	return lines
}

func (m model) renderStartField(field startField) string {
	if field == startFieldMode {
		return m.renderStartModeSelector()
	}
	lineWidth := styleContentWidth(contentWidth(m.width), panelStyle)
	layout := layoutFor(m.width, m.height)
	if m.startStep == startStepAdvanced && (layout.Mode == layoutWide || layout.Mode == layoutMedium) {
		rightStyle := lipgloss.NewStyle().PaddingLeft(2)
		lineWidth = styleContentWidth(maxInt(24, layout.MainWidth-3), rightStyle)
	}
	labelWidth := clampInt(lineWidth/2, 4, 30)
	valueWidth := lineWidth - 2 - labelWidth - 1 - 4 // prefix, separator, and "[ ]"
	if valueWidth < 1 {
		labelWidth = maxInt(1, labelWidth-(1-valueWidth))
		valueWidth = 1
	}
	prefix := "  "
	if field == m.startField {
		prefix = "> "
	}
	var value string
	switch field {
	case startFieldTaskDir:
		value = emptyDash(m.opts.TaskDir)
	case startFieldRepoURL:
		value = emptyDash(m.opts.RepoURL)
	case startFieldCommit:
		value = emptyDash(m.opts.Commit)
	case startFieldWorkspace:
		value = emptyDash(m.opts.Workspace)
	case startFieldTaskOutput:
		value = emptyDash(m.opts.TaskOutputDir)
	case startFieldTestsAnalysis:
		value = emptyDash(m.opts.TestsAnalysis)
	case startFieldQwenResult:
		value = emptyDash(m.opts.QwenResult)
	case startFieldOpusResult:
		value = emptyDash(m.opts.OpusResult)
	case startFieldQwenScreenshot:
		value = emptyDash(m.opts.QwenScreenshot)
	case startFieldOpusScreenshot:
		value = emptyDash(m.opts.OpusScreenshot)
	case startFieldOutput:
		value = emptyDash(m.opts.OutputDir)
	case startFieldVerifyDocker:
		value = checkbox(m.opts.VerifyDocker)
	case startFieldQualityCheck:
		value = checkbox(m.opts.QualityCheck)
	case startFieldQualityAgent:
		value = checkbox(m.opts.QualityAgent)
	case startFieldSimilarityCheck:
		value = checkbox(m.opts.SimilarityCheck)
	case startFieldSimilarityGitHub:
		value = checkbox(m.opts.SimilarityGitHub)
	case startFieldSimilarityThreshold:
		value = formatFloatInput(m.opts.SimilarityThreshold)
	case startFieldHistoryDirs:
		value = emptyDash(joinStartList(m.opts.SimilarityHistoryDirs))
	case startFieldTB3Dirs:
		value = emptyDash(joinStartList(m.opts.SimilarityTB3Dirs))
	case startFieldRunHarbor:
		value = checkbox(m.opts.RunHarbor)
	case startFieldHarborAgent:
		value = emptyDash(m.opts.HarborAgent)
	case startFieldQwenModel:
		value = emptyDash(m.opts.QwenModel)
	case startFieldOpusModel:
		value = emptyDash(m.opts.OpusModel)
	case startFieldQwenHarborBaseURL:
		value = emptyDash(m.opts.QwenHarborBaseURL)
	case startFieldOpusHarborBaseURL:
		value = emptyDash(m.opts.OpusHarborBaseURL)
	case startFieldHarborTimeout:
		value = formatIntInput(m.opts.HarborTimeout)
	case startFieldHarborSetupTimeout:
		value = formatIntInput(m.opts.HarborSetupTimeout)
	case startFieldHarborPreflight:
		value = checkbox(m.opts.HarborPreflight)
	case startFieldHarborConcurrency:
		value = formatIntInput(m.opts.HarborConcurrency)
	case startFieldHarborAttempts:
		value = formatIntInput(m.opts.HarborAttempts)
	case startFieldHarborInfraRetries:
		value = formatIntInput(m.opts.HarborInfraRetries)
	case startFieldPackage:
		value = checkbox(m.opts.Package)
	case startFieldTaskName:
		value = emptyDash(m.opts.TaskName)
	case startFieldCodeLang:
		value = emptyDash(m.opts.CodeLang)
	case startFieldTaskType:
		value = emptyDash(m.opts.TaskType)
	case startFieldApplication:
		value = emptyDash(m.opts.Application)
	case startFieldAHT:
		value = emptyDash(m.opts.AHT)
	case startFieldDescription:
		value = emptyDash(m.opts.Description)
	case startFieldZeroToOne:
		value = checkbox(m.opts.IsZeroToOne)
	case startFieldCodexModel:
		value = emptyDash(m.opts.Model)
	case startFieldCodexReasoning:
		value = emptyDash(m.opts.Reasoning)
	case startFieldCodexPath:
		value = emptyDash(m.opts.CodexPath)
	case startFieldAgentTimeout:
		value = formatIntInput(m.opts.AgentTimeout)
	default:
		value = "-"
	}
	localizedLabel := localizeField(field)
	if unit := fieldUnit(field); unit != "" {
		localizedLabel += " (" + unit + ")"
	}
	if isTextStartField(field) {
		if field == m.startField {
			if input, ok := m.startInputs[field]; ok {
				if sanitized := redactSingleLineUI(input.Value()); sanitized != input.Value() {
					value = boxedText(sanitized, valueWidth+4)
				} else {
					value = boxedTextInput(input, valueWidth+4)
				}
			} else {
				value = boxedText(redactSingleLineUI(value), valueWidth+4)
			}
		} else {
			value = boxedText(redactSingleLineUI(value), valueWidth+4)
		}
	}
	localizedLabel = truncateDisplay(localizedLabel, labelWidth)
	return clipDisplay(prefix+padRightDisplay(localizedLabel, labelWidth)+" "+value, lineWidth)
}

func (m model) renderStartModeSelector() string {
	prefix := "  "
	if m.startField == startFieldMode {
		prefix = "> "
	}
	existing := "  运行已有任务"
	generate := "  从仓库生成"
	if m.startMode == startGenerateTask {
		generate = selectedStyle.Render("▶ 从仓库生成")
		existing = subtleStyle.Render(existing)
	} else {
		existing = selectedStyle.Render("▶ 运行已有任务")
		generate = subtleStyle.Render(generate)
	}
	if layoutFor(m.width, m.height).Mode == layoutMinimal || (m.height > 0 && m.height < 20) {
		value := "◉ 运行已有任务  ○ 从仓库生成"
		if m.startMode == startGenerateTask {
			value = "○ 运行已有任务  ◉ 从仓库生成"
		}
		return prefix + padRightDisplay("模式", 12) + " " + value
	}
	return prefix + "模式\n    " + existing + "\n    " + generate
}
