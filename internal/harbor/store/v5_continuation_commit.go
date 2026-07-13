package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

var (
	// ErrContinuationPlanExpired means a frozen plan reached its immutable
	// expiry before it could be committed for execution.
	ErrContinuationPlanExpired = errors.New("store: continuation plan expired")
	// ErrContinuationReconciliationRequired means an unknown side-effect
	// outcome must be reconciled before another continuation can begin.
	ErrContinuationReconciliationRequired = errors.New("store: continuation reconciliation is required")
)

// CommitContinuationExecutionRequest is the single transactional boundary
// between a frozen continuation plan and durable worker dispatch. It only
// supports a plan whose target is its source run; revision-changing plans use
// the candidate-revision commit path added with ChangeProvider support.
type CommitContinuationExecutionRequest struct {
	ID             string
	PlanID         string
	RunID          string
	IdempotencyKey string
	PayloadJSON    string
	Expected       ControlCheckpointRef
	Actor          string
	Reason         string
	Priority       int
}

// ContinuationExecutionCommit is returned after the execution record, its
// durable job, outbox event, and next execution epoch become visible together.
type ContinuationExecutionCommit struct {
	Execution ContinuationExecution
	Job       DurableJob
	Run       WorkflowRun
}

type preparedContinuationExecutionCommit struct {
	ID             string
	PlanID         string
	RunID          string
	IdempotencyKey string
	PayloadJSON    string
	Expected       ControlCheckpointRef
	Actor          string
	Reason         string
	Priority       int
	JobID          string
	JobKey         string
}

