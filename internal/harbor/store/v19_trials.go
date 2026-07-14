package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const trialExecutionSelect = `
	SELECT id, run_id, stage_attempt_id, stage_key, ordinal, status,
	       created_at, updated_at, started_at, finished_at, version
	FROM trial_executions_v19`

const trialAttemptSelect = `
	SELECT id, trial_execution_id, retry_of_trial_attempt_id, ordinal, status,
	       error_text, failure_class, created_at, updated_at, started_at, finished_at, version
	FROM trial_attempts_v19`

// CreateTrialExecution persists one logical evaluator sample. Its coordinate
// is immutable and unique, so a caller cannot create a replacement sample for
// the same Run/StageAttempt/stage/ordinal after a technical failure.
func (s *Store) CreateTrialExecution(ctx context.Context, request CreateTrialExecutionRequest) (TrialExecution, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return TrialExecution{}, err
	}
	if !isUUIDv7(request.RunID) || !isUUIDv7(request.StageAttemptID) {
		return TrialExecution{}, ErrInvalidUUIDv7Identity
	}
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return TrialExecution{}, err
	}
	stageKey, err := normalizeRequired(request.StageKey, "trial execution stage key")
	if err != nil {
		return TrialExecution{}, err
	}
	if request.Ordinal <= 0 {
		return TrialExecution{}, fmt.Errorf("trial execution ordinal must be positive")
	}
	now := s.now().UTC()
	execution := TrialExecution{
		ID:             id,
		RunID:          request.RunID,
		StageAttemptID: request.StageAttemptID,
		StageKey:       stageKey,
		Ordinal:        request.Ordinal,
		Status:         TrialExecutionQueued,
		CreatedAt:      now,
		UpdatedAt:      now,
		Version:        1,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TrialExecution{}, err
	}
	defer tx.Rollback()
	if _, err := getWorkflowRunTx(ctx, tx, execution.RunID); err != nil {
		return TrialExecution{}, err
	}
	stage, err := getStageAttemptTx(ctx, tx, execution.StageAttemptID)
	if err != nil {
		return TrialExecution{}, err
	}
	if stage.RunID != execution.RunID || stage.StageKey != execution.StageKey {
		return TrialExecution{}, fmt.Errorf("%w: trial execution does not match Run/StageAttempt binding", ErrInvalidTransition)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO trial_executions_v19 (
			id, run_id, stage_attempt_id, stage_key, ordinal, status,
			created_at, updated_at, started_at, finished_at, version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?)
	`, execution.ID, execution.RunID, execution.StageAttemptID, execution.StageKey, execution.Ordinal, execution.Status,
		execution.CreatedAt, execution.UpdatedAt, execution.Version)
	if err != nil {
		if isGlobalIdentityCollision(err) || isUniqueConstraint(err) {
			return TrialExecution{}, fmt.Errorf("%w: trial execution %s", ErrIdentityCollision, execution.ID)
		}
		return TrialExecution{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       request.Actor,
		EntityType:  "trial_execution",
		EntityID:    execution.ID,
		Action:      "trial_execution.created",
		Reason:      request.Reason,
		PayloadJSON: auditPayload(map[string]any{"run_id": execution.RunID, "stage_attempt_id": execution.StageAttemptID, "stage_key": execution.StageKey, "ordinal": execution.Ordinal}),
		CreatedAt:   now,
	}); err != nil {
		return TrialExecution{}, err
	}
	if err := tx.Commit(); err != nil {
		return TrialExecution{}, err
	}
	return execution, nil
}

func (s *Store) GetTrialExecution(ctx context.Context, trialExecutionID string) (*TrialExecution, error) {
	if !isUUIDv7(trialExecutionID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	execution, err := scanTrialExecution(s.db.QueryRowContext(ctx, trialExecutionSelect+" WHERE id = ?", trialExecutionID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &execution, nil
}

// GetTrialExecutionForStageAttempt looks up the one logical sample at ordinal
// for a known StageAttempt. The StageAttempt itself fixes the stage key and
// Run binding, so callers cannot substitute a different logical coordinate.
func (s *Store) GetTrialExecutionForStageAttempt(ctx context.Context, stageAttemptID string, ordinal int) (*TrialExecution, error) {
	if !isUUIDv7(stageAttemptID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	if ordinal <= 0 {
		return nil, fmt.Errorf("trial execution ordinal must be positive")
	}
	execution, err := scanTrialExecution(s.db.QueryRowContext(ctx, trialExecutionSelect+" WHERE stage_attempt_id = ? AND ordinal = ?", stageAttemptID, ordinal))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &execution, nil
}

func (s *Store) ListTrialExecutionsForRun(ctx context.Context, runID string) ([]TrialExecution, error) {
	if !isUUIDv7(runID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	rows, err := s.db.QueryContext(ctx, trialExecutionSelect+" WHERE run_id = ? ORDER BY stage_attempt_id ASC, ordinal ASC, id ASC", runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTrialExecutions(rows)
}

func (s *Store) ListTrialExecutionsForStageAttempt(ctx context.Context, stageAttemptID string) ([]TrialExecution, error) {
	if !isUUIDv7(stageAttemptID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	rows, err := s.db.QueryContext(ctx, trialExecutionSelect+" WHERE stage_attempt_id = ? ORDER BY ordinal ASC, id ASC", stageAttemptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTrialExecutions(rows)
}

// TransitionTrialExecution records the final policy disposition of a logical
// sample. Technical attempt outcomes advance the parent only through
// TransitionTrialAttempt; they never make the parent terminal themselves.
func (s *Store) TransitionTrialExecution(ctx context.Context, request TransitionTrialExecutionRequest) (TrialExecution, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return TrialExecution{}, err
	}
	if !isUUIDv7(request.TrialExecutionID) {
		return TrialExecution{}, ErrInvalidUUIDv7Identity
	}
	if request.ExpectedVersion <= 0 {
		return TrialExecution{}, fmt.Errorf("expected trial execution version must be positive")
	}
	if !validTrialExecutionStatus(request.Status) {
		return TrialExecution{}, fmt.Errorf("invalid trial execution status %q", request.Status)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TrialExecution{}, err
	}
	defer tx.Rollback()
	execution, err := getTrialExecutionTx(ctx, tx, request.TrialExecutionID)
	if err != nil {
		return TrialExecution{}, err
	}
	if execution.Version != request.ExpectedVersion {
		return TrialExecution{}, fmt.Errorf("%w: trial execution %s", ErrOptimisticLock, execution.ID)
	}
	if !validTrialExecutionTransition(execution.Status, request.Status) {
		return TrialExecution{}, fmt.Errorf("%w: trial execution %s from %s to %s", ErrInvalidTransition, execution.ID, execution.Status, request.Status)
	}
	if isTerminalTrialExecutionStatus(request.Status) {
		active, err := activeTrialAttemptTx(ctx, tx, execution.ID)
		if err != nil {
			return TrialExecution{}, err
		}
		if active != nil {
			return TrialExecution{}, fmt.Errorf("%w: trial execution %s has active attempt %s", ErrInvalidTransition, execution.ID, active.ID)
		}
	}
	now := s.now().UTC()
	execution.Status = request.Status
	if (execution.Status == TrialExecutionRunning || execution.Status == TrialExecutionWaiting) && execution.StartedAt == nil {
		execution.StartedAt = &now
	}
	if isTerminalTrialExecutionStatus(execution.Status) {
		execution.FinishedAt = &now
	}
	execution.UpdatedAt = now
	execution.Version++
	result, err := tx.ExecContext(ctx, `
		UPDATE trial_executions_v19
		SET status = ?, updated_at = ?, started_at = ?, finished_at = ?, version = ?
		WHERE id = ? AND version = ?
	`, execution.Status, execution.UpdatedAt, execution.StartedAt, execution.FinishedAt, execution.Version, execution.ID, request.ExpectedVersion)
	if err != nil {
		return TrialExecution{}, err
	}
	if err := requireOneRow(result, "trial execution", execution.ID); err != nil {
		return TrialExecution{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       request.Actor,
		EntityType:  "trial_execution",
		EntityID:    execution.ID,
		Action:      "trial_execution.transitioned",
		Reason:      request.Reason,
		PayloadJSON: auditPayload(map[string]any{"status": execution.Status, "version": execution.Version}),
		CreatedAt:   now,
	}); err != nil {
		return TrialExecution{}, err
	}
	if err := tx.Commit(); err != nil {
		return TrialExecution{}, err
	}
	return execution, nil
}

// CreateTrialAttempt appends a technical attempt under an existing logical
// TrialExecution. It deliberately has no Run/Stage/ordinal-for-sample fields:
// those are immutable on the parent, so this API cannot allocate a new sample
// as a side effect of retrying.
func (s *Store) CreateTrialAttempt(ctx context.Context, request CreateTrialAttemptRequest) (TrialAttempt, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return TrialAttempt{}, err
	}
	if !isUUIDv7(request.TrialExecutionID) {
		return TrialAttempt{}, ErrInvalidUUIDv7Identity
	}
	if request.RetryOfTrialAttemptID != "" && !isUUIDv7(request.RetryOfTrialAttemptID) {
		return TrialAttempt{}, ErrInvalidUUIDv7Identity
	}
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return TrialAttempt{}, err
	}
	if request.Ordinal <= 0 {
		return TrialAttempt{}, fmt.Errorf("trial attempt ordinal must be positive")
	}
	now := s.now().UTC()
	attempt := TrialAttempt{
		ID:                    id,
		TrialExecutionID:      request.TrialExecutionID,
		RetryOfTrialAttemptID: strings.TrimSpace(request.RetryOfTrialAttemptID),
		Ordinal:               request.Ordinal,
		Status:                TrialAttemptQueued,
		CreatedAt:             now,
		UpdatedAt:             now,
		Version:               1,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TrialAttempt{}, err
	}
	defer tx.Rollback()
	execution, err := getTrialExecutionTx(ctx, tx, attempt.TrialExecutionID)
	if err != nil {
		return TrialAttempt{}, err
	}
	if !trialExecutionAcceptsAttempt(execution.Status) {
		return TrialAttempt{}, fmt.Errorf("%w: trial execution %s is %s", ErrInvalidTransition, execution.ID, execution.Status)
	}
	if attempt.Ordinal > 1 && execution.Status != TrialExecutionRunning {
		return TrialAttempt{}, fmt.Errorf("%w: retry trial attempt requires running trial execution %s", ErrInvalidTransition, execution.ID)
	}
	if active, err := activeTrialAttemptTx(ctx, tx, attempt.TrialExecutionID); err != nil {
		return TrialAttempt{}, err
	} else if active != nil {
		return TrialAttempt{}, fmt.Errorf("%w: trial execution %s already has active attempt %s", ErrInvalidTransition, execution.ID, active.ID)
	}
	if err := validateTrialAttemptLineageTx(ctx, tx, attempt); err != nil {
		return TrialAttempt{}, err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO trial_attempts_v19 (
			id, trial_execution_id, retry_of_trial_attempt_id, ordinal, status,
			error_text, failure_class, created_at, updated_at, started_at, finished_at, version
		) VALUES (?, ?, ?, ?, ?, '', '', ?, ?, NULL, NULL, ?)
	`, attempt.ID, attempt.TrialExecutionID, nullableString(attempt.RetryOfTrialAttemptID), attempt.Ordinal, attempt.Status,
		attempt.CreatedAt, attempt.UpdatedAt, attempt.Version)
	if err != nil {
		if isGlobalIdentityCollision(err) || isUniqueConstraint(err) {
			return TrialAttempt{}, fmt.Errorf("%w: trial attempt %s", ErrIdentityCollision, attempt.ID)
		}
		return TrialAttempt{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       request.Actor,
		EntityType:  "trial_attempt",
		EntityID:    attempt.ID,
		Action:      "trial_attempt.created",
		Reason:      request.Reason,
		PayloadJSON: auditPayload(map[string]any{"trial_execution_id": attempt.TrialExecutionID, "retry_of_trial_attempt_id": attempt.RetryOfTrialAttemptID, "ordinal": attempt.Ordinal}),
		CreatedAt:   now,
	}); err != nil {
		return TrialAttempt{}, err
	}
	if err := tx.Commit(); err != nil {
		return TrialAttempt{}, err
	}
	return attempt, nil
}

