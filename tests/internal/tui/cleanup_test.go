package tui_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/executor"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	tuiapp "github.com/xuanli520/p2r_tui/internal/tui"
)

func TestForceExitCleanupReportsOnlyFailedTaskCleanup(t *testing.T) {
	cfg := config.Default()
	cfg.ScanPath = t.TempDir()
	runner := &tuiCleanupRunner{failProject: "p2r_fail"}
	tasks := []tuiapp.TaskProject{
		{ID: "TASK-OK", DockerRunning: true, ComposeMeta: model.ComposeMeta{Project: "p2r_ok", ComposeFiles: []string{"compose.yml"}, WorkDir: "/tmp/ok"}},
		{ID: "TASK-FAIL", DockerRunning: true, ComposeMeta: model.ComposeMeta{Project: "p2r_fail", ComposeFiles: []string{"compose.yml"}, WorkDir: "/tmp/fail"}},
		{ID: "TASK-STOPPED", DockerRunning: false, ComposeMeta: model.ComposeMeta{Project: "p2r_stopped"}},
	}

	err := tuiapp.ForceExitCleanupForTest(context.Background(), cfg, runner, tasks)
	if err == nil {
		t.Fatal("expected cleanup error")
	}
	text := err.Error()
	if !strings.Contains(text, "TASK-FAIL") || strings.Contains(text, "TASK-OK") || strings.Contains(text, "TASK-STOPPED") {
		t.Fatalf("error should map only failed running cleanup to task ID, got %q", text)
	}
	if count := strings.Count(text, "cleanup TASK-FAIL"); count != 1 {
		t.Fatalf("failed task cleanup should be reported once, got %d in %q", count, text)
	}

	stopped, err := tuiapp.ForceExitCleanupStoppedForTest(context.Background(), cfg, &tuiCleanupRunner{failProject: "p2r_fail"}, tasks)
	if err == nil {
		t.Fatal("expected cleanup error with partial success")
	}
	if len(stopped) != 1 || stopped[0] != "TASK-OK" {
		t.Fatalf("partial cleanup should report successfully stopped tasks, got %#v", stopped)
	}
}

type tuiCleanupRunner struct {
	failProject string
	commands    []string
}

func (r *tuiCleanupRunner) LookPath(name string) (string, error) {
	return name, nil
}

func (r *tuiCleanupRunner) Run(ctx context.Context, timeout time.Duration, dir string, env []string, name string, args ...string) executor.Result {
	command := strings.Join(append([]string{name}, args...), " ")
	r.commands = append(r.commands, command)
	if strings.Contains(command, " compose ") && strings.Contains(command, " -p "+r.failProject+" down ") {
		return executor.Result{Command: command, Err: errors.New("compose down failed"), Stderr: "permission denied"}
	}
	return executor.Result{Command: command}
}

func (r *tuiCleanupRunner) RunStreamingWithOutput(ctx context.Context, timeout time.Duration, dir string, env []string, writer io.Writer, onOutput executor.OutputCallback, name string, args ...string) executor.Result {
	return r.Run(ctx, timeout, dir, env, name, args...)
}
