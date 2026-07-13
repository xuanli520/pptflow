package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const candidateGarbageCollectionOperationV10Select = `
	SELECT id, candidate_id, idempotency_key, expected_candidate_version,
	       actor, reason, state, last_error, created_at, updated_at,
	       completed_at, version
	FROM candidate_gc_operations_v10`

const releaseWithdrawOperationV10Select = `
	SELECT id, release_id, idempotency_key, expected_release_version, actor,
	       reason, state, receipt_id, result_release_version, created_at,
	       completed_at
	FROM release_withdraw_operations_v10`

const releaseWithdrawReceiptV10Select = `
	SELECT id, operation_id, release_id, release_version, expected_record_version,
	       result_record_version, receipt_json, receipt_digest, created_by, created_at
	FROM release_withdraw_receipts_v10`

// ListRevisionCandidatesReadyForGarbageCollection returns only terminal
// candidates whose fixed retention interval has elapsed and whose checkout
// has not already been tombstoned. It is intentionally read-only so a worker
// can choose and persist a separate idempotent operation for each candidate.
func (s *Store) ListRevisionCandidatesReadyForGarbageCollection(ctx context.Context, limit int) ([]RevisionCandidate, error) {
	if limit < 0 {
		return nil, fmt.Errorf("candidate garbage collection limit cannot be negative")
	}
	query := revisionCandidateV8Select + `
		WHERE state IN ('no_op', 'discarded')
		  AND retain_until IS NOT NULL
		  AND retain_until <= ?
		  AND checkout_tombstoned_at IS NULL
		ORDER BY retain_until ASC, id ASC`
	args := []any{s.now().UTC()}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	candidates := make([]RevisionCandidate, 0)
	for rows.Next() {
		candidate, err := scanRevisionCandidate(rows)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

// PrepareCandidateGarbageCollection captures a terminal candidate's CAS
// checkpoint before the managed checkout directory is touched. The candidate,
// its immutable evidence, UUID reservations, and audit lineage stay intact.
func (s *Store) PrepareCandidateGarbageCollection(ctx context.Context, request PrepareCandidateGarbageCollectionRequest) (PrepareCandidateGarbageCollectionResult, error) {
	prepared, err := prepareCandidateGarbageCollectionRequest(s, request)
	if err != nil {
		return PrepareCandidateGarbageCollectionResult{}, err
	}
	if _, err := s.BackupBeforeCriticalOperation(ctx, "candidate_garbage_collection_prepare"); err != nil {
		return PrepareCandidateGarbageCollectionResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PrepareCandidateGarbageCollectionResult{}, err
	}
	defer tx.Rollback()

	if existing, err := getCandidateGarbageCollectionOperationByKeyTx(ctx, tx, prepared.idempotencyKey); err != nil {
		return PrepareCandidateGarbageCollectionResult{}, err
	} else if existing != nil {
		if !sameCandidateGarbageCollectionRequest(*existing, prepared) {
			return PrepareCandidateGarbageCollectionResult{}, fmt.Errorf("%w: candidate garbage collection key %s", ErrIdempotencyConflict, prepared.idempotencyKey)
		}
		candidate, err := getRevisionCandidateTx(ctx, tx, existing.CandidateID)
		if err != nil {
			return PrepareCandidateGarbageCollectionResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return PrepareCandidateGarbageCollectionResult{}, err
		}
		return PrepareCandidateGarbageCollectionResult{Candidate: candidate, Operation: *existing}, nil
	}

	candidate, err := getRevisionCandidateTx(ctx, tx, prepared.candidateID)
	if err != nil {
		return PrepareCandidateGarbageCollectionResult{}, err
	}
	if candidate.CheckoutTombstonedAt != nil {
		return PrepareCandidateGarbageCollectionResult{}, fmt.Errorf("%w: candidate checkout %s is already tombstoned", ErrImmutable, candidate.ID)
	}
	if candidate.Version != prepared.expectedCandidateVersion {
		return PrepareCandidateGarbageCollectionResult{}, fmt.Errorf("%w: revision candidate %s", ErrOptimisticLock, candidate.ID)
	}
	now := s.now().UTC()
	if !candidateReadyForGarbageCollection(candidate, now) {
		return PrepareCandidateGarbageCollectionResult{}, fmt.Errorf("%w: revision candidate %s is not eligible for retention cleanup", ErrInvalidTransition, candidate.ID)
	}
	if active, err := getInProgressCandidateGarbageCollectionForCandidateTx(ctx, tx, candidate.ID); err != nil {
		return PrepareCandidateGarbageCollectionResult{}, err
	} else if active != nil {
		return PrepareCandidateGarbageCollectionResult{}, fmt.Errorf("%w: candidate %s operation %s", ErrCandidateGCInProgress, candidate.ID, active.ID)
	}
	operation := CandidateGarbageCollectionOperation{
		ID:                       prepared.id,
		CandidateID:              candidate.ID,
		IdempotencyKey:           prepared.idempotencyKey,
		ExpectedCandidateVersion: prepared.expectedCandidateVersion,
		Actor:                    prepared.actor,
		Reason:                   prepared.reason,
		State:                    CandidateGarbageCollectionInProgress,
		CreatedAt:                now,
		UpdatedAt:                now,
		Version:                  1,
	}
	if err := insertCandidateGarbageCollectionOperationTx(ctx, tx, operation); err != nil {
		return PrepareCandidateGarbageCollectionResult{}, err
	}
	if err := s.appendCandidateGarbageCollectionAuditTx(ctx, tx, operation, "candidate_gc.prepared", now); err != nil {
		return PrepareCandidateGarbageCollectionResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return PrepareCandidateGarbageCollectionResult{}, err
	}
	return PrepareCandidateGarbageCollectionResult{Candidate: candidate, Operation: operation}, nil
}

// FinalizeCandidateGarbageCollection performs the one allowed material
// deletion while SQLite holds the write transaction. A missing directory is a
// successful replay state after a process stops between deletion and commit.
func (s *Store) FinalizeCandidateGarbageCollection(ctx context.Context, request FinalizeCandidateGarbageCollectionRequest) (FinalizeCandidateGarbageCollectionResult, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return FinalizeCandidateGarbageCollectionResult{}, err
	}
	prepared, err := prepareFinalizeCandidateGarbageCollectionRequest(request)
	if err != nil {
		return FinalizeCandidateGarbageCollectionResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return FinalizeCandidateGarbageCollectionResult{}, err
	}
	defer tx.Rollback()
	operation, err := getCandidateGarbageCollectionOperationTx(ctx, tx, prepared.operationID)
	if err != nil {
		return FinalizeCandidateGarbageCollectionResult{}, err
	}
	if operation.Version != prepared.expectedVersion {
		return FinalizeCandidateGarbageCollectionResult{}, fmt.Errorf("%w: candidate garbage collection operation %s", ErrOptimisticLock, operation.ID)
	}
	candidate, err := getRevisionCandidateTx(ctx, tx, operation.CandidateID)
	if err != nil {
		return FinalizeCandidateGarbageCollectionResult{}, err
	}
	if operation.State == CandidateGarbageCollectionCompleted {
		if candidate.CheckoutTombstonedAt == nil {
			return FinalizeCandidateGarbageCollectionResult{}, fmt.Errorf("completed candidate garbage collection %s has no candidate tombstone", operation.ID)
		}
		if err := tx.Commit(); err != nil {
			return FinalizeCandidateGarbageCollectionResult{}, err
		}
		return FinalizeCandidateGarbageCollectionResult{Candidate: candidate, Operation: operation, Collected: true}, nil
	}
	if operation.State != CandidateGarbageCollectionInProgress {
		return FinalizeCandidateGarbageCollectionResult{}, fmt.Errorf("%w: unsupported candidate garbage collection state %q", ErrInvalidTransition, operation.State)
	}
	now := s.now().UTC()
	if candidate.Version != operation.ExpectedCandidateVersion {
		return FinalizeCandidateGarbageCollectionResult{}, fmt.Errorf("%w: revision candidate %s", ErrOptimisticLock, candidate.ID)
	}
	if !candidateReadyForGarbageCollection(candidate, now) {
		return FinalizeCandidateGarbageCollectionResult{}, fmt.Errorf("%w: revision candidate %s is no longer eligible for retention cleanup", ErrInvalidTransition, candidate.ID)
	}
	if candidate.CheckoutTombstonedAt != nil {
		return FinalizeCandidateGarbageCollectionResult{}, fmt.Errorf("%w: candidate checkout %s is already tombstoned", ErrImmutable, candidate.ID)
	}
	if err := prepared.removeDirectory(); err != nil {
		return FinalizeCandidateGarbageCollectionResult{Candidate: candidate, Operation: operation}, fmt.Errorf("%w: %v", ErrCandidateGCFilesystem, err)
	}
	candidate.CheckoutTombstonedAt = &now
	candidate.CheckoutTombstonedBy = prepared.actor
	candidate.UpdatedAt = now
	candidate.Version++
	if err := updateRevisionCandidateTx(ctx, tx, candidate, operation.ExpectedCandidateVersion); err != nil {
		return FinalizeCandidateGarbageCollectionResult{}, err
	}
	operation.State = CandidateGarbageCollectionCompleted
	operation.LastError = ""
	operation.UpdatedAt = now
	operation.CompletedAt = &now
	operation.Version++
	if err := updateCandidateGarbageCollectionOperationTx(ctx, tx, operation); err != nil {
		return FinalizeCandidateGarbageCollectionResult{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: operation.Actor, EntityType: "revision_candidate", EntityID: candidate.ID,
		Action: "revision_candidate.checkout_tombstoned", Reason: operation.Reason,
		PayloadJSON: auditPayload(map[string]any{
			"operation_id": operation.ID, "retain_until": candidate.RetainUntil,
			"target_revision_id": candidate.TargetRevisionID, "target_run_id": candidate.TargetRunID,
		}),
		OperationKey: operation.IdempotencyKey, CreatedAt: now,
	}); err != nil {
		return FinalizeCandidateGarbageCollectionResult{}, err
	}
	if err := s.appendCandidateGarbageCollectionAuditTx(ctx, tx, operation, "candidate_gc.completed", now); err != nil {
		return FinalizeCandidateGarbageCollectionResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return FinalizeCandidateGarbageCollectionResult{}, err
	}
	return FinalizeCandidateGarbageCollectionResult{Candidate: candidate, Operation: operation, Collected: true}, nil
}

// RecordCandidateGarbageCollectionFailure records a filesystem failure and
// leaves the same operation in progress for an idempotent retry. It never
// clears the candidate's retention facts or alters immutable evidence.
func (s *Store) RecordCandidateGarbageCollectionFailure(ctx context.Context, request RecordCandidateGarbageCollectionFailureRequest) (CandidateGarbageCollectionOperation, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return CandidateGarbageCollectionOperation{}, err
	}
	prepared, err := prepareRecordCandidateGarbageCollectionFailureRequest(request)
	if err != nil {
		return CandidateGarbageCollectionOperation{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CandidateGarbageCollectionOperation{}, err
	}
	defer tx.Rollback()
	operation, err := getCandidateGarbageCollectionOperationTx(ctx, tx, prepared.operationID)
	if err != nil {
		return CandidateGarbageCollectionOperation{}, err
	}
	if operation.Version != prepared.expectedVersion {
		return CandidateGarbageCollectionOperation{}, fmt.Errorf("%w: candidate garbage collection operation %s", ErrOptimisticLock, operation.ID)
	}
	if operation.State != CandidateGarbageCollectionInProgress {
		return CandidateGarbageCollectionOperation{}, fmt.Errorf("%w: candidate garbage collection operation %s is %s", ErrImmutable, operation.ID, operation.State)
	}
	operation.LastError = prepared.errorText
	operation.UpdatedAt = s.now().UTC()
	operation.Version++
	if err := updateCandidateGarbageCollectionOperationTx(ctx, tx, operation); err != nil {
		return CandidateGarbageCollectionOperation{}, err
	}
	if err := s.appendCandidateGarbageCollectionAuditTx(ctx, tx, operation, "candidate_gc.filesystem_failed", operation.UpdatedAt); err != nil {
		return CandidateGarbageCollectionOperation{}, err
	}
	if err := tx.Commit(); err != nil {
		return CandidateGarbageCollectionOperation{}, err
	}
	return operation, nil
}

func (s *Store) GetCandidateGarbageCollectionOperation(ctx context.Context, operationID string) (*CandidateGarbageCollectionOperation, error) {
	if !isUUIDv7(operationID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	operation, err := scanCandidateGarbageCollectionOperation(s.db.QueryRowContext(ctx, candidateGarbageCollectionOperationV10Select+" WHERE id = ?", operationID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &operation, nil
}

func candidateReadyForGarbageCollection(candidate RevisionCandidate, now time.Time) bool {
	if candidate.CheckoutTombstonedAt != nil || candidate.RetainUntil == nil {
		return false
	}
	if candidate.State != RevisionCandidateNoOp && candidate.State != RevisionCandidateDiscarded {
		return false
	}
	return !now.Before(candidate.RetainUntil.UTC())
}

type preparedCandidateGarbageCollectionRequest struct {
	id                       string
	candidateID              string
	expectedCandidateVersion int64
	idempotencyKey           string
	actor                    string
	reason                   string
}

func prepareCandidateGarbageCollectionRequest(s *Store, request PrepareCandidateGarbageCollectionRequest) (preparedCandidateGarbageCollectionRequest, error) {
	if !isUUIDv7(request.CandidateID) || !isUUIDv7(request.IdempotencyKey) {
		return preparedCandidateGarbageCollectionRequest{}, ErrInvalidUUIDv7Identity
	}
	if request.ExpectedCandidateVersion <= 0 {
		return preparedCandidateGarbageCollectionRequest{}, fmt.Errorf("expected candidate version must be positive")
	}
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return preparedCandidateGarbageCollectionRequest{}, err
	}
	actor, err := normalizeRequired(request.Actor, "candidate garbage collection actor")
	if err != nil {
		return preparedCandidateGarbageCollectionRequest{}, err
	}
	reason, err := normalizeRequired(request.Reason, "candidate garbage collection reason")
	if err != nil {
		return preparedCandidateGarbageCollectionRequest{}, err
	}
	return preparedCandidateGarbageCollectionRequest{
		id: id, candidateID: request.CandidateID, expectedCandidateVersion: request.ExpectedCandidateVersion,
		idempotencyKey: request.IdempotencyKey, actor: actor, reason: reason,
	}, nil
}

type preparedFinalizeCandidateGarbageCollectionRequest struct {
	operationID     string
	expectedVersion int64
	actor           string
	reason          string
	removeDirectory func() error
}

func prepareFinalizeCandidateGarbageCollectionRequest(request FinalizeCandidateGarbageCollectionRequest) (preparedFinalizeCandidateGarbageCollectionRequest, error) {
	if !isUUIDv7(request.OperationID) {
		return preparedFinalizeCandidateGarbageCollectionRequest{}, ErrInvalidUUIDv7Identity
	}
	if request.ExpectedVersion <= 0 {
		return preparedFinalizeCandidateGarbageCollectionRequest{}, fmt.Errorf("expected candidate garbage collection operation version must be positive")
	}
	actor, err := normalizeRequired(request.Actor, "candidate garbage collection actor")
	if err != nil {
		return preparedFinalizeCandidateGarbageCollectionRequest{}, err
	}
	reason, err := normalizeRequired(request.Reason, "candidate garbage collection reason")
	if err != nil {
		return preparedFinalizeCandidateGarbageCollectionRequest{}, err
	}
	if request.RemoveDirectory == nil {
		return preparedFinalizeCandidateGarbageCollectionRequest{}, fmt.Errorf("candidate garbage collection remove directory callback is required")
	}
	return preparedFinalizeCandidateGarbageCollectionRequest{
		operationID: request.OperationID, expectedVersion: request.ExpectedVersion, actor: actor,
		reason: reason, removeDirectory: request.RemoveDirectory,
	}, nil
}

type preparedRecordCandidateGarbageCollectionFailureRequest struct {
	operationID     string
	expectedVersion int64
	actor           string
	reason          string
	errorText       string
}

func prepareRecordCandidateGarbageCollectionFailureRequest(request RecordCandidateGarbageCollectionFailureRequest) (preparedRecordCandidateGarbageCollectionFailureRequest, error) {
	if !isUUIDv7(request.OperationID) {
		return preparedRecordCandidateGarbageCollectionFailureRequest{}, ErrInvalidUUIDv7Identity
	}
	if request.ExpectedVersion <= 0 {
		return preparedRecordCandidateGarbageCollectionFailureRequest{}, fmt.Errorf("expected candidate garbage collection operation version must be positive")
	}
	actor, err := normalizeRequired(request.Actor, "candidate garbage collection actor")
	if err != nil {
		return preparedRecordCandidateGarbageCollectionFailureRequest{}, err
	}
	reason, err := normalizeRequired(request.Reason, "candidate garbage collection reason")
	if err != nil {
		return preparedRecordCandidateGarbageCollectionFailureRequest{}, err
	}
	errorText, err := normalizeRequired(request.ErrorText, "candidate garbage collection error")
	if err != nil {
		return preparedRecordCandidateGarbageCollectionFailureRequest{}, err
	}
	return preparedRecordCandidateGarbageCollectionFailureRequest{
		operationID: request.OperationID, expectedVersion: request.ExpectedVersion, actor: actor,
		reason: reason, errorText: errorText,
	}, nil
}

func sameCandidateGarbageCollectionRequest(operation CandidateGarbageCollectionOperation, request preparedCandidateGarbageCollectionRequest) bool {
	return operation.CandidateID == request.candidateID && operation.ExpectedCandidateVersion == request.expectedCandidateVersion &&
		operation.Actor == request.actor && operation.Reason == request.reason
}

func insertCandidateGarbageCollectionOperationTx(ctx context.Context, tx *sql.Tx, operation CandidateGarbageCollectionOperation) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO candidate_gc_operations_v10 (
			id, candidate_id, idempotency_key, expected_candidate_version, actor, reason,
			state, last_error, created_at, updated_at, completed_at, version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)
	`, operation.ID, operation.CandidateID, operation.IdempotencyKey, operation.ExpectedCandidateVersion,
		operation.Actor, operation.Reason, operation.State, operation.LastError, operation.CreatedAt,
		operation.UpdatedAt, operation.Version)
	if err != nil {
		if isGlobalIdentityCollision(err) {
			return fmt.Errorf("%w: candidate garbage collection operation %s", ErrIdentityCollision, operation.ID)
		}
		if isUniqueConstraint(err) {
			return fmt.Errorf("%w: candidate garbage collection operation %s", ErrCandidateGCInProgress, operation.ID)
		}
	}
	return err
}

func updateCandidateGarbageCollectionOperationTx(ctx context.Context, tx *sql.Tx, operation CandidateGarbageCollectionOperation) error {
	result, err := tx.ExecContext(ctx, `
		UPDATE candidate_gc_operations_v10
		SET state = ?, last_error = ?, updated_at = ?, completed_at = ?, version = ?
		WHERE id = ? AND version = ?
	`, operation.State, operation.LastError, operation.UpdatedAt, operation.CompletedAt, operation.Version,
		operation.ID, operation.Version-1)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("%w: candidate garbage collection operation %s", ErrOptimisticLock, operation.ID)
	}
	return nil
}

func getCandidateGarbageCollectionOperationTx(ctx context.Context, tx *sql.Tx, operationID string) (CandidateGarbageCollectionOperation, error) {
	operation, err := scanCandidateGarbageCollectionOperation(tx.QueryRowContext(ctx, candidateGarbageCollectionOperationV10Select+" WHERE id = ?", operationID))
	if errors.Is(err, sql.ErrNoRows) {
		return CandidateGarbageCollectionOperation{}, fmt.Errorf("%w: candidate garbage collection operation %s", ErrNotFound, operationID)
	}
	return operation, err
}

func getCandidateGarbageCollectionOperationByKeyTx(ctx context.Context, tx *sql.Tx, idempotencyKey string) (*CandidateGarbageCollectionOperation, error) {
	operation, err := scanCandidateGarbageCollectionOperation(tx.QueryRowContext(ctx, candidateGarbageCollectionOperationV10Select+" WHERE idempotency_key = ?", idempotencyKey))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &operation, nil
}

func getInProgressCandidateGarbageCollectionForCandidateTx(ctx context.Context, tx *sql.Tx, candidateID string) (*CandidateGarbageCollectionOperation, error) {
	operation, err := scanCandidateGarbageCollectionOperation(tx.QueryRowContext(ctx, candidateGarbageCollectionOperationV10Select+" WHERE candidate_id = ? AND state = 'in_progress'", candidateID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &operation, nil
}

func scanCandidateGarbageCollectionOperation(scanner rowScanner) (CandidateGarbageCollectionOperation, error) {
	var operation CandidateGarbageCollectionOperation
	var completedAt sql.NullTime
	if err := scanner.Scan(
		&operation.ID, &operation.CandidateID, &operation.IdempotencyKey, &operation.ExpectedCandidateVersion,
		&operation.Actor, &operation.Reason, &operation.State, &operation.LastError, &operation.CreatedAt,
		&operation.UpdatedAt, &completedAt, &operation.Version,
	); err != nil {
		return CandidateGarbageCollectionOperation{}, err
	}
	if !isUUIDv7(operation.ID) || !isUUIDv7(operation.CandidateID) || !isUUIDv7(operation.IdempotencyKey) ||
		!validCandidateGarbageCollectionState(operation.State) || operation.ExpectedCandidateVersion <= 0 || operation.Version <= 0 {
		return CandidateGarbageCollectionOperation{}, fmt.Errorf("invalid persisted candidate garbage collection operation %s", operation.ID)
	}
	operation.CreatedAt = operation.CreatedAt.UTC()
	operation.UpdatedAt = operation.UpdatedAt.UTC()
	operation.CompletedAt = nullableTimePtr(completedAt)
	if operation.State == CandidateGarbageCollectionCompleted && operation.CompletedAt == nil {
		return CandidateGarbageCollectionOperation{}, fmt.Errorf("completed candidate garbage collection operation %s has no completion time", operation.ID)
	}
	return operation, nil
}

func validCandidateGarbageCollectionState(state CandidateGarbageCollectionState) bool {
	return state == CandidateGarbageCollectionInProgress || state == CandidateGarbageCollectionCompleted
}

func (s *Store) appendCandidateGarbageCollectionAuditTx(ctx context.Context, tx *sql.Tx, operation CandidateGarbageCollectionOperation, action string, now time.Time) error {
	_, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: operation.Actor, EntityType: "candidate_gc_operation", EntityID: operation.ID, Action: action,
		Reason: operation.Reason, OperationKey: operation.IdempotencyKey, CreatedAt: now,
		PayloadJSON: auditPayload(map[string]any{
			"candidate_id": operation.CandidateID, "expected_candidate_version": operation.ExpectedCandidateVersion,
			"state": operation.State, "last_error": operation.LastError, "version": operation.Version,
		}),
	})
	return err
}

// ExecuteReleaseWithdraw atomically performs the release CAS, writes a
// durable idempotency operation, and records an immutable receipt. There is
// no old direct withdrawal path: every new withdrawal has this audit chain.
func (s *Store) ExecuteReleaseWithdraw(ctx context.Context, request ExecuteReleaseWithdrawRequest) (ReleaseWithdrawResult, error) {
	prepared, err := prepareExecuteReleaseWithdrawRequest(s, request)
	if err != nil {
		return ReleaseWithdrawResult{}, err
	}
	if _, err := s.BackupBeforeCriticalOperation(ctx, "release_withdraw"); err != nil {
		return ReleaseWithdrawResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReleaseWithdrawResult{}, err
	}
	defer tx.Rollback()

	if existing, err := getReleaseWithdrawOperationByKeyTx(ctx, tx, prepared.idempotencyKey); err != nil {
		return ReleaseWithdrawResult{}, err
	} else if existing != nil {
		if !sameReleaseWithdrawRequest(*existing, prepared) {
			return ReleaseWithdrawResult{}, fmt.Errorf("%w: release withdraw key %s", ErrIdempotencyConflict, prepared.idempotencyKey)
		}
		return s.replayReleaseWithdrawTx(ctx, tx, *existing)
	}

	release, err := getLocalPackageReleaseTx(ctx, tx, prepared.releaseID)
	if err != nil {
		return ReleaseWithdrawResult{}, err
	}
	if release.RecordVersion != prepared.expectedReleaseVersion {
		return ReleaseWithdrawResult{}, fmt.Errorf("%w: release %s", ErrOptimisticLock, release.ID)
	}
	if release.WithdrawnAt != nil {
		return ReleaseWithdrawResult{}, fmt.Errorf("%w: release %s has already been withdrawn", ErrImmutable, release.ID)
	}
	now := s.now().UTC()
	receiptID, err := s.newV2ID("")
	if err != nil {
		return ReleaseWithdrawResult{}, err
	}
	release.WithdrawnAt = &now
	release.WithdrawnBy = prepared.actor
	release.RecordVersion++
	result, err := tx.ExecContext(ctx, `
		UPDATE releases SET withdrawn_at = ?, withdrawn_by = ?, record_version = ?
		WHERE id = ? AND withdrawn_at IS NULL AND record_version = ?
	`, release.WithdrawnAt, release.WithdrawnBy, release.RecordVersion, release.ID, prepared.expectedReleaseVersion)
	if err != nil {
		return ReleaseWithdrawResult{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return ReleaseWithdrawResult{}, err
	}
	if changed != 1 {
		return ReleaseWithdrawResult{}, fmt.Errorf("%w: release %s", ErrOptimisticLock, release.ID)
	}
	operation := ReleaseWithdrawOperation{
		ID:                     prepared.id,
		ReleaseID:              release.ID,
		IdempotencyKey:         prepared.idempotencyKey,
		ExpectedReleaseVersion: prepared.expectedReleaseVersion,
		Actor:                  prepared.actor,
		Reason:                 prepared.reason,
		State:                  ReleaseWithdrawCompleted,
		ReceiptID:              receiptID,
		ResultReleaseVersion:   release.RecordVersion,
		CreatedAt:              now,
		CompletedAt:            now,
	}
	payload, err := json.Marshal(releaseWithdrawReceiptPayload{
		Format:                "harbor.release-withdraw-receipt.v1",
		OperationID:           operation.ID,
		ReleaseID:             release.ID,
		ReleaseVersion:        release.ReleaseVersion,
		ExpectedRecordVersion: operation.ExpectedReleaseVersion,
		ResultRecordVersion:   release.RecordVersion,
		WithdrawnAt:           now,
	})
	if err != nil {
		return ReleaseWithdrawResult{}, err
	}
	receiptJSON, err := normalizeJSON(string(payload), "release withdraw receipt")
	if err != nil {
		return ReleaseWithdrawResult{}, err
	}
	receipt := ReleaseWithdrawReceipt{
		ID:                    receiptID,
		OperationID:           operation.ID,
		ReleaseID:             release.ID,
		ReleaseVersion:        release.ReleaseVersion,
		ExpectedRecordVersion: operation.ExpectedReleaseVersion,
		ResultRecordVersion:   release.RecordVersion,
		ReceiptJSON:           receiptJSON,
		ReceiptDigest:         v4PayloadDigest(receiptJSON),
		CreatedBy:             prepared.actor,
		CreatedAt:             now,
	}
	if err := insertReleaseWithdrawOperationTx(ctx, tx, operation); err != nil {
		return ReleaseWithdrawResult{}, err
	}
	if err := insertReleaseWithdrawReceiptTx(ctx, tx, receipt); err != nil {
		return ReleaseWithdrawResult{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: operation.Actor, EntityType: "release", EntityID: release.ID, Action: "release.withdrawn",
		Reason: operation.Reason, OperationKey: operation.IdempotencyKey, CreatedAt: now,
		PayloadJSON: auditPayload(map[string]any{
			"version": release.ReleaseVersion, "record_version": release.RecordVersion,
			"operation_id": operation.ID, "receipt_id": receipt.ID,
		}),
	}); err != nil {
		return ReleaseWithdrawResult{}, err
	}
	if err := s.appendReleaseWithdrawAuditTx(ctx, tx, operation, receipt, "release_withdraw.completed", now); err != nil {
		return ReleaseWithdrawResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ReleaseWithdrawResult{}, err
	}
	return ReleaseWithdrawResult{Release: release, Operation: operation, Receipt: receipt}, nil
}

// GetReleaseWithdrawOperation exposes the durable operation for read-only UI
// and reconciliation surfaces.
func (s *Store) GetReleaseWithdrawOperation(ctx context.Context, operationID string) (*ReleaseWithdrawOperation, error) {
	if !isUUIDv7(operationID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	operation, err := scanReleaseWithdrawOperation(s.db.QueryRowContext(ctx, releaseWithdrawOperationV10Select+" WHERE id = ?", operationID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &operation, nil
}

func (s *Store) GetReleaseWithdrawReceipt(ctx context.Context, receiptID string) (*ReleaseWithdrawReceipt, error) {
	if !isUUIDv7(receiptID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	receipt, err := scanReleaseWithdrawReceipt(s.db.QueryRowContext(ctx, releaseWithdrawReceiptV10Select+" WHERE id = ?", receiptID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &receipt, nil
}

type preparedExecuteReleaseWithdrawRequest struct {
	id                     string
	releaseID              string
	expectedReleaseVersion int64
	idempotencyKey         string
	actor                  string
	reason                 string
}

func prepareExecuteReleaseWithdrawRequest(s *Store, request ExecuteReleaseWithdrawRequest) (preparedExecuteReleaseWithdrawRequest, error) {
	if !isUUIDv7(request.ReleaseID) || !isUUIDv7(request.IdempotencyKey) {
		return preparedExecuteReleaseWithdrawRequest{}, ErrInvalidUUIDv7Identity
	}
	if request.ExpectedReleaseVersion <= 0 {
		return preparedExecuteReleaseWithdrawRequest{}, fmt.Errorf("expected release version must be positive")
	}
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return preparedExecuteReleaseWithdrawRequest{}, err
	}
	if id == request.ReleaseID {
		return preparedExecuteReleaseWithdrawRequest{}, fmt.Errorf("release withdraw operation identity must differ from release identity")
	}
	actor, err := normalizeRequired(request.Actor, "release withdraw actor")
	if err != nil {
		return preparedExecuteReleaseWithdrawRequest{}, err
	}
	reason, err := normalizeRequired(request.Reason, "release withdraw reason")
	if err != nil {
		return preparedExecuteReleaseWithdrawRequest{}, err
	}
	return preparedExecuteReleaseWithdrawRequest{
		id: id, releaseID: request.ReleaseID, expectedReleaseVersion: request.ExpectedReleaseVersion,
		idempotencyKey: request.IdempotencyKey, actor: actor, reason: reason,
	}, nil
}

func sameReleaseWithdrawRequest(operation ReleaseWithdrawOperation, request preparedExecuteReleaseWithdrawRequest) bool {
	return operation.ReleaseID == request.releaseID && operation.ExpectedReleaseVersion == request.expectedReleaseVersion &&
		operation.Actor == request.actor && operation.Reason == request.reason
}

func (s *Store) replayReleaseWithdrawTx(ctx context.Context, tx *sql.Tx, operation ReleaseWithdrawOperation) (ReleaseWithdrawResult, error) {
	release, err := getLocalPackageReleaseTx(ctx, tx, operation.ReleaseID)
	if err != nil {
		return ReleaseWithdrawResult{}, err
	}
	receipt, err := getReleaseWithdrawReceiptTx(ctx, tx, operation.ReceiptID)
	if err != nil {
		return ReleaseWithdrawResult{}, err
	}
	if operation.State != ReleaseWithdrawCompleted || release.WithdrawnAt == nil || release.RecordVersion != operation.ResultReleaseVersion ||
		receipt.OperationID != operation.ID || receipt.ReleaseID != release.ID || receipt.ResultRecordVersion != release.RecordVersion {
		return ReleaseWithdrawResult{}, fmt.Errorf("invalid persisted release withdraw operation %s", operation.ID)
	}
	if err := tx.Commit(); err != nil {
		return ReleaseWithdrawResult{}, err
	}
	return ReleaseWithdrawResult{Release: release, Operation: operation, Receipt: receipt}, nil
}

func insertReleaseWithdrawOperationTx(ctx context.Context, tx *sql.Tx, operation ReleaseWithdrawOperation) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO release_withdraw_operations_v10 (
			id, release_id, idempotency_key, expected_release_version, actor, reason,
			state, receipt_id, result_release_version, created_at, completed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, operation.ID, operation.ReleaseID, operation.IdempotencyKey, operation.ExpectedReleaseVersion,
		operation.Actor, operation.Reason, operation.State, operation.ReceiptID, operation.ResultReleaseVersion,
		operation.CreatedAt, operation.CompletedAt)
	if err != nil && isUniqueConstraint(err) {
		return fmt.Errorf("%w: release withdraw operation %s", ErrIdentityCollision, operation.ID)
	}
	return err
}

func insertReleaseWithdrawReceiptTx(ctx context.Context, tx *sql.Tx, receipt ReleaseWithdrawReceipt) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO release_withdraw_receipts_v10 (
			id, operation_id, release_id, release_version, expected_record_version,
			result_record_version, receipt_json, receipt_digest, created_by, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, receipt.ID, receipt.OperationID, receipt.ReleaseID, receipt.ReleaseVersion, receipt.ExpectedRecordVersion,
		receipt.ResultRecordVersion, receipt.ReceiptJSON, receipt.ReceiptDigest, receipt.CreatedBy, receipt.CreatedAt)
	if err != nil && isUniqueConstraint(err) {
		return fmt.Errorf("%w: release withdraw receipt %s", ErrIdentityCollision, receipt.ID)
	}
	return err
}

func getReleaseWithdrawOperationByKeyTx(ctx context.Context, tx *sql.Tx, idempotencyKey string) (*ReleaseWithdrawOperation, error) {
	operation, err := scanReleaseWithdrawOperation(tx.QueryRowContext(ctx, releaseWithdrawOperationV10Select+" WHERE idempotency_key = ?", idempotencyKey))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &operation, nil
}

func getReleaseWithdrawReceiptTx(ctx context.Context, tx *sql.Tx, receiptID string) (ReleaseWithdrawReceipt, error) {
	receipt, err := scanReleaseWithdrawReceipt(tx.QueryRowContext(ctx, releaseWithdrawReceiptV10Select+" WHERE id = ?", receiptID))
	if errors.Is(err, sql.ErrNoRows) {
		return ReleaseWithdrawReceipt{}, fmt.Errorf("%w: release withdraw receipt %s", ErrNotFound, receiptID)
	}
	return receipt, err
}

func scanReleaseWithdrawOperation(scanner rowScanner) (ReleaseWithdrawOperation, error) {
	var operation ReleaseWithdrawOperation
	if err := scanner.Scan(
		&operation.ID, &operation.ReleaseID, &operation.IdempotencyKey, &operation.ExpectedReleaseVersion,
		&operation.Actor, &operation.Reason, &operation.State, &operation.ReceiptID, &operation.ResultReleaseVersion,
		&operation.CreatedAt, &operation.CompletedAt,
	); err != nil {
		return ReleaseWithdrawOperation{}, err
	}
	if !isUUIDv7(operation.ID) || !isUUIDv7(operation.ReleaseID) || !isUUIDv7(operation.IdempotencyKey) || !isUUIDv7(operation.ReceiptID) ||
		operation.State != ReleaseWithdrawCompleted || operation.ExpectedReleaseVersion <= 0 || operation.ResultReleaseVersion <= 0 {
		return ReleaseWithdrawOperation{}, fmt.Errorf("invalid persisted release withdraw operation %s", operation.ID)
	}
	operation.CreatedAt = operation.CreatedAt.UTC()
	operation.CompletedAt = operation.CompletedAt.UTC()
	return operation, nil
}

func scanReleaseWithdrawReceipt(scanner rowScanner) (ReleaseWithdrawReceipt, error) {
	var receipt ReleaseWithdrawReceipt
	if err := scanner.Scan(
		&receipt.ID, &receipt.OperationID, &receipt.ReleaseID, &receipt.ReleaseVersion, &receipt.ExpectedRecordVersion,
		&receipt.ResultRecordVersion, &receipt.ReceiptJSON, &receipt.ReceiptDigest, &receipt.CreatedBy, &receipt.CreatedAt,
	); err != nil {
		return ReleaseWithdrawReceipt{}, err
	}
	if !isUUIDv7(receipt.ID) || !isUUIDv7(receipt.OperationID) || !isUUIDv7(receipt.ReleaseID) ||
		receipt.ExpectedRecordVersion <= 0 || receipt.ResultRecordVersion <= 0 || !json.Valid([]byte(receipt.ReceiptJSON)) ||
		receipt.ReceiptDigest != v4PayloadDigest(receipt.ReceiptJSON) {
		return ReleaseWithdrawReceipt{}, fmt.Errorf("invalid persisted release withdraw receipt %s", receipt.ID)
	}
	receipt.CreatedAt = receipt.CreatedAt.UTC()
	return receipt, nil
}

func (s *Store) appendReleaseWithdrawAuditTx(ctx context.Context, tx *sql.Tx, operation ReleaseWithdrawOperation, receipt ReleaseWithdrawReceipt, action string, now time.Time) error {
	_, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: operation.Actor, EntityType: "release_withdraw_operation", EntityID: operation.ID, Action: action,
		Reason: operation.Reason, OperationKey: operation.IdempotencyKey, CreatedAt: now,
		PayloadJSON: auditPayload(map[string]any{
			"release_id": operation.ReleaseID, "expected_release_version": operation.ExpectedReleaseVersion,
			"result_release_version": operation.ResultReleaseVersion, "receipt_id": receipt.ID,
			"receipt_digest": receipt.ReceiptDigest,
		}),
	})
	return err
}

type releaseWithdrawReceiptPayload struct {
	Format                string    `json:"format"`
	OperationID           string    `json:"operation_id"`
	ReleaseID             string    `json:"release_id"`
	ReleaseVersion        string    `json:"release_version"`
	ExpectedRecordVersion int64     `json:"expected_record_version"`
	ResultRecordVersion   int64     `json:"result_record_version"`
	WithdrawnAt           time.Time `json:"withdrawn_at"`
}
