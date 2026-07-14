package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const capacityPoolV5Select = `
	SELECT id, pool_key, capacity, created_at, updated_at, version
	FROM capacity_pools_v5`

const durableJobDispatchClaimV5Select = `
	SELECT id, idempotency_key, run_id, job_id, owner, lease_ttl_ms, dispatch_lease_id,
	       capacity_pool_key, capacity_lease_id, state, claimed_at, updated_at
	FROM job_dispatch_claims_v5`

func (s *Store) ConfigureCapacityPool(ctx context.Context, request ConfigureCapacityPoolRequest) (CapacityPool, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return CapacityPool{}, err
	}
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return CapacityPool{}, err
	}
	poolKey, err := normalizeRequired(request.PoolKey, "capacity pool key")
	if err != nil {
		return CapacityPool{}, fmt.Errorf("%w: %v", ErrInvalidDispatch, err)
	}
	if request.Capacity <= 0 {
		return CapacityPool{}, fmt.Errorf("%w: capacity must be positive", ErrInvalidDispatch)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CapacityPool{}, err
	}
	defer tx.Rollback()
	existing, err := getCapacityPoolByKeyTx(ctx, tx, poolKey)
	if err != nil {
		return CapacityPool{}, err
	}
	now := s.now().UTC()
	if existing == nil {
		if request.ExpectedVersion != 0 {
			return CapacityPool{}, fmt.Errorf("%w: new capacity pool must use expected version zero", ErrOptimisticLock)
		}
		pool := CapacityPool{ID: id, PoolKey: poolKey, Capacity: request.Capacity, CreatedAt: now, UpdatedAt: now, Version: 1}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO capacity_pools_v5 (id, pool_key, capacity, created_at, updated_at, version)
			VALUES (?, ?, ?, ?, ?, ?)
		`, pool.ID, pool.PoolKey, pool.Capacity, pool.CreatedAt, pool.UpdatedAt, pool.Version); err != nil {
			if isUniqueConstraint(err) {
				return CapacityPool{}, fmt.Errorf("%w: capacity pool %s", ErrIdentityCollision, pool.PoolKey)
			}
			return CapacityPool{}, err
		}
		if _, err := s.appendAuditTx(ctx, tx, AuditEvent{Actor: request.Actor, EntityType: "capacity_pool", EntityID: pool.ID, Action: "capacity_pool.created", Reason: request.Reason, PayloadJSON: auditPayload(map[string]any{"pool_key": pool.PoolKey, "capacity": pool.Capacity}), CreatedAt: now}); err != nil {
			return CapacityPool{}, err
		}
		if err := tx.Commit(); err != nil {
			return CapacityPool{}, err
		}
		return pool, nil
	}
	if request.ExpectedVersion != existing.Version {
		return CapacityPool{}, fmt.Errorf("%w: capacity pool %s", ErrOptimisticLock, existing.ID)
	}
	existing.Capacity = request.Capacity
	existing.UpdatedAt = now
	existing.Version++
	result, err := tx.ExecContext(ctx, `
		UPDATE capacity_pools_v5 SET capacity = ?, updated_at = ?, version = ?
		WHERE id = ? AND version = ?
	`, existing.Capacity, existing.UpdatedAt, existing.Version, existing.ID, request.ExpectedVersion)
	if err != nil {
		return CapacityPool{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return CapacityPool{}, err
	}
	if changed != 1 {
		return CapacityPool{}, fmt.Errorf("%w: capacity pool %s", ErrOptimisticLock, existing.ID)
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{Actor: request.Actor, EntityType: "capacity_pool", EntityID: existing.ID, Action: "capacity_pool.updated", Reason: request.Reason, PayloadJSON: auditPayload(map[string]any{"pool_key": existing.PoolKey, "capacity": existing.Capacity, "version": existing.Version}), CreatedAt: now}); err != nil {
		return CapacityPool{}, err
	}
	if err := tx.Commit(); err != nil {
		return CapacityPool{}, err
	}
	return *existing, nil
}

func (s *Store) GetCapacityPool(ctx context.Context, poolKey string) (*CapacityPool, error) {
	poolKey, err := normalizeRequired(poolKey, "capacity pool key")
	if err != nil {
		return nil, err
	}
	pool, err := scanCapacityPool(s.db.QueryRowContext(ctx, capacityPoolV5Select+" WHERE pool_key = ?", poolKey))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &pool, nil
}

func (s *Store) ListQueuedDurableJobs(ctx context.Context, request ListQueuedDurableJobsRequest) ([]DurableJob, error) {
	limit := request.Limit
	if limit < 0 {
		return nil, fmt.Errorf("%w: queued job limit cannot be negative", ErrInvalidDispatch)
	}
	query := durableJobSelect + " WHERE state = 'queued' ORDER BY priority DESC, created_at ASC, id ASC"
	args := []any(nil)
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []DurableJob
	for rows.Next() {
		job, err := scanDurableJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (s *Store) ClaimNextDurableJob(ctx context.Context, request ClaimNextDurableJobRequest) (DurableJobDispatchClaim, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return DurableJobDispatchClaim{}, err
	}
	prepared, err := prepareDurableJobClaim(s, request)
	if err != nil {
		return DurableJobDispatchClaim{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DurableJobDispatchClaim{}, err
	}
	defer tx.Rollback()
	if existing, err := getDurableJobDispatchClaimByKeyTx(ctx, tx, prepared.IdempotencyKey); err != nil {
		return DurableJobDispatchClaim{}, err
	} else if existing != nil {
		if existing.Owner != prepared.Owner || existing.RunID != prepared.RunID || existing.LeaseTTL != time.Duration(durationMilliseconds(prepared.LeaseTTL))*time.Millisecond || existing.CapacityPoolKey != prepared.CapacityPoolKey {
			return DurableJobDispatchClaim{}, fmt.Errorf("%w: durable job claim key %s", ErrIdempotencyConflict, prepared.IdempotencyKey)
		}
		if err := s.loadDurableJobDispatchClaimTx(ctx, tx, existing); err != nil {
			return DurableJobDispatchClaim{}, err
		}
		if existing.Job != nil && existing.RunID != "" && existing.Job.RunID != prepared.RunID {
			return DurableJobDispatchClaim{}, fmt.Errorf("%w: durable job claim key %s", ErrIdempotencyConflict, prepared.IdempotencyKey)
		}
		if err := tx.Commit(); err != nil {
			return DurableJobDispatchClaim{}, err
		}
		return *existing, nil
	}
	var pool *CapacityPool
	if prepared.CapacityPoolKey != "" {
		pool, err = getCapacityPoolByKeyTx(ctx, tx, prepared.CapacityPoolKey)
		if err != nil {
			return DurableJobDispatchClaim{}, err
		}
		if pool == nil {
			return DurableJobDispatchClaim{}, fmt.Errorf("%w: %s", ErrCapacityNotConfigured, prepared.CapacityPoolKey)
		}
	}
	now := s.now().UTC()
	job, err := selectNextQueuedDurableJobTx(ctx, tx, now, prepared.RunID)
	if err != nil {
		return DurableJobDispatchClaim{}, err
	}
	if job == nil {
		claim := DurableJobDispatchClaim{ID: prepared.ID, IdempotencyKey: prepared.IdempotencyKey, RunID: prepared.RunID, Owner: prepared.Owner, LeaseTTL: prepared.LeaseTTL, CapacityPoolKey: prepared.CapacityPoolKey, State: "empty", ClaimedAt: now, UpdatedAt: now}
		if err := insertDurableJobDispatchClaimTx(ctx, tx, claim); err != nil {
			return DurableJobDispatchClaim{}, err
		}
		if err := tx.Commit(); err != nil {
			return DurableJobDispatchClaim{}, err
		}
		return claim, nil
	}
	var capacityLease *Lease
	if pool != nil {
		capacityLease, err = s.acquireCapacityLeaseTx(ctx, tx, *pool, job.ID, prepared.Owner, prepared.LeaseTTL, prepared.Actor, prepared.Reason, now)
		if err != nil {
			return DurableJobDispatchClaim{}, err
		}
	}
	dispatchLease, err := s.acquireV5LeaseTx(ctx, tx, "job_dispatch", job.ID, prepared.Owner, job.ID, prepared.LeaseTTL, prepared.Actor, prepared.Reason, now)
	if err != nil {
		return DurableJobDispatchClaim{}, err
	}
	job.State = JobRunning
	job.UpdatedAt = now
	job.StartedAt = &now
	job.Version++
	result, err := tx.ExecContext(ctx, `
		UPDATE jobs SET state = ?, updated_at = ?, started_at = ?, version = ?
		WHERE id = ? AND state = 'queued' AND version = ?
	`, job.State, job.UpdatedAt, job.StartedAt, job.Version, job.ID, job.Version-1)
	if err != nil {
		return DurableJobDispatchClaim{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return DurableJobDispatchClaim{}, err
	}
	if changed != 1 {
		return DurableJobDispatchClaim{}, fmt.Errorf("%w: durable job %s", ErrOptimisticLock, job.ID)
	}
	claim := DurableJobDispatchClaim{ID: prepared.ID, IdempotencyKey: prepared.IdempotencyKey, RunID: prepared.RunID, Job: job, Owner: prepared.Owner, LeaseTTL: prepared.LeaseTTL,
		DispatchLease: &dispatchLease, CapacityPoolKey: prepared.CapacityPoolKey, CapacityLease: capacityLease, State: "active", ClaimedAt: now, UpdatedAt: now}
	if err := insertDurableJobDispatchClaimTx(ctx, tx, claim); err != nil {
		return DurableJobDispatchClaim{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{Actor: prepared.Actor, EntityType: "job", EntityID: job.ID, Action: "job.claimed", Reason: prepared.Reason,
		PayloadJSON: auditPayload(map[string]any{"claim_id": claim.ID, "owner": claim.Owner, "dispatch_fencing_token": dispatchLease.FencingToken, "capacity_pool": claim.CapacityPoolKey}), OperationKey: claim.IdempotencyKey, CreatedAt: now}); err != nil {
		return DurableJobDispatchClaim{}, err
	}
	if err := tx.Commit(); err != nil {
		return DurableJobDispatchClaim{}, err
	}
	return claim, nil
}

// ScanExpiredDurableJobsForReconcile detects a lost worker fence. It never
// restarts work: it marks the job interrupted, releases any still-active job
// leases (including capacity slots), and makes unfinished control commands
// explicitly reconcile-required for the application service.
func (s *Store) ScanExpiredDurableJobsForReconcile(ctx context.Context, request ScanExpiredDurableJobsRequest) ([]ExpiredDurableJobRecovery, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return nil, err
	}
	if request.Limit < 0 {
		return nil, fmt.Errorf("%w: recovery scan limit cannot be negative", ErrInvalidDispatch)
	}
	if strings.TrimSpace(request.RunID) != "" && !isUUIDv7(request.RunID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	limit := request.Limit
	if limit == 0 {
		limit = 100
	}
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	query := durableJobDispatchClaimV5Select + `
		WHERE state = 'active' AND dispatch_lease_id IN (
			SELECT id FROM leases WHERE state = 'active' AND expires_at <= ?
		)`
	args := []any{now}
	if runID := strings.TrimSpace(request.RunID); runID != "" {
		query += ` AND job_id IN (SELECT id FROM jobs WHERE run_id = ?)`
		args = append(args, runID)
	}
	query += ` ORDER BY claimed_at ASC, id ASC LIMIT ?`
	args = append(args, limit)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var claims []DurableJobDispatchClaim
	for rows.Next() {
		claim, err := scanDurableJobDispatchClaim(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		claims = append(claims, claim)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var recoveries []ExpiredDurableJobRecovery
	for _, claim := range claims {
		if err := s.loadDurableJobDispatchClaimTx(ctx, tx, &claim); err != nil {
			return nil, err
		}
		if claim.Job == nil || claim.DispatchLease == nil {
			return nil, fmt.Errorf("%w: active dispatch claim %s is incomplete", ErrInvalidDispatch, claim.ID)
		}
		if claim.DispatchLease.State != LeaseActive || claim.DispatchLease.ExpiresAt.After(now) {
			continue
		}
		if err := s.expireLeaseTx(ctx, tx, *claim.DispatchLease, resolveActor(request.Actor), now, "recovery observed expired job dispatch lease"); err != nil {
			return nil, err
		}
		// The dispatch fence is the loss signal. Keep a paired capacity lease
		// active until the terminal job projection below releases every active
		// job lease in one place, even when its own TTL elapsed concurrently.
		job := *claim.Job
		if !isTerminalJobState(job.State) {
			job.State = JobInterrupted
			job.UpdatedAt = now
			job.FinishedAt = &now
			job.Version++
			result, err := tx.ExecContext(ctx, `
				UPDATE jobs SET state = ?, updated_at = ?, finished_at = ?, version = ?
				WHERE id = ? AND version = ?
			`, job.State, job.UpdatedAt, job.FinishedAt, job.Version, job.ID, job.Version-1)
			if err != nil {
				return nil, err
			}
			changed, err := result.RowsAffected()
			if err != nil {
				return nil, err
			}
			if changed != 1 {
				return nil, fmt.Errorf("%w: durable job %s", ErrOptimisticLock, job.ID)
			}
			if err := s.releaseJobLeasesTx(ctx, tx, job.ID, resolveActor(request.Actor), now); err != nil {
				return nil, err
			}
		}
		claim.State = "expired"
		claim.UpdatedAt = now
		if _, err := tx.ExecContext(ctx, `UPDATE job_dispatch_claims_v5 SET state = ?, updated_at = ? WHERE id = ? AND state = 'active'`, claim.State, claim.UpdatedAt, claim.ID); err != nil {
			return nil, err
		}
		operations, err := s.markPendingControlsForReconcileTx(ctx, tx, job.RunID, resolveActor(request.Actor), request.Reason, now)
		if err != nil {
			return nil, err
		}
		claim.Job = &job
		recoveries = append(recoveries, ExpiredDurableJobRecovery{Claim: claim, Job: job, Operations: operations})
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return recoveries, nil
}

type preparedDurableJobClaim struct {
	ID              string
	IdempotencyKey  string
	Owner           string
	RunID           string
	LeaseTTL        time.Duration
	CapacityPoolKey string
	Actor           string
	Reason          string
}

func prepareDurableJobClaim(s *Store, request ClaimNextDurableJobRequest) (preparedDurableJobClaim, error) {
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return preparedDurableJobClaim{}, err
	}
	key, err := normalizeRequired(request.IdempotencyKey, "durable job claim idempotency key")
	if err != nil {
		return preparedDurableJobClaim{}, fmt.Errorf("%w: %v", ErrInvalidDispatch, err)
	}
	owner, err := normalizeRequired(request.Owner, "durable job claim owner")
	if err != nil {
		return preparedDurableJobClaim{}, fmt.Errorf("%w: %v", ErrInvalidDispatch, err)
	}
	if request.LeaseTTL <= 0 {
		return preparedDurableJobClaim{}, fmt.Errorf("%w: durable job claim ttl must be positive", ErrInvalidDispatch)
	}
	runID := strings.TrimSpace(request.RunID)
	if runID != "" && !isUUIDv7(runID) {
		return preparedDurableJobClaim{}, ErrInvalidUUIDv7Identity
	}
	return preparedDurableJobClaim{ID: id, IdempotencyKey: key, Owner: owner, LeaseTTL: request.LeaseTTL,
		RunID: runID, CapacityPoolKey: strings.TrimSpace(request.CapacityPoolKey), Actor: resolveActor(request.Actor), Reason: strings.TrimSpace(request.Reason)}, nil
}

func getCapacityPoolByKeyTx(ctx context.Context, tx *sql.Tx, poolKey string) (*CapacityPool, error) {
	pool, err := scanCapacityPool(tx.QueryRowContext(ctx, capacityPoolV5Select+" WHERE pool_key = ?", poolKey))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &pool, nil
}

func selectNextQueuedDurableJobTx(ctx context.Context, tx *sql.Tx, now time.Time, runID string) (*DurableJob, error) {
	query := durableJobSelect + `
		WHERE state = 'queued'
		  AND NOT EXISTS (
			SELECT 1 FROM leases
			WHERE resource_type = 'job_dispatch' AND resource_id = jobs.id
			  AND state = 'active' AND expires_at > ?
		  )`
	args := []any{now}
	if runID != "" {
		query += ` AND run_id = ?`
		args = append(args, runID)
	}
	query += ` ORDER BY priority DESC, created_at ASC, id ASC LIMIT 1`
	job, err := scanDurableJob(tx.QueryRowContext(ctx, query, args...))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &job, nil
}

func (s *Store) acquireCapacityLeaseTx(ctx context.Context, tx *sql.Tx, pool CapacityPool, jobID, owner string, ttl time.Duration, actor, reason string, now time.Time) (*Lease, error) {
	for slot := 1; slot <= pool.Capacity; slot++ {
		resourceID := capacitySlotResourceID(pool.PoolKey, slot)
		existing, err := getActiveLeaseForResourceTx(ctx, tx, "capacity_slot", resourceID)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			if existing.ExpiresAt.After(now) {
				continue
			}
			if err := s.expireLeaseTx(ctx, tx, *existing, actor, now, "dispatch observed expired capacity lease"); err != nil {
				return nil, err
			}
		}
		lease, err := s.acquireV5LeaseTx(ctx, tx, "capacity_slot", resourceID, owner, jobID, ttl, actor, reason, now)
		if err != nil {
			return nil, err
		}
		return &lease, nil
	}
	return nil, fmt.Errorf("%w: capacity pool %s", ErrCapacityExhausted, pool.PoolKey)
}

func capacitySlotResourceID(poolKey string, slot int) string {
	return "capacity:" + poolKey + ":slot:" + strconv.Itoa(slot)
}

func (s *Store) acquireV5LeaseTx(ctx context.Context, tx *sql.Tx, resourceType, resourceID, owner, jobID string, ttl time.Duration, actor, reason string, now time.Time) (Lease, error) {
	existing, err := getActiveLeaseForResourceTx(ctx, tx, resourceType, resourceID)
	if err != nil {
		return Lease{}, err
	}
	if existing != nil {
		if existing.ExpiresAt.After(now) {
			return Lease{}, fmt.Errorf("%w: %s/%s", ErrLeaseHeld, resourceType, resourceID)
		}
		if err := s.expireLeaseTx(ctx, tx, *existing, actor, now, "dispatch observed expired lease"); err != nil {
			return Lease{}, err
		}
	}
	var maximumToken int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(fencing_token), 0) FROM leases WHERE resource_type = ? AND resource_id = ?`, resourceType, resourceID).Scan(&maximumToken); err != nil {
		return Lease{}, err
	}
	if maximumToken < 0 || maximumToken == maxStoreInt64 {
		return Lease{}, fmt.Errorf("%w: lease fencing token overflow", ErrInvalidDispatch)
	}
	id, err := s.newV2ID("")
	if err != nil {
		return Lease{}, err
	}
	lease := Lease{ID: id, ResourceType: resourceType, ResourceID: resourceID, Owner: owner, JobID: jobID,
		ExpiresAt: now.Add(ttl), FencingToken: uint64(maximumToken + 1), State: LeaseActive, CreatedAt: now, UpdatedAt: now, Version: 1}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO leases (id, resource_type, resource_id, owner, job_id, expires_at, fencing_token, state, created_at, updated_at, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, lease.ID, lease.ResourceType, lease.ResourceID, lease.Owner, lease.JobID, lease.ExpiresAt, int64(lease.FencingToken), lease.State, lease.CreatedAt, lease.UpdatedAt, lease.Version); err != nil {
		if isGlobalIdentityCollision(err) {
			return Lease{}, fmt.Errorf("%w: lease %s", ErrIdentityCollision, lease.ID)
		}
		if isUniqueConstraint(err) {
			return Lease{}, fmt.Errorf("%w: %s/%s", ErrLeaseHeld, resourceType, resourceID)
		}
		return Lease{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{Actor: actor, EntityType: "lease", EntityID: lease.ID, Action: "lease.acquired_for_dispatch", Reason: reason,
		PayloadJSON: auditPayload(map[string]any{"resource_type": lease.ResourceType, "resource_id": lease.ResourceID, "job_id": lease.JobID, "fencing_token": lease.FencingToken}), CreatedAt: now}); err != nil {
		return Lease{}, err
	}
	return lease, nil
}

