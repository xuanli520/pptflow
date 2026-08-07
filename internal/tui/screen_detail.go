package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/purplevoid/harbor-factory/internal/app"
)

// detailModel renders one task as a decision surface. Its body scrolls inside a
// viewport so the header, breadcrumb, and footer stay pinned: previously the
// body rendered its natural height (~71 rows) regardless of the terminal, which
// pushed the chrome off the top of an alt-screen session.
type detailModel struct {
	task *TaskItem
	body *scrollPane
	// evidenceExpanded reveals the contract, claim, and lineage digests. They
	// are audit data, not decision data, so they stay collapsed by default.
	evidenceExpanded bool
}

func newDetailModel(task *TaskItem) *detailModel {
	return &detailModel{task: task, body: newScrollPane()}
}

func (d *detailModel) ToggleEvidence() {
	if d == nil {
		return
	}
	d.evidenceExpanded = !d.evidenceExpanded
	// Expanding changes the content length under the current offset, so return
	// to a known position instead of leaving the viewport mid-document.
	d.body.GoToStart()
}

// evidenceToggleHint states which way the [e] key will move, so the footer
// never claims to expand a section that is already open.
func (d *detailModel) evidenceToggleHint() string {
	if d != nil && d.evidenceExpanded {
		return "[e] 收起证据"
	}
	return "[e] 展开证据"
}

// The detail body scrolls through the one shared scroll primitive. These
// delegates exist so the key handler never reaches into the pane directly.

func (d *detailModel) MoveUp() {
	if d != nil {
		d.body.MoveUp()
	}
}

func (d *detailModel) MoveDown() {
	if d != nil {
		d.body.MoveDown()
	}
}

func (d *detailModel) PageUp() {
	if d != nil {
		d.body.PageUp()
	}
}

func (d *detailModel) PageDown() {
	if d != nil {
		d.body.PageDown()
	}
}

func (d *detailModel) GoToStart() {
	if d != nil {
		d.body.GoToStart()
	}
}

