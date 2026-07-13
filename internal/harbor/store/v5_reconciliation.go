package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const sideEffectOperationV5Select = `
	SELECT id, operation_key, run_id, stage_attempt_id, effect_kind, idempotency_key,
	       source_digest, destination_digest, receipt_ref, payload_json, state,
	       created_at, updated_at, version
	FROM side_effect_operations_v5`

const reconciliationAttemptV5Select = `
	SELECT id, operation_key, subject_type, subject_id, side_effect_operation_id,
	       control_operation_id, ordinal, state, observed_json, resolution,
	       created_at, finished_at, version
	FROM reconciliation_attempts_v5`

func (s *Store) CreateSideEffectOperation(ctx context.Context, request CreateSideEffectOperationRequest) (SideEffectOperation, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return SideEffectOperation{}, err
	}
	prepared, err := prepareSideEffectOperation(s, request)
	if err != nil {
		return SideEffectOperation{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SideEffectOperation{}, err
	}
	defer tx.Rollback()
	if existing, err := getSideEffectByOperationKeyTx(ctx, tx, prepared.OperationKey); err != nil {
		return SideEffectOperation{}, err
	} else if existing != nil {
		if !sameSideEffectCreateRequest(*existing, prepared) {
			return SideEffectOperation{}, fmt.Errorf("%w: side effect operation key %s", ErrIdempotencyConflict, prepared.OperationKey)
		}
		if err := tx.Commit(); err != nil {
			return SideEffectOperation{}, err
		}
		return *existing, nil
	}
	if existing, err := getSideEffectByIdempotencyKeyTx(ctx, tx, prepared.IdempotencyKey); err != nil {
		return SideEffectOperation{}, err
	} else if existing != nil {
		if !sameSideEffectCreateRequest(*existing, prepared) {
			return SideEffectOperation{}, fmt.Errorf("%w: side effect idempotency key %s", ErrIdempotencyConflict, prepared.IdempotencyKey)
		}
		if err := tx.Commit(); err != nil {
			return SideEffectOperation{}, err
		}
		return *existing, nil
	}
	if err := verifySideEffectScopeTx(ctx, tx, prepared.RunID, prepared.StageAttemptID); err != nil {
		return SideEffectOperation{}, err
	}
	now := s.now().UTC()
	operation := SideEffectOperation{
		ID: prepared.ID, OperationKey: prepared.OperationKey, RunID: prepared.RunID, StageAttemptID: prepared.StageAttemptID,
		EffectKind: prepared.EffectKind, IdempotencyKey: prepared.IdempotencyKey, SourceDigest: prepared.SourceDigest,
		PayloadJSON: prepared.PayloadJSON, State: SideEffectPrepared, CreatedAt: now, UpdatedAt: now, Version: 1,
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO side_effect_operations_v5 (
			id, operation_key, run_id, stage_attempt_id, effect_kind, idempotency_key,
			source_digest, destination_digest, receipt_ref, payload_json, state, created_at, updated_at, version
		) VALUES (?, ?, ?, ?, ?, ?, ?, '', '', ?, ?, ?, ?, ?)
	`, operation.ID, operation.OperationKey, operation.RunID, operation.StageAttemptID, operation.EffectKind,
		operation.IdempotencyKey, operation.SourceDigest, operation.PayloadJSON, operation.State, operation.CreatedAt,
		operation.UpdatedAt, operation.Version)
	if err != nil {
		if isUniqueConstraint(err) {
			return SideEffectOperation{}, fmt.Errorf("%w: side effect operation %s", ErrIdentityCollision, operation.ID)
		}
		return SideEffectOperation{}, err
	}
	if err := s.appendV5OutboxTx(ctx, tx, "side_effect.prepared", "side_effect_operation", operation.ID,
		operation.OperationKey+":prepared", auditPayload(map[string]any{"operation_key": operation.OperationKey, "effect_kind": operation.EffectKind, "source_digest": operation.SourceDigest}), now); err != nil {
		return SideEffectOperation{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: request.Actor, EntityType: "side_effect_operation", EntityID: operation.ID, Action: "side_effect_operation.prepared",
		Reason: request.Reason, PayloadJSON: auditPayload(map[string]any{"operation_key": operation.OperationKey, "effect_kind": operation.EffectKind, "run_id": operation.RunID}),
		OperationKey: operation.OperationKey, CreatedAt: now,
	}); err != nil {
		return SideEffectOperation{}, err
	}
	if err := tx.Commit(); err != nil {
		return SideEffectOperation{}, err
	}
	return operation, nil
}

func (s *Store) GetSideEffectOperation(ctx context.Context, operationID string) (*SideEffectOperation, error) {
	if !isUUIDv7(operationID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	operation, err := scanSideEffectOperation(s.db.QueryRowContext(ctx, sideEffectOperationV5Select+" WHERE id = ?", operationID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &operation, nil
}

func (s *Store) TransitionSideEffectOperation(ctx context.Context, request TransitionSideEffectOperationRequest) (SideEffectOperation, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return SideEffectOperation{}, err
	}
	prepared, err := prepareSideEffectTransition(request)
	if err != nil {
		return SideEffectOperation{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SideEffectOperation{}, err
	}
	defer tx.Rollback()
	operation, err := getSideEffectOperationTx(ctx, tx, prepared.OperationID)
	if err != nil {
		return SideEffectOperation{}, err
	}
	if operation.Version != prepared.ExpectedVersion {
		return SideEffectOperation{}, fmt.Errorf("%w: side effect operation %s", ErrOptimisticLock, operation.ID)
	}
	if !validSideEffectTransition(operation.State, prepared.State) {
		return SideEffectOperation{}, fmt.Errorf("%w: side effect operation %s from %s to %s", ErrInvalidTransition, operation.ID, operation.State, prepared.State)
	}
	if err := mergeSideEffectFacts(&operation, prepared); err != nil {
		return SideEffectOperation{}, err
	}
	now := s.now().UTC()
	operation.State = prepared.State
	operation.UpdatedAt = now
	operation.Version++
	result, err := tx.ExecContext(ctx, `
		UPDATE side_effect_operations_v5
		SET destination_digest = ?, receipt_ref = ?, payload_json = ?, state = ?, updated_at = ?, version = ?
		WHERE id = ? AND version = ?
	`, operation.DestinationDigest, operation.ReceiptRef, operation.PayloadJSON, operation.State,
		operation.UpdatedAt, operation.Version, operation.ID, prepared.ExpectedVersion)
	if err != nil {
		return SideEffectOperation{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return SideEffectOperation{}, err
	}
	if changed != 1 {
		return SideEffectOperation{}, fmt.Errorf("%w: side effect operation %s", ErrOptimisticLock, operation.ID)
	}
	if err := s.appendV5OutboxTx(ctx, tx, "side_effect."+string(operation.State), "side_effect_operation", operation.ID,
		operation.OperationKey+":"+string(operation.State), auditPayload(map[string]any{"state": operation.State, "receipt_ref": operation.ReceiptRef, "destination_digest": operation.DestinationDigest}), now); err != nil {
		return SideEffectOperation{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: prepared.Actor, EntityType: "side_effect_operation", EntityID: operation.ID, Action: "side_effect_operation.transitioned",
		Reason: prepared.Reason, PayloadJSON: auditPayload(map[string]any{"state": operation.State, "version": operation.Version}),
		OperationKey: operation.OperationKey, CreatedAt: now,
	}); err != nil {
		return SideEffectOperation{}, err
	}
	if err := tx.Commit(); err != nil {
		return SideEffectOperation{}, err
	}
	return operation, nil
}

func (s *Store) StartReconciliationAttempt(ctx context.Context, request StartReconciliationAttemptRequest) (ReconciliationAttempt, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return ReconciliationAttempt{}, err
	}
	prepared, err := prepareReconciliationStart(s, request)
	if err != nil {
		return ReconciliationAttempt{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReconciliationAttempt{}, err
	}
	defer tx.Rollback()
	if existing, err := getReconciliationAttemptByKeyTx(ctx, tx, prepared.OperationKey); err != nil {
		return ReconciliationAttempt{}, err
	} else if existing != nil {
		if !sameReconciliationStart(*existing, prepared) {
			return ReconciliationAttempt{}, fmt.Errorf("%w: reconciliation operation key %s", ErrIdempotencyConflict, prepared.OperationKey)
		}
		if err := tx.Commit(); err != nil {
			return ReconciliationAttempt{}, err
		}
		return *existing, nil
	}
	if err := verifyReconciliationSubjectTx(ctx, tx, prepared); err != nil {
		return ReconciliationAttempt{}, err
	}
	now := s.now().UTC()
	attempt := ReconciliationAttempt{
		ID: prepared.ID, OperationKey: prepared.OperationKey, SubjectType: prepared.SubjectType, SubjectID: prepared.SubjectID,
		SideEffectOperationID: prepared.SideEffectOperationID, ControlOperationID: prepared.ControlOperationID,
		Ordinal: prepared.Ordinal, State: ReconciliationRunning, ObservedJSON: prepared.ObservedJSON,
		CreatedAt: now, Version: 1,
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO reconciliation_attempts_v5 (
			id, operation_key, subject_type, subject_id, side_effect_operation_id,
			control_operation_id, ordinal, state, observed_json, resolution, created_at, finished_at, version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, NULL, ?)
	`, attempt.ID, attempt.OperationKey, attempt.SubjectType, attempt.SubjectID, nullableString(attempt.SideEffectOperationID),
		nullableString(attempt.ControlOperationID), attempt.Ordinal, attempt.State, attempt.ObservedJSON, attempt.CreatedAt, attempt.Version)
	if err != nil {
		if isUniqueConstraint(err) {
			return ReconciliationAttempt{}, fmt.Errorf("%w: reconciliation attempt %s", ErrIdentityCollision, attempt.ID)
		}
		return ReconciliationAttempt{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: request.Actor, EntityType: "reconciliation_attempt", EntityID: attempt.ID, Action: "reconciliation.started",
		Reason: request.Reason, PayloadJSON: auditPayload(map[string]any{"subject_type": attempt.SubjectType, "subject_id": attempt.SubjectID, "ordinal": attempt.Ordinal}),
		OperationKey: attempt.OperationKey, CreatedAt: now,
	}); err != nil {
		return ReconciliationAttempt{}, err
	}
	if err := tx.Commit(); err != nil {
		return ReconciliationAttempt{}, err
	}
	return attempt, nil
}

