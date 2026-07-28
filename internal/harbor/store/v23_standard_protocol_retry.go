package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CommitStageProtocolRetryRequest is the atomic handoff for an explicitly
// confirmed retry of one already-terminal protocol failure. The caller owns
// protocol eligibility; the Store protects the durable retry chain, frozen
// attempt inputs, optimistic checkpoint, execution epoch, job and outbox.
//
// JobPayloadJSON is intentionally opaque at this layer. Its producer must
// bind NewGeneration and NewStageAttemptID before calling this method.
type CommitStageProtocolRetryRequest struct {
	SourceStageAttemptID              string
	ExpectedSourceStageAttemptVersion int64
	ExpectedSourceErrorText           string
	ExpectedSourceFailureClass        string
	ExpectedRunVersion                int64
	ExpectedRunExecutionEpoch         int
	NewStageAttemptID                 string
	NewGeneration                     int
	RetrySnapshotJSON                 string
	JobID                             string
	JobPayloadJSON                    string
	IdempotencyKey                    string
	Actor                             string
	Reason                            string
}

// StageProtocolRetryCommit is the complete durable result. Replays return the
// previously committed retry before comparing the now-advanced Run checkpoint.
type StageProtocolRetryCommit struct {
	Run        WorkflowRun
	Source     StageAttempt
	Retry      StageAttempt
	RunAttempt RunAttempt
	Job        DurableJob
	Replayed   bool
}

type preparedStageProtocolRetry struct {
	sourceStageAttemptID              string
	expectedSourceStageAttemptVersion int64
	expectedSourceErrorText           string
	expectedSourceFailureClass        string
	expectedRunVersion                int64
	expectedRunExecutionEpoch         int
	newStageAttemptID                 string
	newGeneration                     int
	retrySnapshotJSON                 string
	jobID                             string
	jobPayloadJSON                    string
	idempotencyKey                    string
	actor                             string
	reason                            string
}

