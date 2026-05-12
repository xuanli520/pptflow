package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/xuanli520/p2r_tui/assets"
	"github.com/xuanli520/p2r_tui/internal/codex/appserver"
	"github.com/xuanli520/p2r_tui/internal/db"
	"github.com/xuanli520/p2r_tui/internal/displaytime"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/preflight"
	"github.com/xuanli520/p2r_tui/internal/scanner"
	"github.com/xuanli520/p2r_tui/internal/taskdocs"
)

type runState struct {
	runner   Runner
	ctx      context.Context
	taskID   string
	opts     RunOptions
	progress func(RunProgress)

	project      scanner.Project
	pathWarnings []ProjectPathWarning
	previousRuns []model.RunRecord

	start        time.Time
	runID        string
	artifactRoot string
	writer       ArtifactWriter

	released        []assets.ReleasedFile
	releaseErr      error
	importedDocs    []taskdocs.Document
	docsManifest    taskdocs.Manifest
	docsImportErr   error
	docsManifestErr error

	keepRuntime bool

	run                model.RunRecord
	stages             []model.StageRecord
	results            map[string]model.StageRecord
	runtime            RuntimeState
	runtimeCleanupDone bool
	cleanupFailed      bool
	runCreated         bool
	runFinished        bool
	artifactWarnings   []ArtifactWarning
}

func makeRunProgress(taskID string, reporter ProgressReporter) func(RunProgress) {
	return func(update RunProgress) {
		if reporter == nil {
			return
		}
		if update.TaskID == "" {
			update.TaskID = taskID
		}
		reporter(update)
	}
}

func codexDeltaProgress(runID, stage string, progress func(RunProgress)) func(appserver.Update) {
	if progress == nil {
		return nil
	}
	return func(update appserver.Update) {
		progress(RunProgress{
			RunID: runID,
			Stage: stage,
			Event: EventStageStream,
			Stream: &StreamUpdate{
				Stage:     stage,
				Mode:      StreamModeCumulative,
				ItemID:    update.ItemID,
				Text:      update.Text,
				Delta:     update.Delta,
				Done:      update.Done,
				Truncated: update.Truncated,
			},
		})
	}
}

func appendStreamProgress(runID, stage, line, source string, done bool, progress func(RunProgress)) {
	if progress == nil {
		return
	}
	progress(RunProgress{
		RunID: runID,
		Stage: stage,
		Event: EventStageStream,
		Stream: &StreamUpdate{
			Stage:  stage,
			Mode:   StreamModeAppend,
			Delta:  line,
			Source: source,
			Done:   done,
		},
	})
}

func (r Runner) loadAndValidateRunInputs(ctx context.Context, taskID string, opts RunOptions, progress func(RunProgress)) (scanner.Project, []ProjectPathWarning, RunOptions, error) {
	project, err := r.store.GetProject(ctx, taskID)
	if err != nil {
		err = dbNotFoundTask(taskID)
		progress(RunProgress{Event: EventRunCrashed, Done: true, Err: err})
		return scanner.Project{}, nil, opts, err
	}
	project, pathWarnings, err := r.canonicalizeProjectForRun(project)
	if err != nil {
		progress(RunProgress{Event: EventRunCrashed, Done: true, Err: err})
		return scanner.Project{}, nil, opts, err
	}
	opts, err = r.normalizeRunOptions(ctx, project, opts)
	if err != nil {
		progress(RunProgress{Event: EventRunCrashed, Done: true, Err: err})
		return scanner.Project{}, nil, opts, err
	}
	return project, pathWarnings, opts, nil
}

func dbNotFoundTask(taskID string) error {
	return db.FormatNotFound("task", taskID)
}