func (s *Store) CompleteReconciliationAttempt(ctx context.Context, request CompleteReconciliationAttemptRequest) (ReconciliationAttempt, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return ReconciliationAttempt{}, err
	}
	if !isUUIDv7(request.AttemptID) {
		return ReconciliationAttempt{}, ErrInvalidUUIDv7Identity
	}
	if request.ExpectedVersion <= 0 || (request.State != ReconciliationCompleted && request.State != ReconciliationFailed) {
		return ReconciliationAttempt{}, fmt.Errorf("%w: invalid reconciliation completion", ErrInvalidControl)
	}
	observed, err := normalizeJSON(request.ObservedJSON, "reconciliation observed facts")
	if err != nil {
		return ReconciliationAttempt{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReconciliationAttempt{}, err
	}
	defer tx.Rollback()
	attempt, err := getReconciliationAttemptTx(ctx, tx, request.AttemptID)
	if err != nil {
		return ReconciliationAttempt{}, err
	}
	if attempt.Version != request.ExpectedVersion {
		return ReconciliationAttempt{}, fmt.Errorf("%w: reconciliation attempt %s", ErrOptimisticLock, attempt.ID)
	}
	if attempt.State != ReconciliationRunning {
		return ReconciliationAttempt{}, fmt.Errorf("%w: reconciliation attempt %s is %s", ErrInvalidTransition, attempt.ID, attempt.State)
	}
	now := s.now().UTC()
	attempt.State = request.State
	attempt.ObservedJSON = observed
	attempt.Resolution = strings.TrimSpace(request.Resolution)
	attempt.FinishedAt = &now
	attempt.Version++
	result, err := tx.ExecContext(ctx, `
		UPDATE reconciliation_attempts_v5
		SET state = ?, observed_json = ?, resolution = ?, finished_at = ?, version = ?
		WHERE id = ? AND version = ?
	`, attempt.State, attempt.ObservedJSON, attempt.Resolution, attempt.FinishedAt, attempt.Version, attempt.ID, request.ExpectedVersion)
	if err != nil {
		return ReconciliationAttempt{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return ReconciliationAttempt{}, err
	}
	if changed != 1 {
		return ReconciliationAttempt{}, fmt.Errorf("%w: reconciliation attempt %s", ErrOptimisticLock, attempt.ID)
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: request.Actor, EntityType: "reconciliation_attempt", EntityID: attempt.ID, Action: "reconciliation.completed",
		Reason: request.Reason, PayloadJSON: auditPayload(map[string]any{"state": attempt.State, "resolution": attempt.Resolution}),
		OperationKey: attempt.OperationKey, CreatedAt: now,
	}); err != nil {
		return ReconciliationAttempt{}, err
	}
	if err := tx.Commit(); err != nil {
		return ReconciliationAttempt{}, err
	}
	return attempt, nil
}

