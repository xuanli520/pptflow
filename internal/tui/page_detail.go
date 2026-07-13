package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type detailPage struct{ pageBase }

func (p *detailPage) Update(msg tea.Msg) (bool, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok || p.m == nil {
		return false, nil
	}
	switch key.String() {
	case "tab":
		p.m.selectNextArtifact()
	case "shift+tab":
		p.m.selectPrevArtifact()
	case "2":
		if p.m.activeGate != nil {
			p.m.setView(viewGate)
		}
	case "3", "l":
		p.m.setView(viewLogs)
	case "4":
		if p.m.done {
			p.m.setView(viewDone)
		} else {
			return true, p.m.showToast("运行尚未完成", toastWarning)
		}
	case "j", "down":
		p.m.selectNextNode()
		p.m.detailScroll = 0
	case "k", "up":
		p.m.selectPrevNode()
		p.m.detailScroll = 0
	case "pgdown":
		if p.m.syncDetailViewport() {
			p.m.detailViewport.PageDown()
			p.m.detailScroll = p.m.detailViewport.YOffset
		}
	case "pgup":
		if p.m.syncDetailViewport() {
			p.m.detailViewport.PageUp()
			p.m.detailScroll = p.m.detailViewport.YOffset
		}
	case "home", "g":
		if p.m.syncDetailViewport() {
			p.m.detailViewport.GotoTop()
			p.m.detailScroll = p.m.detailViewport.YOffset
		}
	case "end", "G":
		if p.m.syncDetailViewport() {
			p.m.detailViewport.GotoBottom()
			p.m.detailScroll = p.m.detailViewport.YOffset
		}
	default:
		return false, nil
	}
	return true, nil
}

func (m *model) syncDetailViewport() bool {
	artifact, ok := m.selectedNodeArtifact()
	if !ok {
		return false
	}
	visible := detailPreviewLines(m.height)
	m.detailViewport.Width = maxInt(20, contentWidth(m.width)-6)
	m.detailViewport.Height = maxInt(3, visible)
	m.detailViewport.MouseWheelEnabled = true
	m.detailViewport.MouseWheelDelta = 3
	m.detailViewport.SetContent(m.artifactContent(artifact))
	m.detailViewport.SetYOffset(m.detailScroll)
	return true
}

func (p *detailPage) HandleKey(key tea.KeyMsg) tea.Cmd {
	_, cmd := p.Update(key)
	return cmd
}

func (p *detailPage) View(width, height int) string {
	p.m.width = width
	p.m.height = height
	return p.m.nodeDetailView()
}

func (m model) nodeDetailView() string {
	lines := []string{sectionStyle.Render("节点详情")}
	if strings.TrimSpace(m.filter) != "" {
		lines = append(lines, defaultTheme.Focused.Render("筛选："+redactSingleLineUI(m.filter)))
	}
	if m.err != nil {
		lines = append(lines, failStyle.Render(redactSingleLineUI(localizeRuntimeError(m.err))))
	}
	if len(m.nodes) == 0 {
		lines = append(lines, subtleStyle.Render("暂无节点事件"))
		return panelStyle.Width(contentWidth(m.width)).Render(strings.Join(lines, "\n"))
	}
	layout := layoutFor(m.width, m.height)
	nodeLineWidth := styleContentWidth(contentWidth(m.width), panelStyle)
	artifactLineWidth := nodeLineWidth
	if layout.Mode == layoutWide || layout.Mode == layoutMedium {
		nodeLineWidth = maxInt(1, layout.SidebarWidth-2)
		artifactLineWidth = styleContentWidth(maxInt(24, layout.MainWidth-3), lipgloss.NewStyle().PaddingLeft(2))
	}
	nodeLines := []string{sectionStyle.Render("节点")}
	selectedLine := 0
	for _, id := range nodeOrder() {
		event, ok := m.nodes[id]
		if !ok {
			continue
		}
		if !matchesFilter(m.filter, id, localizeNode(id), event.Message, event.Status) {
			continue
		}
		prefix := "  "
		if id == m.selectedNode {
			prefix = "> "
			selectedLine = len(nodeLines)
		}
		nodeLines = append(nodeLines, clipDisplay(fmt.Sprintf("%s%s %s (%s) %s", prefix, statusIcon(event.Status), localizeNode(id), id, redactSingleLineUI(event.Message)), nodeLineWidth))
		if id == m.selectedNode && event.Path != "" {
			nodeLines = append(nodeLines, subtleStyle.Render(clipDisplay("    "+redactSingleLineUI(event.Path), nodeLineWidth)))
		}
	}
	nodeLimit := 12
	if layout.Mode == layoutWide || layout.Mode == layoutMedium {
		nodeLimit = clampInt(layout.ContentHeight-5, 5, 24)
	} else if m.height > 0 {
		nodeLimit = clampInt(m.height-detailPreviewLines(m.height)-15, 3, 12)
	}
	if len(nodeLines) > nodeLimit {
		start := clampInt(selectedLine-nodeLimit/2, 0, len(nodeLines)-nodeLimit)
		nodeLines = append(append([]string{}, nodeLines[start:start+nodeLimit]...), scrollIndicator(start, nodeLimit, len(nodeLines)))
	}
	artifactLines := []string{sectionStyle.Render("工件预览")}
	if artifacts := m.nodeArtifacts(m.selectedNode); len(artifacts) > 0 {
		idx := m.selectedArtifact
		if idx < 0 || idx >= len(artifacts) {
			idx = 0
		}
		artifact := artifacts[idx]
		artifactLines = append(artifactLines, sectionStyle.Render(clipDisplay(fmt.Sprintf("%d/%d：%s", idx+1, len(artifacts), redactSingleLineUI(artifact.Name)), artifactLineWidth)))
		artifactLines = append(artifactLines, subtleStyle.Render(clipDisplay(redactSingleLineUI(artifact.Path), artifactLineWidth)))
		m.syncDetailViewport()
		if layout.Mode == layoutWide || layout.Mode == layoutMedium {
			m.detailViewport.Width = maxInt(24, layout.MainWidth-7)
			m.detailViewport.Height = maxInt(5, layout.ContentHeight-7)
		}
		artifactLines = append(artifactLines, m.detailViewport.View())
		artifactLines = append(artifactLines, scrollIndicator(m.detailViewport.YOffset, m.detailViewport.Height, m.detailViewport.TotalLineCount()))
	} else {
		artifactLines = append(artifactLines, subtleStyle.Render("当前节点没有可预览工件"))
	}
	lines = append(lines, joinResponsiveColumns(layout, strings.Join(nodeLines, "\n"), strings.Join(artifactLines, "\n")))
	lines = append(lines, "")
	actions := "只读快照  [j/k] 选择节点  [PgUp/PgDn] 滚动  [Tab] 下一工件  [Ctrl+L] 日志"
	lines = append(lines, subtleStyle.Render(actions))
	return panelStyle.Width(contentWidth(m.width)).Render(strings.Join(lines, "\n"))
}
