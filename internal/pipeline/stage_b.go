package pipeline

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/xuanli520/p2r_tui/internal/config"
	dockermgr "github.com/xuanli520/p2r_tui/internal/docker"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/scanner"
)

func (r Runner) stageB(ctx context.Context, run model.RunRecord, project scanner.Project, progress func(RunProgress)) StageOutcome {
	start := time.Now()
	record := startStage("B")
	logPath := filepath.Join(run.ArtifactRoot, "logs", "B_docker.log")
	record.LogPath = logPath
	writer := NewArtifactWriter(run.ArtifactRoot)
	portMapPath := filepath.Join(run.ArtifactRoot, "port_map.json")
	runtimeSummaryPath := filepath.Join(run.ArtifactRoot, "docker_runtime_summary.json")
	mirrorSummaryPath := filepath.Join(run.ArtifactRoot, "docker_mirror_summary.json")
	effectiveConfigPath := filepath.Join(run.ArtifactRoot, "docker_compose_effective_config.yml")
	stageCProxyConfigPath := filepath.Join(run.ArtifactRoot, "docker_compose_stage_c_proxy_config.yml")
	stageCProxyPath := filepath.Join(run.ArtifactRoot, "p2r_stage_c_proxy.json")
	stageCPortsEnvPath := filepath.Join(run.ArtifactRoot, "p2r_ports.env")
	screenshotPath := qaArtifactPath(run.ArtifactRoot, "docker_startup.png")
	record.ArtifactPaths = append(record.ArtifactPaths, portMapPath, runtimeSummaryPath, mirrorSummaryPath, effectiveConfigPath, stageCProxyConfigPath, stageCProxyPath, stageCPortsEnvPath, screenshotPath)
	repoPath := filepath.Join(project.Path, "repo")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		recordArtifactWarning(&record, newArtifactWarning(writer.RelativePath(logPath), "write_text", false, err))
		logFile = nil
	}
	if logFile != nil {
		defer logFile.Close()
	}
	logWriter := io.Writer(io.Discard)
	if logFile != nil {
		logWriter = logFile
	}
	service := dockermgr.Service{Exec: r.exec, Config: r.cfg.Docker}
	result, startErr := service.StartRuntime(ctx, dockermgr.StartRuntimeRequest{
		ProjectPath:  project.Path,
		RepoPath:     repoPath,
		ArtifactRoot: run.ArtifactRoot,
		TaskID:       project.TaskID,
		RunID:        run.RunID,
		Labels:       runtimeLabels(r.cfg.Docker, project.TaskID, run.RunID),
		Env:          dockerCommandEnv(),
		Log:          logWriter,
		Progress: func(event dockermgr.ProgressEvent) {
			appendStreamProgress(run.RunID, "B", event.Line, event.Source, event.Done, progress)
		},
		Timeouts: dockermgr.RuntimeTimeouts{
			Pull:   r.stageTimeout("B_PULL", 300),
			Build:  r.stageTimeout("B_BUILD", 600),
			Up:     r.stageTimeout("B_UP", 300),
			Health: r.stageTimeout("B_HEALTH", 60),
			Port:   r.stageTimeout("B_PORT", 30),
		},
	})
	if result.RuntimeSummary.RunID == "" {
		result.RuntimeSummary.RunID = run.RunID
		result.RuntimeSummary.TaskID = project.TaskID
		result.RuntimeSummary.PullPolicy = r.cfg.Docker.PullPolicy
	}
	if result.MirrorSummary.Mode == "" {
		result.MirrorSummary.Enabled = r.cfg.Docker.BuildMirrors.Enabled
		result.MirrorSummary.Mode = r.cfg.Docker.BuildMirrors.Mode
		result.MirrorSummary.Profile = r.cfg.Docker.BuildMirrors.Profile
	}
	bestEffortStageJSON(&record, writer, "docker_runtime_summary.json", result.RuntimeSummary)
	bestEffortStageJSON(&record, writer, "docker_mirror_summary.json", result.MirrorSummary)
	if strings.TrimSpace(result.EffectiveConfigContent) != "" {
		bestEffortStageText(&record, writer, "docker_compose_effective_config.yml", result.EffectiveConfigContent)
	}
	mergeDockerRuntimeIntoManifest(run.ArtifactRoot, result)
	cleanupMeta := cleanupMetaFromRuntime(run.RunID, project.TaskID, result.Runtime, r.cfg.Docker)
	if startErr != nil {
		runtimeErr, _ := startErr.(*dockermgr.StartRuntimeError)
		evidence := startErr.Error()
		fix := "Fix Docker runtime startup and rerun stage B."
		if runtimeErr != nil && runtimeErr.Fix != "" {
			fix = runtimeErr.Fix
		}
		category := "docker_runtime_failed"
		if runtimeErr != nil && runtimeErr.Category != "" {
			category = runtimeErr.Category
		}
		record.ErrorSummary = category + ": " + evidence
		return stageBFailureOutcome(r.failB(record, start, logPath, portMapPath, screenshotPath, cleanupMeta, evidence, fix), cleanupMeta)
	}
	portMap := map[string]any{
		"run_id":                run.RunID,
		"compose_project":       result.Runtime.ComposeProject,
		"compose_file":          result.Runtime.ComposeFile,
		"compose_files":         result.Runtime.ComposeFiles,
		"work_dir":              result.Runtime.WorkDir,
		"services":              result.Runtime.Services,
		"mappings":              result.Runtime.Mappings,
		"probes":                result.Runtime.Probes,
		"mirror":                result.Runtime.Mirror,
		"labels":                runtimeLabels(r.cfg.Docker, project.TaskID, run.RunID),
		"cleanup_command":       dockerCleanupCommandText(r.cfg.Docker, result.Runtime.ComposeFiles, result.Runtime.ComposeProject, result.Runtime.WorkDir),
		"runtime_summary":       "docker_runtime_summary.json",
		"docker_mirror_summary": "docker_mirror_summary.json",
		"stage_timeouts": map[string]int{
			"B_PULL":   int(r.stageTimeout("B_PULL", 300).Seconds()),
			"B_BUILD":  int(r.stageTimeout("B_BUILD", 600).Seconds()),
			"B_UP":     int(r.stageTimeout("B_UP", 300).Seconds()),
			"B_HEALTH": int(r.stageTimeout("B_HEALTH", 60).Seconds()),
			"B_PORT":   int(r.stageTimeout("B_PORT", 30).Seconds()),
		},
	}
	if err := writer.RequiredJSON("port_map.json", portMap); err != nil {
		record = recordArtifactWriteError(record, err, portMapPath)
		return StageOutcome{Record: finishStage(record, model.StageFailed, start), Runtime: &result.Runtime}
	}
	bestEffortStageCProxyArtifacts(&record, writer, result.Runtime, repoPath, run.ArtifactRoot, r.cfg.Pipeline.StageC, result.StageCProxyConfigContent)
	pages, _ := renderLogFile(logPath, screenshotPath)
	record.ArtifactPaths = []string{portMapPath, runtimeSummaryPath, mirrorSummaryPath}
	if strings.TrimSpace(result.EffectiveConfigContent) != "" {
		record.ArtifactPaths = append(record.ArtifactPaths, effectiveConfigPath)
	}
	if strings.TrimSpace(result.StageCProxyConfigContent) != "" {
		record.ArtifactPaths = append(record.ArtifactPaths, stageCProxyConfigPath)
	}
	record.ArtifactPaths = append(record.ArtifactPaths, stageCProxyPath, stageCPortsEnvPath)
	record.ArtifactPaths = append(record.ArtifactPaths, pages...)
	if result.RuntimeSummary.PortCollection.Status == "failed" && !result.Runtime.HasServiceMappings() {
		record.Findings = []model.Finding{{
			Stage:      "B",
			Severity:   "High",
			Title:      "Docker started but port inspection failed",
			Rule:       "B evidence requires Docker/Compose inspection.",
			Evidence:   "port_inspection_failed",
			Impact:     "Runtime ports could not be recorded in port_map.json.",
			MinimumFix: "Ensure docker compose ps --format json works for the project.",
		}}
		record.ErrorSummary = "port inspection failed"
		return StageOutcome{Record: finishStage(record, model.StageFailed, start), Runtime: &result.Runtime}
	}
	if !result.Runtime.HasServiceMappings() {
		record.Findings = []model.Finding{{
			Stage:      "B",
			Severity:   "High",
			Title:      "Docker port mappings were empty",
			Rule:       "Stage B must record real Docker/Compose port mappings.",
			Evidence:   "docker compose ps and docker port fallback returned no published ports.",
			Impact:     "External browser/runtime evidence cannot be collected from mapped host ports.",
			MinimumFix: "Expose the service ports in docker-compose.yml and rerun B.",
		}}
		record.ErrorSummary = "no published ports"
		return StageOutcome{Record: finishStage(record, model.StageFailed, start), Runtime: &result.Runtime}
	}
	return StageOutcome{Record: finishStage(record, model.StageDone, start), Runtime: &result.Runtime}
}