type preparedSideEffectOperation struct {
	ID             string
	OperationKey   string
	RunID          string
	StageAttemptID string
	EffectKind     string
	IdempotencyKey string
	SourceDigest   string
	PayloadJSON    string
	Actor          string
	Reason         string
}

func prepareSideEffectOperation(s *Store, request CreateSideEffectOperationRequest) (preparedSideEffectOperation, error) {
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return preparedSideEffectOperation{}, err
	}
	operationKey, err := normalizeRequired(request.OperationKey, "side effect operation key")
	if err != nil {
		return preparedSideEffectOperation{}, fmt.Errorf("%w: %v", ErrInvalidControl, err)
	}
	idempotencyKey, err := normalizeRequired(request.IdempotencyKey, "side effect idempotency key")
	if err != nil {
		return preparedSideEffectOperation{}, fmt.Errorf("%w: %v", ErrInvalidControl, err)
	}
	effectKind, err := normalizeRequired(request.EffectKind, "side effect kind")
	if err != nil {
		return preparedSideEffectOperation{}, fmt.Errorf("%w: %v", ErrInvalidControl, err)
	}
	sourceDigest, err := normalizeRequired(request.SourceDigest, "side effect source digest")
	if err != nil {
		return preparedSideEffectOperation{}, fmt.Errorf("%w: %v", ErrInvalidControl, err)
	}
	runID := strings.TrimSpace(request.RunID)
	stageAttemptID := strings.TrimSpace(request.StageAttemptID)
	if runID != "" && !isUUIDv7(runID) {
		return preparedSideEffectOperation{}, ErrInvalidUUIDv7Identity
	}
	if stageAttemptID != "" && !isUUIDv7(stageAttemptID) {
		return preparedSideEffectOperation{}, ErrInvalidUUIDv7Identity
	}
	if stageAttemptID != "" && runID == "" {
		return preparedSideEffectOperation{}, fmt.Errorf("%w: stage side effect requires its workflow run", ErrInvalidControl)
	}
	payload, err := normalizeJSON(request.PayloadJSON, "side effect payload")
	if err != nil {
		return preparedSideEffectOperation{}, err
	}
	return preparedSideEffectOperation{ID: id, OperationKey: operationKey, RunID: runID, StageAttemptID: stageAttemptID,
		EffectKind: effectKind, IdempotencyKey: idempotencyKey, SourceDigest: sourceDigest, PayloadJSON: payload,
		Actor: resolveActor(request.Actor), Reason: strings.TrimSpace(request.Reason)}, nil
}

