package pipeline

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/preflight"
	"github.com/xuanli520/p2r_tui/internal/scanner"
)

type Stage interface {
	ID() string
	Execute(context.Context, StageContext) StageOutcome
}

type StageContext struct {
	Run       model.RunRecord
	Project   scanner.Project
	Options   RunOptions
	Prior     map[string]model.StageRecord
	Preflight preflight.CheckResult
	Runtime   RuntimeState
	Progress  func(RunProgress)
	Writer    ArtifactWriter
	Timeout   func(key string, fallbackSeconds int) time.Duration
}

type StageOutcome struct {
	Record            model.StageRecord
	Runtime           *RuntimeState
	BlockedDependents []string
}

type preflightMaterializingStage interface {
	MaterializesBlockedPreflight() bool
}

type stageAAdapter struct {
	runner Runner
}

func (s stageAAdapter) ID() string {
	return string(model.StageA)
}

func (s stageAAdapter) Execute(ctx context.Context, sc StageContext) StageOutcome {
	return StageOutcome{Record: s.runner.stageA(ctx, sc.Run, sc.Project, sc.Progress)}
}

type stageBAdapter struct {
	runner Runner
}

func (s stageBAdapter) ID() string {
	return string(model.StageB)
}

func (s stageBAdapter) Execute(ctx context.Context, sc StageContext) StageOutcome {
	return s.runner.stageB(ctx, sc.Run, sc.Project, sc.Progress)
}

type stageCAdapter struct {
	runner Runner
}

func (s stageCAdapter) ID() string {
	return string(model.StageC)
}

func (s stageCAdapter) Execute(ctx context.Context, sc StageContext) StageOutcome {
	return StageOutcome{Record: s.runner.stageC(ctx, sc.Run, sc.Project, sc.Runtime, sc.Prior, sc.Progress)}
}

type stageGAdapter struct {
	runner Runner
}

func (s stageGAdapter) ID() string {
	return string(model.StageG)
}

func (s stageGAdapter) Execute(ctx context.Context, sc StageContext) StageOutcome {
	return StageOutcome{Record: s.runner.stageG(ctx, sc)}
}

func (r Runner) stageRegistry() map[string]Stage {
	stages := map[string]Stage{
		string(model.StageA): stageAAdapter{runner: r},
		string(model.StageB): stageBAdapter{runner: r},
		string(model.StageC): stageCAdapter{runner: r},
		string(model.StageG): stageGAdapter{runner: r},
	}
	for _, spec := range codexReviewStageSpecs() {
		stages[spec.ID] = CodexReviewStage{runner: r, spec: spec}
	}
	return stages
}

func (r Runner) executeStage(ctx context.Context, run model.RunRecord, project scanner.Project, stage string, prior map[string]model.StageRecord, opts RunOptions, preflightResult preflight.CheckResult, runtime RuntimeState, progress func(RunProgress)) StageOutcome {
	registry := r.stageRegistry()
	stageImpl, known := registry[stage]
	if check, ok := preflightResult.BlockingCheck(stage); ok {
		if stageMaterializesBlockedPreflight(stageImpl) {
			// Static stages materialize their own unavailable-review reports so
			// the external QA artifact contract stays intact.
		} else {
			return StageOutcome{Record: r.materializeSkippedStage(run, blockedStage(stage, stageName(stage), nil, check.Message))}
		}
	}
	sc := StageContext{
		Run:       run,
		Project:   project,
		Options:   opts,
		Prior:     prior,
		Preflight: preflightResult,
		Runtime:   runtime,
		Progress:  progress,
		Writer:    NewArtifactWriter(run.ArtifactRoot),
		Timeout:   r.stageTimeout,
	}
	if !known {
		return StageOutcome{Record: skippedStage(stage, "Unknown stage.")}
	}
	return stageImpl.Execute(ctx, sc)
}

func stageMaterializesBlockedPreflight(stage Stage) bool {
	materializing, ok := stage.(preflightMaterializingStage)
	return ok && materializing.MaterializesBlockedPreflight()
}

func selectedStages(opts RunOptions, staticOnly bool) map[string]bool {
	selected := stageSet(defaultRunStages())
	if len(opts.Stages) > 0 {
		selected = map[string]bool{}
		for _, stage := range opts.Stages {
			if normalized, ok := model.NormalizeStage(stage); ok {
				selected[normalized] = true
			}
		}
		return filterRuntimeStages(selected, staticOnly)
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
		return filterRuntimeStages(selected, staticOnly)
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
		return filterRuntimeStages(selected, staticOnly)
	}
	if staticOnly {
		return staticStageSet()
	}
	return filterRuntimeStages(selected, staticOnly)
}

func initialStages(selected map[string]bool, staticOnly bool) []model.StageRecord {
	stages := make([]model.StageRecord, 0, len(model.AllStages()))
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
			reason = "--static-only skips runtime evidence stages."
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

func filterRuntimeStages(selected map[string]bool, staticOnly bool) map[string]bool {
	if !staticOnly {
		return selected
	}
	filtered := map[string]bool{}
	for stage, ok := range selected {
		if ok && !model.IsRuntimeStage(stage) {
			filtered[stage] = true
		}
	}
	return filtered
}

func defaultRunStages() []string {
	stages := make([]string, 0, len(model.AllStages()))
	for _, stage := range model.AllStages() {
		stages = append(stages, stage)
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

func blockedDependents(stage string) []string {
	switch strings.ToUpper(strings.TrimSpace(stage)) {
	case string(model.StageB):
		return []string{string(model.StageG), string(model.StageC)}
	default:
		return nil
	}
}

func (r Runner) materializeSkippedStage(run model.RunRecord, record model.StageRecord) model.StageRecord {
	writer := NewArtifactWriter(run.ArtifactRoot)
	switch record.Stage {
	case "B":
		logPath := filepath.Join(run.ArtifactRoot, "logs", "B_docker.log")
		portMapPath := filepath.Join(run.ArtifactRoot, "port_map.json")
		screenshotPath := qaArtifactPath(run.ArtifactRoot, "docker_startup.png")
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
		screenshotPath := qaArtifactPath(run.ArtifactRoot, "run_tests_screenshot.png")
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
	case "G":
		logPath := filepath.Join(run.ArtifactRoot, "logs", "G_frontend_e2e.log")
		screenshotPath := qaArtifactPath(run.ArtifactRoot, "frontend_e2e_screenshot.png")
		summaryPath := filepath.Join(run.ArtifactRoot, "frontend_e2e_summary.json")
		reportPath := qaArtifactPath(run.ArtifactRoot, "frontend_e2e_report.md")
		reason := record.ErrorSummary
		if reason == "" {
			reason = "Stage G was not executed."
		}
		if err := writer.RequiredText("logs/G_frontend_e2e.log", reason); err != nil {
			record = recordArtifactWriteError(record, err, logPath)
		}
		if err := writer.RequiredJSON("frontend_e2e_summary.json", map[string]any{"schema_version": frontendE2ESchemaVersion, "status": "blocked", "reason": reason}); err != nil {
			record = recordArtifactWriteError(record, err, summaryPath)
		}
		if err := writer.RequiredText("frontend_e2e_report.md", "# Browser Frontend E2E\n\n"+reason+"\n"); err != nil {
			record = recordArtifactWriteError(record, err, reportPath)
		}
		pages, _ := renderTerminalLog(reason, screenshotPath)
		record.LogPath = logPath
		record.ArtifactPaths = append([]string{logPath, summaryPath, reportPath}, pages...)
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
