package workflowkit

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestInMemoryQuotaReserveChargeSettleAndGrantAreIdempotent(t *testing.T) {
	clock := newServiceClock()
	scope := ResourceScope{Kind: "subject", ID: "subject-1"}
	manager := newQuotaManager(t, clock, QuotaLimit{Scope: scope, Dimension: "token", Limit: 10})
	request := ReserveRequest{
		LeaseID:        "lease-1",
		IdempotencyKey: "reserve-1",
		Owner:          "worker-1",
		Claim:          QuotaRequest{Scope: scope, Dimension: "token", Units: 6, ReclaimPolicy: ReclaimUnused},
		TTL:            time.Hour,
	}
	lease, err := manager.Reserve(context.Background(), request)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if lease.Reserved != 6 || lease.Consumed != 0 || lease.Released != 0 || lease.Status != LeaseActive {
		t.Fatalf("unexpected lease: %#v", lease)
	}
	duplicate, err := manager.Reserve(context.Background(), request)
	if err != nil {
		t.Fatalf("repeat reserve: %v", err)
	}
	if duplicate != lease {
		t.Fatalf("idempotent reserve = %#v, want %#v", duplicate, lease)
	}

	event := UsageEvent{EventID: "usage-1", LeaseID: lease.LeaseID, FencingToken: lease.FencingToken, Units: 2, OccurredAt: clock.Now()}
	snapshot, err := manager.Charge(context.Background(), event)
	if err != nil {
		t.Fatalf("charge: %v", err)
	}
	assertQuotaSnapshot(t, snapshot, 10, 2, 4, 4)
	duplicateSnapshot, err := manager.Charge(context.Background(), event)
	if err != nil {
		t.Fatalf("repeat charge: %v", err)
	}
	assertQuotaSnapshot(t, duplicateSnapshot, 10, 2, 4, 4)

	settlementRequest := SettlementRequest{
		SettlementID:   "settlement-1",
		IdempotencyKey: "settle-1",
		LeaseID:        lease.LeaseID,
		Owner:          lease.Owner,
		FencingToken:   lease.FencingToken,
		Outcome:        SettlementCompleted,
	}
	settlement, err := manager.Settle(context.Background(), settlementRequest)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if settlement.Consumed != 2 || settlement.Released != 4 || settlement.Lease.Status != LeaseSettled {
		t.Fatalf("unexpected settlement: %#v", settlement)
	}
	duplicateSettlement, err := manager.Settle(context.Background(), settlementRequest)
	if err != nil {
		t.Fatalf("repeat settle: %v", err)
	}
	if duplicateSettlement != settlement {
		t.Fatalf("idempotent settlement = %#v, want %#v", duplicateSettlement, settlement)
	}
	snapshot, err = manager.Snapshot(context.Background(), scope, "token")
	if err != nil {
		t.Fatalf("snapshot after settle: %v", err)
	}
	assertQuotaSnapshot(t, snapshot, 10, 2, 0, 8)

	grantRequest := BudgetGrantRequest{
		GrantID:         "grant-1",
		IdempotencyKey:  "grant-key-1",
		Scope:           scope,
		Dimension:       "token",
		Delta:           5,
		ExpectedVersion: snapshot.Version,
		Actor:           "owner-1",
		Reason:          "approved additional work",
	}
	grant, err := manager.Grant(context.Background(), grantRequest)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if grant.Limit != 15 || grant.PreviousVersion != snapshot.Version || grant.Version != snapshot.Version+1 {
		t.Fatalf("unexpected grant: %#v", grant)
	}
	duplicateGrant, err := manager.Grant(context.Background(), grantRequest)
	if err != nil {
		t.Fatalf("repeat grant: %v", err)
	}
	if duplicateGrant != grant {
		t.Fatalf("idempotent grant = %#v, want %#v", duplicateGrant, grant)
	}
	stale := grantRequest
	stale.GrantID = "grant-stale"
	stale.IdempotencyKey = "grant-stale-key"
	if _, err := manager.Grant(context.Background(), stale); !errors.Is(err, ErrStaleQuotaGrant) {
		t.Fatalf("stale grant error = %v, want ErrStaleQuotaGrant", err)
	}
}

