package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	legacyEvaluatorPromptFingerprint = "sha256:19b355c61b0c8b47ddab5b98155a1933285f2469d20e40b84acb135428ae4d2d"
	legacyEvaluatorSchemaFingerprint = "sha256:6a04d660c28b0eb37dfa18d7348da483943993f8c1e6119a449ede71c55a1417"
)

func TestBuildCreatesAttestedEvaluatorLockFromAssetsAndControlledProbes(t *testing.T) {
	fixture := newEvaluatorLockGeneratorFixture(t)
	lock, err := build(fixture.config())
	if err != nil {
		t.Fatalf("build evaluator lock: %v", err)
	}
	catalogRaw, err := os.ReadFile(fixture.catalogPath)
	if err != nil {
		t.Fatal(err)
	}
	catalogDocument, err := stageprovider.ParseDeploymentOperationCatalogJSON(catalogRaw)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := stageprovider.NewDeploymentOperationCatalogResolver(catalogDocument)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stageprovider.NewDeploymentOperationCatalogLockResolver(catalog, lock); err != nil {
		t.Fatalf("generated lock does not validate: %v", err)
	}
	if lock.CodeEdgeEvaluatorChildExecutionProfile == nil {
		t.Fatal("generated evaluator lock omitted its complete child-owned execution profile")
	}
	if lock.HarborFlowBuild.Module != modulePath || lock.HarborFlowBuild.Version != "v2.0.0" {
		t.Fatalf("generated build identity = %#v", lock.HarborFlowBuild)
	}
	if len(lock.Operations) != 2 {
		t.Fatalf("generated operation count = %d, want two", len(lock.Operations))
	}
	prompt, err := os.ReadFile(filepath.Join(fixture.contractRoot, "contracts", "harbor-pass-at-four.v0.18.json"))
	if err != nil {
		t.Fatal(err)
	}
	schema, err := os.ReadFile(filepath.Join(fixture.contractRoot, "schemas", "harbor-run-bundle.v0.18.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range lock.Operations {
		if record.Secrets == nil || len(record.Secrets) != 1 {
			t.Fatalf("record %q did not retain its explicit secret allow-list: %#v", record.Stage.Key, record.Secrets)
		}
		if record.PromptContentFingerprint != workflowkit.SHA256Fingerprint(prompt) || record.SchemaContentFingerprint != workflowkit.SHA256Fingerprint(schema) {
			t.Fatalf("record %q did not bind source-owned evaluator assets: %#v", record.Stage.Key, record)
		}
		if record.PromptContentFingerprint == workflowkit.Fingerprint(legacyEvaluatorPromptFingerprint) || record.SchemaContentFingerprint == workflowkit.Fingerprint(legacyEvaluatorSchemaFingerprint) {
			t.Fatalf("record %q retained a legacy magic asset fingerprint", record.Stage.Key)
		}
		if record.HarborEvaluator == nil || record.LocalExecutable == nil || record.HarborEvaluator.Launcher != *record.LocalExecutable {
			t.Fatalf("record %q did not duplicate the frozen Harbor launcher", record.Stage.Key)
		}
		evaluator := record.HarborEvaluator
		contract := evaluator.Contract
		if evaluator.DockerCLI.AbsolutePath != fixture.dockerCLI || filepath.Base(evaluator.DockerCLI.AbsolutePath) != "docker" || evaluator.DockerServerVersion != stageprovider.HarborEvaluatorDockerServerVersion ||
			evaluator.ClaudeCodeExecutable.AbsolutePath != fixture.claudeCodeExecutable || evaluator.ClaudeCodeExecutable.CommandID != stageprovider.HarborEvaluatorClaudeCodeCommandID || evaluator.ClaudeCodeExecutable.Version != contract.AgentVersion ||
			evaluator.DockerComposePlugin.AbsolutePath != fixture.dockerComposePlugin || evaluator.DockerComposePlugin.Version != stageprovider.HarborEvaluatorDockerComposeVersion || evaluator.DockerComposeVersionOutput != stageprovider.HarborEvaluatorDockerComposeVersionOutput ||
			evaluator.DockerBuildxPlugin.AbsolutePath != fixture.dockerBuildxPlugin || evaluator.DockerBuildxPlugin.Version != stageprovider.HarborEvaluatorDockerBuildxVersion || evaluator.DockerBuildxVersionOutput != stageprovider.HarborEvaluatorDockerBuildxVersionOutput {
			t.Fatalf("record %q did not freeze the complete Docker runtime: %#v", record.Stage.Key, evaluator)
		}
		if contract.Attempts != 4 || contract.ConcurrentTrials != 1 || contract.MaxRetries != 3 || !contract.RequireTrajectory {
			t.Fatalf("record %q lost the fixed pass@4/retry contract: %#v", record.Stage.Key, contract)
		}
	}
	canonical, err := lock.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{fixture.environment[qwenEndpointEnvironment], fixture.environment[opusEndpointEnvironment], fixture.environment[qwenCredentialEnvironment], fixture.environment[opusCredentialEnvironment]} {
		if strings.Contains(string(canonical), value) {
			t.Fatalf("generated lock persisted an environment value")
		}
	}
	if _, err := os.Stat(fixture.nonVersionProbeMarker); !os.IsNotExist(err) {
		t.Fatalf("lock generation invoked Harbor outside --version probing: %v", err)
	}
	for _, marker := range []string{
		fixture.dockerServerProbeMarker,
		fixture.dockerComposeInfoProbeMarker,
		fixture.dockerBuildxInfoProbeMarker,
		fixture.dockerComposeProbeMarker,
		fixture.dockerBuildxProbeMarker,
	} {
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("lock generation omitted a required controlled Docker probe: %v", err)
		}
	}
	if got := fixture.lookedUpNames(); !sameStrings(got, []string{opusCredentialEnvironment, opusEndpointEnvironment, qwenCredentialEnvironment, qwenEndpointEnvironment}) {
		t.Fatalf("environment names read = %v, want exactly approved names", got)
	}
}

