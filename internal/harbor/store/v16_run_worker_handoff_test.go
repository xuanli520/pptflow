package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRunWorkerHandoffReserveReplaysAndFencesConcurrentKeys(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	run := newRunWorkerHandoffFixture(t, s)
	request := runWorkerHandoffReserveRequest(t, run, "sha256:reserve-a")
	reserved, err := s.ReserveRunWorkerHandoff(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if !reserved.Launch || reserved.Replayed || reserved.Handoff.State != RunWorkerHandoffLaunching || !reserved.Handoff.LaunchDeadlineAt.Equal(now.Add(DefaultLeaseTTL)) {
		t.Fatalf("reserved handoff = %+v", reserved)
	}

	retry := request
	retry.ID = mustUUIDv7(t)
	replayed, err := s.ReserveRunWorkerHandoff(ctx, retry)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.Launch || replayed.Handoff.ID != reserved.Handoff.ID {
		t.Fatalf("handoff replay = %+v, want original %+v", replayed, reserved.Handoff)
	}
	changed := request
	changed.RequestFingerprint = "sha256:changed-request"
	if _, err := s.ReserveRunWorkerHandoff(ctx, changed); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed handoff replay = %v, want idempotency conflict", err)
	}
	competing := request
	competing.ID = ""
	competing.IdempotencyKey = mustUUIDv7(t)
	competing.RequestFingerprint = "sha256:reserve-b"
	if _, err := s.ReserveRunWorkerHandoff(ctx, competing); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("competing handoff reserve = %v, want held active handoff", err)
	}

	staleRun := newRunWorkerHandoffFixture(t, s)
	stale := runWorkerHandoffReserveRequest(t, staleRun, "sha256:stale")
	stale.ExpectedRunVersion++
	if _, err := s.ReserveRunWorkerHandoff(ctx, stale); !errors.Is(err, ErrOptimisticLock) {
		t.Fatalf("stale handoff checkpoint = %v, want optimistic lock", err)
	}
	stored, err := s.GetRunWorkerHandoffByIdempotencyKey(ctx, stale.IdempotencyKey)
	if err != nil || stored != nil {
		t.Fatalf("stale reserve persisted an operation = %+v, %v", stored, err)
	}
}

