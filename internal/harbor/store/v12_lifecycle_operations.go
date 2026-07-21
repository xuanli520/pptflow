package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const lifecycleOperationV12Select = `
	SELECT id, idempotency_key, action, request_fingerprint,
	       task_id, revision_id, run_id, review_request_id, release_id, deletion_record_id, target_lifecycle_state,
	       expected_task_id, expected_revision_id, expected_run_id, expected_release_id, expected_review_request_id,
	       expected_task_version, expected_revision_state_version, expected_revision_digest,
	       expected_run_version, expected_run_execution_epoch, expected_run_definition_hash,
	       expected_release_record_version, expected_review_revision_id, expected_review_state,
	       expected_review_evidence_digest, expected_codeedge_compliance_record_id,
	       expected_codeedge_authorization_fingerprint, actor, reason, state, result_json, created_at,
	       updated_at, completed_at, version
	FROM lifecycle_operations_v12`

// BeginLifecycleOperation creates a durable receipt boundary before an
// application service changes a lifecycle entity. A replay uses only the
// caller-issued key and canonical request fingerprint; server-allocated target
// IDs from a later retry are ignored in favor of the persisted allocation.
func (s *Store) BeginLifecycleOperation(ctx context.Context, request BeginLifecycleOperationRequest) (BeginLifecycleOperationResult, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return BeginLifecycleOperationResult{}, err
	}
	prepared, err := prepareBeginLifecycleOperationRequest(s, request)
	if err != nil {
		return BeginLifecycleOperationResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BeginLifecycleOperationResult{}, err
	}
	defer tx.Rollback()
	if existing, err := getLifecycleOperationByKeyTx(ctx, tx, prepared.idempotencyKey); err != nil {
		return BeginLifecycleOperationResult{}, err
	} else if existing != nil {
		if existing.Action != prepared.action || existing.RequestFingerprint != prepared.requestFingerprint {
			return BeginLifecycleOperationResult{}, fmt.Errorf("%w: lifecycle operation key %s", ErrIdempotencyConflict, prepared.idempotencyKey)
		}
		if err := tx.Commit(); err != nil {
			return BeginLifecycleOperationResult{}, err
		}
		return BeginLifecycleOperationResult{Operation: *existing, Replayed: true}, nil
	}
	now := s.now().UTC()
	operation := LifecycleOperation{
		ID: prepared.id, IdempotencyKey: prepared.idempotencyKey, Action: prepared.action,
		RequestFingerprint: prepared.requestFingerprint, TaskID: prepared.taskID, RevisionID: prepared.revisionID,
		RunID: prepared.runID, ReviewRequestID: prepared.reviewRequestID, ReleaseID: prepared.releaseID,
		DeletionRecordID: prepared.deletionRecordID, TargetLifecycleState: prepared.targetLifecycleState,
		ExpectedTaskID: prepared.expectedTaskID, ExpectedRevisionID: prepared.expectedRevisionID, ExpectedRunID: prepared.expectedRunID,
		ExpectedReleaseID: prepared.expectedReleaseID, ExpectedReviewRequestID: prepared.expectedReviewRequestID, ExpectedTaskVersion: prepared.expectedTaskVersion,
		ExpectedRevisionStateVersion: prepared.expectedRevisionStateVersion, ExpectedRevisionDigest: prepared.expectedRevisionDigest,
		ExpectedRunVersion: prepared.expectedRunVersion, ExpectedRunExecutionEpoch: prepared.expectedRunExecutionEpoch,
		ExpectedRunDefinitionHash: prepared.expectedRunDefinitionHash, ExpectedReleaseRecordVersion: prepared.expectedReleaseRecordVersion,
		ExpectedReviewRevisionID: prepared.expectedReviewRevisionID, ExpectedReviewState: prepared.expectedReviewState,
		ExpectedReviewEvidenceDigest:             prepared.expectedReviewEvidenceDigest,
		ExpectedCodeEdgeComplianceRecordID:       prepared.expectedCodeEdgeComplianceRecordID,
		ExpectedCodeEdgeAuthorizationFingerprint: prepared.expectedCodeEdgeAuthorizationFingerprint,
		Actor:                                    prepared.actor, Reason: prepared.reason,
		State: LifecycleOperationPrepared, CreatedAt: now, UpdatedAt: now, Version: 1,
	}
	if err := insertLifecycleOperationTx(ctx, tx, operation); err != nil {
		return BeginLifecycleOperationResult{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: operation.Actor, EntityType: "lifecycle_operation", EntityID: operation.ID,
		Action: "lifecycle_operation.prepared", Reason: operation.Reason, OperationKey: operation.IdempotencyKey,
		PayloadJSON: auditPayload(map[string]any{"action": operation.Action, "task_id": operation.TaskID, "revision_id": operation.RevisionID, "run_id": operation.RunID, "release_id": operation.ReleaseID}),
		CreatedAt:   now,
	}); err != nil {
		return BeginLifecycleOperationResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return BeginLifecycleOperationResult{}, err
	}
	return BeginLifecycleOperationResult{Operation: operation}, nil
}

