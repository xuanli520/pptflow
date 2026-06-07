package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
)

func (r Runner) markRunCrashed(ctx context.Context, run model.RunRecord, start time.Time, reason string) error {
	return r.crashRun(ctx, run, start, nil, RuntimeState{}, false, true, reason)
}

func (r Runner) crashRun(ctx context.Context, run model.RunRecord, start time.Time, stages []model.StageRecord, runtime RuntimeState, keepRuntime bool, runtimeCleanupDone bool, reason string) error {
	return r.finishTerminalRun(ctx, terminalRunRequest{
		Run:                run,
		Started:            start,
		Stages:             stages,
		Runtime:            runtime,
		KeepRuntime:        keepRuntime,
		RuntimeCleanupDone: runtimeCleanupDone,
		Status:             model.RunCrashed,
		SummaryFile:        "crash_summary.json",
		CleanupReason:      "crash",
		DefaultReason:      "pipeline exited before finishing the run",
		Reason:             reason,
	})
}

func (r Runner) abortRun(ctx context.Context, run model.RunRecord, start time.Time, stages []model.StageRecord, runtime RuntimeState, keepRuntime bool, runtimeCleanupDone bool, reason string) error {
	return r.finishTerminalRun(ctx, terminalRunRequest{
		Run:                run,
		Started:            start,
		Stages:             stages,
		Runtime:            runtime,
		KeepRuntime:        keepRuntime,
		RuntimeCleanupDone: runtimeCleanupDone,
		Status:             model.RunAborted,
		SummaryFile:        "abort_summary.json",
		CleanupReason:      "abort",
		DefaultReason:      "pipeline aborted before finishing the run",
		Reason:             reason,
	})
}

type terminalRunRequest struct {
	Run                model.RunRecord
	Started            time.Time
	Stages             []model.StageRecord
	Runtime            RuntimeState
	KeepRuntime        bool
	RuntimeCleanupDone bool
	Status             string
	SummaryFile        string
	CleanupReason      string
	DefaultReason      string
	Reason             string
}

func (r Runner) finishTerminalRun(ctx context.Context, request terminalRunRequest) error {
	if ctx == nil {
		ctx = context.Background()
	}
	reason := request.Reason
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = strings.TrimSpace(request.DefaultReason)
	}
	if reason == "" {
		reason = "pipeline exited before finishing the run"
	}
	status := strings.TrimSpace(request.Status)
	if status == "" {
		status = model.RunCrashed
	}
	summaryFile := strings.TrimSpace(request.SummaryFile)
	if summaryFile == "" {
		summaryFile = "terminal_summary.json"
	}
	cleanupReason := strings.TrimSpace(request.CleanupReason)
	if cleanupReason == "" {
		cleanupReason = status
	}
	saveErrors := []error{}
	cleanupStatus := "not_applicable"
	if request.RuntimeCleanupDone {
		cleanupStatus = "already_done"
	} else if runtimeStageWasSelected(request.Stages) {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		cleanup := r.finalizeRuntime(cleanupCtx, request.Run, request.Stages, request.Runtime, request.KeepRuntime, cleanupReason)
		cleanupCancel()
		cleanupStatus = cleanup.Summary.Status
		saveErrors = append(saveErrors, cleanup.PersistErrors...)
	}
	if err := NewArtifactWriter(request.Run.ArtifactRoot).RequiredJSON(summaryFile, map[string]any{
		"run_id":         request.Run.RunID,
		"task_id":        request.Run.TaskID,
		"status":         status,
		"reason":         reason,
		"save_errors":    cleanupPersistErrorStrings(cleanupOutcome{PersistErrors: saveErrors}),
		"cleanup_status": cleanupStatus,
		"recorded_at":    time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		saveErrors = append(saveErrors, err)
	}
	saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.store.FinishRun(saveCtx, request.Run.RunID, request.Run.TaskID, status, time.Since(request.Started)); err != nil {
		saveErrors = append(saveErrors, fmt.Errorf("finish %s run: %w", status, err))
	}
	return errors.Join(saveErrors...)
}

