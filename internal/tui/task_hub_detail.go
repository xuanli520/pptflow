package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// TaskHubDetailTab groups the read-only lifecycle facts available from a
// Task/Run detail surface. The tab names are stable UI state, not lifecycle
// commands.
type TaskHubDetailTab string

const (
	TaskHubDetailOverviewTab  TaskHubDetailTab = "overview"
	TaskHubDetailRevisionsTab TaskHubDetailTab = "revisions"
	TaskHubDetailRunsTab      TaskHubDetailTab = "runs_stages"
	TaskHubDetailReleasesTab  TaskHubDetailTab = "releases"
	TaskHubDetailFactsTab     TaskHubDetailTab = "facts"
)

func taskHubDetailTabs() []TaskHubDetailTab {
	return []TaskHubDetailTab{
		TaskHubDetailOverviewTab,
		TaskHubDetailRevisionsTab,
		TaskHubDetailRunsTab,
		TaskHubDetailReleasesTab,
		TaskHubDetailFactsTab,
	}
}

func (tab TaskHubDetailTab) label() string {
	switch tab {
	case TaskHubDetailOverviewTab:
		return "概览"
	case TaskHubDetailRevisionsTab:
		return "修订"
	case TaskHubDetailRunsTab:
		return "运行/阶段"
	case TaskHubDetailReleasesTab:
		return "本地包"
	case TaskHubDetailFactsTab:
		return "证据/审核/返修"
	default:
		return "概览"
	}
}

// TaskHubDetailQuery identifies a durable Task and, optionally, the selected
// Run that contextualized the detail view. It cannot carry a filesystem path
// or mutation input.
type TaskHubDetailQuery struct {
	TaskID string `json:"task_id,omitempty"`
	RunID  string `json:"run_id,omitempty"`
}

// TaskHubDetailReader is an optional application-service read boundary. It
// keeps basic Task Hub implementations compatible while enabling a richer
// V2 detail screen when the real lifecycle adapter is present.
type TaskHubDetailReader interface {
	QueryTaskHubDetail(context.Context, TaskHubDetailQuery) (TaskHubDetail, error)
}

// TaskHubDetail is an UI-safe projection of immutable lifecycle facts. It
// deliberately excludes mutable workspace locations, manifest payload JSON,
// provider inputs, and audit reasons.
type TaskHubDetail struct {
	Task          TaskHubDetailTask     `json:"task"`
	SelectedRunID string                `json:"selected_run_id,omitempty"`
	Revisions     []TaskHubRevisionFact `json:"revisions,omitempty"`
	Runs          []TaskHubRunFact      `json:"runs,omitempty"`
	Releases      []TaskHubReleaseFact  `json:"releases,omitempty"`
	Artifacts     []TaskHubArtifactFact `json:"artifacts,omitempty"`
	Reviews       []TaskHubReviewFact   `json:"reviews,omitempty"`
	Repairs       []TaskHubRepairFact   `json:"repairs,omitempty"`
	ObservedAt    time.Time             `json:"observed_at"`
}