func TestInMemoryQuotaExhaustionAndConservativeReconciliation(t *testing.T) {
	clock := newServiceClock()
	scope := ResourceScope{Kind: "execution", ID: "run-1"}
	manager := newQuotaManager(t, clock, QuotaLimit{Scope: scope, Dimension: "trial", Limit: 3})
	lease := reserveQuota(t, manager, "lease-uncertain", "reserve-uncertain", scope, "trial", 2, 2*time.Minute)
	if _, err := manager.Reserve(context.Background(), ReserveRequest{
		LeaseID:        "lease-over",
		IdempotencyKey: "reserve-over",
		Owner:          "worker-1",
		Claim:          QuotaRequest{Scope: scope, Dimension: "trial", Units: 2, ReclaimPolicy: ReclaimUnused},
		TTL:            time.Minute,
	}); !errors.Is(err, ErrQuotaExhausted) {
		t.Fatalf("quota exhaustion error = %v, want ErrQuotaExhausted", err)
	}
	if _, err := manager.Settle(context.Background(), SettlementRequest{
		SettlementID:   "settlement-uncertain",
		IdempotencyKey: "settle-uncertain",
		LeaseID:        lease.LeaseID,
		Owner:          lease.Owner,
		FencingToken:   lease.FencingToken,
		Outcome:        SettlementUncertain,
	}); err != nil {
		t.Fatalf("settle uncertain: %v", err)
	}
	snapshot, err := manager.Snapshot(context.Background(), scope, "trial")
	if err != nil {
		t.Fatalf("snapshot uncertain: %v", err)
	}
	assertQuotaSnapshot(t, snapshot, 3, 0, 2, 1)
	if _, err := manager.Reserve(context.Background(), ReserveRequest{
		LeaseID:        "lease-blocked",
		IdempotencyKey: "reserve-blocked",
		Owner:          "worker-1",
		Claim:          QuotaRequest{Scope: scope, Dimension: "trial", Units: 2, ReclaimPolicy: ReclaimUnused},
		TTL:            time.Minute,
	}); !errors.Is(err, ErrQuotaExhausted) {
		t.Fatalf("unknown outcome unexpectedly released capacity: %v", err)
	}
	if _, err := manager.Reconcile(context.Background(), QuotaReconcileRequest{
		ReconcileID:    "reconcile-uncertain",
		IdempotencyKey: "reconcile-uncertain",
		LeaseID:        lease.LeaseID,
		Owner:          lease.Owner,
		FencingToken:   lease.FencingToken,
		Outcome:        SettlementCanceled,
	}); err != nil {
		t.Fatalf("reconcile uncertain lease: %v", err)
	}
	snapshot, err = manager.Snapshot(context.Background(), scope, "trial")
	if err != nil {
		t.Fatalf("snapshot reconciled: %v", err)
	}
	assertQuotaSnapshot(t, snapshot, 3, 0, 0, 3)

	expiring := reserveQuota(t, manager, "lease-expiring", "reserve-expiring", scope, "trial", 2, time.Minute)
	clock.Advance(time.Minute)
	if _, err := manager.Heartbeat(context.Background(), LeaseHeartbeat{
		HeartbeatID:  "heartbeat-expired",
		LeaseID:      expiring.LeaseID,
		Owner:        expiring.Owner,
		FencingToken: expiring.FencingToken,
		TTL:          time.Minute,
	}); !errors.Is(err, ErrQuotaLeaseExpired) {
		t.Fatalf("expired heartbeat error = %v, want ErrQuotaLeaseExpired", err)
	}
	snapshot, err = manager.Snapshot(context.Background(), scope, "trial")
	if err != nil {
		t.Fatalf("snapshot expired lease: %v", err)
	}
	assertQuotaSnapshot(t, snapshot, 3, 0, 2, 1)
	if _, err := manager.Reconcile(context.Background(), QuotaReconcileRequest{
		ReconcileID:    "reconcile-expired",
		IdempotencyKey: "reconcile-expired",
		LeaseID:        expiring.LeaseID,
		Owner:          expiring.Owner,
		FencingToken:   expiring.FencingToken,
		Outcome:        SettlementCanceled,
	}); err != nil {
		t.Fatalf("reconcile expired lease: %v", err)
	}
}

