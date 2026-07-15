package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
)

const taskHubPrefixTimeout = 1200 * time.Millisecond

// TaskHubTab is the top-level Task Hub projection selected by the operator.
type TaskHubTab string

const (
	TaskHubTasksTab TaskHubTab = "tasks"
	TaskHubRunsTab  TaskHubTab = "runs"
	TaskHubQueueTab TaskHubTab = "queue"
)

func (tab TaskHubTab) valid() bool {
	switch tab {
	case TaskHubTasksTab, TaskHubRunsTab, TaskHubQueueTab:
		return true
	default:
		return false
	}
}

// TaskHubAction is a semantic lifecycle action. The TUI never implements its
// business behavior; it asks the injected lifecycle service for a plan.
type TaskHubAction string

const (
	TaskHubActionNewTask    TaskHubAction = "task.new"
	TaskHubActionImportTask TaskHubAction = "task.import"
	// TaskHubActionStartStandardAuthoring starts the deployment-owned Standard
	// authoring flow for an operator-supplied, exact repository coordinate. It
	// is global because it creates a new draft Task together with its
	// source/session Run; it must never bind a selected TaskRevision.
	TaskHubActionStartStandardAuthoring TaskHubAction = "authoring.standard.start"
	TaskHubActionEditTask               TaskHubAction = "task.edit"
	TaskHubActionForkTask               TaskHubAction = "task.fork"
	TaskHubActionArchiveTask            TaskHubAction = "task.archive"
	TaskHubActionSoftDeleteTask         TaskHubAction = "task.soft_delete"
	TaskHubActionRestoreTask            TaskHubAction = "task.restore"
	TaskHubActionContinue               TaskHubAction = "execution.continue"
	TaskHubActionStartRun               TaskHubAction = "execution.start"
	TaskHubActionEvaluateCodeEdge       TaskHubAction = "evaluation.codeedge.start"
	// TaskHubActionAdoptCodeEdgeEvaluatorEvidenceHandoff adopts the verified
	// Qwen/Opus evidence of one completed evaluator child Run into its durable
	// Phase-1 parent. The selected Run is always the child; the adapter derives
	// the parent from immutable child lineage rather than accepting it from UI
	// input.
	TaskHubActionAdoptCodeEdgeEvaluatorEvidenceHandoff TaskHubAction = "evaluation.codeedge.adopt_evidence_handoff"
	TaskHubActionAttachRun                             TaskHubAction = "execution.attach"
	TaskHubActionOpenRunControl                        TaskHubAction = "execution.open_control"
	TaskHubActionApproveReview                         TaskHubAction = "review.approve"
	TaskHubActionRequestChanges                        TaskHubAction = "review.request_changes"
	TaskHubActionRejectReview                          TaskHubAction = "review.reject_terminal"
	TaskHubActionPackageRevision                       TaskHubAction = "release.local_package"
	TaskHubActionWithdrawRelease                       TaskHubAction = "release.withdraw"
)

// TaskHubActionState is supplied by the lifecycle service for the currently
// selected subject. Disabled actions are rendered with their authoritative
// reason instead of being guessed from UI-local state.
type TaskHubActionState struct {
	Action         TaskHubAction `json:"action"`
	Enabled        bool          `json:"enabled"`
	DisabledReason string        `json:"disabled_reason,omitempty"`
}

// TaskHubTask is the UI-safe task projection returned by a lifecycle query.
// It contains stable identities and summary facts, never a mutable workspace
// path or database handle.
type TaskHubTask struct {
	TaskID                     string               `json:"task_id"`
	Name                       string               `json:"name"`
	Lifecycle                  string               `json:"lifecycle"`
	RevisionID                 string               `json:"revision_id,omitempty"`
	Revision                   string               `json:"revision,omitempty"`
	TaskDigest                 string               `json:"task_digest,omitempty"`
	LatestRelease              string               `json:"latest_release,omitempty"`
	ActiveReview               string               `json:"active_review,omitempty"`
	ActiveReviewID             string               `json:"active_review_id,omitempty"`
	ActiveReviewRevisionID     string               `json:"active_review_revision_id,omitempty"`
	ActiveAuthoringReviewID    string               `json:"active_authoring_review_id,omitempty"`
	ActiveAuthoringReviewRunID string               `json:"active_authoring_review_run_id,omitempty"`
	ActiveRepair               string               `json:"active_repair,omitempty"`
	ArtifactBytes              int64                `json:"artifact_bytes,omitempty"`
	UpdatedAt                  time.Time            `json:"updated_at"`
	Actions                    []TaskHubActionState `json:"actions,omitempty"`
}

// Clone returns an independent projection value.
func (task TaskHubTask) Clone() TaskHubTask {
	task.Actions = append([]TaskHubActionState(nil), task.Actions...)
	return task
}

// TaskHubRunControlAction identifies an operator-initiated Run action whose
// impact can be inspected from the Run Control overlay. Most actions create a
// durable ControlOperation; reconcile instead invokes the same scoped local
// recovery boundary as `harbor run reconcile`. Selecting any action in the
// TUI never creates it.
type TaskHubRunControlAction string

const (
	TaskHubRunControlPause       TaskHubRunControlAction = "pause"
	TaskHubRunControlCancelStage TaskHubRunControlAction = "cancel_stage"
	TaskHubRunControlTerminate   TaskHubRunControlAction = "terminate"
	TaskHubRunControlReconcile   TaskHubRunControlAction = "reconcile"
)

// TaskHubRunControlActionState is an authoritative capability projection.
// The adapter reports disabled reasons rather than having the TUI infer them
// from status strings or missing local state.
type TaskHubRunControlActionState struct {
	Action         TaskHubRunControlAction `json:"action"`
	Enabled        bool                    `json:"enabled"`
	DisabledReason string                  `json:"disabled_reason,omitempty"`
}

// TaskHubControlCheckpoint is the complete optimistic identity captured while
// an operator reviews a run-control action. A later confirmation must carry
// this exact value to the application service; it must never silently target a
// newer execution epoch, task revision, or workflow definition.
type TaskHubControlCheckpoint struct {
	Sequence            uint64 `json:"sequence"`
	ExecutionEpoch      int    `json:"execution_epoch"`
	SubjectVersion      int64  `json:"subject_version"`
	SubjectID           string `json:"subject_id,omitempty"`
	SubjectRevisionID   string `json:"subject_revision_id,omitempty"`
	SubjectDigest       string `json:"subject_digest,omitempty"`
	WorkflowFingerprint string `json:"workflow_fingerprint,omitempty"`
}

// TaskHubLifecycleCheckpoint is the full task/revision/run/release/review CAS
// identity captured by a lifecycle plan preview. It is passed unchanged by the
// confirmation form and must be rejected when any relevant durable fact has
// moved rather than silently rebound to newer state.
type TaskHubLifecycleCheckpoint struct {
	TaskID               string `json:"task_id,omitempty"`
	TaskVersion          int64  `json:"task_version,omitempty"`
	RevisionID           string `json:"revision_id,omitempty"`
	RevisionStateVersion int64  `json:"revision_state_version,omitempty"`
	RevisionDigest       string `json:"revision_digest,omitempty"`
	RunID                string `json:"run_id,omitempty"`
	RunVersion           int64  `json:"run_version,omitempty"`
	RunExecutionEpoch    int    `json:"run_execution_epoch,omitempty"`
	RunDefinitionHash    string `json:"run_definition_hash,omitempty"`
	// CodeEdgeComplianceRecordID and CodeEdgeAuthorizationFingerprint bind a
	// CodeEdge local-package confirmation to the immutable authorization that
	// was observed for the selected frozen Run. They are never user-editable.
	CodeEdgeComplianceRecordID       string `json:"codeedge_compliance_record_id,omitempty"`
	CodeEdgeAuthorizationFingerprint string `json:"codeedge_authorization_fingerprint,omitempty"`
	ReleaseID                        string `json:"release_id,omitempty"`
	ReleaseRecordVersion             int64  `json:"release_record_version,omitempty"`
	ReviewRequestID                  string `json:"review_request_id,omitempty"`
	ReviewRevisionID                 string `json:"review_revision_id,omitempty"`
	ReviewState                      string `json:"review_state,omitempty"`
	ReviewEvidenceDigest             string `json:"review_evidence_digest,omitempty"`
}

// TaskHubAuthoringReviewCheckpoint is the source/session review counterpart
// to TaskHubLifecycleCheckpoint. It never contains a TaskRevision identity.
type TaskHubAuthoringReviewCheckpoint struct {
	ReviewRequestID        string `json:"review_request_id,omitempty"`
	BindingID              string `json:"binding_id,omitempty"`
	RunID                  string `json:"run_id,omitempty"`
	AuthoringSessionID     string `json:"authoring_session_id,omitempty"`
	AuthoringSourceID      string `json:"authoring_source_id,omitempty"`
	SourceSnapshotDigest   string `json:"source_snapshot_digest,omitempty"`
	DefinitionHash         string `json:"definition_hash,omitempty"`
	StageAttemptID         string `json:"stage_attempt_id,omitempty"`
	InputFingerprint       string `json:"input_fingerprint,omitempty"`
	EvidenceManifestDigest string `json:"evidence_manifest_digest,omitempty"`
	RunVersion             int64  `json:"run_version,omitempty"`
	StageAttemptVersion    int64  `json:"stage_attempt_version,omitempty"`
}

// TaskHubRunControl is a read-only summary of the durable control facts that
// matter while an operator is considering an action. The confirmation form
// supplies the actor, reason, and idempotency key; this projection supplies the
// immutable target checkpoint that the application service validates.
type TaskHubRunControl struct {
	StageAttemptID      string `json:"stage_attempt_id,omitempty"`
	StageExecutionState string `json:"stage_execution_state,omitempty"`
	CheckpointSequence  uint64 `json:"checkpoint_sequence,omitempty"`
	ExecutionEpoch      int    `json:"execution_epoch,omitempty"`
	// GracePeriod is read from the frozen run manifest. It is deliberately a
	// projection rather than a confirmation-form field: an operator may inspect
	// the policy but cannot alter termination semantics for one command.
	GracePeriod            time.Duration                  `json:"grace_period,omitempty"`
	OperationID            string                         `json:"operation_id,omitempty"`
	OperationAction        TaskHubRunControlAction        `json:"operation_action,omitempty"`
	OperationStatus        string                         `json:"operation_status,omitempty"`
	CheckpointID           string                         `json:"checkpoint_id,omitempty"`
	QuotaSettlementID      string                         `json:"quota_settlement_id,omitempty"`
	RuntimeReceiptCount    int                            `json:"runtime_receipt_count,omitempty"`
	ExternalOutcomeUnknown bool                           `json:"external_outcome_unknown,omitempty"`
	FailureReason          string                         `json:"failure_reason,omitempty"`
	Expected               TaskHubControlCheckpoint       `json:"expected"`
	Actions                []TaskHubRunControlActionState `json:"actions,omitempty"`
}

