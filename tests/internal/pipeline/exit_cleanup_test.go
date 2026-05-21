package pipeline_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/executor"
	pipelinepkg "github.com/xuanli520/p2r_tui/internal/pipeline"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
)

func TestForceExitCleanupKeepsGoingAfterProjectFailure(t *testing.T) {
	cfg := config.Default()
	cfg.ScanPath = t.TempDir()
	runner := &exitCleanupRunner{failProject: "p2r_fail"}

	summary, err := pipelinepkg.ForceExitCleanup(context.Background(), runner, cfg, []model.ComposeMeta{
		{Project: "p2r_ok", ComposeFiles: []string{"compose.yml"}, WorkDir: "/tmp/ok"},
		{Project: "p2r_fail", ComposeFiles: []string{"compose.yml"}, WorkDir: "/tmp/fail"},
	})
	if err == nil || !strings.Contains(err.Error(), "p2r_fail") {
		t.Fatalf("expected project cleanup error, got %v", err)
	}
	if len(summary.Runtime) != 2 {
		t.Fatalf("runtime summaries = %d, want 2", len(summary.Runtime))
	}
	if summary.Runtime[0].Status != "ok" || summary.Runtime[1].Status != "failed" {
		t.Fatalf("unexpected runtime statuses: %#v", summary.Runtime)
	}
	if !summary.GC.OK || summary.GC.Trigger != "force_exit" {
		t.Fatalf("force exit should still run label GC: %#v", summary.GC)
	}
	if !containsExitCleanupCommand(runner.commands, " -p p2r_ok down ") || !containsExitCleanupCommand(runner.commands, " -p p2r_fail down ") {
		t.Fatalf("expected both compose projects to be cleaned: %#v", runner.commands)
	}
}

func TestLightExitCleanupRunsOnlyLabelGC(t *testing.T) {
	cfg := config.Default()
	cfg.ScanPath = t.TempDir()
	runner := &exitCleanupRunner{}

	summary, err := pipelinepkg.LightExitCleanup(context.Background(), runner, cfg)
	if err != nil {
		t.Fatalf("light cleanup failed: %v", err)
	}
	if len(summary.Runtime) != 0 {
		t.Fatalf("light cleanup should not stop compose projects: %#v", summary.Runtime)
	}
	if !summary.GC.OK || summary.GC.Trigger != "light_exit" {
		t.Fatalf("unexpected GC summary: %#v", summary.GC)
	}
	if containsExitCleanupCommand(runner.commands, " compose ") {
		t.Fatalf("light cleanup should not run docker compose: %#v", runner.commands)
	}
}

type exitCleanupRunner struct {
	failProject string
	commands    []string
}

func (r *exitCleanupRunner) LookPath(name string) (string, error) {
	return name, nil
}

func (r *exitCleanupRunner) Run(ctx context.Context, timeout time.Duration, dir string, env []string, name string, args ...string) executor.Result {
	command := strings.Join(append([]string{name}, args...), " ")
	r.commands = append(r.commands, command)
	if strings.Contains(command, " compose ") && strings.Contains(command, " -p "+r.failProject+" down ") {
		return executor.Result{Command: command, Err: errors.New("compose down failed"), Stderr: "permission denied"}
	}
	return executor.Result{Command: command}
}

func (r *exitCleanupRunner) RunStreamingWithOutput(ctx context.Context, timeout time.Duration, dir string, env []string, writer io.Writer, onOutput executor.OutputCallback, name string, args ...string) executor.Result {
	return r.Run(ctx, timeout, dir, env, name, args...)
}

func containsExitCleanupCommand(commands []string, fragment string) bool {
	for _, command := range commands {
		if strings.Contains(command, fragment) {
			return true
		}
	}
	return false
}
