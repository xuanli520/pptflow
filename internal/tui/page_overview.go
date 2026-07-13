package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
)

type overviewPage struct{ pageBase }

func (p *overviewPage) Focus() {
	if p.m != nil {
		p.m.overviewTable.Focus()
	}
}

func (p *overviewPage) Blur() {
	if p.m != nil {
		p.m.overviewTable.Blur()
	}
}

func (p *overviewPage) Update(msg tea.Msg) (bool, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok || p.m == nil {
		return false, nil
	}
	switch key.String() {
	case "tab":
		p.m.cyclePage(1)
	case "2":
		if p.m.activeGate != nil {
			p.m.setView(viewGate)
		} else {
			p.m.setView(viewNodeDetail)
		}
	case "3", "l":
		p.m.setView(viewLogs)
	case "4":
		if p.m.done {
			p.m.setView(viewDone)
		} else {
			return true, p.m.showToast("运行尚未完成", toastWarning)
		}
	case "d":
		p.m.setView(viewNodeDetail)
	case "g":
		if p.m.activeGate != nil {
			p.m.setView(viewGate)
		} else {
			return true, p.m.showToast("当前没有活跃审查关卡", toastWarning)
		}
	case "j", "down", "k", "up", "pgdown", "pgup", "home", "end":
		p.m.syncOverviewTable()
		var cmd tea.Cmd
		p.m.overviewTable, cmd = p.m.overviewTable.Update(key)
		p.m.syncSelectedOverviewRow()
		return true, cmd
	case "enter":
		if p.m.selectedNode != "" {
			p.m.setView(viewNodeDetail)
		}
	case "r":
		return true, p.m.confirmSelectedNodeRetry()
	default:
		return false, nil
	}
	return true, nil
}

func (m *model) syncSelectedOverviewRow() {
	cursor := m.overviewTable.Cursor()
	if cursor >= 0 && cursor < len(m.overviewRowIDs) {
		m.selectedNode = m.overviewRowIDs[cursor]
	}
}

func (m *model) syncOverviewTable() {
	l := layoutFor(m.width, m.height)
	columns := []table.Column{{Title: "状态", Width: 8}, {Title: "节点", Width: 30}, {Title: "消息", Width: maxInt(20, l.ContentWidth-48)}}
	if l.Mode == layoutMinimal {
		columns = []table.Column{{Title: "状态", Width: 6}, {Title: "节点", Width: maxInt(18, l.ContentWidth-12)}}
	}
	rows := make([]table.Row, 0, len(nodeOrder()))
	ids := make([]string, 0, len(nodeOrder()))
	selected := -1
	for _, id := range nodeOrder() {
		event, ok := m.nodes[id]
		status, message := "pending", ""
		if ok {
			status, message = event.Status, redactSingleLineUI(event.Message)
		}
		if !matchesFilter(m.filter, id, localizeNode(id), message, status, localizeStatus(status)) {
			continue
		}
		// bubbles/table truncates cell values before styling and is not ANSI
		// aware. Keep table data plain; the selected-row style still provides
		// focus feedback, while detail views retain colored statusIcon output.
		row := table.Row{statusGlyph(status) + " " + localizeStatus(status), localizeNode(id) + " (" + id + ")", message}
		if l.Mode == layoutMinimal {
			row = row[:2]
		}
		if id == m.selectedNode {
			selected = len(rows)
		}
		rows = append(rows, row)
		ids = append(ids, id)
	}
	m.overviewRowIDs = ids
	m.overviewTable.SetColumns(columns)
	m.overviewTable.SetRows(rows)
	tableHeight := clampInt(l.ContentHeight-5, 5, 24)
	if m.height == 0 {
		tableHeight = len(rows) + 1
	}
	m.overviewTable.SetHeight(tableHeight)
	m.overviewTable.SetWidth(maxInt(20, l.ContentWidth-4))
	m.overviewTable.Focus()
	styles := table.DefaultStyles()
	styles.Header = styles.Header.Bold(true).Foreground(defaultTheme.Primary)
	styles.Selected = selectedStyle
	m.overviewTable.SetStyles(styles)
	if selected >= 0 {
		m.overviewTable.SetCursor(selected)
	} else if len(ids) > 0 {
		cursor := clampInt(m.overviewTable.Cursor(), 0, len(ids)-1)
		m.overviewTable.SetCursor(cursor)
		m.selectedNode = ids[cursor]
	}
}

func (p *overviewPage) HandleKey(key tea.KeyMsg) tea.Cmd {
	_, cmd := p.Update(key)
	return cmd
}

func (p *overviewPage) View(width, height int) string {
	p.m.width = width
	p.m.height = height
	return p.m.overview()
}

func (m *model) overview() string {
	var lines []string
	lines = append(lines, sectionStyle.Render("总览"))
	if strings.TrimSpace(m.filter) != "" {
		lines = append(lines, defaultTheme.Focused.Render("筛选："+redactSingleLineUI(m.filter)))
	}
	if strings.TrimSpace(m.notice) != "" {
		lines = append(lines, warnStyle.Render(redactSingleLineUI(m.notice)))
	}
	if m.err != nil {
		lines = append(lines, failStyle.Render(redactSingleLineUI(localizeRuntimeError(m.err))))
	}
	m.syncOverviewTable()
	if len(m.overviewRowIDs) == 0 {
		if m.filter != "" {
			lines = append(lines, subtleStyle.Render("没有匹配的节点。"))
		} else {
			lines = append(lines, subtleStyle.Render("暂无事件"))
		}
	} else {
		lines = append(lines, m.overviewTable.View())
		lines = append(lines, scrollIndicator(maxInt(0, m.overviewTable.Cursor()-m.overviewTable.Height()/2), m.overviewTable.Height(), len(m.overviewRowIDs)))
		if event, ok := m.nodes[m.selectedNode]; ok && event.Path != "" {
			lines = append(lines, subtleStyle.Render("路径："+redactSingleLineUI(event.Path)))
		}
	}
	return panelStyle.Width(contentWidth(m.width)).Render(strings.Join(lines, "\n"))
}
