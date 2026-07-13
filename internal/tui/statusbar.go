package tui

import "time"

func (m model) statusBar() string {
	if m.view == viewStart || m.view == viewHub {
		return ""
	}
	status := localizeStatus(m.summary.Status)
	if m.done {
		if m.err != nil || !m.summary.Passed {
			status = "失败"
		} else {
			status = "成功"
		}
	}
	if !m.done && !m.readOnly {
		status = m.spinner.View() + " 正在运行"
	}
	start := m.summary.StartedAt
	if start.IsZero() {
		for _, event := range m.events {
			if !event.CreatedAt.IsZero() {
				start = event.CreatedAt
				break
			}
		}
	}
	elapsed := ""
	if !start.IsZero() {
		end := time.Now()
		if !m.summary.FinishedAt.IsZero() {
			end = m.summary.FinishedAt
		}
		elapsed = " · 已用时 " + end.Sub(start).Round(time.Second).String()
	}
	readonly := ""
	if m.readOnly {
		readonly = " · " + warnStyle.Render("只读快照")
	}
	line := subtleStyle.Render(redactSingleLineUI(status+elapsed)) + readonly
	if m.width > 0 {
		line = clipDisplay(line, m.width)
	}
	return line
}
