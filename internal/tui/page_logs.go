package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

type logsPage struct{ pageBase }

func (p *logsPage) Update(msg tea.Msg) (bool, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok || p.m == nil {
		return false, nil
	}
	updated, cmd := p.m.updateLogsKey(key)
	p.apply(updated)
	return true, cmd
}

func (p *logsPage) HandleKey(key tea.KeyMsg) tea.Cmd {
	_, cmd := p.Update(key)
	return cmd
}

func (p *logsPage) View(width, height int) string {
	p.m.width = width
	p.m.height = height
	return p.m.logsView()
}

func (m model) logsView() string {
	var lines []string
	lineWidth := styleContentWidth(contentWidth(m.width), panelStyle)
	lines = append(lines, sectionStyle.Render("日志"))
	if strings.TrimSpace(m.filter) != "" {
		lines = append(lines, defaultTheme.Focused.Render(clipDisplay("筛选："+redactSingleLineUI(m.filter), lineWidth)))
	}
	if m.err != nil {
		lines = append(lines, failStyle.Render(clipDisplay(redactSingleLineUI(localizeRuntimeError(m.err)), lineWidth)))
	}
	eventLimit := 18
	if m.height > 0 {
		eventLimit = maxInt(3, m.height-logPreviewLines(m.height)-18)
	}
	start := 0
	if m.filter == "" && len(m.events) > eventLimit {
		start = len(m.events) - eventLimit
	}
	var eventLines []string
	for _, event := range m.events[start:] {
		node := event.NodeID
		if node == "" {
			node = "run"
		}
		if !matchesFilter(m.filter, node, localizeNode(node), event.Message, event.Type) {
			continue
		}
		prefix := fmt.Sprintf("%s %s ", event.CreatedAt.Format("15:04:05"), padRightDisplay(localizeNode(node)+" ("+node+")", 26))
		eventLines = append(eventLines, clipDisplay(prefix+clipDisplay(redactSingleLineUI(event.Message), maxInt(0, lineWidth-ansi.StringWidth(prefix))), lineWidth))
	}
	if len(eventLines) > eventLimit {
		eventLines = eventLines[len(eventLines)-eventLimit:]
	}
	lines = append(lines, eventLines...)
	if len(eventLines) == 0 {
		lines = append(lines, subtleStyle.Render("暂无日志"))
	}
	files := m.visibleLogArtifacts()
	if len(files) > 0 {
		idx := m.selectedLogFile
		if idx < 0 || idx >= len(files) {
			idx = 0
		}
		artifact := files[idx]
		lines = append(lines, "")
		mode := "顶部"
		if m.logTail {
			mode = "尾部跟踪"
		}
		lines = append(lines, sectionStyle.Render(clipDisplay(fmt.Sprintf("文件 %d/%d：%s [%s]", idx+1, len(files), redactSingleLineUI(artifact.Name), mode), lineWidth)))
		lines = append(lines, subtleStyle.Render(clipDisplay(redactSingleLineUI(artifact.Path), lineWidth)))
		content := m.logArtifactContent(artifact)
		offset := m.logContentOffset(content, logPreviewLines(m.height))
		lines = append(lines, trimLinesFrom(content, logPreviewLines(m.height), offset))
		lines = append(lines, scrollIndicator(offset, logPreviewLines(m.height), len(strings.Split(content, "\n"))))
		lines = append(lines, subtleStyle.Render("[Tab] 下一文件  [j/k] 滚动  [PgUp/PgDn] 翻页  [t] 尾部跟踪"))
	}
	return panelStyle.Width(contentWidth(m.width)).Render(strings.Join(lines, "\n"))
}
