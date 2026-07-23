package app

import (
	"context"
	"errors"
	"strings"
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

func TestBudgetGrantServiceResolvesTaskAndAuthoringRunQuotaOwners(t *testing.T) {
	ctx := context.Background()

	taskServices, task, revision := newControlLifecycleFixture(t, "task-budget-owner")
	taskRun, err := taskServices.Runs.StartRun(ctx, StartRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, Profile: lifecycleCompleteProfile(t), ExecutionSpec: lifecycleExecutionSpec(task.ID, revision.ID, revision.TaskDigest),
		Trigger: "budget-test", Actor: "task-budget-owner", Reason: "start task-bound budget fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	taskAccount, err := taskServices.Store().CreateQuotaAccount(ctx, store.CreateQuotaAccountRequest{
		ScopeKind: store.QuotaScopeTask, ScopeID: task.ID, Dimension: "agent_tokens", LimitUnits: 10,
		Actor: "task-budget-owner", Reason: "configure task-bound budget fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	if taskID, err := taskServices.Budgets.QuotaTaskIDForRun(ctx, taskRun.ID); err != nil || taskID != task.ID {
		t.Fatalf("task-bound quota Task = %q, %v; want %q", taskID, err, task.ID)
	}
	accounts, err := taskServices.Budgets.ListRunBudgets(ctx, taskRun.ID)
	if err != nil || len(accounts) != 1 || accounts[0].ID != taskAccount.ID {
		t.Fatalf("task-bound run budgets = %+v, %v", accounts, err)
	}
	taskGrant, err := taskServices.Budgets.GrantRunBudget(ctx, GrantRunBudgetRequest{
		RunID: taskRun.ID, IdempotencyKey: "task-run-budget-grant", Dimension: "agent_tokens", DeltaUnits: 2,
		ExpectedVersion: taskAccount.Version, Actor: "task-budget-owner", Reason: "grant task-bound run budget",
	})
	if err != nil || taskGrant.ScopeID != task.ID || taskGrant.LimitUnits != 12 {
		t.Fatalf("task-bound run grant = %+v, %v", taskGrant, err)
	}

	authoringServices, _, _, authoringTask, authoringRun := newAuthoringSessionRuntimeFixture(t, "authoring-budget-owner")
	if authoringRun.TaskID != "" {
		t.Fatalf("authoring Run unexpectedly has task ID %q", authoringRun.TaskID)
	}
	authoringAccount, err := authoringServices.Store().CreateQuotaAccount(ctx, store.CreateQuotaAccountRequest{
		ScopeKind: store.QuotaScopeTask, ScopeID: authoringTask.ID, Dimension: "agent_tokens", LimitUnits: 20,
		Actor: "authoring-budget-owner", Reason: "configure authoring budget fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	if taskID, err := authoringServices.Budgets.QuotaTaskIDForRun(ctx, authoringRun.ID); err != nil || taskID != authoringTask.ID {
		t.Fatalf("authoring quota Task = %q, %v; want %q", taskID, err, authoringTask.ID)
	}
	accounts, err = authoringServices.Budgets.ListRunBudgets(ctx, authoringRun.ID)
	if err != nil || len(accounts) != 1 || accounts[0].ID != authoringAccount.ID {
		t.Fatalf("authoring run budgets = %+v, %v", accounts, err)
	}
	authoringGrant, err := authoringServices.Budgets.GrantRunBudget(ctx, GrantRunBudgetRequest{
		RunID: authoringRun.ID, IdempotencyKey: "authoring-run-budget-grant", Dimension: "agent_tokens", DeltaUnits: 5,
		ExpectedVersion: authoringAccount.Version, Actor: "authoring-budget-owner", Reason: "grant authoring run budget",
	})
	if err != nil || authoringGrant.ScopeID != authoringTask.ID || authoringGrant.LimitUnits != 25 {
		t.Fatalf("authoring run grant = %+v, %v", authoringGrant, err)
	}
	if _, err := authoringServices.Budgets.GrantRunBudget(ctx, GrantRunBudgetRequest{
		RunID: authoringRun.ID, IdempotencyKey: "authoring-run-budget-non-owner", Dimension: "agent_tokens", DeltaUnits: 1,
		ExpectedVersion: authoringGrant.Version, Actor: "another-local-user", Reason: "unauthorized authoring budget grant",
	}); err == nil || !strings.Contains(err.Error(), "only task owner") {
		t.Fatalf("authoring non-owner budget grant error = %v", err)
	}
}
