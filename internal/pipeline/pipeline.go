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
	"github.com/xuanli520/p2r_tui/internal/executor"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/preflight"
	"github.com/xuanli520/p2r_tui/internal/scanner"
)

type RunOptions struct {
	Stage      string
	From       string
	StaticOnly bool
	Stages     []string
	Mode       string
	RefRun     string
	ExtraDocs  []string
}

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
	path := strings.TrimSpace(cfg.Pipeline.SelfTestReportPath)
	if path == "" {
		path = "repo/self_test_report.md"
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(projectPath, path))
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
		if !dirExists(ref.ArtifactRoot) {
			return opts, fmt.Errorf("ref run %s artifact root is missing: %s", opts.RefRun, ref.ArtifactRoot)
		}
	default:
		return opts, fmt.Errorf("invalid --mode %q; expected initial or recheck", opts.Mode)
	}
	return opts, nil
}

func (r Runner) Run(ctx context.Context, taskID string, opts RunOptions) (Result, error) {
	project, err := r.store.GetProject(ctx, taskID)
	if err != nil {
		return Result{}, db.FormatNotFound("task", taskID)
	}
	opts, err = r.normalizeRunOptions(ctx, project, opts)
	if err != nil {
		return Result{}, err
	}
	start := time.Now().UTC()
	runID := fmt.Sprintf("run-%s-%d", start.Format("20060102-150405"), start.UnixNano()%1000000)
	artifactRoot := filepath.Join(project.Path, "qa", "runs", runID)
	if err := os.MkdirAll(filepath.Join(artifactRoot, "logs"), 0o755); err != nil {
		return Result{}, err
	}
	controlDir := filepath.Join(r.cfg.ScanPath, ".qa-control")
	released, releaseErr := assets.Release(controlDir)
	toolVersions, _ := json.Marshal(map[string]any{"assets": released})
	if releaseErr != nil {
		toolVersions, _ = json.Marshal(map[string]any{"assets_error": releaseErr.Error()})
	}
	staticOnly := opts.StaticOnly || r.cfg.Pipeline.StaticOnly
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
		return Result{}, err
	}
	stages := initialStages(selectedStages(opts, staticOnly), staticOnly)
	if err := r.writeRunManifest(run, project, opts, released, releaseErr); err != nil {
		return Result{}, err
	}
	if err := r.writeStageStatus(runID, artifactRoot, stages); err != nil {
		return Result{}, err
	}
	preflightResult := preflight.Run(ctx, r.exec, r.cfg)
	preflightPath := filepath.Join(artifactRoot, "preflight.json")
	_ = writeJSON(preflightPath, preflightResult)

	results := map[string]model.StageRecord{}
	for index := range stages {
		stage := stages[index].Stage
		if stages[index].Status == model.StageSkipped {
			record := r.materializeSkippedStage(run, stages[index])
			stages[index] = record
			results[stage] = record
			_ = r.store.PutStage(ctx, runID, record)
			_ = r.writeStageStatus(runID, artifactRoot, stages)
			continue
		}
		record := r.executeStage(ctx, run, project, stage, results, opts, preflightResult)
		if stage != "F" {
			record.Findings = assignFindingIDs(stage, record.Findings)
		}
		stages[index] = record
		results[stage] = record
		_ = r.store.PutStage(ctx, runID, record)
		_ = r.store.InsertFindings(ctx, runID, record.Findings)
		_ = r.writeStageStatus(runID, artifactRoot, stages)
	}

	status := runStatus(stages)
	if err := r.store.FinishRun(ctx, runID, taskID, status, time.Since(start)); err != nil {
		return Result{}, err
	}
	run.Status = status
	run.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	run.DurationMS = time.Since(start).Milliseconds()
	return Result{Run: run, Stages: stages}, nil
}

func (r Runner) executeStage(ctx context.Context, run model.RunRecord, project scanner.Project, stage string, prior map[string]model.StageRecord, opts RunOptions, preflightResult preflight.CheckResult) model.StageRecord {
	if check, ok := preflightResult.BlockingCheck(stage); ok {
		return r.materializeSkippedStage(run, blockedStage(stage, stageName(stage), nil, check.Message))
	}
	switch stage {
	case "A":
		return r.stageA(run, project)
	case "B":
		if prior["A"].Status == model.StageFailed {
			return r.materializeSkippedStage(run, blockedStage("B", stageName("B"), []string{"A"}, "Stage A failed; runtime evidence is blocked."))
		}
		return r.stageB(ctx, run, project)
	case "C":
		if prior["B"].Status != model.StageDone {
			return r.materializeSkippedStage(run, blockedStage("C", stageName("C"), []string{"B"}, "Stage C requires successful Docker/runtime evidence from B."))
		}
		return r.stageC(ctx, run, project)
	case "D":
		return r.stageCodex(ctx, run, project, opts, "D", "tests_coverage_report.md", "tests_coverage_report.md", "4_测试有效性报告_api端点真实性.md")
	case "E":
		return r.stageCodex(ctx, run, project, opts, "E", "static_acceptance_audit.md", "static_acceptance_audit_report.md", "1_质检AI测试报告.md")
	case "F":
		return r.stageF(run, prior)
	default:
		return skippedStage(stage, "Unknown stage.")
	}
}

func selectedStages(opts RunOptions, staticOnly bool) map[string]bool {
	selected := map[string]bool{"A": true, "B": true, "C": true, "D": true, "E": true, "F": true}
	if len(opts.Stages) > 0 {
		selected = map[string]bool{"F": true}
		for _, stage := range opts.Stages {
			selected[strings.ToUpper(stage)] = true
		}
		return selected
	}
	if staticOnly {
		return map[string]bool{"A": true, "D": true, "E": true, "F": true}
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
	switch stage {
	case "A":
		return "structure and rules check"
	case "B":
		return "Docker runtime evidence"
	case "C":
		return "run_tests runtime evidence"
	case "D":
		return "tests effectiveness static review"
	case "E":
		return "static acceptance audit"
	case "F":
		return "repair summary and short_comment"
	default:
		return "unknown"
	}
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

func (r Runner) writeRunManifest(run model.RunRecord, project scanner.Project, opts RunOptions, released []assets.ReleasedFile, releaseErr error) error {
	manifest := map[string]any{
		"run_id":           run.RunID,
		"task_id":          run.TaskID,
		"started_at":       run.StartedAt,
		"project_path":     project.Path,
		"static_only":      run.StaticOnly,
		"stage":            opts.Stage,
		"from":             opts.From,
		"stages":           opts.Stages,
		"qa_mode":          opts.Mode,
		"ref_run":          opts.RefRun,
		"extra_docs":       opts.ExtraDocs,
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
			"home_reuse_strategy": "per-stage .codex-home-D/.codex-home-E paths, removed and recreated before each stage",
			"env_keys":            configuredEnvKeys(r.cfg.Codex.Env),
			"extra_args":          r.cfg.Codex.ExtraArgs,
			"docker_socket":       "not mounted",
		},
	}
	if releaseErr != nil {
		manifest["asset_release_error"] = releaseErr.Error()
	}
	return writeJSON(filepath.Join(run.ArtifactRoot, "run_manifest.json"), manifest)
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
