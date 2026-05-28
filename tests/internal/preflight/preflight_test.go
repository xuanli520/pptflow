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
	result := preflight.Run(context.Background(), preflightExec{}, config.Default())
	for _, check := range result.Checks {
		if check.Name == "bash" {
			return
		}
	}
	t.Fatalf("host Stage C should keep bash preflight: %#v", result.Checks)
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
