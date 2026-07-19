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
	packageInput := standardAuthoringTaskPackageFixture(t)
	executor, err := NewStandardAuthoringMaterializeExecutor(StandardAuthoringMaterializeExecutorConfig{ManagedRoot: root, Store: database, Admission: &packageInput.Admission})
	if err != nil {
		t.Fatal(err)
	}
	sourceDigest := "sha256:" + strings.Repeat("a", 64)
	source, err := database.CreateAuthoringSource(ctx, store.CreateAuthoringSourceRequest{
		RepositoryURL: packageInput.Source.RepositoryURL, CommitSHA: packageInput.Source.CommitSHA,
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
		WorkflowTemplateVersion: workflowadapter.StandardAuthoringBriefTemplateVersion, SessionManifestJSON: `{"format":"test"}`,
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

	template := workflowadapter.StandardAuthoringBriefWorkflowTemplate()
	profile := lifecycleCompleteProfileForTemplate(t, template)
	resolved, err := template.Compile(profile)
	if err != nil {
		t.Fatal(err)
	}
	stage, found := resolved.Descriptor.Stage(workflowkit.StageKey(workflowadapter.MaterializeTask))
	if !found {
		t.Fatal("compiled authoring workflow omitted materialize_task")
	}
	policyRaw, err := packageInput.Environment.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	briefRaw, err := packageInput.Brief.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	contents := map[string][]byte{
		"instruction": append([]byte(nil), packageInput.Instruction...),
		"task_toml":   append([]byte(nil), packageInput.TaskTOMLDraft...),
		"dockerfile":  append([]byte(nil), packageInput.Dockerfile...),
		workflowadapter.StandardAuthoringEnvironmentPolicyArtifact: policyRaw,
		workflowadapter.StandardAuthoringBriefArtifact:             briefRaw,
		"solve_script":                      append([]byte(nil), packageInput.SolveScript...),
		"test_script":                       append([]byte(nil), packageInput.TestScript...),
		"tests_analysis":                    append([]byte(nil), packageInput.TestsAnalysis...),
		"solution_review_decision":          approvedAuthoringSolutionDecision(t, source, session, run),
		"codeedge_package_admission_report": []byte(`{}`),
	}
	inputs := standardAuthoringMaterializerBindings(t, stage, contents)
	compiled, err := CompileStandardAuthoringTaskPackage(packageInput)
	if err != nil || !compiled.Report.Passed {
		t.Fatalf("compile materializer fixture package: report=%+v err=%v", compiled.Report, err)
	}
	admissionInputs := make([]workflowkit.ArtifactBinding, 0, 8)
	for _, binding := range inputs {
		switch binding.Name {
		case "instruction", "task_toml", "dockerfile", workflowadapter.StandardAuthoringEnvironmentPolicyArtifact,
			workflowadapter.StandardAuthoringBriefArtifact, "solve_script", "test_script", "tests_analysis":
			admissionInputs = append(admissionInputs, binding)
		}
	}
	inputFingerprint, err := workflowkit.FingerprintArtifactBindings(admissionInputs)
	if err != nil {
		t.Fatal(err)
	}
	admissionReceipt, err := json.Marshal(standardAuthoringAdmissionReceipt{
		Format: standardAuthoringAdmissionReceiptFormat, Version: standardAuthoringAdmissionReceiptVersion,
		RunID: run.ID, AuthoringSourceID: source.ID, AuthoringSessionID: session.ID,
		InputFingerprint: inputFingerprint, Report: compiled.Report,
	})
	if err != nil {
		t.Fatal(err)
	}
	contents["codeedge_package_admission_report"] = admissionReceipt
	for index := range inputs {
		if inputs[index].Name == "codeedge_package_admission_report" {
			inputs[index].ContentDigest = workflowkit.SHA256Fingerprint(admissionReceipt)
		}
	}
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
	template := workflowadapter.StandardAuthoringBriefWorkflowTemplate()
	profile := lifecycleCompleteProfileForTemplate(t, template)
	resolved, err := template.Compile(profile)
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
		WorkflowTemplateVersion: workflowadapter.StandardAuthoringBriefTemplateVersion,
	}
	packageInput := standardAuthoringTaskPackageFixture(t)
	briefRaw, err := packageInput.Brief.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	contents := map[string][]byte{
		"instruction":                       []byte("# Task\n"),
		"task_toml":                         []byte("[task]\nid = \"fixture\"\n"),
		"dockerfile":                        []byte("FROM docker.io/library/debian:bookworm@sha256:" + strings.Repeat("b", 64) + "\n"),
		"environment_policy":                standardAuthoringLaunchTestEnvironmentPolicyJSON(t),
		"authoring_brief":                   briefRaw,
		"solve_script":                      []byte("#!/bin/sh\nexit 0\n"),
		"test_script":                       []byte("#!/bin/sh\nexit 0\n"),
		"tests_analysis":                    []byte("tests\n"),
		"solution_review_decision":          approvedAuthoringSolutionDecision(t, source, session, run),
		"codeedge_package_admission_report": []byte(`{}`),
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
	}, &packageInput.Admission)
	if err == nil || !strings.Contains(err.Error(), "Dockerfile base image") {
		t.Fatalf("mismatched frozen Dockerfile policy error = %v", err)
	}
}

