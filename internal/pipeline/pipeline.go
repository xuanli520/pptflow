package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xuanli520/p2r_tui/assets"
	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/db"
	"github.com/xuanli520/p2r_tui/internal/displaytime"
	"github.com/xuanli520/p2r_tui/internal/executor"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/preflight"
	"github.com/xuanli520/p2r_tui/internal/projectlayout"
	"github.com/xuanli520/p2r_tui/internal/scanner"
	"github.com/xuanli520/p2r_tui/internal/taskdocs"
)

type RunOptions struct {
	Stage       string
	From        string
	StaticOnly  bool
	Stages      []string
	Mode        string
	RefRun      string
	ExtraDocs   []string
	KeepRuntime bool
	Progress    ProgressReporter
}

type RunProgress struct {
	RunID       string
	TaskID      string
	Stage       string
	Event       string
	StageRecord model.StageRecord
	Done        bool
	Err         error
}

type ProgressReporter func(RunProgress)

type Result struct {
	Run    model.RunRecord
	Stages []model.StageRecord
}

type Runner struct {
	store *db.Store
	cfg   config.Config
	exec  executor.Runner
}

func NewRunner(store *db.Store, cfg config.Config) Runner {
	return Runner{store: store, cfg: cfg, exec: executor.New()}
}

func SelfTestReportPath(projectPath string, cfg config.Config) string {
	candidates := SelfTestReportCandidates(projectPath, cfg)
	if len(candidates) == 0 {
		return filepath.Clean(filepath.Join(projectPath, "repo", "self_test_report.md"))
	}
	return candidates[0]
}

func SelfTestReportCandidates(projectPath string, cfg config.Config) []string {
	var candidates []string
	add := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" {
			return
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(projectPath, path)
		}
		path = filepath.Clean(path)
		for _, existing := range candidates {
			if existing == path {
				return
			}
		}
		candidates = append(candidates, path)
	}
	add(cfg.Pipeline.SelfTestReportPath)
	add("repo/self_test_report.md")
	add("docs/self-test-report.md")
	return candidates
}

func runArtifactRoot(scanPath string, project scanner.Project, runID string) string {
	batchDir := projectlayout.SafePathSegment(project.Batch, "unbatched")
	taskDir := projectlayout.SafePathSegment(project.TaskID, "TASK-UNKNOWN")
	runDir := projectlayout.SafePathSegment(runID, "run-unknown")
	primary := filepath.Join(filepath.Clean(scanPath), "result", batchDir, taskDir, runDir)
	if pathWithin(primary, project.Path) {
		return filepath.Join(filepath.Clean(scanPath), ".qa-control", "runs", batchDir, taskDir, runDir)
	}
	return primary
}

