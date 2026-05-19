package docker

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/executor"
)

type Service struct {
	Exec   executor.CommandRunner
	Config config.DockerConfig
}

type StartRuntimeRequest struct {
	ProjectPath  string
	RepoPath     string
	ArtifactRoot string
	TaskID       string
	RunID        string
	Env          []string
	Log          io.Writer
	Progress     func(ProgressEvent)
	Timeouts     RuntimeTimeouts
}

type StartRuntimeResult struct {
	Runtime                RuntimeState
	MirrorSummary          BuildMirrorSummary
	RuntimeSummary         DockerRuntimeSummary
	EffectiveConfigContent string
	LogHints               []string
	Warnings               []string
}

type DockerRuntimeSummary struct {
	OK                   bool             `json:"ok"`
	RunID                string           `json:"run_id"`
	TaskID               string           `json:"task_id"`
	ComposeProject       string           `json:"compose_project"`
	ComposeFile          string           `json:"compose_file,omitempty"`
	ComposeFiles         []string         `json:"compose_files,omitempty"`
	WorkDir              string           `json:"work_dir"`
	PullPolicy           string           `json:"pull_policy"`
	Pull                 StepSummary      `json:"pull"`
	Build                BuildStepSummary `json:"build"`
	Up                   StepSummary      `json:"up"`
	PortCollection       StepSummary      `json:"port_collection"`
	ReadmeCommandMode    bool             `json:"readme_command_mode,omitempty"`
	EffectiveConfig      string           `json:"effective_config,omitempty"`
	RuntimeErrorCategory string           `json:"runtime_error_category,omitempty"`
	Warnings             []string         `json:"warnings,omitempty"`
}

