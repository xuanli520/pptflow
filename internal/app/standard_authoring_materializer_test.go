package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/taskpolicy"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestStandardAuthoringMaterializerSealsFirstRevisionAndBindsReceiptToStageArtifact(t *testing.T) {
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
		WorkflowTemplateVersion: workflowadapter.StandardAuthoringCurrentTemplateReference().Version, SessionManifestJSON: `{"format":"test"}`,
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

	template := workflowadapter.StandardAuthoringCurrentWorkflowTemplate()
	profile := lifecycleCompleteProfileForTemplate(t, template)
	resolved, err := template.Compile(profile)
	if err != nil {
		t.Fatal(err)
	}
	stage, found := resolved.Descriptor.Stage(workflowkit.StageKey(workflowadapter.MaterializeTask))
	if !found {
		t.Fatal("compiled authoring workflow omitted materialize_task")
	}
	packageInput.Contract = standardAuthoringTaskPackageContractForSubject(t, packageInput, task.ID, task.Slug, task.Title, source)
	packageInput.Source = source
	contractRaw, err := packageInput.Contract.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	contents := standardAuthoringValidatedPackageContents(t, run.ID, packageInput)
	contents["instruction"] = append([]byte(nil), packageInput.Instruction...)
	contents["task_toml"] = append([]byte(nil), packageInput.TaskTOMLDraft...)
	contents[workflowadapter.AuthoringContractArtifact] = contractRaw
	contents["tests_analysis"] = append([]byte(nil), packageInput.TestsAnalysis...)
	contents["solution_review_decision"] = approvedAuthoringSolutionDecision(t, source, session, run)
	contents["codeedge_package_admission_report"] = []byte(`{}`)
	inputs := standardAuthoringMaterializerBindings(t, stage, contents)
	compiled, err := CompileStandardAuthoringTaskPackage(packageInput)
	if err != nil || !compiled.Report.Passed {
		t.Fatalf("compile materializer fixture package: report=%+v err=%v", compiled.Report, err)
	}
	admissionInputs := make([]workflowkit.ArtifactBinding, 0, 8)
	for _, binding := range inputs {
		switch binding.Name {
		case "instruction", "task_toml", "dockerfile", "solve_script", "test_script", "tests_analysis",
			"candidate_snapshot", "validation_receipt", "final_attestation", workflowadapter.AuthoringContractArtifact:
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
	receipt := parseMaterializerReceipt(t, result)
	if receipt.TaskID != task.ID || receipt.RevisionID != revision.ID || receipt.RevisionDigest != workflowkit.SubjectDigest(revision.TaskDigest) || receipt.AuthoringSessionID != session.ID {
		t.Fatalf("materialization receipt lineage = %+v", receipt)
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
		if reference.ArtifactKey == "task_snapshot" && reference.ID != string(receipt.TaskSnapshot.ID) {
			t.Fatalf("persisted snapshot ref %s, want receipt ref %s", reference.ID, receipt.TaskSnapshot.ID)
		}
	}

	// A retry after Store materialization reuses the same real revision rather
	// than manufacturing another one. The pending stage result may reserve a
	// fresh output ID because the first result was deliberately not committed.
	replayed, err := executor.ExecuteHarborBuiltin(ctx, invocation, workflowadapter.HarborBuiltinOperationPayload{HandlerID: standardAuthoringMaterializeHandlerID})
	if err != nil {
		t.Fatal(err)
	}
	replayedReceipt := parseMaterializerReceipt(t, replayed)
	if replayedReceipt.RevisionID != revision.ID || replayedReceipt.TaskID != task.ID || replayedReceipt.TaskSnapshot.ID == receipt.TaskSnapshot.ID {
		t.Fatalf("materialization replay = %+v, want same revision and a fresh uncommitted output reference", replayedReceipt)
	}
}

func TestStandardAuthoringPackageAdmissionReadsRootContractAndReportsMetadataMismatch(t *testing.T) {
	fixture := newStandardAuthoringContractStageInputFixture(t, workflowkit.StageKey(workflowadapter.CodeEdgePackageAdmission))
	fixture.contents["task_toml"] = []byte(strings.Replace(string(fixture.contents["task_toml"]), `task_type = "bugfix"`, `task_type = "feature"`, 1))
	request := fixture.request(t)
	inputs, err := standardAuthoringPackageAdmissionInputs(context.Background(), request, fixture.run)
	if err != nil {
		t.Fatal(err)
	}
	if inputs.contract != fixture.packageInput.Contract {
		t.Fatalf("package admission root contract = %+v, want %+v", inputs.contract, fixture.packageInput.Contract)
	}
	compiled, err := CompileStandardAuthoringTaskPackage(StandardAuthoringTaskPackageInput{
		Instruction: inputs.instruction, TaskTOMLDraft: inputs.taskTOML, Dockerfile: inputs.dockerfile,
		SolveScript: inputs.solveScript, TestScript: inputs.testScript, TestsAnalysis: inputs.testsAnalysis,
		Source: *fixture.subject.AuthoringSource, Contract: inputs.contract, Admission: fixture.packageInput.Admission,
	})
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Report.Passed {
		t.Fatalf("package admission mismatch unexpectedly passed: %+v", compiled.Report)
	}
	assertAdmissionViolationMessage(t, compiled.Report, "task_metadata", "metadata.task_type")
}

func TestStandardAuthoringPackageAdmissionReturnsNeedsRepairForMalformedModelContent(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string][]byte)
		code   string
	}{
		{
			name: "instruction",
			mutate: func(contents map[string][]byte) {
				contents["instruction"] = []byte("\x00")
			},
			code: "task_instruction",
		},
		{
			name: "task TOML",
			mutate: func(contents map[string][]byte) {
				contents["task_toml"] = []byte("{not valid TOML or a wrapper")
			},
			code: "task_metadata",
		},
		{
			name: "tests analysis",
			mutate: func(contents map[string][]byte) {
				contents["tests_analysis"] = []byte(`{"provided_information":"\u0000","theoretical_path":"path","passing_evidence":"evidence"}`)
			},
			code: "tests_analysis",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := executeStandardAuthoringPackageAdmissionFixture(t, test.mutate)
			if result.Outcome.Status != workflowkit.StatusCompleted || result.Outcome.Verdict != workflowkit.VerdictNeedsRepair || len(result.Artifacts) != 1 {
				t.Fatalf("package admission result = %+v", result)
			}
			var receipt standardAuthoringAdmissionReceipt
			if err := json.Unmarshal(result.Artifacts[0].Content, &receipt); err != nil {
				t.Fatal(err)
			}
			if receipt.Report.Passed {
				t.Fatalf("malformed model content unexpectedly passed: %+v", receipt.Report)
			}
			assertAdmissionCode(t, receipt.Report, test.code)
		})
	}
}