// TaskHubDetailTask is the stable Task identity and lifecycle summary.
type TaskHubDetailTask struct {
	TaskID            string    `json:"task_id"`
	Slug              string    `json:"slug,omitempty"`
	Name              string    `json:"name"`
	Lifecycle         string    `json:"lifecycle"`
	CurrentRevisionID string    `json:"current_revision_id,omitempty"`
	SourceRepo        string    `json:"source_repo,omitempty"`
	SourceCommit      string    `json:"source_commit,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// TaskHubRevisionFact summarizes an immutable revision without exposing its
// snapshot directory or mutable source data.
type TaskHubRevisionFact struct {
	RevisionID                 string    `json:"revision_id"`
	VersionNumber              int       `json:"version_number"`
	ParentRevisionID           string    `json:"parent_revision_id,omitempty"`
	Origin                     string    `json:"origin"`
	State                      string    `json:"state"`
	TaskDigest                 string    `json:"task_digest"`
	ValidationEvidenceManifest string    `json:"validation_evidence_manifest,omitempty"`
	ChangeSummary              string    `json:"change_summary,omitempty"`
	Current                    bool      `json:"current"`
	CreatedAt                  time.Time `json:"created_at"`
	StateUpdatedAt             time.Time `json:"state_updated_at"`
}

// TaskHubRunFact summarizes a frozen Run and all persisted stage attempts.
type TaskHubRunFact struct {
	RunID               string             `json:"run_id"`
	RevisionID          string             `json:"revision_id"`
	ParentRunID         string             `json:"parent_run_id,omitempty"`
	Status              string             `json:"status"`
	Trigger             string             `json:"trigger,omitempty"`
	ExecutionEpoch      int                `json:"execution_epoch"`
	WorkflowTemplateID  string             `json:"workflow_template_id,omitempty"`
	WorkflowTemplateVer string             `json:"workflow_template_version,omitempty"`
	ResolvedProfileHash string             `json:"resolved_profile_hash,omitempty"`
	DefinitionHash      string             `json:"definition_hash,omitempty"`
	CreatedAt           time.Time          `json:"created_at"`
	StartedAt           time.Time          `json:"started_at,omitempty"`
	FinishedAt          time.Time          `json:"finished_at,omitempty"`
	Stages              []TaskHubStageFact `json:"stages,omitempty"`
}

// TaskHubStageFact is a safe view of one stage attempt. Raw error text and
// runtime payloads remain intentionally unavailable to the TUI detail view.
type TaskHubStageFact struct {
	StageAttemptID     string    `json:"stage_attempt_id"`
	RetryOfStageID     string    `json:"retry_of_stage_id,omitempty"`
	StageKey           string    `json:"stage_key"`
	StageGroup         string    `json:"stage_group,omitempty"`
	Ordinal            int       `json:"ordinal"`
	ExecutionState     string    `json:"execution_state"`
	Verdict            string    `json:"verdict,omitempty"`
	FailureClass       string    `json:"failure_class,omitempty"`
	HasRecordedError   bool      `json:"has_recorded_error,omitempty"`
	ArtifactManifestID string    `json:"artifact_manifest_id,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	StartedAt          time.Time `json:"started_at,omitempty"`
	FinishedAt         time.Time `json:"finished_at,omitempty"`
}

// TaskHubReleaseFact describes a local managed package only. It intentionally
// does not expose a local package path or any external destination.
type TaskHubReleaseFact struct {
	ReleaseID      string    `json:"release_id"`
	ReleaseVersion string    `json:"release_version"`
	RevisionID     string    `json:"revision_id"`
	TaskDigest     string    `json:"task_digest"`
	EvidenceRef    string    `json:"evidence_ref,omitempty"`
	PublishedAt    time.Time `json:"published_at"`
	WithdrawnAt    time.Time `json:"withdrawn_at,omitempty"`
}

// TaskHubArtifactFact exposes immutable evidence lineage, never artifact
// payload bytes or file paths.
type TaskHubArtifactFact struct {
	ManifestID          string                   `json:"manifest_id"`
	RevisionID          string                   `json:"revision_id"`
	SubjectDigest       string                   `json:"subject_digest"`
	WorkflowFingerprint string                   `json:"workflow_fingerprint"`
	CreatedAt           time.Time                `json:"created_at"`
	Refs                []TaskHubArtifactRefFact `json:"refs,omitempty"`
}

// TaskHubArtifactRefFact is one typed immutable artifact reference.
type TaskHubArtifactRefFact struct {
	ArtifactKey   string `json:"artifact_key"`
	ContentDigest string `json:"content_digest"`
	SchemaVersion string `json:"schema_version,omitempty"`
	RunID         string `json:"run_id,omitempty"`
	StageKey      string `json:"stage_key,omitempty"`
	AttemptID     string `json:"attempt_id,omitempty"`
	TurnOrdinal   int    `json:"turn_ordinal"`
}

// TaskHubReviewFact makes review state and durable decisions inspectable
// without exposing reviewer reason text or a mutation command.
type TaskHubReviewFact struct {
	ReviewRequestID  string                      `json:"review_request_id"`
	RevisionID       string                      `json:"revision_id"`
	State            string                      `json:"state"`
	EvidenceManifest string                      `json:"evidence_manifest"`
	CreatedAt        time.Time                   `json:"created_at"`
	ClosedAt         time.Time                   `json:"closed_at,omitempty"`
	Decisions        []TaskHubReviewDecisionFact `json:"decisions,omitempty"`
}

// TaskHubReviewDecisionFact contains only the immutable decision outcome.
type TaskHubReviewDecisionFact struct {
	DecisionID             string    `json:"decision_id"`
	Action                 string    `json:"action"`
	ExpectedRevisionDigest string    `json:"expected_revision_digest"`
	CreatedAt              time.Time `json:"created_at"`
}

