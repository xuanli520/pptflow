package app

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/testsupport"
	"github.com/purplevoid/harbor-factory/internal/workflowruntime"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestStandardAuthoringLaunchCapturesSourceCreatesRevisionFreeTaskAndQueuesRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	capturer := &standardAuthoringSourceCapturerFixture{snapshot: standardAuthoringLaunchTestSnapshot(t)}
	definitions := standardAuthoringLaunchTestDefinitionProvider(t)
	services, err := NewLifecycleServicesWithOptions(root, database, LifecycleServicesOptions{
		OperationResolver:                      testsupport.AcceptAllStageOperationResolver(),
		StandardAuthoringSourceCapturer:        capturer,
		StandardAuthoringRunDefinitionProvider: definitions,
	})
	if err != nil {
		t.Fatal(err)
	}
	key, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	command := StandardAuthoringLaunchCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{IdempotencyKey: key, Actor: "author", Reason: "create fixed Tower HTTP task"},
		Slug:                         "tower-http-authoring", Title: "Tower HTTP authoring", MetadataJSON: `{"difficulty":"hard"}`,
	}
	receipt, err := services.AuthoringLaunches.Start(ctx, command)
	if err != nil {
		t.Fatalf("start Standard authoring: %v", err)
	}
	if receipt.Action != standardAuthoringLaunchAction || receipt.AuthoringSourceID == "" || receipt.AuthoringSessionID == "" || receipt.TaskID == "" || receipt.RunID == "" {
		t.Fatalf("launch receipt is incomplete: %+v", receipt)
	}
	if capturer.calls != 1 {
		t.Fatalf("source capturer calls = %d, want 1", capturer.calls)
	}

	source, err := database.GetAuthoringSource(ctx, receipt.AuthoringSourceID)
	if err != nil || source == nil {
		t.Fatalf("load AuthoringSource: source=%+v err=%v", source, err)
	}
	if source.RepositoryURL != StandardAuthoringSourceRepositoryURL || source.CommitSHA != StandardAuthoringSourceCommit || source.SnapshotArtifactRef != source.SnapshotContentDigest || source.SnapshotSchemaVersion != StandardAuthoringSourceSnapshotSchemaVersion {
		t.Fatalf("frozen source = %+v", source)
	}
	object, err := services.core.objects.ReadAll(ctx, workflowruntime.ObjectRef{Digest: workflowkit.Fingerprint(source.SnapshotContentDigest), SizeBytes: int64(len(capturer.snapshot.Content))})
	if err != nil || !bytes.Equal(object, capturer.snapshot.Content) {
		t.Fatalf("source object = %d bytes err=%v", len(object), err)
	}

	task, err := database.GetTaskV2(ctx, receipt.TaskID)
	if err != nil || task == nil {
		t.Fatalf("load draft Task: task=%+v err=%v", task, err)
	}
	if task.LifecycleState != store.TaskLifecycleDraft || task.CurrentRevisionID != "" || task.SourceRepo != source.RepositoryURL || task.SourceCommit != source.CommitSHA {
		t.Fatalf("authoring task must be a source-bound revision-free draft: %+v", task)
	}
	session, err := database.GetAuthoringSession(ctx, receipt.AuthoringSessionID)
	if err != nil || session == nil || session.SourceID != source.ID || session.TargetTaskID != task.ID {
		t.Fatalf("authoring session = %+v err=%v", session, err)
	}
	run, err := database.GetWorkflowRun(ctx, receipt.RunID)
	if err != nil || run == nil {
		t.Fatalf("load authoring Run: run=%+v err=%v", run, err)
	}
	if run.SubjectKind != store.WorkflowRunSubjectAuthoringSession || run.TaskID != "" || run.RevisionID != "" || run.AuthoringSessionID != session.ID || run.SubjectID != source.ID || run.SubjectRevisionID != session.ID || run.SubjectDigest != source.SnapshotContentDigest {
		t.Fatalf("authoring Run subject = %+v", run)
	}
	if _, _, err := services.core.verifyRunManagedExecutionInputs(ctx, *run); err != nil {
		t.Fatalf("verify frozen authoring execution inputs: %v", err)
	}

	replayed, err := services.AuthoringLaunches.Start(ctx, command)
	if err != nil {
		t.Fatalf("replay Standard authoring: %v", err)
	}
	if replayed != receipt || capturer.calls != 1 {
		t.Fatalf("replay = %+v; first=%+v; capturer calls=%d", replayed, receipt, capturer.calls)
	}
}

