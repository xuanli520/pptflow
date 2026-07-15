package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestCommitRevisionCandidateContinuationAtomicallyPersistsChildRunInputsAndReplays(t *testing.T) {
	ctx := context.Background()
	fixture, request, inputID := prepareCandidateContinuationCommitFixture(t)

	if input, err := fixture.store.GetRunInputArtifact(ctx, inputID); err != nil || input != nil {
		t.Fatalf("child input existed before candidate continuation commit: %+v, %v", input, err)
	}
	committed, err := fixture.store.CommitRevisionCandidateContinuation(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if committed.Candidate.State != RevisionCandidateCommitted || committed.Revision.ID != fixture.candidate.TargetRevisionID || committed.Run.ID != fixture.candidate.TargetRunID {
		t.Fatalf("candidate continuation commit = %+v", committed)
	}
	if revision, err := fixture.store.GetTaskRevision(ctx, fixture.candidate.TargetRevisionID); err != nil || revision == nil || revision.ID != committed.Revision.ID {
		t.Fatalf("committed child revision = %+v, %v", revision, err)
	}
	if run, err := fixture.store.GetWorkflowRun(ctx, fixture.candidate.TargetRunID); err != nil || run == nil || run.ID != committed.Run.ID || run.RevisionID != committed.Revision.ID {
		t.Fatalf("committed child run = %+v, %v", run, err)
	}
	for _, identity := range []struct {
		id         string
		entityType string
	}{
		{id: committed.Revision.ID, entityType: "task_revision"},
		{id: committed.Run.ID, entityType: "workflow_run"},
	} {
		var entityType string
		if err := fixture.store.db.QueryRow(`SELECT entity_type FROM entity_id_registry WHERE id = ?`, identity.id).Scan(&entityType); err != nil {
			t.Fatalf("read promoted identity %s: %v", identity.id, err)
		}
		if entityType != identity.entityType {
			t.Fatalf("promoted identity %s registry type=%q, want %q", identity.id, entityType, identity.entityType)
		}
	}
	input, err := fixture.store.GetRunInputArtifact(ctx, inputID)
	if err != nil || input == nil {
		t.Fatalf("committed child run input = %+v, %v", input, err)
	}
	if input.RunID != committed.Run.ID || input.TaskID != committed.Run.TaskID || input.RevisionID != committed.Revision.ID ||
		input.RevisionDigest != committed.Revision.TaskDigest || input.Port != "task_snapshot" || input.ContentDigest != "sha256:candidate-task-snapshot" {
		t.Fatalf("child input does not bind the committed child subject: %+v", input)
	}
	if execution, err := fixture.store.GetContinuationExecution(ctx, committed.Execution.ID); err != nil || execution == nil || execution.RunID != committed.Run.ID {
		t.Fatalf("committed continuation execution = %+v, %v", execution, err)
	}
	if job, err := fixture.store.GetDurableJob(ctx, committed.Job.ID); err != nil || job == nil || job.RunID != committed.Run.ID || job.EntityID != committed.Execution.ID {
		t.Fatalf("committed continuation job = %+v, %v", job, err)
	}

	replayed, err := fixture.store.CommitRevisionCandidateContinuation(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Revision.ID != committed.Revision.ID || replayed.Run.ID != committed.Run.ID || replayed.Execution.ID != committed.Execution.ID || replayed.Job.ID != committed.Job.ID {
		t.Fatalf("candidate continuation replay = %+v, want %+v", replayed, committed)
	}
	replayedInput, err := fixture.store.GetRunInputArtifact(ctx, inputID)
	if err != nil || replayedInput == nil || replayedInput.ID != input.ID || replayedInput.IdempotencyKey != input.IdempotencyKey {
		t.Fatalf("candidate continuation replay input = %+v, %v", replayedInput, err)
	}
	var inputCount int
	if err := fixture.store.db.QueryRow(`SELECT COUNT(*) FROM run_input_artifacts WHERE run_id = ?`, committed.Run.ID).Scan(&inputCount); err != nil {
		t.Fatal(err)
	}
	if inputCount != 1 {
		t.Fatalf("candidate continuation replay input count = %d, want 1", inputCount)
	}
}

func TestCommitRevisionCandidateContinuationRollsBackWhenChildInputInsertFails(t *testing.T) {
	ctx := context.Background()
	fixture, request, inputID := prepareCandidateContinuationCommitFixture(t)
	if _, err := fixture.store.db.Exec(`
		CREATE TRIGGER force_candidate_child_run_input_failure
		BEFORE INSERT ON run_input_artifacts
		BEGIN
			SELECT RAISE(ABORT, 'forced candidate child run input failure');
		END
	`); err != nil {
		t.Fatal(err)
	}

	if _, err := fixture.store.CommitRevisionCandidateContinuation(ctx, request); err == nil {
		t.Fatal("candidate continuation committed after forced child input failure")
	}
	if revision, err := fixture.store.GetTaskRevision(ctx, fixture.candidate.TargetRevisionID); err != nil || revision != nil {
		t.Fatalf("failed candidate continuation retained child revision: %+v, %v", revision, err)
	}
	if run, err := fixture.store.GetWorkflowRun(ctx, fixture.candidate.TargetRunID); err != nil || run != nil {
		t.Fatalf("failed candidate continuation retained child run: %+v, %v", run, err)
	}
	if input, err := fixture.store.GetRunInputArtifact(ctx, inputID); err != nil || input != nil {
		t.Fatalf("failed candidate continuation retained child input: %+v, %v", input, err)
	}
	if execution, err := fixture.store.GetContinuationExecutionByIdempotency(ctx, request.IdempotencyKey); err != nil || execution != nil {
		t.Fatalf("failed candidate continuation retained execution: %+v, %v", execution, err)
	}
	if job, err := fixture.store.GetDurableJobByIdempotency(ctx, "candidate-continuation-job:"+request.IdempotencyKey); err != nil || job != nil {
		t.Fatalf("failed candidate continuation retained job: %+v, %v", job, err)
	}
	if candidate, err := fixture.store.GetRevisionCandidate(ctx, fixture.candidate.ID); err != nil || candidate == nil || candidate.State != RevisionCandidatePrepared || candidate.Version != fixture.candidate.Version {
		t.Fatalf("failed candidate continuation changed candidate: %+v, %v", candidate, err)
	}
	if sourceRun, err := fixture.store.GetWorkflowRun(ctx, fixture.run.ID); err != nil || sourceRun == nil || sourceRun.ExecutionEpoch != fixture.run.ExecutionEpoch || sourceRun.Version != fixture.run.Version {
		t.Fatalf("failed candidate continuation changed source run: %+v, %v", sourceRun, err)
	}
}

func prepareCandidateContinuationCommitFixture(t *testing.T) (revisionCandidatePlanFixture, CommitRevisionCandidateContinuationRequest, string) {
	t.Helper()
	fixture := prepareRevisionCandidatePlanFixture(t)
	expiresAt := fixture.store.now().UTC().Add(time.Hour)
	planRequest := fixture.planRequest(t, expiresAt)
	checkpoint := ControlCheckpointRef{
		Sequence:            uint64(fixture.run.Version),
		ExecutionEpoch:      fixture.run.ExecutionEpoch,
		SubjectVersion:      fixture.task.Version,
		SubjectID:           fixture.task.ID,
		SubjectRevisionID:   fixture.revision.ID,
		SubjectDigest:       fixture.revision.TaskDigest,
		WorkflowFingerprint: fixture.run.DefinitionHash,
	}
	snapshot := workflowkit.ContinuationPlanSnapshot{
		PlanID:             planRequest.ID,
		CommandID:          fixture.command.ID,
		Strategy:           workflowkit.StrategyReviseSubject,
		BaseCheckpoint:     workflowkit.CheckpointRef{Sequence: checkpoint.Sequence, ExecutionEpoch: checkpoint.ExecutionEpoch, SubjectVersion: checkpoint.SubjectVersion, SubjectID: checkpoint.SubjectID, SubjectRevisionID: checkpoint.SubjectRevisionID, SubjectDigest: workflowkit.SubjectDigest(checkpoint.SubjectDigest), WorkflowFingerprint: workflowkit.Fingerprint(checkpoint.WorkflowFingerprint)},
		NextExecutionEpoch: fixture.run.ExecutionEpoch + 1,
		SourceRunID:        fixture.run.ID,
		TargetRunRelation:  workflowkit.RelationChildRun,
		PreparedChangeID:   fixture.change.ID,
		SubjectRevisionID:  fixture.candidate.TargetRevisionID,
		SubjectDigest:      workflowkit.SubjectDigest(fixture.candidate.AfterDigest),
		ExpiresAt:          expiresAt,
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	planRequest.PayloadJSON = string(payload)
	plan, candidate, err := fixture.store.CreateAndBindRevisionCandidatePlan(context.Background(), CreateAndBindRevisionCandidatePlanRequest{
		Plan: planRequest, CandidateID: fixture.candidate.ID, ExpectedCandidateVersion: fixture.candidate.Version,
		FinalManifestID: "candidate-final-manifest", ChildRunManifestJSON: `{}`, Actor: "tester", Reason: "freeze candidate continuation",
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.candidate = candidate
	inputID := mustUUIDv7(t)
	request := CommitRevisionCandidateContinuationRequest{
		ID:             mustUUIDv7(t),
		PlanID:         plan.ID,
		IdempotencyKey: "candidate-continuation:" + candidate.ID,
		PayloadJSON:    `{"continuation":"candidate"}`,
		Expected:       checkpoint,
		ChildRunInputs: []CreateRunInputArtifactRequest{{
			ID: inputID, RevisionDigest: candidate.AfterDigest, Port: "task_snapshot",
			ContentDigest: "sha256:candidate-task-snapshot", SchemaVersion: "harbor.task_snapshot.v1", SizeBytes: 42,
			IdempotencyKey: "candidate-run-input:" + candidate.TargetRunID + ":task_snapshot",
		}},
		Actor: "tester", Reason: "commit candidate continuation", Priority: 7,
	}
	return fixture, request, inputID
}
