package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

const continuationCommandV4Select = `
	SELECT id, command_key, subject_id, run_id, payload_json, payload_digest,
	       actor, reason, created_at
	FROM continuation_commands_v4`

const repairSessionV4Select = `
	SELECT id, command_id, subject_id, base_revision_id, max_rounds, status,
	       findings_json, policy_json, idempotency_key, created_by, created_at,
	       updated_at, version
	FROM repair_sessions_v4`

const preparedChangeV4Select = `
	SELECT id, command_id, repair_session_id, round_ordinal, provider_id,
	       operation_key, payload_json, observed_changes_json, before_digest,
	       after_digest, created_by, created_at
	FROM prepared_changes_v4`

const mutationReceiptV4Select = `
	SELECT id, prepared_change_id, operation_key, outcome, receipt_json,
	       receipt_digest, supersedes_receipt_id, idempotency_key, created_by,
	       created_at
	FROM mutation_receipts_v4`

const frozenPlanV4Select = `
	SELECT id, command_id, prepared_change_id, subject_id, subject_revision_id,
	       subject_digest, workflow_fingerprint, plan_fingerprint, payload_json,
	       payload_digest, expires_at, created_by, created_at
	FROM frozen_plans_v4`

const continuationExecutionV4Select = `
	SELECT id, plan_id, parent_execution_id, run_id, idempotency_key, state,
	       payload_json, created_by, created_at, updated_at, finished_at, version
	FROM continuation_executions_v4`

// CreateContinuationCommand stores one immutable user command. CommandKey is
// its idempotency identity; the canonical JSON payload prevents a retry from
// silently changing intent after a process restart.
func (s *Store) CreateContinuationCommand(ctx context.Context, request CreateContinuationCommandRequest) (ContinuationCommand, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return ContinuationCommand{}, err
	}
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return ContinuationCommand{}, err
	}
	commandKey, err := normalizeRequired(request.CommandKey, "continuation command key")
	if err != nil {
		return ContinuationCommand{}, err
	}
	subjectID, err := normalizeRequired(request.SubjectID, "continuation command subject ID")
	if err != nil {
		return ContinuationCommand{}, err
	}
	payloadJSON, err := normalizeV4JSON(request.PayloadJSON, "continuation command payload")
	if err != nil {
		return ContinuationCommand{}, err
	}
	now := s.now().UTC()
	command := ContinuationCommand{
		ID:            id,
		CommandKey:    commandKey,
		SubjectID:     subjectID,
		RunID:         strings.TrimSpace(request.RunID),
		PayloadJSON:   payloadJSON,
		PayloadDigest: v4PayloadDigest(payloadJSON),
		Actor:         resolveActor(request.Actor),
		Reason:        strings.TrimSpace(request.Reason),
		CreatedAt:     now,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ContinuationCommand{}, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO continuation_commands_v4 (
			id, command_key, subject_id, run_id, payload_json, payload_digest,
			actor, reason, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, command.ID, command.CommandKey, command.SubjectID, command.RunID, command.PayloadJSON, command.PayloadDigest,
		command.Actor, command.Reason, command.CreatedAt)
	if err != nil {
		if !isUniqueConstraint(err) {
			return ContinuationCommand{}, err
		}
		existing, existingErr := getContinuationCommandByKeyTx(ctx, tx, command.CommandKey)
		if existingErr == nil {
			if sameContinuationCommand(existing, command) {
				if err := tx.Commit(); err != nil {
					return ContinuationCommand{}, err
				}
				return existing, nil
			}
			return ContinuationCommand{}, fmt.Errorf("%w: continuation command key %s", ErrIdempotencyConflict, command.CommandKey)
		}
		if !isNotFound(existingErr) {
			return ContinuationCommand{}, existingErr
		}
		return ContinuationCommand{}, fmt.Errorf("%w: continuation command %s", ErrIdentityCollision, command.ID)
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       command.Actor,
		EntityType:  "continuation_command",
		EntityID:    command.ID,
		Action:      "continuation_command.created",
		Reason:      command.Reason,
		PayloadJSON: auditPayload(map[string]any{"command_key": command.CommandKey, "subject_id": command.SubjectID, "payload_digest": command.PayloadDigest}),
		CreatedAt:   now,
	}); err != nil {
		return ContinuationCommand{}, err
	}
	if err := tx.Commit(); err != nil {
		return ContinuationCommand{}, err
	}
	return command, nil
}

