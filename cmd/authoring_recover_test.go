package cmd

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/testsupport"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestAuthoringRecoverCommandPreviewsExecutesAndReplays(t *testing.T) {
	ctx := context.Background()
	if defaultLifecycleActor() == "" {
		t.Skip("local OS actor is unavailable in this test environment")
	}

	root, run, config := newCommandAuthoringRecoveryFixture(t, ctx)
	beforePreview := snapshotCommandControlPlane(t, root)
	previewOutput, err := executeAuthoringCommand(t, ctx, config, []string{
		"recover", "--run", run.ID, "--idempotency-key", commandLifecycleUUID(t),
		"--reason", "inspect recoverable Standard authoring Run", "--dry-run",
	})
	if err != nil {
		t.Fatalf("authoring recover dry-run: %v\n%s", err, previewOutput)
	}
	var preview taskContinuationPlanOutput
	if err := json.Unmarshal([]byte(previewOutput), &preview); err != nil {
		t.Fatalf("decode authoring recover dry-run: %v\n%s", err, previewOutput)
	}
	if preview.Persisted || preview.Executable || preview.Plan.PlanID == "" || preview.Plan.SourceRunID != run.ID ||
		preview.Plan.Strategy != workflowkit.StrategyRetryAttempt {
		t.Fatalf("authoring recover dry-run result = %+v", preview)
	}
	if afterPreview := snapshotCommandControlPlane(t, root); !reflect.DeepEqual(afterPreview, beforePreview) {
		t.Fatal("authoring recover --dry-run changed durable control-plane state")
	}

	key := commandLifecycleUUID(t)
	args := []string{
		"recover", "--run", run.ID, "--idempotency-key", key,
		"--reason", "recover transient Standard authoring failure",
	}
	firstOutput, err := executeAuthoringCommand(t, ctx, config, args)
	if err != nil {
		t.Fatalf("authoring recover: %v\n%s", err, firstOutput)
	}
	var first store.ContinuationExecution
	if err := json.Unmarshal([]byte(firstOutput), &first); err != nil {
		t.Fatalf("decode authoring recovery execution: %v\n%s", err, firstOutput)
	}
	if first.ID == "" || first.RunID != run.ID || first.PlanID == "" || first.State != store.ContinuationExecutionQueued {
		t.Fatalf("authoring recovery execution = %+v", first)
	}

	replayOutput, err := executeAuthoringCommand(t, ctx, config, args)
	if err != nil {
		t.Fatalf("authoring recover replay: %v\n%s", err, replayOutput)
	}
	var replay store.ContinuationExecution
	if err := json.Unmarshal([]byte(replayOutput), &replay); err != nil {
		t.Fatalf("decode authoring recovery replay: %v\n%s", err, replayOutput)
	}
	if replay.ID != first.ID || replay.PlanID != first.PlanID || replay.Version != first.Version {
		t.Fatalf("authoring recovery replay = %+v, want %+v", replay, first)
	}

	check, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	updated, err := check.GetWorkflowRun(ctx, run.ID)
	if err != nil || updated == nil || updated.ExecutionEpoch != run.ExecutionEpoch+1 || updated.Version != run.Version+1 {
		t.Fatalf("authoring recovery Run = %+v, %v", updated, err)
	}
	jobs, err := check.ListDurableJobsForRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var recoveries int
	for _, job := range jobs {
		if job.CommandType == "task_continuation.execute" {
			recoveries++
		}
	}
	if recoveries != 1 {
		t.Fatalf("authoring recovery durable jobs = %+v", jobs)
	}
}

type commandAuthoringRecoverySourceCapturer struct {
	coordinate app.StandardAuthoringSourceCoordinate
	snapshot   app.StandardAuthoringSourceSnapshot
}

func (capturer commandAuthoringRecoverySourceCapturer) CaptureStandardAuthoringSource(_ context.Context, coordinate app.StandardAuthoringSourceCoordinate) (app.StandardAuthoringSourceSnapshot, error) {
	if coordinate != capturer.coordinate {
		return app.StandardAuthoringSourceSnapshot{}, fmt.Errorf("unexpected source coordinate: %+v", coordinate)
	}
	snapshot := capturer.snapshot
	snapshot.Content = append([]byte(nil), capturer.snapshot.Content...)
	return snapshot, nil
}

