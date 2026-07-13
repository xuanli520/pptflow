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

const taskPurgeOperationV7Select = `
	SELECT id, task_id, idempotency_key, expected_task_version, actor, reason, state,
	       lease_id, lease_owner, lease_fencing_token, lease_version,
	       dependencies_json, last_error, created_at, updated_at, completed_at, version
	FROM task_purge_operations_v7`

// PrepareTaskPurge atomically binds a user command to one soft-deleted task,
// checks the preview CAS, freezes current blockers, and takes a task-scoped
// fencing lease when filesystem cleanup may proceed. A replay returns the
// original operation; it never creates another purge record.
func (s *Store) PrepareTaskPurge(ctx context.Context, request PrepareTaskPurgeRequest) (PrepareTaskPurgeResult, error) {
	prepared, err := prepareTaskPurgeRequestValue(s, request)
	if err != nil {
		return PrepareTaskPurgeResult{}, err
	}
	if _, err := s.BackupBeforeCriticalOperation(ctx, "task_purge_prepare"); err != nil {
		return PrepareTaskPurgeResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PrepareTaskPurgeResult{}, err
	}
	defer tx.Rollback()

	if existing, err := getTaskPurgeOperationByKeyTx(ctx, tx, prepared.idempotencyKey); err != nil {
		return PrepareTaskPurgeResult{}, err
	} else if existing != nil {
		if !sameTaskPurgeRequest(*existing, prepared) {
			return PrepareTaskPurgeResult{}, fmt.Errorf("%w: task purge key %s", ErrIdempotencyConflict, prepared.idempotencyKey)
		}
		task, err := getTaskV2Tx(ctx, tx, existing.TaskID)
		if err != nil {
			return PrepareTaskPurgeResult{}, err
		}
		result, err := s.replayTaskPurgeTx(ctx, tx, task, *existing, prepared)
		if err != nil {
			return PrepareTaskPurgeResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return PrepareTaskPurgeResult{}, err
		}
		return result, nil
	}

	task, err := getTaskV2Tx(ctx, tx, prepared.taskID)
	if err != nil {
		return PrepareTaskPurgeResult{}, err
	}
	if task.Version != prepared.expectedTaskVersion {
		return PrepareTaskPurgeResult{}, fmt.Errorf("%w: task %s", ErrOptimisticLock, task.ID)
	}
	if task.LifecycleState != TaskLifecycleDeleted {
		return PrepareTaskPurgeResult{}, fmt.Errorf("%w: only soft-deleted task %s may be purged", ErrInvalidTransition, task.ID)
	}
	if completed, err := getCompletedTaskPurgeForTaskTx(ctx, tx, task.ID); err != nil {
		return PrepareTaskPurgeResult{}, err
	} else if completed != nil {
		return PrepareTaskPurgeResult{}, fmt.Errorf("%w: task %s operation %s", ErrTaskPurged, task.ID, completed.ID)
	}
	dependencies, err := queryPurgeDependenciesForTask(ctx, tx, task.ID)
	if err != nil {
		return PrepareTaskPurgeResult{}, err
	}
	dependenciesJSON, err := encodePurgeDependencies(dependencies)
	if err != nil {
		return PrepareTaskPurgeResult{}, err
	}
	now := s.now().UTC()
	operation := TaskPurgeOperation{
		ID:                  prepared.id,
		TaskID:              task.ID,
		IdempotencyKey:      prepared.idempotencyKey,
		ExpectedTaskVersion: prepared.expectedTaskVersion,
		Actor:               prepared.actor,
		Reason:              prepared.reason,
		Dependencies:        dependencies,
		CreatedAt:           now,
		UpdatedAt:           now,
		Version:             1,
	}
	if dependencies.HasBlockers() {
		operation.State = TaskPurgeBlocked
		if err := s.insertTaskPurgeOperationTx(ctx, tx, operation, dependenciesJSON); err != nil {
			return PrepareTaskPurgeResult{}, err
		}
		if err := s.appendTaskPurgeAuditTx(ctx, tx, operation, "task_purge.blocked", now); err != nil {
			return PrepareTaskPurgeResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return PrepareTaskPurgeResult{}, err
		}
		return PrepareTaskPurgeResult{Task: task, Operation: operation}, nil
	}
	if active, err := getInProgressTaskPurgeForTaskTx(ctx, tx, task.ID); err != nil {
		return PrepareTaskPurgeResult{}, err
	} else if active != nil {
		return PrepareTaskPurgeResult{}, fmt.Errorf("%w: task %s operation %s", ErrTaskPurgeInProgress, task.ID, active.ID)
	}
	lease, err := s.acquireTaskPurgeLeaseTx(ctx, tx, task.ID, taskPurgeLeaseOwner(operation.ID), prepared.leaseTTL, prepared.actor, prepared.reason, now)
	if err != nil {
		return PrepareTaskPurgeResult{}, err
	}
	operation.State = TaskPurgeInProgress
	operation.LeaseID = lease.ID
	operation.LeaseOwner = lease.Owner
	operation.LeaseFencingToken = lease.FencingToken
	operation.LeaseVersion = lease.Version
	if err := s.insertTaskPurgeOperationTx(ctx, tx, operation, dependenciesJSON); err != nil {
		return PrepareTaskPurgeResult{}, err
	}
	if err := s.appendTaskPurgeAuditTx(ctx, tx, operation, "task_purge.prepared", now); err != nil {
		return PrepareTaskPurgeResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return PrepareTaskPurgeResult{}, err
	}
	return PrepareTaskPurgeResult{Task: task, Operation: operation, Acquired: true}, nil
}

