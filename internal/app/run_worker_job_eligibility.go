package app

import (
	"context"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

// runWorkerEligibleCommandTypes defines the only queued jobs a controlled
// worker may claim while its Run is outside the ordinary execution states.
// A nil slice with runnable=true means the normal execution states may claim
// any frozen Run-scoped job. Keeping this policy at the application boundary
// prevents a stale stage job from being selected merely because a terminal
// Run has a separate repair or reconciliation coordinator waiting.
func runWorkerEligibleCommandTypes(status store.WorkflowRunStatus) ([]string, bool) {
	switch status {
	case store.WorkflowRunQueued, store.WorkflowRunRunning, store.WorkflowRunPauseRequested, store.WorkflowRunPausing,
		store.WorkflowRunResumeRequested, store.WorkflowRunCancelRequested, store.WorkflowRunStopRequested, store.WorkflowRunCanceling:
		return nil, true
	case store.WorkflowRunWaitingReview:
		return []string{store.ReviewGateResolutionCommandType, store.AuthoringReviewGateResolutionCommandType}, true
	case store.WorkflowRunWaitingContinuation:
		return []string{"task_continuation.execute", repairSessionAdvanceCommandType}, true
	case store.WorkflowRunPaused, store.WorkflowRunFailedRecoverable:
		return []string{"task_continuation.execute"}, true
	case store.WorkflowRunInDoubt:
		return []string{codeEdgeEvaluatorReconciliationCommandType, repairSessionAdvanceCommandType, standardAuthoringHandoffRedriveCommandType}, true
	case store.WorkflowRunSucceeded, store.WorkflowRunFailedTerminal:
		return []string{repairSessionAdvanceCommandType}, true
	default:
		return nil, false
	}
}

func runWorkerJobIsEligible(status store.WorkflowRunStatus, job store.DurableJob) bool {
	if job.State != store.JobQueued {
		return false
	}
	allowed, runnable := runWorkerEligibleCommandTypes(status)
	if !runnable {
		return false
	}
	if len(allowed) == 0 {
		return true
	}
	for _, commandType := range allowed {
		if job.CommandType == commandType {
			return true
		}
	}
	return false
}

func (session *RunWorkerSession) hasEligibleQueuedDurableJob(ctx context.Context, runID string, status store.WorkflowRunStatus) (bool, error) {
	if session == nil || session.services == nil || session.services.core == nil || session.services.core.store == nil {
		return false, ErrRunWorkerConfiguration
	}
	if _, runnable := runWorkerEligibleCommandTypes(status); !runnable {
		return false, nil
	}
	jobs, err := session.services.core.store.ListDurableJobsForRun(ctx, runID)
	if err != nil {
		return false, err
	}
	for _, job := range jobs {
		if runWorkerJobIsEligible(status, job) {
			return true, nil
		}
	}
	return false, nil
}

func (session *RunWorkerSession) eligibleQueuedRunWork(ctx context.Context, run store.WorkflowRun) ([]string, bool, error) {
	allowed, runnable := runWorkerEligibleCommandTypes(run.Status)
	if !runnable {
		return nil, false, nil
	}
	// Ordinary running states retain the existing worker-poll behavior. Special
	// wait/terminal states require a matching queued coordinator before the
	// child stays alive.
	if len(allowed) == 0 {
		return nil, true, nil
	}
	hasQueued, err := session.hasEligibleQueuedDurableJob(ctx, run.ID, run.Status)
	if err != nil {
		return nil, false, err
	}
	return allowed, hasQueued, nil
}
