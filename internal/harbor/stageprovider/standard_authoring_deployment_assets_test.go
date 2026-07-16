package stageprovider

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestStandardAuthoringDeploymentCatalogAndAssetsAreExactAndLoadable(t *testing.T) {
	root := standardAuthoringDeploymentRepositoryRoot(t)
	catalogRaw, err := os.ReadFile(filepath.Join(root, "deployments", "standard-authoring", "operation-catalog.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	catalogDocument, err := ParseDeploymentOperationCatalogJSON(catalogRaw)
	if err != nil {
		t.Fatalf("parse Standard authoring catalog: %v", err)
	}
	catalog, err := NewDeploymentOperationCatalogResolver(catalogDocument)
	if err != nil {
		t.Fatalf("resolve Standard authoring catalog: %v", err)
	}
	if !catalog.Template().Equal(workflowadapter.StandardAuthoringTemplateReference()) {
		t.Fatalf("catalog template = %s@%s, want Standard authoring", catalog.Template().ID, catalog.Template().Version)
	}

	manifestRaw, err := os.ReadFile(filepath.Join(root, "deployments", "standard-authoring", "contract-assets.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := ParseStandardAuthoringContractAssetManifestJSON(manifestRaw)
	if err != nil {
		t.Fatalf("parse Standard authoring asset manifest: %v", err)
	}
	if len(catalog.Catalog().Operations) != len(manifest.Operations) || len(manifest.Operations) != len(workflowadapter.StandardAuthoringStageOrder()) {
		t.Fatalf("catalog/manifest operation coverage = %d/%d, want %d", len(catalog.Catalog().Operations), len(manifest.Operations), len(workflowadapter.StandardAuthoringStageOrder()))
	}

	byStage := make(map[string]StandardAuthoringContractAssetManifestEntry, len(manifest.Operations))
	for _, entry := range manifest.Operations {
		byStage[string(entry.StageKey)] = entry
		for _, asset := range []StandardAuthoringContractAssetReference{entry.Prompt, entry.Schema} {
			assetPath := filepath.Join(root, "deployments", "standard-authoring", filepath.FromSlash(asset.RelativePath))
			info, err := os.Lstat(assetPath)
			if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				t.Fatalf("asset %q is not a regular non-symlink file: info=%v error=%v", asset.RelativePath, info, err)
			}
		}
	}

	agentStages := 0
	for _, registration := range catalog.Catalog().Operations {
		entry, found := byStage[string(registration.Stage.Key)]
		if !found {
			t.Fatalf("catalog stage %q has no typed asset entry", registration.Stage.Key)
		}
		payload, isAgentTurn := registration.Operation.Payload.(workflowadapter.AgentTurnOperationPayload)
		if !isAgentTurn {
			continue
		}
		agentStages++
		if !IsCodexAppServerProductionPayload(payload) {
			t.Fatalf("agent stage %q payload = %+v, want frozen %s/%s", registration.Stage.Key, payload, CodexAppServerProductionModelID, CodexAppServerProductionReasoningEffort)
		}
		promptRaw, err := os.ReadFile(filepath.Join(root, "deployments", "standard-authoring", filepath.FromSlash(entry.Prompt.RelativePath)))
		if err != nil {
			t.Fatal(err)
		}
		program, err := ParseStandardAuthoringCodexTurnProgramAsset(promptRaw)
		if err != nil {
			t.Fatalf("parse canonical prompt program for %q: %v", registration.Stage.Key, err)
		}
		if program.ID != entry.Prompt.ID || program.Version != entry.Prompt.Version || len(program.TurnPrompts) != payload.MaxTurns {
			t.Fatalf("prompt program for %q does not match frozen manifest/payload", registration.Stage.Key)
		}
		if entry.Schema.ID != "standard-authoring.codex-stage-output-schema" || entry.Schema.Version != "1.0.0" || entry.Schema.RelativePath != "schemas/codex-stage-output.schema.json" {
			t.Fatalf("agent stage %q has non-Codex schema asset %q@%q", registration.Stage.Key, entry.Schema.ID, entry.Schema.Version)
		}
		schemaRaw, err := os.ReadFile(filepath.Join(root, "deployments", "standard-authoring", filepath.FromSlash(entry.Schema.RelativePath)))
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateStandardAuthoringCodexOutputSchemaAsset(schemaRaw); err != nil {
			t.Fatalf("validate canonical Codex schema asset for %q: %v", registration.Stage.Key, err)
		}
	}
	if agentStages != 9 {
		t.Fatalf("catalog Codex agent stages = %d, want 9", agentStages)
	}
}

func TestStandardAuthoringContractAssetManifestRejectsMissingOrUnknownStage(t *testing.T) {
	root := standardAuthoringDeploymentRepositoryRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "deployments", "standard-authoring", "contract-assets.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := ParseStandardAuthoringContractAssetManifestJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	missing := manifest.Clone()
	missing.Operations = missing.Operations[:len(missing.Operations)-1]
	if err := missing.Validate(); err == nil {
		t.Fatal("manifest with a missing Standard authoring stage was accepted")
	}
	unknown := manifest.Clone()
	unknown.Operations[0].StageKey = "unknown_stage"
	if err := unknown.Validate(); err == nil {
		t.Fatal("manifest with an unknown Standard authoring stage was accepted")
	}
}

func TestStandardAuthoringSSHKnownHostsAssetRequiresExplicitPreNetworkHosts(t *testing.T) {
	knownHosts := []byte("github.com ssh-ed25519 AQID\n[git.example]:2222 ssh-ed25519 BAUG\n")
	if err := ValidateStandardAuthoringSSHKnownHostsAsset(knownHosts); err != nil {
		t.Fatalf("validate explicit known_hosts allow-list: %v", err)
	}
	for _, test := range []struct {
		host string
		port string
		want bool
	}{
		{host: "github.com", want: true},
		{host: "github.com", port: "22", want: true},
		{host: "github.com", port: "2222", want: false},
		{host: "git.example", port: "2222", want: true},
		{host: "git.example", want: false},
		{host: "unlisted.example", want: false},
	} {
		got, err := StandardAuthoringSSHKnownHostsAllowsHost(knownHosts, test.host, test.port)
		if err != nil || got != test.want {
			t.Fatalf("allow host %q:%q = %t, %v; want %t", test.host, test.port, got, err, test.want)
		}
	}
	for _, raw := range [][]byte{
		[]byte("*.example ssh-ed25519 AQID\n"),
		[]byte("|1|salt|hash ssh-ed25519 AQID\n"),
		[]byte("@cert-authority example ssh-ed25519 AQID\n"),
		[]byte("github.com ssh-ed25519 not-base64\n"),
	} {
		if err := ValidateStandardAuthoringSSHKnownHostsAsset(raw); err == nil {
			t.Fatalf("non-explicit known_hosts input was accepted: %q", raw)
		}
	}
}

func TestLoadStandardAuthoringDeploymentAssetBundleStrictlyBindsGeneratedLockAndAssets(t *testing.T) {
	root := standardAuthoringDeploymentRepositoryRoot(t)
	deploymentRoot := filepath.Join(t.TempDir(), "deployments", "standard-authoring")
	if err := os.MkdirAll(deploymentRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"operation-catalog.v1.json", "contract-assets.v1.json", filepath.Join("ssh", "known_hosts")} {
		contents, err := os.ReadFile(filepath.Join(root, "deployments", "standard-authoring", name))
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(deploymentRoot, name)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifestRaw, err := os.ReadFile(filepath.Join(deploymentRoot, "contract-assets.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := ParseStandardAuthoringContractAssetManifestJSON(manifestRaw)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range manifest.Operations {
		for _, asset := range []StandardAuthoringContractAssetReference{entry.Prompt, entry.Schema} {
			source := filepath.Join(root, "deployments", "standard-authoring", filepath.FromSlash(asset.RelativePath))
			contents, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(deploymentRoot, filepath.FromSlash(asset.RelativePath))
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, contents, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	catalogRaw, err := os.ReadFile(filepath.Join(deploymentRoot, "operation-catalog.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	catalogDocument, err := ParseDeploymentOperationCatalogJSON(catalogRaw)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := NewDeploymentOperationCatalogResolver(catalogDocument)
	if err != nil {
		t.Fatal(err)
	}
	lock := standardAuthoringDeploymentTestLock(t, catalog, manifest, deploymentRoot)
	canonical, err := lock.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(deploymentRoot, "operation-catalog.lock.json")
	if err := os.WriteFile(lockPath, canonical, 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, err := LoadStandardAuthoringDeploymentAssetBundle(filepath.Join(deploymentRoot, "operation-catalog.v1.json"), lockPath, deploymentRoot)
	if err != nil {
		t.Fatalf("load exact generated deployment bundle: %v", err)
	}
	expectedFingerprint, err := lock.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Verifier.LockIdentity().Fingerprint != expectedFingerprint || len(bundle.Lock.Operations) != len(manifest.Operations) {
		t.Fatalf("loaded deployment bundle did not retain the exact static lock")
	}

	entry := manifest.Operations[0]
	promptPath := filepath.Join(deploymentRoot, filepath.FromSlash(entry.Prompt.RelativePath))
	if err := os.WriteFile(promptPath, []byte("asset drift"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadStandardAuthoringDeploymentAssetBundle(filepath.Join(deploymentRoot, "operation-catalog.v1.json"), lockPath, deploymentRoot); err == nil {
		t.Fatal("asset-drifted generated deployment bundle was accepted")
	}
}

func TestLoadStandardAuthoringDeploymentAssetBundleFailsClosedWithoutGeneratedLock(t *testing.T) {
	root := standardAuthoringDeploymentRepositoryRoot(t)
	deploymentRoot := filepath.Join(root, "deployments", "standard-authoring")
	missing := filepath.Join(t.TempDir(), "operation-catalog.lock.json")
	if _, err := LoadStandardAuthoringDeploymentAssetBundle(filepath.Join(deploymentRoot, "operation-catalog.v1.json"), missing, deploymentRoot); err == nil {
		t.Fatal("missing generated Standard authoring lock was accepted")
	}
}

func standardAuthoringDeploymentTestLock(t *testing.T, catalog *DeploymentOperationCatalogResolver, manifest StandardAuthoringContractAssetManifest, root string) DeploymentOperationCatalogLock {
	t.Helper()
	assets := make(map[workflowkit.StageKey]StandardAuthoringContractAssetManifestEntry, len(manifest.Operations))
	for _, entry := range manifest.Operations {
		assets[entry.StageKey] = entry
	}
	operations := make([]DeploymentOperationCatalogLockRecord, 0, len(catalog.Catalog().Operations))
	for _, registration := range catalog.Catalog().Operations {
		entry, found := assets[registration.Stage.Key]
		if !found {
			t.Fatalf("missing asset entry for %q", registration.Stage.Key)
		}
		prompt, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(entry.Prompt.RelativePath)))
		if err != nil {
			t.Fatal(err)
		}
		schema, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(entry.Schema.RelativePath)))
		if err != nil {
			t.Fatal(err)
		}
		record := DeploymentOperationCatalogLockRecord{
			Stage: registration.Stage, Provider: registration.Provider, Operation: registration.Operation.Clone(), Runtime: registration.Runtime,
			Checkout: registration.Checkout, Secrets: append([]workflowadapter.SecretReference{}, registration.Secrets...),
			PromptContentFingerprint: workflowkit.SHA256Fingerprint(prompt), SchemaContentFingerprint: workflowkit.SHA256Fingerprint(schema), ExecutionKind: registration.Operation.Payload.Kind(),
			StandardAuthoringContract: &StandardAuthoringContractLock{Format: StandardAuthoringContractLockFormat, Version: StandardAuthoringContractLockVersion, Prompt: entry.Prompt, Schema: entry.Schema},
		}
		switch payload := registration.Operation.Payload.(type) {
		case workflowadapter.LocalCommandOperationPayload:
			local := LocalExecutableLock{CommandID: payload.CommandID, AbsolutePath: "/opt/standard-authoring/git", Version: "2.47.3", ContentSHA256: workflowkit.SHA256Fingerprint([]byte("locked git"))}
			record.LocalExecutable = &local
		case workflowadapter.AgentTurnOperationPayload:
			codex := CodexAppServerOperationLock{
				Format: CodexAppServerOperationLockFormat, Version: CodexAppServerOperationLockVersion,
				JavaScriptLauncher: LocalExecutableLock{CommandID: CodexAppServerJavaScriptLauncherCommandID, AbsolutePath: "/opt/standard-authoring/codex.js", Version: "0.133.0", ContentSHA256: workflowkit.SHA256Fingerprint([]byte("locked launcher"))},
				NodeExecutable:     LocalExecutableLock{CommandID: CodexAppServerNodeExecutableCommandID, AbsolutePath: "/opt/standard-authoring/node", Version: "v26.2.0", ContentSHA256: workflowkit.SHA256Fingerprint([]byte("locked node"))},
				CodexHomeDirectory: "/opt/standard-authoring/codex-home", CLIVersionOutput: "codex-cli 0.133.0",
				SandboxMode: CodexAppServerSandboxModeWorkspaceWrite, SandboxPolicy: CodexAppServerSandboxPolicyWorkspaceWrite,
			}
			record.AgentModel = &AgentModelLock{
				AgentID: payload.AgentID, AgentVersion: "0.133.0", ModelID: payload.ModelID,
				ModelVersion: "gpt-5.6-terra",
			}
			record.CodexAppServer = &codex
		case workflowadapter.DurableReviewOperationPayload:
			record.DurableReviewPolicy = &DurableReviewPolicyLock{PolicyID: payload.PolicyID, Version: "1.0.0"}
		case workflowadapter.HarborBuiltinOperationPayload:
			record.HarborFlowBuiltin = &HarborFlowBuiltinOperationLock{Format: HarborFlowBuiltinOperationLockFormat, Version: HarborFlowBuiltinOperationLockVersion, HandlerID: payload.HandlerID, HandlerVersion: "1.0.0"}
		default:
			t.Fatalf("unsupported payload %T", payload)
		}
		operations = append(operations, record)
	}
	knownHosts, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(StandardAuthoringSSHKnownHostsRelativePath)))
	if err != nil {
		t.Fatal(err)
	}
	return DeploymentOperationCatalogLock{
		Format: DeploymentOperationCatalogLockFormat, Version: DeploymentOperationCatalogLockVersion,
		LockID: "standard-authoring-deployment-assets-test", LockVersion: "test-v1", CatalogReceipt: catalog.Receipt(),
		HarborFlowBuild:                   HarborFlowBuildIdentity{Module: "github.com/purplevoid/harbor-factory", Version: "v2.0.0", Commit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ContentSHA256: workflowkit.SHA256Fingerprint([]byte("test build"))},
		StandardAuthoringExecutionProfile: &StandardAuthoringExecutionProfileLock{Profile: standardAuthoringTestExecutionProfile(t)},
		StandardAuthoringSSHTransport:     standardAuthoringSSHTransportTestLock(t, knownHosts),
		Operations:                        operations,
	}
}

const standardAuthoringSSHTransportTestKnownHosts = "github.com ssh-ed25519 AQID\n"

func standardAuthoringSSHTransportTestLock(t *testing.T, knownHosts []byte) *StandardAuthoringSSHTransportLock {
	t.Helper()
	sshContent := workflowkit.SHA256Fingerprint([]byte("locked ssh"))
	shellContent := workflowkit.SHA256Fingerprint([]byte("locked shell"))
	return &StandardAuthoringSSHTransportLock{
		Format:  StandardAuthoringSSHTransportLockFormat,
		Version: StandardAuthoringSSHTransportLockVersion,
		SSHExecutable: LocalExecutableLock{
			CommandID: StandardAuthoringSSHTransportCommandID, AbsolutePath: "/opt/standard-authoring/ssh", Version: "OpenSSH_10.0p2", ContentSHA256: sshContent,
		},
		WrapperShell: LocalExecutableLock{
			CommandID: StandardAuthoringSSHWrapperShellCommandID, AbsolutePath: "/opt/standard-authoring/dash", Version: string(shellContent), ContentSHA256: shellContent,
		},
		KnownHosts:                 StandardAuthoringSSHKnownHostsLock{Format: StandardAuthoringSSHKnownHostsLockFormat, Version: StandardAuthoringSSHKnownHostsLockVersion, RelativePath: StandardAuthoringSSHKnownHostsRelativePath, ContentSHA256: workflowkit.SHA256Fingerprint(knownHosts)},
		AgentSocketEnvironmentName: StandardAuthoringSSHAgentSocketEnvironment,
	}
}

func standardAuthoringDeploymentRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate deployment asset test")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}
