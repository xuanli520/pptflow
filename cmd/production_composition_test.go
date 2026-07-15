package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestCodeEdgeProductionDeploymentMaterialsAreStrictAndSecretFree(t *testing.T) {
	catalogPath, lockPath := testCodeEdgeProductionDeploymentPaths(t)
	catalog, err := stageprovider.LoadDeploymentOperationCatalogFile(catalogPath)
	if err != nil {
		t.Fatal(err)
	}
	if !catalog.Template().Equal(workflowadapter.CodeEdgeEvaluatorChildTemplateReference()) {
		t.Fatalf("catalog template = %+v, want evaluator child", catalog.Template())
	}
	lockRaw, err := readCodeEdgeProductionFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := stageprovider.ParseDeploymentOperationCatalogLockJSON(lockRaw)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := stageprovider.NewDeploymentOperationCatalogLockResolver(catalog, lock)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.VerifyLockIdentity(verifier.LockIdentity()); err != nil {
		t.Fatalf("verify loaded lock identity: %v", err)
	}

	for _, name := range []string{"ANTHROPIC_AUTH_TOKEN", "QWEN_HARBOR_BASE_URL", "OPUS_HARBOR_BASE_URL"} {
		if value, present := os.LookupEnv(name); present && value != "" && bytes.Contains(lockRaw, []byte(value)) {
			t.Fatalf("production lock contains a value from %s", name)
		}
		catalogRaw, readErr := readCodeEdgeProductionFile(catalogPath)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if value, present := os.LookupEnv(name); present && value != "" && bytes.Contains(catalogRaw, []byte(value)) {
			t.Fatalf("production catalog contains a value from %s", name)
		}
	}
}

func TestCodeEdgeProductionCompositionBuildsWithoutReadingEnvironmentValues(t *testing.T) {
	catalogPath, lockPath := testCodeEdgeProductionDeploymentPaths(t)
	binding := testCodeEdgeProductionBuildBinding(t)
	root := t.TempDir()
	database, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	lookups := 0
	services, err := newCodeEdgeProductionLifecycleServicesWithConfig(root, database, codeEdgeProductionCompositionConfig{
		CatalogPath:               catalogPath,
		LockPath:                  lockPath,
		HarborFlowBuild:           binding.HarborFlowBuild,
		CatalogReceiptFingerprint: binding.CatalogReceiptFingerprint,
		LockIdentity:              binding.LockIdentity,
		LookupEnvironment: func(string) (string, bool) {
			lookups++
			return "", false
		},
	})
	if err != nil {
		t.Fatalf("compose locked CodeEdge evaluator services: %v", err)
	}
	if lookups != 0 {
		t.Fatalf("service composition looked up %d environment values before execution", lookups)
	}
	if services.CatalogLockAttestedWorkflowkitProviderResolver() == nil {
		t.Fatal("production lifecycle services did not retain a catalog-lock-attested resolver")
	}
}

func TestLinkedCodeEdgeProductionBuildBindingUsesFullLockIdentity(t *testing.T) {
	want := testCodeEdgeProductionBuildBinding(t)
	setCodeEdgeProductionBuildBindingForTest(t, want)
	got, err := linkedCodeEdgeProductionBuildBinding()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("linked CodeEdge production build binding = %+v, want %+v", got, want)
	}
}

