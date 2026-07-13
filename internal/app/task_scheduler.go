package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
)

const MaxTaskConcurrency = 10

var (
	ErrTaskSchedulerClosed    = errors.New("task scheduler is closed")
	ErrTaskWorkspaceScheduled = errors.New("workspace is already scheduled")
)

type TaskStatus string

const (
	TaskQueued    TaskStatus = "queued"
	TaskRunning   TaskStatus = "running"
	TaskSucceeded TaskStatus = "succeeded"
	TaskFailed    TaskStatus = "failed"
	TaskCanceled  TaskStatus = "canceled"
)

type TaskSnapshot struct {
	ID          string
	Workspace   string
	TaskName    string
	Status      TaskStatus
	SubmittedAt time.Time
	StartedAt   time.Time
	FinishedAt  time.Time
	Error       string
}

type TaskSchedulerSnapshot struct {
	Limit     int
	Queued    int
	Running   int
	Succeeded int
	Failed    int
	Canceled  int
	Tasks     []TaskSnapshot
}

type scheduledRunner interface {
	Run(context.Context) (domain.RunSummary, error)
}

type taskJob struct {
	snapshot TaskSnapshot
	runner   *Runner
	executor scheduledRunner
	ctx      context.Context
	cancel   context.CancelFunc
}

type TaskScheduler struct {
	ctx      context.Context
	cancel   context.CancelFunc
	limit    int
	factory  func(RunnerOptions) (*Runner, scheduledRunner)
	sequence atomic.Uint64

	mu        sync.Mutex
	cond      *sync.Cond
	queue     []*taskJob
	jobs      map[string]*taskJob
	workspace map[string]*taskJob
	closed    bool
	workers   sync.WaitGroup
}

func NewTaskScheduler(ctx context.Context, limit int) (*TaskScheduler, error) {
	return newTaskScheduler(ctx, limit, func(opts RunnerOptions) (*Runner, scheduledRunner) {
		runner := NewRunner(opts)
		return runner, runner
	})
}

