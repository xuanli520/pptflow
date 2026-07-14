package app

import (
	"bytes"
	"context"
	"errors"
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

func TestLifecycleCatalogRequirementIsExplicitAndNonProductionDoesNotInventReceipt(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	if _, err := NewLifecycleServicesWithOptions(root, database, LifecycleServicesOptions{
		OperationResolver:        testsupport.AcceptAllStageOperationResolver(),
		RequireDeploymentCatalog: true,
	}); !errors.Is(err, stageprovider.ErrDeploymentOperationCatalogUnavailable) {
		t.Fatalf("required catalog without catalog-aware resolver = %v, want unavailable catalog", err)
	}
	if _, err := NewLifecycleServicesWithOptions(root, database, LifecycleServicesOptions{
		OperationResolver:      testsupport.AcceptAllStageOperationResolver(),
		RequireDeploymentLock: true,
	}); !errors.Is(err, stageprovider.ErrDeploymentOperationCatalogLockUnavailable) {
		t.Fatalf("required deployment lock without lock-aware resolver = %v, want unavailable lock", err)
	}

	services, err := NewLifecycleServicesWithOptions(root, database, LifecycleServicesOptions{
		OperationResolver: testsupport.AcceptAllStageOperationResolver(),
	})
	if err != nil {
		t.Fatal(err)
	}
	task, revision := importCatalogReceiptFixture(t, ctx, services, "nonproduction-no-receipt")
	profile := catalogReceiptProfile(t)
	specification := catalogReceiptExecutionSpec(task.ID, revision.ID, revision.TaskDigest)
	run, err := services.Runs.StartRun(ctx, StartRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, Profile: profile, ExecutionSpec: specification,
		Trigger: "nonproduction", Actor: "tester", Reason: "prove no catalog receipt is invented",
	})
	if err != nil {
		t.Fatal(err)
	}
	var manifest runManifest
	if err := decodeStrictJSON(run.RunManifestJSON, &manifest); err != nil {
		t.Fatal(err)
	}
	if receipt, err := canonicalManifestDeploymentCatalogReceipt(manifest); err != nil || len(receipt) != 0 {
		t.Fatalf("non-production run receipt = %q, %v; want absent", receipt, err)
	}
	if identity, err := canonicalManifestDeploymentCatalogLockIdentity(manifest); err != nil || identity != nil {
		t.Fatalf("non-production run lock identity = %+v, %v; want absent", identity, err)
	}
	if _, err := os.Stat(filepath.Join(root, managedRunsDirectory, run.ID, deploymentCatalogReceiptFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("non-production run unexpectedly wrote deployment catalog receipt: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, managedRunsDirectory, run.ID, deploymentCatalogLockIdentityFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("non-production run unexpectedly wrote deployment catalog lock identity: %v", err)
	}
}

func TestCatalogEnabledStartRunFreezesCanonicalReceiptInInputBundleAndRunManifest(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	// The task/revision identity belongs in the explicit execution spec, so
	// create the catalog after importing the fixture subject.
	bootstrap, err := NewLifecycleServicesWithOptions(root, database, LifecycleServicesOptions{
		OperationResolver: testsupport.AcceptAllStageOperationResolver(),
	})
	if err != nil {
		t.Fatal(err)
	}
	task, revision := importCatalogReceiptFixture(t, ctx, bootstrap, "catalog-freeze")
	specification := catalogReceiptExecutionSpec(task.ID, revision.ID, revision.TaskDigest)
	profile := catalogReceiptProfile(t)
	resolver := catalogReceiptResolverForSpec(t, specification, "catalog-freeze", "v1")
	services, err := NewLifecycleServicesWithOptions(root, database, LifecycleServicesOptions{
		// The resolver itself exposes the receipt/verifier contract, so the
		// lifecycle constructor must derive the catalog binding rather than
		// requiring a duplicate option value.
		OperationResolver: resolver, RequireDeploymentCatalog: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	expectedReceipt, err := resolver.CanonicalReceiptJSON()
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := services.Mutations.CaptureCheckpoint(ctx, task.ID, revision.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	command := StartRunLifecycleCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{
			IdempotencyKey: mustLifecycleMutationUUID(t), Actor: "tester", Reason: "freeze catalog receipt", Expected: checkpoint,
		},
		ProfilePath:       writeCatalogReceiptProfile(t, root, profile),
		ExecutionSpecPath: writeCatalogReceiptExecutionSpec(t, root, specification),
		Trigger:           "catalog-freeze",
	}
	prepared, err := services.Mutations.PrepareStartRun(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	bundleDirectory := filepath.Join(root, managedRunInputsDirectory, prepared.InputBundleID)
	bundleReceipt, err := os.ReadFile(filepath.Join(bundleDirectory, deploymentCatalogReceiptFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bundleReceipt, expectedReceipt) {
		t.Fatalf("input bundle receipt = %s, want %s", bundleReceipt, expectedReceipt)
	}
	var bundle runStartInputBundle
	bundleManifest, err := os.ReadFile(filepath.Join(bundleDirectory, runStartInputManifestFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := decodeStrictJSON(string(bundleManifest), &bundle); err != nil {
		t.Fatal(err)
	}
	canonicalBundleReceipt, err := canonicalManifestDeploymentCatalogReceipt(runManifest{DeploymentCatalogReceipt: bundle.DeploymentCatalogReceipt})
	if err != nil || !bytes.Equal(canonicalBundleReceipt, expectedReceipt) {
		t.Fatalf("bundle manifest deployment catalog receipt = %s, %v; want %s", canonicalBundleReceipt, err, expectedReceipt)
	}

	started, err := services.Mutations.StartRun(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	run, err := services.Runs.Get(ctx, started.RunID)
	if err != nil {
		t.Fatal(err)
	}
	var manifest runManifest
	if err := decodeStrictJSON(run.RunManifestJSON, &manifest); err != nil {
		t.Fatal(err)
	}
	canonicalRunReceipt, err := canonicalManifestDeploymentCatalogReceipt(manifest)
	if err != nil || !bytes.Equal(canonicalRunReceipt, expectedReceipt) {
		t.Fatalf("run manifest deployment catalog receipt = %s, %v; want %s", canonicalRunReceipt, err, expectedReceipt)
	}
	runReceipt, err := os.ReadFile(filepath.Join(root, managedRunsDirectory, run.ID, deploymentCatalogReceiptFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(runReceipt, expectedReceipt) {
		t.Fatalf("run deployment catalog receipt = %s, want %s", runReceipt, expectedReceipt)
	}
	if err := services.core.verifyRunDeploymentCatalogReceipt(run); err != nil {
		t.Fatalf("verify frozen catalog receipt: %v", err)
	}
	if replayed, err := services.Mutations.StartRun(ctx, command); err != nil || replayed != started {
		t.Fatalf("catalog-bound StartRun replay = %+v, %v; want %+v", replayed, err, started)
	}
	// A Run replay must compare the independently managed receipt file rather
	// than trusting only the JSON copied into the database manifest.
	tamperedResolver := catalogReceiptResolverForSpec(t, specification, "catalog-freeze", "v2")
	tamperedReceipt, err := tamperedResolver.CanonicalReceiptJSON()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, managedRunsDirectory, run.ID, deploymentCatalogReceiptFileName), tamperedReceipt, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := services.Runs.StartRun(ctx, StartRunRequest{
		ID: run.ID, TaskID: task.ID, RevisionID: revision.ID, Profile: profile, ExecutionSpec: specification,
		InputBundleID: command.IdempotencyKey, Trigger: command.Trigger, Actor: "tester", Reason: "freeze catalog receipt",
	}); !errors.Is(err, store.ErrIdempotencyConflict) || !errors.Is(err, stageprovider.ErrDeploymentOperationCatalogDrift) {
		t.Fatalf("replay with changed managed catalog receipt = %v, want idempotency conflict + catalog drift", err)
	}
}

func TestCatalogReceiptDriftRejectsPreparedReplayRuntimeLoadAndEngineBridge(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	bootstrap, err := NewLifecycleServicesWithOptions(root, database, LifecycleServicesOptions{
		OperationResolver: testsupport.AcceptAllStageOperationResolver(),
	})
	if err != nil {
		t.Fatal(err)
	}
	task, revision := importCatalogReceiptFixture(t, ctx, bootstrap, "catalog-drift")
	specification := catalogReceiptExecutionSpec(task.ID, revision.ID, revision.TaskDigest)
	profile := catalogReceiptProfile(t)
	resolverA := catalogReceiptResolverForSpec(t, specification, "catalog-drift", "v1")
	resolverB := catalogReceiptResolverForSpec(t, specification, "catalog-drift", "v2")
	servicesA := catalogReceiptLifecycleServices(t, root, database, resolverA)
	servicesB := catalogReceiptLifecycleServices(t, root, database, resolverB)

	checkpoint, err := servicesA.Mutations.CaptureCheckpoint(ctx, task.ID, revision.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	command := StartRunLifecycleCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{
			IdempotencyKey: mustLifecycleMutationUUID(t), Actor: "tester", Reason: "catalog drift fixture", Expected: checkpoint,
		},
		ProfilePath:       writeCatalogReceiptProfile(t, root, profile),
		ExecutionSpecPath: writeCatalogReceiptExecutionSpec(t, root, specification),
		Trigger:           "catalog-drift",
	}
	if _, err := servicesA.Mutations.PrepareStartRun(ctx, command); err != nil {
		t.Fatal(err)
	}
	if _, err := servicesB.Mutations.StartRun(ctx, command); !errors.Is(err, stageprovider.ErrDeploymentOperationCatalogDrift) {
		t.Fatalf("prepared StartRun under changed catalog = %v, want catalog drift", err)
	}
	runs, err := servicesA.Runs.ListForTask(ctx, task.ID)
	if err != nil || len(runs) != 0 {
		t.Fatalf("catalog-drifted prepared StartRun created Runs = %+v, %v", runs, err)
	}

	started, err := servicesA.Mutations.StartRun(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	run, err := servicesA.Runs.Get(ctx, started.RunID)
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := decodeFrozenRunDefinition(run)
	if err != nil {
		t.Fatal(err)
	}
	runtime := newFrozenRuntime(t, servicesB, catalogReceiptRuntimeRegistry(t, frozen.Workflow, func(ctx context.Context, request workflowkit.StageExecutionRequest) (workflowkit.StageExecutionResult, error) {
		return completedFixtureStage(ctx, request)
	}))
	if _, _, err := runtime.loadFrozenRun(ctx, run.ID, run.DefinitionHash, frozen.QuotaPolicy); !errors.Is(err, stageprovider.ErrDeploymentOperationCatalogDrift) || !errors.Is(err, ErrFrozenExecutionPayload) {
		t.Fatalf("load catalog-drifted frozen run = %v, want frozen payload + catalog drift", err)
	}
	backend := &workflowkitStageBackend{runtime: runtime, run: run}
	if _, err := backend.frozenExecution(); !errors.Is(err, stageprovider.ErrDeploymentOperationCatalogDrift) || !errors.Is(err, ErrFrozenExecutionPayload) {
		t.Fatalf("public Engine bridge accepted catalog-drifted run = %v", err)
	}

	worker := newFrozenRuntimeWorker(t, database, runtime, "catalog-drift-worker")
	result, workerErr := worker.RunOnce(ctx)
	if !errors.Is(workerErr, stageprovider.ErrDeploymentOperationCatalogDrift) || result.FinalState != store.JobFailed {
		t.Fatalf("catalog-drifted worker result = %+v, %v", result, workerErr)
	}
	updated, err := database.GetWorkflowRun(ctx, run.ID)
	if err != nil || updated == nil || updated.Status != store.WorkflowRunInDoubt {
		t.Fatalf("catalog-drifted worker did not project in_doubt = %+v, %v", updated, err)
	}
}

func importCatalogReceiptFixture(t *testing.T, ctx context.Context, services *LifecycleServices, slug string) (store.TaskV2, store.TaskRevision) {
	t.Helper()
	task, revision, err := services.Tasks.ImportTask(ctx, ImportTaskRequest{
		CreateDraftTaskRequest: CreateDraftTaskRequest{Slug: slug, Title: slug, Actor: "tester", Reason: "catalog receipt fixture"},
		SourceDirectory:        writeLifecycleSnapshot(t, "catalog receipt fixture\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return task, revision
}

// These focused receipt tests freeze their own Standard-template fixtures so
// they remain independent from the broader template-registry migration's
// shared test helpers. They intentionally exercise the same explicit-file
// boundary as TUI/CLI StartRun rather than constructing an in-memory shortcut.
func catalogReceiptProfile(t *testing.T) workflowadapter.ExecutionProfile {
	t.Helper()
	profile := lifecycleCompleteProfile(t)
	profile.Template = workflowadapter.StandardTemplateReference()
	return profile
}

func catalogReceiptExecutionSpec(taskID, revisionID, revisionDigest string) workflowadapter.RunExecutionSpec {
	specification := lifecycleExecutionSpec(taskID, revisionID, revisionDigest)
	specification.Template = workflowadapter.StandardTemplateReference()
	return specification
}

func writeCatalogReceiptProfile(t *testing.T, root string, profile workflowadapter.ExecutionProfile) string {
	t.Helper()
	raw, err := profile.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "catalog-receipt-profile.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeCatalogReceiptExecutionSpec(t *testing.T, root string, specification workflowadapter.RunExecutionSpec) string {
	t.Helper()
	raw, err := specification.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "catalog-receipt-execution-spec.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func catalogReceiptRuntimeRegistry(t *testing.T, workflow workflowkit.WorkflowDescriptor, executor workflowkit.StageExecutorFunc) *workflowkit.ControlledPluginRegistry[workflowkit.StageExecutor] {
	t.Helper()
	if len(workflow.Stages) == 0 {
		t.Fatal("catalog receipt fixture workflow has no stages")
	}
	registry, err := workflowkit.NewControlledPluginRegistry([]workflowkit.PluginRegistration[workflowkit.StageExecutor]{{
		Binding: workflow.Stages[0].Plugin, Implementation: executor,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func catalogReceiptLifecycleServices(t *testing.T, root string, database *store.Store, resolver *stageprovider.DeploymentOperationCatalogResolver) *LifecycleServices {
	t.Helper()
	services, err := NewLifecycleServicesWithOptions(root, database, LifecycleServicesOptions{
		OperationResolver: resolver, DeploymentCatalogResolver: resolver, RequireDeploymentCatalog: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return services
}

func catalogReceiptResolverForSpec(t *testing.T, specification workflowadapter.RunExecutionSpec, catalogID, catalogVersion string) *stageprovider.DeploymentOperationCatalogResolver {
	t.Helper()
	template, err := workflowadapter.ResolveWorkflowTemplate(specification.Template)
	if err != nil {
		t.Fatalf("resolve fixture execution template: %v", err)
	}
	registrations := make([]stageprovider.DeploymentOperationRegistration, 0, len(specification.Stages))
	for _, stage := range template.Catalog.Stages {
		resolution, err := specification.ResolveStageOperation(stage.Key)
		if err != nil {
			t.Fatalf("resolve fixture catalog stage %q: %v", stage.Key, err)
		}
		secrets := make([]workflowadapter.SecretReference, len(resolution.Secrets))
		copy(secrets, resolution.Secrets)
		registrations = append(registrations, stageprovider.DeploymentOperationRegistration{
			Stage: stageprovider.DeploymentStageContract{
				Key: resolution.StageKey, Type: resolution.StageType, Group: stage.Group, Plugin: resolution.Plugin,
			},
			Provider:  resolution.Provider,
			Operation: resolution.Operation.Clone(),
			Runtime:   resolution.Runtime,
			Checkout:  stageprovider.DeploymentCheckoutContract{ID: resolution.Checkout.ID, Purpose: "app-catalog-receipt-test"},
			Secrets:   secrets,
		})
	}
	resolver, err := stageprovider.NewDeploymentOperationCatalogResolver(stageprovider.DeploymentOperationCatalog{
		Format: stageprovider.DeploymentOperationCatalogFormat, Version: stageprovider.DeploymentOperationCatalogVersion,
		CatalogID: catalogID, CatalogVersion: catalogVersion, Template: specification.Template, Operations: registrations,
	})
	if err != nil {
		t.Fatal(err)
	}
	return resolver
}
