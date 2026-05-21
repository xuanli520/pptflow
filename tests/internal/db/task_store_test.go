package db_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/xuanli520/p2r_tui/internal/db"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
)

func TestTaskLifecycleTransitions(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	scanPath := t.TempDir()
	task, err := store.CreateTaskWithBatch(ctx, "TASK-20260521-ABCDEF", "https://gitlab.example/TASK-20260521-ABCDEF", scanPath)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != model.TaskInspecting || task.BatchID != "batch-001" {
		t.Fatalf("created task = %#v", task)
	}
	if want := filepath.Join(scanPath, "batch-001", task.ID, task.ID); task.RepoPath != want {
		t.Fatalf("repo path = %q, want %q", task.RepoPath, want)
	}

	if err := store.CreateRun(ctx, model.RunRecord{
		RunID:        "run-1",
		TaskID:       task.ID,
		StartedAt:    time.Now().UTC().Format(time.RFC3339),
		Status:       model.RunRunning,
		ArtifactRoot: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	task, err = store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task.CurrentRunID != "run-1" || task.State != model.TaskInspecting {
		t.Fatalf("running task = %#v", task)
	}
	run, err := store.GetRun(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if run.CompletionRound != 1 {
		t.Fatalf("completion round = %d, want 1", run.CompletionRound)
	}

	meta := model.ComposeMeta{Project: "p2rqa_task", ComposeFiles: []string{"docker-compose.yml"}, WorkDir: task.RepoPath}
	if err := store.RecordTaskRuntime(ctx, task.ID, "http://localhost:3000", true, meta); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, "run-1", task.ID, model.RunCompletedWithFindings, time.Second); err != nil {
		t.Fatal(err)
	}
	task, err = store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != model.TaskWaitingManual || task.CurrentRunID != "" || !task.DockerRunning || task.FrontendURL == "" || task.EnteredWaitingAt == "" {
		t.Fatalf("waiting task = %#v", task)
	}
	if task.ComposeMeta.Project != meta.Project || task.ComposeMeta.WorkDir != meta.WorkDir {
		t.Fatalf("compose meta = %#v, want %#v", task.ComposeMeta, meta)
	}

	task, err = store.CompleteTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != model.TaskCompleted || task.CompletionCount != 1 || task.DockerRunning || task.LastCompletedAt == "" {
		t.Fatalf("completed task = %#v", task)
	}

	if err := store.RecordTaskGitError(ctx, task.ID, assertErr("network timeout")); err != nil {
		t.Fatal(err)
	}
	task, err = store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != model.TaskCompleted || task.SyncError == "" {
		t.Fatalf("failed force-pull should leave task completed with sync error: %#v", task)
	}

	if err := store.CreateRun(ctx, model.RunRecord{
		RunID:        "run-2",
		TaskID:       task.ID,
		StartedAt:    time.Now().UTC().Format(time.RFC3339),
		Status:       model.RunRunning,
		ArtifactRoot: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	task, err = store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != model.TaskInspecting || task.CurrentRunID != "run-2" || task.SyncError != "" {
		t.Fatalf("reinspection running task = %#v", task)
	}
	run, err = store.GetRun(ctx, "run-2")
	if err != nil {
		t.Fatal(err)
	}
	if run.CompletionRound != 2 {
		t.Fatalf("second completion round = %d, want 2", run.CompletionRound)
	}
}

func TestProjectQueryFiltersByTaskState(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scanPath := t.TempDir()

	inspecting, err := store.CreateTaskWithBatch(ctx, "TASK-20260521-AAAAAA", "https://gitlab.example/TASK-20260521-AAAAAA", scanPath)
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := store.CreateTaskWithBatch(ctx, "TASK-20260521-BBBBBB", "https://gitlab.example/TASK-20260521-BBBBBB", scanPath)
	if err != nil {
		t.Fatal(err)
	}
	completed, err := store.CreateTaskWithBatch(ctx, "TASK-20260521-CCCCCC", "https://gitlab.example/TASK-20260521-CCCCCC", scanPath)
	if err != nil {
		t.Fatal(err)
	}
	finishTaskRun(t, store, waiting.ID)
	finishTaskRun(t, store, completed.ID)
	if _, err := store.CompleteTask(ctx, completed.ID); err != nil {
		t.Fatal(err)
	}

	projects, total, err := store.ListProjectsPaginated(ctx, db.ProjectQuery{
		Search: db.ProjectSearch{Terms: []db.ProjectSearchTerm{{TaskStates: []string{model.TaskWaitingManual}}}},
		Limit:  20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(projects) != 1 || projects[0].TaskID != waiting.ID {
		t.Fatalf("waiting filter total=%d projects=%#v; inspecting=%s completed=%s", total, projects, inspecting.ID, completed.ID)
	}
}

func finishTaskRun(t *testing.T, store *db.Store, taskID string) {
	t.Helper()
	ctx := context.Background()
	runID := "run-" + taskID
	if err := store.CreateRun(ctx, model.RunRecord{
		RunID:        runID,
		TaskID:       taskID,
		StartedAt:    time.Now().UTC().Format(time.RFC3339),
		Status:       model.RunRunning,
		ArtifactRoot: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, runID, taskID, model.RunCompletedClean, time.Second); err != nil {
		t.Fatal(err)
	}
}

type assertErr string

func (e assertErr) Error() string {
	return string(e)
}