// CommitStageProtocolRetry creates exactly one fenced retry StageAttempt and
// its executable durable job. A successful commit also moves the Run from
// failed_recoverable to running and advances its execution epoch, so stale
// run-worker handoffs cannot authorize the new generation.
func (s *Store) CommitStageProtocolRetry(ctx context.Context, request CommitStageProtocolRetryRequest) (StageProtocolRetryCommit, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return StageProtocolRetryCommit{}, err
	}
	prepared, err := prepareStageProtocolRetry(s, request)
	if err != nil {
		return StageProtocolRetryCommit{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return StageProtocolRetryCommit{}, err
	}
	defer tx.Rollback()

	if existing, err := getDurableJobByIdempotencyTx(ctx, tx, prepared.idempotencyKey); err == nil {
		commit, replayErr := stageProtocolRetryReplayTx(ctx, tx, prepared, existing)
		if replayErr != nil {
			return StageProtocolRetryCommit{}, replayErr
		}
		if err := tx.Commit(); err != nil {
			return StageProtocolRetryCommit{}, err
		}
		commit.Replayed = true
		return commit, nil
	} else if !errors.Is(err, ErrNotFound) {
		return StageProtocolRetryCommit{}, err
	}

	// The source attempt is the sole retry target. Loading it before the Run
	// prevents a caller from selecting a Run independent of frozen attempt
	// lineage.
	source, err := getStageAttemptTx(ctx, tx, prepared.sourceStageAttemptID)
	if err != nil {
		return StageProtocolRetryCommit{}, err
	}
	run, err := getWorkflowRunTx(ctx, tx, source.RunID)
	if err != nil {
		return StageProtocolRetryCommit{}, err
	}
	if err := validateStageProtocolRetrySourceTx(ctx, tx, run, source, prepared); err != nil {
		return StageProtocolRetryCommit{}, err
	}

	now := s.now().UTC()
	retry := StageAttempt{
		ID:                    prepared.newStageAttemptID,
		RunID:                 source.RunID,
		RetryOfStageAttemptID: source.ID,
		StageKey:              source.StageKey,
		StageGroup:            source.StageGroup,
		Ordinal:               source.Ordinal + 1,
		InputFingerprint:      source.InputFingerprint,
		ExecutionStatus:       StageExecutionQueued,
		BudgetSnapshotJSON:    source.BudgetSnapshotJSON,
		RetrySnapshotJSON:     prepared.retrySnapshotJSON,
		CreatedAt:             now,
		Version:               1,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO stage_attempts (
			id, run_id, retry_of_stage_attempt_id, stage_key, stage_group, ordinal,
			input_fingerprint, execution_status, verdict, budget_snapshot_json, retry_snapshot_json,
			artifact_manifest_id, error_text, failure_class, created_at, started_at, finished_at, version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, '', '', '', ?, NULL, NULL, ?)
	`, retry.ID, retry.RunID, retry.RetryOfStageAttemptID, retry.StageKey, retry.StageGroup, retry.Ordinal,
		retry.InputFingerprint, retry.ExecutionStatus, retry.BudgetSnapshotJSON, retry.RetrySnapshotJSON,
		retry.CreatedAt, retry.Version); err != nil {
		if isUniqueConstraint(err) {
			return StageProtocolRetryCommit{}, fmt.Errorf("%w: protocol retry stage attempt", ErrOptimisticLock)
		}
		return StageProtocolRetryCommit{}, err
	}

	job := DurableJob{
		ID:             prepared.jobID,
		CommandType:    "stage_attempt.execute",
		EntityType:     "stage_attempt",
		EntityID:       retry.ID,
		RunID:          run.ID,
		StageAttemptID: retry.ID,
		State:          JobQueued,
		PayloadJSON:    prepared.jobPayloadJSON,
		IdempotencyKey: prepared.idempotencyKey,
		CreatedBy:      prepared.actor,
		CreatedAt:      now,
		UpdatedAt:      now,
		Version:        1,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO jobs (
			id, command_type, entity_type, entity_id, run_id, stage_attempt_id, state,
			priority, payload_json, failure_code, failure_message, failure_details_json, idempotency_key, created_by, created_at, updated_at,
			started_at, finished_at, version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '', '', '{}', ?, ?, ?, ?, NULL, NULL, ?)
	`, job.ID, job.CommandType, job.EntityType, job.EntityID, job.RunID, job.StageAttemptID, job.State,
		job.Priority, job.PayloadJSON, job.IdempotencyKey, job.CreatedBy, job.CreatedAt, job.UpdatedAt, job.Version); err != nil {
		if isUniqueConstraint(err) {
			return StageProtocolRetryCommit{}, fmt.Errorf("%w: protocol retry job %s", ErrIdempotencyConflict, prepared.idempotencyKey)
		}
		return StageProtocolRetryCommit{}, err
	}

	runAttempt, err := createRunningStageProtocolRetryRunAttemptTx(ctx, s, tx, run, source, prepared, now)
	if err != nil {
		return StageProtocolRetryCommit{}, err
	}
	previousRunVersion := run.Version
	run.Status = WorkflowRunRunning
	run.ExecutionEpoch++
	run.FinishedAt = nil
	run.Version++
	updatedRun, err := tx.ExecContext(ctx, `
		UPDATE workflow_runs
		SET status = ?, execution_epoch = ?, started_at = ?, finished_at = NULL, version = ?
		WHERE id = ? AND version = ? AND execution_epoch = ?
	`, run.Status, run.ExecutionEpoch, run.StartedAt, run.Version, run.ID, previousRunVersion, prepared.expectedRunExecutionEpoch)
	if err != nil {
		return StageProtocolRetryCommit{}, err
	}
	changed, err := updatedRun.RowsAffected()
	if err != nil {
		return StageProtocolRetryCommit{}, err
	}
	if changed != 1 {
		return StageProtocolRetryCommit{}, fmt.Errorf("%w: workflow run %s", ErrOptimisticLock, run.ID)
	}

	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: prepared.actor, EntityType: "stage_attempt", EntityID: retry.ID, Action: "stage_attempt.protocol_retry_created", Reason: prepared.reason,
		PayloadJSON: auditPayload(map[string]any{"run_id": run.ID, "retry_of_stage_attempt_id": source.ID, "stage_key": retry.StageKey, "ordinal": retry.Ordinal, "generation": prepared.newGeneration}), CreatedAt: now,
	}); err != nil {
		return StageProtocolRetryCommit{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: prepared.actor, EntityType: "job", EntityID: job.ID, Action: "job.protocol_retry_created", Reason: prepared.reason,
		PayloadJSON: auditPayload(map[string]any{"run_id": run.ID, "stage_attempt_id": retry.ID, "idempotency_key": job.IdempotencyKey}), CreatedAt: now,
	}); err != nil {
		return StageProtocolRetryCommit{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: prepared.actor, EntityType: "workflow_run", EntityID: run.ID, Action: "workflow_run.protocol_retry_committed", Reason: prepared.reason,
		PayloadJSON: auditPayload(map[string]any{"source_stage_attempt_id": source.ID, "retry_stage_attempt_id": retry.ID, "execution_epoch": run.ExecutionEpoch, "version": run.Version}), CreatedAt: now,
	}); err != nil {
		return StageProtocolRetryCommit{}, err
	}
	if err := s.appendDurableJobQueuedOutboxTx(ctx, tx, job, now); err != nil {
		return StageProtocolRetryCommit{}, err
	}
	if err := tx.Commit(); err != nil {
		return StageProtocolRetryCommit{}, err
	}
	return StageProtocolRetryCommit{Run: run, Source: source, Retry: retry, RunAttempt: runAttempt, Job: job}, nil
}

