package stageprovider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestHarborEvaluatorCatalogAndLockRequireExactChildContract(t *testing.T) {
	fixture := newHarborEvaluatorAttestationFixture(t, HarborEvaluatorQwenCommandID)
	catalog, err := NewDeploymentOperationCatalogResolver(fixture.catalog)
	if err != nil {
		t.Fatalf("construct evaluator catalog: %v", err)
	}
	resolver, err := NewDeploymentOperationCatalogLockResolver(catalog, fixture.lock)
	if err != nil {
		t.Fatalf("construct evaluator catalog lock: %v", err)
	}
	if _, err := resolver.VerifyStageOperation(fixture.attestation.Resolution); err != nil {
		t.Fatalf("verify locked evaluator stage: %v", err)
	}

	canonicalCatalog, err := fixture.catalog.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	canonicalLock, err := fixture.lock.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range [][]byte{canonicalCatalog, canonicalLock} {
		if strings.Contains(string(raw), fixture.endpoint) || strings.Contains(string(raw), fixture.secretValue) {
			t.Fatalf("catalog or lock persisted endpoint or secret value: %s", raw)
		}
	}

	t.Run("catalog requires typed evaluator contract", func(t *testing.T) {
		candidate := fixture.catalog.Clone()
		candidate.Operations[0].HarborEvaluator = nil
		if err := candidate.Validate(); err == nil || !errors.Is(err, ErrInvalidDeploymentOperationCatalog) {
			t.Fatalf("untyped evaluator catalog error = %v, want invalid catalog", err)
		}
	})

	t.Run("only evaluator child template is accepted", func(t *testing.T) {
		candidate := fixture.catalog.Clone()
		candidate.Template = workflowadapter.CodeEdgePhase1TemplateReference()
		if err := candidate.Validate(); err == nil || !errors.Is(err, ErrInvalidDeploymentOperationCatalog) {
			t.Fatalf("parent template catalog error = %v, want invalid catalog", err)
		}
	})

	t.Run("provider must be an evaluation provider", func(t *testing.T) {
		candidate := fixture.catalog.Clone()
		candidate.Operations[0].Provider.Kind = "controlled"
		if err := candidate.Validate(); err == nil || !errors.Is(err, ErrInvalidDeploymentOperationCatalog) {
			t.Fatalf("provider kind catalog error = %v, want invalid catalog", err)
		}
	})

	t.Run("caller argv is forbidden", func(t *testing.T) {
		candidate := fixture.catalog.Clone()
		payload := candidate.Operations[0].Operation.Payload.(workflowadapter.LocalCommandOperationPayload)
		payload.Arguments = []string{"--jobs-dir=/tmp/escape"}
		candidate.Operations[0].Operation.Payload = payload
		if err := candidate.Validate(); err == nil || !errors.Is(err, ErrInvalidDeploymentOperationCatalog) {
			t.Fatalf("argv catalog error = %v, want invalid catalog", err)
		}
	})

	t.Run("bundle and screenshot schemas must match child stage outputs", func(t *testing.T) {
		candidate := fixture.catalog.Clone()
		candidate.Operations[0].HarborEvaluator.ScreenshotRenderer.SchemaVersion = "image/jpeg"
		if err := candidate.Validate(); err == nil || !errors.Is(err, ErrInvalidDeploymentOperationCatalog) {
			t.Fatalf("screenshot schema catalog error = %v, want invalid catalog", err)
		}
	})

	t.Run("internal retry limit is explicit and fixed", func(t *testing.T) {
		candidate := fixture.catalog.Clone()
		candidate.Operations[0].HarborEvaluator.MaxRetries = HarborEvaluatorMaxRetries - 1
		if err := candidate.Validate(); err == nil || !errors.Is(err, ErrInvalidDeploymentOperationCatalog) {
			t.Fatalf("catalog retry limit error = %v, want invalid catalog", err)
		}
		locked := fixture.lock.Clone()
		locked.Operations[0].HarborEvaluator.Contract.MaxRetries = HarborEvaluatorMaxRetries - 1
		if err := locked.Validate(); err == nil || !errors.Is(err, ErrInvalidDeploymentOperationCatalogLock) {
			t.Fatalf("lock retry limit error = %v, want invalid lock", err)
		}
	})

	t.Run("lock contract must exactly match catalog", func(t *testing.T) {
		candidate := fixture.lock.Clone()
		candidate.Operations[0].HarborEvaluator.Contract.ModelVersion = "model-v2"
		if _, err := NewDeploymentOperationCatalogLockResolver(catalog, candidate); err == nil || !errors.Is(err, ErrDeploymentOperationCatalogLockDrift) {
			t.Fatalf("lock contract drift error = %v, want lock drift", err)
		}
	})

	t.Run("host environment mapping is lock significant", func(t *testing.T) {
		candidate := fixture.lock.Clone()
		candidate.Operations[0].HarborEvaluator.Contract.SecretEnvTemplates[0].HostEnvKey = "OTHER_MODEL_API_KEY"
		if _, err := NewDeploymentOperationCatalogLockResolver(catalog, candidate); err == nil || !errors.Is(err, ErrDeploymentOperationCatalogLockDrift) {
			t.Fatalf("host environment mapping drift error = %v, want lock drift", err)
		}
	})

	t.Run("host environment mapping must be explicit", func(t *testing.T) {
		candidate := fixture.catalog.Clone()
		candidate.Operations[0].HarborEvaluator.SecretEnvTemplates[0].HostEnvKey = ""
		if err := candidate.Validate(); err == nil || !errors.Is(err, ErrInvalidDeploymentOperationCatalog) {
			t.Fatalf("missing host environment key error = %v, want invalid catalog", err)
		}
	})

	t.Run("launcher must be duplicated exactly", func(t *testing.T) {
		candidate := fixture.lock.Clone()
		candidate.Operations[0].HarborEvaluator.Launcher.Version = "other-v1"
		if err := candidate.Validate(); err == nil || !errors.Is(err, ErrInvalidDeploymentOperationCatalogLock) {
			t.Fatalf("launcher duplicate error = %v, want invalid lock", err)
		}
	})

	t.Run("secret template cannot become an interpolation language", func(t *testing.T) {
		candidate := fixture.catalog.Clone()
		candidate.Operations[0].HarborEvaluator.SecretEnvTemplates[0].Template = "Bearer {{secret}}"
		if err := candidate.Validate(); err == nil || !errors.Is(err, ErrInvalidDeploymentOperationCatalog) {
			t.Fatalf("secret template catalog error = %v, want invalid catalog", err)
		}
	})
}