func (s *Store) GetContinuationCommand(ctx context.Context, commandID string) (*ContinuationCommand, error) {
	if _, err := requireV4ID(commandID, "continuation command ID"); err != nil {
		return nil, err
	}
	command, err := scanContinuationCommand(s.db.QueryRowContext(ctx, continuationCommandV4Select+" WHERE id = ?", commandID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &command, nil
}

// CreateRepairSession creates a bounded, CAS-projected session linked to one
// immutable command. It does not invoke a mutator or modify any revision.
func (s *Store) CreateRepairSession(ctx context.Context, request CreateRepairSessionRequest) (RepairSession, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return RepairSession{}, err
	}
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return RepairSession{}, err
	}
	commandID, err := requireV4ID(request.CommandID, "repair session command ID")
	if err != nil {
		return RepairSession{}, err
	}
	subjectID, err := normalizeRequired(request.SubjectID, "repair session subject ID")
	if err != nil {
		return RepairSession{}, err
	}
	baseRevisionID, err := normalizeRequired(request.BaseRevisionID, "repair session base revision ID")
	if err != nil {
		return RepairSession{}, err
	}
	if request.MaxRounds <= 0 {
		return RepairSession{}, fmt.Errorf("repair session max rounds must be positive")
	}
	findingsJSON, err := normalizeV4JSON(request.FindingsJSON, "repair session findings")
	if err != nil {
		return RepairSession{}, err
	}
	policyJSON, err := normalizeV4JSON(request.PolicyJSON, "repair session policy")
	if err != nil {
		return RepairSession{}, err
	}
	key, err := normalizeRequired(request.IdempotencyKey, "repair session idempotency key")
	if err != nil {
		return RepairSession{}, err
	}
	now := s.now().UTC()
	session := RepairSession{
		ID:             id,
		CommandID:      commandID,
		SubjectID:      subjectID,
		BaseRevisionID: baseRevisionID,
		MaxRounds:      request.MaxRounds,
		Status:         RepairSessionOpen,
		FindingsJSON:   findingsJSON,
		PolicyJSON:     policyJSON,
		IdempotencyKey: key,
		CreatedBy:      resolveActor(request.Actor),
		CreatedAt:      now,
		UpdatedAt:      now,
		Version:        1,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RepairSession{}, err
	}
	defer tx.Rollback()
	if _, err := getContinuationCommandTx(ctx, tx, session.CommandID); err != nil {
		return RepairSession{}, err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO repair_sessions_v4 (
			id, command_id, subject_id, base_revision_id, max_rounds, status,
			findings_json, policy_json, idempotency_key, created_by, created_at,
			updated_at, version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, session.ID, session.CommandID, session.SubjectID, session.BaseRevisionID, session.MaxRounds, session.Status,
		session.FindingsJSON, session.PolicyJSON, session.IdempotencyKey, session.CreatedBy, session.CreatedAt,
		session.UpdatedAt, session.Version)
	if err != nil {
		if !isUniqueConstraint(err) {
			return RepairSession{}, err
		}
		existing, existingErr := getRepairSessionByKeyTx(ctx, tx, session.IdempotencyKey)
		if existingErr == nil {
			if sameRepairSession(existing, session) {
				if err := tx.Commit(); err != nil {
					return RepairSession{}, err
				}
				return existing, nil
			}
			return RepairSession{}, fmt.Errorf("%w: repair session key %s", ErrIdempotencyConflict, session.IdempotencyKey)
		}
		if !isNotFound(existingErr) {
			return RepairSession{}, existingErr
		}
		return RepairSession{}, fmt.Errorf("%w: repair session %s", ErrIdentityCollision, session.ID)
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       request.Actor,
		EntityType:  "repair_session",
		EntityID:    session.ID,
		Action:      "repair_session.created",
		Reason:      request.Reason,
		PayloadJSON: auditPayload(map[string]any{"command_id": session.CommandID, "max_rounds": session.MaxRounds, "idempotency_key": session.IdempotencyKey}),
		CreatedAt:   now,
	}); err != nil {
		return RepairSession{}, err
	}
	if err := tx.Commit(); err != nil {
		return RepairSession{}, err
	}
	return session, nil
}