// TaskHubRepairFact summarizes bounded repair work and observed provider
// receipt outcomes. The provider payload and raw findings stay out of the UI.
type TaskHubRepairFact struct {
	RepairSessionID string                    `json:"repair_session_id"`
	SubjectID       string                    `json:"subject_id"`
	BaseRevisionID  string                    `json:"base_revision_id"`
	Status          string                    `json:"status"`
	MaxRounds       int                       `json:"max_rounds"`
	CreatedAt       time.Time                 `json:"created_at"`
	UpdatedAt       time.Time                 `json:"updated_at"`
	Changes         []TaskHubRepairChangeFact `json:"changes,omitempty"`
}

// TaskHubRepairChangeFact is a normalized, immutable provider change result.
type TaskHubRepairChangeFact struct {
	PreparedChangeID string                       `json:"prepared_change_id"`
	RoundOrdinal     int                          `json:"round_ordinal"`
	ProviderID       string                       `json:"provider_id"`
	BeforeDigest     string                       `json:"before_digest"`
	AfterDigest      string                       `json:"after_digest"`
	CreatedAt        time.Time                    `json:"created_at"`
	Receipts         []TaskHubMutationReceiptFact `json:"receipts,omitempty"`
}

// TaskHubMutationReceiptFact is the durable provider outcome summary.
type TaskHubMutationReceiptFact struct {
	ReceiptID string    `json:"receipt_id"`
	Outcome   string    `json:"outcome"`
	CreatedAt time.Time `json:"created_at"`
}

// Clone returns an independent detail snapshot suitable for Bubble Tea value
// updates. All slices are copied so an asynchronous message cannot share state
// with a later render.
func (detail TaskHubDetail) Clone() TaskHubDetail {
	detail.Revisions = append([]TaskHubRevisionFact(nil), detail.Revisions...)
	detail.Runs = append([]TaskHubRunFact(nil), detail.Runs...)
	for index := range detail.Runs {
		detail.Runs[index].Stages = append([]TaskHubStageFact(nil), detail.Runs[index].Stages...)
	}
	detail.Releases = append([]TaskHubReleaseFact(nil), detail.Releases...)
	detail.Artifacts = append([]TaskHubArtifactFact(nil), detail.Artifacts...)
	for index := range detail.Artifacts {
		detail.Artifacts[index].Refs = append([]TaskHubArtifactRefFact(nil), detail.Artifacts[index].Refs...)
	}
	detail.Reviews = append([]TaskHubReviewFact(nil), detail.Reviews...)
	for index := range detail.Reviews {
		detail.Reviews[index].Decisions = append([]TaskHubReviewDecisionFact(nil), detail.Reviews[index].Decisions...)
	}
	detail.Repairs = append([]TaskHubRepairFact(nil), detail.Repairs...)
	for index := range detail.Repairs {
		detail.Repairs[index].Changes = append([]TaskHubRepairChangeFact(nil), detail.Repairs[index].Changes...)
		for changeIndex := range detail.Repairs[index].Changes {
			detail.Repairs[index].Changes[changeIndex].Receipts = append([]TaskHubMutationReceiptFact(nil), detail.Repairs[index].Changes[changeIndex].Receipts...)
		}
	}
	return detail
}

type taskHubDetailLoadedMsg struct {
	query  TaskHubDetailQuery
	detail TaskHubDetail
	err    error
}

// TaskHubDetailOverlay is a read-only, keyboard and mouse navigable Task/Run
// detail surface. It is deliberately not a confirmation dialog and contains
// no mutation affordances. Selecting a review or release only captures a
// stable identity for a later two-key lifecycle plan; it never changes state.
type TaskHubDetailOverlay struct {
	Query                   TaskHubDetailQuery
	Detail                  TaskHubDetail
	Tab                     TaskHubDetailTab
	SelectedReviewRequestID string
	SelectedReleaseID       string
	Loading                 bool
	Error                   string
	Scroll                  int
}

func newTaskHubDetailOverlay(query TaskHubDetailQuery) *TaskHubDetailOverlay {
	return &TaskHubDetailOverlay{
		Query:   query,
		Tab:     TaskHubDetailOverviewTab,
		Loading: true,
	}
}

