package pipeline

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	dockermgr "github.com/xuanli520/p2r_tui/internal/docker"
	"github.com/xuanli520/p2r_tui/internal/executor"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/scanner"
)

func (r Runner) stageB(ctx context.Context, run model.RunRecord, project scanner.Project) model.StageRecord {
	start := time.Now()
	record := startStage("B")
	logPath := filepath.Join(run.ArtifactRoot, "logs", "B_docker.log")
	record.LogPath = logPath
	writer := NewArtifactWriter(run.ArtifactRoot)
	portMapPath := filepath.Join(run.ArtifactRoot, "port_map.json")
	screenshotPath := filepath.Join(run.ArtifactRoot, "5_Docker启动截图.png")
	record.ArtifactPaths = append(record.ArtifactPaths, portMapPath, screenshotPath)
	repoPath := filepath.Join(project.Path, "repo")
	compose := findCompose(repoPath)
	readmeCommand := []string{}
	if compose == "" {
		readmeCommand = readmeComposeCommand(repoPath)
	}
	if compose == "" && len(readmeCommand) == 0 {
		return r.failB(record, start, logPath, portMapPath, screenshotPath, nil, "No docker-compose.yml or README-declared docker compose startup command found in repo/.", "Add repo/docker-compose.yml, document a docker compose startup command, or run --static-only.")
	}
	if _, err := r.exec.LookPath("docker"); err != nil {
		return r.failB(record, start, logPath, portMapPath, screenshotPath, nil, "docker executable not found on PATH.", "Install Docker or run --static-only.")
	}
	projectName := dockermgr.ComposeProjectName(r.cfg.Docker.ComposeProjectPrefix, project.TaskID, run.RunID)
	workDir := repoPath
	var pullArgs []string
	var buildArgs []string
	upArgs := []string{"compose", "-f", compose, "-p", projectName, "up", "-d"}
	psArgs := []string{"compose", "-f", compose, "-p", projectName, "ps", "--format", "json"}
	psQArgs := []string{"compose", "-f", compose, "-p", projectName, "ps", "-q"}
	servicesArgs := []string{"compose", "-f", compose, "-p", projectName, "config", "--services"}
	if compose != "" {
		workDir = filepath.Dir(compose)
		pullArgs = []string{"compose", "-f", compose, "-p", projectName, "pull", "--ignore-buildable"}
		buildArgs = []string{"compose", "-f", compose, "-p", projectName, "build"}
	} else {
		upArgs = composeArgsWithProject(readmeCommand, projectName)
		psArgs = composePSArgs(readmeCommand, projectName)
		psQArgs = composePSQArgs(readmeCommand, projectName)
		servicesArgs = composeServicesArgs(readmeCommand, projectName)
	}
	cleanupMeta := map[string]any{
		"run_id":          run.RunID,
		"compose_project": projectName,
		"compose_file":    compose,
		"work_dir":        workDir,
		"labels": map[string]string{
			"managed_by":  "p2rqa",
			"p2r.task_id": project.TaskID,
			"p2r.run_id":  run.RunID,
		},
	}
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
	runDockerStep := func(name string, timeout time.Duration, args []string, required bool) executor.Result {
		fmt.Fprintf(logWriter, "=== %s start ===\n", name)
		if len(args) == 0 {
			fmt.Fprintf(logWriter, "%s skipped\n=== %s end: skipped ===\n\n", name, name)
			return executor.Result{}
		}
		result := r.exec.RunStreaming(ctx, timeout, workDir, nil, logWriter, "docker", args...)
		fmt.Fprintf(logWriter, "\n=== %s end: exit=%d timeout=%t err=%v ===\n\n", name, result.ExitCode, result.Timeout, result.Err)
		if result.Err != nil && required {
			_, _ = renderLogFile(logPath, screenshotPath)
		}
		return result
	}
	if pullArgs != nil {
		pull := runDockerStep("B1 docker compose pull", r.stageTimeout("B_PULL", 300), pullArgs, true)
		if pull.Err != nil {
			reason := "B1 docker compose pull failed"
			if pull.Timeout {
				reason = "B1 docker compose pull timed out"
			}
			return r.failB(record, start, logPath, portMapPath, screenshotPath, cleanupMeta, reason+": "+strings.TrimSpace(firstNonEmpty(pull.Stderr, pull.Stdout)), "Fix Docker image pull or compose image declarations and rerun stage B.")
		}
	}
	if buildArgs != nil {
		build := runDockerStep("B2 docker compose build", r.stageTimeout("B_BUILD", 600), buildArgs, true)
		if build.Err != nil {
			reason := "B2 docker compose build failed"
			if build.Timeout {
				reason = "B2 docker compose build timed out"
			}
			return r.failB(record, start, logPath, portMapPath, screenshotPath, cleanupMeta, reason+": "+strings.TrimSpace(firstNonEmpty(build.Stderr, build.Stdout)), "Fix Docker build failures and rerun stage B.")
		}
	}
	up := runDockerStep("B3 docker compose up", r.stageTimeout("B_UP", 300), upArgs, true)
	if up.Err != nil {
		reason := "B3 docker compose up failed"
		if up.Timeout {
			reason = "B3 docker compose up timed out"
		}
		return r.failB(record, start, logPath, portMapPath, screenshotPath, cleanupMeta, reason+": "+strings.TrimSpace(firstNonEmpty(up.Stderr, up.Stdout)), "Fix Docker startup and rerun stage B.")
	}
	ps := runDockerStep("B5 docker compose port collection", r.stageTimeout("B_PORT", 30), psArgs, false)
	mappings, services := parseComposePS(ps.Stdout)
	if ps.Err != nil || len(mappings) == 0 {
		fallbackMappings, fallbackServices, fallbackLog := r.dockerPortFallback(ctx, workDir, psQArgs, servicesArgs)
		_, _ = logWriter.Write([]byte(fallbackLog))
		if len(fallbackMappings) > 0 {
			mappings = fallbackMappings
			services = fallbackServices
		}
	}
	fmt.Fprintln(logWriter, "=== B4 health check probe start ===")
	probes := probeMappings(mappings, minDuration(r.stageTimeout("B_HEALTH", 60), time.Duration(r.cfg.Docker.HealthCheckTimeoutSeconds)*time.Second))
	for _, probe := range probes {
		fmt.Fprintf(logWriter, "%s %s ok=%t status=%d error=%s\n", probe.Service, probe.URL, probe.OK, probe.Status, probe.Error)
	}
	fmt.Fprintln(logWriter, "=== B4 health check probe end ===")
	portMap := map[string]any{
		"run_id":          run.RunID,
		"compose_project": projectName,
		"compose_file":    compose,
		"work_dir":        workDir,
		"services":        services,
		"raw_compose_ps":  ps.Stdout,
		"mappings":        mappings,
		"probes":          probes,
		"labels": map[string]string{
			"managed_by":  "p2rqa",
			"p2r.task_id": project.TaskID,
			"p2r.run_id":  run.RunID,
		},
		"cleanup_command": dockerCleanupCommandText(compose, projectName),
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
		return finishStage(record, model.StageFailed, start)
	}
	pages, _ := renderLogFile(logPath, screenshotPath)
	record.ArtifactPaths = []string{portMapPath}
	record.ArtifactPaths = append(record.ArtifactPaths, pages...)
	if ps.Err != nil && len(mappings) == 0 {
		record.Findings = []model.Finding{{
			Stage:      "B",
			Severity:   "High",
			Title:      "Docker started but port inspection failed",
			Rule:       "B evidence requires Docker/Compose inspection.",
			Evidence:   strings.TrimSpace(ps.Stderr),
			Impact:     "Runtime ports could not be recorded in port_map.json.",
			MinimumFix: "Ensure docker compose ps --format json works for the project.",
		}}
		record.ErrorSummary = "port inspection failed"
		return finishStage(record, model.StageFailed, start)
	}
	if len(mappings) == 0 {
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
		return finishStage(record, model.StageFailed, start)
	}
	return finishStage(record, model.StageDone, start)
}