func TestStandardAuthoringPackageAdmissionStrictlyParsesRootContract(t *testing.T) {
	fixture := newStandardAuthoringContractStageInputFixture(t, workflowkit.StageKey(workflowadapter.CodeEdgePackageAdmission))
	fixture.contents[workflowadapter.AuthoringContractArtifact] = []byte(`{"format":"harbor.standard-authoring-contract.v2","version":"2","unexpected":true}`)
	_, err := standardAuthoringPackageAdmissionInputs(context.Background(), fixture.request(t), fixture.run)
	if err == nil || !strings.Contains(err.Error(), "decode frozen Standard authoring root contract") {
		t.Fatalf("malformed package admission root contract error = %v", err)
	}
}

func TestStandardAuthoringPackageAdmissionRejectsCandidateEvidenceThatDoesNotMatchFiles(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(map[string][]byte)
	}{
		{
			name: "candidate script changed",
			mutate: func(contents map[string][]byte) {
				contents["solve_script"] = []byte("#!/bin/sh\necho tampered\n")
				contents["_preserve_v3_evidence"] = []byte("true")
			},
		},
		{
			name: "validation receipt changed",
			mutate: func(contents map[string][]byte) {
				receipt := append([]byte(nil), contents["validation_receipt"]...)
				contents["validation_receipt"] = append(receipt, 'x')
				contents["_preserve_v3_evidence"] = []byte("true")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newStandardAuthoringContractStageInputFixture(t, workflowkit.StageKey(workflowadapter.CodeEdgePackageAdmission))
			test.mutate(fixture.contents)
			_, err := standardAuthoringPackageAdmissionInputs(context.Background(), fixture.request(t), fixture.run)
			if err == nil || !strings.Contains(err.Error(), "candidate") {
				t.Fatalf("mismatched candidate evidence error = %v", err)
			}
		})
	}
}