func TestHarborEvaluatorRuntimeAttestorProvesLocalInstallationWithoutSecretPersistence(t *testing.T) {
	fixture := newHarborEvaluatorAttestationFixture(t, HarborEvaluatorQwenCommandID)
	t.Setenv(fixture.contract.EndpointEnvName, fixture.endpoint)
	attestor, err := NewHarborEvaluatorRuntimeAttestor(HarborEvaluatorRuntimeAttestorConfig{HarborFlowBuild: fixture.attestation.HarborFlowBuild})
	if err != nil {
		t.Fatal(err)
	}
	invocation, err := attestor.AttestHarborEvaluatorOperation(context.Background(), fixture.attestation)
	if err != nil {
		t.Fatalf("attest evaluator installation: %v", err)
	}
	if invocation.CommandID != HarborEvaluatorQwenCommandID || invocation.Attempts != 4 || invocation.ConcurrentTrials != 1 || invocation.MaxRetries != HarborEvaluatorMaxRetries || !invocation.RequireTrajectory {
		t.Fatalf("invocation = %+v, want fixed Qwen serial pass@4 contract", invocation)
	}
	if invocation.LauncherPath != fixture.launcher || invocation.PythonInterpreterPath != fixture.interpreter || invocation.PythonSourceTreePath != fixture.sourceTree || invocation.DockerCLIPath != fixture.docker || invocation.DockerVersion != HarborEvaluatorDockerVersion {
		t.Fatalf("invocation paths = %+v, want locked paths", invocation)
	}
	if invocation.EndpointEnvName != fixture.contract.EndpointEnvName || invocation.EndpointFingerprint != fixture.contract.EndpointFingerprint {
		t.Fatalf("invocation endpoint identity = %+v, want fingerprint-only identity", invocation)
	}
	encoded, err := json.Marshal(invocation)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), fixture.endpoint) || strings.Contains(string(encoded), fixture.secretValue) {
		t.Fatalf("secret-free invocation unexpectedly persisted endpoint or secret value: %s", encoded)
	}
	if err := attestor.AttestDeploymentOperation(context.Background(), fixture.attestation); err != nil {
		t.Fatalf("generic attestor boundary rejected exact evaluator operation: %v", err)
	}
}