func prepareStageProtocolRetry(s *Store, request CommitStageProtocolRetryRequest) (preparedStageProtocolRetry, error) {
	if s == nil {
		return preparedStageProtocolRetry{}, fmt.Errorf("protocol retry store is required")
	}
	sourceID, err := s.newV2ID(request.SourceStageAttemptID)
	if err != nil {
		return preparedStageProtocolRetry{}, err
	}
	if request.ExpectedSourceStageAttemptVersion <= 0 || request.ExpectedRunVersion <= 0 || request.ExpectedRunExecutionEpoch < 0 || request.NewGeneration < 0 {
		return preparedStageProtocolRetry{}, fmt.Errorf("protocol retry expected versions and generation are invalid")
	}
	if strings.TrimSpace(request.ExpectedSourceErrorText) == "" || strings.TrimSpace(request.ExpectedSourceFailureClass) == "" {
		return preparedStageProtocolRetry{}, fmt.Errorf("protocol retry source failure classification is required")
	}
	newAttemptID, err := s.newV2ID(request.NewStageAttemptID)
	if err != nil {
		return preparedStageProtocolRetry{}, err
	}
	jobID, err := s.newV2ID(request.JobID)
	if err != nil {
		return preparedStageProtocolRetry{}, err
	}
	retrySnapshot, err := normalizeJSON(request.RetrySnapshotJSON, "protocol retry snapshot")
	if err != nil {
		return preparedStageProtocolRetry{}, err
	}
	jobPayload, err := normalizeJSON(request.JobPayloadJSON, "protocol retry job payload")
	if err != nil {
		return preparedStageProtocolRetry{}, err
	}
	key, err := normalizeRequired(request.IdempotencyKey, "protocol retry idempotency key")
	if err != nil {
		return preparedStageProtocolRetry{}, err
	}
	return preparedStageProtocolRetry{
		sourceStageAttemptID: sourceID, expectedSourceStageAttemptVersion: request.ExpectedSourceStageAttemptVersion,
		expectedSourceErrorText: strings.TrimSpace(request.ExpectedSourceErrorText), expectedSourceFailureClass: strings.TrimSpace(request.ExpectedSourceFailureClass),
		expectedRunVersion: request.ExpectedRunVersion, expectedRunExecutionEpoch: request.ExpectedRunExecutionEpoch,
		newStageAttemptID: newAttemptID, newGeneration: request.NewGeneration, retrySnapshotJSON: retrySnapshot,
		jobID: jobID, jobPayloadJSON: jobPayload, idempotencyKey: key, actor: resolveActor(request.Actor), reason: strings.TrimSpace(request.Reason),
	}, nil
}

func stageProtocolRetryReplayTx(ctx context.Context, tx *sql.Tx, prepared preparedStageProtocolRetry, job DurableJob) (StageProtocolRetryCommit, error) {
	if job.CommandType != "stage_attempt.execute" || job.EntityType != "stage_attempt" || job.RunID == "" || job.StageAttemptID == "" || job.EntityID != job.StageAttemptID {
		return StageProtocolRetryCommit{}, fmt.Errorf("%w: protocol retry job %s", ErrIdempotencyConflict, job.ID)
	}
	retry, err := getStageAttemptTx(ctx, tx, job.StageAttemptID)
	if err != nil {
		return StageProtocolRetryCommit{}, err
	}
	if retry.RetryOfStageAttemptID != prepared.sourceStageAttemptID || retry.RunID != job.RunID {
		return StageProtocolRetryCommit{}, fmt.Errorf("%w: protocol retry job %s", ErrIdempotencyConflict, job.ID)
	}
	source, err := getStageAttemptTx(ctx, tx, retry.RetryOfStageAttemptID)
	if err != nil {
		return StageProtocolRetryCommit{}, err
	}
	run, err := getWorkflowRunTx(ctx, tx, retry.RunID)
	if err != nil {
		return StageProtocolRetryCommit{}, err
	}
	runAttempt, err := latestRunAttemptForProtocolRetryTx(ctx, tx, run.ID, source.ID)
	if err != nil {
		return StageProtocolRetryCommit{}, err
	}
	return StageProtocolRetryCommit{Run: run, Source: source, Retry: retry, RunAttempt: runAttempt, Job: job}, nil
}