func (overlay *TaskHubDetailOverlay) Clone() *TaskHubDetailOverlay {
	if overlay == nil {
		return nil
	}
	clone := *overlay
	clone.Detail = overlay.Detail.Clone()
	return &clone
}

func (overlay *TaskHubDetailOverlay) Init() tea.Cmd { return nil }
func (overlay *TaskHubDetailOverlay) Focus()        {}
func (overlay *TaskHubDetailOverlay) Blur()         {}
func (overlay *TaskHubDetailOverlay) ZIndex() int   { return 110 }
func (overlay *TaskHubDetailOverlay) InterceptsAllKeys() bool {
	return true
}
func (overlay *TaskHubDetailOverlay) HandleKey(tea.KeyMsg) tea.Cmd { return nil }
func (overlay *TaskHubDetailOverlay) Update(tea.Msg) (bool, tea.Cmd) {
	return true, nil
}

func (overlay *TaskHubDetailOverlay) title() string {
	if overlay == nil {
		return "生命周期详情"
	}
	if strings.TrimSpace(overlay.Query.RunID) != "" {
		return "Run 详情"
	}
	return "Task 详情"
}

func (overlay *TaskHubDetailOverlay) cycleTab(delta int) {
	if overlay == nil {
		return
	}
	tabs := taskHubDetailTabs()
	current := 0
	for index, tab := range tabs {
		if tab == overlay.Tab {
			current = index
			break
		}
	}
	overlay.Tab = tabs[(current+delta+len(tabs))%len(tabs)]
	overlay.Scroll = 0
}

func (overlay *TaskHubDetailOverlay) scroll(delta, height int) {
	if overlay == nil || delta == 0 {
		return
	}
	contentRows := overlay.contentRows()
	available := taskHubDetailContentCapacity(height)
	maxScroll := maxInt(0, len(contentRows)-available)
	overlay.Scroll = clampInt(overlay.Scroll+delta, 0, maxScroll)
}

func (overlay *TaskHubDetailOverlay) selectableOpenReviews() []TaskHubReviewFact {
	if overlay == nil {
		return nil
	}
	reviews := make([]TaskHubReviewFact, 0, len(overlay.Detail.Reviews))
	for _, review := range overlay.Detail.Reviews {
		if strings.TrimSpace(review.ReviewRequestID) != "" && review.State == "open" {
			reviews = append(reviews, review)
		}
	}
	return reviews
}

func (overlay *TaskHubDetailOverlay) selectableActiveReleases() []TaskHubReleaseFact {
	if overlay == nil {
		return nil
	}
	releases := make([]TaskHubReleaseFact, 0, len(overlay.Detail.Releases))
	for _, release := range overlay.Detail.Releases {
		if strings.TrimSpace(release.ReleaseID) != "" && release.WithdrawnAt.IsZero() {
			releases = append(releases, release)
		}
	}
	return releases
}

func (overlay *TaskHubDetailOverlay) selectedOpenReview() (TaskHubReviewFact, bool) {
	for _, review := range overlay.selectableOpenReviews() {
		if review.ReviewRequestID == overlay.SelectedReviewRequestID {
			return review, true
		}
	}
	return TaskHubReviewFact{}, false
}

func (overlay *TaskHubDetailOverlay) selectedActiveRelease() (TaskHubReleaseFact, bool) {
	for _, release := range overlay.selectableActiveReleases() {
		if release.ReleaseID == overlay.SelectedReleaseID {
			return release, true
		}
	}
	return TaskHubReleaseFact{}, false
}

func (overlay *TaskHubDetailOverlay) normalizeSelections() {
	if overlay == nil {
		return
	}
	if _, found := overlay.selectedOpenReview(); !found {
		overlay.SelectedReviewRequestID = ""
	}
	if _, found := overlay.selectedActiveRelease(); !found {
		overlay.SelectedReleaseID = ""
	}
}

func cycleTaskHubDetailSelection[T any](values []T, current string, id func(T) string, delta int) (T, bool) {
	var zero T
	if len(values) == 0 {
		return zero, false
	}
	index := -1
	for candidate, value := range values {
		if id(value) == current {
			index = candidate
			break
		}
	}
	if index < 0 {
		if delta < 0 {
			return values[len(values)-1], true
		}
		return values[0], true
	}
	return values[(index+delta+len(values))%len(values)], true
}

