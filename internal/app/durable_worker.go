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

var (
	// ErrDurableWorkerConfiguration marks an incomplete local child-worker
	// configuration. Worker identity is explicit so a lease cannot be renewed
	// by an unrelated UI or process.
	ErrDurableWorkerConfiguration = errors.New("v2 executor: invalid durable worker configuration")
	// ErrDurableJobHandlerUnavailable means a claimed job has no V2 consumer.
	// The worker projects it as failed instead of treating it as completed.
	ErrDurableJobHandlerUnavailable = errors.New("v2 executor: durable job handler is unavailable")
)

// DurableJobHandler is the only extension point used by DurableWorker after a
// job is atomically claimed. The handler must perform its domain projection
// through application/store APIs and return the terminal job state it proved.
// It never receives a mutable scheduler root context or raw SQLite handle.
type DurableJobHandler interface {
	HandleDurableJob(context.Context, DurableJobExecution) (store.JobState, error)
}

// DurableJobHandlerFunc adapts local functions and focused integration fakes.
type DurableJobHandlerFunc func(context.Context, DurableJobExecution) (store.JobState, error)

func (function DurableJobHandlerFunc) HandleDurableJob(ctx context.Context, execution DurableJobExecution) (store.JobState, error) {
	return function(ctx, execution)
}

// DurableJobExecution is the immutable claim plus a scoped lease-liveness
// signal. A handler should abandon execution as soon as LeaseLost is closed;
// continuing after a failed heartbeat would violate the dispatch fence.
type DurableJobExecution struct {
	Claim     store.DurableJobDispatchClaim
	LeaseLost <-chan struct{}
}

// DurableWorkerConfig controls one local process worker. Lease defaults are
// the confirmed 90 seconds / 20 seconds; callers may override both only as a
// deployment profile, never through individual UI actions.
type DurableWorkerConfig struct {
	Store           *store.Store
	Owner           string
	Actor           string
	Reason          string
	LeaseTTL        time.Duration
	HeartbeatEvery  time.Duration
	CapacityPoolKey string
	PollInterval    time.Duration
	Handler         DurableJobHandler
}

// DurableWorker processes V2 durable jobs. It is suitable for an in-process
// foreground runner or a controlled child process: work lifetime is anchored
// to database leases, never to the caller's TUI context.
type DurableWorker struct {
	store           *store.Store
	owner           string
	actor           string
	reason          string
	leaseTTL        time.Duration
	heartbeatEvery  time.Duration
	capacityPoolKey string
	pollInterval    time.Duration
	handler         DurableJobHandler
}

// NewDurableWorker validates an explicit local worker profile.
func NewDurableWorker(config DurableWorkerConfig) (*DurableWorker, error) {
	if config.Store == nil {
		return nil, fmt.Errorf("%w: store is required", ErrDurableWorkerConfiguration)
	}
	owner := strings.TrimSpace(config.Owner)
	if owner == "" {
		return nil, fmt.Errorf("%w: owner is required", ErrDurableWorkerConfiguration)
	}
	if config.Handler == nil {
		return nil, fmt.Errorf("%w: handler is required", ErrDurableWorkerConfiguration)
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
		return nil, fmt.Errorf("%w: lease TTL, heartbeat interval, and poll interval must be positive; heartbeat must be shorter than TTL", ErrDurableWorkerConfiguration)
	}
	return &DurableWorker{
		store:           config.Store,
		owner:           owner,
		actor:           defaultWorkerActor(config.Actor, owner),
		reason:          strings.TrimSpace(config.Reason),
		leaseTTL:        config.LeaseTTL,
		heartbeatEvery:  config.HeartbeatEvery,
		capacityPoolKey: strings.TrimSpace(config.CapacityPoolKey),
		pollInterval:    config.PollInterval,
		handler:         config.Handler,
	}, nil
}

func defaultWorkerActor(actor, owner string) string {
	if actor = strings.TrimSpace(actor); actor != "" {
		return actor
	}
	return owner
}

