package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"sort"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// TaskBoardColumn is the coarse workflow state exposed to an operator-facing
// task board. It deliberately projects durable lifecycle facts rather than
// inventing a second task state machine in the TUI.
type TaskBoardColumn string

const (
	TaskBoardPending   TaskBoardColumn = "pending"
	TaskBoardRunning   TaskBoardColumn = "running"
	TaskBoardCompleted TaskBoardColumn = "completed"
)

var (
	// ErrTaskBoardRecoveryPreviewRequired prevents a recovery confirmation from
	// silently constructing a plan the operator never inspected.
	ErrTaskBoardRecoveryPreviewRequired = errors.New("task board recovery preview is required")
	// ErrTaskBoardRecoveryPreviewStale marks a preview whose checkpoint or
	// executable semantics changed before the operator confirmed it.
	ErrTaskBoardRecoveryPreviewStale = errors.New("task board recovery preview is stale")
)

// TaskBoardReviewKind keeps the two durable review contracts distinct. An
// authoring-session gate cannot be decided through the TaskRevision route.
type TaskBoardReviewKind string

const (
	TaskBoardAuthoringReview TaskBoardReviewKind = "authoring"
	TaskBoardRevisionReview  TaskBoardReviewKind = "revision"
)

// TaskBoardReview identifies the one unambiguous open review that a board
// detail can act on. A task with multiple open gates remains visible but is
// intentionally not made actionable by this compact surface.
type TaskBoardReview struct {
	Kind       TaskBoardReviewKind
	RequestID  string
	RevisionID string
}

// TaskBoardOperatorSummary is the business-facing readout for the current
// authoring flow. It keeps validation semantics separate from StageAttempt
// execution, where a host verifier can complete successfully while rejecting
// the candidate it inspected.
type TaskBoardOperatorSummary struct {
	Status           string                     `json:"status"`
	Cause            string                     `json:"cause,omitempty"`
	NextAction       string                     `json:"next_action,omitempty"`
	LatestValidation *TaskBoardLatestValidation `json:"latest_validation,omitempty"`
}

// TaskBoardLatestValidation describes the latest immutable host validation
// receipt without exposing raw logs, paths, environment, or transcripts.
type TaskBoardLatestValidation struct {
	Status               string     `json:"status"`
	Verdict              string     `json:"verdict"`
	Stage                string     `json:"stage"`
	StageAttemptID       string     `json:"stage_attempt_id"`
	StageExecutionStatus string     `json:"stage_execution_status"`
	StageVerdict         string     `json:"stage_verdict"`
	FailureCode          string     `json:"failure_code,omitempty"`
	FailedStep           string     `json:"failed_step,omitempty"`
	ExitCode             int        `json:"exit_code,omitempty"`
	TestStarted          bool       `json:"test_started"`
	CandidateDigest      string     `json:"candidate_digest"`
	ReceiptDigest        string     `json:"receipt_digest"`
	ArtifactID           string     `json:"artifact_id"`
	RecordedAt           *time.Time `json:"recorded_at,omitempty"`
}

// TaskBoardTask is the presentation-neutral task projection consumed by the
// terminal board. It carries only durable identifiers and status facts; task
// content remains behind the lifecycle inspection boundary.
type TaskBoardTask struct {
	ID              string
	Slug            string
	Title           string
	RepositoryURL   string
	CommitSHA       string
	LifecycleState  string
	Column          TaskBoardColumn
	RunID           string
	RunStatus       string
	CurrentStage    string
	OperatorSummary *TaskBoardOperatorSummary
	Review          *TaskBoardReview
	OpenReviewCount int
	Runs            []TaskBoardRun
}

// TaskBoardAuthoringLaunch is a failed Standard source-capture operation that
// has not created a Task yet. It stays distinct from TaskBoardTask so the TUI
// never invents a Task or Run identity for a pre-materialization failure.
type TaskBoardAuthoringLaunch struct {
	OperationID    string
	RepositoryURL  string
	CommitSHA      string
	Slug           string
	Title          string
	TaskType       string
	Application    string
	CodeLang       string
	Is0To1         bool
	Objective      string
	Status         string
	FailureCode    string
	FailureSummary string
	CreatedAt      time.Time
	CanRetry       bool
}

// TaskBoardRun is the compact, presentation-neutral run history shown from a
// task detail. It contains durable facts only and never exposes a direct
// filesystem capability to the terminal adapter.
type TaskBoardRun struct {
	ID                    string
	ParentRunID           string
	AuthoringEvidence     *TaskBoardAuthoringEvidence
	AgentTurnTranscripts  []TaskBoardAgentTranscript
	Status                string
	CurrentStage          string
	OperatorSummary       *TaskBoardOperatorSummary
	FailureStage          string
	FailureClass          string
	FailureReason         string
	FailureCode           string
	FailureSummary        string
	FailureJobID          string
	FailureArtifactID     string
	FailureRecordedAt     *time.Time
	FailureRecoveryAction TaskBoardFailureRecoveryAction
	CanRedrive            bool
	CreatedAt             time.Time
	StartedAt             *time.Time
	FinishedAt            *time.Time
	LogPath               string
	HasLog                bool
	CanRetry              bool
	RetryReason           string
	RetryStrategy         TaskBoardRetryStrategy
	StandardProtocolRetry *TaskBoardStandardProtocolRetry
}

// TaskBoardStandardProtocolRetry identifies the one fenced Agent-stage retry
// currently eligible for a Standard Authoring Run. It is a display projection;
// Preview and confirmation revalidate every durable identity.
type TaskBoardStandardProtocolRetry struct {
	StageAttemptID string
	StageKey       string
	NodeAttemptID  string
	TranscriptID   string
	FailureCode    string
}

// TaskBoardAgentTranscript is the operator-facing, read-only projection of
// one retained Agent turn. It never grants artifact authority: the executor's
// separately validated stage result remains the only completion input.
type TaskBoardAgentTranscript struct {
	ID                    string
	StageKey              string
	StageAttemptID        string
	NodeAttemptID         string
	Turn                  int
	ResponseText          string
	ResponseSHA256        string
	ResponseBytes         int64
	ModelID               string
	SubmissionStatus      string
	SubmissionCount       int
	ProtocolRejectionCode string
	FailureCode           string
	CreatedAt             time.Time
	ExpiresAt             time.Time
	ExpiredAt             *time.Time
}

// TaskBoardAuthoringEvidence is a bounded, read-only projection of the
// immutable evidence that governs a Standard Authoring run. It deliberately
// omits model transcripts, source archives, repair prose, credentials, and
// worker logs.
type TaskBoardAuthoringEvidence struct {
	Contract TaskBoardAuthoringContract
	Claims   []TaskBoardAuthoringClaim
	Lineage  []TaskBoardAuthoringArtifact
}

// TaskBoardAuthoringContract contains the host-owned facts an operator needs
// to identify the task. Its digest remains the identity of the complete
// canonical contract stored in the object store.
type TaskBoardAuthoringContract struct {
	Digest             string
	TaskID             string
	Slug               string
	Title              string
	CodeLang           string
	TaskType           string
	Application        string
	Is0To1             bool
	RepositoryURL      string
	CommitSHA          string
	SnapshotDigest     string
	CheckoutRoot       string
	BaseImage          string
	Objective          string
	ProfileFingerprint string
	PackageFormat      string
}

// TaskBoardAuthoringClaim records the host's result for an allowlisted
// structured artifact. A match means the artifact was accepted only after
// its canonical claims matched the root contract; claim values are never
// echoed back into the terminal surface.
type TaskBoardAuthoringClaim struct {
	ArtifactKey string
	State       string
}

// TaskBoardAuthoringArtifact is immutable final-package evidence metadata.
// It identifies the Docker, harness, admission, and final package artifacts
// without reading their potentially large bodies.
type TaskBoardAuthoringArtifact struct {
	ArtifactKey string
	ArtifactID  string
	Digest      string
}

// TaskBoardFailureRecoveryAction is the explicitly supported next operation
// for a durable job failure. It is a display projection, not a capability:
// the receiving service revalidates the job state and failure code.
type TaskBoardFailureRecoveryAction string

const (
	TaskBoardFailureRecoveryNone           TaskBoardFailureRecoveryAction = ""
	TaskBoardFailureRecoveryReconcile      TaskBoardFailureRecoveryAction = "reconcile"
	TaskBoardFailureRecoveryRepairOrNewRun TaskBoardFailureRecoveryAction = "repair_or_new_run"
)

// TaskBoardRetryStrategy identifies the durable recovery contract selected for
// a Run. The terminal adapter uses it only to route an already-confirmed
// action; each application service revalidates the full subject checkpoint.
type TaskBoardRetryStrategy string

const (
	TaskBoardRetryStrategyNone                  TaskBoardRetryStrategy = ""
	TaskBoardRetryStrategyTaskContinuation      TaskBoardRetryStrategy = "task_continuation"
	TaskBoardRetryStrategyStandardProtocolStage TaskBoardRetryStrategy = "standard_protocol_stage"
)

// TaskBoardLog is a bounded read of a worker log selected through a Run's
// durable handoff record. The TUI receives content, not a filesystem handle.
//
// Records holds whole worker records parsed from the tail. Content keeps the
// verbatim text they came from so the terminal can always fall back to the
// original bytes; neither field is a filesystem handle or a live stream.
type TaskBoardLog struct {
	RunID   string
	Path    string
	Content string
	Records []TaskBoardLogRecord
	// HandoffSummary reports how many worker processes contributed to the file.
	HandoffSummary string
	Message        string
	// Truncated means older whole records were dropped from Records. It does not
	// mean any returned record is partial: every record in Records is complete.
	Truncated bool
	// RawTruncated means Content was clipped to its byte budget. Records can be
	// complete while the raw text behind them is not, so the two are reported
	// separately and the terminal can label each view honestly.
	RawTruncated bool
}

// TaskBoardSnapshot is a read-only point-in-time board projection.
type TaskBoardSnapshot struct {
	Tasks                    []TaskBoardTask
	PendingAuthoringLaunches []TaskBoardAuthoringLaunch
	AuthoringAvailable       bool
}

// TaskBoardStartAuthoringRequest contains the caller-selected immutable task
// identity, source coordinate, environment policy input, audit reason, and
// client command key. The service derives the actor and validates the full
// lifecycle protocol.
type TaskBoardStartAuthoringRequest struct {
	IdempotencyKey string
	RepositoryURL  string
	CommitSHA      string
	BaseImage      string
	Slug           string
	Title          string
	TaskType       string
	Application    string
	CodeLang       string
	Is0To1         bool
	Objective      string
	MetadataJSON   string
	Reason         string
}