func (overlay *TaskHubDetailOverlay) cycleOpenReview(delta int) (TaskHubReviewFact, bool) {
	review, found := cycleTaskHubDetailSelection(overlay.selectableOpenReviews(), overlay.SelectedReviewRequestID, func(value TaskHubReviewFact) string {
		return value.ReviewRequestID
	}, delta)
	if found {
		overlay.SelectedReviewRequestID = review.ReviewRequestID
	}
	return review, found
}

func (overlay *TaskHubDetailOverlay) cycleActiveRelease(delta int) (TaskHubReleaseFact, bool) {
	release, found := cycleTaskHubDetailSelection(overlay.selectableActiveReleases(), overlay.SelectedReleaseID, func(value TaskHubReleaseFact) string {
		return value.ReleaseID
	}, delta)
	if found {
		overlay.SelectedReleaseID = release.ReleaseID
	}
	return release, found
}

func (overlay *TaskHubDetailOverlay) View(width, height int) string {
	if overlay == nil {
		return ""
	}
	panelWidth := boundedPanelWidth(width, 40, 100)
	contentWidth := styleContentWidth(panelWidth, panelStyle)
	rows := []string{sectionStyle.Render(overlay.title()), overlay.tabLine(contentWidth)}
	if overlay.Loading {
		rows = append(rows, "", "正在读取受管生命周期事实...")
	} else if strings.TrimSpace(overlay.Error) != "" {
		rows = append(rows, "", failStyle.Render("读取失败："+overlay.Error))
	} else {
		content := overlay.contentRows()
		available := taskHubDetailContentCapacity(height)
		maxScroll := maxInt(0, len(content)-available)
		overlay.Scroll = clampInt(overlay.Scroll, 0, maxScroll)
		end := minInt(len(content), overlay.Scroll+available)
		if overlay.Scroll > 0 {
			rows = append(rows, subtleStyle.Render("↑ 更多"))
		}
		rows = append(rows, content[overlay.Scroll:end]...)
		if end < len(content) {
			rows = append(rows, subtleStyle.Render("↓ 更多"))
		}
	}
	rows = append(rows, "", subtleStyle.Render("[Tab] 切换分类  [↑↓] 浏览  [ / ] 选择目标  [r] 刷新  [Esc] 返回"))
	// The capacity above reserves both scroll indicators. This final guard also
	// keeps the panel bounded on terminals shorter than the regular chrome.
	rows = fitOverlayRows(rows, height, 1)
	rows = clipOverlayRows(rows, contentWidth)
	body := lipgloss.JoinVertical(lipgloss.Left, rows...)
	box := panelStyle.Width(panelWidth).Align(lipgloss.Left).Render(body)
	return lipgloss.Place(maxInt(1, width), maxInt(1, height), lipgloss.Center, lipgloss.Center, box)
}

func taskHubDetailContentCapacity(height int) int {
	// Title + tabs + footer consume four rows; reserve two more for possible
	// up/down indicators so scroll navigation never increases panel height.
	return maxInt(1, height-panelStyle.GetVerticalFrameSize()-6)
}

func (overlay *TaskHubDetailOverlay) tabLine(width int) string {
	parts := make([]string, 0, len(taskHubDetailTabs()))
	for _, tab := range taskHubDetailTabs() {
		label := tab.label()
		if tab == overlay.Tab {
			label = selectedStyle.Render(label)
		}
		parts = append(parts, label)
	}
	return fitDisplay(strings.Join(parts, "  "), width)
}

func (overlay *TaskHubDetailOverlay) contentRows() []string {
	if overlay == nil {
		return nil
	}
	switch overlay.Tab {
	case TaskHubDetailRevisionsTab:
		return overlay.revisionRows()
	case TaskHubDetailRunsTab:
		return overlay.runRows()
	case TaskHubDetailReleasesTab:
		return overlay.releaseRows()
	case TaskHubDetailFactsTab:
		return overlay.factRows()
	default:
		return overlay.overviewRows()
	}
}