func (r Runner) dockerPortFallback(ctx context.Context, workDir string, psQArgs, servicesArgs []string) (map[string][]portMapping, []string, string) {
	var log strings.Builder
	servicesResult := r.exec.Run(ctx, 30*time.Second, workDir, nil, "docker", servicesArgs...)
	log.WriteString("\n\n" + servicesResult.Command + "\nSTDOUT:\n" + servicesResult.Stdout + "\nSTDERR:\n" + servicesResult.Stderr)
	serviceNames := splitNonEmptyLines(servicesResult.Stdout)
	psQ := r.exec.Run(ctx, 30*time.Second, workDir, nil, "docker", psQArgs...)
	log.WriteString("\n\n" + psQ.Command + "\nSTDOUT:\n" + psQ.Stdout + "\nSTDERR:\n" + psQ.Stderr)
	containers := splitNonEmptyLines(psQ.Stdout)
	mappings := map[string][]portMapping{}
	var services []string
	for index, container := range containers {
		service := container
		if index < len(serviceNames) {
			service = serviceNames[index]
		}
		services = append(services, service)
		port := r.exec.Run(ctx, 30*time.Second, workDir, nil, "docker", "port", container)
		log.WriteString("\n\n" + port.Command + "\nSTDOUT:\n" + port.Stdout + "\nSTDERR:\n" + port.Stderr)
		mappings[service] = append(mappings[service], parseDockerPort(service, port.Stdout)...)
	}
	return mappings, services, log.String()
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

func minDuration(a, b time.Duration) time.Duration {
	if a <= 0 {
		return b
	}
	if b <= 0 || a < b {
		return a
	}
	return b
}

func dockerCleanupCommandText(composeFile, projectName string) string {
	args := []string{"docker", "compose"}
	if strings.TrimSpace(composeFile) != "" {
		args = append(args, "-f", composeFile)
	}
	args = append(args, "-p", projectName, "down", "-v", "--remove-orphans", "--rmi", "local")
	return strings.Join(args, " ")
}