func TestStandardAuthoringPackageAdmissionReadsBriefAndReportsMetadataMismatch(t *testing.T) {
	fixture := newStandardAuthoringBriefStageInputFixture(t, workflowkit.StageKey(workflowadapter.CodeEdgePackageAdmission))
	fixture.contents["task_toml"] = []byte(strings.Replace(string(fixture.contents["task_toml"]), `task_type = "bugfix"`, `task_type = "feature"`, 1))
	request := fixture.request(t)
	inputs, err := standardAuthoringPackageAdmissionInputs(context.Background(), request, fixture.run)
	if err != nil {
		t.Fatal(err)
	}
	if inputs.brief == nil || inputs.brief.TaskType != fixture.packageInput.Brief.TaskType || inputs.brief.Application != fixture.packageInput.Brief.Application {
		t.Fatalf("package admission brief = %+v, want %+v", inputs.brief, fixture.packageInput.Brief)
	}
	compiled, err := CompileStandardAuthoringTaskPackage(StandardAuthoringTaskPackageInput{
		Instruction: inputs.instruction, TaskTOMLDraft: inputs.taskTOML, Dockerfile: inputs.dockerfile,
		SolveScript: inputs.solveScript, TestScript: inputs.testScript, TestsAnalysis: inputs.testsAnalysis,
		Source: fixture.packageInput.Source, Environment: inputs.environment, Brief: inputs.brief, Admission: fixture.packageInput.Admission,
	})
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Report.Passed {
		t.Fatalf("package admission mismatch unexpectedly passed: %+v", compiled.Report)
	}
	assertAdmissionViolationMessage(t, compiled.Report, "task_metadata", "metadata.task_type")
}

func TestStandardAuthoringPackageAdmissionStrictlyParsesBrief(t *testing.T) {
	fixture := newStandardAuthoringBriefStageInputFixture(t, workflowkit.StageKey(workflowadapter.CodeEdgePackageAdmission))
	fixture.contents[workflowadapter.StandardAuthoringBriefArtifact] = []byte(`{"format":"harbor.standard-authoring-brief.v1","version":"1","task_type":"bugfix","application":"widget","objective":"Repair the widget","unexpected":true}`)
	_, err := standardAuthoringPackageAdmissionInputs(context.Background(), fixture.request(t), fixture.run)
	if err == nil || !strings.Contains(err.Error(), "decode frozen Standard authoring brief") {
		t.Fatalf("malformed package admission brief error = %v", err)
	}
}

func TestStandardAuthoringMaterializerRejectsBriefMetadataMismatch(t *testing.T) {
	for _, test := range []struct {
		name        string
		old         string
		replacement string
		want        string
	}{
		{name: "task type", old: `task_type = "bugfix"`, replacement: `task_type = "feature"`, want: "metadata.task_type"},
		{name: "application", old: `application = "widget"`, replacement: `application = "backend"`, want: "metadata.application"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newStandardAuthoringBriefStageInputFixture(t, workflowkit.StageKey(workflowadapter.MaterializeTask))
			fixture.contents["task_toml"] = []byte(strings.Replace(string(fixture.contents["task_toml"]), test.old, test.replacement, 1))
			_, err := standardAuthoringMaterializeInputs(context.Background(), fixture.request(t), fixture.run, fixture.subject, &fixture.packageInput.Admission)
			if err == nil || !strings.Contains(err.Error(), "rejected materialization") || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("mismatched materializer brief metadata error = %v", err)
			}
		})
	}
}

