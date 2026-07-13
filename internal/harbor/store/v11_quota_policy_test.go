package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMigrateV10ToV11InstallsQuotaPolicyBindingTable(t *testing.T) {
	s := tempDB(t)
	root := s.rootDir
	if _, err := s.db.Exec(`DROP TABLE quota_account_policy_bindings_v11`); err != nil {
		t.Fatalf("remove v11 fixture table: %v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM schema_version WHERE version = 11`); err != nil {
		t.Fatalf("rewind schema fixture to v10: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(root)
	if err != nil {
		t.Fatalf("migrate V10 store to V11: %v", err)
	}
	defer migrated.Close()
	var version, tableCount int
	if err := migrated.db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := migrated.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'quota_account_policy_bindings_v11'`).Scan(&tableCount); err != nil {
		t.Fatal(err)
	}
	if version != 11 || tableCount != 1 {
		t.Fatalf("V11 migration result version=%d table_count=%d", version, tableCount)
	}
}

func TestV11AdmissionInitializesPolicyAccountsAndRequiresExplicitUpgradeGrant(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	task, _ := createValidatedTaskAndRevision(t, s)
	const actor = "quota-policy-owner"

	request := testFrozenQuotaAdmission(task.ID, actor, "quota-policy-v1")
	missingBootstrap := request
	missingBootstrap.IdempotencyKey = "quota-policy-missing-bootstrap"
	missingBootstrap.BootstrapAccounts = nil
	if _, err := s.AdmitTaskActorQuota(ctx, missingBootstrap); !errors.Is(err, ErrInvalidQuota) {
		t.Fatalf("admission without frozen bootstrap accounts = %v, want ErrInvalidQuota", err)
	}

	decision, err := s.AdmitTaskActorQuota(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if !decision.Accepted || len(decision.Leases) != 2 {
		t.Fatalf("policy-backed admission = %+v, want accepted task+actor leases", decision)
	}

	for _, expectation := range []struct {
		kind        QuotaScopeKind
		id          string
		dimension   string
		limit       int64
		policyID    string
		version     string
		fingerprint string
	}{
		{QuotaScopeTask, task.ID, "stage_attempt", 10, "test.local.operator", "1.0.0", "sha256:quota-policy-v1"},
		{QuotaScopeActor, actor, "stage_attempt", 20, "test.local.operator", "1.0.0", "sha256:quota-policy-v1"},
		{QuotaScopeTask, task.ID, "agent_turn", 64, "test.local.operator", "1.0.0", "sha256:quota-policy-v1"},
		{QuotaScopeActor, actor, "agent_turn", 128, "test.local.operator", "1.0.0", "sha256:quota-policy-v1"},
	} {
		account, err := s.GetQuotaAccountForScope(ctx, expectation.kind, expectation.id, expectation.dimension)
		if err != nil || account == nil {
			t.Fatalf("account %s/%s/%s = %+v, %v", expectation.kind, expectation.id, expectation.dimension, account, err)
		}
		if account.LimitUnits != expectation.limit {
			t.Fatalf("account %s/%s/%s limit = %d, want %d", expectation.kind, expectation.id, expectation.dimension, account.LimitUnits, expectation.limit)
		}
		binding, err := s.GetQuotaAccountPolicyBinding(ctx, account.ID)
		if err != nil || binding == nil {
			t.Fatalf("account binding %s = %+v, %v", account.ID, binding, err)
		}
		if binding.PolicyID != expectation.policyID || binding.PolicyVersion != expectation.version || binding.PolicyFingerprint != expectation.fingerprint || binding.InitialLimitUnits != expectation.limit {
			t.Fatalf("account binding = %+v, want policy %s@%s limit %d", binding, expectation.policyID, expectation.version, expectation.limit)
		}
	}

	replayed, err := s.AdmitTaskActorQuota(ctx, request)
	if err != nil || replayed.ID != decision.ID || len(replayed.Leases) != len(decision.Leases) {
		t.Fatalf("idempotent policy admission = %+v, %v; want %+v", replayed, err, decision)
	}

	changed := testFrozenQuotaAdmission(task.ID, actor, "quota-policy-v2")
	changed.Policy = QuotaPolicyBinding{PolicyID: "test.local.operator", PolicyVersion: "2.0.0", PolicyFingerprint: "sha256:quota-policy-v2"}
	changed.BootstrapAccounts[0] = QuotaAccountBootstrap{Dimension: "stage_attempt", TaskLimitUnits: 11, ActorLimitUnits: 21}
	if _, err := s.AdmitTaskActorQuota(ctx, changed); !errors.Is(err, ErrQuotaPolicyAccountMismatch) {
		t.Fatalf("changed policy silently altered existing accounts: %v, want ErrQuotaPolicyAccountMismatch", err)
	}

	taskAccount, err := s.GetQuotaAccountForScope(ctx, QuotaScopeTask, task.ID, "stage_attempt")
	if err != nil || taskAccount == nil {
		t.Fatal(err)
	}
	actorAccount, err := s.GetQuotaAccountForScope(ctx, QuotaScopeActor, actor, "stage_attempt")
	if err != nil || actorAccount == nil {
		t.Fatal(err)
	}
	if _, err := s.GrantQuota(ctx, GrantBudgetRequest{
		IdempotencyKey: "quota-policy-task-grant", ScopeKind: QuotaScopeTask, ScopeID: task.ID, Dimension: "stage_attempt",
		DeltaUnits: 1, ExpectedVersion: taskAccount.Version, Actor: actor, Reason: "adopt quota policy v2",
	}); err != nil {
		t.Fatalf("explicit task grant: %v", err)
	}
	if _, err := s.GrantQuota(ctx, GrantBudgetRequest{
		IdempotencyKey: "quota-policy-actor-grant", ScopeKind: QuotaScopeActor, ScopeID: actor, Dimension: "stage_attempt",
		DeltaUnits: 1, ExpectedVersion: actorAccount.Version, Actor: actor, Reason: "adopt quota policy v2",
	}); err != nil {
		t.Fatalf("explicit actor grant: %v", err)
	}
	updated, err := s.AdmitTaskActorQuota(ctx, changed)
	if err != nil || !updated.Accepted || len(updated.Leases) != 2 {
		t.Fatalf("admission after explicit grants = %+v, %v", updated, err)
	}
}

func testFrozenQuotaAdmission(taskID, actor, key string) AdmitTaskActorQuotaRequest {
	return AdmitTaskActorQuotaRequest{
		IdempotencyKey: key,
		TaskID:         taskID,
		Actor:          actor,
		LeaseOwner:     "quota-policy-worker",
		LeaseTTL:       time.Hour,
		Policy: QuotaPolicyBinding{
			PolicyID: "test.local.operator", PolicyVersion: "1.0.0", PolicyFingerprint: "sha256:quota-policy-v1",
		},
		BootstrapAccounts: []QuotaAccountBootstrap{
			{Dimension: "stage_attempt", TaskLimitUnits: 10, ActorLimitUnits: 20},
			{Dimension: "agent_turn", TaskLimitUnits: 64, ActorLimitUnits: 128},
		},
		Claims: []TaskActorQuotaClaim{{Dimension: "stage_attempt", Units: 1, ReclaimPolicy: QuotaReclaimUnused}},
		Reason: "admit frozen policy fixture",
	}
}