// FinalizeTaskPurge rechecks the operation's CAS, fencing lease, task state,
// and dependencies in one write transaction. It invokes RemoveDirectory while
// SQLite holds that write lock, so local lifecycle mutations cannot interleave
// with the irreversible directory removal. Filesystem removal is deliberately
// idempotent: a crash after removal but before commit is recovered by replay.
func (s *Store) FinalizeTaskPurge(ctx context.Context, request FinalizeTaskPurgeRequest) (FinalizeTaskPurgeResult, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return FinalizeTaskPurgeResult{}, err
	}
	prepared, err := prepareFinalizeTaskPurgeRequest(request)
	if err != nil {
		return FinalizeTaskPurgeResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return FinalizeTaskPurgeResult{}, err
	}
	defer tx.Rollback()
	operation, err := getTaskPurgeOperationTx(ctx, tx, prepared.operationID)
	if err != nil {
		return FinalizeTaskPurgeResult{}, err
	}
	if operation.Version != prepared.expectedVersion {
		return FinalizeTaskPurgeResult{}, fmt.Errorf("%w: task purge operation %s", ErrOptimisticLock, operation.ID)
	}
	if operation.State == TaskPurgeCompleted {
		if err := tx.Commit(); err != nil {
			return FinalizeTaskPurgeResult{}, err
		}
		return FinalizeTaskPurgeResult{Operation: operation, Dependencies: operation.Dependencies, Purged: true}, nil
	}
	if operation.State == TaskPurgeBlocked {
		if err := tx.Commit(); err != nil {
			return FinalizeTaskPurgeResult{}, err
		}
		return FinalizeTaskPurgeResult{Operation: operation, Dependencies: operation.Dependencies}, nil
	}
	if operation.State != TaskPurgeInProgress {
		return FinalizeTaskPurgeResult{}, fmt.Errorf("%w: unsupported task purge state %q", ErrInvalidTransition, operation.State)
	}
	now := s.now().UTC()
	task, err := getTaskV2Tx(ctx, tx, operation.TaskID)
	if err != nil {
		return FinalizeTaskPurgeResult{}, err
	}
	if task.Version != operation.ExpectedTaskVersion {
		return FinalizeTaskPurgeResult{}, fmt.Errorf("%w: task %s", ErrOptimisticLock, task.ID)
	}
	if task.LifecycleState != TaskLifecycleDeleted {
		return FinalizeTaskPurgeResult{}, fmt.Errorf("%w: task %s is no longer soft-deleted", ErrInvalidTransition, task.ID)
	}
	lease, err := getLeaseTx(ctx, tx, operation.LeaseID)
	if err != nil {
		return FinalizeTaskPurgeResult{}, err
	}
	if err := s.validateTaskPurgeLeaseTx(ctx, tx, lease, operation, now); err != nil {
		return FinalizeTaskPurgeResult{}, err
	}

	// This write obtains SQLite's exclusive writer slot before the filesystem
	// boundary. Rollback restores this heartbeat if RemoveDirectory fails.
	result, err := tx.ExecContext(ctx, `
		UPDATE task_purge_operations_v7 SET updated_at = ?
		WHERE id = ? AND state = 'in_progress' AND version = ?
	`, now, operation.ID, operation.Version)
	if err != nil {
		return FinalizeTaskPurgeResult{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return FinalizeTaskPurgeResult{}, err
	}
	if changed != 1 {
		return FinalizeTaskPurgeResult{}, fmt.Errorf("%w: task purge operation %s", ErrOptimisticLock, operation.ID)
	}
	dependencies, err := queryPurgeDependenciesForTask(ctx, tx, task.ID)
	if err != nil {
		return FinalizeTaskPurgeResult{}, err
	}
	dependenciesJSON, err := encodePurgeDependencies(dependencies)
	if err != nil {
		return FinalizeTaskPurgeResult{}, err
	}
	if dependencies.HasBlockers() {
		operation.State = TaskPurgeBlocked
		operation.Dependencies = dependencies
		operation.UpdatedAt = now
		operation.Version++
		if err := updateTaskPurgeOperationTx(ctx, tx, operation, dependenciesJSON); err != nil {
			return FinalizeTaskPurgeResult{}, err
		}
		if err := s.releaseTaskPurgeLeaseTx(ctx, tx, lease, prepared.actor, prepared.reason, now); err != nil {
			return FinalizeTaskPurgeResult{}, err
		}
		if err := s.appendTaskPurgeAuditTx(ctx, tx, operation, "task_purge.blocked", now); err != nil {
			return FinalizeTaskPurgeResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return FinalizeTaskPurgeResult{}, err
		}
		return FinalizeTaskPurgeResult{Operation: operation, Dependencies: dependencies}, nil
	}
	if err := prepared.removeDirectory(); err != nil {
		return FinalizeTaskPurgeResult{Operation: operation, Dependencies: dependencies}, fmt.Errorf("%w: %v", ErrTaskPurgeFilesystem, err)
	}
	operation.State = TaskPurgeCompleted
	operation.Dependencies = dependencies
	operation.LastError = ""
	operation.UpdatedAt = now
	operation.CompletedAt = &now
	operation.Version++
	if err := updateTaskPurgeOperationTx(ctx, tx, operation, dependenciesJSON); err != nil {
		return FinalizeTaskPurgeResult{}, err
	}
	if err := s.releaseTaskPurgeLeaseTx(ctx, tx, lease, prepared.actor, prepared.reason, now); err != nil {
		return FinalizeTaskPurgeResult{}, err
	}
	if err := s.appendTaskPurgeAuditTx(ctx, tx, operation, "task_purge.completed", now); err != nil {
		return FinalizeTaskPurgeResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return FinalizeTaskPurgeResult{}, err
	}
	return FinalizeTaskPurgeResult{Operation: operation, Dependencies: dependencies, Purged: true}, nil
}

