package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const (
	DefaultLeaseTTL               = 90 * time.Second
	DefaultLeaseHeartbeatInterval = 20 * time.Second
	// DurableJobQueuedOutboxTopic is the durable wake-up signal for a Run
	// scoped job. Consumers must re-read the job and its Run; the event payload
	// is only a delivery hint.
	DurableJobQueuedOutboxTopic = "durable_job.queued"
)

const durableJobSelect = `
	SELECT id, command_type, entity_type, entity_id, run_id, stage_attempt_id,
	       state, priority, payload_json, idempotency_key, created_by,
	       created_at, updated_at, started_at, finished_at, version
	FROM jobs`

func (s *Store) CreateDurableJob(ctx context.Context, request CreateDurableJobRequest) (DurableJob, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return DurableJob{}, err
	}
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return DurableJob{}, err
	}
	commandType, err := normalizeRequired(request.CommandType, "job command type")
	if err != nil {
		return DurableJob{}, err
	}
	entityType, err := normalizeRequired(request.EntityType, "job entity type")
	if err != nil {
		return DurableJob{}, err
	}
	entityID, err := normalizeRequired(request.EntityID, "job entity ID")
	if err != nil {
		return DurableJob{}, err
	}
	idempotencyKey, err := normalizeRequired(request.IdempotencyKey, "job idempotency key")
	if err != nil {
		return DurableJob{}, err
	}
	if request.RunID != "" && !isUUIDv7(request.RunID) {
		return DurableJob{}, ErrInvalidUUIDv7Identity
	}
	if request.StageAttemptID != "" && !isUUIDv7(request.StageAttemptID) {
		return DurableJob{}, ErrInvalidUUIDv7Identity
	}
	payload, err := normalizeJSON(request.PayloadJSON, "job payload")
	if err != nil {
		return DurableJob{}, err
	}
	now := s.now().UTC()
	job := DurableJob{
		ID:             id,
		CommandType:    commandType,
		EntityType:     entityType,
		EntityID:       entityID,
		RunID:          strings.TrimSpace(request.RunID),
		StageAttemptID: strings.TrimSpace(request.StageAttemptID),
		State:          JobQueued,
		Priority:       request.Priority,
		PayloadJSON:    payload,
		IdempotencyKey: idempotencyKey,
		CreatedBy:      resolveActor(request.Actor),
		CreatedAt:      now,
		UpdatedAt:      now,
		Version:        1,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DurableJob{}, err
	}
	defer tx.Rollback()
	if job.RunID != "" {
		if _, err := getWorkflowRunTx(ctx, tx, job.RunID); err != nil {
			return DurableJob{}, err
		}
	}
	if job.StageAttemptID != "" {
		stage, err := getStageAttemptTx(ctx, tx, job.StageAttemptID)
		if err != nil {
			return DurableJob{}, err
		}
		if job.RunID != "" && stage.RunID != job.RunID {
			return DurableJob{}, fmt.Errorf("job stage attempt belongs to another workflow run")
		}
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO jobs (
			id, command_type, entity_type, entity_id, run_id, stage_attempt_id, state,
			priority, payload_json, idempotency_key, created_by, created_at, updated_at,
			started_at, finished_at, version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?)
	`, job.ID, job.CommandType, job.EntityType, job.EntityID, nullableString(job.RunID), nullableString(job.StageAttemptID),
		job.State, job.Priority, job.PayloadJSON, job.IdempotencyKey, job.CreatedBy, job.CreatedAt, job.UpdatedAt, job.Version)
	if err != nil {
		if isGlobalIdentityCollision(err) {
			return DurableJob{}, fmt.Errorf("%w: durable job %s", ErrIdentityCollision, job.ID)
		}
		if !isUniqueConstraint(err) {
			return DurableJob{}, err
		}
		existing, existingErr := getDurableJobByIdempotencyTx(ctx, tx, job.IdempotencyKey)
		if existingErr != nil {
			return DurableJob{}, existingErr
		}
		if sameDurableJobRequest(existing, job) {
			if err := tx.Commit(); err != nil {
				return DurableJob{}, err
			}
			return existing, nil
		}
		return DurableJob{}, fmt.Errorf("%w: %s", ErrIdempotencyConflict, job.IdempotencyKey)
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       request.Actor,
		EntityType:  "job",
		EntityID:    job.ID,
		Action:      "job.created",
		Reason:      request.Reason,
		PayloadJSON: auditPayload(map[string]any{"command_type": job.CommandType, "entity_type": job.EntityType, "entity_id": job.EntityID, "idempotency_key": job.IdempotencyKey}),
		CreatedAt:   now,
	}); err != nil {
		return DurableJob{}, err
	}
	if err := s.appendDurableJobQueuedOutboxTx(ctx, tx, job, now); err != nil {
		return DurableJob{}, err
	}
	if err := tx.Commit(); err != nil {
		return DurableJob{}, err
	}
	return job, nil
}

// appendDurableJobQueuedOutboxTx records one delivery signal for every
// Run-scoped queued job created through the generic store boundary. The job
// itself remains authoritative, so callers never need to serialize executable
// data into the outbox payload.
func (s *Store) appendDurableJobQueuedOutboxTx(ctx context.Context, tx *sql.Tx, job DurableJob, now time.Time) error {
	if job.RunID == "" || job.State != JobQueued {
		return nil
	}
	return s.appendV5OutboxTx(ctx, tx, DurableJobQueuedOutboxTopic, "durable_job", job.ID,
		job.IdempotencyKey+":queued", auditPayload(map[string]any{
			"run_id": job.RunID, "command_type": job.CommandType, "entity_type": job.EntityType, "entity_id": job.EntityID,
		}), now)
}

func (s *Store) GetDurableJob(ctx context.Context, jobID string) (*DurableJob, error) {
	if !isUUIDv7(jobID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	job, err := scanDurableJob(s.db.QueryRowContext(ctx, durableJobSelect+" WHERE id = ?", jobID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &job, nil
}

func (s *Store) GetDurableJobByIdempotency(ctx context.Context, idempotencyKey string) (*DurableJob, error) {
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" {
		return nil, fmt.Errorf("job idempotency key is required")
	}
	job, err := scanDurableJob(s.db.QueryRowContext(ctx, durableJobSelect+" WHERE idempotency_key = ?", idempotencyKey))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &job, nil
}

// ListDurableJobsForRun returns the durable execution history for one frozen
// workflow run. It is read-only so attach/reconcile surfaces can render job
// and lease state without reaching into SQLite directly.
func (s *Store) ListDurableJobsForRun(ctx context.Context, runID string) ([]DurableJob, error) {
	if !isUUIDv7(runID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	rows, err := s.db.QueryContext(ctx, durableJobSelect+" WHERE run_id = ? ORDER BY created_at ASC, id ASC", runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	jobs := make([]DurableJob, 0)
	for rows.Next() {
		job, err := scanDurableJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (s *Store) TransitionDurableJob(ctx context.Context, request TransitionDurableJobRequest) (DurableJob, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return DurableJob{}, err
	}
	if !isUUIDv7(request.JobID) {
		return DurableJob{}, ErrInvalidUUIDv7Identity
	}
	if request.ExpectedVersion <= 0 {
		return DurableJob{}, fmt.Errorf("expected job version must be positive")
	}
	if !validJobState(request.State) {
		return DurableJob{}, fmt.Errorf("invalid job state %q", request.State)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DurableJob{}, err
	}
	defer tx.Rollback()
	job, err := getDurableJobTx(ctx, tx, request.JobID)
	if err != nil {
		return DurableJob{}, err
	}
	if job.Version != request.ExpectedVersion {
		return DurableJob{}, fmt.Errorf("%w: job %s", ErrOptimisticLock, job.ID)
	}
	if !validJobTransition(job.State, request.State) {
		return DurableJob{}, fmt.Errorf("%w: job %s from %s to %s", ErrInvalidTransition, job.ID, job.State, request.State)
	}
	now := s.now().UTC()
	job.State = request.State
	job.UpdatedAt = now
	if job.State == JobRunning && job.StartedAt == nil {
		job.StartedAt = &now
	}
	if isJobDeliveryFinalState(job.State) {
		job.FinishedAt = &now
	}
	job.Version++
	result, err := tx.ExecContext(ctx, `
		UPDATE jobs SET state = ?, updated_at = ?, started_at = ?, finished_at = ?, version = ?
		WHERE id = ? AND version = ?
	`, job.State, job.UpdatedAt, job.StartedAt, job.FinishedAt, job.Version, job.ID, request.ExpectedVersion)
	if err != nil {
		return DurableJob{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return DurableJob{}, err
	}
	if changed != 1 {
		return DurableJob{}, fmt.Errorf("%w: job %s", ErrOptimisticLock, job.ID)
	}
	if isJobDeliveryFinalState(job.State) {
		if err := s.releaseJobLeasesTx(ctx, tx, job.ID, resolveActor(request.Actor), now); err != nil {
			return DurableJob{}, err
		}
		if err := s.releaseDurableJobDispatchClaimsTx(ctx, tx, job.ID, now); err != nil {
			return DurableJob{}, err
		}
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       request.Actor,
		EntityType:  "job",
		EntityID:    job.ID,
		Action:      "job.transitioned",
		Reason:      request.Reason,
		PayloadJSON: auditPayload(map[string]any{"state": job.State, "version": job.Version}),
		CreatedAt:   now,
	}); err != nil {
		return DurableJob{}, err
	}
	if err := tx.Commit(); err != nil {
		return DurableJob{}, err
	}
	return job, nil
}

const leaseSelect = `
	SELECT id, resource_type, resource_id, owner, job_id, expires_at, fencing_token,
	       state, created_at, updated_at, version
	FROM leases`

func (s *Store) AcquireLease(ctx context.Context, request AcquireLeaseRequest) (Lease, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return Lease{}, err
	}
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return Lease{}, err
	}
	resourceType, err := normalizeRequired(request.ResourceType, "lease resource type")
	if err != nil {
		return Lease{}, err
	}
	resourceID, err := normalizeRequired(request.ResourceID, "lease resource ID")
	if err != nil {
		return Lease{}, err
	}
	owner, err := normalizeRequired(request.Owner, "lease owner")
	if err != nil {
		return Lease{}, err
	}
	if request.JobID != "" && !isUUIDv7(request.JobID) {
		return Lease{}, ErrInvalidUUIDv7Identity
	}
	ttl := request.TTL
	if ttl == 0 {
		ttl = DefaultLeaseTTL
	}
	if ttl < 0 {
		return Lease{}, fmt.Errorf("lease TTL must be positive")
	}
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Lease{}, err
	}
	defer tx.Rollback()
	if request.JobID != "" {
		if _, err := getDurableJobTx(ctx, tx, request.JobID); err != nil {
			return Lease{}, err
		}
	}
	existing, err := getActiveLeaseForResourceTx(ctx, tx, resourceType, resourceID)
	if err != nil {
		return Lease{}, err
	}
	if existing != nil {
		if existing.ExpiresAt.After(now) {
			return Lease{}, fmt.Errorf("%w: %s/%s is owned by %s until %s", ErrLeaseHeld, resourceType, resourceID, existing.Owner, existing.ExpiresAt.Format(time.RFC3339))
		}
		if err := s.expireLeaseTx(ctx, tx, *existing, resolveActor(request.Actor), now, "acquire observed expired lease"); err != nil {
			return Lease{}, err
		}
	}
	var maxFencing int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(fencing_token), 0) FROM leases WHERE resource_type = ? AND resource_id = ?`, resourceType, resourceID).Scan(&maxFencing); err != nil {
		return Lease{}, err
	}
	if maxFencing == int64(^uint64(0)>>1) {
		return Lease{}, fmt.Errorf("lease fencing token overflow")
	}
	lease := Lease{
		ID:           id,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		Owner:        owner,
		JobID:        strings.TrimSpace(request.JobID),
		ExpiresAt:    now.Add(ttl),
		FencingToken: uint64(maxFencing + 1),
		State:        LeaseActive,
		CreatedAt:    now,
		UpdatedAt:    now,
		Version:      1,
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO leases (
			id, resource_type, resource_id, owner, job_id, expires_at, fencing_token,
			state, created_at, updated_at, version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, lease.ID, lease.ResourceType, lease.ResourceID, lease.Owner, nullableString(lease.JobID), lease.ExpiresAt,
		int64(lease.FencingToken), lease.State, lease.CreatedAt, lease.UpdatedAt, lease.Version)
	if err != nil {
		if isGlobalIdentityCollision(err) {
			return Lease{}, fmt.Errorf("%w: lease %s", ErrIdentityCollision, lease.ID)
		}
		if isUniqueConstraint(err) {
			return Lease{}, fmt.Errorf("%w: %s/%s", ErrLeaseHeld, resourceType, resourceID)
		}
		return Lease{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       request.Actor,
		EntityType:  "lease",
		EntityID:    lease.ID,
		Action:      "lease.acquired",
		Reason:      request.Reason,
		PayloadJSON: auditPayload(map[string]any{"resource_type": lease.ResourceType, "resource_id": lease.ResourceID, "fencing_token": lease.FencingToken, "job_id": lease.JobID}),
		CreatedAt:   now,
	}); err != nil {
		return Lease{}, err
	}
	if err := tx.Commit(); err != nil {
		return Lease{}, err
	}
	return lease, nil
}

