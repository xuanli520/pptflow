package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/user"
	"sort"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
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
}

// TaskBoardRun is the compact, presentation-neutral run history shown from a
// task detail. It contains durable facts only and never exposes a direct
// filesystem capability to the terminal adapter.
type TaskBoardRun struct {
	ID            string
	Status        string
	CurrentStage  string
	FailureStage  string
	FailureClass  string
	FailureReason string
	CreatedAt     time.Time
	StartedAt     *time.Time
	FinishedAt    *time.Time
	LogPath       string
	HasLog        bool
	CanRetry      bool
	RetryReason   string
}

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
	Tasks              []TaskBoardTask
	AuthoringAvailable bool
}

// TaskBoardStartAuthoringRequest contains the caller-selected immutable task
// identity, source coordinate, audit reason, and client command key. The
// service derives the actor and validates the full lifecycle protocol.
type TaskBoardStartAuthoringRequest struct {
	IdempotencyKey string
	RepositoryURL  string
	CommitSHA      string
	Slug           string
	Title          string
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

// TaskBoardRetryRunRequest resumes the selected Run through the existing
// continuation service. Its key is retained by the TUI if a request needs to
// be retried after an infrastructure failure.
type TaskBoardRetryRunRequest struct {
	IdempotencyKey string
	TaskID         string
	RunID          string
	Reason         string
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
	TaskID  string
	RunID   string
	Summary string
}

// TaskBoardGateway is the complete application boundary used by the TUI. It
// keeps presentation code away from Store and managed filesystem operations.
type TaskBoardGateway interface {
	NewIdempotencyKey() (string, error)
	List(context.Context) (TaskBoardSnapshot, error)
	StartAuthoring(context.Context, TaskBoardStartAuthoringRequest) (TaskBoardMutation, error)
	DecideReview(context.Context, TaskBoardDecideReviewRequest) (TaskBoardMutation, error)
	ReadRunLog(context.Context, TaskBoardReadRunLogRequest) (TaskBoardLog, error)
	RetryRun(context.Context, TaskBoardRetryRunRequest) (TaskBoardMutation, error)
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
	control          *ExecutionControlService
	actor            func() (string, error)
}

func newTaskBoardService(core *lifecycleServiceCore, inspection *LifecycleInspectionService, authoring *StandardAuthoringLaunchService, authoringReviews *AuthoringReviewService, mutations *LifecycleMutationService, activations *RunActivationService, continuations *TaskContinuationService, control *ExecutionControlService) *TaskBoardService {
	return &TaskBoardService{
		core:             core,
		inspection:       inspection,
		authoring:        authoring,
		authoringReviews: authoringReviews,
		mutations:        mutations,
		activations:      activations,
		continuations:    continuations,
		control:          control,
		actor:            localTaskBoardActor,
	}
}

// List projects every non-deleted task through LifecycleInspectionService.
// The TUI therefore never opens SQLite or reconstructs review/run ownership.
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
		Tasks:              make([]TaskBoardTask, 0, len(tasks)),
		AuthoringAvailable: service.authoring != nil && service.authoring.Available(),
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
		Slug:          request.Slug,
		Title:         request.Title,
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

// RetryRun plans and executes the existing no-content continuation flow for
// the selected Run. The continuation service remains the authority for which
// stages are safe to retry and rejects content-changing or operator-only work.
func (service *TaskBoardService) RetryRun(ctx context.Context, request TaskBoardRetryRunRequest) (TaskBoardMutation, error) {
	if service == nil || service.continuations == nil {
		return TaskBoardMutation{}, fmt.Errorf("task board continuation service is not configured")
	}
	prepared, actor, err := service.prepareTaskBoardRunAction(ctx, request.IdempotencyKey, request.TaskID, request.RunID, request.Reason)
	if err != nil {
		return TaskBoardMutation{}, err
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
		task.Runs = append(task.Runs, projectTaskBoardRun(inspected, handoffs))
	}
	latest := task.Runs[0]
	task.RunID = latest.ID
	task.RunStatus = latest.Status
	task.CurrentStage = latest.CurrentStage
	if task.Review != nil {
		task.Column = TaskBoardPending
		return task, nil
	}
	task.Column = taskBoardColumnForRun(detail.Runs[0].Run.Status)
	return task, nil
}

func projectTaskBoardRun(inspected RunInspection, handoffs []store.RunWorkerHandoff) TaskBoardRun {
	logPath := taskBoardLogPath(handoffs)
	run := TaskBoardRun{
		ID:           inspected.Run.ID,
		Status:       string(inspected.Run.Status),
		CurrentStage: taskBoardCurrentStage(inspected.Stages),
		CreatedAt:    inspected.Run.CreatedAt,
		StartedAt:    inspected.Run.StartedAt,
		FinishedAt:   inspected.Run.FinishedAt,
		LogPath:      logPath,
		HasLog:       logPath != "",
	}
	run.FailureStage, run.FailureClass, run.FailureReason = taskBoardFailure(inspected.Stages)
	if run.FailureReason == "" {
		run.FailureReason = taskBoardHandoffFailure(handoffs)
	}
	run.CanRetry, run.RetryReason = taskBoardRetryAvailability(inspected.Run)
	return run
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

func taskBoardFailure(stages []store.StageAttempt) (stageKey, failureClass, reason string) {
	for index := len(stages) - 1; index >= 0; index-- {
		stage := stages[index]
		if strings.TrimSpace(stage.ErrorText) == "" && strings.TrimSpace(stage.FailureClass) == "" {
			continue
		}
		return stage.StageKey, stage.FailureClass, stage.ErrorText
	}
	return "", "", ""
}

func taskBoardHandoffFailure(handoffs []store.RunWorkerHandoff) string {
	for index := len(handoffs) - 1; index >= 0; index-- {
		if reason := strings.TrimSpace(handoffs[index].FailureReason); reason != "" {
			return reason
		}
	}
	return ""
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
		case store.StageExecutionRunning, store.StageExecutionWaiting, store.StageExecutionQueued, store.StageExecutionReconciling:
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