// RecordTaskPurgeFailure makes a local filesystem failure visible and releases
// the fencing lease so the same idempotency key can reconcile immediately.
func (s *Store) RecordTaskPurgeFailure(ctx context.Context, request RecordTaskPurgeFailureRequest) (TaskPurgeOperation, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return TaskPurgeOperation{}, err
	}
	prepared, err := prepareRecordTaskPurgeFailureRequest(request)
	if err != nil {
		return TaskPurgeOperation{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TaskPurgeOperation{}, err
	}
	defer tx.Rollback()
	operation, err := getTaskPurgeOperationTx(ctx, tx, prepared.operationID)
	if err != nil {
		return TaskPurgeOperation{}, err
	}
	if operation.Version != prepared.expectedVersion {
		return TaskPurgeOperation{}, fmt.Errorf("%w: task purge operation %s", ErrOptimisticLock, operation.ID)
	}
	if operation.State != TaskPurgeInProgress {
		return TaskPurgeOperation{}, fmt.Errorf("%w: task purge operation %s is %s", ErrImmutable, operation.ID, operation.State)
	}
	lease, err := getLeaseTx(ctx, tx, operation.LeaseID)
	if err != nil {
		return TaskPurgeOperation{}, err
	}
	now := s.now().UTC()
	if err := s.validateTaskPurgeLeaseTx(ctx, tx, lease, operation, now); err != nil {
		return TaskPurgeOperation{}, err
	}
	operation.LastError = prepared.errorText
	operation.UpdatedAt = now
	operation.Version++
	encoded, err := encodePurgeDependencies(operation.Dependencies)
	if err != nil {
		return TaskPurgeOperation{}, err
	}
	if err := updateTaskPurgeOperationTx(ctx, tx, operation, encoded); err != nil {
		return TaskPurgeOperation{}, err
	}
	if err := s.releaseTaskPurgeLeaseTx(ctx, tx, lease, prepared.actor, prepared.reason, now); err != nil {
		return TaskPurgeOperation{}, err
	}
	if err := s.appendTaskPurgeAuditTx(ctx, tx, operation, "task_purge.filesystem_failed", now); err != nil {
		return TaskPurgeOperation{}, err
	}
	if err := tx.Commit(); err != nil {
		return TaskPurgeOperation{}, err
	}
	return operation, nil
}