func TestStandardAuthoringMaterializerStrictlyParsesBrief(t *testing.T) {
	fixture := newStandardAuthoringBriefStageInputFixture(t, workflowkit.StageKey(workflowadapter.MaterializeTask))
	fixture.contents[workflowadapter.StandardAuthoringBriefArtifact] = []byte(`{"format":"harbor.standard-authoring-brief.v1","version":"1","task_type":"bugfix","application":"widget","objective":"Repair the widget"} trailing`)
	_, err := standardAuthoringMaterializeInputs(context.Background(), fixture.request(t), fixture.run, fixture.subject, &fixture.packageInput.Admission)
	if err == nil || !strings.Contains(err.Error(), "decode frozen Standard authoring brief") {
		t.Fatalf("malformed materializer brief error = %v", err)
	}
}

type standardAuthoringBriefStageInputFixture struct {
	stage        workflowkit.StageDescriptor
	run          store.WorkflowRun
	subject      workflowRunSubject
	contents     map[string][]byte
	packageInput StandardAuthoringTaskPackageInput
}

func newStandardAuthoringBriefStageInputFixture(t *testing.T, stageKey workflowkit.StageKey) standardAuthoringBriefStageInputFixture {
	t.Helper()
	template := workflowadapter.StandardAuthoringBriefWorkflowTemplate()
	profile := lifecycleCompleteProfileForTemplate(t, template)
	resolved, err := template.Compile(profile)
	if err != nil {
		t.Fatal(err)
	}
	stage, found := resolved.Descriptor.Stage(stageKey)
	if !found {
		t.Fatalf("compiled brief authoring workflow omitted %s", stageKey)
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
	packageInput := standardAuthoringTaskPackageFixture(t)
	sourceDigest := "sha256:" + strings.Repeat("a", 64)
	source := store.AuthoringSource{
		ID: sourceID, RepositoryURL: packageInput.Source.RepositoryURL, CommitSHA: packageInput.Source.CommitSHA,
		SnapshotContentDigest: sourceDigest,
	}
	session := store.AuthoringSession{ID: sessionID}
	run := store.WorkflowRun{
		ID: runID, WorkflowTemplateID: workflowadapter.StandardAuthoringWorkflowTemplateID,
		WorkflowTemplateVersion: workflowadapter.StandardAuthoringBriefTemplateVersion,
	}
	policyRaw, err := packageInput.Environment.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	briefRaw, err := packageInput.Brief.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	contents := map[string][]byte{
		"instruction": append([]byte(nil), packageInput.Instruction...),
		"task_toml":   append([]byte(nil), packageInput.TaskTOMLDraft...),
		"dockerfile":  append([]byte(nil), packageInput.Dockerfile...),
		workflowadapter.StandardAuthoringEnvironmentPolicyArtifact: policyRaw,
		workflowadapter.StandardAuthoringBriefArtifact:             briefRaw,
		"solve_script":                      append([]byte(nil), packageInput.SolveScript...),
		"test_script":                       append([]byte(nil), packageInput.TestScript...),
		"tests_analysis":                    append([]byte(nil), packageInput.TestsAnalysis...),
		"solution_review_decision":          approvedAuthoringSolutionDecision(t, source, session, run),
		"codeedge_package_admission_report": []byte(`{"format":"fixture"}`),
	}
	subject := workflowRunSubject{
		Binding: workflowkit.SubjectBinding{SubjectID: source.ID, RevisionID: session.ID, Digest: workflowkit.SubjectDigest(sourceDigest)},
		Kind:    store.WorkflowRunSubjectAuthoringSession, AuthoringSource: &source, AuthoringSession: &session,
	}
	return standardAuthoringBriefStageInputFixture{stage: stage, run: run, subject: subject, contents: contents, packageInput: packageInput}
}

func (fixture standardAuthoringBriefStageInputFixture) request(t *testing.T) workflowkit.StageExecutionRequest {
	t.Helper()
	return workflowkit.StageExecutionRequest{
		Stage: fixture.stage, Inputs: standardAuthoringMaterializerBindings(t, fixture.stage, fixture.contents),
		ReadInput: func(_ context.Context, binding workflowkit.ArtifactBinding) ([]byte, error) {
			return append([]byte(nil), fixture.contents[binding.Name]...), nil
		},
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
