package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

const runAttemptSelect = `
	SELECT id, run_id, ordinal, trigger, resume_from, status, created_at, started_at, finished_at, version
	FROM run_attempts`

func (s *Store) CreateRunAttempt(ctx context.Context, request CreateRunAttemptRequest) (RunAttempt, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return RunAttempt{}, err
	}
	if !isUUIDv7(request.RunID) {
		return RunAttempt{}, ErrInvalidUUIDv7Identity
	}
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return RunAttempt{}, err
	}
	if request.Ordinal <= 0 {
		return RunAttempt{}, fmt.Errorf("run attempt ordinal must be positive")
	}
	trigger, err := normalizeRequired(request.Trigger, "run attempt trigger")
	if err != nil {
		return RunAttempt{}, err
	}
	now := s.now().UTC()
	attempt := RunAttempt{
		ID:         id,
		RunID:      request.RunID,
		Ordinal:    request.Ordinal,
		Trigger:    trigger,
		ResumeFrom: strings.TrimSpace(request.ResumeFrom),
		Status:     RunAttemptQueued,
		CreatedAt:  now,
		Version:    1,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RunAttempt{}, err
	}
	defer tx.Rollback()
	if _, err := getWorkflowRunTx(ctx, tx, attempt.RunID); err != nil {
		return RunAttempt{}, err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO run_attempts (id, run_id, ordinal, trigger, resume_from, status, created_at, started_at, finished_at, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?)
	`, attempt.ID, attempt.RunID, attempt.Ordinal, attempt.Trigger, attempt.ResumeFrom, attempt.Status, attempt.CreatedAt, attempt.Version)
	if err != nil {
		if isUniqueConstraint(err) {
			return RunAttempt{}, fmt.Errorf("%w: run attempt %s or ordinal %d", ErrIdentityCollision, attempt.ID, attempt.Ordinal)
		}
		return RunAttempt{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       request.Actor,
		EntityType:  "run_attempt",
		EntityID:    attempt.ID,
		Action:      "run_attempt.created",
		Reason:      request.Reason,
		PayloadJSON: auditPayload(map[string]any{"run_id": attempt.RunID, "ordinal": attempt.Ordinal, "trigger": attempt.Trigger, "resume_from": attempt.ResumeFrom}),
		CreatedAt:   now,
	}); err != nil {
		return RunAttempt{}, err
	}
	if err := tx.Commit(); err != nil {
		return RunAttempt{}, err
	}
	return attempt, nil
}

func (s *Store) GetRunAttempt(ctx context.Context, runAttemptID string) (*RunAttempt, error) {
	if !isUUIDv7(runAttemptID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	attempt, err := scanRunAttempt(s.db.QueryRowContext(ctx, runAttemptSelect+" WHERE id = ?", runAttemptID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &attempt, nil
}

func (s *Store) ListRunAttempts(ctx context.Context, runID string) ([]RunAttempt, error) {
	if !isUUIDv7(runID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	rows, err := s.db.QueryContext(ctx, runAttemptSelect+" WHERE run_id = ? ORDER BY ordinal ASC", runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var attempts []RunAttempt
	for rows.Next() {
		attempt, err := scanRunAttempt(rows)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, attempt)
	}
	return attempts, rows.Err()
}

func (s *Store) TransitionRunAttempt(ctx context.Context, request TransitionRunAttemptRequest) (RunAttempt, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return RunAttempt{}, err
	}
	if !isUUIDv7(request.RunAttemptID) {
		return RunAttempt{}, ErrInvalidUUIDv7Identity
	}
	if request.ExpectedVersion <= 0 {
		return RunAttempt{}, fmt.Errorf("expected run attempt version must be positive")
	}
	if !validRunAttemptStatus(request.Status) {
		return RunAttempt{}, fmt.Errorf("invalid run attempt status %q", request.Status)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RunAttempt{}, err
	}
	defer tx.Rollback()
	attempt, err := getRunAttemptTx(ctx, tx, request.RunAttemptID)
	if err != nil {
		return RunAttempt{}, err
	}
	if attempt.Version != request.ExpectedVersion {
		return RunAttempt{}, fmt.Errorf("%w: run attempt %s", ErrOptimisticLock, attempt.ID)
	}
	if !validRunAttemptTransition(attempt.Status, request.Status) {
		return RunAttempt{}, fmt.Errorf("%w: run attempt %s from %s to %s", ErrInvalidTransition, attempt.ID, attempt.Status, request.Status)
	}
	now := s.now().UTC()
	attempt.Status = request.Status
	if attempt.Status == RunAttemptRunning && attempt.StartedAt == nil {
		attempt.StartedAt = &now
	}
	if isTerminalRunAttemptStatus(attempt.Status) {
		attempt.FinishedAt = &now
	}
	attempt.Version++
	result, err := tx.ExecContext(ctx, `
		UPDATE run_attempts SET status = ?, started_at = ?, finished_at = ?, version = ?
		WHERE id = ? AND version = ?
	`, attempt.Status, attempt.StartedAt, attempt.FinishedAt, attempt.Version, attempt.ID, request.ExpectedVersion)
	if err != nil {
		return RunAttempt{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return RunAttempt{}, err
	}
	if changed != 1 {
		return RunAttempt{}, fmt.Errorf("%w: run attempt %s", ErrOptimisticLock, attempt.ID)
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       request.Actor,
		EntityType:  "run_attempt",
		EntityID:    attempt.ID,
		Action:      "run_attempt.transitioned",
		Reason:      request.Reason,
		PayloadJSON: auditPayload(map[string]any{"status": attempt.Status, "version": attempt.Version}),
		CreatedAt:   now,
	}); err != nil {
		return RunAttempt{}, err
	}
	if err := tx.Commit(); err != nil {
		return RunAttempt{}, err
	}
	return attempt, nil
}

const nodeAttemptSelect = `
	SELECT id, stage_attempt_id, node_id, generation, attempt, status, idempotency_key,
	       error_text, created_at, started_at, finished_at, version
	FROM node_attempts`

func (s *Store) CreateNodeAttempt(ctx context.Context, request CreateNodeAttemptRequest) (NodeAttempt, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return NodeAttempt{}, err
	}
	if !isUUIDv7(request.StageAttemptID) {
		return NodeAttempt{}, ErrInvalidUUIDv7Identity
	}
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return NodeAttempt{}, err
	}
	nodeID, err := normalizeRequired(request.NodeID, "node ID")
	if err != nil {
		return NodeAttempt{}, err
	}
	if request.Generation < 0 || request.Attempt <= 0 {
		return NodeAttempt{}, fmt.Errorf("node generation must be non-negative and attempt must be positive")
	}
	now := s.now().UTC()
	attempt := NodeAttempt{
		ID:             id,
		StageAttemptID: request.StageAttemptID,
		NodeID:         nodeID,
		Generation:     request.Generation,
		Attempt:        request.Attempt,
		Status:         NodeAttemptQueued,
		IdempotencyKey: strings.TrimSpace(request.IdempotencyKey),
		CreatedAt:      now,
		Version:        1,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return NodeAttempt{}, err
	}
	defer tx.Rollback()
	if _, err := getStageAttemptTx(ctx, tx, attempt.StageAttemptID); err != nil {
		return NodeAttempt{}, err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO node_attempts (
			id, stage_attempt_id, node_id, generation, attempt, status, idempotency_key,
			started_at, finished_at, error_text, created_at, version
		) VALUES (?, ?, ?, ?, ?, ?, ?, NULL, NULL, '', ?, ?)
	`, attempt.ID, attempt.StageAttemptID, attempt.NodeID, attempt.Generation, attempt.Attempt, attempt.Status,
		attempt.IdempotencyKey, attempt.CreatedAt, attempt.Version)
	if err != nil {
		if isUniqueConstraint(err) {
			return NodeAttempt{}, fmt.Errorf("%w: node attempt %s", ErrIdentityCollision, attempt.ID)
		}
		return NodeAttempt{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       request.Actor,
		EntityType:  "node_attempt",
		EntityID:    attempt.ID,
		Action:      "node_attempt.created",
		Reason:      request.Reason,
		PayloadJSON: auditPayload(map[string]any{"stage_attempt_id": attempt.StageAttemptID, "node_id": attempt.NodeID, "generation": attempt.Generation, "attempt": attempt.Attempt, "idempotency_key": attempt.IdempotencyKey}),
		CreatedAt:   now,
	}); err != nil {
		return NodeAttempt{}, err
	}
	if err := tx.Commit(); err != nil {
		return NodeAttempt{}, err
	}
	return attempt, nil
}

