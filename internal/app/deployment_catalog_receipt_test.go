package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/codeedge"
	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/testsupport"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestLifecycleCatalogRequirementIsExplicitAndNonProductionDoesNotInventReceipt(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.OpenForTest(root)
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
		OperationResolver:     testsupport.AcceptAllStageOperationResolver(),
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
	database, err := store.OpenForTest(root)
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
	if _, err := NewLifecycleServicesWithOptions(root, database, LifecycleServicesOptions{
		OperationResolver: resolver, RequireDeploymentCatalog: true, RequireDeploymentLock: true,
	}); !errors.Is(err, stageprovider.ErrDeploymentOperationCatalogLockUnavailable) {
		t.Fatalf("catalog-only resolver satisfied required deployment lock: %v", err)
	}
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
	if identity, err := canonicalManifestDeploymentCatalogLockIdentity(runManifest{DeploymentCatalogLockIdentity: bundle.DeploymentCatalogLockIdentity}); err != nil || identity != nil {
		t.Fatalf("catalog-only bundle lock identity = %+v, %v; want absent", identity, err)
	}
	if _, err := os.Stat(filepath.Join(bundleDirectory, deploymentCatalogLockIdentityFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("catalog-only bundle unexpectedly wrote deployment lock identity: %v", err)
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
	if identity, err := canonicalManifestDeploymentCatalogLockIdentity(manifest); err != nil || identity != nil {
		t.Fatalf("catalog-only run lock identity = %+v, %v; want absent", identity, err)
	}
	runReceipt, err := os.ReadFile(filepath.Join(root, managedRunsDirectory, run.ID, deploymentCatalogReceiptFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(runReceipt, expectedReceipt) {
		t.Fatalf("run deployment catalog receipt = %s, want %s", runReceipt, expectedReceipt)
	}
	if _, err := os.Stat(filepath.Join(root, managedRunsDirectory, run.ID, deploymentCatalogLockIdentityFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("catalog-only run unexpectedly wrote deployment lock identity: %v", err)
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
	database, err := store.OpenForTest(root)
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
	if _, _, err := runtime.loadFrozenRun(ctx, run.ID, run.DefinitionHash, frozen.ExecutionSpecFingerprint, frozen.QuotaPolicy); !errors.Is(err, stageprovider.ErrDeploymentOperationCatalogDrift) || !errors.Is(err, ErrFrozenExecutionPayload) {
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

func TestCatalogLockEnabledStartRunFreezesIdentityInBundleManifestAndManagedFiles(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.OpenForTest(root)
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
	task, revision := importCatalogReceiptFixture(t, ctx, bootstrap, "catalog-lock-freeze")
	profile := catalogReceiptProfile(t)
	specification := catalogReceiptExecutionSpec(task.ID, revision.ID, revision.TaskDigest)
	resolver := catalogLockAttestedResolverForSpec(t, specification, "catalog-lock-freeze", "v1", "lock-v1")
	services, err := NewLifecycleServicesWithOptions(root, database, LifecycleServicesOptions{
		OperationResolver: resolver, RequireDeploymentCatalog: true, RequireDeploymentLock: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	expectedIdentity := resolver.LockIdentity()
	if err := resolver.VerifyLockIdentity(expectedIdentity); err != nil {
		t.Fatalf("verify fixture lock identity: %v", err)
	}
	checkpoint, err := services.Mutations.CaptureCheckpoint(ctx, task.ID, revision.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	command := StartRunLifecycleCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{
			IdempotencyKey: mustLifecycleMutationUUID(t), Actor: "tester", Reason: "freeze catalog lock identity", Expected: checkpoint,
		},
		ProfilePath:       writeCatalogReceiptProfile(t, root, profile),
		ExecutionSpecPath: writeCatalogReceiptExecutionSpec(t, root, specification),
		Trigger:           "catalog-lock-freeze",
	}
	prepared, err := services.Mutations.PrepareStartRun(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	bundleDirectory := filepath.Join(root, managedRunInputsDirectory, prepared.InputBundleID)
	bundleIdentityRaw, err := os.ReadFile(filepath.Join(bundleDirectory, deploymentCatalogLockIdentityFileName))
	if err != nil {
		t.Fatal(err)
	}
	bundleIdentity, canonicalBundleIdentity, err := parseDeploymentCatalogLockIdentityJSON(bundleIdentityRaw)
	if err != nil || !bytes.Equal(bundleIdentityRaw, canonicalBundleIdentity) || bundleIdentity != expectedIdentity {
		t.Fatalf("input bundle lock identity = %+v, %v; want %+v", bundleIdentity, err, expectedIdentity)
	}
	bundleManifestRaw, err := os.ReadFile(filepath.Join(bundleDirectory, runStartInputManifestFileName))
	if err != nil {
		t.Fatal(err)
	}
	var bundle runStartInputBundle
	if err := decodeStrictJSON(string(bundleManifestRaw), &bundle); err != nil {
		t.Fatal(err)
	}
	bundleManifestIdentity, err := canonicalManifestDeploymentCatalogLockIdentity(runManifest{DeploymentCatalogLockIdentity: bundle.DeploymentCatalogLockIdentity})
	if err != nil || bundleManifestIdentity == nil || *bundleManifestIdentity != expectedIdentity {
		t.Fatalf("input bundle manifest lock identity = %+v, %v; want %+v", bundleManifestIdentity, err, expectedIdentity)
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
	runManifestIdentity, err := canonicalManifestDeploymentCatalogLockIdentity(manifest)
	if err != nil || runManifestIdentity == nil || *runManifestIdentity != expectedIdentity {
		t.Fatalf("run manifest lock identity = %+v, %v; want %+v", runManifestIdentity, err, expectedIdentity)
	}
	runIdentityRaw, err := os.ReadFile(filepath.Join(root, managedRunsDirectory, run.ID, deploymentCatalogLockIdentityFileName))
	if err != nil {
		t.Fatal(err)
	}
	runIdentity, canonicalRunIdentity, err := parseDeploymentCatalogLockIdentityJSON(runIdentityRaw)
	if err != nil || !bytes.Equal(runIdentityRaw, canonicalRunIdentity) || runIdentity != expectedIdentity {
		t.Fatalf("run managed lock identity = %+v, %v; want %+v", runIdentity, err, expectedIdentity)
	}
	if err := services.core.verifyRunDeploymentCatalogReceipt(run); err != nil {
		t.Fatalf("verify frozen catalog/lock binding: %v", err)
	}

	// A run replay must compare the separately managed identity file instead
	// of trusting only the value embedded in the database manifest.
	tamperedIdentity := expectedIdentity
	tamperedIdentity.LockVersion = "lock-v2"
	tamperedIdentityRaw, err := canonicalDeploymentCatalogLockIdentity(tamperedIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, managedRunsDirectory, run.ID, deploymentCatalogLockIdentityFileName), tamperedIdentityRaw, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := services.Runs.StartRun(ctx, StartRunRequest{
		ID: run.ID, TaskID: task.ID, RevisionID: revision.ID, Profile: profile, ExecutionSpec: specification,
		InputBundleID: command.IdempotencyKey, Trigger: command.Trigger, Actor: "tester", Reason: "freeze catalog lock identity",
	}); !errors.Is(err, store.ErrIdempotencyConflict) || !errors.Is(err, stageprovider.ErrDeploymentOperationCatalogLockDrift) {
		t.Fatalf("replay with changed managed lock identity = %v, want idempotency conflict + lock drift", err)
	}
}

func TestCatalogLockDriftRejectsPreparedReplayRuntimeLoadAndEngineBridge(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.OpenForTest(root)
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
	task, revision := importCatalogReceiptFixture(t, ctx, bootstrap, "catalog-lock-drift")
	profile := catalogReceiptProfile(t)
	specification := catalogReceiptExecutionSpec(task.ID, revision.ID, revision.TaskDigest)
	resolverA := catalogLockAttestedResolverForSpec(t, specification, "catalog-lock-drift", "v1", "lock-v1")
	resolverB := catalogLockAttestedResolverForSpec(t, specification, "catalog-lock-drift", "v1", "lock-v2")
	servicesA := catalogLockLifecycleServices(t, root, database, resolverA)
	servicesB := catalogLockLifecycleServices(t, root, database, resolverB)

	checkpoint, err := servicesA.Mutations.CaptureCheckpoint(ctx, task.ID, revision.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	command := StartRunLifecycleCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{
			IdempotencyKey: mustLifecycleMutationUUID(t), Actor: "tester", Reason: "catalog lock drift fixture", Expected: checkpoint,
		},
		ProfilePath:       writeCatalogReceiptProfile(t, root, profile),
		ExecutionSpecPath: writeCatalogReceiptExecutionSpec(t, root, specification),
		Trigger:           "catalog-lock-drift",
	}
	if _, err := servicesA.Mutations.PrepareStartRun(ctx, command); err != nil {
		t.Fatal(err)
	}
	if _, err := servicesB.Mutations.StartRun(ctx, command); !errors.Is(err, stageprovider.ErrDeploymentOperationCatalogLockDrift) {
		t.Fatalf("prepared StartRun under changed lock = %v, want lock drift", err)
	}
	if runs, err := servicesA.Runs.ListForTask(ctx, task.ID); err != nil || len(runs) != 0 {
		t.Fatalf("lock-drifted prepared StartRun created Runs = %+v, %v", runs, err)
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
	runtime := newFrozenRuntime(t, servicesB, catalogReceiptRuntimeRegistry(t, frozen.Workflow, completedFixtureStage))
	if _, _, err := runtime.loadFrozenRun(ctx, run.ID, run.DefinitionHash, frozen.ExecutionSpecFingerprint, frozen.QuotaPolicy); !errors.Is(err, stageprovider.ErrDeploymentOperationCatalogLockDrift) || !errors.Is(err, ErrFrozenExecutionPayload) {
		t.Fatalf("load lock-drifted frozen run = %v, want frozen payload + lock drift", err)
	}
	backend := &workflowkitStageBackend{runtime: runtime, run: run}
	if _, err := backend.frozenExecution(); !errors.Is(err, stageprovider.ErrDeploymentOperationCatalogLockDrift) || !errors.Is(err, ErrFrozenExecutionPayload) {
		t.Fatalf("public Engine bridge accepted lock-drifted run = %v", err)
	}

	worker := newFrozenRuntimeWorker(t, database, runtime, "catalog-lock-drift-worker")
	result, workerErr := worker.RunOnce(ctx)
	if !errors.Is(workerErr, stageprovider.ErrDeploymentOperationCatalogLockDrift) || result.FinalState != store.JobFailed {
		t.Fatalf("lock-drifted worker result = %+v, %v", result, workerErr)
	}
	updated, err := database.GetWorkflowRun(ctx, run.ID)
	if err != nil || updated == nil || updated.Status != store.WorkflowRunInDoubt {
		t.Fatalf("lock-drifted worker did not project in_doubt = %+v, %v", updated, err)
	}
}

func TestCatalogLockBuildOnlyCompatibilityAcceptsOnlyReviewedPredecessor(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.OpenForTest(root)
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
	task, revision := importCatalogReceiptFixture(t, ctx, bootstrap, "catalog-lock-build-compatibility")
	profile := catalogReceiptProfile(t)
	specification := catalogReceiptExecutionSpec(task.ID, revision.ID, revision.TaskDigest)
	oldResolver := catalogLockAttestedResolverForSpecWithBuild(t, specification, "catalog-lock-build-compatibility", "v1", "lock-v1", stageprovider.HarborFlowBuildIdentity{
		Module: "github.com/purplevoid/harbor-factory", Version: "v2.0.0", Commit: strings.Repeat("a", 40),
		ContentSHA256: workflowkit.SHA256Fingerprint([]byte("catalog-lock-old-build")),
	})
	currentResolver := catalogLockAttestedResolverForSpecWithBuild(t, specification, "catalog-lock-build-compatibility", "v1", "lock-v2", stageprovider.HarborFlowBuildIdentity{
		Module: "github.com/purplevoid/harbor-factory", Version: "v2.1.0", Commit: strings.Repeat("b", 40),
		ContentSHA256: workflowkit.SHA256Fingerprint([]byte("catalog-lock-current-build")),
	})
	contract, err := currentResolver.ExecutionContractFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	proof := stageprovider.DeploymentOperationCatalogLockCompatibilityProof{
		Predecessor: oldResolver.LockIdentity(), ExecutionContractFingerprint: contract,
	}
	servicesOld := catalogLockLifecycleServices(t, root, database, oldResolver)
	run, err := servicesOld.Runs.StartRun(ctx, StartRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, Profile: profile, ExecutionSpec: specification,
		Trigger: "catalog lock build compatibility", Actor: "tester", Reason: "freeze predecessor lock",
	})
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := decodeFrozenRunDefinition(run)
	if err != nil {
		t.Fatal(err)
	}

	withoutProof := catalogLockLifecycleServices(t, root, database, currentResolver)
	rejectedRuntime := newFrozenRuntime(t, withoutProof, catalogReceiptRuntimeRegistry(t, frozen.Workflow, completedFixtureStage))
	if _, _, err := rejectedRuntime.loadFrozenRun(ctx, run.ID, run.DefinitionHash, frozen.ExecutionSpecFingerprint, frozen.QuotaPolicy); !errors.Is(err, stageprovider.ErrDeploymentOperationCatalogLockDrift) || !errors.Is(err, ErrFrozenExecutionPayload) {
		t.Fatalf("unapproved predecessor lock load = %v, want frozen payload + lock drift", err)
	}

	withProof := catalogLockLifecycleServicesWithProofs(t, root, database, currentResolver, []stageprovider.DeploymentOperationCatalogLockCompatibilityProof{proof})
	compatibleRuntime := newFrozenRuntime(t, withProof, catalogReceiptRuntimeRegistry(t, frozen.Workflow, completedFixtureStage))
	if _, _, err := compatibleRuntime.loadFrozenRun(ctx, run.ID, run.DefinitionHash, frozen.ExecutionSpecFingerprint, frozen.QuotaPolicy); err != nil {
		t.Fatalf("reviewed predecessor lock load = %v", err)
	}

	var manifest runManifest
	if err := decodeStrictJSON(run.RunManifestJSON, &manifest); err != nil {
		t.Fatal(err)
	}
	tampered := oldResolver.LockIdentity()
	tampered.Fingerprint = workflowkit.SHA256Fingerprint([]byte("unreviewed-predecessor-lock"))
	manifest.DeploymentCatalogLockIdentity = &tampered
	manifestRaw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	tamperedRun := run
	tamperedRun.RunManifestJSON = string(manifestRaw)
	if err := withProof.core.verifyRunDeploymentCatalogReceipt(tamperedRun); !errors.Is(err, stageprovider.ErrDeploymentOperationCatalogLockDrift) {
		t.Fatalf("unreviewed predecessor identity verification = %v, want lock drift", err)
	}
}

func TestTemplateDeploymentCatalogRegistryFreezesAndVerifiesEachTemplateBinding(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.OpenForTest(root)
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
	task, revision := importCatalogReceiptFixture(t, ctx, bootstrap, "template-keyed-catalog-registry")
	standardSpecification := catalogReceiptExecutionSpec(task.ID, revision.ID, revision.TaskDigest)
	parentSpecification := testsupport.CompleteCodeEdgePhase1RunExecutionSpec(task.ID, revision.ID, revision.TaskDigest)
	childSpecification := testsupport.CompleteCodeEdgeEvaluatorChildRunExecutionSpec(task.ID, revision.ID, revision.TaskDigest)
	standardResolver := catalogLockAttestedResolverForSpec(t, standardSpecification, "template-keyed-standard", "v1", "lock-standard")
	parentResolver := catalogLockAttestedResolverForSpec(t, parentSpecification, "template-keyed-parent", "v1", "lock-parent")
	childResolver := catalogLockAttestedResolverForSpec(t, childSpecification, "template-keyed-child", "v1", "lock-child")
	router, err := stageprovider.NewTemplateWorkflowkitProviderOperationResolver([]stageprovider.TemplateWorkflowkitProviderRegistration{
		{Template: standardSpecification.Template, Resolver: standardResolver},
		{Template: parentSpecification.Template, Resolver: parentResolver},
		{Template: childSpecification.Template, Resolver: childResolver},
	})
	if err != nil {
		t.Fatalf("construct exact multi-template provider router: %v", err)
	}
	services, err := NewLifecycleServicesWithOptions(root, database, LifecycleServicesOptions{
		// Admission and worker execution both use the same exact template
		// router. It never selects a catalog based on shared stage keys or on
		// whichever bundle was registered first.
		OperationResolver: router,
		DeploymentCatalogResolvers: []TemplateDeploymentCatalogResolver{
			{Template: standardSpecification.Template, Resolver: standardResolver},
			{Template: parentSpecification.Template, Resolver: parentResolver},
			{Template: childSpecification.Template, Resolver: childResolver},
		},
		RequireDeploymentCatalog: true,
		RequireDeploymentLock:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := services.WorkflowkitProviderOperationResolver(); got != router {
		t.Fatal("multi-template production router was not preserved for worker composition")
	}
	standardOperation, err := standardSpecification.ResolveStageOperation(workflowkit.StageKey(workflowadapter.RepoPrepare))
	if err != nil {
		t.Fatal(err)
	}
	// A router has no implicit Standard/current/default bundle. Even a valid
	// stage operation is rejected if its enclosing frozen template is not one
	// of the explicitly installed production bundles.
	standardOperation.Template = workflowadapter.StandardAuthoringCurrentTemplateReference()
	if err := router.ValidateStageOperation(standardOperation); !errors.Is(err, stageprovider.ErrProviderUnavailable) {
		t.Fatalf("multi-template router fell back for an uninstalled template: %v", err)
	}

	standard, err := services.Runs.StartRun(ctx, StartRunRequest{
		TaskID: task.ID, RevisionID: revision.ID,
		Profile: catalogReceiptProfile(t), ExecutionSpec: standardSpecification,
		Trigger: "template-keyed-standard", Actor: "tester", Reason: "freeze Standard template binding",
	})
	if err != nil {
		t.Fatalf("start Standard Run: %v", err)
	}
	parent, err := services.Runs.StartRun(ctx, StartRunRequest{
		TaskID: task.ID, RevisionID: revision.ID,
		Profile: codeEdgePhase1RuntimeProfile(t), ExecutionSpec: parentSpecification,
		Trigger: "template-keyed-parent", Actor: "tester", Reason: "freeze parent template binding",
	})
	if err != nil {
		t.Fatalf("start parent Run: %v", err)
	}
	childProfile := codeEdgeEvaluatorRuntimeProfile(t)
	child, err := services.Runs.StartRun(ctx, StartRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, ParentRunID: parent.ID,
		Profile: childProfile, ExecutionSpec: childSpecification,
		Trigger: "template-keyed-child", Actor: "tester", Reason: "freeze evaluator child template binding",
	})
	if err != nil {
		t.Fatalf("start evaluator child Run: %v", err)
	}

	assertTemplateCatalogBinding := func(run store.WorkflowRun, template workflowadapter.TemplateReference, resolver *stageprovider.CatalogLockAttestedWorkflowkitProviderOperationResolver) {
		t.Helper()
		var manifest runManifest
		if err := decodeStrictJSON(run.RunManifestJSON, &manifest); err != nil {
			t.Fatal(err)
		}
		receipt, err := canonicalManifestDeploymentCatalogReceipt(manifest)
		if err != nil {
			t.Fatal(err)
		}
		expectedReceipt, err := resolver.CanonicalReceiptJSON()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(receipt, expectedReceipt) {
			t.Fatalf("Run %s catalog receipt = %s, want binding for %s@%s", run.ID, receipt, template.ID, template.Version)
		}
		parsedReceipt, err := stageprovider.ParseDeploymentOperationCatalogReceiptJSON(receipt)
		if err != nil || !parsedReceipt.Template.Equal(template) {
			t.Fatalf("Run %s receipt template = %+v, %v; want %s@%s", run.ID, parsedReceipt.Template, err, template.ID, template.Version)
		}
		identity, err := canonicalManifestDeploymentCatalogLockIdentity(manifest)
		if err != nil || identity == nil || *identity != resolver.LockIdentity() {
			t.Fatalf("Run %s catalog lock identity = %+v, %v; want %+v", run.ID, identity, err, resolver.LockIdentity())
		}
		if err := services.core.verifyRunDeploymentCatalogReceipt(run); err != nil {
			t.Fatalf("verify Run %s own template catalog binding: %v", run.ID, err)
		}
	}
	assertTemplateCatalogBinding(standard, standardSpecification.Template, standardResolver)
	assertTemplateCatalogBinding(parent, parentSpecification.Template, parentResolver)
	assertTemplateCatalogBinding(child, childSpecification.Template, childResolver)

	// The same durable worker entrypoint used before a stage claim reselects
	// the binding from the child RunExecutionSpec.Template, not from the
	// parent or whichever resolver was registered first.
	childFrozen, err := decodeFrozenRunDefinition(child)
	if err != nil {
		t.Fatal(err)
	}
	runtime := newFrozenRuntime(t, services, frozenRuntimeRegistry(t, childFrozen.Workflow, completedFixtureStage))
	if _, _, err := runtime.loadFrozenRun(ctx, child.ID, child.DefinitionHash, childFrozen.ExecutionSpecFingerprint, childFrozen.QuotaPolicy); err != nil {
		t.Fatalf("worker load with child binding: %v", err)
	}

	// A normal idempotent replay also proves the child binding, rather than
	// replacing it with the parent catalog/lock currently in memory.
	if replayed, err := services.Runs.StartRun(ctx, StartRunRequest{
		ID: child.ID, TaskID: task.ID, RevisionID: revision.ID, ParentRunID: parent.ID,
		Profile: childProfile, ExecutionSpec: childSpecification,
		Trigger: "template-keyed-child", Actor: "tester", Reason: "freeze evaluator child template binding",
	}); err != nil || replayed.ID != child.ID {
		t.Fatalf("replay child Run = %+v, %v; want existing Run", replayed, err)
	}

	parentReceipt, err := parentResolver.CanonicalReceiptJSON()
	if err != nil {
		t.Fatal(err)
	}
	childReceiptPath := filepath.Join(root, managedRunsDirectory, child.ID, deploymentCatalogReceiptFileName)
	childReceipt, err := os.ReadFile(childReceiptPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(childReceiptPath, parentReceipt, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runtime.loadFrozenRun(ctx, child.ID, child.DefinitionHash, childFrozen.ExecutionSpecFingerprint, childFrozen.QuotaPolicy); !errors.Is(err, ErrFrozenExecutionPayload) || !errors.Is(err, stageprovider.ErrDeploymentOperationCatalogDrift) {
		t.Fatalf("worker accepted parent receipt for evaluator child = %v", err)
	}
	if _, err := services.Runs.StartRun(ctx, StartRunRequest{
		ID: child.ID, TaskID: task.ID, RevisionID: revision.ID, ParentRunID: parent.ID,
		Profile: childProfile, ExecutionSpec: childSpecification,
		Trigger: "template-keyed-child", Actor: "tester", Reason: "freeze evaluator child template binding",
	}); !errors.Is(err, store.ErrIdempotencyConflict) || !errors.Is(err, stageprovider.ErrDeploymentOperationCatalogDrift) {
		t.Fatalf("replay accepted parent receipt for evaluator child = %v", err)
	}
	if err := os.WriteFile(childReceiptPath, childReceipt, 0o640); err != nil {
		t.Fatal(err)
	}

	parentLock, err := canonicalDeploymentCatalogLockIdentity(parentResolver.LockIdentity())
	if err != nil {
		t.Fatal(err)
	}
	childLockPath := filepath.Join(root, managedRunsDirectory, child.ID, deploymentCatalogLockIdentityFileName)
	childLock, err := os.ReadFile(childLockPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(childLockPath, parentLock, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runtime.loadFrozenRun(ctx, child.ID, child.DefinitionHash, childFrozen.ExecutionSpecFingerprint, childFrozen.QuotaPolicy); !errors.Is(err, ErrFrozenExecutionPayload) || !errors.Is(err, stageprovider.ErrDeploymentOperationCatalogLockDrift) {
		t.Fatalf("worker accepted parent lock for evaluator child = %v", err)
	}
	if _, err := services.Runs.StartRun(ctx, StartRunRequest{
		ID: child.ID, TaskID: task.ID, RevisionID: revision.ID, ParentRunID: parent.ID,
		Profile: childProfile, ExecutionSpec: childSpecification,
		Trigger: "template-keyed-child", Actor: "tester", Reason: "freeze evaluator child template binding",
	}); !errors.Is(err, store.ErrIdempotencyConflict) || !errors.Is(err, stageprovider.ErrDeploymentOperationCatalogLockDrift) {
		t.Fatalf("replay accepted parent lock for evaluator child = %v", err)
	}
	if err := os.WriteFile(childLockPath, childLock, 0o640); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogLockIdentityPropagatesToCandidateChildManifest(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.OpenForTest(root)
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
	task, revision := importCatalogReceiptFixture(t, ctx, bootstrap, "catalog-lock-child")
	profile := catalogReceiptProfile(t)
	specification := catalogReceiptExecutionSpec(task.ID, revision.ID, revision.TaskDigest)
	resolver := catalogLockAttestedResolverForSpec(t, specification, "catalog-lock-child", "v1", "lock-v1")
	services := catalogLockLifecycleServices(t, root, database, resolver)
	run, err := services.Runs.StartRun(ctx, StartRunRequest{
		TaskID: task.ID, RevisionID: revision.ID, Profile: profile, ExecutionSpec: specification,
		Trigger: "catalog-lock-child", Actor: "tester", Reason: "freeze source run",
	})
	if err != nil {
		t.Fatal(err)
	}
	candidate := store.RevisionCandidate{
		TaskID: task.ID, BaseRevisionID: revision.ID, BaseDigest: revision.TaskDigest, AfterDigest: revision.TaskDigest,
		TargetRevisionID: mustLifecycleMutationUUID(t), TargetRunID: mustLifecycleMutationUUID(t),
	}
	// Candidate child managed inputs are derived from the pre-commit immutable
	// target snapshot, not from the source Run's input identity. This fixture
	// exercises only catalog-lock propagation, so materialize an unchanged
	// target snapshot explicitly instead of bypassing that invariant.
	baseSnapshot, err := services.Revisions.SnapshotDirectory(task.ID, revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	preparedSnapshot, _, err := (&RevisionService{core: services.core}).prepareSnapshot(ctx, task.ID, candidate.TargetRevisionID, baseSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if preparedSnapshot.TaskDigest != candidate.AfterDigest {
		t.Fatalf("prepared candidate snapshot digest = %s, want %s", preparedSnapshot.TaskDigest, candidate.AfterDigest)
	}
	childRaw, err := services.Changes.ensureCandidateChildRunManifest(ctx, candidate, run)
	if err != nil {
		t.Fatal(err)
	}
	var child runManifest
	if err := decodeStrictJSON(childRaw, &child); err != nil {
		t.Fatal(err)
	}
	if child.Inputs == nil || child.Inputs.BundleID != "" || child.Inputs.ProfileFingerprint == "" || len(child.ExecutionSpec) == 0 {
		t.Fatalf("candidate child did not create fresh execution inputs: %+v", child.Inputs)
	}
	childSpecification, err := workflowadapter.ParseRunExecutionSpecJSON(child.ExecutionSpec)
	if err != nil {
		t.Fatal(err)
	}
	if childSpecification.Selection.TaskID != task.ID || childSpecification.Selection.RevisionID != candidate.TargetRevisionID || string(childSpecification.Selection.RevisionDigest) != candidate.AfterDigest {
		t.Fatalf("candidate child execution selection = %+v", childSpecification.Selection)
	}
	for _, checkout := range childSpecification.References.Checkouts {
		if checkout.RevisionID != candidate.TargetRevisionID || string(checkout.RevisionDigest) != candidate.AfterDigest {
			t.Fatalf("candidate child checkout %q was not rebound: %+v", checkout.ID, checkout)
		}
	}
	expectedIdentity := resolver.LockIdentity()
	childIdentity, err := canonicalManifestDeploymentCatalogLockIdentity(child)
	if err != nil || childIdentity == nil || *childIdentity != expectedIdentity {
		t.Fatalf("candidate child manifest lock identity = %+v, %v; want %+v", childIdentity, err, expectedIdentity)
	}
	childIdentityRaw, err := os.ReadFile(filepath.Join(root, managedRunsDirectory, candidate.TargetRunID, deploymentCatalogLockIdentityFileName))
	if err != nil {
		t.Fatal(err)
	}
	storedIdentity, canonicalIdentity, err := parseDeploymentCatalogLockIdentityJSON(childIdentityRaw)
	if err != nil || !bytes.Equal(childIdentityRaw, canonicalIdentity) || storedIdentity != expectedIdentity {
		t.Fatalf("candidate child managed lock identity = %+v, %v; want %+v", storedIdentity, err, expectedIdentity)
	}

	// The idempotent child-manifest path must reject a changed independently
	// managed identity before a candidate can reuse its frozen child Run.
	tamperedIdentity := expectedIdentity
	tamperedIdentity.LockVersion = "lock-v2"
	tamperedIdentityRaw, err := canonicalDeploymentCatalogLockIdentity(tamperedIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, managedRunsDirectory, candidate.TargetRunID, deploymentCatalogLockIdentityFileName), tamperedIdentityRaw, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := services.Changes.ensureCandidateChildRunManifest(ctx, candidate, run); !errors.Is(err, stageprovider.ErrDeploymentOperationCatalogLockDrift) && !strings.Contains(err.Error(), "lock identity") {
		t.Fatalf("reused candidate child manifest accepted changed lock identity: %v", err)
	}
}

func importCatalogReceiptFixture(t *testing.T, ctx context.Context, services *LifecycleServices, slug string) (store.TaskV2, store.TaskRevision) {
	t.Helper()
	return createStandardMaterializedLifecycleTask(t, ctx, services, slug, slug, "catalog receipt fixture\n")
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

func catalogLockLifecycleServices(t *testing.T, root string, database *store.Store, resolver *stageprovider.CatalogLockAttestedWorkflowkitProviderOperationResolver) *LifecycleServices {
	t.Helper()
	return catalogLockLifecycleServicesWithProofs(t, root, database, resolver, nil)
}

func catalogLockLifecycleServicesWithProofs(t *testing.T, root string, database *store.Store, resolver *stageprovider.CatalogLockAttestedWorkflowkitProviderOperationResolver, proofs []stageprovider.DeploymentOperationCatalogLockCompatibilityProof) *LifecycleServices {
	t.Helper()
	services, err := NewLifecycleServicesWithOptions(root, database, LifecycleServicesOptions{
		OperationResolver: resolver,
		DeploymentCatalogResolvers: []TemplateDeploymentCatalogResolver{{
			Template: resolver.Receipt().Template, Resolver: resolver,
			CompatibleLockProofs: append([]stageprovider.DeploymentOperationCatalogLockCompatibilityProof(nil), proofs...),
		}},
		RequireDeploymentCatalog: true, RequireDeploymentLock: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return services
}

func catalogLockAttestedResolverForSpec(t *testing.T, specification workflowadapter.RunExecutionSpec, catalogID, catalogVersion, lockVersion string) *stageprovider.CatalogLockAttestedWorkflowkitProviderOperationResolver {
	return catalogLockAttestedResolverForSpecWithBuild(t, specification, catalogID, catalogVersion, lockVersion, stageprovider.HarborFlowBuildIdentity{
		Module: "github.com/purplevoid/harbor-factory", Version: "v2.0.0", Commit: strings.Repeat("a", 40),
		ContentSHA256: workflowkit.SHA256Fingerprint([]byte("app-catalog-lock-build:" + catalogID)),
	})
}

func catalogLockAttestedResolverForSpecWithBuild(t *testing.T, specification workflowadapter.RunExecutionSpec, catalogID, catalogVersion, lockVersion string, build stageprovider.HarborFlowBuildIdentity) *stageprovider.CatalogLockAttestedWorkflowkitProviderOperationResolver {
	t.Helper()
	template, err := workflowadapter.ResolveWorkflowTemplate(specification.Template)
	if err != nil {
		t.Fatalf("resolve fixture execution template: %v", err)
	}
	registrations := make([]stageprovider.DeploymentOperationRegistration, 0, len(template.Catalog.Stages))
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
			Provider:        resolution.Provider,
			Operation:       resolution.Operation.Clone(),
			Runtime:         resolution.Runtime,
			Checkout:        stageprovider.DeploymentCheckoutContract{ID: resolution.Checkout.ID, Purpose: "app-catalog-lock-test"},
			Secrets:         secrets,
			HarborEvaluator: catalogLockFixtureHarborEvaluatorContract(t, resolution, secrets),
		})
	}
	catalog, err := stageprovider.NewDeploymentOperationCatalogResolver(stageprovider.DeploymentOperationCatalog{
		Format: stageprovider.DeploymentOperationCatalogFormat, Version: stageprovider.DeploymentOperationCatalogVersion,
		CatalogID: catalogID, CatalogVersion: catalogVersion, Template: specification.Template, Operations: registrations,
	})
	if err != nil {
		t.Fatal(err)
	}
	lock := stageprovider.DeploymentOperationCatalogLock{
		Format: stageprovider.DeploymentOperationCatalogLockFormat, Version: stageprovider.DeploymentOperationCatalogLockVersion,
		LockID: "app-catalog-lock-" + catalogID, LockVersion: lockVersion, CatalogReceipt: catalog.Receipt(),
		HarborFlowBuild: build,
		Operations:      make([]stageprovider.DeploymentOperationCatalogLockRecord, 0, len(registrations)),
	}
	if specification.Template.Equal(workflowadapter.CodeEdgePhase1TemplateReference()) {
		profile := lifecycleCompleteProfileForTemplate(t, workflowadapter.CodeEdgePhase1WorkflowTemplate())
		preflight := catalogLockFixtureCodeEdgePhase1PreflightProfile()
		policy := specification.CodeEdgeFinalCompliancePolicy.Clone()
		lock.CodeEdgePhase1ExecutionProfile = &stageprovider.CodeEdgePhase1ExecutionProfileLock{Profile: profile}
		lock.CodeEdgePhase1PreflightProfile = &stageprovider.CodeEdgePhase1PreflightProfileLock{Profile: preflight}
		lock.CodeEdgePhase1FinalCompliancePolicy = &stageprovider.CodeEdgePhase1FinalCompliancePolicyLock{Policy: policy}
	}
	for _, registration := range registrations {
		lock.Operations = append(lock.Operations, catalogLockFixtureRecord(t, registration))
	}
	verifier, err := stageprovider.NewDeploymentOperationCatalogLockResolver(catalog, lock)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := stageprovider.NewCatalogLockAttestedWorkflowkitProviderOperationResolver(verifier, catalogLockFixtureDelegate{}, catalogLockFixtureAttestor{})
	if err != nil {
		t.Fatal(err)
	}
	return resolver
}

func catalogLockFixtureCodeEdgePhase1PreflightProfile() codeedge.Profile {
	return codeedge.Profile{
		Metadata: codeedge.MetadataFieldMapping{
			CodeLang:    codeedge.TOMLPath{"metadata", "code_lang"},
			TaskType:    codeedge.TOMLPath{"metadata", "task_type"},
			Application: codeedge.TOMLPath{"metadata", "application"},
			IsZeroToOne: codeedge.TOMLPath{"metadata", "is_0_to_1"},
			GitHubURL:   codeedge.TOMLPath{"metadata", "github_url"},
			CommitID:    codeedge.TOMLPath{"metadata", "commit_id"},
		},
		ProtectedEnvironmentVariables: []string{
			"ANTHROPIC_AUTH_TOKEN",
			"ANTHROPIC_BASE_URL",
			"QWEN_HARBOR_BASE_URL",
		},
	}
}

func catalogLockFixtureRecord(t *testing.T, registration stageprovider.DeploymentOperationRegistration) stageprovider.DeploymentOperationCatalogLockRecord {
	t.Helper()
	secrets := make([]workflowadapter.SecretReference, len(registration.Secrets))
	copy(secrets, registration.Secrets)
	record := stageprovider.DeploymentOperationCatalogLockRecord{
		Stage: registration.Stage, Provider: registration.Provider, Operation: registration.Operation.Clone(), Runtime: registration.Runtime,
		Checkout: registration.Checkout, Secrets: secrets,
		PromptContentFingerprint: workflowkit.SHA256Fingerprint([]byte("prompt:" + string(registration.Stage.Key))),
		SchemaContentFingerprint: workflowkit.SHA256Fingerprint([]byte("schema:" + string(registration.Stage.Key))),
		ExecutionKind:            registration.Operation.Payload.Kind(),
	}
	switch payload := registration.Operation.Payload.(type) {
	case workflowadapter.LocalCommandOperationPayload:
		record.LocalExecutable = &stageprovider.LocalExecutableLock{
			CommandID: payload.CommandID, AbsolutePath: "/opt/harbor/bin/" + payload.CommandID, Version: "v1.0.0",
			ContentSHA256: workflowkit.SHA256Fingerprint([]byte("local:" + payload.CommandID)),
		}
		if registration.HarborEvaluator != nil {
			contract := registration.HarborEvaluator.Clone()
			record.HarborEvaluator = &stageprovider.HarborEvaluatorOperationLock{
				Contract: contract, Launcher: *record.LocalExecutable,
				ClaudeCodeExecutable: stageprovider.LocalExecutableLock{
					CommandID: stageprovider.HarborEvaluatorClaudeCodeCommandID, AbsolutePath: "/opt/harbor/bin/claude", Version: contract.AgentVersion,
					ContentSHA256: workflowkit.SHA256Fingerprint([]byte("harbor-claude-code-fixture")),
				},
				PythonInterpreter: stageprovider.LocalExecutableLock{
					CommandID: stageprovider.HarborEvaluatorPythonCommandID, AbsolutePath: "/opt/harbor/bin/python3", Version: "3.13.0",
					ContentSHA256: workflowkit.SHA256Fingerprint([]byte("harbor-python-fixture")),
				},
				PythonSourceTree: stageprovider.HarborPythonSourceTreeLock{
					AbsolutePath: "/opt/harbor/site-packages/harbor", PythonFilesSHA256: workflowkit.SHA256Fingerprint([]byte("harbor-python-tree-fixture")),
				},
				DockerCLI: stageprovider.LocalExecutableLock{
					CommandID: stageprovider.HarborEvaluatorDockerCommandID, AbsolutePath: "/opt/harbor/bin/docker", Version: stageprovider.HarborEvaluatorDockerVersion,
					ContentSHA256: workflowkit.SHA256Fingerprint([]byte("harbor-docker-fixture")),
				},
				DockerServerVersion: stageprovider.HarborEvaluatorDockerServerVersion,
				DockerComposePlugin: stageprovider.LocalExecutableLock{
					CommandID: stageprovider.HarborEvaluatorDockerComposeCommandID, AbsolutePath: "/opt/harbor/libexec/docker/cli-plugins/docker-compose", Version: stageprovider.HarborEvaluatorDockerComposeVersion,
					ContentSHA256: workflowkit.SHA256Fingerprint([]byte("harbor-docker-compose-fixture")),
				},
				DockerBuildxPlugin: stageprovider.LocalExecutableLock{
					CommandID: stageprovider.HarborEvaluatorDockerBuildxCommandID, AbsolutePath: "/opt/harbor/libexec/docker/cli-plugins/docker-buildx", Version: stageprovider.HarborEvaluatorDockerBuildxVersion,
					ContentSHA256: workflowkit.SHA256Fingerprint([]byte("harbor-docker-buildx-fixture")),
				},
				HarborVersionOutput: stageprovider.HarborEvaluatorHarborVersion, DockerComposeVersionOutput: stageprovider.HarborEvaluatorDockerComposeVersionOutput,
				DockerBuildxVersionOutput: stageprovider.HarborEvaluatorDockerBuildxVersionOutput,
			}
		}
	case workflowadapter.ContainerCommandOperationPayload:
		record.ContainerRuntime = &stageprovider.PinnedContainerRuntimeLock{ImageDigest: payload.ImageDigest, Runtime: registration.Runtime}
	case workflowadapter.AgentTurnOperationPayload:
		record.AgentModel = &stageprovider.AgentModelLock{
			AgentID: payload.AgentID, AgentVersion: "v1.0.0", ModelID: payload.ModelID, ModelVersion: "v1.0.0",
		}
	case workflowadapter.DurableReviewOperationPayload:
		record.DurableReviewPolicy = &stageprovider.DurableReviewPolicyLock{PolicyID: payload.PolicyID, Version: "v1.0.0"}
	default:
		t.Fatalf("unsupported catalog-lock fixture payload %T", registration.Operation.Payload)
	}
	return record
}

func catalogLockFixtureHarborEvaluatorContract(t *testing.T, resolution workflowadapter.StageOperationResolution, secrets []workflowadapter.SecretReference) *stageprovider.HarborEvaluatorOperationContract {
	t.Helper()
	payload, ok := resolution.Operation.Payload.(workflowadapter.LocalCommandOperationPayload)
	if !ok || (payload.CommandID != stageprovider.HarborEvaluatorQwenCommandID && payload.CommandID != stageprovider.HarborEvaluatorOpusCommandID) {
		return nil
	}
	if len(secrets) != 1 {
		t.Fatalf("CodeEdge evaluator fixture secrets = %+v, want one controlled reference", secrets)
	}
	model := "fixture-qwen"
	endpoint := "FIXTURE_QWEN_HARBOR_BASE_URL"
	if payload.CommandID == stageprovider.HarborEvaluatorOpusCommandID {
		model = "fixture-opus"
		endpoint = "FIXTURE_OPUS_HARBOR_BASE_URL"
	}
	return &stageprovider.HarborEvaluatorOperationContract{
		Format: stageprovider.HarborEvaluatorOperationContractFormat, Version: stageprovider.HarborEvaluatorOperationContractVersion,
		HarborVersion: stageprovider.HarborEvaluatorHarborVersion, ResultABIFormat: stageprovider.HarborEvaluatorResultABIFormat, ResultABIVersion: stageprovider.HarborEvaluatorResultABIVersion,
		TaskArtifactPort: stageprovider.HarborEvaluatorTaskArtifactPort, TaskArtifactSchema: stageprovider.HarborEvaluatorTaskArtifactSchema,
		AgentID: "fixture-agent", AgentVersion: "1", ModelID: model, ModelVersion: "1",
		EndpointEnvName: endpoint, EndpointChildEnvKey: "ANTHROPIC_BASE_URL", EndpointFingerprint: workflowkit.SHA256Fingerprint([]byte("fixture-endpoint:" + endpoint)),
		SecretEnvTemplates: []stageprovider.HarborEvaluatorSecretEnvTemplate{{
			Secret: secrets[0], HostEnvKey: "FIXTURE_ANTHROPIC_AUTH_TOKEN", ChildEnvKey: "ANTHROPIC_AUTH_TOKEN", Template: stageprovider.HarborEvaluatorSecretValueTemplate,
		}},
		Attempts: stageprovider.HarborEvaluatorTrialCount, ConcurrentTrials: stageprovider.HarborEvaluatorConcurrentTrials, MaxRetries: stageprovider.HarborEvaluatorMaxRetries, RequireTrajectory: true,
		ScreenshotRenderer: stageprovider.HarborEvaluatorScreenshotRenderer{ID: "harbor-terminal-png", Version: "1", SchemaVersion: workflowadapter.CodeEdgeEvaluatorScreenshotSchemaVersion},
	}
}

type catalogLockFixtureDelegate struct{}

func (catalogLockFixtureDelegate) ValidateStageOperation(workflowadapter.StageOperationResolution) error {
	return nil
}

func (catalogLockFixtureDelegate) ResolveWorkflowkitStageOperation(workflowadapter.StageOperationResolution) (workflowkit.StageExecutor, error) {
	return workflowkit.StageExecutorFunc(func(context.Context, workflowkit.StageExecutionRequest) (workflowkit.StageExecutionResult, error) {
		return workflowkit.StageExecutionResult{}, nil
	}), nil
}

type catalogLockFixtureAttestor struct{}

func (catalogLockFixtureAttestor) AttestDeploymentOperation(context.Context, stageprovider.DeploymentOperationRuntimeAttestation) error {
	return nil
}