// CompleteLifecycleOperation stores the typed response only after its domain
// mutation has succeeded. A second completion accepts only byte-identical
// canonical JSON, preventing a retry from rewriting an immutable receipt.
func (s *Store) CompleteLifecycleOperation(ctx context.Context, request CompleteLifecycleOperationRequest) (LifecycleOperation, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return LifecycleOperation{}, err
	}
	if !isUUIDv7(request.OperationID) {
		return LifecycleOperation{}, ErrInvalidUUIDv7Identity
	}
	if request.ExpectedVersion <= 0 {
		return LifecycleOperation{}, fmt.Errorf("expected lifecycle operation version must be positive")
	}
	resultJSON, err := normalizeLifecycleOperationJSON(request.ResultJSON)
	if err != nil {
		return LifecycleOperation{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return LifecycleOperation{}, err
	}
	defer tx.Rollback()
	operation, err := getLifecycleOperationTx(ctx, tx, request.OperationID)
	if err != nil {
		return LifecycleOperation{}, err
	}
	if operation.State == LifecycleOperationCompleted {
		if operation.ResultJSON != resultJSON {
			return LifecycleOperation{}, fmt.Errorf("%w: lifecycle operation %s result", ErrIdempotencyConflict, operation.ID)
		}
		if err := tx.Commit(); err != nil {
			return LifecycleOperation{}, err
		}
		return operation, nil
	}
	if operation.State != LifecycleOperationPrepared || operation.Version != request.ExpectedVersion {
		return LifecycleOperation{}, fmt.Errorf("%w: lifecycle operation %s", ErrOptimisticLock, operation.ID)
	}
	now := s.now().UTC()
	operation.State = LifecycleOperationCompleted
	operation.ResultJSON = resultJSON
	operation.UpdatedAt = now
	operation.CompletedAt = &now
	operation.Version++
	result, err := tx.ExecContext(ctx, `
		UPDATE lifecycle_operations_v12
		SET state = ?, result_json = ?, updated_at = ?, completed_at = ?, version = ?
		WHERE id = ? AND state = 'prepared' AND version = ?
	`, operation.State, operation.ResultJSON, operation.UpdatedAt, operation.CompletedAt, operation.Version, operation.ID, request.ExpectedVersion)
	if err != nil {
		return LifecycleOperation{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return LifecycleOperation{}, err
	}
	if changed != 1 {
		return LifecycleOperation{}, fmt.Errorf("%w: lifecycle operation %s", ErrOptimisticLock, operation.ID)
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: operation.Actor, EntityType: "lifecycle_operation", EntityID: operation.ID,
		Action: "lifecycle_operation.completed", Reason: operation.Reason, OperationKey: operation.IdempotencyKey,
		PayloadJSON: auditPayload(map[string]any{"action": operation.Action, "result_json": operation.ResultJSON}),
		CreatedAt:   now,
	}); err != nil {
		return LifecycleOperation{}, err
	}
	if err := tx.Commit(); err != nil {
		return LifecycleOperation{}, err
	}
	return operation, nil
}

