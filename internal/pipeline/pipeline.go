package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/executor"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/scanner"
)

type RunOptions struct {
	Stage               string
	From                string
	StaticOnly          bool
	Stages              []string
	Mode                string
	RefRun              string
	ExtraDocs           []string
	KeepRuntime         bool
	DeferRuntimeCleanup bool
	Progress            ProgressReporter
}

type RunProgress struct {
	RunID       string
	TaskID      string
	Stage       string
	Event       ProgressEvent
	StageRecord model.StageRecord
	Stream      *StreamUpdate
	Done        bool
	Err         error
}

type ProgressEvent string

type StreamMode int

const (
	StreamModeCumulative StreamMode = iota
	StreamModeAppend
)

type StreamLine struct {
	Source string
	Text   string
}

type StreamUpdate struct {
	Stage     string
	Mode      StreamMode
	ItemID    string
	Text      string
	Delta     string
	Source    string
	Lines     []StreamLine
	Done      bool
	Truncated bool
}

const (
	EventRunCreated   ProgressEvent = "run_created"
	EventPathWarning  ProgressEvent = "path_warning"
	EventStagePending ProgressEvent = "stage_pending"
	EventStageRunning ProgressEvent = "stage_running"
	EventStageStream  ProgressEvent = "stage_stream"
	EventStageDone    ProgressEvent = "stage_done"
	EventCleanup      ProgressEvent = "cleanup"
	EventRunDone      ProgressEvent = "run_done"
	EventRunAborted   ProgressEvent = "run_aborted"
	EventRunCrashed   ProgressEvent = "run_crashed"
)

type ProjectPathWarning struct {
	Type          string `json:"type"`
	DBPath        string `json:"db_path"`
	CanonicalPath string `json:"canonical_path"`
}

type ProgressReporter func(RunProgress)

type Result struct {
	Run    model.RunRecord
	Stages []model.StageRecord
}

type runStore interface {
	GetProject(context.Context, string) (scanner.Project, error)
	GetRun(context.Context, string) (model.RunRecord, error)
	ListRunsForTask(context.Context, string) ([]model.RunRecord, error)
	CreateRun(context.Context, model.RunRecord) error
	PutStage(context.Context, string, model.StageRecord) error
	InsertFindings(context.Context, string, []model.Finding) error
	FinishRun(context.Context, string, string, string, time.Duration) error
}

type CommandRunner interface {
	LookPath(name string) (string, error)
	Run(ctx context.Context, timeout time.Duration, dir string, env []string, name string, args ...string) executor.Result
	RunStreamingWithOutput(ctx context.Context, timeout time.Duration, dir string, env []string, writer io.Writer, onOutput executor.OutputCallback, name string, args ...string) executor.Result
}

type RunnerOption func(*Runner)

type Runner struct {
	store runStore
	cfg   config.Config
	exec  CommandRunner
}

func WithCommandRunner(exec CommandRunner) RunnerOption {
	return func(r *Runner) {
		if exec != nil {
			r.exec = exec
		}
	}
}

func NewRunner(store runStore, cfg config.Config, opts ...RunnerOption) Runner {
	runner := Runner{store: store, cfg: cfg, exec: executor.New()}
	for _, opt := range opts {
		if opt != nil {
			opt(&runner)
		}
	}
	return runner
}

func (r Runner) Run(ctx context.Context, taskID string, opts RunOptions) (result Result, err error) {
	progress := makeRunProgress(taskID, opts.Progress)
	project, pathWarnings, opts, err := r.loadAndValidateRunInputs(ctx, taskID, opts, progress)
	if err != nil {
		return Result{}, err
	}
	lock, err := r.acquireTaskRunLock(taskID)
	if err != nil {
		progress(RunProgress{Event: EventRunCrashed, Done: true, Err: err})
		return Result{}, err
	}
	defer lock.Release()

	var state *runState
	defer func() {
		if recovered := recover(); recovered != nil {
			if state != nil && state.canPersistCrash() {
				_ = state.persistCrash(r, fmt.Sprintf("panic: %v", recovered))
			}
			progress(RunProgress{RunID: runIDFromState(state), Event: EventRunCrashed, Done: true, Err: fmt.Errorf("panic: %v", recovered)})
			panic(recovered)
		}
		if err != nil && state != nil && state.canPersistCrash() {
			if persistErr := state.persistCrash(r, err.Error()); persistErr != nil {
				err = errors.Join(err, persistErr)
			}
			progress(RunProgress{RunID: runIDFromState(state), Event: EventRunCrashed, Done: true, Err: err})
		}
	}()

	state, err = r.prepareRun(runPrepareInput{
		ctx:          ctx,
		taskID:       taskID,
		opts:         opts,
		progress:     progress,
		project:      project,
		pathWarnings: pathWarnings,
	})
	if err != nil {
		return Result{}, err
	}
	if result, err, aborted := state.abortIfCancelled(r); aborted {
		return result, err
	}
	if err := state.persistInitialArtifacts(r); err != nil {
		return Result{}, err
	}
	if result, err, aborted := state.abortIfCancelled(r); aborted {
		return result, err
	}
	if result, err, aborted := state.persistInitialStages(r); aborted || err != nil {
		return result, err
	}
	preflightResult, err := state.runPreflightAndCleanup(r)
	if err != nil {
		if result, err, aborted := state.abortOrError(r, err); aborted {
			return result, err
		}
		return Result{}, err
	}
	if result, err, aborted := state.abortIfCancelled(r); aborted {
		return result, err
	}
	if result, err, aborted := state.executeStageLoop(r, preflightResult); aborted || err != nil {
		return result, err
	}
	if result, err, aborted := state.finalizeRuntimeCleanup(r); aborted || err != nil {
		return result, err
	}
	state.aggregateSubmitArtifacts(r)
	result, err, _ = state.finishRun(r)
	return result, err
}

func runIDFromState(state *runState) string {
	if state == nil {
		return ""
	}
	return state.identity.runID
}