// TaskBoardReviewDecision is the only pair of review actions exposed by the
// compact board. Terminal rejection remains available from the explicit CLI.
type TaskBoardReviewDecision string

const (
	TaskBoardApprove        TaskBoardReviewDecision = "approve"
	TaskBoardRequestChanges TaskBoardReviewDecision = "request_changes"
)

// TaskBoardDecideReviewRequest describes a reasoned decision selected from a
// previous board projection. The service verifies its durable ownership and
// replay boundary before deciding.
type TaskBoardDecideReviewRequest struct {
	IdempotencyKey string
	TaskID         string
	Review         TaskBoardReview
	Decision       TaskBoardReviewDecision
	Reason         string
}

// TaskBoardReadRunLogRequest identifies the selected task Run whose managed
// local worker log should be rendered in the terminal.
type TaskBoardReadRunLogRequest struct {
	TaskID string
	RunID  string
}

// TaskBoardRunSummaryRequest selects one Run for the operator-facing summary
// CLI. Task ownership is derived from the durable Run when omitted by callers.
type TaskBoardRunSummaryRequest struct {
	RunID string
}

// TaskBoardRunSummary is a compact JSON surface for operators and scripts.
type TaskBoardRunSummary struct {
	TaskID          string                    `json:"task_id"`
	TaskSlug        string                    `json:"task_slug"`
	TaskTitle       string                    `json:"task_title"`
	RunID           string                    `json:"run_id"`
	RunStatus       string                    `json:"run_status"`
	CurrentStage    string                    `json:"current_stage"`
	OperatorSummary *TaskBoardOperatorSummary `json:"operator_summary,omitempty"`
}

// TaskBoardRetryRunRequest identifies one confirmed retry or recovery action.
// Its key is retained by the TUI if a request needs to be retried after an
// infrastructure failure.
type TaskBoardRetryRunRequest struct {
	IdempotencyKey                  string
	TaskID                          string
	RunID                           string
	Reason                          string
	ExpectedRecoveryCheckpoint      *workflowkit.CheckpointRef
	ExpectedRecoveryPlanFingerprint workflowkit.Fingerprint
	ExpectedStandardProtocolRetry   *StandardProtocolStageRetryCheckpoint
}

// TaskBoardPreviewRunRecoveryRequest identifies a read-only recovery-plan
// preview. It intentionally has no idempotency key because it creates no
// durable command, plan, execution, job, or audit record.
type TaskBoardPreviewRunRecoveryRequest struct {
	TaskID string
	RunID  string
	Reason string
}

// TaskBoardRecoveryPreview is the small, presentation-neutral explanation of
// a freshly validated continuation plan. The preview cannot be executed: the
// confirmation path creates and revalidates its own durable frozen plan.
type TaskBoardRecoveryPreview struct {
	TaskID                  string
	RunID                   string
	Strategy                TaskBoardRetryStrategy
	CheckpointSequence      uint64
	CurrentExecutionEpoch   int
	NextExecutionEpoch      int
	SubjectDigest           string
	WorkflowFingerprint     string
	Checkpoint              workflowkit.CheckpointRef
	SemanticPlanFingerprint workflowkit.Fingerprint
	TargetStages            []string
	ReusedStages            []string
	ScheduledStages         []string
	InvalidatedStages       []string
	OperatorOnlyStages      []string
	StageReasons            map[string][]string
}

// TaskBoardStandardProtocolRetryPreview is the reviewable retry source for a
// failed Standard Agent stage. It intentionally exposes only transcript
// metadata, never treating response text as an artifact or completion input.
type TaskBoardStandardProtocolRetryPreview struct {
	TaskID       string
	RunID        string
	StageKey     string
	Source       TaskBoardStandardProtocolRetry
	Checkpoint   StandardProtocolStageRetryCheckpoint
	ResponseSHA  string
	ResponseSize int64
	ModelID      string
	Status       string
}

type TaskBoardPreviewStandardProtocolRetryRequest struct {
	TaskID         string
	RunID          string
	StageAttemptID string
	Reason         string
}

type TaskBoardPrepareStandardProtocolRetryRequest struct {
	TaskBoardPreviewStandardProtocolRetryRequest
	Expected StandardProtocolStageRetryCheckpoint
}

// TaskBoardPreparedStandardProtocolRetry is the last read-only fence before
// the operator confirms the Store-backed retry transaction.
type TaskBoardPreparedStandardProtocolRetry struct {
	TaskBoardStandardProtocolRetryPreview
	Reason string
}

// TaskBoardRetryAuthoringLaunchRequest targets a failed pre-Task source
// capture. It intentionally accepts no new idempotency key, actor, or reason:
// the service reconstructs and replays the immutable original launch command.
type TaskBoardRetryAuthoringLaunchRequest struct {
	OperationID string
}

// TaskBoardCancelRunRequest requests durable termination for the selected
// Run. It never cancels a local process directly.
type TaskBoardCancelRunRequest struct {
	IdempotencyKey string
	TaskID         string
	RunID          string
	Reason         string
}

// TaskBoardMutation is the small success result a TUI needs to refresh its
// projection and report the durable effect without interpreting raw records.
type TaskBoardMutation struct {
	OperationID string
	TaskID      string
	RunID       string
	Summary     string
}

// TaskBoardGateway is the complete application boundary used by the TUI. It
// keeps presentation code away from Store and managed filesystem operations.
type TaskBoardGateway interface {
	NewIdempotencyKey() (string, error)
	List(context.Context) (TaskBoardSnapshot, error)
	StartAuthoring(context.Context, TaskBoardStartAuthoringRequest) (TaskBoardMutation, error)
	DecideReview(context.Context, TaskBoardDecideReviewRequest) (TaskBoardMutation, error)
	InspectReview(context.Context, TaskBoardInspectReviewRequest) (TaskBoardReviewInspection, error)
	ReadRunLog(context.Context, TaskBoardReadRunLogRequest) (TaskBoardLog, error)
	PreviewRunRecovery(context.Context, TaskBoardPreviewRunRecoveryRequest) (TaskBoardRecoveryPreview, error)
	PreviewStandardProtocolRetry(context.Context, TaskBoardPreviewStandardProtocolRetryRequest) (TaskBoardStandardProtocolRetryPreview, error)
	PrepareStandardProtocolRetry(context.Context, TaskBoardPrepareStandardProtocolRetryRequest) (TaskBoardPreparedStandardProtocolRetry, error)
	RetryRun(context.Context, TaskBoardRetryRunRequest) (TaskBoardMutation, error)
	RetryAuthoringLaunch(context.Context, TaskBoardRetryAuthoringLaunchRequest) (TaskBoardMutation, error)
	CancelRun(context.Context, TaskBoardCancelRunRequest) (TaskBoardMutation, error)
	FlushQueuedRuns(context.Context) error
}

const taskBoardLogReadLimit int64 = 64 * 1024

// NewIdempotencyKey allocates a client command key for one board interaction.
// The TUI retains it across retries; the durable application services remain
// the authority that validates and replays it.
func (service *TaskBoardService) NewIdempotencyKey() (string, error) {
	if service == nil || service.core == nil || service.core.store == nil {
		return "", fmt.Errorf("task board service is not configured")
	}
	key, err := store.NewUUIDv7()
	if err != nil {
		return "", fmt.Errorf("allocate task board idempotency key: %w", err)
	}
	return key, nil
}

// TaskBoardService joins existing lifecycle services into the deliberately
// small read/mutate contract required by the new TUI. It does not add business
// rules: source capture, review CAS, durable jobs, and activation stay owned
// by their established services.
type TaskBoardService struct {
	core             *lifecycleServiceCore
	inspection       *LifecycleInspectionService
	authoring        *StandardAuthoringLaunchService
	authoringReviews *AuthoringReviewService
	mutations        *LifecycleMutationService
	activations      *RunActivationService
	continuations    *TaskContinuationService
	runs             *RunService
	control          *ExecutionControlService
	workerLauncher   RunWorkerHandoffLauncher
	actor            func() (string, error)
}

func newTaskBoardService(core *lifecycleServiceCore, inspection *LifecycleInspectionService, authoring *StandardAuthoringLaunchService, authoringReviews *AuthoringReviewService, mutations *LifecycleMutationService, activations *RunActivationService, continuations *TaskContinuationService, runs *RunService, control *ExecutionControlService, workerLauncher RunWorkerHandoffLauncher) *TaskBoardService {
	return &TaskBoardService{
		core:             core,
		inspection:       inspection,
		authoring:        authoring,
		authoringReviews: authoringReviews,
		mutations:        mutations,
		activations:      activations,
		continuations:    continuations,
		runs:             runs,
		control:          control,
		workerLauncher:   workerLauncher,
		actor:            localTaskBoardActor,
	}
}

