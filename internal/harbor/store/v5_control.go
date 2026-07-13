package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const durableControlOperationV5Select = `
	SELECT id, operation_key, action, run_id, stage_attempt_id, checkpoint_sequence,
	       execution_epoch, subject_version, subject_id, subject_revision_id,
	       subject_digest, workflow_fingerprint, actor, reason, grace_period_ms,
	       status, checkpoint_id, quota_settlement_id, failure_reason, created_at,
	       updated_at, version
	FROM control_operations_v5`

func (s *Store) CreateExecutionControlOperation(ctx context.Context, command ExecutionControlCommand) (DurableControlOperation, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return DurableControlOperation{}, err
	}
	prepared, err := prepareExecutionControlCommand(s, command)
	if err != nil {
		return DurableControlOperation{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DurableControlOperation{}, err
	}
	defer tx.Rollback()
	if existing, err := getDurableControlOperationByKeyTx(ctx, tx, prepared.OperationKey); err != nil {
		return DurableControlOperation{}, err
	} else if existing != nil {
		if !sameExecutionControlCommand(*existing, prepared) {
			return DurableControlOperation{}, fmt.Errorf("%w: execution control key %s", ErrIdempotencyConflict, prepared.OperationKey)
		}
		if err := s.loadControlReceiptsTx(ctx, tx, existing); err != nil {
			return DurableControlOperation{}, err
		}
		if err := tx.Commit(); err != nil {
			return DurableControlOperation{}, err
		}
		return *existing, nil
	}
	run, err := getWorkflowRunTx(ctx, tx, prepared.RunID)
	if err != nil {
		return DurableControlOperation{}, err
	}
	if err := verifyControlCheckpointTx(ctx, tx, run, prepared.Expected); err != nil {
		return DurableControlOperation{}, err
	}
	if prepared.StageAttemptID != "" {
		stage, err := getStageAttemptTx(ctx, tx, prepared.StageAttemptID)
		if err != nil {
			return DurableControlOperation{}, err
		}
		if stage.RunID != run.ID {
			return DurableControlOperation{}, fmt.Errorf("%w: stage attempt belongs to another workflow run", ErrInvalidControl)
		}
		if isTerminalStageExecutionStatus(stage.ExecutionStatus) {
			return DurableControlOperation{}, fmt.Errorf("%w: stage attempt %s is terminal", ErrInvalidControl, stage.ID)
		}
	}
	now := s.now().UTC()
	if err := s.requestControlTargetTransitionTx(ctx, tx, &run, prepared, now); err != nil {
		return DurableControlOperation{}, err
	}
	operation := DurableControlOperation{
		ID: prepared.ID, OperationKey: prepared.OperationKey, Action: prepared.Action, RunID: prepared.RunID,
		StageAttemptID: prepared.StageAttemptID, Expected: prepared.Expected, Actor: prepared.Actor,
		Reason: prepared.Reason, GracePeriod: prepared.GracePeriod, Status: ControlOperationRequested,
		CreatedAt: now, UpdatedAt: now, Version: 1,
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO control_operations_v5 (
			id, operation_key, action, run_id, stage_attempt_id, checkpoint_sequence,
			execution_epoch, subject_version, subject_id, subject_revision_id, subject_digest,
			workflow_fingerprint, actor, reason, grace_period_ms, status, checkpoint_id,
			quota_settlement_id, failure_reason, created_at, updated_at, version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', '', '', ?, ?, ?)
	`, operation.ID, operation.OperationKey, operation.Action, operation.RunID, nullableString(operation.StageAttemptID),
		int64(operation.Expected.Sequence), operation.Expected.ExecutionEpoch, operation.Expected.SubjectVersion,
		operation.Expected.SubjectID, operation.Expected.SubjectRevisionID, operation.Expected.SubjectDigest,
		operation.Expected.WorkflowFingerprint, operation.Actor, operation.Reason, durationMilliseconds(operation.GracePeriod),
		operation.Status, operation.CreatedAt, operation.UpdatedAt, operation.Version)
	if err != nil {
		if isUniqueConstraint(err) {
			return DurableControlOperation{}, fmt.Errorf("%w: execution control %s", ErrIdentityCollision, operation.ID)
		}
		return DurableControlOperation{}, err
	}
	if err := s.appendControlOutboxTx(ctx, tx, operation, "control.requested", now); err != nil {
		return DurableControlOperation{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:        operation.Actor,
		EntityType:   "control_operation",
		EntityID:     operation.ID,
		Action:       "control_operation.requested",
		Reason:       operation.Reason,
		PayloadJSON:  auditPayload(map[string]any{"action": operation.Action, "run_id": operation.RunID, "stage_attempt_id": operation.StageAttemptID, "operation_key": operation.OperationKey}),
		OperationKey: operation.OperationKey,
		CreatedAt:    now,
	}); err != nil {
		return DurableControlOperation{}, err
	}
	if err := tx.Commit(); err != nil {
		return DurableControlOperation{}, err
	}
	return operation, nil
}

func (s *Store) GetExecutionControlOperation(ctx context.Context, operationID string) (*DurableControlOperation, error) {
	if !isUUIDv7(operationID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	operation, err := scanDurableControlOperation(s.db.QueryRowContext(ctx, durableControlOperationV5Select+" WHERE id = ?", operationID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := s.loadControlReceipts(ctx, &operation); err != nil {
		return nil, err
	}
	return &operation, nil
}

// ListExecutionControlOperationsForRun returns the durable control history for
// one Run, newest first. It is a read-only projection for application adapters
// such as the Task Hub; callers cannot use it to infer or alter control state.
func (s *Store) ListExecutionControlOperationsForRun(ctx context.Context, runID string) ([]DurableControlOperation, error) {
	if !isUUIDv7(runID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	rows, err := s.db.QueryContext(ctx, durableControlOperationV5Select+" WHERE run_id = ? ORDER BY updated_at DESC, id DESC", runID)
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
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	// The store has one SQL connection. Close the result set before loading
	// each operation's immutable receipt facts through a second query.
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for index := range operations {
		if err := s.loadControlReceipts(ctx, &operations[index]); err != nil {
			return nil, err
		}
	}
	return operations, nil
}

func (s *Store) TransitionExecutionControlOperation(ctx context.Context, request TransitionControlOperationRequest) (DurableControlOperation, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return DurableControlOperation{}, err
	}
	prepared, err := prepareControlTransition(s, request)
	if err != nil {
		return DurableControlOperation{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DurableControlOperation{}, err
	}
	defer tx.Rollback()
	if existing, err := getControlTransitionTx(ctx, tx, prepared.ID); err != nil {
		return DurableControlOperation{}, err
	} else if existing != nil {
		if !sameControlTransitionRequest(*existing, prepared) {
			return DurableControlOperation{}, fmt.Errorf("%w: control transition %s", ErrIdempotencyConflict, prepared.ID)
		}
		operation, err := getDurableControlOperationTx(ctx, tx, existing.OperationID)
		if err != nil {
			return DurableControlOperation{}, err
		}
		if err := s.loadControlReceiptsTx(ctx, tx, &operation); err != nil {
			return DurableControlOperation{}, err
		}
		if err := tx.Commit(); err != nil {
			return DurableControlOperation{}, err
		}
		return operation, nil
	}
	operation, err := getDurableControlOperationTx(ctx, tx, prepared.OperationID)
	if err != nil {
		return DurableControlOperation{}, err
	}
	if operation.Version != prepared.ExpectedVersion {
		return DurableControlOperation{}, fmt.Errorf("%w: control operation %s", ErrOptimisticLock, operation.ID)
	}
	if !validControlOperationTransition(operation.Status, prepared.Status) {
		return DurableControlOperation{}, fmt.Errorf("%w: control operation %s from %s to %s", ErrInvalidControl, operation.ID, operation.Status, prepared.Status)
	}
	if err := s.mergeControlFactsTx(ctx, tx, &operation, prepared); err != nil {
		return DurableControlOperation{}, err
	}
	now := s.now().UTC()
	operation.Status = prepared.Status
	operation.UpdatedAt = now
	operation.Version++
	result, err := tx.ExecContext(ctx, `
		UPDATE control_operations_v5
		SET status = ?, checkpoint_id = ?, quota_settlement_id = ?, failure_reason = ?, updated_at = ?, version = ?
		WHERE id = ? AND version = ?
	`, operation.Status, operation.CheckpointID, operation.QuotaSettlementID, operation.FailureReason,
		operation.UpdatedAt, operation.Version, operation.ID, prepared.ExpectedVersion)
	if err != nil {
		return DurableControlOperation{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return DurableControlOperation{}, err
	}
	if changed != 1 {
		return DurableControlOperation{}, fmt.Errorf("%w: control operation %s", ErrOptimisticLock, operation.ID)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO control_operation_transitions_v5 (
			id, operation_id, expected_version, status, checkpoint_id, quota_settlement_id,
			failure_reason, actor, reason, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, prepared.ID, operation.ID, prepared.ExpectedVersion, prepared.Status, prepared.CheckpointID,
		prepared.QuotaSettlementID, prepared.FailureReason, prepared.Actor, prepared.Reason, now); err != nil {
		if isUniqueConstraint(err) {
			return DurableControlOperation{}, fmt.Errorf("%w: control transition %s", ErrIdentityCollision, prepared.ID)
		}
		return DurableControlOperation{}, err
	}
	if err := s.appendControlOutboxTx(ctx, tx, operation, "control."+string(operation.Status), now); err != nil {
		return DurableControlOperation{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:        prepared.Actor,
		EntityType:   "control_operation",
		EntityID:     operation.ID,
		Action:       "control_operation.transitioned",
		Reason:       prepared.Reason,
		PayloadJSON:  auditPayload(map[string]any{"status": operation.Status, "version": operation.Version, "transition_id": prepared.ID}),
		OperationKey: operation.OperationKey,
		CreatedAt:    now,
	}); err != nil {
		return DurableControlOperation{}, err
	}
	if err := tx.Commit(); err != nil {
		return DurableControlOperation{}, err
	}
	return operation, nil
}

type preparedExecutionControlCommand struct {
	ID             string
	OperationKey   string
	Action         ControlAction
	RunID          string
	StageAttemptID string
	Expected       ControlCheckpointRef
	Actor          string
	Reason         string
	GracePeriod    time.Duration
}

func prepareExecutionControlCommand(s *Store, command ExecutionControlCommand) (preparedExecutionControlCommand, error) {
	id, err := s.newV2ID(command.ID)
	if err != nil {
		return preparedExecutionControlCommand{}, err
	}
	key, err := normalizeRequired(command.OperationKey, "control operation key")
	if err != nil {
		return preparedExecutionControlCommand{}, fmt.Errorf("%w: %v", ErrInvalidControl, err)
	}
	if !validControlAction(command.Action) {
		return preparedExecutionControlCommand{}, fmt.Errorf("%w: unsupported action %q", ErrInvalidControl, command.Action)
	}
	if !isUUIDv7(command.RunID) {
		return preparedExecutionControlCommand{}, ErrInvalidUUIDv7Identity
	}
	stageAttemptID := strings.TrimSpace(command.StageAttemptID)
	if command.Action == ControlActionCancelStage {
		if !isUUIDv7(stageAttemptID) {
			return preparedExecutionControlCommand{}, ErrInvalidUUIDv7Identity
		}
	} else if stageAttemptID != "" {
		return preparedExecutionControlCommand{}, fmt.Errorf("%w: action %s cannot target a stage", ErrInvalidControl, command.Action)
	}
	if err := validateControlCheckpoint(command.Expected); err != nil {
		return preparedExecutionControlCommand{}, err
	}
	actor, err := normalizeRequired(command.Actor, "control actor")
	if err != nil {
		return preparedExecutionControlCommand{}, fmt.Errorf("%w: %v", ErrInvalidControl, err)
	}
	reason, err := normalizeRequired(command.Reason, "control reason")
	if err != nil {
		return preparedExecutionControlCommand{}, fmt.Errorf("%w: %v", ErrInvalidControl, err)
	}
	if command.GracePeriod < 0 {
		return preparedExecutionControlCommand{}, fmt.Errorf("%w: control grace period cannot be negative", ErrInvalidControl)
	}
	return preparedExecutionControlCommand{ID: id, OperationKey: key, Action: command.Action, RunID: command.RunID,
		StageAttemptID: stageAttemptID, Expected: command.Expected, Actor: actor, Reason: reason, GracePeriod: command.GracePeriod}, nil
}

func validateControlCheckpoint(checkpoint ControlCheckpointRef) error {
	if checkpoint.ExecutionEpoch < 0 || checkpoint.SubjectVersion < 0 {
		return fmt.Errorf("%w: checkpoint versions cannot be negative", ErrInvalidControl)
	}
	if !isUUIDv7(checkpoint.SubjectID) || !isUUIDv7(checkpoint.SubjectRevisionID) {
		return ErrInvalidUUIDv7Identity
	}
	if strings.TrimSpace(checkpoint.SubjectDigest) == "" || strings.TrimSpace(checkpoint.WorkflowFingerprint) == "" {
		return fmt.Errorf("%w: checkpoint digest and workflow fingerprint are required", ErrInvalidControl)
	}
	return nil
}

func verifyControlCheckpointTx(ctx context.Context, tx *sql.Tx, run WorkflowRun, checkpoint ControlCheckpointRef) error {
	task, err := getTaskV2Tx(ctx, tx, run.TaskID)
	if err != nil {
		return err
	}
	revision, err := getTaskRevisionTx(ctx, tx, run.RevisionID)
	if err != nil {
		return err
	}
	if checkpoint.Sequence != uint64(run.Version) || checkpoint.SubjectID != run.TaskID || checkpoint.SubjectRevisionID != run.RevisionID ||
		checkpoint.SubjectVersion != task.Version || checkpoint.SubjectDigest != revision.TaskDigest ||
		checkpoint.WorkflowFingerprint != run.DefinitionHash || checkpoint.ExecutionEpoch != run.ExecutionEpoch {
		return fmt.Errorf("%w: control checkpoint is stale", ErrOptimisticLock)
	}
	return nil
}

// requestControlTargetTransitionTx makes the durable command and the run's
// requested state one transaction. StageAttempt has no request-state in the
// persisted V2 vocabulary, so a stage cancellation is represented by the
// target-scoped ControlOperation until the runtime acknowledges cancellation.
func (s *Store) requestControlTargetTransitionTx(ctx context.Context, tx *sql.Tx, run *WorkflowRun, command preparedExecutionControlCommand, now time.Time) error {
	var target WorkflowRunStatus
	switch command.Action {
	case ControlActionPause:
		if run.Status != WorkflowRunRunning {
			return fmt.Errorf("%w: only a running workflow run can be paused", ErrInvalidControl)
		}
		target = WorkflowRunPauseRequested
	case ControlActionTerminate:
		switch run.Status {
		case WorkflowRunQueued:
			target = WorkflowRunCancelRequested
		case WorkflowRunRunning:
			target = WorkflowRunStopRequested
		case WorkflowRunPauseRequested, WorkflowRunPausing, WorkflowRunPaused, WorkflowRunResumeRequested,
			WorkflowRunWaitingReview, WorkflowRunWaitingContinuation:
			target = WorkflowRunCancelRequested
		default:
			return fmt.Errorf("%w: workflow run %s cannot be terminated from %s", ErrInvalidControl, run.ID, run.Status)
		}
	case ControlActionCancelStage:
		return nil
	default:
		return fmt.Errorf("%w: unsupported action %q", ErrInvalidControl, command.Action)
	}
	if !validWorkflowRunTransition(run.Status, target) {
		return fmt.Errorf("%w: workflow run %s from %s to %s", ErrInvalidTransition, run.ID, run.Status, target)
	}
	previousVersion := run.Version
	run.Status = target
	run.Version++
	result, err := tx.ExecContext(ctx, `
		UPDATE workflow_runs SET status = ?, version = ? WHERE id = ? AND version = ?
	`, run.Status, run.Version, run.ID, previousVersion)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("%w: workflow run %s", ErrOptimisticLock, run.ID)
	}
	_, err = s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:        command.Actor,
		EntityType:   "workflow_run",
		EntityID:     run.ID,
		Action:       "workflow_run.control_requested",
		Reason:       command.Reason,
		PayloadJSON:  auditPayload(map[string]any{"action": command.Action, "status": run.Status, "version": run.Version}),
		OperationKey: command.OperationKey,
		CreatedAt:    now,
	})
	return err
}

func validControlAction(action ControlAction) bool {
	return action == ControlActionPause || action == ControlActionCancelStage || action == ControlActionTerminate
}

func validControlOperationStatus(status ControlOperationStatus) bool {
	switch status {
	case ControlOperationRequested, ControlOperationPropagating, ControlOperationAcknowledged, ControlOperationReconcileRequired, ControlOperationFailed:
		return true
	default:
		return false
	}
}

func validControlOperationTransition(from, to ControlOperationStatus) bool {
	if from == to {
		return false
	}
	switch from {
	case ControlOperationRequested:
		return to == ControlOperationPropagating || to == ControlOperationAcknowledged || to == ControlOperationReconcileRequired || to == ControlOperationFailed
	case ControlOperationPropagating:
		return to == ControlOperationAcknowledged || to == ControlOperationReconcileRequired || to == ControlOperationFailed
	default:
		return false
	}
}

func sameExecutionControlCommand(operation DurableControlOperation, command preparedExecutionControlCommand) bool {
	return operation.OperationKey == command.OperationKey && operation.Action == command.Action && operation.RunID == command.RunID &&
		operation.StageAttemptID == command.StageAttemptID && operation.Expected == command.Expected && operation.Actor == command.Actor &&
		operation.Reason == command.Reason && operation.GracePeriod == command.GracePeriod
}

type preparedControlTransition struct {
	ID                string
	OperationID       string
	ExpectedVersion   int64
	Status            ControlOperationStatus
	RuntimeReceipts   []RuntimeTerminationReceipt
	CheckpointID      string
	QuotaSettlementID string
	FailureReason     string
	Actor             string
	Reason            string
}

func prepareControlTransition(s *Store, request TransitionControlOperationRequest) (preparedControlTransition, error) {
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return preparedControlTransition{}, err
	}
	if !isUUIDv7(request.OperationID) {
		return preparedControlTransition{}, ErrInvalidUUIDv7Identity
	}
	if request.ExpectedVersion <= 0 || !validControlOperationStatus(request.Status) || request.Status == ControlOperationRequested {
		return preparedControlTransition{}, fmt.Errorf("%w: invalid control transition", ErrInvalidControl)
	}
	failureReason := strings.TrimSpace(request.FailureReason)
	if request.Status == ControlOperationFailed && failureReason == "" {
		return preparedControlTransition{}, fmt.Errorf("%w: failed control transition needs a failure reason", ErrInvalidControl)
	}
	if request.Status != ControlOperationFailed && failureReason != "" {
		return preparedControlTransition{}, fmt.Errorf("%w: only failed transitions may carry a failure reason", ErrInvalidControl)
	}
	actor, err := normalizeRequired(request.Actor, "control transition actor")
	if err != nil {
		return preparedControlTransition{}, fmt.Errorf("%w: %v", ErrInvalidControl, err)
	}
	receipts, err := normalizeRuntimeReceipts(s, request.RuntimeReceipts)
	if err != nil {
		return preparedControlTransition{}, err
	}
	return preparedControlTransition{
		ID: id, OperationID: request.OperationID, ExpectedVersion: request.ExpectedVersion, Status: request.Status,
		RuntimeReceipts: receipts, CheckpointID: strings.TrimSpace(request.CheckpointID),
		QuotaSettlementID: strings.TrimSpace(request.QuotaSettlementID), FailureReason: failureReason,
		Actor: actor, Reason: strings.TrimSpace(request.Reason),
	}, nil
}

func normalizeRuntimeReceipts(s *Store, receipts []RuntimeTerminationReceipt) ([]RuntimeTerminationReceipt, error) {
	normalized := make([]RuntimeTerminationReceipt, 0, len(receipts))
	seen := make(map[string]struct{}, len(receipts))
	for _, receipt := range receipts {
		id, err := s.newV2ID(receipt.ID)
		if err != nil {
			return nil, err
		}
		scopeID, err := normalizeRequired(receipt.RuntimeScopeID, "runtime receipt scope id")
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidControl, err)
		}
		if receipt.ObservedAt.IsZero() {
			return nil, fmt.Errorf("%w: runtime receipt observation time is required", ErrInvalidControl)
		}
		payload, err := normalizeJSON(receipt.PayloadJSON, "runtime receipt payload")
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidControl, err)
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, fmt.Errorf("%w: duplicate runtime receipt %s", ErrInvalidControl, id)
		}
		seen[id] = struct{}{}
		normalized = append(normalized, RuntimeTerminationReceipt{
			ID: id, RuntimeScopeID: scopeID, ObservedAt: receipt.ObservedAt.UTC(), Graceful: receipt.Graceful,
			ExternalOutcomeUnknown: receipt.ExternalOutcomeUnknown, PayloadJSON: payload,
		})
	}
	return normalized, nil
}

type durableControlTransitionRow struct {
	ID                string
	OperationID       string
	ExpectedVersion   int64
	Status            ControlOperationStatus
	CheckpointID      string
	QuotaSettlementID string
	FailureReason     string
	Actor             string
	Reason            string
}

func getControlTransitionTx(ctx context.Context, tx *sql.Tx, transitionID string) (*durableControlTransitionRow, error) {
	var row durableControlTransitionRow
	err := tx.QueryRowContext(ctx, `
		SELECT id, operation_id, expected_version, status, checkpoint_id, quota_settlement_id,
		       failure_reason, actor, reason
		FROM control_operation_transitions_v5 WHERE id = ?
	`, transitionID).Scan(&row.ID, &row.OperationID, &row.ExpectedVersion, &row.Status, &row.CheckpointID,
		&row.QuotaSettlementID, &row.FailureReason, &row.Actor, &row.Reason)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func sameControlTransitionRequest(existing durableControlTransitionRow, request preparedControlTransition) bool {
	return existing.OperationID == request.OperationID && existing.ExpectedVersion == request.ExpectedVersion &&
		existing.Status == request.Status && existing.CheckpointID == request.CheckpointID &&
		existing.QuotaSettlementID == request.QuotaSettlementID && existing.FailureReason == request.FailureReason &&
		existing.Actor == request.Actor && existing.Reason == request.Reason
}

func (s *Store) mergeControlFactsTx(ctx context.Context, tx *sql.Tx, operation *DurableControlOperation, transition preparedControlTransition) error {
	if err := mergeControlFact(&operation.CheckpointID, transition.CheckpointID, "checkpoint id"); err != nil {
		return err
	}
	if err := mergeControlFact(&operation.QuotaSettlementID, transition.QuotaSettlementID, "quota settlement id"); err != nil {
		return err
	}
	if transition.Status == ControlOperationFailed {
		operation.FailureReason = transition.FailureReason
	}
	existing, err := listRuntimeReceiptsTx(ctx, tx, operation.ID)
	if err != nil {
		return err
	}
	byID := make(map[string]RuntimeTerminationReceipt, len(existing))
	for _, receipt := range existing {
		byID[receipt.ID] = receipt
	}
	for _, receipt := range transition.RuntimeReceipts {
		if previous, found := byID[receipt.ID]; found {
			if previous != receipt {
				return fmt.Errorf("%w: runtime receipt %s conflicts with prior fact", ErrInvalidControl, receipt.ID)
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO runtime_termination_receipts_v5 (
				id, control_operation_id, runtime_scope_id, observed_at, graceful,
				external_outcome_unknown, payload_json, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		`, receipt.ID, operation.ID, receipt.RuntimeScopeID, receipt.ObservedAt, receipt.Graceful,
			receipt.ExternalOutcomeUnknown, receipt.PayloadJSON, s.now().UTC()); err != nil {
			if isUniqueConstraint(err) {
				return fmt.Errorf("%w: runtime receipt %s", ErrIdentityCollision, receipt.ID)
			}
			return err
		}
		operation.RuntimeReceipts = append(operation.RuntimeReceipts, receipt)
		byID[receipt.ID] = receipt
	}
	return nil
}

