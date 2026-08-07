package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/purplevoid/harbor-factory/internal/app"
)

// logModel presents a worker log as a list of records rather than a wall of
// JSON. Each durable worker process appends one result record to the file, so
// the useful unit is a record, not a line: the previous view wrapped raw JSON
// and an operator had to read a frozen run manifest to find a job state.
//
// The raw text stays reachable behind [R] so a projection that omits something
// never hides the original bytes.
type logModel struct {
	taskName string
	runID    string
	path     string
	// content is the verbatim record text used by the raw fallback view.
	content string
	message string
	// handoffSummary states how many worker processes appended to this file.
	handoffSummary string
	// recordsTruncated means older records exist that this read did not return;
	// rawTruncated means only the raw fallback text was clipped. Keeping them
	// apart stops the header from claiming records are missing when they are not.
	recordsTruncated bool
	rawTruncated     bool
	records          []app.TaskBoardLogRecord
	selected         int
	expanded         bool
	raw              bool
	pane             *scrollPane
}

func newLogModel(task *TaskItem, log app.TaskBoardLog) *logModel {
	taskName := "任务日志"
	if task != nil && task.Name != "" {
		taskName = task.Name
	}
	model := &logModel{
		taskName: taskName, runID: log.RunID, path: log.Path, content: log.Content,
		message: log.Message, handoffSummary: log.HandoffSummary,
		recordsTruncated: log.Truncated, rawTruncated: log.RawTruncated,
		records: log.Records, pane: newScrollPane(),
	}
	// The newest record is the one an operator opens the log to read.
	if len(model.records) > 0 {
		model.selected = len(model.records) - 1
	}
	return model
}

func (m *logModel) hasRecords() bool {
	return m != nil && len(m.records) > 0
}

// listMode reports whether the record list drives navigation. Raw text and an
// empty log both fall back to plain scrolling.
func (m *logModel) listMode() bool {
	return m.hasRecords() && !m.raw
}

func (m *logModel) ToggleRaw() {
	if m == nil {
		return
	}
	m.raw = !m.raw
}

func (m *logModel) ToggleExpanded() {
	if m == nil || !m.listMode() {
		return
	}
	m.expanded = !m.expanded
}

func (m *logModel) MoveUp() {
	if m == nil {
		return
	}
	if !m.listMode() {
		m.pane.MoveUp()
		return
	}
	if m.selected > 0 {
		m.selected--
	}
}

func (m *logModel) MoveDown() {
	if m == nil {
		return
	}
	if !m.listMode() {
		m.pane.MoveDown()
		return
	}
	if m.selected+1 < len(m.records) {
		m.selected++
	}
}

func (m *logModel) PageUp() {
	if m == nil {
		return
	}
	m.pane.PageUp()
	if m.listMode() {
		m.selected = max(0, m.selected-m.pageStep())
	}
}

func (m *logModel) PageDown() {
	if m == nil {
		return
	}
	m.pane.PageDown()
	if m.listMode() {
		m.selected = min(len(m.records)-1, m.selected+m.pageStep())
	}
}

func (m *logModel) GoToStart() {
	if m == nil {
		return
	}
	m.pane.GoToStart()
	if m.listMode() {
		m.selected = 0
	}
}

func (m *logModel) GoToEnd() {
	if m == nil {
		return
	}
	m.pane.GoToEnd()
	if m.listMode() {
		m.selected = len(m.records) - 1
	}
}

func (m *logModel) pageStep() int {
	return max(1, m.pane.view.Height)
}

// FooterText states only the keys that currently do something. An expand hint
// on an empty log, or a record hint in raw mode, would be a lie.
func (m *logModel) FooterText() string {
	if m == nil {
		return "[q] 返回详情"
	}
	actions := make([]string, 0, 6)
	actions = append(actions, "[↑↓/jk] 滚动")
	if m.listMode() {
		if m.expanded {
			actions = append(actions, "[enter] 收起记录")
		} else {
			actions = append(actions, "[enter] 展开记录")
		}
	}
	if m.hasRecords() {
		if m.raw {
			actions = append(actions, "[R] 记录视图")
		} else {
			actions = append(actions, "[R] 原文")
		}
	}
	actions = append(actions, "[PgUp/PgDn] 翻页", "[r] 刷新", "[q] 返回详情")
	return strings.Join(actions, "  ")
}

// recordSummary renders one record as a single line: when it happened, what the
// Run was doing, which durable job ran, and why it stopped.
func recordSummary(record app.TaskBoardLogRecord, selected bool) string {
	parts := make([]string, 0, 5)
	parts = append(parts, formatDetailTime(record.ObservedAt))
	if stage := strings.TrimSpace(record.StageAttemptID); stage != "" && record.JobCommandType != "" {
		parts = append(parts, record.JobCommandType)
	} else if record.JobCommandType != "" {
		parts = append(parts, record.JobCommandType)
	} else if record.CycleEmpty {
		parts = append(parts, "空轮询")
	}
	if record.JobState != "" {
		parts = append(parts, record.JobState)
	}
	if record.RunStatus != "" {
		parts = append(parts, "run:"+record.RunStatus)
	}
	if record.StoppedFor != "" {
		parts = append(parts, "stopped_for:"+record.StoppedFor)
	}
	if record.ParseError != "" {
		parts = append(parts, "解析失败")
	}
	line := fmt.Sprintf("%3d  %s", record.Sequence, strings.Join(parts, " · "))
	switch {
	case selected:
		return detailHistoryCurrentStyle.Render("▶ " + line)
	case record.ParseError != "" || record.FailureCode != "":
		return styleFail.Render("  " + line)
	default:
		return detailValueStyle.Render("  " + line)
	}
}

