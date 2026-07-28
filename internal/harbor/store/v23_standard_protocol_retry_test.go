package store

import (
	"context"
	"errors"
	"testing"
)

func TestCommitStageProtocolRetryCreatesFencedAttemptJobAndRunEpoch(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	run, source := protocolRetryFixture(t, ctx, s)

	retryID, err := NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	request := CommitStageProtocolRetryRequest{
		SourceStageAttemptID: source.ID, ExpectedSourceStageAttemptVersion: source.Version,
		ExpectedSourceErrorText: source.ErrorText, ExpectedSourceFailureClass: source.FailureClass,
		ExpectedRunVersion: run.Version, ExpectedRunExecutionEpoch: run.ExecutionEpoch,
		NewStageAttemptID: retryID, NewGeneration: 1,
		RetrySnapshotJSON: `{"format":"harbor.runtime-stage-attempt.v1","execution_key":"initial","generation":1,"retry":{}}`,
		JobID:             jobID, JobPayloadJSON: `{"format":"test.retry","generation":1}`, IdempotencyKey: "standard-protocol-retry-command-1",
		Actor: "tester", Reason: "operator confirmed protocol retry",
	}
	committed, err := s.CommitStageProtocolRetry(ctx, request)
	if err != nil {
		t.Fatalf("commit protocol retry: %v", err)
	}
	if committed.Replayed || committed.Source.ID != source.ID || committed.Retry.ID != retryID || committed.Retry.RetryOfStageAttemptID != source.ID ||
		committed.Retry.Ordinal != source.Ordinal+1 || committed.Retry.InputFingerprint != source.InputFingerprint ||
		committed.Retry.BudgetSnapshotJSON != source.BudgetSnapshotJSON || committed.Retry.ExecutionStatus != StageExecutionQueued {
		t.Fatalf("retry commit = %+v", committed)
	}
	if committed.Job.ID != jobID || committed.Job.State != JobQueued || committed.Job.RunID != run.ID || committed.Job.StageAttemptID != committed.Retry.ID ||
		committed.Job.EntityID != committed.Retry.ID || committed.Job.IdempotencyKey != request.IdempotencyKey {
		t.Fatalf("retry job = %+v", committed.Job)
	}
	if committed.Run.Status != WorkflowRunRunning || committed.Run.ExecutionEpoch != run.ExecutionEpoch+1 || committed.Run.Version != run.Version+1 || committed.Run.FinishedAt != nil {
		t.Fatalf("retry run projection = %+v", committed.Run)
	}
	if committed.RunAttempt.RunID != run.ID || committed.RunAttempt.Status != RunAttemptRunning || committed.RunAttempt.ResumeFrom != "stage_attempt:"+source.ID {
		t.Fatalf("retry run attempt = %+v", committed.RunAttempt)
	}
	pending, err := s.ListPendingOutboxEvents(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, event := range pending {
		if event.Topic == DurableJobQueuedOutboxTopic && event.EntityID == committed.Job.ID && event.IdempotencyKey == request.IdempotencyKey+":queued" {
			found = true
		}
	}
	if !found {
		t.Fatalf("retry job outbox event missing from %+v", pending)
	}

	// A retried client command need not retain generated child IDs or payload.
	// The idempotency key returns the immutable original retry before checking
	// the now-advanced Run checkpoint.
	replayRequest := request
	replayRequest.NewStageAttemptID, _ = NewUUIDv7()
	replayRequest.JobID, _ = NewUUIDv7()
	replayRequest.JobPayloadJSON = `{"different":"ignored-on-replay"}`
	replayed, err := s.CommitStageProtocolRetry(ctx, replayRequest)
	if err != nil || !replayed.Replayed || replayed.Retry.ID != committed.Retry.ID || replayed.Job.ID != committed.Job.ID || replayed.Run.ID != committed.Run.ID {
		t.Fatalf("retry replay = %+v, %v", replayed, err)
	}
	runningRetry, err := s.TransitionStageAttempt(ctx, TransitionStageAttemptRequest{
		StageAttemptID: committed.Retry.ID, ExpectedVersion: committed.Retry.Version, ExecutionStatus: StageExecutionRunning,
		Actor: "worker", Reason: "prove client retry replay survives worker start",
	})
	if err != nil {
		t.Fatal(err)
	}
	runningReplay, err := s.CommitStageProtocolRetry(ctx, replayRequest)
	if err != nil || !runningReplay.Replayed || runningReplay.Retry.ID != committed.Retry.ID || runningReplay.Retry.ExecutionStatus != StageExecutionRunning || runningReplay.Retry.Version != runningRetry.Version {
		t.Fatalf("running retry replay = %+v, %v", runningReplay, err)
	}

	second := request
	second.IdempotencyKey = "standard-protocol-retry-command-2"
	second.ExpectedRunVersion = committed.Run.Version
	second.ExpectedRunExecutionEpoch = committed.Run.ExecutionEpoch
	second.NewStageAttemptID, _ = NewUUIDv7()
	second.JobID, _ = NewUUIDv7()
	if _, err := s.CommitStageProtocolRetry(ctx, second); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("second retry err = %v, want source retry-chain rejection", err)
	}
}