func mergeControlFact(current *string, incoming, label string) error {
	if incoming == "" {
		return nil
	}
	if *current != "" && *current != incoming {
		return fmt.Errorf("%w: control %s cannot change", ErrInvalidControl, label)
	}
	*current = incoming
	return nil
}

func getDurableControlOperationTx(ctx context.Context, tx *sql.Tx, operationID string) (DurableControlOperation, error) {
	operation, err := scanDurableControlOperation(tx.QueryRowContext(ctx, durableControlOperationV5Select+" WHERE id = ?", operationID))
	if err == sql.ErrNoRows {
		return DurableControlOperation{}, fmt.Errorf("%w: control operation %s", ErrNotFound, operationID)
	}
	return operation, err
}

func getDurableControlOperationByKeyTx(ctx context.Context, tx *sql.Tx, key string) (*DurableControlOperation, error) {
	operation, err := scanDurableControlOperation(tx.QueryRowContext(ctx, durableControlOperationV5Select+" WHERE operation_key = ?", key))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &operation, nil
}

func (s *Store) loadControlReceipts(ctx context.Context, operation *DurableControlOperation) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, runtime_scope_id, observed_at, graceful, external_outcome_unknown, payload_json
		FROM runtime_termination_receipts_v5 WHERE control_operation_id = ? ORDER BY observed_at, id
	`, operation.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		receipt, err := scanRuntimeTerminationReceipt(rows)
		if err != nil {
			return err
		}
		operation.RuntimeReceipts = append(operation.RuntimeReceipts, receipt)
	}
	return rows.Err()
}

func (s *Store) loadControlReceiptsTx(ctx context.Context, tx *sql.Tx, operation *DurableControlOperation) error {
	receipts, err := listRuntimeReceiptsTx(ctx, tx, operation.ID)
	if err != nil {
		return err
	}
	operation.RuntimeReceipts = receipts
	return nil
}

func listRuntimeReceiptsTx(ctx context.Context, tx *sql.Tx, operationID string) ([]RuntimeTerminationReceipt, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, runtime_scope_id, observed_at, graceful, external_outcome_unknown, payload_json
		FROM runtime_termination_receipts_v5 WHERE control_operation_id = ? ORDER BY observed_at, id
	`, operationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var receipts []RuntimeTerminationReceipt
	for rows.Next() {
		receipt, err := scanRuntimeTerminationReceipt(rows)
		if err != nil {
			return nil, err
		}
		receipts = append(receipts, receipt)
	}
	return receipts, rows.Err()
}