// recordDetailFields expands one record into the fields an operator decides on.
func recordDetailFields(record app.TaskBoardLogRecord, width int) string {
	fields := []string{
		detailField("记录", fmt.Sprintf("%d", record.Sequence), width),
		detailField("时间", formatDetailTime(record.ObservedAt), width),
		detailField("Run 状态", record.RunStatus, width),
	}
	if record.StoppedFor != "" {
		fields = append(fields, detailField("停止原因", record.StoppedFor, width))
	}
	if record.JobCommandType != "" {
		fields = append(fields, detailField("Job 类型", record.JobCommandType, width))
	}
	if record.JobState != "" {
		fields = append(fields, detailField("Job 状态", record.JobState, width))
	}
	if record.StageAttemptID != "" {
		fields = append(fields, detailField("阶段 attempt", record.StageAttemptID, width))
	}
	if record.JobStartedAt != nil || record.JobFinishedAt != nil {
		fields = append(fields,
			detailField("开始", formatDetailTime(record.JobStartedAt), width),
			detailField("结束", formatDetailTime(record.JobFinishedAt), width),
		)
	}
	if record.CycleEmpty {
		fields = append(fields, detailField("本轮", "未领取到durable job", width))
	}
	if record.HandoffSummary != "" {
		fields = append(fields, detailField("交接", record.HandoffSummary, width))
	}
	if record.FailureCode != "" {
		fields = append(fields, detailField("失败码", record.FailureCode, width))
	}
	if record.FailureSummary != "" {
		fields = append(fields, styleFail.Render(wrapDisplay(record.FailureSummary, max(1, width-detailFieldGutter))))
	}
	if record.Legacy {
		fields = append(fields, mutedStyle.Render("旧格式记录（由升级前的 worker 写入）"))
	}
	if record.ParseError != "" {
		fields = append(fields, styleFail.Render("记录解析失败: "+record.ParseError), mutedStyle.Render("按 R 查看原文"))
	}
	return detailFields(fields...)
}

// bodyContent builds the scrollable text for the current mode.
func (m *logModel) bodyContent(width int) string {
	if m.raw || !m.hasRecords() {
		text := strings.TrimRight(m.content, "\n")
		if strings.TrimSpace(text) == "" {
			if m.message != "" {
				return mutedStyle.Render(m.message)
			}
			return mutedStyle.Render("日志为空")
		}
		return text
	}
	lines := make([]string, 0, len(m.records)+8)
	for index, record := range m.records {
		lines = append(lines, recordSummary(record, index == m.selected))
		if m.expanded && index == m.selected {
			lines = append(lines, recordDetailFields(record, width))
		}
	}
	return strings.Join(lines, "\n")
}

// selectedLine is the 0-based content row of the selected record, used to keep
// it on screen as the selection moves.
func (m *logModel) selectedLine() int {
	if !m.listMode() {
		return 0
	}
	line := 0
	for index := range m.records {
		if index == m.selected {
			return line
		}
		line++
	}
	return line
}

func (m *logModel) View(width, bodyRows int) string {
	contentWidth := max(24, width)
	header := m.headerView(contentWidth)
	headerRows := lipgloss.Height(header)
	paneRows := max(1, bodyRows-headerRows-framedPaneRows)
	lineWidth := max(1, contentWidth-4)

	m.pane.Resize(lineWidth, paneRows)
	m.pane.SetContent(m.bodyContent(lineWidth), lineWidth)
	if m.listMode() {
		m.pane.EnsureVisible(m.selectedLine())
	}
	body := logContentStyle.Width(lineWidth).Height(paneRows).Render(m.pane.View())
	return lipgloss.JoinVertical(lipgloss.Left, header, body)
}

func (m *logModel) headerView(width int) string {
	path := m.path
	if path == "" {
		path = "暂无本地日志"
	}
	location := ""
	switch {
	case m.listMode():
		location = fmt.Sprintf("记录 %d / %d", m.selected+1, len(m.records))
	case m.pane.LineCount() > 0:
		location = fmt.Sprintf("行 %d-%d / %d", m.pane.FirstVisibleLine(), m.pane.LastVisibleLine(), m.pane.LineCount())
	}
	if m.recordsTruncated {
		location = "仅最近记录 · " + location
	}
	if m.raw && m.hasRecords() {
		location = "原文 · " + location
		// The clip only affects the raw view, so say so only here rather than
		// letting the record list imply records were dropped.
		if m.rawTruncated {
			location = "原文已截断 · " + location
		}
	}
	rows := []string{
		detailBreadcrumbStyle.Render("题目管理 / 任务详情 / 日志"),
		detailTitleStyle.Width(max(1, width-2)).Render(truncateDisplay(m.taskName, max(1, width-4))),
		detailField("Run ID", m.runID, width),
		detailField("日志文件", path, width),
	}
	if m.handoffSummary != "" {
		rows = append(rows, detailField("worker", m.handoffSummary, width))
	}
	if location != "" {
		rows = append(rows, mutedStyle.Render(truncateDisplay(location, width)))
	}
	if m.message != "" && m.hasRecords() {
		rows = append(rows, mutedStyle.Render(truncateDisplay(m.message, width)))
	}
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}
