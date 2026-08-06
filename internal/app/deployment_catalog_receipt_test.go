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

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/testsupport"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// deploymentCatalogLockIdentityFileName names the legacy per-Run lock identity
// companion file. The freeze mechanism was removed; tests keep the constant to
// prove no new Run or bundle ever writes it again.
const deploymentCatalogLockIdentityFileName = "deployment-catalog.lock-identity.json"

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
	if _, err := os.Stat(filepath.Join(root, managedRunsDirectory, run.ID, deploymentCatalogReceiptFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("non-production run unexpectedly wrote deployment catalog receipt: %v", err)
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

func TestCatalogLockEnabledStartRunKeepsReceiptWithoutFrozenIdentity(t *testing.T) {
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
		Trigger:           "catalog-lock-freeze",
	}
	prepared, err := services.Mutations.PrepareStartRun(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	bundleDirectory := filepath.Join(root, managedRunInputsDirectory, prepared.InputBundleID)
	bundleReceipt, err := os.ReadFile(filepath.Join(bundleDirectory, deploymentCatalogReceiptFileName))
	if err != nil || !bytes.Equal(bundleReceipt, expectedReceipt) {
		t.Fatalf("input bundle receipt = %s, %v; want frozen receipt", bundleReceipt, err)
	}
	if _, err := os.Stat(filepath.Join(bundleDirectory, deploymentCatalogLockIdentityFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("input bundle unexpectedly contains a lock identity file: %v", err)
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
	if _, err := os.Stat(filepath.Join(root, managedRunsDirectory, run.ID, deploymentCatalogLockIdentityFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("run unexpectedly contains a lock identity file: %v", err)
	}
	if err := services.core.verifyRunDeploymentCatalogReceipt(run); err != nil {
		t.Fatalf("verify frozen catalog receipt: %v", err)
	}
	if replayed, err := services.Mutations.StartRun(ctx, command); err != nil || replayed != started {
		t.Fatalf("catalog-bound StartRun replay = %+v, %v; want %+v", replayed, err, started)
	}
}

// TestLegacyFrozenLockIdentityFieldStaysDecodableAndCanonical proves that
// persistent records written by a binary that still froze the deployment lock
// identity remain strictly decodable and byte-canonical after the freeze
// mechanism was removed. The legacy field is tolerated, never read, and never
// re-persisted by a new Run.
func TestLegacyFrozenLockIdentityFieldStaysDecodableAndCanonical(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	bootstrap, err := NewLifecycleServicesWithOptions(root, database, LifecycleServicesOptions{OperationResolver: testsupport.AcceptAllStageOperationResolver()})
	if err != nil {
		t.Fatal(err)
	}
	task, revision := importCatalogReceiptFixture(t, ctx, bootstrap, "legacy-lock-field")
	profile := catalogReceiptProfile(t)
	specification := catalogReceiptExecutionSpec(task.ID, revision.ID, revision.TaskDigest)
	resolver := catalogLockAttestedResolverForSpec(t, specification, "legacy-lock-field", "v1", "lock-v1")
	services, err := NewLifecycleServicesWithOptions(root, database, LifecycleServicesOptions{
		OperationResolver: resolver, RequireDeploymentCatalog: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := services.Mutations.CaptureCheckpoint(ctx, task.ID, revision.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	command := StartRunLifecycleCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{
			IdempotencyKey: mustLifecycleMutationUUID(t), Actor: "tester", Reason: "legacy lock identity compatibility", Expected: checkpoint,
		},
		ProfilePath:       writeCatalogReceiptProfile(t, root, profile),
		ExecutionSpecPath: writeCatalogReceiptExecutionSpec(t, root, specification),
		Trigger:           "legacy-lock-field",
	}
	prepared, err := services.Mutations.PrepareStartRun(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	bundleRaw, err := os.ReadFile(filepath.Join(root, managedRunInputsDirectory, prepared.InputBundleID, runStartInputManifestFileName))
	if err != nil {
		t.Fatal(err)
	}
	legacyBundle := injectLegacyDeploymentCatalogLockIdentity(t, string(bundleRaw), `"created_at"`)
	var bundle runStartInputBundle
	if err := decodeStrictJSON(string(legacyBundle), &bundle); err != nil {
		t.Fatalf("decode legacy StartRun input bundle: %v", err)
	}
	if len(bundle.LegacyDeploymentCatalogLockIdentity) == 0 {
		t.Fatal("legacy StartRun input bundle did not retain the lock identity field")
	}
	// The input bundle manifest is written indented, so re-marshaling is not
	// byte-canonical; only strict decodability and field tolerance matter.
	if _, err := json.Marshal(bundle); err != nil {
		t.Fatalf("re-marshal legacy StartRun input bundle: %v", err)
	}

	started, err := services.Mutations.StartRun(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	run, err := services.Runs.Get(ctx, started.RunID)
	if err != nil {
		t.Fatal(err)
	}
	legacyManifest := injectLegacyDeploymentCatalogLockIdentity(t, run.RunManifestJSON, `"created_at"`)
	var manifest runManifest
	if err := decodeStrictJSON(string(legacyManifest), &manifest); err != nil {
		t.Fatalf("decode run manifest with legacy lock identity: %v", err)
	}
	if len(manifest.LegacyDeploymentCatalogLockIdentity) == 0 {
		t.Fatal("legacy run manifest did not retain the lock identity field")
	}
	canonicalManifest, err := json.Marshal(manifest)
	if err != nil || !bytes.Equal(canonicalManifest, legacyManifest) {
		t.Fatalf("legacy run manifest canonical round trip = %s, %v", canonicalManifest, err)
	}
	legacyRun := run
	legacyRun.RunManifestJSON = string(legacyManifest)
	if err := services.core.verifyRunDeploymentCatalogReceipt(legacyRun); err != nil {
		t.Fatalf("verify legacy run manifest catalog receipt: %v", err)
	}

	legacyPreparation := injectLegacyDeploymentCatalogLockIdentity(t,
		`{"format":"harbor.standard-authoring-launch-preparation.v1","version":"5","lifecycle_operation_id":"018f0a73-3b49-7000-8000-000000000001","requested_source_id":"018f0a73-3b49-7000-8000-000000000002","target_task_id":"018f0a73-3b49-7000-8000-000000000003","authoring_session_id":"018f0a73-3b49-7000-8000-000000000004","run_id":"018f0a73-3b49-7000-8000-000000000005","workflow_template_id":"harbor.standard-authoring.v2","workflow_template_version":"1","source_snapshot_schema_version":"1","execution_profile":{},"profile_fingerprint":"fp","deployment_catalog_receipt":{},"preparation_fingerprint":"prep"}`,
		`"preparation_fingerprint"`)
	var preparation standardAuthoringLaunchPreparation
	if err := decodeStrictJSON(string(legacyPreparation), &preparation); err != nil {
		t.Fatalf("decode legacy Standard authoring launch preparation: %v", err)
	}
	if len(preparation.LegacyDeploymentCatalogLockIdentity) == 0 {
		t.Fatal("legacy launch preparation did not retain the lock identity field")
	}
	canonicalPreparation, err := json.Marshal(preparation)
	if err != nil || !bytes.Equal(canonicalPreparation, legacyPreparation) {
		t.Fatalf("legacy launch preparation canonical round trip = %s, %v", canonicalPreparation, err)
	}
}

// injectLegacyDeploymentCatalogLockIdentity inserts the lock identity field
// at the exact position the pre-runtime-resolution binary wrote it: after the
// deployment catalog receipt and before the next struct field.
func injectLegacyDeploymentCatalogLockIdentity(t *testing.T, canonicalJSON string, anchor string) []byte {
	t.Helper()
	legacy := `"deployment_catalog_lock_identity":{"lock_id":"standard-authoring-production-lock","lock_version":"v1.8-9ac6a2c-codex146","fingerprint":"sha256:ec4072ef21d92ca5cc0237bc170662e37ef49fbfafe9fc4f95191cc9436742eb"},`
	index := strings.Index(canonicalJSON, anchor)
	if index < 0 {
		t.Fatalf("legacy lock identity anchor %q missing from %s", anchor, canonicalJSON)
	}
	return []byte(canonicalJSON[:index] + legacy + canonicalJSON[index:])
}

func TestCatalogLockChangeStaysCompatibleWhenReceiptMatches(t *testing.T) {
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
	// A Run frozen under one lock version must remain compatible with a
	// deployment that installed a newer lock for the same approved catalog;
	// only the catalog receipt is frozen per Run.
	started, err := servicesB.Mutations.StartRun(ctx, command)
	if err != nil {
		t.Fatalf("prepared StartRun under changed lock version = %v, want compatible start", err)
	}
	run, err := servicesB.Runs.Get(ctx, started.RunID)
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := decodeFrozenRunDefinition(run)
	if err != nil {
		t.Fatal(err)
	}
	runtime := newFrozenRuntime(t, servicesB, catalogReceiptRuntimeRegistry(t, frozen.Workflow, completedFixtureStage))
	if _, _, err := runtime.loadFrozenRun(ctx, run.ID, run.DefinitionHash, frozen.ExecutionSpecFingerprint, frozen.QuotaPolicy); err != nil {
		t.Fatalf("load frozen run under changed lock version = %v", err)
	}

	worker := newFrozenRuntimeWorker(t, database, runtime, "catalog-lock-drift-worker")
	result, workerErr := worker.RunOnce(ctx)
	if workerErr != nil || result.FinalState != store.JobSucceeded {
		t.Fatalf("worker under changed lock version = %+v, %v; want succeeded", result, workerErr)
	}
	updated, err := database.GetWorkflowRun(ctx, run.ID)
	if err != nil || updated == nil || updated.Status == store.WorkflowRunInDoubt {
		t.Fatalf("worker under changed lock version left run = %+v, %v; want not in_doubt", updated, err)
	}
}

func TestCandidateChildManifestKeepsReceiptWithoutLockIdentity(t *testing.T) {
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
	// exercises only catalog receipt propagation, so materialize an unchanged
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
	expectedReceipt, err := resolver.CanonicalReceiptJSON()
	if err != nil {
		t.Fatal(err)
	}
	childReceipt, err := canonicalManifestDeploymentCatalogReceipt(child)
	if err != nil || !bytes.Equal(childReceipt, expectedReceipt) {
		t.Fatalf("candidate child manifest receipt = %s, %v; want %s", childReceipt, err, expectedReceipt)
	}
	// The lock identity is no longer frozen into candidate child runs; only
	// the deployment catalog receipt carries over from the source Run.
	if _, err := os.Stat(filepath.Join(root, managedRunsDirectory, candidate.TargetRunID, deploymentCatalogLockIdentityFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("candidate child unexpectedly contains a lock identity file: %v", err)
	}
	if reused, err := services.Changes.ensureCandidateChildRunManifest(ctx, candidate, run); err != nil || reused != string(childRaw) {
		t.Fatalf("candidate child manifest reuse = %q, %v; want unchanged %q", reused, err, childRaw)
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
	return catalogLockLifecycleServicesWithTemplateResolvers(t, root, database, resolver)
}

func catalogLockLifecycleServicesWithTemplateResolvers(t *testing.T, root string, database *store.Store, resolvers ...*stageprovider.CatalogLockAttestedWorkflowkitProviderOperationResolver) *LifecycleServices {
	t.Helper()
	if len(resolvers) == 1 {
		services, err := NewLifecycleServicesWithOptions(root, database, LifecycleServicesOptions{
			OperationResolver:          resolvers[0],
			DeploymentCatalogResolvers: []TemplateDeploymentCatalogResolver{{Template: resolvers[0].Receipt().Template, Resolver: resolvers[0]}},
			RequireDeploymentCatalog:   true,
		})
		if err != nil {
			t.Fatal(err)
		}
		return services
	}
	registrations := make([]stageprovider.TemplateWorkflowkitProviderRegistration, 0, len(resolvers))
	bindings := make([]TemplateDeploymentCatalogResolver, 0, len(resolvers))
	for _, resolver := range resolvers {
		registrations = append(registrations, stageprovider.TemplateWorkflowkitProviderRegistration{Template: resolver.Receipt().Template, Resolver: resolver})
		bindings = append(bindings, TemplateDeploymentCatalogResolver{Template: resolver.Receipt().Template, Resolver: resolver})
	}
	router, err := stageprovider.NewTemplateWorkflowkitProviderOperationResolver(registrations)
	if err != nil {
		t.Fatal(err)
	}
	services, err := NewLifecycleServicesWithOptions(root, database, LifecycleServicesOptions{
		OperationResolver:          router,
		DeploymentCatalogResolvers: bindings,
		RequireDeploymentCatalog:   true,
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
			Provider:  resolution.Provider,
			Operation: resolution.Operation.Clone(),
			Runtime:   resolution.Runtime,
			Checkout:  stageprovider.DeploymentCheckoutContract{ID: resolution.Checkout.ID, Purpose: "app-catalog-lock-test"},
			Secrets:   secrets,
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