func (s *Store) GetRepairSession(ctx context.Context, sessionID string) (*RepairSession, error) {
	if _, err := requireV4ID(sessionID, "repair session ID"); err != nil {
		return nil, err
	}
	session, err := scanRepairSession(s.db.QueryRowContext(ctx, repairSessionV4Select+" WHERE id = ?", sessionID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &session, nil
}

// ListRepairSessionsForSubject exposes durable repair envelopes for a stable
// lifecycle subject without exposing their mutable execution machinery.
func (s *Store) ListRepairSessionsForSubject(ctx context.Context, subjectID string) ([]RepairSession, error) {
	subjectID, err := normalizeRequired(subjectID, "repair session subject ID")
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, repairSessionV4Select+" WHERE subject_id = ? ORDER BY created_at DESC, id ASC", subjectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sessions := make([]RepairSession, 0)
	for rows.Next() {
		session, err := scanRepairSession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

// TransitionRepairSession updates only the CAS-protected state projection.
func (s *Store) TransitionRepairSession(ctx context.Context, request TransitionRepairSessionRequest) (RepairSession, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return RepairSession{}, err
	}
	sessionID, err := requireV4ID(request.RepairSessionID, "repair session ID")
	if err != nil {
		return RepairSession{}, err
	}
	if request.ExpectedVersion <= 0 {
		return RepairSession{}, fmt.Errorf("expected repair session version must be positive")
	}
	if !validRepairSessionState(request.Status) {
		return RepairSession{}, fmt.Errorf("invalid repair session state %q", request.Status)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RepairSession{}, err
	}
	defer tx.Rollback()
	session, err := getRepairSessionTx(ctx, tx, sessionID)
	if err != nil {
		return RepairSession{}, err
	}
	if session.Version != request.ExpectedVersion {
		return RepairSession{}, fmt.Errorf("%w: repair session %s", ErrOptimisticLock, session.ID)
	}
	if !validRepairSessionTransition(session.Status, request.Status) {
		return RepairSession{}, fmt.Errorf("%w: repair session %s from %s to %s", ErrInvalidTransition, session.ID, session.Status, request.Status)
	}
	now := s.now().UTC()
	session.Status = request.Status
	session.UpdatedAt = now
	session.Version++
	result, err := tx.ExecContext(ctx, `
		UPDATE repair_sessions_v4 SET status = ?, updated_at = ?, version = ?
		WHERE id = ? AND version = ?
	`, session.Status, session.UpdatedAt, session.Version, session.ID, request.ExpectedVersion)
	if err != nil {
		return RepairSession{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return RepairSession{}, err
	}
	if changed != 1 {
		return RepairSession{}, fmt.Errorf("%w: repair session %s", ErrOptimisticLock, session.ID)
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       request.Actor,
		EntityType:  "repair_session",
		EntityID:    session.ID,
		Action:      "repair_session.transitioned",
		Reason:      request.Reason,
		PayloadJSON: auditPayload(map[string]any{"state": session.Status, "version": session.Version}),
		CreatedAt:   now,
	}); err != nil {
		return RepairSession{}, err
	}
	if err := tx.Commit(); err != nil {
		return RepairSession{}, err
	}
	return session, nil
}

// CreatePreparedChange persists normalized mutation intent and observed change
// facts before a frozen plan can reference them.
func (s *Store) CreatePreparedChange(ctx context.Context, request CreatePreparedChangeRequest) (PreparedChange, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return PreparedChange{}, err
	}
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return PreparedChange{}, err
	}
	commandID, err := requireV4ID(request.CommandID, "prepared change command ID")
	if err != nil {
		return PreparedChange{}, err
	}
	repairSessionID, err := optionalV4ID(request.RepairSessionID, "prepared change repair session ID")
	if err != nil {
		return PreparedChange{}, err
	}
	if repairSessionID == "" && request.RoundOrdinal != 0 {
		return PreparedChange{}, fmt.Errorf("non-repair prepared change must use round ordinal zero")
	}
	if repairSessionID != "" && request.RoundOrdinal <= 0 {
		return PreparedChange{}, fmt.Errorf("repair prepared change round ordinal must be positive")
	}
	providerID, err := normalizeRequired(request.ProviderID, "prepared change provider ID")
	if err != nil {
		return PreparedChange{}, err
	}
	operationKey, err := normalizeRequired(request.OperationKey, "prepared change operation key")
	if err != nil {
		return PreparedChange{}, err
	}
	payloadJSON, err := normalizeV4JSON(request.PayloadJSON, "prepared change payload")
	if err != nil {
		return PreparedChange{}, err
	}
	changesJSON, err := normalizeV4JSON(request.ObservedChangesJSON, "prepared change observed changes")
	if err != nil {
		return PreparedChange{}, err
	}
	beforeDigest, err := normalizeRequired(request.BeforeDigest, "prepared change before digest")
	if err != nil {
		return PreparedChange{}, err
	}
	afterDigest, err := normalizeRequired(request.AfterDigest, "prepared change after digest")
	if err != nil {
		return PreparedChange{}, err
	}
	now := s.now().UTC()
	change := PreparedChange{
		ID:                  id,
		CommandID:           commandID,
		RepairSessionID:     repairSessionID,
		RoundOrdinal:        request.RoundOrdinal,
		ProviderID:          providerID,
		OperationKey:        operationKey,
		PayloadJSON:         payloadJSON,
		ObservedChangesJSON: changesJSON,
		BeforeDigest:        beforeDigest,
		AfterDigest:         afterDigest,
		CreatedBy:           resolveActor(request.Actor),
		CreatedAt:           now,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PreparedChange{}, err
	}
	defer tx.Rollback()
	if _, err := getContinuationCommandTx(ctx, tx, change.CommandID); err != nil {
		return PreparedChange{}, err
	}
	if change.RepairSessionID != "" {
		session, err := getRepairSessionTx(ctx, tx, change.RepairSessionID)
		if err != nil {
			return PreparedChange{}, err
		}
		if session.CommandID != change.CommandID {
			return PreparedChange{}, fmt.Errorf("prepared change repair session belongs to another command")
		}
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO prepared_changes_v4 (
			id, command_id, repair_session_id, round_ordinal, provider_id,
			operation_key, payload_json, observed_changes_json, before_digest,
			after_digest, created_by, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, change.ID, change.CommandID, nullableString(change.RepairSessionID), change.RoundOrdinal, change.ProviderID,
		change.OperationKey, change.PayloadJSON, change.ObservedChangesJSON, change.BeforeDigest,
		change.AfterDigest, change.CreatedBy, change.CreatedAt)
	if err != nil {
		if !isUniqueConstraint(err) {
			return PreparedChange{}, err
		}
		existing, existingErr := getPreparedChangeByOperationTx(ctx, tx, change.OperationKey)
		if existingErr == nil {
			if samePreparedChange(existing, change) {
				if err := tx.Commit(); err != nil {
					return PreparedChange{}, err
				}
				return existing, nil
			}
			return PreparedChange{}, fmt.Errorf("%w: prepared change operation key %s", ErrIdempotencyConflict, change.OperationKey)
		}
		if !isNotFound(existingErr) {
			return PreparedChange{}, existingErr
		}
		return PreparedChange{}, fmt.Errorf("%w: prepared change %s", ErrIdentityCollision, change.ID)
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       request.Actor,
		EntityType:  "prepared_change",
		EntityID:    change.ID,
		Action:      "prepared_change.created",
		Reason:      request.Reason,
		PayloadJSON: auditPayload(map[string]any{"command_id": change.CommandID, "operation_key": change.OperationKey, "before_digest": change.BeforeDigest, "after_digest": change.AfterDigest}),
		CreatedAt:   now,
	}); err != nil {
		return PreparedChange{}, err
	}
	if err := tx.Commit(); err != nil {
		return PreparedChange{}, err
	}
	return change, nil
}

// ListPreparedChangesForRepairSession returns the immutable prepared changes
// produced by one bounded repair session, ordered by repair round.
func (s *Store) ListPreparedChangesForRepairSession(ctx context.Context, repairSessionID string) ([]PreparedChange, error) {
	if _, err := requireV4ID(repairSessionID, "prepared change repair session ID"); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, preparedChangeV4Select+" WHERE repair_session_id = ? ORDER BY round_ordinal DESC, created_at DESC, id ASC", repairSessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	changes := make([]PreparedChange, 0)
	for rows.Next() {
		change, err := scanPreparedChange(rows)
		if err != nil {
			return nil, err
		}
		changes = append(changes, change)
	}
	return changes, rows.Err()
}

func (s *Store) GetPreparedChange(ctx context.Context, preparedChangeID string) (*PreparedChange, error) {
	if _, err := requireV4ID(preparedChangeID, "prepared change ID"); err != nil {
		return nil, err
	}
	change, err := scanPreparedChange(s.db.QueryRowContext(ctx, preparedChangeV4Select+" WHERE id = ?", preparedChangeID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &change, nil
}

// CreateMutationReceipt appends a provider result. A receipt cannot modify a
// prepared change, and a later reconcile must create a new linked receipt.
func (s *Store) CreateMutationReceipt(ctx context.Context, request CreateMutationReceiptRequest) (MutationReceipt, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return MutationReceipt{}, err
	}
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return MutationReceipt{}, err
	}
	preparedChangeID, err := requireV4ID(request.PreparedChangeID, "mutation receipt prepared change ID")
	if err != nil {
		return MutationReceipt{}, err
	}
	operationKey, err := normalizeRequired(request.OperationKey, "mutation receipt operation key")
	if err != nil {
		return MutationReceipt{}, err
	}
	if !validMutationReceiptOutcome(request.Outcome) {
		return MutationReceipt{}, fmt.Errorf("invalid mutation receipt outcome %q", request.Outcome)
	}
	receiptJSON, err := normalizeV4JSON(request.ReceiptJSON, "mutation receipt payload")
	if err != nil {
		return MutationReceipt{}, err
	}
	supersedesID, err := optionalV4ID(request.SupersedesReceiptID, "mutation receipt supersedes ID")
	if err != nil {
		return MutationReceipt{}, err
	}
	key, err := normalizeRequired(request.IdempotencyKey, "mutation receipt idempotency key")
	if err != nil {
		return MutationReceipt{}, err
	}
	now := s.now().UTC()
	receipt := MutationReceipt{
		ID:                  id,
		PreparedChangeID:    preparedChangeID,
		OperationKey:        operationKey,
		Outcome:             request.Outcome,
		ReceiptJSON:         receiptJSON,
		ReceiptDigest:       v4PayloadDigest(receiptJSON),
		SupersedesReceiptID: supersedesID,
		IdempotencyKey:      key,
		CreatedBy:           resolveActor(request.Actor),
		CreatedAt:           now,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MutationReceipt{}, err
	}
	defer tx.Rollback()
	change, err := getPreparedChangeTx(ctx, tx, receipt.PreparedChangeID)
	if err != nil {
		return MutationReceipt{}, err
	}
	if change.OperationKey != receipt.OperationKey {
		return MutationReceipt{}, fmt.Errorf("mutation receipt operation key does not match prepared change")
	}
	if receipt.SupersedesReceiptID != "" {
		previous, err := getMutationReceiptTx(ctx, tx, receipt.SupersedesReceiptID)
		if err != nil {
			return MutationReceipt{}, err
		}
		if previous.PreparedChangeID != receipt.PreparedChangeID {
			return MutationReceipt{}, fmt.Errorf("mutation receipt supersedes a receipt for another prepared change")
		}
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO mutation_receipts_v4 (
			id, prepared_change_id, operation_key, outcome, receipt_json,
			receipt_digest, supersedes_receipt_id, idempotency_key, created_by,
			created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, receipt.ID, receipt.PreparedChangeID, receipt.OperationKey, receipt.Outcome, receipt.ReceiptJSON,
		receipt.ReceiptDigest, nullableString(receipt.SupersedesReceiptID), receipt.IdempotencyKey, receipt.CreatedBy,
		receipt.CreatedAt)
	if err != nil {
		if !isUniqueConstraint(err) {
			return MutationReceipt{}, err
		}
		existing, existingErr := getMutationReceiptByKeyTx(ctx, tx, receipt.IdempotencyKey)
		if existingErr == nil {
			if sameMutationReceipt(existing, receipt) {
				if err := tx.Commit(); err != nil {
					return MutationReceipt{}, err
				}
				return existing, nil
			}
			return MutationReceipt{}, fmt.Errorf("%w: mutation receipt key %s", ErrIdempotencyConflict, receipt.IdempotencyKey)
		}
		if !isNotFound(existingErr) {
			return MutationReceipt{}, existingErr
		}
		return MutationReceipt{}, fmt.Errorf("%w: mutation receipt %s", ErrIdentityCollision, receipt.ID)
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       request.Actor,
		EntityType:  "mutation_receipt",
		EntityID:    receipt.ID,
		Action:      "mutation_receipt.created",
		Reason:      request.Reason,
		PayloadJSON: auditPayload(map[string]any{"prepared_change_id": receipt.PreparedChangeID, "operation_key": receipt.OperationKey, "outcome": receipt.Outcome, "receipt_digest": receipt.ReceiptDigest}),
		CreatedAt:   now,
	}); err != nil {
		return MutationReceipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return MutationReceipt{}, err
	}
	return receipt, nil
}

// ListMutationReceiptsForPreparedChange returns immutable provider outcomes
// for a prepared change. It is used by read-only lifecycle projections to
// make uncertain external outcomes visible.
func (s *Store) ListMutationReceiptsForPreparedChange(ctx context.Context, preparedChangeID string) ([]MutationReceipt, error) {
	if _, err := requireV4ID(preparedChangeID, "mutation receipt prepared change ID"); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, mutationReceiptV4Select+" WHERE prepared_change_id = ? ORDER BY created_at DESC, id ASC", preparedChangeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	receipts := make([]MutationReceipt, 0)
	for rows.Next() {
		receipt, err := scanMutationReceipt(rows)
		if err != nil {
			return nil, err
		}
		receipts = append(receipts, receipt)
	}
	return receipts, rows.Err()
}

func (s *Store) GetMutationReceipt(ctx context.Context, receiptID string) (*MutationReceipt, error) {
	if _, err := requireV4ID(receiptID, "mutation receipt ID"); err != nil {
		return nil, err
	}
	receipt, err := scanMutationReceipt(s.db.QueryRowContext(ctx, mutationReceiptV4Select+" WHERE id = ?", receiptID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &receipt, nil
}

// CreateFrozenPlan persists the planner's immutable plan exactly once per
// command. It never reinterprets selectors or recalculates payload on replay.
func (s *Store) CreateFrozenPlan(ctx context.Context, request CreateFrozenPlanRequest) (FrozenPlan, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return FrozenPlan{}, err
	}
	plan, err := s.prepareFrozenPlan(request)
	if err != nil {
		return FrozenPlan{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return FrozenPlan{}, err
	}
	defer tx.Rollback()
	if err := validateFrozenPlanDependenciesTx(ctx, tx, plan); err != nil {
		return FrozenPlan{}, err
	}
	if err := insertFrozenPlanTx(ctx, tx, plan); err != nil {
		if !isUniqueConstraint(err) {
			return FrozenPlan{}, err
		}
		existing, existingErr := getFrozenPlanByCommandTx(ctx, tx, plan.CommandID)
		if existingErr == nil {
			if sameFrozenPlan(existing, plan) {
				if err := tx.Commit(); err != nil {
					return FrozenPlan{}, err
				}
				return existing, nil
			}
			return FrozenPlan{}, fmt.Errorf("%w: frozen plan command %s", ErrIdempotencyConflict, plan.CommandID)
		}
		if !isNotFound(existingErr) {
			return FrozenPlan{}, existingErr
		}
		return FrozenPlan{}, fmt.Errorf("%w: frozen plan %s", ErrIdentityCollision, plan.ID)
	}
	if err := s.appendFrozenPlanAuditTx(ctx, tx, plan, request.Actor, request.Reason); err != nil {
		return FrozenPlan{}, err
	}
	if err := tx.Commit(); err != nil {
		return FrozenPlan{}, err
	}
	return plan, nil
}

// prepareFrozenPlan normalizes the immutable facts shared by ordinary plan
// creation and the candidate-bound plan transaction. Callers must still prove
// command/prepared-change ownership inside the transaction that writes it.
func (s *Store) prepareFrozenPlan(request CreateFrozenPlanRequest) (FrozenPlan, error) {
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return FrozenPlan{}, err
	}
	commandID, err := requireV4ID(request.CommandID, "frozen plan command ID")
	if err != nil {
		return FrozenPlan{}, err
	}
	preparedChangeID, err := optionalV4ID(request.PreparedChangeID, "frozen plan prepared change ID")
	if err != nil {
		return FrozenPlan{}, err
	}
	subjectID, err := normalizeRequired(request.SubjectID, "frozen plan subject ID")
	if err != nil {
		return FrozenPlan{}, err
	}
	subjectRevisionID, err := normalizeRequired(request.SubjectRevisionID, "frozen plan subject revision ID")
	if err != nil {
		return FrozenPlan{}, err
	}
	subjectDigest, err := normalizeRequired(request.SubjectDigest, "frozen plan subject digest")
	if err != nil {
		return FrozenPlan{}, err
	}
	workflowFingerprint, err := normalizeRequired(request.WorkflowFingerprint, "frozen plan workflow fingerprint")
	if err != nil {
		return FrozenPlan{}, err
	}
	planFingerprint, err := normalizeRequired(request.PlanFingerprint, "frozen plan fingerprint")
	if err != nil {
		return FrozenPlan{}, err
	}
	payloadJSON, err := normalizeV4JSON(request.PayloadJSON, "frozen plan payload")
	if err != nil {
		return FrozenPlan{}, err
	}
	if request.ExpiresAt.IsZero() {
		return FrozenPlan{}, fmt.Errorf("frozen plan expiration is required")
	}
	now := s.now().UTC()
	expiresAt := request.ExpiresAt.UTC()
	if !expiresAt.After(now) {
		return FrozenPlan{}, fmt.Errorf("frozen plan expiration must be in the future")
	}
	return FrozenPlan{
		ID:                  id,
		CommandID:           commandID,
		PreparedChangeID:    preparedChangeID,
		SubjectID:           subjectID,
		SubjectRevisionID:   subjectRevisionID,
		SubjectDigest:       subjectDigest,
		WorkflowFingerprint: workflowFingerprint,
		PlanFingerprint:     planFingerprint,
		PayloadJSON:         payloadJSON,
		PayloadDigest:       v4PayloadDigest(payloadJSON),
		ExpiresAt:           expiresAt,
		CreatedBy:           resolveActor(request.Actor),
		CreatedAt:           now,
	}, nil
}

func validateFrozenPlanDependenciesTx(ctx context.Context, tx *sql.Tx, plan FrozenPlan) error {
	if _, err := getContinuationCommandTx(ctx, tx, plan.CommandID); err != nil {
		return err
	}
	if plan.PreparedChangeID == "" {
		return nil
	}
	change, err := getPreparedChangeTx(ctx, tx, plan.PreparedChangeID)
	if err != nil {
		return err
	}
	if change.CommandID != plan.CommandID {
		return fmt.Errorf("frozen plan prepared change belongs to another command")
	}
	return nil
}

func insertFrozenPlanTx(ctx context.Context, tx *sql.Tx, plan FrozenPlan) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO frozen_plans_v4 (
			id, command_id, prepared_change_id, subject_id, subject_revision_id,
			subject_digest, workflow_fingerprint, plan_fingerprint, payload_json,
			payload_digest, expires_at, created_by, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, plan.ID, plan.CommandID, nullableString(plan.PreparedChangeID), plan.SubjectID, plan.SubjectRevisionID,
		plan.SubjectDigest, plan.WorkflowFingerprint, plan.PlanFingerprint, plan.PayloadJSON,
		plan.PayloadDigest, plan.ExpiresAt, plan.CreatedBy, plan.CreatedAt)
	return err
}

func (s *Store) appendFrozenPlanAuditTx(ctx context.Context, tx *sql.Tx, plan FrozenPlan, actor, reason string) error {
	_, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       actor,
		EntityType:  "frozen_plan",
		EntityID:    plan.ID,
		Action:      "frozen_plan.created",
		Reason:      reason,
		PayloadJSON: auditPayload(map[string]any{"command_id": plan.CommandID, "plan_fingerprint": plan.PlanFingerprint, "payload_digest": plan.PayloadDigest, "expires_at": plan.ExpiresAt}),
		CreatedAt:   plan.CreatedAt,
	})
	return err
}