func TestValidateConfigRequiresDockerPluginBasenames(t *testing.T) {
	fixture := newEvaluatorLockGeneratorFixture(t)
	config := fixture.config()
	config.dockerCLI = fixture.pythonInterpreter
	if err := validateConfig(&config); err == nil || !strings.Contains(err.Error(), "Docker CLI basename") {
		t.Fatalf("renamed Docker CLI error = %v, want fixed basename", err)
	}

	config = fixture.config()
	config.dockerComposePlugin = fixture.pythonInterpreter
	if err := validateConfig(&config); err == nil || !strings.Contains(err.Error(), "Docker Compose plugin basename") {
		t.Fatalf("renamed Compose plugin error = %v, want fixed basename", err)
	}

	config = fixture.config()
	config.dockerBuildxPlugin = fixture.pythonInterpreter
	if err := validateConfig(&config); err == nil || !strings.Contains(err.Error(), "Docker Buildx plugin basename") {
		t.Fatalf("renamed Buildx plugin error = %v, want fixed basename", err)
	}
}

func TestBuildRequiresTheContractClaudeCodeVersion(t *testing.T) {
	fixture := newEvaluatorLockGeneratorFixture(t)
	writeGeneratorFile(t, fixture.claudeCodeExecutable, "#!/bin/sh\nif [ \"${1:-}\" = \"--version\" ]; then printf '2.1.206 (Claude Code)\\n'; exit 0; fi\nexit 1\n", 0o700)
	testGit(t, fixture.root, fixture.gitExecutable, "add", "runtime/claude")
	testGit(t, fixture.root, fixture.gitExecutable, "commit", "-m", "change Claude version")
	if _, err := build(fixture.config()); err == nil || !strings.Contains(err.Error(), "does not match evaluator agent version") {
		t.Fatalf("Claude Code version mismatch error = %v", err)
	}
}

func TestDiscoverEvaluatorRuntimeRejectsMalformedClaudeCodeVersion(t *testing.T) {
	fixture := newEvaluatorLockGeneratorFixture(t)
	writeGeneratorFile(t, fixture.claudeCodeExecutable, "#!/bin/sh\nif [ \"${1:-}\" = \"--version\" ]; then printf 'claude 2.1.207\\n'; exit 0; fi\nexit 1\n", 0o700)
	if _, err := discoverEvaluatorRuntime(fixture.config()); err == nil || !strings.Contains(err.Error(), "Claude Code --version") {
		t.Fatalf("malformed Claude Code version error = %v", err)
	}
}

func TestDiscoverEvaluatorRuntimeRejectsPluginAndDaemonDrift(t *testing.T) {
	t.Run("Compose version", func(t *testing.T) {
		fixture := newEvaluatorLockGeneratorFixture(t)
		config := fixture.dockerScriptConfig()
		config.composeOutput = "Docker Compose version v5.1.2"
		writeGeneratorFile(t, fixture.dockerCLI, evaluatorGeneratorDockerScript(config), 0o700)
		if _, err := discoverEvaluatorRuntime(fixture.config()); err == nil || !strings.Contains(err.Error(), "Compose version") {
			t.Fatalf("Compose version drift error = %v", err)
		}
	})

	t.Run("Buildx exact output", func(t *testing.T) {
		fixture := newEvaluatorLockGeneratorFixture(t)
		config := fixture.dockerScriptConfig()
		config.buildxOutput = "github.com/docker/buildx v0.33.0 " + strings.Repeat("0", 40)
		writeGeneratorFile(t, fixture.dockerCLI, evaluatorGeneratorDockerScript(config), 0o700)
		if _, err := discoverEvaluatorRuntime(fixture.config()); err == nil || !strings.Contains(err.Error(), "Buildx version") {
			t.Fatalf("Buildx exact-output drift error = %v", err)
		}
	})

	t.Run("server version", func(t *testing.T) {
		fixture := newEvaluatorLockGeneratorFixture(t)
		config := fixture.dockerScriptConfig()
		config.serverVersion = "29.4.0"
		writeGeneratorFile(t, fixture.dockerCLI, evaluatorGeneratorDockerScript(config), 0o700)
		if _, err := discoverEvaluatorRuntime(fixture.config()); err == nil || !strings.Contains(err.Error(), "daemon version") {
			t.Fatalf("Docker server version drift error = %v", err)
		}
	})

	t.Run("daemon unavailable", func(t *testing.T) {
		fixture := newEvaluatorLockGeneratorFixture(t)
		config := fixture.dockerScriptConfig()
		config.daemonAvailable = false
		writeGeneratorFile(t, fixture.dockerCLI, evaluatorGeneratorDockerScript(config), 0o700)
		if _, err := discoverEvaluatorRuntime(fixture.config()); err == nil || !strings.Contains(err.Error(), "daemon") {
			t.Fatalf("daemon availability error = %v", err)
		}
	})
}

