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