func (s *Store) GetFrozenPlan(ctx context.Context, planID string) (*FrozenPlan, error) {
	if _, err := requireV4ID(planID, "frozen plan ID"); err != nil {
		return nil, err
	}
	plan, err := scanFrozenPlan(s.db.QueryRowContext(ctx, frozenPlanV4Select+" WHERE id = ?", planID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &plan, nil
}

// CreateContinuationExecution creates a mutable execution projection for an
// already frozen plan. It is idempotent by caller key and never mutates plan
// identity, payload, or parent linkage after creation.
func (s *Store) CreateContinuationExecution(ctx context.Context, request CreateContinuationExecutionRequest) (ContinuationExecution, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return ContinuationExecution{}, err
	}
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return ContinuationExecution{}, err
	}
	planID, err := requireV4ID(request.PlanID, "continuation execution plan ID")
	if err != nil {
		return ContinuationExecution{}, err
	}
	parentID, err := optionalV4ID(request.ParentExecutionID, "continuation execution parent ID")
	if err != nil {
		return ContinuationExecution{}, err
	}
	key, err := normalizeRequired(request.IdempotencyKey, "continuation execution idempotency key")
	if err != nil {
		return ContinuationExecution{}, err
	}
	payloadJSON, err := normalizeV4JSON(request.PayloadJSON, "continuation execution payload")
	if err != nil {
		return ContinuationExecution{}, err
	}
	now := s.now().UTC()
	execution := ContinuationExecution{
		ID:                id,
		PlanID:            planID,
		ParentExecutionID: parentID,
		RunID:             strings.TrimSpace(request.RunID),
		IdempotencyKey:    key,
		State:             ContinuationExecutionQueued,
		PayloadJSON:       payloadJSON,
		CreatedBy:         resolveActor(request.Actor),
		CreatedAt:         now,
		UpdatedAt:         now,
		Version:           1,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ContinuationExecution{}, err
	}
	defer tx.Rollback()
	if _, err := getFrozenPlanTx(ctx, tx, execution.PlanID); err != nil {
		return ContinuationExecution{}, err
	}
	if execution.ParentExecutionID != "" {
		if _, err := getContinuationExecutionTx(ctx, tx, execution.ParentExecutionID); err != nil {
			return ContinuationExecution{}, err
		}
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO continuation_executions_v4 (
			id, plan_id, parent_execution_id, run_id, idempotency_key, state,
			payload_json, created_by, created_at, updated_at, finished_at, version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)
	`, execution.ID, execution.PlanID, nullableString(execution.ParentExecutionID), nullableString(execution.RunID), execution.IdempotencyKey,
		execution.State, execution.PayloadJSON, execution.CreatedBy, execution.CreatedAt, execution.UpdatedAt, execution.Version)
	if err != nil {
		if !isUniqueConstraint(err) {
			return ContinuationExecution{}, err
		}
		existing, existingErr := getContinuationExecutionByKeyTx(ctx, tx, execution.IdempotencyKey)
		if existingErr == nil {
			if sameContinuationExecution(existing, execution) {
				if err := tx.Commit(); err != nil {
					return ContinuationExecution{}, err
				}
				return existing, nil
			}
			return ContinuationExecution{}, fmt.Errorf("%w: continuation execution key %s", ErrIdempotencyConflict, execution.IdempotencyKey)
		}
		if !isNotFound(existingErr) {
			return ContinuationExecution{}, existingErr
		}
		return ContinuationExecution{}, fmt.Errorf("%w: continuation execution %s", ErrIdentityCollision, execution.ID)
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       request.Actor,
		EntityType:  "continuation_execution",
		EntityID:    execution.ID,
		Action:      "continuation_execution.created",
		Reason:      request.Reason,
		PayloadJSON: auditPayload(map[string]any{"plan_id": execution.PlanID, "parent_execution_id": execution.ParentExecutionID, "idempotency_key": execution.IdempotencyKey}),
		CreatedAt:   now,
	}); err != nil {
		return ContinuationExecution{}, err
	}
	if err := tx.Commit(); err != nil {
		return ContinuationExecution{}, err
	}
	return execution, nil
}

func (s *Store) GetContinuationExecution(ctx context.Context, executionID string) (*ContinuationExecution, error) {
	if _, err := requireV4ID(executionID, "continuation execution ID"); err != nil {
		return nil, err
	}
	execution, err := scanContinuationExecution(s.db.QueryRowContext(ctx, continuationExecutionV4Select+" WHERE id = ?", executionID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &execution, nil
}

// TransitionContinuationExecution changes only execution state under CAS.
func (s *Store) TransitionContinuationExecution(ctx context.Context, request TransitionContinuationExecutionRequest) (ContinuationExecution, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return ContinuationExecution{}, err
	}
	executionID, err := requireV4ID(request.ContinuationExecutionID, "continuation execution ID")
	if err != nil {
		return ContinuationExecution{}, err
	}
	if request.ExpectedVersion <= 0 {
		return ContinuationExecution{}, fmt.Errorf("expected continuation execution version must be positive")
	}
	if !validContinuationExecutionState(request.State) {
		return ContinuationExecution{}, fmt.Errorf("invalid continuation execution state %q", request.State)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ContinuationExecution{}, err
	}
	defer tx.Rollback()
	execution, err := getContinuationExecutionTx(ctx, tx, executionID)
	if err != nil {
		return ContinuationExecution{}, err
	}
	if execution.Version != request.ExpectedVersion {
		return ContinuationExecution{}, fmt.Errorf("%w: continuation execution %s", ErrOptimisticLock, execution.ID)
	}
	if !validContinuationExecutionTransition(execution.State, request.State) {
		return ContinuationExecution{}, fmt.Errorf("%w: continuation execution %s from %s to %s", ErrInvalidTransition, execution.ID, execution.State, request.State)
	}
	now := s.now().UTC()
	execution.State = request.State
	execution.UpdatedAt = now
	if isTerminalContinuationExecutionState(execution.State) {
		execution.FinishedAt = &now
	}
	execution.Version++
	result, err := tx.ExecContext(ctx, `
		UPDATE continuation_executions_v4
		SET state = ?, updated_at = ?, finished_at = ?, version = ?
		WHERE id = ? AND version = ?
	`, execution.State, execution.UpdatedAt, execution.FinishedAt, execution.Version, execution.ID, request.ExpectedVersion)
	if err != nil {
		return ContinuationExecution{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return ContinuationExecution{}, err
	}
	if changed != 1 {
		return ContinuationExecution{}, fmt.Errorf("%w: continuation execution %s", ErrOptimisticLock, execution.ID)
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       request.Actor,
		EntityType:  "continuation_execution",
		EntityID:    execution.ID,
		Action:      "continuation_execution.transitioned",
		Reason:      request.Reason,
		PayloadJSON: auditPayload(map[string]any{"state": execution.State, "version": execution.Version}),
		CreatedAt:   now,
	}); err != nil {
		return ContinuationExecution{}, err
	}
	if err := tx.Commit(); err != nil {
		return ContinuationExecution{}, err
	}
	return execution, nil
}

func getContinuationCommandTx(ctx context.Context, tx *sql.Tx, commandID string) (ContinuationCommand, error) {
	command, err := scanContinuationCommand(tx.QueryRowContext(ctx, continuationCommandV4Select+" WHERE id = ?", commandID))
	if err == sql.ErrNoRows {
		return ContinuationCommand{}, fmt.Errorf("%w: continuation command %s", ErrNotFound, commandID)
	}
	return command, err
}

func getContinuationCommandByKeyTx(ctx context.Context, tx *sql.Tx, key string) (ContinuationCommand, error) {
	command, err := scanContinuationCommand(tx.QueryRowContext(ctx, continuationCommandV4Select+" WHERE command_key = ?", key))
	if err == sql.ErrNoRows {
		return ContinuationCommand{}, fmt.Errorf("%w: continuation command key %s", ErrNotFound, key)
	}
	return command, err
}

func getRepairSessionTx(ctx context.Context, tx *sql.Tx, sessionID string) (RepairSession, error) {
	session, err := scanRepairSession(tx.QueryRowContext(ctx, repairSessionV4Select+" WHERE id = ?", sessionID))
	if err == sql.ErrNoRows {
		return RepairSession{}, fmt.Errorf("%w: repair session %s", ErrNotFound, sessionID)
	}
	return session, err
}

func getRepairSessionByKeyTx(ctx context.Context, tx *sql.Tx, key string) (RepairSession, error) {
	session, err := scanRepairSession(tx.QueryRowContext(ctx, repairSessionV4Select+" WHERE idempotency_key = ?", key))
	if err == sql.ErrNoRows {
		return RepairSession{}, fmt.Errorf("%w: repair session key %s", ErrNotFound, key)
	}
	return session, err
}

func getPreparedChangeTx(ctx context.Context, tx *sql.Tx, changeID string) (PreparedChange, error) {
	change, err := scanPreparedChange(tx.QueryRowContext(ctx, preparedChangeV4Select+" WHERE id = ?", changeID))
	if err == sql.ErrNoRows {
		return PreparedChange{}, fmt.Errorf("%w: prepared change %s", ErrNotFound, changeID)
	}
	return change, err
}

func getPreparedChangeByOperationTx(ctx context.Context, tx *sql.Tx, operationKey string) (PreparedChange, error) {
	change, err := scanPreparedChange(tx.QueryRowContext(ctx, preparedChangeV4Select+" WHERE operation_key = ?", operationKey))
	if err == sql.ErrNoRows {
		return PreparedChange{}, fmt.Errorf("%w: prepared change operation key %s", ErrNotFound, operationKey)
	}
	return change, err
}

func getMutationReceiptTx(ctx context.Context, tx *sql.Tx, receiptID string) (MutationReceipt, error) {
	receipt, err := scanMutationReceipt(tx.QueryRowContext(ctx, mutationReceiptV4Select+" WHERE id = ?", receiptID))
	if err == sql.ErrNoRows {
		return MutationReceipt{}, fmt.Errorf("%w: mutation receipt %s", ErrNotFound, receiptID)
	}
	return receipt, err
}

func getMutationReceiptByKeyTx(ctx context.Context, tx *sql.Tx, key string) (MutationReceipt, error) {
	receipt, err := scanMutationReceipt(tx.QueryRowContext(ctx, mutationReceiptV4Select+" WHERE idempotency_key = ?", key))
	if err == sql.ErrNoRows {
		return MutationReceipt{}, fmt.Errorf("%w: mutation receipt key %s", ErrNotFound, key)
	}
	return receipt, err
}

func getFrozenPlanTx(ctx context.Context, tx *sql.Tx, planID string) (FrozenPlan, error) {
	plan, err := scanFrozenPlan(tx.QueryRowContext(ctx, frozenPlanV4Select+" WHERE id = ?", planID))
	if err == sql.ErrNoRows {
		return FrozenPlan{}, fmt.Errorf("%w: frozen plan %s", ErrNotFound, planID)
	}
	return plan, err
}

func getFrozenPlanByCommandTx(ctx context.Context, tx *sql.Tx, commandID string) (FrozenPlan, error) {
	plan, err := scanFrozenPlan(tx.QueryRowContext(ctx, frozenPlanV4Select+" WHERE command_id = ?", commandID))
	if err == sql.ErrNoRows {
		return FrozenPlan{}, fmt.Errorf("%w: frozen plan command %s", ErrNotFound, commandID)
	}
	return plan, err
}

func getContinuationExecutionTx(ctx context.Context, tx *sql.Tx, executionID string) (ContinuationExecution, error) {
	execution, err := scanContinuationExecution(tx.QueryRowContext(ctx, continuationExecutionV4Select+" WHERE id = ?", executionID))
	if err == sql.ErrNoRows {
		return ContinuationExecution{}, fmt.Errorf("%w: continuation execution %s", ErrNotFound, executionID)
	}
	return execution, err
}

func getContinuationExecutionByKeyTx(ctx context.Context, tx *sql.Tx, key string) (ContinuationExecution, error) {
	execution, err := scanContinuationExecution(tx.QueryRowContext(ctx, continuationExecutionV4Select+" WHERE idempotency_key = ?", key))
	if err == sql.ErrNoRows {
		return ContinuationExecution{}, fmt.Errorf("%w: continuation execution key %s", ErrNotFound, key)
	}
	return execution, err
}

func scanContinuationCommand(scanner rowScanner) (ContinuationCommand, error) {
	var command ContinuationCommand
	if err := scanner.Scan(
		&command.ID, &command.CommandKey, &command.SubjectID, &command.RunID, &command.PayloadJSON, &command.PayloadDigest,
		&command.Actor, &command.Reason, &command.CreatedAt,
	); err != nil {
		return ContinuationCommand{}, err
	}
	command.CreatedAt = command.CreatedAt.UTC()
	return command, nil
}

func scanRepairSession(scanner rowScanner) (RepairSession, error) {
	var session RepairSession
	if err := scanner.Scan(
		&session.ID, &session.CommandID, &session.SubjectID, &session.BaseRevisionID, &session.MaxRounds, &session.Status,
		&session.FindingsJSON, &session.PolicyJSON, &session.IdempotencyKey, &session.CreatedBy, &session.CreatedAt,
		&session.UpdatedAt, &session.Version,
	); err != nil {
		return RepairSession{}, err
	}
	session.CreatedAt = session.CreatedAt.UTC()
	session.UpdatedAt = session.UpdatedAt.UTC()
	return session, nil
}

func scanPreparedChange(scanner rowScanner) (PreparedChange, error) {
	var change PreparedChange
	var repairSessionID sql.NullString
	if err := scanner.Scan(
		&change.ID, &change.CommandID, &repairSessionID, &change.RoundOrdinal, &change.ProviderID,
		&change.OperationKey, &change.PayloadJSON, &change.ObservedChangesJSON, &change.BeforeDigest,
		&change.AfterDigest, &change.CreatedBy, &change.CreatedAt,
	); err != nil {
		return PreparedChange{}, err
	}
	change.RepairSessionID = nullableStringValue(repairSessionID)
	change.CreatedAt = change.CreatedAt.UTC()
	return change, nil
}

func scanMutationReceipt(scanner rowScanner) (MutationReceipt, error) {
	var receipt MutationReceipt
	var supersedesID sql.NullString
	if err := scanner.Scan(
		&receipt.ID, &receipt.PreparedChangeID, &receipt.OperationKey, &receipt.Outcome, &receipt.ReceiptJSON,
		&receipt.ReceiptDigest, &supersedesID, &receipt.IdempotencyKey, &receipt.CreatedBy,
		&receipt.CreatedAt,
	); err != nil {
		return MutationReceipt{}, err
	}
	receipt.SupersedesReceiptID = nullableStringValue(supersedesID)
	receipt.CreatedAt = receipt.CreatedAt.UTC()
	return receipt, nil
}

func scanFrozenPlan(scanner rowScanner) (FrozenPlan, error) {
	var plan FrozenPlan
	var preparedChangeID sql.NullString
	if err := scanner.Scan(
		&plan.ID, &plan.CommandID, &preparedChangeID, &plan.SubjectID, &plan.SubjectRevisionID,
		&plan.SubjectDigest, &plan.WorkflowFingerprint, &plan.PlanFingerprint, &plan.PayloadJSON,
		&plan.PayloadDigest, &plan.ExpiresAt, &plan.CreatedBy, &plan.CreatedAt,
	); err != nil {
		return FrozenPlan{}, err
	}
	plan.PreparedChangeID = nullableStringValue(preparedChangeID)
	plan.ExpiresAt = plan.ExpiresAt.UTC()
	plan.CreatedAt = plan.CreatedAt.UTC()
	return plan, nil
}

func scanContinuationExecution(scanner rowScanner) (ContinuationExecution, error) {
	var execution ContinuationExecution
	var parentID, runID sql.NullString
	var finishedAt sql.NullTime
	if err := scanner.Scan(
		&execution.ID, &execution.PlanID, &parentID, &runID, &execution.IdempotencyKey, &execution.State,
		&execution.PayloadJSON, &execution.CreatedBy, &execution.CreatedAt, &execution.UpdatedAt, &finishedAt, &execution.Version,
	); err != nil {
		return ContinuationExecution{}, err
	}
	execution.ParentExecutionID = nullableStringValue(parentID)
	execution.RunID = nullableStringValue(runID)
	execution.CreatedAt = execution.CreatedAt.UTC()
	execution.UpdatedAt = execution.UpdatedAt.UTC()
	execution.FinishedAt = nullableTimePtr(finishedAt)
	return execution, nil
}

func sameContinuationCommand(left, right ContinuationCommand) bool {
	return left.SubjectID == right.SubjectID && left.RunID == right.RunID && left.PayloadJSON == right.PayloadJSON && left.PayloadDigest == right.PayloadDigest &&
		left.Actor == right.Actor && left.Reason == right.Reason
}

func sameRepairSession(left, right RepairSession) bool {
	return left.CommandID == right.CommandID && left.SubjectID == right.SubjectID && left.BaseRevisionID == right.BaseRevisionID &&
		left.MaxRounds == right.MaxRounds && left.FindingsJSON == right.FindingsJSON && left.PolicyJSON == right.PolicyJSON
}

func samePreparedChange(left, right PreparedChange) bool {
	return left.CommandID == right.CommandID && left.RepairSessionID == right.RepairSessionID && left.RoundOrdinal == right.RoundOrdinal &&
		left.ProviderID == right.ProviderID && left.PayloadJSON == right.PayloadJSON && left.ObservedChangesJSON == right.ObservedChangesJSON &&
		left.BeforeDigest == right.BeforeDigest && left.AfterDigest == right.AfterDigest
}

func sameMutationReceipt(left, right MutationReceipt) bool {
	return left.PreparedChangeID == right.PreparedChangeID && left.OperationKey == right.OperationKey && left.Outcome == right.Outcome &&
		left.ReceiptJSON == right.ReceiptJSON && left.ReceiptDigest == right.ReceiptDigest && left.SupersedesReceiptID == right.SupersedesReceiptID
}

func sameFrozenPlan(left, right FrozenPlan) bool {
	return left.CommandID == right.CommandID && left.PreparedChangeID == right.PreparedChangeID && left.SubjectID == right.SubjectID &&
		left.SubjectRevisionID == right.SubjectRevisionID && left.SubjectDigest == right.SubjectDigest &&
		left.WorkflowFingerprint == right.WorkflowFingerprint && left.PlanFingerprint == right.PlanFingerprint &&
		left.PayloadJSON == right.PayloadJSON && left.PayloadDigest == right.PayloadDigest && left.ExpiresAt.Equal(right.ExpiresAt)
}

func sameContinuationExecution(left, right ContinuationExecution) bool {
	return left.PlanID == right.PlanID && left.ParentExecutionID == right.ParentExecutionID && left.RunID == right.RunID && left.PayloadJSON == right.PayloadJSON
}