// List projects every non-deleted Task through LifecycleInspectionService plus
// durable failed pre-Task authoring launches. The TUI therefore never opens
// SQLite or reconstructs review/run or launch-recovery ownership.
func (service *TaskBoardService) List(ctx context.Context) (TaskBoardSnapshot, error) {
	if service == nil || service.core == nil || service.core.store == nil || service.inspection == nil {
		return TaskBoardSnapshot{}, fmt.Errorf("task board service is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	tasks, err := service.core.store.ListTasksV2(ctx, false)
	if err != nil {
		return TaskBoardSnapshot{}, fmt.Errorf("list task board tasks: %w", err)
	}
	sort.SliceStable(tasks, func(left, right int) bool {
		if !tasks[left].UpdatedAt.Equal(tasks[right].UpdatedAt) {
			return tasks[left].UpdatedAt.After(tasks[right].UpdatedAt)
		}
		return tasks[left].ID < tasks[right].ID
	})
	snapshot := TaskBoardSnapshot{
		Tasks:                    make([]TaskBoardTask, 0, len(tasks)),
		PendingAuthoringLaunches: make([]TaskBoardAuthoringLaunch, 0),
		AuthoringAvailable:       service.authoring != nil && service.authoring.Available(),
	}
	for _, task := range tasks {
		detail, err := service.inspection.ReadTaskDetail(ctx, TaskInspectionQuery{TaskID: task.ID})
		if err != nil {
			return TaskBoardSnapshot{}, fmt.Errorf("inspect task board task %s: %w", task.ID, err)
		}
		projected, err := service.projectTaskBoardTask(ctx, detail)
		if err != nil {
			return TaskBoardSnapshot{}, fmt.Errorf("project task board task %s: %w", task.ID, err)
		}
		snapshot.Tasks = append(snapshot.Tasks, projected)
	}
	if service.authoring != nil {
		canRetry := service.authoring.Available()
		launches, err := service.authoring.listFailedPreTaskLaunches(ctx)
		if err != nil {
			return TaskBoardSnapshot{}, fmt.Errorf("list failed Standard authoring source captures: %w", err)
		}
		for _, launch := range launches {
			snapshot.PendingAuthoringLaunches = append(snapshot.PendingAuthoringLaunches, TaskBoardAuthoringLaunch{
				OperationID:    launch.Operation.ID,
				RepositoryURL:  launch.Request.RepositoryURL,
				CommitSHA:      launch.Request.CommitSHA,
				Slug:           launch.Request.Slug,
				Title:          launch.Request.Title,
				TaskType:       launch.Request.TaskType,
				Application:    launch.Request.Application,
				Objective:      launch.Request.Objective,
				Status:         "source_capture_failed",
				FailureCode:    launch.Failure.Code,
				FailureSummary: launch.Failure.Summary,
				CreatedAt:      launch.Operation.CreatedAt,
				CanRetry:       canRetry,
			})
		}
	}
	return snapshot, nil
}

// StartAuthoring launches the existing closed Standard authoring workflow and
// then gives the application-owned activation service a chance to hand work
// to its controlled child worker.
func (service *TaskBoardService) StartAuthoring(ctx context.Context, request TaskBoardStartAuthoringRequest) (TaskBoardMutation, error) {
	if service == nil || service.authoring == nil || !service.authoring.Available() {
		return TaskBoardMutation{}, ErrStandardAuthoringLaunchUnavailable
	}
	actor, err := service.currentActor()
	if err != nil {
		return TaskBoardMutation{}, err
	}
	if strings.TrimSpace(request.Reason) == "" {
		return TaskBoardMutation{}, fmt.Errorf("task board authoring reason is required")
	}
	if err := store.ValidateUUIDv7(strings.TrimSpace(request.IdempotencyKey)); err != nil {
		return TaskBoardMutation{}, fmt.Errorf("task board authoring idempotency key: %w", err)
	}
	receipt, err := service.authoring.Start(ctx, StandardAuthoringLaunchCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{
			IdempotencyKey: strings.TrimSpace(request.IdempotencyKey),
			Actor:          actor,
			Reason:         strings.TrimSpace(request.Reason),
		},
		RepositoryURL: request.RepositoryURL,
		CommitSHA:     request.CommitSHA,
		BaseImage:     request.BaseImage,
		Slug:          request.Slug,
		Title:         request.Title,
		TaskType:      request.TaskType,
		Application:   request.Application,
		CodeLang:      request.CodeLang,
		Is0To1:        request.Is0To1,
		Objective:     request.Objective,
		MetadataJSON:  request.MetadataJSON,
	})
	if err != nil {
		return TaskBoardMutation{}, err
	}
	if err := service.FlushQueuedRuns(ctx); err != nil {
		return TaskBoardMutation{}, err
	}
	return TaskBoardMutation{TaskID: receipt.TaskID, RunID: receipt.RunID, Summary: receipt.Summary}, nil
}

// DecideReview sends the decision through the correct authoring-session or
// TaskRevision contract. A caller-held key resumes a committed or prepared
// decision before reading any mutable review checkpoint again.
func (service *TaskBoardService) DecideReview(ctx context.Context, request TaskBoardDecideReviewRequest) (TaskBoardMutation, error) {
	if service == nil || service.mutations == nil || service.authoringReviews == nil {
		return TaskBoardMutation{}, fmt.Errorf("task board review services are not configured")
	}
	request.TaskID = strings.TrimSpace(request.TaskID)
	request.Review.RequestID = strings.TrimSpace(request.Review.RequestID)
	request.Review.RevisionID = strings.TrimSpace(request.Review.RevisionID)
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	request.Reason = strings.TrimSpace(request.Reason)
	if request.TaskID == "" || request.Review.RequestID == "" {
		return TaskBoardMutation{}, fmt.Errorf("task board review target is required")
	}
	if err := store.ValidateUUIDv7(request.TaskID); err != nil {
		return TaskBoardMutation{}, fmt.Errorf("task board review task ID: %w", err)
	}
	if request.Reason == "" {
		return TaskBoardMutation{}, fmt.Errorf("task board review reason is required")
	}
	action, err := taskBoardReviewAction(request.Decision)
	if err != nil {
		return TaskBoardMutation{}, err
	}
	actor, err := service.currentActor()
	if err != nil {
		return TaskBoardMutation{}, err
	}
	if err := store.ValidateUUIDv7(request.IdempotencyKey); err != nil {
		return TaskBoardMutation{}, fmt.Errorf("task board review idempotency key: %w", err)
	}
	result := TaskBoardMutation{TaskID: request.TaskID}
	switch request.Review.Kind {
	case TaskBoardAuthoringReview:
		snapshot, err := service.authoringReviews.Inspect(ctx, request.Review.RequestID)
		if err != nil {
			return TaskBoardMutation{}, err
		}
		session, err := service.core.store.GetAuthoringSession(ctx, snapshot.Binding.AuthoringSessionID)
		if err != nil {
			return TaskBoardMutation{}, err
		}
		if session == nil {
			return TaskBoardMutation{}, fmt.Errorf("%w: authoring session %s", ErrLifecycleNotFound, snapshot.Binding.AuthoringSessionID)
		}
		if session.TargetTaskID != request.TaskID {
			return TaskBoardMutation{}, fmt.Errorf("%w: authoring review request %s does not belong to task %s", store.ErrImmutable, request.Review.RequestID, request.TaskID)
		}
		if len(snapshot.Decisions) == 1 {
			existing := snapshot.Decisions[0]
			if existing.IdempotencyKey != request.IdempotencyKey || existing.Action != action || existing.Actor != actor || existing.Reason != request.Reason {
				return TaskBoardMutation{}, fmt.Errorf("%w: authoring review request %s", store.ErrIdempotencyConflict, request.Review.RequestID)
			}
			result.RunID = snapshot.Binding.RunID
			result.Summary = "已重放 authoring 审核决定"
			break
		}
		checkpoint := authoringReviewCheckpoint(snapshot)
		decision, err := service.authoringReviews.Decide(ctx, DecideAuthoringReviewRequest{
			IdempotencyKey: request.IdempotencyKey,
			Action:         action,
			Actor:          actor,
			Reason:         request.Reason,
			Expected:       checkpoint,
		})
		if err != nil {
			return TaskBoardMutation{}, err
		}
		result.RunID = decision.Binding.RunID
		result.Summary = "已记录 authoring 审核决定并排队 resolution job"
	case TaskBoardRevisionReview:
		if request.Review.RevisionID == "" {
			return TaskBoardMutation{}, fmt.Errorf("TaskRevision review target is missing its revision")
		}
		base := LifecycleMutationCommandBase{
			IdempotencyKey: request.IdempotencyKey,
			Actor:          actor,
			Reason:         request.Reason,
			Expected: LifecycleMutationCheckpoint{
				TaskID:          request.TaskID,
				RevisionID:      request.Review.RevisionID,
				ReviewRequestID: request.Review.RequestID,
			},
		}
		receipt, resumed, err := service.mutations.ResumeReview(ctx, base, action)
		if err != nil {
			return TaskBoardMutation{}, err
		}
		if !resumed {
			checkpoint, err := service.mutations.CaptureReviewCheckpoint(ctx, request.TaskID, request.Review.RevisionID, request.Review.RequestID)
			if err != nil {
				return TaskBoardMutation{}, err
			}
			base.Expected = checkpoint
			receipt, err = service.mutations.DecideReview(ctx, DecideReviewLifecycleCommand{
				LifecycleMutationCommandBase: base,
				Decision:                     action,
			})
			if err != nil {
				return TaskBoardMutation{}, err
			}
		}
		if err := validateTaskBoardReviewReceipt(receipt, request, action); err != nil {
			return TaskBoardMutation{}, err
		}
		result.RunID = receipt.RunID
		result.Summary = receipt.Summary
	default:
		return TaskBoardMutation{}, fmt.Errorf("unsupported task board review kind %q", request.Review.Kind)
	}
	if err := service.FlushQueuedRuns(ctx); err != nil {
		return TaskBoardMutation{}, err
	}
	return result, nil
}

// ReadRunLog returns at most the tail of one worker log. The caller may name
// a task and Run, but the service verifies their durable ownership before it
// looks up the Run's own handoff record.
func (service *TaskBoardService) ReadRunLog(ctx context.Context, request TaskBoardReadRunLogRequest) (TaskBoardLog, error) {
	if service == nil || service.core == nil || service.core.store == nil || service.inspection == nil {
		return TaskBoardLog{}, fmt.Errorf("task board service is not configured")
	}
	request.TaskID = strings.TrimSpace(request.TaskID)
	request.RunID = strings.TrimSpace(request.RunID)
	if _, err := service.taskBoardRun(ctx, request.TaskID, request.RunID); err != nil {
		return TaskBoardLog{}, err
	}

	handoffs, err := service.core.store.ListRunWorkerHandoffsForRun(ctx, request.RunID)
	if err != nil {
		return TaskBoardLog{}, fmt.Errorf("list worker handoffs for Run %s: %w", request.RunID, err)
	}
	path := taskBoardLogPath(handoffs)
	log := TaskBoardLog{RunID: request.RunID, Path: path}
	if path == "" {
		log.Message = "当前 Run 暂无可读取的本地日志"
		return log, nil
	}

	file, err := os.Open(path)
	if err != nil {
		log.Message = "日志暂时不可读取: " + err.Error()
		return log, nil
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		log.Message = "无法读取日志信息: " + err.Error()
		return log, nil
	}
	if !info.Mode().IsRegular() {
		log.Message = "日志目标不是可读取的常规文件"
		return log, nil
	}

	// Read by record boundary, not by byte window. One worker record can exceed
	// the old 64 KiB window on its own, so a fixed byte tail almost always began
	// and ended mid-record and the terminal could only ever show a fragment.
	start := max(int64(0), info.Size()-taskBoardLogScanLimit)
	content := make([]byte, info.Size()-start)
	count, readErr := file.ReadAt(content, start)
	if readErr != nil && readErr != io.EOF {
		log.Message = "读取日志失败: " + readErr.Error()
		return log, nil
	}
	text := strings.ToValidUTF8(string(content[:count]), "?")
	tail := readTaskBoardLogTail(text, taskBoardLogRecordLimit)
	log.Records = tail.Records
	log.Content = tail.Raw
	// A scan window that began past the start of the file means earlier records
	// exist that this read did not return.
	log.Truncated = tail.RecordsDropped || start > 0
	log.RawTruncated = tail.RawTruncated
	log.HandoffSummary = taskBoardLogHandoffSummary(handoffs)
	if strings.TrimSpace(log.Content) == "" && len(log.Records) == 0 {
		log.Message = "日志文件当前为空"
	} else if len(log.Records) == 0 {
		// Text without a single record boundary is still returned verbatim, but the
		// operator is told why it was not broken into records.
		log.Message = "日志内容不是 worker 记录格式，已按原文显示"
	}
	return log, nil
}

// Summary returns the same operator-facing Run readout used by the TUI, but
// shaped as a compact CLI response.
func (service *TaskBoardService) Summary(ctx context.Context, request TaskBoardRunSummaryRequest) (TaskBoardRunSummary, error) {
	if service == nil || service.inspection == nil {
		return TaskBoardRunSummary{}, fmt.Errorf("task board service is not configured")
	}
	runID := strings.TrimSpace(request.RunID)
	if err := store.ValidateUUIDv7(runID); err != nil {
		return TaskBoardRunSummary{}, fmt.Errorf("task board summary run ID: %w", err)
	}
	detail, err := service.taskBoardRun(ctx, "", runID)
	if err != nil {
		return TaskBoardRunSummary{}, err
	}
	projected, err := service.projectTaskBoardTask(ctx, detail)
	if err != nil {
		return TaskBoardRunSummary{}, err
	}
	for _, run := range projected.Runs {
		if run.ID != runID {
			continue
		}
		return TaskBoardRunSummary{
			TaskID:          projected.ID,
			TaskSlug:        projected.Slug,
			TaskTitle:       projected.Title,
			RunID:           run.ID,
			RunStatus:       run.Status,
			CurrentStage:    run.CurrentStage,
			OperatorSummary: cloneTaskBoardOperatorSummary(run.OperatorSummary),
		}, nil
	}
	return TaskBoardRunSummary{}, fmt.Errorf("%w: run %s", ErrLifecycleNotFound, runID)
}

// RetryRun selects the recovery contract from the durable Run subject. The
// terminal UI submits one retry intent; task revisions and pre-materialization
// authoring sessions keep their distinct checkpoint and commit contracts here.
func (service *TaskBoardService) RetryRun(ctx context.Context, request TaskBoardRetryRunRequest) (TaskBoardMutation, error) {
	if service == nil {
		return TaskBoardMutation{}, fmt.Errorf("task board service is not configured")
	}
	prepared, actor, err := service.prepareTaskBoardRunAction(ctx, request.IdempotencyKey, request.TaskID, request.RunID, request.Reason)
	if err != nil {
		return TaskBoardMutation{}, err
	}
	run, err := service.taskBoardWorkflowRun(ctx, prepared.RunID)
	if err != nil {
		return TaskBoardMutation{}, err
	}
	switch run.SubjectKind {
	case store.WorkflowRunSubjectTaskRevision:
		return service.retryTaskRevisionRun(ctx, prepared, actor, request.ExpectedRecoveryCheckpoint, request.ExpectedRecoveryPlanFingerprint)
	case store.WorkflowRunSubjectAuthoringSession:
		if request.ExpectedStandardProtocolRetry != nil {
			return service.retryStandardProtocolStage(ctx, prepared, actor, request.ExpectedStandardProtocolRetry)
		}
		return service.retryAuthoringRun(ctx, prepared, actor, request.ExpectedRecoveryCheckpoint, request.ExpectedRecoveryPlanFingerprint)
	default:
		return TaskBoardMutation{}, fmt.Errorf("Run %s has no retry contract", prepared.RunID)
	}
}

// PreviewStandardProtocolRetry renders the source facts for an eligible
// Standard Agent retry. It does not create a command, stage attempt, job, or
// audit record; Prepare and RetryRun revalidate the same immutable checkpoint.
func (service *TaskBoardService) PreviewStandardProtocolRetry(ctx context.Context, request TaskBoardPreviewStandardProtocolRetryRequest) (TaskBoardStandardProtocolRetryPreview, error) {
	if service == nil || service.runs == nil {
		return TaskBoardStandardProtocolRetryPreview{}, fmt.Errorf("task board Standard protocol retry service is not configured")
	}
	if strings.TrimSpace(request.Reason) == "" {
		return TaskBoardStandardProtocolRetryPreview{}, fmt.Errorf("task board Standard protocol retry reason is required")
	}
	if _, err := service.taskBoardRun(ctx, strings.TrimSpace(request.TaskID), strings.TrimSpace(request.RunID)); err != nil {
		return TaskBoardStandardProtocolRetryPreview{}, err
	}
	preview, err := service.runs.PreviewStandardProtocolStageRetry(ctx, request.RunID, request.StageAttemptID)
	if err != nil {
		return TaskBoardStandardProtocolRetryPreview{}, err
	}
	return taskBoardStandardProtocolRetryPreview(request.TaskID, preview), nil
}

// PrepareStandardProtocolRetry is the second, read-only confirmation fence.
// It requires a reason and replays the preview against current durable facts
// so the final mutation cannot silently retarget a newer failure.
func (service *TaskBoardService) PrepareStandardProtocolRetry(ctx context.Context, request TaskBoardPrepareStandardProtocolRetryRequest) (TaskBoardPreparedStandardProtocolRetry, error) {
	preview, err := service.PreviewStandardProtocolRetry(ctx, request.TaskBoardPreviewStandardProtocolRetryRequest)
	if err != nil {
		return TaskBoardPreparedStandardProtocolRetry{}, err
	}
	if preview.Checkpoint != request.Expected {
		return TaskBoardPreparedStandardProtocolRetry{}, ErrStandardProtocolStageRetryStale
	}
	return TaskBoardPreparedStandardProtocolRetry{TaskBoardStandardProtocolRetryPreview: preview, Reason: strings.TrimSpace(request.Reason)}, nil
}

func taskBoardStandardProtocolRetryPreview(taskID string, preview StandardProtocolStageRetryPreview) TaskBoardStandardProtocolRetryPreview {
	transcript := preview.Transcript
	return TaskBoardStandardProtocolRetryPreview{
		TaskID: taskID, RunID: preview.Run.ID, StageKey: string(preview.StageKey),
		Source: TaskBoardStandardProtocolRetry{
			StageAttemptID: preview.SourceStageAttempt.ID, StageKey: string(preview.StageKey), NodeAttemptID: preview.SourceNodeAttempt.ID,
			TranscriptID: transcript.ID, FailureCode: preview.Checkpoint.FailureCode,
		},
		Checkpoint: preview.Checkpoint, ResponseSHA: transcript.ResponseSHA256, ResponseSize: transcript.ResponseBytes,
		ModelID: transcript.ModelID, Status: string(transcript.SubmissionStatus),
	}
}

// PreviewRunRecovery proves the current Run can resume from a verified
// checkpoint and summarizes exactly which frozen stages would be reused or
// scheduled. It deliberately uses the same preview planners as the CLI, but
// it never persists the ephemeral plan that it receives.
func (service *TaskBoardService) PreviewRunRecovery(ctx context.Context, request TaskBoardPreviewRunRecoveryRequest) (TaskBoardRecoveryPreview, error) {
	if service == nil {
		return TaskBoardRecoveryPreview{}, fmt.Errorf("task board service is not configured")
	}
	request.TaskID = strings.TrimSpace(request.TaskID)
	request.RunID = strings.TrimSpace(request.RunID)
	request.Reason = strings.TrimSpace(request.Reason)
	if request.Reason == "" {
		return TaskBoardRecoveryPreview{}, fmt.Errorf("task board recovery preview reason is required")
	}
	if _, err := service.taskBoardRun(ctx, request.TaskID, request.RunID); err != nil {
		return TaskBoardRecoveryPreview{}, err
	}
	actor, err := service.currentActor()
	if err != nil {
		return TaskBoardRecoveryPreview{}, err
	}
	run, err := service.taskBoardWorkflowRun(ctx, request.RunID)
	if err != nil {
		return TaskBoardRecoveryPreview{}, err
	}
	commandKey, err := store.NewUUIDv7()
	if err != nil {
		return TaskBoardRecoveryPreview{}, fmt.Errorf("allocate task board recovery preview key: %w", err)
	}

	var plan workflowkit.ContinuationPlan
	strategy := taskBoardRetryStrategy(run)
	continuationTaskID := request.TaskID
	if run.SubjectKind == store.WorkflowRunSubjectAuthoringSession {
		// A pre-materialization authoring run binds its checkpoint to the
		// frozen source/session, not to a durable Task; passing the board
		// Task ID would fail the continuation checkpoint subject equality
		// check that task-revision recoveries rely on.
		continuationTaskID = ""
	}
	switch run.SubjectKind {
	case store.WorkflowRunSubjectTaskRevision, store.WorkflowRunSubjectAuthoringSession:
		if service.continuations == nil {
			return TaskBoardRecoveryPreview{}, fmt.Errorf("task board continuation service is not configured")
		}
		checkpoint, err := service.continuations.CurrentCheckpoint(ctx, request.RunID)
		if err != nil {
			return TaskBoardRecoveryPreview{}, err
		}
		plan, err = service.continuations.PreviewTaskContinuation(ctx, ContinueTaskCommand{
			CommandKey: commandKey,
			TaskID:     continuationTaskID,
			RunID:      request.RunID,
			Expected:   checkpoint,
			Actor:      actor,
			Reason:     request.Reason,
		})
		if err != nil {
			return TaskBoardRecoveryPreview{}, err
		}
	default:
		return TaskBoardRecoveryPreview{}, fmt.Errorf("Run %s has no recovery preview contract", request.RunID)
	}
	return taskBoardRecoveryPreview(request.TaskID, request.RunID, strategy, plan)
}

func taskBoardRecoveryPreview(taskID, runID string, strategy TaskBoardRetryStrategy, plan workflowkit.ContinuationPlan) (TaskBoardRecoveryPreview, error) {
	snapshot := plan.Snapshot()
	semanticFingerprint, err := plan.SemanticFingerprint()
	if err != nil {
		return TaskBoardRecoveryPreview{}, fmt.Errorf("fingerprint task board recovery preview: %w", err)
	}
	preview := TaskBoardRecoveryPreview{
		TaskID:                  taskID,
		RunID:                   runID,
		Strategy:                strategy,
		CheckpointSequence:      snapshot.BaseCheckpoint.Sequence,
		CurrentExecutionEpoch:   snapshot.BaseCheckpoint.ExecutionEpoch,
		NextExecutionEpoch:      snapshot.NextExecutionEpoch,
		SubjectDigest:           string(snapshot.BaseCheckpoint.SubjectDigest),
		WorkflowFingerprint:     string(snapshot.BaseCheckpoint.WorkflowFingerprint),
		Checkpoint:              snapshot.BaseCheckpoint,
		SemanticPlanFingerprint: semanticFingerprint,
		StageReasons:            make(map[string][]string, len(snapshot.Nodes)),
	}
	for _, transition := range snapshot.Nodes {
		nodeID := string(transition.NodeID)
		if len(transition.ReasonCodes) > 0 {
			reasons := make([]string, 0, len(transition.ReasonCodes))
			for _, reason := range transition.ReasonCodes {
				reasons = append(reasons, string(reason))
			}
			preview.StageReasons[nodeID] = reasons
		}
		switch transition.Disposition {
		case workflowkit.DispositionPreserve:
			preview.ReusedStages = append(preview.ReusedStages, nodeID)
		case workflowkit.DispositionSchedule:
			preview.ScheduledStages = append(preview.ScheduledStages, nodeID)
		case workflowkit.DispositionInvalidate:
			preview.InvalidatedStages = append(preview.InvalidatedStages, nodeID)
		case workflowkit.DispositionOperatorOnly:
			preview.OperatorOnlyStages = append(preview.OperatorOnlyStages, nodeID)
		}
		for _, reason := range transition.ReasonCodes {
			if reason == workflowkit.PlanReason("retry_requested") || reason == workflowkit.PlanReason("force_recompute") {
				preview.TargetStages = append(preview.TargetStages, nodeID)
				break
			}
		}
	}
	return preview, nil
}

// RetryAuthoringLaunch replays the one immutable authoring.start command that
// produced the selected durable source-capture failure. It never turns the
// operation into a synthetic Task/Run retry and never accepts new mutable
// launch input from the terminal.
func (service *TaskBoardService) RetryAuthoringLaunch(ctx context.Context, request TaskBoardRetryAuthoringLaunchRequest) (TaskBoardMutation, error) {
	if service == nil || service.authoring == nil || !service.authoring.Available() {
		return TaskBoardMutation{}, ErrStandardAuthoringLaunchUnavailable
	}
	operationID := strings.TrimSpace(request.OperationID)
	if err := store.ValidateUUIDv7(operationID); err != nil {
		return TaskBoardMutation{}, fmt.Errorf("task board Standard authoring launch operation ID: %w", err)
	}
	receipt, err := service.authoring.retryFailedPreTaskLaunch(ctx, operationID)
	if err != nil {
		return TaskBoardMutation{}, err
	}
	if err := service.FlushQueuedRuns(ctx); err != nil {
		return TaskBoardMutation{}, err
	}
	return TaskBoardMutation{OperationID: receipt.OperationID, TaskID: receipt.TaskID, RunID: receipt.RunID, Summary: receipt.Summary}, nil
}

func (service *TaskBoardService) retryTaskRevisionRun(ctx context.Context, prepared preparedTaskBoardRunAction, actor string, expectedCheckpoint *workflowkit.CheckpointRef, expectedPlanFingerprint workflowkit.Fingerprint) (TaskBoardMutation, error) {
	return service.continueRunRecovery(ctx, prepared, actor, expectedCheckpoint, expectedPlanFingerprint)
}

// retryAuthoringRun resumes a pre-materialization authoring Run from its
// frozen source/session checkpoint. The confirmation replays the preview
// fence: the durable plan must still match the checkpoint and semantic plan
// fingerprint the operator saw, otherwise the run changed underneath the
// action and the retry is rejected instead of silently rebinding.
func (service *TaskBoardService) retryAuthoringRun(ctx context.Context, prepared preparedTaskBoardRunAction, actor string, expectedCheckpoint *workflowkit.CheckpointRef, expectedPlanFingerprint workflowkit.Fingerprint) (TaskBoardMutation, error) {
	return service.continueRunRecovery(ctx, prepared, actor, expectedCheckpoint, expectedPlanFingerprint)
}

// continueRunRecovery is the subject-neutral confirmation path shared by task
// revisions and authoring sessions. It revalidates the previewed checkpoint
// and semantic plan fingerprint, freezes a durable continuation command, and
// re-queues the failed stage plus any unfinished downstream stages.
func (service *TaskBoardService) continueRunRecovery(ctx context.Context, prepared preparedTaskBoardRunAction, actor string, expectedCheckpoint *workflowkit.CheckpointRef, expectedPlanFingerprint workflowkit.Fingerprint) (TaskBoardMutation, error) {
	if service.continuations == nil {
		return TaskBoardMutation{}, fmt.Errorf("task board continuation service is not configured")
	}
	checkpoint, err := service.continuations.CurrentCheckpoint(ctx, prepared.RunID)
	if err != nil {
		return TaskBoardMutation{}, err
	}
	if expectedCheckpoint != nil && *expectedCheckpoint != checkpoint {
		return TaskBoardMutation{}, fmt.Errorf("%w: recovery checkpoint is stale", store.ErrOptimisticLock)
	}
	run, err := service.taskBoardWorkflowRun(ctx, prepared.RunID)
	if err != nil {
		return TaskBoardMutation{}, err
	}
	continuationTaskID := prepared.TaskID
	if run.SubjectKind == store.WorkflowRunSubjectAuthoringSession {
		// A pre-materialization authoring run binds its checkpoint to the
		// frozen source/session, not to a durable Task; passing the board
		// Task ID would fail the continuation checkpoint subject equality
		// check that task-revision recoveries rely on.
		continuationTaskID = ""
	}
	plan, err := service.continuations.PlanTaskContinuation(ctx, ContinueTaskCommand{
		CommandKey: prepared.IdempotencyKey,
		TaskID:     continuationTaskID,
		RunID:      prepared.RunID,
		Expected:   checkpoint,
		Actor:      actor,
		Reason:     prepared.Reason,
	})
	if err != nil {
		return TaskBoardMutation{}, err
	}
	if expectedPlanFingerprint != "" {
		semantic, err := plan.SemanticFingerprint()
		if err != nil {
			return TaskBoardMutation{}, fmt.Errorf("fingerprint recovery plan: %w", err)
		}
		if semantic != expectedPlanFingerprint {
			return TaskBoardMutation{}, fmt.Errorf("%w: recovery plan changed since preview", store.ErrOptimisticLock)
		}
	}
	if _, err := service.continuations.ExecuteTaskContinuation(ctx, plan.ID()); err != nil {
		return TaskBoardMutation{}, err
	}
	if err := service.FlushQueuedRuns(ctx); err != nil {
		return TaskBoardMutation{}, err
	}
	return TaskBoardMutation{TaskID: prepared.TaskID, RunID: prepared.RunID, Summary: "已为当前 Run 排队恢复"}, nil
}

func (service *TaskBoardService) retryStandardProtocolStage(ctx context.Context, prepared preparedTaskBoardRunAction, actor string, expected *StandardProtocolStageRetryCheckpoint) (TaskBoardMutation, error) {
	if service.runs == nil || expected == nil {
		return TaskBoardMutation{}, ErrStandardProtocolStageRetryStale
	}
	receipt, err := service.runs.RetryStandardProtocolStage(ctx, StandardProtocolStageRetryCommand{
		IdempotencyKey: prepared.IdempotencyKey, Actor: actor, Reason: prepared.Reason, RunID: prepared.RunID,
		SourceStageAttemptID: expected.StageAttemptID, Expected: *expected,
	})
	if err != nil {
		return TaskBoardMutation{}, err
	}
	if err := service.FlushQueuedRuns(ctx); err != nil {
		return TaskBoardMutation{}, err
	}
	return TaskBoardMutation{TaskID: prepared.TaskID, RunID: receipt.Run.ID, Summary: "已为当前 Standard 阶段排队重试"}, nil
}

// CancelRun records a durable termination request for the selected Run. A
// worker observes and acknowledges the operation; the TUI never kills a
// process on its own.
func (service *TaskBoardService) CancelRun(ctx context.Context, request TaskBoardCancelRunRequest) (TaskBoardMutation, error) {
	if service == nil || service.control == nil {
		return TaskBoardMutation{}, fmt.Errorf("task board execution control service is not configured")
	}
	prepared, actor, err := service.prepareTaskBoardRunAction(ctx, request.IdempotencyKey, request.TaskID, request.RunID, request.Reason)
	if err != nil {
		return TaskBoardMutation{}, err
	}
	run, err := service.taskBoardWorkflowRun(ctx, prepared.RunID)
	if err != nil {
		return TaskBoardMutation{}, err
	}
	checkpoint, err := service.control.CurrentCheckpoint(ctx, prepared.RunID)
	if err != nil {
		return TaskBoardMutation{}, err
	}
	operation, err := service.control.Request(ctx, RequestExecutionControlRequest{
		OperationKey: prepared.IdempotencyKey,
		Action:       store.ControlActionTerminate,
		RunID:        prepared.RunID,
		Expected:     checkpoint,
		Actor:        actor,
		Reason:       prepared.Reason,
	})
	if err != nil {
		return TaskBoardMutation{}, err
	}
	if cancellationNeedsCoordinatorWakeup(run.Status) {
		if err := service.enqueueCancellationCoordinator(ctx, run, operation, actor); err != nil {
			return TaskBoardMutation{}, err
		}
		if err := service.FlushQueuedRuns(ctx); err != nil {
			return TaskBoardMutation{}, err
		}
	}
	return TaskBoardMutation{TaskID: prepared.TaskID, RunID: prepared.RunID, Summary: "已请求取消当前 Run"}, nil
}

// cancellationNeedsCoordinatorWakeup identifies states in which a Run has no
// executing coordinator to observe a newly-recorded termination request. The
// normal running states deliberately remain excluded: their active worker is
// responsible for preserving the current stage's termination semantics.
func cancellationNeedsCoordinatorWakeup(status store.WorkflowRunStatus) bool {
	switch status {
	case store.WorkflowRunWaitingReview, store.WorkflowRunWaitingContinuation:
		return true
	default:
		return false
	}
}

// enqueueCancellationCoordinator makes a durable termination observable when
// a reviewed or continuation-blocked Run has already consumed its previous
// workflow coordinator job. Reusing the frozen payload is safe because the
// runtime verifies it before handling the queued control operation; the new
// idempotency key is bound to this exact termination request.
func (service *TaskBoardService) enqueueCancellationCoordinator(ctx context.Context, run store.WorkflowRun, operation store.DurableControlOperation, actor string) error {
	if service == nil || service.core == nil || service.core.store == nil {
		return fmt.Errorf("task board service is not configured")
	}
	if operation.Action != store.ControlActionTerminate || operation.RunID != run.ID {
		return fmt.Errorf("termination control operation does not match Run %s", run.ID)
	}
	jobs, err := service.core.store.ListDurableJobsForRun(ctx, run.ID)
	if err != nil {
		return fmt.Errorf("list workflow coordinator jobs for Run %s: %w", run.ID, err)
	}
	var payload string
	for _, job := range jobs {
		if job.CommandType == "workflow_run.execute" && job.EntityType == "workflow_run" && job.EntityID == run.ID && job.PayloadJSON != "" {
			payload = job.PayloadJSON
			break
		}
	}
	if payload == "" {
		return fmt.Errorf("Run %s has no frozen workflow coordinator payload for termination", run.ID)
	}
	job, err := service.core.store.CreateDurableJob(ctx, store.CreateDurableJobRequest{
		CommandType:    "workflow_run.execute",
		EntityType:     "workflow_run",
		EntityID:       run.ID,
		RunID:          run.ID,
		PayloadJSON:    payload,
		IdempotencyKey: "workflow-run-terminate:" + run.ID + ":" + operation.ID,
		Actor:          actor,
		Reason:         "acknowledge queued Run termination after operator review boundary",
	})
	if err != nil {
		return fmt.Errorf("queue termination coordinator for Run %s: %w", run.ID, err)
	}
	if job.CommandType != "workflow_run.execute" || job.EntityType != "workflow_run" || job.EntityID != run.ID || job.RunID != run.ID || job.PayloadJSON != payload {
		return fmt.Errorf("termination coordinator for Run %s does not match frozen workflow payload", run.ID)
	}
	return nil
}

type preparedTaskBoardRunAction struct {
	IdempotencyKey string
	TaskID         string
	RunID          string
	Reason         string
}

func (service *TaskBoardService) prepareTaskBoardRunAction(ctx context.Context, idempotencyKey, taskID, runID, reason string) (preparedTaskBoardRunAction, string, error) {
	prepared := preparedTaskBoardRunAction{
		IdempotencyKey: strings.TrimSpace(idempotencyKey),
		TaskID:         strings.TrimSpace(taskID),
		RunID:          strings.TrimSpace(runID),
		Reason:         strings.TrimSpace(reason),
	}
	if err := store.ValidateUUIDv7(prepared.IdempotencyKey); err != nil {
		return preparedTaskBoardRunAction{}, "", fmt.Errorf("task board Run action idempotency key: %w", err)
	}
	if prepared.Reason == "" {
		return preparedTaskBoardRunAction{}, "", fmt.Errorf("task board Run action reason is required")
	}
	if _, err := service.taskBoardRun(ctx, prepared.TaskID, prepared.RunID); err != nil {
		return preparedTaskBoardRunAction{}, "", err
	}
	actor, err := service.currentActor()
	if err != nil {
		return preparedTaskBoardRunAction{}, "", err
	}
	return prepared, actor, nil
}

func (service *TaskBoardService) taskBoardRun(ctx context.Context, taskID, runID string) (TaskInspectionSnapshot, error) {
	if service == nil || service.inspection == nil {
		return TaskInspectionSnapshot{}, fmt.Errorf("task board inspection service is not configured")
	}
	detail, err := service.inspection.ReadTaskDetail(ctx, TaskInspectionQuery{TaskID: taskID, RunID: runID})
	if err != nil {
		return TaskInspectionSnapshot{}, fmt.Errorf("read task board Run: %w", err)
	}
	return detail, nil
}

func (service *TaskBoardService) taskBoardWorkflowRun(ctx context.Context, runID string) (store.WorkflowRun, error) {
	if service == nil || service.core == nil || service.core.store == nil {
		return store.WorkflowRun{}, fmt.Errorf("task board service is not configured")
	}
	run, err := service.core.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return store.WorkflowRun{}, fmt.Errorf("read task board Run %s: %w", runID, err)
	}
	if run == nil {
		return store.WorkflowRun{}, fmt.Errorf("%w: run %s", ErrLifecycleNotFound, runID)
	}
	return *run, nil
}