func pathWithin(path, parent string) bool {
	path = absClean(path)
	parent = absClean(parent)
	rel, err := filepath.Rel(parent, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func absClean(path string) string {
	cleaned := filepath.Clean(path)
	if abs, err := filepath.Abs(cleaned); err == nil {
		return abs
	}
	return cleaned
}

func (r Runner) normalizeRunOptions(ctx context.Context, project scanner.Project, opts RunOptions) (RunOptions, error) {
	opts.Mode = strings.ToLower(strings.TrimSpace(opts.Mode))
	if opts.Mode == "" {
		opts.Mode = "initial"
	}
	switch opts.Mode {
	case "initial":
		if strings.TrimSpace(opts.RefRun) != "" {
			return opts, fmt.Errorf("--ref-run is only valid with --mode recheck")
		}
		if len(opts.ExtraDocs) > 0 {
			return opts, fmt.Errorf("--extra-docs is only valid with --mode recheck")
		}
	case "recheck":
		opts.RefRun = strings.TrimSpace(opts.RefRun)
		if opts.RefRun == "" {
			return opts, fmt.Errorf("--mode recheck requires --ref-run")
		}
		ref, err := r.store.GetRun(ctx, opts.RefRun)
		if err != nil {
			return opts, fmt.Errorf("ref run %s does not exist: %w", opts.RefRun, err)
		}
		if ref.TaskID != project.TaskID {
			return opts, fmt.Errorf("ref run %s belongs to task %s, not %s", opts.RefRun, ref.TaskID, project.TaskID)
		}
		if !completedRefRunStatus(ref.Status) {
			return opts, fmt.Errorf("ref run %s status is %s; --mode recheck requires a completed reference run", opts.RefRun, ref.Status)
		}
		if !dirExists(ref.ArtifactRoot) {
			return opts, fmt.Errorf("ref run %s artifact root is missing: %s", opts.RefRun, ref.ArtifactRoot)
		}
	default:
		return opts, fmt.Errorf("invalid --mode %q; expected initial or recheck", opts.Mode)
	}
	return opts, nil
}

func completedRefRunStatus(status string) bool {
	return status == model.RunCompletedClean || status == model.RunCompletedWithFindings
}

func (r Runner) Run(ctx context.Context, taskID string, opts RunOptions) (result Result, err error) {
	progress := func(update RunProgress) {
		if opts.Progress == nil {
			return
		}
		if update.TaskID == "" {
			update.TaskID = taskID
		}
		opts.Progress(update)
	}
	project, err := r.store.GetProject(ctx, taskID)
	if err != nil {
		progress(RunProgress{Event: "run_crashed", Done: true, Err: db.FormatNotFound("task", taskID)})
		return Result{}, db.FormatNotFound("task", taskID)
	}
	opts, err = r.normalizeRunOptions(ctx, project, opts)
	if err != nil {
		progress(RunProgress{Event: "run_crashed", Done: true, Err: err})
		return Result{}, err
	}
	lock, err := r.acquireTaskRunLock(taskID)
	if err != nil {
		progress(RunProgress{Event: "run_crashed", Done: true, Err: err})
		return Result{}, err
	}
	defer lock.Release()
	previousRuns, _ := r.store.ListRunsForTask(ctx, taskID)
	start := time.Now().UTC()
	runID := displaytime.RunID(start)
	artifactRoot := runArtifactRoot(r.cfg.ScanPath, project, runID)
	if err := os.MkdirAll(filepath.Join(artifactRoot, "logs"), 0o755); err != nil {
		progress(RunProgress{RunID: runID, Event: "run_crashed", Done: true, Err: err})
		return Result{}, err
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
		progress(RunProgress{RunID: runID, Event: "run_crashed", Done: true, Err: err})
		return Result{}, err
	}
	progress(RunProgress{RunID: runID, Event: "run_created"})
	runCreated := true
	runFinished := false
	defer func() {
		if recovered := recover(); recovered != nil {
			if runCreated && !runFinished {
				r.markRunCrashed(context.Background(), run, start, fmt.Sprintf("panic: %v", recovered))
			}
			progress(RunProgress{RunID: runID, Event: "run_crashed", Done: true, Err: fmt.Errorf("panic: %v", recovered)})
			panic(recovered)
		}
		if err != nil && runCreated && !runFinished {
			r.markRunCrashed(context.Background(), run, start, err.Error())
			progress(RunProgress{RunID: runID, Event: "run_crashed", Done: true, Err: err})
		}
	}()
	stages := initialStages(selectedStages(opts, staticOnly), staticOnly)
	results := map[string]model.StageRecord{}
	runtimeCleanupDone := false
	cleanupFailed := false
	abortIfCancelled := func() (Result, error, bool) {
		if abortErr := ctx.Err(); abortErr != nil {
			result, err := r.finishAbortedRun(abortErr, &run, start, stages, keepRuntime, &runtimeCleanupDone, &runFinished, progress)
			stages = result.Stages
			return result, err, true
		}
		return Result{}, nil, false
	}
	if result, err, aborted := abortIfCancelled(); aborted {
		return result, err
	}
	if err := r.writeRunManifest(run, project, opts, released, releaseErr, importedDocs, docsManifest, firstErrorString(docsImportErr, docsManifestErr)); err != nil {
		return Result{}, err
	}
	if err := r.writeStageStatus(runID, artifactRoot, stages); err != nil {
		return Result{}, err
	}
	if result, err, aborted := abortIfCancelled(); aborted {
		return result, err
	}
	for _, stage := range stages {
		if result, err, aborted := abortIfCancelled(); aborted {
			return result, err
		}
		_ = r.store.PutStage(ctx, runID, stage)
		progress(RunProgress{RunID: runID, Stage: stage.Stage, Event: "stage_pending", StageRecord: stage})
	}
	if result, err, aborted := abortIfCancelled(); aborted {
		return result, err
	}
	preflightResult := preflight.Run(ctx, r.exec, r.cfg)
	preflightPath := filepath.Join(artifactRoot, "preflight.json")
	_ = writeJSON(preflightPath, preflightResult)
	preCleanup := r.cleanupStaleRuns(ctx, previousRuns, runID, artifactRoot)
	if len(preCleanup) > 0 {
		mergeCleanupIntoManifest(filepath.Join(artifactRoot, "run_manifest.json"), "pre_run_cleanup", preCleanup)
	}
	if result, err, aborted := abortIfCancelled(); aborted {
		return result, err
	}
	for index := range stages {
		stage := stages[index].Stage
		if result, err, aborted := abortIfCancelled(); aborted {
			return result, err
		}
		if stages[index].Status == model.StageSkipped {
			record := r.materializeSkippedStage(run, stages[index])
			stages[index] = record
			results[stage] = record
			if result, err, aborted := abortIfCancelled(); aborted {
				return result, err
			}
			_ = r.store.PutStage(ctx, runID, record)
			_ = r.writeStageStatus(runID, artifactRoot, stages)
			progress(RunProgress{RunID: runID, Stage: record.Stage, Event: "stage_done", StageRecord: record})
			if result, err, aborted := abortIfCancelled(); aborted {
				return result, err
			}
			continue
		}
		running := runningStage(run, stage)
		stages[index] = running
		results[stage] = running
		if result, err, aborted := abortIfCancelled(); aborted {
			return result, err
		}
		_ = r.store.PutStage(ctx, runID, running)
		_ = r.writeStageStatus(runID, artifactRoot, stages)
		progress(RunProgress{RunID: runID, Stage: stage, Event: "stage_running", StageRecord: running})
		record := r.executeStage(ctx, run, project, stage, results, opts, preflightResult)
		record.Findings = assignMissingFindingIDs(stage, record.Findings)
		stages[index] = record
		results[stage] = record
		if result, err, aborted := abortIfCancelled(); aborted {
			return result, err
		}
		_ = r.store.PutStage(ctx, runID, record)
		_ = r.store.InsertFindings(ctx, runID, record.Findings)
		_ = r.writeStageStatus(runID, artifactRoot, stages)
		progress(RunProgress{RunID: runID, Stage: record.Stage, Event: "stage_done", StageRecord: record})
		if result, err, aborted := abortIfCancelled(); aborted {
			return result, err
		}
		if !runtimeCleanupDone && runtimeCleanupPoint(stage, stages) {
			cleanupSummary := r.cleanupCurrentRuntime(ctx, run, keepRuntime)
			mergeCleanupIntoManifest(filepath.Join(artifactRoot, "run_manifest.json"), "cleanup", cleanupSummary)
			if cleanupSummary.Status == "failed" {
				cleanupFailed = true
				_ = r.store.InsertFindings(ctx, runID, []model.Finding{cleanupFinding(cleanupSummary)})
			}
			runtimeCleanupDone = cleanupSummary.Status != "failed" || ctx.Err() == nil
			progress(RunProgress{RunID: runID, Event: "cleanup"})
			if result, err, aborted := abortIfCancelled(); aborted {
				return result, err
			}
		}
	}
	if !runtimeCleanupDone && runtimeStageWasSelected(stages) {
		if result, err, aborted := abortIfCancelled(); aborted {
			return result, err
		}
		cleanupSummary := r.cleanupCurrentRuntime(ctx, run, keepRuntime)
		mergeCleanupIntoManifest(filepath.Join(artifactRoot, "run_manifest.json"), "cleanup", cleanupSummary)
		if cleanupSummary.Status == "failed" {
			cleanupFailed = true
			_ = r.store.InsertFindings(ctx, runID, []model.Finding{cleanupFinding(cleanupSummary)})
		}
		runtimeCleanupDone = cleanupSummary.Status != "failed" || ctx.Err() == nil
		progress(RunProgress{RunID: runID, Event: "cleanup"})
		if result, err, aborted := abortIfCancelled(); aborted {
			return result, err
		}
	}

	status := runStatus(stages)
	if cleanupFailed {
		status = model.RunCompletedWithFindings
	}
	if result, err, aborted := abortIfCancelled(); aborted {
		return result, err
	}
	if err := r.store.FinishRun(ctx, runID, taskID, status, time.Since(start)); err != nil {
		return Result{}, err
	}
	runFinished = true
	run.Status = status
	run.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	run.DurationMS = time.Since(start).Milliseconds()
	progress(RunProgress{RunID: runID, Event: "run_done", Done: true})
	return Result{Run: run, Stages: stages}, nil
}

func (r Runner) markRunCrashed(ctx context.Context, run model.RunRecord, start time.Time, reason string) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "pipeline exited before finishing the run"
	}
	_ = writeJSON(filepath.Join(run.ArtifactRoot, "crash_summary.json"), map[string]any{
		"run_id":      run.RunID,
		"task_id":     run.TaskID,
		"status":      model.RunCrashed,
		"reason":      reason,
		"recorded_at": time.Now().UTC().Format(time.RFC3339),
	})
	_ = r.store.FinishRun(ctx, run.RunID, run.TaskID, model.RunCrashed, time.Since(start))
}