func verifySideEffectScopeTx(ctx context.Context, tx *sql.Tx, runID, stageAttemptID string) error {
	if runID == "" {
		return nil
	}
	if _, err := getWorkflowRunTx(ctx, tx, runID); err != nil {
		return err
	}
	if stageAttemptID == "" {
		return nil
	}
	stage, err := getStageAttemptTx(ctx, tx, stageAttemptID)
	if err != nil {
		return err
	}
	if stage.RunID != runID {
		return fmt.Errorf("%w: side effect stage belongs to another run", ErrInvalidControl)
	}
	return nil
}

func getSideEffectByOperationKeyTx(ctx context.Context, tx *sql.Tx, operationKey string) (*SideEffectOperation, error) {
	operation, err := scanSideEffectOperation(tx.QueryRowContext(ctx, sideEffectOperationV5Select+" WHERE operation_key = ?", operationKey))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &operation, nil
}

func getSideEffectByIdempotencyKeyTx(ctx context.Context, tx *sql.Tx, idempotencyKey string) (*SideEffectOperation, error) {
	operation, err := scanSideEffectOperation(tx.QueryRowContext(ctx, sideEffectOperationV5Select+" WHERE idempotency_key = ?", idempotencyKey))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &operation, nil
}

func getSideEffectOperationTx(ctx context.Context, tx *sql.Tx, operationID string) (SideEffectOperation, error) {
	operation, err := scanSideEffectOperation(tx.QueryRowContext(ctx, sideEffectOperationV5Select+" WHERE id = ?", operationID))
	if err == sql.ErrNoRows {
		return SideEffectOperation{}, fmt.Errorf("%w: side effect operation %s", ErrNotFound, operationID)
	}
	return operation, err
}