func validateTaskBoardReviewReceipt(receipt LifecycleMutationReceipt, request TaskBoardDecideReviewRequest, action store.ReviewDecisionAction) error {
	if receipt.Action != LifecycleMutationReview || receipt.TaskID != request.TaskID || receipt.RevisionID != request.Review.RevisionID ||
		receipt.ReviewRequestID != request.Review.RequestID || receipt.ReviewDecision != string(action) {
		return fmt.Errorf("%w: lifecycle review receipt does not match task board request", store.ErrIdempotencyConflict)
	}
	return nil
}

// FlushQueuedRuns preserves the old Task Hub's safe handoff boundary. It is
// also used after board mutations and before the TUI exits.
func (service *TaskBoardService) FlushQueuedRuns(ctx context.Context) error {
	if service == nil || service.activations == nil || !service.activations.Available() {
		return nil
	}
	if err := service.activations.Drain(ctx); err != nil {
		return fmt.Errorf("activate queued Run workers: %w", err)
	}
	return nil
}

func (service *TaskBoardService) currentActor() (string, error) {
	if service == nil || service.actor == nil {
		return "", fmt.Errorf("task board actor is not configured")
	}
	return service.actor()
}

func localTaskBoardActor() (string, error) {
	current, err := user.Current()
	if err != nil || strings.TrimSpace(current.Username) == "" {
		return "", fmt.Errorf("local OS actor is required")
	}
	return strings.TrimSpace(current.Username), nil
}

