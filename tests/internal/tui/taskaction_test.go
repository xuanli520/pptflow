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
	"github.com/xuanli520/p2r_tui/internal/pipeline"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	tuiapp "github.com/xuanli520/p2r_tui/internal/tui"
)

func TestStartInspectionRecordsSyncErrorWhenSubmitFails(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.ScanPath = t.TempDir()
	writeTUIDropboxDoc(t, cfg.ScanPath, "TASK-20260521-ABCDEF")
	store, err := db.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scheduler := &failingInspectionScheduler{err: errors.New("scheduler down")}

	err = tuiapp.StartInspectionForTest(ctx, store, cfg, scheduler, "TASK-20260521-ABCDEF")
	if err == nil || !strings.Contains(err.Error(), "scheduler down") {
		t.Fatalf("expected scheduler failure, got %v", err)
	}
	if scheduler.calls != 1 {
		t.Fatalf("submit calls = %d, want 1", scheduler.calls)
	}
	task, err := store.GetTask(ctx, "TASK-20260521-ABCDEF")
	if err != nil {
		t.Fatal(err)
	}
	if task.State != model.TaskInspecting || task.CurrentRunID != "" || !strings.Contains(task.SyncError, "scheduler down") {
		t.Fatalf("failed submit should leave retryable inspecting task with sync error: %#v", task)
	}
}

func TestStartInspectionDocsGateDoesNotSubmitOrRecordSyncError(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.ScanPath = t.TempDir()
	store, err := db.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scheduler := &failingInspectionScheduler{err: errors.New("scheduler should not be called")}

	err = tuiapp.StartInspectionForTest(ctx, store, cfg, scheduler, "TASK-20260521-ABC123")
	if err == nil || !strings.Contains(err.Error(), "至少需要一个补充文档") {
		t.Fatalf("expected docs gate error, got %v", err)
	}
	if scheduler.calls != 0 {
		t.Fatalf("docs gate should not call scheduler, calls=%d", scheduler.calls)
	}
	task, err := store.GetTask(ctx, "TASK-20260521-ABC123")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(task.SyncError) != "" {
		t.Fatalf("docs gate should not be recorded as git sync error: %#v", task)
	}
}

func TestFindStaleInspectingOnlyReturnsRetryableOrphans(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.ScanPath = t.TempDir()
	store, err := db.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	stale, err := store.CreateTaskWithBatch(ctx, "TASK-20260521-AAAAAA", "https://gitlab.example/TASK-20260521-AAAAAA", cfg.ScanPath)
	if err != nil {
		t.Fatal(err)
	}
	withError, err := store.CreateTaskWithBatch(ctx, "TASK-20260521-BBBBBB", "https://gitlab.example/TASK-20260521-BBBBBB", cfg.ScanPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordTaskGitError(ctx, withError.ID, errors.New("auth failed")); err != nil {
		t.Fatal(err)
	}
	active, err := store.CreateTaskWithBatch(ctx, "TASK-20260521-CCCCCC", "https://gitlab.example/TASK-20260521-CCCCCC", cfg.ScanPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRun(ctx, model.RunRecord{
		RunID:        "run-active",
		TaskID:       active.ID,
		StartedAt:    time.Now().UTC().Format(time.RFC3339),
		Status:       model.RunRunning,
		ArtifactRoot: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	aborted, err := store.CreateTaskWithBatch(ctx, "TASK-20260521-DDDDDD", "https://gitlab.example/TASK-20260521-DDDDDD", cfg.ScanPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRun(ctx, model.RunRecord{
		RunID:        "run-aborted",
		TaskID:       aborted.ID,
		StartedAt:    time.Now().UTC().Format(time.RFC3339),
		Status:       model.RunRunning,
		ArtifactRoot: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, "run-aborted", aborted.ID, model.RunAborted, time.Second); err != nil {
		t.Fatal(err)
	}

	tasks, err := tuiapp.FindStaleInspectingForTest(ctx, store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].ID != stale.ID || tasks[0].GitURL == "" {
		t.Fatalf("stale inspecting tasks = %#v", tasks)
	}
}

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
	run, err := store.LatestRunForTask(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	if run.ManualVerdict != model.ManualPass {
		t.Fatalf("manual verdict = %s, want %s", run.ManualVerdict, model.ManualPass)
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

func writeTUIDropboxDoc(t *testing.T, scanPath, taskID string) {
	t.Helper()
	dropbox := filepath.Join(scanPath, "task-docs", taskID)
	if err := os.MkdirAll(dropbox, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dropbox, "notes.md"), []byte("extra context"), 0o644); err != nil {
		t.Fatal(err)
	}
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

type failingInspectionScheduler struct {
	err   error
	calls int
}

func (s *failingInspectionScheduler) SubmitInspection(taskID, batchID, gitURL string, opts pipeline.RunOptions) (string, error) {
	s.calls++
	return "", s.err
}