func (overlay *TaskHubDetailOverlay) overviewRows() []string {
	task := overlay.Detail.Task
	rows := []string{
		"Task：" + emptyDash(task.Name),
		"Task ID：" + emptyDash(task.TaskID),
		"生命周期：" + emptyDash(task.Lifecycle),
		"当前 revision：" + emptyDash(task.CurrentRevisionID),
		"创建时间：" + taskHubDetailTime(task.CreatedAt),
		"更新时间：" + taskHubDetailTime(task.UpdatedAt),
	}
	if task.Slug != "" {
		rows = append(rows, "标识："+task.Slug)
	}
	if task.SourceRepo != "" {
		rows = append(rows, "来源仓库："+task.SourceRepo)
	}
	if task.SourceCommit != "" {
		rows = append(rows, "来源提交："+task.SourceCommit)
	}
	if overlay.Detail.SelectedRunID != "" {
		rows = append(rows, "", sectionStyle.Render("选中 Run"), "Run ID："+overlay.Detail.SelectedRunID)
		for _, run := range overlay.Detail.Runs {
			if run.RunID == overlay.Detail.SelectedRunID {
				rows = append(rows, "状态："+run.Status, "触发方式："+emptyDash(run.Trigger), "关联 revision："+run.RevisionID)
				break
			}
		}
	}
	rows = append(rows,
		"",
		sectionStyle.Render("可用事实"),
		fmt.Sprintf("修订 %d  |  Run %d  |  本地包 %d", len(overlay.Detail.Revisions), len(overlay.Detail.Runs), len(overlay.Detail.Releases)),
		fmt.Sprintf("工件 %d  |  审核 %d  |  返修 %d", len(overlay.Detail.Artifacts), len(overlay.Detail.Reviews), len(overlay.Detail.Repairs)),
		"读取时间："+taskHubDetailTime(overlay.Detail.ObservedAt),
	)
	return rows
}

func (overlay *TaskHubDetailOverlay) revisionRows() []string {
	if len(overlay.Detail.Revisions) == 0 {
		return []string{"暂无受管 TaskRevision。"}
	}
	rows := make([]string, 0, len(overlay.Detail.Revisions)*5)
	for _, revision := range overlay.Detail.Revisions {
		label := fmt.Sprintf("v%d  %s / %s", revision.VersionNumber, revision.State, revision.Origin)
		if revision.Current {
			label += "  当前"
		}
		rows = append(rows, sectionStyle.Render(label), "ID："+revision.RevisionID, "digest："+revision.TaskDigest)
		if revision.ParentRevisionID != "" {
			rows = append(rows, "父 revision："+revision.ParentRevisionID)
		}
		if revision.ValidationEvidenceManifest != "" {
			rows = append(rows, "验证证据："+revision.ValidationEvidenceManifest)
		}
		if revision.ChangeSummary != "" {
			rows = append(rows, "变更摘要："+revision.ChangeSummary)
		}
		rows = append(rows, "创建："+taskHubDetailTime(revision.CreatedAt), "")
	}
	return rows
}

func (overlay *TaskHubDetailOverlay) runRows() []string {
	if len(overlay.Detail.Runs) == 0 {
		return []string{"暂无受管 Run 或 StageAttempt。"}
	}
	rows := make([]string, 0, len(overlay.Detail.Runs)*7)
	for _, run := range overlay.Detail.Runs {
		label := "Run：" + run.RunID
		if run.RunID == overlay.Detail.SelectedRunID {
			label += "  当前查看"
		}
		rows = append(rows, sectionStyle.Render(label), "状态："+run.Status, "revision："+run.RevisionID, "触发："+emptyDash(run.Trigger))
		if run.ParentRunID != "" {
			rows = append(rows, "父 Run："+run.ParentRunID)
		}
		rows = append(rows, fmt.Sprintf("epoch %d  模板 %s@%s", run.ExecutionEpoch, emptyDash(run.WorkflowTemplateID), emptyDash(run.WorkflowTemplateVer)))
		if len(run.Stages) == 0 {
			rows = append(rows, subtleStyle.Render("暂无 StageAttempt"), "")
			continue
		}
		for _, stage := range run.Stages {
			stageLabel := fmt.Sprintf("阶段 %d：%s", stage.Ordinal, taskHubStageLabel(stage.StageGroup, stage.StageKey))
			rows = append(rows, stageLabel, "状态："+stage.ExecutionState+"  verdict："+emptyDash(stage.Verdict))
			if stage.FailureClass != "" || stage.HasRecordedError {
				failure := emptyDash(stage.FailureClass)
				if stage.HasRecordedError {
					failure += "（已记录错误，详情不显示原始内容）"
				}
				rows = append(rows, warnStyle.Render("失败事实："+failure))
			}
			if stage.ArtifactManifestID != "" {
				rows = append(rows, "工件 manifest："+stage.ArtifactManifestID)
			}
		}
		rows = append(rows, "")
	}
	return rows
}