func (service *TaskBoardService) projectTaskBoardTask(ctx context.Context, detail TaskInspectionSnapshot) (TaskBoardTask, error) {
	task := TaskBoardTask{
		ID:             detail.Task.ID,
		Slug:           detail.Task.Slug,
		Title:          detail.Task.Title,
		RepositoryURL:  detail.Task.SourceRepo,
		CommitSHA:      detail.Task.SourceCommit,
		LifecycleState: string(detail.Task.LifecycleState),
		Column:         TaskBoardPending,
	}
	task.Review, task.OpenReviewCount = taskBoardOpenReview(detail)
	if len(detail.Runs) == 0 {
		return task, nil
	}
	task.Runs = make([]TaskBoardRun, 0, len(detail.Runs))
	for _, inspected := range detail.Runs {
		handoffs, err := service.core.store.ListRunWorkerHandoffsForRun(ctx, inspected.Run.ID)
		if err != nil {
			return TaskBoardTask{}, fmt.Errorf("list worker handoffs for Run %s: %w", inspected.Run.ID, err)
		}
		projected := service.projectTaskBoardRun(ctx, inspected, handoffs)
		task.Runs = append(task.Runs, projected)
	}
	latest := task.Runs[0]
	task.RunID = latest.ID
	task.RunStatus = latest.Status
	task.CurrentStage = latest.CurrentStage
	task.OperatorSummary = cloneTaskBoardOperatorSummary(latest.OperatorSummary)
	if task.Review != nil {
		task.Column = TaskBoardPending
		return task, nil
	}
	task.Column = taskBoardColumnForRun(detail.Runs[0].Run.Status)
	return task, nil
}