type StepSummary struct {
	Status   string `json:"status"`
	Required bool   `json:"required,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
	Error    string `json:"error,omitempty"`
	Command  string `json:"command,omitempty"`
	Method   string `json:"method,omitempty"`
}

type BuildStepSummary struct {
	Status              string `json:"status"`
	FallbackUsed        bool   `json:"fallback_used"`
	UsingMirrorOverride bool   `json:"using_mirror_override"`
	ExitCode            int    `json:"exit_code,omitempty"`
	Error               string `json:"error,omitempty"`
	Command             string `json:"command,omitempty"`
}

type StartRuntimeError struct {
	Category string
	Message  string
	Fix      string
	Result   executor.Result
}

func (e *StartRuntimeError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func (s Service) StartRuntime(ctx context.Context, req StartRuntimeRequest) (StartRuntimeResult, error) {
	timeouts := req.Timeouts
	if timeouts.Pull == 0 {
		timeouts.Pull = 300 * time.Second
	}
	if timeouts.Build == 0 {
		timeouts.Build = 600 * time.Second
	}
	if timeouts.Up == 0 {
		timeouts.Up = 300 * time.Second
	}
	if timeouts.Health == 0 {
		timeouts.Health = 60 * time.Second
	}
	if timeouts.Port == 0 {
		timeouts.Port = 30 * time.Second
	}
	repoPath := filepath.Clean(req.RepoPath)
	projectName := ComposeProjectName(s.Config.ComposeProjectPrefix, req.TaskID, req.RunID)
	composeFile := FindCompose(repoPath)
	readmeCommand := []string{}
	if composeFile == "" {
		readmeCommand = ReadmeComposeCommand(repoPath)
	}
	if composeFile == "" && len(readmeCommand) == 0 {
		return StartRuntimeResult{}, &StartRuntimeError{
			Category: "compose_config_failed",
			Message:  "No docker-compose.yml or README-declared docker compose startup command found in repo/.",
			Fix:      "Add repo/docker-compose.yml, document a docker compose startup command, or run --static-only.",
		}
	}
	if _, err := s.Exec.LookPath("docker"); err != nil {
		return StartRuntimeResult{}, &StartRuntimeError{
			Category: "compose_config_failed",
			Message:  "docker executable not found on PATH.",
			Fix:      "Install Docker or run --static-only.",
		}
	}
	workDir := repoPath
	if composeFile != "" {
		workDir = filepath.Dir(composeFile)
	}
	cmd := commandContext{Exec: s.Exec, WorkDir: workDir, Env: req.Env, Log: req.Log, Progress: req.Progress}
	result := StartRuntimeResult{}
	summary := DockerRuntimeSummary{
		RunID:          req.RunID,
		TaskID:         req.TaskID,
		ComposeProject: projectName,
		ComposeFile:    composeFile,
		ComposeFiles:   []string{},
		WorkDir:        workDir,
		PullPolicy:     s.Config.PullPolicy,
	}
	mirrorSummary := BuildMirrorSummary{
		Enabled:      s.Config.BuildMirrors.Enabled,
		Mode:         s.Config.BuildMirrors.Mode,
		Profile:      s.Config.BuildMirrors.Profile,
		RepoModified: false,
		ComposeFile:  composeFile,
	}
	activeFiles := []string{}
	originalFiles := []string{}
	var effectiveConfig string
	if composeFile != "" {
		originalFiles = []string{composeFile}
		activeFiles = append([]string{}, originalFiles...)
		configResult := cmd.runStreaming(ctx, "B0 docker compose config", timeouts.Port, ComposeCommandArgs(originalFiles, projectName, "config"), true)
		effectiveConfig = configResult.Stdout
		if configResult.Err != nil {
			summary.RuntimeErrorCategory = "compose_config_failed"
			result.RuntimeSummary = summary
			result.MirrorSummary = mirrorSummary
			return result, &StartRuntimeError{Category: "compose_config_failed", Message: "docker compose config failed: " + trimResultText(configResult), Fix: "Fix docker compose configuration and rerun stage B.", Result: configResult}
		}
		prepared := prepareBuildMirror(repoPath, composeFile, req.ArtifactRoot, s.Config)
		mirrorSummary = prepared.Summary
		if len(prepared.ComposeFiles) > 1 {
			verify := cmd.runStreaming(ctx, "B0 docker compose mirror config", timeouts.Port, ComposeCommandArgs(prepared.ComposeFiles, projectName, "config"), true)
			if verify.Err == nil {
				activeFiles = append([]string{}, prepared.ComposeFiles...)
				effectiveConfig = verify.Stdout
				mirrorSummary.OverrideVerified = true
			} else {
				mirrorSummary.FallbackUsed = true
				mirrorSummary.FallbackReason = "mirror override config failed: " + trimResultText(verify)
				mirrorSummary.FallbackFrom = append([]string{}, prepared.ComposeFiles...)
				mirrorSummary.FallbackTo = append([]string{}, originalFiles...)
				mirrorSummary.Warnings = append(mirrorSummary.Warnings, mirrorSummary.FallbackReason)
				cmd.logLine("mirror override config failed; falling back to original compose file set", "p2r", false)
			}
		}
	} else {
		summary.ReadmeCommandMode = true
		summary.Warnings = append(summary.Warnings, "README compose command mode: pull/build/mirror patch skipped")
		mirrorSummary.Warnings = append(mirrorSummary.Warnings, "README compose command mode: Dockerfile patch skipped")
	}
	summary.ComposeFiles = append([]string{}, activeFiles...)
	summary.ComposeFile = firstComposeFile(activeFiles)
	mirrorSummary.ComposeFiles = append([]string{}, activeFiles...)
	if mirrorSummary.ComposeFile == "" {
		mirrorSummary.ComposeFile = firstComposeFile(activeFiles)
	}
	if composeFile != "" {
		pullPolicy := strings.TrimSpace(s.Config.PullPolicy)
		switch pullPolicy {
		case "skip":
			summary.Pull = StepSummary{Status: "skipped", Required: false}
		default:
			required := pullPolicy == "required"
			pull := cmd.runStreaming(ctx, "B1 docker compose pull", timeouts.Pull, ComposeCommandArgs(activeFiles, projectName, "pull", "--ignore-buildable"), required)
			summary.Pull = stepSummaryFromResult(pull, required, "ok")
			if pull.Err != nil && required {
				summary.Pull.Status = "failed"
				summary.RuntimeErrorCategory = "pull_failed_required"
				result.RuntimeSummary = summary
				result.MirrorSummary = mirrorSummary
				return result, &StartRuntimeError{Category: "pull_failed_required", Message: "B1 docker compose pull failed: " + trimResultText(pull), Fix: "Fix Docker image pull or set docker.pull_policy to best_effort/skip.", Result: pull}
			}
			if pull.Err != nil {
				summary.Pull.Status = "warning"
				summary.Warnings = append(summary.Warnings, "best-effort docker compose pull failed: "+trimResultText(pull))
			}
		}
		build := cmd.runStreaming(ctx, "B2 docker compose build", timeouts.Build, ComposeCommandArgs(activeFiles, projectName, "build"), true)
		summary.Build = buildSummaryFromResult(build, len(activeFiles) > len(originalFiles))
		if build.Err != nil && len(activeFiles) > len(originalFiles) && s.Config.BuildMirrors.FallbackToOriginal {
			reason := "patched build failed: " + trimResultText(build)
			cmd.logLine(reason, "p2r", false)
			cmd.logLine("falling back to original compose file set for build/up", "p2r", false)
			mirrorSummary.FallbackUsed = true
			mirrorSummary.FallbackReason = reason
			mirrorSummary.FallbackFrom = append([]string{}, activeFiles...)
			mirrorSummary.FallbackTo = append([]string{}, originalFiles...)
			summary.Build.FallbackUsed = true
			activeFiles = append([]string{}, originalFiles...)
			fallbackBuild := cmd.runStreaming(ctx, "B2 docker compose build fallback", timeouts.Build, ComposeCommandArgs(activeFiles, projectName, "build"), true)
			if fallbackBuild.Err != nil {
				summary.Build.Status = "failed"
				summary.Build.Error = "patched build and fallback build failed: " + trimResultText(fallbackBuild)
				summary.RuntimeErrorCategory = "patched_build_failed_and_fallback_failed"
				result.RuntimeSummary = summary
				result.MirrorSummary = mirrorSummary
				return result, &StartRuntimeError{Category: "patched_build_failed_and_fallback_failed", Message: summary.Build.Error, Fix: "Fix Dockerfile build failures or disable docker.build_mirrors.", Result: fallbackBuild}
			}
			summary.Build.Status = "ok"
			summary.Build.Command = fallbackBuild.Command
			summary.Build.ExitCode = fallbackBuild.ExitCode
			summary.Build.Error = ""
			summary.Build.UsingMirrorOverride = false
		} else if build.Err != nil {
			summary.Build.Status = "failed"
			summary.RuntimeErrorCategory = "build_failed"
			result.RuntimeSummary = summary
			result.MirrorSummary = mirrorSummary
			return result, &StartRuntimeError{Category: "build_failed", Message: "B2 docker compose build failed: " + trimResultText(build), Fix: "Fix Docker build failures and rerun stage B.", Result: build}
		}
	}
	var upArgs []string
	if composeFile != "" {
		upArgs = ComposeCommandArgs(activeFiles, projectName, "up", "-d")
	} else {
		upArgs = ComposeArgsWithProject(readmeCommand, projectName)
	}
	up := cmd.runStreaming(ctx, "B3 docker compose up", timeouts.Up, upArgs, true)
	summary.Up = stepSummaryFromResult(up, true, "ok")
	if up.Err != nil {
		summary.Up.Status = "failed"
		summary.RuntimeErrorCategory = "up_failed"
		result.RuntimeSummary = summary
		result.MirrorSummary = mirrorSummary
		return result, &StartRuntimeError{Category: "up_failed", Message: "B3 docker compose up failed: " + trimResultText(up), Fix: "Fix Docker startup and rerun stage B.", Result: up}
	}
	psArgs := ComposeCommandArgs(activeFiles, projectName, "ps", "--format", "json")
	psQArgs := ComposeCommandArgs(activeFiles, projectName, "ps", "-q")
	servicesArgs := ComposeCommandArgs(activeFiles, projectName, "config", "--services")
	if composeFile == "" {
		psArgs = ComposePSArgs(readmeCommand, projectName)
		psQArgs = ComposePSQArgs(readmeCommand, projectName)
		servicesArgs = ComposeServicesArgs(readmeCommand, projectName)
	}
	ps := cmd.runStreaming(ctx, "B5 docker compose port collection", timeouts.Port, psArgs, false)
	mappings, services := ParseComposePS(ps.Stdout)
	portMethod := "compose_ps_json"
	if ps.Err != nil || len(mappings) == 0 {
		fallbackMappings, fallbackServices, fallbackLog := s.dockerPortFallback(ctx, cmd, timeouts.Port, psQArgs, servicesArgs)
		if req.Log != nil {
			_, _ = req.Log.Write([]byte(fallbackLog))
		}
		if len(fallbackMappings) > 0 {
			mappings = fallbackMappings
			services = fallbackServices
			portMethod = "docker_port_fallback"
		}
	}
	summary.PortCollection = stepSummaryFromResult(ps, false, "ok")
	summary.PortCollection.Method = portMethod
	if ps.Err != nil && len(mappings) == 0 {
		summary.PortCollection.Status = "failed"
	} else if len(mappings) == 0 {
		summary.PortCollection.Status = "empty"
	}
	cmd.logLine("=== B4 health check probe start ===", "p2r", false)
	probes := ProbeMappings(mappings, minDuration(timeouts.Health, time.Duration(s.Config.HealthCheckTimeoutSeconds)*time.Second))
	for _, probe := range probes {
		cmd.logLine(fmt.Sprintf("%s %s ok=%t status=%d error=%s", probe.Service, probe.URL, probe.OK, probe.Status, probe.Error), "p2r", false)
	}
	cmd.logLine("=== B4 health check probe end ===", "p2r", false)
	runtime := RuntimeState{
		ComposeProject: projectName,
		ComposeFile:    firstComposeFile(activeFiles),
		ComposeFiles:   append([]string{}, activeFiles...),
		WorkDir:        workDir,
		Services:       services,
		Mappings:       mappings,
		Probes:         probes,
		Mirror: RuntimeMirrorState{
			BuildMirrorEnabled:      mirrorSummary.Enabled,
			BuildMirrorMode:         mirrorSummary.Mode,
			BuildMirrorFallbackUsed: mirrorSummary.FallbackUsed,
			BuildMirrorSummary:      "docker_mirror_summary.json",
		},
	}
	runtime.Normalize()
	summary.OK = true
	summary.ComposeFile = runtime.ComposeFile
	summary.ComposeFiles = append([]string{}, runtime.ComposeFiles...)
	summary.WorkDir = runtime.WorkDir
	mirrorSummary.ComposeFiles = append([]string{}, runtime.ComposeFiles...)
	if mirrorSummary.ComposeFile == "" {
		mirrorSummary.ComposeFile = runtime.ComposeFile
	}
	result.Runtime = runtime
	result.RuntimeSummary = summary
	result.MirrorSummary = mirrorSummary
	result.EffectiveConfigContent = effectiveConfig
	result.Warnings = append(result.Warnings, summary.Warnings...)
	result.Warnings = append(result.Warnings, mirrorSummary.Warnings...)
	return result, nil
}

func (s Service) dockerPortFallback(ctx context.Context, cmd commandContext, timeout time.Duration, psQArgs, servicesArgs []string) (map[string][]PortMapping, []string, string) {
	var log strings.Builder
	servicesResult := cmd.run(ctx, timeout, servicesArgs)
	log.WriteString("\n\n" + servicesResult.Command + "\nSTDOUT:\n" + servicesResult.Stdout + "\nSTDERR:\n" + servicesResult.Stderr)
	serviceNames := SplitNonEmptyLines(servicesResult.Stdout)
	psQ := cmd.run(ctx, timeout, psQArgs)
	log.WriteString("\n\n" + psQ.Command + "\nSTDOUT:\n" + psQ.Stdout + "\nSTDERR:\n" + psQ.Stderr)
	containers := SplitNonEmptyLines(psQ.Stdout)
	mappings := map[string][]PortMapping{}
	var services []string
	for index, container := range containers {
		service := container
		if index < len(serviceNames) {
			service = serviceNames[index]
		}
		services = append(services, service)
		port := cmd.run(ctx, timeout, []string{"port", container})
		log.WriteString("\n\n" + port.Command + "\nSTDOUT:\n" + port.Stdout + "\nSTDERR:\n" + port.Stderr)
		mappings[service] = append(mappings[service], ParseDockerPort(service, port.Stdout)...)
	}
	return mappings, services, log.String()
}

func stepSummaryFromResult(result executor.Result, required bool, okStatus string) StepSummary {
	summary := StepSummary{Status: okStatus, Required: required, ExitCode: result.ExitCode, Command: result.Command}
	if result.Err != nil {
		summary.Status = "failed"
		summary.Error = result.Err.Error()
	}
	return summary
}

func buildSummaryFromResult(result executor.Result, usingMirror bool) BuildStepSummary {
	summary := BuildStepSummary{Status: "ok", UsingMirrorOverride: usingMirror, ExitCode: result.ExitCode, Command: result.Command}
	if result.Err != nil {
		summary.Status = "failed"
		summary.Error = result.Err.Error()
	}
	return summary
}

func trimResultText(result executor.Result) string {
	value := strings.TrimSpace(firstNonEmpty(result.Stderr, result.Stdout, resultErrorString(result)))
	if value == "" {
		value = result.Command
	}
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func firstComposeFile(files []string) string {
	for _, file := range files {
		if strings.TrimSpace(file) != "" {
			return file
		}
	}
	return ""
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

func resultErrorString(result executor.Result) string {
	if result.Err == nil {
		return ""
	}
	return result.Err.Error()
}
