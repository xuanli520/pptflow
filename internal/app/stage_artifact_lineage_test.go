package app

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/workflowruntime"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestPersistStageArtifactsBindsImmutableOutputToFrozenLineage(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dataStore, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	services, err := newLifecycleServicesForTest(root, dataStore)
	if err != nil {
		t.Fatal(err)
	}
	task, revision, err := services.Tasks.ImportTask(ctx, ImportTaskRequest{
		CreateDraftTaskRequest: CreateDraftTaskRequest{Slug: "artifact-lineage", Actor: "tester", Reason: "fixture"},
		SourceDirectory:        writeLifecycleSnapshot(t, "artifact lineage\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := services.Runs.StartRun(ctx, StartRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, Profile: lifecycleCompleteProfile(t), ExecutionSpec: lifecycleExecutionSpec(task.ID, revision.ID, revision.TaskDigest), Trigger: "verify", Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err = dataStore.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunRunning, Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	emptyInputs, err := workflowkit.FingerprintArtifactBindings(nil)
	if err != nil {
		t.Fatal(err)
	}
	stageAttempt, err := dataStore.CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: run.ID, StageKey: "source", StageGroup: "source_prepare", Ordinal: 1, InputFingerprint: string(emptyInputs),
		BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`, Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	stageAttempt, err = dataStore.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: stageAttempt.ID, ExpectedVersion: stageAttempt.Version, ExecutionStatus: store.StageExecutionRunning, Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	nodeAttempt, err := dataStore.CreateNodeAttempt(ctx, store.CreateNodeAttemptRequest{
		StageAttemptID: stageAttempt.ID, NodeID: "source", Generation: 0, Attempt: 1, IdempotencyKey: "artifact-lineage-node", Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	nodeAttempt, err = dataStore.TransitionNodeAttempt(ctx, store.TransitionNodeAttemptRequest{
		NodeAttemptID: nodeAttempt.ID, ExpectedVersion: nodeAttempt.Version, Status: store.NodeAttemptRunning, Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	stage := workflowkit.StageDescriptor{
		Key: "source", Version: "v1", Outputs: []workflowkit.ArtifactSpec{{Name: "report", SchemaVersion: "v1", Required: true}},
	}
	manifest, references, err := persistStageArtifacts(ctx, services.core, run, revision, stageAttempt, nodeAttempt, stage, nil, []StageArtifact{{
		Key: "report", SchemaVersion: "v1", Content: []byte("actual report\n"), TurnOrdinal: 1,
	}}, "tester", "persist actual output")
	if err != nil {
		t.Fatal(err)
	}
	if len(references) != 1 || references[0].ManifestID != manifest.ID || references[0].SubjectRevisionID != revision.ID || references[0].WorkflowFingerprint != run.DefinitionHash {
		t.Fatalf("artifact refs = %+v", references)
	}
	stageAttempt, err = dataStore.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: stageAttempt.ID, ExpectedVersion: stageAttempt.Version, ExecutionStatus: store.StageExecutionCompleted,
		Verdict: store.VerdictPass, ArtifactManifestID: manifest.ID, Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dataStore.TransitionNodeAttempt(ctx, store.TransitionNodeAttemptRequest{
		NodeAttemptID: nodeAttempt.ID, ExpectedVersion: nodeAttempt.Version, Status: store.NodeAttemptCompleted, Actor: "tester", Reason: "fixture",
	}); err != nil {
		t.Fatal(err)
	}
	child := workflowkit.StageDescriptor{
		Key: "child", Version: "v1", Inputs: []workflowkit.ArtifactSpec{{Name: "report", SchemaVersion: "v1", Required: true}},
	}
	bindings, err := resolveStageInputs(ctx, dataStore, services.core.objects, run, revision, child)
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 || bindings[0].ArtifactID != workflowkit.ArtifactID(references[0].ID) || bindings[0].ContentDigest != workflowkit.Fingerprint(references[0].ContentDigest) {
		t.Fatalf("resolved immutable input bindings = %+v", bindings)
	}
	reader := newStageInputReader(dataStore, services.core.objects, run, revision, bindings)
	content, err := reader(ctx, bindings[0])
	if err != nil || string(content) != "actual report\n" {
		t.Fatalf("read frozen stage input = %q, %v", content, err)
	}
	forged := bindings[0]
	forged.Name = "another-report"
	if _, err := reader(ctx, forged); !errors.Is(err, ErrInvalidStageExecution) {
		t.Fatalf("read forged stage binding = %v, want ErrInvalidStageExecution", err)
	}
	// Replaying the same physical output uses the stage-attempt idempotency
	// keys and cannot create a second manifest/ref lineage record.
	replayed, replayedRefs, err := persistStageArtifacts(ctx, services.core, run, revision, stageAttempt, nodeAttempt, stage, nil, []StageArtifact{{
		Key: "report", SchemaVersion: "v1", Content: []byte("actual report\n"), TurnOrdinal: 1,
	}}, "tester", "persist actual output")
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ID != manifest.ID || len(replayedRefs) != 1 || replayedRefs[0].ID != references[0].ID {
		t.Fatalf("artifact replay changed immutable lineage: manifest=%+v refs=%+v", replayed, replayedRefs)
	}
	if _, _, err := persistStageArtifacts(ctx, services.core, run, revision, stageAttempt, nodeAttempt, stage, nil, []StageArtifact{{
		Key: "report", SchemaVersion: "v1", Content: []byte("different report\n"), TurnOrdinal: 1,
	}}, "tester", "reject changed replay"); !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Fatalf("changed output replay error = %v, want ErrIdempotencyConflict", err)
	}

	manifestIndex, err := loadStageArtifactManifestIndex(ctx, dataStore, manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	object, err := manifestIndex.objectFor(references[0])
	if err != nil {
		t.Fatal(err)
	}
	path, err := services.core.objects.ObjectPath(object)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove immutable object for corruption fixture: %v", err)
	}
	if err := os.WriteFile(path, []byte("tampered report\n"), 0o600); err != nil {
		t.Fatalf("write corrupt immutable object: %v", err)
	}
	if _, err := resolveStageInputs(ctx, dataStore, services.core.objects, run, revision, child); !errors.Is(err, workflowruntime.ErrObjectCorrupt) {
		t.Fatalf("corrupt object input resolution error = %v, want ErrObjectCorrupt", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove immutable object for missing fixture: %v", err)
	}
	if _, err := resolveStageInputs(ctx, dataStore, services.core.objects, run, revision, child); !errors.Is(err, workflowruntime.ErrObjectNotFound) {
		t.Fatalf("missing object input resolution error = %v, want ErrObjectNotFound", err)
	}
	optionalChild := child
	optionalChild.Inputs[0].Required = false
	if bindings, err := resolveStageInputs(ctx, dataStore, services.core.objects, run, revision, optionalChild); err != nil || len(bindings) != 0 {
		t.Fatalf("missing optional object input = %+v, %v; want unavailable binding omitted", bindings, err)
	}
}
