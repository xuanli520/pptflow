package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/db"
	gitsync "github.com/xuanli520/p2r_tui/internal/git"
	"github.com/xuanli520/p2r_tui/internal/pipeline"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
)

type JobState int

var ErrJobCancelledByUser = errors.New("cancelled by user")

const (
	JobQueued JobState = iota
	JobRunning
	JobDone
	JobCancelled
	JobFailed
)

type JobKind string

const (
	JobPipeline   JobKind = "pipeline"
	JobGitSync    JobKind = "git_sync"
	JobInspection JobKind = "inspection"
)

func (s JobState) String() string {
	switch s {
	case JobQueued:
		return "queued"
	case JobRunning:
		return "running"
	case JobDone:
		return "done"
	case JobCancelled:
		return "cancelled"
	case JobFailed:
		return "failed"
	default:
		return "unknown"
	}
}

type Job struct {
	JobID         string
	RunID         string
	TaskID        string
	Kind          JobKind
	State         JobState
	CurrentStage  string
	Stages        []model.StageRecord
	Result        *pipeline.Result
	Err           error
	SubmittedAt   time.Time
	StartedAt     time.Time
	FinishedAt    time.Time
	StreamByStage map[string]pipeline.StreamUpdate
	SyncProgress  gitsync.SyncProgress

	opts               pipeline.RunOptions
	batchID            string
	gitURL             string
	cancel             context.CancelFunc
	cancelRequested    bool
	streamLinesByStage map[string][]pipeline.StreamLine
	mu                 sync.RWMutex
}

type JobSnapshot struct {
	JobID         string
	RunID         string
	TaskID        string
	Kind          JobKind
	State         JobState
	CurrentStage  string
	Stages        []model.StageRecord
	Result        *pipeline.Result
	Err           string
	SubmittedAt   time.Time
	StartedAt     time.Time
	FinishedAt    time.Time
	StreamByStage map[string]pipeline.StreamUpdate
	SyncProgress  gitsync.SyncProgress

	CancelRequested bool
}

type PipelineRunner interface {
	Run(context.Context, string, pipeline.RunOptions) (pipeline.Result, error)
}

type GitSyncRunner interface {
	Sync(context.Context, string, string, string, gitsync.SyncCallback) (*gitsync.SyncResult, error)
}

type RunnerFactory func(*db.Store, config.Config) PipelineRunner

type Option func(*Scheduler)

type Scheduler struct {
	store              *db.Store
	cfg                config.Config
	runnerFactory      RunnerFactory
	gitSyncer          GitSyncRunner
	maxParallel        int
	sem                chan struct{}
	mu                 sync.Mutex
	jobs               []*Job
	queue              []*Job
	jobByID            map[string]*Job
	activeByTask       map[string]*Job
	recentTerminal     []*Job
	recentTerminalByID map[string]bool
	notifyCh           chan struct{}
	closed             bool
	nextID             int
	wg                 sync.WaitGroup
}

func WithRunnerFactory(factory RunnerFactory) Option {
	return func(s *Scheduler) {
		if factory != nil {
			s.runnerFactory = factory
		}
	}
}

func WithGitSyncRunner(syncer GitSyncRunner) Option {
	return func(s *Scheduler) {
		if syncer != nil {
			s.gitSyncer = syncer
		}
	}
}

func New(store *db.Store, cfg config.Config, opts ...Option) *Scheduler {
	maxParallel := config.NormalizeMaxConcurrent(cfg.Pipeline.MaxConcurrent)
	s := &Scheduler{
		store:              store,
		cfg:                cfg,
		runnerFactory:      defaultRunnerFactory,
		gitSyncer:          gitsync.NewSyncer(cfg.ScanPath, cfg.Git),
		maxParallel:        maxParallel,
		sem:                make(chan struct{}, maxParallel),
		jobByID:            map[string]*Job{},
		activeByTask:       map[string]*Job{},
		recentTerminalByID: map[string]bool{},
		notifyCh:           make(chan struct{}, 1),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	return s
}

func defaultRunnerFactory(store *db.Store, cfg config.Config) PipelineRunner {
	return pipeline.NewRunner(store, cfg)
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
		Kind:        JobPipeline,
		State:       JobQueued,
		SubmittedAt: time.Now().UTC(),
		opts:        opts,
	}
	startNow = s.enqueueJobLocked(job)
	s.mu.Unlock()

	if startNow {
		s.startJob(job)
	}
	s.notify()
	return jobID, nil
}