func TestAdmissionReservesAllScopesAtomicallyAndIsIdempotent(t *testing.T) {
	clock := newServiceClock()
	taskScope := ResourceScope{Kind: "subject", ID: "subject-1"}
	actorScope := ResourceScope{Kind: "actor", ID: "actor-1"}
	capacityScope := ResourceScope{Kind: "worker", ID: "pool-1"}
	manager := newQuotaManager(t, clock,
		QuotaLimit{Scope: taskScope, Dimension: "attempt", Limit: 3},
		QuotaLimit{Scope: actorScope, Dimension: "attempt", Limit: 3},
		QuotaLimit{Scope: capacityScope, Dimension: "slot", Limit: 1},
	)
	admission, err := NewInMemoryAdmissionControl(manager)
	if err != nil {
		t.Fatalf("new admission control: %v", err)
	}
	request := AdmissionRequest{
		AdmissionID:    "admission-1",
		IdempotencyKey: "admission-key-1",
		Owner:          "worker-1",
		LeaseTTL:       time.Hour,
		Claims: []QuotaRequest{
			{Scope: actorScope, Dimension: "attempt", Units: 2, ReclaimPolicy: ReclaimUnused},
			{Scope: taskScope, Dimension: "attempt", Units: 2, ReclaimPolicy: ReclaimUnused},
		},
	}
	decision, err := admission.AdmitAndReserve(context.Background(), request)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if !decision.Accepted || decision.Reason != AdmissionAccepted || len(decision.Leases) != 2 {
		t.Fatalf("unexpected admission decision: %#v", decision)
	}
	reordered := request.Clone()
	reordered.Claims[0], reordered.Claims[1] = reordered.Claims[1], reordered.Claims[0]
	duplicate, err := admission.AdmitAndReserve(context.Background(), reordered)
	if err != nil {
		t.Fatalf("repeat admission: %v", err)
	}
	if !duplicate.Accepted || len(duplicate.Leases) != 2 || duplicate.Leases[0].LeaseID != decision.Leases[0].LeaseID {
		t.Fatalf("idempotent admission result = %#v, want %#v", duplicate, decision)
	}

	deniedRequest := request.Clone()
	deniedRequest.AdmissionID = "admission-2"
	deniedRequest.IdempotencyKey = "admission-key-2"
	denied, err := admission.AdmitAndReserve(context.Background(), deniedRequest)
	if err != nil {
		t.Fatalf("denied admission returned error: %v", err)
	}
	if denied.Accepted || denied.Reason != AdmissionQuotaExhausted || len(denied.Leases) != 0 {
		t.Fatalf("quota exhaustion decision = %#v", denied)
	}
	for _, scope := range []ResourceScope{taskScope, actorScope} {
		snapshot, err := manager.Snapshot(context.Background(), scope, "attempt")
		if err != nil {
			t.Fatalf("snapshot %q: %v", scope.Kind, err)
		}
		assertQuotaSnapshot(t, snapshot, 3, 0, 2, 1)
	}

	permit, err := admission.AdmitDispatch(context.Background(), DispatchRequest{
		DispatchID:     "dispatch-1",
		IdempotencyKey: "dispatch-key-1",
		Owner:          "worker-1",
		LeaseTTL:       time.Minute,
		CapacityClaims: []QuotaRequest{{Scope: capacityScope, Dimension: "slot", Units: 1, ReclaimPolicy: ReclaimUnused}},
	})
	if err != nil || !permit.Accepted {
		t.Fatalf("dispatch permit = %#v, err = %v", permit, err)
	}
	blocked, err := admission.AdmitDispatch(context.Background(), DispatchRequest{
		DispatchID:     "dispatch-2",
		IdempotencyKey: "dispatch-key-2",
		Owner:          "worker-2",
		LeaseTTL:       time.Minute,
		CapacityClaims: []QuotaRequest{{Scope: capacityScope, Dimension: "slot", Units: 1, ReclaimPolicy: ReclaimUnused}},
	})
	if err != nil || blocked.Accepted || blocked.Reason != AdmissionQuotaExhausted {
		t.Fatalf("blocked dispatch = %#v, err = %v", blocked, err)
	}
}

