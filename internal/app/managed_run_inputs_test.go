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
	database, err := store.OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	services, err := NewLifecycleServicesWithOptions(root, database, LifecycleServicesOptions{OperationResolver: testsupport.AcceptAllStageOperationResolver()})
	if err != nil {
		t.Fatal(err)
	}
	task, revision := createStandardMaterializedLifecycleTask(t, ctx, services, "managed-input", "Managed Input", "managed run input\n")
	requested := testsupport.CompleteCodeEdgePhase1RunExecutionSpec(task.ID, revision.ID, revision.TaskDigest)
	request := StartRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, Profile: codeEdgePhase1RuntimeProfile(t), ExecutionSpec: requested,
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
	if len(manifest.Inputs.ManagedInputs) != 2 {
		t.Fatalf("managed inputs = %+v, want task and source snapshots", manifest.Inputs.ManagedInputs)
	}
	taskInput := managedInputByPort(t, manifest.Inputs.ManagedInputs, managedTaskSnapshotInputPort)
	sourceInput := managedInputByPort(t, manifest.Inputs.ManagedInputs, workflowadapter.CodeEdgeSourceSnapshotArtifact)
	if taskInput.RevisionDigest != workflowkit.SubjectDigest(revision.TaskDigest) || taskInput.SchemaVersion != "harbor.artifact.v1" {
		t.Fatalf("managed task snapshot binding = %+v", taskInput)
	}
	if sourceInput.RevisionDigest != workflowkit.SubjectDigest(revision.TaskDigest) || sourceInput.SchemaVersion != workflowadapter.CodeEdgeSourceSnapshotSchemaVersion {
		t.Fatalf("managed source snapshot binding = %+v", sourceInput)
	}
	for _, input := range []runManifestManagedInput{taskInput, sourceInput} {
		persisted, err := database.GetRunInputArtifact(ctx, input.ID)
		if err != nil || persisted == nil || persisted.RunID != run.ID || persisted.Port != input.Port || persisted.ContentDigest != string(input.ContentDigest) || persisted.SizeBytes != input.SizeBytes {
			t.Fatalf("durable managed %s = %+v, %v", input.Port, persisted, err)
		}
	}
	object, err := materializeManagedTaskSnapshotObject(ctx, services.core, revision)
	if err != nil {
		t.Fatal(err)
	}
	if object != taskInput.objectRef() {
		t.Fatalf("deterministic task snapshot object = %+v, want %+v", object, taskInput.objectRef())
	}
	sourceObject, err := materializeManagedSourceSnapshotObject(ctx, services.core, revision)
	if err != nil {
		t.Fatal(err)
	}
	if sourceObject != sourceInput.objectRef() {
		t.Fatalf("deterministic source snapshot object = %+v, want %+v", sourceObject, sourceInput.objectRef())
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
	if len(bindings) != 2 {
		t.Fatalf("resolved root managed input = %+v", bindings)
	}
	for _, input := range []runManifestManagedInput{taskInput, sourceInput} {
		binding := artifactBindingByName(t, bindings, input.Port)
		if binding.ArtifactID != workflowkit.ArtifactID(input.ID) || binding.ContentDigest != input.ContentDigest || binding.SchemaVersion != input.SchemaVersion {
			t.Fatalf("resolved %s binding = %+v, want %+v", input.Port, binding, input)
		}
		content, err := newStageInputReader(database, services.core.objects, run, revision, bindings)(ctx, binding)
		if err != nil || len(content) != int(input.SizeBytes) {
			t.Fatalf("read managed %s = %d bytes, %v", input.Port, len(content), err)
		}
	}

	// Same caller request and Run identity reuses the immutable manifest/input
	// rather than allocating another input or starting a worker early.
	request.ID = run.ID
	replayed, err := services.Runs.StartRun(ctx, request)
	if err != nil || replayed.ID != run.ID {
		t.Fatalf("replayed managed StartRun = %+v, %v", replayed, err)
	}
	for _, input := range []runManifestManagedInput{taskInput, sourceInput} {
		byPort, err := database.GetRunInputArtifactForPort(ctx, run.ID, input.Port)
		if err != nil || byPort == nil || byPort.ID != input.ID {
			t.Fatalf("replayed managed %s input = %+v, %v", input.Port, byPort, err)
		}
	}

	path, err := services.core.objects.ObjectPath(taskInput.objectRef())
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

func managedInputByPort(t *testing.T, inputs []runManifestManagedInput, port string) runManifestManagedInput {
	t.Helper()
	for _, input := range inputs {
		if input.Port == port {
			return input
		}
	}
	t.Fatalf("managed input %q not found in %+v", port, inputs)
	return runManifestManagedInput{}
}

func artifactBindingByName(t *testing.T, bindings []workflowkit.ArtifactBinding, name string) workflowkit.ArtifactBinding {
	t.Helper()
	for _, binding := range bindings {
		if binding.Name == name {
			return binding
		}
	}
	t.Fatalf("artifact binding %q not found in %+v", name, bindings)
	return workflowkit.ArtifactBinding{}
}