func TestDiscoverEvaluatorRuntimeRejectsCrossProbeHarborRuntimeReplacement(t *testing.T) {
	t.Run("launcher cannot launder its shebang", func(t *testing.T) {
		fixture := newEvaluatorLockGeneratorFixture(t)
		alternateInterpreter := filepath.Join(filepath.Dir(fixture.pythonInterpreter), "alternate-python")
		writeGeneratorFile(t, alternateInterpreter, "#!/bin/sh\nexec /bin/sh \"$@\"\n", 0o700)
		replacement := fixture.harborLauncher + ".replacement"
		writeGeneratorFile(t, replacement, "#!"+fixture.pythonInterpreter+"\nif [ \"${1:-}\" = \"--version\" ]; then printf '0.18.0\\n'; exit 0; fi\nexit 1\n", 0o700)
		writeGeneratorFile(t, fixture.harborLauncher, "#!"+alternateInterpreter+"\nif [ \"${1:-}\" = \"--version\" ]; then /bin/mv "+fmt.Sprintf("%q", replacement)+" "+fmt.Sprintf("%q", fixture.harborLauncher)+"; printf '0.18.0\\n'; exit 0; fi\nexit 1\n", 0o700)

		if _, err := discoverEvaluatorRuntime(fixture.config()); err == nil || !strings.Contains(err.Error(), "shebang does not resolve to the pinned Python interpreter") {
			t.Fatalf("cross-identity shebang error = %v, want initial launcher shebang failure", err)
		}
		if _, err := os.Stat(replacement); err != nil {
			t.Fatalf("launcher was executed before its frozen shebang was verified: %v", err)
		}
	})

	t.Run("launcher replaces itself", func(t *testing.T) {
		fixture := newEvaluatorLockGeneratorFixture(t)
		replacement := fixture.harborLauncher + ".replacement"
		writeGeneratorFile(t, replacement, "#!"+fixture.pythonInterpreter+"\nif [ \"${1:-}\" = \"--version\" ]; then printf '0.18.0\\n'; exit 0; fi\nexit 1\n", 0o700)
		writeGeneratorFile(t, fixture.harborLauncher, "#!"+fixture.pythonInterpreter+"\nif [ \"${1:-}\" = \"--version\" ]; then /bin/mv "+fmt.Sprintf("%q", replacement)+" "+fmt.Sprintf("%q", fixture.harborLauncher)+"; printf '0.18.0\\n'; exit 0; fi\nexit 1\n", 0o700)

		if _, err := discoverEvaluatorRuntime(fixture.config()); err == nil || !strings.Contains(err.Error(), "Harbor launcher identity changed") {
			t.Fatalf("launcher replacement error = %v, want frozen launcher identity failure", err)
		}
	})

	t.Run("interpreter replaces itself", func(t *testing.T) {
		fixture := newEvaluatorLockGeneratorFixture(t)
		replacement := fixture.pythonInterpreter + ".replacement"
		writeGeneratorFile(t, replacement, "#!/bin/sh\nif [ \"${1:-}\" = \"--version\" ]; then printf 'Python 3.13.5\\n'; exit 0; fi\nexec /bin/sh \"$@\"\n", 0o700)
		writeGeneratorFile(t, fixture.pythonInterpreter, "#!/bin/sh\nif [ \"${1:-}\" = \"--version\" ]; then /bin/mv "+fmt.Sprintf("%q", replacement)+" "+fmt.Sprintf("%q", fixture.pythonInterpreter)+"; printf 'Python 3.13.5\\n'; exit 0; fi\nexec /bin/sh \"$@\"\n", 0o700)

		if _, err := discoverEvaluatorRuntime(fixture.config()); err == nil || !strings.Contains(err.Error(), "Python interpreter identity changed") {
			t.Fatalf("interpreter replacement error = %v, want frozen interpreter identity failure", err)
		}
	})

	t.Run("launcher mutates source tree", func(t *testing.T) {
		fixture := newEvaluatorLockGeneratorFixture(t)
		source := filepath.Join(fixture.pythonSourceTree, "__init__.py")
		replacement := filepath.Join(filepath.Dir(fixture.pythonSourceTree), "replacement.txt")
		writeGeneratorFile(t, replacement, "VERSION = 'mutated'\n", 0o600)
		writeGeneratorFile(t, fixture.harborLauncher, "#!"+fixture.pythonInterpreter+"\nif [ \"${1:-}\" = \"--version\" ]; then /bin/mv "+fmt.Sprintf("%q", replacement)+" "+fmt.Sprintf("%q", source)+"; printf '0.18.0\\n'; exit 0; fi\nexit 1\n", 0o700)

		if _, err := discoverEvaluatorRuntime(fixture.config()); err == nil || !strings.Contains(err.Error(), "Harbor Python source tree identity changed") {
			t.Fatalf("source-tree drift error = %v, want frozen source-tree identity failure", err)
		}
	})
}

func TestFinalEvaluatorRuntimeVerificationRejectsSameContentPathReplacement(t *testing.T) {
	t.Run("launcher inode", func(t *testing.T) {
		fixture := newEvaluatorLockGeneratorFixture(t)
		runtime, err := discoverEvaluatorRuntime(fixture.config())
		if err != nil {
			t.Fatal(err)
		}
		replaceGeneratorFileWithSameContent(t, fixture.harborLauncher, 0o700)

		if err := verifyEvaluatorRuntimeUnchanged(fixture.config(), runtime); err == nil || !strings.Contains(err.Error(), "Harbor launcher identity changed") {
			t.Fatalf("same-content launcher replacement error = %v, want pathname/inode identity failure", err)
		}
	})

	t.Run("source file inode", func(t *testing.T) {
		fixture := newEvaluatorLockGeneratorFixture(t)
		runtime, err := discoverEvaluatorRuntime(fixture.config())
		if err != nil {
			t.Fatal(err)
		}
		replaceGeneratorFileWithSameContent(t, filepath.Join(fixture.pythonSourceTree, "__init__.py"), 0o600)

		if err := verifyEvaluatorRuntimeUnchanged(fixture.config(), runtime); err == nil || !strings.Contains(err.Error(), "Harbor Python source tree identity changed") {
			t.Fatalf("same-content source replacement error = %v, want pathname/inode identity failure", err)
		}
	})
}

func TestBuildRejectsEndpointDriftWithoutReadingUnapprovedEnvironment(t *testing.T) {
	fixture := newEvaluatorLockGeneratorFixture(t)
	fixture.environment[qwenEndpointEnvironment] = "https://changed.invalid/v1"
	if _, err := build(fixture.config()); err == nil || !strings.Contains(err.Error(), "does not match the source-controlled catalog") {
		t.Fatalf("endpoint drift build error = %v, want catalog mismatch", err)
	}
	for _, name := range fixture.lookedUpNames() {
		if name != qwenEndpointEnvironment && name != opusEndpointEnvironment && name != qwenCredentialEnvironment && name != opusCredentialEnvironment {
			t.Fatalf("generator read unapproved environment name %q", name)
		}
	}
}