func (overlay *TaskHubDetailOverlay) releaseRows() []string {
	if len(overlay.Detail.Releases) == 0 {
		return []string{"暂无本地受管 package。不会显示或联系任何外部 provider。"}
	}
	rows := make([]string, 0, len(overlay.Detail.Releases)*5)
	for _, release := range overlay.Detail.Releases {
		marker := "  "
		if release.ReleaseID == overlay.SelectedReleaseID && release.WithdrawnAt.IsZero() {
			marker = "> "
		}
		status := "已生成"
		if !release.WithdrawnAt.IsZero() {
			status = "已撤回"
		}
		rows = append(rows, sectionStyle.Render(marker+release.ReleaseVersion+"  "+status), "release ID："+release.ReleaseID, "revision："+release.RevisionID, "digest："+release.TaskDigest)
		if release.EvidenceRef != "" {
			rows = append(rows, "证据："+release.EvidenceRef)
		}
		rows = append(rows, "生成："+taskHubDetailTime(release.PublishedAt), "")
	}
	return rows
}

func (overlay *TaskHubDetailOverlay) factRows() []string {
	rows := []string{sectionStyle.Render("工件")}
	if len(overlay.Detail.Artifacts) == 0 {
		rows = append(rows, "暂无已登记的 immutable artifact manifest。")
	} else {
		for _, artifact := range overlay.Detail.Artifacts {
			rows = append(rows, "manifest："+artifact.ManifestID, "revision："+artifact.RevisionID, fmt.Sprintf("关联工件：%d", len(artifact.Refs)))
			for _, reference := range artifact.Refs {
				rows = append(rows, "  "+reference.ArtifactKey+"  "+reference.ContentDigest)
			}
		}
	}
	rows = append(rows, "", sectionStyle.Render("审核"))
	if len(overlay.Detail.Reviews) == 0 {
		rows = append(rows, "暂无 ReviewRequest。")
	} else {
		for _, review := range overlay.Detail.Reviews {
			marker := "  "
			if review.ReviewRequestID == overlay.SelectedReviewRequestID && review.State == "open" {
				marker = "> "
			}
			rows = append(rows, marker+"review："+review.ReviewRequestID+"  "+review.State, "revision："+review.RevisionID, "证据："+review.EvidenceManifest)
			if len(review.Decisions) == 0 {
				rows = append(rows, "  暂无 ReviewDecision")
			}
			for _, decision := range review.Decisions {
				rows = append(rows, "  决定："+decision.Action+"  "+taskHubDetailTime(decision.CreatedAt))
			}
		}
	}
	rows = append(rows, "", sectionStyle.Render("返修"))
	if len(overlay.Detail.Repairs) == 0 {
		rows = append(rows, "暂无 RepairSession。")
	} else {
		for _, repair := range overlay.Detail.Repairs {
			rows = append(rows, fmt.Sprintf("返修：%s  %s  %d 轮上限", repair.RepairSessionID, repair.Status, repair.MaxRounds), "基准 revision："+repair.BaseRevisionID)
			if len(repair.Changes) == 0 {
				rows = append(rows, "  暂无 PreparedChange")
			}
			for _, change := range repair.Changes {
				rows = append(rows, fmt.Sprintf("  第 %d 轮 / provider：%s", change.RoundOrdinal, change.ProviderID), "  digest："+change.BeforeDigest+" -> "+change.AfterDigest)
				if len(change.Receipts) == 0 {
					rows = append(rows, "    暂无 MutationReceipt")
				}
				for _, receipt := range change.Receipts {
					rows = append(rows, "    receipt："+receipt.Outcome+"  "+taskHubDetailTime(receipt.CreatedAt))
				}
			}
		}
	}
	return rows
}

func taskHubDetailTime(value time.Time) string {
	if value.IsZero() {
		return "-"
	}
	return value.Local().Format("2006-01-02 15:04:05")
}

func taskHubStageLabel(group, key string) string {
	group = strings.TrimSpace(group)
	key = strings.TrimSpace(key)
	switch {
	case group != "" && key != "" && group != key:
		return group + "/" + key
	case group != "":
		return group
	default:
		return emptyDash(key)
	}
}