func (s *Store) GetNodeAttempt(ctx context.Context, nodeAttemptID string) (*NodeAttempt, error) {
	if !isUUIDv7(nodeAttemptID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	attempt, err := scanNodeAttempt(s.db.QueryRowContext(ctx, nodeAttemptSelect+" WHERE id = ?", nodeAttemptID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &attempt, nil
}

func (s *Store) ListNodeAttempts(ctx context.Context, stageAttemptID string) ([]NodeAttempt, error) {
	if !isUUIDv7(stageAttemptID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	rows, err := s.db.QueryContext(ctx, nodeAttemptSelect+" WHERE stage_attempt_id = ? ORDER BY node_id, generation, attempt", stageAttemptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var attempts []NodeAttempt
	for rows.Next() {
		attempt, err := scanNodeAttempt(rows)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, attempt)
	}
	return attempts, rows.Err()
}

func (s *Store) TransitionNodeAttempt(ctx context.Context, request TransitionNodeAttemptRequest) (NodeAttempt, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return NodeAttempt{}, err
	}
	if !isUUIDv7(request.NodeAttemptID) {
		return NodeAttempt{}, ErrInvalidUUIDv7Identity
	}
	if request.ExpectedVersion <= 0 {
		return NodeAttempt{}, fmt.Errorf("expected node attempt version must be positive")
	}
	if !validNodeAttemptStatus(request.Status) {
		return NodeAttempt{}, fmt.Errorf("invalid node attempt status %q", request.Status)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return NodeAttempt{}, err
	}
	defer tx.Rollback()
	attempt, err := getNodeAttemptTx(ctx, tx, request.NodeAttemptID)
	if err != nil {
		return NodeAttempt{}, err
	}
	if attempt.Version != request.ExpectedVersion {
		return NodeAttempt{}, fmt.Errorf("%w: node attempt %s", ErrOptimisticLock, attempt.ID)
	}
	if !validNodeAttemptTransition(attempt.Status, request.Status) {
		return NodeAttempt{}, fmt.Errorf("%w: node attempt %s from %s to %s", ErrInvalidTransition, attempt.ID, attempt.Status, request.Status)
	}
	now := s.now().UTC()
	attempt.Status = request.Status
	if value := strings.TrimSpace(request.ErrorText); value != "" {
		attempt.ErrorText = value
	}
	if (attempt.Status == NodeAttemptRunning || attempt.Status == NodeAttemptWaiting) && attempt.StartedAt == nil {
		attempt.StartedAt = &now
	}
	if isTerminalNodeAttemptStatus(attempt.Status) {
		attempt.FinishedAt = &now
	}
	attempt.Version++
	result, err := tx.ExecContext(ctx, `
		UPDATE node_attempts SET status = ?, error_text = ?, started_at = ?, finished_at = ?, version = ?
		WHERE id = ? AND version = ?
	`, attempt.Status, attempt.ErrorText, attempt.StartedAt, attempt.FinishedAt, attempt.Version, attempt.ID, request.ExpectedVersion)
	if err != nil {
		return NodeAttempt{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return NodeAttempt{}, err
	}
	if changed != 1 {
		return NodeAttempt{}, fmt.Errorf("%w: node attempt %s", ErrOptimisticLock, attempt.ID)
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       request.Actor,
		EntityType:  "node_attempt",
		EntityID:    attempt.ID,
		Action:      "node_attempt.transitioned",
		Reason:      request.Reason,
		PayloadJSON: auditPayload(map[string]any{"status": attempt.Status, "version": attempt.Version}),
		CreatedAt:   now,
	}); err != nil {
		return NodeAttempt{}, err
	}
	if err := tx.Commit(); err != nil {
		return NodeAttempt{}, err
	}
	return attempt, nil
}

const turnCheckpointSelect = `
	SELECT id, node_attempt_id, turn, substep, status, input_digest, artifact_id, payload_json,
	       created_at, finished_at, version
	FROM turn_checkpoints`

func (s *Store) CreateTurnCheckpoint(ctx context.Context, request CreateTurnCheckpointRequest) (TurnCheckpoint, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return TurnCheckpoint{}, err
	}
	if !isUUIDv7(request.NodeAttemptID) {
		return TurnCheckpoint{}, ErrInvalidUUIDv7Identity
	}
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return TurnCheckpoint{}, err
	}
	if request.Turn <= 0 {
		return TurnCheckpoint{}, fmt.Errorf("checkpoint turn must be positive")
	}
	substep, err := normalizeRequired(request.Substep, "checkpoint substep")
	if err != nil {
		return TurnCheckpoint{}, err
	}
	inputDigest, err := normalizeRequired(request.InputDigest, "checkpoint input digest")
	if err != nil {
		return TurnCheckpoint{}, err
	}
	payload, err := normalizeJSON(request.PayloadJSON, "checkpoint payload")
	if err != nil {
		return TurnCheckpoint{}, err
	}
	now := s.now().UTC()
	checkpoint := TurnCheckpoint{
		ID:            id,
		NodeAttemptID: request.NodeAttemptID,
		Turn:          request.Turn,
		Substep:       substep,
		Status:        TurnCheckpointStarted,
		InputDigest:   inputDigest,
		ArtifactID:    strings.TrimSpace(request.ArtifactID),
		PayloadJSON:   payload,
		CreatedAt:     now,
		Version:       1,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TurnCheckpoint{}, err
	}
	defer tx.Rollback()
	if _, err := getNodeAttemptTx(ctx, tx, checkpoint.NodeAttemptID); err != nil {
		return TurnCheckpoint{}, err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO turn_checkpoints (
			id, node_attempt_id, turn, substep, status, input_digest, artifact_id, payload_json, created_at, finished_at, version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)
	`, checkpoint.ID, checkpoint.NodeAttemptID, checkpoint.Turn, checkpoint.Substep, checkpoint.Status,
		checkpoint.InputDigest, checkpoint.ArtifactID, checkpoint.PayloadJSON, checkpoint.CreatedAt, checkpoint.Version)
	if err != nil {
		if isUniqueConstraint(err) {
			return TurnCheckpoint{}, fmt.Errorf("%w: checkpoint %s", ErrIdentityCollision, checkpoint.ID)
		}
		return TurnCheckpoint{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       request.Actor,
		EntityType:  "turn_checkpoint",
		EntityID:    checkpoint.ID,
		Action:      "turn_checkpoint.created",
		Reason:      request.Reason,
		PayloadJSON: auditPayload(map[string]any{"node_attempt_id": checkpoint.NodeAttemptID, "turn": checkpoint.Turn, "substep": checkpoint.Substep, "input_digest": checkpoint.InputDigest}),
		CreatedAt:   now,
	}); err != nil {
		return TurnCheckpoint{}, err
	}
	if err := tx.Commit(); err != nil {
		return TurnCheckpoint{}, err
	}
	return checkpoint, nil
}

func (s *Store) GetTurnCheckpoint(ctx context.Context, checkpointID string) (*TurnCheckpoint, error) {
	if !isUUIDv7(checkpointID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	checkpoint, err := scanTurnCheckpoint(s.db.QueryRowContext(ctx, turnCheckpointSelect+" WHERE id = ?", checkpointID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &checkpoint, nil
}

func (s *Store) ListTurnCheckpoints(ctx context.Context, nodeAttemptID string) ([]TurnCheckpoint, error) {
	if !isUUIDv7(nodeAttemptID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	rows, err := s.db.QueryContext(ctx, turnCheckpointSelect+" WHERE node_attempt_id = ? ORDER BY turn ASC, substep ASC", nodeAttemptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var checkpoints []TurnCheckpoint
	for rows.Next() {
		checkpoint, err := scanTurnCheckpoint(rows)
		if err != nil {
			return nil, err
		}
		checkpoints = append(checkpoints, checkpoint)
	}
	return checkpoints, rows.Err()
}

func (s *Store) TransitionTurnCheckpoint(ctx context.Context, request TransitionTurnCheckpointRequest) (TurnCheckpoint, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return TurnCheckpoint{}, err
	}
	if !isUUIDv7(request.CheckpointID) {
		return TurnCheckpoint{}, ErrInvalidUUIDv7Identity
	}
	if request.ExpectedVersion <= 0 {
		return TurnCheckpoint{}, fmt.Errorf("expected checkpoint version must be positive")
	}
	if !validTurnCheckpointStatus(request.Status) {
		return TurnCheckpoint{}, fmt.Errorf("invalid checkpoint status %q", request.Status)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TurnCheckpoint{}, err
	}
	defer tx.Rollback()
	checkpoint, err := getTurnCheckpointTx(ctx, tx, request.CheckpointID)
	if err != nil {
		return TurnCheckpoint{}, err
	}
	if checkpoint.Version != request.ExpectedVersion {
		return TurnCheckpoint{}, fmt.Errorf("%w: checkpoint %s", ErrOptimisticLock, checkpoint.ID)
	}
	if !validTurnCheckpointTransition(checkpoint.Status, request.Status) {
		return TurnCheckpoint{}, fmt.Errorf("%w: checkpoint %s from %s to %s", ErrInvalidTransition, checkpoint.ID, checkpoint.Status, request.Status)
	}
	if value := strings.TrimSpace(request.ArtifactID); value != "" {
		checkpoint.ArtifactID = value
	}
	if strings.TrimSpace(request.PayloadJSON) != "" {
		payload, err := normalizeJSON(request.PayloadJSON, "checkpoint payload")
		if err != nil {
			return TurnCheckpoint{}, err
		}
		checkpoint.PayloadJSON = payload
	}
	now := s.now().UTC()
	checkpoint.Status = request.Status
	checkpoint.FinishedAt = &now
	checkpoint.Version++
	result, err := tx.ExecContext(ctx, `
		UPDATE turn_checkpoints SET status = ?, artifact_id = ?, payload_json = ?, finished_at = ?, version = ?
		WHERE id = ? AND version = ?
	`, checkpoint.Status, checkpoint.ArtifactID, checkpoint.PayloadJSON, checkpoint.FinishedAt, checkpoint.Version,
		checkpoint.ID, request.ExpectedVersion)
	if err != nil {
		return TurnCheckpoint{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return TurnCheckpoint{}, err
	}
	if changed != 1 {
		return TurnCheckpoint{}, fmt.Errorf("%w: checkpoint %s", ErrOptimisticLock, checkpoint.ID)
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       request.Actor,
		EntityType:  "turn_checkpoint",
		EntityID:    checkpoint.ID,
		Action:      "turn_checkpoint.transitioned",
		Reason:      request.Reason,
		PayloadJSON: auditPayload(map[string]any{"status": checkpoint.Status, "version": checkpoint.Version, "artifact_id": checkpoint.ArtifactID}),
		CreatedAt:   now,
	}); err != nil {
		return TurnCheckpoint{}, err
	}
	if err := tx.Commit(); err != nil {
		return TurnCheckpoint{}, err
	}
	return checkpoint, nil
}

func getRunAttemptTx(ctx context.Context, tx *sql.Tx, runAttemptID string) (RunAttempt, error) {
	attempt, err := scanRunAttempt(tx.QueryRowContext(ctx, runAttemptSelect+" WHERE id = ?", runAttemptID))
	if err == sql.ErrNoRows {
		return RunAttempt{}, fmt.Errorf("%w: run attempt %s", ErrNotFound, runAttemptID)
	}
	return attempt, err
}

func getNodeAttemptTx(ctx context.Context, tx *sql.Tx, nodeAttemptID string) (NodeAttempt, error) {
	attempt, err := scanNodeAttempt(tx.QueryRowContext(ctx, nodeAttemptSelect+" WHERE id = ?", nodeAttemptID))
	if err == sql.ErrNoRows {
		return NodeAttempt{}, fmt.Errorf("%w: node attempt %s", ErrNotFound, nodeAttemptID)
	}
	return attempt, err
}

func getTurnCheckpointTx(ctx context.Context, tx *sql.Tx, checkpointID string) (TurnCheckpoint, error) {
	checkpoint, err := scanTurnCheckpoint(tx.QueryRowContext(ctx, turnCheckpointSelect+" WHERE id = ?", checkpointID))
	if err == sql.ErrNoRows {
		return TurnCheckpoint{}, fmt.Errorf("%w: checkpoint %s", ErrNotFound, checkpointID)
	}
	return checkpoint, err
}

func scanRunAttempt(scanner rowScanner) (RunAttempt, error) {
	var attempt RunAttempt
	var startedAt, finishedAt sql.NullTime
	if err := scanner.Scan(&attempt.ID, &attempt.RunID, &attempt.Ordinal, &attempt.Trigger, &attempt.ResumeFrom, &attempt.Status, &attempt.CreatedAt, &startedAt, &finishedAt, &attempt.Version); err != nil {
		return RunAttempt{}, err
	}
	attempt.CreatedAt = attempt.CreatedAt.UTC()
	attempt.StartedAt = nullableTimePtr(startedAt)
	attempt.FinishedAt = nullableTimePtr(finishedAt)
	return attempt, nil
}

func scanNodeAttempt(scanner rowScanner) (NodeAttempt, error) {
	var attempt NodeAttempt
	var createdAt, startedAt, finishedAt sql.NullTime
	if err := scanner.Scan(&attempt.ID, &attempt.StageAttemptID, &attempt.NodeID, &attempt.Generation, &attempt.Attempt, &attempt.Status, &attempt.IdempotencyKey, &attempt.ErrorText, &createdAt, &startedAt, &finishedAt, &attempt.Version); err != nil {
		return NodeAttempt{}, err
	}
	attempt.CreatedAt = nullableTimeValue(createdAt)
	attempt.StartedAt = nullableTimePtr(startedAt)
	attempt.FinishedAt = nullableTimePtr(finishedAt)
	return attempt, nil
}

func scanTurnCheckpoint(scanner rowScanner) (TurnCheckpoint, error) {
	var checkpoint TurnCheckpoint
	var finishedAt sql.NullTime
	if err := scanner.Scan(&checkpoint.ID, &checkpoint.NodeAttemptID, &checkpoint.Turn, &checkpoint.Substep, &checkpoint.Status, &checkpoint.InputDigest, &checkpoint.ArtifactID, &checkpoint.PayloadJSON, &checkpoint.CreatedAt, &finishedAt, &checkpoint.Version); err != nil {
		return TurnCheckpoint{}, err
	}
	checkpoint.CreatedAt = checkpoint.CreatedAt.UTC()
	checkpoint.FinishedAt = nullableTimePtr(finishedAt)
	return checkpoint, nil
}

func validRunAttemptStatus(status RunAttemptStatus) bool {
	switch status {
	case RunAttemptQueued, RunAttemptRunning, RunAttemptSucceeded, RunAttemptFailed, RunAttemptCanceled, RunAttemptInterrupted:
		return true
	default:
		return false
	}
}

func validRunAttemptTransition(from, to RunAttemptStatus) bool {
	if from == to || isTerminalRunAttemptStatus(from) {
		return false
	}
	switch from {
	case RunAttemptQueued:
		return to == RunAttemptRunning || to == RunAttemptCanceled
	case RunAttemptRunning:
		return to == RunAttemptSucceeded || to == RunAttemptFailed || to == RunAttemptCanceled || to == RunAttemptInterrupted
	default:
		return false
	}
}

func isTerminalRunAttemptStatus(status RunAttemptStatus) bool {
	switch status {
	case RunAttemptSucceeded, RunAttemptFailed, RunAttemptCanceled, RunAttemptInterrupted:
		return true
	default:
		return false
	}
}

func validNodeAttemptStatus(status NodeAttemptStatus) bool {
	switch status {
	case NodeAttemptQueued, NodeAttemptRunning, NodeAttemptWaiting, NodeAttemptCompleted, NodeAttemptInfraFailed, NodeAttemptInterrupted, NodeAttemptInDoubt, NodeAttemptCanceled:
		return true
	default:
		return false
	}
}

func validNodeAttemptTransition(from, to NodeAttemptStatus) bool {
	if from == to || isTerminalNodeAttemptStatus(from) {
		return false
	}
	switch from {
	case NodeAttemptQueued:
		return to == NodeAttemptRunning || to == NodeAttemptWaiting || to == NodeAttemptCanceled
	case NodeAttemptRunning:
		return to == NodeAttemptWaiting || to == NodeAttemptCompleted || to == NodeAttemptInfraFailed || to == NodeAttemptInterrupted || to == NodeAttemptInDoubt || to == NodeAttemptCanceled
	case NodeAttemptWaiting:
		return to == NodeAttemptRunning || to == NodeAttemptCompleted || to == NodeAttemptInfraFailed || to == NodeAttemptInterrupted || to == NodeAttemptInDoubt || to == NodeAttemptCanceled
	case NodeAttemptInDoubt:
		return to == NodeAttemptCompleted || to == NodeAttemptInfraFailed || to == NodeAttemptInterrupted || to == NodeAttemptCanceled
	default:
		return false
	}
}

func isTerminalNodeAttemptStatus(status NodeAttemptStatus) bool {
	switch status {
	case NodeAttemptCompleted, NodeAttemptInfraFailed, NodeAttemptInterrupted, NodeAttemptCanceled:
		return true
	default:
		return false
	}
}

func validTurnCheckpointStatus(status TurnCheckpointStatus) bool {
	switch status {
	case TurnCheckpointStarted, TurnCheckpointCompleted, TurnCheckpointInterrupted, TurnCheckpointFailed:
		return true
	default:
		return false
	}
}

func validTurnCheckpointTransition(from, to TurnCheckpointStatus) bool {
	return from == TurnCheckpointStarted && (to == TurnCheckpointCompleted || to == TurnCheckpointInterrupted || to == TurnCheckpointFailed)
}
