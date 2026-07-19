package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

const (
	// RunWorkerLeaseResourceType is the durable, run-scoped fence held by one
	// controlled local child worker. It is distinct from short-lived dispatch
	// leases: it prevents two detached supervisors from competing for the same
	// Run while individual jobs retain their own dispatch fences.
	RunWorkerLeaseResourceType = "workflow_run_worker"
)

var (
	// ErrRunWorkerConfiguration marks a child-worker supervisor that cannot be
	// safely bound to one durable Run.
	ErrRunWorkerConfiguration = errors.New("v2 executor: invalid run worker configuration")
	// ErrRunWorkerLeaseLost means another process may now own the supervisor
	// fence. The current worker stops claiming further jobs and leaves any
	// in-flight dispatch lease to its normal recovery protocol.
	ErrRunWorkerLeaseLost = errors.New("v2 executor: run worker supervision lease lost")
	// ErrRunWorkerInconsistentState means a Run claims to be active without any
	// queued job, valid dispatch lease, or durable lease-loss recovery fact.
	ErrRunWorkerInconsistentState = errors.New("v2 executor: running workflow has no eligible durable work")
)

// RunWorkerSessionConfig binds one controlled local worker to one durable Run.
// Handler is deliberately injected: Harbor-owned stage implementations are
// composed outside app, preserving the application package's domain boundary.
type RunWorkerSessionConfig struct {
	Services *LifecycleServices
	RunID    string
	Owner    string
	Actor    string
	Reason   string
	// HandoffOperationID is supplied only by a detached child worker. It
	// consumes the parent-reserved handoff and its supervisor lease instead of
	// independently acquiring a competing lease for the Run.
	HandoffOperationID string
	HandoffProcessID   int
	HandoffLogPath     string
	Handler            DurableJobHandler
	LeaseTTL           time.Duration
	HeartbeatEvery     time.Duration
	PollInterval       time.Duration
	NewOperationKey    func() (string, error)
}

// RunWorkerSession owns only the supervisor lease and one run-scoped
// DurableWorker. It does not own a TUI, CLI, or scheduler root context.
type RunWorkerSession struct {
	services           *LifecycleServices
	runID              string
	owner              string
	actor              string
	reason             string
	worker             *DurableWorker
	handoffOperationID string
	handoffProcessID   int
	handoffLogPath     string

	leaseTTL        time.Duration
	heartbeatEvery  time.Duration
	pollInterval    time.Duration
	newOperationKey func() (string, error)
	heartbeatLease  func(context.Context, store.HeartbeatLeaseRequest) (store.Lease, error)
	getLease        func(context.Context, string) (*store.Lease, error)

	mu                 sync.Mutex
	pauseOperation     *store.DurableControlOperation
	terminateOperation *store.DurableControlOperation
}

// RunWorkerSessionResult is the final durable observation made by a worker
// process before it releases its supervisor fence.
type RunWorkerSessionResult struct {
	Run         store.WorkflowRun       `json:"run"`
	WorkerLease store.Lease             `json:"worker_lease"`
	Handoff     *store.RunWorkerHandoff `json:"handoff,omitempty"`
	LastCycle   DurableWorkerResult     `json:"last_cycle"`
	StoppedFor  store.WorkflowRunStatus `json:"stopped_for"`
}

