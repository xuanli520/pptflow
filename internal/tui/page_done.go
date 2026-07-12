package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
)

type donePage struct{ pageBase }

func (p *donePage) Update(msg tea.Msg) (bool, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok || p.m == nil {
		return false, nil
	}
	switch key.String() {
	case "tab":
		p.m.cyclePage(1)
	case "ctrl+r":
		return true, p.m.openRunConfig(p.m.opts.Workspace, taskLabel(p.m.opts, p.m.opts.Workspace))
	case "ctrl+n":
		p.m.openStartForm(p.m.opts.Workspace)
	case "2":
		p.m.setView(viewNodeDetail)
	case "3", "l":
		p.m.setView(viewLogs)
	default:
		return false, nil
	}
	return true, nil
}

func (p *donePage) HandleKey(key tea.KeyMsg) tea.Cmd {
	_, cmd := p.Update(key)
	return cmd
}

func (p *donePage) View(width, height int) string {
	p.m.width = width
	p.m.height = height
	return p.m.doneView()
}

func (m model) doneView() string {
	var lines []string
	lines = append(lines, sectionStyle.Render("完成"))
	if !m.done {
		lines = append(lines, "运行仍在进行中。")
		return panelStyle.Width(contentWidth(m.width)).Render(strings.Join(lines, "\n"))
	}
	if m.err != nil {
		lines = append(lines, failStyle.Render("失败："+redactUI(localizeRuntimeError(m.err))))
	} else if !m.summary.Passed {
		lines = append(lines, failStyle.Render("已完成，但有检查未通过。"))
		if event, ok := m.lastFailureEvent(); ok {
			node := event.NodeID
			if node == "" {
				node = "run"
			}
			lines = append(lines, fmt.Sprintf("最后失败：%s：%s（%s）", redactUI(node), redactUI(event.Message), redactUI(localizeNode(node))))
			if strings.TrimSpace(event.Path) != "" {
				lines = append(lines, subtleStyle.Render("  "+redactUI(event.Path)))
			}
		}
	} else {
		lines = append(lines, passStyle.Render("已成功完成。"))
	}
	if m.summary.Recovered {
		lines = append(lines, fmt.Sprintf("恢复运行: %s -> %s", redactUI(m.summary.PreviousRunID), redactUI(m.summary.RunID)))
		if len(m.summary.ReusedNodes) > 0 {
			lines = append(lines, "复用节点: "+redactUI(strings.Join(m.summary.ReusedNodes, ", ")))
		}
		if len(m.summary.RerunNodes) > 0 {
			lines = append(lines, "重跑节点: "+redactUI(strings.Join(m.summary.RerunNodes, ", ")))
		}
	}
	if m.summary.RepoPrepared != nil {
		lines = append(lines, "仓库提交: "+redactUI(m.summary.RepoPrepared.ResolvedCommit))
		lines = append(lines, subtleStyle.Render("源码路径: "+redactUI(m.summary.RepoPrepared.SourcePath)))
	}
	if m.summary.LintReport != nil {
		lines = append(lines, fmt.Sprintf("代码检查通过: %s（%d 项检查）", localizeBool(m.summary.LintReport.Passed), len(m.summary.LintReport.Checks)))
		for _, check := range m.summary.LintReport.Checks {
			if check.Status == domain.CheckFail || check.Status == domain.CheckWarn {
				lines = append(lines, fmt.Sprintf("  %s %s: %s", statusIcon(string(check.Status)), check.ID, redactUI(check.Message)))
			}
		}
	}
	if m.summary.GenReport != nil {
		lines = append(lines, fmt.Sprintf("生成任务: %s", redactUI(m.summary.GenReport.TaskDir)))
		lines = append(lines, fmt.Sprintf("任务提案: %s", redactUI(m.summary.GenReport.TaskProposal.TaskName)))
		lines = append(lines, subtleStyle.Render("测试分析: "+redactUI(m.summary.GenReport.TestsAnalysisPath)))
	}
	if m.summary.VerifyReport != nil {
		lines = append(lines, fmt.Sprintf("验证通过: %s", localizeBool(m.summary.VerifyReport.Passed)))
		lines = append(lines, fmt.Sprintf("初始状态暴露问题: %s", localizeBool(m.summary.VerifyReport.InitialExposesIssue)))
	}
	if m.summary.QualityReport != nil {
		lines = append(lines, fmt.Sprintf("质量检查通过: %s（%d 项检查）", localizeBool(m.summary.QualityReport.OverallPass), len(m.summary.QualityReport.Checks)))
		for _, issue := range m.summary.QualityReport.Issues {
			lines = append(lines, "  "+failStyle.Render(redactUI(issue)))
		}
	}
	if m.summary.SimilarityReport != nil {
		lines = append(lines, fmt.Sprintf("相似度检查通过: %s（最高 %.3f）", localizeBool(m.summary.SimilarityReport.OverallPass), m.summary.SimilarityReport.MaxScore))
		for _, issue := range m.summary.SimilarityReport.Issues {
			lines = append(lines, "  "+failStyle.Render(redactUI(issue)))
		}
	}
	if m.summary.QwenResult != nil {
		lines = append(lines, fmt.Sprintf("Qwen pass@4: %.2f（%d/%d），平均轮次 %.1f", m.summary.QwenResult.PassAt4, m.summary.QwenResult.PassCount, m.summary.QwenResult.Trials, m.summary.QwenResult.AverageTurns))
	}
	if m.summary.OpusResult != nil {
		lines = append(lines, fmt.Sprintf("Opus pass@4: %.2f（%d/%d），平均轮次 %.1f", m.summary.OpusResult.PassAt4, m.summary.OpusResult.PassCount, m.summary.OpusResult.Trials, m.summary.OpusResult.AverageTurns))
	}
	if m.summary.PackageReport != nil {
		lines = append(lines, fmt.Sprintf("打包文件: %s", redactUI(m.summary.PackageReport.OutputZip)))
		lines = append(lines, fmt.Sprintf("提交报告: %s", redactUI(m.summary.PackageReport.ReportPath)))
	}
	lines = append(lines, "", subtleStyle.Render("Esc 返回工作区中枢  Ctrl+R 重跑  Ctrl+N 基于此配置新建"))
	return panelStyle.Width(contentWidth(m.width)).Render(strings.Join(lines, "\n"))
}