// Clone returns an independent control projection suitable for Bubble Tea
// value updates.
func (control TaskHubRunControl) Clone() TaskHubRunControl {
	control.Actions = append([]TaskHubRunControlActionState(nil), control.Actions...)
	return control
}

// TaskHubWorkerHandoff is the UI-safe, read-only summary of the most recent
// controlled child-worker handoff for a Run. Process IDs and log paths remain
// diagnostics in the durable store and are intentionally not rendered by the
// Task Hub.
type TaskHubWorkerHandoff struct {
	OperationID      string    `json:"operation_id"`
	State            string    `json:"state"`
	WorkerLeaseID    string    `json:"worker_lease_id,omitempty"`
	LaunchDeadlineAt time.Time `json:"launch_deadline_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	FailureRecorded  bool      `json:"failure_recorded,omitempty"`
}

// Clone returns an independent handoff summary.
func (handoff *TaskHubWorkerHandoff) Clone() *TaskHubWorkerHandoff {
	if handoff == nil {
		return nil
	}
	copy := *handoff
	return &copy
}

// TaskHubRunHandoffCheckpoint is the complete Run identity an operator
// inspected before requesting a controlled child worker. It is deliberately
// separate from control checkpoints: handoff must reject a new Run version,
// execution epoch, or frozen definition instead of silently targeting it.
type TaskHubRunHandoffCheckpoint struct {
	RunVersion     int64  `json:"run_version"`
	ExecutionEpoch int    `json:"execution_epoch"`
	DefinitionHash string `json:"definition_hash"`
}

// TaskHubRunHandoffCapability is the authoritative handoff capability
// projection for one active Run. A disabled Run remains visible in the exit
// panel so the operator can see why no child worker needs or may receive it.
type TaskHubRunHandoffCapability struct {
	Enabled        bool                        `json:"enabled"`
	DisabledReason string                      `json:"disabled_reason,omitempty"`
	Expected       TaskHubRunHandoffCheckpoint `json:"expected"`
}

// TaskHubRun is the UI-safe run projection returned by a lifecycle query.
type TaskHubRun struct {
	RunID          string                      `json:"run_id"`
	TaskID         string                      `json:"task_id"`
	RevisionID     string                      `json:"revision_id,omitempty"`
	ExecutionState string                      `json:"execution_state"`
	Stage          string                      `json:"stage,omitempty"`
	Failure        string                      `json:"failure,omitempty"`
	ControlStatus  string                      `json:"control_status,omitempty"`
	Active         bool                        `json:"active"`
	QueuePosition  int                         `json:"queue_position,omitempty"`
	StartedAt      time.Time                   `json:"started_at"`
	FinishedAt     time.Time                   `json:"finished_at,omitempty"`
	Actions        []TaskHubActionState        `json:"actions,omitempty"`
	Control        TaskHubRunControl           `json:"control,omitempty"`
	WorkerHandoff  *TaskHubWorkerHandoff       `json:"worker_handoff,omitempty"`
	Handoff        TaskHubRunHandoffCapability `json:"handoff"`
}

// Clone returns an independent projection value.
func (run TaskHubRun) Clone() TaskHubRun {
	run.Actions = append([]TaskHubActionState(nil), run.Actions...)
	run.Control = run.Control.Clone()
	run.WorkerHandoff = run.WorkerHandoff.Clone()
	return run
}

// TaskHubQueue exposes admission state without allowing the TUI to enqueue or
// mutate jobs directly.
type TaskHubQueue struct {
	Running int `json:"running"`
	Queued  int `json:"queued"`
	// Concurrency is zero when the lifecycle backend has not exposed a
	// configured capacity pool. It is not a claim that dispatch capacity is zero.
	Concurrency int       `json:"concurrency"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// TaskHubSnapshot is the single read model returned by a lifecycle service.
// GlobalActions are used for actions with no selected Task or Run, such as
// creating or importing a task.
type TaskHubSnapshot struct {
	Tasks         []TaskHubTask        `json:"tasks"`
	Runs          []TaskHubRun         `json:"runs"`
	Queue         TaskHubQueue         `json:"queue"`
	GlobalActions []TaskHubActionState `json:"global_actions,omitempty"`
	ObservedAt    time.Time            `json:"observed_at"`
}

// Clone returns an independent snapshot suitable for Bubble Tea value updates.
func (snapshot TaskHubSnapshot) Clone() TaskHubSnapshot {
	tasks := snapshot.Tasks
	snapshot.Tasks = make([]TaskHubTask, len(tasks))
	for index, task := range tasks {
		snapshot.Tasks[index] = task.Clone()
	}
	runs := snapshot.Runs
	snapshot.Runs = make([]TaskHubRun, len(runs))
	for index, run := range runs {
		snapshot.Runs[index] = run.Clone()
	}
	snapshot.GlobalActions = append([]TaskHubActionState(nil), snapshot.GlobalActions...)
	return snapshot
}

// TaskHubQuery describes a UI read request. Lifecycle filtering and access
// control remain the service's responsibility.
type TaskHubQuery struct {
	Tab    TaskHubTab `json:"tab"`
	Filter string     `json:"filter,omitempty"`
}

// TaskHubTarget identifies the selected immutable lifecycle subject without
// accepting an arbitrary workspace path from the TUI.
type TaskHubTarget struct {
	TaskID           string `json:"task_id,omitempty"`
	RunID            string `json:"run_id,omitempty"`
	RevisionID       string `json:"revision_id,omitempty"`
	StageAttemptID   string `json:"stage_attempt_id,omitempty"`
	ReviewRequestID  string `json:"review_request_id,omitempty"`
	ReviewRevisionID string `json:"review_revision_id,omitempty"`
	// AuthoringReviewRequestID names a source/session gate and is never
	// interpreted as a TaskRevision ReviewRequest.
	AuthoringReviewRequestID string `json:"authoring_review_request_id,omitempty"`
	ReleaseID                string `json:"release_id,omitempty"`
}

// TaskHubCommand is a plan request. The service may use CommandID to make a
// later confirmation idempotent, but this TUI layer only asks for the plan.
type TaskHubCommand struct {
	CommandID string        `json:"command_id,omitempty"`
	Action    TaskHubAction `json:"action"`
	Target    TaskHubTarget `json:"target"`
}

// TaskHubPlanPreview is a UI projection of a frozen lifecycle plan. It keeps
// the planner explanation visible before any confirmation/execution step.
type TaskHubPlanPreview struct {
	PlanID                  string                           `json:"plan_id,omitempty"`
	Title                   string                           `json:"title"`
	Summary                 string                           `json:"summary"`
	Reason                  string                           `json:"reason,omitempty"`
	RevisionImpact          string                           `json:"revision_impact,omitempty"`
	ExecutionScope          []string                         `json:"execution_scope,omitempty"`
	InvalidatedEvidence     []string                         `json:"invalidated_evidence,omitempty"`
	ReusedEvidence          []string                         `json:"reused_evidence,omitempty"`
	BudgetImpact            string                           `json:"budget_impact,omitempty"`
	ExternalEffects         []string                         `json:"external_effects,omitempty"`
	ConfirmationNeeded      bool                             `json:"confirmation_needed"`
	Expected                TaskHubLifecycleCheckpoint       `json:"expected,omitempty"`
	AuthoringReviewExpected TaskHubAuthoringReviewCheckpoint `json:"authoring_review_expected,omitempty"`
}

// Clone returns an independent plan preview.
func (preview TaskHubPlanPreview) Clone() TaskHubPlanPreview {
	preview.ExecutionScope = append([]string(nil), preview.ExecutionScope...)
	preview.InvalidatedEvidence = append([]string(nil), preview.InvalidatedEvidence...)
	preview.ReusedEvidence = append([]string(nil), preview.ReusedEvidence...)
	preview.ExternalEffects = append([]string(nil), preview.ExternalEffects...)
	return preview
}

// taskHubPlanExplanationRows renders the complete fixed consequence set that
// every plan preview must expose before confirmation. Values are projections
// of frozen planner facts; the TUI never infers an execution scope or evidence
// reuse decision on its own.
func taskHubPlanExplanationRows(preview TaskHubPlanPreview, width int) []string {
	width = maxInt(1, width)
	line := func(label, value string) string {
		return subtleStyle.Render(clipDisplay(label+value, width))
	}
	list := func(values []string, fallback string) string {
		clean := make([]string, 0, len(values))
		for _, value := range values {
			if value = strings.TrimSpace(value); value != "" {
				clean = append(clean, value)
			}
		}
		if len(clean) == 0 {
			return fallback
		}
		return strings.Join(clean, "、")
	}
	value := func(raw, fallback string) string {
		if raw = strings.TrimSpace(raw); raw != "" {
			return raw
		}
		return fallback
	}
	externalEffects := list(preview.ExternalEffects, "无")
	rows := []string{
		line("原因：", value(preview.Reason, "未提供")),
		line("Task 版本影响：", value(preview.RevisionImpact, "未声明")),
		line("将执行：", list(preview.ExecutionScope, "无")),
		line("将失效：", list(preview.InvalidatedEvidence, "无")),
		line("将复用：", list(preview.ReusedEvidence, "无")),
		line("预算变化：", value(preview.BudgetImpact, "未声明")),
	}
	if len(preview.ExternalEffects) == 0 {
		rows = append(rows, line("外部副作用：", externalEffects))
	} else {
		rows = append(rows, warnStyle.Render(clipDisplay("外部副作用："+externalEffects, width)))
	}
	return rows
}

// TaskHubLifecycleService is the sole V2 TUI boundary for lifecycle data and
// commands. An app-service adapter owns all SQLite, filesystem, worker, and
// business mutations; this package only queries and renders plan previews.
type TaskHubLifecycleService interface {
	QueryTaskHub(context.Context, TaskHubQuery) (TaskHubSnapshot, error)
	PlanTaskHubCommand(context.Context, TaskHubCommand) (TaskHubPlanPreview, error)
}

// TaskHubMutationRequest is submitted only after a native Task Hub
// confirmation form collected a non-empty reason, derived a local OS actor,
// and allocated a UUIDv7 idempotency key. Values holds action-specific typed
// fields such as a local package version or import snapshot source. It never
// carries a mutable target workspace, database path, or execution default.
type TaskHubMutationRequest struct {
	Action                  TaskHubAction                    `json:"action"`
	Target                  TaskHubTarget                    `json:"target"`
	PlanID                  string                           `json:"plan_id,omitempty"`
	Actor                   string                           `json:"actor"`
	Reason                  string                           `json:"reason"`
	IdempotencyKey          string                           `json:"idempotency_key"`
	Expected                TaskHubLifecycleCheckpoint       `json:"expected"`
	AuthoringReviewExpected TaskHubAuthoringReviewCheckpoint `json:"authoring_review_expected,omitempty"`
	Values                  map[string]string                `json:"values,omitempty"`
}

// Clone returns an independent command value for asynchronous Bubble Tea work.
func (request TaskHubMutationRequest) Clone() TaskHubMutationRequest {
	request.Values = cloneTaskHubMutationValues(request.Values)
	return request
}

func cloneTaskHubMutationValues(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}

// TaskHubPreparedMutation is a durable or read-only prepared command awaiting
// a final explicit confirmation. Continuation planning returns a frozen PlanID;
// other actions may return the existing preview unchanged when no separate
// durable planning phase is required.
type TaskHubPreparedMutation struct {
	Preview TaskHubPlanPreview `json:"preview"`
	// Actor and Reason are the immutable command provenance returned after a
	// continuation plan freezes. The confirmation form must render and submit
	// these values verbatim on its second confirmation.
	Actor  string `json:"actor,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// TaskHubMutationResult is a UI-safe receipt of a confirmed lifecycle action.
// It intentionally contains identities and descriptions only, never mutable
// filesystem paths or provider payloads.
type TaskHubMutationResult struct {
	Action      TaskHubAction `json:"action"`
	Target      TaskHubTarget `json:"target"`
	PlanID      string        `json:"plan_id,omitempty"`
	ExecutionID string        `json:"execution_id,omitempty"`
	ReceiptID   string        `json:"receipt_id,omitempty"`
	Summary     string        `json:"summary"`
}

// TaskHubMutationPlanner is optional. It is used for actions such as
// continuation whose application service must first freeze a durable plan from
// the confirmation form's actor, reason, and idempotency key.
type TaskHubMutationPlanner interface {
	PrepareTaskHubMutation(context.Context, TaskHubMutationRequest) (TaskHubPreparedMutation, error)
}

// TaskHubMutationExecutor is the only V2 Task Hub mutation boundary. The TUI
// never accesses SQLite, managed snapshots, workers, or providers directly.
type TaskHubMutationExecutor interface {
	ExecuteTaskHubMutation(context.Context, TaskHubMutationRequest) (TaskHubMutationResult, error)
}

// TaskHubRunControlCommand requests an impact preview only. The command
// deliberately cannot carry mutation credentials or confirmation data.
type TaskHubRunControlCommand struct {
	Action TaskHubRunControlAction `json:"action"`
	Target TaskHubTarget           `json:"target"`
}

// TaskHubRunControlPlanner is optional so read-only Task Hub services remain
// usable while adapters add durable control-preview support. Implementations
// must only read authoritative facts and must never create a ControlOperation.
type TaskHubRunControlPlanner interface {
	PlanTaskHubRunControl(context.Context, TaskHubRunControlCommand) (TaskHubPlanPreview, error)
}

// TaskHubRunControlMutationRequest is sent after the Run Control overlay has
// shown its impact preview and the native confirmation form collected input.
// Expected is the immutable checkpoint captured by the preview for durable
// control actions. The scoped local reconcile action deliberately follows the
// CLI contract and uses only its explicit Run, actor, and audit reason.
type TaskHubRunControlMutationRequest struct {
	Action         TaskHubRunControlAction  `json:"action"`
	Target         TaskHubTarget            `json:"target"`
	Expected       TaskHubControlCheckpoint `json:"expected"`
	Actor          string                   `json:"actor"`
	Reason         string                   `json:"reason"`
	IdempotencyKey string                   `json:"idempotency_key"`
}

// TaskHubRunControlMutationResult is a UI-safe confirmed Run-action receipt.
type TaskHubRunControlMutationResult struct {
	Action      TaskHubRunControlAction `json:"action"`
	OperationID string                  `json:"operation_id,omitempty"`
	Summary     string                  `json:"summary"`
}

// TaskHubRunControlMutationExecutor owns the confirmed Run action.
// ControlOperation actions must use the supplied expected checkpoint rather
// than recomputing one after the operator has confirmed the preview. Local
// reconciliation intentionally matches the CLI's scoped recovery contract.
type TaskHubRunControlMutationExecutor interface {
	ExecuteTaskHubRunControlMutation(context.Context, TaskHubRunControlMutationRequest) (TaskHubRunControlMutationResult, error)
}

// TaskHubRunHandoffRequest is a narrow, per-Run request emitted only after an
// operator has chosen that Run in the TUI exit panel. Both operation identity
// fields are UUIDv7 values allocated when the panel opens so a lost response
// replays the same durable handoff rather than spawning another child.
type TaskHubRunHandoffRequest struct {
	RunID              string                      `json:"run_id"`
	Expected           TaskHubRunHandoffCheckpoint `json:"expected"`
	HandoffOperationID string                      `json:"handoff_operation_id"`
	IdempotencyKey     string                      `json:"idempotency_key"`
	Owner              string                      `json:"owner"`
	Actor              string                      `json:"actor"`
	Reason             string                      `json:"reason"`
}

// TaskHubRunHandoffResult is a UI-safe receipt. It never exposes the child
// PID or managed log path, which are retained as controlled diagnostics.
type TaskHubRunHandoffResult struct {
	RunID       string `json:"run_id"`
	OperationID string `json:"operation_id"`
	State       string `json:"state"`
	Summary     string `json:"summary"`
}

// TaskHubRunHandoffExecutor is intentionally optional. The TUI can still
// render a read-only Task Hub without a local process launcher, but selected
// exit handoffs require this exact application-service contract.
type TaskHubRunHandoffExecutor interface {
	ExecuteTaskHubRunHandoff(context.Context, TaskHubRunHandoffRequest) (TaskHubRunHandoffResult, error)
}

// TaskHubRow aggregates one Task with its latest Run for the Tasks tab.
type TaskHubRow struct {
	Task           TaskHubTask
	LatestRun      TaskHubRun
	HasLatestRun   bool
	RunCount       int
	ActiveRunCount int
}

// TaskHubState is the local immutable-ish view state owned by the TUI. All
// facts are copies from TaskHubLifecycleService; it has no store or filesystem
// pointer and cannot perform lifecycle mutations by itself.
type TaskHubState struct {
	Snapshot                         TaskHubSnapshot
	Rows                             []TaskHubRow
	Query                            TaskHubQuery
	SelectedTaskID                   string
	SelectedRunID                    string
	SelectedReviewTaskID             string
	SelectedReviewRequestID          string
	SelectedReviewRevisionID         string
	SelectedAuthoringReviewTaskID    string
	SelectedAuthoringReviewRequestID string
	SelectedAuthoringReviewRunID     string
	SelectedReleaseTaskID            string
	SelectedReleaseID                string
	Loading                          bool
	LastRefresh                      time.Time
}

func newTaskHubState() TaskHubState {
	return TaskHubState{Query: TaskHubQuery{Tab: TaskHubTasksTab}}
}

// Clone returns an independent Bubble Tea update value.
func (state TaskHubState) Clone() TaskHubState {
	state.Snapshot = state.Snapshot.Clone()
	rows := state.Rows
	state.Rows = make([]TaskHubRow, len(rows))
	for index, row := range rows {
		state.Rows[index] = TaskHubRow{
			Task:           row.Task.Clone(),
			LatestRun:      row.LatestRun.Clone(),
			HasLatestRun:   row.HasLatestRun,
			RunCount:       row.RunCount,
			ActiveRunCount: row.ActiveRunCount,
		}
	}
	return state
}

func (state TaskHubState) selectedReviewForTask(taskID string) (requestID, revisionID string, found bool) {
	if strings.TrimSpace(state.SelectedReviewTaskID) != strings.TrimSpace(taskID) {
		return "", "", false
	}
	requestID = strings.TrimSpace(state.SelectedReviewRequestID)
	revisionID = strings.TrimSpace(state.SelectedReviewRevisionID)
	return requestID, revisionID, requestID != "" && revisionID != ""
}

func (state TaskHubState) selectedAuthoringReviewForTask(taskID string) (requestID, runID string, found bool) {
	if strings.TrimSpace(state.SelectedAuthoringReviewTaskID) != strings.TrimSpace(taskID) {
		return "", "", false
	}
	requestID = strings.TrimSpace(state.SelectedAuthoringReviewRequestID)
	runID = strings.TrimSpace(state.SelectedAuthoringReviewRunID)
	return requestID, runID, requestID != "" && runID != ""
}

func (state TaskHubState) selectedReleaseForTask(taskID string) (releaseID string, found bool) {
	if strings.TrimSpace(state.SelectedReleaseTaskID) != strings.TrimSpace(taskID) {
		return "", false
	}
	releaseID = strings.TrimSpace(state.SelectedReleaseID)
	return releaseID, releaseID != ""
}

// AggregateTaskHub derives deterministic Task rows without mutating or
// inspecting application storage. Runs whose TaskID is absent remain visible
// in the Runs tab but are not invented as synthetic Task records.
func AggregateTaskHub(snapshot TaskHubSnapshot) []TaskHubRow {
	tasks := make(map[string]TaskHubTask, len(snapshot.Tasks))
	for _, task := range snapshot.Tasks {
		if strings.TrimSpace(task.TaskID) == "" {
			continue
		}
		if prior, exists := tasks[task.TaskID]; !exists || task.UpdatedAt.After(prior.UpdatedAt) {
			tasks[task.TaskID] = task.Clone()
		}
	}
	rows := make(map[string]TaskHubRow, len(tasks))
	for id, task := range tasks {
		rows[id] = TaskHubRow{Task: task}
	}
	for _, run := range snapshot.Runs {
		row, found := rows[run.TaskID]
		if !found {
			continue
		}
		run = run.Clone()
		row.RunCount++
		if run.Active {
			row.ActiveRunCount++
		}
		if !row.HasLatestRun || taskHubRunTime(run).After(taskHubRunTime(row.LatestRun)) {
			row.LatestRun = run
			row.HasLatestRun = true
		}
		rows[run.TaskID] = row
	}
	output := make([]TaskHubRow, 0, len(rows))
	for _, row := range rows {
		output = append(output, row)
	}
	sort.SliceStable(output, func(left, right int) bool {
		if !output[left].Task.UpdatedAt.Equal(output[right].Task.UpdatedAt) {
			return output[left].Task.UpdatedAt.After(output[right].Task.UpdatedAt)
		}
		if output[left].Task.Name != output[right].Task.Name {
			return output[left].Task.Name < output[right].Task.Name
		}
		return output[left].Task.TaskID < output[right].Task.TaskID
	})
	return output
}

func taskHubRunTime(run TaskHubRun) time.Time {
	if !run.FinishedAt.IsZero() {
		return run.FinishedAt
	}
	return run.StartedAt
}

func sortTaskHubRuns(runs []TaskHubRun) []TaskHubRun {
	output := make([]TaskHubRun, len(runs))
	for index, run := range runs {
		output[index] = run.Clone()
	}
	sort.SliceStable(output, func(left, right int) bool {
		if !taskHubRunTime(output[left]).Equal(taskHubRunTime(output[right])) {
			return taskHubRunTime(output[left]).After(taskHubRunTime(output[right]))
		}
		return output[left].RunID < output[right].RunID
	})
	return output
}

type taskHubPrefixState struct {
	Prefix   rune
	Sequence uint64
	Expires  time.Time
}

type taskHubLoadedMsg struct {
	snapshot TaskHubSnapshot
	query    TaskHubQuery
	sequence uint64
	err      error
}

type taskHubPlanMsg struct {
	command TaskHubCommand
	preview TaskHubPlanPreview
	err     error
}

type taskHubMutationPreparedMsg struct {
	idempotencyKey string
	prepared       TaskHubPreparedMutation
	err            error
}

type taskHubMutationExecutedMsg struct {
	idempotencyKey string
	result         TaskHubMutationResult
	err            error
}

type taskHubRunControlMutationExecutedMsg struct {
	idempotencyKey string
	result         TaskHubRunControlMutationResult
	err            error
}

type taskHubRunControlPlanMsg struct {
	runID   string
	action  TaskHubRunControlAction
	preview TaskHubPlanPreview
	err     error
}

type taskHubPrefixTimeoutMsg struct{ sequence uint64 }

// initialTaskHubLoadV2 deliberately does not advance the stored request
// sequence: Bubble Tea invokes Init on a value copy. Any later request gets a
// greater sequence and makes this initial response stale before it can apply.
func (m model) initialTaskHubLoadV2() tea.Cmd {
	return taskHubLoadCommand(m.lifecycle, m.ctx, m.taskHub.Query, m.taskHubLoadSequence)
}

// loadTaskHubV2 captures both a monotonically increasing request generation
// and the exact query. Poll, search, and mutation refreshes may overlap; only
// the newest request for the current query may update the displayed snapshot.
func (m *model) loadTaskHubV2() tea.Cmd {
	m.taskHubLoadSequence++
	return taskHubLoadCommand(m.lifecycle, m.ctx, m.taskHub.Query, m.taskHubLoadSequence)
}

func taskHubLoadCommand(service TaskHubLifecycleService, ctx context.Context, query TaskHubQuery, sequence uint64) tea.Cmd {
	return func() tea.Msg {
		if service == nil {
			return taskHubLoadedMsg{query: query, sequence: sequence, err: fmt.Errorf("task lifecycle service is unavailable")}
		}
		snapshot, err := service.QueryTaskHub(ctx, query)
		return taskHubLoadedMsg{snapshot: snapshot, query: query, sequence: sequence, err: err}
	}
}

func sameTaskHubQuery(left, right TaskHubQuery) bool {
	return left.Tab == right.Tab && strings.TrimSpace(left.Filter) == strings.TrimSpace(right.Filter)
}

func (m *model) applyTaskHubSnapshot(snapshot TaskHubSnapshot) {
	previousTask := m.taskHub.SelectedTaskID
	previousRun := m.taskHub.SelectedRunID
	m.taskHub.Snapshot = snapshot.Clone()
	m.taskHub.Rows = AggregateTaskHub(snapshot)
	m.taskHub.LastRefresh = time.Now().UTC()
	m.taskHub.Loading = false

	if m.taskHubTaskByID(previousTask).TaskID != "" {
		m.taskHub.SelectedTaskID = previousTask
	} else if len(m.taskHub.Rows) > 0 {
		m.taskHub.SelectedTaskID = m.taskHub.Rows[0].Task.TaskID
	} else {
		m.taskHub.SelectedTaskID = ""
	}
	if m.taskHubRunByID(previousRun).RunID != "" {
		m.taskHub.SelectedRunID = previousRun
	} else if row, found := m.selectedTaskHubRow(); found && row.HasLatestRun {
		m.taskHub.SelectedRunID = row.LatestRun.RunID
	} else if len(m.taskHub.Snapshot.Runs) > 0 {
		m.taskHub.SelectedRunID = sortTaskHubRuns(m.taskHub.Snapshot.Runs)[0].RunID
	} else {
		m.taskHub.SelectedRunID = ""
	}
}

func (m *model) updateTaskHubV2Key(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "tab", "right":
		m.cycleTaskHubTab(1)
		m.taskHub.Loading = true
		return m.loadTaskHubV2()
	case "shift+tab", "left":
		m.cycleTaskHubTab(-1)
		m.taskHub.Loading = true
		return m.loadTaskHubV2()
	case "up", "k":
		m.moveTaskHubSelection(-1)
	case "down", "j":
		m.moveTaskHubSelection(1)
	case "/":
		m.hubSearching = true
		m.hubSearch.SetValue(m.taskHub.Query.Filter)
		m.hubSearch.Focus()
		m.focusMgr.Push(focusSearch)
		return m.hubSearch.Cursor.BlinkCmd()
	case "enter", "d":
		if m.taskHubPlan != nil {
			if !m.taskHubPlan.ConfirmationNeeded {
				return m.showToast("该计划仅供查看，当前不需要提交", toastWarning)
			}
			return m.openTaskHubPlanConfirmation()
		}
		return m.openTaskHubDetail()
	case "esc":
		if m.taskHubPlan != nil {
			m.taskHubPlan = nil
			m.taskHubPlanCommand = nil
			return m.showToast("已关闭生命周期计划预览", toastSuccess)
		}
	}
	return nil
}

func (m model) taskHubDetailQuery() (TaskHubDetailQuery, bool) {
	query := TaskHubDetailQuery{TaskID: strings.TrimSpace(m.taskHub.SelectedTaskID)}
	if m.taskHub.Query.Tab == TaskHubRunsTab || m.taskHub.Query.Tab == TaskHubQueueTab {
		run := m.taskHubRunByID(m.taskHub.SelectedRunID)
		if run.RunID != "" {
			query.RunID = run.RunID
			query.TaskID = run.TaskID
		}
	}
	return query, query.TaskID != "" || query.RunID != ""
}

func (m *model) openTaskHubDetail() tea.Cmd {
	reader, okay := m.lifecycle.(TaskHubDetailReader)
	if !okay || reader == nil {
		return m.showToast("当前生命周期服务未提供只读详情", toastWarning)
	}
	query, okay := m.taskHubDetailQuery()
	if !okay {
		return m.showToast("请先选择 Task 或 Run", toastWarning)
	}
	m.taskHubDetail = newTaskHubDetailOverlay(query)
	m.restoreTaskHubDetailSelection()
	m.focusMgr.Push(focusOverlay)
	return m.loadTaskHubDetail(reader, query)
}

func (m model) loadTaskHubDetail(reader TaskHubDetailReader, query TaskHubDetailQuery) tea.Cmd {
	return func() tea.Msg {
		detail, err := reader.QueryTaskHubDetail(m.ctx, query)
		return taskHubDetailLoadedMsg{query: query, detail: detail, err: err}
	}
}

func sameTaskHubDetailQuery(left, right TaskHubDetailQuery) bool {
	return strings.TrimSpace(left.TaskID) == strings.TrimSpace(right.TaskID) && strings.TrimSpace(left.RunID) == strings.TrimSpace(right.RunID)
}

func (m *model) updateTaskHubDetailKey(msg tea.KeyMsg) tea.Cmd {
	if m.taskHubDetail == nil {
		return nil
	}
	switch msg.String() {
	case "esc", "q":
		m.closeTaskHubDetail()
		return nil
	case "tab", "right":
		m.taskHubDetail.cycleTab(1)
	case "shift+tab", "left":
		m.taskHubDetail.cycleTab(-1)
	case "up", "k":
		m.taskHubDetail.scroll(-1, m.taskHubDetailHeight())
	case "down", "j":
		m.taskHubDetail.scroll(1, m.taskHubDetailHeight())
	case "pgup":
		m.taskHubDetail.scroll(-maxInt(1, m.taskHubDetailHeight()/2), m.taskHubDetailHeight())
	case "pgdown":
		m.taskHubDetail.scroll(maxInt(1, m.taskHubDetailHeight()/2), m.taskHubDetailHeight())
	case "home":
		m.taskHubDetail.Scroll = 0
	case "end":
		m.taskHubDetail.scroll(len(m.taskHubDetail.contentRows()), m.taskHubDetailHeight())
	case "[", "]":
		delta := 1
		if msg.String() == "[" {
			delta = -1
		}
		switch m.taskHubDetail.Tab {
		case TaskHubDetailFactsTab:
			review, authoringReview, authoring, selected := m.taskHubDetail.cycleOpenReviewTarget(delta)
			if !selected {
				return m.showToast("当前详情没有可选择的打开审核请求", toastWarning)
			}
			if authoring {
				m.selectTaskHubDetailAuthoringReview(authoringReview)
				return m.showToast("已选择 source/session 审核 "+shortTaskHubID(authoringReview.ReviewRequestID), toastSuccess)
			}
			m.selectTaskHubDetailReview(review)
			return m.showToast("已选择 ReviewRequest "+shortTaskHubID(review.ReviewRequestID), toastSuccess)
		case TaskHubDetailReleasesTab:
			release, selected := m.taskHubDetail.cycleActiveRelease(delta)
			if !selected {
				return m.showToast("当前详情没有可选择的未撤回本地 package", toastWarning)
			}
			m.selectTaskHubDetailRelease(release)
			return m.showToast("已选择本地 package "+clipDisplay(release.ReleaseVersion, 24), toastSuccess)
		default:
			return m.showToast("请在“证据/审核/返修”或“本地包”分类中选择目标", toastWarning)
		}
	case "r":
		reader, okay := m.lifecycle.(TaskHubDetailReader)
		if !okay || reader == nil {
			return m.showToast("当前生命周期服务未提供只读详情", toastWarning)
		}
		m.taskHubDetail.Loading = true
		m.taskHubDetail.Error = ""
		m.taskHubDetail.Scroll = 0
		return m.loadTaskHubDetail(reader, m.taskHubDetail.Query)
	}
	return nil
}

func (m model) taskHubDetailHeight() int {
	return maxInt(8, m.height-3)
}

func (m *model) closeTaskHubDetail() {
	if m.taskHubDetail == nil {
		return
	}
	m.taskHubDetail = nil
	m.focusMgr.Pop()
}

func (m *model) restoreTaskHubDetailSelection() {
	if m == nil || m.taskHubDetail == nil {
		return
	}
	taskID := strings.TrimSpace(m.taskHubDetail.Query.TaskID)
	if requestID, _, found := m.taskHub.selectedReviewForTask(taskID); found {
		m.taskHubDetail.SelectedReviewRequestID = requestID
	}
	if requestID, _, found := m.taskHub.selectedAuthoringReviewForTask(taskID); found {
		m.taskHubDetail.SelectedAuthoringReviewRequestID = requestID
	}
	if releaseID, found := m.taskHub.selectedReleaseForTask(taskID); found {
		m.taskHubDetail.SelectedReleaseID = releaseID
	}
}

func (m *model) selectTaskHubDetailReview(review TaskHubReviewFact) {
	if m == nil || m.taskHubDetail == nil {
		return
	}
	taskID := strings.TrimSpace(m.taskHubDetail.Detail.Task.TaskID)
	if taskID == "" {
		taskID = strings.TrimSpace(m.taskHubDetail.Query.TaskID)
	}
	m.taskHub.SelectedReviewTaskID = taskID
	m.taskHub.SelectedReviewRequestID = strings.TrimSpace(review.ReviewRequestID)
	m.taskHub.SelectedReviewRevisionID = strings.TrimSpace(review.RevisionID)
	m.taskHub.SelectedAuthoringReviewTaskID = ""
	m.taskHub.SelectedAuthoringReviewRequestID = ""
	m.taskHub.SelectedAuthoringReviewRunID = ""
	m.taskHubDetail.SelectedAuthoringReviewRequestID = ""
}

func (m *model) selectTaskHubDetailAuthoringReview(review TaskHubAuthoringReviewFact) {
	if m == nil || m.taskHubDetail == nil {
		return
	}
	taskID := strings.TrimSpace(m.taskHubDetail.Detail.Task.TaskID)
	if taskID == "" {
		taskID = strings.TrimSpace(m.taskHubDetail.Query.TaskID)
	}
	m.taskHub.SelectedAuthoringReviewTaskID = taskID
	m.taskHub.SelectedAuthoringReviewRequestID = strings.TrimSpace(review.ReviewRequestID)
	m.taskHub.SelectedAuthoringReviewRunID = strings.TrimSpace(review.RunID)
	m.taskHub.SelectedReviewTaskID = ""
	m.taskHub.SelectedReviewRequestID = ""
	m.taskHub.SelectedReviewRevisionID = ""
	m.taskHubDetail.SelectedReviewRequestID = ""
}

func (m *model) selectTaskHubDetailRelease(release TaskHubReleaseFact) {
	if m == nil || m.taskHubDetail == nil {
		return
	}
	taskID := strings.TrimSpace(m.taskHubDetail.Detail.Task.TaskID)
	if taskID == "" {
		taskID = strings.TrimSpace(m.taskHubDetail.Query.TaskID)
	}
	m.taskHub.SelectedReleaseTaskID = taskID
	m.taskHub.SelectedReleaseID = strings.TrimSpace(release.ReleaseID)
}

func (m *model) syncTaskHubDetailSelections() {
	if m == nil || m.taskHubDetail == nil {
		return
	}
	m.taskHubDetail.normalizeSelections()
	taskID := strings.TrimSpace(m.taskHubDetail.Detail.Task.TaskID)
	if taskID == "" {
		taskID = strings.TrimSpace(m.taskHubDetail.Query.TaskID)
	}
	if review, found := m.taskHubDetail.selectedOpenReview(); found {
		m.taskHub.SelectedReviewTaskID = taskID
		m.taskHub.SelectedReviewRequestID = review.ReviewRequestID
		m.taskHub.SelectedReviewRevisionID = review.RevisionID
	} else if m.taskHub.SelectedReviewTaskID == taskID {
		m.taskHub.SelectedReviewRequestID = ""
		m.taskHub.SelectedReviewRevisionID = ""
	}
	if review, found := m.taskHubDetail.selectedOpenAuthoringReview(); found {
		m.taskHub.SelectedAuthoringReviewTaskID = taskID
		m.taskHub.SelectedAuthoringReviewRequestID = review.ReviewRequestID
		m.taskHub.SelectedAuthoringReviewRunID = review.RunID
	} else if m.taskHub.SelectedAuthoringReviewTaskID == taskID {
		m.taskHub.SelectedAuthoringReviewRequestID = ""
		m.taskHub.SelectedAuthoringReviewRunID = ""
	}
	if release, found := m.taskHubDetail.selectedActiveRelease(); found {
		m.taskHub.SelectedReleaseTaskID = taskID
		m.taskHub.SelectedReleaseID = release.ReleaseID
	} else if m.taskHub.SelectedReleaseTaskID == taskID {
		m.taskHub.SelectedReleaseID = ""
	}
}

func (m *model) updateTaskHubV2Search(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		m.hubSearching = false
		m.hubSearch.Blur()
		m.focusMgr.Pop()
		return nil
	case "enter":
		m.hubSearching = false
		m.hubSearch.Blur()
		m.focusMgr.Pop()
		m.taskHub.Query.Filter = strings.TrimSpace(m.hubSearch.Value())
		m.taskHub.Loading = true
		return m.loadTaskHubV2()
	}
	var command tea.Cmd
	m.hubSearch, command = m.hubSearch.Update(msg)
	query := strings.TrimSpace(m.hubSearch.Value())
	return tea.Batch(command, tea.Tick(150*time.Millisecond, func(time.Time) tea.Msg {
		return taskHubSearchMsg{query: query}
	}))
}

type taskHubSearchMsg struct{ query string }

func (m *model) cycleTaskHubTab(delta int) {
	tabs := []TaskHubTab{TaskHubTasksTab, TaskHubRunsTab, TaskHubQueueTab}
	current := 0
	for index, tab := range tabs {
		if m.taskHub.Query.Tab == tab {
			current = index
			break
		}
	}
	m.taskHub.Query.Tab = tabs[(current+delta+len(tabs))%len(tabs)]
	m.normalizeTaskHubSelection()
}

func (m *model) selectTaskHubTab(tab TaskHubTab) tea.Cmd {
	if !tab.valid() {
		return nil
	}
	if m.taskHub.Query.Tab == tab {
		return nil
	}
	m.taskHub.Query.Tab = tab
	m.normalizeTaskHubSelection()
	m.taskHub.Loading = true
	return m.loadTaskHubV2()
}

func (m *model) moveTaskHubSelection(delta int) {
	switch m.taskHub.Query.Tab {
	case TaskHubRunsTab:
		runs := sortTaskHubRuns(m.taskHub.Snapshot.Runs)
		if len(runs) == 0 {
			return
		}
		index := 0
		for current, run := range runs {
			if run.RunID == m.taskHub.SelectedRunID {
				index = current
				break
			}
		}
		index = (index + delta + len(runs)) % len(runs)
		m.taskHub.SelectedRunID = runs[index].RunID
		m.taskHub.SelectedTaskID = runs[index].TaskID
	case TaskHubTasksTab:
		if len(m.taskHub.Rows) == 0 {
			return
		}
		index := 0
		for current, row := range m.taskHub.Rows {
			if row.Task.TaskID == m.taskHub.SelectedTaskID {
				index = current
				break
			}
		}
		index = (index + delta + len(m.taskHub.Rows)) % len(m.taskHub.Rows)
		m.taskHub.SelectedTaskID = m.taskHub.Rows[index].Task.TaskID
		if m.taskHub.Rows[index].HasLatestRun {
			m.taskHub.SelectedRunID = m.taskHub.Rows[index].LatestRun.RunID
		}
	}
}

func (m *model) normalizeTaskHubSelection() {
	if m.taskHubTaskByID(m.taskHub.SelectedTaskID).TaskID == "" && len(m.taskHub.Rows) > 0 {
		m.taskHub.SelectedTaskID = m.taskHub.Rows[0].Task.TaskID
	}
	if m.taskHubRunByID(m.taskHub.SelectedRunID).RunID == "" {
		if row, found := m.selectedTaskHubRow(); found && row.HasLatestRun {
			m.taskHub.SelectedRunID = row.LatestRun.RunID
		}
	}
}

func (m model) taskHubTaskByID(id string) TaskHubTask {
	for _, task := range m.taskHub.Snapshot.Tasks {
		if task.TaskID == id {
			return task.Clone()
		}
	}
	return TaskHubTask{}
}

func (m model) taskHubRunByID(id string) TaskHubRun {
	for _, run := range m.taskHub.Snapshot.Runs {
		if run.RunID == id {
			return run.Clone()
		}
	}
	return TaskHubRun{}
}

func (m model) selectedTaskHubRow() (TaskHubRow, bool) {
	for _, row := range m.taskHub.Rows {
		if row.Task.TaskID == m.taskHub.SelectedTaskID {
			return row, true
		}
	}
	return TaskHubRow{}, false
}

func (m *model) handleTaskHubPrefixKey(msg tea.KeyMsg) (bool, tea.Cmd) {
	if m.lifecycle == nil || m.view != viewHub || m.taskHubInputFocused() {
		return false, nil
	}
	key := msg.String()
	if m.taskHubPrefix.Prefix != 0 {
		if key == "esc" {
			m.clearTaskHubPrefix()
			return true, nil
		}
		second, okay := taskHubRuneKey(msg)
		prefix := m.taskHubPrefix.Prefix
		m.clearTaskHubPrefix()
		if !okay {
			return true, m.showToast("生命周期快捷键已取消", toastWarning)
		}
		action, found := taskHubActionForSequence(prefix, second)
		if !found {
			return true, m.showToast("该前缀不支持此第二键", toastWarning)
		}
		return true, m.previewTaskHubAction(action)
	}
	prefix, okay := taskHubRuneKey(msg)
	if !okay || !taskHubPrefixKnown(prefix) {
		return false, nil
	}
	m.taskHubPrefix.Sequence++
	m.taskHubPrefix.Prefix = prefix
	m.taskHubPrefix.Expires = time.Now().Add(taskHubPrefixTimeout)
	sequence := m.taskHubPrefix.Sequence
	return true, tea.Tick(taskHubPrefixTimeout, func(time.Time) tea.Msg {
		return taskHubPrefixTimeoutMsg{sequence: sequence}
	})
}

func (m model) taskHubInputFocused() bool {
	if m.hubSearching {
		return true
	}
	switch m.focusMgr.Current() {
	case focusSearch:
		return true
	default:
		return false
	}
}

func taskHubRuneKey(msg tea.KeyMsg) (rune, bool) {
	if msg.Type != tea.KeyRunes || len(msg.Runes) != 1 {
		return 0, false
	}
	return unicode.ToLower(msg.Runes[0]), true
}

func (m *model) clearTaskHubPrefix() {
	m.taskHubPrefix.Prefix = 0
	m.taskHubPrefix.Expires = time.Time{}
}

func taskHubPrefixKnown(prefix rune) bool {
	return prefix == 't' || prefix == 'x' || prefix == 'v' || prefix == 'p'
}

func taskHubActionForSequence(prefix, second rune) (TaskHubAction, bool) {
	switch prefix {
	case 't':
		switch second {
		case 'n':
			return TaskHubActionNewTask, true
		case 'i':
			return TaskHubActionImportTask, true
		case 's':
			return TaskHubActionStartStandardAuthoring, true
		case 'e':
			return TaskHubActionEditTask, true
		case 'f':
			return TaskHubActionForkTask, true
		case 'a':
			return TaskHubActionArchiveTask, true
		case 'd':
			return TaskHubActionSoftDeleteTask, true
		case 'u':
			return TaskHubActionRestoreTask, true
		}
	case 'x':
		switch second {
		case 'c':
			return TaskHubActionContinue, true
		case 'n':
			return TaskHubActionStartRun, true
		case 'e':
			return TaskHubActionEvaluateCodeEdge, true
		case 'h':
			return TaskHubActionAdoptCodeEdgeEvaluatorEvidenceHandoff, true
		case 'a':
			return TaskHubActionAttachRun, true
		case 'k':
			return TaskHubActionOpenRunControl, true
		}
	case 'v':
		switch second {
		case 'a':
			return TaskHubActionApproveReview, true
		case 'c':
			return TaskHubActionRequestChanges, true
		case 'r':
			return TaskHubActionRejectReview, true
		}
	case 'p':
		switch second {
		case 'p':
			return TaskHubActionPackageRevision, true
		case 'w':
			return TaskHubActionWithdrawRelease, true
		}
	}
	return "", false
}

func taskHubPrefixActions(prefix rune) []TaskHubAction {
	switch prefix {
	case 't':
		return []TaskHubAction{TaskHubActionNewTask, TaskHubActionImportTask, TaskHubActionStartStandardAuthoring, TaskHubActionEditTask, TaskHubActionForkTask, TaskHubActionArchiveTask, TaskHubActionSoftDeleteTask, TaskHubActionRestoreTask}
	case 'x':
		return []TaskHubAction{TaskHubActionContinue, TaskHubActionStartRun, TaskHubActionEvaluateCodeEdge, TaskHubActionAdoptCodeEdgeEvaluatorEvidenceHandoff, TaskHubActionAttachRun, TaskHubActionOpenRunControl}
	case 'v':
		return []TaskHubAction{TaskHubActionApproveReview, TaskHubActionRequestChanges, TaskHubActionRejectReview}
	case 'p':
		return []TaskHubAction{TaskHubActionPackageRevision, TaskHubActionWithdrawRelease}
	default:
		return nil
	}
}

func (m model) taskHubActionState(action TaskHubAction) TaskHubActionState {
	find := func(states []TaskHubActionState) (TaskHubActionState, bool) {
		for _, state := range states {
			if state.Action == action {
				return state, true
			}
		}
		return TaskHubActionState{}, false
	}
	state := TaskHubActionState{Action: action, DisabledReason: "当前选择未声明该操作可用"}
	stateFound := false
	if foundState, found := find(m.taskHub.Snapshot.GlobalActions); found {
		state, stateFound = foundState, true
	}
	if !stateFound {
		if run := m.taskHubRunByID(m.taskHub.SelectedRunID); run.RunID != "" {
			if foundState, found := find(run.Actions); found {
				state, stateFound = foundState, true
			}
		}
	}
	if !stateFound {
		if task := m.taskHubTaskByID(m.taskHub.SelectedTaskID); task.TaskID != "" {
			if foundState, found := find(task.Actions); found {
				state = foundState
			}
		}
	}
	if !state.Enabled && action == TaskHubActionEditTask {
		task := m.taskHubTaskByID(m.taskHub.SelectedTaskID)
		run := m.taskHubRunByID(m.taskHub.SelectedRunID)
		if task.TaskID != "" && run.RunID != "" && run.TaskID == task.TaskID &&
			task.RevisionID != "" && run.RevisionID == task.RevisionID &&
			!strings.Contains(state.DisabledReason, "LifecycleMutationService") {
			return TaskHubActionState{Action: action, Enabled: true}
		}
	}
	if !state.Enabled && action == TaskHubActionWithdrawRelease {
		if releaseID, selected := m.taskHub.selectedReleaseForTask(m.taskHub.SelectedTaskID); selected && releaseID != "" &&
			!strings.Contains(state.DisabledReason, "LifecycleMutationService") {
			return TaskHubActionState{Action: action, Enabled: true}
		}
	}
	if !state.Enabled && taskHubReviewAction(action) && !strings.Contains(state.DisabledReason, "LifecycleMutationService") {
		if _, _, selected := m.taskHub.selectedReviewForTask(m.taskHub.SelectedTaskID); selected {
			// Detail selection captures a stable request ID. The adapter still
			// validates its Task/revision ownership and current open state.
			return TaskHubActionState{Action: action, Enabled: true}
		}
	}
	return state
}

func taskHubReviewAction(action TaskHubAction) bool {
	switch action {
	case TaskHubActionApproveReview, TaskHubActionRequestChanges, TaskHubActionRejectReview:
		return true
	default:
		return false
	}
}

func (m *model) previewTaskHubAction(action TaskHubAction) tea.Cmd {
	state := m.taskHubActionState(action)
	if !state.Enabled {
		reason := strings.TrimSpace(state.DisabledReason)
		if reason == "" {
			reason = "当前操作不可用"
		}
		m.notice = taskHubActionLabel(action) + "不可用：" + reason
		return m.showToast(m.notice, toastWarning)
	}
	if action == TaskHubActionOpenRunControl {
		m.openRunControl()
		return nil
	}
	if m.lifecycle == nil {
		return m.showToast("生命周期服务不可用", toastError)
	}
	target := TaskHubTarget{}
	if !taskHubGlobalAction(action) {
		target = m.taskHubTarget()
	}
	command := TaskHubCommand{Action: action, Target: target}
	service := m.lifecycle
	return func() tea.Msg {
		preview, err := service.PlanTaskHubCommand(m.ctx, command)
		return taskHubPlanMsg{command: command, preview: preview, err: err}
	}
}

func taskHubGlobalAction(action TaskHubAction) bool {
	switch action {
	case TaskHubActionNewTask, TaskHubActionImportTask, TaskHubActionStartStandardAuthoring:
		return true
	default:
		return false
	}
}

// openTaskHubPlanConfirmation transfers a read-only plan preview into the
// native mutation form. Opening the form allocates an idempotency key but does
// not call a planner, worker, provider, filesystem, or store mutation.
func (m *model) openTaskHubPlanConfirmation() tea.Cmd {
	if m.taskHubPlan == nil || m.taskHubPlanCommand == nil {
		return m.showToast("计划缺少可确认的动作目标", toastError)
	}
	overlay, err := newTaskHubMutationOverlay(m.taskHubPlanCommand.Action, m.taskHubPlanCommand.Target, *m.taskHubPlan)
	if err != nil {
		return m.showToast("无法打开生命周期确认："+err.Error(), toastError)
	}
	m.taskHubMutation = overlay
	if m.router != nil {
		m.router.PushOverlay(overlay)
	}
	m.focusMgr.Push(focusOverlay)
	return nil
}

// openTaskHubRunControlConfirmation is invoked only after the operator chose
// a control action and inspected its impact preview. The captured checkpoint is
// copied into the form so a later submit cannot be rebound to newer work.
func (m *model) openTaskHubRunControlConfirmation() tea.Cmd {
	if m.runControl == nil || m.runControl.Preview == nil || m.runControl.SelectedAction == "" {
		return m.showToast("请先选择并查看运行控制影响", toastWarning)
	}
	overlay, err := newTaskHubRunControlMutationOverlay(
		m.runControl.SelectedAction,
		m.taskHubRunControlTarget(),
		m.runControl.Expected,
		*m.runControl.Preview,
	)
	if err != nil {
		return m.showToast("无法打开运行控制确认："+err.Error(), toastError)
	}
	m.taskHubMutation = overlay
	if m.router != nil {
		m.router.PushOverlay(overlay)
	}
	m.focusMgr.Push(focusOverlay)
	return nil
}

func (m *model) updateTaskHubMutationKey(msg tea.KeyMsg) tea.Cmd {
	overlay := m.taskHubMutation
	if overlay == nil {
		return nil
	}
	if overlay.Phase != taskHubMutationReady {
		return nil
	}
	if overlay.isFrozen() {
		switch msg.String() {
		case "esc", "q":
			m.closeTaskHubMutation()
			return m.showToast("已取消生命周期确认，未提交任何操作", toastSuccess)
		case "enter":
			if err := overlay.validate(); err != nil {
				overlay.Error = err.Error()
				return nil
			}
			return m.executeTaskHubMutation()
		default:
			// Frozen plans retain the original actor/reason and accept no
			// editable field input before their explicit final confirmation.
			return nil
		}
	}
	switch msg.String() {
	case "esc", "q":
		m.closeTaskHubMutation()
		return m.showToast("已取消生命周期确认，未提交任何操作", toastSuccess)
	case "tab", "down":
		overlay.focusInput(overlay.FocusedField + 1)
		return nil
	case "shift+tab", "up":
		overlay.focusInput(overlay.FocusedField - 1)
		return nil
	case "enter", "ctrl+s":
		if msg.String() == "enter" && overlay.focusedInputIsMultiline() {
			return overlay.updateFocusedInput(msg)
		}
		if err := overlay.validate(); err != nil {
			overlay.Error = err.Error()
			return nil
		}
		if overlay.isRunControl() {
			return m.executeTaskHubRunControlMutation()
		}
		if taskHubMutationRequiresPreparation(overlay.Action) && strings.TrimSpace(overlay.Preview.PlanID) == "" {
			return m.prepareTaskHubMutation()
		}
		return m.executeTaskHubMutation()
	default:
		return overlay.updateFocusedInput(msg)
	}
}

func taskHubMutationRequiresPreparation(action TaskHubAction) bool {
	switch action {
	case TaskHubActionContinue, TaskHubActionEditTask, TaskHubActionStartRun, TaskHubActionEvaluateCodeEdge, TaskHubActionAdoptCodeEdgeEvaluatorEvidenceHandoff:
		return true
	default:
		return false
	}
}

func (m *model) prepareTaskHubMutation() tea.Cmd {
	overlay := m.taskHubMutation
	if overlay == nil {
		return nil
	}
	planner, supported := m.lifecycle.(TaskHubMutationPlanner)
	if !supported || planner == nil {
		overlay.Error = "当前生命周期服务未提供冻结计划确认接口"
		return nil
	}
	request := overlay.request().Clone()
	overlay.Phase = taskHubMutationPreparing
	overlay.Error = ""
	return func() tea.Msg {
		prepared, err := planner.PrepareTaskHubMutation(m.ctx, request)
		return taskHubMutationPreparedMsg{idempotencyKey: request.IdempotencyKey, prepared: prepared, err: err}
	}
}

func (m *model) executeTaskHubMutation() tea.Cmd {
	overlay := m.taskHubMutation
	if overlay == nil {
		return nil
	}
	executor, supported := m.lifecycle.(TaskHubMutationExecutor)
	if !supported || executor == nil {
		overlay.Error = "当前生命周期服务未提供确认提交接口"
		return nil
	}
	request := overlay.request().Clone()
	overlay.Phase = taskHubMutationExecuting
	overlay.Error = ""
	return func() tea.Msg {
		result, err := executor.ExecuteTaskHubMutation(m.ctx, request)
		return taskHubMutationExecutedMsg{idempotencyKey: request.IdempotencyKey, result: result, err: err}
	}
}

func (m *model) executeTaskHubRunControlMutation() tea.Cmd {
	overlay := m.taskHubMutation
	if overlay == nil {
		return nil
	}
	executor, supported := m.lifecycle.(TaskHubRunControlMutationExecutor)
	if !supported || executor == nil {
		overlay.Error = "当前生命周期服务未提供运行控制确认接口"
		return nil
	}
	request := overlay.request()
	command := TaskHubRunControlMutationRequest{
		Action:         overlay.ControlAction,
		Target:         overlay.Target,
		Expected:       overlay.Expected,
		Actor:          request.Actor,
		Reason:         request.Reason,
		IdempotencyKey: request.IdempotencyKey,
	}
	overlay.Phase = taskHubMutationExecuting
	overlay.Error = ""
	return func() tea.Msg {
		result, err := executor.ExecuteTaskHubRunControlMutation(m.ctx, command)
		return taskHubRunControlMutationExecutedMsg{idempotencyKey: command.IdempotencyKey, result: result, err: err}
	}
}

func (m *model) closeTaskHubMutation() {
	if m.taskHubMutation == nil {
		return
	}
	m.taskHubMutation = nil
	if m.router != nil {
		m.router.PopOverlay()
	}
	m.focusMgr.Pop()
}

// previewTaskHubRunControl asks the optional application-service capability
// for an impact preview. It never invokes an execution control mutation, even
// when a preview describes what a future confirmed command would affect.
func (m *model) previewTaskHubRunControl(action TaskHubRunControlAction) tea.Cmd {
	if m.runControl == nil {
		return nil
	}
	runID := strings.TrimSpace(m.runControl.RunID)
	if !taskHubRunControlActionKnown(action) {
		return m.showToast("当前运行控制项不可用", toastWarning)
	}
	command := TaskHubRunControlCommand{Action: action, Target: m.taskHubRunControlTarget()}
	planner, supported := m.lifecycle.(TaskHubRunControlPlanner)
	if !supported {
		preview := TaskHubPlanPreview{
			Title:   taskHubRunControlActionLabel(action) + "（只读预览）",
			Summary: "生命周期服务未声明运行控制预览能力；不会创建 ControlOperation。",
			Reason:  "当前服务仅支持 Task Hub 的基础查询和计划。",
		}
		return func() tea.Msg {
			return taskHubRunControlPlanMsg{runID: runID, action: action, preview: preview}
		}
	}
	return func() tea.Msg {
		preview, err := planner.PlanTaskHubRunControl(m.ctx, command)
		return taskHubRunControlPlanMsg{runID: runID, action: action, preview: preview, err: err}
	}
}

func (m model) taskHubTarget() TaskHubTarget {
	target := TaskHubTarget{TaskID: m.taskHub.SelectedTaskID, RunID: m.taskHub.SelectedRunID}
	if run := m.taskHubRunByID(target.RunID); run.RunID != "" {
		if target.TaskID == "" {
			target.TaskID = run.TaskID
		}
		target.RevisionID = run.RevisionID
		target.StageAttemptID = run.Control.StageAttemptID
		if task := m.taskHubTaskByID(target.TaskID); task.TaskID != "" {
			target.ReviewRequestID = task.ActiveReviewID
			target.ReviewRevisionID = task.ActiveReviewRevisionID
		}
		return m.applyTaskHubDetailTargetSelection(target)
	}
	if task := m.taskHubTaskByID(target.TaskID); task.TaskID != "" {
		target.RevisionID = task.RevisionID
		target.ReviewRequestID = task.ActiveReviewID
		target.ReviewRevisionID = task.ActiveReviewRevisionID
	}
	return m.applyTaskHubDetailTargetSelection(target)
}

func (m model) applyTaskHubDetailTargetSelection(target TaskHubTarget) TaskHubTarget {
	if requestID, runID, found := m.taskHub.selectedAuthoringReviewForTask(target.TaskID); found {
		target.AuthoringReviewRequestID = requestID
		target.RunID = runID
		target.RevisionID = ""
		target.ReviewRequestID = ""
		target.ReviewRevisionID = ""
		return target
	}
	if task := m.taskHubTaskByID(target.TaskID); task.TaskID != "" && strings.TrimSpace(task.ActiveAuthoringReviewID) != "" &&
		strings.TrimSpace(task.ActiveAuthoringReviewRunID) != "" {
		target.AuthoringReviewRequestID = strings.TrimSpace(task.ActiveAuthoringReviewID)
		target.RunID = strings.TrimSpace(task.ActiveAuthoringReviewRunID)
		target.RevisionID = ""
		target.ReviewRequestID = ""
		target.ReviewRevisionID = ""
		return target
	}
	if requestID, revisionID, found := m.taskHub.selectedReviewForTask(target.TaskID); found {
		target.ReviewRequestID = requestID
		target.ReviewRevisionID = revisionID
	}
	if releaseID, found := m.taskHub.selectedReleaseForTask(target.TaskID); found {
		target.ReleaseID = releaseID
	}
	return target
}

func (m model) taskHubRunControlTarget() TaskHubTarget {
	if m.runControl == nil {
		return m.taskHubTarget()
	}
	// The overlay captures stable identities when it opens. Do not rebind an
	// in-flight preview to a different row if a polling refresh changes hub
	// selection while the operator is reading control facts.
	return TaskHubTarget{
		TaskID:         strings.TrimSpace(m.runControl.TaskID),
		RunID:          strings.TrimSpace(m.runControl.RunID),
		RevisionID:     strings.TrimSpace(m.runControl.RevisionID),
		StageAttemptID: strings.TrimSpace(m.runControl.StageAttemptID),
	}
}

func (m model) taskHubPrefixHint() string {
	if m.taskHubPrefix.Prefix == 0 {
		return "[t 任务] [x 执行] [v 审核] [p 本地 package]"
	}
	parts := make([]string, 0)
	for _, action := range taskHubPrefixActions(m.taskHubPrefix.Prefix) {
		state := m.taskHubActionState(action)
		label := taskHubActionKey(action) + " " + taskHubActionLabel(action)
		if !state.Enabled {
			reason := strings.TrimSpace(state.DisabledReason)
			if reason == "" {
				reason = "不可用"
			}
			label += "（" + reason + "）"
		}
		parts = append(parts, label)
	}
	return string(m.taskHubPrefix.Prefix) + " · " + strings.Join(parts, "  ") + "  [Esc 取消]"
}

func taskHubActionKey(action TaskHubAction) string {
	for _, pair := range []struct {
		prefix rune
		second rune
		action TaskHubAction
	}{
		{'t', 'n', TaskHubActionNewTask}, {'t', 'i', TaskHubActionImportTask}, {'t', 's', TaskHubActionStartStandardAuthoring}, {'t', 'e', TaskHubActionEditTask}, {'t', 'f', TaskHubActionForkTask}, {'t', 'a', TaskHubActionArchiveTask}, {'t', 'd', TaskHubActionSoftDeleteTask}, {'t', 'u', TaskHubActionRestoreTask},
		{'x', 'c', TaskHubActionContinue}, {'x', 'n', TaskHubActionStartRun}, {'x', 'e', TaskHubActionEvaluateCodeEdge}, {'x', 'h', TaskHubActionAdoptCodeEdgeEvaluatorEvidenceHandoff}, {'x', 'a', TaskHubActionAttachRun}, {'x', 'k', TaskHubActionOpenRunControl},
		{'v', 'a', TaskHubActionApproveReview}, {'v', 'c', TaskHubActionRequestChanges}, {'v', 'r', TaskHubActionRejectReview},
		{'p', 'p', TaskHubActionPackageRevision}, {'p', 'w', TaskHubActionWithdrawRelease},
	} {
		if pair.action == action {
			return string(pair.prefix) + " " + string(pair.second)
		}
	}
	return "?"
}

func taskHubActionLabel(action TaskHubAction) string {
	switch action {
	case TaskHubActionNewTask:
		return "新建 Task"
	case TaskHubActionImportTask:
		return "导入 Task"
	case TaskHubActionStartStandardAuthoring:
		return "启动 Standard 创题"
	case TaskHubActionEditTask:
		return "创建 draft 修改"
	case TaskHubActionForkTask:
		return "Fork Task"
	case TaskHubActionArchiveTask:
		return "归档 Task"
	case TaskHubActionSoftDeleteTask:
		return "软删除 Task"
	case TaskHubActionRestoreTask:
		return "恢复 Task"
	case TaskHubActionContinue:
		return "继续处理"
	case TaskHubActionStartRun:
		return "启动新 Run"
	case TaskHubActionEvaluateCodeEdge:
		return "执行 CodeEdge 评测"
	case TaskHubActionAdoptCodeEdgeEvaluatorEvidenceHandoff:
		return "采用 CodeEdge 评测证据"
	case TaskHubActionAttachRun:
		return "Attach Run"
	case TaskHubActionOpenRunControl:
		return "打开运行控制"
	case TaskHubActionApproveReview:
		return "审核通过"
	case TaskHubActionRequestChanges:
		return "要求修改"
	case TaskHubActionRejectReview:
		return "终止性拒绝"
	case TaskHubActionPackageRevision:
		return "生成本地 package"
	case TaskHubActionWithdrawRelease:
		return "撤回 release"
	default:
		return string(action)
	}
}

func taskHubRunControlActionKnown(action TaskHubRunControlAction) bool {
	switch action {
	case TaskHubRunControlPause, TaskHubRunControlCancelStage, TaskHubRunControlTerminate, TaskHubRunControlReconcile:
		return true
	default:
		return false
	}
}

func taskHubRunControlActionLabel(action TaskHubRunControlAction) string {
	switch action {
	case TaskHubRunControlPause:
		return "暂停运行"
	case TaskHubRunControlCancelStage:
		return "取消选中阶段"
	case TaskHubRunControlTerminate:
		return "终止本次运行"
	case TaskHubRunControlReconcile:
		return "本地 reconcile"
	default:
		return string(action)
	}
}

func (m model) taskHubV2View() string {
	layout := layoutFor(m.width, m.height)
	width := maxInt(1, layout.ContentWidth)
	lines := []string{sectionStyle.Render("Task Hub")}
	tabs := []string{"Tasks", "Runs", "Queue"}
	for index, tab := range []TaskHubTab{TaskHubTasksTab, TaskHubRunsTab, TaskHubQueueTab} {
		if tab == m.taskHub.Query.Tab {
			tabs[index] = selectedStyle.Render(" " + tabs[index] + " ")
		}
	}
	lines = append(lines, strings.Join(tabs, "  "))
	if filter := strings.TrimSpace(m.taskHub.Query.Filter); filter != "" {
		lines = append(lines, subtleStyle.Render("筛选: "+clipDisplay(filter, maxInt(8, width-10))))
	}
	if m.taskHub.Loading {
		lines = append(lines, subtleStyle.Render("正在刷新生命周期视图..."))
	}
	if m.err != nil {
		lines = append(lines, failStyle.Render(clipDisplay(redactSingleLineUI(m.err.Error()), width)))
	}
	switch m.taskHub.Query.Tab {
	case TaskHubRunsTab:
		lines = append(lines, m.taskHubRunsLines(width)...)
	case TaskHubQueueTab:
		lines = append(lines, m.taskHubQueueLines(width)...)
	default:
		lines = append(lines, m.taskHubTasksLines(width)...)
	}
	if m.taskHubPlan != nil {
		preview := m.taskHubPlan
		lines = append(lines, "", sectionStyle.Render("计划预览"))
		lines = append(lines, clipDisplay(preview.Title, width), clipDisplay(preview.Summary, width))
		lines = append(lines, taskHubPlanExplanationRows(*preview, width)...)
		lines = append(lines, subtleStyle.Render("[Esc] 关闭预览"))
	}
	if hint := m.taskHubPrefixHint(); hint != "" {
		lines = append(lines, "", subtleStyle.Render(clipDisplay(hint, width)))
	}
	clipped := make([]string, len(lines))
	for index, line := range lines {
		clipped[index] = clipDisplay(line, width)
	}
	return panelStyle.Width(width).Render(strings.Join(clipped, "\n"))
}

func (m model) taskHubTasksLines(width int) []string {
	if len(m.taskHub.Rows) == 0 {
		return []string{subtleStyle.Render("暂无 Task")}
	}
	lines := make([]string, 0, len(m.taskHub.Rows))
	for _, row := range m.taskHub.Rows {
		marker := "  "
		if row.Task.TaskID == m.taskHub.SelectedTaskID {
			marker = "> "
		}
		if layoutFor(m.width, m.height).Mode == layoutMinimal {
			line := marker + shortTaskHubID(row.Task.TaskID) + "  " + emptyDash(row.Task.Name) + " · " + emptyDash(row.Task.Lifecycle)
			lines = append(lines, clipDisplay(line, width))
			continue
		}
		latest := "未运行"
		if row.HasLatestRun {
			latest = taskHubRunDisplayState(row.LatestRun)
			if row.LatestRun.Stage != "" {
				latest += " / " + row.LatestRun.Stage
			}
		}
		line := fmt.Sprintf("%s%s  %s  %s  %s", marker, emptyDash(row.Task.Name), shortTaskHubID(row.Task.TaskID), emptyDash(row.Task.Revision), latest)
		lines = append(lines, clipDisplay(line, width))
	}
	return lines
}

func (m model) taskHubRunsLines(width int) []string {
	runs := sortTaskHubRuns(m.taskHub.Snapshot.Runs)
	if len(runs) == 0 {
		return []string{subtleStyle.Render("暂无 Run")}
	}
	lines := make([]string, 0, len(runs))
	for _, run := range runs {
		marker := "  "
		if run.RunID == m.taskHub.SelectedRunID {
			marker = "> "
		}
		line := marker + shortTaskHubID(run.RunID) + "  " + taskHubRunDisplayState(run)
		if run.Stage != "" {
			line += " / " + run.Stage
		}
		if run.ControlStatus != "" {
			line += " · " + run.ControlStatus
		}
		if run.WorkerHandoff != nil {
			line += " · worker " + taskHubWorkerHandoffDisplayState(run.WorkerHandoff.State)
		}
		lines = append(lines, clipDisplay(line, width))
	}
	return lines
}

func (m model) taskHubQueueLines(width int) []string {
	queue := m.taskHub.Snapshot.Queue
	capacity := "未配置"
	if queue.Concurrency > 0 {
		capacity = fmt.Sprintf("%d", queue.Concurrency)
	}
	lines := []string{fmt.Sprintf("运行中 %d / %s", queue.Running, capacity), fmt.Sprintf("排队 %d", queue.Queued)}
	queuedRuns := make([]TaskHubRun, 0)
	for _, run := range m.taskHub.Snapshot.Runs {
		if run.QueuePosition > 0 {
			queuedRuns = append(queuedRuns, run)
		}
	}
	sort.SliceStable(queuedRuns, func(left, right int) bool { return queuedRuns[left].QueuePosition < queuedRuns[right].QueuePosition })
	for _, run := range queuedRuns {
		lines = append(lines, clipDisplay(fmt.Sprintf("#%d %s · %s", run.QueuePosition, shortTaskHubID(run.RunID), emptyDash(run.ExecutionState)), width))
	}
	return lines
}

// taskHubRunDisplayState maps the four control outcomes called out in the V2
// interaction contract without changing the underlying durable status.
func taskHubRunDisplayState(run TaskHubRun) string {
	switch strings.TrimSpace(run.ExecutionState) {
	case "paused":
		return "已暂停·可继续"
	case "canceled":
		return "已终止"
	case "interrupted", "in_doubt":
		return "异常中断·待 reconcile"
	}
	if run.Active && run.Control.OperationAction == TaskHubRunControlCancelStage && run.Control.OperationStatus == "acknowledged" {
		return "阶段已取消·Run 仍进行"
	}
	return emptyDash(run.ExecutionState)
}

func taskHubWorkerHandoffDisplayState(state string) string {
	switch strings.TrimSpace(state) {
	case "launching":
		return "启动中"
	case "handed_off":
		return "已接管"
	case "released":
		return "已释放"
	case "failed":
		return "启动失败"
	case "expired":
		return "已过期"
	default:
		return emptyDash(state)
	}
}

func shortTaskHubID(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 12 {
		return emptyDash(value)
	}
	return value[:12]
}
