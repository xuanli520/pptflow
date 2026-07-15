package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/testsupport"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

type codeEdgePhase1DefinitionProviderFunc func(context.Context, CodeEdgePhase1RunDefinitionRequest) (CodeEdgePhase1RunDefinition, error)

func (function codeEdgePhase1DefinitionProviderFunc) DefinitionForCodeEdgePhase1Run(ctx context.Context, request CodeEdgePhase1RunDefinitionRequest) (CodeEdgePhase1RunDefinition, error) {
	return function(ctx, request)
}

func TestStandardAuthoringHandoffCreatesOneTaskBoundPhase1ChildFromPersistedReceipt(t *testing.T) {
	ctx := context.Background()
	fixture := newStandardAuthoringHandoffFixture(t)
	provider := codeEdgePhase1DefinitionProviderFunc(func(_ context.Context, request CodeEdgePhase1RunDefinitionRequest) (CodeEdgePhase1RunDefinition, error) {
		if request.TaskID != fixture.handoff.TaskID || request.RevisionID != fixture.handoff.RevisionID || request.RevisionDigest != fixture.handoff.RevisionDigest ||
			request.AuthoringRunID != fixture.run.ID || request.AuthoringSourceID != fixture.source.ID || request.AuthoringSessionID != fixture.session.ID ||
			request.TaskSnapshot != fixture.handoff.TaskSnapshot {
			t.Fatalf("definition request = %+v; does not match persisted handoff", request)
		}
		return CodeEdgePhase1RunDefinition{
			Profile:       lifecycleCompleteProfileForTemplate(t, workflowadapter.CodeEdgePhase1WorkflowTemplate()),
			ExecutionSpec: testsupport.CompleteCodeEdgePhase1RunExecutionSpec(request.TaskID, request.RevisionID, string(request.RevisionDigest)),
		}, nil
	})
	services, err := NewLifecycleServicesWithOptions(fixture.root, fixture.database, LifecycleServicesOptions{
		OperationResolver:                   testsupport.AcceptAllStageOperationResolver(),
		CodeEdgePhase1RunDefinitionProvider: provider,
	})
	if err != nil {
		t.Fatal(err)
	}
	childID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	request := StandardAuthoringHandoffRequest{
		AuthoringRunID: fixture.run.ID, StageAttemptID: fixture.stageAttempt.ID,
		HandoffArtifactID: fixture.handoffArtifactID, ChildRunID: childID,
		Actor: "worker", Reason: "consume durable authoring handoff",
	}
	child, err := services.StandardAuthoringHandoffs.Consume(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if child.ID != childID || child.ParentRunID != fixture.run.ID || child.TaskID != fixture.handoff.TaskID || child.RevisionID != fixture.handoff.RevisionID ||
		child.WorkflowTemplateID != workflowadapter.CodeEdgePhase1WorkflowTemplateID || child.WorkflowTemplateVersion != workflowadapter.CodeEdgePhase1WorkflowTemplateVersion {
		t.Fatalf("child Run = %+v", child)
	}
	managedSnapshot, err := fixture.database.GetRunInputArtifactForPort(ctx, child.ID, managedTaskSnapshotInputPort)
	if err != nil {
		t.Fatal(err)
	}
	if managedSnapshot == nil || managedSnapshot.ContentDigest != string(fixture.handoff.TaskSnapshot.ContentDigest) || managedSnapshot.SchemaVersion != fixture.handoff.TaskSnapshot.SchemaVersion || managedSnapshot.ID == fixture.handoffArtifactID {
		t.Fatalf("child managed snapshot = %+v; want fresh input identity over handoff bytes", managedSnapshot)
	}
	replayed, err := services.StandardAuthoringHandoffs.Consume(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ID != child.ID || replayed.ParentRunID != fixture.run.ID {
		t.Fatalf("replayed child Run = %+v; want %+v", replayed, child)
	}
	// Once the child row committed, replay must return it before consulting a
	// potentially changed/missing deployment definition. The durable record,
	// rather than the mutable provider process, remains the recovery authority.
	withoutDefinition, err := NewLifecycleServicesWithOptions(fixture.root, fixture.database, LifecycleServicesOptions{OperationResolver: testsupport.AcceptAllStageOperationResolver()})
	if err != nil {
		t.Fatal(err)
	}
	differentChildID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := withoutDefinition.StandardAuthoringHandoffs.Consume(ctx, StandardAuthoringHandoffRequest{
		AuthoringRunID: fixture.run.ID, StageAttemptID: fixture.stageAttempt.ID, HandoffArtifactID: fixture.handoffArtifactID,
		ChildRunID: differentChildID, Actor: "worker", Reason: "recover existing child without provider",
	})
	if err != nil || recovered.ID != child.ID {
		t.Fatalf("provider-free existing-child recovery = %+v, %v; want %s", recovered, err, child.ID)
	}
	// Generic StartRun never receives the private durable handoff capability.
	// Even a matching template/parent/task request must be rejected rather than
	// creating a second Phase-1 child around the authoring artifact boundary.
	unauthorizedID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	_, err = services.Runs.StartRun(ctx, StartRunRequest{
		ID: unauthorizedID, TaskID: fixture.handoff.TaskID, RevisionID: fixture.handoff.RevisionID,
		Profile:       lifecycleCompleteProfileForTemplate(t, workflowadapter.CodeEdgePhase1WorkflowTemplate()),
		ExecutionSpec: testsupport.CompleteCodeEdgePhase1RunExecutionSpec(fixture.handoff.TaskID, fixture.handoff.RevisionID, string(fixture.handoff.RevisionDigest)),
		ParentRunID:   fixture.run.ID, Trigger: standardAuthoringHandoffRunTrigger, Actor: "worker", Reason: "attempt bypass",
	})
	if !strings.Contains(errString(err), "authoring parent requires a persisted Phase-1 handoff") {
		t.Fatalf("generic authoring-parent StartRun error = %v", err)
	}
	runs, err := fixture.database.ListWorkflowRunsForTask(ctx, fixture.handoff.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	children := 0
	for _, candidate := range runs {
		if candidate.ParentRunID == fixture.run.ID && candidate.WorkflowTemplateID == workflowadapter.CodeEdgePhase1WorkflowTemplateID {
			children++
		}
	}
	if children != 1 {
		t.Fatalf("Phase-1 children = %d; want exactly one", children)
	}
}

func TestStandardAuthoringHandoffFailsClosedWithoutPhase1Definition(t *testing.T) {
	ctx := context.Background()
	fixture := newStandardAuthoringHandoffFixture(t)
	services, err := NewLifecycleServicesWithOptions(fixture.root, fixture.database, LifecycleServicesOptions{OperationResolver: testsupport.AcceptAllStageOperationResolver()})
	if err != nil {
		t.Fatal(err)
	}
	childID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	_, err = services.StandardAuthoringHandoffs.Consume(ctx, StandardAuthoringHandoffRequest{
		AuthoringRunID: fixture.run.ID, StageAttemptID: fixture.stageAttempt.ID,
		HandoffArtifactID: fixture.handoffArtifactID, ChildRunID: childID, Actor: "worker", Reason: "verify fail closed",
	})
	if !strings.Contains(errString(err), ErrCodeEdgePhase1DefinitionUnavailable.Error()) {
		t.Fatalf("Consume without definition error = %v; want definition unavailable", err)
	}
	if child, lookupErr := fixture.database.GetWorkflowRun(ctx, childID); lookupErr != nil || child != nil {
		t.Fatalf("fail-closed handoff created child = %+v, %v", child, lookupErr)
	}
	if ref, lookupErr := fixture.database.GetArtifactRef(ctx, fixture.handoffArtifactID); lookupErr != nil || ref == nil {
		t.Fatalf("fail-closed handoff lost durable artifact = %+v, %v", ref, lookupErr)
	}
	prepared, err := fixture.database.GetAuthoringPhase1HandoffForAuthoringRun(ctx, fixture.run.ID)
	if err != nil || prepared == nil || prepared.ChildRunID != childID {
		t.Fatalf("fail-closed handoff did not preserve its redrive identity = %+v, %v", prepared, err)
	}
	// A later approved provider consumes that exact durable record even when a
	// retry supplies a fresh UUID. This proves the first request cannot be used
	// to manufacture a second child after a configuration change.
	provider := codeEdgePhase1DefinitionProviderFunc(func(_ context.Context, definition CodeEdgePhase1RunDefinitionRequest) (CodeEdgePhase1RunDefinition, error) {
		return CodeEdgePhase1RunDefinition{
			Profile:       lifecycleCompleteProfileForTemplate(t, workflowadapter.CodeEdgePhase1WorkflowTemplate()),
			ExecutionSpec: testsupport.CompleteCodeEdgePhase1RunExecutionSpec(definition.TaskID, definition.RevisionID, string(definition.RevisionDigest)),
		}, nil
	})
	redriveServices, err := NewLifecycleServicesWithOptions(fixture.root, fixture.database, LifecycleServicesOptions{OperationResolver: testsupport.AcceptAllStageOperationResolver(), CodeEdgePhase1RunDefinitionProvider: provider})
	if err != nil {
		t.Fatal(err)
	}
	newRequestID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	child, err := redriveServices.StandardAuthoringHandoffs.Consume(ctx, StandardAuthoringHandoffRequest{
		AuthoringRunID: fixture.run.ID, StageAttemptID: fixture.stageAttempt.ID, HandoffArtifactID: fixture.handoffArtifactID,
		ChildRunID: newRequestID, Actor: "worker", Reason: "redrive after provider installation",
	})
	if err != nil || child.ID != prepared.ChildRunID || child.ID != childID {
		t.Fatalf("redriven child = %+v, %v; want original %s", child, err, childID)
	}
}

func TestFrozenRuntimeEnqueuesAndConsumesOneStandardAuthoringHandoffJob(t *testing.T) {
	ctx := context.Background()
	fixture := newStandardAuthoringHandoffFixture(t)
	provider := codeEdgePhase1DefinitionProviderFunc(func(_ context.Context, definition CodeEdgePhase1RunDefinitionRequest) (CodeEdgePhase1RunDefinition, error) {
		return CodeEdgePhase1RunDefinition{
			Profile:       lifecycleCompleteProfileForTemplate(t, workflowadapter.CodeEdgePhase1WorkflowTemplate()),
			ExecutionSpec: testsupport.CompleteCodeEdgePhase1RunExecutionSpec(definition.TaskID, definition.RevisionID, string(definition.RevisionDigest)),
		}, nil
	})
	services, err := NewLifecycleServicesWithOptions(fixture.root, fixture.database, LifecycleServicesOptions{
		OperationResolver:                   testsupport.AcceptAllStageOperationResolver(),
		CodeEdgePhase1RunDefinitionProvider: provider,
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime := &FrozenExecutionRuntime{core: services.core, services: services}
	stageJob := store.DurableJob{CreatedBy: "worker", Priority: 7}
	if err := runtime.enqueueStandardAuthoringHandoff(ctx, stageJob, fixture.run, fixture.stageAttempt); err != nil {
		t.Fatal(err)
	}
	job, err := fixture.database.GetDurableJobByIdempotency(ctx, "standard-authoring-handoff:"+fixture.stageAttempt.ID)
	if err != nil || job == nil {
		t.Fatalf("durable handoff job = %+v, %v", job, err)
	}
	payload, err := standardAuthoringHandoffJobPayload(*job)
	if err != nil || payload.AuthoringRunID != fixture.run.ID || payload.HandoffArtifactID != fixture.handoffArtifactID {
		t.Fatalf("durable handoff payload = %+v, %v", payload, err)
	}
	state, err := runtime.handleStandardAuthoringHandoff(ctx, *job)
	if err != nil || state != store.JobSucceeded {
		t.Fatalf("consume durable handoff = %s, %v", state, err)
	}
	child, err := fixture.database.GetWorkflowRun(ctx, payload.ChildRunID)
	if err != nil || child == nil || child.ParentRunID != fixture.run.ID {
		t.Fatalf("durable handoff child = %+v, %v", child, err)
	}
	if err := runtime.enqueueStandardAuthoringHandoff(ctx, stageJob, fixture.run, fixture.stageAttempt); err != nil {
		t.Fatal(err)
	}
	jobs, err := fixture.database.ListDurableJobsForRun(ctx, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, candidate := range jobs {
		if candidate.CommandType == standardAuthoringHandoffCommandType {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("handoff jobs = %d; want exactly one", count)
	}
}

type standardAuthoringHandoffFixture struct {
	root              string
	database          *store.Store
	source            store.AuthoringSource
	session           store.AuthoringSession
	run               store.WorkflowRun
	stageAttempt      store.StageAttempt
	handoff           workflowadapter.StandardAuthoringTaskHandoff
	handoffArtifactID string
}

func newStandardAuthoringHandoffFixture(t *testing.T) standardAuthoringHandoffFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	executor, err := NewStandardAuthoringMaterializeExecutor(StandardAuthoringMaterializeExecutorConfig{ManagedRoot: root, Store: database})
	if err != nil {
		t.Fatal(err)
	}
	sourceDigest := "sha256:" + strings.Repeat("a", 64)
	source, err := database.CreateAuthoringSource(ctx, store.CreateAuthoringSourceRequest{
		RepositoryURL: "https://github.com/tower-rs/tower-http.git", CommitSHA: "f066e10ebc07ea9050a2ce4576315abfa568edf4",
		SnapshotArtifactRef: sourceDigest, SnapshotContentDigest: sourceDigest, SnapshotSchemaVersion: "harbor.source-snapshot.v1",
		IdempotencyKey: "handoff-source", Actor: "author", Reason: "freeze source",
	})
	if err != nil {
		t.Fatal(err)
	}
	task, err := database.CreateTaskV2(ctx, store.CreateTaskV2Request{
		Slug: "handoff-task", Title: "Handoff task", MetadataJSON: `{}`,
		SourceRepo: source.RepositoryURL, SourceCommit: source.CommitSHA, Actor: "author", Reason: "reserve draft task",
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := database.CreateAuthoringSession(ctx, store.CreateAuthoringSessionRequest{
		SourceID: source.ID, TargetTaskID: task.ID, WorkflowTemplateID: workflowadapter.StandardAuthoringWorkflowTemplateID,
		WorkflowTemplateVersion: workflowadapter.StandardAuthoringWorkflowTemplateVersion, SessionManifestJSON: `{"format":"test"}`,
		IdempotencyKey: "handoff-session", Actor: "author", Reason: "freeze session",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.CreateAuthoringWorkflowRun(ctx, store.CreateAuthoringWorkflowRunRequest{
		AuthoringSessionID: session.ID, WorkflowTemplateID: session.WorkflowTemplateID, WorkflowTemplateVersion: session.WorkflowTemplateVersion,
		ResolvedProfileHash: "sha256:handoff-profile", DefinitionHash: "sha256:handoff-definition", RunManifestJSON: `{}`,
		Trigger: "task.generate", Actor: "author", Reason: "start handoff fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, managedRunsDirectory, run.ID), 0o750); err != nil {
		t.Fatal(err)
	}
	run, err = database.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunRunning, Actor: "author", Reason: "run materializer"})
	if err != nil {
		t.Fatal(err)
	}
	stageAttempt, err := database.CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: run.ID, StageKey: workflowadapter.MaterializeTask, StageGroup: string(workflowadapter.StageTaskGeneration), Ordinal: 1,
		InputFingerprint: "sha256:handoff-input", BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`,
		Actor: "worker", Reason: "create materialization stage",
	})
	if err != nil {
		t.Fatal(err)
	}
	stageAttempt, err = database.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{StageAttemptID: stageAttempt.ID, ExpectedVersion: stageAttempt.Version, ExecutionStatus: store.StageExecutionRunning, Actor: "worker", Reason: "execute materialization stage"})
	if err != nil {
		t.Fatal(err)
	}
	profile := lifecycleCompleteProfileForTemplate(t, workflowadapter.StandardAuthoringWorkflowTemplate())
	resolved, err := workflowadapter.StandardAuthoringWorkflowTemplate().Compile(profile)
	if err != nil {
		t.Fatal(err)
	}
	stage, found := resolved.Descriptor.Stage(workflowkit.StageKey(workflowadapter.MaterializeTask))
	if !found {
		t.Fatal("compiled authoring workflow omitted materialize_task")
	}
	contents := map[string][]byte{
		"instruction":              []byte("# Task\n\nImplement the requested behavior.\n"),
		"task_toml":                []byte("[task]\nid = \"handoff-task\"\n"),
		"dockerfile":               []byte("FROM alpine:3.20\n"),
		"solve_script":             []byte("#!/bin/sh\nexit 0\n"),
		"test_script":              []byte("#!/bin/sh\nexit 0\n"),
		"tests_analysis":           []byte("The tests validate the requested behavior.\n"),
		"solution_review_decision": approvedAuthoringSolutionDecision(t, source, session, run),
	}
	inputs := standardAuthoringMaterializerBindings(t, stage, contents)
	subject := workflowkit.SubjectBinding{SubjectID: source.ID, RevisionID: session.ID, Digest: workflowkit.SubjectDigest(source.SnapshotContentDigest)}
	result, err := executor.ExecuteHarborBuiltin(ctx, stageprovider.StageOperationInvocation{
		Request: workflowkit.StageExecutionRequest{
			Execution: workflowkit.FrozenExecution{ID: run.ID, Subject: subject, Actor: "worker"},
			Claim:     workflowkit.JobClaim{Stage: &workflowkit.StageClaim{StageAttempt: workflowkit.AttemptIdentity{ID: workflowkit.AttemptID(stageAttempt.ID)}, Stage: stage}},
			Stage:     stage, Inputs: inputs,
			ReadInput: func(_ context.Context, binding workflowkit.ArtifactBinding) ([]byte, error) {
				return append([]byte(nil), contents[binding.Name]...), nil
			},
		},
		Resolution: workflowadapter.StageOperationResolution{StageKey: workflowkit.StageKey(workflowadapter.MaterializeTask)},
	}, workflowadapter.HarborBuiltinOperationPayload{HandlerID: standardAuthoringMaterializeHandlerID})
	if err != nil {
		t.Fatal(err)
	}
	handoff := parseMaterializerHandoff(t, result)
	node, err := database.CreateNodeAttempt(ctx, store.CreateNodeAttemptRequest{StageAttemptID: stageAttempt.ID, NodeID: workflowadapter.MaterializeTask, Generation: 0, Attempt: 1, IdempotencyKey: "handoff-node", Actor: "worker", Reason: "persist materialized outputs"})
	if err != nil {
		t.Fatal(err)
	}
	services, err := NewLifecycleServicesWithOptions(root, database, LifecycleServicesOptions{OperationResolver: testsupport.AcceptAllStageOperationResolver()})
	if err != nil {
		t.Fatal(err)
	}
	resolvedSubject, err := services.core.resolveWorkflowRunSubject(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	artifacts := make([]StageArtifact, 0, len(result.Artifacts))
	for _, artifact := range result.Artifacts {
		artifacts = append(artifacts, StageArtifact{ID: string(artifact.ID), Key: artifact.Name, SchemaVersion: artifact.SchemaVersion, Content: artifact.Content, TurnOrdinal: artifact.TurnOrdinal})
	}
	manifest, refs, err := persistStageArtifactsForSubject(ctx, services.core, run, resolvedSubject, stageAttempt, node, stage, inputs, artifacts, "worker", "persist materialized handoff outputs")
	if err != nil {
		t.Fatal(err)
	}
	stageAttempt, err = database.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{StageAttemptID: stageAttempt.ID, ExpectedVersion: stageAttempt.Version, ExecutionStatus: store.StageExecutionCompleted, Verdict: store.VerdictPass, ArtifactManifestID: manifest.ID, Actor: "worker", Reason: "persist materialization result"})
	if err != nil {
		t.Fatal(err)
	}
	handoffArtifactID := ""
	for _, reference := range refs {
		if reference.ArtifactKey == workflowadapter.StandardAuthoringTaskHandoffArtifact {
			handoffArtifactID = reference.ID
		}
	}
	if handoffArtifactID == "" {
		t.Fatal("persisted materializer output omitted handoff artifact")
	}
	materializedTask, err := database.GetTaskV2(ctx, task.ID)
	if err != nil || materializedTask == nil || materializedTask.CurrentRevisionID != handoff.RevisionID {
		t.Fatalf("materialized Task = %+v, %v; want current revision %s", materializedTask, err, handoff.RevisionID)
	}
	return standardAuthoringHandoffFixture{root: root, database: database, source: source, session: session, run: run, stageAttempt: stageAttempt, handoff: handoff, handoffArtifactID: handoffArtifactID}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
