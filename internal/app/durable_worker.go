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
	// ErrDurableJobLeaseLost is returned only when the claimed dispatch fence can
	// no longer authorize worker-owned writes. The scanner/recovery applicator,
	// rather than the stale worker, owns every subsequent durable projection.
	ErrDurableJobLeaseLost = errors.New("v2 executor: durable job dispatch lease lost")
)

// DurableJobHeartbeatErrorClass is a stable, operator-safe classification. It
// intentionally excludes raw Store errors, paths, payloads, and provider text.
type DurableJobHeartbeatErrorClass string

const (
	DurableJobHeartbeatFenceInvalid   DurableJobHeartbeatErrorClass = "fence_invalid"
	DurableJobHeartbeatStoreTransient DurableJobHeartbeatErrorClass = "store_transient"
	DurableJobHeartbeatStoreFailure   DurableJobHeartbeatErrorClass = "store_failure"
	DurableJobHeartbeatRetryExhausted DurableJobHeartbeatErrorClass = "retry_window_exhausted"
	DurableJobHeartbeatRecovered      DurableJobHeartbeatErrorClass = "recovered"
)

// DurableJobLeaseLostError carries only bounded recovery timing and stable
// classifications. Error deliberately omits the underlying Store error.
type DurableJobLeaseLostError struct {
	JobID             string
	DispatchExpiresAt time.Time
	FirstErrorClass   DurableJobHeartbeatErrorClass
	FinalErrorClass   DurableJobHeartbeatErrorClass
}

func (err *DurableJobLeaseLostError) Error() string {
	if err == nil {
		return ErrDurableJobLeaseLost.Error()
	}
	return fmt.Sprintf("%s: job %s (%s)", ErrDurableJobLeaseLost, err.JobID, err.FinalErrorClass)
}

func (err *DurableJobLeaseLostError) Is(target error) bool {
	return target == ErrDurableJobLeaseLost
}

// DurableJobHandler is the only extension point used by DurableWorker after a
// job is atomically claimed. The handler must perform its domain projection
// through application/store APIs and return the terminal delivery fact it
// proved. It never receives a mutable scheduler root context or raw SQLite
// handle.
type DurableJobHandler interface {
	HandleDurableJob(context.Context, DurableJobExecution) (DurableJobResult, error)
}

// DurableJobRecoveryHandler is an optional domain reconciliation hook. The
// store has already fenced and projected every recovered job before this hook
// runs. Implementations may therefore restore only durable follow-up work
// that is provably missing; they must not resume the in_doubt job itself.
//
// It is deliberately separate from DurableJobHandler: recovery is not a
// second attempt at an unknown side effect. In particular, a stage runtime may
// enqueue a coordinator after observing an already-persisted terminal stage
// result, while an external-effect stage remains in_doubt for explicit
// reconciliation.
type DurableJobRecoveryHandler interface {
	ReconcileDurableJobRecoveries(context.Context, DurableJobRecoveryRequest) error
}

type DurableJobRecoveryRequest struct {
	RunID      string
	Recoveries []store.ExpiredDurableJobRecovery
}

// DurableJobHandlerFunc adapts result-returning handlers and focused
// integration fakes. Failure semantics are part of DurableJobResult; callers
// must not return an error and expect DurableWorker to classify it.
type DurableJobHandlerFunc func(context.Context, DurableJobExecution) (DurableJobResult, error)

func (function DurableJobHandlerFunc) HandleDurableJob(ctx context.Context, execution DurableJobExecution) (DurableJobResult, error) {
	return function(ctx, execution)
}

// DurableJobExecution is the immutable claim plus a scoped lease-liveness
// signal. A handler should abandon execution as soon as LeaseLost is closed;
// continuing after a failed heartbeat would violate the dispatch fence.
type DurableJobExecution struct {
	Claim         store.DurableJobDispatchClaim
	DispatchFence store.DispatchFence
	LeaseLost     <-chan struct{}
}