func (d *detailModel) GoToEnd() {
	if d != nil {
		d.body.GoToEnd()
	}
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

// View renders the fixed chrome plus exactly bodyRows of scrollable body.
func (d *detailModel) View(width, bodyRows int) string {
	if d == nil || d.task == nil {
		return mutedStyle.Render("未选择题目")
	}
	contentWidth := max(24, width)
	header := d.headerView(contentWidth)
	// The body receives whatever rows the header did not consume, so the
	// rendered total is always the budget the caller was given.
	remaining := max(1, bodyRows-lipgloss.Height(header))
	d.body.SetContent(d.bodyContent(contentWidth), contentWidth)
	d.body.Resize(contentWidth, remaining)
	return lipgloss.JoinVertical(lipgloss.Left, header, d.body.View())
}

func (d *detailModel) headerView(width int) string {
	if d.hasAuthoringLaunch() {
		launch := d.authoringLaunch()
		return lipgloss.JoinVertical(lipgloss.Left,
			detailBreadcrumbStyle.Render("题目管理 / 启动恢复"),
			detailTitleStyle.Width(max(1, width-2)).Render(truncateDisplay(launch.Title, max(1, width-4))),
			detailSubtitleStyle.Render("源码捕获失败，尚未创建 Task"),
		)
	}
	summary := "任务状态: " + d.task.Lifecycle + "  ·  标识: " + d.task.Slug
	return lipgloss.JoinVertical(lipgloss.Left,
		detailBreadcrumbStyle.Render("题目管理 / 任务详情"),
		detailTitleStyle.Width(max(1, width-2)).Render(truncateDisplay(d.task.Name, max(1, width-4))),
		detailSubtitleStyle.Render(truncateDisplay(summary, width)),
	)
}

// bodyContent assembles only what an operator needs to decide what to do next.
// Contract digests, claim comparisons, and artifact lineage move behind [e].
func (d *detailModel) bodyContent(width int) string {
	if d.hasAuthoringLaunch() {
		return d.authoringLaunchBody(width)
	}
	sections := []string{
		renderDetailSection("来源", detailFields(
			detailField("仓库", d.task.RepoURL, width),
			detailField("Commit SHA", d.task.CommitSHA, width),
		), width),
		renderDetailSection("当前运行", d.currentRunFields(width), width),
	}
	if failure := d.failureFields(width); failure != "" {
		sections = append(sections, renderDetailSection("失败原因", failure, width))
	}
	sections = append(sections, renderDetailSection("审核", d.reviewFields(width), width))
	if transcripts := d.agentTranscriptFields(width); transcripts != "" {
		sections = append(sections, renderDetailSection("Agent 回合", transcripts, width))
	}
	sections = append(sections, renderDetailSection("运行记录", d.historyFields(width), width))
	if d.evidenceExpanded {
		if identifiers := d.identifierFields(width); identifiers != "" {
			sections = append(sections, renderDetailSection("运行标识", identifiers, width))
		}
		if evidence := d.evidenceFields(width); evidence != "" {
			sections = append(sections, renderDetailSection("产物与契约", evidence, width))
		} else {
			sections = append(sections, renderDetailSection("产物与契约", mutedStyle.Render("当前 Run 尚无授权产物证据"), width))
		}
	}
	return lipgloss.JoinVertical(lipgloss.Left, sections...)
}

func (d *detailModel) authoringLaunchBody(width int) string {
	launch := d.authoringLaunch()
	if launch == nil {
		return mutedStyle.Render("未选择源码捕获启动")
	}
	failure := []string{detailField("错误码", launch.FailureCode, width)}
	summary := strings.TrimSpace(launch.FailureSummary)
	if summary == "" {
		summary = "未提供错误摘要"
	}
	failure = append(failure, styleFail.Render(wrapDisplay(summary, max(1, width-detailFieldGutter))))
	return lipgloss.JoinVertical(lipgloss.Left,
		renderDetailSection("来源", detailFields(
			detailField("仓库", launch.RepositoryURL, width),
			detailField("Commit SHA", launch.CommitSHA, width),
		), width),
		renderDetailSection("启动", detailFields(
			detailField("标识", launch.Slug, width),
			detailField("操作 ID", launch.OperationID, width),
			detailField("状态", launch.Status, width),
			detailField("创建时间", formatDetailTime(&launch.CreatedAt), width),
		), width),
		renderDetailSection("失败原因", detailFields(failure...), width),
	)
}

// currentRunFields is the decision core: what the Run is doing, what the
// business verdict is, and which recovery actions are available.
func (d *detailModel) currentRunFields(width int) string {
	run := d.currentRun()
	if run == nil {
		return mutedStyle.Render("尚未创建 Run")
	}
	stage := "-"
	if run.CurrentStage != "" {
		stage = displayStageName(run.CurrentStage)
	}
	retry := "可用"
	retryLabel := "重试"
	switch run.RetryStrategy {
	case app.TaskBoardRetryStrategyTaskContinuation:
		retryLabel = "断点恢复"
	case app.TaskBoardRetryStrategyStandardProtocolStage:
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
	logAvailability := "可读取（按 l）"
	if strings.TrimSpace(run.LogPath) == "" {
		logAvailability = "暂无本地日志"
	}

	fields := []string{
		detailField("Run ID", run.ID, width),
		detailField("状态", run.Status, width),
		detailField("当前阶段", stage, width),
	}
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
		detailField("开始时间", formatDetailTime(run.StartedAt, &run.CreatedAt), width),
		detailField("日志", logAvailability, width),
		detailField(retryLabel, retry, width),
		detailField("取消", cancel, width),
	)
	return detailFields(fields...)
}

func (d *detailModel) failureFields(width int) string {
	run := d.currentRun()
	if run == nil || (strings.TrimSpace(run.FailureSummary) == "" && strings.TrimSpace(run.FailureCode) == "") {
		return ""
	}
	stage := "当前 Run"
	if run.FailureStage != "" {
		stage = displayStageName(run.FailureStage)
	}
	summary := strings.TrimSpace(run.FailureSummary)
	if summary == "" {
		summary = "未提供错误摘要"
	}
	fields := []string{detailField("失败阶段", stage, width)}
	if code := strings.TrimSpace(run.FailureCode); code != "" {
		fields = append(fields, detailField("错误码", code, width))
	}
	if run.FailureRecordedAt != nil {
		fields = append(fields, detailField("记录时间", formatDetailTime(run.FailureRecordedAt), width))
	}
	if recovery := detailFailureRecoveryAction(run); recovery != "" {
		fields = append(fields, detailField("恢复操作", recovery, width))
	}
	fields = append(fields, styleFail.Render(wrapDisplay(summary, max(1, width-detailFieldGutter))))
	return detailFields(fields...)
}

// reviewFields states the gate situation plainly. A task with several open
// gates stays visible but is not made actionable by this compact surface.
func (d *detailModel) reviewFields(width int) string {
	if d.task.Review != nil {
		return detailFields(
			detailField("状态", "待审核", width),
			detailField("门禁", displayReviewKind(*d.task.Review), width),
			detailField("请求 ID", d.task.Review.RequestID, width),
			mutedStyle.Render("按 v 打开审核页查看待审产物与 agent 评审意见"),
		)
	}
	if d.task.OpenReviews > 1 {
		return styleFail.Render(wrapDisplay("存在多个待处理审核，请使用 CLI 选择明确的审核请求", max(1, width-detailFieldGutter)))
	}
	return mutedStyle.Render("当前没有待处理的审核门禁")
}

// identifierFields are the durable identifiers an operator needs to correlate a
// screen with the store, but not to choose an action. They live behind [e] so
// the default body stays a decision surface without losing diagnostics.
func (d *detailModel) identifierFields(width int) string {
	run := d.currentRun()
	if run == nil {
		return ""
	}
	fields := make([]string, 0, 4)
	logPath := run.LogPath
	if strings.TrimSpace(logPath) == "" {
		logPath = "暂无本地日志"
	}
	fields = append(fields, detailField("日志文件", logPath, width))
	if jobID := strings.TrimSpace(run.FailureJobID); jobID != "" {
		fields = append(fields, detailField("Job ID", jobID, width))
	}
	if artifactID := strings.TrimSpace(run.FailureArtifactID); artifactID != "" {
		fields = append(fields, detailField("Artifact ID", artifactID, width))
	}
	if retry := run.StandardProtocolRetry; retry != nil && retry.StageAttemptID != "" {
		fields = append(fields, detailField("阶段 attempt", retry.StageAttemptID, width))
	}
	return detailFields(fields...)
}

// evidenceFields is the collapsed audit block behind [e].
func (d *detailModel) evidenceFields(width int) string {
	run := d.currentRun()
	if run == nil || run.AuthoringEvidence == nil {
		return ""
	}
	evidence := run.AuthoringEvidence
	contract := evidence.Contract
	fields := []string{
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
	}
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
	return detailFields(fields...)
}

func (d *detailModel) agentTranscriptFields(width int) string {
	transcripts := d.agentTurnTranscripts()
	if len(transcripts) == 0 {
		return ""
	}
	limit := min(3, len(transcripts))
	fields := make([]string, 0, limit+1)
	for _, transcript := range transcripts[:limit] {
		status := transcript.SubmissionStatus
		if transcript.ProtocolRejectionCode != "" {
			status += " / " + transcript.ProtocolRejectionCode
		}
		if transcript.FailureCode != "" {
			status += " / " + transcript.FailureCode
		}
		label := displayStageName(transcript.StageKey) + fmt.Sprintf(" · 第 %d 回合", transcript.Turn)
		fields = append(fields, detailField(label, status, width))
	}
	if remaining := len(transcripts) - limit; remaining > 0 {
		fields = append(fields, mutedStyle.Render(fmt.Sprintf("另有 %d 个较早回合（按 p 查看）", remaining)))
	}
	return detailFields(fields...)
}

func detailFailureRecoveryAction(run *TaskRunItem) string {
	if run == nil {
		return ""
	}
	switch run.FailureRecoveryAction {
	case app.TaskBoardFailureRecoveryReconcile:
		return "显式 reconcile"
	case app.TaskBoardFailureRecoveryRepairOrNewRun:
		if run.CanRetry {
			return "按 t 断点恢复（同 Run 继续）"
		}
		if run.Status == "failed_terminal" {
			return "内容已判死：run restart 重跑（同一输入）"
		}
		return "不可恢复：run restart 重跑或新建创题"
	}
	return ""
}

// historyFields lists recent Runs. The body scrolls, so the list no longer
// shrinks itself based on terminal height.
func (d *detailModel) historyFields(width int) string {
	if len(d.task.Runs) == 0 {
		return mutedStyle.Render("尚无运行记录")
	}
	limit := min(4, len(d.task.Runs))
	rows := make([]string, 0, limit+1)
	for index := 0; index < limit; index++ {
		run := d.task.Runs[index]
		marker := mutedStyle.Render("历史")
		if run.ID == d.task.RunID {
			marker = detailHistoryCurrentStyle.Render("当前")
		}
		when := formatDetailTime(run.FinishedAt, run.StartedAt, &run.CreatedAt)
		row := marker + "  " + detailValueStyle.Render(truncateMiddleDisplay(run.ID, 16)) + "  " +
			runStatusStyle(run.Status) + "  " + mutedStyle.Render(when)
		rows = append(rows, truncateDisplay(row, max(1, width-detailFieldGutter)))
	}
	if remaining := len(d.task.Runs) - limit; remaining > 0 {
		rows = append(rows, mutedStyle.Render(fmt.Sprintf("另有 %d 条较早记录", remaining)))
	}
	return strings.Join(rows, "\n")
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
		return styleFail.Render(status)
	case "running", "queued", "waiting_review", "waiting_continuation", "paused":
		return statusRunningStyle.Render(status)
	default:
		return detailValueStyle.Render(status)
	}
}

// displayReviewKind names a gate in operator language.
func displayReviewKind(review app.TaskBoardReview) string {
	switch review.Kind {
	case app.TaskBoardAuthoringReview:
		return "创题门禁 (authoring)"
	case app.TaskBoardRevisionReview:
		return "任务修订门禁 (revision)"
	default:
		return string(review.Kind)
	}
}
