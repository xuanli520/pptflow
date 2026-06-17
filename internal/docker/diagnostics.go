package docker

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/xuanli520/p2r_tui/internal/executor"
)

const runtimeDiagnosticTextLimit = 32 << 10

type RuntimeFailureDiagnostics struct {
	Trigger  string                     `json:"trigger,omitempty"`
	Category string                     `json:"category,omitempty"`
	Title    string                     `json:"title,omitempty"`
	Cause    string                     `json:"cause,omitempty"`
	Fix      string                     `json:"fix,omitempty"`
	Commands []RuntimeDiagnosticCommand `json:"diagnostic_commands,omitempty"`
	Cleanup  *CleanupSummary            `json:"cleanup,omitempty"`
	Warnings []string                   `json:"warnings,omitempty"`
}

type RuntimeFailureClassification struct {
	Category string `json:"category,omitempty"`
	Title    string `json:"title,omitempty"`
	Cause    string `json:"cause,omitempty"`
	Fix      string `json:"fix,omitempty"`
}

type RuntimeDiagnosticCommand struct {
	Name     string `json:"name"`
	Command  string `json:"command"`
	ExitCode int    `json:"exit_code,omitempty"`
	Timeout  bool   `json:"timeout,omitempty"`
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	Error    string `json:"error,omitempty"`
}

func ClassifyRuntimeFailureResult(result executor.Result) (RuntimeFailureClassification, bool) {
	category, title, cause, fix := classifyRuntimeFailure(runtimeFailureDiagnosticRequest{Result: result}, nil)
	classification := RuntimeFailureClassification{
		Category: category,
		Title:    title,
		Cause:    cause,
		Fix:      fix,
	}
	return classification, strings.TrimSpace(category) != ""
}

type runtimeFailureDiagnosticRequest struct {
	Trigger          string
	DefaultCategory  string
	DefaultFix       string
	Result           executor.Result
	ProjectName      string
	WorkDir          string
	ComposeFiles     []string
	EnvFiles         []string
	ReadmeCommand    []string
	ReadmeLabelFiles []string
}

func (s Service) diagnoseRuntimeFailure(ctx context.Context, cmd commandContext, req runtimeFailureDiagnosticRequest) RuntimeFailureDiagnostics {
	diagnostics := RuntimeFailureDiagnostics{Trigger: strings.TrimSpace(req.Trigger)}
	timeout := 20 * time.Second
	composeArgs := func(tail ...string) []string {
		if len(req.ReadmeCommand) > 0 {
			return append(ComposeGlobalsWithFiles(req.ReadmeCommand, req.ProjectName, req.ReadmeLabelFiles), tail...)
		}
		return ComposeCommandArgsWithProjectDirAndEnvFiles(req.ComposeFiles, req.WorkDir, req.ProjectName, req.EnvFiles, tail...)
	}

	psAll := cmd.runDiagnostic(ctx, timeout, "compose_ps_all", composeArgs("ps", "--all", "--format", "json"))
	diagnostics.Commands = append(diagnostics.Commands, psAll)

	psQ := cmd.runDiagnostic(ctx, timeout, "compose_ps_q_all", composeArgs("ps", "--all", "-q"))
	diagnostics.Commands = append(diagnostics.Commands, psQ)
	containers := firstNStrings(SplitNonEmptyLines(psQ.Stdout), 20)
	if len(containers) > 0 {
		inspectArgs := append([]string{"inspect"}, containers...)
		diagnostics.Commands = append(diagnostics.Commands, cmd.runDiagnostic(ctx, timeout, "container_inspect", inspectArgs))
	}

	diagnostics.Commands = append(diagnostics.Commands,
		cmd.runDiagnostic(ctx, timeout, "compose_logs_tail", composeArgs("logs", "--no-color", "--tail", "200")),
		cmd.runDiagnostic(ctx, timeout, "compose_config_services", composeArgs("config", "--services")),
		cmd.runDiagnostic(ctx, timeout, "docker_project_containers", []string{"ps", "-a", "--filter", "label=com.docker.compose.project=" + req.ProjectName, "--format", "json"}),
		cmd.runDiagnostic(ctx, timeout, "docker_project_networks", []string{"network", "ls", "--filter", "label=com.docker.compose.project=" + req.ProjectName, "--format", "json"}),
		cmd.runDiagnostic(ctx, timeout, "docker_compose_networks", []string{"network", "ls", "--filter", "label=com.docker.compose.project", "--format", "json"}),
	)
	if strings.TrimSpace(s.Config.ManagedLabel) != "" {
		diagnostics.Commands = append(diagnostics.Commands,
			cmd.runDiagnostic(ctx, timeout, "docker_managed_networks", []string{"network", "ls", "--filter", "label=" + s.Config.ManagedLabel, "--format", "json"}),
		)
	}
	diagnostics.Commands = append(diagnostics.Commands,
		cmd.runDiagnostic(ctx, timeout, "docker_system_df", []string{"system", "df"}),
	)

	category, title, cause, fix := classifyRuntimeFailure(req, diagnostics.Commands)
	if strings.TrimSpace(category) == "" {
		category = strings.TrimSpace(req.DefaultCategory)
	}
	if strings.TrimSpace(fix) == "" {
		fix = strings.TrimSpace(req.DefaultFix)
	}
	diagnostics.Category = category
	diagnostics.Title = title
	diagnostics.Cause = cause
	diagnostics.Fix = fix

	failureRuntime := RuntimeState{
		ComposeProject: req.ProjectName,
		ComposeFile:    firstComposeFile(req.ComposeFiles),
		ComposeFiles:   append([]string{}, req.ComposeFiles...),
		EnvFiles:       append([]string{}, req.EnvFiles...),
		WorkDir:        req.WorkDir,
		Mappings:       map[string][]PortMapping{},
	}
	failureRuntime.Normalize()
	cleanup := s.cleanupRuntimeFailure(ctx, cmd, failureRuntime)
	diagnostics.Cleanup = &cleanup
	return diagnostics
}

