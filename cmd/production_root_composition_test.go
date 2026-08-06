package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestHarborFlowProductionCompositionInstallsStandardAuthoringTemplateBundle(t *testing.T) {
	fixture := newHarborFlowProductionCompositionFixture(t)
	dataStore, err := store.OpenForTest(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()

	lookups := 0
	config := fixture.config
	config.LookupEnvironment = func(string) (string, bool) {
		lookups++
		return "", false
	}
	services, err := newHarborFlowProductionLifecycleServicesWithConfig(t.TempDir(), dataStore, config)
	if err != nil {
		t.Fatalf("compose unified Harbor Flow production lifecycle services: %v", err)
	}
	if lookups != 0 {
		t.Fatalf("production composition looked up %d environment values before execution", lookups)
	}

	router, ok := services.WorkflowkitProviderOperationResolver().(*stageprovider.TemplateWorkflowkitProviderOperationResolver)
	if !ok || router == nil {
		t.Fatalf("production operation resolver = %T, want template-scoped router", services.WorkflowkitProviderOperationResolver())
	}
	wantTemplates := []workflowadapter.TemplateReference{
		workflowadapter.StandardAuthoringCurrentTemplateReference(),
	}
	if got := router.Templates(); !reflect.DeepEqual(got, wantTemplates) {
		t.Fatalf("installed production template bundles = %#v, want %#v", got, wantTemplates)
	}
	if services.CatalogLockAttestedWorkflowkitProviderResolver() != nil {
		t.Fatal("unified production composition exposed a single-template resolver")
	}

	if services.AuthoringLaunches == nil || !services.AuthoringLaunches.Available() {
		t.Fatal("unified production composition omitted the Standard source capture and definition capability")
	}
	if services.RunActivations == nil || !services.RunActivations.Available() {
		t.Fatal("unified production composition omitted the queued Run activation capability")
	}

	for _, test := range []struct {
		name     string
		template workflowadapter.TemplateReference
		record   stageprovider.DeploymentOperationCatalogLockRecord
		wrong    workflowadapter.TemplateReference
	}{
		{
			name:     "Standard authoring",
			template: workflowadapter.StandardAuthoringCurrentTemplateReference(),
			record:   fixture.standardLock.Operations[0],
			wrong:    workflowadapter.TemplateReference{ID: workflowadapter.StandardAuthoringWorkflowTemplateID, Version: "2.0.0"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			resolution := rootCompositionResolution(test.template, test.record)
			if err := router.ValidateStageOperation(resolution); err != nil {
				t.Fatalf("validate %s operation through its template bundle: %v", test.name, err)
			}
			wrongTemplate := resolution.Clone()
			wrongTemplate.Template = test.wrong
			if err := router.ValidateStageOperation(wrongTemplate); err == nil {
				t.Fatalf("%s operation was accepted through %s@%s", test.name, test.wrong.ID, test.wrong.Version)
			}
		})
	}
}

func TestHarborFlowProductionCompositionRejectsMismatchedBundleBuildIdentity(t *testing.T) {
	fixture := newHarborFlowProductionCompositionFixture(t)
	dataStore, err := store.OpenForTest(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()

	config := fixture.config
	config.StandardBinding.HarborFlowBuild.ContentSHA256 = workflowkit.SHA256Fingerprint([]byte("different-standard-build"))
	if _, err := newHarborFlowProductionLifecycleServicesWithConfig(t.TempDir(), dataStore, config); err == nil || !strings.Contains(err.Error(), "production build identity does not match") {
		t.Fatalf("mismatched bundle build identity error = %v", err)
	}
}

func TestProductionDeploymentPathsBesideExecutableRequiresStandardBundleAndContractManifest(t *testing.T) {
	packageRoot := t.TempDir()
	executable := filepath.Join(packageRoot, "harbor-factory")
	if err := os.WriteFile(executable, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := productionDeploymentPathsBesideExecutable(executable); err == nil {
		t.Fatal("package missing the deployment tree was accepted")
	}

	deployments := filepath.Join(packageRoot, productionDeploymentsDirectory)
	standard := filepath.Join(deployments, standardAuthoringDeploymentDirectory)
	rootCompositionWritePathFixtureBundle(t, standard, true, true)
	if _, err := productionDeploymentPathsBesideExecutable(executable); err == nil || !strings.Contains(err.Error(), "Standard authoring contract asset manifest") {
		t.Fatalf("package without Standard contract manifest error = %v", err)
	}
	rootCompositionWritePathFixtureFile(t, filepath.Join(standard, standardAuthoringContractManifestFile))
	if _, err := productionDeploymentPathsBesideExecutable(executable); err == nil || !strings.Contains(err.Error(), "Standard authoring SSH known_hosts") {
		t.Fatalf("package without Standard SSH known_hosts error = %v", err)
	}
	rootCompositionWritePathFixtureFile(t, filepath.Join(standard, filepath.FromSlash(stageprovider.StandardAuthoringSSHKnownHostsRelativePath)))

	if err := os.Remove(filepath.Join(standard, productionDeploymentLockFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := productionDeploymentPathsBesideExecutable(executable); err == nil || !strings.Contains(err.Error(), "Standard authoring lock") {
		t.Fatalf("package with incomplete Standard bundle error = %v", err)
	}
	rootCompositionWritePathFixtureFile(t, filepath.Join(standard, productionDeploymentLockFile))

	paths, err := productionDeploymentPathsBesideExecutable(executable)
	if err != nil {
		t.Fatalf("complete Standard bundle package rejected: %v", err)
	}
	want := productionDeploymentPaths{
		StandardCatalog:      filepath.Join(standard, productionDeploymentCatalogFile),
		StandardLock:         filepath.Join(standard, productionDeploymentLockFile),
		StandardContractRoot: standard,
	}
	if paths != want {
		t.Fatalf("production deployment paths = %#v, want %#v", paths, want)
	}
}

func TestPreflightHarborFlowProductionDeploymentBundlesRejectsStaleLockWithoutManagedRoot(t *testing.T) {
	fixture := newHarborFlowProductionCompositionFixture(t)
	lockPath := fixture.config.Paths.StandardLock
	raw, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	stale := strings.Replace(string(raw), `"version":"3"`, `"version":"2"`, 1)
	if stale == string(raw) {
		t.Fatal("could not produce stale Standard authoring lock fixture")
	}
	if err := os.WriteFile(lockPath, []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	managedRoot := filepath.Join(t.TempDir(), "managed-control-plane")
	err = preflightHarborFlowProductionDeploymentBundles(fixture.config.Paths, fixture.config.StandardBinding)
	if err == nil || !strings.Contains(err.Error(), "Standard authoring") {
		t.Fatalf("stale Standard lock preflight error = %v", err)
	}
	assertNoPreflightManagedRoot(t, managedRoot)
}

type harborFlowProductionCompositionFixture struct {
	config       harborFlowProductionCompositionConfig
	standardLock stageprovider.DeploymentOperationCatalogLock
}

func newHarborFlowProductionCompositionFixture(t *testing.T) harborFlowProductionCompositionFixture {
	t.Helper()
	packageRoot := t.TempDir()
	deployments := filepath.Join(packageRoot, productionDeploymentsDirectory)
	build := stageprovider.HarborFlowBuildIdentity{
		Module: "github.com/purplevoid/harbor-factory", Version: "v2.0.0", Commit: strings.Repeat("c", 40),
		ContentSHA256: workflowkit.SHA256Fingerprint([]byte("root-production-composition-test")),
	}

	standardSource, _, standardLock := standardAuthoringProductionTestDeployment(t)
	standard := filepath.Join(deployments, standardAuthoringDeploymentDirectory)
	copyStandardAuthoringDeploymentTree(t, standardSource, standard)
	standardLock = standardLock.Clone()
	standardLock.HarborFlowBuild = build
	rootCompositionWriteLock(t, filepath.Join(standard, productionDeploymentLockFile), standardLock)

	standardReceipt, standardIdentity := rootCompositionLockBinding(t, standardLock)
	return harborFlowProductionCompositionFixture{
		config: harborFlowProductionCompositionConfig{
			Paths: productionDeploymentPaths{
				StandardCatalog:      filepath.Join(standard, productionDeploymentCatalogFile),
				StandardLock:         filepath.Join(standard, productionDeploymentLockFile),
				StandardContractRoot: standard,
			},
			StandardBinding: standardAuthoringProductionBuildBinding{
				HarborFlowBuild: build, CatalogReceiptFingerprint: standardReceipt, LockIdentity: standardIdentity,
			},
		},
		standardLock: standardLock,
	}
}

func rootCompositionLockBinding(t *testing.T, lock stageprovider.DeploymentOperationCatalogLock) (workflowkit.Fingerprint, stageprovider.DeploymentOperationCatalogLockIdentity) {
	t.Helper()
	receipt, err := lock.CatalogReceipt.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := lock.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	return receipt, stageprovider.DeploymentOperationCatalogLockIdentity{
		LockID: lock.LockID, LockVersion: lock.LockVersion, Fingerprint: fingerprint,
	}
}

func rootCompositionWriteLock(t *testing.T, path string, lock stageprovider.DeploymentOperationCatalogLock) {
	t.Helper()
	canonical, err := lock.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, canonical, 0o600); err != nil {
		t.Fatal(err)
	}
}

func rootCompositionResolution(template workflowadapter.TemplateReference, record stageprovider.DeploymentOperationCatalogLockRecord) workflowadapter.StageOperationResolution {
	return workflowadapter.StageOperationResolution{
		Template: template, StageKey: record.Stage.Key, StageType: record.Stage.Type, Plugin: record.Stage.Plugin,
		Provider: record.Provider, Operation: record.Operation.Clone(),
		Checkout: workflowadapter.CheckoutReference{
			ID: record.Checkout.ID, RevisionID: "018f0a73-3b49-7000-8000-000000000701",
			RevisionDigest: workflowkit.SubjectDigest("harbor.task.v2:sha256:" + strings.Repeat("a", 64)),
		},
		Runtime: record.Runtime, Secrets: append([]workflowadapter.SecretReference(nil), record.Secrets...),
	}
}

func rootCompositionWritePathFixtureBundle(t *testing.T, directory string, catalog, lock bool) {
	t.Helper()
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if catalog {
		rootCompositionWritePathFixtureFile(t, filepath.Join(directory, productionDeploymentCatalogFile))
	}
	if lock {
		rootCompositionWritePathFixtureFile(t, filepath.Join(directory, productionDeploymentLockFile))
	}
}

func rootCompositionWritePathFixtureFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
}