func validateStageProtocolRetrySourceTx(ctx context.Context, tx *sql.Tx, run WorkflowRun, source StageAttempt, prepared preparedStageProtocolRetry) error {
	if run.Version != prepared.expectedRunVersion || run.ExecutionEpoch != prepared.expectedRunExecutionEpoch {
		return fmt.Errorf("%w: workflow run %s", ErrOptimisticLock, run.ID)
	}
	if run.Status != WorkflowRunFailedRecoverable {
		return fmt.Errorf("%w: workflow run %s is %s", ErrInvalidTransition, run.ID, run.Status)
	}
	if source.Version != prepared.expectedSourceStageAttemptVersion || source.ExecutionStatus != StageExecutionInfraFailed ||
		source.ErrorText != prepared.expectedSourceErrorText || source.FailureClass != prepared.expectedSourceFailureClass {
		return fmt.Errorf("%w: source stage attempt %s", ErrOptimisticLock, source.ID)
	}
	var childCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM stage_attempts WHERE retry_of_stage_attempt_id = ?`, source.ID).Scan(&childCount); err != nil {
		return err
	}
	if childCount != 0 {
		return fmt.Errorf("%w: source stage attempt %s already has a retry", ErrInvalidTransition, source.ID)
	}
	var maxOrdinal int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(ordinal), 0) FROM stage_attempts WHERE run_id = ? AND stage_key = ?`, run.ID, source.StageKey).Scan(&maxOrdinal); err != nil {
		return err
	}
	if source.Ordinal != maxOrdinal {
		return fmt.Errorf("%w: source stage attempt %s is not the latest retry leaf", ErrInvalidTransition, source.ID)
	}
	return nil
}

func createRunningStageProtocolRetryRunAttemptTx(ctx context.Context, s *Store, tx *sql.Tx, run WorkflowRun, source StageAttempt, prepared preparedStageProtocolRetry, now time.Time) (RunAttempt, error) {
	var maxOrdinal int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(ordinal), 0) FROM run_attempts WHERE run_id = ?`, run.ID).Scan(&maxOrdinal); err != nil {
		return RunAttempt{}, err
	}
	id, err := s.newV2ID("")
	if err != nil {
		return RunAttempt{}, err
	}
	attempt := RunAttempt{
		ID: id, RunID: run.ID, Ordinal: maxOrdinal + 1, Trigger: "standard_protocol_retry", ResumeFrom: "stage_attempt:" + source.ID,
		Status: RunAttemptRunning, CreatedAt: now, StartedAt: &now, Version: 1,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO run_attempts (id, run_id, ordinal, trigger, resume_from, status, created_at, started_at, finished_at, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)
	`, attempt.ID, attempt.RunID, attempt.Ordinal, attempt.Trigger, attempt.ResumeFrom, attempt.Status, attempt.CreatedAt, attempt.StartedAt, attempt.Version); err != nil {
		if isUniqueConstraint(err) {
			return RunAttempt{}, fmt.Errorf("%w: protocol retry run attempt", ErrOptimisticLock)
		}
		return RunAttempt{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: prepared.actor, EntityType: "run_attempt", EntityID: attempt.ID, Action: "run_attempt.protocol_retry_created", Reason: prepared.reason,
		PayloadJSON: auditPayload(map[string]any{"run_id": run.ID, "ordinal": attempt.Ordinal, "source_stage_attempt_id": source.ID}), CreatedAt: now,
	}); err != nil {
		return RunAttempt{}, err
	}
	return attempt, nil
}

func latestRunAttemptForProtocolRetryTx(ctx context.Context, tx *sql.Tx, runID, sourceStageAttemptID string) (RunAttempt, error) {
	return scanRunAttempt(tx.QueryRowContext(ctx, runAttemptSelect+` WHERE run_id = ? AND resume_from = ? ORDER BY ordinal DESC LIMIT 1`, runID, "stage_attempt:"+sourceStageAttemptID))
}