func (s *Scheduler) SubmitInspection(taskID, batchID, gitURL string, opts pipeline.RunOptions) (string, error) {
	if s == nil {
		return "", errors.New("scheduler is nil")
	}
	taskID = strings.TrimSpace(taskID)
	batchID = strings.TrimSpace(batchID)
	gitURL = strings.TrimSpace(gitURL)
	if taskID == "" {
		return "", errors.New("task id is required")
	}
	if batchID == "" {
		return "", errors.New("batch id is required")
	}
	if gitURL == "" {
		return "", errors.New("git url is required")
	}

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
		Kind:        JobGitSync,
		State:       JobQueued,
		SubmittedAt: time.Now().UTC(),
		opts:        opts,
		batchID:     batchID,
		gitURL:      gitURL,
	}
	startNow := s.enqueueJobLocked(job)
	s.mu.Unlock()

	if startNow {
		s.startJob(job)
	}
	s.notify()
	return jobID, nil
}

func (s *Scheduler) enqueueJobLocked(job *Job) bool {
	s.jobs = append(s.jobs, job)
	s.jobByID[job.JobID] = job
	s.activeByTask[job.TaskID] = job
	startNow := false
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
	return startNow
}

func (s *Scheduler) CancelTask(taskID string) error {
	if s == nil {
		return errors.New("scheduler is nil")
	}
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return errors.New("task id is required")
	}

	var cancel context.CancelFunc
	notify := false
	now := time.Now().UTC()

	s.mu.Lock()
	job := s.activeByTask[taskID]
	if job == nil {
		s.mu.Unlock()
		return fmt.Errorf("task %s has no active job", taskID)
	}

	job.mu.Lock()
	switch job.State {
	case JobQueued:
		job.cancelRequested = true
		finishCancelledJobLocked(job, now)
		s.addRecentTerminalLocked(job)
		s.deleteActiveJobLocked(job)
		s.removeFromQueueLocked(job.JobID)
		notify = true
	case JobRunning:
		if !job.cancelRequested {
			job.cancelRequested = true
			cancel = job.cancel
		}
		notify = true
	default:
		err := fmt.Errorf("job %s is in state %s, cannot cancel", job.JobID, job.State)
		job.mu.Unlock()
		s.mu.Unlock()
		return err
	}
	job.mu.Unlock()
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if notify {
		s.notify()
	}
	return nil
}

func (s *Scheduler) Snapshot() []JobSnapshot {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	jobs := append([]*Job(nil), s.jobs...)
	s.mu.Unlock()
	sort.SliceStable(jobs, func(i, j int) bool {
		if !jobs[i].SubmittedAt.Equal(jobs[j].SubmittedAt) {
			return jobs[i].SubmittedAt.Before(jobs[j].SubmittedAt)
		}
		return jobs[i].JobID < jobs[j].JobID
	})
	return snapshotJobs(jobs)
}

func (s *Scheduler) ActiveSnapshot() []JobSnapshot {
	if s == nil {
		return nil
	}
	now := time.Now()
	s.mu.Lock()
	jobs := make([]*Job, 0, len(s.activeByTask)+len(s.recentTerminal))
	seen := map[string]bool{}
	for _, job := range s.activeByTask {
		if job == nil || seen[job.JobID] {
			continue
		}
		seen[job.JobID] = true
		jobs = append(jobs, job)
	}
	pruned := s.recentTerminal[:0]
	for _, job := range s.recentTerminal {
		job.mu.RLock()
		terminal := job.State != JobQueued && job.State != JobRunning
		recent := !job.FinishedAt.IsZero() && now.Sub(job.FinishedAt) <= terminalSnapshotGrace
		job.mu.RUnlock()
		if terminal && recent {
			pruned = append(pruned, job)
		} else {
			delete(s.recentTerminalByID, job.JobID)
		}
		if terminal && recent && !seen[job.JobID] {
			seen[job.JobID] = true
			jobs = append(jobs, job)
		}
	}
	s.recentTerminal = pruned
	s.mu.Unlock()
	sort.SliceStable(jobs, func(i, j int) bool {
		if !jobs[i].SubmittedAt.Equal(jobs[j].SubmittedAt) {
			return jobs[i].SubmittedAt.Before(jobs[j].SubmittedAt)
		}
		return jobs[i].JobID < jobs[j].JobID
	})
	return snapshotJobs(jobs)
}