func (r Runner) prepareRun(ctx context.Context, taskID string, project scanner.Project, pathWarnings []ProjectPathWarning, opts RunOptions, progress func(RunProgress)) (*runState, error) {
	previousRuns, _ := r.store.ListRunsForTask(ctx, taskID)
	start := time.Now().UTC()
	runID := displaytime.RunID(start)
	artifactRoot := runArtifactRoot(r.cfg.ScanPath, project, runID)
	if err := os.MkdirAll(filepath.Join(artifactRoot, "logs"), 0o755); err != nil {
		progress(RunProgress{RunID: runID, Event: EventRunCrashed, Done: true, Err: err})
		return nil, err
	}

	controlDir := filepath.Join(r.cfg.ScanPath, ".qa-control")
	released, releaseErr := assets.Release(controlDir)
	toolVersions, _ := json.Marshal(map[string]any{"assets": released})
	if releaseErr != nil {
		toolVersions, _ = json.Marshal(map[string]any{"assets_error": releaseErr.Error()})
	}
	importedDocs, docsImportErr := taskdocs.ImportDropbox(r.cfg.ScanPath, taskID, r.cfg.Docs, "p2r-run")
	docsManifest, docsManifestErr := taskdocs.ReadManifest(r.cfg.ScanPath, taskID)
	staticOnly := opts.StaticOnly || r.cfg.Pipeline.StaticOnly
	keepRuntime := opts.KeepRuntime || r.cfg.Docker.KeepRuntime
	run := model.RunRecord{
		RunID:          runID,
		TaskID:         taskID,
		StartedAt:      start.Format(time.RFC3339),
		Status:         model.RunRunning,
		ManualVerdict:  model.ManualUnset,
		StaticOnly:     staticOnly,
		ArtifactRoot:   artifactRoot,
		ToolVersions:   string(toolVersions),
		PromptVersions: string(toolVersions),
	}
	if err := r.store.CreateRun(ctx, run); err != nil {
		progress(RunProgress{RunID: runID, Event: EventRunCrashed, Done: true, Err: err})
		return nil, err
	}

	state := &runState{
		runner:          r,
		ctx:             ctx,
		taskID:          taskID,
		opts:            opts,
		progress:        progress,
		project:         project,
		pathWarnings:    pathWarnings,
		previousRuns:    previousRuns,
		start:           start,
		runID:           runID,
		artifactRoot:    artifactRoot,
		writer:          NewArtifactWriter(artifactRoot),
		released:        released,
		releaseErr:      releaseErr,
		importedDocs:    importedDocs,
		docsManifest:    docsManifest,
		docsImportErr:   docsImportErr,
		docsManifestErr: docsManifestErr,
		keepRuntime:     keepRuntime,
		run:             run,
		stages:          initialStages(selectedStages(opts, staticOnly), staticOnly),
		results:         map[string]model.StageRecord{},
		runCreated:      true,
	}
	state.recordPathWarnings()
	progress(RunProgress{RunID: runID, Event: EventRunCreated})
	return state, nil
}

func (s *runState) recordPathWarnings() {
	if len(s.pathWarnings) == 0 {
		return
	}
	s.addArtifactWarning(s.writer.BestEffortText("logs/path_warnings.log", formatProjectPathWarnings(s.pathWarnings)))
	for _, warning := range s.pathWarnings {
		s.progress(RunProgress{RunID: s.runID, Event: EventPathWarning, Err: errors.New(warning.Message())})
	}
}

func (s *runState) persistInitialArtifacts() error {
	if err := s.runner.writeRunManifest(s.run, s.project, s.opts, s.released, s.releaseErr, s.importedDocs, s.docsManifest, firstErrorString(s.docsImportErr, s.docsManifestErr), s.pathWarnings, s.artifactWarnings); err != nil {
		return err
	}
	if err := s.runner.writeStageStatus(s.runID, s.artifactRoot, s.stages); err != nil {
		return err
	}
	s.persistArtifactWarnings()
	return nil
}

func (s *runState) persistInitialStages() (Result, error, bool) {
	for _, stage := range s.stages {
		if result, err, aborted := s.abortIfCancelled(); aborted {
			return result, err, true
		}
		if err := s.runner.store.PutStage(s.ctx, s.runID, stage); err != nil {
			return s.abortOrError(err)
		}
		s.progress(RunProgress{RunID: s.runID, Stage: stage.Stage, Event: EventStagePending, StageRecord: stage})
	}
	return Result{}, nil, false
}

func (s *runState) runPreflightAndCleanup() (preflight.CheckResult, error) {
	preflightResult := preflight.Run(s.ctx, s.runner.exec, s.runner.cfg)
	if err := s.writer.RequiredJSON("preflight.json", preflightResult); err != nil {
		return preflight.CheckResult{}, err
	}
	preCleanup := s.runner.cleanupStaleRuns(s.ctx, s.previousRuns, s.runID, s.artifactRoot)
	if len(preCleanup) > 0 {
		if err := mergeCleanupIntoManifest(filepath.Join(s.artifactRoot, "run_manifest.json"), "pre_run_cleanup", preCleanup); err != nil {
			return preflightResult, err
		}
	}
	return preflightResult, nil
}

func (s *runState) executeStageLoop(preflightResult preflight.CheckResult) (Result, error, bool) {
	for index := range s.stages {
		stage := s.stages[index].Stage
		if result, err, aborted := s.abortIfCancelled(); aborted {
			return result, err, true
		}
		if s.stages[index].Status == model.StageSkipped {
			record := s.runner.materializeSkippedStage(s.run, s.stages[index])
			s.stages[index] = record
			s.results[stage] = record
			if result, err, aborted := s.persistStageUpdate(record, len(record.Findings) > 0, EventStageDone); aborted || err != nil {
				return result, err, aborted
			}
			continue
		}

		running := runningStage(s.run, stage)
		s.stages[index] = running
		s.results[stage] = running
		if result, err, aborted := s.persistStageUpdate(running, false, EventStageRunning); aborted || err != nil {
			return result, err, aborted
		}

		outcome := s.runner.executeStage(s.ctx, s.run, s.project, stage, s.results, s.opts, preflightResult, s.runtime, s.progress)
		if outcome.Runtime != nil {
			s.runtime = *outcome.Runtime
		}
		record := outcome.Record
		record.Findings = assignMissingFindingIDs(stage, record.Findings)
		s.stages[index] = record
		s.results[stage] = record
		if result, err, aborted := s.persistStageUpdate(record, true, EventStageDone); aborted || err != nil {
			return result, err, aborted
		}
		if !s.runtimeCleanupDone && runtimeCleanupPoint(stage, s.stages) {
			if result, err, aborted := s.cleanupRuntime(); aborted || err != nil {
				return result, err, aborted
			}
		}
	}
	return Result{}, nil, false
}