func scanDurableControlOperation(scanner rowScanner) (DurableControlOperation, error) {
	var operation DurableControlOperation
	var stageAttemptID sql.NullString
	var sequence, graceMilliseconds int64
	if err := scanner.Scan(&operation.ID, &operation.OperationKey, &operation.Action, &operation.RunID, &stageAttemptID,
		&sequence, &operation.Expected.ExecutionEpoch, &operation.Expected.SubjectVersion, &operation.Expected.SubjectID,
		&operation.Expected.SubjectRevisionID, &operation.Expected.SubjectDigest, &operation.Expected.WorkflowFingerprint,
		&operation.Actor, &operation.Reason, &graceMilliseconds, &operation.Status, &operation.CheckpointID,
		&operation.QuotaSettlementID, &operation.FailureReason, &operation.CreatedAt, &operation.UpdatedAt, &operation.Version); err != nil {
		return DurableControlOperation{}, err
	}
	if sequence < 0 || graceMilliseconds < 0 || !validControlAction(operation.Action) || !validControlOperationStatus(operation.Status) {
		return DurableControlOperation{}, fmt.Errorf("%w: invalid persisted control operation %s", ErrInvalidControl, operation.ID)
	}
	operation.StageAttemptID = nullableStringValue(stageAttemptID)
	operation.Expected.Sequence = uint64(sequence)
	operation.GracePeriod = time.Duration(graceMilliseconds) * time.Millisecond
	operation.CreatedAt = operation.CreatedAt.UTC()
	operation.UpdatedAt = operation.UpdatedAt.UTC()
	return operation, nil
}