// DurableWorkerConfig controls one local process worker. Lease defaults are
// the confirmed 90 seconds / 20 seconds; callers may override both only as a
// deployment profile, never through individual UI actions.
type DurableWorkerConfig struct {
	Store  *store.Store
	Owner  string
	Actor  string
	Reason string
	// RunID fences a controlled child to jobs and expired-lease recovery for
	// exactly one durable Run. An empty value is reserved for an explicitly
	// deployment-wide worker supervisor.
	RunID           string
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
	runID           string
	leaseTTL        time.Duration
	heartbeatEvery  time.Duration
	capacityPoolKey string
	pollInterval    time.Duration
	handler         DurableJobHandler
	heartbeatLease  func(context.Context, store.HeartbeatLeaseRequest) (store.Lease, error)
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
	runID := strings.TrimSpace(config.RunID)
	if runID != "" {
		if err := store.ValidateUUIDv7(runID); err != nil {
			return nil, fmt.Errorf("%w: run ID: %v", ErrDurableWorkerConfiguration, err)
		}
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
		runID:           runID,
		leaseTTL:        config.LeaseTTL,
		heartbeatEvery:  config.HeartbeatEvery,
		capacityPoolKey: strings.TrimSpace(config.CapacityPoolKey),
		pollInterval:    config.PollInterval,
		handler:         config.Handler,
		heartbeatLease:  config.Store.HeartbeatLease,
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
	Claim                    store.DurableJobDispatchClaim
	Job                      *store.DurableJob
	FinalState               store.JobState
	Empty                    bool
	Recoveries               []store.ExpiredDurableJobRecovery
	HeartbeatFirstErrorClass DurableJobHeartbeatErrorClass
	HeartbeatFinalErrorClass DurableJobHeartbeatErrorClass
}

// RunOnce first reconciles expired worker fences, then atomically claims and
// executes one queued job. A claimed job always receives a delivery-final
// durable projection unless a lease is lost, in which case recovery owns the
// later in_doubt/reconcile projection instead of a stale worker guessing it.
// JobInDoubt is delivery-final; a new explicit redrive job owns any later
// attempt.
func (worker *DurableWorker) RunOnce(ctx context.Context) (DurableWorkerResult, error) {
	return worker.RunOnceForCommandTypes(ctx, nil)
}

// RunOnceForCommandTypes processes one claimed job while optionally limiting
// the SQLite claim to exact command types. RunWorkerSession uses this for
// durable review, continuation, repair, and reconciliation states so an old
// queued stage job cannot be selected outside ordinary Run execution.
func (worker *DurableWorker) RunOnceForCommandTypes(ctx context.Context, commandTypes []string) (DurableWorkerResult, error) {
	if err := worker.validate(); err != nil {
		return DurableWorkerResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return DurableWorkerResult{}, err
	}
	recoveries, err := worker.store.ScanExpiredDurableJobsForReconcile(ctx, store.ScanExpiredDurableJobsRequest{
		RunID:  worker.runID,
		Limit:  100,
		Actor:  worker.actor,
		Reason: worker.reasonFor("recover expired durable worker"),
	})
	if err != nil {
		return DurableWorkerResult{}, fmt.Errorf("recover expired durable jobs: %w", err)
	}
	if reconciler, ok := worker.handler.(DurableJobRecoveryHandler); ok {
		if err := reconciler.ReconcileDurableJobRecoveries(ctx, DurableJobRecoveryRequest{RunID: worker.runID, Recoveries: recoveries}); err != nil {
			return DurableWorkerResult{Recoveries: recoveries}, fmt.Errorf("reconcile recovered durable jobs: %w", err)
		}
	}
	claimKey, err := store.NewUUIDv7()
	if err != nil {
		return DurableWorkerResult{}, fmt.Errorf("allocate durable claim key: %w", err)
	}
	claim, err := worker.store.ClaimNextDurableJob(ctx, store.ClaimNextDurableJobRequest{
		IdempotencyKey:  "durable-worker-claim:" + claimKey,
		Owner:           worker.owner,
		RunID:           worker.runID,
		CommandTypes:    append([]string(nil), commandTypes...),
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

	if claim.DispatchLease == nil {
		return result, fmt.Errorf("%w: claimed job %s has no dispatch lease", ErrDurableWorkerConfiguration, job.ID)
	}
	leaseContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	fence := store.DispatchFence{LeaseID: claim.DispatchLease.ID, Owner: claim.Owner, FencingToken: claim.DispatchLease.FencingToken}
	guard := newDispatchFenceGuard(cancel)
	leaseContext = store.WithDispatchFence(leaseContext, fence, guard)
	heartbeats := newDispatchLeaseHeartbeats(worker, claim, guard)
	heartbeats.start()
	defer heartbeats.stop()

	handlerResult, handlerErr := worker.handler.HandleDurableJob(leaseContext, DurableJobExecution{Claim: claim, DispatchFence: fence, LeaseLost: guard.lost})
	heartbeats.stop()
	result.HeartbeatFirstErrorClass, result.HeartbeatFinalErrorClass = heartbeats.errorClasses()
	if handlerResult.State == "" {
		if handlerErr == nil {
			handlerErr = fmt.Errorf("%w: handler returned no terminal job result", ErrDurableJobHandlerUnavailable)
		}
		handlerResult = DurableJobResult{
			State:   store.JobFailed,
			Failure: newDurableJobFailure("worker.handler_result_missing", "The durable worker handler did not return a terminal result.", durableJobFailureDetails(job, "handler_result")),
		}
	}
	if !isWorkerTerminalJobState(handlerResult.State) {
		handlerErr = fmt.Errorf("%w: handler returned non-terminal state %q", ErrDurableWorkerConfiguration, handlerResult.State)
		handlerResult = DurableJobResult{
			State:   store.JobFailed,
			Failure: newDurableJobFailure("worker.handler_result_invalid", "The durable worker handler returned an invalid terminal result.", durableJobFailureDetails(job, "handler_result")),
		}
	}
	if (handlerResult.State == store.JobFailed || handlerResult.State == store.JobInDoubt) && handlerResult.Failure == nil {
		handlerErr = fmt.Errorf("%w: handler returned %s without a failure record", ErrDurableWorkerConfiguration, handlerResult.State)
		handlerResult.Failure = newDurableJobFailure("worker.failure_record_missing", "The durable worker handler omitted its required failure record.", durableJobFailureDetails(job, "failure_record"))
	}
	if handlerResult.State != store.JobFailed && handlerResult.State != store.JobInDoubt && handlerResult.Failure != nil {
		handlerErr = fmt.Errorf("%w: handler returned a failure record for %s", ErrDurableWorkerConfiguration, handlerResult.State)
		handlerResult = DurableJobResult{
			State:   store.JobFailed,
			Failure: newDurableJobFailure("worker.handler_result_invalid", "The durable worker handler returned an invalid terminal result.", durableJobFailureDetails(job, "failure_record")),
		}
	}
	if !validWorkerRunProjection(job, handlerResult) {
		handlerErr = fmt.Errorf("%w: handler returned an invalid Run projection for terminal state %s", ErrDurableWorkerConfiguration, handlerResult.State)
		handlerResult = DurableJobResult{
			State:   store.JobFailed,
			Failure: newDurableJobFailure("worker.handler_result_invalid", "The durable worker handler returned an invalid terminal result.", durableJobFailureDetails(job, "run_projection")),
		}
	}
	handlerErr = durableJobHandlerError(handlerResult, handlerErr)
	state := handlerResult.State
	if heartbeats.wasLost() {
		return result, heartbeats.leaseLostError(job.ID)
	}

	current, err := worker.store.GetDurableJob(context.Background(), job.ID)
	if err != nil {
		return result, err
	}
	if current == nil {
		return result, fmt.Errorf("%w: claimed job %s disappeared", store.ErrNotFound, job.ID)
	}
	if current.State == state {
		result.Job = current
		result.FinalState = state
		return result, handlerErr
	}
	if current.State != store.JobRunning {
		return result, fmt.Errorf("%w: claimed job %s moved to %s", store.ErrOptimisticLock, current.ID, current.State)
	}
	transitionContext := context.WithoutCancel(leaseContext)
	transitioned, transitionErr := worker.store.TransitionDurableJob(transitionContext, store.TransitionDurableJobRequest{
		JobID:           current.ID,
		ExpectedVersion: current.Version,
		State:           state,
		Failure:         handlerResult.Failure,
		RunProjection:   handlerResult.RunProjection,
		Actor:           worker.actor,
		Reason:          worker.reasonFor("complete durable job"),
	})
	if transitionErr != nil {
		if errors.Is(transitionErr, store.ErrDispatchFenceLost) {
			guard.lose(DurableJobHeartbeatFenceInvalid)
			result.HeartbeatFirstErrorClass, result.HeartbeatFinalErrorClass = heartbeats.errorClasses()
			return result, heartbeats.leaseLostError(job.ID)
		}
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
	case store.JobSucceeded, store.JobFailed, store.JobCanceled, store.JobInterrupted, store.JobInDoubt:
		return true
	default:
		return false
	}
}

func validWorkerRunProjection(job store.DurableJob, result DurableJobResult) bool {
	if result.RunProjection == nil {
		return true
	}
	if store.ValidateUUIDv7(job.RunID) != nil {
		return false
	}
	switch result.State {
	case store.JobSucceeded:
		return result.RunProjection.Status == store.WorkflowRunRunning
	case store.JobInDoubt:
		return result.RunProjection.Status == store.WorkflowRunInDoubt
	case store.JobFailed:
		return result.RunProjection.Status == store.WorkflowRunFailedTerminal
	default:
		return false
	}
}

type dispatchFenceGuard struct {
	mu         sync.Mutex
	lost       chan struct{}
	once       sync.Once
	cancel     context.CancelFunc
	finalClass DurableJobHeartbeatErrorClass
}

func newDispatchFenceGuard(cancel context.CancelFunc) *dispatchFenceGuard {
	return &dispatchFenceGuard{lost: make(chan struct{}), cancel: cancel}
}

func (guard *dispatchFenceGuard) BeginDispatchFenceMutation() error {
	guard.mu.Lock()
	if guard.wasLostLocked() {
		guard.mu.Unlock()
		return store.ErrDispatchFenceLost
	}
	return nil
}

func (guard *dispatchFenceGuard) EndDispatchFenceMutation() {
	guard.mu.Unlock()
}

func (guard *dispatchFenceGuard) withHeartbeat(operation func() error) error {
	guard.mu.Lock()
	defer guard.mu.Unlock()
	if guard.wasLostLocked() {
		return store.ErrDispatchFenceLost
	}
	return operation()
}

func (guard *dispatchFenceGuard) lose(class DurableJobHeartbeatErrorClass) {
	guard.mu.Lock()
	defer guard.mu.Unlock()
	guard.loseLocked(class)
}

func (guard *dispatchFenceGuard) loseLocked(class DurableJobHeartbeatErrorClass) {
	guard.once.Do(func() {
		guard.finalClass = class
		close(guard.lost)
		guard.cancel()
	})
}

func (guard *dispatchFenceGuard) wasLost() bool {
	guard.mu.Lock()
	defer guard.mu.Unlock()
	return guard.wasLostLocked()
}

func (guard *dispatchFenceGuard) wasLostLocked() bool {
	select {
	case <-guard.lost:
		return true
	default:
		return false
	}
}

type dispatchLeaseHeartbeats struct {
	worker *DurableWorker
	guard  *dispatchFenceGuard

	mu         sync.Mutex
	leases     []store.Lease
	firstClass DurableJobHeartbeatErrorClass
	finalClass DurableJobHeartbeatErrorClass
	stopOnce   sync.Once
	stopCh     chan struct{}
	doneCh     chan struct{}
}

func newDispatchLeaseHeartbeats(worker *DurableWorker, claim store.DurableJobDispatchClaim, guard *dispatchFenceGuard) *dispatchLeaseHeartbeats {
	leases := make([]store.Lease, 0, 2)
	if claim.DispatchLease != nil {
		leases = append(leases, *claim.DispatchLease)
	}
	if claim.CapacityLease != nil {
		leases = append(leases, *claim.CapacityLease)
	}
	heartbeats := &dispatchLeaseHeartbeats{
		worker: worker,
		guard:  guard,
		leases: leases,
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
				if !heartbeats.heartbeatWithinSafeWindow() {
					return
				}
			}
		}
	}()
}

func (heartbeats *dispatchLeaseHeartbeats) stop() {
	heartbeats.stopOnce.Do(func() { close(heartbeats.stopCh) })
	<-heartbeats.doneCh
}

func (heartbeats *dispatchLeaseHeartbeats) wasLost() bool {
	return heartbeats.guard != nil && heartbeats.guard.wasLost()
}

func (heartbeats *dispatchLeaseHeartbeats) heartbeat(ctx context.Context) error {
	heartbeats.mu.Lock()
	defer heartbeats.mu.Unlock()
	for index := range heartbeats.leases {
		lease := heartbeats.leases[index]
		updated, err := heartbeats.worker.heartbeatLease(ctx, store.HeartbeatLeaseRequest{
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

func (heartbeats *dispatchLeaseHeartbeats) heartbeatWithinSafeWindow() bool {
	for {
		err := heartbeats.guard.withHeartbeat(func() error {
			return heartbeats.heartbeat(context.Background())
		})
		if err == nil {
			heartbeats.recordHeartbeatClass(DurableJobHeartbeatRecovered, true)
			return true
		}
		class := classifyDispatchHeartbeatError(err)
		heartbeats.recordHeartbeatClass(class, false)
		if class != DurableJobHeartbeatStoreTransient {
			heartbeats.guard.lose(class)
			return false
		}
		if errors.Is(err, store.ErrOptimisticLock) {
			if refreshErr := heartbeats.refreshLeases(context.Background()); refreshErr != nil {
				refreshClass := classifyDispatchHeartbeatError(refreshErr)
				heartbeats.recordHeartbeatClass(refreshClass, false)
				if refreshClass != DurableJobHeartbeatStoreTransient {
					heartbeats.guard.lose(refreshClass)
					return false
				}
			}
		}
		deadline := heartbeats.dispatchExpiresAt()
		delay := heartbeats.retryDelay(deadline)
		if delay <= 0 {
			heartbeats.recordHeartbeatClass(DurableJobHeartbeatRetryExhausted, false)
			heartbeats.guard.lose(DurableJobHeartbeatRetryExhausted)
			return false
		}
		timer := time.NewTimer(delay)
		select {
		case <-heartbeats.stopCh:
			if !timer.Stop() {
				<-timer.C
			}
			return true
		case <-timer.C:
		}
	}
}

func (heartbeats *dispatchLeaseHeartbeats) refreshLeases(ctx context.Context) error {
	heartbeats.mu.Lock()
	defer heartbeats.mu.Unlock()
	now := time.Now().UTC()
	for index := range heartbeats.leases {
		current := heartbeats.leases[index]
		loaded, err := heartbeats.worker.store.GetLease(ctx, current.ID)
		if err != nil {
			return err
		}
		if loaded == nil || loaded.Owner != current.Owner || loaded.FencingToken != current.FencingToken || loaded.State != store.LeaseActive || !loaded.ExpiresAt.After(now) {
			return store.ErrDispatchFenceLost
		}
		heartbeats.leases[index] = *loaded
	}
	return nil
}

func (heartbeats *dispatchLeaseHeartbeats) retryDelay(deadline time.Time) time.Duration {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return 0
	}
	delay := heartbeats.worker.heartbeatEvery / 4
	if delay <= 0 || delay > 250*time.Millisecond {
		delay = 250 * time.Millisecond
	}
	if delay < 10*time.Millisecond {
		delay = 10 * time.Millisecond
	}
	if delay >= remaining {
		return 0
	}
	return delay
}

func (heartbeats *dispatchLeaseHeartbeats) dispatchExpiresAt() time.Time {
	heartbeats.mu.Lock()
	defer heartbeats.mu.Unlock()
	for _, lease := range heartbeats.leases {
		if lease.ResourceType == "job_dispatch" {
			return lease.ExpiresAt.UTC()
		}
	}
	return time.Time{}
}

func (heartbeats *dispatchLeaseHeartbeats) recordHeartbeatClass(class DurableJobHeartbeatErrorClass, recovered bool) {
	heartbeats.mu.Lock()
	defer heartbeats.mu.Unlock()
	if recovered {
		if heartbeats.firstClass != "" {
			heartbeats.finalClass = class
		}
		return
	}
	if heartbeats.firstClass == "" {
		heartbeats.firstClass = class
	}
	heartbeats.finalClass = class
}

func (heartbeats *dispatchLeaseHeartbeats) errorClasses() (DurableJobHeartbeatErrorClass, DurableJobHeartbeatErrorClass) {
	heartbeats.mu.Lock()
	defer heartbeats.mu.Unlock()
	if heartbeats.guard != nil && heartbeats.guard.finalClass != "" {
		heartbeats.finalClass = heartbeats.guard.finalClass
	}
	if heartbeats.firstClass == "" && heartbeats.finalClass != "" {
		heartbeats.firstClass = heartbeats.finalClass
	}
	return heartbeats.firstClass, heartbeats.finalClass
}

func (heartbeats *dispatchLeaseHeartbeats) leaseLostError(jobID string) error {
	first, final := heartbeats.errorClasses()
	if first == "" {
		first = final
	}
	if final == "" {
		final = DurableJobHeartbeatFenceInvalid
	}
	return &DurableJobLeaseLostError{JobID: jobID, DispatchExpiresAt: heartbeats.dispatchExpiresAt(), FirstErrorClass: first, FinalErrorClass: final}
}

func classifyDispatchHeartbeatError(err error) DurableJobHeartbeatErrorClass {
	if errors.Is(err, store.ErrDispatchFenceLost) || errors.Is(err, store.ErrFencingToken) || errors.Is(err, store.ErrImmutable) || errors.Is(err, store.ErrLeaseHeld) || errors.Is(err, store.ErrNotFound) {
		return DurableJobHeartbeatFenceInvalid
	}
	if errors.Is(err, store.ErrOptimisticLock) || transientSQLiteError(err) {
		return DurableJobHeartbeatStoreTransient
	}
	return DurableJobHeartbeatStoreFailure
}

func transientSQLiteError(err error) bool {
	type sqliteCoder interface{ Code() int }
	var coded sqliteCoder
	if !errors.As(err, &coded) {
		return false
	}
	switch coded.Code() & 0xff {
	case 5, 6, 10: // SQLITE_BUSY, SQLITE_LOCKED, SQLITE_IOERR
		return true
	default:
		return false
	}
}