func (service *TaskBoardService) projectTaskBoardRun(ctx context.Context, inspected RunInspection, handoffs []store.RunWorkerHandoff) TaskBoardRun {
	logPath := taskBoardLogPath(handoffs)
	run := TaskBoardRun{
		ID:           inspected.Run.ID,
		ParentRunID:  inspected.Run.ParentRunID,
		Status:       string(inspected.Run.Status),
		CurrentStage: taskBoardCurrentStage(inspected.Stages),
		CreatedAt:    inspected.Run.CreatedAt,
		StartedAt:    inspected.Run.StartedAt,
		FinishedAt:   inspected.Run.FinishedAt,
		LogPath:      logPath,
		HasLog:       logPath != "",
	}
	if failure := taskBoardDurableJobFailure(inspected.Jobs, inspected.Stages); failure != nil {
		run.FailureStage = failure.Stage
		run.FailureCode = failure.Code
		run.FailureSummary = failure.Summary
		// Keep the legacy reason populated for presentation clients that have
		// not yet adopted FailureSummary. The durable record remains the source.
		run.FailureReason = failure.Summary
		run.FailureJobID = failure.JobID
		run.FailureArtifactID = failure.ArtifactID
		run.FailureRecordedAt = failure.RecordedAt
		run.FailureRecoveryAction = failure.RecoveryAction
		run.CanRedrive = failure.CanRedrive
	}
	run.RetryStrategy = taskBoardRetryStrategy(inspected.Run)
	service.projectTaskBoardAuthoringContext(ctx, inspected.Run, &run)
	run.OperatorSummary = service.projectTaskBoardOperatorSummary(ctx, inspected)
	run.AgentTurnTranscripts = service.projectTaskBoardAgentTurnTranscripts(ctx, inspected.Run, inspected.Stages)
	if protocolRetry := service.projectTaskBoardStandardProtocolRetry(ctx, inspected.Run, inspected.Stages); protocolRetry != nil {
		run.RetryStrategy = TaskBoardRetryStrategyStandardProtocolStage
		run.StandardProtocolRetry = protocolRetry
		run.CanRetry = true
		return run
	}
	run.CanRetry, run.RetryReason = taskBoardRetryAvailability(inspected.Run)
	return run
}

func (service *TaskBoardService) projectTaskBoardStandardProtocolRetry(ctx context.Context, run store.WorkflowRun, attempts []store.StageAttempt) *TaskBoardStandardProtocolRetry {
	if service == nil || service.runs == nil || run.SubjectKind != store.WorkflowRunSubjectAuthoringSession || run.Status != store.WorkflowRunFailedRecoverable {
		return nil
	}
	for index := len(attempts) - 1; index >= 0; index-- {
		attempt := attempts[index]
		if attempt.ExecutionStatus != store.StageExecutionInfraFailed {
			continue
		}
		preview, err := service.runs.PreviewStandardProtocolStageRetry(ctx, run.ID, attempt.ID)
		if err != nil {
			continue
		}
		return &TaskBoardStandardProtocolRetry{
			StageAttemptID: preview.SourceStageAttempt.ID, StageKey: string(preview.StageKey), NodeAttemptID: preview.SourceNodeAttempt.ID,
			TranscriptID: preview.Transcript.ID, FailureCode: preview.Checkpoint.FailureCode,
		}
	}
	return nil
}

