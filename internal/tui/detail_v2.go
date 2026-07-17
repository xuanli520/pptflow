package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/purplevoid/harbor-factory/internal/app"
)

// detailModel renders one task as a small hierarchy instead of an ungrouped
// list of durable fields.
type detailModel struct {
	task *TaskItem
}

func newDetailModel(task *TaskItem) *detailModel {
	return &detailModel{task: task}
}

func (d *detailModel) currentRun() *TaskRunItem {
	if d == nil || d.task == nil {
		return nil
	}
	for index := range d.task.Runs {
		if d.task.Runs[index].ID == d.task.RunID {
			return &d.task.Runs[index]
		}
	}
	if len(d.task.Runs) > 0 {
		return &d.task.Runs[0]
	}
	return nil
}

func (d *detailModel) hasCurrentRun() bool {
	return d.currentRun() != nil
}

func (d *detailModel) canCancelCurrentRun() bool {
	run := d.currentRun()
	if run == nil {
		return false
	}
	switch run.Status {
	case "queued", "running", "pause_requested", "pausing", "paused", "resume_requested", "waiting_review", "waiting_continuation":
		return true
	default:
		return false
	}
}

func (d *detailModel) canRetryCurrentRun() bool {
	run := d.currentRun()
	return run != nil && run.CanRetry
}

func (d *detailModel) View(width, height int) string {
	if d == nil || d.task == nil {
		return mutedStyle.Render("未选择题目")
	}
	contentWidth := max(24, width)
	title := ansi.TruncateWc(d.task.Name, max(1, contentWidth-2), "...")
	summary := ansi.TruncateWc("任务状态: "+d.task.Lifecycle+"  ·  标识: "+d.task.Slug, contentWidth, "...")
	parts := []string{
		detailBreadcrumbStyle.Render("题目管理 / 任务详情"),
		detailTitleStyle.Width(max(1, contentWidth-2)).Render(title),
		detailSubtitleStyle.Render(summary),
	}

	source := renderDetailSection("来源", detailFields(contentWidth,
		detailField("仓库", d.task.RepoURL, contentWidth),
		detailField("Commit SHA", d.task.CommitSHA, contentWidth),
	), contentWidth)
	run := renderDetailSection("当前运行", d.currentRunFields(contentWidth), contentWidth)
	if contentWidth >= 96 {
		leftWidth := (contentWidth - 1) / 2
		rightWidth := contentWidth - leftWidth - 1
		parts = append(parts, lipgloss.JoinHorizontal(lipgloss.Top,
			renderDetailSection("来源", detailFields(leftWidth,
				detailField("仓库", d.task.RepoURL, leftWidth),
				detailField("Commit SHA", d.task.CommitSHA, leftWidth),
			), leftWidth),
			" ",
			renderDetailSection("当前运行", d.currentRunFields(rightWidth), rightWidth),
		))
	} else {
		parts = append(parts, source, run)
	}

	if failure := d.failureFields(contentWidth); failure != "" {
		parts = append(parts, renderDetailSection("失败原因", failure, contentWidth))
	}
	parts = append(parts, renderDetailSection("运行记录", d.historyFields(contentWidth, height), contentWidth))
	if d.task.Review != nil {
		parts = append(parts, renderDetailSection("审核", detailFields(contentWidth,
			detailField("状态", "待审核 ("+string(d.task.Review.Kind)+")", contentWidth),
		), contentWidth))
	} else if d.task.OpenReviews > 1 {
		parts = append(parts, renderDetailSection("审核", failStyleV2.Render("存在多个待处理审核，请使用 CLI 选择明确的审核请求"), contentWidth))
	}
	return lipgloss.NewStyle().Width(contentWidth).Render(lipgloss.JoinVertical(lipgloss.Left, parts...))
}

