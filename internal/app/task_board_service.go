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
	Review          *TaskBoardReview
	OpenReviewCount int
	Runs            []TaskBoardRun
	// Evaluator describes the one unambiguous CodeEdge Phase-1/evaluator-child
	// action chain for this task. It is a projection only; every mutation below
	// revalidates its immutable parent/child bindings.
	Evaluator *TaskBoardEvaluatorStatus
}

// TaskBoardEvaluatorState names the narrow evaluator action state that can be
// safely projected to the terminal board.
type TaskBoardEvaluatorState string

const (
	TaskBoardEvaluatorAwaitingFinalReview TaskBoardEvaluatorState = "awaiting_final_review"
	TaskBoardEvaluatorReadyToLaunch       TaskBoardEvaluatorState = "ready_to_launch"
	TaskBoardEvaluatorChildActive         TaskBoardEvaluatorState = "child_active"
	TaskBoardEvaluatorReadyToAdopt        TaskBoardEvaluatorState = "ready_to_adopt"
	TaskBoardEvaluatorAdopted             TaskBoardEvaluatorState = "adopted"
	TaskBoardEvaluatorUnavailable         TaskBoardEvaluatorState = "unavailable"
)

// TaskBoardEvaluatorStatus is the UI-safe summary of one parent/child chain.
// It intentionally carries no profile, endpoint, credential, command, or
// evidence payload bytes.
type TaskBoardEvaluatorStatus struct {
	ParentRunID string
	ChildRunID  string
	State       TaskBoardEvaluatorState
	Reason      string
	CanLaunch   bool
	CanAdopt    bool
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
	Status                string
	CurrentStage          string
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
}

// TaskBoardFailureRecoveryAction is the explicitly supported next operation
// for a durable job failure. It is a display projection, not a capability:
// the receiving service revalidates the job state and failure code.
type TaskBoardFailureRecoveryAction string

const (
	TaskBoardFailureRecoveryNone                    TaskBoardFailureRecoveryAction = ""
	TaskBoardFailureRecoveryReconcile               TaskBoardFailureRecoveryAction = "reconcile"
	TaskBoardFailureRecoveryRedriveAuthoringHandoff TaskBoardFailureRecoveryAction = "redrive_standard_authoring_handoff"
	TaskBoardFailureRecoveryRepairOrNewRun          TaskBoardFailureRecoveryAction = "repair_or_new_run"
)

// TaskBoardRetryStrategy identifies the durable recovery contract selected for
// a Run. The terminal adapter uses it only to route an already-confirmed
// action; each application service revalidates the full subject checkpoint.
type TaskBoardRetryStrategy string

const (
	TaskBoardRetryStrategyNone                     TaskBoardRetryStrategy = ""
	TaskBoardRetryStrategyTaskContinuation         TaskBoardRetryStrategy = "task_continuation"
	TaskBoardRetryStrategyAuthoringRecovery        TaskBoardRetryStrategy = "authoring_recovery"
	TaskBoardRetryStrategyAuthoringAdmissionRepair TaskBoardRetryStrategy = "authoring_admission_repair"
)