func (r Runner) finishAbortedRun(abortErr error, run *model.RunRecord, start time.Time, stages []model.StageRecord, keepRuntime bool, runtimeCleanupDone *bool, runFinished *bool, progress func(RunProgress)) (Result, error) {
	if abortErr == nil {
		abortErr = context.Canceled
	}
	*runFinished = true
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
		cleanupSummary := r.cleanupCurrentRuntime(cleanupCtx, *run, keepRuntime)
		cleanupCancel()
		cleanupStatus = cleanupSummary.Status
		mergeCleanupIntoManifest(filepath.Join(run.ArtifactRoot, "run_manifest.json"), "cleanup", cleanupSummary)
		if cleanupSummary.Status == "failed" {
			findingCtx, findingCancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := r.store.InsertFindings(findingCtx, run.RunID, []model.Finding{cleanupFinding(cleanupSummary)}); err != nil {
				saveErrors = append(saveErrors, "insert cleanup finding: "+err.Error())
			}
			findingCancel()
		}
		if runtimeCleanupDone != nil {
			*runtimeCleanupDone = cleanupSummary.Status != "failed"
		}
	}

	finishCtx, finishCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := r.store.FinishRun(finishCtx, run.RunID, run.TaskID, model.RunAborted, time.Since(start)); err != nil {
		saveErrors = append(saveErrors, "finish aborted run: "+err.Error())
	}
	finishCancel()

	run.Status = model.RunAborted
	run.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	run.DurationMS = time.Since(start).Milliseconds()
	_ = writeJSON(filepath.Join(run.ArtifactRoot, "abort_summary.json"), map[string]any{
		"run_id":         run.RunID,
		"task_id":        run.TaskID,
		"status":         model.RunAborted,
		"reason":         abortErr.Error(),
		"save_errors":    saveErrors,
		"cleanup_status": cleanupStatus,
		"recorded_at":    time.Now().UTC().Format(time.RFC3339),
	})
	progress(RunProgress{RunID: run.RunID, Event: "run_done", Done: true, Err: abortErr})
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