func scanRuntimeTerminationReceipt(scanner rowScanner) (RuntimeTerminationReceipt, error) {
	var receipt RuntimeTerminationReceipt
	if err := scanner.Scan(&receipt.ID, &receipt.RuntimeScopeID, &receipt.ObservedAt, &receipt.Graceful,
		&receipt.ExternalOutcomeUnknown, &receipt.PayloadJSON); err != nil {
		return RuntimeTerminationReceipt{}, err
	}
	receipt.ObservedAt = receipt.ObservedAt.UTC()
	return receipt, nil
}

func (s *Store) appendControlOutboxTx(ctx context.Context, tx *sql.Tx, operation DurableControlOperation, topic string, now time.Time) error {
	id, err := s.newV2ID("")
	if err != nil {
		return err
	}
	payload := auditPayload(map[string]any{
		"operation_id": operation.ID, "operation_key": operation.OperationKey, "action": operation.Action,
		"run_id": operation.RunID, "stage_attempt_id": operation.StageAttemptID, "status": operation.Status,
	})
	key := operation.OperationKey + ":" + topic
	_, err = tx.ExecContext(ctx, `
		INSERT INTO outbox_events (
			id, topic, entity_type, entity_id, payload_json, idempotency_key, state,
			created_at, published_at, version, available_at, lease_owner, lease_expires_at,
			lease_fencing_token, delivery_count, last_error, updated_at
		) VALUES (?, ?, 'control_operation', ?, ?, ?, 'pending', ?, NULL, 1, ?, '', NULL, 0, 0, '', ?)
	`, id, topic, operation.ID, payload, key, now, now, now)
	if err != nil {
		if isGlobalIdentityCollision(err) {
			return fmt.Errorf("%w: outbox event %s", ErrIdentityCollision, id)
		}
		if isUniqueConstraint(err) {
			return fmt.Errorf("%w: control outbox %s", ErrIdempotencyConflict, key)
		}
		return err
	}
	return nil
}
