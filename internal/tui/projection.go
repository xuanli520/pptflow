package tui

import (
	"github.com/purplevoid/harbor-factory/internal/app"
)

// This file is the only place a board snapshot becomes terminal state.
// Isolating the projection makes one rule checkable by reading a single file:
// the TUI copies exactly the fields it renders and nothing else. A durable
// record therefore cannot leak raw provider output or a credential onto a
// screen merely by being present in the application-layer projection.

// taskItemForID finds the board projection of one Task across all columns.
// It is used to refresh the open detail pane in place after a snapshot load.
func taskItemForID(pending, running, completed []TaskItem, id string) *TaskItem {
	for _, column := range [][]TaskItem{pending, running, completed} {
		for index := range column {
			if column[index].ID == id {
				item := column[index]
				return &item
			}
		}
	}
	return nil
}

func taskItemsForSnapshot(snapshot app.TaskBoardSnapshot) (pending, running, completed []TaskItem) {
	for _, launch := range snapshot.PendingAuthoringLaunches {
		copy := launch
		pending = append(pending, TaskItem{
			ID:              launch.OperationID,
			Slug:            launch.Slug,
			Name:            launch.Title,
			RepoURL:         launch.RepositoryURL,
			CommitSHA:       launch.CommitSHA,
			State:           TaskPending,
			RunStatus:       launch.Status,
			Lifecycle:       "source_capture_failed",
			AuthoringLaunch: &copy,
		})
	}
	for _, task := range snapshot.Tasks {
		item := TaskItem{
			ID:           task.ID,
			Slug:         task.Slug,
			Name:         task.Title,
			RepoURL:      task.RepositoryURL,
			CommitSHA:    task.CommitSHA,
			RunID:        task.RunID,
			CurrentStage: task.CurrentStage,
			OperatorSummary: cloneTaskBoardOperatorSummaryForTUI(
				task.OperatorSummary,
			),
			RunStatus:   task.RunStatus,
			Lifecycle:   task.LifecycleState,
			Review:      task.Review,
			OpenReviews: task.OpenReviewCount,
			Runs:        make([]TaskRunItem, 0, len(task.Runs)),
		}
		for _, run := range task.Runs {
			var authoringEvidence *app.TaskBoardAuthoringEvidence
			if run.AuthoringEvidence != nil {
				copy := *run.AuthoringEvidence
				copy.Claims = append([]app.TaskBoardAuthoringClaim(nil), run.AuthoringEvidence.Claims...)
				copy.Lineage = append([]app.TaskBoardAuthoringArtifact(nil), run.AuthoringEvidence.Lineage...)
				authoringEvidence = &copy
			}
			var standardProtocolRetry *app.TaskBoardStandardProtocolRetry
			if run.StandardProtocolRetry != nil {
				copy := *run.StandardProtocolRetry
				standardProtocolRetry = &copy
			}
			item.Runs = append(item.Runs, TaskRunItem{
				ID:                    run.ID,
				AuthoringEvidence:     authoringEvidence,
				AgentTurnTranscripts:  append([]app.TaskBoardAgentTranscript(nil), run.AgentTurnTranscripts...),
				Status:                run.Status,
				CurrentStage:          run.CurrentStage,
				OperatorSummary:       cloneTaskBoardOperatorSummaryForTUI(run.OperatorSummary),
				FailureStage:          run.FailureStage,
				FailureCode:           run.FailureCode,
				FailureSummary:        run.FailureSummary,
				FailureJobID:          run.FailureJobID,
				FailureArtifactID:     run.FailureArtifactID,
				FailureRecordedAt:     run.FailureRecordedAt,
				FailureRecoveryAction: run.FailureRecoveryAction,
				CreatedAt:             run.CreatedAt,
				StartedAt:             run.StartedAt,
				FinishedAt:            run.FinishedAt,
				LogPath:               run.LogPath,
				CanRetry:              run.CanRetry,
				RetryReason:           run.RetryReason,
				RetryStrategy:         run.RetryStrategy,
				StandardProtocolRetry: standardProtocolRetry,
			})
		}
		switch task.Column {
		case app.TaskBoardRunning:
			item.State = TaskRunning
			running = append(running, item)
		case app.TaskBoardCompleted:
			item.State = TaskCompleted
			completed = append(completed, item)
		default:
			item.State = TaskPending
			pending = append(pending, item)
		}
	}
	return pending, running, completed
}

func cloneTaskBoardOperatorSummaryForTUI(summary *app.TaskBoardOperatorSummary) *app.TaskBoardOperatorSummary {
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