func TestExecutionControlValidatesTransitionsAndIdempotency(t *testing.T) {
	clock := newServiceClock()
	store := NewInMemoryExecutionControl(clock)
	workflow := testWorkflow(t)
	checkpoint := testCheckpoint(mustWorkflowFingerprint(t, workflow))
	command := ExecutionControlCommand{
		OperationKey: "pause-1",
		Action:       ControlPause,
		RunID:        "run-1",
		Expected:     checkpoint,
		Actor:        "actor-1",
		Reason:       "operator requested pause",
		GracePeriod:  30 * time.Second,
	}
	operation, err := store.RequestControl(context.Background(), command)
	if err != nil {
		t.Fatalf("request pause: %v", err)
	}
	if operation.Status != ControlRequested || operation.Version != 1 {
		t.Fatalf("initial operation = %#v", operation)
	}
	duplicate, err := store.RequestControl(context.Background(), command)
	if err != nil || duplicate.OperationID != operation.OperationID || duplicate.Version != operation.Version || duplicate.Status != operation.Status {
		t.Fatalf("idempotent control request = %#v, err = %v", duplicate, err)
	}
	if err := ValidateControlExecutionTransition(StatusRunning, ControlPause); err != nil {
		t.Fatalf("running pause should be legal: %v", err)
	}
	if err := ValidateControlExecutionTransition(StatusCompleted, ControlPause); !errors.Is(err, ErrInvalidControl) {
		t.Fatalf("terminal pause error = %v, want ErrInvalidControl", err)
	}
	if _, err := store.RequestControl(context.Background(), ExecutionControlCommand{
		OperationKey: "bad-stage-cancel",
		Action:       ControlCancelStage,
		RunID:        "run-1",
		Expected:     checkpoint,
		Actor:        "actor-1",
		Reason:       "bad target",
	}); !errors.Is(err, ErrInvalidControl) {
		t.Fatalf("stage cancel without target error = %v, want ErrInvalidControl", err)
	}

	propagating, err := store.TransitionControl(context.Background(), ControlTransition{
		TransitionID:    "transition-propagate",
		OperationID:     operation.OperationID,
		ExpectedVersion: operation.Version,
		Status:          ControlPropagating,
		RuntimeReceipts: []RuntimeTerminationReceipt{{ReceiptID: "receipt-1", RuntimeScopeID: "runtime-1", ObservedAt: clock.Now(), Graceful: true}},
	})
	if err != nil {
		t.Fatalf("propagate control: %v", err)
	}
	if propagating.Status != ControlPropagating || propagating.Version != 2 || len(propagating.RuntimeReceipts) != 1 {
		t.Fatalf("propagating operation = %#v", propagating)
	}
	idempotentTransition, err := store.TransitionControl(context.Background(), ControlTransition{
		TransitionID:    "transition-propagate",
		OperationID:     operation.OperationID,
		ExpectedVersion: operation.Version,
		Status:          ControlPropagating,
		RuntimeReceipts: []RuntimeTerminationReceipt{{ReceiptID: "receipt-1", RuntimeScopeID: "runtime-1", ObservedAt: clock.Now(), Graceful: true}},
	})
	if err != nil || idempotentTransition.OperationID != propagating.OperationID || idempotentTransition.Version != propagating.Version || idempotentTransition.Status != propagating.Status {
		t.Fatalf("idempotent transition = %#v, err = %v", idempotentTransition, err)
	}
	acknowledged, err := store.TransitionControl(context.Background(), ControlTransition{
		TransitionID:      "transition-ack",
		OperationID:       operation.OperationID,
		ExpectedVersion:   propagating.Version,
		Status:            ControlAcknowledged,
		CheckpointID:      "checkpoint-1",
		QuotaSettlementID: "settlement-1",
	})
	if err != nil {
		t.Fatalf("acknowledge control: %v", err)
	}
	if acknowledged.Status != ControlAcknowledged || acknowledged.Version != 3 {
		t.Fatalf("acknowledged operation = %#v", acknowledged)
	}
	if _, err := store.TransitionControl(context.Background(), ControlTransition{
		TransitionID:    "transition-stale",
		OperationID:     operation.OperationID,
		ExpectedVersion: propagating.Version,
		Status:          ControlFailed,
		FailureReason:   "stale writer",
	}); !errors.Is(err, ErrInvalidControl) {
		t.Fatalf("stale control transition error = %v, want ErrInvalidControl", err)
	}
}