// NewRunWorkerSession validates and constructs a run-scoped controlled worker.
func NewRunWorkerSession(config RunWorkerSessionConfig) (*RunWorkerSession, error) {
	if config.Services == nil || config.Services.core == nil || config.Services.core.store == nil || config.Services.Control == nil || config.Services.WorkerHandoffs == nil {
		return nil, fmt.Errorf("%w: lifecycle services are required", ErrRunWorkerConfiguration)
	}
	runID := strings.TrimSpace(config.RunID)
	if err := store.ValidateUUIDv7(runID); err != nil {
		return nil, fmt.Errorf("%w: run ID: %v", ErrRunWorkerConfiguration, err)
	}
	owner := strings.TrimSpace(config.Owner)
	actor := strings.TrimSpace(config.Actor)
	reason := strings.TrimSpace(config.Reason)
	if owner == "" || actor == "" || reason == "" {
		return nil, fmt.Errorf("%w: owner, actor, and reason are required", ErrRunWorkerConfiguration)
	}
	if config.Handler == nil {
		return nil, fmt.Errorf("%w: durable job handler is required", ErrRunWorkerConfiguration)
	}
	handoffOperationID := strings.TrimSpace(config.HandoffOperationID)
	if handoffOperationID != "" {
		if err := store.ValidateUUIDv7(handoffOperationID); err != nil {
			return nil, fmt.Errorf("%w: handoff operation ID: %v", ErrRunWorkerConfiguration, err)
		}
		if config.HandoffProcessID <= 0 || strings.TrimSpace(config.HandoffLogPath) == "" {
			return nil, fmt.Errorf("%w: handoff process ID and log path are required", ErrRunWorkerConfiguration)
		}
	} else if config.HandoffProcessID != 0 || strings.TrimSpace(config.HandoffLogPath) != "" {
		return nil, fmt.Errorf("%w: handoff receipt requires a handoff operation ID", ErrRunWorkerConfiguration)
	}
	if config.LeaseTTL == 0 {
		config.LeaseTTL = store.DefaultLeaseTTL
	}
	if config.HeartbeatEvery == 0 {
		config.HeartbeatEvery = store.DefaultLeaseHeartbeatInterval
	}
	if config.PollInterval == 0 {
		config.PollInterval = 250 * time.Millisecond
	}
	if config.LeaseTTL <= 0 || config.HeartbeatEvery <= 0 || config.HeartbeatEvery >= config.LeaseTTL || config.PollInterval <= 0 {
		return nil, fmt.Errorf("%w: lease TTL, heartbeat interval, and poll interval are invalid", ErrRunWorkerConfiguration)
	}
	if config.NewOperationKey == nil {
		config.NewOperationKey = store.NewUUIDv7
	}
	worker, err := NewDurableWorker(DurableWorkerConfig{
		Store: config.Services.core.store, Owner: owner, Actor: actor, Reason: reason, RunID: runID,
		LeaseTTL: config.LeaseTTL, HeartbeatEvery: config.HeartbeatEvery, PollInterval: config.PollInterval, Handler: config.Handler,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: create durable worker: %v", ErrRunWorkerConfiguration, err)
	}
	return &RunWorkerSession{
		services: config.Services, runID: runID, owner: owner, actor: actor, reason: reason, worker: worker,
		handoffOperationID: handoffOperationID, handoffProcessID: config.HandoffProcessID, handoffLogPath: strings.TrimSpace(config.HandoffLogPath),
		leaseTTL: config.LeaseTTL, heartbeatEvery: config.HeartbeatEvery, pollInterval: config.PollInterval,
		newOperationKey: config.NewOperationKey, heartbeatLease: config.Services.core.store.HeartbeatLease, getLease: config.Services.core.store.GetLease,
	}, nil
}

// Run claims the run-scoped supervisor fence, processes durable jobs, and
// exits when the Run becomes quiescent. A caller cancellation only ends this
// supervisor process; it never creates a synthetic pause or terminate action.
func (session *RunWorkerSession) Run(ctx context.Context) (RunWorkerSessionResult, error) {
	if session == nil || session.services == nil || session.services.core == nil || session.worker == nil {
		return RunWorkerSessionResult{}, ErrRunWorkerConfiguration
	}
	if ctx == nil {
		ctx = context.Background()
	}
	run, err := session.services.Runs.Get(ctx, session.runID)
	if err != nil {
		// database/sql may surface SQLite's interrupted status when the process
		// context is cancelled while a read is in flight. The session boundary
		// promises process-cancellation semantics, not a driver-specific error.
		if contextErr := ctx.Err(); contextErr != nil {
			return RunWorkerSessionResult{}, contextErr
		}
		return RunWorkerSessionResult{}, err
	}
	if _, ready, readyErr := session.eligibleQueuedRunWork(ctx, run); readyErr != nil {
		return RunWorkerSessionResult{}, fmt.Errorf("inspect eligible queued Run work: %w", readyErr)
	} else if !ready && run.Status != store.WorkflowRunRunning {
		return RunWorkerSessionResult{Run: run, StoppedFor: run.Status}, nil
	}
	var lease store.Lease
	var handoff *store.RunWorkerHandoff
	if session.handoffOperationID != "" {
		claim, claimErr := session.services.WorkerHandoffs.ClaimRunWorkerHandoff(ctx, session.handoffOperationID, session.runID, session.owner,
			session.handoffProcessID, session.handoffLogPath, session.actor, session.reason+": child claim controlled handoff", session.leaseTTL)
		if claimErr != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return RunWorkerSessionResult{}, contextErr
			}
			return RunWorkerSessionResult{}, fmt.Errorf("claim controlled run-worker handoff: %w", claimErr)
		}
		lease = claim.WorkerLease
		claimed := claim.Handoff
		handoff = &claimed
	} else {
		acquired, acquireErr := session.services.core.store.AcquireLease(ctx, store.AcquireLeaseRequest{
			ResourceType: RunWorkerLeaseResourceType, ResourceID: session.runID, Owner: session.owner, TTL: session.leaseTTL,
			Actor: session.actor, Reason: session.reason + ": acquire controlled run worker",
		})
		if acquireErr != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return RunWorkerSessionResult{}, contextErr
			}
			return RunWorkerSessionResult{}, fmt.Errorf("acquire controlled run-worker lease: %w", acquireErr)
		}
		lease = acquired
	}
	result := RunWorkerSessionResult{Run: run, WorkerLease: lease, Handoff: handoff}
	heartbeats := newRunWorkerLeaseHeartbeats(session, lease)
	session.worker.executionAbort = heartbeats.lostC
	heartbeats.start()
	defer func() {
		heartbeats.stop()
		latest := heartbeats.latest()
		if latest.State == store.LeaseActive {
			if session.handoffOperationID != "" {
				_, _ = session.services.WorkerHandoffs.ReleaseRunWorkerHandoff(context.Background(), session.handoffOperationID, latest,
					session.actor, session.reason+": controlled handoff worker stopped")
			} else {
				_, _ = session.services.core.store.ReleaseLease(context.Background(), store.ReleaseLeaseRequest{
					LeaseID: latest.ID, Owner: session.owner, FencingToken: latest.FencingToken, ExpectedVersion: latest.Version,
					Actor: session.actor, Reason: session.reason + ": controlled run worker stopped",
				})
			}
		}
	}()

	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if err := heartbeats.err(); err != nil {
			return result, fmt.Errorf("%w: %v", ErrRunWorkerLeaseLost, err)
		}
		commandTypes, ready, readyErr := session.eligibleQueuedRunWork(ctx, run)
		if readyErr != nil {
			return result, fmt.Errorf("inspect eligible queued Run work: %w", readyErr)
		}
		if !ready {
			if run.Status == store.WorkflowRunRunning {
				cycle, cycleErr := session.worker.RunOnceForCommandTypes(ctx, commandTypes)
				result.LastCycle = cycle
				if cycleErr != nil {
					return result, cycleErr
				}
				current, err := session.services.Runs.Get(ctx, session.runID)
				if err != nil {
					return result, err
				}
				result.Run = current
				run = current
				if current.Status == store.WorkflowRunRunning && cycle.Empty {
					if err := waitRunWorkerPoll(ctx, session.pollInterval); err != nil {
						return result, err
					}
					return result, fmt.Errorf("%w: run %s", ErrRunWorkerInconsistentState, current.ID)
				}
				continue
			}
			result.StoppedFor = run.Status
			return result, nil
		}
		cycle, cycleErr := session.worker.RunOnceForCommandTypes(ctx, commandTypes)
		result.LastCycle = cycle
		if cycleErr != nil && cycle.FinalState == "" {
			// A cancellation can race an otherwise-empty durable claim. SQLite
			// reports that race as "interrupted (9)" rather than context.Canceled;
			// preserve the Run contract at the process boundary.
			if contextErr := ctx.Err(); contextErr != nil {
				return result, contextErr
			}
			if errors.Is(cycleErr, ErrDurableJobLeaseLost) {
				var leaseLoss *DurableJobLeaseLostError
				if errors.As(cycleErr, &leaseLoss) && leaseLoss.FinalErrorClass != DurableJobHeartbeatFenceInvalid {
					if err := waitRunWorkerDispatchRecovery(ctx, heartbeats, leaseLoss.DispatchExpiresAt, session.pollInterval); err != nil {
						return result, err
					}
				}
				continue
			}
			return result, cycleErr
		}
		// A completed handler can create another durable Run (for example the
		// Standard materialization handoff) or queue continuation work. Drain
		// the same filtered activation outbox before this supervisor observes
		// its next state, so child Runs do not depend on a TUI exit gesture.
		if !cycle.Empty && session.services.RunActivations != nil && session.services.RunActivations.Available() {
			if err := session.services.RunActivations.Drain(ctx); err != nil {
				return result, fmt.Errorf("activate queued child Run workers: %w", err)
			}
		}
		current, err := session.services.Runs.Get(ctx, session.runID)
		if err != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return result, contextErr
			}
			return result, err
		}
		result.Run = current
		run = current
		if cycle.Empty {
			if err := waitRunWorkerPoll(ctx, session.pollInterval); err != nil {
				return result, err
			}
		}
	}
}