func (r Runner) executeStage(ctx context.Context, run model.RunRecord, project scanner.Project, stage string, prior map[string]model.StageRecord, opts RunOptions, preflightResult preflight.CheckResult) model.StageRecord {
	if check, ok := preflightResult.BlockingCheck(stage); ok {
		if stage == "D" || stage == "E" || stage == "F" {
			// Static stages materialize their own unavailable-review reports so
			// the external QA artifact contract stays intact.
		} else {
			return r.materializeSkippedStage(run, blockedStage(stage, stageName(stage), nil, check.Message))
		}
	}
	switch stage {
	case "A":
		return r.stageA(ctx, run, project)
	case "B":
		return r.stageB(ctx, run, project)
	case "C":
		return r.stageC(ctx, run, project)
	case "D":
		if opts.Mode == "recheck" {
			return r.stageCodex(ctx, run, project, opts, "D", "tests_coverage_report.md", "4_测试有效性报告_api端点真实性_确认修复报告.md")
		}
		return r.stageCodex(ctx, run, project, opts, "D", "tests_coverage_report.md", "tests_coverage_report.md", "4_测试有效性报告_api端点真实性.md")
	case "E":
		if opts.Mode == "recheck" {
			return r.stageCodex(ctx, run, project, opts, "E", "static_acceptance_audit.md", "1_质检AI测试报告_确认修复报告.md", "static_acceptance_audit_report.md")
		}
		return r.stageCodex(ctx, run, project, opts, "E", "static_acceptance_audit.md", "static_acceptance_audit_report.md", "1_质检AI测试报告.md")
	case "F":
		return r.stageF(ctx, run, project, opts, prior)
	default:
		return skippedStage(stage, "Unknown stage.")
	}
}

