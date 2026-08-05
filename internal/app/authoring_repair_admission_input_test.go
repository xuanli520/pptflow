package app

import (
	"context"
	"errors"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func startAuthoringRepairFixture(t *testing.T) (*LifecycleServices, store.WorkflowRun, workflowRunSubject, workflowkit.WorkflowDescriptor) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	capturer := &standardAuthoringSourceCapturerFixture{coordinate: standardAuthoringLaunchTestCoordinate, snapshot: standardAuthoringLaunchTestSnapshot(t, standardAuthoringLaunchTestCoordinate)}
	definitions := standardAuthoringLaunchTestDefinitionProvider(t)
	services, err := NewLifecycleServicesWithOptions(root, database, standardAuthoringLaunchTestOptions(capturer, definitions, definitions.catalog))
	if err != nil {
		t.Fatal(err)
	}
	key, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	command := StandardAuthoringLaunchCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{IdempotencyKey: key, Actor: "author", Reason: "create immutable source task"},
		RepositoryURL:                standardAuthoringLaunchTestCoordinate.RepositoryURL,
		CommitSHA:                    standardAuthoringLaunchTestCoordinate.CommitSHA,
		BaseImage:                    standardAuthoringLaunchTestBaseImage,
		TaskType:                     standardAuthoringLaunchTestTaskType, Application: standardAuthoringLaunchTestApplication, CodeLang: "rust", Objective: standardAuthoringLaunchTestObjective,
		Slug: "fixture-authoring-repair", Title: "Fixture authoring repair", MetadataJSON: `{"difficulty":"hard"}`,
	}
	receipt, err := services.AuthoringLaunches.Start(ctx, command)
	if err != nil {
		t.Fatalf("start Standard authoring: %v", err)
	}
	run, err := database.GetWorkflowRun(ctx, receipt.RunID)
	if err != nil || run == nil {
		t.Fatalf("load authoring Run: run=%+v err=%v", run, err)
	}
	subject, err := services.core.resolveWorkflowRunSubject(ctx, *run)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := workflowadapter.StandardAuthoringCurrentWorkflowTemplate().Compile(standardAuthoringLaunchTestProfile())
	if err != nil {
		t.Fatal(err)
	}
	return services, *run, subject, resolved.Descriptor
}