func (r Runner) finishAbortedRun(abortErr error, run *model.RunRecord, start time.Time, stages []model.StageRecord, runtime RuntimeState, keepRuntime bool, runtimeCleanupDone *bool, runFinished *bool, progress func(RunProgress)) (Result, error) {
	if abortErr == nil {
		abortErr = context.Canceled
	}
	stages = markInFlightStageAborted(stages, abortErr)
	saveErrors := []string{}

	saveCtx, saveCancel := context.WithTimeout(context.Background(), 5*time.Second)
	for index := range stages {
		stages[index].Findings = assignMissingFindingIDs(stages[index].Stage, stages[index].Findings)
		if err := r.store.PutStage(saveCtx, run.RunID, stages[index]); err != nil {
			saveErrors = append(saveErrors, fmt.Sprintf("put stage %s: %s", stages[index].Stage, err))
		}
		if len(stages[index].Findings) > 0 {
			if err := r.store.InsertFindings(saveCtx, run.RunID, stages[index].Findings); err != nil {
				saveErrors = append(saveErrors, fmt.Sprintf("insert findings %s: %s", stages[index].Stage, err))
			}
		}
	}
	saveCancel()
	if err := r.writeStageStatus(run.RunID, run.ArtifactRoot, stages); err != nil {
		saveErrors = append(saveErrors, "write stage_status.json: "+err.Error())
	}

	cleanupStatus := "not_applicable"
	if runtimeCleanupDone != nil && *runtimeCleanupDone {
		cleanupStatus = "already_done"
	} else if runtimeStageWasSelected(stages) {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		cleanup := r.finalizeRuntime(cleanupCtx, *run, stages, runtime, keepRuntime, "abort")
		cleanupCancel()
		cleanupStatus = cleanup.Summary.Status
		saveErrors = append(saveErrors, cleanupPersistErrorStrings(cleanup)...)
		if runtimeCleanupDone != nil {
			*runtimeCleanupDone = cleanup.RuntimeCleanupDone
		}
	}

	finishCtx, finishCancel := context.WithTimeout(context.Background(), 5*time.Second)
	terminalPersisted := true
	if err := r.store.FinishRun(finishCtx, run.RunID, run.TaskID, model.RunAborted, time.Since(start)); err != nil {
		terminalPersisted = false
		saveErrors = append(saveErrors, "finish aborted run: "+err.Error())
	}
	finishCancel()
	if terminalPersisted && runFinished != nil {
		*runFinished = true
	}

	run.Status = model.RunAborted
	run.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	run.DurationMS = time.Since(start).Milliseconds()
	if err := NewArtifactWriter(run.ArtifactRoot).RequiredJSON("abort_summary.json", map[string]any{
		"run_id":         run.RunID,
		"task_id":        run.TaskID,
		"status":         model.RunAborted,
		"reason":         abortErr.Error(),
		"save_errors":    saveErrors,
		"cleanup_status": cleanupStatus,
		"recorded_at":    time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		saveErrors = append(saveErrors, "write abort_summary.json: "+err.Error())
	}
	if len(saveErrors) > 0 {
		return Result{Run: *run, Stages: stages}, fmt.Errorf("%w; abort persistence errors: %s", abortErr, strings.Join(saveErrors, "; "))
	}
	progress(RunProgress{RunID: run.RunID, Event: EventRunAborted, Done: true, Err: abortErr})
	return Result{Run: *run, Stages: stages}, abortErr
}

func markInFlightStageAborted(stages []model.StageRecord, abortErr error) []model.StageRecord {
	now := time.Now().UTC()
	reason := "pipeline aborted"
	if abortErr != nil && strings.TrimSpace(abortErr.Error()) != "" {
		reason += ": " + abortErr.Error()
	}
	for index := range stages {
		if stages[index].Status != model.StageRunning {
			continue
		}
		stages[index].Status = model.StageFailed
		stages[index].FinishedAt = now.Format(time.RFC3339)
		if started, err := time.Parse(time.RFC3339, stages[index].StartedAt); err == nil {
			stages[index].DurationMS = now.Sub(started).Milliseconds()
		}
		if stages[index].ArtifactPaths == nil {
			stages[index].ArtifactPaths = []string{}
		}
		stages[index].ErrorSummary = reason
	}
	return stages
}