func TestSourceBuildIdentityExcludesAllGeneratedProductionLocks(t *testing.T) {
	git := testGitExecutable(t)
	root := t.TempDir()
	writeGeneratorFile(t, filepath.Join(root, "stable.txt"), "stable\n", 0o600)
	generatedLocks := []string{
		"deployments/standard-authoring/operation-catalog.lock.json",
		"deployments/codeedge-phase1/operation-catalog.lock.json",
		"deployments/codeedge-evaluator-child/operation-catalog.lock.json",
	}
	if len(generatedProductionLocks) != len(generatedLocks) {
		t.Fatalf("generated lock paths = %v, want %v", generatedProductionLocks, generatedLocks)
	}
	for _, want := range generatedLocks {
		if _, found := generatedProductionLocks[want]; !found {
			t.Fatalf("generated lock paths omit %q: %v", want, generatedProductionLocks)
		}
	}
	for _, lock := range generatedLocks {
		writeGeneratorFile(t, filepath.Join(root, lock), "first lock\n", 0o600)
	}
	testGit(t, root, git, "init")
	testGit(t, root, git, "config", "user.email", "lock-generator@example.invalid")
	testGit(t, root, git, "config", "user.name", "Lock Generator")
	testGit(t, root, git, "add", ".")
	testGit(t, root, git, "commit", "-m", "first")
	_, first, err := sourceBuildIdentity(root, git)
	if err != nil {
		t.Fatal(err)
	}
	for _, lock := range generatedLocks {
		writeGeneratorFile(t, filepath.Join(root, lock), "second lock\n", 0o600)
	}
	testGit(t, root, git, "add", "deployments")
	testGit(t, root, git, "commit", "-m", "generated locks only")
	_, second, err := sourceBuildIdentity(root, git)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("source manifest changed only because generated locks changed: %s != %s", first, second)
	}
}