func TestHarborEvaluatorRuntimeAttestorFailsClosedOnRuntimeDrift(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, fixture *harborEvaluatorAttestationFixture)
	}{
		{
			name: "endpoint fingerprint drift",
			mutate: func(t *testing.T, fixture *harborEvaluatorAttestationFixture) {
				t.Setenv(fixture.contract.EndpointEnvName, "https://other.example/v1")
			},
		},
		{
			name: "launcher file drift",
			mutate: func(t *testing.T, fixture *harborEvaluatorAttestationFixture) {
				writeHarborEvaluatorTestFile(t, fixture.launcher, "#!"+fixture.interpreter+"\nprintf '0.18.0\\n'\n", 0o700)
			},
		},
		{
			name: "python source tree drift",
			mutate: func(t *testing.T, fixture *harborEvaluatorAttestationFixture) {
				writeHarborEvaluatorTestFile(t, filepath.Join(fixture.sourceTree, "cli.py"), "VALUE = 'changed'\n", 0o600)
			},
		},
		{
			name: "Docker version output drift after matching file hash",
			mutate: func(t *testing.T, fixture *harborEvaluatorAttestationFixture) {
				contents := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then printf 'Docker version 29.5.1, build controlled\\n'; exit 0; fi\nexit 1\n"
				writeHarborEvaluatorTestFile(t, fixture.docker, contents, 0o700)
				fixture.attestation.Record.HarborEvaluator.DockerCLI.ContentSHA256 = workflowkit.SHA256Fingerprint([]byte(contents))
			},
		},
		{
			name: "launcher version output drift after matching file hash",
			mutate: func(t *testing.T, fixture *harborEvaluatorAttestationFixture) {
				contents := "#!" + fixture.interpreter + "\nif [ \"$1\" = \"--version\" ]; then printf '0.17.0\\n'; exit 0; fi\nexit 1\n"
				writeHarborEvaluatorTestFile(t, fixture.launcher, contents, 0o700)
				fingerprint := workflowkit.SHA256Fingerprint([]byte(contents))
				fixture.attestation.Record.LocalExecutable.ContentSHA256 = fingerprint
				fixture.attestation.Record.HarborEvaluator.Launcher.ContentSHA256 = fingerprint
			},
		},
		{
			name: "launcher shebang interpreter drift after matching file hash",
			mutate: func(t *testing.T, fixture *harborEvaluatorAttestationFixture) {
				otherInterpreter := filepath.Join(filepath.Dir(fixture.interpreter), "other-python")
				writeHarborEvaluatorTestFile(t, otherInterpreter, "#!/bin/sh\nexec /bin/sh \"$@\"\n", 0o700)
				contents := "#!" + otherInterpreter + "\nif [ \"$1\" = \"--version\" ]; then printf '0.18.0\\n'; exit 0; fi\nexit 1\n"
				writeHarborEvaluatorTestFile(t, fixture.launcher, contents, 0o700)
				fingerprint := workflowkit.SHA256Fingerprint([]byte(contents))
				fixture.attestation.Record.LocalExecutable.ContentSHA256 = fingerprint
				fixture.attestation.Record.HarborEvaluator.Launcher.ContentSHA256 = fingerprint
			},
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			fixture := newHarborEvaluatorAttestationFixture(t, HarborEvaluatorQwenCommandID)
			t.Setenv(fixture.contract.EndpointEnvName, fixture.endpoint)
			test.mutate(t, fixture)
			attestor, err := NewHarborEvaluatorRuntimeAttestor(HarborEvaluatorRuntimeAttestorConfig{HarborFlowBuild: fixture.attestation.HarborFlowBuild})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := attestor.AttestHarborEvaluatorOperation(context.Background(), fixture.attestation); err == nil || !errors.Is(err, ErrDeploymentOperationRuntimeAttestationFailed) {
				t.Fatalf("runtime drift error = %v, want runtime attestation failed", err)
			}
		})
	}
}