func createAuthoringAdmissionReport(t *testing.T, services *LifecycleServices, run store.WorkflowRun, subject workflowRunSubject, descriptor workflowkit.WorkflowDescriptor) store.StageAttempt {
	t.Helper()
	ctx := context.Background()
	emptyInputs, err := workflowkit.FingerprintArtifactBindings(nil)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := services.core.store.CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: run.ID, StageKey: string(workflowadapter.CodeEdgePackageAdmission), StageGroup: "final_review", Ordinal: 1,
		InputFingerprint: string(emptyInputs), BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`, Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	node, err := services.core.store.CreateNodeAttempt(ctx, store.CreateNodeAttemptRequest{
		StageAttemptID: attempt.ID, NodeID: string(workflowadapter.CodeEdgePackageAdmission), Generation: 0, Attempt: 1,
		IdempotencyKey: "admission-report-node", Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err = services.core.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: attempt.ID, ExpectedVersion: attempt.Version, ExecutionStatus: store.StageExecutionRunning, Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	node, err = services.core.store.TransitionNodeAttempt(ctx, store.TransitionNodeAttemptRequest{
		NodeAttemptID: node.ID, ExpectedVersion: node.Version, Status: store.NodeAttemptRunning, Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	stage, exists := descriptor.Stage(workflowadapter.CodeEdgePackageAdmission)
	if !exists {
		t.Fatal("admission stage missing from descriptor")
	}
	content := []byte(`{"format":"harbor.standard-authoring-task-package-admission.v1","version":"1","report":{"passed":false,"violations":[]}}`)
	manifest, references, err := persistStageArtifactsForSubject(ctx, services.core, run, subject, attempt, node, stage, nil, []StageArtifact{{
		Key: workflowadapter.StandardAuthoringPackageAdmissionReportArtifact, SchemaVersion: workflowadapter.StandardAuthoringPackageAdmissionReportSchemaVersion, Content: content, TurnOrdinal: 1,
	}}, "tester", "persist admission report")
	if err != nil {
		t.Fatalf("persist admission report: %v", err)
	}
	if len(references) != 1 || references[0].ArtifactKey != workflowadapter.StandardAuthoringPackageAdmissionReportArtifact {
		t.Fatalf("admission report refs = %+v", references)
	}
	attempt, err = services.core.store.TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: attempt.ID, ExpectedVersion: attempt.Version, ExecutionStatus: store.StageExecutionCompleted,
		Verdict: store.VerdictNeedsRepair, ArtifactManifestID: manifest.ID, Actor: "tester", Reason: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := services.core.store.TransitionNodeAttempt(ctx, store.TransitionNodeAttemptRequest{
		NodeAttemptID: node.ID, ExpectedVersion: node.Version, Status: store.NodeAttemptCompleted, Actor: "tester", Reason: "fixture",
	}); err != nil {
		t.Fatal(err)
	}
	return attempt
}

func TestAuthoringRepairAdmissionReportInputBindsLatestReport(t *testing.T) {
	ctx := context.Background()
	services, run, subject, descriptor := startAuthoringRepairFixture(t)
	attempt := createAuthoringAdmissionReport(t, services, run, subject, descriptor)
	state := continuationRunState{Latest: map[workflowkit.NodeID]store.StageAttempt{workflowadapter.CodeEdgePackageAdmission: attempt}}
	invalidation, err := workflowkit.PlanInvalidation(descriptor, workflowkit.InvalidationRequest{
		RecomputeNodes: []workflowkit.NodeID{workflowadapter.CodeEdgePackageAdmission},
	})
	if err != nil {
		t.Fatal(err)
	}
	repairScheduled := false
	for _, entry := range invalidation.Entries {
		if entry.NodeID == workflowkit.StageKey(workflowadapter.AuthoringRepair) {
			repairScheduled = entry.Impact == workflowkit.ImpactInvalidate || entry.Impact == workflowkit.ImpactRequiresConfirmation
		}
	}
	if !repairScheduled {
		t.Fatal("authoring repair stage was not invalidated by admission recompute")
	}
	bindings, err := services.Continuations.authoringRepairAdmissionReportInput(ctx, run, subject, descriptor, state, invalidation)
	if err != nil {
		t.Fatalf("admission report input: %v", err)
	}
	if len(bindings) != 1 || bindings[0].Name != workflowadapter.StandardAuthoringPackageAdmissionReportArtifact ||
		bindings[0].ArtifactID == "" || bindings[0].ContentDigest == "" || bindings[0].SchemaVersion != workflowadapter.StandardAuthoringPackageAdmissionReportSchemaVersion {
		t.Fatalf("admission report bindings = %+v", bindings)
	}
}

func TestAuthoringRepairAdmissionReportInputSkipsWhenRepairPreserved(t *testing.T) {
	ctx := context.Background()
	services, run, subject, descriptor := startAuthoringRepairFixture(t)
	state := continuationRunState{Latest: map[workflowkit.NodeID]store.StageAttempt{}}
	// Recomputing an unrelated node (repo_prepare) preserves the repair stage.
	invalidation, err := workflowkit.PlanInvalidation(descriptor, workflowkit.InvalidationRequest{
		RecomputeNodes: []workflowkit.NodeID{workflowadapter.RepoPrepare},
	})
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := services.Continuations.authoringRepairAdmissionReportInput(ctx, run, subject, descriptor, state, invalidation)
	if err != nil {
		t.Fatalf("admission report input: %v", err)
	}
	if len(bindings) != 0 {
		t.Fatalf("unexpected admission report bindings for preserved repair stage = %+v", bindings)
	}
}

func TestAuthoringRepairAcceptsPlannedAdmissionReportWithoutFrozenPort(t *testing.T) {
	ctx := context.Background()
	services, run, subject, descriptor := startAuthoringRepairFixture(t)
	attempt := createAuthoringAdmissionReport(t, services, run, subject, descriptor)
	references, err := services.core.store.ListArtifactRefs(ctx, attempt.ArtifactManifestID)
	if err != nil || len(references) != 1 {
		t.Fatalf("admission report refs = %+v, err=%v", references, err)
	}
	reference := references[0]
	binding := workflowkit.ArtifactBinding{
		Name:          workflowadapter.StandardAuthoringPackageAdmissionReportArtifact,
		ArtifactID:    workflowkit.ArtifactID(reference.ID),
		ContentDigest: workflowkit.Fingerprint(reference.ContentDigest),
		SchemaVersion: reference.SchemaVersion,
	}
	// An older frozen descriptor may predate the report port; the
	// continuation-plan-only repair input is still accepted.
	repairStage := workflowkit.StageDescriptor{
		Key: workflowkit.StageKey(workflowadapter.AuthoringRepair), Version: "v1",
		Inputs: []workflowkit.ArtifactSpec{},
	}
	inputs, err := resolveStageInputsForSubjectWithExplicitInputs(ctx, services.core.store, services.core.objects, run, subject, repairStage, []workflowkit.ArtifactBinding{binding})
	if err != nil {
		t.Fatalf("resolve planned admission report input: %v", err)
	}
	found := false
	for _, resolved := range inputs {
		if resolved.Name == binding.Name {
			found = true
			if resolved != binding {
				t.Fatalf("planned admission report binding changed = %+v", resolved)
			}
		}
	}
	if !found {
		t.Fatalf("planned admission report input missing from resolved inputs = %+v", inputs)
	}
}

func TestAuthoringRepairRejectsUndeclaredNonRepairInput(t *testing.T) {
	ctx := context.Background()
	services, run, subject, _ := startAuthoringRepairFixture(t)
	binding := workflowkit.ArtifactBinding{
		Name: "instruction", ArtifactID: "019fd1d5-a1bf-755a-850e-d4b6ebb191bd",
		ContentDigest: workflowkit.SHA256Fingerprint([]byte("not real")), SchemaVersion: "harbor.artifact.v1",
	}
	repairStage := workflowkit.StageDescriptor{
		Key: workflowkit.StageKey(workflowadapter.AuthoringRepair), Version: "v1",
		Inputs: []workflowkit.ArtifactSpec{},
	}
	_, err := resolveStageInputsForSubjectWithExplicitInputs(ctx, services.core.store, services.core.objects, run, subject, repairStage, []workflowkit.ArtifactBinding{binding})
	if err == nil || !errors.Is(err, ErrInvalidStageExecution) {
		t.Fatalf("undeclared non-repair explicit input error = %v, want ErrInvalidStageExecution", err)
	}
}
