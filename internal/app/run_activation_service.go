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
			return nil
		}
	}
}

func runActivationOutboxTopics() []string {
	return []string{
		workflowRunQueuedOutboxTopic,
		continuationExecutionQueuedOutboxTopic,
		revisionCandidateContinuationQueuedOutboxTopic,
	}
}

// DeliverOutboxEvent implements OutboxDeliveryHandler. The event's payload
// is intentionally not parsed: the store is the authority for the Run,
// continuation and queued durable-job lineage.
func (service *RunActivationService) DeliverOutboxEvent(ctx context.Context, event store.OutboxEvent) error {
	if !service.Available() {
		return fmt.Errorf("run activation worker launcher is not configured")
	}
	run, jobEntityID, err := service.activationRunForEvent(ctx, event)
	if err != nil {
		return err
	}
	if !runActivationRunnable(run.Status) {
		return nil
	}
	if err := service.verifyActivationJob(ctx, run, event.Topic, jobEntityID); err != nil {
		return err
	}
	return service.ensureRunWorkerHandoff(ctx, run)
}

func (service *RunActivationService) activationRunForEvent(ctx context.Context, event store.OutboxEvent) (store.WorkflowRun, string, error) {
	switch event.Topic {
	case workflowRunQueuedOutboxTopic:
		if event.EntityType != "workflow_run" {
			return store.WorkflowRun{}, "", fmt.Errorf("queued Run activation event %s has entity type %q", event.ID, event.EntityType)
		}
		if err := store.ValidateUUIDv7(event.EntityID); err != nil {
			return store.WorkflowRun{}, "", fmt.Errorf("queued Run activation event %s Run ID: %w", event.ID, err)
		}
		run, err := service.core.store.GetWorkflowRun(ctx, event.EntityID)
		if err != nil {
			return store.WorkflowRun{}, "", err
		}
		if run == nil {
			return store.WorkflowRun{}, "", fmt.Errorf("%w: queued Run activation Run %s", ErrLifecycleNotFound, event.EntityID)
		}
		return *run, run.ID, nil
	case continuationExecutionQueuedOutboxTopic, revisionCandidateContinuationQueuedOutboxTopic:
		if event.EntityType != "continuation_execution" {
			return store.WorkflowRun{}, "", fmt.Errorf("continuation activation event %s has entity type %q", event.ID, event.EntityType)
		}
		execution, err := service.core.store.GetContinuationExecution(ctx, event.EntityID)
		if err != nil {
			return store.WorkflowRun{}, "", err
		}
		if execution == nil {
			return store.WorkflowRun{}, "", fmt.Errorf("%w: continuation activation %s", ErrLifecycleNotFound, event.EntityID)
		}
		run, err := service.core.store.GetWorkflowRun(ctx, execution.RunID)
		if err != nil {
			return store.WorkflowRun{}, "", err
		}
		if run == nil {
			return store.WorkflowRun{}, "", fmt.Errorf("%w: continuation activation Run %s", ErrLifecycleNotFound, execution.RunID)
		}
		return *run, execution.ID, nil
	default:
		return store.WorkflowRun{}, "", fmt.Errorf("unsupported queued Run activation topic %q", event.Topic)
	}
}

func (service *RunActivationService) verifyActivationJob(ctx context.Context, run store.WorkflowRun, topic, entityID string) error {
	jobs, err := service.core.store.ListDurableJobsForRun(ctx, run.ID)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if job.RunID != run.ID {
			continue
		}
		switch topic {
		case workflowRunQueuedOutboxTopic:
			if job.CommandType != "workflow_run.execute" || job.EntityType != "workflow_run" || job.EntityID != run.ID {
				continue
			}
		case continuationExecutionQueuedOutboxTopic, revisionCandidateContinuationQueuedOutboxTopic:
			if job.EntityType != "continuation_execution" || job.EntityID != entityID {
				continue
			}
		default:
			return fmt.Errorf("unsupported queued Run activation topic %q", topic)
		}
		return nil
	}
	return fmt.Errorf("queued Run activation %s has no durable job for Run %s", topic, run.ID)
}

func (service *RunActivationService) ensureRunWorkerHandoff(ctx context.Context, run store.WorkflowRun) error {
	if !runActivationRunnable(run.Status) {
		return nil
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
	actor := strings.TrimSpace(run.CreatedBy)
	if actor == "" {
		actor = runActivationActor
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

func runActivationRunnable(status store.WorkflowRunStatus) bool {
	switch status {
	case store.WorkflowRunQueued, store.WorkflowRunRunning, store.WorkflowRunPauseRequested, store.WorkflowRunPausing,
		store.WorkflowRunResumeRequested, store.WorkflowRunCancelRequested, store.WorkflowRunStopRequested, store.WorkflowRunCanceling:
		return true
	default:
		return false
	}
}

var _ OutboxDeliveryHandler = (*RunActivationService)(nil)
