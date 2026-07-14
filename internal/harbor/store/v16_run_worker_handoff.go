package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const runWorkerHandoffV16Select = `
	SELECT id, idempotency_key, request_fingerprint, run_id, expected_run_version,
	       expected_run_execution_epoch, expected_run_definition_hash, owner, actor,
	       reason, state, launch_deadline_at, worker_lease_id, worker_lease_owner,
	       worker_lease_fencing_token, worker_lease_version, process_id, log_path,
	       receipt_json, failure_reason, created_at, updated_at, spawned_at,
	       handed_off_at, released_at, version
	FROM run_worker_handoffs_v16`

const runWorkerHandoffReceiptFormat = "harbor.run-worker-handoff.v1"

// ReserveRunWorkerHandoff records the only launch authority for one Run. A
// retry with the same UUIDv7 key returns the original operation without
// starting another process; a different key is rejected while a handoff or
// controlled worker remains active.
func (s *Store) ReserveRunWorkerHandoff(ctx context.Context, request ReserveRunWorkerHandoffRequest) (ReserveRunWorkerHandoffResult, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return ReserveRunWorkerHandoffResult{}, err
	}
	prepared, err := prepareReserveRunWorkerHandoffRequest(s, request)
	if err != nil {
		return ReserveRunWorkerHandoffResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReserveRunWorkerHandoffResult{}, err
	}
	defer tx.Rollback()
	if existing, err := getRunWorkerHandoffByKeyTx(ctx, tx, prepared.idempotencyKey); err != nil {
		return ReserveRunWorkerHandoffResult{}, err
	} else if existing != nil {
		if !sameRunWorkerHandoffReserve(*existing, prepared) {
			return ReserveRunWorkerHandoffResult{}, fmt.Errorf("%w: run-worker handoff key %s", ErrIdempotencyConflict, prepared.idempotencyKey)
		}
		if err := tx.Commit(); err != nil {
			return ReserveRunWorkerHandoffResult{}, err
		}
		return ReserveRunWorkerHandoffResult{Handoff: *existing, Replayed: true}, nil
	}
	now := s.now().UTC()
	run, err := getWorkflowRunTx(ctx, tx, prepared.runID)
	if err != nil {
		return ReserveRunWorkerHandoffResult{}, err
	}
	if err := validateRunWorkerHandoffCheckpoint(run, prepared); err != nil {
		return ReserveRunWorkerHandoffResult{}, err
	}
	if !runWorkerHandoffRunnable(run.Status) {
		return ReserveRunWorkerHandoffResult{}, fmt.Errorf("%w: workflow run %s is %s", ErrInvalidTransition, run.ID, run.Status)
	}
	if err := s.reconcileActiveRunWorkerHandoffTx(ctx, tx, run.ID, prepared.actor, prepared.reason, now); err != nil {
		return ReserveRunWorkerHandoffResult{}, err
	}
	if active, err := getActiveRunWorkerHandoffTx(ctx, tx, run.ID); err != nil {
		return ReserveRunWorkerHandoffResult{}, err
	} else if active != nil {
		return ReserveRunWorkerHandoffResult{}, handoffHeldError(*active)
	}
	if err := s.reconcileUnboundRunWorkerSupervisorLeaseTx(ctx, tx, run.ID, prepared.actor, prepared.reason, now); err != nil {
		return ReserveRunWorkerHandoffResult{}, err
	}
	handoff := RunWorkerHandoff{
		ID: prepared.id, IdempotencyKey: prepared.idempotencyKey, RequestFingerprint: prepared.requestFingerprint,
		RunID: prepared.runID, ExpectedRunVersion: prepared.expectedRunVersion,
		ExpectedRunExecutionEpoch: prepared.expectedRunExecutionEpoch, ExpectedRunDefinitionHash: prepared.expectedRunDefinitionHash,
		Owner: prepared.owner, Actor: prepared.actor, Reason: prepared.reason, State: RunWorkerHandoffLaunching,
		LaunchDeadlineAt: now.Add(prepared.launchTTL), CreatedAt: now, UpdatedAt: now, Version: 1,
	}
	if err := insertRunWorkerHandoffTx(ctx, tx, handoff); err != nil {
		return ReserveRunWorkerHandoffResult{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: handoff.Actor, EntityType: "run_worker_handoff", EntityID: handoff.ID, Action: "run_worker_handoff.reserved",
		Reason: handoff.Reason, OperationKey: handoff.IdempotencyKey,
		PayloadJSON: auditPayload(map[string]any{"run_id": handoff.RunID, "launch_deadline_at": handoff.LaunchDeadlineAt, "request_fingerprint": handoff.RequestFingerprint}), CreatedAt: now,
	}); err != nil {
		return ReserveRunWorkerHandoffResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ReserveRunWorkerHandoffResult{}, err
	}
	return ReserveRunWorkerHandoffResult{Handoff: handoff, Launch: true}, nil
}