func (c commandContext) runDiagnostic(ctx context.Context, timeout time.Duration, name string, args []string) RuntimeDiagnosticCommand {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "docker_command"
	}
	c.logLine(fmt.Sprintf("=== B3 failure diagnostic %s start ===", name), "p2r", false)
	c.logLine("$ "+CommandLine("docker", args), "p2r", false)
	result := c.run(ctx, timeout, args)
	c.logDiagnosticResult(name, result)
	return diagnosticCommandFromResult(name, result)
}

func (c commandContext) logDiagnosticResult(name string, result executor.Result) {
	if c.Log != nil {
		_, _ = fmt.Fprintln(c.Log, "STDOUT:")
		if strings.TrimSpace(result.Stdout) != "" {
			_, _ = fmt.Fprintln(c.Log, RedactLogText(result.Stdout))
		}
		_, _ = fmt.Fprintln(c.Log, "STDERR:")
		if strings.TrimSpace(result.Stderr) != "" {
			_, _ = fmt.Fprintln(c.Log, RedactLogText(result.Stderr))
		}
	}
	c.logLine(fmt.Sprintf("=== B3 failure diagnostic %s end: exit=%d timeout=%t err=%v ===", name, result.ExitCode, result.Timeout, result.Err), "p2r", true)
}

func diagnosticCommandFromResult(name string, result executor.Result) RuntimeDiagnosticCommand {
	command := RuntimeDiagnosticCommand{
		Name:     name,
		Command:  result.Command,
		ExitCode: result.ExitCode,
		Timeout:  result.Timeout,
		Stdout:   truncateDiagnosticText(RedactLogText(result.Stdout)),
		Stderr:   truncateDiagnosticText(RedactLogText(result.Stderr)),
	}
	if result.Err != nil {
		command.Error = result.Err.Error()
	}
	return command
}

func (s Service) cleanupRuntimeFailure(ctx context.Context, cmd commandContext, runtime RuntimeState) CleanupSummary {
	runtime.Normalize()
	summary := CleanupSummary{
		Status:         "not_applicable",
		ComposeFile:    runtime.ComposeFile,
		ComposeFiles:   runtime.ComposeFiles,
		EnvFiles:       runtime.EnvFiles,
		ComposeProject: runtime.ComposeProject,
		WorkDir:        runtime.WorkDir,
	}
	if strings.TrimSpace(runtime.ComposeProject) == "" {
		summary.Warnings = append(summary.Warnings, "compose project is empty")
		return summary
	}
	args := CleanupComposeArgsFilesWithProjectDirAndEnvFiles(s.Config, runtime.ComposeFiles, runtime.EnvFiles, runtime.ComposeProject, runtime.WorkDir)
	summary.ManualCommand = CommandLine("docker", args)
	cmd.logLine("=== B3 failure cleanup start ===", "p2r", false)
	cmd.logLine("$ "+summary.ManualCommand, "p2r", false)
	result := cmd.run(ctx, 2*time.Minute, args)
	summary.Status = "ok"
	summary.Command = result.Command
	summary.ExitCode = result.ExitCode
	summary.Stdout = truncateDiagnosticText(RedactLogText(result.Stdout))
	summary.Stderr = truncateDiagnosticText(RedactLogText(result.Stderr))
	cmd.logDiagnosticResult("failure_cleanup", result)
	if result.Err != nil {
		summary.Status = "failed"
		summary.Error = result.Err.Error()
		return summary
	}
	psArgs := ComposeCommandArgsWithProjectDirAndEnvFiles(runtime.ComposeFiles, runtime.WorkDir, runtime.ComposeProject, runtime.EnvFiles, "ps", "-q")
	verify := cmd.run(ctx, 30*time.Second, psArgs)
	summary.Verification = truncateDiagnosticText(RedactLogText(strings.TrimSpace(verify.Stdout + "\n" + verify.Stderr)))
	if strings.TrimSpace(verify.Stdout) != "" {
		summary.Status = "failed"
		summary.Warnings = append(summary.Warnings, "compose ps still reports containers after failure cleanup")
	}
	cmd.logLine("=== B3 failure cleanup end: status="+summary.Status+" ===", "p2r", true)
	return summary
}