func TestStandardAuthoringLaunchCompletedKeyReturnsOriginalReceiptWithoutRecapture(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	capturer := &standardAuthoringSourceCapturerFixture{snapshot: standardAuthoringLaunchTestSnapshot(t)}
	services, err := NewLifecycleServicesWithOptions(root, database, LifecycleServicesOptions{
		OperationResolver:                      testsupport.AcceptAllStageOperationResolver(),
		StandardAuthoringSourceCapturer:        capturer,
		StandardAuthoringRunDefinitionProvider: standardAuthoringLaunchTestDefinitionProvider(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	key, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	base := StandardAuthoringLaunchCommand{LifecycleMutationCommandBase: LifecycleMutationCommandBase{IdempotencyKey: key, Actor: "author", Reason: "capture source"}, Slug: "tower-http", Title: "Tower HTTP", MetadataJSON: `{}`}
	first, err := services.AuthoringLaunches.Start(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	changed := base
	changed.Title = "different title"
	replayed, err := services.AuthoringLaunches.Start(ctx, changed)
	if err != nil || replayed != first || capturer.calls != 1 {
		t.Fatalf("completed launch replay = %+v err=%v first=%+v calls=%d", replayed, err, first, capturer.calls)
	}
}

type standardAuthoringSourceCapturerFixture struct {
	snapshot StandardAuthoringSourceSnapshot
	calls    int
}

func (fixture *standardAuthoringSourceCapturerFixture) CaptureStandardAuthoringSource(context.Context) (StandardAuthoringSourceSnapshot, error) {
	fixture.calls++
	result := fixture.snapshot
	result.Content = append([]byte(nil), fixture.snapshot.Content...)
	return result, nil
}

func standardAuthoringLaunchTestSnapshot(t *testing.T) StandardAuthoringSourceSnapshot {
	t.Helper()
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	for name, content := range map[string]string{
		"tower-http/Cargo.toml": "[package]\nname = \"tower-http\"\n",
		"tower-http/src/lib.rs": "pub fn fixture() {}\n",
	} {
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return StandardAuthoringSourceSnapshot{RepositoryURL: StandardAuthoringSourceRepositoryURL, CommitSHA: StandardAuthoringSourceCommit, SchemaVersion: StandardAuthoringSourceSnapshotSchemaVersion, Content: archive.Bytes()}
}

func standardAuthoringLaunchTestDefinitionProvider(t *testing.T) *CatalogStandardAuthoringRunDefinitionProvider {
	t.Helper()
	catalogDocument := stageprovider.DeploymentOperationCatalog{
		Format: stageprovider.DeploymentOperationCatalogFormat, Version: stageprovider.DeploymentOperationCatalogVersion,
		CatalogID: "standard-authoring-test", CatalogVersion: "1", Template: workflowadapter.StandardAuthoringTemplateReference(), Operations: []stageprovider.DeploymentOperationRegistration{},
	}
	for _, stage := range workflowadapter.StandardAuthoringStageCatalog().Stages {
		operation := workflowadapter.StageOperationBinding{ProviderID: "standard-authoring-test-provider", OperationID: "test." + string(stage.Key), Version: "1"}
		switch stage.Key {
		case workflowkit.StageKey(workflowadapter.RepoPrepare):
			operation.Payload = workflowadapter.LocalCommandOperationPayload{CommandID: "test-source-capture", Arguments: []string{}}
		case workflowkit.StageKey(workflowadapter.TaskReview), workflowkit.StageKey(workflowadapter.ContentReview), workflowkit.StageKey(workflowadapter.SolutionReview):
			operation.Payload = workflowadapter.DurableReviewOperationPayload{PolicyID: "test-review"}
		case workflowkit.StageKey(workflowadapter.MaterializeTask):
			operation.Payload = workflowadapter.HarborBuiltinOperationPayload{HandlerID: "test-materialize"}
		default:
			operation.Payload = workflowadapter.AgentTurnOperationPayload{AgentID: "test-agent", ModelID: "test-model", MaxTurns: stage.RequiredTurns}
		}
		catalogDocument.Operations = append(catalogDocument.Operations, stageprovider.DeploymentOperationRegistration{
			Stage:     stageprovider.DeploymentStageContract{Key: stage.Key, Type: standardAuthoringLaunchTestStageType(t, stage.Key), Group: stage.Group, Plugin: workflowkit.PluginBinding{ID: stage.Plugin.ID, Version: stage.Plugin.Version}},
			Provider:  workflowadapter.ProviderReference{ID: "standard-authoring-test-provider", Kind: "test", Version: "1"},
			Operation: operation, Runtime: workflowadapter.RuntimeReference{ID: "standard-authoring-test-runtime", Kind: "test", Version: "1"},
			Checkout: stageprovider.DeploymentCheckoutContract{ID: "standard-authoring-test-checkout", Purpose: "source"}, Secrets: []workflowadapter.SecretReference{},
		})
	}
	catalog, err := stageprovider.NewDeploymentOperationCatalogResolver(catalogDocument)
	if err != nil {
		t.Fatalf("build test Standard authoring catalog: %v", err)
	}
	provider, err := NewCatalogStandardAuthoringRunDefinitionProvider(catalog, standardAuthoringLaunchTestProfile())
	if err != nil {
		t.Fatalf("build test Standard authoring definition provider: %v", err)
	}
	return provider
}

func standardAuthoringLaunchTestStageType(t *testing.T, key workflowkit.StageKey) workflowadapter.StageBindingType {
	t.Helper()
	switch key {
	case workflowkit.StageKey(workflowadapter.RepoPrepare):
		return workflowadapter.StageBindingRepoPrepare
	case workflowkit.StageKey(workflowadapter.RepoAnalyze):
		return workflowadapter.StageBindingRepoAnalyze
	case workflowkit.StageKey(workflowadapter.TaskDesign):
		return workflowadapter.StageBindingTaskDesign
	case workflowkit.StageKey(workflowadapter.TaskReview):
		return workflowadapter.StageBindingTaskReview
	case workflowkit.StageKey(workflowadapter.GenerateTaskFiles):
		return workflowadapter.StageBindingGenerateTaskFiles
	case workflowkit.StageKey(workflowadapter.InstructionGen):
		return workflowadapter.StageBindingInstructionGen
	case workflowkit.StageKey(workflowadapter.TaskTOMLGen):
		return workflowadapter.StageBindingTaskTOMLGen
	case workflowkit.StageKey(workflowadapter.DockerfileGen):
		return workflowadapter.StageBindingDockerfileGen
	case workflowkit.StageKey(workflowadapter.ContentReview):
		return workflowadapter.StageBindingContentReview
	case workflowkit.StageKey(workflowadapter.SolveGen):
		return workflowadapter.StageBindingSolveGen
	case workflowkit.StageKey(workflowadapter.TestGen):
		return workflowadapter.StageBindingTestGen
	case workflowkit.StageKey(workflowadapter.TestsAnalysis):
		return workflowadapter.StageBindingTestsAnalysis
	case workflowkit.StageKey(workflowadapter.SolutionReview):
		return workflowadapter.StageBindingSolutionReview
	case workflowkit.StageKey(workflowadapter.MaterializeTask):
		return workflowadapter.StageBindingMaterializeTask
	default:
		t.Fatalf("unsupported Standard authoring test stage %q", key)
		return ""
	}
}

func standardAuthoringLaunchTestProfile() workflowadapter.ExecutionProfile {
	template := workflowadapter.StandardAuthoringWorkflowTemplate()
	profile := workflowadapter.ExecutionProfile{
		Template: template.Reference(), ID: "standard-authoring-test", Version: "1", ContinuationPlanTTL: workflowadapter.RequiredContinuationPlanTTL,
		ControlGracePeriod: 30 * time.Second, CandidateProviderBudget: workflowadapter.CandidateProviderBudget{AttemptTimeout: time.Minute},
	}
	for _, stage := range template.Catalog.Stages {
		turns := max(1, stage.RequiredTurns)
		profile.Stages = append(profile.Stages, workflowadapter.StageBudget{StageKey: stage.Key, Budget: workflowkit.ExecutionBudget{
			TurnTimeout: time.Second, MaxTurns: turns, AttemptTimeout: time.Duration(turns) * time.Second, MaxAttempts: 1, MaxElapsed: time.Duration(turns) * time.Second,
		}})
	}
	if err := profile.Validate(); err != nil {
		panic(fmt.Sprintf("invalid Standard authoring test profile: %v", err))
	}
	return profile
}
