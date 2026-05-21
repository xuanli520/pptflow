package tui_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/db"
	"github.com/xuanli520/p2r_tui/internal/executor"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	tuiapp "github.com/xuanli520/p2r_tui/internal/tui"
)

func TestConfirmCompleteSkipsDockerCleanupWhenDaemonUnavailable(t *testing.T) {
	store, cfg, taskID := waitingManualTask(t)
	exec := &confirmCompleteExec{dockerInfoErr: errors.New("daemon unavailable")}

	if err := tuiapp.ConfirmCompleteForTest(context.Background(), store, cfg, exec, taskID); err != nil {
		t.Fatal(err)
	}
	task, err := store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != model.TaskCompleted || task.CompletionCount != 1 || task.DockerRunning {
		t.Fatalf("task should complete when daemon is unavailable: %#v", task)
	}
	if _, err := os.Stat(tuiapp.CleanupCheckpointPathForTest(cfg.ScanPath)); !os.IsNotExist(err) {
		t.Fatalf("cleanup checkpoint should be removed, stat err=%v", err)
	}
	if exec.hasCommand(" compose ") {
		t.Fatalf("compose cleanup should be skipped when docker info fails: %#v", exec.commands)
	}
}

func waitingManualTask(t *testing.T) (*db.Store, config.Config, string) {
	t.Helper()
	ctx := context.Background()
	cfg := config.Default()
	cfg.ScanPath = t.TempDir()
	store, err := db.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	task, err := store.CreateTaskWithBatch(ctx, "TASK-20260521-DDDDDD", "https://gitlab.example/TASK-20260521-DDDDDD", cfg.ScanPath)
	if err != nil {
		t.Fatal(err)
	}
	runID := "run-waiting"
	if err := store.CreateRun(ctx, model.RunRecord{
		RunID:        runID,
		TaskID:       task.ID,
		StartedAt:    time.Now().UTC().Format(time.RFC3339),
		Status:       model.RunRunning,
		ArtifactRoot: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordTaskRuntime(ctx, task.ID, "http://localhost:38080", true, model.ComposeMeta{
		Project:      "p2r_test",
		ComposeFiles: []string{"compose.yml"},
		WorkDir:      task.RepoPath,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, runID, task.ID, model.RunCompletedClean, time.Second); err != nil {
		t.Fatal(err)
	}
	return store, cfg, task.ID
}

type confirmCompleteExec struct {
	dockerInfoErr error
	commands      []string
}

func (confirmCompleteExec) LookPath(name string) (string, error) {
	return name, nil
}

func (e *confirmCompleteExec) Run(ctx context.Context, timeout time.Duration, dir string, env []string, name string, args ...string) executor.Result {
	command := strings.Join(append([]string{name}, args...), " ")
	e.commands = append(e.commands, command)
	if command == "docker info" && e.dockerInfoErr != nil {
		return executor.Result{Command: command, Err: e.dockerInfoErr}
	}
	return executor.Result{Command: command}
}

func (e *confirmCompleteExec) RunStreamingWithOutput(ctx context.Context, timeout time.Duration, dir string, env []string, writer io.Writer, onOutput executor.OutputCallback, name string, args ...string) executor.Result {
	return e.Run(ctx, timeout, dir, env, name, args...)
}

func (e *confirmCompleteExec) hasCommand(fragment string) bool {
	for _, command := range e.commands {
		if strings.Contains(command, fragment) {
			return true
		}
	}
	return false
}
