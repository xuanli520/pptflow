package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
)

type scheduledRunnerFunc func(context.Context) (domain.RunSummary, error)

func (f scheduledRunnerFunc) Run(ctx context.Context) (domain.RunSummary, error) {
	return f(ctx)
}

func TestTaskSchedulerRejectsInvalidConcurrency(t *testing.T) {
	for _, limit := range []int{0, -1, MaxTaskConcurrency + 1} {
		if scheduler, err := NewTaskScheduler(context.Background(), limit); err == nil || scheduler != nil {
			t.Fatalf("NewTaskScheduler(%d) = (%v, %v), want validation error", limit, scheduler, err)
		}
	}
}

func TestTaskSchedulerBoundsConcurrencyAndDrainsQueue(t *testing.T) {
	const limit = 3
	release := make(chan struct{})
	var active atomic.Int32
	var maximum atomic.Int32
	scheduler, err := newTaskScheduler(context.Background(), limit, func(RunnerOptions) (*Runner, scheduledRunner) {
		return nil, scheduledRunnerFunc(func(ctx context.Context) (domain.RunSummary, error) {
			current := active.Add(1)
			defer active.Add(-1)
			for {
				observed := maximum.Load()
				if current <= observed || maximum.CompareAndSwap(observed, current) {
					break
				}
			}
			select {
			case <-release:
				return domain.RunSummary{Status: "succeeded", Passed: true}, nil
			case <-ctx.Done():
				return domain.RunSummary{}, ctx.Err()
			}
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	defer scheduler.Close()

	for i := 0; i < 8; i++ {
		workspace := filepath.Join(t.TempDir(), fmt.Sprintf("workspace-%d", i))
		if _, err := scheduler.Submit(RunnerOptions{Workspace: workspace, TaskName: fmt.Sprintf("task-%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	waitForScheduler(t, scheduler, func(snapshot TaskSchedulerSnapshot) bool {
		return snapshot.Running == limit && snapshot.Queued == 5
	})
	if got := maximum.Load(); got != limit {
		t.Fatalf("maximum active runners = %d, want %d", got, limit)
	}
	close(release)
	waitForScheduler(t, scheduler, func(snapshot TaskSchedulerSnapshot) bool {
		return snapshot.Succeeded == 8 && snapshot.Running == 0 && snapshot.Queued == 0
	})
}

func TestTaskSchedulerIsolatesFailureAndRejectsDuplicateWorkspace(t *testing.T) {
	scheduler, err := newTaskScheduler(context.Background(), 2, func(opts RunnerOptions) (*Runner, scheduledRunner) {
		return nil, scheduledRunnerFunc(func(context.Context) (domain.RunSummary, error) {
			if opts.TaskName == "bad" {
				return domain.RunSummary{Status: "failed"}, errors.New("isolated failure")
			}
			return domain.RunSummary{Status: "succeeded", Passed: true}, nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	defer scheduler.Close()

	workspace := filepath.Join(t.TempDir(), "shared")
	if _, err := scheduler.Submit(RunnerOptions{Workspace: workspace, TaskName: "bad"}); err != nil {
		t.Fatal(err)
	}
	if !scheduler.HasWorkspace(workspace) {
		t.Fatal("submitted workspace was not reserved")
	}
	if _, err := scheduler.Submit(RunnerOptions{Workspace: workspace, TaskName: "duplicate"}); !errors.Is(err, ErrTaskWorkspaceScheduled) {
		t.Fatalf("duplicate submit error = %v, want ErrTaskWorkspaceScheduled", err)
	}
	if _, err := scheduler.Submit(RunnerOptions{Workspace: filepath.Join(t.TempDir(), "good"), TaskName: "good"}); err != nil {
		t.Fatal(err)
	}
	waitForScheduler(t, scheduler, func(snapshot TaskSchedulerSnapshot) bool {
		return snapshot.Failed == 1 && snapshot.Succeeded == 1
	})
	snapshot := scheduler.Snapshot()
	if len(snapshot.Tasks) != 2 || snapshot.Tasks[0].TaskName != "bad" || snapshot.Tasks[1].TaskName != "good" {
		t.Fatalf("tasks are not in stable submission order: %+v", snapshot.Tasks)
	}
	for _, task := range snapshot.Tasks {
		if task.TaskName == "bad" && task.Error != "isolated failure" {
			t.Fatalf("failed task error = %q", task.Error)
		}
	}
}

func TestTaskSchedulerCancelsRunningTaskWithoutStoppingOthers(t *testing.T) {
	started := make(chan string, 2)
	scheduler, err := newTaskScheduler(context.Background(), 2, func(opts RunnerOptions) (*Runner, scheduledRunner) {
		return nil, scheduledRunnerFunc(func(ctx context.Context) (domain.RunSummary, error) {
			started <- opts.Workspace
			if opts.TaskName == "keep" {
				return domain.RunSummary{Status: "succeeded", Passed: true}, nil
			}
			<-ctx.Done()
			return domain.RunSummary{}, ctx.Err()
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	defer scheduler.Close()
	cancelWorkspace := filepath.Join(t.TempDir(), "cancel")
	if _, err := scheduler.Submit(RunnerOptions{Workspace: cancelWorkspace, TaskName: "cancel"}); err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.Submit(RunnerOptions{Workspace: filepath.Join(t.TempDir(), "keep"), TaskName: "keep"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("scheduled task did not start")
		}
	}
	if !scheduler.CancelWorkspace(cancelWorkspace) {
		t.Fatal("running task was not canceled")
	}
	waitForScheduler(t, scheduler, func(snapshot TaskSchedulerSnapshot) bool {
		return snapshot.Canceled == 1 && snapshot.Succeeded == 1
	})
}

func TestTaskSchedulerCancelAndCloseWaitForRunningTasks(t *testing.T) {
	started := make(chan string, 2)
	finished := make(chan string, 2)
	scheduler, err := newTaskScheduler(context.Background(), 1, func(opts RunnerOptions) (*Runner, scheduledRunner) {
		return nil, scheduledRunnerFunc(func(ctx context.Context) (domain.RunSummary, error) {
			started <- opts.Workspace
			<-ctx.Done()
			finished <- opts.Workspace
			return domain.RunSummary{}, ctx.Err()
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(t.TempDir(), "first")
	second := filepath.Join(t.TempDir(), "second")
	if _, err := scheduler.Submit(RunnerOptions{Workspace: first}); err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.Submit(RunnerOptions{Workspace: second}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first task did not start")
	}
	if !scheduler.CancelWorkspace(second) {
		t.Fatal("queued task was not canceled")
	}
	scheduler.Close()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("Close returned before the running task observed cancellation")
	}
	snapshot := scheduler.Snapshot()
	if snapshot.Canceled != 2 || snapshot.Running != 0 || snapshot.Queued != 0 {
		t.Fatalf("snapshot after Close = %+v", snapshot)
	}
	if _, err := scheduler.Submit(RunnerOptions{Workspace: filepath.Join(t.TempDir(), "late")}); !errors.Is(err, ErrTaskSchedulerClosed) {
		t.Fatalf("submit after Close error = %v", err)
	}
}

func TestTaskSchedulerDrainsRunnerEventsBeyondBufferCapacity(t *testing.T) {
	scheduler, err := newTaskScheduler(context.Background(), 1, func(opts RunnerOptions) (*Runner, scheduledRunner) {
		runner := NewRunner(opts)
		executor := scheduledRunnerFunc(func(context.Context) (domain.RunSummary, error) {
			for i := 0; i < 4*cap(runner.events); i++ {
				runner.events <- domain.RunnerEvent{Type: "node_progress", Message: fmt.Sprintf("event-%d", i)}
			}
			close(runner.events)
			return domain.RunSummary{Status: "succeeded", Passed: true}, nil
		})
		return runner, executor
	})
	if err != nil {
		t.Fatal(err)
	}
	defer scheduler.Close()
	workspace := filepath.Join(t.TempDir(), "event-heavy")
	if _, err := scheduler.Submit(RunnerOptions{Workspace: workspace}); err != nil {
		t.Fatal(err)
	}
	waitForScheduler(t, scheduler, func(snapshot TaskSchedulerSnapshot) bool {
		return snapshot.Succeeded == 1
	})
	if runner, _, live := scheduler.RunnerForWorkspace(workspace); live || runner != nil {
		t.Fatal("terminal task exposed a writable live runner")
	}
}

func waitForScheduler(t *testing.T, scheduler *TaskScheduler, predicate func(TaskSchedulerSnapshot) bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if predicate(scheduler.Snapshot()) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("scheduler condition not met; final snapshot: %+v", scheduler.Snapshot())
}
