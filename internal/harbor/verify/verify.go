package verify

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/executor"
	"github.com/purplevoid/harbor-factory/internal/harbor/commandlog"
	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/harborrun"
	"github.com/purplevoid/harbor-factory/internal/harbor/nodes"
	"github.com/purplevoid/harbor-factory/internal/harbor/sanitize"
)

type Options struct {
	TaskDir        string
	Workspace      string
	ImageTag       string
	TimeoutSeconds int
	Exec           executor.CommandRunner
	WriteReport    string
}

func Run(ctx context.Context, opts Options) (domain.VerifyReport, error) {
	taskDir := strings.TrimSpace(opts.TaskDir)
	if taskDir == "" {
		return domain.VerifyReport{}, fmt.Errorf("task directory is required")
	}
	exec := opts.Exec
	if exec == nil {
		exec = executor.New()
	}
	imageTag := strings.TrimSpace(opts.ImageTag)
	if imageTag == "" {
		imageTag = "harbor-task-" + fmt.Sprintf("%d", time.Now().UnixNano())
	}
	timeout := time.Duration(opts.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	taskDigest, err := harborrun.ComputeTaskDigest(taskDir)
	if err != nil {
		return domain.VerifyReport{}, fmt.Errorf("compute task digest: %w", err)
	}
	report := domain.VerifyReport{
		SchemaVersion: "harbor.verify_report.v1",
		TaskDir:       taskDir,
		TaskDigest:    taskDigest,
		ImageTag:      imageTag,
		CreatedAt:     time.Now().UTC(),
	}
	dockerfile := filepath.Join(taskDir, "environment", "Dockerfile")
	compose := filepath.Join(taskDir, "environment", "docker-compose.yaml")
	if _, err := os.Stat(dockerfile); err == nil {
		return runDockerfileVerify(ctx, exec, timeout, opts, report, dockerfile, imageTag)
	}
	if _, err := os.Stat(compose); err == nil {
		return runComposeVerify(ctx, exec, timeout, opts, report, compose)
	}
	report.Passed = false
	_ = writeReport(report, opts.WriteReport)
	return report, fmt.Errorf("environment must contain Dockerfile or docker-compose.yaml")
}

func runDockerfileVerify(ctx context.Context, exec executor.CommandRunner, timeout time.Duration, opts Options, report domain.VerifyReport, dockerfile, imageTag string) (domain.VerifyReport, error) {
	taskDir := strings.TrimSpace(opts.TaskDir)
	contextDir := filepath.Join(taskDir, "environment")
	build, err := runCommand(ctx, exec, timeout, commandOutputDir(opts, nodes.DockerBuild), "", nodes.DockerBuild, "docker", "build", "-t", imageTag, "-f", dockerfile, contextDir)
	if err != nil {
		return report, err
	}
	build.Passed = build.ExitCode == 0 && !build.Timeout
	report.DockerBuild = &build
	addCommand(&report, build)
	if err := writeCommandArtifact(opts, nodes.DockerBuild, build); err != nil {
		return report, err
	}
	if !build.Passed {
		report.Passed = false
		_ = writeReport(report, opts.WriteReport)
		return report, fmt.Errorf("docker build failed")
	}

	testsDir := filepath.Join(taskDir, "tests")
	initial, err := runCommand(ctx, exec, timeout, commandOutputDir(opts, nodes.InitialVerify), "", nodes.InitialVerify, "docker", "run", "--rm", "-v", testsDir+":/tests:ro", imageTag, "/bin/sh", "-c", "/tests/test.sh")
	if err != nil {
		return report, err
	}
	report.InitialExposesIssue = initialVerificationExposesIssue(initial)
	initial.Passed = report.InitialExposesIssue
	report.InitialVerify = &initial
	addCommand(&report, initial)
	if err := writeCommandArtifact(opts, nodes.InitialVerify, initial); err != nil {
		return report, err
	}
	if !report.InitialExposesIssue {
		if err := runDockerImageCleanup(exec, timeout, opts, &report, imageTag); err != nil {
			return report, err
		}
		report.Passed = false
		_ = writeReport(report, opts.WriteReport)
		return report, fmt.Errorf("initial verification unexpectedly passed")
	}

	solutionDir := filepath.Join(taskDir, "solution")
	oracle, err := runCommand(ctx, exec, timeout, commandOutputDir(opts, nodes.OracleVerify), "", nodes.OracleVerify, "docker", "run", "--rm", "-v", solutionDir+":/solution:ro", "-v", testsDir+":/tests:ro", imageTag, "/bin/sh", "-c", "/solution/solve.sh && /tests/test.sh")
	if err != nil {
		return report, err
	}
	oracle.Passed = oracle.ExitCode == 0 && !oracle.Timeout
	report.OracleVerify = &oracle
	addCommand(&report, oracle)
	if err := writeCommandArtifact(opts, nodes.OracleVerify, oracle); err != nil {
		return report, err
	}
	report.Passed = oracle.Passed
	if err := runDockerImageCleanup(exec, timeout, opts, &report, imageTag); err != nil {
		return report, err
	}
	if err := writeReport(report, opts.WriteReport); err != nil {
		return report, err
	}
	if !oracle.Passed {
		return report, fmt.Errorf("oracle verification failed")
	}
	return report, nil
}

func runDockerImageCleanup(exec executor.CommandRunner, timeout time.Duration, opts Options, report *domain.VerifyReport, imageTag string) error {
	cleanup, err := runCommand(context.Background(), exec, timeout, commandOutputDir(opts, "docker_image_rm"), "", "docker_image_rm", "docker", "image", "rm", "-f", imageTag)
	if err != nil {
		return err
	}
	cleanup.Passed = cleanup.ExitCode == 0 && !cleanup.Timeout
	report.Cleanup = &cleanup
	addCommand(report, cleanup)
	return nil
}

func runComposeVerify(ctx context.Context, exec executor.CommandRunner, timeout time.Duration, opts Options, report domain.VerifyReport, compose string) (domain.VerifyReport, error) {
	taskDir := strings.TrimSpace(opts.TaskDir)
	project := "harbor-task-" + fmt.Sprintf("%d", time.Now().UnixNano())
	build, err := runCommand(ctx, exec, timeout, commandOutputDir(opts, nodes.DockerBuild), taskDir, nodes.DockerBuild, "docker", "compose", "-p", project, "-f", compose, "--project-directory", taskDir, "build", "main")
	if err != nil {
		return report, err
	}
	build.Passed = build.ExitCode == 0 && !build.Timeout
	report.DockerBuild = &build
	addCommand(&report, build)
	if err := writeCommandArtifact(opts, nodes.DockerBuild, build); err != nil {
		return report, err
	}
	if !build.Passed {
		report.Passed = false
		_ = writeReport(report, opts.WriteReport)
		return report, fmt.Errorf("docker compose build failed")
	}

	testsDir := filepath.Join(taskDir, "tests")
	initial, err := runCommand(ctx, exec, timeout, commandOutputDir(opts, nodes.InitialVerify), taskDir, nodes.InitialVerify, "docker", "compose", "-p", project, "-f", compose, "--project-directory", taskDir, "run", "--rm", "-v", testsDir+":/tests:ro", "main", "/bin/sh", "-c", "/tests/test.sh")
	if err != nil {
		return report, err
	}
	report.InitialExposesIssue = initialVerificationExposesIssue(initial)
	initial.Passed = report.InitialExposesIssue
	report.InitialVerify = &initial
	addCommand(&report, initial)
	if err := writeCommandArtifact(opts, nodes.InitialVerify, initial); err != nil {
		return report, err
	}
	if !report.InitialExposesIssue {
		if err := runComposeCleanup(exec, timeout, opts, &report, taskDir, compose, project); err != nil {
			return report, err
		}
		report.Passed = false
		_ = writeReport(report, opts.WriteReport)
		return report, fmt.Errorf("initial verification unexpectedly passed")
	}

	solutionDir := filepath.Join(taskDir, "solution")
	oracle, err := runCommand(ctx, exec, timeout, commandOutputDir(opts, nodes.OracleVerify), taskDir, nodes.OracleVerify, "docker", "compose", "-p", project, "-f", compose, "--project-directory", taskDir, "run", "--rm", "-v", solutionDir+":/solution:ro", "-v", testsDir+":/tests:ro", "main", "/bin/sh", "-c", "/solution/solve.sh && /tests/test.sh")
	if err != nil {
		return report, err
	}
	oracle.Passed = oracle.ExitCode == 0 && !oracle.Timeout
	report.OracleVerify = &oracle
	addCommand(&report, oracle)
	if err := writeCommandArtifact(opts, nodes.OracleVerify, oracle); err != nil {
		return report, err
	}
	report.Passed = oracle.Passed
	if err := runComposeCleanup(exec, timeout, opts, &report, taskDir, compose, project); err != nil {
		return report, err
	}
	if err := writeReport(report, opts.WriteReport); err != nil {
		return report, err
	}
	if !oracle.Passed {
		return report, fmt.Errorf("oracle verification failed")
	}
	return report, nil
}

func runComposeCleanup(exec executor.CommandRunner, timeout time.Duration, opts Options, report *domain.VerifyReport, taskDir, compose, project string) error {
	cleanup, err := runCommand(context.Background(), exec, timeout, commandOutputDir(opts, "docker_compose_down"), taskDir, "docker_compose_down", "docker", "compose", "-p", project, "-f", compose, "--project-directory", taskDir, "down", "--volumes", "--remove-orphans")
	if err != nil {
		return err
	}
	cleanup.Passed = cleanup.ExitCode == 0 && !cleanup.Timeout
	report.Cleanup = &cleanup
	addCommand(report, cleanup)
	return nil
}

func runCommand(ctx context.Context, exec executor.CommandRunner, timeout time.Duration, outputDir, dir, name, command string, args ...string) (domain.CommandRun, error) {
	start := time.Now().UTC()
	result := exec.Run(ctx, timeout, dir, nil, command, args...)
	finish := time.Now().UTC()
	exitCode := result.ExitCode
	if result.Err != nil && exitCode == 0 {
		exitCode = -1
	}
	stdout := commandlog.RedactText(result.Stdout)
	stderr := commandlog.RedactText(result.Stderr)
	stdoutPath, stderrPath, outputErr := commandlog.WriteOutputFiles(outputDir, stdout, stderr)
	run := domain.CommandRun{
		Name:         name,
		Command:      commandlog.RedactText(result.Command),
		Argv:         commandlog.RedactArgv(append([]string{command}, args...)),
		Attempt:      1,
		Dir:          commandlog.ResolveCWD(dir),
		Env:          commandlog.RedactEnv(commandlog.EffectiveEnv(nil)),
		ExitCode:     exitCode,
		Stdout:       stdout,
		Stderr:       stderr,
		StdoutPath:   stdoutPath,
		StderrPath:   stderrPath,
		Timeout:      result.Timeout,
		FailureClass: commandlog.ClassifyFailure(exitCode, result.Timeout, stdout, stderr),
		StartedAt:    start,
		FinishedAt:   finish,
		DurationMS:   finish.Sub(start).Milliseconds(),
	}
	if outputErr != nil {
		return run, outputErr
	}
	return run, nil
}

func initialVerificationExposesIssue(run domain.CommandRun) bool {
	if run.Timeout || run.ExitCode == 0 {
		return false
	}
	switch run.FailureClass {
	case "missing_tool_or_path", "network_or_timeout", "permission_or_auth", "docker_daemon":
		return false
	default:
		return true
	}
}

func addCommand(report *domain.VerifyReport, run domain.CommandRun) {
	report.CommandLogs = append(report.CommandLogs, run)
}

func commandOutputDir(opts Options, name string) string {
	workspace := strings.TrimSpace(opts.Workspace)
	if workspace != "" {
		return nodes.VerifyCommandLogDir(workspace, name)
	}
	if strings.TrimSpace(opts.WriteReport) != "" {
		return filepath.Join(filepath.Dir(opts.WriteReport), "command_logs", name)
	}
	return ""
}

func writeCommandArtifact(opts Options, nodeID string, run domain.CommandRun) error {
	path := commandArtifactPath(opts, nodeID)
	if strings.TrimSpace(path) == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func commandArtifactPath(opts Options, nodeID string) string {
	workspace := strings.TrimSpace(opts.Workspace)
	if workspace != "" {
		return nodes.PrimaryArtifactPath(workspace, nodeID)
	}
	if strings.TrimSpace(opts.WriteReport) != "" {
		filename := filepath.Base(nodes.PrimaryArtifactPath(nodes.DefaultWorkspace(""), nodeID))
		return filepath.Join(filepath.Dir(opts.WriteReport), nodeID, filename)
	}
	return ""
}

func writeReport(report domain.VerifyReport, path string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	report = sanitize.VerifyReport(report)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
