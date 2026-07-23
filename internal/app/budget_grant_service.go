package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

// BudgetGrantService keeps owner-authorized quota increases behind an
// application boundary. The store enforces optimistic account versions and
// idempotency; this layer verifies that the actor is the local task owner.
type BudgetGrantService struct{ core *lifecycleServiceCore }

type GrantBudgetRequest struct {
	ID              string
	IdempotencyKey  string
	TaskID          string
	Dimension       string
	DeltaUnits      int64
	ExpectedVersion int64
	Actor           string
	Reason          string
}

// GrantRunBudgetRequest grants quota to the Task account durably associated
// with a workflow Run. A Run may be bound directly to a TaskRevision or to an
// AuthoringSession, whose draft target Task owns its quota before
// materialization.
type GrantRunBudgetRequest struct {
	ID              string
	IdempotencyKey  string
	RunID           string
	Dimension       string
	DeltaUnits      int64
	ExpectedVersion int64
	Actor           string
	Reason          string
}

// ListTaskBudgets returns every configured task-scoped quota account in a
// deterministic dimension order. A missing account is not fabricated as a
// zero-limit budget: callers must distinguish it from configured exhaustion.
func (service *BudgetGrantService) ListTaskBudgets(ctx context.Context, taskID string) ([]store.QuotaAccount, error) {
	if service == nil || service.core == nil {
		return nil, fmt.Errorf("budget grant service is not configured")
	}
	task, err := service.core.store.GetTaskV2(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, fmt.Errorf("%w: task %s", ErrLifecycleNotFound, taskID)
	}
	return service.core.store.ListQuotaAccountsForScope(ctx, store.QuotaScopeTask, task.ID)
}

// QuotaTaskIDForRun resolves the durable Task account that owns quota for a
// Run without exposing the Run's subject representation to callers. In
// particular, an AuthoringSession Run has no Run.TaskID even though its draft
// target Task owns quota.
func (service *BudgetGrantService) QuotaTaskIDForRun(ctx context.Context, runID string) (string, error) {
	if service == nil || service.core == nil {
		return "", fmt.Errorf("budget grant service is not configured")
	}
	run, err := service.core.store.GetWorkflowRun(ctx, strings.TrimSpace(runID))
	if err != nil {
		return "", err
	}
	if run == nil {
		return "", fmt.Errorf("%w: run %s", ErrLifecycleNotFound, strings.TrimSpace(runID))
	}
	subject, err := service.core.resolveWorkflowRunSubject(ctx, *run)
	if err != nil {
		return "", fmt.Errorf("resolve quota-owning Task for Run %s: %w", run.ID, err)
	}
	taskID, err := subject.quotaTaskID()
	if err != nil {
		return "", fmt.Errorf("resolve quota-owning Task for Run %s: %w", run.ID, err)
	}
	return taskID, nil
}

// ListRunBudgets returns configured quota accounts for the Task that owns the
// supplied Run. It supports both TaskRevision and AuthoringSession subjects.
func (service *BudgetGrantService) ListRunBudgets(ctx context.Context, runID string) ([]store.QuotaAccount, error) {
	taskID, err := service.QuotaTaskIDForRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	return service.ListTaskBudgets(ctx, taskID)
}

func (service *BudgetGrantService) GrantTaskBudget(ctx context.Context, request GrantBudgetRequest) (store.DurableBudgetGrant, error) {
	if service == nil || service.core == nil {
		return store.DurableBudgetGrant{}, fmt.Errorf("budget grant service is not configured")
	}
	if strings.TrimSpace(request.Actor) == "" || strings.TrimSpace(request.Reason) == "" || strings.TrimSpace(request.IdempotencyKey) == "" {
		return store.DurableBudgetGrant{}, fmt.Errorf("budget grant actor, reason, and idempotency key are required")
	}
	task, err := service.core.store.GetTaskV2(ctx, request.TaskID)
	if err != nil {
		return store.DurableBudgetGrant{}, err
	}
	if task == nil {
		return store.DurableBudgetGrant{}, fmt.Errorf("%w: task %s", ErrLifecycleNotFound, request.TaskID)
	}
	owner, err := service.taskOwner(ctx, task.ID)
	if err != nil {
		return store.DurableBudgetGrant{}, err
	}
	if owner != request.Actor {
		return store.DurableBudgetGrant{}, fmt.Errorf("only task owner %q may grant this task budget", owner)
	}
	return service.core.store.GrantQuota(ctx, store.GrantBudgetRequest{
		ID:              request.ID,
		IdempotencyKey:  request.IdempotencyKey,
		ScopeKind:       store.QuotaScopeTask,
		ScopeID:         request.TaskID,
		Dimension:       request.Dimension,
		DeltaUnits:      request.DeltaUnits,
		ExpectedVersion: request.ExpectedVersion,
		Actor:           request.Actor,
		Reason:          request.Reason,
	})
}

// GrantRunBudget grants quota to the Task that owns the supplied Run. The
// final authorization remains in GrantTaskBudget, so an AuthoringSession does
// not weaken the existing immutable task-owner check.
func (service *BudgetGrantService) GrantRunBudget(ctx context.Context, request GrantRunBudgetRequest) (store.DurableBudgetGrant, error) {
	taskID, err := service.QuotaTaskIDForRun(ctx, request.RunID)
	if err != nil {
		return store.DurableBudgetGrant{}, err
	}
	return service.GrantTaskBudget(ctx, GrantBudgetRequest{
		ID:              request.ID,
		IdempotencyKey:  request.IdempotencyKey,
		TaskID:          taskID,
		Dimension:       request.Dimension,
		DeltaUnits:      request.DeltaUnits,
		ExpectedVersion: request.ExpectedVersion,
		Actor:           request.Actor,
		Reason:          request.Reason,
	})
}

// taskOwner derives ownership from the immutable task.created audit event.
// The existing V2 task table predates owner storage; the append-only audit
// record is therefore the authoritative, migration-free source of truth.
func (service *BudgetGrantService) taskOwner(ctx context.Context, taskID string) (string, error) {
	return taskOwnerFromAudit(ctx, service.core.store, taskID)
}

// taskOwnerFromAudit resolves the immutable creator identity used for local
// owner-authorized budget changes. Recovery uses the same authority check
// before accepting a quota-exhaustion retry exception.
func taskOwnerFromAudit(ctx context.Context, dataStore *store.Store, taskID string) (string, error) {
	if dataStore == nil {
		return "", fmt.Errorf("task owner lookup store is not configured")
	}
	events, err := dataStore.ListAuditEvents(ctx, store.ListAuditEventsRequest{
		EntityType: "task",
		EntityID:   taskID,
	})
	if err != nil {
		return "", err
	}
	for _, event := range events {
		if event.Action == "task.created" && strings.TrimSpace(event.Actor) != "" {
			return event.Actor, nil
		}
	}
	return "", fmt.Errorf("task %s has no owner audit event", taskID)
}
