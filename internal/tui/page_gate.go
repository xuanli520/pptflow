package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
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

func (m *model) gateView() string {
	gate := m.activeGate
	if gate == nil && m.confirm != nil {
		gate = m.confirm.Gate
	}
	if gate == nil {
		return panelStyle.Width(contentWidth(m.width)).Render("暂无活跃审查关卡")
	}
	m.syncGateViewport(gate)
	body := m.gateViewport.View()
	if gateViewportShowsIndicator(m.gateViewport.Height, m.gatePanelInnerHeight()) {
		body += "\n" + scrollIndicator(m.gateViewport.YOffset, m.gateViewport.Height, m.gateViewport.TotalLineCount())
	}
	return panelStyle.Width(contentWidth(m.width)).Render(body)
}

func (m *model) syncGateViewport(gate *domain.GateRequest) bool {
	if gate == nil {
		return false
	}
	innerHeight := m.gatePanelInnerHeight()
	viewportHeight := innerHeight
	if innerHeight >= 2 {
		viewportHeight--
	}
	viewportWidth := maxInt(1, contentWidth(m.width)-panelStyle.GetHorizontalFrameSize())
	m.gateViewport.Width = viewportWidth
	m.gateViewport.Height = maxInt(1, viewportHeight)
	m.gateViewport.MouseWheelEnabled = true
	m.gateViewport.MouseWheelDelta = 3
	content := lipgloss.NewStyle().Width(viewportWidth).MaxWidth(viewportWidth).Render(m.gateContent(gate))
	m.gateViewport.SetContent(content)
	m.gateViewport.SetYOffset(m.gateScroll)
	m.gateScroll = m.gateViewport.YOffset
	return true
}

func (m model) gatePanelInnerHeight() int {
	height := m.height
	if height <= 0 {
		height = 24
	}
	chromeHeight := lipgloss.Height(m.header()) + lipgloss.Height(m.footer())
	if status := m.statusBar(); status != "" {
		chromeHeight += lipgloss.Height(status)
	}
	if toast := m.renderToast(); toast != "" {
		chromeHeight += lipgloss.Height(toast)
	}
	panelHeight := maxInt(panelStyle.GetVerticalFrameSize()+1, height-chromeHeight)
	return maxInt(1, panelHeight-panelStyle.GetVerticalFrameSize())
}

func gateViewportShowsIndicator(viewportHeight, innerHeight int) bool {
	return innerHeight >= 2 && viewportHeight < innerHeight
}

func (m model) gateContent(gate *domain.GateRequest) string {
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
		content := m.artifactContent(artifact)
		if strings.TrimSpace(content) == "" {
			content = subtleStyle.Render("（内容为空或不可用）")
		}
		artifactLines = append(artifactLines, content)
	} else {
		artifactLines = append(artifactLines, subtleStyle.Render("当前关卡没有工件"))
	}
	lines = append(lines, "", joinResponsiveColumns(layoutFor(m.width, m.height), strings.Join(checkLines, "\n"), strings.Join(artifactLines, "\n")))
	lines = append(lines, "")
	if m.gateEditingNote {
		lines = append(lines, warnStyle.Render("正在编辑审查备注 / Codex 返修指导（Ctrl+S 保存，Esc 完成）："))
		if sanitized := redactUI(m.notesInput.Value()); sanitized != m.notesInput.Value() {
			lines = append(lines, sanitized)
		} else {
			lines = append(lines, m.notesInput.View())
		}
	} else if m.gateNotes != "" {
		lines = append(lines, "审查备注 / Codex 指导："+redactUI(m.gateNotes))
	}
	return strings.Join(lines, "\n")
}

func (m *model) scrollGate(key string) bool {
	if !m.syncGateViewport(m.activeGate) {
		return false
	}
	switch key {
	case "j", "down":
		m.gateViewport.LineDown(1)
	case "k", "up":
		m.gateViewport.LineUp(1)
	case "pgdown":
		m.gateViewport.PageDown()
	case "pgup":
		m.gateViewport.PageUp()
	case "home", "g":
		m.gateViewport.GotoTop()
	case "end", "G":
		m.gateViewport.GotoBottom()
	default:
		return false
	}
	m.gateScroll = m.gateViewport.YOffset
	return true
}