func TestRecoveryPolicyIsConservative(t *testing.T) {
	now := time.Date(2026, time.July, 13, 10, 0, 0, 0, time.UTC)
	decision, err := DecideRecovery(RecoverySubject{
		SubjectID:             "paused-reusable",
		Status:                StatusPaused,
		ObservedAt:            now,
		CheckpointRecoverable: true,
		InputsUnchanged:       true,
		DefinitionUnchanged:   true,
	})
	if err != nil || decision.Action != RecoveryResumeCheckpoint {
		t.Fatalf("reusable paused recovery = %#v, err = %v", decision, err)
	}
	decision, err = DecideRecovery(RecoverySubject{
		SubjectID:             "paused-new-attempt",
		Status:                StatusPaused,
		ObservedAt:            now,
		CheckpointRecoverable: false,
		InputsUnchanged:       true,
		DefinitionUnchanged:   true,
	})
	if err != nil || decision.Action != RecoveryScheduleNewAttempt {
		t.Fatalf("unrecoverable paused recovery = %#v, err = %v", decision, err)
	}
	decision, err = DecideRecovery(RecoverySubject{
		SubjectID:      "lost-worker",
		Status:         StatusRunning,
		ObservedAt:     now,
		LeaseExpiresAt: now,
	})
	if err != nil || decision.Action != RecoveryMarkInterrupted {
		t.Fatalf("expired running recovery = %#v, err = %v", decision, err)
	}
	decision, err = DecideRecovery(RecoverySubject{
		SubjectID:      "active-worker",
		Status:         StatusRunning,
		ObservedAt:     now,
		LeaseExpiresAt: now.Add(time.Minute),
	})
	if err != nil || decision.Action != RecoveryNoAction {
		t.Fatalf("active running recovery = %#v, err = %v", decision, err)
	}
	decision, err = DecideRecovery(RecoverySubject{
		SubjectID:              "unknown-effect",
		Status:                 StatusRunning,
		ObservedAt:             now,
		LeaseExpiresAt:         now,
		UnknownExternalOutcome: true,
	})
	if err != nil || decision.Action != RecoveryReconcile {
		t.Fatalf("unknown effect recovery = %#v, err = %v", decision, err)
	}
	decision, err = DecideRecovery(RecoverySubject{
		SubjectID:      "terminate-intent",
		Status:         StatusRunning,
		ObservedAt:     now,
		LeaseExpiresAt: now,
		ControlAction:  ControlTerminate,
		ControlStatus:  ControlPropagating,
	})
	if err != nil || decision.Action != RecoveryAwaitControlOutcome {
		t.Fatalf("terminate recovery = %#v, err = %v", decision, err)
	}
}

