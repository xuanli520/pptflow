package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/xuanli520/p2r_tui/assets"
	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/displaytime"
	"github.com/xuanli520/p2r_tui/internal/executor"
	"github.com/xuanli520/p2r_tui/internal/pathutil"
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
	Event       ProgressEvent
	StageRecord model.StageRecord
	Done        bool
	Err         error
}

type ProgressEvent string

const (
	EventRunCreated   ProgressEvent = "run_created"
	EventPathWarning  ProgressEvent = "path_warning"
	EventStagePending ProgressEvent = "stage_pending"
	EventStageRunning ProgressEvent = "stage_running"
	EventStageDone    ProgressEvent = "stage_done"
	EventCleanup      ProgressEvent = "cleanup"
	EventRunDone      ProgressEvent = "run_done"
	EventRunAborted   ProgressEvent = "run_aborted"
	EventRunCrashed   ProgressEvent = "run_crashed"
)

type ProjectPathWarning struct {
	Type          string `json:"type"`
	DBPath        string `json:"db_path"`
	CanonicalPath string `json:"canonical_path"`
}

type ProgressReporter func(RunProgress)

type Result struct {
	Run    model.RunRecord
	Stages []model.StageRecord
}

type runStore interface {
	GetProject(context.Context, string) (scanner.Project, error)
	GetRun(context.Context, string) (model.RunRecord, error)
	ListRunsForTask(context.Context, string) ([]model.RunRecord, error)
	CreateRun(context.Context, model.RunRecord) error
	PutStage(context.Context, string, model.StageRecord) error
	InsertFindings(context.Context, string, []model.Finding) error
	FinishRun(context.Context, string, string, string, time.Duration) error
}

type CommandRunner interface {
	LookPath(name string) (string, error)
	Run(ctx context.Context, timeout time.Duration, dir string, env []string, name string, args ...string) executor.Result
	RunStreaming(ctx context.Context, timeout time.Duration, dir string, env []string, writer io.Writer, name string, args ...string) executor.Result
}

type RunnerOption func(*Runner)

type Runner struct {
	store runStore
	cfg   config.Config
	exec  CommandRunner
}

func WithCommandRunner(exec CommandRunner) RunnerOption {
	return func(r *Runner) {
		if exec != nil {
			r.exec = exec
		}
	}
}

func NewRunner(store runStore, cfg config.Config, opts ...RunnerOption) Runner {
	runner := Runner{store: store, cfg: cfg, exec: executor.New()}
	for _, opt := range opts {
		if opt != nil {
			opt(&runner)
		}
	}
	return runner
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
	if pathutil.PathWithin(primary, project.Path) {
		return filepath.Join(filepath.Clean(scanPath), ".qa-control", "runs", batchDir, taskDir, runDir)
	}
	return primary
}