func sameSideEffectCreateRequest(operation SideEffectOperation, request preparedSideEffectOperation) bool {
	return operation.OperationKey == request.OperationKey && operation.RunID == request.RunID &&
		operation.StageAttemptID == request.StageAttemptID && operation.EffectKind == request.EffectKind &&
		operation.IdempotencyKey == request.IdempotencyKey && operation.SourceDigest == request.SourceDigest &&
		operation.PayloadJSON == request.PayloadJSON
}

type preparedSideEffectTransition struct {
	OperationID       string
	ExpectedVersion   int64
	State             SideEffectOperationState
	DestinationDigest string
	ReceiptRef        string
	PayloadJSON       string
	HasPayload        bool
	Actor             string
	Reason            string
}

func prepareSideEffectTransition(request TransitionSideEffectOperationRequest) (preparedSideEffectTransition, error) {
	if !isUUIDv7(request.OperationID) {
		return preparedSideEffectTransition{}, ErrInvalidUUIDv7Identity
	}
	if request.ExpectedVersion <= 0 || !validSideEffectOperationState(request.State) || request.State == SideEffectPrepared {
		return preparedSideEffectTransition{}, fmt.Errorf("%w: invalid side effect transition", ErrInvalidControl)
	}
	destinationDigest := strings.TrimSpace(request.DestinationDigest)
	receiptRef := strings.TrimSpace(request.ReceiptRef)
	if request.State == SideEffectSucceeded && (destinationDigest == "" || receiptRef == "") {
		return preparedSideEffectTransition{}, fmt.Errorf("%w: successful side effect needs immutable destination digest and receipt", ErrInvalidControl)
	}
	payload := ""
	hasPayload := strings.TrimSpace(request.PayloadJSON) != ""
	if hasPayload {
		var err error
		payload, err = normalizeJSON(request.PayloadJSON, "side effect transition payload")
		if err != nil {
			return preparedSideEffectTransition{}, err
		}
	}
	return preparedSideEffectTransition{OperationID: request.OperationID, ExpectedVersion: request.ExpectedVersion, State: request.State,
		DestinationDigest: destinationDigest, ReceiptRef: receiptRef, PayloadJSON: payload, HasPayload: hasPayload,
		Actor: resolveActor(request.Actor), Reason: strings.TrimSpace(request.Reason)}, nil
}

func validSideEffectOperationState(state SideEffectOperationState) bool {
	switch state {
	case SideEffectPrepared, SideEffectStarted, SideEffectSucceeded, SideEffectFailed, SideEffectUnknown:
		return true
	default:
		return false
	}
}

func validSideEffectTransition(from, to SideEffectOperationState) bool {
	if from == to {
		return false
	}
	switch from {
	case SideEffectPrepared:
		return to == SideEffectStarted || to == SideEffectFailed
	case SideEffectStarted:
		return to == SideEffectSucceeded || to == SideEffectFailed || to == SideEffectUnknown
	case SideEffectUnknown:
		return to == SideEffectSucceeded || to == SideEffectFailed
	default:
		return false
	}
}