func classifyRuntimeFailure(req runtimeFailureDiagnosticRequest, commands []RuntimeDiagnosticCommand) (category, title, cause, fix string) {
	text := strings.ToLower(runtimeFailureCorpus(req, commands))
	cause = trimResultText(req.Result)
	switch {
	case strings.Contains(text, "all predefined address pools have been fully subnetted") ||
		strings.Contains(text, "could not find an available, non-overlapping ipv4 address pool") ||
		strings.Contains(text, "invalid pool request") ||
		strings.Contains(text, "pool overlaps with other one on this address space") ||
		strings.Contains(text, "no available network"):
		return "docker_address_pool_exhausted",
			"Docker address pools are exhausted",
			"Docker cannot allocate another compose network subnet: " + cause,
			"Remove stale p2r Docker networks with `p2r admin docker-gc run --yes`, or prune old Docker networks manually; if the host runs many stacks, configure Docker daemon default-address-pools."
	case strings.Contains(text, "port is already allocated") ||
		strings.Contains(text, "bind: address already in use") ||
		strings.Contains(text, "ports are not available"):
		return "host_port_conflict",
			"Host port is already allocated",
			"Docker Compose could not bind a required host port: " + cause,
			"Free the occupied host port or remove fixed published ports from docker-compose.yml so Stage B can use runtime-allocated ports."
	case strings.Contains(text, "no space left on device") ||
		strings.Contains(text, "there is not enough space") ||
		strings.Contains(text, "disk quota exceeded"):
		return "docker_storage_exhausted",
			"Docker storage is exhausted",
			"Docker ran out of storage while starting the runtime: " + cause,
			"Free Docker disk usage with targeted cleanup, then rerun Stage B."
	case strings.Contains(text, "cannot connect to the docker daemon") ||
		strings.Contains(text, "is the docker daemon running") ||
		strings.Contains(text, "docker daemon is not running"):
		return "docker_daemon_unavailable",
			"Docker daemon is unavailable",
			"Stage B could not talk to the Docker daemon: " + cause,
			"Start Docker, verify DOCKER_HOST, and rerun Stage B."
	case req.Result.Timeout:
		return "docker_runtime_timeout",
			"Docker runtime startup timed out",
			"Docker Compose did not finish before the Stage B timeout: " + cause,
			"Inspect the compose logs captured in B_docker.log, fix slow or blocked services, or increase the B_UP timeout."
	case strings.Contains(text, "dependency failed to start") ||
		strings.Contains(text, "failed to start") ||
		strings.Contains(text, "exited (1)") ||
		strings.Contains(text, "exited with code"):
		return "compose_service_failed",
			"A compose service exited during startup",
			"A required service failed while Docker Compose was starting: " + cause,
			"Inspect the captured compose logs and container inspect output in logs/B_docker.log, fix the failing service entrypoint/configuration, and rerun Stage B."
	default:
		if strings.TrimSpace(cause) == "" {
			cause = strings.TrimSpace(req.DefaultCategory)
		}
		return strings.TrimSpace(req.DefaultCategory),
			"Docker runtime startup failed",
			cause,
			strings.TrimSpace(req.DefaultFix)
	}
}

func runtimeFailureCorpus(req runtimeFailureDiagnosticRequest, commands []RuntimeDiagnosticCommand) string {
	var builder strings.Builder
	builder.WriteString(req.Result.Stdout)
	builder.WriteString("\n")
	builder.WriteString(req.Result.Stderr)
	builder.WriteString("\n")
	builder.WriteString(resultErrorString(req.Result))
	for _, command := range commands {
		builder.WriteString("\n")
		builder.WriteString(command.Stdout)
		builder.WriteString("\n")
		builder.WriteString(command.Stderr)
		builder.WriteString("\n")
		builder.WriteString(command.Error)
	}
	return builder.String()
}

func truncateDiagnosticText(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= runtimeDiagnosticTextLimit {
		return value
	}
	return value[:runtimeDiagnosticTextLimit] + "\n[truncated]"
}

func firstNStrings(values []string, limit int) []string {
	if limit <= 0 || len(values) <= limit {
		return append([]string{}, values...)
	}
	return append([]string{}, values[:limit]...)
}
