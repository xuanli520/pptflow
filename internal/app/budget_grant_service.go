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

// taskOwner derives ownership from the immutable task.created audit event.
// The existing V2 task table predates owner storage; the append-only audit
// record is therefore the authoritative, migration-free source of truth.
func (service *BudgetGrantService) taskOwner(ctx context.Context, taskID string) (string, error) {
	events, err := service.core.store.ListAuditEvents(ctx, store.ListAuditEventsRequest{
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
