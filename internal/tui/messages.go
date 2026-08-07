package tui

import (
	"time"

	"github.com/purplevoid/harbor-factory/internal/app"
)

// This file holds every message the task-board TUI can receive and the small
// pending-intent records that outlive a single Update. Keeping them apart from
// the model and its commands makes the async surface auditable in one place:
// each message carries the epoch it was issued under so a late reply from a
// superseded request is discarded instead of overwriting fresh state.

type taskBoardGateway = app.TaskBoardGateway

type taskBoardLoadedMsg struct {
	snapshot app.TaskBoardSnapshot
	epoch    uint64
	err      error
}

// taskBoardActivationMsg keeps queued-run activation off the board refresh
// path so a busy durable outbox cannot prevent the operator from seeing or
// deciding an already-open review.
type taskBoardActivationMsg struct {
	err error
}

type taskBoardMutationMsg struct {
	kind     taskBoardMutationKind
	mutation app.TaskBoardMutation
	err      error
}

type taskBoardRecoveryPreviewMsg struct {
	preview app.TaskBoardRecoveryPreview
	epoch   uint64
	taskID  string
	runID   string
	reason  string
	err     error
}

type taskBoardProtocolRetryPreviewMsg struct {
	preview app.TaskBoardStandardProtocolRetryPreview
	epoch   uint64
	taskID  string
	runID   string
	stageID string
	reason  string
	err     error
}

type taskBoardProtocolRetryPreparedMsg struct {
	prepared app.TaskBoardPreparedStandardProtocolRetry
	epoch    uint64
	taskID   string
	runID    string
	stageID  string
	reason   string
	err      error
}

type taskBoardLogMsg struct {
	log   app.TaskBoardLog
	epoch uint64
	err   error
}

// taskBoardReviewInspectionMsg carries the review-gate inspection that backs
// the dedicated review screen, including the agent finding bodies read from
// the Run's own critic stage attempts.
type taskBoardReviewInspectionMsg struct {
	inspection app.TaskBoardReviewInspection
	epoch      uint64
	taskID     string
	requestID  string
	err        error
}

type taskBoardExitMsg struct{ err error }

type tickMsg struct{}

type taskBoardMutationKind string

const (
	taskBoardStartMutation                taskBoardMutationKind = "start_authoring"
	taskBoardReviewMutation               taskBoardMutationKind = "review"
	taskBoardRetryMutation                taskBoardMutationKind = "retry_run"
	taskBoardRetryAuthoringLaunchMutation taskBoardMutationKind = "retry_authoring_launch"
	taskBoardCancelMutation               taskBoardMutationKind = "cancel_run"
)

type taskBoardRunActionKind string

const (
	taskBoardRetryAction                taskBoardRunActionKind = "retry"
	taskBoardRetryAuthoringLaunchAction taskBoardRunActionKind = "retry_authoring_launch"
	taskBoardCancelAction               taskBoardRunActionKind = "cancel"
)

// recoveryPreviewTimeout bounds a non-durable plan read so the terminal cannot
// hang on a busy control plane while an operator waits to confirm recovery.
const recoveryPreviewTimeout = 15 * time.Second

// pollInterval is how often an idle board asks for a fresh projection.
const pollInterval = 2 * time.Second

type pendingTaskBoardStart struct {
	message TaskSubmitMsg
	key     string
}

type pendingTaskBoardReview struct {
	taskID   string
	review   app.TaskBoardReview
	decision app.TaskBoardReviewDecision
	reason   string
	key      string
}

type pendingTaskBoardRunAction struct {
	kind            taskBoardRunActionKind
	operationID     string
	taskID          string
	runID           string
	reason          string
	key             string
	recoveryPreview *app.TaskBoardRecoveryPreview
	protocolRetry   *app.TaskBoardPreparedStandardProtocolRetry
}

func isRetryAction(kind taskBoardRunActionKind) bool {
	return kind == taskBoardRetryAction || kind == taskBoardRetryAuthoringLaunchAction
}

func requiresRecoveryPreview(strategy app.TaskBoardRetryStrategy) bool {
	return strategy == app.TaskBoardRetryStrategyTaskContinuation
}

func requiresProtocolRetryPreview(strategy app.TaskBoardRetryStrategy) bool {
	return strategy == app.TaskBoardRetryStrategyStandardProtocolStage
}
