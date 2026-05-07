package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/db"
	"github.com/xuanli520/p2r_tui/internal/pipeline"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
)

type JobState int

const (
	JobQueued JobState = iota
	JobRunning
	JobDone
	JobFailed
)

func (s JobState) String() string {
	switch s {
	case JobQueued:
		return "queued"
	case JobRunning:
		return "running"
	case JobDone:
		return "done"
	case JobFailed:
		return "failed"
	default:
		return "unknown"
	}
}

type Job struct {
	JobID        string
	RunID        string
	TaskID       string
	State        JobState
	CurrentStage string
	Stages       []model.StageRecord
	Result       *pipeline.Result
	Err          error
	SubmittedAt  time.Time
	StartedAt    time.Time
	FinishedAt   time.Time

	opts   pipeline.RunOptions
	cancel context.CancelFunc
	mu     sync.RWMutex
}

type JobSnapshot struct {
	JobID        string
	RunID        string
	TaskID       string
	State        JobState
	CurrentStage string
	Stages       []model.StageRecord
	Result       *pipeline.Result
	Err          string
	SubmittedAt  time.Time
	StartedAt    time.Time
	FinishedAt   time.Time
}

type Scheduler struct {
	store        *db.Store
	cfg          config.Config
	maxParallel  int
	sem          chan struct{}
	mu           sync.Mutex
	jobs         []*Job
	queue        []*Job
	jobByID      map[string]*Job
	activeByTask map[string]*Job
	notifyCh     chan struct{}
	closed       bool
	nextID       int
	wg           sync.WaitGroup
}

func New(store *db.Store, cfg config.Config) *Scheduler {
	maxParallel := cfg.Pipeline.MaxConcurrent
	if maxParallel <= 0 {
		maxParallel = 3
	}
	if maxParallel > 8 {
		maxParallel = 8
	}
	return &Scheduler{
		store:        store,
		cfg:          cfg,
		maxParallel:  maxParallel,
		sem:          make(chan struct{}, maxParallel),
		jobByID:      map[string]*Job{},
		activeByTask: map[string]*Job{},
		notifyCh:     make(chan struct{}, 1),
	}
}

func (s *Scheduler) Submit(taskID string, opts pipeline.RunOptions) (string, error) {
	if s == nil {
		return "", errors.New("scheduler is nil")
	}
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return "", errors.New("task id is required")
	}

	var startNow bool
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", errors.New("scheduler is shut down")
	}
	if existing := s.activeByTask[taskID]; existing != nil {
		s.mu.Unlock()
		return "", fmt.Errorf("task %s already has an active job %s", taskID, existing.JobID)
	}
	s.nextID++
	jobID := fmt.Sprintf("job-%s-%06d", time.Now().UTC().Format("20060102-150405"), s.nextID)
	job := &Job{
		JobID:       jobID,
		TaskID:      taskID,
		State:       JobQueued,
		SubmittedAt: time.Now().UTC(),
		opts:        opts,
	}
	s.jobs = append(s.jobs, job)
	s.jobByID[jobID] = job
	s.activeByTask[taskID] = job
	if len(s.queue) == 0 {
		select {
		case s.sem <- struct{}{}:
			startNow = true
		default:
		}
	}
	if !startNow {
		s.queue = append(s.queue, job)
	}
	s.mu.Unlock()

	if startNow {
		s.startJob(job)
	}
	s.notify()
	return jobID, nil
}

func (s *Scheduler) Snapshot() []JobSnapshot {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	jobs := append([]*Job(nil), s.jobs...)
	s.mu.Unlock()

	snapshots := make([]JobSnapshot, 0, len(jobs))
	for _, job := range jobs {
		job.mu.RLock()
		snapshot := JobSnapshot{
			JobID:        job.JobID,
			RunID:        job.RunID,
			TaskID:       job.TaskID,
			State:        job.State,
			CurrentStage: job.CurrentStage,
			Stages:       append([]model.StageRecord(nil), job.Stages...),
			Result:       cloneResult(job.Result),
			SubmittedAt:  job.SubmittedAt,
			StartedAt:    job.StartedAt,
			FinishedAt:   job.FinishedAt,
		}
		if job.Err != nil {
			snapshot.Err = job.Err.Error()
		}
		job.mu.RUnlock()
		snapshots = append(snapshots, snapshot)
	}
	return snapshots
}

func (s *Scheduler) NotifyCh() <-chan struct{} {
	if s == nil {
		return nil
	}
	return s.notifyCh
}