func TestCodeEdgeEvaluatorProviderDefinitionBindsOnlyApprovedEnvironmentNames(t *testing.T) {
	_, lockPath := testCodeEdgeProductionDeploymentPaths(t)
	lockRaw, err := readCodeEdgeProductionFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := stageprovider.ParseDeploymentOperationCatalogLockJSON(lockRaw)
	if err != nil {
		t.Fatal(err)
	}
	invocations, registrations, provider, err := codeEdgeEvaluatorProviderDefinition(lock)
	if err != nil {
		t.Fatal(err)
	}
	if provider.ID != "codeedge-harbor-evaluator" || provider.Kind != "evaluation" || len(registrations) != 2 || len(invocations) != 2 {
		t.Fatalf("provider definition = provider=%+v registrations=%d invocations=%d", provider, len(registrations), len(invocations))
	}
	byCommand := make(map[string]stageprovider.HarborEvaluatorInvocation, len(invocations))
	for _, invocation := range invocations {
		byCommand[invocation.CommandID] = invocation
	}
	assertInvocation := func(commandID, model, endpoint string) {
		t.Helper()
		invocation, found := byCommand[commandID]
		if !found {
			t.Fatalf("missing invocation %q", commandID)
		}
		if invocation.AgentID != "claude-code" || invocation.AgentVersion != "2.1.207" || invocation.ModelID != model || invocation.EndpointEnvName != endpoint || invocation.EndpointChildEnvKey != "ANTHROPIC_BASE_URL" || invocation.Attempts != 4 || invocation.ConcurrentTrials != 1 || invocation.MaxRetries != stageprovider.HarborEvaluatorMaxRetries {
			t.Fatalf("invocation %q is not the frozen evaluator contract", commandID)
		}
		if len(invocation.SecretEnvTemplates) != 1 || invocation.SecretEnvTemplates[0].HostEnvKey != "ANTHROPIC_AUTH_TOKEN" || invocation.SecretEnvTemplates[0].ChildEnvKey != "ANTHROPIC_AUTH_TOKEN" || invocation.SecretEnvTemplates[0].Template != stageprovider.HarborEvaluatorSecretValueTemplate {
			t.Fatalf("invocation %q secret mapping is not the approved private env-file mapping", commandID)
		}
	}
	assertInvocation(stageprovider.HarborEvaluatorQwenCommandID, "qwen3.7-max", "QWEN_HARBOR_BASE_URL")
	assertInvocation(stageprovider.HarborEvaluatorOpusCommandID, "claude-opus-4-6", "OPUS_HARBOR_BASE_URL")
}