func composeMetaFromRuntime(runtime RuntimeState) model.ComposeMeta {
	return model.ComposeMeta{
		Project:      runtime.ComposeProject,
		ComposeFiles: append([]string(nil), runtime.ComposeFiles...),
		WorkDir:      runtime.WorkDir,
		Ports:        servicePortsFromRuntime(runtime),
	}
}

func servicePortsFromRuntime(runtime RuntimeState) []model.ServicePort {
	names := append([]string(nil), runtime.Services...)
	seen := map[string]bool{}
	for _, name := range names {
		seen[name] = true
	}
	var extra []string
	for name := range runtime.Mappings {
		if !seen[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	names = append(names, extra...)
	var ports []model.ServicePort
	for _, service := range names {
		for _, mapping := range runtime.Mappings[service] {
			if mapping.Host == 0 {
				continue
			}
			ports = append(ports, model.ServicePort{
				Service:   service,
				URL:       servicePortURL(mapping),
				Host:      mapping.Host,
				Container: mapping.Container,
				Protocol:  mapping.Protocol,
			})
		}
	}
	return ports
}

func servicePortURL(mapping portMapping) string {
	if mapping.Host == 0 {
		return ""
	}
	scheme := "http"
	if mapping.Container == 443 || mapping.Host == 443 {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s:%d", scheme, normalizeHost(mapping.URL), mapping.Host)
}

func firstFrontendURL(runtime RuntimeState) string {
	for _, service := range runtime.Services {
		if url := preferredServiceURL(service, runtime.Mappings[service], runtime.Probes); url != "" {
			return url
		}
	}
	for service, mappings := range runtime.Mappings {
		if url := preferredServiceURL(service, mappings, runtime.Probes); url != "" {
			return url
		}
	}
	return ""
}

func stageBFailureOutcome(record model.StageRecord, meta map[string]any) StageOutcome {
	runtime := runtimeStateFromCleanupMeta(meta)
	if !runtime.HasCleanupTarget() {
		return StageOutcome{Record: record, SkipNextStage: true}
	}
	return StageOutcome{Record: record, Runtime: &runtime}
}

func mergeDockerRuntimeIntoManifest(artifactRoot string, result dockermgr.StartRuntimeResult) {
	manifestPath := filepath.Join(artifactRoot, "run_manifest.json")
	_ = mergeCleanupIntoManifest(manifestPath, "docker_runtime", map[string]any{
		"summary":         "docker_runtime_summary.json",
		"pull_policy":     result.RuntimeSummary.PullPolicy,
		"compose_project": result.RuntimeSummary.ComposeProject,
		"compose_file":    result.RuntimeSummary.ComposeFile,
		"compose_files":   result.RuntimeSummary.ComposeFiles,
		"pull_status":     result.RuntimeSummary.Pull.Status,
		"build_status":    result.RuntimeSummary.Build.Status,
		"fallback_used":   result.RuntimeSummary.Build.FallbackUsed,
	})
	_ = mergeCleanupIntoManifest(manifestPath, "docker_mirror", map[string]any{
		"summary":         "docker_mirror_summary.json",
		"enabled":         result.MirrorSummary.Enabled,
		"mode":            result.MirrorSummary.Mode,
		"fallback_used":   result.MirrorSummary.FallbackUsed,
		"fallback_reason": result.MirrorSummary.FallbackReason,
		"compose_files":   result.MirrorSummary.ComposeFiles,
		"override_file":   result.MirrorSummary.OverrideFile,
	})
}

func runtimeStateFromCleanupMeta(meta map[string]any) RuntimeState {
	if len(meta) == 0 {
		return RuntimeState{}
	}
	return RuntimeState{
		ComposeProject: strings.TrimSpace(fmt.Sprint(meta["compose_project"])),
		ComposeFile:    strings.TrimSpace(fmt.Sprint(meta["compose_file"])),
		ComposeFiles:   metaComposeFiles(meta),
		WorkDir:        strings.TrimSpace(fmt.Sprint(meta["work_dir"])),
	}.Normalized()
}

func (r Runner) failB(record model.StageRecord, start time.Time, logPath, portMapPath, screenshotPath string, meta map[string]any, evidence, fix string) model.StageRecord {
	writer := NewArtifactWriter(filepath.Dir(portMapPath))
	if fileExists(logPath) {
		bestEffortStageAppend(&record, writer, writer.RelativePath(logPath), "\nERROR SUMMARY:\n"+evidence+"\n")
	} else {
		bestEffortStageText(&record, writer, writer.RelativePath(logPath), evidence)
	}
	payload := map[string]any{"mappings": map[string]any{}, "reason": evidence}
	for key, value := range meta {
		payload[key] = value
	}
	if err := writer.RequiredJSON(filepath.Base(portMapPath), payload); err != nil {
		record = recordArtifactWriteError(record, err, portMapPath)
	}
	pages, _ := renderLogFile(logPath, screenshotPath)
	record.ArtifactPaths = []string{portMapPath}
	for _, name := range []string{"docker_runtime_summary.json", "docker_mirror_summary.json", "docker_compose_effective_config.yml"} {
		path := filepath.Join(filepath.Dir(portMapPath), name)
		if fileExists(path) {
			record.ArtifactPaths = append(record.ArtifactPaths, path)
		}
	}
	record.ArtifactPaths = append(record.ArtifactPaths, pages...)
	record.Findings = append(record.Findings, model.Finding{
		Stage:      "B",
		Severity:   "High",
		Title:      "Docker runtime evidence was not collected",
		Rule:       "Stage B must collect runtime evidence from Docker/Compose.",
		Evidence:   evidence,
		Impact:     "Stage C runtime tests cannot run from this evidence chain.",
		MinimumFix: fix,
	})
	if record.ErrorSummary == "" {
		record.ErrorSummary = evidence
	}
	return finishStage(record, model.StageFailed, start)
}

func cleanupMetaFromRuntime(runID, taskID string, runtime RuntimeState, cfg config.DockerConfig) map[string]any {
	meta := map[string]any{
		"run_id":          runID,
		"compose_project": runtime.ComposeProject,
		"compose_file":    runtime.ComposeFile,
		"compose_files":   runtime.ComposeFiles,
		"work_dir":        runtime.WorkDir,
		"labels":          runtimeLabels(cfg, taskID, runID),
	}
	return meta
}

func runtimeLabels(cfg config.DockerConfig, taskID, runID string) map[string]string {
	labels := map[string]string{
		"p2r.task_id": taskID,
		"p2r.run_id":  runID,
	}
	key, value, ok := strings.Cut(cfg.ManagedLabel, "=")
	if ok {
		labels[strings.TrimSpace(key)] = strings.TrimSpace(value)
	} else if strings.TrimSpace(cfg.ManagedLabel) != "" {
		labels[strings.TrimSpace(cfg.ManagedLabel)] = "true"
	}
	return labels
}

func metaComposeFiles(meta map[string]any) []string {
	raw, ok := meta["compose_files"]
	if !ok {
		return nil
	}
	switch typed := raw.(type) {
	case []string:
		return append([]string(nil), typed...)
	case []any:
		files := make([]string, 0, len(typed))
		for _, item := range typed {
			if text := strings.TrimSpace(fmt.Sprint(item)); text != "" {
				files = append(files, text)
			}
		}
		return files
	default:
		return nil
	}
}

func dockerCleanupCommandText(cfg config.DockerConfig, composeFiles []string, projectName, workDir string) string {
	return dockermgr.CommandLine("docker", dockermgr.CleanupComposeArgsFilesWithProjectDir(cfg, composeFiles, projectName, workDir))
}