func (r Runner) normalizeRunOptions(ctx context.Context, project scanner.Project, opts RunOptions) (RunOptions, error) {
	var err error
	if opts, err = normalizeStageOptions(opts); err != nil {
		return opts, err
	}
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

func normalizeStageOptions(opts RunOptions) (RunOptions, error) {
	if opts.Stage != "" {
		stage, ok := model.NormalizeStage(opts.Stage)
		if !ok {
			return opts, fmt.Errorf("invalid stage %q; expected A..F", opts.Stage)
		}
		opts.Stage = stage
	}
	if opts.From != "" {
		from, ok := model.NormalizeStage(opts.From)
		if !ok {
			return opts, fmt.Errorf("invalid from stage %q; expected A..F", opts.From)
		}
		opts.From = from
	}
	if len(opts.Stages) > 0 {
		normalized := make([]string, 0, len(opts.Stages))
		seen := map[string]bool{}
		for _, raw := range opts.Stages {
			stage, ok := model.NormalizeStage(raw)
			if !ok {
				return opts, fmt.Errorf("invalid stage %q in stage list; expected A..F", raw)
			}
			if seen[stage] {
				continue
			}
			seen[stage] = true
			normalized = append(normalized, stage)
		}
		opts.Stages = normalized
	}
	return opts, nil
}

func completedRefRunStatus(status string) bool {
	return status == model.RunCompletedClean || status == model.RunCompletedWithFindings
}

func (r Runner) canonicalizeProjectForRun(project scanner.Project) (scanner.Project, []ProjectPathWarning, error) {
	dbPath := filepath.Clean(project.Path)
	expected := projectlayout.ExpectedProjectPath(r.cfg.ScanPath, project.Batch, project.TaskID)
	validation := projectlayout.ValidatePackageRoot(expected)
	if !validation.Valid {
		return project, nil, invalidIndexedProjectPathError(r.cfg.ScanPath, expected)
	}

	project.Path = filepath.Clean(expected)
	project.MetadataPromptMissing = projectlayout.MetadataPromptMissing(project.Path)
	if dbPath != project.Path {
		return project, []ProjectPathWarning{{
			Type:          "stale_project_path",
			DBPath:        dbPath,
			CanonicalPath: project.Path,
		}}, nil
	}
	return project, nil, nil
}

func invalidIndexedProjectPathError(scanRoot, expected string) error {
	return fmt.Errorf("indexed project path is invalid or stale:\nexpected package root %s\nbut it does not contain metadata.json, docs/, repo/, and an original session marker.\nPlease rerun p2r scan --path %s; if old artifact rows remain, run p2r scan --path %s --prune-artifacts.", expected, filepath.Clean(scanRoot), filepath.Clean(scanRoot))
}

func (warning ProjectPathWarning) Message() string {
	if warning.Type == "stale_project_path" {
		return fmt.Sprintf("DB project path was stale; runtime used canonical package root.\ndb_path=%s\ncanonical_path=%s", warning.DBPath, warning.CanonicalPath)
	}
	return fmt.Sprintf("project path warning: %s db_path=%s canonical_path=%s", warning.Type, warning.DBPath, warning.CanonicalPath)
}

func formatProjectPathWarnings(warnings []ProjectPathWarning) string {
	var builder strings.Builder
	builder.WriteString("=== project path warnings ===\n")
	for _, warning := range warnings {
		builder.WriteString(warning.Message())
		builder.WriteString("\n")
	}
	return builder.String()
}

func (r Runner) Run(ctx context.Context, taskID string, opts RunOptions) (result Result, err error) {
	progress := makeRunProgress(taskID, opts.Progress)
	project, pathWarnings, opts, err := r.loadAndValidateRunInputs(ctx, taskID, opts, progress)
	if err != nil {
		return Result{}, err
	}
	lock, err := r.acquireTaskRunLock(taskID)
	if err != nil {
		progress(RunProgress{Event: EventRunCrashed, Done: true, Err: err})
		return Result{}, err
	}
	defer lock.Release()

	var state *runState
	defer func() {
		if recovered := recover(); recovered != nil {
			if state != nil && state.runCreated && !state.runFinished {
				_ = r.crashRun(context.Background(), state.run, state.start, state.stages, state.keepRuntime, state.runtimeCleanupDone, fmt.Sprintf("panic: %v", recovered))
			}
			progress(RunProgress{RunID: runIDFromState(state), Event: EventRunCrashed, Done: true, Err: fmt.Errorf("panic: %v", recovered)})
			panic(recovered)
		}
		if err != nil && state != nil && state.runCreated && !state.runFinished {
			if persistErr := r.crashRun(context.Background(), state.run, state.start, state.stages, state.keepRuntime, state.runtimeCleanupDone, err.Error()); persistErr != nil {
				err = errors.Join(err, persistErr)
			}
			progress(RunProgress{RunID: state.runID, Event: EventRunCrashed, Done: true, Err: err})
		}
	}()

	state, err = r.prepareRun(ctx, taskID, project, pathWarnings, opts, progress)
	if err != nil {
		return Result{}, err
	}
	if result, err, aborted := state.abortIfCancelled(); aborted {
		return result, err
	}
	if err := state.persistInitialArtifacts(); err != nil {
		return Result{}, err
	}
	if result, err, aborted := state.abortIfCancelled(); aborted {
		return result, err
	}
	if result, err, aborted := state.persistInitialStages(); aborted || err != nil {
		return result, err
	}
	preflightResult, err := state.runPreflightAndCleanup()
	if err != nil {
		if result, err, aborted := state.abortOrError(err); aborted {
			return result, err
		}
		return Result{}, err
	}
	if result, err, aborted := state.abortIfCancelled(); aborted {
		return result, err
	}
	if result, err, aborted := state.executeStageLoop(preflightResult); aborted || err != nil {
		return result, err
	}
	if result, err, aborted := state.finalizeRuntimeCleanup(); aborted || err != nil {
		return result, err
	}
	result, err, _ = state.finishRun()
	return result, err
}

func runIDFromState(state *runState) string {
	if state == nil {
		return ""
	}
	return state.runID
}

func (r Runner) markRunCrashed(ctx context.Context, run model.RunRecord, start time.Time, reason string) error {
	return r.crashRun(ctx, run, start, nil, false, true, reason)
}

func (r Runner) crashRun(ctx context.Context, run model.RunRecord, start time.Time, stages []model.StageRecord, keepRuntime bool, runtimeCleanupDone bool, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "pipeline exited before finishing the run"
	}
	saveErrors := []error{}
	cleanupStatus := "not_applicable"
	if runtimeCleanupDone {
		cleanupStatus = "already_done"
	} else if runtimeStageWasSelected(stages) {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		cleanup := r.finalizeRuntime(cleanupCtx, run, stages, keepRuntime, "crash")
		cleanupCancel()
		cleanupStatus = cleanup.Summary.Status
		saveErrors = append(saveErrors, cleanup.PersistErrors...)
	}
	if err := NewArtifactWriter(run.ArtifactRoot).RequiredJSON("crash_summary.json", map[string]any{
		"run_id":         run.RunID,
		"task_id":        run.TaskID,
		"status":         model.RunCrashed,
		"reason":         reason,
		"save_errors":    cleanupPersistErrorStrings(cleanupOutcome{PersistErrors: saveErrors}),
		"cleanup_status": cleanupStatus,
		"recorded_at":    time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		saveErrors = append(saveErrors, err)
	}
	saveCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := r.store.FinishRun(saveCtx, run.RunID, run.TaskID, model.RunCrashed, time.Since(start)); err != nil {
		saveErrors = append(saveErrors, fmt.Errorf("finish crashed run: %w", err))
	}
	return errors.Join(saveErrors...)
}

func (r Runner) finishAbortedRun(abortErr error, run *model.RunRecord, start time.Time, stages []model.StageRecord, keepRuntime bool, runtimeCleanupDone *bool, runFinished *bool, progress func(RunProgress)) (Result, error) {
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
		cleanup := r.finalizeRuntime(cleanupCtx, *run, stages, keepRuntime, "abort")
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
	selected := stageSet(model.AllStages())
	if len(opts.Stages) > 0 {
		selected = map[string]bool{}
		for _, stage := range opts.Stages {
			if normalized, ok := model.NormalizeStage(stage); ok {
				selected[normalized] = true
			}
		}
		return selected
	}
	if opts.Stage != "" {
		stage, ok := model.NormalizeStage(opts.Stage)
		if !ok {
			return map[string]bool{}
		}
		selected := map[string]bool{stage: true}
		if stage != string(model.StageF) {
			selected[string(model.StageF)] = true
		}
		return selected
	}
	if opts.From != "" {
		selected = map[string]bool{}
		include := false
		from, ok := model.NormalizeStage(opts.From)
		if !ok {
			return selected
		}
		for _, stage := range model.AllStages() {
			if stage == from {
				include = true
			}
			if include {
				selected[stage] = true
			}
		}
		return selected
	}
	if staticOnly {
		return staticStageSet()
	}
	return selected
}

func initialStages(selected map[string]bool, staticOnly bool) []model.StageRecord {
	stages := make([]model.StageRecord, 0, 6)
	for _, stage := range model.AllStages() {
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
		if staticOnly && model.IsRuntimeStage(stage) {
			reason = "--static-only skips Docker and run_tests evidence."
		}
		stages = append(stages, skippedStage(stage, reason))
	}
	return stages
}

func stageSet(stages []string) map[string]bool {
	selected := make(map[string]bool, len(stages))
	for _, stage := range stages {
		if normalized, ok := model.NormalizeStage(stage); ok {
			selected[normalized] = true
		}
	}
	return selected
}

func staticStageSet() map[string]bool {
	selected := map[string]bool{}
	for _, spec := range model.AllStageSpecs() {
		if spec.Static {
			selected[string(spec.ID)] = true
		}
	}
	return selected
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
	return filepath.Join(artifactRoot, "logs", model.StageLogName(stage))
}

func finishStage(record model.StageRecord, status string, started time.Time) model.StageRecord {
	if record.Status == model.StageFailed && status == model.StageDone {
		status = model.StageFailed
	}
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
	writer := NewArtifactWriter(run.ArtifactRoot)
	switch record.Stage {
	case "B":
		logPath := filepath.Join(run.ArtifactRoot, "logs", "B_docker.log")
		portMapPath := filepath.Join(run.ArtifactRoot, "port_map.json")
		screenshotPath := filepath.Join(run.ArtifactRoot, "5_Docker启动截图.png")
		reason := record.ErrorSummary
		if reason == "" {
			reason = "Stage B was not executed."
		}
		if err := writer.RequiredText("logs/B_docker.log", reason); err != nil {
			record = recordArtifactWriteError(record, err, logPath)
		}
		if err := writer.RequiredJSON("port_map.json", map[string]any{"run_id": run.RunID, "mappings": map[string]any{}, "reason": reason}); err != nil {
			record = recordArtifactWriteError(record, err, portMapPath)
		}
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
		if err := writer.RequiredText("logs/C_tests.log", reason); err != nil {
			record = recordArtifactWriteError(record, err, logPath)
		}
		if err := writer.RequiredJSON("test_runtime_summary.json", map[string]any{"ok": false, "reason": reason}); err != nil {
			record = recordArtifactWriteError(record, err, summaryPath)
		}
		pages, _ := renderTerminalLog(reason, screenshotPath)
		record.LogPath = logPath
		record.ArtifactPaths = append([]string{logPath}, pages...)
		record.ArtifactPaths = append(record.ArtifactPaths, summaryPath)
	}
	return record
}

func recordArtifactWriteError(record model.StageRecord, err error, sourcePath string) model.StageRecord {
	if err == nil {
		return record
	}
	record.Status = model.StageFailed
	record.ErrorSummary = err.Error()
	record.Findings = append(record.Findings, model.Finding{
		Stage:      "INFRA",
		Severity:   "High",
		Title:      "Required p2r artifact could not be written",
		Rule:       "p2r stages must persist required evidence artifacts.",
		Evidence:   err.Error(),
		Impact:     "The run evidence is incomplete even if the underlying stage work completed.",
		MinimumFix: "Ensure the run artifact directory is writable and rerun the affected stage.",
		SourcePath: sourcePath,
	})
	return record
}

func (r Runner) writeRunManifest(run model.RunRecord, project scanner.Project, opts RunOptions, released []assets.ReleasedFile, releaseErr error, importedDocs []taskdocs.Document, docsManifest taskdocs.Manifest, docsErr string, pathWarnings []ProjectPathWarning, artifactWarnings []ArtifactWarning) error {
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
	if len(pathWarnings) > 0 {
		manifest["path_warnings"] = pathWarnings
	}
	if len(artifactWarnings) > 0 {
		manifest["artifact_warnings"] = artifactWarnings
	}
	return NewArtifactWriter(run.ArtifactRoot).RequiredJSON("run_manifest.json", manifest)
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
	return NewArtifactWriter(artifactRoot).RequiredJSON("stage_status.json", model.StageStatusFile{RunID: runID, Stages: stages})
}

func runStatus(stages []model.StageRecord) string {
	for _, stage := range stages {
		if stage.Status == model.StageFailed || stage.Status == model.StageBlocked || len(stage.Findings) > 0 {
			return model.RunCompletedWithFindings
		}
	}
	return model.RunCompletedClean
}
