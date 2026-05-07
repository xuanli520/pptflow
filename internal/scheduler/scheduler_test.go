package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/pipeline"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
)

func TestSubmitQueuesAndRejectsDuplicateTask(t *testing.T) {
	cfg := config.Default()
	cfg.Pipeline.MaxConcurrent = 1
	s := New(nil, cfg)
	s.sem <- struct{}{}

	jobID, err := s.Submit("TASK-1", pipeline.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if jobID == "" {
		t.Fatal("job id should be returned")
	}
	if _, err := s.Submit("TASK-1", pipeline.RunOptions{}); err == nil || !strings.Contains(err.Error(), "already has an active job") {
		t.Fatalf("duplicate active task should be rejected, got %v", err)
	}
	snapshots := s.Snapshot()
	if len(snapshots) != 1 || snapshots[0].State != JobQueued || snapshots[0].TaskID != "TASK-1" {
		t.Fatalf("snapshot = %#v", snapshots)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshots = s.Snapshot()
	if len(snapshots) != 1 || snapshots[0].State != JobFailed || snapshots[0].Err == "" {
		t.Fatalf("queued shutdown snapshot = %#v", snapshots)
	}
}

func TestApplyProgressBindsRunAndUpdatesStages(t *testing.T) {
	s := New(nil, config.Default())
	job := &Job{JobID: "job-1", TaskID: "TASK-1", State: JobQueued}

	s.applyProgress(job, pipeline.RunProgress{RunID: "run-1", Event: "run_created"})
	s.applyProgress(job, pipeline.RunProgress{
		RunID:       "run-1",
		Stage:       "A",
		Event:       "stage_running",
		StageRecord: model.StageRecord{Stage: "A", Status: model.StageRunning},
	})
	s.applyProgress(job, pipeline.RunProgress{
		RunID:       "run-1",
		Stage:       "A",
		Event:       "stage_done",
		StageRecord: model.StageRecord{Stage: "A", Status: model.StageDone},
	})
	s.applyProgress(job, pipeline.RunProgress{RunID: "run-1", Event: "run_done", Done: true})

	snapshot := snapshotJob(job)
	if snapshot.RunID != "run-1" || snapshot.State != JobDone || snapshot.CurrentStage != "" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if len(snapshot.Stages) != 1 || snapshot.Stages[0].Status != model.StageDone {
		t.Fatalf("stages = %#v", snapshot.Stages)
	}
}

func TestStartJobHonorsShutdownBeforeGoroutineStarts(t *testing.T) {
	cfg := config.Default()
	cfg.Pipeline.MaxConcurrent = 1
	s := New(nil, cfg)
	s.sem <- struct{}{}
	job := &Job{JobID: "job-1", TaskID: "TASK-1", State: JobQueued}
	s.jobs = []*Job{job}
	s.jobByID[job.JobID] = job
	s.activeByTask[job.TaskID] = job
	s.closed = true

	s.startJob(job)

	job.mu.RLock()
	state := job.State
	err := job.Err
	job.mu.RUnlock()
	if state != JobFailed || !errors.Is(err, context.Canceled) {
		t.Fatalf("job state=%s err=%v, want failed canceled", state, err)
	}
	if len(s.sem) != 0 {
		t.Fatalf("semaphore slot was not released, len=%d", len(s.sem))
	}
	if s.activeByTask[job.TaskID] != nil {
		t.Fatalf("task should no longer be active after canceled start")
	}
}

func snapshotJob(job *Job) JobSnapshot {
	job.mu.RLock()
	defer job.mu.RUnlock()
	return JobSnapshot{
		JobID:        job.JobID,
		RunID:        job.RunID,
		TaskID:       job.TaskID,
		State:        job.State,
		CurrentStage: job.CurrentStage,
		Stages:       append([]model.StageRecord(nil), job.Stages...),
	}
}
