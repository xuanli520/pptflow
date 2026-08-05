package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestHarborFlowProductionCompositionInstallsThreeIndependentTemplateBundles(t *testing.T) {
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

	if fixture.config.StandardBinding.CatalogReceiptFingerprint == fixture.config.CodeEdgePhase1Binding.CatalogReceiptFingerprint ||
		fixture.config.StandardBinding.CatalogReceiptFingerprint == fixture.config.EvaluatorBinding.CatalogReceiptFingerprint ||
		fixture.config.CodeEdgePhase1Binding.CatalogReceiptFingerprint == fixture.config.EvaluatorBinding.CatalogReceiptFingerprint {
		t.Fatal("test fixture must use three distinct deployment catalog receipts")
	}
	if fixture.config.StandardBinding.LockIdentity == fixture.config.CodeEdgePhase1Binding.LockIdentity ||
		fixture.config.StandardBinding.LockIdentity == fixture.config.EvaluatorBinding.LockIdentity ||
		fixture.config.CodeEdgePhase1Binding.LockIdentity == fixture.config.EvaluatorBinding.LockIdentity {
		t.Fatal("test fixture must use three distinct deployment lock identities")
	}

	router, ok := services.WorkflowkitProviderOperationResolver().(*stageprovider.TemplateWorkflowkitProviderOperationResolver)
	if !ok || router == nil {
		t.Fatalf("production operation resolver = %T, want template-scoped router", services.WorkflowkitProviderOperationResolver())
	}
	wantTemplates := []workflowadapter.TemplateReference{
		workflowadapter.CodeEdgeEvaluatorChildTemplateReference(),
		workflowadapter.CodeEdgePhase1TemplateReference(),
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
	if services.EvaluatorLaunches == nil || !services.EvaluatorLaunches.Available() {
		t.Fatal("unified production composition omitted the lock-owned evaluator-child definition capability")
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
			wrong:    workflowadapter.CodeEdgePhase1TemplateReference(),
		},
		{
			name:     "CodeEdge Phase-1 parent",
			template: workflowadapter.CodeEdgePhase1TemplateReference(),
			record:   fixture.parentLock.Operations[0],
			wrong:    workflowadapter.CodeEdgeEvaluatorChildTemplateReference(),
		},
		{
			name:     "CodeEdge evaluator child",
			template: workflowadapter.CodeEdgeEvaluatorChildTemplateReference(),
			record:   fixture.evaluatorLock.Operations[0],
			wrong:    workflowadapter.StandardAuthoringCurrentTemplateReference(),
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
	config.CodeEdgePhase1Binding.HarborFlowBuild.ContentSHA256 = workflowkit.SHA256Fingerprint([]byte("different-parent-build"))
	if _, err := newHarborFlowProductionLifecycleServicesWithConfig(t.TempDir(), dataStore, config); err == nil || !strings.Contains(err.Error(), "all three production deployment locks must bind the same Harbor Flow build identity") {
		t.Fatalf("mismatched three-bundle build identity error = %v", err)
	}
}

func TestHarborFlowProductionCompositionAcceptsReviewedStandardAuthoringPredecessorLock(t *testing.T) {
	fixture := newHarborFlowProductionCompositionFixture(t)
	dataStore, err := store.OpenForTest(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()

	predecessor := fixture.standardLock.Clone()
	predecessor.LockVersion = "v2.0.0-reviewed-predecessor"
	predecessorFingerprint, err := predecessor.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	contract, err := predecessor.ExecutionContractFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	config := fixture.config
	config.StandardAuthoringCompatibleLockProofs = []stageprovider.DeploymentOperationCatalogLockCompatibilityProof{{
		Predecessor: stageprovider.DeploymentOperationCatalogLockIdentity{
			LockID: predecessor.LockID, LockVersion: predecessor.LockVersion, Fingerprint: predecessorFingerprint,
		},
		ExecutionContractFingerprint: contract,
	}}
	if _, err := newHarborFlowProductionLifecycleServicesWithConfig(t.TempDir(), dataStore, config); err != nil {
		t.Fatalf("compose with reviewed Standard authoring predecessor lock: %v", err)
	}
}

func TestProductionDeploymentPathsBesideExecutableRequiresAllThreeBundlesAndStandardContractManifest(t *testing.T) {
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
	parent := filepath.Join(deployments, codeEdgePhase1DeploymentDirectory)
	rootCompositionWritePathFixtureBundle(t, parent, true, true)
	evaluator := filepath.Join(deployments, codeEdgeEvaluatorChildDeploymentDirectory)
	rootCompositionWritePathFixtureBundle(t, evaluator, true, true)
	if _, err := productionDeploymentPathsBesideExecutable(executable); err == nil || !strings.Contains(err.Error(), "Standard authoring contract asset manifest") {
		t.Fatalf("package without Standard contract manifest error = %v", err)
	}
	rootCompositionWritePathFixtureFile(t, filepath.Join(standard, standardAuthoringContractManifestFile))
	if _, err := productionDeploymentPathsBesideExecutable(executable); err == nil || !strings.Contains(err.Error(), "Standard authoring SSH known_hosts") {
		t.Fatalf("package without Standard SSH known_hosts error = %v", err)
	}
	rootCompositionWritePathFixtureFile(t, filepath.Join(standard, filepath.FromSlash(stageprovider.StandardAuthoringSSHKnownHostsRelativePath)))

	if err := os.Remove(filepath.Join(parent, productionDeploymentLockFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := productionDeploymentPathsBesideExecutable(executable); err == nil || !strings.Contains(err.Error(), "CodeEdge Phase-1 lock") {
		t.Fatalf("package with incomplete parent bundle error = %v", err)
	}
	rootCompositionWritePathFixtureFile(t, filepath.Join(parent, productionDeploymentLockFile))

	if err := os.Remove(filepath.Join(evaluator, productionDeploymentLockFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := productionDeploymentPathsBesideExecutable(executable); err == nil || !strings.Contains(err.Error(), "CodeEdge evaluator child lock") {
		t.Fatalf("package with incomplete evaluator bundle error = %v", err)
	}
	rootCompositionWritePathFixtureFile(t, filepath.Join(evaluator, productionDeploymentLockFile))

	paths, err := productionDeploymentPathsBesideExecutable(executable)
	if err != nil {
		t.Fatalf("complete three-bundle package rejected: %v", err)
	}
	want := productionDeploymentPaths{
		StandardCatalog:      filepath.Join(standard, productionDeploymentCatalogFile),
		StandardLock:         filepath.Join(standard, productionDeploymentLockFile),
		StandardContractRoot: standard,
		ParentCatalog:        filepath.Join(parent, productionDeploymentCatalogFile),
		ParentLock:           filepath.Join(parent, productionDeploymentLockFile),
		EvaluatorCatalog:     filepath.Join(evaluator, productionDeploymentCatalogFile),
		EvaluatorLock:        filepath.Join(evaluator, productionDeploymentLockFile),
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
	err = preflightHarborFlowProductionDeploymentBundles(fixture.config.Paths, fixture.config.StandardBinding, fixture.config.CodeEdgePhase1Binding, fixture.config.EvaluatorBinding)
	if err == nil || !strings.Contains(err.Error(), "Standard authoring") {
		t.Fatalf("stale Standard lock preflight error = %v", err)
	}
	assertNoPreflightManagedRoot(t, managedRoot)
}

type harborFlowProductionCompositionFixture struct {
	config        harborFlowProductionCompositionConfig
	standardLock  stageprovider.DeploymentOperationCatalogLock
	parentLock    stageprovider.DeploymentOperationCatalogLock
	evaluatorLock stageprovider.DeploymentOperationCatalogLock
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

	sourceRoot := standardAuthoringProductionRepositoryRoot(t)
	parent := filepath.Join(deployments, codeEdgePhase1DeploymentDirectory)
	copyStandardAuthoringDeploymentTree(t, filepath.Join(sourceRoot, productionDeploymentsDirectory, codeEdgePhase1DeploymentDirectory), parent)
	parentLock := rootCompositionParentLock(t, filepath.Join(parent, productionDeploymentCatalogFile), build)
	rootCompositionWriteLock(t, filepath.Join(parent, productionDeploymentLockFile), parentLock)

	evaluator := filepath.Join(deployments, codeEdgeEvaluatorChildDeploymentDirectory)
	copyStandardAuthoringDeploymentTree(t, filepath.Join(sourceRoot, productionDeploymentsDirectory, codeEdgeEvaluatorChildDeploymentDirectory), evaluator)
	evaluatorLock := rootCompositionEvaluatorLock(t, filepath.Join(evaluator, productionDeploymentCatalogFile), build)
	rootCompositionWriteLock(t, filepath.Join(evaluator, productionDeploymentLockFile), evaluatorLock)

	standardReceipt, standardIdentity := rootCompositionLockBinding(t, standardLock)
	parentReceipt, parentIdentity := rootCompositionLockBinding(t, parentLock)
	evaluatorReceipt, evaluatorIdentity := rootCompositionLockBinding(t, evaluatorLock)
	return harborFlowProductionCompositionFixture{
		config: harborFlowProductionCompositionConfig{
			Paths: productionDeploymentPaths{
				StandardCatalog:      filepath.Join(standard, productionDeploymentCatalogFile),
				StandardLock:         filepath.Join(standard, productionDeploymentLockFile),
				StandardContractRoot: standard,
				ParentCatalog:        filepath.Join(parent, productionDeploymentCatalogFile),
				ParentLock:           filepath.Join(parent, productionDeploymentLockFile),
				EvaluatorCatalog:     filepath.Join(evaluator, productionDeploymentCatalogFile),
				EvaluatorLock:        filepath.Join(evaluator, productionDeploymentLockFile),
			},
			StandardBinding: standardAuthoringProductionBuildBinding{
				HarborFlowBuild: build, CatalogReceiptFingerprint: standardReceipt, LockIdentity: standardIdentity,
			},
			CodeEdgePhase1Binding: codeEdgePhase1ProductionBuildBinding{
				HarborFlowBuild: build, CatalogReceiptFingerprint: parentReceipt, LockIdentity: parentIdentity,
			},
			EvaluatorBinding: codeEdgeProductionBuildBinding{
				HarborFlowBuild: build, CatalogReceiptFingerprint: evaluatorReceipt, LockIdentity: evaluatorIdentity,
			},
		},
		standardLock: standardLock, parentLock: parentLock, evaluatorLock: evaluatorLock,
	}
}

func rootCompositionParentLock(t *testing.T, catalogPath string, build stageprovider.HarborFlowBuildIdentity) stageprovider.DeploymentOperationCatalogLock {
	t.Helper()
	catalog := rootCompositionCatalog(t, catalogPath)
	lock := stageprovider.DeploymentOperationCatalogLock{
		Format: stageprovider.DeploymentOperationCatalogLockFormat, Version: stageprovider.DeploymentOperationCatalogLockVersion,
		LockID: "root-composition-codeedge-phase1", LockVersion: "1.0.0", CatalogReceipt: catalog.Receipt(), HarborFlowBuild: build,
		CodeEdgePhase1ExecutionProfile:      &stageprovider.CodeEdgePhase1ExecutionProfileLock{Profile: codeEdgePhase1DefinitionProviderProfile(t)},
		CodeEdgePhase1PreflightProfile:      &stageprovider.CodeEdgePhase1PreflightProfileLock{Profile: codeEdgePhase1DefinitionProviderPreflightProfile(t)},
		CodeEdgePhase1FinalCompliancePolicy: &stageprovider.CodeEdgePhase1FinalCompliancePolicyLock{Policy: codeEdgePhase1DefinitionProviderPolicy()},
		Operations:                          make([]stageprovider.DeploymentOperationCatalogLockRecord, 0, len(catalog.Catalog().Operations)),
	}
	for _, registration := range catalog.Catalog().Operations {
		lock.Operations = append(lock.Operations, rootCompositionParentLockRecord(t, registration))
	}
	if _, err := stageprovider.NewDeploymentOperationCatalogLockResolver(catalog, lock); err != nil {
		t.Fatalf("construct parent fixture catalog lock: %v", err)
	}
	return lock
}

func rootCompositionParentLockRecord(t *testing.T, registration stageprovider.DeploymentOperationRegistration) stageprovider.DeploymentOperationCatalogLockRecord {
	t.Helper()
	record := rootCompositionLockRecord(registration)
	switch payload := registration.Operation.Payload.(type) {
	case workflowadapter.LocalCommandOperationPayload:
		record.LocalExecutable = &stageprovider.LocalExecutableLock{
			CommandID: payload.CommandID, AbsolutePath: "/opt/harbor-factory-test/" + payload.CommandID, Version: "1.0.0",
			ContentSHA256: workflowkit.SHA256Fingerprint([]byte("parent-command:" + payload.CommandID)),
		}
	case workflowadapter.HarborBuiltinOperationPayload:
		record.HarborFlowBuiltin = &stageprovider.HarborFlowBuiltinOperationLock{
			Format: stageprovider.HarborFlowBuiltinOperationLockFormat, Version: stageprovider.HarborFlowBuiltinOperationLockVersion,
			HandlerID: payload.HandlerID, HandlerVersion: "1.0.0",
		}
	case workflowadapter.DurableReviewOperationPayload:
		record.DurableReviewPolicy = &stageprovider.DurableReviewPolicyLock{PolicyID: payload.PolicyID, Version: "1.0.0"}
	default:
		t.Fatalf("unsupported parent fixture payload %T", payload)
	}
	return record
}

func rootCompositionEvaluatorLock(t *testing.T, catalogPath string, build stageprovider.HarborFlowBuildIdentity) stageprovider.DeploymentOperationCatalogLock {
	t.Helper()
	catalog := rootCompositionCatalog(t, catalogPath)
	lock := stageprovider.DeploymentOperationCatalogLock{
		Format: stageprovider.DeploymentOperationCatalogLockFormat, Version: stageprovider.DeploymentOperationCatalogLockVersion,
		LockID: "root-composition-codeedge-evaluator", LockVersion: "1.0.0", CatalogReceipt: catalog.Receipt(), HarborFlowBuild: build,
		CodeEdgeEvaluatorChildExecutionProfile: &stageprovider.CodeEdgeEvaluatorChildExecutionProfileLock{Profile: rootCompositionEvaluatorProfile(t)},
		Operations:                             make([]stageprovider.DeploymentOperationCatalogLockRecord, 0, len(catalog.Catalog().Operations)),
	}
	for _, registration := range catalog.Catalog().Operations {
		payload, ok := registration.Operation.Payload.(workflowadapter.LocalCommandOperationPayload)
		if !ok || registration.HarborEvaluator == nil {
			t.Fatalf("evaluator fixture registration %q is not a locked local Harbor evaluator operation", registration.Stage.Key)
		}
		launcher := stageprovider.LocalExecutableLock{
			CommandID: payload.CommandID, AbsolutePath: "/opt/harbor-factory-test/" + payload.CommandID, Version: "0.18.0",
			ContentSHA256: workflowkit.SHA256Fingerprint([]byte("evaluator-launcher:" + payload.CommandID)),
		}
		record := rootCompositionLockRecord(registration)
		record.LocalExecutable = &launcher
		record.HarborEvaluator = &stageprovider.HarborEvaluatorOperationLock{
			Contract: registration.HarborEvaluator.Clone(), Launcher: launcher,
			ClaudeCodeExecutable: stageprovider.LocalExecutableLock{
				CommandID: stageprovider.HarborEvaluatorClaudeCodeCommandID, AbsolutePath: "/opt/harbor-factory-test/claude", Version: registration.HarborEvaluator.AgentVersion,
				ContentSHA256: workflowkit.SHA256Fingerprint([]byte("evaluator-claude-code")),
			},
			PythonInterpreter: stageprovider.LocalExecutableLock{
				CommandID: stageprovider.HarborEvaluatorPythonCommandID, AbsolutePath: "/opt/harbor-factory-test/python3", Version: "3.13.0",
				ContentSHA256: workflowkit.SHA256Fingerprint([]byte("evaluator-python")),
			},
			PythonSourceTree: stageprovider.HarborPythonSourceTreeLock{
				AbsolutePath: "/opt/harbor-factory-test/site-packages/harbor", PythonFilesSHA256: workflowkit.SHA256Fingerprint([]byte("evaluator-python-tree")),
			},
			DockerCLI: stageprovider.LocalExecutableLock{
				CommandID: stageprovider.HarborEvaluatorDockerCommandID, AbsolutePath: "/opt/harbor-factory-test/docker", Version: stageprovider.HarborEvaluatorDockerVersion,
				ContentSHA256: workflowkit.SHA256Fingerprint([]byte("evaluator-docker")),
			},
			DockerServerVersion: stageprovider.HarborEvaluatorDockerServerVersion,
			DockerComposePlugin: stageprovider.LocalExecutableLock{
				CommandID: stageprovider.HarborEvaluatorDockerComposeCommandID, AbsolutePath: "/opt/harbor-factory-test/cli-plugins/docker-compose", Version: stageprovider.HarborEvaluatorDockerComposeVersion,
				ContentSHA256: workflowkit.SHA256Fingerprint([]byte("evaluator-docker-compose")),
			},
			DockerBuildxPlugin: stageprovider.LocalExecutableLock{
				CommandID: stageprovider.HarborEvaluatorDockerBuildxCommandID, AbsolutePath: "/opt/harbor-factory-test/cli-plugins/docker-buildx", Version: stageprovider.HarborEvaluatorDockerBuildxVersion,
				ContentSHA256: workflowkit.SHA256Fingerprint([]byte("evaluator-docker-buildx")),
			},
			HarborVersionOutput: stageprovider.HarborEvaluatorHarborVersion, DockerComposeVersionOutput: stageprovider.HarborEvaluatorDockerComposeVersionOutput,
			DockerBuildxVersionOutput: stageprovider.HarborEvaluatorDockerBuildxVersionOutput,
		}
		lock.Operations = append(lock.Operations, record)
	}
	if _, err := stageprovider.NewDeploymentOperationCatalogLockResolver(catalog, lock); err != nil {
		t.Fatalf("construct evaluator fixture catalog lock: %v", err)
	}
	return lock
}

func rootCompositionEvaluatorProfile(t *testing.T) workflowadapter.ExecutionProfile {
	t.Helper()
	template := workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplate()
	profile := workflowadapter.ExecutionProfile{
		Template: template.Reference(), ID: "root-composition-evaluator", Version: "1.0.0",
		ContinuationPlanTTL: workflowadapter.RequiredContinuationPlanTTL, ControlGracePeriod: time.Minute,
		CandidateProviderBudget: workflowadapter.CandidateProviderBudget{AttemptTimeout: 5 * time.Minute},
		Stages:                  make([]workflowadapter.StageBudget, 0, len(template.Catalog.Stages)),
	}
	for _, stage := range template.Catalog.Stages {
		turns := stage.RequiredTurns
		if turns < 1 {
			turns = 1
		}
		attempt := time.Duration(turns) * time.Minute
		profile.Stages = append(profile.Stages, workflowadapter.StageBudget{StageKey: stage.Key, Budget: workflowkit.ExecutionBudget{
			TurnTimeout: time.Minute, MaxTurns: turns, AttemptTimeout: attempt, MaxAttempts: 1, MaxElapsed: attempt,
		}})
	}
	if err := profile.Validate(); err != nil {
		t.Fatalf("construct evaluator fixture profile: %v", err)
	}
	return profile
}

func rootCompositionCatalog(t *testing.T, path string) *stageprovider.DeploymentOperationCatalogResolver {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	document, err := stageprovider.ParseDeploymentOperationCatalogJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := stageprovider.NewDeploymentOperationCatalogResolver(document)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func rootCompositionLockRecord(registration stageprovider.DeploymentOperationRegistration) stageprovider.DeploymentOperationCatalogLockRecord {
	return stageprovider.DeploymentOperationCatalogLockRecord{
		Stage: registration.Stage, Provider: registration.Provider, Operation: registration.Operation.Clone(), Runtime: registration.Runtime,
		Checkout: registration.Checkout, Secrets: append([]workflowadapter.SecretReference{}, registration.Secrets...),
		PromptContentFingerprint: workflowkit.SHA256Fingerprint([]byte("root-composition-prompt:" + string(registration.Stage.Key))),
		SchemaContentFingerprint: workflowkit.SHA256Fingerprint([]byte("root-composition-schema:" + string(registration.Stage.Key))),
		ExecutionKind:            registration.Operation.Payload.Kind(),
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