func TestInMemoryOutboxIsOrderedLeasedAndIdempotent(t *testing.T) {
	clock := newServiceClock()
	outbox := NewInMemoryOutbox(clock)
	for _, id := range []string{"message-2", "message-1"} {
		message, err := outbox.Enqueue(context.Background(), OutboxEnqueueRequest{
			MessageID:      id,
			IdempotencyKey: "enqueue-" + id,
			Topic:          "execution.control",
			SubjectID:      "run-1",
			Payload:        []byte(id),
			AvailableAt:    clock.Now(),
		})
		if err != nil {
			t.Fatalf("enqueue %q: %v", id, err)
		}
		message.Payload[0] = 'x'
	}
	claimed, err := outbox.Claim(context.Background(), OutboxClaimRequest{ClaimID: "claim-1", Owner: "worker-1", Limit: 2, TTL: time.Minute})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if got, want := len(claimed), 2; got != want || claimed[0].MessageID != "message-1" || claimed[1].MessageID != "message-2" {
		t.Fatalf("claimed order = %#v", claimed)
	}
	idempotentClaim, err := outbox.Claim(context.Background(), OutboxClaimRequest{ClaimID: "claim-1", Owner: "worker-1", Limit: 2, TTL: time.Minute})
	if err != nil || len(idempotentClaim) != 2 || idempotentClaim[0].MessageID != "message-1" {
		t.Fatalf("idempotent claim = %#v, err = %v", idempotentClaim, err)
	}
	heartbeated, err := outbox.Heartbeat(context.Background(), OutboxHeartbeatRequest{HeartbeatID: "heartbeat-1", MessageID: claimed[0].MessageID, Owner: "worker-1", ExpectedVersion: claimed[0].Version, TTL: 2 * time.Minute})
	if err != nil || heartbeated.Version != claimed[0].Version+1 || !heartbeated.LeaseExpiresAt.Equal(clock.Now().Add(2*time.Minute)) {
		t.Fatalf("heartbeat = %#v, err = %v", heartbeated, err)
	}
	heartbeatReplay, err := outbox.Heartbeat(context.Background(), OutboxHeartbeatRequest{HeartbeatID: "heartbeat-1", MessageID: claimed[0].MessageID, Owner: "worker-1", ExpectedVersion: claimed[0].Version, TTL: 2 * time.Minute})
	if err != nil || heartbeatReplay.Version != heartbeated.Version || !heartbeatReplay.LeaseExpiresAt.Equal(heartbeated.LeaseExpiresAt) {
		t.Fatalf("idempotent heartbeat = %#v, err = %v", heartbeatReplay, err)
	}
	if _, err := outbox.Ack(context.Background(), OutboxAckRequest{AckID: "ack-stale", MessageID: claimed[0].MessageID, Owner: "worker-1", ExpectedVersion: claimed[0].Version}); !errors.Is(err, ErrInvalidOutbox) {
		t.Fatalf("stale acknowledgement = %v, want invalid outbox", err)
	}
	acknowledged, err := outbox.Ack(context.Background(), OutboxAckRequest{AckID: "ack-1", MessageID: claimed[0].MessageID, Owner: "worker-1", ExpectedVersion: heartbeated.Version})
	if err != nil || acknowledged.Status != OutboxDelivered {
		t.Fatalf("ack = %#v, err = %v", acknowledged, err)
	}
	nacked, err := outbox.Nack(context.Background(), OutboxNackRequest{NackID: "nack-1", MessageID: claimed[1].MessageID, Owner: "worker-1", ExpectedVersion: claimed[1].Version, Delay: time.Minute, ErrorCode: "temporary"})
	if err != nil || nacked.Status != OutboxPending {
		t.Fatalf("nack = %#v, err = %v", nacked, err)
	}
	if retry, err := outbox.Claim(context.Background(), OutboxClaimRequest{ClaimID: "claim-before-delay", Owner: "worker-2", Limit: 1, TTL: time.Minute}); err != nil || len(retry) != 0 {
		t.Fatalf("claim before delay = %#v, err = %v", retry, err)
	}
	clock.Advance(time.Minute)
	retry, err := outbox.Claim(context.Background(), OutboxClaimRequest{ClaimID: "claim-after-delay", Owner: "worker-2", Limit: 1, TTL: time.Minute})
	if err != nil || len(retry) != 1 || retry[0].MessageID != "message-2" || retry[0].DeliveryCount != 2 {
		t.Fatalf("claim after delay = %#v, err = %v", retry, err)
	}
	clock.Advance(time.Minute)
	message, found, err := outbox.Get(context.Background(), "message-2")
	if err != nil || !found || message.Status != OutboxPending {
		t.Fatalf("expired outbox lease = %#v, found = %t, err = %v", message, found, err)
	}
}

