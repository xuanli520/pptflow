package store

import (
	"context"
	"errors"
	"testing"
)

func TestRunInputArtifactIsImmutableRunScopedAndIdempotent(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	task, revision := createValidatedTaskAndRevision(t, s)
	run, err := s.CreateWorkflowRun(ctx, CreateWorkflowRunRequest{
		TaskID: task.ID, RevisionID: revision.ID,
		WorkflowTemplateID: "harbor.test", WorkflowTemplateVersion: "v1",
		ResolvedProfileHash: "profile", DefinitionHash: "definition", RunManifestJSON: `{}`,
		Trigger: "test", Actor: "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	id, err := NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	request := CreateRunInputArtifactRequest{
		ID: id, RunID: run.ID, TaskID: task.ID, RevisionID: revision.ID, RevisionDigest: revision.TaskDigest,
		Port: "task_snapshot", ContentDigest: "sha256:run-input", SchemaVersion: "harbor.artifact.v1", SizeBytes: 17,
		IdempotencyKey: "run-input:" + run.ID + ":task_snapshot", Actor: "tester", Reason: "freeze subject input",
	}
	input, err := s.CreateRunInputArtifact(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if input.ID != id || input.RunID != run.ID || input.Port != "task_snapshot" {
		t.Fatalf("created run input = %+v", input)
	}
	replayed, err := s.CreateRunInputArtifact(ctx, request)
	if err != nil || replayed.ID != input.ID {
		t.Fatalf("replayed run input = %+v, %v", replayed, err)
	}
	otherID, err := NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	identitySubstitution := request
	identitySubstitution.ID = otherID
	if _, err := s.CreateRunInputArtifact(ctx, identitySubstitution); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("substituted input identity error = %v, want idempotency conflict", err)
	}
	portConflict := request
	portConflict.ID, err = NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	portConflict.IdempotencyKey = "run-input:" + run.ID + ":another-key"
	portConflict.ContentDigest = "sha256:other"
	if _, err := s.CreateRunInputArtifact(ctx, portConflict); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("same run/port conflict error = %v, want idempotency conflict", err)
	}
	loaded, err := s.GetRunInputArtifactForPort(ctx, run.ID, "task_snapshot")
	if err != nil || loaded == nil || loaded.ID != input.ID {
		t.Fatalf("loaded run input = %+v, %v", loaded, err)
	}
	if _, err := s.db.Exec(`UPDATE run_input_artifacts SET content_digest = 'sha256:mutated' WHERE id = ?`, input.ID); err == nil {
		t.Fatal("immutable run input accepted direct update")
	}
	if _, err := s.db.Exec(`DELETE FROM run_input_artifacts WHERE id = ?`, input.ID); err == nil {
		t.Fatal("immutable run input accepted direct delete")
	}
}

func TestCreateWorkflowRunAtomicallyPersistsInitialInputsDispatchAndOutbox(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	task, revision := createValidatedTaskAndRevision(t, s)
	runID, err := NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	inputID, err := NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateWorkflowRun(ctx, CreateWorkflowRunRequest{
		ID: runID, TaskID: task.ID, RevisionID: revision.ID,
		WorkflowTemplateID: "harbor.test", WorkflowTemplateVersion: "v1",
		ResolvedProfileHash: "profile", DefinitionHash: "definition", RunManifestJSON: `{}`,
		Trigger: "test", Actor: "tester", Reason: "atomic initial handoff",
		InitialInputArtifacts: []CreateRunInputArtifactRequest{{
			ID: inputID, RevisionDigest: revision.TaskDigest, Port: "task_snapshot",
			ContentDigest: "sha256:initial-input", SchemaVersion: "harbor.artifact.v1", SizeBytes: 29,
			IdempotencyKey: "initial-input:" + runID + ":task_snapshot",
		}},
		Dispatch: &WorkflowRunDispatchRequest{
			CommandType: "workflow_run.execute", PayloadJSON: `{"run_id":"` + runID + `"}`,
			IdempotencyKey: "initial-dispatch:" + runID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	input, err := s.GetRunInputArtifact(ctx, inputID)
	if err != nil || input == nil {
		t.Fatalf("initial run input = %+v, %v", input, err)
	}
	if input.RunID != run.ID || input.TaskID != task.ID || input.RevisionID != revision.ID || input.RevisionDigest != revision.TaskDigest || input.Port != "task_snapshot" {
		t.Fatalf("initial run input did not derive run subject: %+v", input)
	}
	jobs, err := s.ListDurableJobsForRun(ctx, run.ID)
	if err != nil || len(jobs) != 1 || jobs[0].IdempotencyKey != "initial-dispatch:"+runID {
		t.Fatalf("initial dispatch = %+v, %v", jobs, err)
	}
	events, err := s.ListPendingOutboxEvents(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Topic == "workflow_run.queued" && event.EntityID == run.ID && event.IdempotencyKey == "initial-dispatch:"+runID+":queued" {
			found = true
		}
	}
	if !found {
		t.Fatalf("initial workflow outbox event missing: %+v", events)
	}
}

func TestCreateWorkflowRunRollsBackWhenInitialInputViolatesRevisionSubject(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	task, revision := createValidatedTaskAndRevision(t, s)
	runID, err := NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	inputID, err := NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.CreateWorkflowRun(ctx, CreateWorkflowRunRequest{
		ID: runID, TaskID: task.ID, RevisionID: revision.ID,
		WorkflowTemplateID: "harbor.test", WorkflowTemplateVersion: "v1",
		ResolvedProfileHash: "profile", DefinitionHash: "definition", RunManifestJSON: `{}`,
		Trigger: "test", Actor: "tester", Reason: "reject malformed initial input",
		InitialInputArtifacts: []CreateRunInputArtifactRequest{{
			ID: inputID, RevisionDigest: validTaskDigest("f"), Port: "task_snapshot",
			ContentDigest: "sha256:wrong-revision", SchemaVersion: "harbor.artifact.v1", SizeBytes: 1,
			IdempotencyKey: "bad-initial-input:" + runID,
		}},
		Dispatch: &WorkflowRunDispatchRequest{
			CommandType: "workflow_run.execute", PayloadJSON: `{}`,
			IdempotencyKey: "bad-initial-dispatch:" + runID,
		},
	})
	if err == nil {
		t.Fatal("workflow run accepted an initial input for another immutable revision digest")
	}
	if run, lookupErr := s.GetWorkflowRun(ctx, runID); lookupErr != nil || run != nil {
		t.Fatalf("failed atomic create retained workflow run: %+v, %v", run, lookupErr)
	}
	if input, lookupErr := s.GetRunInputArtifact(ctx, inputID); lookupErr != nil || input != nil {
		t.Fatalf("failed atomic create retained run input: %+v, %v", input, lookupErr)
	}
	jobs, lookupErr := s.ListDurableJobsForRun(ctx, runID)
	if lookupErr != nil || len(jobs) != 0 {
		t.Fatalf("failed atomic create retained durable job: %+v, %v", jobs, lookupErr)
	}
	events, lookupErr := s.ListPendingOutboxEvents(ctx, 0)
	if lookupErr != nil {
		t.Fatal(lookupErr)
	}
	for _, event := range events {
		if event.EntityID == runID || event.IdempotencyKey == "bad-initial-dispatch:"+runID+":queued" {
			t.Fatalf("failed atomic create retained outbox event: %+v", event)
		}
	}
}
