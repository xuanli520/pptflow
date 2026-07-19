package store

import (
	"context"
	"fmt"
)

// MarkContinuationExecutionReconcileRequiredRequest identifies one committed
// continuation whose frozen execution inputs can no longer be proved.
type MarkContinuationExecutionReconcileRequiredRequest struct {
	ContinuationExecutionID string
	RunID                   string
	Actor                   string
	Reason                  string
}

// ContinuationExecutionReconciliation is the atomic projection written when a
// committed continuation can no longer safely start or advance.
type ContinuationExecutionReconciliation struct {
	Execution ContinuationExecution
	Run       WorkflowRun
}

// MarkContinuationExecutionReconcileRequired atomically fences a committed
// continuation and its Run. A missing frozen input after commit is not a
// cancellation: the advanced execution epoch and queued durable job are
// already observable, so an operator must reconcile the durable boundary.
func (s *Store) MarkContinuationExecutionReconcileRequired(ctx context.Context, request MarkContinuationExecutionReconcileRequiredRequest) (ContinuationExecutionReconciliation, error) {
	if err := s.mutationPreflight(ctx); err != nil {
		return ContinuationExecutionReconciliation{}, err
	}
	executionID, err := requireV4ID(request.ContinuationExecutionID, "continuation execution ID")
	if err != nil {
		return ContinuationExecutionReconciliation{}, err
	}
	runID, err := requireV4ID(request.RunID, "continuation execution Run ID")
	if err != nil {
		return ContinuationExecutionReconciliation{}, err
	}
	actor, err := normalizeRequired(request.Actor, "continuation reconciliation actor")
	if err != nil {
		return ContinuationExecutionReconciliation{}, err
	}
	reason, err := normalizeRequired(request.Reason, "continuation reconciliation reason")
	if err != nil {
		return ContinuationExecutionReconciliation{}, err
	}

	tx, releaseFence, err := s.beginDispatchFenceTx(ctx)
	if err != nil {
		return ContinuationExecutionReconciliation{}, err
	}
	defer tx.Rollback()
	defer releaseFence()

	execution, err := getContinuationExecutionTx(ctx, tx, executionID)
	if err != nil {
		return ContinuationExecutionReconciliation{}, err
	}
	if execution.RunID != runID {
		return ContinuationExecutionReconciliation{}, fmt.Errorf("%w: continuation execution %s does not belong to workflow run %s", ErrImmutable, execution.ID, runID)
	}
	run, err := getWorkflowRunTx(ctx, tx, runID)
	if err != nil {
		return ContinuationExecutionReconciliation{}, err
	}
	if execution.State != ContinuationExecutionReconcileRequired && !validContinuationExecutionTransition(execution.State, ContinuationExecutionReconcileRequired) {
		return ContinuationExecutionReconciliation{}, fmt.Errorf("%w: continuation execution %s from %s to %s", ErrInvalidTransition, execution.ID, execution.State, ContinuationExecutionReconcileRequired)
	}
	if run.Status != WorkflowRunInDoubt && !validWorkflowRunTransition(run.Status, WorkflowRunInDoubt) {
		return ContinuationExecutionReconciliation{}, fmt.Errorf("%w: workflow run %s from %s to %s", ErrInvalidTransition, run.ID, run.Status, WorkflowRunInDoubt)
	}

	now := s.now().UTC()
	if execution.State != ContinuationExecutionReconcileRequired {
		execution.State = ContinuationExecutionReconcileRequired
		execution.UpdatedAt = now
		execution.Version++
		result, err := tx.ExecContext(ctx, `
			UPDATE continuation_executions_v4
			SET state = ?, updated_at = ?, version = ?
			WHERE id = ? AND version = ?
		`, execution.State, execution.UpdatedAt, execution.Version, execution.ID, execution.Version-1)
		if err != nil {
			return ContinuationExecutionReconciliation{}, err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return ContinuationExecutionReconciliation{}, err
		}
		if changed != 1 {
			return ContinuationExecutionReconciliation{}, fmt.Errorf("%w: continuation execution %s", ErrOptimisticLock, execution.ID)
		}
		if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
			Actor: actor, EntityType: "continuation_execution", EntityID: execution.ID,
			Action: "continuation_execution.reconciliation_required", Reason: reason,
			PayloadJSON: auditPayload(map[string]any{"state": execution.State, "run_id": run.ID, "version": execution.Version}), CreatedAt: now,
		}); err != nil {
			return ContinuationExecutionReconciliation{}, err
		}
	}
	if run.Status != WorkflowRunInDoubt {
		run.Status = WorkflowRunInDoubt
		run.Version++
		result, err := tx.ExecContext(ctx, `
			UPDATE workflow_runs
			SET status = ?, version = ?
			WHERE id = ? AND version = ?
		`, run.Status, run.Version, run.ID, run.Version-1)
		if err != nil {
			return ContinuationExecutionReconciliation{}, err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return ContinuationExecutionReconciliation{}, err
		}
		if changed != 1 {
			return ContinuationExecutionReconciliation{}, fmt.Errorf("%w: workflow run %s", ErrOptimisticLock, run.ID)
		}
		if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
			Actor: actor, EntityType: "workflow_run", EntityID: run.ID,
			Action: "workflow_run.continuation_reconciliation_required", Reason: reason,
			PayloadJSON: auditPayload(map[string]any{"continuation_execution_id": execution.ID, "status": run.Status, "version": run.Version}), CreatedAt: now,
		}); err != nil {
			return ContinuationExecutionReconciliation{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return ContinuationExecutionReconciliation{}, err
	}
	return ContinuationExecutionReconciliation{Execution: execution, Run: run}, nil
}
