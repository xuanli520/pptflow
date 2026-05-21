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
	prepare   runPrepareInput
	history   runHistory
	identity  runIdentity
	released  runReleasedInputs
	execution runExecutionState
}

type runPrepareInput struct {
	ctx      context.Context
	taskID   string
	opts     RunOptions
	progress func(RunProgress)

	project      scanner.Project
	pathWarnings []ProjectPathWarning
}

type runHistory struct {
	previousRuns []model.RunRecord
}

type runIdentity struct {
	start        time.Time
	runID        string
	artifactRoot string
	writer       ArtifactWriter
}

type runReleasedInputs struct {
	assets          []assets.ReleasedFile
	assetsErr       error
	importedDocs    []taskdocs.Document
	docsManifest    taskdocs.Manifest
	docsImportErr   error
	docsManifestErr error
}

type runExecutionState struct {
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

func (r Runner) prepareRun(input runPrepareInput) (*runState, error) {
	previousRuns, _ := r.store.ListRunsForTask(input.ctx, input.taskID)
	start := time.Now().UTC()
	runID := displaytime.RunID(start)
	artifactRoot := runArtifactRoot(r.cfg.ScanPath, input.project, runID)
	if err := os.MkdirAll(filepath.Join(artifactRoot, "logs"), 0o755); err != nil {
		input.progress(RunProgress{RunID: runID, Event: EventRunCrashed, Done: true, Err: err})
		return nil, err
	}

	controlDir := filepath.Join(r.cfg.ScanPath, ".qa-control")
	released, releaseErr := assets.Release(controlDir)
	toolVersions, _ := json.Marshal(map[string]any{"assets": released})
	if releaseErr != nil {
		toolVersions, _ = json.Marshal(map[string]any{"assets_error": releaseErr.Error()})
	}
	importedDocs, docsImportErr := taskdocs.ImportDropbox(r.cfg.ScanPath, input.taskID, r.cfg.Docs, "p2r-run")
	docsManifest, docsManifestErr := taskdocs.ReadManifest(r.cfg.ScanPath, input.taskID)
	staticOnly := input.opts.StaticOnly || r.cfg.Pipeline.StaticOnly
	keepRuntime := input.opts.KeepRuntime || r.cfg.Docker.KeepRuntime
	run := model.RunRecord{
		RunID:          runID,
		TaskID:         input.taskID,
		StartedAt:      start.Format(time.RFC3339),
		Status:         model.RunRunning,
		ManualVerdict:  model.ManualUnset,
		StaticOnly:     staticOnly,
		ArtifactRoot:   artifactRoot,
		ToolVersions:   string(toolVersions),
		PromptVersions: string(toolVersions),
	}
	if err := r.store.CreateRun(input.ctx, run); err != nil {
		input.progress(RunProgress{RunID: runID, Event: EventRunCrashed, Done: true, Err: err})
		return nil, err
	}

	state := &runState{
		prepare: input,
		history: runHistory{
			previousRuns: previousRuns,
		},
		identity: runIdentity{
			start:        start,
			runID:        runID,
			artifactRoot: artifactRoot,
			writer:       NewArtifactWriter(artifactRoot),
		},
		released: runReleasedInputs{
			assets:          released,
			assetsErr:       releaseErr,
			importedDocs:    importedDocs,
			docsManifest:    docsManifest,
			docsImportErr:   docsImportErr,
			docsManifestErr: docsManifestErr,
		},
		execution: runExecutionState{
			keepRuntime: keepRuntime,
			run:         run,
			stages:      initialStages(selectedStages(input.opts, staticOnly), staticOnly),
			results:     map[string]model.StageRecord{},
			runCreated:  true,
		},
	}
	state.recordPathWarnings()
	input.progress(RunProgress{RunID: runID, Event: EventRunCreated})
	return state, nil
}

func (s *runState) recordPathWarnings() {
	warnings := s.prepare.pathWarnings
	if len(warnings) == 0 {
		return
	}
	s.addArtifactWarning(s.identity.writer.BestEffortText("logs/path_warnings.log", formatProjectPathWarnings(warnings)))
	for _, warning := range warnings {
		s.prepare.progress(RunProgress{RunID: s.identity.runID, Event: EventPathWarning, Err: errors.New(warning.Message())})
	}
}

func (s *runState) persistInitialArtifacts(r Runner) error {
	released := s.released
	docsErr := firstErrorString(released.docsImportErr, released.docsManifestErr)
	if err := r.writeRunManifest(
		s.execution.run,
		s.prepare.project,
		s.prepare.opts,
		released.assets,
		released.assetsErr,
		released.importedDocs,
		released.docsManifest,
		docsErr,
		s.prepare.pathWarnings,
		s.execution.artifactWarnings,
	); err != nil {
		return err
	}
	if err := r.writeStageStatus(s.identity.runID, s.identity.artifactRoot, s.execution.stages); err != nil {
		return err
	}
	s.persistArtifactWarnings()
	return nil
}

func (s *runState) persistInitialStages(r Runner) (Result, error, bool) {
	for _, stage := range s.execution.stages {
		if result, err, aborted := s.abortIfCancelled(r); aborted {
			return result, err, true
		}
		if err := r.store.PutStage(s.prepare.ctx, s.identity.runID, stage); err != nil {
			return s.abortOrError(r, err)
		}
		s.prepare.progress(RunProgress{RunID: s.identity.runID, Stage: stage.Stage, Event: EventStagePending, StageRecord: stage})
	}
	return Result{}, nil, false
}

func (s *runState) runPreflightAndCleanup(r Runner) (preflight.CheckResult, error) {
	preflightResult := preflight.Run(s.prepare.ctx, r.exec, r.cfg)
	if err := s.identity.writer.RequiredJSON("preflight.json", preflightResult); err != nil {
		return preflight.CheckResult{}, err
	}
	preCleanup := r.cleanupStaleRuns(
		s.prepare.ctx,
		s.history.previousRuns,
		s.identity.runID,
		s.identity.artifactRoot,
	)
	if len(preCleanup) > 0 {
		if err := mergeCleanupIntoManifest(filepath.Join(s.identity.artifactRoot, "run_manifest.json"), "pre_run_cleanup", preCleanup); err != nil {
			return preflightResult, err
		}
	}
	return preflightResult, nil
}

func (s *runState) executeStageLoop(r Runner, preflightResult preflight.CheckResult) (Result, error, bool) {
	for index := 0; index < len(s.execution.stages); index++ {
		stage := s.execution.stages[index].Stage
		if result, err, aborted := s.abortIfCancelled(r); aborted {
			return result, err, true
		}
		if s.execution.stages[index].Status == model.StageSkipped {
			record := r.materializeSkippedStage(s.execution.run, s.execution.stages[index])
			s.execution.stages[index] = record
			s.execution.results[stage] = record
			if result, err, aborted := s.persistStageUpdate(r, record, len(record.Findings) > 0, EventStageDone); aborted || err != nil {
				return result, err, aborted
			}
			continue
		}

		running := runningStage(s.execution.run, stage)
		s.execution.stages[index] = running
		s.execution.results[stage] = running
		if result, err, aborted := s.persistStageUpdate(r, running, false, EventStageRunning); aborted || err != nil {
			return result, err, aborted
		}

		outcome := r.executeStage(
			s.prepare.ctx,
			s.execution.run,
			s.prepare.project,
			stage,
			s.execution.results,
			s.prepare.opts,
			preflightResult,
			s.execution.runtime,
			s.prepare.progress,
		)
		if outcome.Runtime != nil {
			s.execution.runtime = *outcome.Runtime
		}
		record := outcome.Record
		record.Findings = assignMissingFindingIDs(stage, record.Findings)
		s.execution.stages[index] = record
		s.execution.results[stage] = record
		if result, err, aborted := s.persistStageUpdate(r, record, true, EventStageDone); aborted || err != nil {
			return result, err, aborted
		}
		if outcome.SkipNextStage && index+1 < len(s.execution.stages) {
			nextStage := s.execution.stages[index+1].Stage
			skipped := skippedStage(nextStage, "Skipped because previous stage failed before required runtime was available.")
			skipped = r.materializeSkippedStage(s.execution.run, skipped)
			s.execution.stages[index+1] = skipped
			s.execution.results[nextStage] = skipped
			if result, err, aborted := s.persistStageUpdate(r, skipped, len(skipped.Findings) > 0, EventStageDone); aborted || err != nil {
				return result, err, aborted
			}
			index++
		}
		if !s.prepare.opts.DeferRuntimeCleanup && !s.execution.runtimeCleanupDone && runtimeCleanupPoint(stage, s.execution.stages) {
			if result, err, aborted := s.cleanupRuntime(r); aborted || err != nil {
				return result, err, aborted
			}
		}
	}
	return Result{}, nil, false
}

func (s *runState) finalizeRuntimeCleanup(r Runner) (Result, error, bool) {
	if s.prepare.opts.DeferRuntimeCleanup {
		return Result{}, nil, false
	}
	if s.execution.runtimeCleanupDone || !runtimeStageWasSelected(s.execution.stages) {
		return Result{}, nil, false
	}
	return s.cleanupRuntime(r)
}

func (s *runState) cleanupRuntime(r Runner) (Result, error, bool) {
	if result, err, aborted := s.abortIfCancelled(r); aborted {
		return result, err, true
	}
	outcome := r.finalizeRuntime(
		s.prepare.ctx,
		s.execution.run,
		s.execution.stages,
		s.execution.runtime,
		s.execution.keepRuntime,
		"normal",
	)
	if outcome.Summary.Status == "failed" {
		s.execution.cleanupFailed = true
	}
	if err := cleanupPersistError(outcome); err != nil {
		return s.abortOrError(r, err)
	}
	s.execution.runtimeCleanupDone = outcome.RuntimeCleanupDone
	s.prepare.progress(RunProgress{RunID: s.identity.runID, Event: EventCleanup})
	if result, err, aborted := s.abortIfCancelled(r); aborted {
		return result, err, true
	}
	return Result{}, nil, false
}

func (s *runState) persistStageUpdate(r Runner, record model.StageRecord, includeFindings bool, event ProgressEvent) (Result, error, bool) {
	if result, err, aborted := s.abortIfCancelled(r); aborted {
		return result, err, true
	}
	if err := r.store.PutStage(s.prepare.ctx, s.identity.runID, record); err != nil {
		return s.abortOrError(r, err)
	}
	if includeFindings && len(record.Findings) > 0 {
		if err := r.store.InsertFindings(s.prepare.ctx, s.identity.runID, record.Findings); err != nil {
			return s.abortOrError(r, err)
		}
	}
	s.addArtifactWarnings(record.ArtifactWarnings...)
	s.persistArtifactWarnings()
	if err := r.writeStageStatus(s.identity.runID, s.identity.artifactRoot, s.execution.stages); err != nil {
		return Result{}, err, false
	}
	s.prepare.progress(RunProgress{RunID: s.identity.runID, Stage: record.Stage, Event: event, StageRecord: record})
	if result, err, aborted := s.abortIfCancelled(r); aborted {
		return result, err, true
	}
	return Result{}, nil, false
}

func (s *runState) finishRun(r Runner) (Result, error, bool) {
	status := runStatus(s.execution.stages)
	if s.execution.cleanupFailed {
		status = model.RunCompletedWithFindings
	}
	if result, err, aborted := s.abortIfCancelled(r); aborted {
		return result, err, true
	}
	if err := r.store.FinishRun(s.prepare.ctx, s.identity.runID, s.prepare.taskID, status, time.Since(s.identity.start)); err != nil {
		return s.abortOrError(r, err)
	}
	s.execution.runFinished = true
	s.execution.run.Status = status
	s.execution.run.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	s.execution.run.DurationMS = time.Since(s.identity.start).Milliseconds()
	s.prepare.progress(RunProgress{RunID: s.identity.runID, Event: EventRunDone, Done: true})
	return Result{Run: s.execution.run, Stages: s.execution.stages}, nil, false
}

func (s *runState) abortIfCancelled(r Runner) (Result, error, bool) {
	if abortErr := s.prepare.ctx.Err(); abortErr != nil {
		result, err := r.finishAbortedRun(
			abortErr,
			&s.execution.run,
			s.identity.start,
			s.execution.stages,
			s.execution.runtime,
			s.execution.keepRuntime,
			&s.execution.runtimeCleanupDone,
			&s.execution.runFinished,
			s.prepare.progress,
		)
		s.execution.stages = result.Stages
		return result, err, true
	}
	return Result{}, nil, false
}

func (s *runState) abortOrError(r Runner, err error) (Result, error, bool) {
	if result, abortErr, aborted := s.abortIfCancelled(r); aborted {
		return result, abortErr, true
	}
	return Result{}, err, false
}

func (s *runState) canPersistCrash() bool {
	return s.execution.runCreated && !s.execution.runFinished
}

func (s *runState) persistCrash(r Runner, reason string) error {
	return r.crashRun(
		context.Background(),
		s.execution.run,
		s.identity.start,
		s.execution.stages,
		s.execution.runtime,
		s.execution.keepRuntime,
		s.execution.runtimeCleanupDone,
		reason,
	)
}

func (s *runState) addArtifactWarning(warning ArtifactWarning) {
	if warning.OK() {
		return
	}
	s.execution.artifactWarnings = append(s.execution.artifactWarnings, warning)
}

func (s *runState) addArtifactWarnings(warnings ...ArtifactWarning) {
	for _, warning := range warnings {
		s.addArtifactWarning(warning)
	}
}

func (s *runState) persistArtifactWarnings() {
	if len(s.execution.artifactWarnings) == 0 {
		return
	}
	_ = s.identity.writer.BestEffortJSON("artifact_warnings.json", s.execution.artifactWarnings)
}
