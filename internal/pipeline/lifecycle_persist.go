package pipeline

import (
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
