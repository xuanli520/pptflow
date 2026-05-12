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
	Record  model.StageRecord
	Runtime *RuntimeState
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
	return StageOutcome{Record: s.runner.stageC(ctx, sc.Run, sc.Project, sc.Runtime, sc.Progress)}
}

func (r Runner) executeStage(ctx context.Context, run model.RunRecord, project scanner.Project, stage string, prior map[string]model.StageRecord, opts RunOptions, preflightResult preflight.CheckResult, runtime RuntimeState, progress func(RunProgress)) StageOutcome {
	if check, ok := preflightResult.BlockingCheck(stage); ok {
		if stage == "D" || stage == "E" || stage == "F" {
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
	switch stage {
	case "A":
		return stageAAdapter{runner: r}.Execute(ctx, sc)
	case "B":
		return stageBAdapter{runner: r}.Execute(ctx, sc)
	case "C":
		return stageCAdapter{runner: r}.Execute(ctx, sc)
	case "D":
		if opts.Mode == "recheck" {
			return StageOutcome{Record: r.stageCodex(ctx, run, project, opts, "D", "tests_coverage_report.md", "test_effectiveness_verification.md", progress)}
		}
		return StageOutcome{Record: r.stageCodex(ctx, run, project, opts, "D", "tests_coverage_report.md", "test_effectiveness_report.md", progress)}
	case "E":
		if opts.Mode == "recheck" {
			return StageOutcome{Record: r.stageCodex(ctx, run, project, opts, "E", "static_acceptance_audit.md", "codex_report_verification.md", progress)}
		}
		return StageOutcome{Record: r.stageCodex(ctx, run, project, opts, "E", "static_acceptance_audit.md", "codex_report.md", progress)}
	case "F":
		return StageOutcome{Record: r.stageF(ctx, run, project, opts, prior, progress)}
	default:
		return StageOutcome{Record: skippedStage(stage, "Unknown stage.")}
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
