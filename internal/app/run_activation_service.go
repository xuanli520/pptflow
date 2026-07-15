package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

const (
	workflowRunQueuedOutboxTopic                   = "workflow_run.queued"
	continuationExecutionQueuedOutboxTopic         = "continuation_execution.queued"
	revisionCandidateContinuationQueuedOutboxTopic = "revision_candidate.continuation_queued"

	runActivationActor  = "harbor-factory-run-activation"
	runActivationReason = "automatically activate durable queued Run"
)

// RunActivationService is the single application-owned delivery route from a
// durable queued-work event to a controlled child worker. It deliberately
// re-reads the referenced durable entities instead of trusting an outbox
// payload and delegates process spawning to the existing handoff protocol.
//
// A nil launcher leaves activation unavailable for tests and read/control
// compositions; it does not weaken queued Run persistence or create an
// in-process execution fallback.
type RunActivationService struct {
	core     *lifecycleServiceCore
	launcher RunWorkerHandoffLauncher
}

type queuedRunActivation struct {
	run store.WorkflowRun
	job store.DurableJob
}

func (service *RunActivationService) Available() bool {
	return service != nil && service.core != nil && service.core.store != nil && service.launcher != nil
}

// Drain delivers all currently-ready queued-work activation events using the
// durable outbox claim/heartbeat/ack protocol. New events created while a
// worker is handling work are observed on its next drain, and a failure is
// NACKed before the error is returned to the caller.
func (service *RunActivationService) Drain(ctx context.Context) error {
	if service == nil || service.core == nil || service.core.store == nil {
		return fmt.Errorf("run activation service is not configured")
	}
	if service.launcher == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ownerID, err := store.NewUUIDv7()
	if err != nil {
		return fmt.Errorf("allocate queued Run activation owner: %w", err)
	}
	dispatcher, err := NewOutboxDispatcher(OutboxDispatcherConfig{
		Store:      service.core.store,
		Owner:      "run-activation:" + ownerID,
		Actor:      runActivationActor,
		Reason:     runActivationReason,
		RetryDelay: time.Second,
		Topics:     runActivationOutboxTopics(),
		Handler:    service,
	})
	if err != nil {
		return fmt.Errorf("construct queued Run activation dispatcher: %w", err)
	}
	for {
		result, runErr := dispatcher.RunOnce(ctx)
		if runErr != nil {
			return fmt.Errorf("deliver queued Run activation: %w", runErr)
		}
		if result.Empty {
			break
		}
	}
	return service.sweepQueuedRunJobs(ctx)
}

func runActivationOutboxTopics() []string {
	return []string{
		workflowRunQueuedOutboxTopic,
		continuationExecutionQueuedOutboxTopic,
		revisionCandidateContinuationQueuedOutboxTopic,
		store.DurableJobQueuedOutboxTopic,
	}
}

// DeliverOutboxEvent implements OutboxDeliveryHandler. The event's payload
// is intentionally not parsed: the store is the authority for the Run,
// continuation and queued durable-job lineage.
func (service *RunActivationService) DeliverOutboxEvent(ctx context.Context, event store.OutboxEvent) error {
	if !service.Available() {
		return fmt.Errorf("run activation worker launcher is not configured")
	}
	activation, found, err := service.activationForEvent(ctx, event)
	if err != nil {
		return err
	}
	if !found || !runWorkerJobIsEligible(activation.run.Status, activation.job) {
		return nil
	}
	return service.ensureRunWorkerHandoff(ctx, activation.run)
}

func (service *RunActivationService) activationForEvent(ctx context.Context, event store.OutboxEvent) (queuedRunActivation, bool, error) {
	switch event.Topic {
	case workflowRunQueuedOutboxTopic:
		if event.EntityType != "workflow_run" {
			return queuedRunActivation{}, false, fmt.Errorf("queued Run activation event %s has entity type %q", event.ID, event.EntityType)
		}
		if err := store.ValidateUUIDv7(event.EntityID); err != nil {
			return queuedRunActivation{}, false, fmt.Errorf("queued Run activation event %s Run ID: %w", event.ID, err)
		}
		run, err := service.core.store.GetWorkflowRun(ctx, event.EntityID)
		if err != nil {
			return queuedRunActivation{}, false, err
		}
		if run == nil {
			return queuedRunActivation{}, false, fmt.Errorf("%w: queued Run activation Run %s", ErrLifecycleNotFound, event.EntityID)
		}
		return service.queuedJobForRun(ctx, *run, func(job store.DurableJob) bool {
			return job.CommandType == "workflow_run.execute" && job.EntityType == "workflow_run" && job.EntityID == run.ID
		})
	case continuationExecutionQueuedOutboxTopic, revisionCandidateContinuationQueuedOutboxTopic:
		if event.EntityType != "continuation_execution" {
			return queuedRunActivation{}, false, fmt.Errorf("continuation activation event %s has entity type %q", event.ID, event.EntityType)
		}
		execution, err := service.core.store.GetContinuationExecution(ctx, event.EntityID)
		if err != nil {
			return queuedRunActivation{}, false, err
		}
		if execution == nil {
			return queuedRunActivation{}, false, fmt.Errorf("%w: continuation activation %s", ErrLifecycleNotFound, event.EntityID)
		}
		run, err := service.core.store.GetWorkflowRun(ctx, execution.RunID)
		if err != nil {
			return queuedRunActivation{}, false, err
		}
		if run == nil {
			return queuedRunActivation{}, false, fmt.Errorf("%w: continuation activation Run %s", ErrLifecycleNotFound, execution.RunID)
		}
		return service.queuedJobForRun(ctx, *run, func(job store.DurableJob) bool {
			return job.EntityType == "continuation_execution" && job.EntityID == execution.ID
		})
	case store.DurableJobQueuedOutboxTopic:
		if event.EntityType != "durable_job" {
			return queuedRunActivation{}, false, fmt.Errorf("durable job activation event %s has entity type %q", event.ID, event.EntityType)
		}
		job, err := service.core.store.GetDurableJob(ctx, event.EntityID)
		if err != nil {
			return queuedRunActivation{}, false, err
		}
		if job == nil || job.State != store.JobQueued || job.RunID == "" {
			return queuedRunActivation{}, false, nil
		}
		return service.queuedActivationForJob(ctx, *job)
	default:
		return queuedRunActivation{}, false, fmt.Errorf("unsupported queued Run activation topic %q", event.Topic)
	}
}