func insertDurableJobDispatchClaimTx(ctx context.Context, tx *sql.Tx, claim DurableJobDispatchClaim) error {
	jobID := ""
	if claim.Job != nil {
		jobID = claim.Job.ID
	}
	dispatchLeaseID := ""
	if claim.DispatchLease != nil {
		dispatchLeaseID = claim.DispatchLease.ID
	}
	capacityLeaseID := ""
	if claim.CapacityLease != nil {
		capacityLeaseID = claim.CapacityLease.ID
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO job_dispatch_claims_v5 (
			id, idempotency_key, run_id, job_id, owner, lease_ttl_ms, dispatch_lease_id,
			capacity_pool_key, capacity_lease_id, state, claimed_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, claim.ID, claim.IdempotencyKey, claim.RunID, nullableString(jobID), claim.Owner, durationMilliseconds(claim.LeaseTTL),
		nullableString(dispatchLeaseID), claim.CapacityPoolKey, nullableString(capacityLeaseID), claim.State, claim.ClaimedAt, claim.UpdatedAt)
	if err != nil {
		if isUniqueConstraint(err) {
			return fmt.Errorf("%w: durable job dispatch claim %s", ErrIdentityCollision, claim.ID)
		}
		return err
	}
	return nil
}

func getDurableJobDispatchClaimByKeyTx(ctx context.Context, tx *sql.Tx, key string) (*DurableJobDispatchClaim, error) {
	claim, err := scanDurableJobDispatchClaim(tx.QueryRowContext(ctx, durableJobDispatchClaimV5Select+" WHERE idempotency_key = ?", key))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &claim, nil
}

func (s *Store) loadDurableJobDispatchClaimTx(ctx context.Context, tx *sql.Tx, claim *DurableJobDispatchClaim) error {
	var jobID, dispatchLeaseID, capacityLeaseID sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT job_id, dispatch_lease_id, capacity_lease_id
		FROM job_dispatch_claims_v5 WHERE id = ?
	`, claim.ID).Scan(&jobID, &dispatchLeaseID, &capacityLeaseID)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: durable job dispatch claim %s", ErrNotFound, claim.ID)
	}
	if err != nil {
		return err
	}
	if jobID.Valid {
		job, err := getDurableJobTx(ctx, tx, jobID.String)
		if err != nil {
			return err
		}
		claim.Job = &job
	}
	if dispatchLeaseID.Valid {
		lease, err := getLeaseTx(ctx, tx, dispatchLeaseID.String)
		if err != nil {
			return err
		}
		claim.DispatchLease = &lease
	}
	if capacityLeaseID.Valid {
		lease, err := getLeaseTx(ctx, tx, capacityLeaseID.String)
		if err != nil {
			return err
		}
		claim.CapacityLease = &lease
	}
	return nil
}

func (s *Store) markPendingControlsForReconcileTx(ctx context.Context, tx *sql.Tx, runID, actor, reason string, now time.Time) ([]DurableControlOperation, error) {
	rows, err := tx.QueryContext(ctx, durableControlOperationV5Select+" WHERE run_id = ? AND status IN ('requested', 'propagating') ORDER BY created_at, id", runID)
	if err != nil {
		return nil, err
	}
	var operations []DurableControlOperation
	for rows.Next() {
		operation, err := scanDurableControlOperation(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		operations = append(operations, operation)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for index := range operations {
		operation := &operations[index]
		if !validControlOperationTransition(operation.Status, ControlOperationReconcileRequired) {
			continue
		}
		previousVersion := operation.Version
		operation.Status = ControlOperationReconcileRequired
		operation.UpdatedAt = now
		operation.Version++
		result, err := tx.ExecContext(ctx, `
			UPDATE control_operations_v5 SET status = ?, updated_at = ?, version = ? WHERE id = ? AND version = ?
		`, operation.Status, operation.UpdatedAt, operation.Version, operation.ID, previousVersion)
		if err != nil {
			return nil, err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if changed != 1 {
			return nil, fmt.Errorf("%w: control operation %s", ErrOptimisticLock, operation.ID)
		}
		transitionID, err := s.newV2ID("")
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO control_operation_transitions_v5 (
				id, operation_id, expected_version, status, checkpoint_id, quota_settlement_id,
				failure_reason, actor, reason, created_at
			) VALUES (?, ?, ?, ?, ?, ?, '', ?, ?, ?)
		`, transitionID, operation.ID, previousVersion, operation.Status, operation.CheckpointID,
			operation.QuotaSettlementID, actor, reason, now); err != nil {
			return nil, err
		}
		if err := s.appendControlOutboxTx(ctx, tx, *operation, "control.reconcile_required", now); err != nil {
			return nil, err
		}
		if _, err := s.appendAuditTx(ctx, tx, AuditEvent{Actor: actor, EntityType: "control_operation", EntityID: operation.ID, Action: "control_operation.reconcile_required", Reason: reason,
			PayloadJSON: auditPayload(map[string]any{"transition_id": transitionID, "cause": "expired_job_dispatch"}), OperationKey: operation.OperationKey, CreatedAt: now}); err != nil {
			return nil, err
		}
	}
	return operations, nil
}