func selectedStages(opts RunOptions, staticOnly bool) map[string]bool {
	selected := map[string]bool{"A": true, "B": true, "C": true, "D": true, "E": true, "F": true}
	if len(opts.Stages) > 0 {
		selected = map[string]bool{}
		for _, stage := range opts.Stages {
			selected[strings.ToUpper(stage)] = true
		}
		return selected
	}
	if opts.Stage != "" {
		stage := strings.ToUpper(opts.Stage)
		selected := map[string]bool{stage: true}
		if stage != "F" {
			selected["F"] = true
		}
		return selected
	}
	if opts.From != "" {
		selected = map[string]bool{}
		include := false
		for _, stage := range []string{"A", "B", "C", "D", "E", "F"} {
			if stage == strings.ToUpper(opts.From) {
				include = true
			}
			if include {
				selected[stage] = true
			}
		}
		return selected
	}
	if staticOnly {
		return map[string]bool{"A": true, "D": true, "E": true, "F": true}
	}
	return selected
}

func initialStages(selected map[string]bool, staticOnly bool) []model.StageRecord {
	stages := make([]model.StageRecord, 0, 6)
	for _, stage := range []string{"A", "B", "C", "D", "E", "F"} {
		if selected[stage] {
			stages = append(stages, model.StageRecord{
				Stage:         stage,
				Name:          stageName(stage),
				Status:        model.StagePending,
				ArtifactPaths: []string{},
			})
			continue
		}
		reason := "Not selected for this run."
		if staticOnly && (stage == "B" || stage == "C") {
			reason = "--static-only skips Docker and run_tests evidence."
		}
		stages = append(stages, skippedStage(stage, reason))
	}
	return stages
}

func stageName(stage string) string {
	return model.StageDisplayName(stage)
}

func (r Runner) stageTimeout(key string, fallback int) time.Duration {
	normalized := strings.ToUpper(strings.ReplaceAll(key, "-", "_"))
	seconds := r.cfg.Pipeline.StageTimeouts[normalized]
	if seconds <= 0 {
		seconds = fallback
	}
	return time.Duration(seconds) * time.Second
}

func startStage(stage string) model.StageRecord {
	return model.StageRecord{
		Stage:         stage,
		Name:          stageName(stage),
		Status:        model.StageRunning,
		StartedAt:     time.Now().UTC().Format(time.RFC3339),
		ArtifactPaths: []string{},
	}
}

