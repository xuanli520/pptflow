package scheduler_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/db"
	"github.com/xuanli520/p2r_tui/internal/pipeline"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/scanner"
	"github.com/xuanli520/p2r_tui/internal/scheduler"
)

func TestSubmitQueuesAndRejectsDuplicateTask(t *testing.T) {
	s := newTestScheduler(t, time.Second, "TASK-1", "TASK-2")

	firstJobID, err := s.Submit("TASK-1", pipeline.RunOptions{Stage: "A"})
	if err != nil {
		t.Fatal(err)
	}
	if firstJobID == "" {
		t.Fatal("job id should be returned")
	}
	secondJobID, err := s.Submit("TASK-2", pipeline.RunOptions{Stage: "A"})
	if err != nil {
		t.Fatal(err)
	}
	if secondJobID == "" {
		t.Fatal("queued job id should be returned")
	}
	if _, err := s.Submit("TASK-2", pipeline.RunOptions{}); err == nil || !strings.Contains(err.Error(), "already has an active job") {
		t.Fatalf("duplicate active task should be rejected, got %v", err)
	}

	queued := waitForSnapshot(t, s, 2*time.Second, func(snapshot scheduler.JobSnapshot) bool {
		return snapshot.TaskID == "TASK-2" && snapshot.State == scheduler.JobQueued
	})
	if queued.JobID != secondJobID {
		t.Fatalf("queued snapshot job id = %s, want %s", queued.JobID, secondJobID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	queued = snapshotByTask(t, s, "TASK-2")
	if queued.State != scheduler.JobFailed || queued.Err == "" {
		t.Fatalf("queued shutdown snapshot = %#v", queued)
	}
}

func TestSnapshotTracksRunProgressAndStageCompletion(t *testing.T) {
	s := newTestScheduler(t, time.Second, "TASK-1")

	if _, err := s.Submit("TASK-1", pipeline.RunOptions{Stage: "A"}); err != nil {
		t.Fatal(err)
	}
	running := waitForSnapshot(t, s, 2*time.Second, func(snapshot scheduler.JobSnapshot) bool {
		return snapshot.TaskID == "TASK-1" &&
			snapshot.RunID != "" &&
			snapshot.State == scheduler.JobRunning &&
			snapshot.CurrentStage == "A" &&
			stageStatus(snapshot.Stages, "A") == model.StageRunning
	})

	done := waitForSnapshot(t, s, 6*time.Second, func(snapshot scheduler.JobSnapshot) bool {
		return snapshot.TaskID == "TASK-1" && snapshot.State == scheduler.JobDone
	})
	if done.RunID != running.RunID || done.CurrentStage != "" {
		t.Fatalf("done snapshot = %#v, running snapshot = %#v", done, running)
	}
	if status := stageStatus(done.Stages, "A"); status != model.StageDone {
		t.Fatalf("stage A status = %s, want %s; stages = %#v", status, model.StageDone, done.Stages)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestCancelQueuedJobDoesNotReleaseRunningSlot(t *testing.T) {
	s := newTestScheduler(t, 800*time.Millisecond, "TASK-1", "TASK-2", "TASK-3")

	if _, err := s.Submit("TASK-1", pipeline.RunOptions{Stage: "A"}); err != nil {
		t.Fatal(err)
	}
	waitForSnapshot(t, s, 2*time.Second, func(snapshot scheduler.JobSnapshot) bool {
		return snapshot.TaskID == "TASK-1" && snapshot.State == scheduler.JobRunning
	})
	if _, err := s.Submit("TASK-2", pipeline.RunOptions{Stage: "A"}); err != nil {
		t.Fatal(err)
	}
	queued := waitForSnapshot(t, s, 2*time.Second, func(snapshot scheduler.JobSnapshot) bool {
		return snapshot.TaskID == "TASK-2" && snapshot.State == scheduler.JobQueued
	})
	if err := s.CancelTask("TASK-2"); err != nil {
		t.Fatal(err)
	}
	cancelled := waitForSnapshot(t, s, 2*time.Second, func(snapshot scheduler.JobSnapshot) bool {
		return snapshot.JobID == queued.JobID && snapshot.State == scheduler.JobCancelled && snapshot.CancelRequested
	})
	if cancelled.Err != scheduler.ErrJobCancelledByUser.Error() {
		t.Fatalf("cancelled queued err = %q", cancelled.Err)
	}
	if _, err := s.Submit("TASK-3", pipeline.RunOptions{Stage: "A"}); err != nil {
		t.Fatal(err)
	}
	third := waitForSnapshot(t, s, 150*time.Millisecond, func(snapshot scheduler.JobSnapshot) bool {
		return snapshot.TaskID == "TASK-3" && snapshot.State == scheduler.JobQueued
	})
	if third.State != scheduler.JobQueued {
		t.Fatalf("third job should remain queued while first holds slot: %#v", third)
	}
	waitForSnapshot(t, s, 4*time.Second, func(snapshot scheduler.JobSnapshot) bool {
		return snapshot.TaskID == "TASK-3" && snapshot.State == scheduler.JobDone
	})
}

func TestCancelRunningJobMarksUserCancelledAndKeepsPartialRun(t *testing.T) {
	s := newTestScheduler(t, 3*time.Second, "TASK-1")

	if _, err := s.Submit("TASK-1", pipeline.RunOptions{Stage: "A"}); err != nil {
		t.Fatal(err)
	}
	running := waitForSnapshot(t, s, 2*time.Second, func(snapshot scheduler.JobSnapshot) bool {
		return snapshot.TaskID == "TASK-1" && snapshot.State == scheduler.JobRunning && snapshot.RunID != ""
	})
	if err := s.CancelTask("TASK-1"); err != nil {
		t.Fatal(err)
	}
	cancelled := waitForSnapshot(t, s, 6*time.Second, func(snapshot scheduler.JobSnapshot) bool {
		return snapshot.JobID == running.JobID && snapshot.State == scheduler.JobCancelled && snapshot.CancelRequested
	})
	if cancelled.Err != scheduler.ErrJobCancelledByUser.Error() {
		t.Fatalf("cancelled running err = %q", cancelled.Err)
	}
	if cancelled.RunID == "" || cancelled.Result == nil || cancelled.Result.Run.Status != model.RunAborted {
		t.Fatalf("cancelled job should keep aborted partial result: %#v", cancelled)
	}
	if len(cancelled.Stages) == 0 {
		t.Fatalf("cancelled job should retain stage snapshots: %#v", cancelled)
	}
	if err := s.CancelTask("TASK-1"); err == nil || !strings.Contains(err.Error(), "no active job") {
		t.Fatalf("completed cancelled job should no longer be active, got %v", err)
	}
	if !errors.Is(fmt.Errorf("wrapped: %w", scheduler.ErrJobCancelledByUser), scheduler.ErrJobCancelledByUser) {
		t.Fatal("sentinel error should be stable")
	}
}

func TestSchedulerUsesRunnerFactory(t *testing.T) {
	cfg := config.Default()
	cfg.Pipeline.MaxConcurrent = 1
	called := make(chan string, 1)
	factory := func(store *db.Store, cfg config.Config) scheduler.PipelineRunner {
		return fakePipelineRunner{
			run: func(ctx context.Context, taskID string, opts pipeline.RunOptions) (pipeline.Result, error) {
				opts.Progress(pipeline.RunProgress{
					RunID:       "run-factory",
					Stage:       "A",
					Event:       pipeline.EventStageRunning,
					StageRecord: model.StageRecord{Stage: "A", Status: model.StageRunning},
				})
				called <- taskID
				return pipeline.Result{
					Run: model.RunRecord{RunID: "run-factory", TaskID: taskID, Status: model.RunCompletedClean},
					Stages: []model.StageRecord{
						{Stage: "A", Status: model.StageDone},
					},
				}, nil
			},
		}
	}
	s := scheduler.New(nil, cfg, scheduler.WithRunnerFactory(factory))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})

	if _, err := s.Submit("TASK-FACTORY", pipeline.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	done := waitForSnapshot(t, s, 2*time.Second, func(snapshot scheduler.JobSnapshot) bool {
		return snapshot.TaskID == "TASK-FACTORY" && snapshot.State == scheduler.JobDone
	})
	if done.RunID != "run-factory" || stageStatus(done.Stages, "A") != model.StageDone {
		t.Fatalf("factory runner result not reflected in snapshot: %#v", done)
	}
	select {
	case got := <-called:
		if got != "TASK-FACTORY" {
			t.Fatalf("factory runner task = %s", got)
		}
	default:
		t.Fatal("factory runner was not called")
	}
}

func TestSchedulerBuffersAppendStreamInSnapshot(t *testing.T) {
	cfg := config.Default()
	cfg.Pipeline.MaxConcurrent = 1
	release := make(chan struct{})
	factory := func(store *db.Store, cfg config.Config) scheduler.PipelineRunner {
		return fakePipelineRunner{
			run: func(ctx context.Context, taskID string, opts pipeline.RunOptions) (pipeline.Result, error) {
				opts.Progress(pipeline.RunProgress{
					RunID:       "run-stream",
					Stage:       "B",
					Event:       pipeline.EventStageRunning,
					StageRecord: model.StageRecord{Stage: "B", Status: model.StageRunning},
				})
				for i := 0; i < 205; i++ {
					opts.Progress(pipeline.RunProgress{
						RunID: "run-stream",
						Stage: "B",
						Event: pipeline.EventStageStream,
						Stream: &pipeline.StreamUpdate{
							Stage:  "B",
							Mode:   pipeline.StreamModeAppend,
							Delta:  fmt.Sprintf("line-%03d\n", i),
							Source: "stdout",
						},
					})
				}
				<-release
				return pipeline.Result{
					Run:    model.RunRecord{RunID: "run-stream", TaskID: taskID, Status: model.RunCompletedClean},
					Stages: []model.StageRecord{{Stage: "B", Status: model.StageDone}},
				}, nil
			},
		}
	}
	s := scheduler.New(nil, cfg, scheduler.WithRunnerFactory(factory))
	t.Cleanup(func() {
		close(release)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})

	if _, err := s.Submit("TASK-STREAM", pipeline.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	snapshot := waitForSnapshot(t, s, 2*time.Second, func(snapshot scheduler.JobSnapshot) bool {
		stream := snapshot.StreamByStage["B"]
		return snapshot.TaskID == "TASK-STREAM" && snapshot.State == scheduler.JobRunning && len(stream.Lines) == 200 && stream.Truncated
	})
	stream := snapshot.StreamByStage["B"]
	if stream.Text == "" || stream.Lines[0].Text != "line-005" || stream.Lines[len(stream.Lines)-1].Text != "line-204" {
		t.Fatalf("append stream buffer not retained as tail window: %#v", stream)
	}

	stream.Lines[0].Text = "mutated"
	again := snapshotByTask(t, s, "TASK-STREAM")
	if again.StreamByStage["B"].Lines[0].Text == "mutated" {
		t.Fatalf("snapshot stream lines should be deep copied")
	}
}

func TestSchedulerCumulativeStreamDoesNotChangeStageState(t *testing.T) {
	cfg := config.Default()
	release := make(chan struct{})
	factory := func(store *db.Store, cfg config.Config) scheduler.PipelineRunner {
		return fakePipelineRunner{
			run: func(ctx context.Context, taskID string, opts pipeline.RunOptions) (pipeline.Result, error) {
				opts.Progress(pipeline.RunProgress{
					RunID:       "run-codex",
					Stage:       "D",
					Event:       pipeline.EventStageRunning,
					StageRecord: model.StageRecord{Stage: "D", Status: model.StageRunning},
				})
				opts.Progress(pipeline.RunProgress{
					RunID: "run-codex",
					Stage: "D",
					Event: pipeline.EventStageStream,
					Stream: &pipeline.StreamUpdate{
						Stage:  "D",
						Mode:   pipeline.StreamModeCumulative,
						ItemID: "item-1",
						Text:   "streaming review text",
					},
				})
				<-release
				return pipeline.Result{
					Run:    model.RunRecord{RunID: "run-codex", TaskID: taskID, Status: model.RunCompletedClean},
					Stages: []model.StageRecord{{Stage: "D", Status: model.StageDone}},
				}, nil
			},
		}
	}
	s := scheduler.New(nil, cfg, scheduler.WithRunnerFactory(factory))
	t.Cleanup(func() {
		close(release)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})

	if _, err := s.Submit("TASK-CODEX", pipeline.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	snapshot := waitForSnapshot(t, s, 2*time.Second, func(snapshot scheduler.JobSnapshot) bool {
		return snapshot.TaskID == "TASK-CODEX" && snapshot.StreamByStage["D"].Text == "streaming review text"
	})
	if snapshot.CurrentStage != "D" || stageStatus(snapshot.Stages, "D") != model.StageRunning {
		t.Fatalf("stream event should not mutate stage state: %#v", snapshot)
	}
}

type fakePipelineRunner struct {
	run func(context.Context, string, pipeline.RunOptions) (pipeline.Result, error)
}

func (f fakePipelineRunner) Run(ctx context.Context, taskID string, opts pipeline.RunOptions) (pipeline.Result, error) {
	return f.run(ctx, taskID, opts)
}

func newTestScheduler(t *testing.T, firstScriptDelay time.Duration, taskIDs ...string) *scheduler.Scheduler {
	t.Helper()
	installFakePython(t, firstScriptDelay)

	scanPath := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "index.db")
	cfg := config.Default()
	cfg.ScanPath = scanPath
	cfg.DBPath = dbPath
	cfg.Codex.PromptProfilesDir = filepath.Join(t.TempDir(), "missing-prompt-profiles")
	cfg.Pipeline.MaxConcurrent = 1
	cfg.Pipeline.StageTimeouts["A"] = 3

	store, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	projects := make([]scanner.Project, 0, len(taskIDs))
	for _, taskID := range taskIDs {
		projectPath := filepath.Join(scanPath, "batch-1", taskID, taskID)
		for _, dir := range []string{"docs", "repo", "original_sessions"} {
			if err := os.MkdirAll(filepath.Join(projectPath, dir), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		metadata := []byte(`{"prompt":"Build a small app","project_type":"pure_frontend"}`)
		if err := os.WriteFile(filepath.Join(projectPath, "metadata.json"), metadata, 0o644); err != nil {
			t.Fatal(err)
		}
		projects = append(projects, scanner.Project{TaskID: taskID, Batch: "batch-1", Path: projectPath})
	}
	if err := store.UpsertProjects(context.Background(), projects); err != nil {
		t.Fatal(err)
	}

	s := scheduler.New(store, cfg)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
		_ = store.Close()
	})
	return s
}

func installFakePython(t *testing.T, firstScriptDelay time.Duration) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake PATH python shim is Unix-specific")
	}
	binDir := t.TempDir()
	delayMarker := filepath.Join(t.TempDir(), "first-script")
	pythonPath := filepath.Join(binDir, "python")
	content := fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail
if [ "${1:-}" = "--version" ]; then
	echo "Python 3.11.0"
	exit 0
fi
if mkdir %q 2>/dev/null; then
	sleep %q
fi
exit 0
`, delayMarker, fmt.Sprintf("%.3f", firstScriptDelay.Seconds()))
	if err := os.WriteFile(pythonPath, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func waitForSnapshot(t *testing.T, s *scheduler.Scheduler, timeout time.Duration, match func(scheduler.JobSnapshot) bool) scheduler.JobSnapshot {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last []scheduler.JobSnapshot
	for time.Now().Before(deadline) {
		last = s.Snapshot()
		for _, snapshot := range last {
			if match(snapshot) {
				return snapshot
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for scheduler snapshot; last = %#v", last)
	return scheduler.JobSnapshot{}
}

func snapshotByTask(t *testing.T, s *scheduler.Scheduler, taskID string) scheduler.JobSnapshot {
	t.Helper()
	for _, snapshot := range s.Snapshot() {
		if snapshot.TaskID == taskID {
			return snapshot
		}
	}
	t.Fatalf("snapshot for task %s not found", taskID)
	return scheduler.JobSnapshot{}
}

func stageStatus(stages []model.StageRecord, stage string) string {
	for _, record := range stages {
		if record.Stage == stage {
			return record.Status
		}
	}
	return ""
}
