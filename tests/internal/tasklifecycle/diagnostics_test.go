package tasklifecycle_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/db"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/scheduler"
	"github.com/xuanli520/p2r_tui/internal/tasklifecycle"
)

type diagnosticScheduler struct {
	jobs []scheduler.JobSnapshot
}

func (s *diagnosticScheduler) Submit(context.Context, scheduler.SubmitRequest) (scheduler.SubmitResult, error) {
	return scheduler.SubmitResult{}, nil
}

func (s *diagnosticScheduler) ActiveSnapshot() []scheduler.JobSnapshot {
	return append([]scheduler.JobSnapshot(nil), s.jobs...)
}

func TestDiagnoseAndRepairInspectingWithoutRunningRun(t *testing.T) {
	ctx := context.Background()
	store, cfg := newDiagnosticStore(t)
	taskID := "TASK-20260521-ABC001"
	task, err := store.CreateTaskWithBatch(ctx, taskID, "https://gitlab.example/"+taskID, cfg.ScanPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRun(ctx, model.RunRecord{
		RunID:        "run-old",
		TaskID:       task.ID,
		StartedAt:    time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		Status:       model.RunRunning,
		ArtifactRoot: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, "run-old", task.ID, model.RunAborted, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := store.ReopenTaskForInspection(ctx, task.ID); err != nil {
		t.Fatal(err)
	}

	manager := tasklifecycle.NewManager(store, cfg, &diagnosticScheduler{})
	snapshot, err := manager.DiagnoseTask(ctx, task.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasIssue(snapshot.Issues, tasklifecycle.DiagnosticInspectingWithoutRun) {
		t.Fatalf("expected inspecting-without-run issue: %#v", snapshot.Issues)
	}
	result, err := manager.RepairTaskDiagnostics(ctx, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if result.Policy != tasklifecycle.FixTerminalReset || result.LogPath == "" {
		t.Fatalf("repair result = %#v", result)
	}
	repaired, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if repaired.State != model.TaskCompleted || repaired.CurrentRunID != "" {
		t.Fatalf("task should be terminal-reset to completed: %#v", repaired)
	}
	events, err := store.TaskEvents(ctx, task.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if !hasEvent(events, "diagnostic_terminal_reset") {
		t.Fatalf("repair should record event: %#v", events)
	}
}

func TestDiagnoseReportsPreRunFailureOnly(t *testing.T) {
	ctx := context.Background()
	store, cfg := newDiagnosticStore(t)
	taskID := "TASK-20260521-ABC002"
	if _, err := store.CreateTaskWithBatch(ctx, taskID, "https://gitlab.example/"+taskID, cfg.ScanPath); err != nil {
		t.Fatal(err)
	}
	failureDir := filepath.Join(cfg.ScanPath, ".qa-control", "run-failures")
	if err := os.MkdirAll(failureDir, 0o755); err != nil {
		t.Fatal(err)
	}
	failurePath := filepath.Join(failureDir, "task-20260521-abc002_create-run.json")
	if err := os.WriteFile(failurePath, []byte(`{"phase":"CreateRun","error":"db unavailable","created_at":"2026-06-15T00:00:00Z"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	manager := tasklifecycle.NewManager(store, cfg, &diagnosticScheduler{})
	snapshot, err := manager.DiagnoseTask(ctx, taskID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.FailureFiles) != 1 || snapshot.FailureFiles[0].Phase != "CreateRun" {
		t.Fatalf("failure files = %#v", snapshot.FailureFiles)
	}
	if !hasIssue(snapshot.Issues, tasklifecycle.DiagnosticPreRunFailureOnly) {
		t.Fatalf("expected pre-run failure issue: %#v", snapshot.Issues)
	}
}

func TestRepairRefreshesActiveJobsBeforeTerminalReset(t *testing.T) {
	ctx := context.Background()
	store, cfg := newDiagnosticStore(t)
	taskID := "TASK-20260521-ABC003"
	if _, err := store.CreateTaskWithBatch(ctx, taskID, "https://gitlab.example/"+taskID, cfg.ScanPath); err != nil {
		t.Fatal(err)
	}
	sched := &diagnosticScheduler{}
	manager := tasklifecycle.NewManager(store, cfg, sched)
	snapshot, err := manager.DiagnoseTask(ctx, taskID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasIssue(snapshot.Issues, tasklifecycle.DiagnosticInspectingWithoutRun) {
		t.Fatalf("expected repairable issue before job appears: %#v", snapshot.Issues)
	}

	sched.jobs = []scheduler.JobSnapshot{{TaskID: taskID, JobID: "job-live", State: scheduler.JobRunning}}
	result, err := manager.RepairTaskDiagnostics(ctx, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if result.Policy != tasklifecycle.FixNoFix || len(result.FixedIssues) != 0 {
		t.Fatalf("repair should re-evaluate with live job and avoid reset: %#v", result)
	}
	task, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != model.TaskInspecting {
		t.Fatalf("task should remain active while live job exists: %#v", task)
	}
}

func TestRepairStopsCompletedDockerLeak(t *testing.T) {
	ctx := context.Background()
	store, cfg := newDiagnosticStore(t)
	taskID := "TASK-20260521-ABC004"
	task, err := store.CreateTaskWithBatch(ctx, taskID, "https://gitlab.example/"+taskID, cfg.ScanPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRun(ctx, model.RunRecord{
		RunID:        "run-clean",
		TaskID:       task.ID,
		StartedAt:    time.Now().UTC().Format(time.RFC3339),
		Status:       model.RunRunning,
		ArtifactRoot: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, "run-clean", task.ID, model.RunCompletedClean, time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordTaskRuntime(ctx, task.ID, "", true, model.ComposeMeta{}); err != nil {
		t.Fatal(err)
	}

	manager := tasklifecycle.NewManager(store, cfg, &diagnosticScheduler{})
	snapshot, err := manager.DiagnoseTask(ctx, task.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasIssue(snapshot.Issues, tasklifecycle.DiagnosticDockerLeakedDone) {
		t.Fatalf("expected completed docker leak issue: %#v", snapshot.Issues)
	}
	result, err := manager.RepairTaskDiagnostics(ctx, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if result.Policy != tasklifecycle.FixStopLeakedDocker || result.LogPath == "" {
		t.Fatalf("repair result = %#v", result)
	}
	repaired, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if repaired.DockerRunning {
		t.Fatalf("docker flag should be cleared: %#v", repaired)
	}
	events, err := store.TaskEvents(ctx, task.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if !hasEvent(events, "diagnostic_stop_leaked_docker") {
		t.Fatalf("docker repair should record event: %#v", events)
	}
}

func newDiagnosticStore(t *testing.T) (*db.Store, config.Config) {
	t.Helper()
	root := t.TempDir()
	cfg := config.Default()
	cfg.ScanPath = root
	cfg.DBPath = filepath.Join(root, ".qa-control", "index.db")
	store, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, cfg
}

func hasIssue(issues []tasklifecycle.DiagnosticIssue, code tasklifecycle.DiagnosticCode) bool {
	for _, issue := range issues {
		if issue.Code == code {
			return true
		}
	}
	return false
}

func hasEvent(events []model.TaskEvent, kind string) bool {
	for _, event := range events {
		if event.Kind == kind {
			return true
		}
	}
	return false
}
