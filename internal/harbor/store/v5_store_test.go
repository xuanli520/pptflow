package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func tempV5DB(t *testing.T) *Store {
	t.Helper()
	return tempDB(t)
}

func TestV5QuotaLedgerAdmissionAndReconcile(t *testing.T) {
	ctx := context.Background()
	s := tempV5DB(t)
	task, _ := createValidatedTaskAndRevision(t, s)
	actor := "local-owner"

	taskToken, err := s.CreateQuotaAccount(ctx, CreateQuotaAccountRequest{
		ScopeKind: QuotaScopeTask, ScopeID: task.ID, Dimension: "token", LimitUnits: 10, Actor: actor, Reason: "test setup",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateQuotaAccount(ctx, CreateQuotaAccountRequest{
		ScopeKind: QuotaScopeActor, ScopeID: actor, Dimension: "token", LimitUnits: 10, Actor: actor, Reason: "test setup",
	}); err != nil {
		t.Fatal(err)
	}
	lease, err := s.ReserveQuota(ctx, QuotaLeaseRequest{
		IdempotencyKey: "reserve-token", Owner: "worker-a", ScopeKind: QuotaScopeTask, ScopeID: task.ID,
		Dimension: "token", Units: 6, ReclaimPolicy: QuotaReclaimUnused, TTL: time.Hour, Actor: actor, Reason: "generation",
	})
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := s.ReserveQuota(ctx, QuotaLeaseRequest{
		IdempotencyKey: "reserve-token", Owner: "worker-a", ScopeKind: QuotaScopeTask, ScopeID: task.ID,
		Dimension: "token", Units: 6, ReclaimPolicy: QuotaReclaimUnused, TTL: time.Hour, Actor: actor, Reason: "generation",
	})
	if err != nil || duplicate.ID != lease.ID {
		t.Fatalf("idempotent reservation = %+v, %v", duplicate, err)
	}
	if _, err := s.RecordQuotaUsage(ctx, RecordQuotaUsageRequest{
		OperationKey: "turn-1", LeaseID: lease.ID, FencingToken: lease.FencingToken, Units: 2,
		OccurredAt: time.Now().UTC(), Actor: actor, Reason: "completed turn",
	}); err != nil {
		t.Fatal(err)
	}
	settlement, err := s.SettleQuotaLease(ctx, SettleQuotaLeaseRequest{
		IdempotencyKey: "settle-token", LeaseID: lease.ID, Owner: "worker-a", FencingToken: lease.FencingToken,
		Outcome: QuotaSettlementCompleted, Actor: actor, Reason: "completed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if settlement.Lease.State != DurableQuotaLeaseSettled || settlement.ConsumedUnits != 2 || settlement.ReleasedUnits != 4 {
		t.Fatalf("unexpected settled lease: %+v", settlement)
	}
	account, err := s.GetQuotaAccount(ctx, taskToken.ID)
	if err != nil || account == nil || account.ConsumedUnits != 2 || account.ReservedUnits != 0 || account.AvailableUnits() != 8 {
		t.Fatalf("quota account after settlement = %+v, %v", account, err)
	}

	uncertain, err := s.ReserveQuota(ctx, QuotaLeaseRequest{
		IdempotencyKey: "reserve-uncertain", Owner: "worker-b", ScopeKind: QuotaScopeTask, ScopeID: task.ID,
		Dimension: "token", Units: 3, ReclaimPolicy: QuotaReclaimUnused, TTL: time.Hour, Actor: actor, Reason: "provider call",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SettleQuotaLease(ctx, SettleQuotaLeaseRequest{
		IdempotencyKey: "settle-uncertain", LeaseID: uncertain.ID, Owner: "worker-b", FencingToken: uncertain.FencingToken,
		Outcome: QuotaSettlementUncertain, Actor: actor, Reason: "outcome unknown",
	}); err != nil {
		t.Fatal(err)
	}
	blocked, err := s.ReserveQuota(ctx, QuotaLeaseRequest{
		IdempotencyKey: "reserve-blocked", Owner: "worker-c", ScopeKind: QuotaScopeTask, ScopeID: task.ID,
		Dimension: "token", Units: 6, ReclaimPolicy: QuotaReclaimUnused, TTL: time.Hour, Actor: actor, Reason: "must block",
	})
	if err == nil || blocked.ID != "" || !errors.Is(err, ErrQuotaExhausted) {
		t.Fatalf("unknown outcome did not retain capacity: lease=%+v err=%v", blocked, err)
	}
	if _, err := s.ReconcileQuotaLease(ctx, ReconcileQuotaLeaseRequest{
		IdempotencyKey: "reconcile-uncertain", LeaseID: uncertain.ID, Owner: "worker-b", FencingToken: uncertain.FencingToken,
		Outcome: QuotaSettlementCanceled, Actor: actor, Reason: "provider confirmed no use",
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.CreateQuotaAccount(ctx, CreateQuotaAccountRequest{
		ScopeKind: QuotaScopeTask, ScopeID: task.ID, Dimension: "trial", LimitUnits: 2, Actor: actor, Reason: "test setup",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateQuotaAccount(ctx, CreateQuotaAccountRequest{
		ScopeKind: QuotaScopeActor, ScopeID: actor, Dimension: "trial", LimitUnits: 2, Actor: actor, Reason: "test setup",
	}); err != nil {
		t.Fatal(err)
	}
	admission, err := s.AdmitTaskActorQuota(ctx, AdmitTaskActorQuotaRequest{
		IdempotencyKey: "admit-trial", TaskID: task.ID, Actor: actor, LeaseOwner: "worker-d", LeaseTTL: time.Hour,
		Policy:            QuotaPolicyBinding{PolicyID: "test.quota", PolicyVersion: "1", PolicyFingerprint: "sha256:test-quota-v1"},
		BootstrapAccounts: []QuotaAccountBootstrap{{Dimension: "trial", TaskLimitUnits: 2, ActorLimitUnits: 2}},
		Claims:            []TaskActorQuotaClaim{{Dimension: "trial", Units: 1, ReclaimPolicy: QuotaReclaimUnused}}, Reason: "evaluate",
	})
	if err != nil || !admission.Accepted || len(admission.Leases) != 2 {
		t.Fatalf("task+actor admission = %+v, %v", admission, err)
	}
	replayed, err := s.AdmitTaskActorQuota(ctx, AdmitTaskActorQuotaRequest{
		IdempotencyKey: "admit-trial", TaskID: task.ID, Actor: actor, LeaseOwner: "worker-d", LeaseTTL: time.Hour,
		Policy:            QuotaPolicyBinding{PolicyID: "test.quota", PolicyVersion: "1", PolicyFingerprint: "sha256:test-quota-v1"},
		BootstrapAccounts: []QuotaAccountBootstrap{{Dimension: "trial", TaskLimitUnits: 2, ActorLimitUnits: 2}},
		Claims:            []TaskActorQuotaClaim{{Dimension: "trial", Units: 1, ReclaimPolicy: QuotaReclaimUnused}}, Reason: "evaluate",
	})
	if err != nil || replayed.ID != admission.ID || len(replayed.Leases) != 2 {
		t.Fatalf("idempotent admission = %+v, %v", replayed, err)
	}
}

func TestQuotaUsageAndHeartbeatPersistObservedExpiration(t *testing.T) {
	ctx := context.Background()
	s := tempV5DB(t)
	task, _ := createValidatedTaskAndRevision(t, s)
	clock := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	if _, err := s.CreateQuotaAccount(ctx, CreateQuotaAccountRequest{
		ScopeKind: QuotaScopeTask, ScopeID: task.ID, Dimension: "expired", LimitUnits: 4, Actor: "tester",
	}); err != nil {
		t.Fatal(err)
	}
	reserve := func(key string) DurableQuotaLease {
		lease, err := s.ReserveQuota(ctx, QuotaLeaseRequest{
			IdempotencyKey: key, Owner: "worker", ScopeKind: QuotaScopeTask, ScopeID: task.ID,
			Dimension: "expired", Units: 1, ReclaimPolicy: QuotaReclaimUnused, TTL: time.Minute, Actor: "tester",
		})
		if err != nil {
			t.Fatal(err)
		}
		return lease
	}
	usageLease := reserve("expired-usage")
	clock = clock.Add(time.Minute)
	if _, err := s.RecordQuotaUsage(ctx, RecordQuotaUsageRequest{
		OperationKey: "expired-usage-event", LeaseID: usageLease.ID, FencingToken: usageLease.FencingToken,
		Units: 1, OccurredAt: clock, Actor: "tester",
	}); !errors.Is(err, ErrQuotaLeaseExpired) {
		t.Fatalf("expired usage error = %v, want quota lease expired", err)
	}
	persistedUsage, err := s.GetDurableQuotaLease(ctx, usageLease.ID)
	if err != nil || persistedUsage == nil || persistedUsage.State != DurableQuotaLeaseExpired {
		t.Fatalf("usage-observed expiration = %+v, %v", persistedUsage, err)
	}
	heartbeatLease := reserve("expired-quota-heartbeat")
	clock = clock.Add(time.Minute)
	if _, err := s.HeartbeatQuotaLease(ctx, HeartbeatQuotaLeaseRequest{
		IdempotencyKey: "expired-quota-heartbeat-event", LeaseID: heartbeatLease.ID, Owner: heartbeatLease.Owner,
		FencingToken: heartbeatLease.FencingToken, TTL: time.Minute, Actor: "tester",
	}); !errors.Is(err, ErrQuotaLeaseExpired) {
		t.Fatalf("expired quota heartbeat error = %v, want quota lease expired", err)
	}
	persistedHeartbeat, err := s.GetDurableQuotaLease(ctx, heartbeatLease.ID)
	if err != nil || persistedHeartbeat == nil || persistedHeartbeat.State != DurableQuotaLeaseExpired {
		t.Fatalf("heartbeat-observed expiration = %+v, %v", persistedHeartbeat, err)
	}
}

func TestV5QuotaBatchOperationsRollbackWholeStage(t *testing.T) {
	setup := func(t *testing.T) (*Store, TaskV2, []DurableQuotaLease) {
		t.Helper()
		ctx := context.Background()
		s := tempV5DB(t)
		task, _ := createValidatedTaskAndRevision(t, s)
		decision, err := s.AdmitTaskActorQuota(ctx, AdmitTaskActorQuotaRequest{
			IdempotencyKey: "batch-admission-" + t.Name(), TaskID: task.ID, Actor: "batch-actor", LeaseOwner: "batch-worker", LeaseTTL: time.Hour,
			Policy:            QuotaPolicyBinding{PolicyID: "batch", PolicyVersion: "1", PolicyFingerprint: "sha256:batch"},
			BootstrapAccounts: []QuotaAccountBootstrap{{Dimension: "turn", TaskLimitUnits: 5, ActorLimitUnits: 5}},
			Claims:            []TaskActorQuotaClaim{{Dimension: "turn", Units: 2, ReclaimPolicy: QuotaReclaimUnused}}, Reason: "batch fixture",
		})
		if err != nil || !decision.Accepted || len(decision.Leases) != 2 {
			t.Fatalf("batch admission = %+v, %v", decision, err)
		}
		return s, task, decision.Leases
	}
	newID := func(t *testing.T) string {
		t.Helper()
		id, err := NewUUIDv7()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	assertActiveUnchanged := func(t *testing.T, s *Store, originals []DurableQuotaLease) {
		t.Helper()
		for _, original := range originals {
			lease, err := s.GetDurableQuotaLease(context.Background(), original.ID)
			if err != nil || lease == nil || lease.State != DurableQuotaLeaseActive || lease.Version != original.Version ||
				lease.ConsumedUnits != original.ConsumedUnits || lease.ReleasedUnits != original.ReleasedUnits || !lease.ExpiresAt.Equal(original.ExpiresAt) {
				t.Fatalf("quota batch left partial lease mutation: original=%+v current=%+v err=%v", original, lease, err)
			}
		}
	}

	t.Run("usage", func(t *testing.T) {
		s, task, leases := setup(t)
		_, err := s.RecordQuotaUsages(context.Background(), []RecordQuotaUsageRequest{
			{ID: newID(t), OperationKey: "batch-usage-task", LeaseID: leases[0].ID, FencingToken: leases[0].FencingToken, Units: 1, OccurredAt: time.Now().UTC(), Actor: "batch-actor"},
			{ID: task.ID, OperationKey: "batch-usage-actor", LeaseID: leases[1].ID, FencingToken: leases[1].FencingToken, Units: 1, OccurredAt: time.Now().UTC(), Actor: "batch-actor"},
		})
		if !errors.Is(err, ErrIdentityCollision) {
			t.Fatalf("atomic usage error = %v, want identity collision", err)
		}
		assertActiveUnchanged(t, s, leases)
		var events int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM quota_usage_events_v5 WHERE operation_key LIKE 'batch-usage-%'`).Scan(&events); err != nil || events != 0 {
			t.Fatalf("partial usage events = %d, %v", events, err)
		}
	})

	t.Run("heartbeat", func(t *testing.T) {
		s, task, leases := setup(t)
		_, err := s.HeartbeatQuotaLeases(context.Background(), []HeartbeatQuotaLeaseRequest{
			{ID: newID(t), IdempotencyKey: "batch-heartbeat-task", LeaseID: leases[0].ID, Owner: leases[0].Owner, FencingToken: leases[0].FencingToken, TTL: 2 * time.Hour, Actor: "batch-actor"},
			{ID: task.ID, IdempotencyKey: "batch-heartbeat-actor", LeaseID: leases[1].ID, Owner: leases[1].Owner, FencingToken: leases[1].FencingToken, TTL: 2 * time.Hour, Actor: "batch-actor"},
		})
		if !errors.Is(err, ErrIdentityCollision) {
			t.Fatalf("atomic heartbeat error = %v, want identity collision", err)
		}
		assertActiveUnchanged(t, s, leases)
		var events int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM quota_heartbeats_v5 WHERE idempotency_key LIKE 'batch-heartbeat-%'`).Scan(&events); err != nil || events != 0 {
			t.Fatalf("partial heartbeat events = %d, %v", events, err)
		}
	})

	t.Run("settlement", func(t *testing.T) {
		s, task, leases := setup(t)
		_, err := s.SettleQuotaLeases(context.Background(), []SettleQuotaLeaseRequest{
			{ID: newID(t), IdempotencyKey: "batch-settlement-task", LeaseID: leases[0].ID, Owner: leases[0].Owner, FencingToken: leases[0].FencingToken, Outcome: QuotaSettlementCompleted, Actor: "batch-actor"},
			{ID: task.ID, IdempotencyKey: "batch-settlement-actor", LeaseID: leases[1].ID, Owner: leases[1].Owner, FencingToken: leases[1].FencingToken, Outcome: QuotaSettlementCompleted, Actor: "batch-actor"},
		})
		if !errors.Is(err, ErrIdentityCollision) {
			t.Fatalf("atomic settlement error = %v, want identity collision", err)
		}
		assertActiveUnchanged(t, s, leases)
		var events int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM quota_settlements_v5 WHERE idempotency_key LIKE 'batch-settlement-%'`).Scan(&events); err != nil || events != 0 {
			t.Fatalf("partial settlement events = %d, %v", events, err)
		}
	})

	t.Run("reconciliation", func(t *testing.T) {
		s, task, leases := setup(t)
		uncertainRequests := make([]SettleQuotaLeaseRequest, len(leases))
		for index, lease := range leases {
			uncertainRequests[index] = SettleQuotaLeaseRequest{
				ID: newID(t), IdempotencyKey: fmt.Sprintf("batch-uncertain-%d", index), LeaseID: lease.ID,
				Owner: lease.Owner, FencingToken: lease.FencingToken, Outcome: QuotaSettlementUncertain, Actor: "batch-actor",
			}
		}
		uncertain, err := s.SettleQuotaLeases(context.Background(), uncertainRequests)
		if err != nil || len(uncertain) != 2 {
			t.Fatalf("prepare uncertain batch = %+v, %v", uncertain, err)
		}
		_, err = s.ReconcileQuotaLeases(context.Background(), []ReconcileQuotaLeaseRequest{
			{ID: newID(t), IdempotencyKey: "batch-reconcile-task", LeaseID: leases[0].ID, Owner: leases[0].Owner, FencingToken: leases[0].FencingToken, Outcome: QuotaSettlementCanceled, Actor: "batch-actor"},
			{ID: task.ID, IdempotencyKey: "batch-reconcile-actor", LeaseID: leases[1].ID, Owner: leases[1].Owner, FencingToken: leases[1].FencingToken, Outcome: QuotaSettlementCanceled, Actor: "batch-actor"},
		})
		if !errors.Is(err, ErrIdentityCollision) {
			t.Fatalf("atomic reconciliation error = %v, want identity collision", err)
		}
		for _, original := range uncertain {
			lease, err := s.GetDurableQuotaLease(context.Background(), original.LeaseID)
			if err != nil || lease == nil || lease.State != DurableQuotaLeaseUncertain || lease.Version != original.Lease.Version {
				t.Fatalf("reconciliation left partial lease mutation: original=%+v current=%+v err=%v", original.Lease, lease, err)
			}
		}
		var events int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM quota_settlements_v5 WHERE idempotency_key LIKE 'batch-reconcile-%'`).Scan(&events); err != nil || events != 0 {
			t.Fatalf("partial reconciliation events = %d, %v", events, err)
		}
	})
}

func TestQuotaHeartbeatSurvivesConcurrentSQLiteWriters(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	first, err := OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	task, _ := createValidatedTaskAndRevision(t, first)
	decision, err := first.AdmitTaskActorQuota(ctx, AdmitTaskActorQuotaRequest{
		IdempotencyKey: "concurrent-heartbeat-admission", TaskID: task.ID, Actor: "concurrent-actor", LeaseOwner: "concurrent-worker", LeaseTTL: 5 * time.Second,
		Policy:            QuotaPolicyBinding{PolicyID: "concurrent", PolicyVersion: "1", PolicyFingerprint: "sha256:concurrent"},
		BootstrapAccounts: []QuotaAccountBootstrap{{Dimension: "turn", TaskLimitUnits: 10, ActorLimitUnits: 10}},
		Claims:            []TaskActorQuotaClaim{{Dimension: "turn", Units: 1, ReclaimPolicy: QuotaReclaimUnused}}, Reason: "concurrency fixture",
	})
	if err != nil || !decision.Accepted || len(decision.Leases) != 2 {
		t.Fatalf("concurrent admission = %+v, %v", decision, err)
	}
	errorsCh := make(chan error, 80)
	var group sync.WaitGroup
	group.Add(2)
	go func() {
		defer group.Done()
		for ordinal := 0; ordinal < 40; ordinal++ {
			requests := make([]HeartbeatQuotaLeaseRequest, len(decision.Leases))
			for index, lease := range decision.Leases {
				requests[index] = HeartbeatQuotaLeaseRequest{
					IdempotencyKey: fmt.Sprintf("concurrent-heartbeat:%d:%s", ordinal, lease.ID), LeaseID: lease.ID,
					Owner: lease.Owner, FencingToken: lease.FencingToken, TTL: 5 * time.Second, Actor: "concurrent-actor",
				}
			}
			if _, err := first.HeartbeatQuotaLeases(context.Background(), requests); err != nil {
				errorsCh <- err
				return
			}
		}
	}()
	go func() {
		defer group.Done()
		for ordinal := 0; ordinal < 40; ordinal++ {
			key := fmt.Sprintf("concurrent-outbox:%d", ordinal)
			if _, err := second.CreateOutboxEvent(context.Background(), CreateOutboxEventRequest{
				Topic: "concurrent.fixture", EntityType: "workflow_run", EntityID: task.ID, PayloadJSON: `{}`,
				IdempotencyKey: key, Actor: "concurrent-actor",
			}); err != nil {
				errorsCh <- err
				return
			}
		}
	}()
	group.Wait()
	close(errorsCh)
	for err := range errorsCh {
		t.Fatalf("concurrent SQLite writer error: %v", err)
	}
	for _, original := range decision.Leases {
		lease, err := first.GetDurableQuotaLease(ctx, original.ID)
		if err != nil || lease == nil || lease.State != DurableQuotaLeaseActive || lease.Version != original.Version+40 {
			t.Fatalf("concurrent heartbeat lease = %+v, %v", lease, err)
		}
	}
}

func TestV5ExecutionControlAndReconciliationFacts(t *testing.T) {
	ctx := context.Background()
	s := tempV5DB(t)
	task, revision := createValidatedTaskAndRevision(t, s)
	run, err := s.CreateWorkflowRun(ctx, CreateWorkflowRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, WorkflowTemplateID: "harbor.standard", WorkflowTemplateVersion: "v5",
		ResolvedProfileHash: "profile", DefinitionHash: "workflow-fingerprint", RunManifestJSON: `{}`, Trigger: "create", Actor: "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err = s.TransitionWorkflowRun(ctx, TransitionWorkflowRunRequest{RunID: run.ID, ExpectedVersion: run.Version, Status: WorkflowRunRunning, Actor: "tester", Reason: "start"})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := ControlCheckpointRef{Sequence: uint64(run.Version), ExecutionEpoch: run.ExecutionEpoch, SubjectVersion: task.Version,
		SubjectID: task.ID, SubjectRevisionID: revision.ID, SubjectDigest: revision.TaskDigest, WorkflowFingerprint: run.DefinitionHash}
	operation, err := s.CreateExecutionControlOperation(ctx, ExecutionControlCommand{
		OperationKey: "pause-run", Action: ControlActionPause, RunID: run.ID, Expected: checkpoint, Actor: "tester", Reason: "safe pause", GracePeriod: time.Second,
	})
	if err != nil || operation.Status != ControlOperationRequested {
		t.Fatalf("create control operation = %+v, %v", operation, err)
	}
	updatedRun, err := s.GetWorkflowRun(ctx, run.ID)
	if err != nil || updatedRun == nil || updatedRun.Status != WorkflowRunPauseRequested {
		t.Fatalf("control did not atomically request run pause: %+v, %v", updatedRun, err)
	}
	duplicate, err := s.CreateExecutionControlOperation(ctx, ExecutionControlCommand{
		OperationKey: "pause-run", Action: ControlActionPause, RunID: run.ID, Expected: checkpoint, Actor: "tester", Reason: "safe pause", GracePeriod: time.Second,
	})
	if err != nil || duplicate.ID != operation.ID {
		t.Fatalf("idempotent control command = %+v, %v", duplicate, err)
	}
	receiptID, err := NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	transitionID, err := NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	operation, err = s.TransitionExecutionControlOperation(ctx, TransitionControlOperationRequest{
		ID: transitionID, OperationID: operation.ID, ExpectedVersion: operation.Version, Status: ControlOperationPropagating,
		RuntimeReceipts: []RuntimeTerminationReceipt{{ID: receiptID, RuntimeScopeID: "stage-scope", ObservedAt: time.Now().UTC(), Graceful: true, PayloadJSON: `{"checkpoint":"saved"}`}},
		Actor:           "tester", Reason: "runtime acknowledged",
	})
	if err != nil || operation.Status != ControlOperationPropagating || len(operation.RuntimeReceipts) != 1 {
		t.Fatalf("propagate control = %+v, %v", operation, err)
	}
	ackID, err := NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	operation, err = s.TransitionExecutionControlOperation(ctx, TransitionControlOperationRequest{
		ID: ackID, OperationID: operation.ID, ExpectedVersion: operation.Version, Status: ControlOperationAcknowledged,
		CheckpointID: "checkpoint-1", QuotaSettlementID: "settlement-1", Actor: "tester", Reason: "settled",
	})
	if err != nil || operation.Status != ControlOperationAcknowledged || operation.CheckpointID != "checkpoint-1" {
		t.Fatalf("acknowledge control = %+v, %v", operation, err)
	}

	sideEffect, err := s.CreateSideEffectOperation(ctx, CreateSideEffectOperationRequest{
		OperationKey: "local-package", IdempotencyKey: "local-package-idempotency", RunID: run.ID, EffectKind: "local_package",
		SourceDigest: "sha256:source", PayloadJSON: `{}`, Actor: "tester", Reason: "package",
	})
	if err != nil {
		t.Fatal(err)
	}
	sideEffect, err = s.TransitionSideEffectOperation(ctx, TransitionSideEffectOperationRequest{OperationID: sideEffect.ID, ExpectedVersion: sideEffect.Version, State: SideEffectStarted, Actor: "tester", Reason: "write package"})
	if err != nil {
		t.Fatal(err)
	}
	sideEffect, err = s.TransitionSideEffectOperation(ctx, TransitionSideEffectOperationRequest{OperationID: sideEffect.ID, ExpectedVersion: sideEffect.Version, State: SideEffectUnknown, Actor: "tester", Reason: "process lost"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := s.StartReconciliationAttempt(ctx, StartReconciliationAttemptRequest{
		OperationKey: "reconcile-package", SubjectType: "run", SubjectID: run.ID, SideEffectOperationID: sideEffect.ID,
		Ordinal: 1, ObservedJSON: `{"receipt":"found"}`, Actor: "tester", Reason: "inspect local package",
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err = s.CompleteReconciliationAttempt(ctx, CompleteReconciliationAttemptRequest{
		AttemptID: attempt.ID, ExpectedVersion: attempt.Version, State: ReconciliationCompleted, ObservedJSON: `{"receipt":"found"}`,
		Resolution: "package receipt verified", Actor: "tester", Reason: "resolved",
	})
	if err != nil || attempt.State != ReconciliationCompleted || attempt.FinishedAt == nil {
		t.Fatalf("complete reconciliation = %+v, %v", attempt, err)
	}
}

func TestV5DurableDispatcherClaimsAndRecoversExpiredWorkers(t *testing.T) {
	ctx := context.Background()
	s := tempV5DB(t)
	clock := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	pool, err := s.ConfigureCapacityPool(ctx, ConfigureCapacityPoolRequest{PoolKey: "local-workers", Capacity: 1, Actor: "tester", Reason: "test setup"})
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.CreateDurableJob(ctx, CreateDurableJobRequest{CommandType: "execute", EntityType: "test", EntityID: "one", IdempotencyKey: "job-one", Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := s.ClaimNextDurableJob(ctx, ClaimNextDurableJobRequest{IdempotencyKey: "claim-one", Owner: "worker-one", LeaseTTL: time.Minute, CapacityPoolKey: pool.PoolKey, Actor: "tester"})
	if err != nil || claim.Job == nil || claim.Job.ID != job.ID || claim.DispatchLease == nil || claim.CapacityLease == nil {
		t.Fatalf("claim queued job = %+v, %v", claim, err)
	}
	completed, err := s.TransitionDurableJob(ctx, TransitionDurableJobRequest{JobID: claim.Job.ID, ExpectedVersion: claim.Job.Version, State: JobSucceeded, Actor: "tester", Reason: "done"})
	if err != nil || completed.State != JobSucceeded {
		t.Fatalf("complete claimed job = %+v, %v", completed, err)
	}
	capacityLease, err := s.GetLease(ctx, claim.CapacityLease.ID)
	if err != nil || capacityLease == nil || capacityLease.State != LeaseReleased {
		t.Fatalf("terminal job did not release capacity lease: %+v, %v", capacityLease, err)
	}

	job, err = s.CreateDurableJob(ctx, CreateDurableJobRequest{CommandType: "execute", EntityType: "test", EntityID: "two", IdempotencyKey: "job-two", Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	expiring, err := s.ClaimNextDurableJob(ctx, ClaimNextDurableJobRequest{IdempotencyKey: "claim-two", Owner: "worker-two", LeaseTTL: time.Second, CapacityPoolKey: pool.PoolKey, Actor: "tester"})
	if err != nil || expiring.Job == nil || expiring.Job.ID != job.ID {
		t.Fatalf("claim expiring job = %+v, %v", expiring, err)
	}
	clock = clock.Add(2 * time.Second)
	recoveries, err := s.ScanExpiredDurableJobsForReconcile(ctx, ScanExpiredDurableJobsRequest{Actor: "recovery", Reason: "worker lease expired"})
	if err != nil || len(recoveries) != 1 || recoveries[0].Job.ID != job.ID || recoveries[0].Job.State != JobInDoubt || recoveries[0].Job.Failure == nil || recoveries[0].Job.Failure.Code != "job.lease_lost" || recoveries[0].Job.FinishedAt == nil {
		t.Fatalf("expired dispatch recovery = %+v, %v", recoveries, err)
	}
	var failureDetails map[string]string
	if err := json.Unmarshal([]byte(recoveries[0].Job.Failure.DetailsJSON), &failureDetails); err != nil || failureDetails["job_id"] != job.ID || failureDetails["check"] != "dispatch_lease" {
		t.Fatalf("expired dispatch failure details = %q, %v", recoveries[0].Job.Failure.DetailsJSON, err)
	}
	persisted, err := s.GetDurableJob(ctx, job.ID)
	if err != nil || persisted == nil || persisted.State != JobInDoubt || persisted.Failure == nil || *persisted.Failure != *recoveries[0].Job.Failure {
		t.Fatalf("persisted expired dispatch recovery = %+v, %v", persisted, err)
	}
	events, err := s.ListAuditEvents(ctx, ListAuditEventsRequest{EntityType: "job", EntityID: job.ID})
	if err != nil {
		t.Fatal(err)
	}
	foundRecoveryAudit := false
	for _, event := range events {
		if event.Action != "job.transitioned" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(event.PayloadJSON), &payload); err != nil {
			t.Fatal(err)
		}
		if payload["state"] == string(JobInDoubt) {
			foundRecoveryAudit = payload["failure_code"] == "job.lease_lost" && payload["cause"] == "expired_dispatch_lease"
		}
	}
	if !foundRecoveryAudit {
		t.Fatalf("missing durable lease-loss recovery audit: %+v", events)
	}
	if followUp, err := s.ClaimNextDurableJob(ctx, ClaimNextDurableJobRequest{IdempotencyKey: "claim-after-lease-loss", Owner: "worker-three", LeaseTTL: time.Minute, CapacityPoolKey: pool.PoolKey, Actor: "tester"}); err != nil || followUp.State != "empty" || followUp.Job != nil {
		t.Fatalf("lease-lost job was ordinarily retried: %+v, %v", followUp, err)
	}
	capacityLease, err = s.GetLease(ctx, expiring.CapacityLease.ID)
	if err != nil || capacityLease == nil || capacityLease.State != LeaseReleased {
		t.Fatalf("recovery did not release capacity lease: %+v, %v", capacityLease, err)
	}
}

func TestV5ScopedDurableClaimFencesSelectionAndEmptyReplay(t *testing.T) {
	ctx := context.Background()
	s := tempV5DB(t)
	task, revision := createValidatedTaskAndRevision(t, s)
	runA := createV5ReconcileRun(t, ctx, s, task, revision, "scoped-dispatch-a")
	runB := createV5ReconcileRun(t, ctx, s, task, revision, "scoped-dispatch-b")

	jobA, err := s.CreateDurableJob(ctx, CreateDurableJobRequest{
		CommandType: "execute", EntityType: "fixture", EntityID: "scoped-dispatch-a", RunID: runA.ID,
		Priority: 1, PayloadJSON: `{}`, IdempotencyKey: "scoped-dispatch-job-a", Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	jobB, err := s.CreateDurableJob(ctx, CreateDurableJobRequest{
		CommandType: "execute", EntityType: "fixture", EntityID: "scoped-dispatch-b", RunID: runB.ID,
		Priority: 100, PayloadJSON: `{}`, IdempotencyKey: "scoped-dispatch-job-b", Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}

	request := ClaimNextDurableJobRequest{
		IdempotencyKey: "scoped-dispatch-claim-a", Owner: "worker-a", RunID: runA.ID,
		LeaseTTL: time.Minute, Actor: "tester", Reason: "claim only Run A",
	}
	claim, err := s.ClaimNextDurableJob(ctx, request)
	if err != nil || claim.Job == nil || claim.Job.ID != jobA.ID || claim.RunID != runA.ID {
		t.Fatalf("scoped claim = %+v, %v; want Run A job %s", claim, err, jobA.ID)
	}
	other, err := s.GetDurableJob(ctx, jobB.ID)
	if err != nil || other == nil || other.State != JobQueued {
		t.Fatalf("Run B job after Run A scoped claim = %+v, %v; want queued", other, err)
	}

	replayed, err := s.ClaimNextDurableJob(ctx, request)
	if err != nil || replayed.ID != claim.ID || replayed.RunID != runA.ID || replayed.Job == nil || replayed.Job.ID != jobA.ID {
		t.Fatalf("scoped active claim replay = %+v, %v; want %+v", replayed, err, claim)
	}
	conflict := request
	conflict.RunID = runB.ID
	if _, err := s.ClaimNextDurableJob(ctx, conflict); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("claim replay under another Run = %v, want ErrIdempotencyConflict", err)
	}

	emptyRequest := ClaimNextDurableJobRequest{
		IdempotencyKey: "scoped-dispatch-empty-a", Owner: "worker-a", RunID: runA.ID,
		LeaseTTL: time.Minute, Actor: "tester", Reason: "prove empty claim scope",
	}
	empty, err := s.ClaimNextDurableJob(ctx, emptyRequest)
	if err != nil || empty.State != "empty" || empty.Job != nil || empty.RunID != runA.ID {
		t.Fatalf("scoped empty claim = %+v, %v", empty, err)
	}
	emptyReplay, err := s.ClaimNextDurableJob(ctx, emptyRequest)
	if err != nil || emptyReplay.ID != empty.ID || emptyReplay.State != "empty" || emptyReplay.RunID != runA.ID {
		t.Fatalf("scoped empty replay = %+v, %v; want %+v", emptyReplay, err, empty)
	}
	emptyConflict := emptyRequest
	emptyConflict.RunID = runB.ID
	if _, err := s.ClaimNextDurableJob(ctx, emptyConflict); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("empty claim replay under another Run = %v, want ErrIdempotencyConflict", err)
	}
}

func TestV5ScopedDurableClaimFiltersCommandTypesBeforePriority(t *testing.T) {
	ctx := context.Background()
	s := tempV5DB(t)
	task, revision := createValidatedTaskAndRevision(t, s)
	run := createV5ReconcileRun(t, ctx, s, task, revision, "command-filter")
	stale, err := s.CreateDurableJob(ctx, CreateDurableJobRequest{
		CommandType: "stage_attempt.execute", EntityType: "fixture", EntityID: "stale", RunID: run.ID,
		Priority: 100, PayloadJSON: `{}`, IdempotencyKey: "command-filter-stale", Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	allowed, err := s.CreateDurableJob(ctx, CreateDurableJobRequest{
		CommandType: "repair_session.advance", EntityType: "fixture", EntityID: "allowed", RunID: run.ID,
		Priority: 1, PayloadJSON: `{}`, IdempotencyKey: "command-filter-allowed", Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := s.ClaimNextDurableJob(ctx, ClaimNextDurableJobRequest{
		IdempotencyKey: "command-filter-claim", Owner: "worker", RunID: run.ID, CommandTypes: []string{"repair_session.advance"},
		LeaseTTL: time.Minute, Actor: "tester", Reason: "claim only eligible command",
	})
	if err != nil || claim.Job == nil || claim.Job.ID != allowed.ID {
		t.Fatalf("filtered durable claim = %+v, %v; want %s", claim, err, allowed.ID)
	}
	storedStale, err := s.GetDurableJob(ctx, stale.ID)
	if err != nil || storedStale == nil || storedStale.State != JobQueued {
		t.Fatalf("filtered claim changed stale job = %+v, %v", storedStale, err)
	}
}

func TestV5ScopedExpiredJobReconciliationLeavesOtherRunUntouched(t *testing.T) {
	ctx := context.Background()
	s := tempV5DB(t)
	clock := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	task, revision := createValidatedTaskAndRevision(t, s)
	runA := createV5ReconcileRun(t, ctx, s, task, revision, "scoped-reconcile-a")
	runB := createV5ReconcileRun(t, ctx, s, task, revision, "scoped-reconcile-b")

	jobA, err := s.CreateDurableJob(ctx, CreateDurableJobRequest{
		CommandType: "execute", EntityType: "fixture", EntityID: "scoped-a", RunID: runA.ID,
		PayloadJSON: `{}`, IdempotencyKey: "scoped-reconcile-job-a", Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimA, err := s.ClaimNextDurableJob(ctx, ClaimNextDurableJobRequest{
		IdempotencyKey: "scoped-reconcile-claim-a", Owner: "worker-a", LeaseTTL: time.Second, Actor: "tester", Reason: "fixture",
	})
	if err != nil || claimA.Job == nil || claimA.DispatchLease == nil || claimA.Job.ID != jobA.ID {
		t.Fatalf("claim Run A job = %+v, %v", claimA, err)
	}

	jobB, err := s.CreateDurableJob(ctx, CreateDurableJobRequest{
		CommandType: "execute", EntityType: "fixture", EntityID: "scoped-b", RunID: runB.ID,
		PayloadJSON: `{}`, IdempotencyKey: "scoped-reconcile-job-b", Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimB, err := s.ClaimNextDurableJob(ctx, ClaimNextDurableJobRequest{
		IdempotencyKey: "scoped-reconcile-claim-b", Owner: "worker-b", LeaseTTL: time.Second, Actor: "tester", Reason: "fixture",
	})
	if err != nil || claimB.Job == nil || claimB.DispatchLease == nil || claimB.Job.ID != jobB.ID {
		t.Fatalf("claim Run B job = %+v, %v", claimB, err)
	}

	clock = clock.Add(2 * time.Second)
	recoveries, err := s.ScanExpiredDurableJobsForReconcile(ctx, ScanExpiredDurableJobsRequest{
		RunID: runA.ID, Actor: "recovery", Reason: "recover only selected Run",
	})
	if err != nil || len(recoveries) != 1 || recoveries[0].Job.ID != jobA.ID || recoveries[0].Job.State != JobInDoubt || recoveries[0].Job.Failure == nil || recoveries[0].Job.Failure.Code != "job.lease_lost" {
		t.Fatalf("scoped recovery = %+v, %v", recoveries, err)
	}
	otherJob, err := s.GetDurableJob(ctx, jobB.ID)
	if err != nil || otherJob == nil || otherJob.State != JobRunning {
		t.Fatalf("unselected Run job changed by scoped recovery: %+v, %v", otherJob, err)
	}
	otherLease, err := s.GetLease(ctx, claimB.DispatchLease.ID)
	if err != nil || otherLease == nil || otherLease.State != LeaseActive {
		t.Fatalf("unselected Run dispatch lease changed by scoped recovery: %+v, %v", otherLease, err)
	}
}

func TestV5ExpiredDispatchRecoveryPreservesExistingFailure(t *testing.T) {
	ctx := context.Background()
	s := tempV5DB(t)
	clock := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	job, err := s.CreateDurableJob(ctx, CreateDurableJobRequest{
		CommandType: "execute", EntityType: "fixture", EntityID: "preserve-failure", PayloadJSON: `{}`,
		IdempotencyKey: "preserve-failure-job", Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := s.ClaimNextDurableJob(ctx, ClaimNextDurableJobRequest{
		IdempotencyKey: "preserve-failure-claim", Owner: "worker", LeaseTTL: time.Minute, Actor: "tester", Reason: "fixture",
	})
	if err != nil || claim.Job == nil || claim.DispatchLease == nil {
		t.Fatalf("claim fixture job = %+v, %v", claim, err)
	}
	originalFailure := &DurableJobFailure{
		Code:        "handoff.definition_unavailable",
		Message:     "The deployment definition is temporarily unavailable.",
		DetailsJSON: `{"check":"definition"}`,
	}
	inDoubt, err := s.TransitionDurableJob(ctx, TransitionDurableJobRequest{
		JobID: claim.Job.ID, ExpectedVersion: claim.Job.Version, State: JobInDoubt, Failure: originalFailure, Actor: "worker", Reason: "known hold",
	})
	if err != nil || inDoubt.Failure == nil {
		t.Fatalf("record original failure = %+v, %v", inDoubt, err)
	}

	// Simulate a historical claim that remained indexed after its terminal job
	// projection. Recovery must clean it up without replacing the diagnosis.
	if _, err := s.db.Exec(`UPDATE leases SET state = 'active', expires_at = ?, version = version + 1 WHERE id = ?`, clock.Add(-time.Second), claim.DispatchLease.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE job_dispatch_claims_v5 SET state = 'active', updated_at = ? WHERE id = ?`, clock, claim.ID); err != nil {
		t.Fatal(err)
	}
	recoveries, err := s.ScanExpiredDurableJobsForReconcile(ctx, ScanExpiredDurableJobsRequest{Actor: "recovery", Reason: "clean historical claim"})
	if err != nil || len(recoveries) != 1 || recoveries[0].Job.State != JobInDoubt || recoveries[0].Job.Failure == nil || *recoveries[0].Job.Failure != *inDoubt.Failure {
		t.Fatalf("historical claim recovery = %+v, %v", recoveries, err)
	}
	persisted, err := s.GetDurableJob(ctx, job.ID)
	if err != nil || persisted == nil || persisted.Failure == nil || *persisted.Failure != *inDoubt.Failure {
		t.Fatalf("historical recovery replaced failure = %+v, %v", persisted, err)
	}
}

func TestV5ExpireLeasesForRunLeavesOtherRunLeaseUntouched(t *testing.T) {
	ctx := context.Background()
	s := tempV5DB(t)
	clock := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	task, revision := createValidatedTaskAndRevision(t, s)
	runA := createV5ReconcileRun(t, ctx, s, task, revision, "scoped-lease-a")
	runB := createV5ReconcileRun(t, ctx, s, task, revision, "scoped-lease-b")

	jobA, err := s.CreateDurableJob(ctx, CreateDurableJobRequest{
		CommandType: "execute", EntityType: "fixture", EntityID: "lease-a", RunID: runA.ID,
		PayloadJSON: `{}`, IdempotencyKey: "scoped-lease-job-a", Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimA, err := s.ClaimNextDurableJob(ctx, ClaimNextDurableJobRequest{
		IdempotencyKey: "scoped-lease-claim-a", Owner: "worker-a", LeaseTTL: time.Second, Actor: "tester", Reason: "fixture",
	})
	if err != nil || claimA.Job == nil || claimA.DispatchLease == nil || claimA.Job.ID != jobA.ID {
		t.Fatalf("claim Run A job = %+v, %v", claimA, err)
	}

	jobB, err := s.CreateDurableJob(ctx, CreateDurableJobRequest{
		CommandType: "execute", EntityType: "fixture", EntityID: "lease-b", RunID: runB.ID,
		PayloadJSON: `{}`, IdempotencyKey: "scoped-lease-job-b", Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimB, err := s.ClaimNextDurableJob(ctx, ClaimNextDurableJobRequest{
		IdempotencyKey: "scoped-lease-claim-b", Owner: "worker-b", LeaseTTL: time.Second, Actor: "tester", Reason: "fixture",
	})
	if err != nil || claimB.Job == nil || claimB.DispatchLease == nil || claimB.Job.ID != jobB.ID {
		t.Fatalf("claim Run B job = %+v, %v", claimB, err)
	}

	clock = clock.Add(2 * time.Second)
	expired, err := s.ExpireLeasesForRun(ctx, runA.ID, "recovery", "expire only selected Run leases")
	if err != nil || expired != 1 {
		t.Fatalf("expire selected Run leases = %d, %v", expired, err)
	}
	selectedLease, err := s.GetLease(ctx, claimA.DispatchLease.ID)
	if err != nil || selectedLease == nil || selectedLease.State != LeaseExpired {
		t.Fatalf("selected Run lease = %+v, %v", selectedLease, err)
	}
	otherLease, err := s.GetLease(ctx, claimB.DispatchLease.ID)
	if err != nil || otherLease == nil || otherLease.State != LeaseActive {
		t.Fatalf("unselected Run lease changed by scoped expiration: %+v, %v", otherLease, err)
	}
}

func TestV5ExpireQuotaLeasesForScopeLeavesOtherScopesUntouched(t *testing.T) {
	ctx := context.Background()
	s := tempV5DB(t)
	clock := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	taskA, _ := createValidatedTaskAndRevision(t, s)
	taskB, _ := createValidatedTaskAndRevision(t, s)
	const actorA = "scope-owner-a"
	const actorB = "scope-owner-b"

	for _, account := range []CreateQuotaAccountRequest{
		{ScopeKind: QuotaScopeTask, ScopeID: taskA.ID, Dimension: "token", LimitUnits: 10, Actor: actorA, Reason: "fixture"},
		{ScopeKind: QuotaScopeTask, ScopeID: taskB.ID, Dimension: "token", LimitUnits: 10, Actor: actorA, Reason: "fixture"},
		{ScopeKind: QuotaScopeActor, ScopeID: actorA, Dimension: "token", LimitUnits: 10, Actor: actorA, Reason: "fixture"},
		{ScopeKind: QuotaScopeActor, ScopeID: actorB, Dimension: "token", LimitUnits: 10, Actor: actorB, Reason: "fixture"},
	} {
		if _, err := s.CreateQuotaAccount(ctx, account); err != nil {
			t.Fatal(err)
		}
	}
	reserve := func(key, owner string, scopeKind QuotaScopeKind, scopeID string) DurableQuotaLease {
		t.Helper()
		lease, err := s.ReserveQuota(ctx, QuotaLeaseRequest{
			IdempotencyKey: key, Owner: owner, ScopeKind: scopeKind, ScopeID: scopeID, Dimension: "token", Units: 1,
			ReclaimPolicy: QuotaReclaimUnused, TTL: time.Second, Actor: owner, Reason: "fixture",
		})
		if err != nil {
			t.Fatal(err)
		}
		return lease
	}
	taskLeaseA := reserve("scoped-task-lease-a", actorA, QuotaScopeTask, taskA.ID)
	taskLeaseB := reserve("scoped-task-lease-b", actorA, QuotaScopeTask, taskB.ID)
	actorLeaseA := reserve("scoped-actor-lease-a", actorA, QuotaScopeActor, actorA)
	actorLeaseB := reserve("scoped-actor-lease-b", actorB, QuotaScopeActor, actorB)

	clock = clock.Add(2 * time.Second)
	expiredTask, err := s.ExpireQuotaLeasesForScope(ctx, QuotaScopeTask, taskA.ID, actorA, "recover selected Task quota")
	if err != nil || expiredTask != 1 {
		t.Fatalf("expire selected Task quota lease = %d, %v", expiredTask, err)
	}
	expiredActor, err := s.ExpireQuotaLeasesForScope(ctx, QuotaScopeActor, actorA, actorA, "recover selected actor quota")
	if err != nil || expiredActor != 1 {
		t.Fatalf("expire selected actor quota lease = %d, %v", expiredActor, err)
	}
	for _, expectation := range []struct {
		name  string
		lease DurableQuotaLease
		state DurableQuotaLeaseState
	}{
		{name: "selected task", lease: taskLeaseA, state: DurableQuotaLeaseExpired},
		{name: "other task", lease: taskLeaseB, state: DurableQuotaLeaseActive},
		{name: "selected actor", lease: actorLeaseA, state: DurableQuotaLeaseExpired},
		{name: "other actor", lease: actorLeaseB, state: DurableQuotaLeaseActive},
	} {
		t.Run(expectation.name, func(t *testing.T) {
			lease, err := s.GetDurableQuotaLease(ctx, expectation.lease.ID)
			if err != nil || lease == nil || lease.State != expectation.state {
				t.Fatalf("quota lease = %+v, %v; want state %s", lease, err, expectation.state)
			}
		})
	}
}

func createV5ReconcileRun(t *testing.T, ctx context.Context, s *Store, task TaskV2, revision TaskRevision, trigger string) WorkflowRun {
	t.Helper()
	run, err := s.CreateWorkflowRun(ctx, CreateWorkflowRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, WorkflowTemplateID: "harbor.standard", WorkflowTemplateVersion: "v5",
		ResolvedProfileHash: "profile", DefinitionHash: "workflow-fingerprint", RunManifestJSON: `{}`,
		Trigger: trigger, Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	return run
}