func TestEvaluatorAssetsAndProfileAreStrictlyAccepted(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate evaluator lock generator test")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "deployments", "codeedge-evaluator-child"))
	profile, err := readEvaluatorExecutionProfile(filepath.Join(root, "execution-profile.v1.json"))
	if err != nil {
		t.Fatalf("read production evaluator profile: %v", err)
	}
	if !profile.Template.Equal(workflowadapter.CodeEdgeEvaluatorChildTemplateReference()) {
		t.Fatalf("production evaluator profile template = %#v", profile.Template)
	}
	for _, stageKey := range workflowadapter.CodeEdgeEvaluatorChildStageOrder() {
		budget, found := profile.Budget(stageKey)
		if !found || budget.MaxAttempts != 1 || len(budget.Backoff.RetryDelays) != 0 {
			t.Fatalf("production evaluator profile stage %q did not disable generic retries: %#v", stageKey, budget)
		}
	}
	catalogRaw, err := os.ReadFile(filepath.Join(root, "operation-catalog.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	catalogDocument, err := stageprovider.ParseDeploymentOperationCatalogJSON(catalogRaw)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := stageprovider.NewDeploymentOperationCatalogResolver(catalogDocument)
	if err != nil {
		t.Fatal(err)
	}
	registrations, err := requiredEvaluatorRegistrations(catalog)
	if err != nil {
		t.Fatalf("validate production evaluator catalog: %v", err)
	}
	assets, err := readEvaluatorContractAssets(filepath.Join(root, "contract-assets.v1.json"), root, registrations)
	if err != nil {
		t.Fatalf("validate production evaluator source assets: %v", err)
	}
	if len(assets) != 2 {
		t.Fatalf("production evaluator assets cover %d stages, want 2", len(assets))
	}
	manifestRaw, err := os.ReadFile(filepath.Join(root, "contract-assets.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := parseEvaluatorAssetManifest(manifestRaw)
	if err != nil {
		t.Fatalf("parse production evaluator asset manifest: %v", err)
	}
	if manifest.Format != evaluatorContractAssetManifestFormat || len(manifest.Operations) != 2 {
		t.Fatalf("production evaluator asset manifest = %#v", manifest)
	}
	if _, err := parseEvaluatorAssetManifest([]byte(`{"format":"x","format":"x"}`)); err == nil {
		t.Fatal("duplicate evaluator asset manifest key was accepted")
	}
}

func TestWriteNewRegularFileRefusesOverwrite(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "operation-catalog.lock.json")
	writeGeneratorFile(t, path, "old", 0o600)
	if err := writeNewRegularFile(path, []byte("new")); err == nil {
		t.Fatal("existing lock was replaced")
	}
}

func TestBuildRejectsSubstitutedOrUncommittedManagedAssets(t *testing.T) {
	substituted := newEvaluatorLockGeneratorFixture(t)
	substitutedConfig := substituted.config()
	substitutedConfig.catalogPath = filepath.Join(substituted.root, "alternate-catalog.json")
	if _, err := build(substitutedConfig); err == nil || !strings.Contains(err.Error(), "fixed managed asset") {
		t.Fatalf("substituted catalog path was accepted: %v", err)
	}

	uncommitted := newEvaluatorLockGeneratorFixture(t)
	relative, err := filepath.Rel(uncommitted.root, uncommitted.profilePath)
	if err != nil {
		t.Fatal(err)
	}
	relative = filepath.ToSlash(relative)
	writeGeneratorFile(t, filepath.Join(uncommitted.root, ".gitignore"), relative+"\n", 0o600)
	testGit(t, uncommitted.root, uncommitted.gitExecutable, "rm", "--cached", "--", relative)
	testGit(t, uncommitted.root, uncommitted.gitExecutable, "add", ".gitignore")
	testGit(t, uncommitted.root, uncommitted.gitExecutable, "commit", "-m", "ignore evaluator profile")
	if _, err := build(uncommitted.config()); err == nil || !strings.Contains(err.Error(), "required evaluator source asset is not committed") {
		t.Fatalf("uncommitted managed asset was accepted: %v", err)
	}
}

func TestCloneEvaluatorSecretsPreservesExplicitEmptyArray(t *testing.T) {
	cloned := cloneEvaluatorSecrets([]workflowadapter.SecretReference{})
	if cloned == nil || len(cloned) != 0 {
		t.Fatalf("explicit empty secret allow-list became %#v", cloned)
	}
}

type evaluatorLockGeneratorFixture struct {
	root                         string
	catalogPath                  string
	manifestPath                 string
	profilePath                  string
	contractRoot                 string
	outputPath                   string
	gitExecutable                string
	harborLauncher               string
	claudeCodeExecutable         string
	pythonInterpreter            string
	pythonSourceTree             string
	dockerCLI                    string
	dockerComposePlugin          string
	dockerBuildxPlugin           string
	dockerServerProbeMarker      string
	dockerComposeInfoProbeMarker string
	dockerBuildxInfoProbeMarker  string
	dockerComposeProbeMarker     string
	dockerBuildxProbeMarker      string
	nonVersionProbeMarker        string
	environment                  map[string]string
	looked                       []string
}

func newEvaluatorLockGeneratorFixture(t *testing.T) *evaluatorLockGeneratorFixture {
	t.Helper()
	root := t.TempDir()
	contractRoot := filepath.Join(root, "deployments", "codeedge-evaluator-child")
	for _, directory := range []string{contractRoot, filepath.Join(contractRoot, "contracts"), filepath.Join(contractRoot, "schemas")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	environment := map[string]string{
		qwenEndpointEnvironment:   "https://qwen.example.invalid/v1",
		opusEndpointEnvironment:   "https://opus.example.invalid/v1",
		qwenCredentialEnvironment: "qwen-test-token-not-persisted",
		opusCredentialEnvironment: "opus-test-token-not-persisted",
	}
	catalog := evaluatorGeneratorCatalog(t, environment)
	catalogRaw, err := catalog.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	catalogPath := filepath.Join(contractRoot, "operation-catalog.v1.json")
	writeGeneratorFile(t, catalogPath, string(catalogRaw), 0o600)
	writeGeneratorFile(t, filepath.Join(contractRoot, "contract-assets.v1.json"), evaluatorGeneratorAssetManifestJSON, 0o600)
	writeGeneratorFile(t, filepath.Join(contractRoot, "contracts", "harbor-pass-at-four.v0.18.json"), evaluatorGeneratorInvocationContractJSON, 0o600)
	writeGeneratorFile(t, filepath.Join(contractRoot, "schemas", "harbor-run-bundle.v0.18.json"), evaluatorGeneratorResultSchemaJSON, 0o600)
	profile := evaluatorGeneratorProfile(t)
	profileRaw, err := profile.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	profilePath := filepath.Join(contractRoot, "execution-profile.v1.json")
	writeGeneratorFile(t, profilePath, string(profileRaw), 0o600)

	runtimeRoot := filepath.Join(root, "runtime")
	if err := os.MkdirAll(filepath.Join(runtimeRoot, "site-packages", "harbor"), 0o700); err != nil {
		t.Fatal(err)
	}
	python := filepath.Join(runtimeRoot, "python")
	writeGeneratorFile(t, python, "#!/bin/sh\nif [ \"${1:-}\" = \"--version\" ]; then printf 'Python 3.13.5\\n'; exit 0; fi\nexec /bin/sh \"$@\"\n", 0o700)
	marker := filepath.Join(runtimeRoot, "unexpected-harbor-invocation")
	harbor := filepath.Join(runtimeRoot, "harbor")
	writeGeneratorFile(t, harbor, "#!"+python+"\nif [ \"${1:-}\" = \"--version\" ]; then printf '0.18.0\\n'; exit 0; fi\nprintf unexpected > "+marker+"\nexit 1\n", 0o700)
	claude := filepath.Join(runtimeRoot, "claude")
	writeGeneratorFile(t, claude, "#!/bin/sh\nif [ \"${1:-}\" = \"--version\" ]; then printf '2.1.207 (Claude Code)\\n'; exit 0; fi\nexit 1\n", 0o700)
	docker := filepath.Join(runtimeRoot, "docker")
	dockerComposeDirectory := filepath.Join(runtimeRoot, "libexec", "docker", "cli-plugins")
	if err := os.MkdirAll(dockerComposeDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	dockerCompose := filepath.Join(dockerComposeDirectory, "docker-compose")
	writeGeneratorFile(t, dockerCompose, "#!/bin/sh\nif [ \"${1:-}\" = \"version\" ]; then printf 'Docker Compose version v5.1.3\\n'; exit 0; fi\nexit 1\n", 0o700)
	dockerBuildx := filepath.Join(dockerComposeDirectory, "docker-buildx")
	writeGeneratorFile(t, dockerBuildx, "#!/bin/sh\nif [ \"${1:-}\" = \"version\" ]; then printf 'github.com/docker/buildx v0.33.0 f7897eba028583e0071642db3c011e860444f8cf\\n'; exit 0; fi\nexit 1\n", 0o700)
	probeRoot := t.TempDir()
	dockerServerProbeMarker := filepath.Join(probeRoot, "docker-server-probed")
	dockerComposeInfoProbeMarker := filepath.Join(probeRoot, "docker-compose-info-probed")
	dockerBuildxInfoProbeMarker := filepath.Join(probeRoot, "docker-buildx-info-probed")
	dockerComposeProbeMarker := filepath.Join(probeRoot, "docker-compose-probed")
	dockerBuildxProbeMarker := filepath.Join(probeRoot, "docker-buildx-probed")
	writeGeneratorFile(t, docker, evaluatorGeneratorDockerScript(evaluatorGeneratorDockerScriptConfig{
		composePath: dockerCompose, composeOutput: stageprovider.HarborEvaluatorDockerComposeVersionOutput,
		buildxPath: dockerBuildx, buildxOutput: stageprovider.HarborEvaluatorDockerBuildxVersionOutput,
		serverVersion: stageprovider.HarborEvaluatorDockerServerVersion, daemonAvailable: true,
		serverMarker: dockerServerProbeMarker, composeInfoMarker: dockerComposeInfoProbeMarker, buildxInfoMarker: dockerBuildxInfoProbeMarker,
		composeMarker: dockerComposeProbeMarker, buildxMarker: dockerBuildxProbeMarker,
	}), 0o700)
	writeGeneratorFile(t, filepath.Join(runtimeRoot, "site-packages", "harbor", "__init__.py"), "VERSION = '0.18.0'\n", 0o600)

	git := testGitExecutable(t)
	testGit(t, root, git, "init")
	testGit(t, root, git, "config", "user.email", "lock-generator@example.invalid")
	testGit(t, root, git, "config", "user.name", "Lock Generator")
	testGit(t, root, git, "add", ".")
	testGit(t, root, git, "commit", "-m", "source")
	return &evaluatorLockGeneratorFixture{
		root: root, catalogPath: catalogPath, manifestPath: filepath.Join(contractRoot, "contract-assets.v1.json"), profilePath: profilePath,
		contractRoot: contractRoot, outputPath: filepath.Join(contractRoot, "operation-catalog.lock.json"), gitExecutable: git,
		harborLauncher: harbor, claudeCodeExecutable: claude, pythonInterpreter: python, pythonSourceTree: filepath.Join(runtimeRoot, "site-packages", "harbor"), dockerCLI: docker, dockerComposePlugin: dockerCompose, dockerBuildxPlugin: dockerBuildx,
		dockerServerProbeMarker: dockerServerProbeMarker, dockerComposeInfoProbeMarker: dockerComposeInfoProbeMarker, dockerBuildxInfoProbeMarker: dockerBuildxInfoProbeMarker,
		dockerComposeProbeMarker: dockerComposeProbeMarker, dockerBuildxProbeMarker: dockerBuildxProbeMarker, nonVersionProbeMarker: marker, environment: environment,
	}
}

func (fixture *evaluatorLockGeneratorFixture) config() buildConfig {
	return buildConfig{
		sourceRoot: fixture.root, catalogPath: fixture.catalogPath, assetManifest: fixture.manifestPath, profilePath: fixture.profilePath,
		contractRoot: fixture.contractRoot, outputPath: fixture.outputPath, buildVersion: "v2.0.0",
		lockID: "codeedge-evaluator-child-production-lock", lockVersion: "v2.0.0", gitExecutable: fixture.gitExecutable,
		harborLauncher: fixture.harborLauncher, claudeCodeExecutable: fixture.claudeCodeExecutable, pythonInterpreter: fixture.pythonInterpreter, pythonSourceTree: fixture.pythonSourceTree, dockerCLI: fixture.dockerCLI, dockerComposePlugin: fixture.dockerComposePlugin, dockerBuildxPlugin: fixture.dockerBuildxPlugin,
		lookupEnvironment: func(name string) (string, bool) {
			fixture.looked = append(fixture.looked, name)
			value, present := fixture.environment[name]
			return value, present
		},
	}
}

type evaluatorGeneratorDockerScriptConfig struct {
	composePath       string
	composeOutput     string
	buildxPath        string
	buildxOutput      string
	serverVersion     string
	daemonAvailable   bool
	serverMarker      string
	composeInfoMarker string
	buildxInfoMarker  string
	composeMarker     string
	buildxMarker      string
}

func (fixture *evaluatorLockGeneratorFixture) dockerScriptConfig() evaluatorGeneratorDockerScriptConfig {
	return evaluatorGeneratorDockerScriptConfig{
		composePath: fixture.dockerComposePlugin, composeOutput: stageprovider.HarborEvaluatorDockerComposeVersionOutput,
		buildxPath: fixture.dockerBuildxPlugin, buildxOutput: stageprovider.HarborEvaluatorDockerBuildxVersionOutput,
		serverVersion: stageprovider.HarborEvaluatorDockerServerVersion, daemonAvailable: true,
		serverMarker: fixture.dockerServerProbeMarker, composeInfoMarker: fixture.dockerComposeInfoProbeMarker, buildxInfoMarker: fixture.dockerBuildxInfoProbeMarker,
		composeMarker: fixture.dockerComposeProbeMarker, buildxMarker: fixture.dockerBuildxProbeMarker,
	}
}

func evaluatorGeneratorDockerScript(config evaluatorGeneratorDockerScriptConfig) string {
	serverExit := "printf '%s\\n' " + fmt.Sprintf("%q", config.serverVersion) + "; exit 0"
	if !config.daemonAvailable {
		serverExit = "exit 1"
	}
	return "#!/bin/sh\n" +
		"if [ -n \"${QWEN_HARBOR_BASE_URL:-}\" ] || [ -n \"${OPUS_HARBOR_BASE_URL:-}\" ] || [ -n \"${ANTHROPIC_AUTH_TOKEN:-}\" ] || [ -z \"${DOCKER_CONFIG:-}\" ] || [ ! -d \"$DOCKER_CONFIG\" ] || [ -z \"${HOME:-}\" ] || [ -z \"${PATH:-}\" ]; then exit 90; fi\n" +
		"if [ \"${1:-}\" = \"--version\" ]; then printf 'Docker version 29.5.2, build controlled\\n'; exit 0; fi\n" +
		"if [ \"${1:-}\" = \"version\" ] && [ \"${2:-}\" = \"--format\" ] && [ \"${3:-}\" = " + fmt.Sprintf("%q", dockerServerVersionFormat) + " ]; then : > " + fmt.Sprintf("%q", config.serverMarker) + "; " + serverExit + "; fi\n" +
		"if [ \"${1:-}\" = \"info\" ] && [ \"${2:-}\" = \"--format\" ] && [ \"${3:-}\" = " + fmt.Sprintf("%q", dockerComposePathFormat) + " ]; then : > " + fmt.Sprintf("%q", config.composeInfoMarker) + "; printf '%s\\n' " + fmt.Sprintf("%q", config.composePath) + "; exit 0; fi\n" +
		"if [ \"${1:-}\" = \"info\" ] && [ \"${2:-}\" = \"--format\" ] && [ \"${3:-}\" = " + fmt.Sprintf("%q", dockerBuildxPathFormat) + " ]; then : > " + fmt.Sprintf("%q", config.buildxInfoMarker) + "; printf '%s\\n' " + fmt.Sprintf("%q", config.buildxPath) + "; exit 0; fi\n" +
		"if [ \"${1:-}\" = \"compose\" ] && [ \"${2:-}\" = \"version\" ]; then : > " + fmt.Sprintf("%q", config.composeMarker) + "; printf '%s\\n' " + fmt.Sprintf("%q", config.composeOutput) + "; exit 0; fi\n" +
		"if [ \"${1:-}\" = \"buildx\" ] && [ \"${2:-}\" = \"version\" ]; then : > " + fmt.Sprintf("%q", config.buildxMarker) + "; printf '%s\\n' " + fmt.Sprintf("%q", config.buildxOutput) + "; exit 0; fi\n" +
		"exit 1\n"
}

func (fixture *evaluatorLockGeneratorFixture) lookedUpNames() []string {
	values := append([]string(nil), fixture.looked...)
	return uniqueSorted(values)
}

func evaluatorGeneratorCatalog(t *testing.T, environment map[string]string) stageprovider.DeploymentOperationCatalog {
	t.Helper()
	secret := workflowadapter.SecretReference{ID: "anthropic-auth-token", Provider: "environment", Version: "2026.07.14"}
	makeRegistration := func(stageKey workflowkit.StageKey, stageType workflowadapter.StageBindingType, commandID, operationID, modelID, endpointName, credentialHostEnv string) stageprovider.DeploymentOperationRegistration {
		definition, present := workflowadapter.CodeEdgeEvaluatorChildStageCatalog().Stage(stageKey)
		if !present {
			t.Fatalf("missing evaluator child stage %q", stageKey)
		}
		endpointFingerprint, err := stageprovider.CanonicalHarborEvaluatorEndpointFingerprint(environment[endpointName])
		if err != nil {
			t.Fatal(err)
		}
		contract := stageprovider.HarborEvaluatorOperationContract{
			Format: stageprovider.HarborEvaluatorOperationContractFormat, Version: stageprovider.HarborEvaluatorOperationContractVersion,
			HarborVersion: stageprovider.HarborEvaluatorHarborVersion, ResultABIFormat: stageprovider.HarborEvaluatorResultABIFormat, ResultABIVersion: stageprovider.HarborEvaluatorResultABIVersion,
			TaskArtifactPort: stageprovider.HarborEvaluatorTaskArtifactPort, TaskArtifactSchema: stageprovider.HarborEvaluatorTaskArtifactSchema,
			AgentID: "claude-code", AgentVersion: "2.1.207", ModelID: modelID, ModelVersion: "2026.07.14",
			EndpointEnvName: endpointName, EndpointChildEnvKey: "ANTHROPIC_BASE_URL", EndpointFingerprint: endpointFingerprint,
			SecretEnvTemplates: []stageprovider.HarborEvaluatorSecretEnvTemplate{{Secret: secret, HostEnvKey: credentialHostEnv, ChildEnvKey: credentialChildEnvironment, Template: stageprovider.HarborEvaluatorSecretValueTemplate}},
			Attempts:           stageprovider.HarborEvaluatorTrialCount, ConcurrentTrials: stageprovider.HarborEvaluatorConcurrentTrials, MaxRetries: stageprovider.HarborEvaluatorMaxRetries, RequireTrajectory: true,
			ScreenshotRenderer: stageprovider.HarborEvaluatorScreenshotRenderer{ID: stageprovider.HarborEvaluatorTerminalPNGRendererID, Version: stageprovider.HarborEvaluatorTerminalPNGRendererVersion, SchemaVersion: stageprovider.HarborEvaluatorTerminalPNGRendererSchemaVersion},
		}
		return stageprovider.DeploymentOperationRegistration{
			Stage: stageprovider.DeploymentStageContract{
				Key: stageKey, Type: stageType, Group: definition.Group,
				Plugin: workflowkit.PluginBinding{ID: definition.Plugin.ID, Version: definition.Plugin.Version},
			},
			Provider:  workflowadapter.ProviderReference{ID: "codeedge-harbor-evaluator", Kind: "evaluation", Version: "1.0.0"},
			Operation: workflowadapter.StageOperationBinding{ProviderID: "codeedge-harbor-evaluator", OperationID: operationID, Version: "1.0.0", Payload: workflowadapter.LocalCommandOperationPayload{CommandID: commandID, Arguments: []string{}}},
			Runtime:   workflowadapter.RuntimeReference{ID: "codeedge-harbor-0.18-local", Kind: "controlled", Version: "1.0.0"},
			Checkout:  stageprovider.DeploymentCheckoutContract{ID: "codeedge-managed-task-snapshot", Purpose: "isolated-evaluator"},
			Secrets:   []workflowadapter.SecretReference{secret}, HarborEvaluator: &contract,
		}
	}
	return stageprovider.DeploymentOperationCatalog{
		Format: stageprovider.DeploymentOperationCatalogFormat, Version: stageprovider.DeploymentOperationCatalogVersion,
		CatalogID: "codeedge-evaluator-generator-test", CatalogVersion: "1.0.0", Template: workflowadapter.CodeEdgeEvaluatorChildTemplateReference(),
		Operations: []stageprovider.DeploymentOperationRegistration{
			makeRegistration(workflowadapter.HarborRunQwen, workflowadapter.StageBindingHarborRunQwen, stageprovider.HarborEvaluatorQwenCommandID, "codeedge.qwen.pass-at-four", "qwen3.7-max", qwenEndpointEnvironment, qwenCredentialEnvironment),
			makeRegistration(workflowadapter.HarborRunOpus, workflowadapter.StageBindingHarborRunOpus, stageprovider.HarborEvaluatorOpusCommandID, "codeedge.opus.pass-at-four", "claude-opus-4-6", opusEndpointEnvironment, opusCredentialEnvironment),
		},
	}
}

func evaluatorGeneratorProfile(t *testing.T) workflowadapter.ExecutionProfile {
	t.Helper()
	template := workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplate()
	profile := workflowadapter.ExecutionProfile{
		Template: template.Reference(), ID: "evaluator-generator-test", Version: "1.0.0", ContinuationPlanTTL: workflowadapter.RequiredContinuationPlanTTL,
		ControlGracePeriod: time.Minute, CandidateProviderBudget: workflowadapter.CandidateProviderBudget{AttemptTimeout: 30 * time.Minute, StartupGrace: 5 * time.Minute, ShutdownGrace: 5 * time.Minute},
		Stages: []workflowadapter.StageBudget{
			{StageKey: workflowadapter.HarborRunQwen, Budget: workflowkit.ExecutionBudget{TurnTimeout: 110 * time.Minute, MaxTurns: 1, AttemptTimeout: 120 * time.Minute, MaxAttempts: 1, MaxElapsed: 120 * time.Minute, StartupGrace: 5 * time.Minute, ShutdownGrace: 5 * time.Minute, Backoff: workflowkit.BackoffPolicy{RetryDelays: []time.Duration{}}}},
			{StageKey: workflowadapter.HarborRunOpus, Budget: workflowkit.ExecutionBudget{TurnTimeout: 110 * time.Minute, MaxTurns: 1, AttemptTimeout: 120 * time.Minute, MaxAttempts: 1, MaxElapsed: 120 * time.Minute, StartupGrace: 5 * time.Minute, ShutdownGrace: 5 * time.Minute, Backoff: workflowkit.BackoffPolicy{RetryDelays: []time.Duration{}}}},
		},
	}
	if err := profile.Validate(); err != nil {
		t.Fatalf("build evaluator profile: %v", err)
	}
	return profile
}

func testGitExecutable(t *testing.T) string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is unavailable")
	}
	abs, err := filepath.Abs(git)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func testGit(t *testing.T, root, git string, arguments ...string) {
	t.Helper()
	command := exec.Command(git, arguments...)
	command.Dir = root
	command.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "GIT_AUTHOR_NAME=Lock Generator", "GIT_AUTHOR_EMAIL=lock-generator@example.invalid", "GIT_COMMITTER_NAME=Lock Generator", "GIT_COMMITTER_EMAIL=lock-generator@example.invalid"}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(arguments, " "), err, output)
	}
}

func writeGeneratorFile(t *testing.T, file, contents string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
}

func replaceGeneratorFileWithSameContent(t *testing.T, file string, mode os.FileMode) {
	t.Helper()
	contents, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	replacement := file + ".same-content-replacement"
	writeGeneratorFile(t, replacement, string(contents), mode)
	if err := os.Rename(replacement, file); err != nil {
		t.Fatal(err)
	}
}

func uniqueSorted(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		seen[value] = struct{}{}
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	for left := 0; left < len(result); left++ {
		for right := left + 1; right < len(result); right++ {
			if result[right] < result[left] {
				result[left], result[right] = result[right], result[left]
			}
		}
	}
	return result
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

const evaluatorGeneratorAssetManifestJSON = `{
  "format":"harbor.codeedge-evaluator-contract-assets.v1",
  "version":"1",
  "template":{"id":"harbor.codeedge-evaluator","version":"1.0.0"},
  "operations":[
    {"stage_key":"harbor_run_qwen","prompt":{"id":"codeedge-harbor-pass-at-four-invocation","version":"0.18.0","relative_path":"contracts/harbor-pass-at-four.v0.18.json"},"schema":{"id":"harbor-run-bundle","version":"0.18.0","relative_path":"schemas/harbor-run-bundle.v0.18.json"}},
    {"stage_key":"harbor_run_opus","prompt":{"id":"codeedge-harbor-pass-at-four-invocation","version":"0.18.0","relative_path":"contracts/harbor-pass-at-four.v0.18.json"},"schema":{"id":"harbor-run-bundle","version":"0.18.0","relative_path":"schemas/harbor-run-bundle.v0.18.json"}}
  ]
}`

const evaluatorGeneratorInvocationContractJSON = `{
  "format":"harbor.codeedge-evaluator-invocation-contract.v1",
  "version":"1",
  "template":{"id":"harbor.codeedge-evaluator","version":"1.0.0"},
  "harbor_cli":{"version":"0.18.0","subcommand":"run"},
  "caller_arguments":"forbidden",
  "locality":{"remote_upload":"forbidden","remote_reconciliation":"forbidden","managed_jobs_directory":"required"},
  "trial_policy":{"n_attempts":4,"n_concurrent":1,"max_retries":3,"technical_retries_preserve_logical_trial":true},
  "evaluator_order":[
    {"stage_key":"harbor_run_qwen","command_id":"codeedge-qwen-pass4","agent_id":"claude-code","agent_version":"2.1.207","model_id":"qwen3.7-max","endpoint_env_name":"QWEN_HARBOR_BASE_URL"},
    {"stage_key":"harbor_run_opus","command_id":"codeedge-opus-pass4","agent_id":"claude-code","agent_version":"2.1.207","model_id":"claude-opus-4-6","endpoint_env_name":"OPUS_HARBOR_BASE_URL"}
  ]
}`

const evaluatorGeneratorResultSchemaJSON = `{
  "format":"harbor.codeedge-evaluator-result-schema.v1",
  "version":"1",
  "bundle":{"format":"harbor.run-bundle.v0.18","version":"1"},
  "required_evidence":["job.result.json","job.lock.json","trial.result.json","trajectory.json","terminal.png"],
  "trial_contract":{"logical_trial_count":4,"require_single_terminal_screenshot":true,"result_parser":"harbor-factory.codeedge.v0.18"}
}`