func snapshotJobs(jobs []*Job) []JobSnapshot {
	snapshots := make([]JobSnapshot, 0, len(jobs))
	for _, job := range jobs {
		job.mu.RLock()
		snapshot := JobSnapshot{
			JobID:           job.JobID,
			RunID:           job.RunID,
			TaskID:          job.TaskID,
			Kind:            job.Kind,
			State:           job.State,
			CurrentStage:    job.CurrentStage,
			Stages:          append([]model.StageRecord(nil), job.Stages...),
			Result:          cloneResult(job.Result),
			SubmittedAt:     job.SubmittedAt,
			StartedAt:       job.StartedAt,
			FinishedAt:      job.FinishedAt,
			StreamByStage:   cloneStreamByStage(job.StreamByStage),
			SyncProgress:    job.SyncProgress,
			CancelRequested: job.cancelRequested,
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
	var next *Job

	s.mu.Lock()
	job.mu.Lock()
	if s.closed || job.State != JobQueued || job.cancelRequested {
		if job.State == JobQueued {
			if job.cancelRequested {
				finishCancelledJobLocked(job, now)
			} else {
				job.State = JobFailed
				job.Err = context.Canceled
				job.FinishedAt = now
			}
			s.addRecentTerminalLocked(job)
			s.deleteActiveJobLocked(job)
		}
		if !s.closed {
			next = s.popQueuedJobLocked()
		}
		if next == nil {
			s.releaseSlotLocked()
		}
		job.mu.Unlock()
		s.mu.Unlock()
		cancel()
		s.notify()
		if next != nil {
			s.startJob(next)
		}
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

	if job.Kind == JobGitSync || job.Kind == JobInspection {
		if err := s.runGitSync(ctx, job); err != nil {
			job.mu.Lock()
			if job.cancelRequested || errors.Is(err, context.Canceled) {
				finishCancelledJobLocked(job, time.Now().UTC())
			} else {
				job.State = JobFailed
				job.Err = err
				job.CurrentStage = ""
				job.FinishedAt = time.Now().UTC()
			}
			job.mu.Unlock()
			s.notify()
			return
		}
		if job.Kind == JobGitSync {
			job.mu.Lock()
			if job.cancelRequested {
				finishCancelledJobLocked(job, time.Now().UTC())
				job.mu.Unlock()
				s.notify()
				return
			}
			job.State = JobDone
			job.Err = nil
			job.CurrentStage = ""
			job.FinishedAt = time.Now().UTC()
			job.mu.Unlock()
			if err := s.enqueuePipelineAfterGit(job); err != nil {
				job.mu.Lock()
				job.State = JobFailed
				job.Err = err
				job.FinishedAt = time.Now().UTC()
				job.mu.Unlock()
			}
			s.notify()
			return
		}
	}

	opts := job.opts
	opts.Progress = func(update pipeline.RunProgress) {
		s.applyProgress(job, update)
	}
	result, err := s.runnerFactory(s.store, s.cfg).Run(ctx, job.TaskID, opts)
	job.mu.Lock()
	if job.cancelRequested {
		if result.Run.RunID != "" {
			applyResultLocked(job, result)
		}
		finishCancelledJobLocked(job, time.Now().UTC())
	} else if err != nil {
		if result.Run.RunID != "" {
			applyResultLocked(job, result)
		}
		job.State = JobFailed
		job.Err = err
		job.FinishedAt = time.Now().UTC()
	} else {
		applyResultLocked(job, result)
		job.State = JobDone
		job.Err = nil
		job.CurrentStage = ""
		job.FinishedAt = time.Now().UTC()
	}
	job.mu.Unlock()
	s.notify()
}

func (s *Scheduler) enqueuePipelineAfterGit(gitJob *Job) error {
	if s == nil || gitJob == nil {
		return errors.New("scheduler is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("scheduler is shut down")
	}
	s.nextID++
	now := time.Now().UTC()
	jobID := fmt.Sprintf("job-%s-%06d", now.Format("20060102-150405"), s.nextID)
	job := &Job{
		JobID:       jobID,
		TaskID:      gitJob.TaskID,
		Kind:        JobPipeline,
		State:       JobQueued,
		SubmittedAt: now,
		opts:        gitJob.opts,
	}
	s.jobs = append(s.jobs, job)
	s.jobByID[job.JobID] = job
	s.activeByTask[job.TaskID] = job
	s.queue = append(s.queue, job)
	return nil
}

func (s *Scheduler) runGitSync(ctx context.Context, job *Job) error {
	if s.gitSyncer == nil {
		return errors.New("git syncer unavailable")
	}
	if s.store != nil {
		_ = s.store.RecordTaskGitError(ctx, job.TaskID, nil)
	}
	job.mu.Lock()
	job.CurrentStage = "Git"
	job.SyncProgress = gitsync.SyncProgress{Phase: "queued", Percent: 0, Message: "waiting for git sync"}
	job.mu.Unlock()
	s.notify()
	_, err := s.gitSyncer.Sync(ctx, job.TaskID, job.batchID, job.gitURL, func(progress gitsync.SyncProgress) {
		job.mu.Lock()
		job.SyncProgress = progress
		job.CurrentStage = "Git"
		job.mu.Unlock()
		s.notify()
	})
	if s.store != nil {
		_ = s.store.RecordTaskGitError(ctx, job.TaskID, err)
	}
	job.mu.Lock()
	if err == nil {
		job.SyncProgress = gitsync.SyncProgress{Phase: "done", Percent: 100, Message: "git sync completed"}
		job.CurrentStage = ""
	} else {
		job.SyncProgress = gitsync.SyncProgress{Phase: "failed", Percent: job.SyncProgress.Percent, Message: err.Error()}
	}
	job.mu.Unlock()
	s.notify()
	return err
}

func (s *Scheduler) applyProgress(job *Job, update pipeline.RunProgress) {
	job.mu.Lock()
	if update.RunID != "" {
		job.RunID = update.RunID
	}
	if update.Event == pipeline.EventStageStream && update.Stream != nil {
		applyStreamUpdateLocked(job, update)
		job.mu.Unlock()
		return
	}
	if update.StageRecord.Stage != "" {
		job.Stages = upsertStage(job.Stages, update.StageRecord)
	}
	if job.cancelRequested {
		switch update.Event {
		case pipeline.EventStageRunning:
			job.CurrentStage = update.Stage
		case pipeline.EventStageDone, pipeline.EventRunDone, pipeline.EventRunAborted, pipeline.EventRunCrashed:
			if job.CurrentStage == update.Stage || update.Event != pipeline.EventStageDone {
				job.CurrentStage = ""
			}
		}
		if update.Done && job.FinishedAt.IsZero() {
			job.FinishedAt = time.Now().UTC()
		}
		job.mu.Unlock()
		s.notify()
		return
	}
	switch update.Event {
	case pipeline.EventStageRunning:
		job.State = JobRunning
		job.CurrentStage = update.Stage
	case pipeline.EventStageDone:
		if job.CurrentStage == update.Stage {
			job.CurrentStage = ""
		}
	case pipeline.EventRunDone:
		job.State = JobDone
		job.Err = nil
		job.CurrentStage = ""
		job.FinishedAt = time.Now().UTC()
	case pipeline.EventRunAborted:
		job.State = JobCancelled
		job.Err = ErrJobCancelledByUser
		job.CurrentStage = ""
		job.FinishedAt = time.Now().UTC()
	case pipeline.EventRunCrashed:
		job.State = JobFailed
		job.Err = firstErr(update.Err, errors.New("pipeline crashed"))
		job.CurrentStage = ""
		job.FinishedAt = time.Now().UTC()
	}
	if update.Done {
		if update.Event == pipeline.EventRunAborted {
			job.State = JobCancelled
			job.Err = ErrJobCancelledByUser
		} else if update.Err != nil {
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
	s.addRecentTerminalLocked(job)
	s.deleteActiveJobLocked(job)
	if !s.closed {
		next = s.popQueuedJobLocked()
	}
	if next == nil {
		s.releaseSlotLocked()
	}
	s.mu.Unlock()
	if next != nil {
		s.startJob(next)
	}
	s.notify()
}

func (s *Scheduler) addRecentTerminalLocked(job *Job) {
	if job == nil || job.JobID == "" || s.recentTerminalByID[job.JobID] {
		return
	}
	if s.recentTerminalByID == nil {
		s.recentTerminalByID = map[string]bool{}
	}
	s.recentTerminal = append(s.recentTerminal, job)
	s.recentTerminalByID[job.JobID] = true
	for len(s.recentTerminal) > maxRecentTerminalJobs {
		drop := s.recentTerminal[0]
		s.recentTerminal = s.recentTerminal[1:]
		if drop != nil {
			delete(s.recentTerminalByID, drop.JobID)
		}
	}
}

func (s *Scheduler) removeFromQueueLocked(jobID string) {
	if jobID == "" || len(s.queue) == 0 {
		return
	}
	filtered := s.queue[:0]
	for _, job := range s.queue {
		if job.JobID == jobID {
			continue
		}
		filtered = append(filtered, job)
	}
	s.queue = filtered
}

func (s *Scheduler) deleteActiveJobLocked(job *Job) {
	if job == nil {
		return
	}
	if s.activeByTask[job.TaskID] == job {
		delete(s.activeByTask, job.TaskID)
	}
}

func (s *Scheduler) popQueuedJobLocked() *Job {
	for len(s.queue) > 0 {
		job := s.queue[0]
		s.queue = s.queue[1:]
		job.mu.Lock()
		active := s.activeByTask[job.TaskID] == job
		startable := active && job.State == JobQueued && !job.cancelRequested
		if startable {
			job.mu.Unlock()
			return job
		}
		if active && (job.State != JobQueued || job.cancelRequested) {
			s.deleteActiveJobLocked(job)
		}
		job.mu.Unlock()
	}
	return nil
}

func applyResultLocked(job *Job, result pipeline.Result) {
	resultCopy := result
	resultCopy.Stages = append([]model.StageRecord(nil), result.Stages...)
	job.Result = &resultCopy
	job.RunID = result.Run.RunID
	job.Stages = append([]model.StageRecord(nil), result.Stages...)
	job.CurrentStage = ""
}

const (
	maxStreamLines        = 200
	maxRecentTerminalJobs = 32
	terminalSnapshotGrace = 5 * time.Second
)

func applyStreamUpdateLocked(job *Job, update pipeline.RunProgress) {
	if job.StreamByStage == nil {
		job.StreamByStage = map[string]pipeline.StreamUpdate{}
	}
	stage := strings.TrimSpace(update.Stream.Stage)
	if stage == "" {
		stage = strings.TrimSpace(update.Stage)
	}
	if stage == "" {
		return
	}

	stream := *update.Stream
	stream.Stage = stage
	if stream.Mode == pipeline.StreamModeAppend {
		if job.streamLinesByStage == nil {
			job.streamLinesByStage = map[string][]pipeline.StreamLine{}
		}
		previous := job.StreamByStage[stage]
		stream.Truncated = stream.Truncated || previous.Truncated
		lineText := strings.TrimRight(stream.Delta, "\r\n")
		if stream.Source != "" || stream.Delta != "" {
			source := strings.TrimSpace(stream.Source)
			if source == "" {
				source = "stdout"
			}
			job.streamLinesByStage[stage] = append(job.streamLinesByStage[stage], pipeline.StreamLine{
				Source: source,
				Text:   lineText,
			})
		}
		if len(job.streamLinesByStage[stage]) > maxStreamLines {
			drop := len(job.streamLinesByStage[stage]) - maxStreamLines
			job.streamLinesByStage[stage] = append([]pipeline.StreamLine(nil), job.streamLinesByStage[stage][drop:]...)
			stream.Truncated = true
		}
		stream.Lines = append([]pipeline.StreamLine(nil), job.streamLinesByStage[stage]...)
		stream.Text = streamLinesText(stream.Lines)
	}
	job.StreamByStage[stage] = stream
}

func streamLinesText(lines []pipeline.StreamLine) string {
	if len(lines) == 0 {
		return ""
	}
	values := make([]string, 0, len(lines))
	for _, line := range lines {
		values = append(values, line.Text)
	}
	return strings.Join(values, "\n")
}

func finishCancelledJobLocked(job *Job, now time.Time) {
	job.State = JobCancelled
	job.Err = ErrJobCancelledByUser
	job.FinishedAt = now
	job.CurrentStage = ""
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

func cloneStreamByStage(input map[string]pipeline.StreamUpdate) map[string]pipeline.StreamUpdate {
	if len(input) == 0 {
		return nil
	}
	output := make(map[string]pipeline.StreamUpdate, len(input))
	for stage, update := range input {
		update.Lines = append([]pipeline.StreamLine(nil), update.Lines...)
		output[stage] = update
	}
	return output
}

func firstErr(err error, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}