func newTaskScheduler(ctx context.Context, limit int, factory func(RunnerOptions) (*Runner, scheduledRunner)) (*TaskScheduler, error) {
	if limit < 1 || limit > MaxTaskConcurrency {
		return nil, fmt.Errorf("task concurrency must be between 1 and %d", MaxTaskConcurrency)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if factory == nil {
		return nil, fmt.Errorf("task runner factory is required")
	}
	schedulerCtx, cancel := context.WithCancel(ctx)
	s := &TaskScheduler{
		ctx: schedulerCtx, cancel: cancel, limit: limit, factory: factory,
		jobs: map[string]*taskJob{}, workspace: map[string]*taskJob{},
	}
	s.cond = sync.NewCond(&s.mu)
	for range limit {
		s.workers.Add(1)
		go s.worker()
	}
	return s, nil
}

func (s *TaskScheduler) Submit(opts RunnerOptions) (string, error) {
	if s == nil {
		return "", ErrTaskSchedulerClosed
	}
	workspace, err := scheduledWorkspace(opts.Workspace)
	if err != nil {
		return "", err
	}
	opts.Workspace = workspace
	runner, executor := s.factory(opts)
	if executor == nil {
		return "", fmt.Errorf("task runner factory returned nil")
	}
	jobCtx, cancel := context.WithCancel(s.ctx)
	id := fmt.Sprintf("task-%d-%d", time.Now().UTC().UnixNano(), s.sequence.Add(1))
	job := &taskJob{
		snapshot: TaskSnapshot{ID: id, Workspace: workspace, TaskName: strings.TrimSpace(opts.TaskName), Status: TaskQueued, SubmittedAt: time.Now().UTC()},
		runner:   runner, executor: executor, ctx: jobCtx, cancel: cancel,
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.ctx.Err() != nil {
		cancel()
		return "", ErrTaskSchedulerClosed
	}
	if existing := s.workspace[workspace]; existing != nil {
		cancel()
		return "", fmt.Errorf("%w: %s", ErrTaskWorkspaceScheduled, workspace)
	}
	s.jobs[id] = job
	s.workspace[workspace] = job
	s.queue = append(s.queue, job)
	s.cond.Signal()
	return id, nil
}

func (s *TaskScheduler) Snapshot() TaskSchedulerSnapshot {
	if s == nil {
		return TaskSchedulerSnapshot{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot := TaskSchedulerSnapshot{Limit: s.limit, Tasks: make([]TaskSnapshot, 0, len(s.jobs))}
	for _, job := range s.jobs {
		task := job.snapshot
		snapshot.Tasks = append(snapshot.Tasks, task)
		switch task.Status {
		case TaskQueued:
			snapshot.Queued++
		case TaskRunning:
			snapshot.Running++
		case TaskSucceeded:
			snapshot.Succeeded++
		case TaskFailed:
			snapshot.Failed++
		case TaskCanceled:
			snapshot.Canceled++
		}
	}
	sort.Slice(snapshot.Tasks, func(i, j int) bool {
		if snapshot.Tasks[i].SubmittedAt.Equal(snapshot.Tasks[j].SubmittedAt) {
			return snapshot.Tasks[i].ID < snapshot.Tasks[j].ID
		}
		return snapshot.Tasks[i].SubmittedAt.Before(snapshot.Tasks[j].SubmittedAt)
	})
	return snapshot
}

func (s *TaskScheduler) RunnerForWorkspace(workspace string) (*Runner, TaskSnapshot, bool) {
	if s == nil {
		return nil, TaskSnapshot{}, false
	}
	workspace, err := scheduledWorkspace(workspace)
	if err != nil {
		return nil, TaskSnapshot{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.workspace[workspace]
	if job == nil || job.runner == nil || job.snapshot.Status != TaskRunning {
		return nil, TaskSnapshot{}, false
	}
	return job.runner, job.snapshot, true
}

func (s *TaskScheduler) OwnsRunner(workspace string, runner *Runner) bool {
	if s == nil || runner == nil {
		return false
	}
	workspace, err := scheduledWorkspace(workspace)
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.workspace[workspace]
	return job != nil && job.runner == runner
}

func (s *TaskScheduler) HasWorkspace(workspace string) bool {
	if s == nil {
		return false
	}
	workspace, err := scheduledWorkspace(workspace)
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.workspace[workspace] != nil
}

func (s *TaskScheduler) CancelWorkspace(workspace string) bool {
	if s == nil {
		return false
	}
	workspace, err := scheduledWorkspace(workspace)
	if err != nil {
		return false
	}
	s.mu.Lock()
	job := s.workspace[workspace]
	if job == nil || terminalTaskStatus(job.snapshot.Status) {
		s.mu.Unlock()
		return false
	}
	if job.snapshot.Status == TaskQueued {
		job.snapshot.Status = TaskCanceled
		job.snapshot.FinishedAt = time.Now().UTC()
	}
	job.cancel()
	s.mu.Unlock()
	s.cond.Broadcast()
	return true
}

func (s *TaskScheduler) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		s.cancel()
		now := time.Now().UTC()
		for _, job := range s.jobs {
			if job.snapshot.Status == TaskQueued {
				job.snapshot.Status = TaskCanceled
				job.snapshot.FinishedAt = now
			}
			if !terminalTaskStatus(job.snapshot.Status) {
				job.cancel()
			}
		}
		s.queue = nil
		s.cond.Broadcast()
	}
	s.mu.Unlock()
	s.workers.Wait()
}

func (s *TaskScheduler) worker() {
	defer s.workers.Done()
	for {
		job := s.nextJob()
		if job == nil {
			return
		}
		stopDrain := s.drainRunnerEvents(job.runner)
		summary, err := job.executor.Run(job.ctx)
		stopDrain()
		s.finish(job, summary, err)
	}
}

func (s *TaskScheduler) drainRunnerEvents(runner *Runner) func() {
	if runner == nil {
		return func() {}
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case _, ok := <-runner.Events():
				if !ok {
					return
				}
			case <-stop:
				return
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

func (s *TaskScheduler) nextJob() *taskJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		for len(s.queue) > 0 {
			job := s.queue[0]
			s.queue = s.queue[1:]
			if job.snapshot.Status != TaskQueued {
				continue
			}
			if job.ctx.Err() != nil {
				job.snapshot.Status = TaskCanceled
				job.snapshot.FinishedAt = time.Now().UTC()
				continue
			}
			job.snapshot.Status = TaskRunning
			job.snapshot.StartedAt = time.Now().UTC()
			return job
		}
		if s.closed || s.ctx.Err() != nil {
			return nil
		}
		s.cond.Wait()
	}
}

func (s *TaskScheduler) finish(job *taskJob, summary domain.RunSummary, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job.snapshot.FinishedAt = time.Now().UTC()
	switch {
	case job.ctx.Err() != nil:
		job.snapshot.Status = TaskCanceled
	case err != nil:
		job.snapshot.Status = TaskFailed
		job.snapshot.Error = err.Error()
	case summary.Status == "failed" || !summary.Passed:
		job.snapshot.Status = TaskFailed
		job.snapshot.Error = "workflow completed with failed checks"
	default:
		job.snapshot.Status = TaskSucceeded
	}
}

func scheduledWorkspace(workspace string) (string, error) {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		return "", fmt.Errorf("scheduled task workspace is required")
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return "", fmt.Errorf("resolve scheduled task workspace: %w", err)
	}
	return filepath.Clean(abs), nil
}

func terminalTaskStatus(status TaskStatus) bool {
	return status == TaskSucceeded || status == TaskFailed || status == TaskCanceled
}