// DurableWorkerResult reports one claim cycle. Empty is true only when the
// durable dispatcher found no queued work, which is distinct from a failed or
// interrupted worker cycle.
type DurableWorkerResult struct {
	Claim      store.DurableJobDispatchClaim
	Job        *store.DurableJob
	FinalState store.JobState
	Empty      bool
	Recoveries []store.ExpiredDurableJobRecovery
}

// RunOnce first reconciles expired worker fences, then atomically claims and
// executes one queued job. A claimed job always receives a terminal durable
// projection unless a lease is lost, in which case recovery owns the later
// interrupted/reconcile projection instead of a stale worker guessing it.
func (worker *DurableWorker) RunOnce(ctx context.Context) (DurableWorkerResult, error) {
	if err := worker.validate(); err != nil {
		return DurableWorkerResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return DurableWorkerResult{}, err
	}
	recoveries, err := worker.store.ScanExpiredDurableJobsForReconcile(ctx, store.ScanExpiredDurableJobsRequest{
		Limit:  100,
		Actor:  worker.actor,
		Reason: worker.reasonFor("recover expired durable worker"),
	})
	if err != nil {
		return DurableWorkerResult{}, fmt.Errorf("recover expired durable jobs: %w", err)
	}
	claimKey, err := store.NewUUIDv7()
	if err != nil {
		return DurableWorkerResult{}, fmt.Errorf("allocate durable claim key: %w", err)
	}
	claim, err := worker.store.ClaimNextDurableJob(ctx, store.ClaimNextDurableJobRequest{
		IdempotencyKey:  "durable-worker-claim:" + claimKey,
		Owner:           worker.owner,
		LeaseTTL:        worker.leaseTTL,
		CapacityPoolKey: worker.capacityPoolKey,
		Actor:           worker.actor,
		Reason:          worker.reasonFor("claim durable job"),
	})
	if err != nil {
		return DurableWorkerResult{Recoveries: recoveries}, fmt.Errorf("claim durable job: %w", err)
	}
	result := DurableWorkerResult{Claim: claim, Recoveries: recoveries}
	if claim.State == "empty" || claim.Job == nil {
		result.Empty = true
		return result, nil
	}
	job := *claim.Job
	result.Job = &job

	leaseContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	heartbeats := newDispatchLeaseHeartbeats(worker, claim, cancel)
	heartbeats.start()
	defer heartbeats.stop()

	state, handlerErr := worker.handler.HandleDurableJob(leaseContext, DurableJobExecution{Claim: claim, LeaseLost: heartbeats.lost})
	if handlerErr != nil && state == "" {
		state = stateForHandlerError(handlerErr)
	}
	if state == "" {
		state = store.JobFailed
		if handlerErr == nil {
			handlerErr = fmt.Errorf("%w: handler returned no terminal job state", ErrDurableJobHandlerUnavailable)
		}
	}
	if !isWorkerTerminalJobState(state) {
		handlerErr = fmt.Errorf("%w: handler returned non-terminal state %q", ErrDurableWorkerConfiguration, state)
		state = store.JobFailed
	}
	if heartbeats.wasLost() {
		// The active fence may have been reclaimed. Never use a stale writer to
		// overwrite the conservative interrupted/reconcile projection.
		if handlerErr != nil {
			return result, fmt.Errorf("durable job lease lost while handling %s: %w", job.ID, handlerErr)
		}
		return result, fmt.Errorf("durable job lease lost while handling %s", job.ID)
	}

	current, err := worker.store.GetDurableJob(context.Background(), job.ID)
	if err != nil {
		return result, err
	}
	if current == nil {
		return result, fmt.Errorf("%w: claimed job %s disappeared", store.ErrNotFound, job.ID)
	}
	if current.State == state {
		result.FinalState = state
		return result, handlerErr
	}
	if current.State != store.JobRunning {
		return result, fmt.Errorf("%w: claimed job %s moved to %s", store.ErrOptimisticLock, current.ID, current.State)
	}
	transitioned, transitionErr := worker.store.TransitionDurableJob(context.Background(), store.TransitionDurableJobRequest{
		JobID:           current.ID,
		ExpectedVersion: current.Version,
		State:           state,
		Actor:           worker.actor,
		Reason:          worker.reasonFor("complete durable job"),
	})
	if transitionErr != nil {
		return result, transitionErr
	}
	result.Job = &transitioned
	result.FinalState = transitioned.State
	return result, handlerErr
}