func (d *detailModel) currentRunFields(width int) string {
	run := d.currentRun()
	if run == nil {
		return mutedStyle.Render("尚未创建 Run")
	}
	stage := run.CurrentStage
	if stage == "" {
		stage = "-"
	}
	logPath := run.LogPath
	if logPath == "" {
		logPath = "暂无本地日志"
	}
	retry := "可用"
	retryLabel := "重试"
	if run.RetryStrategy == app.TaskBoardRetryStrategyAuthoringRecovery {
		retryLabel = "恢复/重试"
	} else if run.RetryStrategy == app.TaskBoardRetryStrategyAuthoringAdmissionRepair {
		retryLabel = "修复并继续"
	}
	if !run.CanRetry {
		retry = run.RetryReason
		if retry == "" {
			retry = "当前 Run 不可重试"
		}
	}
	cancel := "可用"
	if !d.canCancelCurrentRun() {
		cancel = "当前 Run 状态不可取消"
	}
	return detailFields(width,
		detailField("Run ID", run.ID, width),
		detailField("状态", run.Status, width),
		detailField("当前阶段", stage, width),
		detailField("开始时间", formatDetailTime(run.StartedAt, &run.CreatedAt), width),
		detailField("日志文件", logPath, width),
		detailField(retryLabel, retry, width),
		detailField("取消", cancel, width),
	)
}

func (d *detailModel) failureFields(width int) string {
	run := d.currentRun()
	if run == nil || (strings.TrimSpace(run.FailureSummary) == "" && strings.TrimSpace(run.FailureCode) == "") {
		return ""
	}
	stage := run.FailureStage
	if stage == "" {
		stage = "当前 Run"
	}
	summary := strings.TrimSpace(run.FailureSummary)
	if summary == "" {
		summary = "未提供错误摘要"
	}
	fields := []string{detailField("失败阶段", stage, width)}
	if code := strings.TrimSpace(run.FailureCode); code != "" {
		fields = append(fields, detailField("错误码", code, width))
	}
	if jobID := strings.TrimSpace(run.FailureJobID); jobID != "" {
		fields = append(fields, detailField("Job ID", jobID, width))
	}
	if artifactID := strings.TrimSpace(run.FailureArtifactID); artifactID != "" {
		fields = append(fields, detailField("Artifact ID", artifactID, width))
	}
	if run.FailureRecordedAt != nil {
		fields = append(fields, detailField("记录时间", formatDetailTime(run.FailureRecordedAt), width))
	}
	if recovery := detailFailureRecoveryAction(run); recovery != "" {
		fields = append(fields, detailField("恢复操作", recovery, width))
	}
	wrapped := ansi.WrapWc(ansi.Strip(summary), max(1, width-6), "")
	fields = append(fields, failStyleV2.Render(wrapped))
	return detailFields(width, fields...)
}

func detailFailureRecoveryAction(run *TaskRunItem) string {
	if run == nil {
		return ""
	}
	switch run.FailureRecoveryAction {
	case app.TaskBoardFailureRecoveryRedriveAuthoringHandoff:
		if run.CanRedrive {
			return "显式 redrive"
		}
	case app.TaskBoardFailureRecoveryReconcile:
		return "显式 reconcile"
	case app.TaskBoardFailureRecoveryRepairOrNewRun:
		return "修复或新建运行"
	}
	return ""
}

func (d *detailModel) historyFields(width, height int) string {
	if len(d.task.Runs) == 0 {
		return mutedStyle.Render("尚无运行记录")
	}
	limit := 4
	if height < 24 {
		limit = 2
	}
	if limit > len(d.task.Runs) {
		limit = len(d.task.Runs)
	}
	rows := make([]string, 0, limit+1)
	for index := 0; index < limit; index++ {
		run := d.task.Runs[index]
		marker := mutedStyle.Render("历史")
		if run.ID == d.task.RunID {
			marker = detailHistoryCurrentStyle.Render("当前")
		}
		when := formatDetailTime(run.FinishedAt, run.StartedAt, &run.CreatedAt)
		phase := "Authoring"
		if run.ParentRunID != "" {
			phase = "CodeEdge Phase-1"
		}
		row := marker + "  " + mutedStyle.Render(phase) + "  " + detailValueStyle.Render(truncateMiddle(run.ID, 16)) + "  " + runStatusStyle(run.Status) + "  " + mutedStyle.Render(when)
		rows = append(rows, ansi.TruncateWc(row, max(1, width-6), ""))
	}
	if remaining := len(d.task.Runs) - limit; remaining > 0 {
		rows = append(rows, mutedStyle.Render(fmt.Sprintf("另有 %d 条较早记录", remaining)))
	}
	return strings.Join(rows, "\n")
}