// GetFrozenPlanByCommand returns the one immutable plan associated with a
// command, if planning reached the frozen-plan stage. It enables a retried
// client command to return exactly the original plan rather than re-planning
// from later mutable state.
func (s *Store) GetFrozenPlanByCommand(ctx context.Context, commandID string) (*FrozenPlan, error) {
	if _, err := requireV4ID(commandID, "continuation command ID"); err != nil {
		return nil, err
	}
	plan, err := scanFrozenPlan(s.db.QueryRowContext(ctx, frozenPlanV4Select+" WHERE command_id = ?", commandID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &plan, nil
}

// GetContinuationExecutionByIdempotency returns the durable execution record
// for an idempotent Execute call, if it has already been committed.
func (s *Store) GetContinuationExecutionByIdempotency(ctx context.Context, idempotencyKey string) (*ContinuationExecution, error) {
	idempotencyKey, err := normalizeRequired(idempotencyKey, "continuation execution idempotency key")
	if err != nil {
		return nil, err
	}
	execution, err := scanContinuationExecution(s.db.QueryRowContext(ctx, continuationExecutionV4Select+" WHERE idempotency_key = ?", idempotencyKey))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &execution, nil
}

// CommitContinuationExecution atomically proves a frozen plan is still bound
// to the current run checkpoint, advances that run's execution epoch, writes
// the immutable execution identity, and queues the worker job/outbox record.
// A replay returns the already committed execution before checking the now
// advanced checkpoint, while a second plan based on the old checkpoint loses
// the optimistic compare-and-swap.
func (s *Store) CommitContinuationExecution(ctx context.Context, request CommitContinuationExecutionRequest) (ContinuationExecutionCommit, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return ContinuationExecutionCommit{}, err
	}
	prepared, err := prepareContinuationExecutionCommit(s, request)
	if err != nil {
		return ContinuationExecutionCommit{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ContinuationExecutionCommit{}, err
	}
	defer tx.Rollback()

	if existing, err := getContinuationExecutionByKeyTx(ctx, tx, prepared.IdempotencyKey); err == nil {
		if existing.PlanID != prepared.PlanID || existing.RunID != prepared.RunID || existing.PayloadJSON != prepared.PayloadJSON {
			return ContinuationExecutionCommit{}, fmt.Errorf("%w: continuation execution key %s", ErrIdempotencyConflict, prepared.IdempotencyKey)
		}
		job, err := getDurableJobByIdempotencyTx(ctx, tx, prepared.JobKey)
		if err != nil {
			return ContinuationExecutionCommit{}, fmt.Errorf("load durable job for committed continuation: %w", err)
		}
		if job.EntityType != "continuation_execution" || job.EntityID != existing.ID || job.RunID != existing.RunID || job.PayloadJSON != existing.PayloadJSON {
			return ContinuationExecutionCommit{}, fmt.Errorf("%w: continuation execution %s has inconsistent durable job", ErrImmutable, existing.ID)
		}
		run, err := getWorkflowRunTx(ctx, tx, existing.RunID)
		if err != nil {
			return ContinuationExecutionCommit{}, err
		}
		if err := tx.Commit(); err != nil {
			return ContinuationExecutionCommit{}, err
		}
		return ContinuationExecutionCommit{Execution: existing, Job: job, Run: run}, nil
	} else if !errors.Is(err, ErrNotFound) {
		return ContinuationExecutionCommit{}, err
	}

	plan, err := getFrozenPlanTx(ctx, tx, prepared.PlanID)
	if err != nil {
		return ContinuationExecutionCommit{}, err
	}
	now := s.now().UTC()
	if !plan.ExpiresAt.After(now) {
		return ContinuationExecutionCommit{}, fmt.Errorf("%w: %s", ErrContinuationPlanExpired, plan.ID)
	}
	if err := verifyStoredContinuationPlan(plan, prepared); err != nil {
		return ContinuationExecutionCommit{}, err
	}
	run, err := getWorkflowRunTx(ctx, tx, prepared.RunID)
	if err != nil {
		return ContinuationExecutionCommit{}, err
	}
	if err := verifyControlCheckpointTx(ctx, tx, run, prepared.Expected); err != nil {
		return ContinuationExecutionCommit{}, err
	}
	if run.Status == WorkflowRunInDoubt {
		return ContinuationExecutionCommit{}, fmt.Errorf("%w: workflow run %s is in_doubt", ErrContinuationReconciliationRequired, run.ID)
	}
	if err := continuationRunIsReconciledTx(ctx, tx, run.ID); err != nil {
		return ContinuationExecutionCommit{}, err
	}

	var snapshot workflowkit.ContinuationPlanSnapshot
	if err := json.Unmarshal([]byte(plan.PayloadJSON), &snapshot); err != nil {
		return ContinuationExecutionCommit{}, fmt.Errorf("decode frozen continuation plan %s: %w", plan.ID, err)
	}
	if snapshot.NextExecutionEpoch != run.ExecutionEpoch+1 {
		return ContinuationExecutionCommit{}, fmt.Errorf("%w: continuation plan %s execution epoch is stale", ErrOptimisticLock, plan.ID)
	}

	if _, err := getDurableJobByIdempotencyTx(ctx, tx, prepared.JobKey); err == nil {
		return ContinuationExecutionCommit{}, fmt.Errorf("%w: durable job key %s", ErrIdempotencyConflict, prepared.JobKey)
	} else if !errors.Is(err, ErrNotFound) {
		return ContinuationExecutionCommit{}, err
	}

	execution := ContinuationExecution{
		ID:             prepared.ID,
		PlanID:         prepared.PlanID,
		RunID:          prepared.RunID,
		IdempotencyKey: prepared.IdempotencyKey,
		State:          ContinuationExecutionQueued,
		PayloadJSON:    prepared.PayloadJSON,
		CreatedBy:      prepared.Actor,
		CreatedAt:      now,
		UpdatedAt:      now,
		Version:        1,
	}
	job := DurableJob{
		ID:             prepared.JobID,
		CommandType:    "task_continuation.execute",
		EntityType:     "continuation_execution",
		EntityID:       execution.ID,
		RunID:          run.ID,
		State:          JobQueued,
		Priority:       prepared.Priority,
		PayloadJSON:    execution.PayloadJSON,
		IdempotencyKey: prepared.JobKey,
		CreatedBy:      prepared.Actor,
		CreatedAt:      now,
		UpdatedAt:      now,
		Version:        1,
	}

	// Advancing the epoch is the CAS that prevents another frozen plan bound to
	// this checkpoint from being committed after this transaction succeeds.
	previousVersion := run.Version
	run.ExecutionEpoch = snapshot.NextExecutionEpoch
	run.Version++
	result, err := tx.ExecContext(ctx, `
		UPDATE workflow_runs
		SET execution_epoch = ?, version = ?
		WHERE id = ? AND version = ? AND execution_epoch = ?
	`, run.ExecutionEpoch, run.Version, run.ID, previousVersion, prepared.Expected.ExecutionEpoch)
	if err != nil {
		return ContinuationExecutionCommit{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return ContinuationExecutionCommit{}, err
	}
	if changed != 1 {
		return ContinuationExecutionCommit{}, fmt.Errorf("%w: workflow run %s", ErrOptimisticLock, run.ID)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO continuation_executions_v4 (
			id, plan_id, parent_execution_id, run_id, idempotency_key, state,
			payload_json, created_by, created_at, updated_at, finished_at, version
		) VALUES (?, ?, NULL, ?, ?, ?, ?, ?, ?, ?, NULL, ?)
	`, execution.ID, execution.PlanID, execution.RunID, execution.IdempotencyKey, execution.State,
		execution.PayloadJSON, execution.CreatedBy, execution.CreatedAt, execution.UpdatedAt, execution.Version); err != nil {
		if isUniqueConstraint(err) {
			return ContinuationExecutionCommit{}, fmt.Errorf("%w: continuation execution %s", ErrIdentityCollision, execution.ID)
		}
		return ContinuationExecutionCommit{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO jobs (
			id, command_type, entity_type, entity_id, run_id, stage_attempt_id, state,
			priority, payload_json, idempotency_key, created_by, created_at, updated_at,
			started_at, finished_at, version
		) VALUES (?, ?, ?, ?, ?, NULL, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?)
	`, job.ID, job.CommandType, job.EntityType, job.EntityID, job.RunID, job.State, job.Priority,
		job.PayloadJSON, job.IdempotencyKey, job.CreatedBy, job.CreatedAt, job.UpdatedAt, job.Version); err != nil {
		if isUniqueConstraint(err) {
			return ContinuationExecutionCommit{}, fmt.Errorf("%w: durable continuation job %s", ErrIdentityCollision, job.ID)
		}
		return ContinuationExecutionCommit{}, err
	}
	if err := s.appendV5OutboxTx(ctx, tx, "continuation_execution.queued", "continuation_execution", execution.ID,
		prepared.IdempotencyKey+":queued", auditPayload(map[string]any{"plan_id": plan.ID, "run_id": run.ID, "job_id": job.ID}), now); err != nil {
		return ContinuationExecutionCommit{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: prepared.Actor, EntityType: "workflow_run", EntityID: run.ID, Action: "workflow_run.continuation_committed",
		Reason: prepared.Reason, PayloadJSON: auditPayload(map[string]any{"plan_id": plan.ID, "execution_id": execution.ID, "execution_epoch": run.ExecutionEpoch}),
		OperationKey: prepared.IdempotencyKey, CreatedAt: now,
	}); err != nil {
		return ContinuationExecutionCommit{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		Actor: prepared.Actor, EntityType: "continuation_execution", EntityID: execution.ID, Action: "continuation_execution.committed",
		Reason: prepared.Reason, PayloadJSON: auditPayload(map[string]any{"plan_id": plan.ID, "job_id": job.ID}),
		OperationKey: prepared.IdempotencyKey, CreatedAt: now,
	}); err != nil {
		return ContinuationExecutionCommit{}, err
	}
	if err := tx.Commit(); err != nil {
		return ContinuationExecutionCommit{}, err
	}
	return ContinuationExecutionCommit{Execution: execution, Job: job, Run: run}, nil
}

func prepareContinuationExecutionCommit(s *Store, request CommitContinuationExecutionRequest) (preparedContinuationExecutionCommit, error) {
	id, err := s.newV2ID(request.ID)
	if err != nil {
		return preparedContinuationExecutionCommit{}, err
	}
	planID, err := requireV4ID(request.PlanID, "continuation execution plan ID")
	if err != nil {
		return preparedContinuationExecutionCommit{}, err
	}
	if !isUUIDv7(request.RunID) {
		return preparedContinuationExecutionCommit{}, ErrInvalidUUIDv7Identity
	}
	key, err := normalizeRequired(request.IdempotencyKey, "continuation execution idempotency key")
	if err != nil {
		return preparedContinuationExecutionCommit{}, err
	}
	payload, err := normalizeV4JSON(request.PayloadJSON, "continuation execution payload")
	if err != nil {
		return preparedContinuationExecutionCommit{}, err
	}
	if err := validateControlCheckpoint(request.Expected); err != nil {
		return preparedContinuationExecutionCommit{}, err
	}
	actor, err := normalizeRequired(request.Actor, "continuation execution actor")
	if err != nil {
		return preparedContinuationExecutionCommit{}, err
	}
	reason, err := normalizeRequired(request.Reason, "continuation execution reason")
	if err != nil {
		return preparedContinuationExecutionCommit{}, err
	}
	jobID, err := s.newV2ID("")
	if err != nil {
		return preparedContinuationExecutionCommit{}, err
	}
	return preparedContinuationExecutionCommit{
		ID:             id,
		PlanID:         planID,
		RunID:          strings.TrimSpace(request.RunID),
		IdempotencyKey: key,
		PayloadJSON:    payload,
		Expected:       request.Expected,
		Actor:          actor,
		Reason:         reason,
		Priority:       request.Priority,
		JobID:          jobID,
		JobKey:         key + ":job",
	}, nil
}

func verifyStoredContinuationPlan(plan FrozenPlan, request preparedContinuationExecutionCommit) error {
	if plan.SubjectID != request.Expected.SubjectID || plan.SubjectRevisionID != request.Expected.SubjectRevisionID ||
		plan.SubjectDigest != request.Expected.SubjectDigest || plan.WorkflowFingerprint != request.Expected.WorkflowFingerprint {
		return fmt.Errorf("%w: frozen continuation plan %s does not bind the requested checkpoint", ErrOptimisticLock, plan.ID)
	}
	var snapshot workflowkit.ContinuationPlanSnapshot
	if err := json.Unmarshal([]byte(plan.PayloadJSON), &snapshot); err != nil {
		return fmt.Errorf("decode frozen continuation plan %s: %w", plan.ID, err)
	}
	if snapshot.PlanID != plan.ID || snapshot.CommandID != plan.CommandID || snapshot.SourceRunID != request.RunID ||
		snapshot.TargetRunRelation != workflowkit.RelationSameRunAttempt || snapshot.BaseCheckpoint.SubjectID != request.Expected.SubjectID ||
		snapshot.BaseCheckpoint.SubjectRevisionID != request.Expected.SubjectRevisionID || snapshot.BaseCheckpoint.SubjectDigest != workflowkit.SubjectDigest(request.Expected.SubjectDigest) ||
		snapshot.BaseCheckpoint.WorkflowFingerprint != workflowkit.Fingerprint(request.Expected.WorkflowFingerprint) ||
		snapshot.BaseCheckpoint.Sequence != request.Expected.Sequence || snapshot.BaseCheckpoint.ExecutionEpoch != request.Expected.ExecutionEpoch ||
		snapshot.BaseCheckpoint.SubjectVersion != request.Expected.SubjectVersion || snapshot.SubjectRevisionID != plan.SubjectRevisionID ||
		snapshot.SubjectDigest != workflowkit.SubjectDigest(plan.SubjectDigest) || !snapshot.ExpiresAt.Equal(plan.ExpiresAt) {
		return fmt.Errorf("%w: frozen continuation plan %s payload does not match its stored binding", ErrImmutable, plan.ID)
	}
	return nil
}

func continuationRunIsReconciledTx(ctx context.Context, tx *sql.Tx, runID string) error {
	var unresolved int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM stage_attempts
		WHERE run_id = ? AND execution_status IN ('in_doubt', 'reconciling')
	`, runID).Scan(&unresolved); err != nil {
		return err
	}
	if unresolved > 0 {
		return fmt.Errorf("%w: workflow run %s has stage attempts awaiting reconciliation", ErrContinuationReconciliationRequired, runID)
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM node_attempts AS node
		JOIN stage_attempts AS stage ON stage.id = node.stage_attempt_id
		WHERE stage.run_id = ? AND node.status = 'in_doubt'
	`, runID).Scan(&unresolved); err != nil {
		return err
	}
	if unresolved > 0 {
		return fmt.Errorf("%w: workflow run %s has node attempts awaiting reconciliation", ErrContinuationReconciliationRequired, runID)
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM jobs
		WHERE run_id = ? AND state = 'in_doubt'
	`, runID).Scan(&unresolved); err != nil {
		return err
	}
	if unresolved > 0 {
		return fmt.Errorf("%w: workflow run %s has durable jobs awaiting reconciliation", ErrContinuationReconciliationRequired, runID)
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM control_operations_v5
		WHERE run_id = ? AND status = 'reconcile_required'
	`, runID).Scan(&unresolved); err != nil {
		return err
	}
	if unresolved > 0 {
		return fmt.Errorf("%w: workflow run %s has controls awaiting reconciliation", ErrContinuationReconciliationRequired, runID)
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM side_effect_operations_v5
		WHERE run_id = ? AND state = 'unknown'
	`, runID).Scan(&unresolved); err != nil {
		return err
	}
	if unresolved > 0 {
		return fmt.Errorf("%w: workflow run %s has side effects awaiting reconciliation", ErrContinuationReconciliationRequired, runID)
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM continuation_executions_v4
		WHERE run_id = ? AND state = 'reconcile_required'
	`, runID).Scan(&unresolved); err != nil {
		return err
	}
	if unresolved > 0 {
		return fmt.Errorf("%w: workflow run %s has a continuation awaiting reconciliation", ErrContinuationReconciliationRequired, runID)
	}
	return nil
}