func TestCommitStageProtocolRetryRejectsStaleSourceWithoutSideEffects(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	run, source := protocolRetryFixture(t, ctx, s)
	retryID, _ := NewUUIDv7()
	jobID, _ := NewUUIDv7()
	_, err := s.CommitStageProtocolRetry(ctx, CommitStageProtocolRetryRequest{
		SourceStageAttemptID: source.ID, ExpectedSourceStageAttemptVersion: source.Version - 1,
		ExpectedSourceErrorText: source.ErrorText, ExpectedSourceFailureClass: source.FailureClass,
		ExpectedRunVersion: run.Version, ExpectedRunExecutionEpoch: run.ExecutionEpoch,
		NewStageAttemptID: retryID, NewGeneration: 1, RetrySnapshotJSON: `{}`,
		JobID: jobID, JobPayloadJSON: `{}`, IdempotencyKey: "standard-protocol-retry-stale", Actor: "tester", Reason: "prove stale source fence",
	})
	if !errors.Is(err, ErrOptimisticLock) {
		t.Fatalf("stale retry err = %v, want optimistic lock", err)
	}
	attempts, listErr := s.ListStageAttemptsForRun(ctx, run.ID)
	if listErr != nil || len(attempts) != 1 || attempts[0].ID != source.ID {
		t.Fatalf("stale retry mutated attempts = %+v, %v", attempts, listErr)
	}
	updated, getErr := s.GetWorkflowRun(ctx, run.ID)
	if getErr != nil || updated == nil || updated.Version != run.Version || updated.ExecutionEpoch != run.ExecutionEpoch || updated.Status != WorkflowRunFailedRecoverable {
		t.Fatalf("stale retry mutated run = %+v, %v", updated, getErr)
	}
}

func protocolRetryFixture(t *testing.T, ctx context.Context, s *Store) (WorkflowRun, StageAttempt) {
	t.Helper()
	task, revision := createValidatedTaskAndRevision(t, s)
	run, err := s.CreateWorkflowRun(ctx, CreateWorkflowRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, WorkflowTemplateID: "retry-fixture", WorkflowTemplateVersion: "1",
		ResolvedProfileHash: "retry-profile", DefinitionHash: "retry-definition", RunManifestJSON: `{"fixture":"retry"}`,
		Trigger: "retry fixture", Actor: "tester", Reason: "create protocol retry fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err = s.TransitionWorkflowRun(ctx, TransitionWorkflowRunRequest{RunID: run.ID, ExpectedVersion: run.Version, Status: WorkflowRunRunning, Actor: "tester", Reason: "start protocol retry fixture"})
	if err != nil {
		t.Fatal(err)
	}
	source, err := s.CreateStageAttempt(ctx, CreateStageAttemptRequest{
		RunID: run.ID, StageKey: "agent_stage", StageGroup: "authoring", Ordinal: 1, InputFingerprint: "sha256:frozen-inputs",
		BudgetSnapshotJSON: `{"max_attempts":1}`, RetrySnapshotJSON: `{"format":"harbor.runtime-stage-attempt.v1","execution_key":"initial","generation":0,"retry":{}}`,
		Actor: "tester", Reason: "create failed agent attempt",
	})
	if err != nil {
		t.Fatal(err)
	}
	source, err = s.TransitionStageAttempt(ctx, TransitionStageAttemptRequest{StageAttemptID: source.ID, ExpectedVersion: source.Version, ExecutionStatus: StageExecutionRunning, Actor: "tester", Reason: "start failed agent attempt"})
	if err != nil {
		t.Fatal(err)
	}
	source, err = s.TransitionStageAttempt(ctx, TransitionStageAttemptRequest{
		StageAttemptID: source.ID, ExpectedVersion: source.Version, ExecutionStatus: StageExecutionInfraFailed,
		ErrorText: "standard_authoring_v3_agent_protocol.missing_submission", FailureClass: "process", Actor: "tester", Reason: "record protocol failure",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err = s.TransitionWorkflowRun(ctx, TransitionWorkflowRunRequest{RunID: run.ID, ExpectedVersion: run.Version, Status: WorkflowRunFailedRecoverable, Actor: "tester", Reason: "stage protocol failure is recoverable"})
	if err != nil {
		t.Fatal(err)
	}
	return run, source
}