func waitRunWorkerDispatchRecovery(ctx context.Context, supervisor *runWorkerLeaseHeartbeats, expiresAt time.Time, pollInterval time.Duration) error {
	for time.Now().UTC().Before(expiresAt) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := supervisor.err(); err != nil {
			return fmt.Errorf("%w: %v", ErrRunWorkerLeaseLost, err)
		}
		remaining := time.Until(expiresAt)
		delay := pollInterval
		if delay <= 0 || delay > remaining {
			delay = remaining
		}
		if delay <= 0 {
			break
		}
		if err := waitRunWorkerPoll(ctx, delay); err != nil {
			return err
		}
	}
	return nil
}

// RequestSignalControl maps one process signal to a durable run-scoped control
// operation. A repeated signal returns the original operation, while terminate
// takes precedence over a previously requested pause.
func (session *RunWorkerSession) RequestSignalControl(ctx context.Context, action store.ControlAction) (store.DurableControlOperation, error) {
	if session == nil || session.services == nil || session.services.Control == nil {
		return store.DurableControlOperation{}, ErrRunWorkerConfiguration
	}
	if action != store.ControlActionPause && action != store.ControlActionTerminate {
		return store.DurableControlOperation{}, fmt.Errorf("%w: signal control action %q", ErrRunWorkerConfiguration, action)
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if action == store.ControlActionPause {
		if session.terminateOperation != nil {
			return *session.terminateOperation, nil
		}
		if session.pauseOperation != nil {
			return *session.pauseOperation, nil
		}
	} else if session.terminateOperation != nil {
		return *session.terminateOperation, nil
	}
	if existing, err := session.existingSignalControl(ctx, action); err != nil {
		return store.DurableControlOperation{}, err
	} else if existing != nil {
		if existing.Action == store.ControlActionTerminate {
			session.terminateOperation = existing
		} else {
			session.pauseOperation = existing
		}
		return *existing, nil
	}
	checkpoint, err := session.services.Control.CurrentCheckpoint(ctx, session.runID)
	if err != nil {
		return store.DurableControlOperation{}, err
	}
	operationKey, err := session.newOperationKey()
	if err != nil {
		return store.DurableControlOperation{}, fmt.Errorf("allocate signal control operation key: %w", err)
	}
	operation, err := session.services.Control.Request(ctx, RequestExecutionControlRequest{
		OperationKey: operationKey, Action: action, RunID: session.runID, Expected: checkpoint,
		Actor: session.actor, Reason: session.reason + ": process signal " + string(action),
	})
	if err != nil {
		return store.DurableControlOperation{}, err
	}
	if action == store.ControlActionPause {
		session.pauseOperation = &operation
	} else {
		session.terminateOperation = &operation
	}
	return operation, nil
}

// existingSignalControl makes signal handling idempotent across controlled
// child-process restarts. It only reuses an operation while the current Run
// remains in the state caused by that operation; an acknowledged prior pause
// on a Run that has later resumed must not suppress a new pause signal.
func (session *RunWorkerSession) existingSignalControl(ctx context.Context, action store.ControlAction) (*store.DurableControlOperation, error) {
	run, err := session.services.Runs.Get(ctx, session.runID)
	if err != nil {
		return nil, err
	}
	wanted := action
	switch action {
	case store.ControlActionPause:
		switch run.Status {
		case store.WorkflowRunPauseRequested, store.WorkflowRunPausing, store.WorkflowRunPaused:
			wanted = store.ControlActionPause
		case store.WorkflowRunCancelRequested, store.WorkflowRunStopRequested, store.WorkflowRunCanceling, store.WorkflowRunCanceled:
			// A persisted termination is stronger than a later SIGINT, including
			// when this worker is a restarted child with no in-memory cache.
			wanted = store.ControlActionTerminate
		default:
			return nil, nil
		}
	case store.ControlActionTerminate:
		switch run.Status {
		case store.WorkflowRunCancelRequested, store.WorkflowRunStopRequested, store.WorkflowRunCanceling, store.WorkflowRunCanceled:
			wanted = store.ControlActionTerminate
		default:
			return nil, nil
		}
	default:
		return nil, nil
	}
	operations, err := session.services.Control.ListForRun(ctx, session.runID)
	if err != nil {
		return nil, err
	}
	for index := range operations {
		if operations[index].Action == wanted {
			operation := operations[index]
			return &operation, nil
		}
	}
	return nil, nil
}

func runWorkerRunnable(status store.WorkflowRunStatus) bool {
	switch status {
	case store.WorkflowRunQueued, store.WorkflowRunRunning, store.WorkflowRunPauseRequested, store.WorkflowRunPausing,
		store.WorkflowRunResumeRequested, store.WorkflowRunCancelRequested, store.WorkflowRunStopRequested, store.WorkflowRunCanceling:
		return true
	default:
		return false
	}
}

func waitRunWorkerPoll(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type runWorkerLeaseHeartbeats struct {
	session *RunWorkerSession

	mu       sync.Mutex
	lease    store.Lease
	fail     error
	stopC    chan struct{}
	doneC    chan struct{}
	lostC    chan struct{}
	once     sync.Once
	lostOnce sync.Once
}

func newRunWorkerLeaseHeartbeats(session *RunWorkerSession, lease store.Lease) *runWorkerLeaseHeartbeats {
	return &runWorkerLeaseHeartbeats{session: session, lease: lease, stopC: make(chan struct{}), doneC: make(chan struct{}), lostC: make(chan struct{})}
}

func (heartbeats *runWorkerLeaseHeartbeats) start() {
	go func() {
		defer close(heartbeats.doneC)
		ticker := time.NewTicker(heartbeats.session.heartbeatEvery)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeats.stopC:
				return
			case <-ticker.C:
				if err := heartbeats.heartbeat(); err != nil {
					heartbeats.mu.Lock()
					heartbeats.fail = err
					heartbeats.mu.Unlock()
					heartbeats.lostOnce.Do(func() { close(heartbeats.lostC) })
					heartbeats.session.worker.cancelActiveExecution()
					return
				}
			}
		}
	}()
}

