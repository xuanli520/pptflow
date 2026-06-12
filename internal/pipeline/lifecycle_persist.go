package pipeline

import (
	"path/filepath"
	"strconv"
	"strings"

	"github.com/xuanli520/p2r_tui/assets"
	"github.com/xuanli520/p2r_tui/internal/displaytime"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/scanner"
	"github.com/xuanli520/p2r_tui/internal/taskdocs"
)

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
		"keep_runtime":          opts.KeepRuntime || r.cfg.Docker.KeepRuntime,
		"defer_runtime_cleanup": opts.DeferRuntimeCleanup,
		"self_test_report":      r.cfg.Pipeline.SelfTestReportPath,
		"preflight":             "preflight.json",
		"run_failure_summary":   "run_failure_summary.md",
		"stage_timeouts":        r.cfg.Pipeline.StageTimeouts,
		"tool_versions":         map[string]string{"p2r": "dev"},
		"assets":                released,
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
			"defer_runtime_cleanup":   opts.DeferRuntimeCleanup,
		},
		"docker_runtime": map[string]any{
			"summary":     "docker_runtime_summary.json",
			"pull_policy": r.cfg.Docker.PullPolicy,
		},
		"docker_mirror": map[string]any{
			"summary":              "docker_mirror_summary.json",
			"mode":                 r.cfg.Docker.BuildMirrors.Mode,
			"enabled":              r.cfg.Docker.BuildMirrors.Enabled,
			"fallback_to_original": r.cfg.Docker.BuildMirrors.FallbackToOriginal,
		},
		"docker_gc": map[string]any{
			"summary":  filepath.Join(r.cfg.ScanPath, ".qa-control", "docker_gc_summary.json"),
			"enabled":  r.cfg.Docker.GC.Enabled,
			"p2r_only": r.cfg.Docker.GC.P2ROnly,
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
	writer := NewArtifactWriter(artifactRoot)
	if err := writer.RequiredJSON("stage_status.json", model.StageStatusFile{RunID: runID, Stages: stages}); err != nil {
		return err
	}
	summary := runFailureSummary(runID, stages)
	if err := writer.RequiredJSON("run_failure_summary.json", summary); err != nil {
		return err
	}
	return writer.RequiredText("run_failure_summary.md", runFailureSummaryMarkdown(summary))
}

func runStatus(stages []model.StageRecord) string {
	for _, stage := range stages {
		if stage.Status == model.StageFailed || stage.Status == model.StageBlocked || len(stage.Findings) > 0 {
			return model.RunCompletedWithFindings
		}
	}
	return model.RunCompletedClean
}

type runFailureSummaryPayload struct {
	SchemaVersion         string                     `json:"schema_version"`
	RunID                 string                     `json:"run_id"`
	PrimaryStage          string                     `json:"primary_stage,omitempty"`
	PrimaryStatus         string                     `json:"primary_status,omitempty"`
	PrimaryErrorSummary   string                     `json:"primary_error_summary,omitempty"`
	AcceptanceReportScope string                     `json:"acceptance_report_scope"`
	Failures              []runFailureStageSummary   `json:"failures"`
	Stages                []runFailureStatusSnapshot `json:"stages"`
}

type runFailureStageSummary struct {
	Stage        string `json:"stage"`
	Status       string `json:"status"`
	ErrorSummary string `json:"error_summary,omitempty"`
	FindingCount int    `json:"finding_count,omitempty"`
}

type runFailureStatusSnapshot struct {
	Stage        string `json:"stage"`
	Status       string `json:"status"`
	ErrorSummary string `json:"error_summary,omitempty"`
	FindingCount int    `json:"finding_count,omitempty"`
}

func runFailureSummary(runID string, stages []model.StageRecord) runFailureSummaryPayload {
	summary := runFailureSummaryPayload{
		SchemaVersion:         "p2r.run_failure_summary.v1",
		RunID:                 runID,
		AcceptanceReportScope: "acceptance_report.md is Stage A static acceptance evidence only; use run_failure_summary.json and stage_status.json for run-level failure aggregation.",
		Failures:              []runFailureStageSummary{},
		Stages:                []runFailureStatusSnapshot{},
	}
	for _, stage := range stages {
		snapshot := runFailureStatusSnapshot{
			Stage:        stage.Stage,
			Status:       stage.Status,
			ErrorSummary: strings.TrimSpace(stage.ErrorSummary),
			FindingCount: len(stage.Findings),
		}
		summary.Stages = append(summary.Stages, snapshot)
		if stage.Status == model.StageFailed || stage.Status == model.StageBlocked || len(stage.Findings) > 0 {
			summary.Failures = append(summary.Failures, runFailureStageSummary(snapshot))
		}
	}
	if primary, ok := primaryRunFailureStage(stages); ok {
		summary.PrimaryStage = primary.Stage
		summary.PrimaryStatus = primary.Status
		summary.PrimaryErrorSummary = strings.TrimSpace(primary.ErrorSummary)
	}
	return summary
}

func primaryRunFailureStage(stages []model.StageRecord) (model.StageRecord, bool) {
	bestPriority := 0
	var best model.StageRecord
	for _, stage := range stages {
		priority := runFailureStagePriority(stage)
		if priority == 0 {
			continue
		}
		if bestPriority == 0 || priority < bestPriority {
			bestPriority = priority
			best = stage
		}
	}
	return best, bestPriority != 0
}

func runFailureStagePriority(stage model.StageRecord) int {
	switch stage.Status {
	case model.StageFailed:
		switch stage.Stage {
		case string(model.StageB):
			return 1
		case string(model.StageG):
			return 2
		case string(model.StageC):
			return 3
		default:
			return 10 + runFailureStageOrder(stage.Stage)
		}
	case model.StageBlocked:
		switch stage.Stage {
		case string(model.StageB):
			return 30
		case string(model.StageG):
			return 31
		case string(model.StageC):
			return 32
		default:
			return 40 + runFailureStageOrder(stage.Stage)
		}
	default:
		if len(stage.Findings) > 0 {
			return 60 + runFailureStageOrder(stage.Stage)
		}
		return 0
	}
}

func runFailureStageOrder(stage string) int {
	for index, item := range model.AllStages() {
		if item == stage {
			return index
		}
	}
	return 99
}

func runFailureSummaryMarkdown(summary runFailureSummaryPayload) string {
	var builder strings.Builder
	builder.WriteString("# Run Failure Summary\n\n")
	builder.WriteString("This is the run-level failure aggregation. `acceptance_report.md` is Stage A static acceptance evidence only.\n\n")
	if summary.PrimaryStage == "" {
		builder.WriteString("Primary failure: none\n")
	} else {
		builder.WriteString("Primary failure: Stage " + summary.PrimaryStage + " " + summary.PrimaryStatus)
		if summary.PrimaryErrorSummary != "" {
			builder.WriteString(" - " + summary.PrimaryErrorSummary)
		}
		builder.WriteString("\n")
	}
	if len(summary.Failures) > 0 {
		builder.WriteString("\n## Failed Or Finding Stages\n")
		for _, item := range summary.Failures {
			builder.WriteString("- " + item.Stage + " " + item.Status)
			if item.ErrorSummary != "" {
				builder.WriteString(": " + item.ErrorSummary)
			}
			if item.FindingCount > 0 {
				builder.WriteString(" findings=" + strconv.Itoa(item.FindingCount))
			}
			builder.WriteString("\n")
		}
	}
	builder.WriteString("\n## Stage Statuses\n")
	for _, item := range summary.Stages {
		builder.WriteString("- " + item.Stage + ": " + item.Status + "\n")
	}
	return builder.String()
}