func TestComputeHarborPythonSourceTreeFingerprintUsesObservedManifest(t *testing.T) {
	root := filepath.Join(t.TempDir(), "harbor")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	writeHarborEvaluatorTestFile(t, filepath.Join(root, "z.py"), "z = 1\n", 0o600)
	if err := os.Mkdir(filepath.Join(root, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeHarborEvaluatorTestFile(t, filepath.Join(root, "nested", "a.py"), "a = 1\n", 0o600)
	writeHarborEvaluatorTestFile(t, filepath.Join(root, "ignored.txt"), "ignored\n", 0o600)

	fingerprint, err := ComputeHarborPythonSourceTreeFingerprint(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	first := workflowkit.SHA256Fingerprint([]byte("a = 1\n"))
	second := workflowkit.SHA256Fingerprint([]byte("z = 1\n"))
	manifest := strings.TrimPrefix(string(first), "sha256:") + "  " + filepath.Join(root, "nested", "a.py") + "\n" +
		strings.TrimPrefix(string(second), "sha256:") + "  " + filepath.Join(root, "z.py") + "\n"
	want := workflowkit.SHA256Fingerprint([]byte(manifest))
	if fingerprint != want {
		t.Fatalf("source tree fingerprint = %q, want observed sha256sum manifest digest %q", fingerprint, want)
	}
}

func TestCanonicalHarborEvaluatorEndpointFingerprintRejectsCredentialsAndNormalizesPath(t *testing.T) {
	first, err := CanonicalHarborEvaluatorEndpointFingerprint("https://MODEL.example/v1/")
	if err != nil {
		t.Fatal(err)
	}
	second, err := CanonicalHarborEvaluatorEndpointFingerprint("https://model.example/v1")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("normalized endpoint fingerprints differ: %q vs %q", first, second)
	}
	if _, err := CanonicalHarborEvaluatorEndpointFingerprint("https://token@model.example/v1"); err == nil || !errors.Is(err, ErrInvalidDeploymentOperationCatalogLock) {
		t.Fatalf("credential-bearing endpoint error = %v, want invalid lock", err)
	}
}

type harborEvaluatorAttestationFixture struct {
	catalog     DeploymentOperationCatalog
	lock        DeploymentOperationCatalogLock
	attestation DeploymentOperationRuntimeAttestation
	contract    HarborEvaluatorOperationContract
	endpoint    string
	secretValue string
	launcher    string
	interpreter string
	sourceTree  string
	docker      string
}

func newHarborEvaluatorAttestationFixture(t *testing.T, commandID string) *harborEvaluatorAttestationFixture {
	t.Helper()
	stageKey, stageType, operationID := harborEvaluatorFixtureStage(commandID)
	definition, found := workflowadapter.CodeEdgeEvaluatorChildStageCatalog().Stage(stageKey)
	if !found {
		t.Fatalf("missing evaluator child stage %q", stageKey)
	}
	root := t.TempDir()
	interpreter := filepath.Join(root, "python")
	writeHarborEvaluatorTestFile(t, interpreter, "#!/bin/sh\nexec /bin/sh \"$@\"\n", 0o700)
	launcher := filepath.Join(root, "harbor")
	launcherContents := "#!" + interpreter + "\nif [ \"$1\" = \"--version\" ]; then printf '0.18.0\\n'; exit 0; fi\nexit 1\n"
	writeHarborEvaluatorTestFile(t, launcher, launcherContents, 0o700)
	docker := filepath.Join(root, "docker")
	dockerContents := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then printf 'Docker version 29.5.2, build controlled\\n'; exit 0; fi\nexit 1\n"
	writeHarborEvaluatorTestFile(t, docker, dockerContents, 0o700)
	sourceTree := filepath.Join(root, "site-packages", "harbor")
	if err := os.MkdirAll(filepath.Join(sourceTree, "cli"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeHarborEvaluatorTestFile(t, filepath.Join(sourceTree, "__init__.py"), "VERSION = '0.18.0'\n", 0o600)
	writeHarborEvaluatorTestFile(t, filepath.Join(sourceTree, "cli", "main.py"), "def main():\n    return 0\n", 0o600)
	sourceFingerprint, err := ComputeHarborPythonSourceTreeFingerprint(context.Background(), sourceTree)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "https://model.example/v1"
	endpointFingerprint, err := CanonicalHarborEvaluatorEndpointFingerprint(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	secret := workflowadapter.SecretReference{ID: "secret-model-token", Provider: "environment", Version: "2026-07-14"}
	contract := HarborEvaluatorOperationContract{
		Format: HarborEvaluatorOperationContractFormat, Version: HarborEvaluatorOperationContractVersion,
		HarborVersion: HarborEvaluatorHarborVersion, ResultABIFormat: HarborEvaluatorResultABIFormat, ResultABIVersion: HarborEvaluatorResultABIVersion,
		TaskArtifactPort: HarborEvaluatorTaskArtifactPort, TaskArtifactSchema: HarborEvaluatorTaskArtifactSchema,
		AgentID: "claude-code", AgentVersion: "2.1.207", ModelID: "approved-model", ModelVersion: "2026-07-14",
		EndpointEnvName: "MODEL_ENDPOINT", EndpointChildEnvKey: "ANTHROPIC_BASE_URL", EndpointFingerprint: endpointFingerprint,
		SecretEnvTemplates: []HarborEvaluatorSecretEnvTemplate{{Secret: secret, HostEnvKey: "MODEL_API_KEY", ChildEnvKey: "MODEL_API_KEY", Template: HarborEvaluatorSecretValueTemplate}},
		Attempts:           HarborEvaluatorTrialCount, ConcurrentTrials: HarborEvaluatorConcurrentTrials, MaxRetries: HarborEvaluatorMaxRetries, RequireTrajectory: true,
		ScreenshotRenderer: HarborEvaluatorScreenshotRenderer{ID: "harbor-terminal-png", Version: "1", SchemaVersion: workflowadapter.CodeEdgeEvaluatorScreenshotSchemaVersion},
	}
	registration := DeploymentOperationRegistration{
		Stage:     DeploymentStageContract{Key: stageKey, Type: stageType, Group: definition.Group, Plugin: workflowkit.PluginBinding{ID: definition.Plugin.ID, Version: definition.Plugin.Version}},
		Provider:  workflowadapter.ProviderReference{ID: "provider-codeedge-evaluator", Kind: "evaluation", Version: "1"},
		Operation: workflowadapter.StageOperationBinding{ProviderID: "provider-codeedge-evaluator", OperationID: operationID, Version: "1", Payload: workflowadapter.LocalCommandOperationPayload{CommandID: commandID, Arguments: []string{}}},
		Runtime:   workflowadapter.RuntimeReference{ID: "runtime-codeedge-evaluator", Kind: "controlled", Version: "1"},
		Checkout:  DeploymentCheckoutContract{ID: "checkout-codeedge-evaluator", Purpose: "isolated-evaluator"},
		Secrets:   []workflowadapter.SecretReference{secret}, HarborEvaluator: &contract,
	}
	catalogDocument := DeploymentOperationCatalog{
		Format: DeploymentOperationCatalogFormat, Version: DeploymentOperationCatalogVersion,
		CatalogID: "codeedge-evaluator-test", CatalogVersion: "test-v1", Template: workflowadapter.CodeEdgeEvaluatorChildTemplateReference(),
		Operations: []DeploymentOperationRegistration{registration},
	}
	catalog, err := NewDeploymentOperationCatalogResolver(catalogDocument)
	if err != nil {
		t.Fatal(err)
	}
	launcherLock := LocalExecutableLock{CommandID: commandID, AbsolutePath: launcher, Version: "0.18.0-launcher", ContentSHA256: workflowkit.SHA256Fingerprint([]byte(launcherContents))}
	pythonContents := "#!/bin/sh\nexec /bin/sh \"$@\"\n"
	pythonLock := LocalExecutableLock{CommandID: HarborEvaluatorPythonCommandID, AbsolutePath: interpreter, Version: "3.13.0", ContentSHA256: workflowkit.SHA256Fingerprint([]byte(pythonContents))}
	dockerLock := LocalExecutableLock{CommandID: HarborEvaluatorDockerCommandID, AbsolutePath: docker, Version: HarborEvaluatorDockerVersion, ContentSHA256: workflowkit.SHA256Fingerprint([]byte(dockerContents))}
	record := DeploymentOperationCatalogLockRecord{
		Stage: registration.Stage, Provider: registration.Provider, Operation: registration.Operation.Clone(), Runtime: registration.Runtime,
		Checkout: registration.Checkout, Secrets: cloneDeploymentSecrets(registration.Secrets),
		PromptContentFingerprint: workflowkit.SHA256Fingerprint([]byte("evaluator-prompt")), SchemaContentFingerprint: workflowkit.SHA256Fingerprint([]byte("evaluator-schema")),
		ExecutionKind: workflowadapter.StageOperationPayloadLocalCommand, LocalExecutable: &launcherLock,
		HarborEvaluator: &HarborEvaluatorOperationLock{
			Contract: contract, Launcher: launcherLock, PythonInterpreter: pythonLock,
			PythonSourceTree: HarborPythonSourceTreeLock{AbsolutePath: sourceTree, PythonFilesSHA256: sourceFingerprint}, DockerCLI: dockerLock, HarborVersionOutput: HarborEvaluatorHarborVersion,
		},
	}
	lock := DeploymentOperationCatalogLock{
		Format: DeploymentOperationCatalogLockFormat, Version: DeploymentOperationCatalogLockVersion,
		LockID: "codeedge-evaluator-test", LockVersion: "test-v1", CatalogReceipt: catalog.Receipt(), HarborFlowBuild: runtimeAttestorBuildIdentity(),
		Operations: []DeploymentOperationCatalogLockRecord{record},
	}
	verifier, err := NewDeploymentOperationCatalogLockResolver(catalog, lock)
	if err != nil {
		t.Fatal(err)
	}
	resolution := workflowadapter.StageOperationResolution{
		StageKey: stageKey, StageType: stageType, Plugin: registration.Stage.Plugin, Provider: registration.Provider, Operation: registration.Operation.Clone(),
		Checkout: workflowadapter.CheckoutReference{ID: registration.Checkout.ID, RevisionID: "018f0a73-3b49-7000-8000-000000000091", RevisionDigest: workflowkit.SubjectDigest("harbor.task.v2:sha256:" + strings.Repeat("a", 64))},
		Runtime:  registration.Runtime, ArtifactInputs: []workflowadapter.ArtifactInputReference{{Port: HarborEvaluatorTaskArtifactPort, ArtifactID: "018f0a73-3b49-7000-8000-000000000092"}},
		Secrets: []workflowadapter.SecretReference{secret},
	}
	return &harborEvaluatorAttestationFixture{
		catalog: catalogDocument.Clone(), lock: lock.Clone(), contract: contract.Clone(), endpoint: endpoint, secretValue: "super-secret-never-persisted",
		launcher: launcher, interpreter: interpreter, sourceTree: sourceTree, docker: docker,
		attestation: DeploymentOperationRuntimeAttestation{CatalogReceipt: verifier.CatalogReceipt(), LockIdentity: verifier.LockIdentity(), HarborFlowBuild: verifier.HarborFlowBuild(), Record: record.Clone(), Resolution: resolution},
	}
}

func harborEvaluatorFixtureStage(commandID string) (workflowkit.StageKey, workflowadapter.StageBindingType, string) {
	switch commandID {
	case HarborEvaluatorQwenCommandID:
		return workflowkit.StageKey(workflowadapter.HarborRunQwen), workflowadapter.StageBindingHarborRunQwen, "codeedge.qwen.pass-at-four"
	case HarborEvaluatorOpusCommandID:
		return workflowkit.StageKey(workflowadapter.HarborRunOpus), workflowadapter.StageBindingHarborRunOpus, "codeedge.opus.pass-at-four"
	default:
		panic("unsupported Harbor evaluator test command")
	}
}

func writeHarborEvaluatorTestFile(t *testing.T, filePath, contents string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(filePath, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
}