func (s *Store) GetTrialAttempt(ctx context.Context, trialAttemptID string) (*TrialAttempt, error) {
	if !isUUIDv7(trialAttemptID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	attempt, err := scanTrialAttempt(s.db.QueryRowContext(ctx, trialAttemptSelect+" WHERE id = ?", trialAttemptID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &attempt, nil
}

func (s *Store) ListTrialAttemptsForTrialExecution(ctx context.Context, trialExecutionID string) ([]TrialAttempt, error) {
	if !isUUIDv7(trialExecutionID) {
		return nil, ErrInvalidUUIDv7Identity
	}
	rows, err := s.db.QueryContext(ctx, trialAttemptSelect+" WHERE trial_execution_id = ? ORDER BY ordinal ASC, id ASC", trialExecutionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTrialAttempts(rows)
}

func (s *Store) TransitionTrialAttempt(ctx context.Context, request TransitionTrialAttemptRequest) (TrialAttempt, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return TrialAttempt{}, err
	}
	if !isUUIDv7(request.TrialAttemptID) {
		return TrialAttempt{}, ErrInvalidUUIDv7Identity
	}
	if request.ExpectedVersion <= 0 {
		return TrialAttempt{}, fmt.Errorf("expected trial attempt version must be positive")
	}
	if !validTrialAttemptStatus(request.Status) {
		return TrialAttempt{}, fmt.Errorf("invalid trial attempt status %q", request.Status)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TrialAttempt{}, err
	}
	defer tx.Rollback()
	attempt, err := getTrialAttemptTx(ctx, tx, request.TrialAttemptID)
	if err != nil {
		return TrialAttempt{}, err
	}
	if attempt.Version != request.ExpectedVersion {
		return TrialAttempt{}, fmt.Errorf("%w: trial attempt %s", ErrOptimisticLock, attempt.ID)
	}
	if !validTrialAttemptTransition(attempt.Status, request.Status) {
		return TrialAttempt{}, fmt.Errorf("%w: trial attempt %s from %s to %s", ErrInvalidTransition, attempt.ID, attempt.Status, request.Status)
	}
	execution, err := getTrialExecutionTx(ctx, tx, attempt.TrialExecutionID)
	if err != nil {
		return TrialAttempt{}, err
	}
	if isTerminalTrialExecutionStatus(execution.Status) {
		return TrialAttempt{}, fmt.Errorf("%w: trial execution %s is %s", ErrInvalidTransition, execution.ID, execution.Status)
	}
	if value := strings.TrimSpace(request.ErrorText); value != "" {
		attempt.ErrorText = value
	}
	if value := strings.TrimSpace(request.FailureClass); value != "" {
		attempt.FailureClass = value
	}
	now := s.now().UTC()
	attempt.Status = request.Status
	if (attempt.Status == TrialAttemptRunning || attempt.Status == TrialAttemptWaiting) && attempt.StartedAt == nil {
		attempt.StartedAt = &now
	}
	if isTerminalTrialAttemptStatus(attempt.Status) {
		attempt.FinishedAt = &now
	}
	attempt.UpdatedAt = now
	attempt.Version++
	_, err = s.advanceTrialExecutionForAttemptTx(ctx, tx, execution, attempt, request.Actor, request.Reason, now)
	if err != nil {
		return TrialAttempt{}, err
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE trial_attempts_v19
		SET status = ?, error_text = ?, failure_class = ?, updated_at = ?, started_at = ?, finished_at = ?, version = ?
		WHERE id = ? AND version = ?
	`, attempt.Status, attempt.ErrorText, attempt.FailureClass, attempt.UpdatedAt, attempt.StartedAt, attempt.FinishedAt,
		attempt.Version, attempt.ID, request.ExpectedVersion)
	if err != nil {
		return TrialAttempt{}, err
	}
	if err := requireOneRow(result, "trial attempt", attempt.ID); err != nil {
		return TrialAttempt{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:       request.Actor,
		EntityType:  "trial_attempt",
		EntityID:    attempt.ID,
		Action:      "trial_attempt.transitioned",
		Reason:      request.Reason,
		PayloadJSON: auditPayload(map[string]any{"status": attempt.Status, "version": attempt.Version, "failure_class": attempt.FailureClass}),
		CreatedAt:   now,
	}); err != nil {
		return TrialAttempt{}, err
	}
	if err := tx.Commit(); err != nil {
		return TrialAttempt{}, err
	}
	return attempt, nil
}

// advanceTrialExecutionForAttemptTx keeps the logical sample in a continuable
// state across technical retries. Only an explicit policy transition may make
// the parent terminal; an infra failure or interruption of one technical try
// therefore returns a reconciled parent to running instead of ending the
// logical sample.
func (s *Store) advanceTrialExecutionForAttemptTx(ctx context.Context, tx *sql.Tx, execution TrialExecution, attempt TrialAttempt, actor, reason string, now time.Time) (TrialExecution, error) {
	nextStatus, changed := trialExecutionStatusForAttempt(execution.Status, attempt.Status)
	if !changed {
		return execution, nil
	}
	if !validTrialExecutionTransition(execution.Status, nextStatus) {
		return TrialExecution{}, fmt.Errorf("%w: technical attempt %s cannot advance trial execution %s from %s to %s", ErrInvalidTransition, attempt.ID, execution.ID, execution.Status, nextStatus)
	}
	updated := execution
	updated.Status = nextStatus
	updated.UpdatedAt = now
	if (updated.Status == TrialExecutionRunning || updated.Status == TrialExecutionWaiting) && updated.StartedAt == nil {
		updated.StartedAt = &now
	}
	updated.Version++
	result, err := tx.ExecContext(ctx, `
		UPDATE trial_executions_v19
		SET status = ?, updated_at = ?, started_at = ?, finished_at = ?, version = ?
		WHERE id = ? AND version = ?
	`, updated.Status, updated.UpdatedAt, updated.StartedAt, updated.FinishedAt, updated.Version, updated.ID, execution.Version)
	if err != nil {
		return TrialExecution{}, err
	}
	if err := requireOneRow(result, "trial execution", updated.ID); err != nil {
		return TrialExecution{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor:      actor,
		EntityType: "trial_execution",
		EntityID:   updated.ID,
		Action:     "trial_execution.technical_state_advanced",
		Reason:     reason,
		PayloadJSON: auditPayload(map[string]any{
			"status":           updated.Status,
			"version":          updated.Version,
			"trial_attempt_id": attempt.ID,
		}),
		CreatedAt: now,
	}); err != nil {
		return TrialExecution{}, err
	}
	return updated, nil
}

func trialExecutionStatusForAttempt(current TrialExecutionStatus, attempt TrialAttemptStatus) (TrialExecutionStatus, bool) {
	switch attempt {
	case TrialAttemptRunning:
		return TrialExecutionRunning, current != TrialExecutionRunning
	case TrialAttemptWaiting:
		return TrialExecutionWaiting, current != TrialExecutionWaiting
	case TrialAttemptInDoubt:
		return TrialExecutionInDoubt, current != TrialExecutionInDoubt
	case TrialAttemptReconciling:
		return TrialExecutionReconciling, current != TrialExecutionReconciling
	case TrialAttemptInfraFailed, TrialAttemptInterrupted:
		return TrialExecutionRunning, current != TrialExecutionRunning
	default:
		return current, false
	}
}

func validateTrialAttemptLineageTx(ctx context.Context, tx *sql.Tx, attempt TrialAttempt) error {
	if attempt.Ordinal == 1 {
		if attempt.RetryOfTrialAttemptID != "" {
			return fmt.Errorf("%w: initial trial attempt cannot have a retry predecessor", ErrInvalidTransition)
		}
		return nil
	}
	if attempt.RetryOfTrialAttemptID == "" {
		return fmt.Errorf("%w: retry trial attempt requires its preceding attempt", ErrInvalidTransition)
	}
	predecessor, err := getTrialAttemptTx(ctx, tx, attempt.RetryOfTrialAttemptID)
	if err != nil {
		return err
	}
	if predecessor.TrialExecutionID != attempt.TrialExecutionID || predecessor.Ordinal != attempt.Ordinal-1 || !retryableTrialAttemptStatus(predecessor.Status) {
		return fmt.Errorf("%w: retry trial attempt %s does not follow an eligible predecessor", ErrInvalidTransition, attempt.ID)
	}
	return nil
}

func activeTrialAttemptTx(ctx context.Context, tx *sql.Tx, trialExecutionID string) (*TrialAttempt, error) {
	attempt, err := scanTrialAttempt(tx.QueryRowContext(ctx, trialAttemptSelect+`
		WHERE trial_execution_id = ? AND status IN ('queued', 'running', 'waiting', 'in_doubt', 'reconciling')
		ORDER BY ordinal DESC, id DESC LIMIT 1`, trialExecutionID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &attempt, nil
}

func getTrialExecutionTx(ctx context.Context, tx *sql.Tx, trialExecutionID string) (TrialExecution, error) {
	execution, err := scanTrialExecution(tx.QueryRowContext(ctx, trialExecutionSelect+" WHERE id = ?", trialExecutionID))
	if err == sql.ErrNoRows {
		return TrialExecution{}, fmt.Errorf("%w: trial execution %s", ErrNotFound, trialExecutionID)
	}
	return execution, err
}

func getTrialAttemptTx(ctx context.Context, tx *sql.Tx, trialAttemptID string) (TrialAttempt, error) {
	attempt, err := scanTrialAttempt(tx.QueryRowContext(ctx, trialAttemptSelect+" WHERE id = ?", trialAttemptID))
	if err == sql.ErrNoRows {
		return TrialAttempt{}, fmt.Errorf("%w: trial attempt %s", ErrNotFound, trialAttemptID)
	}
	return attempt, err
}

func scanTrialExecution(scanner rowScanner) (TrialExecution, error) {
	var execution TrialExecution
	var startedAt, finishedAt sql.NullTime
	if err := scanner.Scan(
		&execution.ID, &execution.RunID, &execution.StageAttemptID, &execution.StageKey, &execution.Ordinal, &execution.Status,
		&execution.CreatedAt, &execution.UpdatedAt, &startedAt, &finishedAt, &execution.Version,
	); err != nil {
		return TrialExecution{}, err
	}
	execution.CreatedAt = execution.CreatedAt.UTC()
	execution.UpdatedAt = execution.UpdatedAt.UTC()
	execution.StartedAt = nullableTimePtr(startedAt)
	execution.FinishedAt = nullableTimePtr(finishedAt)
	return execution, nil
}

func scanTrialExecutions(rows *sql.Rows) ([]TrialExecution, error) {
	var executions []TrialExecution
	for rows.Next() {
		execution, err := scanTrialExecution(rows)
		if err != nil {
			return nil, err
		}
		executions = append(executions, execution)
	}
	return executions, rows.Err()
}

func scanTrialAttempt(scanner rowScanner) (TrialAttempt, error) {
	var attempt TrialAttempt
	var retry sql.NullString
	var startedAt, finishedAt sql.NullTime
	if err := scanner.Scan(
		&attempt.ID, &attempt.TrialExecutionID, &retry, &attempt.Ordinal, &attempt.Status,
		&attempt.ErrorText, &attempt.FailureClass, &attempt.CreatedAt, &attempt.UpdatedAt, &startedAt, &finishedAt, &attempt.Version,
	); err != nil {
		return TrialAttempt{}, err
	}
	attempt.RetryOfTrialAttemptID = nullableStringValue(retry)
	attempt.CreatedAt = attempt.CreatedAt.UTC()
	attempt.UpdatedAt = attempt.UpdatedAt.UTC()
	attempt.StartedAt = nullableTimePtr(startedAt)
	attempt.FinishedAt = nullableTimePtr(finishedAt)
	return attempt, nil
}

func scanTrialAttempts(rows *sql.Rows) ([]TrialAttempt, error) {
	var attempts []TrialAttempt
	for rows.Next() {
		attempt, err := scanTrialAttempt(rows)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, attempt)
	}
	return attempts, rows.Err()
}

func validTrialExecutionStatus(status TrialExecutionStatus) bool {
	switch status {
	case TrialExecutionQueued, TrialExecutionRunning, TrialExecutionWaiting, TrialExecutionCompleted, TrialExecutionInfraFailed,
		TrialExecutionInterrupted, TrialExecutionInDoubt, TrialExecutionReconciling, TrialExecutionCanceled:
		return true
	default:
		return false
	}
}

func validTrialExecutionTransition(from, to TrialExecutionStatus) bool {
	if from == to || isTerminalTrialExecutionStatus(from) {
		return false
	}
	switch from {
	case TrialExecutionQueued:
		return to == TrialExecutionRunning || to == TrialExecutionWaiting || to == TrialExecutionCanceled
	case TrialExecutionRunning:
		return to == TrialExecutionWaiting || to == TrialExecutionCompleted || to == TrialExecutionInfraFailed || to == TrialExecutionInterrupted || to == TrialExecutionInDoubt || to == TrialExecutionCanceled
	case TrialExecutionWaiting:
		return to == TrialExecutionRunning || to == TrialExecutionCompleted || to == TrialExecutionInfraFailed || to == TrialExecutionInterrupted || to == TrialExecutionInDoubt || to == TrialExecutionCanceled
	case TrialExecutionInDoubt:
		return to == TrialExecutionReconciling
	case TrialExecutionReconciling:
		return to == TrialExecutionRunning || to == TrialExecutionCompleted || to == TrialExecutionInfraFailed || to == TrialExecutionInterrupted || to == TrialExecutionCanceled
	default:
		return false
	}
}

func isTerminalTrialExecutionStatus(status TrialExecutionStatus) bool {
	switch status {
	case TrialExecutionCompleted, TrialExecutionInfraFailed, TrialExecutionInterrupted, TrialExecutionCanceled:
		return true
	default:
		return false
	}
}

func trialExecutionAcceptsAttempt(status TrialExecutionStatus) bool {
	return status == TrialExecutionQueued || status == TrialExecutionRunning || status == TrialExecutionWaiting
}

func validTrialAttemptStatus(status TrialAttemptStatus) bool {
	switch status {
	case TrialAttemptQueued, TrialAttemptRunning, TrialAttemptWaiting, TrialAttemptCompleted, TrialAttemptInfraFailed,
		TrialAttemptInterrupted, TrialAttemptInDoubt, TrialAttemptReconciling, TrialAttemptCanceled:
		return true
	default:
		return false
	}
}

func validTrialAttemptTransition(from, to TrialAttemptStatus) bool {
	if from == to || isTerminalTrialAttemptStatus(from) {
		return false
	}
	switch from {
	case TrialAttemptQueued:
		return to == TrialAttemptRunning || to == TrialAttemptWaiting || to == TrialAttemptCanceled
	case TrialAttemptRunning:
		return to == TrialAttemptWaiting || to == TrialAttemptCompleted || to == TrialAttemptInfraFailed || to == TrialAttemptInterrupted || to == TrialAttemptInDoubt || to == TrialAttemptCanceled
	case TrialAttemptWaiting:
		return to == TrialAttemptRunning || to == TrialAttemptCompleted || to == TrialAttemptInfraFailed || to == TrialAttemptInterrupted || to == TrialAttemptInDoubt || to == TrialAttemptCanceled
	case TrialAttemptInDoubt:
		return to == TrialAttemptReconciling
	case TrialAttemptReconciling:
		return to == TrialAttemptCompleted || to == TrialAttemptInfraFailed || to == TrialAttemptInterrupted || to == TrialAttemptCanceled
	default:
		return false
	}
}

func isTerminalTrialAttemptStatus(status TrialAttemptStatus) bool {
	switch status {
	case TrialAttemptCompleted, TrialAttemptInfraFailed, TrialAttemptInterrupted, TrialAttemptCanceled:
		return true
	default:
		return false
	}
}

func retryableTrialAttemptStatus(status TrialAttemptStatus) bool {
	return status == TrialAttemptInfraFailed || status == TrialAttemptInterrupted
}