func runningStage(run model.RunRecord, stage string) model.StageRecord {
	record := startStage(stage)
	record.LogPath = stageLogPath(run.ArtifactRoot, stage)
	return record
}

func stageLogPath(artifactRoot, stage string) string {
	switch stage {
	case "A":
		return filepath.Join(artifactRoot, "logs", "A_validate.log")
	case "B":
		return filepath.Join(artifactRoot, "logs", "B_docker.log")
	case "C":
		return filepath.Join(artifactRoot, "logs", "C_tests.log")
	case "D":
		return filepath.Join(artifactRoot, "logs", "D_tests_coverage_static.log")
	case "E":
		return filepath.Join(artifactRoot, "logs", "E_static_audit.log")
	case "F":
		return filepath.Join(artifactRoot, "logs", "F_repair.log")
	default:
		return filepath.Join(artifactRoot, "logs", stage+"_stage.log")
	}
}

func finishStage(record model.StageRecord, status string, started time.Time) model.StageRecord {
	record.Status = status
	record.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	record.DurationMS = time.Since(started).Milliseconds()
	return record
}

func skippedStage(stage, reason string) model.StageRecord {
	return model.StageRecord{
		Stage:         stage,
		Name:          stageName(stage),
		Status:        model.StageSkipped,
		ArtifactPaths: []string{},
		ErrorSummary:  reason,
	}
}

func blockedStage(stage, name string, blockedBy []string, reason string) model.StageRecord {
	return model.StageRecord{
		Stage:         stage,
		Name:          name,
		Status:        model.StageBlocked,
		BlockedBy:     blockedBy,
		ArtifactPaths: []string{},
		ErrorSummary:  reason,
		Findings: []model.Finding{{
			Stage:      stage,
			Severity:   "High",
			Title:      fmt.Sprintf("Stage %s blocked", stage),
			Rule:       "Pipeline dependency",
			Evidence:   reason,
			Impact:     "Required evidence was not collected.",
			MinimumFix: "Fix the upstream stage and rerun the affected dependency chain.",
		}},
	}
}

func (r Runner) materializeSkippedStage(run model.RunRecord, record model.StageRecord) model.StageRecord {
	switch record.Stage {
	case "B":
		logPath := filepath.Join(run.ArtifactRoot, "logs", "B_docker.log")
		portMapPath := filepath.Join(run.ArtifactRoot, "port_map.json")
		screenshotPath := filepath.Join(run.ArtifactRoot, "5_Docker启动截图.png")
		reason := record.ErrorSummary
		if reason == "" {
			reason = "Stage B was not executed."
		}
		_ = writeText(logPath, reason)
		_ = writeJSON(portMapPath, map[string]any{"run_id": run.RunID, "mappings": map[string]any{}, "reason": reason})
		pages, _ := renderTerminalLog(reason, screenshotPath)
		record.LogPath = logPath
		record.ArtifactPaths = append([]string{portMapPath}, pages...)
	case "C":
		logPath := filepath.Join(run.ArtifactRoot, "logs", "C_tests.log")
		screenshotPath := filepath.Join(run.ArtifactRoot, "6_run_tests.sh运行截图.png")
		summaryPath := filepath.Join(run.ArtifactRoot, "test_runtime_summary.json")
		reason := record.ErrorSummary
		if reason == "" {
			reason = "Stage C was not executed."
		}
		_ = writeText(logPath, reason)
		_ = writeJSON(summaryPath, map[string]any{"ok": false, "reason": reason})
		pages, _ := renderTerminalLog(reason, screenshotPath)
		record.LogPath = logPath
		record.ArtifactPaths = append([]string{logPath}, pages...)
		record.ArtifactPaths = append(record.ArtifactPaths, summaryPath)
	}
	return record
}

