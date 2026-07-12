package verify

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/commandlog"
	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	harborverify "github.com/purplevoid/harbor-factory/internal/harbor/verify"
	"github.com/purplevoid/harbor-factory/internal/plugins/pluginutil"
	"github.com/purplevoid/harbor-factory/internal/workflow"
)

func environmentFor(req workflow.NodeRequest) (harborverify.Environment, error) {
	imageTag := pluginutil.String(req, "image_tag")
	if imageTag == "" {
		imageTag = "harbor-task-" + safeName(req.RunID) + "-harbor_verify"
	}
	project := pluginutil.String(req, "compose_project")
	if project == "" {
		project = "harbor-task-" + safeName(req.RunID) + "-harbor_verify"
	}
	return harborverify.ResolveEnvironment(pluginutil.String(req, "task_dir"), imageTag, project)
}

func executeCommand(ctx context.Context, req workflow.NodeRequest, spec harborverify.CommandSpec) (domain.CommandRun, error) {
	if req.Runtimes.Command == nil {
		return domain.CommandRun{}, fmt.Errorf("command runtime is required")
	}
	timeout := pluginutil.Int(req, "timeout_seconds")
	if timeout <= 0 {
		timeout = req.Spec.Policy.TimeoutSeconds
	}
	started := time.Now().UTC()
	result, runErr := req.Runtimes.Command.Run(ctx, workflow.CommandRequest{
		Dir: spec.Dir, Command: spec.Command, Args: append([]string(nil), spec.Args...), TimeoutSeconds: timeout,
	})
	finished := time.Now().UTC()
	exitCode := result.ExitCode
	if runErr != nil && exitCode == 0 {
		exitCode = -1
	}
	stdout := commandlog.RedactText(result.Stdout)
	stderr := commandlog.RedactText(result.Stderr)
	attempt := req.Attempt
	if attempt <= 0 {
		attempt = 1
	}
	run := domain.CommandRun{
		Name: spec.Name, Command: commandlog.RedactText(result.Command),
		Argv: commandlog.RedactArgv(append([]string{spec.Command}, spec.Args...)),
		Dir:  commandlog.ResolveCWD(spec.Dir), Env: commandlog.RedactEnv(commandlog.EffectiveEnv(nil)),
		Attempt: attempt, ExitCode: exitCode, Stdout: stdout, Stderr: stderr, Timeout: result.Timeout,
		FailureClass: commandlog.ClassifyFailure(exitCode, result.Timeout, stdout, stderr),
		StartedAt:    started, FinishedAt: finished, DurationMS: finished.Sub(started).Milliseconds(),
	}
	if run.Command == "" {
		run.Command = strings.Join(run.Argv, " ")
	}
	return run, runErr
}

func storeCommand(ctx context.Context, req workflow.NodeRequest, run *domain.CommandRun) ([]workflow.ArtifactRef, error) {
	if req.Store == nil {
		return nil, fmt.Errorf("artifact store is required")
	}
	base := filepath.ToSlash(filepath.Join("phase2", "artifacts", req.Spec.ID))
	stdoutRef, err := req.Store.PutText(ctx, base+"/stdout.log", "command_stdout", req.Spec.ID, run.Stdout)
	if err != nil {
		return nil, err
	}
	stderrRef, err := req.Store.PutText(ctx, base+"/stderr.log", "command_stderr", req.Spec.ID, run.Stderr)
	if err != nil {
		return nil, err
	}
	run.StdoutPath = stdoutRef.Path
	run.StderrPath = stderrRef.Path
	commandRef, err := req.Store.PutJSON(ctx, pluginutil.ArtifactName(req, base+"/"+commandArtifactFilename(req.Spec.ID)), "command_run", req.Spec.ID, run)
	if err != nil {
		return nil, err
	}
	return []workflow.ArtifactRef{commandRef, stdoutRef, stderrRef}, nil
}

func commandArtifactFilename(nodeID string) string {
	switch nodeID {
	case "docker_build":
		return "build_result.json"
	case "initial_verify":
		return "initial_result.json"
	case "oracle_verify":
		return "oracle_result.json"
	default:
		return "command_run.json"
	}
}

func commandFailure(run domain.CommandRun, err error, operation string) error {
	if errors.Is(err, context.Canceled) {
		return workflow.NewNodeError(workflow.FailureCanceled, false, operation, err)
	}
	if errors.Is(err, context.DeadlineExceeded) || run.Timeout {
		return workflow.NewNodeError(workflow.FailureTimeout, true, operation, firstError(err, fmt.Errorf("command timed out")))
	}
	if err != nil {
		return workflow.NewNodeError(workflow.FailureTransient, true, operation, err)
	}
	return workflow.NewNodeError(workflow.FailurePermanent, false, operation, fmt.Errorf("command exited with code %d", run.ExitCode))
}

func firstError(values ...error) error {
	for _, err := range values {
		if err != nil {
			return err
		}
	}
	return nil
}

func safeName(value string) string {
	value = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, value)
	value = strings.Trim(value, "-_")
	if value == "" {
		return "run"
	}
	return value
}
