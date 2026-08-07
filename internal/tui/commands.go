package tui

import (
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/purplevoid/harbor-factory/internal/app"
)

// This file holds every tea.Cmd that reaches the application layer. The model
// itself performs no I/O: it records an intent, returns one of these commands,
// and interprets the resulting message. Each command re-reads nothing from the
// model beyond the values captured when it was built, so a command can never
// observe a half-applied state transition.

func (m appModel) pollTick() tea.Cmd {
	return tea.Tick(pollInterval, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m appModel) refreshTasks(epoch uint64) tea.Cmd {
	return func() tea.Msg {
		if m.gateway == nil {
			return taskBoardLoadedMsg{epoch: epoch, err: errGatewayUnconfigured()}
		}
		snapshot, err := m.gateway.List(m.ctx)
		return taskBoardLoadedMsg{snapshot: snapshot, epoch: epoch, err: err}
	}
}

func (m appModel) activateQueuedRuns() tea.Cmd {
	return func() tea.Msg {
		if m.gateway == nil {
			return taskBoardActivationMsg{err: errGatewayUnconfigured()}
		}
		return taskBoardActivationMsg{err: m.gateway.FlushQueuedRuns(m.ctx)}
	}
}

func (m appModel) startAuthoring(pending pendingTaskBoardStart) tea.Cmd {
	return func() tea.Msg {
		if m.gateway == nil {
			return taskBoardMutationMsg{kind: taskBoardStartMutation, err: errGatewayUnconfigured()}
		}
		mutation, err := m.gateway.StartAuthoring(m.ctx, app.TaskBoardStartAuthoringRequest{
			IdempotencyKey: pending.key,
			RepositoryURL:  pending.message.RepoURL,
			CommitSHA:      pending.message.CommitSHA,
			BaseImage:      pending.message.BaseImage,
			Slug:           pending.message.Slug,
			Title:          pending.message.Title,
			TaskType:       pending.message.TaskType,
			Application:    pending.message.Application,
			CodeLang:       pending.message.CodeLang,
			Is0To1:         pending.message.Is0To1,
			Objective:      pending.message.Objective,
			MetadataJSON:   "{}",
			Reason:         pending.message.Reason,
		})
		return taskBoardMutationMsg{kind: taskBoardStartMutation, mutation: mutation, err: err}
	}
}

func (m appModel) loadTaskConfig(path string) tea.Cmd {
	return func() tea.Msg {
		config, err := readTaskInputConfigFile(path)
		return TaskConfigLoadedMsg{Path: path, Config: config, Err: err}
	}
}

func (m appModel) decideReview(pending pendingTaskBoardReview) tea.Cmd {
	return func() tea.Msg {
		if m.gateway == nil {
			return taskBoardMutationMsg{kind: taskBoardReviewMutation, err: errGatewayUnconfigured()}
		}
		mutation, err := m.gateway.DecideReview(m.ctx, app.TaskBoardDecideReviewRequest{
			IdempotencyKey: pending.key,
			TaskID:         pending.taskID,
			Review:         pending.review,
			Decision:       pending.decision,
			Reason:         pending.reason,
		})
		return taskBoardMutationMsg{kind: taskBoardReviewMutation, mutation: mutation, err: err}
	}
}

// inspectReview reads the full gate identity, its pending artifacts, prior
// decisions, and the agent finding bodies. It is a read-only projection: the
// decision itself still goes through DecideReview with its own checkpoint.
func (m appModel) inspectReview(taskID string, review app.TaskBoardReview, epoch uint64) tea.Cmd {
	return func() tea.Msg {
		message := taskBoardReviewInspectionMsg{epoch: epoch, taskID: taskID, requestID: review.RequestID}
		if m.gateway == nil {
			message.err = errGatewayUnconfigured()
			return message
		}
		message.inspection, message.err = m.gateway.InspectReview(m.ctx, app.TaskBoardInspectReviewRequest{
			TaskID: taskID, Review: review,
		})
		return message
	}
}

func (m appModel) previewRunRecovery(taskID, runID, reason string, epoch uint64) tea.Cmd {
	return func() tea.Msg {
		if m.gateway == nil {
			return taskBoardRecoveryPreviewMsg{epoch: epoch, taskID: taskID, runID: runID, reason: reason, err: errGatewayUnconfigured()}
		}
		ctx, cancel := context.WithTimeout(m.commandContext(), recoveryPreviewTimeout)
		defer cancel()
		preview, err := m.gateway.PreviewRunRecovery(ctx, app.TaskBoardPreviewRunRecoveryRequest{
			TaskID: taskID, RunID: runID, Reason: reason,
		})
		return taskBoardRecoveryPreviewMsg{preview: preview, epoch: epoch, taskID: taskID, runID: runID, reason: reason, err: err}
	}
}

func (m appModel) previewStandardProtocolRetry(taskID, runID, stageID, reason string, epoch uint64) tea.Cmd {
	return func() tea.Msg {
		if m.gateway == nil {
			return taskBoardProtocolRetryPreviewMsg{epoch: epoch, taskID: taskID, runID: runID, stageID: stageID, reason: reason, err: errGatewayUnconfigured()}
		}
		ctx, cancel := context.WithTimeout(m.commandContext(), recoveryPreviewTimeout)
		defer cancel()
		preview, err := m.gateway.PreviewStandardProtocolRetry(ctx, app.TaskBoardPreviewStandardProtocolRetryRequest{
			TaskID: taskID, RunID: runID, StageAttemptID: stageID, Reason: reason,
		})
		return taskBoardProtocolRetryPreviewMsg{preview: preview, epoch: epoch, taskID: taskID, runID: runID, stageID: stageID, reason: reason, err: err}
	}
}

func (m appModel) prepareStandardProtocolRetry(preview app.TaskBoardStandardProtocolRetryPreview, reason string, epoch uint64) tea.Cmd {
	return func() tea.Msg {
		if m.gateway == nil {
			return taskBoardProtocolRetryPreparedMsg{epoch: epoch, taskID: preview.TaskID, runID: preview.RunID, stageID: preview.Source.StageAttemptID, reason: reason, err: errGatewayUnconfigured()}
		}
		prepared, err := m.gateway.PrepareStandardProtocolRetry(m.ctx, app.TaskBoardPrepareStandardProtocolRetryRequest{
			TaskBoardPreviewStandardProtocolRetryRequest: app.TaskBoardPreviewStandardProtocolRetryRequest{
				TaskID: preview.TaskID, RunID: preview.RunID, StageAttemptID: preview.Source.StageAttemptID, Reason: reason,
			},
			Expected: preview.Checkpoint,
		})
		return taskBoardProtocolRetryPreparedMsg{prepared: prepared, epoch: epoch, taskID: preview.TaskID, runID: preview.RunID, stageID: preview.Source.StageAttemptID, reason: reason, err: err}
	}
}

func (m appModel) runAction(pending pendingTaskBoardRunAction) tea.Cmd {
	return func() tea.Msg {
		kind := mutationKindForAction(pending.kind)
		if m.gateway == nil {
			return taskBoardMutationMsg{kind: kind, err: errGatewayUnconfigured()}
		}
		switch pending.kind {
		case taskBoardRetryAction:
			request := app.TaskBoardRetryRunRequest{
				IdempotencyKey: pending.key, TaskID: pending.taskID, RunID: pending.runID, Reason: pending.reason,
			}
			if pending.recoveryPreview != nil {
				checkpoint := pending.recoveryPreview.Checkpoint
				request.ExpectedRecoveryCheckpoint = &checkpoint
				request.ExpectedRecoveryPlanFingerprint = pending.recoveryPreview.SemanticPlanFingerprint
			}
			if pending.protocolRetry != nil {
				checkpoint := pending.protocolRetry.Checkpoint
				request.ExpectedStandardProtocolRetry = &checkpoint
			}
			mutation, err := m.gateway.RetryRun(m.ctx, request)
			return taskBoardMutationMsg{kind: taskBoardRetryMutation, mutation: mutation, err: err}
		case taskBoardRetryAuthoringLaunchAction:
			mutation, err := m.gateway.RetryAuthoringLaunch(m.ctx, app.TaskBoardRetryAuthoringLaunchRequest{OperationID: pending.operationID})
			return taskBoardMutationMsg{kind: taskBoardRetryAuthoringLaunchMutation, mutation: mutation, err: err}
		case taskBoardCancelAction:
			mutation, err := m.gateway.CancelRun(m.ctx, app.TaskBoardCancelRunRequest{
				IdempotencyKey: pending.key, TaskID: pending.taskID, RunID: pending.runID, Reason: pending.reason,
			})
			return taskBoardMutationMsg{kind: taskBoardCancelMutation, mutation: mutation, err: err}
		default:
			return taskBoardMutationMsg{err: fmt.Errorf("unsupported task board Run action %q", pending.kind)}
		}
	}
}

func (m appModel) readRunLog(taskID, runID string, epoch uint64) tea.Cmd {
	return func() tea.Msg {
		if m.gateway == nil {
			return taskBoardLogMsg{epoch: epoch, err: errGatewayUnconfigured()}
		}
		log, err := m.gateway.ReadRunLog(m.ctx, app.TaskBoardReadRunLogRequest{TaskID: taskID, RunID: runID})
		return taskBoardLogMsg{log: log, epoch: epoch, err: err}
	}
}

func (m appModel) flushBeforeExit() tea.Cmd {
	return func() tea.Msg {
		if m.gateway == nil {
			return taskBoardExitMsg{err: errGatewayUnconfigured()}
		}
		return taskBoardExitMsg{err: m.gateway.FlushQueuedRuns(m.ctx)}
	}
}

func (m appModel) newIdempotencyKey() (string, error) {
	if m.gateway == nil {
		return "", errGatewayUnconfigured()
	}
	return m.gateway.NewIdempotencyKey()
}

// commandContext guarantees a non-nil parent for a timeout-bounded read.
func (m appModel) commandContext() context.Context {
	if m.ctx == nil {
		return context.Background()
	}
	return m.ctx
}

func mutationKindForAction(kind taskBoardRunActionKind) taskBoardMutationKind {
	switch kind {
	case taskBoardCancelAction:
		return taskBoardCancelMutation
	case taskBoardRetryAuthoringLaunchAction:
		return taskBoardRetryAuthoringLaunchMutation
	default:
		return taskBoardRetryMutation
	}
}

func errGatewayUnconfigured() error {
	return fmt.Errorf("task board service is not configured")
}