func (r Runner) writeRunManifest(run model.RunRecord, project scanner.Project, opts RunOptions, released []assets.ReleasedFile, releaseErr error, importedDocs []taskdocs.Document, docsManifest taskdocs.Manifest, docsErr string) error {
	manifest := map[string]any{
		"run_id":           run.RunID,
		"task_id":          run.TaskID,
		"batch":            project.Batch,
		"started_at":       run.StartedAt,
		"started_at_utc":   run.StartedAt,
		"started_at_local": displaytime.LocalRFC3339(run.StartedAt),
		"timezone":         displaytime.Timezone,
		"project_path":     project.Path,
		"artifact_root":    run.ArtifactRoot,
		"static_only":      run.StaticOnly,
		"stage":            opts.Stage,
		"from":             opts.From,
		"stages":           opts.Stages,
		"qa_mode":          opts.Mode,
		"ref_run":          opts.RefRun,
		"extra_docs":       opts.ExtraDocs,
		"supplemental_docs": map[string]any{
			"manifest":         taskdocs.ManifestPath(r.cfg.ScanPath, run.TaskID),
			"managed_store":    taskdocs.StoreDir(r.cfg.ScanPath, run.TaskID),
			"count":            len(docsManifest.Docs),
			"imported_count":   len(importedDocs),
			"docs":             docsManifest.Docs,
			"inline_limit":     r.cfg.Docs.InlineTextLimitBytes,
			"stage_text_limit": r.cfg.Docs.StageInlineMaxBytes,
		},
		"keep_runtime":     opts.KeepRuntime || r.cfg.Docker.KeepRuntime,
		"self_test_report": r.cfg.Pipeline.SelfTestReportPath,
		"preflight":        "preflight.json",
		"stage_timeouts":   r.cfg.Pipeline.StageTimeouts,
		"tool_versions":    map[string]string{"p2r": "dev"},
		"assets":           released,
		"codex_policy": map[string]any{
			"sandbox_image":       r.cfg.Codex.SandboxImage,
			"network":             r.cfg.Codex.Network,
			"max_output_bytes":    r.cfg.Codex.MaxOutputBytes,
			"writable_tmp":        r.cfg.Codex.WritableTmp,
			"sandbox_mode":        "read-only",
			"approval":            "never",
			"home_reuse_strategy": "user HOME/CODEX_HOME/XDG config paths are preserved so Codex can read the configured auth/API key; unrelated shell environment is not inherited",
			"env_keys":            configuredEnvKeys(r.cfg.Codex.Env),
			"extra_args":          r.cfg.Codex.ExtraArgs,
			"docker_socket":       "not mounted",
		},
		"docker_cleanup_policy": map[string]any{
			"cleanup_policy":          r.cfg.Docker.CleanupPolicy,
			"cleanup_images":          r.cfg.Docker.CleanupImages,
			"cleanup_volumes":         r.cfg.Docker.CleanupVolumes,
			"cleanup_build_cache":     r.cfg.Docker.CleanupBuildCache,
			"build_cache_prune_until": r.cfg.Docker.BuildCachePruneUntil,
			"keep_runtime":            opts.KeepRuntime || r.cfg.Docker.KeepRuntime,
		},
	}
	if releaseErr != nil {
		manifest["asset_release_error"] = releaseErr.Error()
	}
	if docsErr != "" {
		manifest["docs_error"] = docsErr
	}
	return writeJSON(filepath.Join(run.ArtifactRoot, "run_manifest.json"), manifest)
}

func firstErrorString(errors ...error) string {
	for _, err := range errors {
		if err != nil {
			return err.Error()
		}
	}
	return ""
}

func (r Runner) writeStageStatus(runID, artifactRoot string, stages []model.StageRecord) error {
	return writeJSON(filepath.Join(artifactRoot, "stage_status.json"), model.StageStatusFile{RunID: runID, Stages: stages})
}

func runStatus(stages []model.StageRecord) string {
	for _, stage := range stages {
		if stage.Status == model.StageFailed || stage.Status == model.StageBlocked || len(stage.Findings) > 0 {
			return model.RunCompletedWithFindings
		}
	}
	return model.RunCompletedClean
}