func executeStandardAuthoringPackageAdmissionFixture(t *testing.T, mutate func(map[string][]byte)) workflowkit.StageExecutionResult {
	t.Helper()
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
		IdempotencyKey: "package-admission-source", Actor: "author", Reason: "freeze source",
	})
	if err != nil {
		t.Fatal(err)
	}
	task, err := database.CreateTaskV2(ctx, store.CreateTaskV2Request{
		Slug: "package-admission-task", Title: "Package admission task", MetadataJSON: `{}`,
		SourceRepo: source.RepositoryURL, SourceCommit: source.CommitSHA, Actor: "author", Reason: "reserve draft task",
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := database.CreateAuthoringSession(ctx, store.CreateAuthoringSessionRequest{
		SourceID: source.ID, TargetTaskID: task.ID, WorkflowTemplateID: workflowadapter.StandardAuthoringWorkflowTemplateID,
		WorkflowTemplateVersion: workflowadapter.StandardAuthoringCurrentTemplateReference().Version, SessionManifestJSON: `{"format":"test"}`,
		IdempotencyKey: "package-admission-session", Actor: "author", Reason: "freeze session",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.CreateAuthoringWorkflowRun(ctx, store.CreateAuthoringWorkflowRunRequest{
		AuthoringSessionID: session.ID, WorkflowTemplateID: session.WorkflowTemplateID, WorkflowTemplateVersion: session.WorkflowTemplateVersion,
		ResolvedProfileHash: "sha256:package-admission-profile", DefinitionHash: "sha256:package-admission-definition", RunManifestJSON: `{}`,
		Trigger: "task.generate", Actor: "author", Reason: "start package admission fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err = database.TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunRunning, Actor: "author", Reason: "run package admission",
	})
	if err != nil {
		t.Fatal(err)
	}
	template := workflowadapter.StandardAuthoringCurrentWorkflowTemplate()
	profile := lifecycleCompleteProfileForTemplate(t, template)
	resolved, err := template.Compile(profile)
	if err != nil {
		t.Fatal(err)
	}
	stage, found := resolved.Descriptor.Stage(workflowkit.StageKey(workflowadapter.CodeEdgePackageAdmission))
	if !found {
		t.Fatal("compiled authoring workflow omitted codeedge_package_admission")
	}
	packageInput.Contract = standardAuthoringTaskPackageContractForSubject(t, packageInput, task.ID, task.Slug, task.Title, source)
	contractRaw, err := packageInput.Contract.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	contents := standardAuthoringValidatedPackageContents(t, run.ID, packageInput)
	contents["instruction"] = append([]byte(nil), packageInput.Instruction...)
	contents["task_toml"] = append([]byte(nil), packageInput.TaskTOMLDraft...)
	contents["tests_analysis"] = append([]byte(nil), packageInput.TestsAnalysis...)
	contents[workflowadapter.AuthoringContractArtifact] = contractRaw
	mutate(contents)
	inputs := standardAuthoringMaterializerBindings(t, stage, contents)
	subject := workflowkit.SubjectBinding{SubjectID: source.ID, RevisionID: session.ID, Digest: workflowkit.SubjectDigest(source.SnapshotContentDigest)}
	request := workflowkit.StageExecutionRequest{
		Execution: workflowkit.FrozenExecution{ID: run.ID, Subject: subject, Actor: "worker"},
		Claim:     workflowkit.JobClaim{Stage: &workflowkit.StageClaim{StageAttempt: workflowkit.AttemptIdentity{ID: "package-admission-attempt"}, Stage: stage}},
		Stage:     stage, Inputs: inputs,
		ReadInput: func(_ context.Context, binding workflowkit.ArtifactBinding) ([]byte, error) {
			return append([]byte(nil), contents[binding.Name]...), nil
		},
	}
	payload := workflowadapter.HarborBuiltinOperationPayload{HandlerID: standardAuthoringPackageAdmissionHandlerID}
	result, err := executor.ExecuteHarborBuiltin(ctx, stageprovider.StageOperationInvocation{
		Request: request,
		Resolution: workflowadapter.StageOperationResolution{
			StageKey: stage.Key, Operation: workflowadapter.StageOperationBinding{Payload: payload},
		},
	}, payload)
	if err != nil {
		t.Fatalf("package admission returned infrastructure error: %v", err)
	}
	return result
}

func TestStandardAuthoringMaterializerStrictlyParsesRootContract(t *testing.T) {
	fixture := newStandardAuthoringContractStageInputFixture(t, workflowkit.StageKey(workflowadapter.MaterializeTask))
	fixture.contents[workflowadapter.AuthoringContractArtifact] = []byte(`{"format":"harbor.standard-authoring-contract.v2","version":"2"} trailing`)
	_, err := standardAuthoringMaterializeInputs(context.Background(), fixture.request(t), fixture.run, fixture.subject)
	if err == nil || !strings.Contains(err.Error(), "decode frozen Standard authoring root contract") {
		t.Fatalf("malformed materializer root contract error = %v", err)
	}
}

type standardAuthoringContractStageInputFixture struct {
	stage        workflowkit.StageDescriptor
	run          store.WorkflowRun
	subject      workflowRunSubject
	contents     map[string][]byte
	packageInput StandardAuthoringTaskPackageInput
}

func newStandardAuthoringContractStageInputFixture(t *testing.T, stageKey workflowkit.StageKey) standardAuthoringContractStageInputFixture {
	t.Helper()
	template := workflowadapter.StandardAuthoringCurrentWorkflowTemplate()
	profile := lifecycleCompleteProfileForTemplate(t, template)
	resolved, err := template.Compile(profile)
	if err != nil {
		t.Fatal(err)
	}
	stage, found := resolved.Descriptor.Stage(stageKey)
	if !found {
		t.Fatalf("compiled root-contract authoring workflow omitted %s", stageKey)
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
	targetTaskID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	session := store.AuthoringSession{ID: sessionID, TargetTaskID: targetTaskID}
	run := store.WorkflowRun{
		ID: runID, WorkflowTemplateID: workflowadapter.StandardAuthoringWorkflowTemplateID,
		WorkflowTemplateVersion: workflowadapter.StandardAuthoringCurrentTemplateReference().Version,
	}
	packageInput.Contract = standardAuthoringTaskPackageContractForSubject(t, packageInput, targetTaskID, "fixture", "Fixture", source)
	contractRaw, err := packageInput.Contract.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	contents := standardAuthoringValidatedPackageContents(t, run.ID, packageInput)
	contents["instruction"] = append([]byte(nil), packageInput.Instruction...)
	contents["task_toml"] = append([]byte(nil), packageInput.TaskTOMLDraft...)
	contents[workflowadapter.AuthoringContractArtifact] = contractRaw
	contents["tests_analysis"] = append([]byte(nil), packageInput.TestsAnalysis...)
	contents["solution_review_decision"] = approvedAuthoringSolutionDecision(t, source, session, run)
	contents["codeedge_package_admission_report"] = []byte(`{"format":"fixture"}`)
	standardAuthoringRefreshV3FixtureEvidence(t, stage, contents)
	subject := workflowRunSubject{
		Binding: workflowkit.SubjectBinding{SubjectID: source.ID, RevisionID: session.ID, Digest: workflowkit.SubjectDigest(sourceDigest)},
		Kind:    store.WorkflowRunSubjectAuthoringSession, AuthoringSource: &source, AuthoringSession: &session,
	}
	return standardAuthoringContractStageInputFixture{stage: stage, run: run, subject: subject, contents: contents, packageInput: packageInput}
}

func (fixture standardAuthoringContractStageInputFixture) request(t *testing.T) workflowkit.StageExecutionRequest {
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
	standardAuthoringRefreshV3FixtureEvidence(t, stage, contents)
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

func standardAuthoringValidatedPackageContents(t *testing.T, runID string, input StandardAuthoringTaskPackageInput) map[string][]byte {
	t.Helper()
	return map[string][]byte{
		"dockerfile":   append([]byte(nil), input.Dockerfile...),
		"solve_script": append([]byte(nil), input.SolveScript...),
		"test_script":  append([]byte(nil), input.TestScript...),
	}
}

func standardAuthoringRefreshV3FixtureEvidence(t *testing.T, stage workflowkit.StageDescriptor, contents map[string][]byte) {
	t.Helper()
	if _, preserve := contents["_preserve_v3_evidence"]; preserve {
		return
	}
	requiresCandidate := false
	for _, input := range stage.Inputs {
		if input.Name == "candidate_snapshot" {
			requiresCandidate = true
			break
		}
	}
	if !requiresCandidate {
		return
	}
	files := standardAuthoringV3CandidateFiles(contents["instruction"], contents["task_toml"], contents["dockerfile"], contents["solve_script"], contents["test_script"], contents["tests_analysis"])
	manifest := make([]workflowkit.CandidateFile, 0, len(files))
	for path, content := range files {
		if len(content) == 0 {
			return
		}
		manifest = append(manifest, workflowkit.CandidateFile{Path: path, SchemaVersion: "harbor.artifact.v1", ContentDigest: workflowkit.SHA256Fingerprint(content), SizeBytes: int64(len(content))})
	}
	snapshot, err := workflowkit.NewCandidateSnapshot(manifest)
	if err != nil {
		t.Fatal(err)
	}
	snapshotRaw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	contract, err := workflowkit.NewCandidateValidationContract(workflowkit.SHA256Fingerprint([]byte("fixture-runtime-contract")), workflowkit.SHA256Fingerprint([]byte("fixture-verification-contract")))
	if err != nil {
		t.Fatal(err)
	}
	contractDigest, err := contract.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	receipt, err := workflowkit.NewValidationReceipt(workflowkit.ValidationReceipt{SnapshotDigest: snapshot.Digest, ContractDigest: contractDigest, Verdict: workflowkit.ValidationPass, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	receiptRaw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	attestationRaw, err := json.Marshal(standardAuthoringFinalAttestation{Format: "harbor.standard-authoring-final-attestation.v1", Version: "1", SnapshotDigest: snapshot.Digest, ValidationReceiptDigest: receipt.Digest, ContentReviewDigest: workflowkit.SHA256Fingerprint([]byte("fixture-content-review")), SolutionReviewDigest: workflowkit.SHA256Fingerprint([]byte("fixture-solution-review"))})
	if err != nil {
		t.Fatal(err)
	}
	contents["candidate_snapshot"] = snapshotRaw
	contents["validation_receipt"] = receiptRaw
	contents["final_attestation"] = attestationRaw
}

func parseMaterializerReceipt(t *testing.T, result workflowkit.StageExecutionResult) workflowadapter.StandardAuthoringMaterializationReceipt {
	t.Helper()
	for _, artifact := range result.Artifacts {
		if artifact.Name == workflowadapter.StandardAuthoringMaterializationReceiptArtifact {
			receipt, err := workflowadapter.ParseStandardAuthoringMaterializationReceiptJSON(artifact.Content)
			if err != nil {
				t.Fatal(err)
			}
			return receipt
		}
	}
	t.Fatal("materializer result omitted materialization receipt")
	return workflowadapter.StandardAuthoringMaterializationReceipt{}
}
