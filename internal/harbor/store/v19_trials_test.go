package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type trialStoreFixture struct {
	run   WorkflowRun
	stage StageAttempt
}

func TestTrialExecutionPersistsImmutableLogicalSamples(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	s.now = func() time.Time { return time.Date(2026, 7, 14, 14, 0, 0, 0, time.UTC) }
	fixture := newTrialStoreFixture(t, s)

	executions := make([]TrialExecution, 0, 4)
	for ordinal := 1; ordinal <= 4; ordinal++ {
		execution, err := s.CreateTrialExecution(ctx, CreateTrialExecutionRequest{
			RunID: fixture.run.ID, StageAttemptID: fixture.stage.ID, StageKey: fixture.stage.StageKey, Ordinal: ordinal,
			Actor: "tester", Reason: "allocate logical evaluator sample",
		})
		if err != nil {
			t.Fatalf("create logical sample %d: %v", ordinal, err)
		}
		if !isUUIDv7(execution.ID) || execution.Status != TrialExecutionQueued || execution.Version != 1 {
			t.Fatalf("logical sample %d = %+v, want UUIDv7 queued version one", ordinal, execution)
		}
		if execution.RunID != fixture.run.ID || execution.StageAttemptID != fixture.stage.ID || execution.StageKey != fixture.stage.StageKey || execution.Ordinal != ordinal {
			t.Fatalf("logical sample %d binding = %+v", ordinal, execution)
		}
		executions = append(executions, execution)
	}

	listed, err := s.ListTrialExecutionsForStageAttempt(ctx, fixture.stage.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 4 {
		t.Fatalf("listed logical samples = %d, want 4", len(listed))
	}
	for index, execution := range listed {
		if execution.ID != executions[index].ID || execution.Ordinal != index+1 {
			t.Fatalf("listed logical sample[%d] = %+v, want %+v", index, execution, executions[index])
		}
		var entityType string
		if err := s.db.QueryRow(`SELECT entity_type FROM entity_id_registry WHERE id = ?`, execution.ID).Scan(&entityType); err != nil {
			t.Fatalf("read registry for logical sample %s: %v", execution.ID, err)
		}
		if entityType != "trial_execution" {
			t.Fatalf("registry type for logical sample = %q, want trial_execution", entityType)
		}
	}

	loaded, err := s.GetTrialExecutionForStageAttempt(ctx, fixture.stage.ID, 3)
	if err != nil || loaded == nil || loaded.ID != executions[2].ID {
		t.Fatalf("get logical sample ordinal three = %+v, %v", loaded, err)
	}
	forRun, err := s.ListTrialExecutionsForRun(ctx, fixture.run.ID)
	if err != nil || len(forRun) != 4 {
		t.Fatalf("list logical samples for run = %d, %v; want 4", len(forRun), err)
	}

	if _, err := s.CreateTrialExecution(ctx, CreateTrialExecutionRequest{
		RunID: fixture.run.ID, StageAttemptID: fixture.stage.ID, StageKey: fixture.stage.StageKey, Ordinal: 1,
		Actor: "tester", Reason: "duplicate logical coordinate",
	}); !errors.Is(err, ErrIdentityCollision) {
		t.Fatalf("duplicate logical coordinate error = %v, want ErrIdentityCollision", err)
	}
	if _, err := s.CreateTrialExecution(ctx, CreateTrialExecutionRequest{
		ID: fixture.run.ID, RunID: fixture.run.ID, StageAttemptID: fixture.stage.ID, StageKey: fixture.stage.StageKey, Ordinal: 5,
		Actor: "tester", Reason: "exercise global UUID registry",
	}); !errors.Is(err, ErrIdentityCollision) {
		t.Fatalf("workflow-run UUID reused for trial execution = %v, want ErrIdentityCollision", err)
	}

	other := newTrialStoreFixture(t, s)
	if _, err := s.CreateTrialExecution(ctx, CreateTrialExecutionRequest{
		RunID: fixture.run.ID, StageAttemptID: other.stage.ID, StageKey: other.stage.StageKey, Ordinal: 5,
		Actor: "tester", Reason: "cross-run stage rebinding",
	}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("cross-run stage binding error = %v, want ErrInvalidTransition", err)
	}
	if _, err := s.CreateTrialExecution(ctx, CreateTrialExecutionRequest{
		RunID: fixture.run.ID, StageAttemptID: fixture.stage.ID, StageKey: "wrong_evaluator", Ordinal: 5,
		Actor: "tester", Reason: "wrong stage key",
	}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("wrong stage key error = %v, want ErrInvalidTransition", err)
	}
}

func TestTrialAttemptRetryLineageKeepsOneLogicalSample(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	fixture := newTrialStoreFixture(t, s)
	execution := newTrialExecution(t, s, fixture, 1)

	first, err := s.CreateTrialAttempt(ctx, CreateTrialAttemptRequest{
		TrialExecutionID: execution.ID, Ordinal: 1, Actor: "tester", Reason: "first technical try",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.RetryOfTrialAttemptID != "" || first.Status != TrialAttemptQueued {
		t.Fatalf("first technical attempt = %+v", first)
	}
	if _, err := s.CreateTrialAttempt(ctx, CreateTrialAttemptRequest{
		TrialExecutionID: execution.ID, Ordinal: 2, RetryOfTrialAttemptID: first.ID, Actor: "tester", Reason: "overlapping retry",
	}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("overlapping retry error = %v, want ErrInvalidTransition", err)
	}

	first, err = s.TransitionTrialAttempt(ctx, TransitionTrialAttemptRequest{
		TrialAttemptID: first.ID, ExpectedVersion: first.Version, Status: TrialAttemptRunning, Actor: "tester", Reason: "begin technical try",
	})
	if err != nil {
		t.Fatal(err)
	}
	execution = getTrialExecution(t, s, execution.ID)
	if execution.Status != TrialExecutionRunning || execution.FinishedAt != nil {
		t.Fatalf("running technical attempt changed logical sample to %+v, want continuable running", execution)
	}
	first = transitionTrialAttempt(t, s, first, TrialAttemptWaiting, "", "")
	execution = getTrialExecution(t, s, execution.ID)
	if execution.Status != TrialExecutionWaiting || execution.FinishedAt != nil {
		t.Fatalf("waiting technical attempt changed logical sample to %+v, want continuable waiting", execution)
	}
	first = transitionTrialAttempt(t, s, first, TrialAttemptRunning, "", "")
	first, err = s.TransitionTrialAttempt(ctx, TransitionTrialAttemptRequest{
		TrialAttemptID: first.ID, ExpectedVersion: first.Version, Status: TrialAttemptInfraFailed,
		ErrorText: "network reset", FailureClass: "transport", Actor: "tester", Reason: "retryable transport failure",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.StartedAt == nil || first.FinishedAt == nil || first.ErrorText != "network reset" || first.FailureClass != "transport" {
		t.Fatalf("failed first technical attempt = %+v", first)
	}
	execution = getTrialExecution(t, s, execution.ID)
	if execution.Status != TrialExecutionRunning || execution.FinishedAt != nil {
		t.Fatalf("technical infra failure terminalized logical sample = %+v", execution)
	}

	retry, err := s.CreateTrialAttempt(ctx, CreateTrialAttemptRequest{
		TrialExecutionID: execution.ID, RetryOfTrialAttemptID: first.ID, Ordinal: 2,
		Actor: "tester", Reason: "retry same logical sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	if retry.TrialExecutionID != execution.ID || retry.RetryOfTrialAttemptID != first.ID || retry.Ordinal != 2 {
		t.Fatalf("retry technical attempt = %+v", retry)
	}
	attempts, err := s.ListTrialAttemptsForTrialExecution(ctx, execution.ID)
	if err != nil || len(attempts) != 2 {
		t.Fatalf("technical attempt lineage = %d, %v; want 2", len(attempts), err)
	}
	executions, err := s.ListTrialExecutionsForStageAttempt(ctx, fixture.stage.ID)
	if err != nil || len(executions) != 1 || executions[0].ID != execution.ID {
		t.Fatalf("retry created logical samples = %+v, %v; want exactly original sample", executions, err)
	}
	collisionExecution := newTrialExecution(t, s, fixture, 4)
	if _, err := s.CreateTrialAttempt(ctx, CreateTrialAttemptRequest{
		ID: collisionExecution.ID, TrialExecutionID: collisionExecution.ID, Ordinal: 1,
		Actor: "tester", Reason: "reuse logical execution UUID",
	}); !errors.Is(err, ErrIdentityCollision) {
		t.Fatalf("trial execution UUID reused for trial attempt = %v, want ErrIdentityCollision", err)
	}

	otherExecution := newTrialExecution(t, s, fixture, 2)
	otherFirst := failedTrialAttempt(t, s, otherExecution.ID)
	if _, err := s.CreateTrialAttempt(ctx, CreateTrialAttemptRequest{
		TrialExecutionID: execution.ID, RetryOfTrialAttemptID: otherFirst.ID, Ordinal: 3,
		Actor: "tester", Reason: "cross-logical-sample predecessor",
	}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("cross-sample retry predecessor error = %v, want ErrInvalidTransition", err)
	}

	nonContiguousExecution := newTrialExecution(t, s, fixture, 3)
	nonContiguousFirst := failedTrialAttempt(t, s, nonContiguousExecution.ID)
	if _, err := s.CreateTrialAttempt(ctx, CreateTrialAttemptRequest{
		TrialExecutionID: nonContiguousExecution.ID, RetryOfTrialAttemptID: nonContiguousFirst.ID, Ordinal: 3,
		Actor: "tester", Reason: "skip retry ordinal",
	}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("non-contiguous retry error = %v, want ErrInvalidTransition", err)
	}
	if _, err := s.CreateTrialAttempt(ctx, CreateTrialAttemptRequest{
		TrialExecutionID: nonContiguousExecution.ID, RetryOfTrialAttemptID: nonContiguousFirst.ID, Ordinal: 1,
		Actor: "tester", Reason: "initial attempt with retry predecessor",
	}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("initial retry predecessor error = %v, want ErrInvalidTransition", err)
	}
}

func TestTrialAttemptRequiresReconciliationBeforeRetry(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	fixture := newTrialStoreFixture(t, s)
	execution := newTrialExecution(t, s, fixture, 1)
	first, err := s.CreateTrialAttempt(ctx, CreateTrialAttemptRequest{
		TrialExecutionID: execution.ID, Ordinal: 1, Actor: "tester", Reason: "first external call",
	})
	if err != nil {
		t.Fatal(err)
	}
	first = transitionTrialAttempt(t, s, first, TrialAttemptRunning, "", "")
	first = transitionTrialAttempt(t, s, first, TrialAttemptInDoubt, "outcome unavailable", "external_outcome_unknown")
	execution = getTrialExecution(t, s, execution.ID)
	if execution.Status != TrialExecutionInDoubt || execution.FinishedAt != nil {
		t.Fatalf("in-doubt technical attempt did not fence logical sample = %+v", execution)
	}
	if _, err := s.CreateTrialAttempt(ctx, CreateTrialAttemptRequest{
		TrialExecutionID: execution.ID, RetryOfTrialAttemptID: first.ID, Ordinal: 2,
		Actor: "tester", Reason: "blind retry before reconcile",
	}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("retry from in_doubt attempt = %v, want ErrInvalidTransition", err)
	}
	first = transitionTrialAttempt(t, s, first, TrialAttemptReconciling, "", "")
	execution = getTrialExecution(t, s, execution.ID)
	if execution.Status != TrialExecutionReconciling || execution.FinishedAt != nil {
		t.Fatalf("reconciling technical attempt did not fence logical sample = %+v", execution)
	}
	first = transitionTrialAttempt(t, s, first, TrialAttemptInfraFailed, "receipt confirmed no completion", "transport")
	execution = getTrialExecution(t, s, execution.ID)
	if execution.Status != TrialExecutionRunning || execution.FinishedAt != nil {
		t.Fatalf("reconciled infra failure did not return logical sample to running = %+v", execution)
	}
	retry, err := s.CreateTrialAttempt(ctx, CreateTrialAttemptRequest{
		TrialExecutionID: execution.ID, RetryOfTrialAttemptID: first.ID, Ordinal: 2,
		Actor: "tester", Reason: "retry after reconciled failure",
	})
	if err != nil {
		t.Fatalf("retry after reconciled technical failure: %v", err)
	}
	if _, err := s.TransitionTrialExecution(ctx, TransitionTrialExecutionRequest{
		TrialExecutionID: execution.ID, ExpectedVersion: execution.Version, Status: TrialExecutionCompleted,
		Actor: "tester", Reason: "try to finalize while technical retry is active",
	}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("finalize with active technical retry error = %v, want ErrInvalidTransition", err)
	}
	retry = transitionTrialAttempt(t, s, retry, TrialAttemptRunning, "", "")
	retry = transitionTrialAttempt(t, s, retry, TrialAttemptCompleted, "", "")
	execution = getTrialExecution(t, s, execution.ID)
	if execution.Status != TrialExecutionRunning || execution.FinishedAt != nil {
		t.Fatalf("completed technical attempt finalized logical sample before policy = %+v", execution)
	}
	execution = transitionTrialExecution(t, s, execution, TrialExecutionCompleted)
	if execution.Status != TrialExecutionCompleted || execution.FinishedAt == nil {
		t.Fatalf("policy finalization did not complete logical sample = %+v", execution)
	}
	if _, err := s.TransitionTrialExecution(ctx, TransitionTrialExecutionRequest{
		TrialExecutionID: execution.ID, ExpectedVersion: execution.Version, Status: TrialExecutionRunning,
		Actor: "tester", Reason: "reopen terminal logical sample",
	}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("reopen terminal logical sample error = %v, want ErrInvalidTransition", err)
	}
	if _, err := s.TransitionTrialExecution(ctx, TransitionTrialExecutionRequest{
		TrialExecutionID: execution.ID, ExpectedVersion: execution.Version - 1, Status: TrialExecutionCompleted,
		Actor: "tester", Reason: "stale terminal transition",
	}); !errors.Is(err, ErrOptimisticLock) {
		t.Fatalf("stale logical sample transition error = %v, want ErrOptimisticLock", err)
	}
	if _, err := s.CreateTrialAttempt(ctx, CreateTrialAttemptRequest{
		TrialExecutionID: execution.ID, RetryOfTrialAttemptID: retry.ID, Ordinal: 3,
		Actor: "tester", Reason: "retry policy-finalized sample",
	}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("retry policy-finalized logical sample error = %v, want ErrInvalidTransition", err)
	}
}

func TestTrialAttemptInterruptionKeepsLogicalSampleContinuable(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	fixture := newTrialStoreFixture(t, s)
	execution := newTrialExecution(t, s, fixture, 1)
	first, err := s.CreateTrialAttempt(ctx, CreateTrialAttemptRequest{
		TrialExecutionID: execution.ID, Ordinal: 1, Actor: "tester", Reason: "start interruptible technical try",
	})
	if err != nil {
		t.Fatal(err)
	}
	first = transitionTrialAttempt(t, s, first, TrialAttemptRunning, "", "")
	first = transitionTrialAttempt(t, s, first, TrialAttemptInterrupted, "worker lease lost", "worker_interrupted")
	execution = getTrialExecution(t, s, execution.ID)
	if execution.Status != TrialExecutionRunning || execution.FinishedAt != nil {
		t.Fatalf("technical interruption terminalized logical sample = %+v", execution)
	}
	if _, err := s.CreateTrialAttempt(ctx, CreateTrialAttemptRequest{
		TrialExecutionID: execution.ID, RetryOfTrialAttemptID: first.ID, Ordinal: 2,
		Actor: "tester", Reason: "retry interrupted technical try",
	}); err != nil {
		t.Fatalf("retry interrupted technical try: %v", err)
	}
}

func TestTrialExecutionTerminalStatesRequireFinalPolicyTransition(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	fixture := newTrialStoreFixture(t, s)
	for ordinal, status := range []TrialExecutionStatus{
		TrialExecutionCompleted,
		TrialExecutionInfraFailed,
		TrialExecutionInterrupted,
		TrialExecutionCanceled,
	} {
		execution := newTrialExecution(t, s, fixture, ordinal+1)
		execution = transitionTrialExecution(t, s, execution, TrialExecutionRunning)
		updated, err := s.TransitionTrialExecution(ctx, TransitionTrialExecutionRequest{
			TrialExecutionID: execution.ID, ExpectedVersion: execution.Version, Status: status,
			Actor: "policy", Reason: "final policy disposition",
		})
		if err != nil {
			t.Fatalf("final policy transition to %s: %v", status, err)
		}
		if updated.Status != status || updated.FinishedAt == nil {
			t.Fatalf("final policy transition to %s = %+v", status, updated)
		}
	}
}

func TestTrialSchemaRejectsRawRebindingAndMutation(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	fixture := newTrialStoreFixture(t, s)
	now := time.Date(2026, 7, 14, 15, 0, 0, 0, time.UTC)

	if _, err := s.db.Exec(`
		INSERT INTO trial_executions_v19 (
			id, run_id, stage_attempt_id, stage_key, ordinal, status, created_at, updated_at, version
		) VALUES (?, ?, ?, ?, 1, 'queued', ?, ?, 1)
	`, mustUUIDv7(t), fixture.run.ID, fixture.stage.ID, "not_the_bound_stage", now, now); err == nil || !strings.Contains(err.Error(), "does not match stage attempt") {
		t.Fatalf("raw mismatched stage binding error = %v, want stage binding rejection", err)
	}

	execution := newTrialExecution(t, s, fixture, 1)
	if _, err := s.db.Exec(`UPDATE trial_executions_v19 SET ordinal = 2 WHERE id = ?`, execution.ID); err == nil || !strings.Contains(err.Error(), "logical identity is immutable") {
		t.Fatalf("raw logical sample rebinding error = %v, want immutable rejection", err)
	}
	if _, err := s.db.Exec(`DELETE FROM trial_executions_v19 WHERE id = ?`, execution.ID); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("raw logical sample deletion error = %v, want append-only rejection", err)
	}

	if _, err := s.db.Exec(`
		INSERT INTO trial_attempts_v19 (
			id, trial_execution_id, retry_of_trial_attempt_id, ordinal, status, created_at, updated_at, version
		) VALUES (?, ?, ?, 1, 'queued', ?, ?, 1)
	`, mustUUIDv7(t), execution.ID, mustUUIDv7(t), now, now); err == nil || !strings.Contains(err.Error(), "invalid trial retry lineage") {
		t.Fatalf("raw initial retry lineage error = %v, want lineage rejection", err)
	}
	if _, err := s.db.Exec(`
		INSERT INTO trial_attempts_v19 (
			id, trial_execution_id, retry_of_trial_attempt_id, ordinal, status, created_at, updated_at, version
		) VALUES (?, ?, NULL, 2, 'queued', ?, ?, 1)
	`, mustUUIDv7(t), execution.ID, now, now); err == nil || !strings.Contains(err.Error(), "invalid trial retry lineage") {
		t.Fatalf("raw missing retry predecessor error = %v, want lineage rejection", err)
	}
	first, err := s.CreateTrialAttempt(ctx, CreateTrialAttemptRequest{
		TrialExecutionID: execution.ID, Ordinal: 1, Actor: "tester", Reason: "raw mutation fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE trial_attempts_v19 SET ordinal = 2 WHERE id = ?`, first.ID); err == nil || !strings.Contains(err.Error(), "attempt identity is immutable") {
		t.Fatalf("raw technical attempt rebinding error = %v, want immutable rejection", err)
	}
	if _, err := s.db.Exec(`DELETE FROM trial_attempts_v19 WHERE id = ?`, first.ID); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("raw technical attempt deletion error = %v, want append-only rejection", err)
	}
	if _, err := s.db.Exec(`UPDATE trial_attempts_v19 SET status = 'completed' WHERE id = ?`, first.ID); err == nil || !strings.Contains(err.Error(), "invalid trial attempt status transition") {
		t.Fatalf("raw queued-to-completed transition error = %v, want state transition rejection", err)
	}
	if _, err := s.db.Exec(`UPDATE trial_executions_v19 SET status = 'canceled' WHERE id = ?`, execution.ID); err == nil || !strings.Contains(err.Error(), "active technical attempt") {
		t.Fatalf("raw terminal logical sample with active attempt error = %v, want active-attempt rejection", err)
	}
	first = transitionTrialAttempt(t, s, first, TrialAttemptRunning, "", "")
	if _, err := s.db.Exec(`UPDATE trial_attempts_v19 SET status = 'in_doubt' WHERE id = ?`, first.ID); err == nil || !strings.Contains(err.Error(), "does not match logical trial state") {
		t.Fatalf("raw in-doubt attempt without parent fence error = %v, want parent-state rejection", err)
	}
	first = transitionTrialAttempt(t, s, first, TrialAttemptInDoubt, "", "")
	if _, err := s.db.Exec(`UPDATE trial_attempts_v19 SET status = 'completed' WHERE id = ?`, first.ID); err == nil || !strings.Contains(err.Error(), "invalid trial attempt status transition") {
		t.Fatalf("raw in-doubt-to-completed transition error = %v, want reconciliation rejection", err)
	}
	first = transitionTrialAttempt(t, s, first, TrialAttemptReconciling, "", "")
	if _, err := s.db.Exec(`UPDATE trial_attempts_v19 SET status = 'infra_failed' WHERE id = ?`, first.ID); err == nil || !strings.Contains(err.Error(), "does not match logical trial state") {
		t.Fatalf("raw reconciliation failure without running parent error = %v, want parent-state rejection", err)
	}
	first = transitionTrialAttempt(t, s, first, TrialAttemptInfraFailed, "reconciled transport failure", "transport")
	if _, err := s.db.Exec(`UPDATE trial_executions_v19 SET status = 'waiting' WHERE id = ?`, execution.ID); err != nil {
		t.Fatalf("raw running-to-waiting logical transition: %v", err)
	}
	if _, err := s.db.Exec(`
		INSERT INTO trial_attempts_v19 (
			id, trial_execution_id, retry_of_trial_attempt_id, ordinal, status, created_at, updated_at, version
		) VALUES (?, ?, ?, 2, 'queued', ?, ?, 1)
	`, mustUUIDv7(t), execution.ID, first.ID, now, now); err == nil || !strings.Contains(err.Error(), "retry requires running logical execution") {
		t.Fatalf("raw retry before logical sample returns to running error = %v, want running rejection", err)
	}
}

func newTrialStoreFixture(t *testing.T, s *Store) trialStoreFixture {
	t.Helper()
	ctx := context.Background()
	task, revision := createValidatedTaskAndRevision(t, s)
	run, err := s.CreateWorkflowRun(ctx, CreateWorkflowRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, WorkflowTemplateID: "test.trial-execution", WorkflowTemplateVersion: "v1",
		ResolvedProfileHash: "trial-profile", DefinitionHash: "trial-definition-" + mustUUIDv7(t), RunManifestJSON: `{}`,
		Trigger: "trial-test", Actor: "tester", Reason: "trial fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	stage, err := s.CreateStageAttempt(ctx, CreateStageAttemptRequest{
		RunID: run.ID, StageKey: "evaluator", StageGroup: "evaluation", Ordinal: 1,
		InputFingerprint: "trial-input-fingerprint", BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`,
		Actor: "tester", Reason: "trial fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	return trialStoreFixture{run: run, stage: stage}
}

func newTrialExecution(t *testing.T, s *Store, fixture trialStoreFixture, ordinal int) TrialExecution {
	t.Helper()
	execution, err := s.CreateTrialExecution(context.Background(), CreateTrialExecutionRequest{
		RunID: fixture.run.ID, StageAttemptID: fixture.stage.ID, StageKey: fixture.stage.StageKey, Ordinal: ordinal,
		Actor: "tester", Reason: "logical evaluator sample fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	return execution
}

func failedTrialAttempt(t *testing.T, s *Store, trialExecutionID string) TrialAttempt {
	t.Helper()
	attempt, err := s.CreateTrialAttempt(context.Background(), CreateTrialAttemptRequest{
		TrialExecutionID: trialExecutionID, Ordinal: 1, Actor: "tester", Reason: "failed technical attempt fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt = transitionTrialAttempt(t, s, attempt, TrialAttemptRunning, "", "")
	return transitionTrialAttempt(t, s, attempt, TrialAttemptInfraFailed, "network reset", "transport")
}

func transitionTrialAttempt(t *testing.T, s *Store, attempt TrialAttempt, status TrialAttemptStatus, errorText, failureClass string) TrialAttempt {
	t.Helper()
	updated, err := s.TransitionTrialAttempt(context.Background(), TransitionTrialAttemptRequest{
		TrialAttemptID: attempt.ID, ExpectedVersion: attempt.Version, Status: status,
		ErrorText: errorText, FailureClass: failureClass, Actor: "tester", Reason: "trial attempt transition",
	})
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

func transitionTrialExecution(t *testing.T, s *Store, execution TrialExecution, status TrialExecutionStatus) TrialExecution {
	t.Helper()
	updated, err := s.TransitionTrialExecution(context.Background(), TransitionTrialExecutionRequest{
		TrialExecutionID: execution.ID, ExpectedVersion: execution.Version, Status: status,
		Actor: "tester", Reason: "trial execution transition",
	})
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

func getTrialExecution(t *testing.T, s *Store, trialExecutionID string) TrialExecution {
	t.Helper()
	execution, err := s.GetTrialExecution(context.Background(), trialExecutionID)
	if err != nil || execution == nil {
		t.Fatalf("get trial execution %s = %+v, %v", trialExecutionID, execution, err)
	}
	return *execution
}