// ExecuteLifecycleTaskTransition atomically consumes a prepared lifecycle
// operation into its reviewed task transition and, for soft deletion, its
// completed DeletionRecord. A process cannot observe the task mutation without
// the durable V12 receipt that makes a lost caller response replayable.
func (s *Store) ExecuteLifecycleTaskTransition(ctx context.Context, request ExecuteLifecycleTaskTransitionRequest) (ExecuteLifecycleTaskTransitionResult, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return ExecuteLifecycleTaskTransitionResult{}, err
	}
	if !isUUIDv7(request.OperationID) {
		return ExecuteLifecycleTaskTransitionResult{}, ErrInvalidUUIDv7Identity
	}
	if request.ExpectedVersion <= 0 {
		return ExecuteLifecycleTaskTransitionResult{}, fmt.Errorf("expected lifecycle operation version must be positive")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ExecuteLifecycleTaskTransitionResult{}, err
	}
	defer tx.Rollback()
	operation, err := getLifecycleOperationTx(ctx, tx, request.OperationID)
	if err != nil {
		return ExecuteLifecycleTaskTransitionResult{}, err
	}
	targetState, needsDeletionRecord, err := lifecycleTaskTransitionTarget(operation)
	if err != nil {
		return ExecuteLifecycleTaskTransitionResult{}, err
	}
	if operation.State == LifecycleOperationCompleted {
		receipt, decodeErr := decodeLifecycleTaskTransitionReceipt(operation.ResultJSON, operation, targetState, needsDeletionRecord)
		if decodeErr != nil {
			return ExecuteLifecycleTaskTransitionResult{}, decodeErr
		}
		if err := tx.Commit(); err != nil {
			return ExecuteLifecycleTaskTransitionResult{}, err
		}
		return ExecuteLifecycleTaskTransitionResult{Operation: operation, Task: receipt.Task, DeletionRecord: receipt.DeletionRecord}, nil
	}
	if operation.State != LifecycleOperationPrepared || operation.Version != request.ExpectedVersion {
		return ExecuteLifecycleTaskTransitionResult{}, fmt.Errorf("%w: lifecycle operation %s", ErrOptimisticLock, operation.ID)
	}
	if operation.ExpectedTaskVersion <= 0 || !isUUIDv7(operation.TaskID) {
		return ExecuteLifecycleTaskTransitionResult{}, fmt.Errorf("invalid lifecycle task transition checkpoint")
	}
	task, err := getTaskV2Tx(ctx, tx, operation.TaskID)
	if err != nil {
		return ExecuteLifecycleTaskTransitionResult{}, err
	}
	now := s.now().UTC()
	if err := s.guardTaskPurgeMutationTx(ctx, tx, task.ID, operation.Actor, now); err != nil {
		return ExecuteLifecycleTaskTransitionResult{}, err
	}
	if task.Version != operation.ExpectedTaskVersion {
		return ExecuteLifecycleTaskTransitionResult{}, fmt.Errorf("%w: task %s", ErrOptimisticLock, task.ID)
	}
	if !validTaskLifecycleTransition(task.LifecycleState, targetState) {
		return ExecuteLifecycleTaskTransitionResult{}, fmt.Errorf("%w: task %s from %s to %s", ErrInvalidTransition, task.ID, task.LifecycleState, targetState)
	}
	task.LifecycleState = targetState
	if targetState == TaskLifecycleDeleted {
		task.DeletedAt = &now
	} else {
		task.DeletedAt = nil
	}
	task.UpdatedAt = now
	task.Version++
	updated, err := tx.ExecContext(ctx, `
		UPDATE tasks_v2
		SET lifecycle_state = ?, updated_at = ?, deleted_at = ?, version = ?
		WHERE id = ? AND version = ?
	`, task.LifecycleState, task.UpdatedAt, task.DeletedAt, task.Version, task.ID, operation.ExpectedTaskVersion)
	if err != nil {
		return ExecuteLifecycleTaskTransitionResult{}, err
	}
	changed, err := updated.RowsAffected()
	if err != nil {
		return ExecuteLifecycleTaskTransitionResult{}, err
	}
	if changed != 1 {
		return ExecuteLifecycleTaskTransitionResult{}, fmt.Errorf("%w: task %s", ErrOptimisticLock, task.ID)
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: operation.Actor, EntityType: "task", EntityID: task.ID, Action: "task.lifecycle_transitioned",
		Reason: operation.Reason, OperationKey: operation.IdempotencyKey,
		PayloadJSON: auditPayload(map[string]any{"operation_id": operation.ID, "lifecycle_state": task.LifecycleState, "version": task.Version}),
		CreatedAt:   now,
	}); err != nil {
		return ExecuteLifecycleTaskTransitionResult{}, err
	}

	var deletionRecord *DeletionRecord
	if needsDeletionRecord {
		record, recordErr := s.createCompletedLifecycleDeletionRecordTx(ctx, tx, operation, now)
		if recordErr != nil {
			return ExecuteLifecycleTaskTransitionResult{}, recordErr
		}
		deletionRecord = &record
	}

	receiptJSON, err := normalizeLifecycleOperationJSON(mustLifecycleTaskTransitionReceiptJSON(task, deletionRecord))
	if err != nil {
		return ExecuteLifecycleTaskTransitionResult{}, err
	}
	operation.State = LifecycleOperationCompleted
	operation.ResultJSON = receiptJSON
	operation.UpdatedAt = now
	operation.CompletedAt = &now
	operation.Version++
	completed, err := tx.ExecContext(ctx, `
		UPDATE lifecycle_operations_v12
		SET state = ?, result_json = ?, updated_at = ?, completed_at = ?, version = ?
		WHERE id = ? AND state = 'prepared' AND version = ?
	`, operation.State, operation.ResultJSON, operation.UpdatedAt, operation.CompletedAt, operation.Version, operation.ID, request.ExpectedVersion)
	if err != nil {
		return ExecuteLifecycleTaskTransitionResult{}, err
	}
	completedRows, err := completed.RowsAffected()
	if err != nil {
		return ExecuteLifecycleTaskTransitionResult{}, err
	}
	if completedRows != 1 {
		return ExecuteLifecycleTaskTransitionResult{}, fmt.Errorf("%w: lifecycle operation %s", ErrOptimisticLock, operation.ID)
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: operation.Actor, EntityType: "lifecycle_operation", EntityID: operation.ID,
		Action: "lifecycle_operation.completed", Reason: operation.Reason, OperationKey: operation.IdempotencyKey,
		PayloadJSON: auditPayload(map[string]any{"action": operation.Action, "task_id": task.ID, "task_version": task.Version, "deletion_record_id": operation.DeletionRecordID}),
		CreatedAt:   now,
	}); err != nil {
		return ExecuteLifecycleTaskTransitionResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ExecuteLifecycleTaskTransitionResult{}, err
	}
	return ExecuteLifecycleTaskTransitionResult{Operation: operation, Task: task, DeletionRecord: deletionRecord}, nil
}