func (s *runState) finalizeRuntimeCleanup() (Result, error, bool) {
	if s.runtimeCleanupDone || !runtimeStageWasSelected(s.stages) {
		return Result{}, nil, false
	}
	return s.cleanupRuntime()
}

func (s *runState) cleanupRuntime() (Result, error, bool) {
	if result, err, aborted := s.abortIfCancelled(); aborted {
		return result, err, true
	}
	outcome := s.runner.finalizeRuntime(s.ctx, s.run, s.stages, s.runtime, s.keepRuntime, "normal")
	if outcome.Summary.Status == "failed" {
		s.cleanupFailed = true
	}
	if err := cleanupPersistError(outcome); err != nil {
		return s.abortOrError(err)
	}
	s.runtimeCleanupDone = outcome.RuntimeCleanupDone
	s.progress(RunProgress{RunID: s.runID, Event: EventCleanup})
	if result, err, aborted := s.abortIfCancelled(); aborted {
		return result, err, true
	}
	return Result{}, nil, false
}

func (s *runState) persistStageUpdate(record model.StageRecord, includeFindings bool, event ProgressEvent) (Result, error, bool) {
	if result, err, aborted := s.abortIfCancelled(); aborted {
		return result, err, true
	}
	if err := s.runner.store.PutStage(s.ctx, s.runID, record); err != nil {
		return s.abortOrError(err)
	}
	if includeFindings && len(record.Findings) > 0 {
		if err := s.runner.store.InsertFindings(s.ctx, s.runID, record.Findings); err != nil {
			return s.abortOrError(err)
		}
	}
	s.addArtifactWarnings(record.ArtifactWarnings...)
	s.persistArtifactWarnings()
	if err := s.runner.writeStageStatus(s.runID, s.artifactRoot, s.stages); err != nil {
		return Result{}, err, false
	}
	s.progress(RunProgress{RunID: s.runID, Stage: record.Stage, Event: event, StageRecord: record})
	if result, err, aborted := s.abortIfCancelled(); aborted {
		return result, err, true
	}
	return Result{}, nil, false
}

func (s *runState) finishRun() (Result, error, bool) {
	status := runStatus(s.stages)
	if s.cleanupFailed {
		status = model.RunCompletedWithFindings
	}
	if result, err, aborted := s.abortIfCancelled(); aborted {
		return result, err, true
	}
	if err := s.runner.store.FinishRun(s.ctx, s.runID, s.taskID, status, time.Since(s.start)); err != nil {
		return s.abortOrError(err)
	}
	s.runFinished = true
	s.run.Status = status
	s.run.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	s.run.DurationMS = time.Since(s.start).Milliseconds()
	s.progress(RunProgress{RunID: s.runID, Event: EventRunDone, Done: true})
	return Result{Run: s.run, Stages: s.stages}, nil, false
}

func (s *runState) abortIfCancelled() (Result, error, bool) {
	if abortErr := s.ctx.Err(); abortErr != nil {
		result, err := s.runner.finishAbortedRun(abortErr, &s.run, s.start, s.stages, s.runtime, s.keepRuntime, &s.runtimeCleanupDone, &s.runFinished, s.progress)
		s.stages = result.Stages
		return result, err, true
	}
	return Result{}, nil, false
}

func (s *runState) abortOrError(err error) (Result, error, bool) {
	if result, abortErr, aborted := s.abortIfCancelled(); aborted {
		return result, abortErr, true
	}
	return Result{}, err, false
}

func (s *runState) addArtifactWarning(warning ArtifactWarning) {
	if warning.OK() {
		return
	}
	s.artifactWarnings = append(s.artifactWarnings, warning)
}

func (s *runState) addArtifactWarnings(warnings ...ArtifactWarning) {
	for _, warning := range warnings {
		s.addArtifactWarning(warning)
	}
}

func (s *runState) persistArtifactWarnings() {
	if len(s.artifactWarnings) == 0 {
		return
	}
	_ = s.writer.BestEffortJSON("artifact_warnings.json", s.artifactWarnings)
}