func (service *RunActivationService) queuedJobForRun(ctx context.Context, run store.WorkflowRun, matches func(store.DurableJob) bool) (queuedRunActivation, bool, error) {
	jobs, err := service.core.store.ListDurableJobsForRun(ctx, run.ID)
	if err != nil {
		return queuedRunActivation{}, false, err
	}
	for _, job := range jobs {
		if job.RunID != run.ID || job.State != store.JobQueued || !matches(job) {
			continue
		}
		return queuedRunActivation{run: run, job: job}, true, nil
	}
	return queuedRunActivation{}, false, nil
}

func (service *RunActivationService) queuedActivationForJob(ctx context.Context, job store.DurableJob) (queuedRunActivation, bool, error) {
	if job.State != store.JobQueued || job.RunID == "" {
		return queuedRunActivation{}, false, nil
	}
	run, err := service.core.store.GetWorkflowRun(ctx, job.RunID)
	if err != nil {
		return queuedRunActivation{}, false, err
	}
	if run == nil {
		return queuedRunActivation{}, false, fmt.Errorf("%w: durable job activation Run %s", ErrLifecycleNotFound, job.RunID)
	}
	return queuedRunActivation{run: *run, job: job}, true, nil
}

// sweepQueuedRunJobs repairs the small window after a parent records a child
// spawn receipt but the child dies before claiming it. It re-reads queued jobs,
// expires only provably stale handoffs, and lets the normal reserve/spawn
// protocol create at most one replacement worker.
func (service *RunActivationService) sweepQueuedRunJobs(ctx context.Context) error {
	jobs, err := service.core.store.ListQueuedDurableJobs(ctx, store.ListQueuedDurableJobsRequest{})
	if err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(jobs))
	for _, job := range jobs {
		activation, found, err := service.queuedActivationForJob(ctx, job)
		if err != nil {
			return err
		}
		if !found || !runWorkerJobIsEligible(activation.run.Status, activation.job) {
			continue
		}
		if _, duplicate := seen[activation.run.ID]; duplicate {
			continue
		}
		seen[activation.run.ID] = struct{}{}
		if err := service.ensureRunWorkerHandoff(ctx, activation.run); err != nil {
			return err
		}
	}
	return nil
}

func (service *RunActivationService) ensureRunWorkerHandoff(ctx context.Context, run store.WorkflowRun) error {
	actor := strings.TrimSpace(run.CreatedBy)
	if actor == "" {
		actor = runActivationActor
	}
	if _, err := service.core.store.ReconcileRunWorkerHandoffs(ctx, store.ReconcileRunWorkerHandoffsRequest{
		RunID: run.ID, Actor: actor, Reason: runActivationReason,
	}); err != nil {
		return fmt.Errorf("reconcile queued Run worker handoffs: %w", err)
	}
	handoffs, err := service.core.store.ListRunWorkerHandoffsForRun(ctx, run.ID)
	if err != nil {
		return err
	}
	for _, handoff := range handoffs {
		switch handoff.State {
		case store.RunWorkerHandoffLaunching, store.RunWorkerHandoffHandedOff:
			return nil
		}
	}
	key, err := store.NewUUIDv7()
	if err != nil {
		return fmt.Errorf("allocate queued Run worker handoff key: %w", err)
	}
	handoff, err := (&RunWorkerHandoffService{core: service.core}).LaunchRunWorkerHandoff(ctx, ReserveRunWorkerHandoffCommand{
		IdempotencyKey: key,
		RunID:          run.ID,
		Expected: RunWorkerHandoffCheckpoint{
			RunVersion: run.Version, ExecutionEpoch: run.ExecutionEpoch, DefinitionHash: run.DefinitionHash,
		},
		Owner:  "automatic-run-worker:" + run.ID,
		Actor:  actor,
		Reason: runActivationReason,
	}, service.launcher)
	if err != nil {
		if errors.Is(err, store.ErrLeaseHeld) {
			return nil
		}
		return err
	}
	switch handoff.State {
	case store.RunWorkerHandoffLaunching, store.RunWorkerHandoffHandedOff, store.RunWorkerHandoffReleased:
		return nil
	case store.RunWorkerHandoffFailed:
		return fmt.Errorf("automatic Run worker handoff %s failed: %s", handoff.ID, strings.TrimSpace(handoff.FailureReason))
	case store.RunWorkerHandoffExpired:
		return fmt.Errorf("automatic Run worker handoff %s expired before child claim", handoff.ID)
	default:
		return fmt.Errorf("automatic Run worker handoff %s returned unsupported state %s", handoff.ID, handoff.State)
	}
}

var _ OutboxDeliveryHandler = (*RunActivationService)(nil)