// RecordRunWorkerHandoffSpawned persists the immutable PID/log receipt after
// exec.Start succeeds. The child can call Claim first after a parent crash;
// exact receipt replay remains valid in either order.
func (s *Store) RecordRunWorkerHandoffSpawned(ctx context.Context, request RecordRunWorkerHandoffSpawnedRequest) (RunWorkerHandoff, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return RunWorkerHandoff{}, err
	}
	prepared, err := prepareRecordRunWorkerHandoffSpawnedRequest(request)
	if err != nil {
		return RunWorkerHandoff{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RunWorkerHandoff{}, err
	}
	defer tx.Rollback()
	handoff, err := getRunWorkerHandoffTx(ctx, tx, prepared.operationID)
	if err != nil {
		return RunWorkerHandoff{}, err
	}
	updated, changed, err := recordRunWorkerHandoffSpawnedTx(ctx, tx, handoff, prepared, s.now().UTC())
	if err != nil {
		return RunWorkerHandoff{}, err
	}
	if changed {
		if err := s.appendRunWorkerHandoffAuditTx(ctx, tx, updated, "run_worker_handoff.spawned", prepared.actor, prepared.reason, map[string]any{"pid": updated.ProcessID, "log_path": updated.LogPath}); err != nil {
			return RunWorkerHandoff{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return RunWorkerHandoff{}, err
	}
	return updated, nil
}

// FailRunWorkerHandoff records a proven exec.Start failure. A spawned or
// claimed child is never rewritten as failed because its real outcome must be
// reconciled from its durable worker lease and log receipt.
func (s *Store) FailRunWorkerHandoff(ctx context.Context, request FailRunWorkerHandoffRequest) (RunWorkerHandoff, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return RunWorkerHandoff{}, err
	}
	prepared, err := prepareFailRunWorkerHandoffRequest(request)
	if err != nil {
		return RunWorkerHandoff{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RunWorkerHandoff{}, err
	}
	defer tx.Rollback()
	handoff, err := getRunWorkerHandoffTx(ctx, tx, prepared.operationID)
	if err != nil {
		return RunWorkerHandoff{}, err
	}
	if handoff.State == RunWorkerHandoffFailed {
		if handoff.FailureReason != prepared.failure {
			return RunWorkerHandoff{}, fmt.Errorf("%w: run-worker handoff %s failure", ErrIdempotencyConflict, handoff.ID)
		}
		if err := tx.Commit(); err != nil {
			return RunWorkerHandoff{}, err
		}
		return handoff, nil
	}
	if handoff.State != RunWorkerHandoffLaunching || handoff.ProcessID != 0 {
		return RunWorkerHandoff{}, fmt.Errorf("%w: run-worker handoff %s cannot be marked failed", ErrImmutable, handoff.ID)
	}
	now := s.now().UTC()
	previousVersion := handoff.Version
	handoff.State = RunWorkerHandoffFailed
	handoff.FailureReason = prepared.failure
	handoff.UpdatedAt = now
	handoff.Version++
	result, err := tx.ExecContext(ctx, `
		UPDATE run_worker_handoffs_v16
		SET state = ?, failure_reason = ?, updated_at = ?, version = ?
		WHERE id = ? AND state = 'launching' AND process_id = 0 AND version = ?
	`, handoff.State, handoff.FailureReason, handoff.UpdatedAt, handoff.Version, handoff.ID, previousVersion)
	if err != nil {
		return RunWorkerHandoff{}, err
	}
	if err := requireOneRunWorkerHandoffRow(result, "run-worker handoff", handoff.ID); err != nil {
		return RunWorkerHandoff{}, err
	}
	if err := s.appendRunWorkerHandoffAuditTx(ctx, tx, handoff, "run_worker_handoff.failed", prepared.actor, prepared.reason, map[string]any{"failure_reason": handoff.FailureReason}); err != nil {
		return RunWorkerHandoff{}, err
	}
	if err := tx.Commit(); err != nil {
		return RunWorkerHandoff{}, err
	}
	return handoff, nil
}

// ClaimRunWorkerHandoff is the child-side atomic handoff. It validates the
// frozen Run checkpoint, records the immutable process receipt if necessary,
// acquires the supervisor lease, and transitions the operation to handed_off
// in one SQLite transaction.
func (s *Store) ClaimRunWorkerHandoff(ctx context.Context, request ClaimRunWorkerHandoffRequest) (RunWorkerHandoffClaim, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return RunWorkerHandoffClaim{}, err
	}
	prepared, err := prepareClaimRunWorkerHandoffRequest(s, request)
	if err != nil {
		return RunWorkerHandoffClaim{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RunWorkerHandoffClaim{}, err
	}
	defer tx.Rollback()
	handoff, err := getRunWorkerHandoffTx(ctx, tx, prepared.operationID)
	if err != nil {
		return RunWorkerHandoffClaim{}, err
	}
	if handoff.RunID != prepared.runID || handoff.Owner != prepared.owner {
		return RunWorkerHandoffClaim{}, fmt.Errorf("%w: child does not match run-worker handoff %s", ErrIdempotencyConflict, handoff.ID)
	}
	if handoff.State != RunWorkerHandoffLaunching {
		return RunWorkerHandoffClaim{}, handoffStateClaimError(handoff)
	}
	now := s.now().UTC()
	if !handoff.LaunchDeadlineAt.After(now) {
		if err := s.expireRunWorkerHandoffTx(ctx, tx, handoff, prepared.actor, prepared.reason, now, "child claim arrived after launch deadline"); err != nil {
			return RunWorkerHandoffClaim{}, err
		}
		if err := tx.Commit(); err != nil {
			return RunWorkerHandoffClaim{}, err
		}
		return RunWorkerHandoffClaim{}, fmt.Errorf("%w: run-worker handoff %s launch deadline elapsed", ErrLeaseHeld, handoff.ID)
	}
	run, err := getWorkflowRunTx(ctx, tx, handoff.RunID)
	if err != nil {
		return RunWorkerHandoffClaim{}, err
	}
	if err := validatePersistedRunWorkerHandoffCheckpoint(run, handoff); err != nil {
		return RunWorkerHandoffClaim{}, err
	}
	if !runWorkerHandoffRunnable(run.Status) {
		return RunWorkerHandoffClaim{}, fmt.Errorf("%w: workflow run %s is %s", ErrInvalidTransition, run.ID, run.Status)
	}
	spawnRequest := preparedRecordRunWorkerHandoffSpawnedRequest{
		operationID: prepared.operationID, processID: prepared.processID, logPath: prepared.logPath, actor: prepared.actor, reason: prepared.reason,
	}
	handoff, spawned, err := recordRunWorkerHandoffSpawnedTx(ctx, tx, handoff, spawnRequest, now)
	if err != nil {
		return RunWorkerHandoffClaim{}, err
	}
	workerLease, err := acquireRunWorkerSupervisorLeaseTx(ctx, tx, prepared, handoff, now)
	if err != nil {
		return RunWorkerHandoffClaim{}, err
	}
	previousVersion := handoff.Version
	handoff.State = RunWorkerHandoffHandedOff
	handoff.WorkerLeaseID = workerLease.ID
	handoff.WorkerLeaseOwner = workerLease.Owner
	handoff.WorkerLeaseFencingToken = workerLease.FencingToken
	handoff.WorkerLeaseVersion = workerLease.Version
	handoff.HandedOffAt = &now
	handoff.UpdatedAt = now
	handoff.Version++
	result, err := tx.ExecContext(ctx, `
		UPDATE run_worker_handoffs_v16
		SET state = ?, worker_lease_id = ?, worker_lease_owner = ?, worker_lease_fencing_token = ?,
			worker_lease_version = ?, handed_off_at = ?, updated_at = ?, version = ?
		WHERE id = ? AND state = 'launching' AND version = ?
	`, handoff.State, handoff.WorkerLeaseID, handoff.WorkerLeaseOwner, int64(handoff.WorkerLeaseFencingToken),
		handoff.WorkerLeaseVersion, handoff.HandedOffAt, handoff.UpdatedAt, handoff.Version, handoff.ID, previousVersion)
	if err != nil {
		return RunWorkerHandoffClaim{}, err
	}
	if err := requireOneRunWorkerHandoffRow(result, "run-worker handoff", handoff.ID); err != nil {
		return RunWorkerHandoffClaim{}, err
	}
	if spawned {
		if err := s.appendRunWorkerHandoffAuditTx(ctx, tx, handoff, "run_worker_handoff.spawned", prepared.actor, prepared.reason, map[string]any{"pid": handoff.ProcessID, "log_path": handoff.LogPath, "recovered_by_child": true}); err != nil {
			return RunWorkerHandoffClaim{}, err
		}
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: prepared.actor, EntityType: "lease", EntityID: workerLease.ID, Action: "lease.acquired", Reason: prepared.reason,
		OperationKey: handoff.IdempotencyKey, PayloadJSON: auditPayload(map[string]any{"resource_type": workerLease.ResourceType, "resource_id": workerLease.ResourceID, "fencing_token": workerLease.FencingToken, "handoff_id": handoff.ID}), CreatedAt: now,
	}); err != nil {
		return RunWorkerHandoffClaim{}, err
	}
	if err := s.appendRunWorkerHandoffAuditTx(ctx, tx, handoff, "run_worker_handoff.claimed", prepared.actor, prepared.reason, map[string]any{"worker_lease_id": workerLease.ID, "worker_fencing_token": workerLease.FencingToken}); err != nil {
		return RunWorkerHandoffClaim{}, err
	}
	if err := tx.Commit(); err != nil {
		return RunWorkerHandoffClaim{}, err
	}
	return RunWorkerHandoffClaim{Handoff: handoff, WorkerLease: workerLease}, nil
}

// ReleaseRunWorkerHandoff atomically releases the exact supervisor lease and
// closes its handoff. A lost/expired lease remains handed_off for scoped
// reconciliation rather than being guessed closed.
func (s *Store) ReleaseRunWorkerHandoff(ctx context.Context, request ReleaseRunWorkerHandoffRequest) (RunWorkerHandoff, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return RunWorkerHandoff{}, err
	}
	prepared, err := prepareReleaseRunWorkerHandoffRequest(request)
	if err != nil {
		return RunWorkerHandoff{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RunWorkerHandoff{}, err
	}
	defer tx.Rollback()
	handoff, err := getRunWorkerHandoffTx(ctx, tx, prepared.operationID)
	if err != nil {
		return RunWorkerHandoff{}, err
	}
	if handoff.State == RunWorkerHandoffReleased {
		if !sameRunWorkerHandoffLease(handoff, prepared.workerLease) {
			return RunWorkerHandoff{}, fmt.Errorf("%w: released run-worker handoff %s lease", ErrIdempotencyConflict, handoff.ID)
		}
		if err := tx.Commit(); err != nil {
			return RunWorkerHandoff{}, err
		}
		return handoff, nil
	}
	if handoff.State != RunWorkerHandoffHandedOff || !sameRunWorkerHandoffLease(handoff, prepared.workerLease) {
		return RunWorkerHandoff{}, fmt.Errorf("%w: run-worker handoff %s lease", ErrFencingToken, handoff.ID)
	}
	lease, err := getLeaseTx(ctx, tx, handoff.WorkerLeaseID)
	if err != nil {
		return RunWorkerHandoff{}, err
	}
	if lease.Owner != handoff.WorkerLeaseOwner || lease.FencingToken != handoff.WorkerLeaseFencingToken || lease.Version != prepared.workerLease.Version {
		return RunWorkerHandoff{}, fmt.Errorf("%w: supervisor lease %s", ErrFencingToken, lease.ID)
	}
	if lease.State != LeaseActive {
		return RunWorkerHandoff{}, fmt.Errorf("%w: supervisor lease %s is %s", ErrInvalidTransition, lease.ID, lease.State)
	}
	now := s.now().UTC()
	lease.State = LeaseReleased
	lease.UpdatedAt = now
	lease.Version++
	leaseResult, err := tx.ExecContext(ctx, `
		UPDATE leases SET state = ?, updated_at = ?, version = ?
		WHERE id = ? AND state = 'active' AND owner = ? AND fencing_token = ? AND version = ?
	`, lease.State, lease.UpdatedAt, lease.Version, lease.ID, lease.Owner, int64(lease.FencingToken), prepared.workerLease.Version)
	if err != nil {
		return RunWorkerHandoff{}, err
	}
	if err := requireOneRunWorkerHandoffRow(leaseResult, "supervisor lease", lease.ID); err != nil {
		return RunWorkerHandoff{}, err
	}
	previousVersion := handoff.Version
	handoff.State = RunWorkerHandoffReleased
	handoff.ReleasedAt = &now
	handoff.UpdatedAt = now
	handoff.Version++
	result, err := tx.ExecContext(ctx, `
		UPDATE run_worker_handoffs_v16
		SET state = ?, released_at = ?, updated_at = ?, version = ?
		WHERE id = ? AND state = 'handed_off' AND version = ?
	`, handoff.State, handoff.ReleasedAt, handoff.UpdatedAt, handoff.Version, handoff.ID, previousVersion)
	if err != nil {
		return RunWorkerHandoff{}, err
	}
	if err := requireOneRunWorkerHandoffRow(result, "run-worker handoff", handoff.ID); err != nil {
		return RunWorkerHandoff{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: prepared.actor, EntityType: "lease", EntityID: lease.ID, Action: "lease.released", Reason: prepared.reason,
		OperationKey: handoff.IdempotencyKey, PayloadJSON: auditPayload(map[string]any{"resource_type": lease.ResourceType, "resource_id": lease.ResourceID, "fencing_token": lease.FencingToken, "handoff_id": handoff.ID}), CreatedAt: now,
	}); err != nil {
		return RunWorkerHandoff{}, err
	}
	if err := s.appendRunWorkerHandoffAuditTx(ctx, tx, handoff, "run_worker_handoff.released", prepared.actor, prepared.reason, map[string]any{"worker_lease_id": lease.ID}); err != nil {
		return RunWorkerHandoff{}, err
	}
	if err := tx.Commit(); err != nil {
		return RunWorkerHandoff{}, err
	}
	return handoff, nil
}

// ReconcileRunWorkerHandoffs expires only selected Run handoffs whose launch
// deadline or claimed worker lease proves the child is no longer authoritative.
func (s *Store) ReconcileRunWorkerHandoffs(ctx context.Context, request ReconcileRunWorkerHandoffsRequest) ([]RunWorkerHandoff, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return nil, err
	}
	runID, actor, reason, err := prepareReconcileRunWorkerHandoffsRequest(request)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := getWorkflowRunTx(ctx, tx, runID); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, runWorkerHandoffV16Select+" WHERE run_id = ? AND state IN ('launching', 'handed_off') ORDER BY created_at, id", runID)
	if err != nil {
		return nil, err
	}
	var active []RunWorkerHandoff
	for rows.Next() {
		handoff, err := scanRunWorkerHandoff(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		active = append(active, handoff)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	now := s.now().UTC()
	changed := make([]RunWorkerHandoff, 0, len(active))
	for _, handoff := range active {
		shouldExpire, err := s.shouldExpireRunWorkerHandoffTx(ctx, tx, handoff, actor, reason, now)
		if err != nil {
			return nil, err
		}
		if !shouldExpire {
			continue
		}
		if err := s.expireRunWorkerHandoffTx(ctx, tx, handoff, actor, reason, now, "scoped run-worker handoff reconciliation"); err != nil {
			return nil, err
		}
		handoff.State = RunWorkerHandoffExpired
		handoff.ReleasedAt = &now
		handoff.UpdatedAt = now
		handoff.Version++
		changed = append(changed, handoff)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return changed, nil
}

func (s *Store) GetRunWorkerHandoff(ctx context.Context, operationID string) (*RunWorkerHandoff, error) {
	if !isUUIDv7(operationID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	handoff, err := scanRunWorkerHandoff(s.db.QueryRowContext(ctx, runWorkerHandoffV16Select+" WHERE id = ?", operationID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &handoff, nil
}

func (s *Store) GetRunWorkerHandoffByIdempotencyKey(ctx context.Context, key string) (*RunWorkerHandoff, error) {
	if !isUUIDv7(strings.TrimSpace(key)) {
		return nil, ErrInvalidUUIDv7Identity
	}
	handoff, err := scanRunWorkerHandoff(s.db.QueryRowContext(ctx, runWorkerHandoffV16Select+" WHERE idempotency_key = ?", strings.TrimSpace(key)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &handoff, nil
}

func (s *Store) ListRunWorkerHandoffsForRun(ctx context.Context, runID string) ([]RunWorkerHandoff, error) {
	if !isUUIDv7(runID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	rows, err := s.db.QueryContext(ctx, runWorkerHandoffV16Select+" WHERE run_id = ? ORDER BY created_at ASC, id ASC", runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	handoffs := make([]RunWorkerHandoff, 0)
	for rows.Next() {
		handoff, err := scanRunWorkerHandoff(rows)
		if err != nil {
			return nil, err
		}
		handoffs = append(handoffs, handoff)
	}
	return handoffs, rows.Err()
}

func (s *Store) reconcileActiveRunWorkerHandoffTx(ctx context.Context, tx *sql.Tx, runID, actor, reason string, now time.Time) error {
	active, err := getActiveRunWorkerHandoffTx(ctx, tx, runID)
	if err != nil || active == nil {
		return err
	}
	shouldExpire, err := s.shouldExpireRunWorkerHandoffTx(ctx, tx, *active, actor, reason, now)
	if err != nil || !shouldExpire {
		return err
	}
	return s.expireRunWorkerHandoffTx(ctx, tx, *active, actor, reason, now, "reserve observed expired handoff")
}

// reconcileUnboundRunWorkerSupervisorLeaseTx prevents a pre-handoff worker
// (including one created before V16) from being bypassed by a new reservation.
// A handoff-bound lease is checked through its active handoff first, so this
// helper only sees an orphaned supervisor lease.
func (s *Store) reconcileUnboundRunWorkerSupervisorLeaseTx(ctx context.Context, tx *sql.Tx, runID, actor, reason string, now time.Time) error {
	lease, err := getActiveLeaseForResourceTx(ctx, tx, RunWorkerSupervisorLeaseResourceType, runID)
	if err != nil || lease == nil {
		return err
	}
	if lease.ExpiresAt.After(now) {
		return fmt.Errorf("%w: %s/%s is owned by %s until %s", ErrLeaseHeld, lease.ResourceType, lease.ResourceID, lease.Owner, lease.ExpiresAt.Format(time.RFC3339))
	}
	return s.expireLeaseTx(ctx, tx, *lease, actor, now, reason+": reserve observed expired unbound supervisor lease")
}

func (s *Store) shouldExpireRunWorkerHandoffTx(ctx context.Context, tx *sql.Tx, handoff RunWorkerHandoff, actor, reason string, now time.Time) (bool, error) {
	switch handoff.State {
	case RunWorkerHandoffLaunching:
		return !handoff.LaunchDeadlineAt.After(now), nil
	case RunWorkerHandoffHandedOff:
		lease, err := getLeaseTx(ctx, tx, handoff.WorkerLeaseID)
		if errors.Is(err, ErrNotFound) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if !runWorkerHandoffLeaseBindingMatches(handoff, lease) {
			// The lease no longer proves that this handoff owns the worker. Do
			// not mutate a mismatched active lease; it may belong to another
			// authoritative recovery path. Expire only this stale handoff.
			return true, nil
		}
		if lease.State != LeaseActive {
			return true, nil
		}
		if lease.ExpiresAt.After(now) {
			return false, nil
		}
		if err := s.expireLeaseTx(ctx, tx, lease, actor, now, reason+": supervisor lease expired"); err != nil {
			return false, err
		}
		return true, nil
	default:
		return false, nil
	}
}

func (s *Store) expireRunWorkerHandoffTx(ctx context.Context, tx *sql.Tx, handoff RunWorkerHandoff, actor, reason string, now time.Time, detail string) error {
	if handoff.State != RunWorkerHandoffLaunching && handoff.State != RunWorkerHandoffHandedOff {
		return nil
	}
	previousVersion := handoff.Version
	result, err := tx.ExecContext(ctx, `
		UPDATE run_worker_handoffs_v16
		SET state = 'expired', released_at = ?, updated_at = ?, version = ?
		WHERE id = ? AND state IN ('launching', 'handed_off') AND version = ?
	`, now, now, previousVersion+1, handoff.ID, previousVersion)
	if err != nil {
		return err
	}
	if err := requireOneRunWorkerHandoffRow(result, "run-worker handoff", handoff.ID); err != nil {
		return err
	}
	handoff.State = RunWorkerHandoffExpired
	handoff.ReleasedAt = &now
	handoff.UpdatedAt = now
	handoff.Version++
	return s.appendRunWorkerHandoffAuditTx(ctx, tx, handoff, "run_worker_handoff.expired", actor, reason, map[string]any{"detail": detail})
}

func acquireRunWorkerSupervisorLeaseTx(ctx context.Context, tx *sql.Tx, request preparedClaimRunWorkerHandoffRequest, handoff RunWorkerHandoff, now time.Time) (Lease, error) {
	existing, err := getActiveLeaseForResourceTx(ctx, tx, RunWorkerSupervisorLeaseResourceType, handoff.RunID)
	if err != nil {
		return Lease{}, err
	}
	if existing != nil {
		if existing.ExpiresAt.After(now) {
			return Lease{}, fmt.Errorf("%w: %s/%s is owned by %s until %s", ErrLeaseHeld, RunWorkerSupervisorLeaseResourceType, handoff.RunID, existing.Owner, existing.ExpiresAt.Format(time.RFC3339))
		}
		if err := request.store.expireLeaseTx(ctx, tx, *existing, request.actor, now, request.reason+": claim observed expired supervisor lease"); err != nil {
			return Lease{}, err
		}
	}
	var maxFencing int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(fencing_token), 0) FROM leases WHERE resource_type = ? AND resource_id = ?`, RunWorkerSupervisorLeaseResourceType, handoff.RunID).Scan(&maxFencing); err != nil {
		return Lease{}, err
	}
	if maxFencing == int64(^uint64(0)>>1) {
		return Lease{}, fmt.Errorf("run-worker supervisor lease fencing token overflow")
	}
	lease := Lease{
		ID: request.leaseID, ResourceType: RunWorkerSupervisorLeaseResourceType, ResourceID: handoff.RunID, Owner: handoff.Owner,
		ExpiresAt: now.Add(request.leaseTTL), FencingToken: uint64(maxFencing + 1), State: LeaseActive,
		CreatedAt: now, UpdatedAt: now, Version: 1,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO leases (id, resource_type, resource_id, owner, job_id, expires_at, fencing_token, state, created_at, updated_at, version)
		VALUES (?, ?, ?, ?, NULL, ?, ?, ?, ?, ?, ?)
	`, lease.ID, lease.ResourceType, lease.ResourceID, lease.Owner, lease.ExpiresAt, int64(lease.FencingToken), lease.State, lease.CreatedAt, lease.UpdatedAt, lease.Version); err != nil {
		if isGlobalIdentityCollision(err) {
			return Lease{}, fmt.Errorf("%w: run-worker supervisor lease %s", ErrIdentityCollision, lease.ID)
		}
		if isUniqueConstraint(err) {
			return Lease{}, fmt.Errorf("%w: %s/%s", ErrLeaseHeld, lease.ResourceType, lease.ResourceID)
		}
		return Lease{}, err
	}
	return lease, nil
}

func recordRunWorkerHandoffSpawnedTx(ctx context.Context, tx *sql.Tx, handoff RunWorkerHandoff, request preparedRecordRunWorkerHandoffSpawnedRequest, now time.Time) (RunWorkerHandoff, bool, error) {
	if handoff.ProcessID != 0 {
		if handoff.ProcessID != request.processID || handoff.LogPath != request.logPath {
			return RunWorkerHandoff{}, false, fmt.Errorf("%w: run-worker handoff %s process receipt", ErrIdempotencyConflict, handoff.ID)
		}
		return handoff, false, nil
	}
	if handoff.State != RunWorkerHandoffLaunching && handoff.State != RunWorkerHandoffHandedOff {
		return RunWorkerHandoff{}, false, fmt.Errorf("%w: run-worker handoff %s is %s", ErrImmutable, handoff.ID, handoff.State)
	}
	receipt, err := encodeRunWorkerHandoffReceipt(handoff, request.processID, request.logPath, now)
	if err != nil {
		return RunWorkerHandoff{}, false, err
	}
	previousVersion := handoff.Version
	handoff.ProcessID = request.processID
	handoff.LogPath = request.logPath
	handoff.ReceiptJSON = receipt
	handoff.SpawnedAt = &now
	handoff.UpdatedAt = now
	handoff.Version++
	result, err := tx.ExecContext(ctx, `
		UPDATE run_worker_handoffs_v16
		SET process_id = ?, log_path = ?, receipt_json = ?, spawned_at = ?, updated_at = ?, version = ?
		WHERE id = ? AND process_id = 0 AND version = ?
	`, handoff.ProcessID, handoff.LogPath, handoff.ReceiptJSON, handoff.SpawnedAt, handoff.UpdatedAt, handoff.Version, handoff.ID, previousVersion)
	if err != nil {
		return RunWorkerHandoff{}, false, err
	}
	if err := requireOneRunWorkerHandoffRow(result, "run-worker handoff", handoff.ID); err != nil {
		return RunWorkerHandoff{}, false, err
	}
	return handoff, true, nil
}

func encodeRunWorkerHandoffReceipt(handoff RunWorkerHandoff, processID int, logPath string, spawnedAt time.Time) (string, error) {
	payload := struct {
		Format      string    `json:"format"`
		OperationID string    `json:"operation_id"`
		RunID       string    `json:"run_id"`
		Owner       string    `json:"owner"`
		ProcessID   int       `json:"pid"`
		LogPath     string    `json:"log_path"`
		SpawnedAt   time.Time `json:"spawned_at"`
	}{
		Format: runWorkerHandoffReceiptFormat, OperationID: handoff.ID, RunID: handoff.RunID, Owner: handoff.Owner,
		ProcessID: processID, LogPath: logPath, SpawnedAt: spawnedAt,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return normalizeJSON(string(raw), "run-worker handoff receipt")
}

func insertRunWorkerHandoffTx(ctx context.Context, tx *sql.Tx, handoff RunWorkerHandoff) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO run_worker_handoffs_v16 (
			id, idempotency_key, request_fingerprint, run_id, expected_run_version, expected_run_execution_epoch,
			expected_run_definition_hash, owner, actor, reason, state, launch_deadline_at, worker_lease_id,
			worker_lease_owner, worker_lease_fencing_token, worker_lease_version, process_id, log_path,
			receipt_json, failure_reason, created_at, updated_at, spawned_at, handed_off_at, released_at, version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', '', 0, 0, 0, '', '', '', ?, ?, NULL, NULL, NULL, ?)
	`, handoff.ID, handoff.IdempotencyKey, handoff.RequestFingerprint, handoff.RunID, handoff.ExpectedRunVersion,
		handoff.ExpectedRunExecutionEpoch, handoff.ExpectedRunDefinitionHash, handoff.Owner, handoff.Actor, handoff.Reason,
		handoff.State, handoff.LaunchDeadlineAt, handoff.CreatedAt, handoff.UpdatedAt, handoff.Version)
	if isGlobalIdentityCollision(err) {
		return fmt.Errorf("%w: run-worker handoff %s", ErrIdentityCollision, handoff.ID)
	}
	if isUniqueConstraint(err) {
		if strings.Contains(strings.ToLower(err.Error()), "run_worker_handoffs_v16.run_id") {
			return fmt.Errorf("%w: active run-worker handoff for run %s", ErrLeaseHeld, handoff.RunID)
		}
		return fmt.Errorf("%w: run-worker handoff key %s", ErrIdempotencyConflict, handoff.IdempotencyKey)
	}
	return err
}

func getRunWorkerHandoffTx(ctx context.Context, tx *sql.Tx, operationID string) (RunWorkerHandoff, error) {
	handoff, err := scanRunWorkerHandoff(tx.QueryRowContext(ctx, runWorkerHandoffV16Select+" WHERE id = ?", operationID))
	if errors.Is(err, sql.ErrNoRows) {
		return RunWorkerHandoff{}, fmt.Errorf("%w: run-worker handoff %s", ErrNotFound, operationID)
	}
	return handoff, err
}

func getRunWorkerHandoffByKeyTx(ctx context.Context, tx *sql.Tx, key string) (*RunWorkerHandoff, error) {
	handoff, err := scanRunWorkerHandoff(tx.QueryRowContext(ctx, runWorkerHandoffV16Select+" WHERE idempotency_key = ?", key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &handoff, nil
}

func getActiveRunWorkerHandoffTx(ctx context.Context, tx *sql.Tx, runID string) (*RunWorkerHandoff, error) {
	handoff, err := scanRunWorkerHandoff(tx.QueryRowContext(ctx, runWorkerHandoffV16Select+" WHERE run_id = ? AND state IN ('launching', 'handed_off')", runID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &handoff, nil
}

func scanRunWorkerHandoff(scanner rowScanner) (RunWorkerHandoff, error) {
	var handoff RunWorkerHandoff
	var fencing int64
	var spawnedAt, handedOffAt, releasedAt sql.NullTime
	if err := scanner.Scan(
		&handoff.ID, &handoff.IdempotencyKey, &handoff.RequestFingerprint, &handoff.RunID, &handoff.ExpectedRunVersion,
		&handoff.ExpectedRunExecutionEpoch, &handoff.ExpectedRunDefinitionHash, &handoff.Owner, &handoff.Actor,
		&handoff.Reason, &handoff.State, &handoff.LaunchDeadlineAt, &handoff.WorkerLeaseID, &handoff.WorkerLeaseOwner,
		&fencing, &handoff.WorkerLeaseVersion, &handoff.ProcessID, &handoff.LogPath, &handoff.ReceiptJSON,
		&handoff.FailureReason, &handoff.CreatedAt, &handoff.UpdatedAt, &spawnedAt, &handedOffAt, &releasedAt, &handoff.Version,
	); err != nil {
		return RunWorkerHandoff{}, err
	}
	if fencing < 0 {
		return RunWorkerHandoff{}, fmt.Errorf("invalid persisted run-worker handoff fencing token for %s", handoff.ID)
	}
	handoff.WorkerLeaseFencingToken = uint64(fencing)
	handoff.LaunchDeadlineAt = handoff.LaunchDeadlineAt.UTC()
	handoff.CreatedAt = handoff.CreatedAt.UTC()
	handoff.UpdatedAt = handoff.UpdatedAt.UTC()
	handoff.SpawnedAt = nullableTimePtr(spawnedAt)
	handoff.HandedOffAt = nullableTimePtr(handedOffAt)
	handoff.ReleasedAt = nullableTimePtr(releasedAt)
	return handoff, nil
}

type preparedReserveRunWorkerHandoffRequest struct {
	id                        string
	idempotencyKey            string
	requestFingerprint        string
	runID                     string
	expectedRunVersion        int64
	expectedRunExecutionEpoch int
	expectedRunDefinitionHash string
	owner                     string
	actor                     string
	reason                    string
	launchTTL                 time.Duration
}

func prepareReserveRunWorkerHandoffRequest(s *Store, request ReserveRunWorkerHandoffRequest) (preparedReserveRunWorkerHandoffRequest, error) {
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return preparedReserveRunWorkerHandoffRequest{}, err
	}
	key := strings.TrimSpace(request.IdempotencyKey)
	runID := strings.TrimSpace(request.RunID)
	if !isUUIDv7(key) || !isUUIDv7(runID) {
		return preparedReserveRunWorkerHandoffRequest{}, ErrInvalidUUIDv7Identity
	}
	fingerprint, err := normalizeRequired(request.RequestFingerprint, "run-worker handoff request fingerprint")
	if err != nil {
		return preparedReserveRunWorkerHandoffRequest{}, err
	}
	definitionHash, err := normalizeRequired(request.ExpectedRunDefinitionHash, "run-worker handoff expected run definition hash")
	if err != nil {
		return preparedReserveRunWorkerHandoffRequest{}, err
	}
	owner, err := normalizeRequired(request.Owner, "run-worker handoff owner")
	if err != nil {
		return preparedReserveRunWorkerHandoffRequest{}, err
	}
	actor, err := normalizeRequired(request.Actor, "run-worker handoff actor")
	if err != nil {
		return preparedReserveRunWorkerHandoffRequest{}, err
	}
	reason, err := normalizeRequired(request.Reason, "run-worker handoff reason")
	if err != nil {
		return preparedReserveRunWorkerHandoffRequest{}, err
	}
	if request.ExpectedRunVersion <= 0 || request.ExpectedRunExecutionEpoch < 0 {
		return preparedReserveRunWorkerHandoffRequest{}, fmt.Errorf("run-worker handoff expected Run checkpoint is invalid")
	}
	ttl := request.LaunchTTL
	if ttl == 0 {
		ttl = DefaultLeaseTTL
	}
	if ttl <= 0 {
		return preparedReserveRunWorkerHandoffRequest{}, fmt.Errorf("run-worker handoff launch TTL must be positive")
	}
	return preparedReserveRunWorkerHandoffRequest{
		id: id, idempotencyKey: key, requestFingerprint: fingerprint, runID: runID, expectedRunVersion: request.ExpectedRunVersion,
		expectedRunExecutionEpoch: request.ExpectedRunExecutionEpoch, expectedRunDefinitionHash: definitionHash, owner: owner,
		actor: actor, reason: reason, launchTTL: ttl,
	}, nil
}

type preparedRecordRunWorkerHandoffSpawnedRequest struct {
	operationID string
	processID   int
	logPath     string
	actor       string
	reason      string
}

func prepareRecordRunWorkerHandoffSpawnedRequest(request RecordRunWorkerHandoffSpawnedRequest) (preparedRecordRunWorkerHandoffSpawnedRequest, error) {
	if !isUUIDv7(strings.TrimSpace(request.OperationID)) {
		return preparedRecordRunWorkerHandoffSpawnedRequest{}, ErrInvalidUUIDv7Identity
	}
	if request.ProcessID <= 0 {
		return preparedRecordRunWorkerHandoffSpawnedRequest{}, fmt.Errorf("run-worker child process ID must be positive")
	}
	logPath, err := normalizeRequired(request.LogPath, "run-worker child log path")
	if err != nil {
		return preparedRecordRunWorkerHandoffSpawnedRequest{}, err
	}
	actor, err := normalizeRequired(request.Actor, "run-worker handoff actor")
	if err != nil {
		return preparedRecordRunWorkerHandoffSpawnedRequest{}, err
	}
	reason, err := normalizeRequired(request.Reason, "run-worker handoff reason")
	if err != nil {
		return preparedRecordRunWorkerHandoffSpawnedRequest{}, err
	}
	return preparedRecordRunWorkerHandoffSpawnedRequest{operationID: strings.TrimSpace(request.OperationID), processID: request.ProcessID, logPath: logPath, actor: actor, reason: reason}, nil
}

type preparedFailRunWorkerHandoffRequest struct {
	operationID string
	failure     string
	actor       string
	reason      string
}

func prepareFailRunWorkerHandoffRequest(request FailRunWorkerHandoffRequest) (preparedFailRunWorkerHandoffRequest, error) {
	if !isUUIDv7(strings.TrimSpace(request.OperationID)) {
		return preparedFailRunWorkerHandoffRequest{}, ErrInvalidUUIDv7Identity
	}
	failure, err := normalizeRequired(request.Failure, "run-worker handoff failure")
	if err != nil {
		return preparedFailRunWorkerHandoffRequest{}, err
	}
	actor, err := normalizeRequired(request.Actor, "run-worker handoff actor")
	if err != nil {
		return preparedFailRunWorkerHandoffRequest{}, err
	}
	reason, err := normalizeRequired(request.Reason, "run-worker handoff reason")
	if err != nil {
		return preparedFailRunWorkerHandoffRequest{}, err
	}
	return preparedFailRunWorkerHandoffRequest{operationID: strings.TrimSpace(request.OperationID), failure: failure, actor: actor, reason: reason}, nil
}

type preparedClaimRunWorkerHandoffRequest struct {
	store       *Store
	operationID string
	leaseID     string
	runID       string
	owner       string
	processID   int
	logPath     string
	leaseTTL    time.Duration
	actor       string
	reason      string
}

func prepareClaimRunWorkerHandoffRequest(s *Store, request ClaimRunWorkerHandoffRequest) (preparedClaimRunWorkerHandoffRequest, error) {
	if !isUUIDv7(strings.TrimSpace(request.OperationID)) || !isUUIDv7(strings.TrimSpace(request.RunID)) {
		return preparedClaimRunWorkerHandoffRequest{}, ErrInvalidUUIDv7Identity
	}
	leaseID, err := s.newV2ID("")
	if err != nil {
		return preparedClaimRunWorkerHandoffRequest{}, err
	}
	owner, err := normalizeRequired(request.Owner, "run-worker handoff owner")
	if err != nil {
		return preparedClaimRunWorkerHandoffRequest{}, err
	}
	if request.ProcessID <= 0 {
		return preparedClaimRunWorkerHandoffRequest{}, fmt.Errorf("run-worker child process ID must be positive")
	}
	logPath, err := normalizeRequired(request.LogPath, "run-worker child log path")
	if err != nil {
		return preparedClaimRunWorkerHandoffRequest{}, err
	}
	actor, err := normalizeRequired(request.Actor, "run-worker handoff actor")
	if err != nil {
		return preparedClaimRunWorkerHandoffRequest{}, err
	}
	reason, err := normalizeRequired(request.Reason, "run-worker handoff reason")
	if err != nil {
		return preparedClaimRunWorkerHandoffRequest{}, err
	}
	ttl := request.LeaseTTL
	if ttl == 0 {
		ttl = DefaultLeaseTTL
	}
	if ttl <= 0 {
		return preparedClaimRunWorkerHandoffRequest{}, fmt.Errorf("run-worker supervisor lease TTL must be positive")
	}
	return preparedClaimRunWorkerHandoffRequest{store: s, operationID: strings.TrimSpace(request.OperationID), leaseID: leaseID, runID: strings.TrimSpace(request.RunID), owner: owner, processID: request.ProcessID, logPath: logPath, leaseTTL: ttl, actor: actor, reason: reason}, nil
}

type preparedReleaseRunWorkerHandoffRequest struct {
	operationID string
	workerLease Lease
	actor       string
	reason      string
}

func prepareReleaseRunWorkerHandoffRequest(request ReleaseRunWorkerHandoffRequest) (preparedReleaseRunWorkerHandoffRequest, error) {
	if !isUUIDv7(strings.TrimSpace(request.OperationID)) || !isUUIDv7(request.WorkerLease.ID) || request.WorkerLease.Version <= 0 || request.WorkerLease.FencingToken == 0 {
		return preparedReleaseRunWorkerHandoffRequest{}, ErrInvalidUUIDv7Identity
	}
	actor, err := normalizeRequired(request.Actor, "run-worker handoff actor")
	if err != nil {
		return preparedReleaseRunWorkerHandoffRequest{}, err
	}
	reason, err := normalizeRequired(request.Reason, "run-worker handoff reason")
	if err != nil {
		return preparedReleaseRunWorkerHandoffRequest{}, err
	}
	return preparedReleaseRunWorkerHandoffRequest{operationID: strings.TrimSpace(request.OperationID), workerLease: request.WorkerLease, actor: actor, reason: reason}, nil
}

func prepareReconcileRunWorkerHandoffsRequest(request ReconcileRunWorkerHandoffsRequest) (string, string, string, error) {
	runID := strings.TrimSpace(request.RunID)
	if !isUUIDv7(runID) {
		return "", "", "", ErrInvalidUUIDv7Identity
	}
	actor, err := normalizeRequired(request.Actor, "run-worker handoff actor")
	if err != nil {
		return "", "", "", err
	}
	reason, err := normalizeRequired(request.Reason, "run-worker handoff reason")
	if err != nil {
		return "", "", "", err
	}
	return runID, actor, reason, nil
}

func validateRunWorkerHandoffCheckpoint(run WorkflowRun, request preparedReserveRunWorkerHandoffRequest) error {
	if run.Version != request.expectedRunVersion || run.ExecutionEpoch != request.expectedRunExecutionEpoch || run.DefinitionHash != request.expectedRunDefinitionHash {
		return fmt.Errorf("%w: workflow run %s handoff checkpoint is stale", ErrOptimisticLock, run.ID)
	}
	return nil
}

func validatePersistedRunWorkerHandoffCheckpoint(run WorkflowRun, handoff RunWorkerHandoff) error {
	if run.Version != handoff.ExpectedRunVersion || run.ExecutionEpoch != handoff.ExpectedRunExecutionEpoch || run.DefinitionHash != handoff.ExpectedRunDefinitionHash {
		return fmt.Errorf("%w: workflow run %s handoff checkpoint is stale", ErrOptimisticLock, run.ID)
	}
	return nil
}

func sameRunWorkerHandoffReserve(existing RunWorkerHandoff, request preparedReserveRunWorkerHandoffRequest) bool {
	return existing.IdempotencyKey == request.idempotencyKey && existing.RequestFingerprint == request.requestFingerprint &&
		existing.RunID == request.runID && existing.ExpectedRunVersion == request.expectedRunVersion &&
		existing.ExpectedRunExecutionEpoch == request.expectedRunExecutionEpoch && existing.ExpectedRunDefinitionHash == request.expectedRunDefinitionHash &&
		existing.Owner == request.owner && existing.Actor == request.actor && existing.Reason == request.reason
}

func sameRunWorkerHandoffLease(handoff RunWorkerHandoff, lease Lease) bool {
	return handoff.WorkerLeaseID == lease.ID && handoff.WorkerLeaseOwner == lease.Owner && handoff.WorkerLeaseFencingToken == lease.FencingToken
}

func runWorkerHandoffLeaseBindingMatches(handoff RunWorkerHandoff, lease Lease) bool {
	return handoff.WorkerLeaseID != "" && handoff.WorkerLeaseOwner != "" && handoff.WorkerLeaseFencingToken != 0 && handoff.WorkerLeaseVersion > 0 &&
		handoff.WorkerLeaseOwner == handoff.Owner && lease.ID == handoff.WorkerLeaseID &&
		lease.ResourceType == RunWorkerSupervisorLeaseResourceType && lease.ResourceID == handoff.RunID &&
		lease.Owner == handoff.WorkerLeaseOwner && lease.FencingToken == handoff.WorkerLeaseFencingToken &&
		lease.Version >= handoff.WorkerLeaseVersion
}

func runWorkerHandoffRunnable(status WorkflowRunStatus) bool {
	switch status {
	case WorkflowRunQueued, WorkflowRunRunning, WorkflowRunPauseRequested, WorkflowRunPausing,
		WorkflowRunResumeRequested, WorkflowRunCancelRequested, WorkflowRunStopRequested, WorkflowRunCanceling:
		return true
	default:
		return false
	}
}

func handoffHeldError(handoff RunWorkerHandoff) error {
	return fmt.Errorf("%w: run-worker handoff %s for run %s is %s", ErrLeaseHeld, handoff.ID, handoff.RunID, handoff.State)
}

func handoffStateClaimError(handoff RunWorkerHandoff) error {
	if handoff.State == RunWorkerHandoffHandedOff {
		return handoffHeldError(handoff)
	}
	return fmt.Errorf("%w: run-worker handoff %s is %s", ErrInvalidTransition, handoff.ID, handoff.State)
}

func requireOneRunWorkerHandoffRow(result sql.Result, entity, id string) error {
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("%w: %s %s", ErrOptimisticLock, entity, id)
	}
	return nil
}

func (s *Store) appendRunWorkerHandoffAuditTx(ctx context.Context, tx *sql.Tx, handoff RunWorkerHandoff, action, actor, reason string, payload map[string]any) error {
	_, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: actor, EntityType: "run_worker_handoff", EntityID: handoff.ID, Action: action, Reason: reason,
		OperationKey: handoff.IdempotencyKey, PayloadJSON: auditPayload(payload), CreatedAt: s.now().UTC(),
	})
	return err
}
