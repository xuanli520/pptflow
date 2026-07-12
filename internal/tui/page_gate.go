package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/purplevoid/harbor-factory/internal/harbor/nodes"
)

type gatePage struct{ pageBase }

func (p *gatePage) Update(msg tea.Msg) (bool, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok || p.m == nil {
		return false, nil
	}
	updated, cmd := p.m.updateGateKey(key)
	p.apply(updated)
	return true, cmd
}

func (p *gatePage) HandleKey(key tea.KeyMsg) tea.Cmd {
	_, cmd := p.Update(key)
	return cmd
}

func (p *gatePage) View(width, height int) string {
	p.m.width = width
	p.m.height = height
	return p.m.gateView()
}

func (m model) gateView() string {
	gate := m.activeGate
	if gate == nil && m.confirm != nil {
		gate = m.confirm.Gate
	}
	if gate == nil {
		return panelStyle.Width(contentWidth(m.width)).Render("暂无活跃审查关卡")
	}
	var lines []string
	lines = append(lines, sectionStyle.Render(localizeGate(gate.GateID, gate.GateName)))
	if strings.TrimSpace(m.filter) != "" {
		lines = append(lines, defaultTheme.Focused.Render("筛选："+redactUI(m.filter)))
	}
	lines = append(lines, redactUI(gate.Message))
	if m.err != nil {
		lines = append(lines, failStyle.Render(redactUI(localizeRuntimeError(m.err))))
	}
	checkLines := []string{sectionStyle.Render("检查清单"), renderChecklist(filterChecklist(gate.Checklist, m.filter))}
	artifactLines := []string{sectionStyle.Render("工件预览")}
	artifacts := m.visibleGateArtifacts(gate)
	if len(artifacts) > 0 {
		idx := clampInt(m.selectedArtifact, 0, len(artifacts)-1)
		artifact := artifacts[idx]
		artifactLines = append(artifactLines, sectionStyle.Render(localizeCount(idx+1, len(artifacts))+"："+redactUI(artifact.Name)))
		artifactLines = append(artifactLines, subtleStyle.Render(redactUI(artifact.Path)))
		previewLines := 14
		layout := layoutFor(m.width, m.height)
		if layout.Mode == layoutWide || layout.Mode == layoutMedium {
			previewLines = maxInt(8, layout.ContentHeight-8)
		}
		artifactLines = append(artifactLines, trimLines(m.artifactContent(artifact), previewLines))
	} else {
		artifactLines = append(artifactLines, subtleStyle.Render("当前关卡没有工件"))
	}
	lines = append(lines, "", joinResponsiveColumns(layoutFor(m.width, m.height), strings.Join(checkLines, "\n"), strings.Join(artifactLines, "\n")))
	lines = append(lines, "")
	if m.gateEditingNote {
		lines = append(lines, warnStyle.Render("正在编辑备注（Ctrl+S 保存，Esc 完成）："))
		if sanitized := redactUI(m.notesInput.Value()); sanitized != m.notesInput.Value() {
			lines = append(lines, sanitized)
		} else {
			lines = append(lines, m.notesInput.View())
		}
	} else if m.gateNotes != "" {
		lines = append(lines, "审查备注："+redactUI(m.gateNotes))
	}
	if m.readOnly {
		lines = append(lines, subtleStyle.Render("只读快照  [Tab] 下一个工件  [1] 总览  [3] 日志"))
		return panelStyle.Width(contentWidth(m.width)).Render(strings.Join(lines, "\n"))
	}
	actions := "[a/Ctrl+A] 批准  [r/Ctrl+R] 拒绝"
	if gate.GateID == nodes.FinalReview {
		actions += "  [v] 修订并重新运行检查"
	} else if gate.GateID == nodes.ResultReview {
		actions += "  [v] 刷新截图证据"
	}
	lines = append(lines, subtleStyle.Render(actions+"  [Ctrl+N] 备注  [e] 编辑工件  [Tab] 下一工件"))
	return panelStyle.Width(contentWidth(m.width)).Render(strings.Join(lines, "\n"))
}