func (s *Scheduler) Shutdown(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var cancels []context.CancelFunc
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		for _, job := range s.jobs {
			job.mu.Lock()
			switch job.State {
			case JobQueued:
				job.State = JobFailed
				job.Err = context.Canceled
				job.FinishedAt = time.Now().UTC()
				delete(s.activeByTask, job.TaskID)
			case JobRunning:
				if job.cancel != nil {
					cancels = append(cancels, job.cancel)
				}
			}
			job.mu.Unlock()
		}
		s.queue = nil
	}
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	s.notify()

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Scheduler) startJob(job *Job) {
	ctx, cancel := context.WithCancel(context.Background())
	now := time.Now().UTC()

	s.mu.Lock()
	job.mu.Lock()
	if s.closed || job.State != JobQueued {
		if job.State == JobQueued {
			job.State = JobFailed
			job.Err = context.Canceled
			job.FinishedAt = now
			delete(s.activeByTask, job.TaskID)
		}
		job.mu.Unlock()
		s.releaseSlotLocked()
		s.mu.Unlock()
		cancel()
		s.notify()
		return
	}
	job.State = JobRunning
	job.cancel = cancel
	job.StartedAt = now
	job.mu.Unlock()
	s.wg.Add(1)
	s.mu.Unlock()
	s.notify()

	go s.runJob(ctx, job)
}

func (s *Scheduler) runJob(ctx context.Context, job *Job) {
	defer s.wg.Done()
	defer s.finishJob(job)
	defer func() {
		if recovered := recover(); recovered != nil {
			job.mu.Lock()
			job.State = JobFailed
			job.Err = fmt.Errorf("panic: %v", recovered)
			job.mu.Unlock()
			s.notify()
		}
	}()

	opts := job.opts
	opts.Progress = func(update pipeline.RunProgress) {
		s.applyProgress(job, update)
	}
	result, err := pipeline.NewRunner(s.store, s.cfg).Run(ctx, job.TaskID, opts)
	job.mu.Lock()
	if err != nil {
		job.State = JobFailed
		job.Err = err
		job.FinishedAt = time.Now().UTC()
	} else {
		resultCopy := result
		resultCopy.Stages = append([]model.StageRecord(nil), result.Stages...)
		job.Result = &resultCopy
		job.State = JobDone
		job.Err = nil
		job.RunID = result.Run.RunID
		job.Stages = append([]model.StageRecord(nil), result.Stages...)
		job.CurrentStage = ""
		job.FinishedAt = time.Now().UTC()
	}
	job.mu.Unlock()
	s.notify()
}

func (s *Scheduler) applyProgress(job *Job, update pipeline.RunProgress) {
	job.mu.Lock()
	if update.RunID != "" {
		job.RunID = update.RunID
	}
	if update.StageRecord.Stage != "" {
		job.Stages = upsertStage(job.Stages, update.StageRecord)
	}
	switch update.Event {
	case "stage_running":
		job.State = JobRunning
		job.CurrentStage = update.Stage
	case "stage_done":
		if job.CurrentStage == update.Stage {
			job.CurrentStage = ""
		}
	case "run_done":
		job.State = JobDone
		job.Err = nil
		job.CurrentStage = ""
		job.FinishedAt = time.Now().UTC()
	case "run_crashed":
		job.State = JobFailed
		job.Err = firstErr(update.Err, errors.New("pipeline crashed"))
		job.CurrentStage = ""
		job.FinishedAt = time.Now().UTC()
	}
	if update.Done {
		if update.Err != nil {
			job.State = JobFailed
			job.Err = update.Err
		} else if job.State != JobFailed {
			job.State = JobDone
		}
		if job.FinishedAt.IsZero() {
			job.FinishedAt = time.Now().UTC()
		}
	}
	job.mu.Unlock()
	s.notify()
}

func (s *Scheduler) finishJob(job *Job) {
	var next *Job
	s.mu.Lock()
	delete(s.activeByTask, job.TaskID)
	if !s.closed && len(s.queue) > 0 {
		next = s.queue[0]
		s.queue = s.queue[1:]
	} else {
		s.releaseSlotLocked()
	}
	s.mu.Unlock()
	if next != nil {
		s.startJob(next)
	}
	s.notify()
}

func (s *Scheduler) releaseSlotLocked() {
	select {
	case <-s.sem:
	default:
	}
}

func (s *Scheduler) notify() {
	select {
	case s.notifyCh <- struct{}{}:
	default:
	}
}

func upsertStage(stages []model.StageRecord, next model.StageRecord) []model.StageRecord {
	for i := range stages {
		if stages[i].Stage == next.Stage {
			stages[i] = next
			return stages
		}
	}
	return append(stages, next)
}

func cloneResult(result *pipeline.Result) *pipeline.Result {
	if result == nil {
		return nil
	}
	cloned := *result
	cloned.Stages = append([]model.StageRecord(nil), result.Stages...)
	return &cloned
}

func firstErr(err error, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}