func newCommandAuthoringRecoveryFixture(t *testing.T, ctx context.Context) (string, store.WorkflowRun, *lifecycleCLIConfig) {
	t.Helper()
	root := t.TempDir()
	_, catalog, lock := standardAuthoringProductionTestDeployment(t)
	profile, err := lock.StandardAuthoringProfile()
	if err != nil {
		t.Fatal(err)
	}
	definitions, err := app.NewCatalogStandardAuthoringRunDefinitionProvider(catalog, profile)
	if err != nil {
		t.Fatal(err)
	}
	coordinate := app.StandardAuthoringSourceCoordinate{
		RepositoryURL: "https://github.com/example/authoring-recovery.git",
		CommitSHA:     "0123456789abcdef0123456789abcdef01234567",
	}
	capturer := commandAuthoringRecoverySourceCapturer{
		coordinate: coordinate,
		snapshot:   commandAuthoringRecoverySourceSnapshot(t, coordinate),
	}
	newServices := func(factoryRoot string, dataStore *store.Store) (*app.LifecycleServices, error) {
		return app.NewLifecycleServicesWithOptions(factoryRoot, dataStore, app.LifecycleServicesOptions{
			OperationResolver: testsupport.AcceptAllStageOperationResolver(),
			DeploymentCatalogResolvers: []app.TemplateDeploymentCatalogResolver{{
				Template: workflowadapter.StandardAuthoringTemplateReference(), Resolver: catalog,
			}},
			StandardAuthoringSourceCapturer:        capturer,
			StandardAuthoringRunDefinitionProvider: definitions,
		})
	}
	database, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	services, err := newServices(root, database)
	if err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	receipt, err := services.AuthoringLaunches.Start(ctx, app.StandardAuthoringLaunchCommand{
		LifecycleMutationCommandBase: app.LifecycleMutationCommandBase{
			IdempotencyKey: commandLifecycleUUID(t), Actor: "tester", Reason: "create recoverable authoring CLI fixture",
		},
		RepositoryURL: coordinate.RepositoryURL,
		CommitSHA:     coordinate.CommitSHA,
		Slug:          "authoring-recover-cli",
		Title:         "Authoring recover CLI fixture",
		MetadataJSON:  `{}`,
	})
	if err != nil {
		_ = services.Store().Close()
		t.Fatal(err)
	}
	run, err := services.Store().GetWorkflowRun(ctx, receipt.RunID)
	if err != nil || run == nil {
		_ = services.Store().Close()
		t.Fatalf("load authoring recovery fixture Run = %+v, %v", run, err)
	}
	updated, err := services.Store().TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: run.ID, ExpectedVersion: run.Version, Status: store.WorkflowRunRunning, Actor: "worker", Reason: "start authoring recovery fixture",
	})
	if err != nil {
		_ = services.Store().Close()
		t.Fatal(err)
	}
	workflow := commandFrozenWorkflow(t, updated)
	stage, found := workflow.Stage(workflowkit.NodeID(workflowadapter.RepoAnalyze))
	if !found || !stage.Retry.Allows(workflowkit.FailureNetwork) {
		_ = services.Store().Close()
		t.Fatalf("recoverable authoring fixture stage = %+v, found=%t", stage, found)
	}
	emptyInputs, err := workflowkit.FingerprintArtifactBindings(nil)
	if err != nil {
		_ = services.Store().Close()
		t.Fatal(err)
	}
	attempt, err := services.Store().CreateStageAttempt(ctx, store.CreateStageAttemptRequest{
		RunID: updated.ID, StageKey: string(stage.Key), StageGroup: stage.Group, Ordinal: 1, InputFingerprint: string(emptyInputs),
		BudgetSnapshotJSON: `{}`, RetrySnapshotJSON: `{}`, Actor: "worker", Reason: "create recoverable authoring stage",
	})
	if err != nil {
		_ = services.Store().Close()
		t.Fatal(err)
	}
	attempt, err = services.Store().TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: attempt.ID, ExpectedVersion: attempt.Version, ExecutionStatus: store.StageExecutionRunning, Actor: "worker", Reason: "run recoverable authoring stage",
	})
	if err != nil {
		_ = services.Store().Close()
		t.Fatal(err)
	}
	if _, err := services.Store().TransitionStageAttempt(ctx, store.TransitionStageAttemptRequest{
		StageAttemptID: attempt.ID, ExpectedVersion: attempt.Version, ExecutionStatus: store.StageExecutionInfraFailed,
		FailureClass: string(workflowkit.FailureNetwork), ErrorText: "simulated transient authoring failure", Actor: "worker", Reason: "fail recoverable authoring stage",
	}); err != nil {
		_ = services.Store().Close()
		t.Fatal(err)
	}
	updated, err = services.Store().TransitionWorkflowRun(ctx, store.TransitionWorkflowRunRequest{
		RunID: updated.ID, ExpectedVersion: updated.Version, Status: store.WorkflowRunFailedRecoverable, Actor: "worker", Reason: "project recoverable authoring failure",
	})
	if err != nil {
		_ = services.Store().Close()
		t.Fatal(err)
	}
	if err := services.Store().Close(); err != nil {
		t.Fatal(err)
	}
	return root, updated, &lifecycleCLIConfig{root: root, newLifecycleService: newServices}
}

func commandAuthoringRecoverySourceSnapshot(t *testing.T, coordinate app.StandardAuthoringSourceCoordinate) app.StandardAuthoringSourceSnapshot {
	t.Helper()
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": coordinate.CommitSHA}}); err != nil {
		t.Fatal(err)
	}
	for _, file := range []struct {
		name    string
		content string
	}{
		{name: "source/Cargo.toml", content: "[package]\nname = \"fixture\"\n"},
		{name: "source/src/lib.rs", content: "pub fn fixture() {}\n"},
	} {
		if err := writer.WriteHeader(&tar.Header{Name: file.name, Mode: 0o644, Size: int64(len(file.content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(file.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return app.StandardAuthoringSourceSnapshot{
		RepositoryURL: coordinate.RepositoryURL,
		CommitSHA:     coordinate.CommitSHA,
		SchemaVersion: app.StandardAuthoringSourceSnapshotSchemaVersion,
		Content:       archive.Bytes(),
	}
}