func (service *TaskBoardService) projectTaskBoardAgentTurnTranscripts(ctx context.Context, run store.WorkflowRun, attempts []store.StageAttempt) []TaskBoardAgentTranscript {
	if service == nil || service.core == nil || service.core.store == nil || run.SubjectKind != store.WorkflowRunSubjectAuthoringSession {
		return nil
	}
	transcripts := make([]TaskBoardAgentTranscript, 0)
	for _, attempt := range attempts {
		items, err := service.core.store.ListAgentTurnTranscriptsForStageAttempt(ctx, attempt.ID)
		if err != nil {
			return nil
		}
		for _, item := range items {
			submissions, err := service.core.store.ListAgentTurnTranscriptSubmissions(ctx, item.ID)
			if err != nil {
				return nil
			}
			transcripts = append(transcripts, TaskBoardAgentTranscript{
				ID: item.ID, StageKey: attempt.StageKey, StageAttemptID: attempt.ID, NodeAttemptID: item.NodeAttemptID,
				Turn: item.Turn, ResponseText: item.ResponseText, ResponseSHA256: item.ResponseSHA256, ResponseBytes: item.ResponseBytes,
				ModelID: item.ModelID, SubmissionStatus: string(item.SubmissionStatus), SubmissionCount: len(submissions),
				ProtocolRejectionCode: item.ProtocolRejectionCode, FailureCode: item.FailureCode, CreatedAt: item.CreatedAt,
				ExpiresAt: item.ExpiresAt, ExpiredAt: item.ExpiredAt,
			})
		}
	}
	sort.SliceStable(transcripts, func(left, right int) bool {
		if !transcripts[left].CreatedAt.Equal(transcripts[right].CreatedAt) {
			return transcripts[left].CreatedAt.After(transcripts[right].CreatedAt)
		}
		if transcripts[left].StageAttemptID != transcripts[right].StageAttemptID {
			return transcripts[left].StageAttemptID > transcripts[right].StageAttemptID
		}
		return transcripts[left].Turn > transcripts[right].Turn
	})
	return transcripts
}

// projectTaskBoardAuthoringContext exposes the contract-safe evidence view
// required to inspect a 3.0 authoring Run. All values are derived from the
// immutable root contract, repair ledger, and artifact references; it does
// not create a second mutable task state.
func (service *TaskBoardService) projectTaskBoardAuthoringContext(ctx context.Context, source store.WorkflowRun, destination *TaskBoardRun) {
	if service == nil || service.core == nil || service.core.store == nil || destination == nil ||
		source.SubjectKind != store.WorkflowRunSubjectAuthoringSession || source.AuthoringSessionID == "" {
		return
	}
	session, err := service.core.store.GetAuthoringSession(ctx, source.AuthoringSessionID)
	if err != nil || session == nil {
		return
	}
	input, err := standardAuthoringContractInputFromSession(ctx, service.core.objects, *session)
	if err != nil {
		return
	}
	contract := input.Contract
	evidence := &TaskBoardAuthoringEvidence{Contract: TaskBoardAuthoringContract{
		Digest: string(input.ContentDigest), TaskID: contract.Task.ID, Slug: contract.Task.Slug, Title: contract.Task.Title,
		CodeLang: contract.Task.CodeLang, TaskType: contract.Task.TaskType, Application: contract.Task.Application, Is0To1: contract.Task.Is0To1,
		RepositoryURL: contract.Source.RepositoryURL, CommitSHA: contract.Source.CommitSHA, SnapshotDigest: contract.Source.SnapshotDigest,
		CheckoutRoot: contract.Source.CheckoutRoot, BaseImage: contract.Environment.BaseImage, Objective: contract.Objective,
		ProfileFingerprint: contract.Delivery.ProfileFingerprint, PackageFormat: contract.Delivery.PackageFormat,
	}}
	evidence.Claims, evidence.Lineage = service.projectTaskBoardAuthoringArtifacts(ctx, source)
	destination.AuthoringEvidence = evidence
}

func (service *TaskBoardService) projectTaskBoardAuthoringArtifacts(ctx context.Context, run store.WorkflowRun) ([]TaskBoardAuthoringClaim, []TaskBoardAuthoringArtifact) {
	if service == nil || service.core == nil || service.core.store == nil {
		return nil, nil
	}
	attempts, err := service.core.store.ListStageAttemptsForRun(ctx, run.ID)
	if err != nil {
		return nil, nil
	}
	latest := make(map[string]stageArtifactCandidate)
	for _, attempt := range attempts {
		if attempt.ExecutionStatus != store.StageExecutionCompleted {
			continue
		}
		references, refsErr := service.core.store.ListArtifactRefsForAttempt(ctx, attempt.ID)
		if refsErr != nil {
			return nil, nil
		}
		for _, reference := range references {
			if reference.RunID != run.ID || reference.StageKey != attempt.StageKey || reference.AttemptID != attempt.ID {
				return nil, nil
			}
			current, found := latest[reference.ArtifactKey]
			if !found || laterArtifactCandidate(attempt, reference, current) {
				latest[reference.ArtifactKey] = stageArtifactCandidate{attempt: attempt, ref: reference}
			}
		}
	}
	claims := make([]TaskBoardAuthoringClaim, 0, 2)
	for _, key := range []string{"task_specification", "candidate_snapshot"} {
		if _, found := latest[key]; found {
			claims = append(claims, TaskBoardAuthoringClaim{ArtifactKey: key, State: "match"})
		}
	}
	lineageKeys := []string{
		"instruction", "task_toml", "dockerfile", "solve_script", "test_script", "tests_analysis",
		"candidate_snapshot", "validation_receipt", "final_attestation", "codeedge_package_admission_report",
	}
	lineage := make([]TaskBoardAuthoringArtifact, 0, len(lineageKeys))
	for _, key := range lineageKeys {
		candidate, found := latest[key]
		if !found {
			continue
		}
		lineage = append(lineage, TaskBoardAuthoringArtifact{ArtifactKey: key, ArtifactID: candidate.ref.ID, Digest: candidate.ref.ContentDigest})
	}
	return claims, lineage
}

func (service *TaskBoardService) projectTaskBoardOperatorSummary(ctx context.Context, inspected RunInspection) *TaskBoardOperatorSummary {
	run := inspected.Run
	if run.SubjectKind != store.WorkflowRunSubjectAuthoringSession || !isAdmissibleStandardAuthoringRun(run) {
		return nil
	}
	candidate, found, err := service.latestTaskBoardValidationReceiptCandidate(ctx, run, inspected.Stages)
	if err != nil {
		return &TaskBoardOperatorSummary{
			Status:     "validation_unavailable",
			Cause:      taskBoardOperatorSummaryCause(err.Error()),
			NextAction: "inspect validation artifact lineage",
		}
	}
	if !found {
		return &TaskBoardOperatorSummary{
			Status:     "validation_pending",
			Cause:      "host_candidate_verify has not produced validation_receipt",
			NextAction: "continue authoring run",
		}
	}
	validation, err := service.readTaskBoardLatestValidation(ctx, run, candidate)
	if err != nil {
		return &TaskBoardOperatorSummary{
			Status:     "validation_unavailable",
			Cause:      taskBoardOperatorSummaryCause(err.Error()),
			NextAction: "inspect validation artifact lineage",
		}
	}
	summary := &TaskBoardOperatorSummary{Status: validation.Status, LatestValidation: validation}
	switch workflowkit.ValidationVerdict(validation.Verdict) {
	case workflowkit.ValidationPass:
		summary.Cause = "candidate passed host validation"
		summary.NextAction = "continue final review"
	case workflowkit.ValidationReject:
		if validation.FailedStep != "" {
			summary.Cause = "rejected at " + validation.FailedStep
		} else {
			summary.Cause = "candidate rejected by host validation"
		}
		summary.NextAction = "repair candidate"
	default:
		summary.Status = "validation_unavailable"
		summary.Cause = "validation receipt has unsupported verdict"
		summary.NextAction = "inspect validation artifact lineage"
	}
	return summary
}

func (service *TaskBoardService) latestTaskBoardValidationReceiptCandidate(ctx context.Context, run store.WorkflowRun, attempts []store.StageAttempt) (stageArtifactCandidate, bool, error) {
	if service == nil || service.core == nil || service.core.store == nil || service.core.objects == nil {
		return stageArtifactCandidate{}, false, fmt.Errorf("task board validation summary storage is not configured")
	}
	subject, err := service.core.resolveWorkflowRunSubject(ctx, run)
	if err != nil {
		return stageArtifactCandidate{}, false, err
	}
	var selected stageArtifactCandidate
	found := false
	for _, attempt := range attempts {
		if attempt.ExecutionStatus != store.StageExecutionCompleted || strings.TrimSpace(attempt.ArtifactManifestID) == "" {
			continue
		}
		if attempt.StageKey != workflowadapter.HostCandidateVerify && attempt.StageKey != workflowadapter.AuthoringRepair {
			continue
		}
		references, err := service.core.store.ListArtifactRefsForAttempt(ctx, attempt.ID)
		if err != nil {
			return stageArtifactCandidate{}, false, fmt.Errorf("list validation artifact refs for stage attempt %s: %w", attempt.ID, err)
		}
		for _, reference := range references {
			if reference.ArtifactKey != "validation_receipt" {
				continue
			}
			if reference.SchemaVersion != workflowkit.ValidationReceiptFormat {
				return stageArtifactCandidate{}, false, fmt.Errorf("validation_receipt artifact %s has schema %q", reference.ID, reference.SchemaVersion)
			}
			if reference.RunID != run.ID || reference.StageKey != attempt.StageKey || reference.AttemptID != attempt.ID ||
				reference.SubjectRevisionID != subject.subjectRevisionID() || reference.SubjectDigest != subject.subjectDigest() ||
				reference.WorkflowFingerprint != run.DefinitionHash {
				return stageArtifactCandidate{}, false, fmt.Errorf("validation_receipt artifact %s does not match Run lineage", reference.ID)
			}
			candidate := stageArtifactCandidate{attempt: attempt, ref: reference}
			if !found || laterArtifactCandidate(attempt, reference, selected) {
				selected = candidate
				found = true
			}
		}
	}
	return selected, found, nil
}

