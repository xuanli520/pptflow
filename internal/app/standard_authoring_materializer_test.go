package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/taskpolicy"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestStandardAuthoringMaterializerSealsFirstRevisionAndBindsHandoffToStageArtifact(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	executor, err := NewStandardAuthoringMaterializeExecutor(StandardAuthoringMaterializeExecutorConfig{ManagedRoot: root, Store: database})
	if err != nil {
		t.Fatal(err)
	}
	sourceDigest := "sha256:" + strings.Repeat("a", 64)
	source, err := database.CreateAuthoringSource(ctx, store.CreateAuthoringSourceRequest{
		RepositoryURL: "https://github.com/tower-rs/tower-http.git", CommitSHA: "f066e10ebc07ea9050a2ce4576315abfa568edf4",
		SnapshotArtifactRef: sourceDigest, SnapshotContentDigest: sourceDigest, SnapshotSchemaVersion: "harbor.source-snapshot.v1",
		IdempotencyKey: "materializer-source", Actor: "author", Reason: "freeze source",
	})
	if err != nil {
		t.Fatal(err)
	}
	task, err := database.CreateTaskV2(ctx, store.CreateTaskV2Request{
		Slug: "materializer-task", Title: "Materializer task", MetadataJSON: `{}`,
		SourceRepo: source.RepositoryURL, SourceCommit: source.CommitSHA, Actor: "author", Reason: "reserve draft task",
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := database.CreateAuthoringSession(ctx, store.CreateAuthoringSessionRequest{
		SourceID: source.ID, TargetTaskID: task.ID, WorkflowTemplateID: workflowadapter.StandardAuthoringWorkflowTemplateID,
		WorkflowTemplateVersion: workflowadapter.StandardAuthoringWorkflowTemplateVersion, SessionManifestJSON: `{"format":"test"}`,
		IdempotencyKey: "materializer-session", Actor: "author", Reason: "freeze session",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.CreateAuthoringWorkflowRun(ctx, store.CreateAuthoringWorkflowRunRequest{
		AuthoringSessionID: session.ID, WorkflowTemplateID: session.WorkflowTemplateID, WorkflowTemplateVersion: session.WorkflowTemplateVersion,
		ResolvedProfileHash: "sha256:materializer-profile", DefinitionHash: "sha256:materializer-definition", RunManifestJSON: `{}`,
		Trigger: "task.generate", Actor: "author", Reason: "start materializer fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, managedRunsDirectory, run.ID), 0o750); err != nil {
		t.Fatal(err)
	}
	run, err = database.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunRunning, Actor: "author", Reason: "run materializer",
	})
	if err != nil {
		t.Fatal(err)
	}
	stageAttempt, err := database.CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: run.ID, StageKey: workflowadapter.MaterializeTask, StageGroup: string(workflowadapter.StageTaskGeneration), Ordinal: 1,
		InputFingerprint: "sha256:materializer-input", BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`,
		Actor: "worker", Reason: "create materialization stage",
	})
	if err != nil {
		t.Fatal(err)
	}
	stageAttempt, err = database.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: stageAttempt.ID, ExpectedVersion: stageAttempt.Version, ExecutionStatus: store.StageExecutionRunning,
		Actor: "worker", Reason: "execute materialization stage",
	})
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
		"task_toml":                []byte("[task]\nid = \"materializer-task\"\n"),
		"dockerfile":               standardAuthoringLaunchTestDockerfile(),
		"environment_policy":       standardAuthoringLaunchTestEnvironmentPolicyJSON(t),
		"solve_script":             []byte("#!/bin/sh\nexit 0\n"),
		"test_script":              []byte("#!/bin/sh\nexit 0\n"),
		"tests_analysis":           []byte("The tests validate the requested behavior.\n"),
		"solution_review_decision": approvedAuthoringSolutionDecision(t, source, session, run),
	}
	inputs := standardAuthoringMaterializerBindings(t, stage, contents)
	subject := workflowkit.SubjectBinding{SubjectID: source.ID, RevisionID: session.ID, Digest: workflowkit.SubjectDigest(source.SnapshotContentDigest)}
	request := workflowkit.StageExecutionRequest{
		Execution: workflowkit.FrozenExecution{ID: run.ID, Subject: subject, Actor: "worker"},
		Claim:     workflowkit.JobClaim{Stage: &workflowkit.StageClaim{StageAttempt: workflowkit.AttemptIdentity{ID: workflowkit.AttemptID(stageAttempt.ID)}, Stage: stage}},
		Stage:     stage, Inputs: inputs,
		ReadInput: func(_ context.Context, binding workflowkit.ArtifactBinding) ([]byte, error) {
			return append([]byte(nil), contents[binding.Name]...), nil
		},
	}
	invocation := stageprovider.StageOperationInvocation{
		Request:    request,
		Resolution: workflowadapter.StageOperationResolution{StageKey: workflowkit.StageKey(workflowadapter.MaterializeTask)},
	}
	result, err := executor.ExecuteHarborBuiltin(ctx, invocation, workflowadapter.HarborBuiltinOperationPayload{HandlerID: standardAuthoringMaterializeHandlerID})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome.Status != workflowkit.StatusCompleted || result.Outcome.Verdict != workflowkit.VerdictPass || len(result.Artifacts) != 3 {
		t.Fatalf("materialization result = %+v", result)
	}
	materialized, err := database.GetAuthoringTaskMaterializationForRun(ctx, run.ID)
	if err != nil || materialized == nil {
		t.Fatalf("materialization receipt = %+v, %v", materialized, err)
	}
	revision, err := database.GetTaskRevision(ctx, materialized.RevisionID)
	if err != nil || revision == nil || revision.Origin != store.RevisionOriginGenerated || revision.State != store.RevisionStateSealed {
		t.Fatalf("generated revision = %+v, %v", revision, err)
	}
	snapshotDirectory := filepath.Join(root, managedTasksDirectory, task.ID, "revisions", revision.ID, "snapshot")
	if digest, digestErr := taskpolicy.ComputeManagedTaskDigestV2(snapshotDirectory); digestErr != nil || digest != revision.TaskDigest {
		t.Fatalf("materialized snapshot digest = %q, %v; want %q", digest, digestErr, revision.TaskDigest)
	}
	handoff := parseMaterializerHandoff(t, result)
	if handoff.TaskID != task.ID || handoff.RevisionID != revision.ID || handoff.RevisionDigest != workflowkit.SubjectDigest(revision.TaskDigest) || handoff.AuthoringSessionID != session.ID {
		t.Fatalf("handoff lineage = %+v", handoff)
	}

	node, err := database.CreateNodeAttempt(ctx, store.CreateNodeAttemptRequest{
		StageAttemptID: stageAttempt.ID, NodeID: workflowadapter.MaterializeTask, Generation: 0, Attempt: 1,
		IdempotencyKey: "materializer-node", Actor: "worker", Reason: "persist materialized outputs",
	})
	if err != nil {
		t.Fatal(err)
	}
	services, err := newLifecycleServicesForTest(root, database)
	if err != nil {
		t.Fatal(err)
	}
	resolvedSubject, err := services.core.resolveWorkflowRunSubject(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	persisted := make([]StageArtifact, 0, len(result.Artifacts))
	for _, artifact := range result.Artifacts {
		persisted = append(persisted, StageArtifact{ID: string(artifact.ID), Key: artifact.Name, SchemaVersion: artifact.SchemaVersion, Content: artifact.Content, TurnOrdinal: artifact.TurnOrdinal})
	}
	_, references, err := persistStageArtifactsForSubject(ctx, services.core, run, resolvedSubject, stageAttempt, node, stage, inputs, persisted, "worker", "persist materialized authoring outputs")
	if err != nil {
		t.Fatal(err)
	}
	for _, reference := range references {
		if reference.ArtifactKey == "task_snapshot" && reference.ID != string(handoff.TaskSnapshot.ID) {
			t.Fatalf("persisted snapshot ref %s, want handoff ref %s", reference.ID, handoff.TaskSnapshot.ID)
		}
	}

	// A retry after Store materialization reuses the same real revision rather
	// than manufacturing another one. The pending stage result may reserve a
	// fresh output ID because the first result was deliberately not committed.
	replayed, err := executor.ExecuteHarborBuiltin(ctx, invocation, workflowadapter.HarborBuiltinOperationPayload{HandlerID: standardAuthoringMaterializeHandlerID})
	if err != nil {
		t.Fatal(err)
	}
	replayedHandoff := parseMaterializerHandoff(t, replayed)
	if replayedHandoff.RevisionID != revision.ID || replayedHandoff.TaskID != task.ID || replayedHandoff.TaskSnapshot.ID == handoff.TaskSnapshot.ID {
		t.Fatalf("materialization replay = %+v, want same revision and a fresh uncommitted output reference", replayedHandoff)
	}
}

func TestStandardAuthoringMaterializerRejectsDockerfileThatDiffersFromFrozenEnvironmentPolicy(t *testing.T) {
	profile := lifecycleCompleteProfileForTemplate(t, workflowadapter.StandardAuthoringWorkflowTemplate())
	resolved, err := workflowadapter.StandardAuthoringWorkflowTemplate().Compile(profile)
	if err != nil {
		t.Fatal(err)
	}
	stage, found := resolved.Descriptor.Stage(workflowkit.StageKey(workflowadapter.MaterializeTask))
	if !found {
		t.Fatal("compiled authoring workflow omitted materialize_task")
	}
	sourceID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	runID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	source := store.AuthoringSource{ID: sourceID, SnapshotContentDigest: "sha256:" + strings.Repeat("a", 64)}
	session := store.AuthoringSession{ID: sessionID}
	run := store.WorkflowRun{
		ID: runID, WorkflowTemplateID: workflowadapter.StandardAuthoringWorkflowTemplateID,
		WorkflowTemplateVersion: workflowadapter.StandardAuthoringWorkflowTemplateVersion,
	}
	contents := map[string][]byte{
		"instruction":              []byte("# Task\n"),
		"task_toml":                []byte("[task]\nid = \"fixture\"\n"),
		"dockerfile":               []byte("FROM docker.io/library/debian:bookworm@sha256:" + strings.Repeat("b", 64) + "\n"),
		"environment_policy":       standardAuthoringLaunchTestEnvironmentPolicyJSON(t),
		"solve_script":             []byte("#!/bin/sh\nexit 0\n"),
		"test_script":              []byte("#!/bin/sh\nexit 0\n"),
		"tests_analysis":           []byte("tests\n"),
		"solution_review_decision": approvedAuthoringSolutionDecision(t, source, session, run),
	}
	inputs := standardAuthoringMaterializerBindings(t, stage, contents)
	_, err = standardAuthoringMaterializeInputs(context.Background(), workflowkit.StageExecutionRequest{
		Stage: stage, Inputs: inputs,
		ReadInput: func(_ context.Context, binding workflowkit.ArtifactBinding) ([]byte, error) {
			return append([]byte(nil), contents[binding.Name]...), nil
		},
	}, run, workflowRunSubject{
		Binding: workflowkit.SubjectBinding{SubjectID: source.ID, RevisionID: session.ID, Digest: workflowkit.SubjectDigest(source.SnapshotContentDigest)},
		Kind:    store.WorkflowRunSubjectAuthoringSession, AuthoringSource: &source, AuthoringSession: &session,
	})
	if err == nil || !strings.Contains(err.Error(), "Dockerfile base image") {
		t.Fatalf("mismatched frozen Dockerfile policy error = %v", err)
	}
}

func standardAuthoringMaterializerBindings(t *testing.T, stage workflowkit.StageDescriptor, contents map[string][]byte) []workflowkit.ArtifactBinding {
	t.Helper()
	bindings := make([]workflowkit.ArtifactBinding, 0, len(stage.Inputs))
	for _, input := range stage.Inputs {
		id, err := store.NewUUIDv7()
		if err != nil {
			t.Fatal(err)
		}
		content, found := contents[input.Name]
		if !found {
			t.Fatalf("fixture omitted required materializer input %q", input.Name)
		}
		bindings = append(bindings, workflowkit.ArtifactBinding{Name: input.Name, ArtifactID: workflowkit.ArtifactID(id), ContentDigest: workflowkit.SHA256Fingerprint(content), SchemaVersion: input.SchemaVersion})
	}
	return bindings
}

func approvedAuthoringSolutionDecision(t *testing.T, source store.AuthoringSource, session store.AuthoringSession, run store.WorkflowRun) []byte {
	t.Helper()
	requestID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	decisionID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(authoringReviewGateDecisionArtifact{
		Format: authoringReviewGateDecisionArtifactFormat, ReviewRequestID: requestID, ReviewDecisionID: decisionID,
		Action: store.ReviewDecisionApprove, AuthoringSourceID: source.ID, AuthoringSessionID: session.ID,
		SourceSnapshotDigest: source.SnapshotContentDigest, ReviewKind: string(workflowadapter.ReviewSolutionVerifier),
		EvidenceManifestDigest: "sha256:" + strings.Repeat("b", 64), InputFingerprint: "sha256:" + strings.Repeat("c", 64),
		DecisionActor: "operator", DecisionReason: "approved generated task",
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func parseMaterializerHandoff(t *testing.T, result workflowkit.StageExecutionResult) workflowadapter.StandardAuthoringTaskHandoff {
	t.Helper()
	for _, artifact := range result.Artifacts {
		if artifact.Name == workflowadapter.StandardAuthoringTaskHandoffArtifact {
			handoff, err := workflowadapter.ParseStandardAuthoringTaskHandoffJSON(artifact.Content)
			if err != nil {
				t.Fatal(err)
			}
			return handoff
		}
	}
	t.Fatal("materializer result omitted authoring task handoff")
	return workflowadapter.StandardAuthoringTaskHandoff{}
}