func mergeSideEffectFacts(operation *SideEffectOperation, transition preparedSideEffectTransition) error {
	if err := mergeSideEffectFact(&operation.DestinationDigest, transition.DestinationDigest, "destination digest"); err != nil {
		return err
	}
	if err := mergeSideEffectFact(&operation.ReceiptRef, transition.ReceiptRef, "receipt ref"); err != nil {
		return err
	}
	if transition.HasPayload {
		if operation.PayloadJSON != "{}" && operation.PayloadJSON != transition.PayloadJSON {
			return fmt.Errorf("%w: side effect payload cannot change after preparation", ErrImmutable)
		}
		operation.PayloadJSON = transition.PayloadJSON
	}
	return nil
}

func mergeSideEffectFact(current *string, incoming, label string) error {
	if incoming == "" {
		return nil
	}
	if *current != "" && *current != incoming {
		return fmt.Errorf("%w: side effect %s cannot change", ErrImmutable, label)
	}
	*current = incoming
	return nil
}

type preparedReconciliationStart struct {
	ID                    string
	OperationKey          string
	SubjectType           string
	SubjectID             string
	SideEffectOperationID string
	ControlOperationID    string
	Ordinal               int
	ObservedJSON          string
	Actor                 string
	Reason                string
}

func prepareReconciliationStart(s *Store, request StartReconciliationAttemptRequest) (preparedReconciliationStart, error) {
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return preparedReconciliationStart{}, err
	}
	operationKey, err := normalizeRequired(request.OperationKey, "reconciliation operation key")
	if err != nil {
		return preparedReconciliationStart{}, fmt.Errorf("%w: %v", ErrInvalidControl, err)
	}
	subjectType, err := normalizeRequired(request.SubjectType, "reconciliation subject type")
	if err != nil {
		return preparedReconciliationStart{}, fmt.Errorf("%w: %v", ErrInvalidControl, err)
	}
	subjectID, err := normalizeRequired(request.SubjectID, "reconciliation subject id")
	if err != nil {
		return preparedReconciliationStart{}, fmt.Errorf("%w: %v", ErrInvalidControl, err)
	}
	sideEffectID := strings.TrimSpace(request.SideEffectOperationID)
	controlID := strings.TrimSpace(request.ControlOperationID)
	if (sideEffectID == "" && controlID == "") || (sideEffectID != "" && controlID != "") {
		return preparedReconciliationStart{}, fmt.Errorf("%w: reconciliation targets exactly one operation", ErrInvalidControl)
	}
	if sideEffectID != "" && !isUUIDv7(sideEffectID) {
		return preparedReconciliationStart{}, ErrInvalidUUIDv7Identity
	}
	if controlID != "" && !isUUIDv7(controlID) {
		return preparedReconciliationStart{}, ErrInvalidUUIDv7Identity
	}
	if request.Ordinal <= 0 {
		return preparedReconciliationStart{}, fmt.Errorf("%w: reconciliation ordinal must be positive", ErrInvalidControl)
	}
	observed, err := normalizeJSON(request.ObservedJSON, "reconciliation observed facts")
	if err != nil {
		return preparedReconciliationStart{}, err
	}
	return preparedReconciliationStart{ID: id, OperationKey: operationKey, SubjectType: subjectType, SubjectID: subjectID,
		SideEffectOperationID: sideEffectID, ControlOperationID: controlID, Ordinal: request.Ordinal, ObservedJSON: observed,
		Actor: resolveActor(request.Actor), Reason: strings.TrimSpace(request.Reason)}, nil
}

func verifyReconciliationSubjectTx(ctx context.Context, tx *sql.Tx, request preparedReconciliationStart) error {
	if request.SideEffectOperationID != "" {
		if _, err := getSideEffectOperationTx(ctx, tx, request.SideEffectOperationID); err != nil {
			return err
		}
	}
	if request.ControlOperationID != "" {
		if _, err := getDurableControlOperationTx(ctx, tx, request.ControlOperationID); err != nil {
			return err
		}
	}
	return nil
}