func TestCodeEdgeEvaluatorDefinitionProviderBuildsLockOwnedChildBudgetAndSpec(t *testing.T) {
	_, lockPath := testCodeEdgeProductionDeploymentPaths(t)
	lockRaw, err := readCodeEdgeProductionFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := stageprovider.ParseDeploymentOperationCatalogLockJSON(lockRaw)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := newCodeEdgeEvaluatorRunDefinitionProvider(lock)
	if err != nil {
		t.Fatal(err)
	}
	parent := productionEvaluatorParentProfile(t)
	request := app.EvaluatorRunDefinitionRequest{
		TaskID:         "018f0a73-3b49-7000-8000-000000000091",
		RevisionID:     "018f0a73-3b49-7000-8000-000000000092",
		RevisionDigest: workflowkit.SubjectDigest("harbor.task.v2:sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		ParentRunID:    "018f0a73-3b49-7000-8000-000000000093",
		ParentProfile:  parent,
	}
	for _, key := range workflowadapter.CodeEdgeEvaluatorChildStageOrder() {
		if _, present := parent.Budget(key); present {
			t.Fatalf("Phase-1 parent unexpectedly contains evaluator stage %q", key)
		}
	}
	definition, err := provider.DefinitionForEvaluatorRun(context.Background(), request)
	if err != nil {
		t.Fatalf("build lock-owned evaluator definition: %v", err)
	}
	if err := definition.Profile.Validate(); err != nil {
		t.Fatalf("validate lock-owned evaluator profile: %v", err)
	}
	if err := definition.ExecutionSpec.Validate(); err != nil {
		t.Fatalf("validate lock-owned evaluator specification: %v", err)
	}
	if !definition.Profile.Template.Equal(workflowadapter.CodeEdgeEvaluatorChildTemplateReference()) || !definition.ExecutionSpec.Template.Equal(workflowadapter.CodeEdgeEvaluatorChildTemplateReference()) {
		t.Fatalf("definition templates = %#v/%#v, want evaluator child", definition.Profile.Template, definition.ExecutionSpec.Template)
	}
	if definition.Profile.ContinuationPlanTTL != workflowadapter.RequiredContinuationPlanTTL || definition.Profile.ControlGracePeriod != time.Minute || definition.Profile.CandidateProviderBudget != (workflowadapter.CandidateProviderBudget{AttemptTimeout: 30 * time.Minute, StartupGrace: 5 * time.Minute, ShutdownGrace: 5 * time.Minute}) {
		t.Fatalf("lock-owned evaluator profile envelope = %#v", definition.Profile)
	}
	for _, stageKey := range workflowadapter.CodeEdgeEvaluatorChildStageOrder() {
		budget, present := definition.Profile.Budget(stageKey)
		if !present || budget.TurnTimeout != 110*time.Minute || budget.MaxTurns != 1 || budget.AttemptTimeout != 120*time.Minute || budget.MaxAttempts != 1 || budget.MaxElapsed != 120*time.Minute || budget.IdleTimeout != 0 || budget.StartupGrace != 5*time.Minute || budget.ShutdownGrace != 5*time.Minute || len(budget.Backoff.RetryDelays) != 0 {
			t.Fatalf("lock-owned evaluator budget %q = %#v", stageKey, budget)
		}
	}
	if len(definition.ExecutionSpec.Stages) != 2 {
		t.Fatalf("lock-owned evaluator stage bindings = %d, want two", len(definition.ExecutionSpec.Stages))
	}
	qwen, err := definition.ExecutionSpec.ResolveStageOperation(workflowkit.StageKey(workflowadapter.HarborRunQwen))
	if err != nil {
		t.Fatalf("resolve lock-owned Qwen binding: %v", err)
	}
	opus, err := definition.ExecutionSpec.ResolveStageOperation(workflowkit.StageKey(workflowadapter.HarborRunOpus))
	if err != nil {
		t.Fatalf("resolve lock-owned Opus binding: %v", err)
	}
	if qwen.Operation.OperationID != "codeedge.qwen.pass-at-four" || opus.Operation.OperationID != "codeedge.opus.pass-at-four" || qwen.Operation.Payload.(workflowadapter.LocalCommandOperationPayload).CommandID != stageprovider.HarborEvaluatorQwenCommandID || opus.Operation.Payload.(workflowadapter.LocalCommandOperationPayload).CommandID != stageprovider.HarborEvaluatorOpusCommandID {
		t.Fatalf("lock-owned evaluator operations = %#v / %#v", qwen.Operation, opus.Operation)
	}

	// The parent profile is supplied only so the application boundary can prove
	// parent approval. The deployment provider must never mine it for evaluator
	// budgets or other child definition fields.
	request.ParentProfile = workflowadapter.ExecutionProfile{}
	again, err := provider.DefinitionForEvaluatorRun(context.Background(), request)
	if err != nil {
		t.Fatalf("build evaluator definition with irrelevant parent profile: %v", err)
	}
	if !reflect.DeepEqual(again.Profile, definition.Profile) || !reflect.DeepEqual(again.ExecutionSpec, definition.ExecutionSpec) {
		t.Fatal("evaluator definition changed with parent profile; child must be lock-owned")
	}

	withoutBudget := lock.Clone()
	withoutBudget.CodeEdgeEvaluatorChildExecutionProfile = nil
	if _, err := newCodeEdgeEvaluatorRunDefinitionProvider(withoutBudget); err == nil {
		t.Fatal("definition provider accepted a lock without the complete child-owned execution profile")
	}
	baselineFingerprint, err := lock.Fingerprint()
	if err != nil {
		t.Fatalf("fingerprint evaluator lock: %v", err)
	}
	budgetDrift := lock.Clone()
	budgetDrift.CodeEdgeEvaluatorChildExecutionProfile.Profile.ControlGracePeriod += time.Second
	if err := budgetDrift.Validate(); err != nil {
		t.Fatalf("validate profile-only budget drift fixture: %v", err)
	}
	driftedFingerprint, err := budgetDrift.Fingerprint()
	if err != nil {
		t.Fatalf("fingerprint profile-only budget drift: %v", err)
	}
	if driftedFingerprint == baselineFingerprint {
		t.Fatal("child-owned execution budget is absent from deployment lock fingerprint")
	}
}

func TestCodeEdgeProductionRuntimeAttestationAgainstLocalApprovedEnvironment(t *testing.T) {
	if os.Getenv("HARBOR_FACTORY_RUN_LOCAL_PRODUCTION_ATTESTATION") != "1" {
		t.Skip("set HARBOR_FACTORY_RUN_LOCAL_PRODUCTION_ATTESTATION=1 to attest the locally pinned Harbor installation")
	}
	if _, err := os.Lstat(sourceCodeEdgeEvaluatorProductionLockPath(t)); err != nil {
		t.Skip("generate the ignored local evaluator deployment lock before attesting the real production environment")
	}
	catalogPath, lockPath := sourceCodeEdgeEvaluatorProductionCatalogPath(t), sourceCodeEdgeEvaluatorProductionLockPath(t)
	catalog, err := stageprovider.LoadDeploymentOperationCatalogFile(catalogPath)
	if err != nil {
		t.Fatal(err)
	}
	lockRaw, err := readCodeEdgeProductionFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := stageprovider.ParseDeploymentOperationCatalogLockJSON(lockRaw)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := stageprovider.NewDeploymentOperationCatalogLockResolver(catalog, lock)
	if err != nil {
		t.Fatal(err)
	}
	attestor, err := stageprovider.NewHarborEvaluatorRuntimeAttestor(stageprovider.HarborEvaluatorRuntimeAttestorConfig{HarborFlowBuild: verifier.HarborFlowBuild()})
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range lock.Operations {
		resolution := productionAttestationResolution(record)
		locked, verifyErr := verifier.VerifyStageOperation(resolution)
		if verifyErr != nil {
			t.Fatalf("verify locked evaluator operation %q: %v", record.Stage.Key, verifyErr)
		}
		invocation, attestErr := attestor.AttestHarborEvaluatorOperation(context.Background(), stageprovider.DeploymentOperationRuntimeAttestation{
			CatalogReceipt: verifier.CatalogReceipt(), LockIdentity: verifier.LockIdentity(), HarborFlowBuild: verifier.HarborFlowBuild(),
			Record: locked, Resolution: resolution,
		})
		if attestErr != nil {
			t.Fatalf("attest locked evaluator operation %q: %v", record.Stage.Key, attestErr)
		}
		if invocation.EndpointEnvName == "" || invocation.EndpointChildEnvKey != "ANTHROPIC_BASE_URL" || len(invocation.SecretEnvTemplates) != 1 {
			t.Fatalf("attested invocation %q did not retain the approved secret-free environment mapping", record.Stage.Key)
		}
	}
}

func TestLinkedCodeEdgeProductionBuildBindingFailsClosedWhenUninjected(t *testing.T) {
	originalModule := codeEdgeProductionBuildModule
	originalVersion := codeEdgeProductionBuildVersion
	originalCommit := codeEdgeProductionBuildCommit
	originalContent := codeEdgeProductionBuildContentSHA256
	originalCatalogReceipt := codeEdgeProductionBuildCatalogReceiptFingerprint
	originalLockID := codeEdgeProductionBuildLockID
	originalLockVersion := codeEdgeProductionBuildLockVersion
	originalLockFingerprint := codeEdgeProductionBuildLockFingerprint
	codeEdgeProductionBuildModule = ""
	codeEdgeProductionBuildVersion = ""
	codeEdgeProductionBuildCommit = ""
	codeEdgeProductionBuildContentSHA256 = ""
	codeEdgeProductionBuildCatalogReceiptFingerprint = ""
	codeEdgeProductionBuildLockID = ""
	codeEdgeProductionBuildLockVersion = ""
	codeEdgeProductionBuildLockFingerprint = ""
	t.Cleanup(func() {
		codeEdgeProductionBuildModule = originalModule
		codeEdgeProductionBuildVersion = originalVersion
		codeEdgeProductionBuildCommit = originalCommit
		codeEdgeProductionBuildContentSHA256 = originalContent
		codeEdgeProductionBuildCatalogReceiptFingerprint = originalCatalogReceipt
		codeEdgeProductionBuildLockID = originalLockID
		codeEdgeProductionBuildLockVersion = originalLockVersion
		codeEdgeProductionBuildLockFingerprint = originalLockFingerprint
	})
	if _, err := linkedCodeEdgeProductionBuildBinding(); err == nil {
		t.Fatal("uninjected production build binding was accepted")
	}
}

func TestCodeEdgeProductionCompositionRejectsMaterialsThatDoNotMatchBuildBinding(t *testing.T) {
	root := t.TempDir()
	database, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	catalogPath, lockPath := testCodeEdgeProductionDeploymentPaths(t)
	binding := testCodeEdgeProductionBuildBinding(t)

	t.Run("catalog receipt", func(t *testing.T) {
		config := codeEdgeProductionCompositionConfig{
			CatalogPath:               catalogPath,
			LockPath:                  lockPath,
			HarborFlowBuild:           binding.HarborFlowBuild,
			CatalogReceiptFingerprint: workflowkit.SHA256Fingerprint([]byte("different catalog receipt")),
			LockIdentity:              binding.LockIdentity,
		}
		if _, err := newCodeEdgeProductionLifecycleServicesWithConfig(root, database, config); err == nil {
			t.Fatal("catalog receipt different from binary binding was accepted")
		}
	})

	t.Run("lock identity", func(t *testing.T) {
		config := codeEdgeProductionCompositionConfig{
			CatalogPath:               catalogPath,
			LockPath:                  lockPath,
			HarborFlowBuild:           binding.HarborFlowBuild,
			CatalogReceiptFingerprint: binding.CatalogReceiptFingerprint,
			LockIdentity: stageprovider.DeploymentOperationCatalogLockIdentity{
				LockID:      binding.LockIdentity.LockID,
				LockVersion: binding.LockIdentity.LockVersion,
				Fingerprint: workflowkit.SHA256Fingerprint([]byte("different operation lock")),
			},
		}
		if _, err := newCodeEdgeProductionLifecycleServicesWithConfig(root, database, config); err == nil {
			t.Fatal("operation lock different from binary binding was accepted")
		}
	})
}

func TestCodeEdgeProductionDeploymentPathsAreOnlyBesideExecutable(t *testing.T) {
	packageRoot := t.TempDir()
	executable := filepath.Join(packageRoot, "harbor-factory")
	if err := os.WriteFile(executable, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	deployment := filepath.Join(packageRoot, "deployments", codeEdgeProductionDeploymentDirectory)
	if err := os.MkdirAll(deployment, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{codeEdgeProductionCatalogFile, codeEdgeProductionLockFile} {
		if err := os.WriteFile(filepath.Join(deployment, name), []byte("deployment material"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	catalog, lock, err := codeEdgeProductionDeploymentPathsBesideExecutable(executable)
	if err != nil {
		t.Fatal(err)
	}
	if catalog != filepath.Join(deployment, codeEdgeProductionCatalogFile) || lock != filepath.Join(deployment, codeEdgeProductionLockFile) {
		t.Fatalf("deployment paths = %q, %q", catalog, lock)
	}

	legacyRoot := t.TempDir()
	legacyExecutable := filepath.Join(legacyRoot, "bin", "harbor-factory")
	if err := os.MkdirAll(filepath.Dir(legacyExecutable), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyExecutable, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	legacyDeployment := filepath.Join(legacyRoot, "deployments", codeEdgeProductionDeploymentDirectory)
	if err := os.MkdirAll(legacyDeployment, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{codeEdgeProductionCatalogFile, codeEdgeProductionLockFile} {
		if err := os.WriteFile(filepath.Join(legacyDeployment, name), []byte("legacy deployment material"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := codeEdgeProductionDeploymentPathsBesideExecutable(legacyExecutable); err == nil {
		t.Fatal("deployment material outside the executable package directory was accepted")
	}

	t.Run("reject deployments directory symlink", func(t *testing.T) {
		packageRoot, executable := newCodeEdgeProductionPackagePathFixture(t)
		target := filepath.Join(packageRoot, "managed-deployments", "codeedge-phase1")
		writeCodeEdgeProductionPathFixture(t, target)
		if err := os.Symlink(filepath.Dir(target), filepath.Join(packageRoot, "deployments")); err != nil {
			t.Skipf("create deployments symlink: %v", err)
		}
		if _, _, err := codeEdgeProductionDeploymentPathsBesideExecutable(executable); err == nil {
			t.Fatal("deployment directory symlink was accepted")
		}
	})

	t.Run("reject codeedge phase directory symlink", func(t *testing.T) {
		packageRoot, executable := newCodeEdgeProductionPackagePathFixture(t)
		deploymentRoot := filepath.Join(packageRoot, "deployments")
		if err := os.MkdirAll(deploymentRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "codeedge-phase1")
		writeCodeEdgeProductionPathFixture(t, outside)
		if err := os.Symlink(outside, filepath.Join(deploymentRoot, "codeedge-phase1")); err != nil {
			t.Skipf("create phase-directory symlink: %v", err)
		}
		if _, _, err := codeEdgeProductionDeploymentPathsBesideExecutable(executable); err == nil {
			t.Fatal("deployment phase directory symlink was accepted")
		}
	})

	t.Run("reject final file outside resolved executable directory", func(t *testing.T) {
		packageRoot, _ := newCodeEdgeProductionPackagePathFixture(t)
		outsideFile := filepath.Join(t.TempDir(), codeEdgeProductionCatalogFile)
		if err := os.WriteFile(outsideFile, []byte("outside deployment material"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := requireCodeEdgeProductionFileWithin("catalog", outsideFile, packageRoot); err == nil {
			t.Fatal("final deployment file outside the resolved executable directory was accepted")
		}
	})
}

func newCodeEdgeProductionPackagePathFixture(t *testing.T) (string, string) {
	t.Helper()
	packageRoot := t.TempDir()
	executable := filepath.Join(packageRoot, "harbor-factory")
	if err := os.WriteFile(executable, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return packageRoot, executable
}

func writeCodeEdgeProductionPathFixture(t *testing.T, phaseDirectory string) {
	t.Helper()
	if err := os.MkdirAll(phaseDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{codeEdgeProductionCatalogFile, codeEdgeProductionLockFile} {
		if err := os.WriteFile(filepath.Join(phaseDirectory, name), []byte("deployment material"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func setCodeEdgeProductionBuildBindingForTest(t *testing.T, binding codeEdgeProductionBuildBinding) {
	t.Helper()
	originalModule := codeEdgeProductionBuildModule
	originalVersion := codeEdgeProductionBuildVersion
	originalCommit := codeEdgeProductionBuildCommit
	originalContent := codeEdgeProductionBuildContentSHA256
	originalCatalogReceipt := codeEdgeProductionBuildCatalogReceiptFingerprint
	originalLockID := codeEdgeProductionBuildLockID
	originalLockVersion := codeEdgeProductionBuildLockVersion
	originalLockFingerprint := codeEdgeProductionBuildLockFingerprint
	codeEdgeProductionBuildModule = binding.HarborFlowBuild.Module
	codeEdgeProductionBuildVersion = binding.HarborFlowBuild.Version
	codeEdgeProductionBuildCommit = binding.HarborFlowBuild.Commit
	codeEdgeProductionBuildContentSHA256 = string(binding.HarborFlowBuild.ContentSHA256)
	codeEdgeProductionBuildCatalogReceiptFingerprint = string(binding.CatalogReceiptFingerprint)
	codeEdgeProductionBuildLockID = binding.LockIdentity.LockID
	codeEdgeProductionBuildLockVersion = binding.LockIdentity.LockVersion
	codeEdgeProductionBuildLockFingerprint = string(binding.LockIdentity.Fingerprint)
	t.Cleanup(func() {
		codeEdgeProductionBuildModule = originalModule
		codeEdgeProductionBuildVersion = originalVersion
		codeEdgeProductionBuildCommit = originalCommit
		codeEdgeProductionBuildContentSHA256 = originalContent
		codeEdgeProductionBuildCatalogReceiptFingerprint = originalCatalogReceipt
		codeEdgeProductionBuildLockID = originalLockID
		codeEdgeProductionBuildLockVersion = originalLockVersion
		codeEdgeProductionBuildLockFingerprint = originalLockFingerprint
	})
}

func testCodeEdgeProductionDeploymentPaths(t *testing.T) (string, string) {
	t.Helper()
	sourceCatalog := sourceCodeEdgeEvaluatorProductionCatalogPath(t)
	raw, err := readCodeEdgeProductionFile(sourceCatalog)
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
	lock := testCodeEdgeEvaluatorDeploymentLock(t, catalog)
	lockRaw, err := lock.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(t.TempDir(), codeEdgeProductionDeploymentDirectory)
	catalogPath := filepath.Join(directory, codeEdgeProductionCatalogFile)
	lockPath := filepath.Join(directory, codeEdgeProductionLockFile)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(catalogPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, lockRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	return catalogPath, lockPath
}

func sourceCodeEdgeEvaluatorProductionCatalogPath(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate production composition test source")
	}
	return filepath.Join(filepath.Dir(filepath.Dir(source)), "deployments", codeEdgeProductionDeploymentDirectory, codeEdgeProductionCatalogFile)
}

func sourceCodeEdgeEvaluatorProductionLockPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(filepath.Dir(sourceCodeEdgeEvaluatorProductionCatalogPath(t)), codeEdgeProductionLockFile)
}

// testCodeEdgeEvaluatorDeploymentLock keeps ordinary unit tests independent
// of a machine-generated deployment lock. Production packages instead load
// the ignored real lock generated from the local Harbor installation.
func testCodeEdgeEvaluatorDeploymentLock(t *testing.T, catalog *stageprovider.DeploymentOperationCatalogResolver) stageprovider.DeploymentOperationCatalogLock {
	t.Helper()
	build := stageprovider.HarborFlowBuildIdentity{
		Module: "github.com/purplevoid/harbor-factory", Version: "v2-test", Commit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ContentSHA256: workflowkit.SHA256Fingerprint([]byte("codeedge-evaluator-test-source")),
	}
	profile := workflowadapter.ExecutionProfile{
		Template: workflowadapter.CodeEdgeEvaluatorChildTemplateReference(), ID: "codeedge-evaluator-test-profile", Version: "1.0.0",
		ContinuationPlanTTL: workflowadapter.RequiredContinuationPlanTTL, ControlGracePeriod: time.Minute,
		CandidateProviderBudget: workflowadapter.CandidateProviderBudget{AttemptTimeout: 30 * time.Minute, StartupGrace: 5 * time.Minute, ShutdownGrace: 5 * time.Minute},
		Stages:                  make([]workflowadapter.StageBudget, 0, len(workflowadapter.CodeEdgeEvaluatorChildStageOrder())),
	}
	for _, stageKey := range workflowadapter.CodeEdgeEvaluatorChildStageOrder() {
		profile.Stages = append(profile.Stages, workflowadapter.StageBudget{StageKey: stageKey, Budget: workflowkit.ExecutionBudget{
			TurnTimeout: 110 * time.Minute, MaxTurns: 1, AttemptTimeout: 120 * time.Minute, MaxAttempts: 1, MaxElapsed: 120 * time.Minute,
			StartupGrace: 5 * time.Minute, ShutdownGrace: 5 * time.Minute, Backoff: workflowkit.BackoffPolicy{RetryDelays: []time.Duration{}},
		}})
	}
	if err := profile.Validate(); err != nil {
		t.Fatal(err)
	}
	operations := make([]stageprovider.DeploymentOperationCatalogLockRecord, 0, len(catalog.Catalog().Operations))
	for _, registration := range catalog.Catalog().Operations {
		payload, ok := registration.Operation.Payload.(workflowadapter.LocalCommandOperationPayload)
		if !ok || registration.HarborEvaluator == nil {
			t.Fatalf("evaluator fixture registration %q is not a Harbor local command", registration.Stage.Key)
		}
		launcher := stageprovider.LocalExecutableLock{CommandID: payload.CommandID, AbsolutePath: "/usr/bin/true", Version: "0.18.0-test", ContentSHA256: workflowkit.SHA256Fingerprint([]byte(payload.CommandID + " launcher"))}
		python := stageprovider.LocalExecutableLock{CommandID: stageprovider.HarborEvaluatorPythonCommandID, AbsolutePath: "/usr/bin/true", Version: "3.13.0", ContentSHA256: workflowkit.SHA256Fingerprint([]byte("python"))}
		docker := stageprovider.LocalExecutableLock{CommandID: stageprovider.HarborEvaluatorDockerCommandID, AbsolutePath: "/usr/bin/true", Version: stageprovider.HarborEvaluatorDockerVersion, ContentSHA256: workflowkit.SHA256Fingerprint([]byte("docker"))}
		contract := registration.HarborEvaluator.Clone()
		operations = append(operations, stageprovider.DeploymentOperationCatalogLockRecord{
			Stage: registration.Stage, Provider: registration.Provider, Operation: registration.Operation.Clone(), Runtime: registration.Runtime,
			Checkout: registration.Checkout, Secrets: append([]workflowadapter.SecretReference(nil), registration.Secrets...),
			PromptContentFingerprint: workflowkit.SHA256Fingerprint([]byte("fixture evaluator prompt " + string(registration.Stage.Key))),
			SchemaContentFingerprint: workflowkit.SHA256Fingerprint([]byte("fixture evaluator schema " + string(registration.Stage.Key))),
			ExecutionKind:            workflowadapter.StageOperationPayloadLocalCommand, LocalExecutable: &launcher,
			HarborEvaluator: &stageprovider.HarborEvaluatorOperationLock{
				Contract: contract, Launcher: launcher, PythonInterpreter: python,
				PythonSourceTree: stageprovider.HarborPythonSourceTreeLock{AbsolutePath: "/tmp/harbor-evaluator-test-source", PythonFilesSHA256: workflowkit.SHA256Fingerprint([]byte("harbor source"))},
				DockerCLI:        docker, HarborVersionOutput: stageprovider.HarborEvaluatorHarborVersion,
			},
		})
	}
	lock := stageprovider.DeploymentOperationCatalogLock{
		Format: stageprovider.DeploymentOperationCatalogLockFormat, Version: stageprovider.DeploymentOperationCatalogLockVersion,
		LockID: "codeedge-evaluator-test-lock", LockVersion: "1.0.0", CatalogReceipt: catalog.Receipt(), HarborFlowBuild: build,
		CodeEdgeEvaluatorChildExecutionProfile: &stageprovider.CodeEdgeEvaluatorChildExecutionProfileLock{Profile: profile}, Operations: operations,
	}
	if _, err := stageprovider.NewDeploymentOperationCatalogLockResolver(catalog, lock); err != nil {
		t.Fatal(err)
	}
	return lock
}

func testCodeEdgeProductionBuildBinding(t *testing.T) codeEdgeProductionBuildBinding {
	t.Helper()
	_, lockPath := testCodeEdgeProductionDeploymentPaths(t)
	lockRaw, err := readCodeEdgeProductionFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := stageprovider.ParseDeploymentOperationCatalogLockJSON(lockRaw)
	if err != nil {
		t.Fatal(err)
	}
	catalogReceiptFingerprint, err := lock.CatalogReceipt.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	lockFingerprint, err := lock.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	return codeEdgeProductionBuildBinding{
		HarborFlowBuild:           lock.HarborFlowBuild,
		CatalogReceiptFingerprint: catalogReceiptFingerprint,
		LockIdentity: stageprovider.DeploymentOperationCatalogLockIdentity{
			LockID: lock.LockID, LockVersion: lock.LockVersion, Fingerprint: lockFingerprint,
		},
	}
}

func productionAttestationResolution(record stageprovider.DeploymentOperationCatalogLockRecord) workflowadapter.StageOperationResolution {
	return workflowadapter.StageOperationResolution{
		StageKey:  record.Stage.Key,
		StageType: record.Stage.Type,
		Plugin:    record.Stage.Plugin,
		Provider:  record.Provider,
		Operation: record.Operation.Clone(),
		Checkout: workflowadapter.CheckoutReference{
			ID: record.Checkout.ID, RevisionID: "018f0a73-3b49-7000-8000-000000000081",
			RevisionDigest: workflowkit.SubjectDigest("harbor.task.v2:sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		},
		Runtime: record.Runtime,
		ArtifactInputs: []workflowadapter.ArtifactInputReference{{
			Port: stageprovider.HarborEvaluatorTaskArtifactPort, ArtifactID: "018f0a73-3b49-7000-8000-000000000082",
		}},
		Secrets: append([]workflowadapter.SecretReference(nil), record.Secrets...),
	}
}

func productionEvaluatorParentProfile(t *testing.T) workflowadapter.ExecutionProfile {
	t.Helper()
	catalog := workflowadapter.CodeEdgePhase1StageCatalog()
	profile := workflowadapter.ExecutionProfile{
		Template:                workflowadapter.CodeEdgePhase1TemplateReference(),
		ID:                      "parent-codeedge-profile",
		Version:                 "1",
		ContinuationPlanTTL:     workflowadapter.RequiredContinuationPlanTTL,
		ControlGracePeriod:      30 * time.Second,
		CandidateProviderBudget: workflowadapter.CandidateProviderBudget{AttemptTimeout: time.Minute},
		Stages:                  make([]workflowadapter.StageBudget, 0, len(catalog.Stages)),
	}
	for _, stage := range catalog.Stages {
		turns := max(1, stage.RequiredTurns)
		attempt := time.Duration(turns) * time.Second
		profile.Stages = append(profile.Stages, workflowadapter.StageBudget{StageKey: stage.Key, Budget: workflowkit.ExecutionBudget{
			TurnTimeout: time.Second, MaxTurns: turns, AttemptTimeout: attempt, MaxAttempts: 1, MaxElapsed: attempt,
		}})
	}
	if err := profile.Validate(); err != nil {
		t.Fatalf("build frozen parent profile fixture: %v", err)
	}
	return profile
}
