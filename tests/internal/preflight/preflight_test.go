package preflight_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/xuanli520/p2r_tui/internal/codex"
	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/executor"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/preflight"
)

func TestValidateExtraArgsRejectsBoundaryFlags(t *testing.T) {
	if _, err := codex.ValidateAppServerExtraArgs([]string{"--model", "gpt-5.4"}); err != nil {
		t.Fatalf("safe args rejected: %s", err)
	}
	for _, flag := range []string{"--full-auto", "--search", "--dangerously-bypass-approvals-and-sandbox"} {
		_, err := codex.ValidateAppServerExtraArgs([]string{flag})
		if err == nil || !strings.Contains(err.Error(), flag) {
			t.Fatalf("expected %s to be rejected, got %v", flag, err)
		}
	}
	if _, err := codex.ValidateAppServerExtraArgs([]string{"--search=true"}); err == nil || !strings.Contains(err.Error(), "--search") {
		t.Fatalf("expected --search=... to be rejected, got %v", err)
	}
}

func TestIsolatedStageCDoesNotRequireHostBash(t *testing.T) {
	cfg := config.Default()
	cfg.Pipeline.StageC.Execution = "isolated"

	result := preflight.Run(context.Background(), preflightExec{}, cfg)
	if _, ok := result.BlockingCheck("C"); ok {
		t.Fatalf("isolated Stage C should not be blocked by host bash: %#v", result.Checks)
	}
	for _, check := range result.Checks {
		if check.Name == "bash" {
			t.Fatalf("isolated Stage C should not run host bash preflight: %#v", result.Checks)
		}
	}
}

func TestHostStageCKeepsBashPreflight(t *testing.T) {
	cfg := config.Default()
	cfg.Pipeline.StageC.Execution = "host"

	result := preflight.Run(context.Background(), preflightExec{}, cfg)
	for _, check := range result.Checks {
		if check.Name == "bash" {
			return
		}
	}
	t.Fatalf("host Stage C should keep bash preflight: %#v", result.Checks)
}

func TestAutoStageCWithRunnerDoesNotRequireHostBash(t *testing.T) {
	cfg := config.Default()
	cfg.Pipeline.StageC.Execution = "auto"
	cfg.Pipeline.StageC.RunnerImage = "p2r/stage-c-runner:test"

	result := preflight.Run(context.Background(), preflightExec{}, cfg)
	for _, check := range result.Checks {
		if check.Name == "bash" {
			t.Fatalf("auto Stage C with isolated runner should not run host bash preflight: %#v", result.Checks)
		}
	}
}

func TestAutoStageCWithoutRunnerKeepsBashPreflight(t *testing.T) {
	cfg := config.Default()
	cfg.Pipeline.StageC.Execution = "auto"
	cfg.Pipeline.StageC.RunnerImage = ""

	result := preflight.Run(context.Background(), preflightExec{}, cfg)
	for _, check := range result.Checks {
		if check.Name == "bash" {
			return
		}
	}
	t.Fatalf("auto Stage C without runner should keep bash preflight: %#v", result.Checks)
}

func TestDockerMissingBlocksRuntimeChain(t *testing.T) {
	result := preflight.Run(context.Background(), missingDockerExec{}, config.Default())
	for _, stage := range []string{string(model.StageB), string(model.StageG), string(model.StageC)} {
		if _, ok := result.BlockingCheck(stage); !ok {
			t.Fatalf("docker missing should block %s: %#v", stage, result.Checks)
		}
	}
}

func TestPlaywrightMissingOnlyBlocksStageG(t *testing.T) {
	result := preflight.Run(context.Background(), missingPlaywrightExec{}, config.Default())
	if _, ok := result.BlockingCheck(string(model.StageG)); !ok {
		t.Fatalf("playwright missing should block G: %#v", result.Checks)
	}
	for _, stage := range []string{string(model.StageB), string(model.StageC)} {
		if check, ok := result.BlockingCheck(stage); ok && check.Name == "playwright" {
			t.Fatalf("playwright should not block %s: %#v", stage, result.Checks)
		}
	}
}

type preflightExec struct{}

func (preflightExec) LookPath(name string) (string, error) {
	if name == "bash" {
		return "", errors.New("missing")
	}
	return name, nil
}

func (preflightExec) Run(ctx context.Context, timeout time.Duration, dir string, env []string, name string, args ...string) executor.Result {
	return executor.Result{Command: strings.Join(append([]string{name}, args...), " "), Stdout: name + " version\n"}
}

func (preflightExec) RunStreamingWithOutput(ctx context.Context, timeout time.Duration, dir string, env []string, writer io.Writer, onOutput executor.OutputCallback, name string, args ...string) executor.Result {
	return executor.Result{Command: strings.Join(append([]string{name}, args...), " ")}
}

type missingDockerExec struct {
	preflightExec
}

func (missingDockerExec) LookPath(name string) (string, error) {
	if name == "docker" {
		return "", errors.New("missing docker")
	}
	return preflightExec{}.LookPath(name)
}

type missingPlaywrightExec struct {
	preflightExec
}

func (missingPlaywrightExec) Run(ctx context.Context, timeout time.Duration, dir string, env []string, name string, args ...string) executor.Result {
	command := strings.Join(append([]string{name}, args...), " ")
	if name == "node" && len(args) > 0 && args[0] == "-e" {
		return executor.Result{Command: command, Stderr: "Cannot find module 'playwright'", Err: errors.New("playwright missing")}
	}
	return preflightExec{}.Run(ctx, timeout, dir, env, name, args...)
}
