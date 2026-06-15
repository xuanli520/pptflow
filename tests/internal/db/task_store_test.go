package db_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
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

func TestTerminalGitErrorReleasesInspectingCapacity(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scanPath := t.TempDir()

	task, err := store.CreateTaskWithBatch(ctx, "TASK-20260521-BADPKG", "https://gitlab.example/TASK-20260521-BADPKG", scanPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordTaskTerminalGitError(ctx, task.ID, assertErr("missing repo/")); err != nil {
		t.Fatal(err)
	}
	task, err = store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != model.TaskCompleted || task.CurrentRunID != "" || !strings.Contains(task.SyncError, "missing repo/") {
		t.Fatalf("terminal git failure should leave completed task with sync error: %#v", task)
	}
	if count, err := store.CountTasksByState(ctx, model.TaskInspecting); err != nil || count != 0 {
		t.Fatalf("inspecting count = %d, err=%v", count, err)
	}
	for i := 0; i < db.ActiveTaskStateLimit; i++ {
		taskID := fmt.Sprintf("TASK-20260521-CAP%02d", i)
		if _, err := store.CreateTaskWithBatch(ctx, taskID, "https://gitlab.example/"+taskID, scanPath); err != nil {
			t.Fatalf("terminal git failure should not consume capacity, create %s: %v", taskID, err)
		}
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

func TestFinishRunAbortedClearsTaskDockerRuntime(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	task, err := store.CreateTaskWithBatch(ctx, "TASK-20260521-DDDDDD", "https://gitlab.example/TASK-20260521-DDDDDD", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runID := "run-aborted"
	if err := store.CreateRun(ctx, model.RunRecord{
		RunID:        runID,
		TaskID:       task.ID,
		StartedAt:    time.Now().UTC().Format(time.RFC3339),
		Status:       model.RunRunning,
		ArtifactRoot: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordTaskRuntime(ctx, task.ID, "http://localhost:3000", true, model.ComposeMeta{Project: "p2r_abort", ComposeFiles: []string{"compose.yml"}, WorkDir: task.RepoPath}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, runID, task.ID, model.RunAborted, time.Second); err != nil {
		t.Fatal(err)
	}
	task, err = store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != model.TaskCompleted || task.CurrentRunID != "" || task.DockerRunning || task.FrontendURL != "" || task.ComposeMeta.Project != "" {
		t.Fatalf("aborted run should clear task runtime: %#v", task)
	}
}

func TestRepairTaskStatesCompletesLegacyAbortedInspectingTask(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "index.db")
	store, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	task, err := store.CreateTaskWithBatch(ctx, "TASK-20260521-CCCCCC", "https://gitlab.example/TASK-20260521-CCCCCC", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runID := "run-legacy-aborted"
	if err := store.CreateRun(ctx, model.RunRecord{
		RunID:        runID,
		TaskID:       task.ID,
		StartedAt:    time.Now().UTC().Format(time.RFC3339),
		Status:       model.RunRunning,
		ArtifactRoot: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, runID, task.ID, model.RunAborted, time.Second); err != nil {
		t.Fatal(err)
	}
	handle, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Exec(`UPDATE tasks SET state = ?, current_run_id = NULL, docker_running = 1, frontend_url = 'http://localhost:3000', compose_meta = '{}' WHERE id = ?`, model.TaskInspecting, task.ID); err != nil {
		_ = handle.Close()
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}

	if err := store.RepairTaskStates(ctx); err != nil {
		t.Fatal(err)
	}
	task, err = store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != model.TaskCompleted || task.CurrentRunID != "" || task.DockerRunning || task.FrontendURL != "" || task.ComposeMeta.Project != "" {
		t.Fatalf("legacy aborted inspecting task should be completed and runtime-cleared: %#v", task)
	}
}

func TestFinishRunRejectsTaskRunMismatch(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	task, err := store.CreateTaskWithBatch(ctx, "TASK-20260521-EEEEEE", "https://gitlab.example/TASK-20260521-EEEEEE", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRun(ctx, model.RunRecord{
		RunID:        "run-old",
		TaskID:       task.ID,
		StartedAt:    time.Now().UTC().Format(time.RFC3339),
		Status:       model.RunRunning,
		ArtifactRoot: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, "run-old", task.ID, model.RunAborted, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRun(ctx, model.RunRecord{
		RunID:        "run-active",
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
	if task.CurrentRunID != "run-active" {
		t.Fatalf("active run was not attached to task: %#v", task)
	}

	if err := store.FinishRun(ctx, "run-old", task.ID, model.RunCompletedClean, time.Second); err == nil {
		t.Fatal("expected finishing a non-current run to fail")
	}
	task, err = store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task.CurrentRunID != "run-active" || task.State != model.TaskInspecting {
		t.Fatalf("mismatched finish should leave active task unchanged: %#v", task)
	}
	run, err := store.GetRun(ctx, "run-old")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != model.RunAborted {
		t.Fatalf("mismatched finish should roll back run update, got %s", run.Status)
	}
}

func TestTaskRunCASAndCompletionCountUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	task, err := store.CreateTaskWithBatch(ctx, "TASK-20260521-FFFFFF", "https://gitlab.example/TASK-20260521-FFFFFF", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	createErrs := runConcurrently(2, func(index int) error {
		return store.CreateRun(ctx, model.RunRecord{
			RunID:        []string{"run-cas-1", "run-cas-2"}[index],
			TaskID:       task.ID,
			StartedAt:    time.Now().UTC().Format(time.RFC3339),
			Status:       model.RunRunning,
			ArtifactRoot: t.TempDir(),
		})
	})
	if countNil(createErrs) != 1 || countErr(createErrs) != 1 {
		t.Fatalf("concurrent CreateRun errors = %#v", createErrs)
	}
	task, err = store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task.CurrentRunID == "" || task.State != model.TaskInspecting {
		t.Fatalf("one run should be attached after CAS race: %#v", task)
	}
	if err := store.FinishRun(ctx, task.CurrentRunID, task.ID, model.RunCompletedClean, time.Second); err != nil {
		t.Fatal(err)
	}

	completeErrs := runConcurrently(2, func(int) error {
		_, err := store.CompleteTask(ctx, task.ID)
		return err
	})
	if countNil(completeErrs) != 1 || countErr(completeErrs) != 1 {
		t.Fatalf("concurrent CompleteTask errors = %#v", completeErrs)
	}
	task, err = store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != model.TaskCompleted || task.CompletionCount != 1 {
		t.Fatalf("completion count should increment once: %#v", task)
	}
}

func TestCompleteTaskWithVerdictArchivesCompletedOverflow(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	var firstTaskID string
	for i := 0; i < db.CompletedTaskStateLimit+1; i++ {
		taskID := fmt.Sprintf("TASK-20260521-A%05X", i)
		if i == 0 {
			firstTaskID = taskID
		}
		task, err := store.CreateTaskWithBatch(ctx, taskID, "https://gitlab.example/"+taskID, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		finishTaskRun(t, store, task.ID)
		if _, err := store.CompleteTaskWithVerdict(ctx, task.ID, model.ManualPass); err != nil {
			t.Fatal(err)
		}
	}

	count, err := store.CountTasksByState(ctx, model.TaskCompleted)
	if err != nil {
		t.Fatal(err)
	}
	if count != db.CompletedTaskStateLimit {
		t.Fatalf("visible completed count = %d, want %d", count, db.CompletedTaskStateLimit)
	}
	task, err := store.GetTask(ctx, firstTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.ArchivedAt == "" {
		t.Fatalf("oldest completed task should be archived: %#v", task)
	}
}

func TestCreateRunRejectsWaitingManualTask(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	task, err := store.CreateTaskWithBatch(ctx, "TASK-20260521-333333", "https://gitlab.example/TASK-20260521-333333", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	finishTaskRun(t, store, task.ID)

	err = store.CreateRun(ctx, model.RunRecord{
		RunID:        "run-waiting-manual",
		TaskID:       task.ID,
		StartedAt:    time.Now().UTC().Format(time.RFC3339),
		Status:       model.RunRunning,
		ArtifactRoot: t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "waiting for manual verdict") {
		t.Fatalf("expected waiting manual CreateRun rejection, got %v", err)
	}
	if _, err := store.GetRun(ctx, "run-waiting-manual"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("rejected run should not be inserted, err=%v", err)
	}
	task, err = store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != model.TaskWaitingManual || task.CurrentRunID != "" {
		t.Fatalf("waiting task should remain unchanged: %#v", task)
	}
}

func TestFinishRunRejectsMissingAndMismatchedRun(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first, err := store.CreateTaskWithBatch(ctx, "TASK-20260521-444444", "https://gitlab.example/TASK-20260521-444444", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateTaskWithBatch(ctx, "TASK-20260521-555555", "https://gitlab.example/TASK-20260521-555555", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRun(ctx, model.RunRecord{
		RunID:        "run-first",
		TaskID:       first.ID,
		StartedAt:    time.Now().UTC().Format(time.RFC3339),
		Status:       model.RunRunning,
		ArtifactRoot: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRun(ctx, model.RunRecord{
		RunID:        "run-second",
		TaskID:       second.ID,
		StartedAt:    time.Now().UTC().Format(time.RFC3339),
		Status:       model.RunRunning,
		ArtifactRoot: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}

	if err := store.FinishRun(ctx, "run-missing", first.ID, model.RunCompletedClean, time.Second); err == nil {
		t.Fatal("expected missing run finish to fail")
	}
	if err := store.FinishRun(ctx, "run-first", second.ID, model.RunCompletedClean, time.Second); err == nil {
		t.Fatal("expected mismatched run/task finish to fail")
	}
	run, err := store.GetRun(ctx, "run-first")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != model.RunRunning {
		t.Fatalf("mismatched finish should roll back run update, got %s", run.Status)
	}
	second, err = store.GetTask(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.CurrentRunID != "run-second" || second.State != model.TaskInspecting {
		t.Fatalf("mismatched finish should leave other task active: %#v", second)
	}
}

func TestFinishRunRejectsIdleTaskForOldRun(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	task, err := store.CreateTaskWithBatch(ctx, "TASK-20260521-666666", "https://gitlab.example/TASK-20260521-666666", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRun(ctx, model.RunRecord{
		RunID:        "run-old-idle",
		TaskID:       task.ID,
		StartedAt:    time.Now().UTC().Format(time.RFC3339),
		Status:       model.RunRunning,
		ArtifactRoot: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, "run-old-idle", task.ID, model.RunAborted, time.Second); err != nil {
		t.Fatal(err)
	}

	if err := store.FinishRun(ctx, "run-old-idle", task.ID, model.RunCompletedClean, time.Second); err == nil {
		t.Fatal("expected old idle run finish to fail")
	}
	task, err = store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != model.TaskCompleted || task.CurrentRunID != "" {
		t.Fatalf("old idle finish should not reopen task: %#v", task)
	}
	run, err := store.GetRun(ctx, "run-old-idle")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != model.RunAborted {
		t.Fatalf("old idle finish should roll back run update, got %s", run.Status)
	}
}

func TestWaitingManualRuntimeUpdateRejectsCompletedTask(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	task, err := store.CreateTaskWithBatch(ctx, "TASK-20260521-111111", "https://gitlab.example/TASK-20260521-111111", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	finishTaskRun(t, store, task.ID)
	if _, err := store.CompleteTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	meta := model.ComposeMeta{Project: "p2r_stale", ComposeFiles: []string{"compose.yml"}, WorkDir: task.RepoPath}
	if err := store.RecordWaitingManualTaskRuntime(ctx, task.ID, "http://localhost:3000", true, meta); err == nil {
		t.Fatal("expected completed task runtime update to fail")
	}
	task, err = store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != model.TaskCompleted || task.DockerRunning || task.FrontendURL != "" || task.ComposeMeta.Project != "" {
		t.Fatalf("completed task should not be marked running: %#v", task)
	}
}

func TestStageRuntimeUpdateRejectsStaleTaskRun(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	task, err := store.CreateTaskWithBatch(ctx, "TASK-20260521-222222", "https://gitlab.example/TASK-20260521-222222", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRun(ctx, model.RunRecord{
		RunID:        "run-stale",
		TaskID:       task.ID,
		StartedAt:    time.Now().UTC().Format(time.RFC3339),
		Status:       model.RunRunning,
		ArtifactRoot: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, "run-stale", task.ID, model.RunCompletedClean, time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRun(ctx, model.RunRecord{
		RunID:        "run-active",
		TaskID:       task.ID,
		StartedAt:    time.Now().UTC().Format(time.RFC3339),
		Status:       model.RunRunning,
		ArtifactRoot: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	stage := model.StageRecord{Stage: string(model.StageB), Status: model.StageDone}
	meta := model.ComposeMeta{Project: "p2r_stale_stage", ComposeFiles: []string{"compose.yml"}, WorkDir: task.RepoPath}
	if err := store.PutStageAndRecordTaskRuntime(ctx, "run-stale", stage, task.ID, "http://localhost:3000", true, meta); err == nil {
		t.Fatal("expected stale run runtime update to fail")
	}
	task, err = store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task.CurrentRunID != "run-active" || task.DockerRunning || task.FrontendURL != "" || task.ComposeMeta.Project != "" {
		t.Fatalf("stale run should not modify active task runtime: %#v", task)
	}
	stages, err := store.Stages(ctx, "run-stale")
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range stages {
		if got.Stage == string(model.StageB) {
			t.Fatalf("stale runtime write should roll back stage upsert: %#v", stages)
		}
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

func runConcurrently(n int, fn func(int) error) []error {
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		index := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[index] = fn(index)
		}()
	}
	close(start)
	wg.Wait()
	return errs
}

func countNil(errs []error) int {
	count := 0
	for _, err := range errs {
		if err == nil {
			count++
		}
	}
	return count
}

func countErr(errs []error) int {
	return len(errs) - countNil(errs)
}
