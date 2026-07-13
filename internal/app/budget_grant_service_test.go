package app

import (
	"context"
	"errors"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

func TestBudgetGrantServiceRequiresTaskOwnerAndPreservesGrantIdempotency(t *testing.T) {
	ctx := context.Background()
	const owner = "budget-owner"
	services, task, _ := newControlLifecycleFixture(t, owner)
	account, err := services.Store().CreateQuotaAccount(ctx, store.CreateQuotaAccountRequest{
		ScopeKind: store.QuotaScopeTask, ScopeID: task.ID, Dimension: "agent_tokens", LimitUnits: 100,
		Actor: owner, Reason: "configure explicit task quota",
	})
	if err != nil {
		t.Fatal(err)
	}

	grant, err := services.Budgets.GrantTaskBudget(ctx, GrantBudgetRequest{
		IdempotencyKey: "budget-grant-fixture", TaskID: task.ID, Dimension: "agent_tokens", DeltaUnits: 25,
		ExpectedVersion: account.Version, Actor: owner, Reason: "approve additional task budget",
	})
	if err != nil {
		t.Fatal(err)
	}
	if grant.PreviousVersion != account.Version || grant.Version != account.Version+1 || grant.LimitUnits != 125 {
		t.Fatalf("budget grant = %+v, want version advance and explicit limit", grant)
	}
	replayed, err := services.Budgets.GrantTaskBudget(ctx, GrantBudgetRequest{
		IdempotencyKey: "budget-grant-fixture", TaskID: task.ID, Dimension: "agent_tokens", DeltaUnits: 25,
		ExpectedVersion: account.Version, Actor: owner, Reason: "approve additional task budget",
	})
	if err != nil {
		t.Fatalf("replay budget grant: %v", err)
	}
	if replayed.ID != grant.ID || replayed.Version != grant.Version || replayed.LimitUnits != grant.LimitUnits {
		t.Fatalf("replayed grant = %+v, want %+v", replayed, grant)
	}
	if _, err := services.Budgets.GrantTaskBudget(ctx, GrantBudgetRequest{
		IdempotencyKey: "budget-grant-non-owner", TaskID: task.ID, Dimension: "agent_tokens", DeltaUnits: 1,
		ExpectedVersion: grant.Version, Actor: "another-local-user", Reason: "unauthorized attempt",
	}); err == nil {
		t.Fatal("non-owner was allowed to grant task budget")
	}
	if _, err := services.Budgets.GrantTaskBudget(ctx, GrantBudgetRequest{
		IdempotencyKey: "budget-grant-stale", TaskID: task.ID, Dimension: "agent_tokens", DeltaUnits: 1,
		ExpectedVersion: account.Version, Actor: owner, Reason: "stale grant",
	}); !errors.Is(err, store.ErrStaleQuotaGrant) {
		t.Fatalf("stale grant error = %v, want %v", err, store.ErrStaleQuotaGrant)
	}
	updated, err := services.Store().GetQuotaAccountForScope(ctx, store.QuotaScopeTask, task.ID, "agent_tokens")
	if err != nil {
		t.Fatal(err)
	}
	if updated == nil || updated.Version != grant.Version || updated.LimitUnits != grant.LimitUnits {
		t.Fatalf("quota account after grant = %+v, want %+v", updated, grant)
	}
	accounts, err := services.Budgets.ListTaskBudgets(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || accounts[0].ID != account.ID || accounts[0].Version != grant.Version {
		t.Fatalf("listed task budgets = %+v, want updated task account", accounts)
	}
}