type lifecycleTaskTransitionReceipt struct {
	Task           TaskV2          `json:"task"`
	DeletionRecord *DeletionRecord `json:"deletion_record,omitempty"`
}

func lifecycleTaskTransitionTarget(operation LifecycleOperation) (TaskLifecycleState, bool, error) {
	target := operation.TargetLifecycleState
	switch operation.Action {
	case "task.archive":
		if target != TaskLifecycleArchived || operation.DeletionRecordID != "" {
			return "", false, fmt.Errorf("invalid archive lifecycle operation %s", operation.ID)
		}
		return target, false, nil
	case "task.soft_delete":
		if target != TaskLifecycleDeleted || !isUUIDv7(operation.DeletionRecordID) {
			return "", false, fmt.Errorf("invalid soft-delete lifecycle operation %s", operation.ID)
		}
		return target, true, nil
	case "task.restore":
		if target == "" || target == TaskLifecycleDeleted || !validTaskLifecycleState(target) || operation.DeletionRecordID != "" {
			return "", false, fmt.Errorf("invalid restore lifecycle operation %s", operation.ID)
		}
		return target, false, nil
	default:
		return "", false, fmt.Errorf("lifecycle operation %s action %q is not a task transition", operation.ID, operation.Action)
	}
}

func (s *Store) createCompletedLifecycleDeletionRecordTx(ctx context.Context, tx *sql.Tx, operation LifecycleOperation, now time.Time) (DeletionRecord, error) {
	record := DeletionRecord{
		ID: operation.DeletionRecordID, EntityType: "task", EntityID: operation.TaskID, Action: "soft_delete",
		State: DeletionCompleted, Actor: operation.Actor, Reason: operation.Reason, CreatedAt: now, CompletedAt: &now, Version: 2,
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO deletion_records (id, entity_type, entity_id, action, state, actor, reason, created_at, completed_at, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, record.ID, record.EntityType, record.EntityID, record.Action, record.State, record.Actor, record.Reason, record.CreatedAt, record.CompletedAt, record.Version)
	if err != nil {
		if isUniqueConstraint(err) {
			return DeletionRecord{}, fmt.Errorf("%w: deletion record %s", ErrIdentityCollision, record.ID)
		}
		return DeletionRecord{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: record.Actor, EntityType: "deletion_record", EntityID: record.ID, Action: "deletion_record.created",
		Reason: record.Reason, OperationKey: operation.IdempotencyKey,
		PayloadJSON: auditPayload(map[string]any{"entity_type": record.EntityType, "entity_id": record.EntityID, "action": record.Action}),
		CreatedAt:   now,
	}); err != nil {
		return DeletionRecord{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: record.Actor, EntityType: "deletion_record", EntityID: record.ID, Action: "deletion_record.transitioned",
		Reason: record.Reason, OperationKey: operation.IdempotencyKey,
		PayloadJSON: auditPayload(map[string]any{"state": record.State, "version": record.Version}),
		CreatedAt:   now,
	}); err != nil {
		return DeletionRecord{}, err
	}
	return record, nil
}

func mustLifecycleTaskTransitionReceiptJSON(task TaskV2, deletionRecord *DeletionRecord) string {
	encoded, err := json.Marshal(lifecycleTaskTransitionReceipt{Task: task, DeletionRecord: deletionRecord})
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func decodeLifecycleTaskTransitionReceipt(raw string, operation LifecycleOperation, target TaskLifecycleState, needsDeletionRecord bool) (lifecycleTaskTransitionReceipt, error) {
	var receipt lifecycleTaskTransitionReceipt
	if err := json.Unmarshal([]byte(raw), &receipt); err != nil {
		return lifecycleTaskTransitionReceipt{}, fmt.Errorf("decode lifecycle task transition receipt %s: %w", operation.ID, err)
	}
	if receipt.Task.ID != operation.TaskID || receipt.Task.LifecycleState != target || receipt.Task.Version != operation.ExpectedTaskVersion+1 {
		return lifecycleTaskTransitionReceipt{}, fmt.Errorf("invalid lifecycle task transition receipt %s", operation.ID)
	}
	if needsDeletionRecord {
		if receipt.DeletionRecord == nil || receipt.DeletionRecord.ID != operation.DeletionRecordID || receipt.DeletionRecord.EntityID != operation.TaskID || receipt.DeletionRecord.State != DeletionCompleted {
			return lifecycleTaskTransitionReceipt{}, fmt.Errorf("invalid lifecycle soft-delete receipt %s", operation.ID)
		}
	} else if receipt.DeletionRecord != nil {
		return lifecycleTaskTransitionReceipt{}, fmt.Errorf("invalid lifecycle task transition deletion receipt %s", operation.ID)
	}
	return receipt, nil
}

func (s *Store) GetLifecycleOperation(ctx context.Context, operationID string) (*LifecycleOperation, error) {
	if !isUUIDv7(operationID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	operation, err := scanLifecycleOperation(s.db.QueryRowContext(ctx, lifecycleOperationV12Select+" WHERE id = ?", operationID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &operation, nil
}

func (s *Store) GetLifecycleOperationByIdempotencyKey(ctx context.Context, idempotencyKey string) (*LifecycleOperation, error) {
	if !isUUIDv7(idempotencyKey) {
		return nil, ErrInvalidUUIDv7Identity
	}
	operation, err := scanLifecycleOperation(s.db.QueryRowContext(ctx, lifecycleOperationV12Select+" WHERE idempotency_key = ?", idempotencyKey))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &operation, nil
}

// ListPreparedLifecycleOperationsByAction returns the outstanding durable
// receipt boundaries for one action. Callers use this only for recovery
// projections; a prepared operation remains the authority for retrying its
// original idempotency key.
func (s *Store) ListPreparedLifecycleOperationsByAction(ctx context.Context, action string) ([]LifecycleOperation, error) {
	action, err := normalizeRequired(action, "lifecycle operation action")
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, lifecycleOperationV12Select+" WHERE action = ? AND state = ? ORDER BY updated_at DESC, id ASC", action, LifecycleOperationPrepared)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	operations := make([]LifecycleOperation, 0)
	for rows.Next() {
		operation, err := scanLifecycleOperation(rows)
		if err != nil {
			return nil, err
		}
		operations = append(operations, operation)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return operations, nil
}

type preparedBeginLifecycleOperationRequest struct {
	id                                       string
	idempotencyKey                           string
	action                                   string
	requestFingerprint                       string
	taskID                                   string
	revisionID                               string
	runID                                    string
	reviewRequestID                          string
	releaseID                                string
	deletionRecordID                         string
	targetLifecycleState                     TaskLifecycleState
	expectedTaskID                           string
	expectedRevisionID                       string
	expectedRunID                            string
	expectedReleaseID                        string
	expectedReviewRequestID                  string
	expectedTaskVersion                      int64
	expectedRevisionStateVersion             int64
	expectedRevisionDigest                   string
	expectedRunVersion                       int64
	expectedRunExecutionEpoch                int
	expectedRunDefinitionHash                string
	expectedReleaseRecordVersion             int64
	expectedReviewRevisionID                 string
	expectedReviewState                      string
	expectedReviewEvidenceDigest             string
	expectedCodeEdgeComplianceRecordID       string
	expectedCodeEdgeAuthorizationFingerprint string
	actor                                    string
	reason                                   string
}

func prepareBeginLifecycleOperationRequest(s *Store, request BeginLifecycleOperationRequest) (preparedBeginLifecycleOperationRequest, error) {
	if !isUUIDv7(request.IdempotencyKey) {
		return preparedBeginLifecycleOperationRequest{}, ErrInvalidUUIDv7Identity
	}
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return preparedBeginLifecycleOperationRequest{}, err
	}
	action, err := normalizeRequired(request.Action, "lifecycle operation action")
	if err != nil {
		return preparedBeginLifecycleOperationRequest{}, err
	}
	fingerprint, err := normalizeRequired(request.RequestFingerprint, "lifecycle operation request fingerprint")
	if err != nil {
		return preparedBeginLifecycleOperationRequest{}, err
	}
	actor, err := normalizeRequired(request.Actor, "lifecycle operation actor")
	if err != nil {
		return preparedBeginLifecycleOperationRequest{}, err
	}
	reason, err := normalizeRequired(request.Reason, "lifecycle operation reason")
	if err != nil {
		return preparedBeginLifecycleOperationRequest{}, err
	}
	for _, identity := range []struct {
		name  string
		value string
	}{
		{"task", request.TaskID}, {"revision", request.RevisionID}, {"run", request.RunID},
		{"review request", request.ReviewRequestID}, {"release", request.ReleaseID}, {"deletion record", request.DeletionRecordID},
		{"expected task", request.ExpectedTaskID}, {"expected revision", request.ExpectedRevisionID},
		{"expected run", request.ExpectedRunID}, {"expected release", request.ExpectedReleaseID},
		{"expected review request", request.ExpectedReviewRequestID}, {"expected review revision", request.ExpectedReviewRevisionID},
		{"expected CodeEdge compliance record", request.ExpectedCodeEdgeComplianceRecordID},
	} {
		if strings.TrimSpace(identity.value) != "" && !isUUIDv7(identity.value) {
			return preparedBeginLifecycleOperationRequest{}, fmt.Errorf("%s identity: %w", identity.name, ErrInvalidUUIDv7Identity)
		}
	}
	if request.ExpectedTaskVersion < 0 || request.ExpectedRevisionStateVersion < 0 || request.ExpectedRunVersion < 0 ||
		request.ExpectedRunExecutionEpoch < 0 || request.ExpectedReleaseRecordVersion < 0 {
		return preparedBeginLifecycleOperationRequest{}, fmt.Errorf("lifecycle operation checkpoint versions cannot be negative")
	}
	if request.TargetLifecycleState != "" && !validTaskLifecycleState(request.TargetLifecycleState) {
		return preparedBeginLifecycleOperationRequest{}, fmt.Errorf("invalid lifecycle operation target state %q", request.TargetLifecycleState)
	}
	return preparedBeginLifecycleOperationRequest{
		id: id, idempotencyKey: request.IdempotencyKey, action: action, requestFingerprint: fingerprint,
		taskID: strings.TrimSpace(request.TaskID), revisionID: strings.TrimSpace(request.RevisionID), runID: strings.TrimSpace(request.RunID),
		reviewRequestID: strings.TrimSpace(request.ReviewRequestID), releaseID: strings.TrimSpace(request.ReleaseID), deletionRecordID: strings.TrimSpace(request.DeletionRecordID),
		targetLifecycleState: request.TargetLifecycleState,
		expectedTaskID:       strings.TrimSpace(request.ExpectedTaskID), expectedRevisionID: strings.TrimSpace(request.ExpectedRevisionID),
		expectedRunID: strings.TrimSpace(request.ExpectedRunID), expectedReleaseID: strings.TrimSpace(request.ExpectedReleaseID),
		expectedReviewRequestID: strings.TrimSpace(request.ExpectedReviewRequestID),
		expectedTaskVersion:     request.ExpectedTaskVersion, expectedRevisionStateVersion: request.ExpectedRevisionStateVersion,
		expectedRevisionDigest: strings.TrimSpace(request.ExpectedRevisionDigest), expectedRunVersion: request.ExpectedRunVersion,
		expectedRunExecutionEpoch: request.ExpectedRunExecutionEpoch, expectedRunDefinitionHash: strings.TrimSpace(request.ExpectedRunDefinitionHash),
		expectedReleaseRecordVersion: request.ExpectedReleaseRecordVersion, expectedReviewRevisionID: strings.TrimSpace(request.ExpectedReviewRevisionID),
		expectedReviewState: strings.TrimSpace(request.ExpectedReviewState), expectedReviewEvidenceDigest: strings.TrimSpace(request.ExpectedReviewEvidenceDigest),
		expectedCodeEdgeComplianceRecordID:       strings.TrimSpace(request.ExpectedCodeEdgeComplianceRecordID),
		expectedCodeEdgeAuthorizationFingerprint: strings.TrimSpace(request.ExpectedCodeEdgeAuthorizationFingerprint),
		actor:                                    actor, reason: reason,
	}, nil
}

func insertLifecycleOperationTx(ctx context.Context, tx *sql.Tx, operation LifecycleOperation) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO lifecycle_operations_v12 (
			id, idempotency_key, action, request_fingerprint, task_id, revision_id, run_id, review_request_id,
			release_id, deletion_record_id, target_lifecycle_state, expected_task_id, expected_revision_id, expected_run_id,
			expected_release_id, expected_review_request_id, expected_task_version, expected_revision_state_version,
			expected_revision_digest, expected_run_version, expected_run_execution_epoch, expected_run_definition_hash,
			expected_release_record_version, expected_review_revision_id, expected_review_state, expected_review_evidence_digest,
			expected_codeedge_compliance_record_id, expected_codeedge_authorization_fingerprint,
			actor, reason, state, result_json, created_at, updated_at, completed_at, version
		) VALUES (
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			?, ?,
			?, ?, ?,
			'', ?, ?, NULL, ?
		)
	`, operation.ID, operation.IdempotencyKey, operation.Action, operation.RequestFingerprint, operation.TaskID, operation.RevisionID,
		operation.RunID, operation.ReviewRequestID, operation.ReleaseID, operation.DeletionRecordID, operation.TargetLifecycleState,
		operation.ExpectedTaskID, operation.ExpectedRevisionID, operation.ExpectedRunID, operation.ExpectedReleaseID, operation.ExpectedReviewRequestID, operation.ExpectedTaskVersion,
		operation.ExpectedRevisionStateVersion, operation.ExpectedRevisionDigest, operation.ExpectedRunVersion, operation.ExpectedRunExecutionEpoch,
		operation.ExpectedRunDefinitionHash, operation.ExpectedReleaseRecordVersion, operation.ExpectedReviewRevisionID,
		operation.ExpectedReviewState, operation.ExpectedReviewEvidenceDigest,
		operation.ExpectedCodeEdgeComplianceRecordID, operation.ExpectedCodeEdgeAuthorizationFingerprint,
		operation.Actor, operation.Reason, operation.State,
		operation.CreatedAt, operation.UpdatedAt, operation.Version)
	if err != nil && isUniqueConstraint(err) {
		return fmt.Errorf("%w: lifecycle operation %s", ErrIdentityCollision, operation.ID)
	}
	return err
}

func getLifecycleOperationTx(ctx context.Context, tx *sql.Tx, operationID string) (LifecycleOperation, error) {
	operation, err := scanLifecycleOperation(tx.QueryRowContext(ctx, lifecycleOperationV12Select+" WHERE id = ?", operationID))
	if errors.Is(err, sql.ErrNoRows) {
		return LifecycleOperation{}, fmt.Errorf("%w: lifecycle operation %s", ErrNotFound, operationID)
	}
	return operation, err
}

func getLifecycleOperationByKeyTx(ctx context.Context, tx *sql.Tx, idempotencyKey string) (*LifecycleOperation, error) {
	operation, err := scanLifecycleOperation(tx.QueryRowContext(ctx, lifecycleOperationV12Select+" WHERE idempotency_key = ?", idempotencyKey))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &operation, nil
}

func scanLifecycleOperation(scanner rowScanner) (LifecycleOperation, error) {
	var operation LifecycleOperation
	var completedAt sql.NullTime
	if err := scanner.Scan(
		&operation.ID, &operation.IdempotencyKey, &operation.Action, &operation.RequestFingerprint,
		&operation.TaskID, &operation.RevisionID, &operation.RunID, &operation.ReviewRequestID, &operation.ReleaseID, &operation.DeletionRecordID, &operation.TargetLifecycleState,
		&operation.ExpectedTaskID, &operation.ExpectedRevisionID, &operation.ExpectedRunID, &operation.ExpectedReleaseID, &operation.ExpectedReviewRequestID,
		&operation.ExpectedTaskVersion, &operation.ExpectedRevisionStateVersion, &operation.ExpectedRevisionDigest,
		&operation.ExpectedRunVersion, &operation.ExpectedRunExecutionEpoch, &operation.ExpectedRunDefinitionHash,
		&operation.ExpectedReleaseRecordVersion, &operation.ExpectedReviewRevisionID, &operation.ExpectedReviewState,
		&operation.ExpectedReviewEvidenceDigest, &operation.ExpectedCodeEdgeComplianceRecordID,
		&operation.ExpectedCodeEdgeAuthorizationFingerprint, &operation.Actor, &operation.Reason, &operation.State, &operation.ResultJSON,
		&operation.CreatedAt, &operation.UpdatedAt, &completedAt, &operation.Version,
	); err != nil {
		return LifecycleOperation{}, err
	}
	if !isUUIDv7(operation.ID) || !isUUIDv7(operation.IdempotencyKey) || strings.TrimSpace(operation.Action) == "" ||
		strings.TrimSpace(operation.RequestFingerprint) == "" || strings.TrimSpace(operation.Actor) == "" || strings.TrimSpace(operation.Reason) == "" ||
		(operation.State != LifecycleOperationPrepared && operation.State != LifecycleOperationCompleted) || operation.Version <= 0 {
		return LifecycleOperation{}, fmt.Errorf("invalid persisted lifecycle operation %s", operation.ID)
	}
	for _, identity := range []string{operation.TaskID, operation.RevisionID, operation.RunID, operation.ReviewRequestID, operation.ReleaseID, operation.DeletionRecordID,
		operation.ExpectedTaskID, operation.ExpectedRevisionID, operation.ExpectedRunID, operation.ExpectedReleaseID, operation.ExpectedReviewRequestID, operation.ExpectedReviewRevisionID,
		operation.ExpectedCodeEdgeComplianceRecordID} {
		if identity != "" && !isUUIDv7(identity) {
			return LifecycleOperation{}, fmt.Errorf("invalid persisted lifecycle operation %s target identity", operation.ID)
		}
	}
	if operation.ExpectedTaskVersion < 0 || operation.ExpectedRevisionStateVersion < 0 || operation.ExpectedRunVersion < 0 ||
		operation.ExpectedRunExecutionEpoch < 0 || operation.ExpectedReleaseRecordVersion < 0 {
		return LifecycleOperation{}, fmt.Errorf("invalid persisted lifecycle operation %s checkpoint", operation.ID)
	}
	if operation.TargetLifecycleState != "" && !validTaskLifecycleState(operation.TargetLifecycleState) {
		return LifecycleOperation{}, fmt.Errorf("invalid persisted lifecycle operation %s target state", operation.ID)
	}
	operation.CreatedAt = operation.CreatedAt.UTC()
	operation.UpdatedAt = operation.UpdatedAt.UTC()
	if completedAt.Valid {
		completed := completedAt.Time.UTC()
		operation.CompletedAt = &completed
	}
	if operation.State == LifecycleOperationCompleted {
		if operation.CompletedAt == nil || !jsonValid(operation.ResultJSON) {
			return LifecycleOperation{}, fmt.Errorf("invalid completed lifecycle operation %s", operation.ID)
		}
	} else if operation.CompletedAt != nil || operation.ResultJSON != "" {
		return LifecycleOperation{}, fmt.Errorf("invalid prepared lifecycle operation %s", operation.ID)
	}
	return operation, nil
}

func jsonValid(value string) bool {
	// normalizeJSON is intentionally not used here: corrupt persisted state must
	// not be silently rewritten while being read.
	return strings.TrimSpace(value) != "" && json.Valid([]byte(value))
}

func normalizeLifecycleOperationJSON(value string) (string, error) {
	decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(value)))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return "", fmt.Errorf("lifecycle operation result must contain valid JSON: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return "", fmt.Errorf("lifecycle operation result must contain one JSON value")
		}
		return "", fmt.Errorf("decode lifecycle operation result trailing data: %w", err)
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		return "", fmt.Errorf("canonicalize lifecycle operation result: %w", err)
	}
	return string(encoded), nil
}
