package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
)

// TaskHubDetailTab groups the read-only lifecycle facts available from a
// Task/Run detail surface. The tab names are stable UI state, not lifecycle
// commands.
type TaskHubDetailTab string

const (
	TaskHubDetailOverviewTab  TaskHubDetailTab = "overview"
	TaskHubDetailRevisionsTab TaskHubDetailTab = "revisions"
	TaskHubDetailRunsTab      TaskHubDetailTab = "runs_stages"
	TaskHubDetailFrozenTab    TaskHubDetailTab = "frozen_execution"
	TaskHubDetailReleasesTab  TaskHubDetailTab = "releases"
	TaskHubDetailFactsTab     TaskHubDetailTab = "facts"
)

const (
	taskHubCodeEdgeQwenTrialResultArtifactKey = "qwen_trial_result"
	taskHubCodeEdgeQwenPass4EvidenceKey       = "qwen_pass4_evidence"
	taskHubCodeEdgeOpusTrialResultArtifactKey = "opus_trial_result"
	taskHubCodeEdgeOpusPass4EvidenceKey       = "opus_pass4_evidence"
)

func taskHubDetailTabs() []TaskHubDetailTab {
	return []TaskHubDetailTab{
		TaskHubDetailOverviewTab,
		TaskHubDetailRevisionsTab,
		TaskHubDetailRunsTab,
		TaskHubDetailFrozenTab,
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
	case TaskHubDetailFrozenTab:
		return "冻结执行"
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
	// FrozenExecutions contains UI-safe projections of each Run's immutable
	// execution manifest. It contains only stable identities, policy values,
	// and catalog receipt facts; raw manifests, execution-spec payloads,
	// secrets, filesystem locations, and runtime attestation payloads remain
	// unavailable to the TUI.
	FrozenExecutions []TaskHubFrozenExecutionFact `json:"frozen_executions,omitempty"`
	// CodeEdgeCompliance contains the compact final-compliance record identity
	// for CodeEdge Runs. Raw evaluator receipts, submission reports, final
	// decision documents, and package authorization documents are never
	// projected into the TUI.
	CodeEdgeCompliance []TaskHubCodeEdgeComplianceFact `json:"codeedge_compliance,omitempty"`
	// CodeEdgeEvaluatorEvidenceHandoffs belongs only to a Phase-1 parent Run.
	// The child remains the owner of Qwen/Opus artifacts and trial history; the
	// parent displays the immutable adoption bridge rather than pretending it
	// executed those stages itself.
	CodeEdgeEvaluatorEvidenceHandoffs []TaskHubCodeEdgeEvaluatorEvidenceHandoffFact `json:"codeedge_evaluator_evidence_handoffs,omitempty"`
	Releases                          []TaskHubReleaseFact                          `json:"releases,omitempty"`
	Artifacts                         []TaskHubArtifactFact                         `json:"artifacts,omitempty"`
	Reviews                           []TaskHubReviewFact                           `json:"reviews,omitempty"`
	Repairs                           []TaskHubRepairFact                           `json:"repairs,omitempty"`
	ObservedAt                        time.Time                                     `json:"observed_at"`
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

// TaskHubCodeEdgeComplianceState is the conservative display state of an
// immutable CodeEdge final-compliance record. An approved record is only a
// recorded authorization fact; package execution still re-verifies the
// frozen Run and all linked evidence through the application service.
type TaskHubCodeEdgeComplianceState string

const (
	TaskHubCodeEdgeComplianceNotRecorded TaskHubCodeEdgeComplianceState = "not_recorded"
	TaskHubCodeEdgeComplianceApproved    TaskHubCodeEdgeComplianceState = "approved"
	TaskHubCodeEdgeComplianceRejected    TaskHubCodeEdgeComplianceState = "rejected"
	TaskHubCodeEdgeComplianceInvalid     TaskHubCodeEdgeComplianceState = "invalid"
)

// TaskHubCodeEdgeComplianceFact contains only durable public identities and
// fingerprints. In particular it cannot expose receipt JSON, authorization
// JSON, provider input, secret values, or operator audit reasons.
type TaskHubCodeEdgeComplianceFact struct {
	RunID                    string                         `json:"run_id"`
	State                    TaskHubCodeEdgeComplianceState `json:"state"`
	ComplianceRecordID       string                         `json:"compliance_record_id,omitempty"`
	RevisionID               string                         `json:"revision_id,omitempty"`
	TaskDigest               string                         `json:"task_digest,omitempty"`
	DecisionFingerprint      string                         `json:"decision_fingerprint,omitempty"`
	AuthorizationFingerprint string                         `json:"authorization_fingerprint,omitempty"`
	RecordedAt               time.Time                      `json:"recorded_at,omitempty"`
}

// TaskHubCodeEdgeEvaluatorEvidenceHandoffState is deliberately weaker than a
// package authorization: it tells an operator whether a durable handoff is
// structurally bound to the parent and child Run. The gate and package path
// re-verify every child artifact, trial, and frozen binding before relying on
// it, so the read-only TUI never overstates validation.
type TaskHubCodeEdgeEvaluatorEvidenceHandoffState string

const (
	TaskHubCodeEdgeEvaluatorEvidenceHandoffNotRecorded TaskHubCodeEdgeEvaluatorEvidenceHandoffState = "not_recorded"
	TaskHubCodeEdgeEvaluatorEvidenceHandoffRecorded    TaskHubCodeEdgeEvaluatorEvidenceHandoffState = "recorded"
	TaskHubCodeEdgeEvaluatorEvidenceHandoffInvalid     TaskHubCodeEdgeEvaluatorEvidenceHandoffState = "invalid"
)

// TaskHubCodeEdgeEvaluatorEvidenceHandoffFact is the safe parent-side bridge
// projection. It excludes canonical handoff JSON, child artifacts, result
// payloads, prompt/model config, endpoint values, and secret data.
type TaskHubCodeEdgeEvaluatorEvidenceHandoffFact struct {
	ParentRunID        string                                       `json:"parent_run_id"`
	State              TaskHubCodeEdgeEvaluatorEvidenceHandoffState `json:"state"`
	HandoffID          string                                       `json:"handoff_id,omitempty"`
	ChildRunID         string                                       `json:"child_run_id,omitempty"`
	HandoffFingerprint string                                       `json:"handoff_fingerprint,omitempty"`
	RecordedAt         time.Time                                    `json:"recorded_at,omitempty"`
}

// TaskHubFrozenExecutionState describes whether the projection can prove the
// stored manifest is structurally usable for read-only display and bound to
// the durable Run row. It is intentionally not a runtime-attestation state:
// a detail query never contacts a worker, executable, image registry, or
// secret provider.
type TaskHubFrozenExecutionState string

const (
	TaskHubFrozenExecutionBound       TaskHubFrozenExecutionState = "bound"
	TaskHubFrozenExecutionUnavailable TaskHubFrozenExecutionState = "unavailable"
	TaskHubFrozenExecutionInvalid     TaskHubFrozenExecutionState = "invalid"
)

// TaskHubDeploymentCatalogState describes only the frozen catalog-receipt
// binding visible in a Run manifest. A receipt being bound does not assert
// that the currently installed deployment lock or runtime attestation has
// passed; those facts require a separate application-service projection.
type TaskHubDeploymentCatalogState string

const (
	TaskHubDeploymentCatalogBound       TaskHubDeploymentCatalogState = "bound"
	TaskHubDeploymentCatalogNotRecorded TaskHubDeploymentCatalogState = "not_recorded"
	TaskHubDeploymentCatalogInvalid     TaskHubDeploymentCatalogState = "invalid"
)

// TaskHubDeploymentCatalogLockState applies the same conservative projection
// rule to the compact operation-catalog lock identity frozen with a Run. It
// is not a statement that a worker's live runtime attestation passed.
type TaskHubDeploymentCatalogLockState string

const (
	TaskHubDeploymentCatalogLockBound       TaskHubDeploymentCatalogLockState = "bound"
	TaskHubDeploymentCatalogLockNotRecorded TaskHubDeploymentCatalogLockState = "not_recorded"
	TaskHubDeploymentCatalogLockInvalid     TaskHubDeploymentCatalogLockState = "invalid"
)

// TaskHubDeploymentCatalogFact is the safe identity of a catalog receipt
// frozen with a Run. It intentionally excludes catalog operations, secret
// values, deployment paths, and lock/runtime-attestation payloads.
type TaskHubDeploymentCatalogFact struct {
	State              TaskHubDeploymentCatalogState     `json:"state"`
	CatalogID          string                            `json:"catalog_id,omitempty"`
	CatalogVersion     string                            `json:"catalog_version,omitempty"`
	TemplateID         string                            `json:"template_id,omitempty"`
	TemplateVersion    string                            `json:"template_version,omitempty"`
	CatalogFingerprint string                            `json:"catalog_fingerprint,omitempty"`
	LockState          TaskHubDeploymentCatalogLockState `json:"lock_state"`
	LockID             string                            `json:"lock_id,omitempty"`
	LockVersion        string                            `json:"lock_version,omitempty"`
	LockFingerprint    string                            `json:"lock_fingerprint,omitempty"`
}

// TaskHubFrozenExecutionFact is a read-only summary of a frozen Run manifest.
// The State is deliberately conservative: invalid or unavailable manifests do
// not expose a partial projection that could be mistaken for an executable
// contract.
type TaskHubFrozenExecutionFact struct {
	RunID                       string                       `json:"run_id"`
	State                       TaskHubFrozenExecutionState  `json:"state"`
	TemplateID                  string                       `json:"template_id,omitempty"`
	TemplateVersion             string                       `json:"template_version,omitempty"`
	ExecutionProfileID          string                       `json:"execution_profile_id,omitempty"`
	ExecutionProfileVersion     string                       `json:"execution_profile_version,omitempty"`
	ContinuationPlanTTL         time.Duration                `json:"continuation_plan_ttl,omitempty"`
	ControlGracePeriod          time.Duration                `json:"control_grace_period,omitempty"`
	TemplateFingerprint         string                       `json:"template_fingerprint,omitempty"`
	ProfileFingerprint          string                       `json:"profile_fingerprint,omitempty"`
	DefinitionFingerprint       string                       `json:"definition_fingerprint,omitempty"`
	ResolvedManifestFingerprint string                       `json:"resolved_manifest_fingerprint,omitempty"`
	InitialPlanFingerprint      string                       `json:"initial_plan_fingerprint,omitempty"`
	InputBundleID               string                       `json:"input_bundle_id,omitempty"`
	ExecutionSpecFingerprint    string                       `json:"execution_spec_fingerprint,omitempty"`
	DeploymentCatalog           TaskHubDeploymentCatalogFact `json:"deployment_catalog"`
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
	detail.FrozenExecutions = append([]TaskHubFrozenExecutionFact(nil), detail.FrozenExecutions...)
	detail.CodeEdgeCompliance = append([]TaskHubCodeEdgeComplianceFact(nil), detail.CodeEdgeCompliance...)
	detail.CodeEdgeEvaluatorEvidenceHandoffs = append([]TaskHubCodeEdgeEvaluatorEvidenceHandoffFact(nil), detail.CodeEdgeEvaluatorEvidenceHandoffs...)
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
	case TaskHubDetailFrozenTab:
		return overlay.frozenExecutionRows()
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
		fmt.Sprintf("冻结执行 %d（按 Tab 查看目录与评测证据摘要）", len(overlay.Detail.FrozenExecutions)),
		"读取时间："+taskHubDetailTime(overlay.Detail.ObservedAt),
	)
	if len(overlay.Detail.CodeEdgeCompliance) > 0 {
		rows = append(rows, fmt.Sprintf("CodeEdge 最终合规记录 %d（仅投影状态和稳定指纹）", len(overlay.Detail.CodeEdgeCompliance)))
	}
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

// frozenExecutionRows renders only immutable identities already projected by
// the application-facing detail reader. It never opens the managed Run
// directory, decodes raw artifact bytes, or initiates a provider/runtime
// verification from the TUI process.
func (overlay *TaskHubDetailOverlay) frozenExecutionRows() []string {
	if len(overlay.Detail.FrozenExecutions) == 0 {
		return []string{"暂无可投影的冻结执行说明书。该详情读取不会猜测 profile、catalog 或评测结果。"}
	}
	facts := append([]TaskHubFrozenExecutionFact(nil), overlay.Detail.FrozenExecutions...)
	sort.SliceStable(facts, func(left, right int) bool {
		leftSelected := facts[left].RunID == overlay.Detail.SelectedRunID
		rightSelected := facts[right].RunID == overlay.Detail.SelectedRunID
		if leftSelected != rightSelected {
			return leftSelected
		}
		return facts[left].RunID < facts[right].RunID
	})
	rows := make([]string, 0, len(facts)*18)
	for _, fact := range facts {
		label := "Run：" + emptyDash(fact.RunID)
		if fact.RunID == overlay.Detail.SelectedRunID {
			label += "  当前查看"
		}
		rows = append(rows, sectionStyle.Render(label))
		switch fact.State {
		case TaskHubFrozenExecutionBound:
			rows = append(rows, "冻结 manifest：已解析并与 durable Run 身份一致")
		case TaskHubFrozenExecutionUnavailable:
			rows = append(rows, warnStyle.Render("冻结 manifest：未记录；不会从当前配置或工作目录推测执行契约"))
		case TaskHubFrozenExecutionInvalid:
			rows = append(rows, failStyle.Render("冻结 manifest：无法通过只读结构/身份绑定检查；不显示部分执行契约"))
		default:
			rows = append(rows, failStyle.Render("冻结 manifest：未知投影状态"))
		}
		rows = append(rows, overlay.codeEdgeComplianceRows(fact.RunID)...)
		if fact.State != TaskHubFrozenExecutionBound {
			rows = append(rows, "")
			continue
		}
		rows = append(rows,
			"模板："+fact.TemplateID+"@"+fact.TemplateVersion,
			"执行 profile："+fact.ExecutionProfileID+"@"+fact.ExecutionProfileVersion,
			"continuation TTL："+taskHubDuration(fact.ContinuationPlanTTL),
			"控制 grace："+taskHubDuration(fact.ControlGracePeriod),
			"模板 fingerprint："+fact.TemplateFingerprint,
			"profile fingerprint："+fact.ProfileFingerprint,
			"定义 fingerprint："+fact.DefinitionFingerprint,
			"解析 manifest fingerprint："+fact.ResolvedManifestFingerprint,
			"初始执行计划 fingerprint："+fact.InitialPlanFingerprint,
			"输入 bundle："+emptyDash(fact.InputBundleID),
			"execution spec fingerprint："+fact.ExecutionSpecFingerprint,
		)
		rows = append(rows, overlay.deploymentCatalogRows(fact.DeploymentCatalog)...)
		rows = append(rows, overlay.evaluationEvidenceRows(fact.RunID)...)
		rows = append(rows, "")
	}
	return rows
}

func (overlay *TaskHubDetailOverlay) codeEdgeComplianceRows(runID string) []string {
	for _, fact := range overlay.Detail.CodeEdgeCompliance {
		if fact.RunID != runID {
			continue
		}
		rows := []string{sectionStyle.Render("CodeEdge 最终合规（只读记录）")}
		switch fact.State {
		case TaskHubCodeEdgeComplianceNotRecorded:
			rows = append(rows, warnStyle.Render("状态：未记录 approved 最终合规；该 Run 不能授权本地 package。"))
		case TaskHubCodeEdgeComplianceApproved:
			rows = append(rows, "状态：approved（打包时仍会重验冻结 Run、catalog/lock 和全部证据）")
		case TaskHubCodeEdgeComplianceRejected:
			rows = append(rows, failStyle.Render("状态：rejected；该 Run 不能授权本地 package。"))
		case TaskHubCodeEdgeComplianceInvalid:
			rows = append(rows, failStyle.Render("状态：记录绑定无效；不会将其视为 package 授权。"))
		default:
			rows = append(rows, failStyle.Render("状态：未知；不会将其视为 package 授权。"))
		}
		if fact.ComplianceRecordID != "" {
			rows = append(rows, "合规记录："+fact.ComplianceRecordID)
		}
		if fact.RevisionID != "" {
			rows = append(rows, "revision："+fact.RevisionID)
		}
		if fact.TaskDigest != "" {
			rows = append(rows, "digest："+fact.TaskDigest)
		}
		if fact.DecisionFingerprint != "" {
			rows = append(rows, "最终决定 fingerprint："+fact.DecisionFingerprint)
		}
		if fact.AuthorizationFingerprint != "" {
			rows = append(rows, "打包授权 fingerprint："+fact.AuthorizationFingerprint)
		}
		if !fact.RecordedAt.IsZero() {
			rows = append(rows, "记录时间："+taskHubDetailTime(fact.RecordedAt))
		}
		rows = append(rows, subtleStyle.Render("不会显示 evaluator receipt、submission report 或 package authorization 原文。"))
		return rows
	}
	return nil
}

func taskHubDuration(value time.Duration) string {
	if value < 0 {
		return "-"
	}
	return value.String()
}

func (overlay *TaskHubDetailOverlay) deploymentCatalogRows(catalog TaskHubDeploymentCatalogFact) []string {
	rows := []string{sectionStyle.Render("Deployment catalog（冻结 receipt）")}
	switch catalog.State {
	case TaskHubDeploymentCatalogBound:
		rows = append(rows,
			"catalog："+catalog.CatalogID+"@"+catalog.CatalogVersion,
			"catalog 模板："+catalog.TemplateID+"@"+catalog.TemplateVersion,
			"catalog fingerprint："+catalog.CatalogFingerprint,
		)
		switch catalog.LockState {
		case TaskHubDeploymentCatalogLockBound:
			rows = append(rows,
				"operation lock："+catalog.LockID+"@"+catalog.LockVersion,
				"lock fingerprint："+catalog.LockFingerprint,
			)
		case TaskHubDeploymentCatalogLockInvalid:
			rows = append(rows, failStyle.Render("冻结 operation lock identity 无法通过只读结构检查。"))
		case TaskHubDeploymentCatalogLockNotRecorded, "":
			rows = append(rows, warnStyle.Render("未冻结 operation lock identity；不能显示受验证的生产 lock 状态。"))
		default:
			rows = append(rows, failStyle.Render("operation lock identity 状态未知。"))
		}
		rows = append(rows, "运行时 attestation：此只读详情不声明已通过；worker 会在外部副作用前重验。")
	case TaskHubDeploymentCatalogNotRecorded:
		rows = append(rows, warnStyle.Render("未冻结 deployment catalog receipt；不会假定当前部署、PATH、镜像或模型可执行。"))
	case TaskHubDeploymentCatalogInvalid:
		rows = append(rows, failStyle.Render("deployment catalog receipt 无法通过只读结构/模板绑定检查。"))
	default:
		rows = append(rows, failStyle.Render("deployment catalog receipt 状态未知。"))
	}
	return rows
}

// evaluationEvidenceRows deliberately summarizes only immutable artifact
// lineage already supplied by the detail reader. The actual EvaluationReceipt
// payload is not read by the TUI, so it cannot be re-parsed, silently trusted,
// or mistaken for a complete trial/compliance result.
func (overlay *TaskHubDetailOverlay) evaluationEvidenceRows(runID string) []string {
	run, found := overlay.runFact(runID)
	if !found {
		return []string{sectionStyle.Render("评测证据（immutable lineage）"), "该 Run 未出现在只读详情中；不会按 artifact key 推测评测状态。"}
	}
	if taskHubIsCodeEdgePhase1RunFact(run) {
		return overlay.codeEdgeParentEvidenceHandoffRows(run)
	}
	if taskHubIsCodeEdgeEvaluatorChildRunFact(run) {
		return overlay.codeEdgeChildEvaluationEvidenceRows(run)
	}
	return overlay.genericEvaluationEvidenceRows(runID)
}

func (overlay *TaskHubDetailOverlay) genericEvaluationEvidenceRows(runID string) []string {
	rows := []string{sectionStyle.Render("评测证据（immutable lineage）")}
	stages := overlay.evaluationStagesForRun(runID)
	if len(stages) == 0 {
		return append(rows, "该 Run 暂无 evaluation StageAttempt；不会按 stage 名称或 artifact key 猜测评测。")
	}
	for _, stage := range stages {
		rows = append(rows,
			"阶段："+taskHubStageLabel(stage.StageGroup, stage.StageKey)+"  状态："+emptyDash(stage.ExecutionState)+"  verdict："+emptyDash(stage.Verdict),
			"StageAttempt："+stage.StageAttemptID,
		)
		references := overlay.evaluationEvidenceRefs(runID, stage)
		if len(references) == 0 {
			rows = append(rows, "  暂无与该 StageAttempt 精确绑定的 immutable evidence ref。")
		} else {
			for _, reference := range references {
				rows = append(rows, "  evidence："+reference.ArtifactKey+"  "+reference.ContentDigest+"  "+emptyDash(reference.SchemaVersion))
			}
		}
		rows = append(rows, subtleStyle.Render("  受验证评测摘要：尚未提供（不会读取 artifact 原文或猜测四次 trial/合规结论）。"))
	}
	return rows
}

type taskHubCodeEdgeEvaluationSlot struct {
	Role                     string
	StageKey                 string
	TrialResultArtifactKey   string
	Pass4EvidenceArtifactKey string
}

func taskHubCodeEdgeEvaluationSlots() [2]taskHubCodeEdgeEvaluationSlot {
	return [2]taskHubCodeEdgeEvaluationSlot{
		{
			Role:                     "Qwen",
			StageKey:                 workflowadapter.HarborRunQwen,
			TrialResultArtifactKey:   taskHubCodeEdgeQwenTrialResultArtifactKey,
			Pass4EvidenceArtifactKey: taskHubCodeEdgeQwenPass4EvidenceKey,
		},
		{
			Role:                     "Opus",
			StageKey:                 workflowadapter.HarborRunOpus,
			TrialResultArtifactKey:   taskHubCodeEdgeOpusTrialResultArtifactKey,
			Pass4EvidenceArtifactKey: taskHubCodeEdgeOpusPass4EvidenceKey,
		},
	}
}

// codeEdgeEvaluationEvidenceRows renders the evidence identity expected by
// the closed CodeEdge template. It intentionally reports only registration
// metadata: a ref being present does not validate its payload or authorize a
// continuation, retry, or package.
func (overlay *TaskHubDetailOverlay) codeEdgeChildEvaluationEvidenceRows(run TaskHubRunFact) []string {
	rows := []string{sectionStyle.Render("CodeEdge Harbor 评测证据（immutable lineage）")}
	if taskHubReconciliationRequired(run.Status) {
		rows = append(rows, warnStyle.Render("Run 状态："+run.Status+"；需 reconcile；TUI 不会继续或自动重跑。"))
	}
	stages := overlay.evaluationStagesForRun(run.RunID)
	for _, slot := range taskHubCodeEdgeEvaluationSlots() {
		matched := make([]TaskHubStageFact, 0, 1)
		for _, stage := range stages {
			if stage.StageKey == slot.StageKey {
				matched = append(matched, stage)
			}
		}
		if len(matched) == 0 {
			rows = append(rows, slot.Role+" Harbor 运行：暂无 StageAttempt；不会按 artifact key 推测已运行。")
			continue
		}
		for _, stage := range matched {
			rows = append(rows, sectionStyle.Render(slot.Role+" Harbor 运行："+stage.StageAttemptID))
			rows = append(rows, slot.Role+" StageAttempt 状态："+emptyDash(stage.ExecutionState)+"  verdict："+emptyDash(stage.Verdict))
			if taskHubReconciliationRequired(stage.ExecutionState) {
				rows = append(rows, warnStyle.Render(slot.Role+" StageAttempt 状态："+stage.ExecutionState+"；需 reconcile；TUI 不会继续或自动重跑。"))
			}
			references := overlay.evaluationEvidenceRefs(run.RunID, stage)
			rows = append(rows,
				taskHubExpectedEvidenceStatus(slot.Role+" Harbor 运行证据包状态", slot.TrialResultArtifactKey, references),
				taskHubExpectedEvidenceStatus(slot.Role+" 截图状态", slot.Pass4EvidenceArtifactKey, references),
			)
		}
	}
	rows = append(rows, subtleStyle.Render("不会读取 Harbor result、截图或 artifact 原文；上述状态仅表示 immutable ref 是否已登记。"))
	return rows
}

// codeEdgeParentEvidenceHandoffRows renders the parent/child topology
// explicitly. The parent never has Qwen/Opus StageAttempts in the V2.2
// descriptor, so showing those names here would be an unsafe UI fabrication.
func (overlay *TaskHubDetailOverlay) codeEdgeParentEvidenceHandoffRows(run TaskHubRunFact) []string {
	rows := []string{sectionStyle.Render("CodeEdge evaluator evidence handoff（immutable lineage）")}
	if taskHubReconciliationRequired(run.Status) {
		rows = append(rows, warnStyle.Render("Run 状态："+run.Status+"；需 reconcile；TUI 不会继续或自动重跑。"))
	}
	for _, fact := range overlay.Detail.CodeEdgeEvaluatorEvidenceHandoffs {
		if fact.ParentRunID != run.RunID {
			continue
		}
		switch fact.State {
		case TaskHubCodeEdgeEvaluatorEvidenceHandoffNotRecorded:
			return append(rows, warnStyle.Render("状态：尚未采用已完成 evaluator child 的不可变证据；不能批准该 gate 或进入 submission。"))
		case TaskHubCodeEdgeEvaluatorEvidenceHandoffRecorded:
			return append(rows,
				"状态：已记录；gate 与 package 仍会重新验证 child artifacts、四个逻辑 Trial 与冻结绑定。",
				"handoff ID："+fact.HandoffID,
				"evaluator child Run："+fact.ChildRunID,
				"handoff fingerprint："+fact.HandoffFingerprint,
			)
		case TaskHubCodeEdgeEvaluatorEvidenceHandoffInvalid:
			return append(rows, failStyle.Render("状态：持久 handoff 与父/子 Run 绑定无效；不会将其视为可批准证据。"))
		default:
			return append(rows, failStyle.Render("状态：未知；不会将其视为可批准证据。"))
		}
	}
	return append(rows, warnStyle.Render("状态：未记录 evaluator evidence handoff；不能批准该 gate 或进入 submission。"))
}

func taskHubExpectedEvidenceStatus(label, key string, references []TaskHubArtifactRefFact) string {
	matches := make([]TaskHubArtifactRefFact, 0, 1)
	for _, reference := range references {
		if reference.ArtifactKey == key {
			matches = append(matches, reference)
		}
	}
	switch len(matches) {
	case 0:
		return label + "：未登记（需要 " + key + "）"
	case 1:
		reference := matches[0]
		return label + "：已登记（" + key + "  " + reference.ContentDigest + "  " + emptyDash(reference.SchemaVersion) + "）"
	default:
		return label + "：登记了 " + fmt.Sprintf("%d", len(matches)) + " 条 " + key + " 引用；不会将其视为单一受验证证据。"
	}
}

func taskHubReconciliationRequired(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "in_doubt", "interrupted":
		return true
	default:
		return false
	}
}

func taskHubIsCodeEdgePhase1RunFact(run TaskHubRunFact) bool {
	return run.WorkflowTemplateID == workflowadapter.CodeEdgePhase1WorkflowTemplateID &&
		run.WorkflowTemplateVer == workflowadapter.CodeEdgePhase1WorkflowTemplateVersion
}

func taskHubIsCodeEdgeEvaluatorChildRunFact(run TaskHubRunFact) bool {
	return run.WorkflowTemplateID == workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateID &&
		run.WorkflowTemplateVer == workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateVersion
}

func (overlay *TaskHubDetailOverlay) runFact(runID string) (TaskHubRunFact, bool) {
	for _, run := range overlay.Detail.Runs {
		if run.RunID == runID {
			return run, true
		}
	}
	return TaskHubRunFact{}, false
}

func (overlay *TaskHubDetailOverlay) evaluationStagesForRun(runID string) []TaskHubStageFact {
	for _, run := range overlay.Detail.Runs {
		if run.RunID != runID {
			continue
		}
		stages := make([]TaskHubStageFact, 0, len(run.Stages))
		for _, stage := range run.Stages {
			if strings.EqualFold(strings.TrimSpace(stage.StageGroup), "evaluation") {
				stages = append(stages, stage)
			}
		}
		sort.SliceStable(stages, func(left, right int) bool {
			if stages[left].Ordinal != stages[right].Ordinal {
				return stages[left].Ordinal < stages[right].Ordinal
			}
			return stages[left].StageAttemptID < stages[right].StageAttemptID
		})
		return stages
	}
	return nil
}

func (overlay *TaskHubDetailOverlay) evaluationEvidenceRefs(runID string, stage TaskHubStageFact) []TaskHubArtifactRefFact {
	refs := make([]TaskHubArtifactRefFact, 0)
	for _, artifact := range overlay.Detail.Artifacts {
		for _, reference := range artifact.Refs {
			if reference.RunID == runID && reference.StageKey == stage.StageKey && reference.AttemptID == stage.StageAttemptID {
				refs = append(refs, reference)
			}
		}
	}
	sort.SliceStable(refs, func(left, right int) bool {
		if refs[left].ArtifactKey != refs[right].ArtifactKey {
			return refs[left].ArtifactKey < refs[right].ArtifactKey
		}
		if refs[left].ContentDigest != refs[right].ContentDigest {
			return refs[left].ContentDigest < refs[right].ContentDigest
		}
		return refs[left].SchemaVersion < refs[right].SchemaVersion
	})
	return refs
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