func renderDetailSection(title, content string, outerWidth int) string {
	innerWidth := max(1, outerWidth-4)
	return detailSectionStyle.Width(innerWidth).Render(lipgloss.JoinVertical(lipgloss.Left,
		detailSectionTitleStyle.Render(title),
		content,
	))
}

func detailFields(width int, fields ...string) string {
	return strings.Join(fields, "\n")
}

func detailField(label, value string, width int) string {
	labelWidth := 11
	available := max(1, width-labelWidth-6)
	return detailLabelStyle.Width(labelWidth).Render(label) + detailValueStyle.Render(truncateMiddle(value, available))
}

func formatDetailTime(values ...*time.Time) string {
	for _, value := range values {
		if value != nil && !value.IsZero() {
			return value.Local().Format("2006-01-02 15:04")
		}
	}
	return "-"
}

func runStatusStyle(status string) string {
	switch status {
	case "failed_terminal", "failed_recoverable", "interrupted", "in_doubt", "canceled":
		return failStyleV2.Render(status)
	case "running", "queued", "waiting_review", "waiting_continuation", "paused":
		return statusRunningStyle.Render(status)
	default:
		return detailValueStyle.Render(status)
	}
}

// logModel owns scroll state for a bounded, read-only worker-log snapshot.
type logModel struct {
	taskName  string
	runID     string
	path      string
	content   string
	message   string
	truncated bool
	offset    int
}

func newLogModel(task *TaskItem, log app.TaskBoardLog) *logModel {
	taskName := "任务日志"
	if task != nil && task.Name != "" {
		taskName = task.Name
	}
	return &logModel{
		taskName: taskName, runID: log.RunID, path: log.Path, content: log.Content,
		message: log.Message, truncated: log.Truncated,
	}
}

func (m *logModel) lines(width int) []string {
	text := ansi.Strip(m.content)
	if text == "" {
		if m.message == "" {
			return []string{"日志为空"}
		}
		return []string{m.message}
	}
	wrapped := ansi.WrapWc(text, max(1, width), "")
	return strings.Split(wrapped, "\n")
}

func (m *logModel) visibleHeight(height int) int {
	return max(1, height-6)
}

func (m *logModel) clampOffset(width, height int) {
	maximum := max(0, len(m.lines(width))-m.visibleHeight(height))
	m.offset = min(max(0, m.offset), maximum)
}

func (m *logModel) MoveUp(width, height int) {
	m.offset--
	m.clampOffset(width, height)
}

func (m *logModel) MoveDown(width, height int) {
	m.offset++
	m.clampOffset(width, height)
}

func (m *logModel) PageUp(width, height int) {
	m.offset -= m.visibleHeight(height)
	m.clampOffset(width, height)
}

func (m *logModel) PageDown(width, height int) {
	m.offset += m.visibleHeight(height)
	m.clampOffset(width, height)
}

func (m *logModel) GoToStart() {
	m.offset = 0
}

func (m *logModel) GoToEnd(width, height int) {
	m.offset = len(m.lines(width))
	m.clampOffset(width, height)
}

func (m *logModel) View(width, height int) string {
	contentWidth := max(24, width)
	lineWidth := max(1, contentWidth-4)
	lines := m.lines(lineWidth)
	m.clampOffset(lineWidth, height)
	end := min(len(lines), m.offset+m.visibleHeight(height))
	visible := strings.Join(lines[m.offset:end], "\n")
	location := fmt.Sprintf("行 %d-%d / %d", m.offset+1, end, len(lines))
	if m.truncated {
		location = "日志尾部 " + location
	}
	path := m.path
	if path == "" {
		path = "暂无本地日志"
	}
	title := ansi.TruncateWc(m.taskName, max(1, contentWidth-2), "...")
	return lipgloss.JoinVertical(lipgloss.Left,
		detailBreadcrumbStyle.Render("题目管理 / 任务详情 / 日志"),
		detailTitleStyle.Width(max(1, contentWidth-2)).Render(title),
		detailFields(contentWidth,
			detailField("Run ID", m.runID, contentWidth),
			detailField("日志文件", path, contentWidth),
			mutedStyle.Render(location),
		),
		logContentStyle.Width(max(1, contentWidth-4)).Height(m.visibleHeight(height)).Render(visible),
	)
}