func (s *Store) GetLease(ctx context.Context, leaseID string) (*Lease, error) {
	if !isUUIDv7(leaseID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	lease, err := scanLease(s.db.QueryRowContext(ctx, leaseSelect+" WHERE id = ?", leaseID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &lease, nil
}

// ListLeasesForJob returns append-only lease history for a durable job. The
// active item, if any, is identified by Lease.State rather than hidden by a
// mutable reservation index.
func (s *Store) ListLeasesForJob(ctx context.Context, jobID string) ([]Lease, error) {
	if !isUUIDv7(jobID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	rows, err := s.db.QueryContext(ctx, leaseSelect+" WHERE job_id = ? ORDER BY created_at ASC, id ASC", jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	leases := make([]Lease, 0)
	for rows.Next() {
		lease, err := scanLease(rows)
		if err != nil {
			return nil, err
		}
		leases = append(leases, lease)
	}
	return leases, rows.Err()
}

// ListLeasesForResource returns append-only lease history for one explicitly
// named resource. It is used by local worker supervision to expose the
// run-scoped child-worker fence without conflating it with a dispatch lease.
func (s *Store) ListLeasesForResource(ctx context.Context, resourceType, resourceID string) ([]Lease, error) {
	resourceType, err := normalizeRequired(resourceType, "lease resource type")
	if err != nil {
		return nil, err
	}
	resourceID, err = normalizeRequired(resourceID, "lease resource ID")
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, leaseSelect+" WHERE resource_type = ? AND resource_id = ? ORDER BY created_at ASC, id ASC", resourceType, resourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	leases := make([]Lease, 0)
	for rows.Next() {
		lease, err := scanLease(rows)
		if err != nil {
			return nil, err
		}
		leases = append(leases, lease)
	}
	return leases, rows.Err()
}

func (s *Store) HeartbeatLease(ctx context.Context, request HeartbeatLeaseRequest) (Lease, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return Lease{}, err
	}
	if !isUUIDv7(request.LeaseID) {
		return Lease{}, ErrInvalidUUIDv7Identity
	}
	if request.ExpectedVersion <= 0 {
		return Lease{}, fmt.Errorf("expected lease version must be positive")
	}
	owner, err := normalizeRequired(request.Owner, "lease owner")
	if err != nil {
		return Lease{}, err
	}
	ttl := request.TTL
	if ttl == 0 {
		ttl = DefaultLeaseTTL
	}
	if ttl < 0 {
		return Lease{}, fmt.Errorf("lease TTL must be positive")
	}
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Lease{}, err
	}
	defer tx.Rollback()
	lease, err := getLeaseTx(ctx, tx, request.LeaseID)
	if err != nil {
		return Lease{}, err
	}
	if lease.Version != request.ExpectedVersion {
		return Lease{}, fmt.Errorf("%w: lease %s", ErrOptimisticLock, lease.ID)
	}
	if lease.Owner != owner || lease.FencingToken != request.FencingToken {
		return Lease{}, fmt.Errorf("%w: lease %s", ErrFencingToken, lease.ID)
	}
	if lease.State != LeaseActive {
		return Lease{}, fmt.Errorf("%w: lease %s is %s", ErrImmutable, lease.ID, lease.State)
	}
	if !lease.ExpiresAt.After(now) {
		if err := s.expireLeaseTx(ctx, tx, lease, resolveActor(request.Actor), now, "heartbeat observed expired lease"); err != nil {
			return Lease{}, err
		}
		return Lease{}, fmt.Errorf("%w: lease %s expired", ErrLeaseHeld, lease.ID)
	}
	lease.ExpiresAt = now.Add(ttl)
	lease.UpdatedAt = now
	lease.Version++
	result, err := tx.ExecContext(ctx, `
		UPDATE leases SET expires_at = ?, updated_at = ?, version = ?
		WHERE id = ? AND state = 'active' AND version = ? AND fencing_token = ?
	`, lease.ExpiresAt, lease.UpdatedAt, lease.Version, lease.ID, request.ExpectedVersion, int64(request.FencingToken))
	if err != nil {
		return Lease{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return Lease{}, err
	}
	if changed != 1 {
		return Lease{}, fmt.Errorf("%w: lease %s", ErrOptimisticLock, lease.ID)
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       request.Actor,
		EntityType:  "lease",
		EntityID:    lease.ID,
		Action:      "lease.heartbeated",
		Reason:      request.Reason,
		PayloadJSON: auditPayload(map[string]any{"expires_at": lease.ExpiresAt, "fencing_token": lease.FencingToken, "version": lease.Version}),
		CreatedAt:   now,
	}); err != nil {
		return Lease{}, err
	}
	if err := tx.Commit(); err != nil {
		return Lease{}, err
	}
	return lease, nil
}

func (s *Store) ReleaseLease(ctx context.Context, request ReleaseLeaseRequest) (Lease, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return Lease{}, err
	}
	if !isUUIDv7(request.LeaseID) {
		return Lease{}, ErrInvalidUUIDv7Identity
	}
	if request.ExpectedVersion <= 0 {
		return Lease{}, fmt.Errorf("expected lease version must be positive")
	}
	owner, err := normalizeRequired(request.Owner, "lease owner")
	if err != nil {
		return Lease{}, err
	}
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Lease{}, err
	}
	defer tx.Rollback()
	lease, err := getLeaseTx(ctx, tx, request.LeaseID)
	if err != nil {
		return Lease{}, err
	}
	if lease.Version != request.ExpectedVersion {
		return Lease{}, fmt.Errorf("%w: lease %s", ErrOptimisticLock, lease.ID)
	}
	if lease.Owner != owner || lease.FencingToken != request.FencingToken {
		return Lease{}, fmt.Errorf("%w: lease %s", ErrFencingToken, lease.ID)
	}
	if lease.State != LeaseActive {
		return Lease{}, fmt.Errorf("%w: lease %s is %s", ErrImmutable, lease.ID, lease.State)
	}
	lease.State = LeaseReleased
	lease.UpdatedAt = now
	lease.Version++
	result, err := tx.ExecContext(ctx, `
		UPDATE leases SET state = ?, updated_at = ?, version = ?
		WHERE id = ? AND state = 'active' AND version = ? AND fencing_token = ?
	`, lease.State, lease.UpdatedAt, lease.Version, lease.ID, request.ExpectedVersion, int64(request.FencingToken))
	if err != nil {
		return Lease{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return Lease{}, err
	}
	if changed != 1 {
		return Lease{}, fmt.Errorf("%w: lease %s", ErrOptimisticLock, lease.ID)
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       request.Actor,
		EntityType:  "lease",
		EntityID:    lease.ID,
		Action:      "lease.released",
		Reason:      request.Reason,
		PayloadJSON: auditPayload(map[string]any{"resource_type": lease.ResourceType, "resource_id": lease.ResourceID, "fencing_token": lease.FencingToken, "version": lease.Version}),
		CreatedAt:   now,
	}); err != nil {
		return Lease{}, err
	}
	if err := tx.Commit(); err != nil {
		return Lease{}, err
	}
	return lease, nil
}

// ExpireLeases is used by recovery workers after process loss. It does not
// delete lease history, preserving the fencing sequence and audit lineage.
func (s *Store) ExpireLeases(ctx context.Context) (int, error) {
	return s.expireLeases(ctx, "", nil, "system", "recovery lease expiration")
}

// ExpireLeasesForRun expires only leases belonging to jobs of one Run. It is
// used by an explicit local run reconciliation so one operator cannot alter
// unrelated worker leases while inspecting a selected run.
func (s *Store) ExpireLeasesForRun(ctx context.Context, runID, actor, reason string) (int, error) {
	if !isUUIDv7(runID) {
		return 0, ErrInvalidUUIDv7Identity
	}
	actor = resolveActor(actor)
	reason, err := normalizeRequired(reason, "run lease reconciliation reason")
	if err != nil {
		return 0, err
	}
	return s.expireLeases(ctx, ` AND job_id IN (SELECT id FROM jobs WHERE run_id = ?)`, []any{runID}, actor, reason)
}

// ExpireLeasesForResource fences only stale leases for one explicitly named
// resource. It is intentionally narrower than ExpireLeases so an attach or
// reconcile action for one Run cannot change another worker's supervision
// fence.
func (s *Store) ExpireLeasesForResource(ctx context.Context, resourceType, resourceID, actor, reason string) (int, error) {
	resourceType, err := normalizeRequired(resourceType, "lease resource type")
	if err != nil {
		return 0, err
	}
	resourceID, err = normalizeRequired(resourceID, "lease resource ID")
	if err != nil {
		return 0, err
	}
	actor = resolveActor(actor)
	reason, err = normalizeRequired(reason, "lease reconciliation reason")
	if err != nil {
		return 0, err
	}
	return s.expireLeases(ctx, ` AND resource_type = ? AND resource_id = ?`, []any{resourceType, resourceID}, actor, reason)
}

func (s *Store) expireLeases(ctx context.Context, scopeClause string, scopeArgs []any, actor, reason string) (int, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return 0, err
	}
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	query := leaseSelect + " WHERE state = 'active' AND expires_at <= ?" + scopeClause + " ORDER BY expires_at ASC"
	args := append([]any{now}, scopeArgs...)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	var leases []Lease
	for rows.Next() {
		lease, err := scanLease(rows)
		if err != nil {
			_ = rows.Close()
			return 0, err
		}
		leases = append(leases, lease)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, lease := range leases {
		if err := s.expireLeaseTx(ctx, tx, lease, actor, now, reason); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(leases), nil
}

func getDurableJobTx(ctx context.Context, tx *sql.Tx, jobID string) (DurableJob, error) {
	job, err := scanDurableJob(tx.QueryRowContext(ctx, durableJobSelect+" WHERE id = ?", jobID))
	if err == sql.ErrNoRows {
		return DurableJob{}, fmt.Errorf("%w: job %s", ErrNotFound, jobID)
	}
	return job, err
}

func getDurableJobByIdempotencyTx(ctx context.Context, tx *sql.Tx, key string) (DurableJob, error) {
	job, err := scanDurableJob(tx.QueryRowContext(ctx, durableJobSelect+" WHERE idempotency_key = ?", key))
	if err == sql.ErrNoRows {
		return DurableJob{}, fmt.Errorf("%w: job idempotency key %s", ErrNotFound, key)
	}
	return job, err
}

func getLeaseTx(ctx context.Context, tx *sql.Tx, leaseID string) (Lease, error) {
	lease, err := scanLease(tx.QueryRowContext(ctx, leaseSelect+" WHERE id = ?", leaseID))
	if err == sql.ErrNoRows {
		return Lease{}, fmt.Errorf("%w: lease %s", ErrNotFound, leaseID)
	}
	return lease, err
}

func getActiveLeaseForResourceTx(ctx context.Context, tx *sql.Tx, resourceType, resourceID string) (*Lease, error) {
	lease, err := scanLease(tx.QueryRowContext(ctx, leaseSelect+" WHERE resource_type = ? AND resource_id = ? AND state = 'active'", resourceType, resourceID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &lease, nil
}

func (s *Store) expireLeaseTx(ctx context.Context, tx *sql.Tx, lease Lease, actor string, now time.Time, reason string) error {
	if lease.State != LeaseActive {
		return nil
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE leases SET state = 'expired', updated_at = ?, version = version + 1
		WHERE id = ? AND state = 'active' AND version = ?
	`, now, lease.ID, lease.Version)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("%w: lease %s", ErrOptimisticLock, lease.ID)
	}
	_, err = s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       actor,
		EntityType:  "lease",
		EntityID:    lease.ID,
		Action:      "lease.expired",
		Reason:      reason,
		PayloadJSON: auditPayload(map[string]any{"resource_type": lease.ResourceType, "resource_id": lease.ResourceID, "fencing_token": lease.FencingToken}),
		CreatedAt:   now,
	})
	return err
}

func (s *Store) releaseJobLeasesTx(ctx context.Context, tx *sql.Tx, jobID, actor string, now time.Time) error {
	rows, err := tx.QueryContext(ctx, leaseSelect+" WHERE job_id = ? AND state = 'active'", jobID)
	if err != nil {
		return err
	}
	var leases []Lease
	for rows.Next() {
		lease, err := scanLease(rows)
		if err != nil {
			_ = rows.Close()
			return err
		}
		leases = append(leases, lease)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, lease := range leases {
		result, err := tx.ExecContext(ctx, `
			UPDATE leases SET state = 'released', updated_at = ?, version = version + 1
			WHERE id = ? AND state = 'active' AND version = ?
		`, now, lease.ID, lease.Version)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return fmt.Errorf("%w: lease %s", ErrOptimisticLock, lease.ID)
		}
		if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
			Actor:       actor,
			EntityType:  "lease",
			EntityID:    lease.ID,
			Action:      "lease.released_for_terminal_job",
			Reason:      "terminal job released active lease",
			PayloadJSON: auditPayload(map[string]any{"job_id": jobID, "resource_type": lease.ResourceType, "resource_id": lease.ResourceID, "fencing_token": lease.FencingToken}),
			CreatedAt:   now,
		}); err != nil {
			return err
		}
	}
	return nil
}

func scanDurableJob(scanner rowScanner) (DurableJob, error) {
	var job DurableJob
	var runID, stageAttemptID sql.NullString
	var startedAt, finishedAt sql.NullTime
	if err := scanner.Scan(
		&job.ID, &job.CommandType, &job.EntityType, &job.EntityID, &runID, &stageAttemptID,
		&job.State, &job.Priority, &job.PayloadJSON, &job.IdempotencyKey, &job.CreatedBy,
		&job.CreatedAt, &job.UpdatedAt, &startedAt, &finishedAt, &job.Version,
	); err != nil {
		return DurableJob{}, err
	}
	job.RunID = nullableStringValue(runID)
	job.StageAttemptID = nullableStringValue(stageAttemptID)
	job.CreatedAt = job.CreatedAt.UTC()
	job.UpdatedAt = job.UpdatedAt.UTC()
	job.StartedAt = nullableTimePtr(startedAt)
	job.FinishedAt = nullableTimePtr(finishedAt)
	return job, nil
}

func scanLease(scanner rowScanner) (Lease, error) {
	var lease Lease
	var jobID sql.NullString
	var fencing int64
	if err := scanner.Scan(
		&lease.ID, &lease.ResourceType, &lease.ResourceID, &lease.Owner, &jobID, &lease.ExpiresAt, &fencing,
		&lease.State, &lease.CreatedAt, &lease.UpdatedAt, &lease.Version,
	); err != nil {
		return Lease{}, err
	}
	if fencing <= 0 {
		return Lease{}, fmt.Errorf("invalid persisted fencing token for lease %s", lease.ID)
	}
	lease.JobID = nullableStringValue(jobID)
	lease.FencingToken = uint64(fencing)
	lease.ExpiresAt = lease.ExpiresAt.UTC()
	lease.CreatedAt = lease.CreatedAt.UTC()
	lease.UpdatedAt = lease.UpdatedAt.UTC()
	return lease, nil
}

func sameDurableJobRequest(existing, candidate DurableJob) bool {
	return existing.CommandType == candidate.CommandType &&
		existing.EntityType == candidate.EntityType &&
		existing.EntityID == candidate.EntityID &&
		existing.RunID == candidate.RunID &&
		existing.StageAttemptID == candidate.StageAttemptID &&
		existing.Priority == candidate.Priority &&
		existing.PayloadJSON == candidate.PayloadJSON
}

func validJobState(state JobState) bool {
	switch state {
	case JobQueued, JobRunning, JobPauseRequested, JobCancelRequested, JobStopRequested, JobPaused,
		JobCanceled, JobSucceeded, JobFailed, JobInterrupted, JobInDoubt:
		return true
	default:
		return false
	}
}

func validJobTransition(from, to JobState) bool {
	if from == to || isJobDeliveryFinalState(from) {
		return false
	}
	switch from {
	case JobQueued:
		return to == JobRunning || to == JobCancelRequested || to == JobCanceled
	case JobRunning:
		return to == JobPauseRequested || to == JobCancelRequested || to == JobStopRequested ||
			to == JobCanceled || to == JobSucceeded || to == JobFailed || to == JobInterrupted || to == JobInDoubt
	case JobPauseRequested:
		return to == JobPaused || to == JobCancelRequested || to == JobInterrupted || to == JobInDoubt
	case JobPaused:
		return to == JobRunning || to == JobCancelRequested || to == JobCanceled
	case JobCancelRequested, JobStopRequested:
		return to == JobCanceled || to == JobInterrupted || to == JobInDoubt
	default:
		return false
	}
}

func isTerminalJobState(state JobState) bool {
	switch state {
	case JobCanceled, JobSucceeded, JobFailed, JobInterrupted:
		return true
	default:
		return false
	}
}

// isJobDeliveryFinalState identifies a completed worker delivery. in_doubt
// remains a distinct reconciliation fact in the read model, but its current
// delivery is closed: an operator must create a new controlled redrive job
// rather than rewriting the original job back to running.
func isJobDeliveryFinalState(state JobState) bool {
	return isTerminalJobState(state) || state == JobInDoubt
}
