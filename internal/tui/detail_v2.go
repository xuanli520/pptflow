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

func (d *detailModel) authoringLaunch() *app.TaskBoardAuthoringLaunch {
	if d == nil || d.task == nil {
		return nil
	}
	return d.task.AuthoringLaunch
}

func (d *detailModel) hasAuthoringLaunch() bool {
	return d.authoringLaunch() != nil
}

func (d *detailModel) canRetryAuthoringLaunch() bool {
	launch := d.authoringLaunch()
	return launch != nil && launch.CanRetry
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

func (d *detailModel) evaluatorStatus() *app.TaskBoardEvaluatorStatus {
	if d == nil || d.task == nil {
		return nil
	}
	return d.task.Evaluator
}

func (d *detailModel) View(width, height int) string {
	if d == nil || d.task == nil {
		return mutedStyle.Render("未选择题目")
	}
	if d.hasAuthoringLaunch() {
		return d.authoringLaunchView(width)
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
	if transcripts := d.agentTranscriptFields(contentWidth); transcripts != "" {
		parts = append(parts, renderDetailSection("Agent 回合", transcripts, contentWidth))
	}
	if evaluator := d.evaluatorStatus(); evaluator != nil {
		fields := []string{
			detailField("状态", displayEvaluatorState(evaluator.State), contentWidth),
			detailField("Phase-1 Run", evaluator.ParentRunID, contentWidth),
		}
		if evaluator.ChildRunID != "" {
			fields = append(fields, detailField("评测子 Run", evaluator.ChildRunID, contentWidth))
		}
		if evaluator.Reason != "" {
			fields = append(fields, detailField("说明", evaluator.Reason, contentWidth))
		}
		parts = append(parts, renderDetailSection("外部评测", detailFields(contentWidth, fields...), contentWidth))
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

func displayEvaluatorState(state app.TaskBoardEvaluatorState) string {
	switch state {
	case app.TaskBoardEvaluatorAwaitingFinalReview:
		return "等待最终审核"
	case app.TaskBoardEvaluatorReadyToLaunch:
		return "可启动 Qwen/Opus 评测"
	case app.TaskBoardEvaluatorChildActive:
		return "评测子 Run 执行中"
	case app.TaskBoardEvaluatorReadyToAdopt:
		return "可采用评测证据"
	case app.TaskBoardEvaluatorAdopted:
		return "评测证据已采用"
	default:
		return "当前不可用"
	}
}

func (d *detailModel) authoringLaunchView(width int) string {
	launch := d.authoringLaunch()
	if launch == nil {
		return mutedStyle.Render("未选择源码捕获启动")
	}
	contentWidth := max(24, width)
	title := ansi.TruncateWc(launch.Title, max(1, contentWidth-2), "...")
	parts := []string{
		detailBreadcrumbStyle.Render("题目管理 / 启动恢复"),
		detailTitleStyle.Width(max(1, contentWidth-2)).Render(title),
		detailSubtitleStyle.Render("源码捕获失败，尚未创建 Task"),
		renderDetailSection("来源", detailFields(contentWidth,
			detailField("仓库", launch.RepositoryURL, contentWidth),
			detailField("Commit SHA", launch.CommitSHA, contentWidth),
		), contentWidth),
		renderDetailSection("启动", detailFields(contentWidth,
			detailField("标识", launch.Slug, contentWidth),
			detailField("操作 ID", launch.OperationID, contentWidth),
			detailField("状态", launch.Status, contentWidth),
			detailField("创建时间", formatDetailTime(&launch.CreatedAt), contentWidth),
		), contentWidth),
	}
	failure := []string{detailField("错误码", launch.FailureCode, contentWidth)}
	summary := strings.TrimSpace(launch.FailureSummary)
	if summary == "" {
		summary = "未提供错误摘要"
	}
	failure = append(failure, failStyleV2.Render(ansi.WrapWc(ansi.Strip(summary), max(1, contentWidth-6), "")))
	parts = append(parts, renderDetailSection("失败原因", detailFields(contentWidth, failure...), contentWidth))
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
	} else {
		stage = displayStageName(stage)
	}
	logPath := run.LogPath
	if logPath == "" {
		logPath = "暂无本地日志"
	}
	retry := "可用"
	retryLabel := "重试"
	if run.RetryStrategy == app.TaskBoardRetryStrategyTaskContinuation {
		retryLabel = "断点恢复"
	} else if run.RetryStrategy == app.TaskBoardRetryStrategyStandardProtocolStage {
		retryLabel = "重试当前阶段"
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
	fields := []string{detailField("Run ID", run.ID, width)}
	if summary := run.OperatorSummary; summary != nil {
		fields = append(fields, detailField("业务状态", displayOperatorSummary(summary), width))
		if validation := summary.LatestValidation; validation != nil {
			validationStage := validation.Stage
			if validationStage != "" {
				validationStage = displayStageName(validationStage)
			}
			fields = append(fields,
				detailField("验证结论", validation.Verdict, width),
				detailField("验证阶段", validationStage, width),
				detailField("阶段结果", validation.StageExecutionStatus+" / "+validation.StageVerdict, width),
			)
			if validation.FailureCode != "" {
				fields = append(fields, detailField("失败码", validation.FailureCode, width))
			}
		}
		if summary.Cause != "" {
			fields = append(fields, detailField("原因", summary.Cause, width))
		}
		if summary.NextAction != "" {
			fields = append(fields, detailField("下一步", summary.NextAction, width))
		}
	}
	fields = append(fields,
		detailField("状态", run.Status, width),
		detailField("当前阶段", stage, width),
		detailField("开始时间", formatDetailTime(run.StartedAt, &run.CreatedAt), width),
		detailField("日志文件", logPath, width),
		detailField(retryLabel, retry, width),
		detailField("取消", cancel, width),
	)
	if evidence := run.AuthoringEvidence; evidence != nil {
		contract := evidence.Contract
		fields = append(fields,
			detailField("根契约", contract.Digest, width),
			detailField("任务", contract.Title+" ("+contract.Slug+")", width),
			detailField("分类", contract.CodeLang+" / "+contract.TaskType+" / "+contract.Application, width),
			detailField("0-to-1", fmt.Sprintf("%t", contract.Is0To1), width),
			detailField("仓库", contract.RepositoryURL, width),
			detailField("Commit", contract.CommitSHA, width),
			detailField("快照", contract.SnapshotDigest, width),
			detailField("源码根", contract.CheckoutRoot, width),
			detailField("基础镜像", contract.BaseImage, width),
			detailField("目标", contract.Objective, width),
			detailField("交付格式", contract.PackageFormat, width),
			detailField("Profile 摘要", contract.ProfileFingerprint, width),
		)
		for _, claim := range evidence.Claims {
			fields = append(fields, detailField("声明比对", claim.ArtifactKey+" = "+claim.State, width))
		}
		for _, artifact := range evidence.Lineage {
			fields = append(fields,
				detailField("最终谱系", artifact.ArtifactKey, width),
				detailField("Artifact ID", artifact.ArtifactID, width),
				detailField("Artifact 摘要", artifact.Digest, width),
			)
		}
	}
	return detailFields(width, fields...)
}

func (d *detailModel) failureFields(width int) string {
	run := d.currentRun()
	if run == nil || (strings.TrimSpace(run.FailureSummary) == "" && strings.TrimSpace(run.FailureCode) == "") {
		return ""
	}
	stage := run.FailureStage
	if stage == "" {
		stage = "当前 Run"
	} else {
		stage = displayStageName(stage)
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

func (d *detailModel) agentTurnTranscripts() []app.TaskBoardAgentTranscript {
	run := d.currentRun()
	if run == nil {
		return nil
	}
	return run.AgentTurnTranscripts
}

func (d *detailModel) hasAgentTurnTranscripts() bool {
	return len(d.agentTurnTranscripts()) > 0
}

func (d *detailModel) agentTranscriptFields(width int) string {
	transcripts := d.agentTurnTranscripts()
	if len(transcripts) == 0 {
		return ""
	}
	limit := min(3, len(transcripts))
	fields := make([]string, 0, limit*3+1)
	for _, transcript := range transcripts[:limit] {
		stage := displayStageName(transcript.StageKey)
		status := transcript.SubmissionStatus
		if transcript.ProtocolRejectionCode != "" {
			status += " / " + transcript.ProtocolRejectionCode
		}
		if transcript.FailureCode != "" {
			status += " / " + transcript.FailureCode
		}
		retention := formatDetailTime(&transcript.ExpiresAt)
		if transcript.ExpiredAt != nil {
			retention = "正文已清除"
		}
		fields = append(fields,
			detailField("阶段", stage+fmt.Sprintf(" · 第 %d 回合", transcript.Turn), width),
			detailField("提交", status, width),
			detailField("保留至", retention, width),
		)
	}
	if remaining := len(transcripts) - limit; remaining > 0 {
		fields = append(fields, mutedStyle.Render(fmt.Sprintf("另有 %d 个较早回合", remaining)))
	}
	return detailFields(width, fields...)
}

func detailFailureRecoveryAction(run *TaskRunItem) string {
	if run == nil {
		return ""
	}
	switch run.FailureRecoveryAction {
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

// agentTranscriptModel presents retained model output without turning it into
// a workflow input. It is intentionally read-only and operates solely on the
// task-board projection already loaded by the TUI.
type agentTranscriptModel struct {
	taskName    string
	transcripts []app.TaskBoardAgentTranscript
	selected    int
	offset      int
}

func newAgentTranscriptModel(task *TaskItem) *agentTranscriptModel {
	name := "Agent 回合"
	transcripts := make([]app.TaskBoardAgentTranscript, 0)
	if task != nil {
		if task.Name != "" {
			name = task.Name
		}
		for _, run := range task.Runs {
			if run.ID == task.RunID {
				transcripts = append(transcripts, run.AgentTurnTranscripts...)
				break
			}
		}
		if len(transcripts) == 0 && len(task.Runs) > 0 {
			transcripts = append(transcripts, task.Runs[0].AgentTurnTranscripts...)
		}
	}
	return &agentTranscriptModel{taskName: name, transcripts: transcripts}
}

func (m *agentTranscriptModel) current() *app.TaskBoardAgentTranscript {
	if m == nil || m.selected < 0 || m.selected >= len(m.transcripts) {
		return nil
	}
	return &m.transcripts[m.selected]
}

func (m *agentTranscriptModel) MovePrevious() {
	if m != nil && m.selected > 0 {
		m.selected--
		m.offset = 0
	}
}

func (m *agentTranscriptModel) MoveNext() {
	if m != nil && m.selected+1 < len(m.transcripts) {
		m.selected++
		m.offset = 0
	}
}

func (m *agentTranscriptModel) lines(width int) []string {
	transcript := m.current()
	if transcript == nil {
		return []string{"未保留 Agent 回合"}
	}
	fields := []string{
		"阶段: " + displayStageName(transcript.StageKey),
		fmt.Sprintf("回合: %d", transcript.Turn),
		"模型: " + transcript.ModelID,
		"提交: " + transcript.SubmissionStatus,
		fmt.Sprintf("响应: %d bytes · %s", transcript.ResponseBytes, transcript.ResponseSHA256),
		"创建时间: " + formatDetailTime(&transcript.CreatedAt),
		"到期时间: " + formatDetailTime(&transcript.ExpiresAt),
	}
	if transcript.ProtocolRejectionCode != "" {
		fields = append(fields, "协议拒绝: "+transcript.ProtocolRejectionCode)
	}
	if transcript.FailureCode != "" {
		fields = append(fields, "失败码: "+transcript.FailureCode)
	}
	fields = append(fields, fmt.Sprintf("工具提交: %d", transcript.SubmissionCount), "", "模型响应:")
	if transcript.ExpiredAt != nil {
		fields = append(fields, "原文已按保留规则清除")
	} else if strings.TrimSpace(transcript.ResponseText) == "" {
		fields = append(fields, "此回合没有返回文本")
	} else {
		fields = append(fields, ansi.WrapWc(ansi.Strip(transcript.ResponseText), max(1, width), ""))
	}
	return strings.Split(strings.Join(fields, "\n"), "\n")
}

func (m *agentTranscriptModel) visibleHeight(height int) int {
	return max(1, height-7)
}

func (m *agentTranscriptModel) clampOffset(width, height int) {
	maximum := max(0, len(m.lines(width))-m.visibleHeight(height))
	m.offset = min(max(0, m.offset), maximum)
}

func (m *agentTranscriptModel) MoveUp(width, height int) {
	m.offset--
	m.clampOffset(width, height)
}

func (m *agentTranscriptModel) MoveDown(width, height int) {
	m.offset++
	m.clampOffset(width, height)
}

func (m *agentTranscriptModel) PageUp(width, height int) {
	m.offset -= m.visibleHeight(height)
	m.clampOffset(width, height)
}

func (m *agentTranscriptModel) PageDown(width, height int) {
	m.offset += m.visibleHeight(height)
	m.clampOffset(width, height)
}

func (m *agentTranscriptModel) View(width, height int) string {
	contentWidth := max(24, width)
	lineWidth := max(1, contentWidth-4)
	lines := m.lines(lineWidth)
	m.clampOffset(lineWidth, height)
	end := min(len(lines), m.offset+m.visibleHeight(height))
	content := strings.Join(lines[m.offset:end], "\n")
	position := fmt.Sprintf("%d / %d", m.selected+1, len(m.transcripts))
	header := detailTitleStyle.Width(max(1, contentWidth-2)).Render(m.taskName + " · Agent 回合 " + position)
	return lipgloss.JoinVertical(lipgloss.Left, header, inputStyle.Width(contentWidth).Render(content))
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