func TestRunWorkerHandoffReserveFencesAnUnboundSupervisorLease(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	now := time.Date(2026, 7, 14, 9, 30, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	run := newRunWorkerHandoffFixture(t, s)
	legacyLease, err := s.AcquireLease(ctx, AcquireLeaseRequest{
		ResourceType: RunWorkerSupervisorLeaseResourceType, ResourceID: run.ID, Owner: "pre-v16-worker", TTL: time.Minute,
		Actor: "tester", Reason: "simulate an existing controlled worker",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := runWorkerHandoffReserveRequest(t, run, "sha256:unbound-supervisor")
	if _, err := s.ReserveRunWorkerHandoff(ctx, request); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("reserve with active unbound supervisor lease = %v, want lease held", err)
	}

	now = now.Add(2 * time.Minute)
	reserved, err := s.ReserveRunWorkerHandoff(ctx, request)
	if err != nil || !reserved.Launch {
		t.Fatalf("reserve after unbound supervisor lease expiry = %+v, %v", reserved, err)
	}
	expiredLease, err := s.GetLease(ctx, legacyLease.ID)
	if err != nil || expiredLease == nil || expiredLease.State != LeaseExpired {
		t.Fatalf("expired unbound supervisor lease = %+v, %v", expiredLease, err)
	}
}

func TestRunWorkerHandoffReserveRejectsANonRunnableRunWithoutPersisting(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	run := newRunWorkerHandoffFixture(t, s)
	run, err := s.TransitionWorkflowRun(ctx, TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: WorkflowRunCanceled, Actor: "tester", Reason: "close handoff fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := runWorkerHandoffReserveRequest(t, run, "sha256:terminal-run")
	if _, err := s.ReserveRunWorkerHandoff(ctx, request); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("reserve terminal run = %v, want invalid transition", err)
	}
	stored, err := s.GetRunWorkerHandoffByIdempotencyKey(ctx, request.IdempotencyKey)
	if err != nil || stored != nil {
		t.Fatalf("terminal reserve persisted a handoff = %+v, %v", stored, err)
	}
}

func TestRunWorkerHandoffSpawnReceiptIsImmutable(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	now := time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	run := newRunWorkerHandoffFixture(t, s)
	reserved, err := s.ReserveRunWorkerHandoff(ctx, runWorkerHandoffReserveRequest(t, run, "sha256:spawn"))
	if err != nil {
		t.Fatal(err)
	}
	spawn := RecordRunWorkerHandoffSpawnedRequest{
		OperationID: reserved.Handoff.ID, ProcessID: 4242, LogPath: "/managed/runs/worker.log", Actor: "tester", Reason: "child process started",
	}
	spawned, err := s.RecordRunWorkerHandoffSpawned(ctx, spawn)
	if err != nil {
		t.Fatal(err)
	}
	if spawned.ProcessID != spawn.ProcessID || spawned.LogPath != spawn.LogPath || spawned.SpawnedAt == nil || !json.Valid([]byte(spawned.ReceiptJSON)) {
		t.Fatalf("spawned handoff = %+v", spawned)
	}
	replayed, err := s.RecordRunWorkerHandoffSpawned(ctx, spawn)
	if err != nil || replayed.Version != spawned.Version || replayed.ReceiptJSON != spawned.ReceiptJSON {
		t.Fatalf("spawn receipt replay = %+v, %v", replayed, err)
	}
	conflict := spawn
	conflict.LogPath = "/managed/runs/different.log"
	if _, err := s.RecordRunWorkerHandoffSpawned(ctx, conflict); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed spawn receipt = %v, want idempotency conflict", err)
	}
	if _, err := s.FailRunWorkerHandoff(ctx, FailRunWorkerHandoffRequest{
		OperationID: spawned.ID, Failure: "must not replace a known child", Actor: "tester", Reason: "test immutable receipt",
	}); !errors.Is(err, ErrImmutable) {
		t.Fatalf("failure after spawn = %v, want immutable receipt error", err)
	}
}

func TestRunWorkerHandoffClaimAfterDeadlinePersistsExpiry(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	now := time.Date(2026, 7, 14, 10, 30, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	run := newRunWorkerHandoffFixture(t, s)
	request := runWorkerHandoffReserveRequest(t, run, "sha256:claim-deadline")
	request.LaunchTTL = time.Minute
	reserved, err := s.ReserveRunWorkerHandoff(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	_, err = s.ClaimRunWorkerHandoff(ctx, ClaimRunWorkerHandoffRequest{
		OperationID: reserved.Handoff.ID, RunID: run.ID, Owner: "worker-owner", ProcessID: 4747,
		LogPath: "/managed/runs/deadline.log", LeaseTTL: time.Minute, Actor: "tester", Reason: "late child claim",
	})
	if !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("late handoff claim = %v, want lease held", err)
	}
	expired, err := s.GetRunWorkerHandoff(ctx, reserved.Handoff.ID)
	if err != nil || expired == nil || expired.State != RunWorkerHandoffExpired || expired.ReleasedAt == nil {
		t.Fatalf("late claim did not persist expired handoff = %+v, %v", expired, err)
	}
	next := runWorkerHandoffReserveRequest(t, run, "sha256:claim-deadline-next")
	if result, err := s.ReserveRunWorkerHandoff(ctx, next); err != nil || !result.Launch {
		t.Fatalf("reserve after deadline expiry = %+v, %v", result, err)
	}
}

func TestRunWorkerHandoffClaimAtomicallyFencesAndReleaseReopensRun(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	now := time.Date(2026, 7, 14, 11, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	run := newRunWorkerHandoffFixture(t, s)
	reserved, err := s.ReserveRunWorkerHandoff(ctx, runWorkerHandoffReserveRequest(t, run, "sha256:claim"))
	if err != nil {
		t.Fatal(err)
	}
	claimRequest := ClaimRunWorkerHandoffRequest{
		OperationID: reserved.Handoff.ID, RunID: run.ID, Owner: "worker-owner", ProcessID: 5151,
		LogPath: "/managed/runs/claim.log", LeaseTTL: time.Minute, Actor: "tester", Reason: "controlled child acquired worker",
	}
	claim, err := s.ClaimRunWorkerHandoff(ctx, claimRequest)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Handoff.State != RunWorkerHandoffHandedOff || claim.Handoff.WorkerLeaseID != claim.WorkerLease.ID || claim.WorkerLease.ResourceType != RunWorkerSupervisorLeaseResourceType || claim.WorkerLease.ResourceID != run.ID || claim.WorkerLease.State != LeaseActive || claim.Handoff.ProcessID != claimRequest.ProcessID {
		t.Fatalf("handoff claim = %+v lease=%+v", claim.Handoff, claim.WorkerLease)
	}
	parentReceipt, err := s.RecordRunWorkerHandoffSpawned(ctx, RecordRunWorkerHandoffSpawnedRequest{
		OperationID: claim.Handoff.ID, ProcessID: claimRequest.ProcessID, LogPath: claimRequest.LogPath,
		Actor: "tester", Reason: "parent recovered after child claim",
	})
	if err != nil || parentReceipt.State != RunWorkerHandoffHandedOff || parentReceipt.ReceiptJSON != claim.Handoff.ReceiptJSON {
		t.Fatalf("parent receipt replay after child claim = %+v, %v", parentReceipt, err)
	}
	if _, err := s.ClaimRunWorkerHandoff(ctx, claimRequest); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("duplicate child claim = %v, want active handoff lease held", err)
	}
	leases, err := s.ListLeasesForResource(ctx, RunWorkerSupervisorLeaseResourceType, run.ID)
	if err != nil || len(leases) != 1 {
		t.Fatalf("supervisor lease history = %+v, %v", leases, err)
	}
	released, err := s.ReleaseRunWorkerHandoff(ctx, ReleaseRunWorkerHandoffRequest{
		OperationID: claim.Handoff.ID, WorkerLease: claim.WorkerLease, Actor: "tester", Reason: "controlled child exited normally",
	})
	if err != nil || released.State != RunWorkerHandoffReleased || released.ReleasedAt == nil {
		t.Fatalf("handoff release = %+v, %v", released, err)
	}
	lease, err := s.GetLease(ctx, claim.WorkerLease.ID)
	if err != nil || lease == nil || lease.State != LeaseReleased {
		t.Fatalf("released supervisor lease = %+v, %v", lease, err)
	}
	if replayed, err := s.ReleaseRunWorkerHandoff(ctx, ReleaseRunWorkerHandoffRequest{
		OperationID: claim.Handoff.ID, WorkerLease: claim.WorkerLease, Actor: "tester", Reason: "replay normal child exit",
	}); err != nil || replayed.ID != released.ID || replayed.State != RunWorkerHandoffReleased {
		t.Fatalf("handoff release replay = %+v, %v", replayed, err)
	}
	next := runWorkerHandoffReserveRequest(t, run, "sha256:claim-next")
	if nextResult, err := s.ReserveRunWorkerHandoff(ctx, next); err != nil || !nextResult.Launch {
		t.Fatalf("reserve after normal release = %+v, %v", nextResult, err)
	}
}

func TestRunWorkerHandoffReleaseRequiresTheLatestSupervisorLeaseVersion(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	now := time.Date(2026, 7, 14, 11, 30, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	run := newRunWorkerHandoffFixture(t, s)
	reserved, err := s.ReserveRunWorkerHandoff(ctx, runWorkerHandoffReserveRequest(t, run, "sha256:release-fence"))
	if err != nil {
		t.Fatal(err)
	}
	claim, err := s.ClaimRunWorkerHandoff(ctx, ClaimRunWorkerHandoffRequest{
		OperationID: reserved.Handoff.ID, RunID: run.ID, Owner: "worker-owner", ProcessID: 6161,
		LogPath: "/managed/runs/release-fence.log", LeaseTTL: time.Minute, Actor: "tester", Reason: "claim release fence fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	latest, err := s.HeartbeatLease(ctx, HeartbeatLeaseRequest{
		LeaseID: claim.WorkerLease.ID, Owner: claim.WorkerLease.Owner, FencingToken: claim.WorkerLease.FencingToken,
		ExpectedVersion: claim.WorkerLease.Version, TTL: time.Minute, Actor: "tester", Reason: "refresh controlled worker fence",
	})
	if err != nil || latest.Version <= claim.WorkerLease.Version {
		t.Fatalf("heartbeat controlled worker lease = %+v, %v", latest, err)
	}
	if _, err := s.ReleaseRunWorkerHandoff(ctx, ReleaseRunWorkerHandoffRequest{
		OperationID: claim.Handoff.ID, WorkerLease: claim.WorkerLease, Actor: "tester", Reason: "stale worker shutdown",
	}); !errors.Is(err, ErrFencingToken) {
		t.Fatalf("release with stale worker lease version = %v, want fencing failure", err)
	}
	stillActive, err := s.GetLease(ctx, latest.ID)
	if err != nil || stillActive == nil || stillActive.State != LeaseActive || stillActive.Version != latest.Version {
		t.Fatalf("stale release altered supervisor lease = %+v, %v", stillActive, err)
	}
	released, err := s.ReleaseRunWorkerHandoff(ctx, ReleaseRunWorkerHandoffRequest{
		OperationID: claim.Handoff.ID, WorkerLease: latest, Actor: "tester", Reason: "latest worker shutdown",
	})
	if err != nil || released.State != RunWorkerHandoffReleased {
		t.Fatalf("release with latest worker lease = %+v, %v", released, err)
	}
}

func TestRunWorkerHandoffReconcileExpiresOnlySelectedStaleOperations(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	runA := newRunWorkerHandoffFixture(t, s)
	runB := newRunWorkerHandoffFixture(t, s)
	launching, err := s.ReserveRunWorkerHandoff(ctx, ReserveRunWorkerHandoffRequest{
		IdempotencyKey: mustUUIDv7(t), RequestFingerprint: "sha256:launching-expiry", RunID: runA.ID,
		ExpectedRunVersion: runA.Version, ExpectedRunExecutionEpoch: runA.ExecutionEpoch, ExpectedRunDefinitionHash: runA.DefinitionHash,
		Owner: "worker-a", Actor: "tester", Reason: "reserve stale handoff", LaunchTTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.ReserveRunWorkerHandoff(ctx, ReserveRunWorkerHandoffRequest{
		IdempotencyKey: mustUUIDv7(t), RequestFingerprint: "sha256:other-active", RunID: runB.ID,
		ExpectedRunVersion: runB.Version, ExpectedRunExecutionEpoch: runB.ExecutionEpoch, ExpectedRunDefinitionHash: runB.DefinitionHash,
		Owner: "worker-b", Actor: "tester", Reason: "reserve other run", LaunchTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	expired, err := s.ReconcileRunWorkerHandoffs(ctx, ReconcileRunWorkerHandoffsRequest{RunID: runA.ID, Actor: "tester", Reason: "recover selected stale handoff"})
	if err != nil || len(expired) != 1 || expired[0].ID != launching.Handoff.ID || expired[0].State != RunWorkerHandoffExpired {
		t.Fatalf("launching reconcile = %+v, %v", expired, err)
	}
	unchanged, err := s.GetRunWorkerHandoff(ctx, other.Handoff.ID)
	if err != nil || unchanged == nil || unchanged.State != RunWorkerHandoffLaunching {
		t.Fatalf("unselected handoff changed = %+v, %v", unchanged, err)
	}
	if repeated, err := s.ReconcileRunWorkerHandoffs(ctx, ReconcileRunWorkerHandoffsRequest{RunID: runA.ID, Actor: "tester", Reason: "repeat selected recovery"}); err != nil || len(repeated) != 0 {
		t.Fatalf("repeat reconcile = %+v, %v", repeated, err)
	}

	claimedReserve := runWorkerHandoffReserveRequest(t, runA, "sha256:claimed-expiry")
	claimed, err := s.ReserveRunWorkerHandoff(ctx, claimedReserve)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := s.ClaimRunWorkerHandoff(ctx, ClaimRunWorkerHandoffRequest{
		OperationID: claimed.Handoff.ID, RunID: runA.ID, Owner: "worker-owner", ProcessID: 7171,
		LogPath: "/managed/runs/reconcile.log", LeaseTTL: time.Minute, Actor: "tester", Reason: "claim expiring worker",
	})
	if err != nil {
		t.Fatal(err)
	}
	if active, err := s.ReconcileRunWorkerHandoffs(ctx, ReconcileRunWorkerHandoffsRequest{RunID: runA.ID, Actor: "tester", Reason: "observe active worker"}); err != nil || len(active) != 0 {
		t.Fatalf("active worker reconcile = %+v, %v", active, err)
	}
	now = now.Add(2 * time.Minute)
	expired, err = s.ReconcileRunWorkerHandoffs(ctx, ReconcileRunWorkerHandoffsRequest{RunID: runA.ID, Actor: "tester", Reason: "recover expired worker"})
	if err != nil || len(expired) != 1 || expired[0].ID != worker.Handoff.ID || expired[0].State != RunWorkerHandoffExpired {
		t.Fatalf("claimed reconcile = %+v, %v", expired, err)
	}
	lease, err := s.GetLease(ctx, worker.WorkerLease.ID)
	if err != nil || lease == nil || lease.State != LeaseExpired {
		t.Fatalf("reconciled worker lease = %+v, %v", lease, err)
	}
}

func TestRunWorkerHandoffReconcileRejectsMismatchedLeaseBindingsWithoutMutatingLease(t *testing.T) {
	testCases := []struct {
		name   string
		mutate func(t *testing.T, s *Store, handoff RunWorkerHandoff, lease Lease)
	}{
		{
			name: "resource type",
			mutate: func(t *testing.T, s *Store, _ RunWorkerHandoff, lease Lease) {
				t.Helper()
				if _, err := s.db.Exec(`UPDATE leases SET resource_type = 'wrong_worker_resource' WHERE id = ?`, lease.ID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "resource run",
			mutate: func(t *testing.T, s *Store, _ RunWorkerHandoff, lease Lease) {
				t.Helper()
				if _, err := s.db.Exec(`UPDATE leases SET resource_id = ? WHERE id = ?`, mustUUIDv7(t), lease.ID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "owner",
			mutate: func(t *testing.T, s *Store, _ RunWorkerHandoff, lease Lease) {
				t.Helper()
				if _, err := s.db.Exec(`UPDATE leases SET owner = 'wrong-worker-owner' WHERE id = ?`, lease.ID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "fencing token",
			mutate: func(t *testing.T, s *Store, _ RunWorkerHandoff, lease Lease) {
				t.Helper()
				if _, err := s.db.Exec(`UPDATE leases SET fencing_token = ? WHERE id = ?`, int64(lease.FencingToken+1), lease.ID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "bound version",
			mutate: func(t *testing.T, s *Store, handoff RunWorkerHandoff, _ Lease) {
				t.Helper()
				if _, err := s.db.Exec(`UPDATE run_worker_handoffs_v16 SET worker_lease_version = worker_lease_version + 1 WHERE id = ?`, handoff.ID); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			s := tempDB(t)
			now := time.Date(2026, 7, 14, 12, 30, 0, 0, time.UTC)
			s.now = func() time.Time { return now }
			run := newRunWorkerHandoffFixture(t, s)
			reserved, err := s.ReserveRunWorkerHandoff(ctx, runWorkerHandoffReserveRequest(t, run, "sha256:binding-"+testCase.name))
			if err != nil {
				t.Fatal(err)
			}
			claim, err := s.ClaimRunWorkerHandoff(ctx, ClaimRunWorkerHandoffRequest{
				OperationID: reserved.Handoff.ID, RunID: run.ID, Owner: "worker-owner", ProcessID: 8181,
				LogPath: "/managed/runs/binding.log", LeaseTTL: time.Hour, Actor: "tester", Reason: "claim binding fixture",
			})
			if err != nil {
				t.Fatal(err)
			}
			testCase.mutate(t, s, claim.Handoff, claim.WorkerLease)
			expired, err := s.ReconcileRunWorkerHandoffs(ctx, ReconcileRunWorkerHandoffsRequest{
				RunID: run.ID, Actor: "tester", Reason: "reject mismatched worker lease binding",
			})
			if err != nil || len(expired) != 1 || expired[0].ID != claim.Handoff.ID || expired[0].State != RunWorkerHandoffExpired {
				t.Fatalf("reconcile mismatched binding = %+v, %v", expired, err)
			}
			lease, err := s.GetLease(ctx, claim.WorkerLease.ID)
			if err != nil || lease == nil || lease.State != LeaseActive {
				t.Fatalf("reconcile mutated mismatched lease = %+v, %v", lease, err)
			}
		})
	}
}

func TestRunWorkerHandoffIdentityUsesGlobalUUIDv7Registry(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	run := newRunWorkerHandoffFixture(t, s)
	request := runWorkerHandoffReserveRequest(t, run, "sha256:identity")
	request.ID = run.ID
	if _, err := s.ReserveRunWorkerHandoff(ctx, request); !errors.Is(err, ErrIdentityCollision) {
		t.Fatalf("handoff operation ID collision = %v, want global identity collision", err)
	}
}

func TestRunWorkerHandoffSchemaFencesRawConcurrentActiveRows(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	now := time.Date(2026, 7, 14, 13, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	run := newRunWorkerHandoffFixture(t, s)
	first := RunWorkerHandoff{
		ID: mustUUIDv7(t), IdempotencyKey: mustUUIDv7(t), RequestFingerprint: "sha256:raw-active-a",
		RunID: run.ID, ExpectedRunVersion: run.Version, ExpectedRunExecutionEpoch: run.ExecutionEpoch,
		ExpectedRunDefinitionHash: run.DefinitionHash, Owner: "worker-owner", Actor: "tester", Reason: "raw schema fixture",
		State: RunWorkerHandoffLaunching, LaunchDeadlineAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now, Version: 1,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertRunWorkerHandoffTx(ctx, tx, first); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	second := first
	second.ID = mustUUIDv7(t)
	second.IdempotencyKey = mustUUIDv7(t)
	second.RequestFingerprint = "sha256:raw-active-b"
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := insertRunWorkerHandoffTx(ctx, tx, second); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("second raw active handoff insert = %v, want active handoff lease held", err)
	}
}

func TestMigrateV15ToV16InstallsRunWorkerHandoffSchemaAndIdentityTriggers(t *testing.T) {
	s := tempDB(t)
	root := s.rootDir
	if _, err := s.db.Exec(`DROP TABLE run_worker_handoffs_v16`); err != nil {
		t.Fatalf("remove V16 handoff table: %v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM schema_version WHERE version = 16`); err != nil {
		t.Fatalf("rewind schema fixture from V16: %v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM schema_version WHERE version = 17`); err != nil {
		t.Fatalf("rewind schema fixture from V17: %v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM schema_version WHERE version = 18`); err != nil {
		t.Fatalf("rewind schema fixture from V18: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := Open(root)
	if err != nil {
		t.Fatalf("migrate V15 store to V16: %v", err)
	}
	defer migrated.Close()
	var version, tableCount, triggerCount int
	var activeIndexSQL string
	if err := migrated.db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := migrated.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'run_worker_handoffs_v16'`).Scan(&tableCount); err != nil {
		t.Fatal(err)
	}
	if err := migrated.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'idx_run_worker_handoffs_v16_active_run'`).Scan(&activeIndexSQL); err != nil {
		t.Fatal(err)
	}
	if err := migrated.db.QueryRow(`
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'trigger' AND name IN (
			'entity_id_registry_run_worker_handoffs_v16_insert',
			'entity_id_registry_run_worker_handoffs_v16_id_immutable'
		)
	`).Scan(&triggerCount); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion || tableCount != 1 || triggerCount != 2 || !strings.Contains(activeIndexSQL, "WHERE state IN ('launching', 'handed_off')") {
		t.Fatalf("V16 migration version=%d table_count=%d trigger_count=%d active_index=%q", version, tableCount, triggerCount, activeIndexSQL)
	}
	ctx := context.Background()
	run := newRunWorkerHandoffFixture(t, migrated)
	collision := runWorkerHandoffReserveRequest(t, run, "sha256:migrated-identity-collision")
	collision.ID = run.ID
	if _, err := migrated.ReserveRunWorkerHandoff(ctx, collision); !errors.Is(err, ErrIdentityCollision) {
		t.Fatalf("migrated V16 global identity collision = %v, want identity collision", err)
	}
	reserved, err := migrated.ReserveRunWorkerHandoff(ctx, runWorkerHandoffReserveRequest(t, run, "sha256:migrated-identity"))
	if err != nil || !reserved.Launch {
		t.Fatalf("reserve on migrated V16 schema = %+v, %v", reserved, err)
	}
	var entityType string
	if err := migrated.db.QueryRow(`SELECT entity_type FROM entity_id_registry WHERE id = ?`, reserved.Handoff.ID).Scan(&entityType); err != nil {
		t.Fatal(err)
	}
	if entityType != runWorkerHandoffIdentitySource.entityType {
		t.Fatalf("migrated V16 registry entity type = %q, want %q", entityType, runWorkerHandoffIdentitySource.entityType)
	}
	if _, err := migrated.db.Exec(`UPDATE run_worker_handoffs_v16 SET id = ? WHERE id = ?`, mustUUIDv7(t), reserved.Handoff.ID); err == nil || !strings.Contains(err.Error(), "identity is immutable") {
		t.Fatalf("migrated V16 immutable identity update = %v, want immutable identity failure", err)
	}
}

func newRunWorkerHandoffFixture(t *testing.T, s *Store) WorkflowRun {
	t.Helper()
	ctx := context.Background()
	task, revision := createValidatedTaskAndRevision(t, s)
	run, err := s.CreateWorkflowRun(ctx, CreateWorkflowRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, WorkflowTemplateID: "handoff.template", WorkflowTemplateVersion: "v1",
		ResolvedProfileHash: "handoff-profile", DefinitionHash: "handoff-definition-" + mustUUIDv7(t),
		RunManifestJSON: `{}`, Trigger: "handoff-fixture", Actor: "tester", Reason: "create handoff fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func runWorkerHandoffReserveRequest(t *testing.T, run WorkflowRun, fingerprint string) ReserveRunWorkerHandoffRequest {
	t.Helper()
	return ReserveRunWorkerHandoffRequest{
		IdempotencyKey: mustUUIDv7(t), RequestFingerprint: fingerprint, RunID: run.ID,
		ExpectedRunVersion: run.Version, ExpectedRunExecutionEpoch: run.ExecutionEpoch, ExpectedRunDefinitionHash: run.DefinitionHash,
		Owner: "worker-owner", Actor: "tester", Reason: "launch controlled worker",
	}
}