// Run keeps consuming jobs until the supplied process context ends. The
// durable execution context passed to handlers is intentionally independent
// from ctx: canceling a TUI/attach caller does not cancel a claimed job.
func (worker *DurableWorker) Run(ctx context.Context) error {
	if err := worker.validate(); err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		result, err := worker.RunOnce(ctx)
		if err != nil {
			// A handler error after a durable terminal projection belongs to
			// that job's audit trail. Do not make a controlled child abandon
			// unrelated queued work merely because one stage failed.
			if result.FinalState != "" {
				continue
			}
			return err
		}
		if !result.Empty {
			continue
		}
		timer := time.NewTimer(worker.pollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (worker *DurableWorker) validate() error {
	if worker == nil || worker.store == nil || worker.handler == nil || worker.owner == "" {
		return ErrDurableWorkerConfiguration
	}
	return nil
}

func (worker *DurableWorker) reasonFor(action string) string {
	if worker.reason == "" {
		return action
	}
	return worker.reason + ": " + action
}

func isWorkerTerminalJobState(state store.JobState) bool {
	switch state {
	case store.JobSucceeded, store.JobFailed, store.JobCanceled, store.JobInterrupted:
		return true
	default:
		return false
	}
}

func stateForHandlerError(err error) store.JobState {
	if errors.Is(err, context.Canceled) {
		return store.JobInterrupted
	}
	return store.JobFailed
}

type dispatchLeaseHeartbeats struct {
	worker *DurableWorker
	cancel context.CancelFunc

	mu     sync.Mutex
	leases []store.Lease
	lost   chan struct{}
	once   sync.Once
	stopCh chan struct{}
	doneCh chan struct{}
}

func newDispatchLeaseHeartbeats(worker *DurableWorker, claim store.DurableJobDispatchClaim, cancel context.CancelFunc) *dispatchLeaseHeartbeats {
	leases := make([]store.Lease, 0, 2)
	if claim.DispatchLease != nil {
		leases = append(leases, *claim.DispatchLease)
	}
	if claim.CapacityLease != nil {
		leases = append(leases, *claim.CapacityLease)
	}
	heartbeats := &dispatchLeaseHeartbeats{
		worker: worker,
		cancel: cancel,
		leases: leases,
		lost:   make(chan struct{}),
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
	return heartbeats
}

func (heartbeats *dispatchLeaseHeartbeats) start() {
	go func() {
		defer close(heartbeats.doneCh)
		ticker := time.NewTicker(heartbeats.worker.heartbeatEvery)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeats.stopCh:
				return
			case <-ticker.C:
				if err := heartbeats.heartbeat(context.Background()); err != nil {
					heartbeats.once.Do(func() {
						close(heartbeats.lost)
						// Cancel only the claimed durable operation; the caller that
						// attached this worker retains its own independent context.
						heartbeats.cancel()
					})
					return
				}
			}
		}
	}()
}

func (heartbeats *dispatchLeaseHeartbeats) stop() {
	close(heartbeats.stopCh)
	<-heartbeats.doneCh
}

func (heartbeats *dispatchLeaseHeartbeats) wasLost() bool {
	select {
	case <-heartbeats.lost:
		return true
	default:
		return false
	}
}

func (heartbeats *dispatchLeaseHeartbeats) heartbeat(ctx context.Context) error {
	heartbeats.mu.Lock()
	defer heartbeats.mu.Unlock()
	for index := range heartbeats.leases {
		lease := heartbeats.leases[index]
		updated, err := heartbeats.worker.store.HeartbeatLease(ctx, store.HeartbeatLeaseRequest{
			LeaseID:         lease.ID,
			Owner:           heartbeats.worker.owner,
			FencingToken:    lease.FencingToken,
			ExpectedVersion: lease.Version,
			TTL:             heartbeats.worker.leaseTTL,
			Actor:           heartbeats.worker.actor,
			Reason:          heartbeats.worker.reasonFor("heartbeat durable dispatch lease"),
		})
		if err != nil {
			return err
		}
		heartbeats.leases[index] = updated
	}
	return nil
}