// TaskBoardLog is a bounded read of a worker log selected through a Run's
// durable handoff record. The TUI receives content, not a filesystem handle.
type TaskBoardLog struct {
	RunID     string
	Path      string
	Content   string
	Message   string
	Truncated bool
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

// TaskBoardEvaluatorLaunchPreviewRequest selects one approved CodeEdge
// Phase-1 parent Run for a read-only evaluator launch preview.
type TaskBoardEvaluatorLaunchPreviewRequest struct {
	TaskID      string
	ParentRunID string
}

// TaskBoardEvaluatorLaunchPreview is the UI-safe form of the immutable child
// Run plan. The definition remains controlled by deployment composition.
type TaskBoardEvaluatorLaunchPreview struct {
	TaskID                      string
	ParentRunID                 string
	RevisionID                  string
	TemplateID                  string
	TemplateVersion             string
	ExecutionProfileFingerprint string
	ExecutionSpecFingerprint    string
}

// TaskBoardEvaluatorLaunchRequest is retained across the prepare/confirm
// boundary. The caller never supplies a profile, model, provider, secret, or
// execution specification.
type TaskBoardEvaluatorLaunchRequest struct {
	IdempotencyKey string
	TaskID         string
	ParentRunID    string
	Reason         string
}

// TaskBoardPreparedEvaluatorLaunch records the first confirmation without
// creating a child Run or invoking the evaluator.
type TaskBoardPreparedEvaluatorLaunch struct {
	TaskID                      string
	ParentRunID                 string
	InputBundleID               string
	ExecutionProfileFingerprint string
	ExecutionSpecFingerprint    string
}

// TaskBoardEvaluatorEvidenceHandoffPreviewRequest selects the immutable
// parent/child pair for a read-only evidence-adoption preview.
type TaskBoardEvaluatorEvidenceHandoffPreviewRequest struct {
	TaskID      string
	ParentRunID string
	ChildRunID  string
}

// TaskBoardEvaluatorEvidenceHandoffPreview contains only durable identities
// and verification fingerprints, never provider-produced payload bytes.
type TaskBoardEvaluatorEvidenceHandoffPreview struct {
	TaskID               string
	ParentRunID          string
	ChildRunID           string
	RevisionID           string
	HandoffFingerprint   string
	QwenTrialFingerprint string
	OpusTrialFingerprint string
}

// TaskBoardEvaluatorEvidenceHandoffRequest is retained across the
// prepare/adopt boundary for one immutable parent/child pair.
type TaskBoardEvaluatorEvidenceHandoffRequest struct {
	IdempotencyKey string
	TaskID         string
	ParentRunID    string
	ChildRunID     string
	Reason         string
}

// TaskBoardPreparedEvaluatorEvidenceHandoff records the first adoption
// confirmation before the durable evidence bridge is created.
type TaskBoardPreparedEvaluatorEvidenceHandoff struct {
	TaskID               string
	OperationID          string
	ParentRunID          string
	ChildRunID           string
	HandoffFingerprint   string
	QwenTrialFingerprint string
	OpusTrialFingerprint string
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
	ReadRunLog(context.Context, TaskBoardReadRunLogRequest) (TaskBoardLog, error)
	PreviewRunRecovery(context.Context, TaskBoardPreviewRunRecoveryRequest) (TaskBoardRecoveryPreview, error)
	RetryRun(context.Context, TaskBoardRetryRunRequest) (TaskBoardMutation, error)
	RetryAuthoringLaunch(context.Context, TaskBoardRetryAuthoringLaunchRequest) (TaskBoardMutation, error)
	CancelRun(context.Context, TaskBoardCancelRunRequest) (TaskBoardMutation, error)
	PreviewEvaluatorLaunch(context.Context, TaskBoardEvaluatorLaunchPreviewRequest) (TaskBoardEvaluatorLaunchPreview, error)
	PrepareEvaluatorLaunch(context.Context, TaskBoardEvaluatorLaunchRequest) (TaskBoardPreparedEvaluatorLaunch, error)
	ConfirmEvaluatorLaunch(context.Context, TaskBoardEvaluatorLaunchRequest) (TaskBoardMutation, error)
	PreviewEvaluatorEvidenceHandoff(context.Context, TaskBoardEvaluatorEvidenceHandoffPreviewRequest) (TaskBoardEvaluatorEvidenceHandoffPreview, error)
	PrepareEvaluatorEvidenceHandoff(context.Context, TaskBoardEvaluatorEvidenceHandoffRequest) (TaskBoardPreparedEvaluatorEvidenceHandoff, error)
	AdoptEvaluatorEvidenceHandoff(context.Context, TaskBoardEvaluatorEvidenceHandoffRequest) (TaskBoardMutation, error)
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
	core              *lifecycleServiceCore
	inspection        *LifecycleInspectionService
	authoring         *StandardAuthoringLaunchService
	authoringReviews  *AuthoringReviewService
	mutations         *LifecycleMutationService
	activations       *RunActivationService
	continuations     *TaskContinuationService
	authoringRecovery *AuthoringRecoveryService
	control           *ExecutionControlService
	evaluatorLaunches *CodeEdgeEvaluatorLaunchService
	evaluatorEvidence *CodeEdgeEvaluatorEvidenceHandoffService
	workerLauncher    RunWorkerHandoffLauncher
	actor             func() (string, error)
}

func newTaskBoardService(core *lifecycleServiceCore, inspection *LifecycleInspectionService, authoring *StandardAuthoringLaunchService, authoringReviews *AuthoringReviewService, mutations *LifecycleMutationService, activations *RunActivationService, continuations *TaskContinuationService, control *ExecutionControlService, authoringRecovery *AuthoringRecoveryService, evaluatorLaunches *CodeEdgeEvaluatorLaunchService, evaluatorEvidence *CodeEdgeEvaluatorEvidenceHandoffService, workerLauncher RunWorkerHandoffLauncher) *TaskBoardService {
	return &TaskBoardService{
		core:              core,
		inspection:        inspection,
		authoring:         authoring,
		authoringReviews:  authoringReviews,
		mutations:         mutations,
		activations:       activations,
		continuations:     continuations,
		authoringRecovery: authoringRecovery,
		control:           control,
		evaluatorLaunches: evaluatorLaunches,
		evaluatorEvidence: evaluatorEvidence,
		workerLauncher:    workerLauncher,
		actor:             localTaskBoardActor,
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

	start := max(int64(0), info.Size()-taskBoardLogReadLimit)
	content := make([]byte, info.Size()-start)
	count, readErr := file.ReadAt(content, start)
	if readErr != nil && readErr != io.EOF {
		log.Message = "读取日志失败: " + readErr.Error()
		return log, nil
	}
	log.Truncated = start > 0
	log.Content = strings.ToValidUTF8(string(content[:count]), "?")
	if log.Truncated {
		if lineEnd := strings.IndexByte(log.Content, '\n'); lineEnd >= 0 {
			log.Content = log.Content[lineEnd+1:]
		}
	}
	if log.Content == "" {
		log.Message = "日志文件当前为空"
	}
	return log, nil
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
		return service.retryTaskRevisionRun(ctx, prepared, actor)
	case store.WorkflowRunSubjectAuthoringSession:
		return service.recoverAuthoringRun(ctx, prepared, actor, request)
	default:
		return TaskBoardMutation{}, fmt.Errorf("Run %s has no retry contract", prepared.RunID)
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
	switch run.SubjectKind {
	case store.WorkflowRunSubjectTaskRevision:
		if service.continuations == nil {
			return TaskBoardRecoveryPreview{}, fmt.Errorf("task board continuation service is not configured")
		}
		checkpoint, err := service.continuations.CurrentCheckpoint(ctx, request.RunID)
		if err != nil {
			return TaskBoardRecoveryPreview{}, err
		}
		plan, err = service.continuations.PreviewTaskContinuation(ctx, ContinueTaskCommand{
			CommandKey: commandKey,
			TaskID:     request.TaskID,
			RunID:      request.RunID,
			Expected:   checkpoint,
			Actor:      actor,
			Reason:     request.Reason,
		})
		if err != nil {
			return TaskBoardRecoveryPreview{}, err
		}
	case store.WorkflowRunSubjectAuthoringSession:
		if service.authoringRecovery == nil {
			return TaskBoardRecoveryPreview{}, fmt.Errorf("task board authoring recovery service is not configured")
		}
		checkpoint, err := service.authoringRecovery.CurrentCheckpoint(ctx, request.RunID)
		if err != nil {
			return TaskBoardRecoveryPreview{}, err
		}
		plan, err = service.authoringRecovery.PreviewAuthoringRecovery(ctx, AuthoringRecoveryCommand{
			CommandKey: commandKey,
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

func (service *TaskBoardService) retryTaskRevisionRun(ctx context.Context, prepared preparedTaskBoardRunAction, actor string) (TaskBoardMutation, error) {
	if service.continuations == nil {
		return TaskBoardMutation{}, fmt.Errorf("task board continuation service is not configured")
	}
	checkpoint, err := service.continuations.CurrentCheckpoint(ctx, prepared.RunID)
	if err != nil {
		return TaskBoardMutation{}, err
	}
	plan, err := service.continuations.PlanTaskContinuation(ctx, ContinueTaskCommand{
		CommandKey: prepared.IdempotencyKey,
		TaskID:     prepared.TaskID,
		RunID:      prepared.RunID,
		Expected:   checkpoint,
		Actor:      actor,
		Reason:     prepared.Reason,
	})
	if err != nil {
		return TaskBoardMutation{}, err
	}
	if _, err := service.continuations.ExecuteTaskContinuation(ctx, plan.ID()); err != nil {
		return TaskBoardMutation{}, err
	}
	if err := service.FlushQueuedRuns(ctx); err != nil {
		return TaskBoardMutation{}, err
	}
	return TaskBoardMutation{TaskID: prepared.TaskID, RunID: prepared.RunID, Summary: "已为当前 Run 排队重试"}, nil
}

func (service *TaskBoardService) recoverAuthoringRun(ctx context.Context, prepared preparedTaskBoardRunAction, actor string, request TaskBoardRetryRunRequest) (TaskBoardMutation, error) {
	if service.authoringRecovery == nil {
		return TaskBoardMutation{}, fmt.Errorf("task board authoring recovery service is not configured")
	}
	if request.ExpectedRecoveryCheckpoint == nil || request.ExpectedRecoveryPlanFingerprint == "" {
		return TaskBoardMutation{}, ErrTaskBoardRecoveryPreviewRequired
	}
	checkpoint := *request.ExpectedRecoveryCheckpoint
	plan, err := service.authoringRecovery.PlanAuthoringRecovery(ctx, AuthoringRecoveryCommand{
		CommandKey: prepared.IdempotencyKey,
		RunID:      prepared.RunID,
		Expected:   checkpoint,
		Actor:      actor,
		Reason:     prepared.Reason,
	})
	if err != nil {
		if errors.Is(err, store.ErrOptimisticLock) {
			return TaskBoardMutation{}, fmt.Errorf("%w: %v", ErrTaskBoardRecoveryPreviewStale, err)
		}
		return TaskBoardMutation{}, err
	}
	semanticFingerprint, err := plan.SemanticFingerprint()
	if err != nil {
		return TaskBoardMutation{}, fmt.Errorf("fingerprint confirmed authoring recovery plan: %w", err)
	}
	if semanticFingerprint != request.ExpectedRecoveryPlanFingerprint {
		return TaskBoardMutation{}, fmt.Errorf("%w: recovery plan changed after preview", ErrTaskBoardRecoveryPreviewStale)
	}
	if _, err := service.authoringRecovery.ExecuteAuthoringRecovery(ctx, plan.ID()); err != nil {
		return TaskBoardMutation{}, err
	}
	if err := service.FlushQueuedRuns(ctx); err != nil {
		return TaskBoardMutation{}, err
	}
	return TaskBoardMutation{TaskID: prepared.TaskID, RunID: prepared.RunID, Summary: "已为当前 Standard 创题 Run 排队恢复"}, nil
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
	checkpoint, err := service.control.CurrentCheckpoint(ctx, prepared.RunID)
	if err != nil {
		return TaskBoardMutation{}, err
	}
	if _, err := service.control.Request(ctx, RequestExecutionControlRequest{
		OperationKey: prepared.IdempotencyKey,
		Action:       store.ControlActionTerminate,
		RunID:        prepared.RunID,
		Expected:     checkpoint,
		Actor:        actor,
		Reason:       prepared.Reason,
	}); err != nil {
		return TaskBoardMutation{}, err
	}
	return TaskBoardMutation{TaskID: prepared.TaskID, RunID: prepared.RunID, Summary: "已请求取消当前 Run"}, nil
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
	task.Evaluator = service.projectTaskBoardEvaluator(ctx, detail)
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
	if inspected.Run.SubjectKind == store.WorkflowRunSubjectTaskRevision && inspected.Run.Status == store.WorkflowRunWaitingContinuation && taskBoardHasNeedsRepair(inspected.Stages) {
		run.RetryStrategy = TaskBoardRetryStrategyNone
		run.CanRetry = false
		run.RetryReason = "不可变 CodeEdge 子 Run 存在确定性内容问题；请创建修复 revision"
		return run
	}
	if run.RetryStrategy == TaskBoardRetryStrategyAuthoringRecovery || run.RetryStrategy == TaskBoardRetryStrategyAuthoringAdmissionRepair {
		if service == nil || service.authoringRecovery == nil {
			run.RetryReason = "Standard 创题恢复服务未配置"
			return run
		}
		canRecover, reason, err := service.authoringRecovery.CanRecover(ctx, inspected.Run.ID)
		if err != nil {
			run.RetryReason = "无法验证 Standard 创题恢复资格: " + err.Error()
			return run
		}
		run.CanRetry, run.RetryReason = canRecover, reason
		return run
	}
	run.CanRetry, run.RetryReason = taskBoardRetryAvailability(inspected.Run)
	return run
}

func taskBoardHasNeedsRepair(stages []store.StageAttempt) bool {
	for _, stage := range stages {
		if stage.ExecutionStatus == store.StageExecutionCompleted && stage.Verdict == store.VerdictNeedsRepair {
			return true
		}
	}
	return false
}

func taskBoardRetryStrategy(run store.WorkflowRun) TaskBoardRetryStrategy {
	switch run.SubjectKind {
	case store.WorkflowRunSubjectTaskRevision:
		return TaskBoardRetryStrategyTaskContinuation
	case store.WorkflowRunSubjectAuthoringSession:
		if run.Status == store.WorkflowRunWaitingContinuation && isCurrentStandardAuthoringRun(run) {
			return TaskBoardRetryStrategyAuthoringAdmissionRepair
		}
		return TaskBoardRetryStrategyAuthoringRecovery
	default:
		return TaskBoardRetryStrategyNone
	}
}

func taskBoardRetryAvailability(run store.WorkflowRun) (bool, string) {
	if run.SubjectKind != store.WorkflowRunSubjectTaskRevision {
		return false, "Standard 创题 Run 需要专用恢复流程"
	}
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
	durableJobs := make([]store.DurableJob, 0, len(jobs))
	for _, inspected := range jobs {
		durableJobs = append(durableJobs, inspected.Job)
	}
	var selected *store.DurableJob
	for index := range jobs {
		job := jobs[index].Job
		if job.Failure == nil || (job.State != store.JobFailed && job.State != store.JobInDoubt) {
			continue
		}
		if standardAuthoringHandoffFailureResolved(job, durableJobs) {
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
		CanRedrive:     taskBoardCanRedriveAuthoringHandoff(*selected),
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
		if taskBoardCanRedriveAuthoringHandoff(job) {
			return TaskBoardFailureRecoveryRedriveAuthoringHandoff
		}
		return TaskBoardFailureRecoveryReconcile
	default:
		return TaskBoardFailureRecoveryNone
	}
}

func taskBoardCanRedriveAuthoringHandoff(job store.DurableJob) bool {
	if job.State != store.JobInDoubt || job.Failure == nil ||
		(job.CommandType != standardAuthoringHandoffCommandType && job.CommandType != standardAuthoringHandoffRedriveCommandType && job.CommandType != standardAuthoringHandoffReconcileCommandType) {
		return false
	}
	switch job.Failure.Code {
	case handoffDefinitionUnavailableCode, handoffDefinitionInvalidCode, handoffStorageUnavailableCode:
		return true
	default:
		return false
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