func scanCapacityPool(scanner rowScanner) (CapacityPool, error) {
	var pool CapacityPool
	if err := scanner.Scan(&pool.ID, &pool.PoolKey, &pool.Capacity, &pool.CreatedAt, &pool.UpdatedAt, &pool.Version); err != nil {
		return CapacityPool{}, err
	}
	pool.CreatedAt = pool.CreatedAt.UTC()
	pool.UpdatedAt = pool.UpdatedAt.UTC()
	return pool, nil
}

func scanDurableJobDispatchClaim(scanner rowScanner) (DurableJobDispatchClaim, error) {
	var claim DurableJobDispatchClaim
	var jobID, dispatchLeaseID, capacityLeaseID sql.NullString
	var ttlMilliseconds int64
	if err := scanner.Scan(&claim.ID, &claim.IdempotencyKey, &claim.RunID, &jobID, &claim.Owner, &ttlMilliseconds,
		&dispatchLeaseID, &claim.CapacityPoolKey, &capacityLeaseID, &claim.State, &claim.ClaimedAt, &claim.UpdatedAt); err != nil {
		return DurableJobDispatchClaim{}, err
	}
	if ttlMilliseconds <= 0 {
		return DurableJobDispatchClaim{}, fmt.Errorf("%w: invalid durable job dispatch claim %s", ErrInvalidDispatch, claim.ID)
	}
	if claim.State == "empty" && (jobID.Valid || dispatchLeaseID.Valid || capacityLeaseID.Valid) {
		return DurableJobDispatchClaim{}, fmt.Errorf("%w: invalid empty durable job dispatch claim %s", ErrInvalidDispatch, claim.ID)
	}
	if claim.State != "empty" && (!jobID.Valid || !dispatchLeaseID.Valid) {
		return DurableJobDispatchClaim{}, fmt.Errorf("%w: incomplete durable job dispatch claim %s", ErrInvalidDispatch, claim.ID)
	}
	if claim.RunID != "" && !isUUIDv7(claim.RunID) {
		return DurableJobDispatchClaim{}, fmt.Errorf("%w: invalid durable job dispatch claim run ID", ErrInvalidDispatch)
	}
	claim.LeaseTTL = time.Duration(ttlMilliseconds) * time.Millisecond
	claim.ClaimedAt = claim.ClaimedAt.UTC()
	claim.UpdatedAt = claim.UpdatedAt.UTC()
	return claim, nil
}

// releaseDurableJobDispatchClaimsTx keeps dispatch history truthful after the
// legacy durable-job transition releases its job-bound leases. Lease history
// remains append-only; this only removes the claim from the active index.
func (s *Store) releaseDurableJobDispatchClaimsTx(ctx context.Context, tx *sql.Tx, jobID string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE job_dispatch_claims_v5
		SET state = 'released', updated_at = ?
		WHERE job_id = ? AND state = 'active'
	`, now, jobID)
	return err
}
