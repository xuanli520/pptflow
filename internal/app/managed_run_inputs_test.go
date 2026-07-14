package app

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/testsupport"
	"github.com/purplevoid/harbor-factory/internal/workflowruntime"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestStartRunMaterializesManagedTaskSnapshotWithoutSyntheticStageLineage(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	services, err := NewLifecycleServicesWithOptions(root, database, LifecycleServicesOptions{OperationResolver: testsupport.AcceptAllStageOperationResolver()})
	if err != nil {
		t.Fatal(err)
	}
	task, revision, err := services.Tasks.ImportTask(ctx, ImportTaskRequest{
		CreateDraftTaskRequest: CreateDraftTaskRequest{Slug: "managed-input", Title: "Managed Input", Actor: "tester", Reason: "fixture"},
		SourceDirectory:        writeLifecycleSnapshot(t, "managed run input\n"), ChangeSummary: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	requested := testsupport.CompleteCodeEdgePhase1RunExecutionSpec(task.ID, revision.ID, revision.TaskDigest)
	request := StartRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, Profile: codeEdgeEvaluatorRuntimeProfile(t), ExecutionSpec: requested,
		Trigger: "managed-input-test", Actor: "tester", Reason: "freeze managed task snapshot",
	}
	run, err := services.Runs.StartRun(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := decodeRunManifest(run)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Inputs.RequestedExecutionSpecFingerprint == "" || manifest.Inputs.ExecutionSpecFingerprint == "" || manifest.Inputs.RequestedExecutionSpecFingerprint == manifest.Inputs.ExecutionSpecFingerprint {
		t.Fatalf("run manifest did not retain distinct requested/final specification fingerprints: %+v", manifest.Inputs)
	}
	if len(manifest.Inputs.ManagedInputs) != 1 {
		t.Fatalf("managed inputs = %+v, want one task snapshot", manifest.Inputs.ManagedInputs)
	}
	input := manifest.Inputs.ManagedInputs[0]
	if input.Port != managedTaskSnapshotInputPort || input.RevisionDigest != workflowkit.SubjectDigest(revision.TaskDigest) {
		t.Fatalf("managed task snapshot binding = %+v", input)
	}
	persisted, err := database.GetRunInputArtifact(ctx, input.ID)
	if err != nil || persisted == nil || persisted.RunID != run.ID || persisted.ContentDigest != string(input.ContentDigest) || persisted.SizeBytes != input.SizeBytes {
		t.Fatalf("durable managed task snapshot = %+v, %v", persisted, err)
	}
	object, err := materializeManagedTaskSnapshotObject(ctx, services.core, revision)
	if err != nil {
		t.Fatal(err)
	}
	if object != input.objectRef() {
		t.Fatalf("deterministic snapshot object = %+v, want %+v", object, input.objectRef())
	}
	attempts, err := database.ListStageAttemptsForRun(ctx, run.ID)
	if err != nil || len(attempts) != 0 {
		t.Fatalf("StartRun created synthetic stage lineage: %+v, %v", attempts, err)
	}
	stage, found := manifest.Resolved.Descriptor.Stage(workflowkit.StageKey(workflowadapter.RepoPrepare))
	if !found {
		t.Fatal("CodeEdge repo_prepare stage is missing")
	}
	bindings, err := resolveStageInputs(ctx, database, services.core.objects, run, revision, stage)
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 || bindings[0].Name != managedTaskSnapshotInputPort || bindings[0].ArtifactID != workflowkit.ArtifactID(input.ID) || bindings[0].ContentDigest != input.ContentDigest || bindings[0].SchemaVersion != input.SchemaVersion {
		t.Fatalf("resolved root managed input = %+v", bindings)
	}
	content, err := newStageInputReader(database, services.core.objects, run, revision, bindings)(ctx, bindings[0])
	if err != nil || len(content) != int(input.SizeBytes) {
		t.Fatalf("read managed task snapshot = %d bytes, %v", len(content), err)
	}

	// Same caller request and Run identity reuses the immutable manifest/input
	// rather than allocating another input or starting a worker early.
	request.ID = run.ID
	replayed, err := services.Runs.StartRun(ctx, request)
	if err != nil || replayed.ID != run.ID {
		t.Fatalf("replayed managed StartRun = %+v, %v", replayed, err)
	}
	byPort, err := database.GetRunInputArtifactForPort(ctx, run.ID, managedTaskSnapshotInputPort)
	if err != nil || byPort == nil || byPort.ID != input.ID {
		t.Fatalf("replayed managed input = %+v, %v", byPort, err)
	}

	path, err := services.core.objects.ObjectPath(input.objectRef())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveStageInputs(ctx, database, services.core.objects, run, revision, stage); !errors.Is(err, workflowruntime.ErrObjectNotFound) {
		t.Fatalf("missing managed snapshot object error = %v, want object-not-found", err)
	}
}