func getReconciliationAttemptByKeyTx(ctx context.Context, tx *sql.Tx, operationKey string) (*ReconciliationAttempt, error) {
	attempt, err := scanReconciliationAttempt(tx.QueryRowContext(ctx, reconciliationAttemptV5Select+" WHERE operation_key = ?", operationKey))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &attempt, nil
}

func getReconciliationAttemptTx(ctx context.Context, tx *sql.Tx, attemptID string) (ReconciliationAttempt, error) {
	attempt, err := scanReconciliationAttempt(tx.QueryRowContext(ctx, reconciliationAttemptV5Select+" WHERE id = ?", attemptID))
	if err == sql.ErrNoRows {
		return ReconciliationAttempt{}, fmt.Errorf("%w: reconciliation attempt %s", ErrNotFound, attemptID)
	}
	return attempt, err
}

func sameReconciliationStart(attempt ReconciliationAttempt, request preparedReconciliationStart) bool {
	return attempt.OperationKey == request.OperationKey && attempt.SubjectType == request.SubjectType && attempt.SubjectID == request.SubjectID &&
		attempt.SideEffectOperationID == request.SideEffectOperationID && attempt.ControlOperationID == request.ControlOperationID &&
		attempt.Ordinal == request.Ordinal && attempt.ObservedJSON == request.ObservedJSON
}

func scanSideEffectOperation(scanner rowScanner) (SideEffectOperation, error) {
	var operation SideEffectOperation
	if err := scanner.Scan(&operation.ID, &operation.OperationKey, &operation.RunID, &operation.StageAttemptID,
		&operation.EffectKind, &operation.IdempotencyKey, &operation.SourceDigest, &operation.DestinationDigest,
		&operation.ReceiptRef, &operation.PayloadJSON, &operation.State, &operation.CreatedAt, &operation.UpdatedAt, &operation.Version); err != nil {
		return SideEffectOperation{}, err
	}
	operation.CreatedAt = operation.CreatedAt.UTC()
	operation.UpdatedAt = operation.UpdatedAt.UTC()
	return operation, nil
}

func scanReconciliationAttempt(scanner rowScanner) (ReconciliationAttempt, error) {
	var attempt ReconciliationAttempt
	var sideEffectID, controlID sql.NullString
	var finishedAt sql.NullTime
	if err := scanner.Scan(&attempt.ID, &attempt.OperationKey, &attempt.SubjectType, &attempt.SubjectID,
		&sideEffectID, &controlID, &attempt.Ordinal, &attempt.State, &attempt.ObservedJSON, &attempt.Resolution,
		&attempt.CreatedAt, &finishedAt, &attempt.Version); err != nil {
		return ReconciliationAttempt{}, err
	}
	attempt.SideEffectOperationID = nullableStringValue(sideEffectID)
	attempt.ControlOperationID = nullableStringValue(controlID)
	attempt.CreatedAt = attempt.CreatedAt.UTC()
	attempt.FinishedAt = nullableTimePtr(finishedAt)
	return attempt, nil
}

func (s *Store) appendV5OutboxTx(ctx context.Context, tx *sql.Tx, topic, entityType, entityID, idempotencyKey, payload string, now time.Time) error {
	id, err := s.newV2ID("")
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO outbox_events (
			id, topic, entity_type, entity_id, payload_json, idempotency_key, state,
			created_at, published_at, version, available_at, lease_owner, lease_expires_at,
			lease_fencing_token, delivery_count, last_error, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, 'pending', ?, NULL, 1, ?, '', NULL, 0, 0, '', ?)
	`, id, topic, entityType, entityID, payload, idempotencyKey, now, now, now)
	if err != nil {
		if isGlobalIdentityCollision(err) {
			return fmt.Errorf("%w: outbox event %s", ErrIdentityCollision, id)
		}
		if isUniqueConstraint(err) {
			return fmt.Errorf("%w: outbox key %s", ErrIdempotencyConflict, idempotencyKey)
		}
		return err
	}
	return nil
}