func (heartbeats *runWorkerLeaseHeartbeats) stop() {
	heartbeats.once.Do(func() { close(heartbeats.stopC) })
	<-heartbeats.doneC
}

func (heartbeats *runWorkerLeaseHeartbeats) heartbeat() error {
	heartbeats.mu.Lock()
	lease := heartbeats.lease
	heartbeats.mu.Unlock()
	updated, err := retryVersionedLeaseHeartbeat(context.Background(), heartbeats.stopC, heartbeats.session.heartbeatEvery, lease,
		func(ctx context.Context, current store.Lease) (store.Lease, error) {
			return heartbeats.session.heartbeatLease(ctx, store.HeartbeatLeaseRequest{
				LeaseID: current.ID, Owner: heartbeats.session.owner, FencingToken: current.FencingToken, ExpectedVersion: current.Version,
				TTL: heartbeats.session.leaseTTL, Actor: heartbeats.session.actor, Reason: heartbeats.session.reason + ": heartbeat controlled run worker",
			})
		}, heartbeats.session.getLease)
	if err != nil {
		return err
	}
	heartbeats.mu.Lock()
	heartbeats.lease = updated
	heartbeats.mu.Unlock()
	return nil
}

func (heartbeats *runWorkerLeaseHeartbeats) latest() store.Lease {
	heartbeats.mu.Lock()
	defer heartbeats.mu.Unlock()
	return heartbeats.lease
}

func (heartbeats *runWorkerLeaseHeartbeats) err() error {
	heartbeats.mu.Lock()
	defer heartbeats.mu.Unlock()
	return heartbeats.fail
}
