package store

import (
	"context"
	"errors"
	"testing"
)

func TestMarkContinuationExecutionReconcileRequiredAtomicallyProjectsExecutionAndRun(t *testing.T) {
	ctx := context.Background()
	fixture := newAuthoringRecoveryCommitFixture(t, ctx)
	plan, checkpoint := createAuthoringRecoveryCommitPlan(t, ctx, fixture)
	committed, err := fixture.store.CommitAuthoringRecoveryExecution(ctx, authoringRecoveryCommitRequest(fixture, plan, checkpoint, "authoring-recovery-input-drift"))
	if err != nil {
		t.Fatal(err)
	}

	projection, err := fixture.store.MarkContinuationExecutionReconcileRequired(ctx, MarkContinuationExecutionReconcileRequiredRequest{
		ContinuationExecutionID: committed.Execution.ID, RunID: committed.Run.ID,
		Actor: "worker", Reason: "frozen continuation input disappeared after commit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if projection.Execution.State != ContinuationExecutionReconcileRequired || projection.Execution.FinishedAt != nil || projection.Execution.Version != committed.Execution.Version+1 {
		t.Fatalf("reconciled continuation execution = %+v", projection.Execution)
	}
	if projection.Run.Status != WorkflowRunInDoubt || projection.Run.Version != committed.Run.Version+1 {
		t.Fatalf("reconciled workflow run = %+v", projection.Run)
	}

	replayed, err := fixture.store.MarkContinuationExecutionReconcileRequired(ctx, MarkContinuationExecutionReconcileRequiredRequest{
		ContinuationExecutionID: committed.Execution.ID, RunID: committed.Run.ID,
		Actor: "worker", Reason: "replay frozen continuation input reconciliation",
	})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Execution.Version != projection.Execution.Version || replayed.Run.Version != projection.Run.Version {
		t.Fatalf("replayed reconciliation changed versions: first=%+v replay=%+v", projection, replayed)
	}
}

func TestMarkContinuationExecutionReconcileRequiredRejectsInvalidRunWithoutPartialExecutionProjection(t *testing.T) {
	ctx := context.Background()
	fixture := newAuthoringRecoveryCommitFixture(t, ctx)
	plan, checkpoint := createAuthoringRecoveryCommitPlan(t, ctx, fixture)
	committed, err := fixture.store.CommitAuthoringRecoveryExecution(ctx, authoringRecoveryCommitRequest(fixture, plan, checkpoint, "authoring-recovery-invalid-reconciliation"))
	if err != nil {
		t.Fatal(err)
	}
	canceled := transitionAuthoringRecoveryCommitRun(t, ctx, fixture.store, committed.Run, WorkflowRunCanceled)

	_, err = fixture.store.MarkContinuationExecutionReconcileRequired(ctx, MarkContinuationExecutionReconcileRequiredRequest{
		ContinuationExecutionID: committed.Execution.ID, RunID: committed.Run.ID,
		Actor: "worker", Reason: "must not partially project a terminal Run",
	})
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("terminal Run reconciliation error = %v, want ErrInvalidTransition", err)
	}
	execution, err := fixture.store.GetContinuationExecution(ctx, committed.Execution.ID)
	if err != nil || execution == nil || execution.State != ContinuationExecutionQueued || execution.Version != committed.Execution.Version {
		t.Fatalf("partially projected continuation execution = %+v, %v", execution, err)
	}
	run, err := fixture.store.GetWorkflowRun(ctx, committed.Run.ID)
	if err != nil || run == nil || run.Status != WorkflowRunCanceled || run.Version != canceled.Version {
		t.Fatalf("terminal workflow run changed after rejected reconciliation = %+v, %v", run, err)
	}
}