func (s *Store) GetTaskPurgeOperation(ctx context.Context, operationID string) (*TaskPurgeOperation, error) {
	if !isUUIDv7(operationID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	operation, err := scanTaskPurgeOperation(s.db.QueryRowContext(ctx, taskPurgeOperationV7Select+" WHERE id = ?", operationID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &operation, nil
}

// GetCompletedTaskPurge returns the irreversible tombstone for a task, if
// one exists. It is a read-only projection used by preview surfaces so they
// do not present a completed purge as executable again.
func (s *Store) GetCompletedTaskPurge(ctx context.Context, taskID string) (*TaskPurgeOperation, error) {
	if !isUUIDv7(taskID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	operation, err := scanTaskPurgeOperation(s.db.QueryRowContext(ctx, taskPurgeOperationV7Select+" WHERE task_id = ? AND state = 'completed' ORDER BY completed_at DESC, id DESC LIMIT 1", taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &operation, nil
}

// GetInProgressTaskPurge returns an operation that currently owns the task's
// purge protocol, including a recoverable filesystem failure that awaits a
// same-key retry. Preview surfaces use it to avoid suggesting a competing
// command while an irreversible operation is unresolved.
func (s *Store) GetInProgressTaskPurge(ctx context.Context, taskID string) (*TaskPurgeOperation, error) {
	if !isUUIDv7(taskID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	operation, err := scanTaskPurgeOperation(s.db.QueryRowContext(ctx, taskPurgeOperationV7Select+" WHERE task_id = ? AND state = 'in_progress'", taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &operation, nil
}

type preparedTaskPurgeRequest struct {
	id                  string
	taskID              string
	expectedTaskVersion int64
	idempotencyKey      string
	actor               string
	reason              string
	leaseTTL            time.Duration
}

func prepareTaskPurgeRequestValue(s *Store, request PrepareTaskPurgeRequest) (preparedTaskPurgeRequest, error) {
	if !isUUIDv7(request.TaskID) {
		return preparedTaskPurgeRequest{}, ErrInvalidUUIDv7Identity
	}
	if request.ExpectedTaskVersion <= 0 {
		return preparedTaskPurgeRequest{}, fmt.Errorf("expected task version must be positive")
	}
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return preparedTaskPurgeRequest{}, err
	}
	key, err := normalizeRequired(request.IdempotencyKey, "task purge idempotency key")
	if err != nil {
		return preparedTaskPurgeRequest{}, err
	}
	actor, err := normalizeRequired(request.Actor, "task purge actor")
	if err != nil {
		return preparedTaskPurgeRequest{}, err
	}
	reason, err := normalizeRequired(request.Reason, "task purge reason")
	if err != nil {
		return preparedTaskPurgeRequest{}, err
	}
	ttl := request.LeaseTTL
	if ttl == 0 {
		ttl = DefaultLeaseTTL
	}
	if ttl <= 0 {
		return preparedTaskPurgeRequest{}, fmt.Errorf("task purge lease TTL must be positive")
	}
	return preparedTaskPurgeRequest{
		id: id, taskID: request.TaskID, expectedTaskVersion: request.ExpectedTaskVersion,
		idempotencyKey: key, actor: actor, reason: reason, leaseTTL: ttl,
	}, nil
}

type preparedFinalizeTaskPurgeRequest struct {
	operationID     string
	expectedVersion int64
	actor           string
	reason          string
	removeDirectory func() error
}

func prepareFinalizeTaskPurgeRequest(request FinalizeTaskPurgeRequest) (preparedFinalizeTaskPurgeRequest, error) {
	if !isUUIDv7(request.OperationID) {
		return preparedFinalizeTaskPurgeRequest{}, ErrInvalidUUIDv7Identity
	}
	if request.ExpectedVersion <= 0 {
		return preparedFinalizeTaskPurgeRequest{}, fmt.Errorf("expected task purge operation version must be positive")
	}
	actor, err := normalizeRequired(request.Actor, "task purge actor")
	if err != nil {
		return preparedFinalizeTaskPurgeRequest{}, err
	}
	reason, err := normalizeRequired(request.Reason, "task purge reason")
	if err != nil {
		return preparedFinalizeTaskPurgeRequest{}, err
	}
	if request.RemoveDirectory == nil {
		return preparedFinalizeTaskPurgeRequest{}, fmt.Errorf("task purge remove directory callback is required")
	}
	return preparedFinalizeTaskPurgeRequest{
		operationID: request.OperationID, expectedVersion: request.ExpectedVersion, actor: actor,
		reason: reason, removeDirectory: request.RemoveDirectory,
	}, nil
}

type preparedRecordTaskPurgeFailureRequest struct {
	operationID     string
	expectedVersion int64
	actor           string
	reason          string
	errorText       string
}

func prepareRecordTaskPurgeFailureRequest(request RecordTaskPurgeFailureRequest) (preparedRecordTaskPurgeFailureRequest, error) {
	if !isUUIDv7(request.OperationID) {
		return preparedRecordTaskPurgeFailureRequest{}, ErrInvalidUUIDv7Identity
	}
	if request.ExpectedVersion <= 0 {
		return preparedRecordTaskPurgeFailureRequest{}, fmt.Errorf("expected task purge operation version must be positive")
	}
	actor, err := normalizeRequired(request.Actor, "task purge actor")
	if err != nil {
		return preparedRecordTaskPurgeFailureRequest{}, err
	}
	reason, err := normalizeRequired(request.Reason, "task purge reason")
	if err != nil {
		return preparedRecordTaskPurgeFailureRequest{}, err
	}
	errorText, err := normalizeRequired(request.ErrorText, "task purge error")
	if err != nil {
		return preparedRecordTaskPurgeFailureRequest{}, err
	}
	return preparedRecordTaskPurgeFailureRequest{
		operationID: request.OperationID, expectedVersion: request.ExpectedVersion, actor: actor,
		reason: reason, errorText: errorText,
	}, nil
}

func (s *Store) replayTaskPurgeTx(ctx context.Context, tx *sql.Tx, task TaskV2, operation TaskPurgeOperation, request preparedTaskPurgeRequest) (PrepareTaskPurgeResult, error) {
	if operation.State == TaskPurgeBlocked || operation.State == TaskPurgeCompleted {
		return PrepareTaskPurgeResult{Task: task, Operation: operation}, nil
	}
	if operation.State != TaskPurgeInProgress {
		return PrepareTaskPurgeResult{}, fmt.Errorf("%w: unsupported task purge state %q", ErrInvalidTransition, operation.State)
	}
	if task.Version != operation.ExpectedTaskVersion {
		return PrepareTaskPurgeResult{}, fmt.Errorf("%w: task %s", ErrOptimisticLock, task.ID)
	}
	if task.LifecycleState != TaskLifecycleDeleted {
		return PrepareTaskPurgeResult{}, fmt.Errorf("%w: task %s is no longer soft-deleted", ErrInvalidTransition, task.ID)
	}
	lease, err := getLeaseTx(ctx, tx, operation.LeaseID)
	if err != nil {
		return PrepareTaskPurgeResult{}, err
	}
	now := s.now().UTC()
	if lease.State == LeaseActive && lease.ExpiresAt.After(now) {
		return PrepareTaskPurgeResult{Task: task, Operation: operation}, nil
	}
	if lease.State == LeaseActive {
		if err := s.expireLeaseTx(ctx, tx, lease, request.actor, now, "task purge replay observed expired lease"); err != nil {
			return PrepareTaskPurgeResult{}, err
		}
	}
	newLease, err := s.acquireTaskPurgeLeaseTx(ctx, tx, task.ID, taskPurgeLeaseOwner(operation.ID), request.leaseTTL, request.actor, request.reason, now)
	if err != nil {
		return PrepareTaskPurgeResult{}, err
	}
	operation.LeaseID = newLease.ID
	operation.LeaseOwner = newLease.Owner
	operation.LeaseFencingToken = newLease.FencingToken
	operation.LeaseVersion = newLease.Version
	operation.LastError = ""
	operation.UpdatedAt = now
	operation.Version++
	encoded, err := encodePurgeDependencies(operation.Dependencies)
	if err != nil {
		return PrepareTaskPurgeResult{}, err
	}
	if err := updateTaskPurgeOperationTx(ctx, tx, operation, encoded); err != nil {
		return PrepareTaskPurgeResult{}, err
	}
	if err := s.appendTaskPurgeAuditTx(ctx, tx, operation, "task_purge.reclaimed", now); err != nil {
		return PrepareTaskPurgeResult{}, err
	}
	return PrepareTaskPurgeResult{Task: task, Operation: operation, Acquired: true}, nil
}

func sameTaskPurgeRequest(operation TaskPurgeOperation, request preparedTaskPurgeRequest) bool {
	return operation.TaskID == request.taskID && operation.ExpectedTaskVersion == request.expectedTaskVersion &&
		operation.Actor == request.actor && operation.Reason == request.reason
}

func taskPurgeLeaseOwner(operationID string) string {
	return "task-purge:" + operationID
}

func (s *Store) acquireTaskPurgeLeaseTx(ctx context.Context, tx *sql.Tx, taskID, owner string, ttl time.Duration, actor, reason string, now time.Time) (Lease, error) {
	existing, err := getActiveLeaseForResourceTx(ctx, tx, "task_purge", taskID)
	if err != nil {
		return Lease{}, err
	}
	if existing != nil {
		if existing.ExpiresAt.After(now) {
			return Lease{}, fmt.Errorf("%w: task purge for %s", ErrLeaseHeld, taskID)
		}
		if err := s.expireLeaseTx(ctx, tx, *existing, actor, now, "task purge observed expired lease"); err != nil {
			return Lease{}, err
		}
	}
	var maximumToken int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(fencing_token), 0) FROM leases WHERE resource_type = 'task_purge' AND resource_id = ?`, taskID).Scan(&maximumToken); err != nil {
		return Lease{}, err
	}
	if maximumToken < 0 || maximumToken == int64(^uint64(0)>>1) {
		return Lease{}, fmt.Errorf("task purge lease fencing token overflow")
	}
	id, err := s.newV2ID("")
	if err != nil {
		return Lease{}, err
	}
	lease := Lease{
		ID: id, ResourceType: "task_purge", ResourceID: taskID, Owner: owner,
		ExpiresAt: now.Add(ttl), FencingToken: uint64(maximumToken + 1), State: LeaseActive,
		CreatedAt: now, UpdatedAt: now, Version: 1,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO leases (id, resource_type, resource_id, owner, job_id, expires_at, fencing_token, state, created_at, updated_at, version)
		VALUES (?, ?, ?, ?, NULL, ?, ?, ?, ?, ?, ?)
	`, lease.ID, lease.ResourceType, lease.ResourceID, lease.Owner, lease.ExpiresAt, int64(lease.FencingToken), lease.State, lease.CreatedAt, lease.UpdatedAt, lease.Version); err != nil {
		if isGlobalIdentityCollision(err) {
			return Lease{}, fmt.Errorf("%w: task purge lease %s", ErrIdentityCollision, lease.ID)
		}
		if isUniqueConstraint(err) {
			return Lease{}, fmt.Errorf("%w: task purge for %s", ErrLeaseHeld, taskID)
		}
		return Lease{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: actor, EntityType: "lease", EntityID: lease.ID, Action: "lease.acquired_for_task_purge", Reason: reason,
		PayloadJSON: auditPayload(map[string]any{"task_id": taskID, "fencing_token": lease.FencingToken}), CreatedAt: now,
	}); err != nil {
		return Lease{}, err
	}
	return lease, nil
}

func (s *Store) validateTaskPurgeLeaseTx(ctx context.Context, tx *sql.Tx, lease Lease, operation TaskPurgeOperation, now time.Time) error {
	if lease.State != LeaseActive || lease.Owner != operation.LeaseOwner || lease.FencingToken != operation.LeaseFencingToken || lease.Version != operation.LeaseVersion {
		return fmt.Errorf("%w: task purge lease %s", ErrFencingToken, lease.ID)
	}
	if !lease.ExpiresAt.After(now) {
		if err := s.expireLeaseTx(ctx, tx, lease, operation.Actor, now, "task purge observed expired lease"); err != nil {
			return err
		}
		return fmt.Errorf("%w: task purge lease %s expired", ErrLeaseHeld, lease.ID)
	}
	return nil
}

func (s *Store) releaseTaskPurgeLeaseTx(ctx context.Context, tx *sql.Tx, lease Lease, actor, reason string, now time.Time) error {
	result, err := tx.ExecContext(ctx, `
		UPDATE leases SET state = 'released', updated_at = ?, version = version + 1
		WHERE id = ? AND state = 'active' AND version = ? AND fencing_token = ?
	`, now, lease.ID, lease.Version, int64(lease.FencingToken))
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("%w: task purge lease %s", ErrOptimisticLock, lease.ID)
	}
	_, err = s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: actor, EntityType: "lease", EntityID: lease.ID, Action: "lease.released_for_task_purge", Reason: reason,
		PayloadJSON: auditPayload(map[string]any{"task_id": lease.ResourceID, "fencing_token": lease.FencingToken}), CreatedAt: now,
	})
	return err
}

func (s *Store) insertTaskPurgeOperationTx(ctx context.Context, tx *sql.Tx, operation TaskPurgeOperation, dependenciesJSON string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO task_purge_operations_v7 (
			id, task_id, idempotency_key, expected_task_version, actor, reason, state,
			lease_id, lease_owner, lease_fencing_token, lease_version, dependencies_json, last_error,
			created_at, updated_at, completed_at, version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, operation.ID, operation.TaskID, operation.IdempotencyKey, operation.ExpectedTaskVersion, operation.Actor, operation.Reason, operation.State,
		nullableString(operation.LeaseID), operation.LeaseOwner, int64(operation.LeaseFencingToken), operation.LeaseVersion,
		dependenciesJSON, operation.LastError, operation.CreatedAt, operation.UpdatedAt, operation.CompletedAt, operation.Version)
	if err != nil {
		if isGlobalIdentityCollision(err) {
			return fmt.Errorf("%w: task purge operation %s", ErrIdentityCollision, operation.ID)
		}
		if isUniqueConstraint(err) {
			return fmt.Errorf("%w: task purge operation %s", ErrTaskPurgeInProgress, operation.ID)
		}
	}
	return err
}

func updateTaskPurgeOperationTx(ctx context.Context, tx *sql.Tx, operation TaskPurgeOperation, dependenciesJSON string) error {
	result, err := tx.ExecContext(ctx, `
		UPDATE task_purge_operations_v7
		SET state = ?, lease_id = ?, lease_owner = ?, lease_fencing_token = ?, lease_version = ?,
			dependencies_json = ?, last_error = ?, updated_at = ?, completed_at = ?, version = ?
		WHERE id = ? AND version = ?
	`, operation.State, nullableString(operation.LeaseID), operation.LeaseOwner, int64(operation.LeaseFencingToken), operation.LeaseVersion,
		dependenciesJSON, operation.LastError, operation.UpdatedAt, operation.CompletedAt, operation.Version,
		operation.ID, operation.Version-1)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("%w: task purge operation %s", ErrOptimisticLock, operation.ID)
	}
	return nil
}

func getTaskPurgeOperationTx(ctx context.Context, tx *sql.Tx, operationID string) (TaskPurgeOperation, error) {
	operation, err := scanTaskPurgeOperation(tx.QueryRowContext(ctx, taskPurgeOperationV7Select+" WHERE id = ?", operationID))
	if errors.Is(err, sql.ErrNoRows) {
		return TaskPurgeOperation{}, fmt.Errorf("%w: task purge operation %s", ErrNotFound, operationID)
	}
	return operation, err
}

func getTaskPurgeOperationByKeyTx(ctx context.Context, tx *sql.Tx, key string) (*TaskPurgeOperation, error) {
	operation, err := scanTaskPurgeOperation(tx.QueryRowContext(ctx, taskPurgeOperationV7Select+" WHERE idempotency_key = ?", key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &operation, nil
}

func getInProgressTaskPurgeForTaskTx(ctx context.Context, tx *sql.Tx, taskID string) (*TaskPurgeOperation, error) {
	operation, err := scanTaskPurgeOperation(tx.QueryRowContext(ctx, taskPurgeOperationV7Select+" WHERE task_id = ? AND state = 'in_progress'", taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &operation, nil
}

func getCompletedTaskPurgeForTaskTx(ctx context.Context, tx *sql.Tx, taskID string) (*TaskPurgeOperation, error) {
	operation, err := scanTaskPurgeOperation(tx.QueryRowContext(ctx, taskPurgeOperationV7Select+" WHERE task_id = ? AND state = 'completed' ORDER BY completed_at DESC, id DESC LIMIT 1", taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &operation, nil
}

func scanTaskPurgeOperation(scanner rowScanner) (TaskPurgeOperation, error) {
	var operation TaskPurgeOperation
	var leaseID sql.NullString
	var fencingToken int64
	var completedAt sql.NullTime
	var dependenciesJSON string
	if err := scanner.Scan(
		&operation.ID, &operation.TaskID, &operation.IdempotencyKey, &operation.ExpectedTaskVersion,
		&operation.Actor, &operation.Reason, &operation.State, &leaseID, &operation.LeaseOwner,
		&fencingToken, &operation.LeaseVersion, &dependenciesJSON, &operation.LastError,
		&operation.CreatedAt, &operation.UpdatedAt, &completedAt, &operation.Version,
	); err != nil {
		return TaskPurgeOperation{}, err
	}
	if !validTaskPurgeOperationState(operation.State) || fencingToken < 0 || operation.LeaseVersion < 0 || operation.Version <= 0 {
		return TaskPurgeOperation{}, fmt.Errorf("invalid persisted task purge operation %s", operation.ID)
	}
	dependencies, err := decodePurgeDependencies(dependenciesJSON)
	if err != nil {
		return TaskPurgeOperation{}, fmt.Errorf("decode task purge operation dependencies: %w", err)
	}
	if dependencies.TaskID != operation.TaskID {
		return TaskPurgeOperation{}, fmt.Errorf("invalid persisted task purge operation %s dependency task", operation.ID)
	}
	operation.LeaseID = nullableStringValue(leaseID)
	operation.LeaseFencingToken = uint64(fencingToken)
	operation.Dependencies = dependencies
	operation.CreatedAt = operation.CreatedAt.UTC()
	operation.UpdatedAt = operation.UpdatedAt.UTC()
	operation.CompletedAt = nullableTimePtr(completedAt)
	return operation, nil
}

func validTaskPurgeOperationState(state TaskPurgeOperationState) bool {
	return state == TaskPurgeInProgress || state == TaskPurgeBlocked || state == TaskPurgeCompleted
}

func encodePurgeDependencies(report PurgeDependencyReport) (string, error) {
	encoded, err := json.Marshal(report)
	if err != nil {
		return "", err
	}
	return normalizeJSON(string(encoded), "task purge dependencies")
}

func decodePurgeDependencies(value string) (PurgeDependencyReport, error) {
	var report PurgeDependencyReport
	if err := json.Unmarshal([]byte(strings.TrimSpace(value)), &report); err != nil {
		return PurgeDependencyReport{}, err
	}
	if !isUUIDv7(report.TaskID) {
		return PurgeDependencyReport{}, ErrInvalidUUIDv7Identity
	}
	return report, nil
}

func (s *Store) appendTaskPurgeAuditTx(ctx context.Context, tx *sql.Tx, operation TaskPurgeOperation, action string, now time.Time) error {
	_, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: operation.Actor, EntityType: "task_purge_operation", EntityID: operation.ID, Action: action, Reason: operation.Reason,
		PayloadJSON: auditPayload(map[string]any{
			"task_id": operation.TaskID, "expected_task_version": operation.ExpectedTaskVersion,
			"state": operation.State, "lease_id": operation.LeaseID, "fencing_token": operation.LeaseFencingToken,
			"version": operation.Version, "dependencies": operation.Dependencies,
		}),
		OperationKey: operation.IdempotencyKey, CreatedAt: now,
	})
	return err
}