func TestInMemoryOutboxHeartbeatCannotReviveExpiredLease(t *testing.T) {
	clock := newServiceClock()
	outbox := NewInMemoryOutbox(clock)
	if _, err := outbox.Enqueue(context.Background(), OutboxEnqueueRequest{
		MessageID: "message-expired", IdempotencyKey: "enqueue-expired", Topic: "execution.control", SubjectID: "run-1", AvailableAt: clock.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	claim, err := outbox.Claim(context.Background(), OutboxClaimRequest{ClaimID: "claim-expired", Owner: "worker-a", Limit: 1, TTL: time.Minute})
	if err != nil || len(claim) != 1 {
		t.Fatalf("claim = %#v, err = %v", claim, err)
	}
	stale := claim[0]
	clock.Advance(time.Minute)
	if _, err := outbox.Heartbeat(context.Background(), OutboxHeartbeatRequest{
		HeartbeatID: "heartbeat-expired", MessageID: stale.MessageID, Owner: stale.LeaseOwner, ExpectedVersion: stale.Version, TTL: time.Minute,
	}); !errors.Is(err, ErrInvalidOutbox) {
		t.Fatalf("expired heartbeat = %v, want invalid outbox", err)
	}
	reclaimed, err := outbox.Claim(context.Background(), OutboxClaimRequest{ClaimID: "claim-reclaimed", Owner: "worker-b", Limit: 1, TTL: time.Minute})
	if err != nil || len(reclaimed) != 1 || reclaimed[0].Version <= stale.Version || reclaimed[0].LeaseOwner != "worker-b" {
		t.Fatalf("reclaimed claim = %#v, err = %v", reclaimed, err)
	}
	if _, err := outbox.Heartbeat(context.Background(), OutboxHeartbeatRequest{
		HeartbeatID: "heartbeat-stale", MessageID: stale.MessageID, Owner: stale.LeaseOwner, ExpectedVersion: stale.Version, TTL: time.Minute,
	}); !errors.Is(err, ErrInvalidOutbox) {
		t.Fatalf("stale heartbeat = %v, want invalid outbox", err)
	}
	if _, err := outbox.Heartbeat(context.Background(), OutboxHeartbeatRequest{
		HeartbeatID: "heartbeat-reclaimed", MessageID: reclaimed[0].MessageID, Owner: reclaimed[0].LeaseOwner, ExpectedVersion: reclaimed[0].Version, TTL: time.Minute,
	}); err != nil {
		t.Fatalf("current heartbeat: %v", err)
	}
	if _, err := outbox.Heartbeat(context.Background(), OutboxHeartbeatRequest{
		HeartbeatID: "heartbeat-reclaimed", MessageID: reclaimed[0].MessageID, Owner: reclaimed[0].LeaseOwner, ExpectedVersion: reclaimed[0].Version, TTL: 2 * time.Minute,
	}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflicting heartbeat replay = %v, want idempotency conflict", err)
	}
}

type serviceClock struct {
	now time.Time
}

func newServiceClock() *serviceClock {
	return &serviceClock{now: time.Date(2026, time.July, 13, 10, 0, 0, 0, time.UTC)}
}

func (clock *serviceClock) Now() time.Time { return clock.now }

func (clock *serviceClock) Advance(duration time.Duration) { clock.now = clock.now.Add(duration) }

func newQuotaManager(t *testing.T, clock Clock, limits ...QuotaLimit) *InMemoryQuotaManager {
	t.Helper()
	manager, err := NewInMemoryQuotaManager(limits, clock)
	if err != nil {
		t.Fatalf("new quota manager: %v", err)
	}
	return manager
}

func reserveQuota(t *testing.T, manager *InMemoryQuotaManager, leaseID, key string, scope ResourceScope, dimension string, units int64, ttl time.Duration) QuotaLease {
	t.Helper()
	lease, err := manager.Reserve(context.Background(), ReserveRequest{
		LeaseID:        leaseID,
		IdempotencyKey: key,
		Owner:          "worker-1",
		Claim:          QuotaRequest{Scope: scope, Dimension: dimension, Units: units, ReclaimPolicy: ReclaimUnused},
		TTL:            ttl,
	})
	if err != nil {
		t.Fatalf("reserve %q: %v", leaseID, err)
	}
	return lease
}

func assertQuotaSnapshot(t *testing.T, snapshot QuotaSnapshot, limit, consumed, reserved, available int64) {
	t.Helper()
	if snapshot.Limit != limit || snapshot.Consumed != consumed || snapshot.Reserved != reserved || snapshot.Available != available {
		t.Fatalf("quota snapshot = %#v, want limit=%d consumed=%d reserved=%d available=%d", snapshot, limit, consumed, reserved, available)
	}
}