func (service *TaskBoardService) readTaskBoardLatestValidation(ctx context.Context, run store.WorkflowRun, candidate stageArtifactCandidate) (*TaskBoardLatestValidation, error) {
	index, err := loadStageArtifactManifestIndex(ctx, service.core.store, candidate.ref.ManifestID)
	if err != nil {
		return nil, err
	}
	subject, err := service.core.resolveWorkflowRunSubject(ctx, run)
	if err != nil {
		return nil, err
	}
	if index.manifest.SubjectRevisionID != subject.subjectRevisionID() || index.manifest.SubjectDigest != subject.subjectDigest() ||
		index.manifest.WorkflowFingerprint != run.DefinitionHash || index.payload.RunID != run.ID ||
		index.payload.StageAttemptID != candidate.ref.AttemptID || string(index.payload.StageKey) != candidate.ref.StageKey {
		return nil, fmt.Errorf("validation_receipt manifest does not match Run lineage")
	}
	object, err := index.objectFor(candidate.ref)
	if err != nil {
		return nil, err
	}
	raw, err := service.core.objects.ReadAll(ctx, object)
	if err != nil {
		return nil, fmt.Errorf("read validation_receipt artifact %s: %w", candidate.ref.ID, err)
	}
	var receipt workflowkit.ValidationReceipt
	if err := decodeStrictJSON(string(raw), &receipt); err != nil {
		return nil, fmt.Errorf("decode validation_receipt artifact %s: %w", candidate.ref.ID, err)
	}
	if err := receipt.Validate(); err != nil {
		return nil, fmt.Errorf("validate validation_receipt artifact %s: %w", candidate.ref.ID, err)
	}
	failedStep, exitCode, testStarted := taskBoardValidationDiagnostic(receipt)
	recordedAt := candidate.ref.CreatedAt.UTC()
	if candidate.attempt.FinishedAt != nil {
		recordedAt = candidate.attempt.FinishedAt.UTC()
	}
	validation := &TaskBoardLatestValidation{
		Status:               taskBoardValidationStatus(receipt.Verdict),
		Verdict:              string(receipt.Verdict),
		Stage:                candidate.attempt.StageKey,
		StageAttemptID:       candidate.attempt.ID,
		StageExecutionStatus: string(candidate.attempt.ExecutionStatus),
		StageVerdict:         string(candidate.attempt.Verdict),
		FailureCode:          string(receipt.FailureCode),
		FailedStep:           failedStep,
		ExitCode:             exitCode,
		TestStarted:          testStarted,
		CandidateDigest:      string(receipt.SnapshotDigest),
		ReceiptDigest:        string(receipt.Digest),
		ArtifactID:           candidate.ref.ID,
		RecordedAt:           &recordedAt,
	}
	return validation, nil
}

func taskBoardValidationDiagnostic(receipt workflowkit.ValidationReceipt) (string, int, bool) {
	if len(receipt.Diagnostics) == 0 {
		return "", 0, false
	}
	selected := receipt.Diagnostics[0]
	for _, diagnostic := range receipt.Diagnostics {
		if diagnostic.ExitCode != 0 {
			selected = diagnostic
			break
		}
	}
	return selected.CommandID, selected.ExitCode, selected.TestStarted
}

func taskBoardValidationStatus(verdict workflowkit.ValidationVerdict) string {
	switch verdict {
	case workflowkit.ValidationPass:
		return "validation_passed"
	case workflowkit.ValidationReject:
		return "validation_rejected"
	default:
		return "validation_unavailable"
	}
}

func taskBoardOperatorSummaryCause(cause string) string {
	cause = strings.TrimSpace(cause)
	if cause == "" {
		return "validation summary is unavailable"
	}
	return taskBoardTruncate(cause, 240)
}

func cloneTaskBoardOperatorSummary(summary *TaskBoardOperatorSummary) *TaskBoardOperatorSummary {
	if summary == nil {
		return nil
	}
	copy := *summary
	if summary.LatestValidation != nil {
		validation := *summary.LatestValidation
		if summary.LatestValidation.RecordedAt != nil {
			recordedAt := summary.LatestValidation.RecordedAt.UTC()
			validation.RecordedAt = &recordedAt
		}
		copy.LatestValidation = &validation
	}
	return &copy
}

func taskBoardTruncate(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	if limit <= 3 {
		return value[:limit]
	}
	return value[:limit-1] + "..."
}

func taskBoardRetryStrategy(run store.WorkflowRun) TaskBoardRetryStrategy {
	switch run.SubjectKind {
	case store.WorkflowRunSubjectTaskRevision, store.WorkflowRunSubjectAuthoringSession:
		return TaskBoardRetryStrategyTaskContinuation
	default:
		return TaskBoardRetryStrategyNone
	}
}

// taskBoardRetryAvailability gates the operator-facing retry entry. Both
// task revisions and pre-materialization authoring sessions share the same
// recoverable status set; terminal outcomes (including content rejections)
// are not retryable in place and are handled by restart/new-run flows instead.
func taskBoardRetryAvailability(run store.WorkflowRun) (bool, string) {
	switch run.Status {
	case store.WorkflowRunFailedRecoverable, store.WorkflowRunInterrupted, store.WorkflowRunCanceled,
		store.WorkflowRunPaused, store.WorkflowRunWaitingContinuation:
		return true, ""
	default:
		return false, "当前 Run 状态不可重试"
	}
}

type taskBoardDurableFailure struct {
	Stage          string
	Code           string
	Summary        string
	JobID          string
	ArtifactID     string
	RecordedAt     *time.Time
	RecoveryAction TaskBoardFailureRecoveryAction
	CanRedrive     bool
}

type taskBoardDurableFailureDetails struct {
	ArtifactID string `json:"artifact_id"`
	Stage      string `json:"stage"`
	StageKey   string `json:"stage_key"`
}

// taskBoardDurableJobFailure selects the newest durable failure record rather
// than reconstructing failure semantics from a Run status or worker log.
func taskBoardDurableJobFailure(jobs []DurableJobInspection, stages []store.StageAttempt) *taskBoardDurableFailure {
	var selected *store.DurableJob
	for index := range jobs {
		job := jobs[index].Job
		if job.Failure == nil || (job.State != store.JobFailed && job.State != store.JobInDoubt) {
			continue
		}
		if selected == nil || taskBoardDurableFailureTime(job).After(taskBoardDurableFailureTime(*selected)) ||
			(taskBoardDurableFailureTime(job).Equal(taskBoardDurableFailureTime(*selected)) && job.ID > selected.ID) {
			candidate := job
			selected = &candidate
		}
	}
	if selected == nil || selected.Failure == nil {
		return nil
	}

	details := taskBoardFailureDetails(selected.Failure.DetailsJSON)
	recordedAt := taskBoardDurableFailureTime(*selected)
	result := &taskBoardDurableFailure{
		Stage:          taskBoardFailureStage(*selected, stages, details),
		Code:           selected.Failure.Code,
		Summary:        selected.Failure.Message,
		JobID:          selected.ID,
		ArtifactID:     taskBoardFailureArtifactID(*selected, details),
		RecordedAt:     &recordedAt,
		RecoveryAction: taskBoardFailureRecoveryAction(*selected),
		CanRedrive:     false,
	}
	return result
}

func taskBoardDurableFailureTime(job store.DurableJob) time.Time {
	if job.FinishedAt != nil {
		return job.FinishedAt.UTC()
	}
	return job.UpdatedAt.UTC()
}

func taskBoardFailureDetails(raw string) taskBoardDurableFailureDetails {
	var details taskBoardDurableFailureDetails
	if err := json.Unmarshal([]byte(raw), &details); err != nil {
		return taskBoardDurableFailureDetails{}
	}
	details.ArtifactID = strings.TrimSpace(details.ArtifactID)
	details.Stage = strings.TrimSpace(details.Stage)
	details.StageKey = strings.TrimSpace(details.StageKey)
	return details
}

func taskBoardFailureStage(job store.DurableJob, stages []store.StageAttempt, details taskBoardDurableFailureDetails) string {
	for _, stage := range stages {
		if stage.ID == job.StageAttemptID && strings.TrimSpace(stage.StageKey) != "" {
			return stage.StageKey
		}
	}
	if details.StageKey != "" {
		return details.StageKey
	}
	return details.Stage
}

func taskBoardFailureArtifactID(job store.DurableJob, details taskBoardDurableFailureDetails) string {
	if job.EntityType == "artifact_ref" && strings.TrimSpace(job.EntityID) != "" {
		return job.EntityID
	}
	return details.ArtifactID
}

func taskBoardFailureRecoveryAction(job store.DurableJob) TaskBoardFailureRecoveryAction {
	switch job.State {
	case store.JobFailed:
		return TaskBoardFailureRecoveryRepairOrNewRun
	case store.JobInDoubt:
		return TaskBoardFailureRecoveryReconcile
	default:
		return TaskBoardFailureRecoveryNone
	}
}

func taskBoardLogPath(handoffs []store.RunWorkerHandoff) string {
	for index := len(handoffs) - 1; index >= 0; index-- {
		if path := strings.TrimSpace(handoffs[index].LogPath); path != "" {
			return path
		}
	}
	return ""
}

func taskBoardOpenReview(detail TaskInspectionSnapshot) (*TaskBoardReview, int) {
	var candidates []TaskBoardReview
	for _, review := range detail.AuthoringReviews {
		if review.State == store.AuthoringReviewGateOpen {
			candidates = append(candidates, TaskBoardReview{Kind: TaskBoardAuthoringReview, RequestID: review.Request.ID})
		}
	}
	for _, review := range detail.Reviews {
		if review.Request.State == "open" {
			candidates = append(candidates, TaskBoardReview{Kind: TaskBoardRevisionReview, RequestID: review.Request.ID, RevisionID: review.Request.RevisionID})
		}
	}
	if len(candidates) != 1 {
		return nil, len(candidates)
	}
	review := candidates[0]
	return &review, 1
}

func taskBoardCurrentStage(stages []store.StageAttempt) string {
	for _, stage := range stages {
		switch stage.ExecutionStatus {
		case store.StageExecutionRunning, store.StageExecutionWaiting, store.StageExecutionQueued, store.StageExecutionInDoubt, store.StageExecutionReconciling:
			return stage.StageKey
		}
	}
	return ""
}

func taskBoardColumnForRun(status store.WorkflowRunStatus) TaskBoardColumn {
	switch status {
	case store.WorkflowRunQueued, store.WorkflowRunWaitingReview, store.WorkflowRunWaitingContinuation,
		store.WorkflowRunPaused, store.WorkflowRunFailedRecoverable, store.WorkflowRunInterrupted, store.WorkflowRunInDoubt:
		return TaskBoardPending
	case store.WorkflowRunSucceeded, store.WorkflowRunFailedTerminal, store.WorkflowRunCanceled:
		return TaskBoardCompleted
	default:
		return TaskBoardRunning
	}
}

func taskBoardReviewAction(decision TaskBoardReviewDecision) (store.ReviewDecisionAction, error) {
	switch decision {
	case TaskBoardApprove:
		return store.ReviewDecisionApprove, nil
	case TaskBoardRequestChanges:
		return store.ReviewDecisionRequestChanges, nil
	default:
		return "", fmt.Errorf("unsupported task board review decision %q", decision)
	}
}
