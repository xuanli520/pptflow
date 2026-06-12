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
	ProjectPath       string
	RepoPath          string
	ArtifactRoot      string
	TaskID            string
	RunID             string
	Labels            map[string]string
	Env               []string
	Log               io.Writer
	Progress          func(ProgressEvent)
	Timeouts          RuntimeTimeouts
	RewriteFixedPorts bool
}

type StartRuntimeResult struct {
	Runtime                  RuntimeState
	FailureRuntime           RuntimeState
	MirrorSummary            BuildMirrorSummary
	RuntimeSummary           DockerRuntimeSummary
	EffectiveConfigContent   string
	StageCProxyConfigContent string
	LogHints                 []string
	Warnings                 []string
}

type DockerRuntimeSummary struct {
	OK                   bool                       `json:"ok"`
	RunID                string                     `json:"run_id"`
	TaskID               string                     `json:"task_id"`
	ComposeProject       string                     `json:"compose_project"`
	ComposeFile          string                     `json:"compose_file,omitempty"`
	ComposeFiles         []string                   `json:"compose_files,omitempty"`
	WorkDir              string                     `json:"work_dir"`
	PullPolicy           string                     `json:"pull_policy"`
	Pull                 StepSummary                `json:"pull"`
	Build                BuildStepSummary           `json:"build"`
	Up                   StepSummary                `json:"up"`
	PortCollection       StepSummary                `json:"port_collection"`
	ReadmeCommandMode    bool                       `json:"readme_command_mode,omitempty"`
	EffectiveConfig      string                     `json:"effective_config,omitempty"`
	PortRewrite          *RuntimePortRewriteSummary `json:"port_rewrite,omitempty"`
	NetworkIPAM          *RuntimeNetworkIPAMSummary `json:"network_ipam,omitempty"`
	RuntimeErrorCategory string                     `json:"runtime_error_category,omitempty"`
	FailureDiagnostics   *RuntimeFailureDiagnostics `json:"failure_diagnostics,omitempty"`
	EnvFilesPrepared     []string                   `json:"env_files_prepared,omitempty"`
	Warnings             []string                   `json:"warnings,omitempty"`
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
	Category    string
	Message     string
	Fix         string
	Result      executor.Result
	Diagnostics *RuntimeFailureDiagnostics
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
	baseRuntimeFiles := []string{}
	readmeLabelFiles := []string{}
	readmeRuntimeFiles := []string{}
	composeEnvFiles := []string{}
	var effectiveConfig string
	if composeFile != "" {
		envPrep := prepareRuntimeEnvFiles(composeFile, workDir, req.ArtifactRoot)
		composeEnvFiles = append(composeEnvFiles, envPrep.EnvFiles...)
		summary.EnvFilesPrepared = append(summary.EnvFilesPrepared, envPrep.Generated...)
		summary.Warnings = append(summary.Warnings, envPrep.Warnings...)
		for _, generated := range envPrep.Generated {
			cmd.logLine("prepared runtime env file from example: "+generated, "p2r", false)
		}
		originalFiles = []string{composeFile}
		baseRuntimeFiles = append([]string{}, originalFiles...)
		if envPrep.ComposeFile != "" {
			baseRuntimeFiles = append(baseRuntimeFiles, envPrep.ComposeFile)
		}
		activeFiles = append([]string{}, baseRuntimeFiles...)
		configResult := cmd.runStreaming(ctx, "B0 docker compose config", timeouts.Port, ComposeCommandArgsWithProjectDirAndEnvFiles(activeFiles, workDir, projectName, composeEnvFiles, "config"), true)
		effectiveConfig = configResult.Stdout
		result.StageCProxyConfigContent = configResult.Stdout
		if configResult.Err != nil {
			summary.RuntimeErrorCategory = "compose_config_failed"
			result.RuntimeSummary = summary
			result.MirrorSummary = mirrorSummary
			return result, &StartRuntimeError{Category: "compose_config_failed", Message: "docker compose config failed: " + trimResultText(configResult), Fix: "Fix docker compose configuration and rerun stage B.", Result: configResult}
		}
		if req.RewriteFixedPorts {
			portRewrite := prepareRuntimePortRewrite(composeFile, req.ArtifactRoot)
			if len(portRewrite.Summary.Warnings) > 0 {
				summary.Warnings = append(summary.Warnings, portRewrite.Summary.Warnings...)
			}
			if portRewrite.Summary.Generated {
				summary.PortRewrite = &portRewrite.Summary
				candidateFiles := append(append([]string{}, baseRuntimeFiles...), portRewrite.Summary.ComposeFile)
				verify := cmd.runStreaming(ctx, "B0 docker compose port rewrite config", timeouts.Port, ComposeCommandArgsWithProjectDirAndEnvFiles(candidateFiles, workDir, projectName, composeEnvFiles, "config"), true)
				if verify.Err == nil {
					baseRuntimeFiles = candidateFiles
					activeFiles = append([]string{}, baseRuntimeFiles...)
					effectiveConfig = verify.Stdout
				} else {
					summary.Warnings = append(summary.Warnings, "runtime port rewrite config failed; falling back to original compose file: "+trimResultText(verify))
					summary.PortRewrite.Warnings = append(summary.PortRewrite.Warnings, "config verification failed: "+trimResultText(verify))
				}
			}
		}
		networkIPAM := prepareRuntimeNetworkIPAMOverride(effectiveConfig, req.ArtifactRoot, projectName)
		if len(networkIPAM.Summary.Warnings) > 0 {
			summary.Warnings = append(summary.Warnings, networkIPAM.Summary.Warnings...)
		}
		if networkIPAM.Summary.Generated {
			summary.NetworkIPAM = &networkIPAM.Summary
			candidateFiles := append(append([]string{}, baseRuntimeFiles...), networkIPAM.Summary.ComposeFile)
			verify := cmd.runStreaming(ctx, "B0 docker compose network ipam config", timeouts.Port, ComposeCommandArgsWithProjectDirAndEnvFiles(candidateFiles, workDir, projectName, composeEnvFiles, "config"), true)
			if verify.Err == nil {
				baseRuntimeFiles = candidateFiles
				activeFiles = append([]string{}, baseRuntimeFiles...)
				effectiveConfig = verify.Stdout
			} else {
				summary.Warnings = append(summary.Warnings, "runtime network ipam override config failed; falling back to Docker default address pools: "+trimResultText(verify))
				summary.NetworkIPAM.Warnings = append(summary.NetworkIPAM.Warnings, "config verification failed: "+trimResultText(verify))
			}
		}
		prepared := prepareBuildMirror(repoPath, composeFile, req.ArtifactRoot, s.Config)
		mirrorSummary = prepared.Summary
		if len(prepared.ComposeFiles) > 1 {
			candidateFiles := append(append([]string{}, baseRuntimeFiles...), prepared.ComposeFiles[1:]...)
			verify := cmd.runStreaming(ctx, "B0 docker compose mirror config", timeouts.Port, ComposeCommandArgsWithProjectDirAndEnvFiles(candidateFiles, workDir, projectName, composeEnvFiles, "config"), true)
			if verify.Err == nil {
				activeFiles = append([]string{}, candidateFiles...)
				effectiveConfig = verify.Stdout
				mirrorSummary.OverrideVerified = true
			} else {
				reason := "mirror override config failed: " + trimResultText(verify)
				fallbackReason := reason
				if dockerfilePathOutsideContextFailure(verify) {
					fallbackReason = "dockerfile_path_outside_context"
				}
				mirrorSummary.FallbackUsed = true
				mirrorSummary.FallbackReason = fallbackReason
				mirrorSummary.FallbackFrom = append([]string{}, candidateFiles...)
				mirrorSummary.FallbackTo = append([]string{}, baseRuntimeFiles...)
				mirrorSummary.Warnings = append(mirrorSummary.Warnings, reason)
				cmd.logLine("mirror override config failed; falling back to base compose file set", "p2r", false)
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
			pull := cmd.runStreaming(ctx, "B1 docker compose pull", timeouts.Pull, ComposeCommandArgsWithProjectDirAndEnvFiles(activeFiles, workDir, projectName, composeEnvFiles, "pull", "--ignore-buildable"), required)
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
		usingMirrorOverride := len(activeFiles) > len(baseRuntimeFiles)
		build := cmd.runStreaming(ctx, "B2 docker compose build", timeouts.Build, ComposeCommandArgsWithProjectDirAndEnvFiles(activeFiles, workDir, projectName, composeEnvFiles, "build"), true)
		summary.Build = buildSummaryFromResult(build, usingMirrorOverride)
		if build.Err != nil && usingMirrorOverride && s.Config.BuildMirrors.FallbackToOriginal {
			reason := "patched build failed: " + trimResultText(build)
			fallbackReason := reason
			if dockerfilePathOutsideContextFailure(build) {
				fallbackReason = "dockerfile_path_outside_context"
			}
			cmd.logLine(reason, "p2r", false)
			cmd.logLine("falling back to base compose file set for build/up", "p2r", false)
			mirrorSummary.FallbackUsed = true
			mirrorSummary.FallbackReason = fallbackReason
			mirrorSummary.Warnings = append(mirrorSummary.Warnings, reason)
			mirrorSummary.FallbackFrom = append([]string{}, activeFiles...)
			mirrorSummary.FallbackTo = append([]string{}, baseRuntimeFiles...)
			summary.Build.FallbackUsed = true
			activeFiles = append([]string{}, baseRuntimeFiles...)
			fallbackBuild := cmd.runStreaming(ctx, "B2 docker compose build fallback", timeouts.Build, ComposeCommandArgsWithProjectDirAndEnvFiles(activeFiles, workDir, projectName, composeEnvFiles, "build"), true)
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
	if composeFile != "" {
		labelOverride := prepareRuntimeLabelOverride(effectiveConfig, req.ArtifactRoot, req.Labels)
		summary.Warnings = append(summary.Warnings, labelOverride.Warnings...)
		if labelOverride.File != "" {
			candidateFiles := append(append([]string{}, activeFiles...), labelOverride.File)
			verify := cmd.runStreaming(ctx, "B0 docker compose label override config", timeouts.Port, ComposeCommandArgsWithProjectDirAndEnvFiles(candidateFiles, workDir, projectName, composeEnvFiles, "config"), true)
			if verify.Err == nil {
				activeFiles = candidateFiles
				effectiveConfig = verify.Stdout
			} else {
				summary.Warnings = append(summary.Warnings, "runtime label override config failed; continuing without managed labels: "+trimResultText(verify))
			}
		}
		summary.ComposeFiles = append([]string{}, activeFiles...)
		summary.ComposeFile = firstComposeFile(activeFiles)
		mirrorSummary.ComposeFiles = append([]string{}, activeFiles...)
		if mirrorSummary.ComposeFile == "" {
			mirrorSummary.ComposeFile = firstComposeFile(activeFiles)
		}
	} else if len(req.Labels) > 0 {
		configResult := cmd.runStreaming(ctx, "B0 docker compose README label config", timeouts.Port, append(ComposeGlobals(readmeCommand, projectName), "config"), true)
		if configResult.Err != nil {
			summary.RuntimeErrorCategory = "readme_label_config_failed"
			result.RuntimeSummary = summary
			result.MirrorSummary = mirrorSummary
			return result, &StartRuntimeError{Category: "readme_label_config_failed", Message: "docker compose config failed before label injection: " + trimResultText(configResult), Fix: "Use a compose file that can be labelled or add a standard docker-compose.yml.", Result: configResult}
		}
		effectiveConfig = configResult.Stdout
		result.StageCProxyConfigContent = configResult.Stdout
		labelOverride := prepareRuntimeLabelOverride(effectiveConfig, req.ArtifactRoot, req.Labels)
		summary.Warnings = append(summary.Warnings, labelOverride.Warnings...)
		if labelOverride.File == "" {
			summary.RuntimeErrorCategory = "readme_label_override_unavailable"
			result.RuntimeSummary = summary
			result.MirrorSummary = mirrorSummary
			return result, &StartRuntimeError{Category: "readme_label_override_unavailable", Message: "runtime label override could not be generated for README compose command mode", Fix: "Use a compose file with named services or disable README command mode.", Result: configResult}
		}
		candidateFiles := []string{labelOverride.File}
		verify := cmd.runStreaming(ctx, "B0 docker compose README label override config", timeouts.Port, append(ComposeGlobalsWithFiles(readmeCommand, projectName, candidateFiles), "config"), true)
		if verify.Err != nil {
			summary.RuntimeErrorCategory = "readme_label_override_config_failed"
			result.RuntimeSummary = summary
			result.MirrorSummary = mirrorSummary
			return result, &StartRuntimeError{Category: "readme_label_override_config_failed", Message: "runtime label override config failed: " + trimResultText(verify), Fix: "Fix README compose command or use a standard compose file.", Result: verify}
		}
		readmeLabelFiles = candidateFiles
		readmeRuntimeFiles = append(readmeComposeFiles(readmeCommand), candidateFiles...)
		effectiveConfig = verify.Stdout
		summary.ComposeFiles = append([]string{}, readmeRuntimeFiles...)
		summary.ComposeFile = firstComposeFile(readmeRuntimeFiles)
		mirrorSummary.ComposeFiles = append([]string{}, readmeRuntimeFiles...)
		if mirrorSummary.ComposeFile == "" {
			mirrorSummary.ComposeFile = firstComposeFile(readmeRuntimeFiles)
		}
	}
	var upArgs []string
	if composeFile != "" {
		upArgs = ComposeCommandArgsWithProjectDirAndEnvFiles(activeFiles, workDir, projectName, composeEnvFiles, "up", "-d")
	} else {
		upArgs = ComposeArgsWithProjectFiles(readmeCommand, projectName, readmeLabelFiles)
	}
	up := cmd.runStreaming(ctx, "B3 docker compose up", timeouts.Up, upArgs, true)
	summary.Up = stepSummaryFromResult(up, true, "ok")
	if up.Err != nil {
		failureRuntime := RuntimeState{
			ComposeProject: projectName,
			ComposeFile:    firstComposeFile(activeFiles),
			ComposeFiles:   append([]string{}, activeFiles...),
			EnvFiles:       append([]string{}, composeEnvFiles...),
			WorkDir:        workDir,
			Mappings:       map[string][]PortMapping{},
		}
		if composeFile == "" {
			failureRuntime.ComposeFile = firstComposeFile(readmeRuntimeFiles)
			failureRuntime.ComposeFiles = append([]string{}, readmeRuntimeFiles...)
		}
		failureRuntime.Normalize()
		diagnostics := s.diagnoseRuntimeFailure(ctx, cmd, runtimeFailureDiagnosticRequest{
			Trigger:          "B3 docker compose up",
			DefaultCategory:  "up_failed",
			DefaultFix:       "Fix Docker startup and rerun stage B.",
			Result:           up,
			ProjectName:      projectName,
			WorkDir:          workDir,
			ComposeFiles:     failureRuntime.ComposeFiles,
			EnvFiles:         failureRuntime.EnvFiles,
			ReadmeCommand:    readmeCommand,
			ReadmeLabelFiles: readmeLabelFiles,
		})
		if diagnostics.Category != "" {
			summary.RuntimeErrorCategory = diagnostics.Category
		} else {
			summary.RuntimeErrorCategory = "up_failed"
		}
		summary.FailureDiagnostics = &diagnostics
		summary.Up.Status = "failed"
		result.RuntimeSummary = summary
		result.MirrorSummary = mirrorSummary
		result.EffectiveConfigContent = effectiveConfig
		result.StageCProxyConfigContent = effectiveConfig
		result.FailureRuntime = failureRuntime
		message := "B3 docker compose up failed: " + diagnostics.Cause
		if strings.TrimSpace(diagnostics.Cause) == "" {
			message = "B3 docker compose up failed: " + trimResultText(up)
		}
		fix := diagnostics.Fix
		if strings.TrimSpace(fix) == "" {
			fix = "Fix Docker startup and rerun stage B."
		}
		return result, &StartRuntimeError{Category: summary.RuntimeErrorCategory, Message: message, Fix: fix, Result: up, Diagnostics: &diagnostics}
	}
	psArgs := ComposeCommandArgsWithProjectDirAndEnvFiles(activeFiles, workDir, projectName, composeEnvFiles, "ps", "--format", "json")
	psQArgs := ComposeCommandArgsWithProjectDirAndEnvFiles(activeFiles, workDir, projectName, composeEnvFiles, "ps", "-q")
	servicesArgs := ComposeCommandArgsWithProjectDirAndEnvFiles(activeFiles, workDir, projectName, composeEnvFiles, "config", "--services")
	if composeFile == "" {
		psArgs = ComposePSArgsWithFiles(readmeCommand, projectName, readmeLabelFiles)
		psQArgs = ComposePSQArgsWithFiles(readmeCommand, projectName, readmeLabelFiles)
		servicesArgs = ComposeServicesArgsWithFiles(readmeCommand, projectName, readmeLabelFiles)
	}
	ps := cmd.runStreaming(ctx, "B5 docker compose port collection", timeouts.Port, psArgs, false)
	mappings, services := ParseComposePS(ps.Stdout)
	portMethod := "compose_ps_json"
	if ps.Err != nil || len(mappings) == 0 {
		fallbackMappings, fallbackServices, fallbackLog := s.dockerPortFallback(ctx, cmd, timeouts.Port, psQArgs, servicesArgs)
		if req.Log != nil {
			_, _ = req.Log.Write([]byte(RedactLogText(fallbackLog)))
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
	runtimeComposeFiles := activeFiles
	if composeFile == "" {
		runtimeComposeFiles = readmeRuntimeFiles
	}
	runtime := RuntimeState{
		ComposeProject: projectName,
		ComposeFile:    firstComposeFile(runtimeComposeFiles),
		ComposeFiles:   append([]string{}, runtimeComposeFiles...),
		EnvFiles:       append([]string{}, composeEnvFiles...),
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
	return RedactLogText(value)
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

func dockerfilePathOutsideContextFailure(result executor.Result) bool {
	text := strings.ToLower(strings.Join([]string{result.Stdout, result.Stderr, resultErrorString(result)}, "\n"))
	return strings.Contains(text, "dockerfile") && (strings.Contains(text, "outside the build context") ||
		strings.Contains(text, "outside build context") ||
		strings.Contains(text, "forbidden path") ||
		strings.Contains(text, "not within the build context"))
}
