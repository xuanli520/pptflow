package stageprovider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestStandardAuthoringV2DeploymentCatalogAndAssetsAreExactAndLoadable(t *testing.T) {
	deploymentRoot := filepath.Join(standardAuthoringDeploymentRepositoryRoot(t), "deployments", "standard-authoring")
	catalogRaw, err := os.ReadFile(filepath.Join(deploymentRoot, "operation-catalog.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	catalogDocument, err := ParseDeploymentOperationCatalogJSON(catalogRaw)
	if err != nil {
		t.Fatalf("parse v2 Standard authoring catalog: %v", err)
	}
	catalog, err := NewDeploymentOperationCatalogResolver(catalogDocument)
	if err != nil {
		t.Fatalf("resolve v2 Standard authoring catalog: %v", err)
	}
	if !catalog.Template().Equal(workflowadapter.StandardAuthoringCurrentTemplateReference()) {
		t.Fatalf("catalog template = %s@%s, want current v2 template", catalog.Template().ID, catalog.Template().Version)
	}

	profileRaw, err := os.ReadFile(filepath.Join(deploymentRoot, "execution-profile.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	profile, err := workflowadapter.ParseExecutionProfileJSON(profileRaw)
	if err != nil {
		t.Fatalf("parse v2 execution profile: %v", err)
	}
	if _, err := workflowadapter.StandardAuthoringCurrentWorkflowTemplate().Compile(profile); err != nil {
		t.Fatalf("compile v2 execution profile: %v", err)
	}

	manifestRaw, err := os.ReadFile(filepath.Join(deploymentRoot, "contract-assets.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := ParseStandardAuthoringContractAssetManifestJSON(manifestRaw)
	if err != nil {
		t.Fatalf("parse v2 contract asset manifest: %v", err)
	}
	if !manifest.Template.Equal(workflowadapter.StandardAuthoringCurrentTemplateReference()) {
		t.Fatalf("manifest template = %s@%s, want current v2 template", manifest.Template.ID, manifest.Template.Version)
	}
	if got, want := len(manifest.Operations), len(workflowadapter.StandardAuthoringStageOrder()); got != want || len(catalog.Catalog().Operations) != want {
		t.Fatalf("catalog/manifest operation coverage = %d/%d, want %d", len(catalog.Catalog().Operations), got, want)
	}

	assets := make(map[workflowkit.StageKey]StandardAuthoringContractAssetManifestEntry, len(manifest.Operations))
	for _, entry := range manifest.Operations {
		assets[entry.StageKey] = entry
		for _, asset := range []StandardAuthoringContractAssetReference{entry.Prompt, entry.Schema} {
			info, err := os.Lstat(filepath.Join(deploymentRoot, filepath.FromSlash(asset.RelativePath)))
			if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
				t.Fatalf("asset %q is not a regular non-symlink file: info=%v error=%v", asset.RelativePath, info, err)
			}
		}
	}

	agentStages := 0
	for _, registration := range catalog.Catalog().Operations {
		entry, found := assets[registration.Stage.Key]
		if !found {
			t.Fatalf("catalog stage %q has no manifest entry", registration.Stage.Key)
		}
		payload, agentTurn := registration.Operation.Payload.(workflowadapter.AgentTurnOperationPayload)
		if !agentTurn {
			continue
		}
		agentStages++
		if !IsCodexAppServerProductionPayload(payload) {
			t.Fatalf("agent stage %q payload = %+v, want frozen production Codex payload", registration.Stage.Key, payload)
		}
		promptRaw, err := os.ReadFile(filepath.Join(deploymentRoot, filepath.FromSlash(entry.Prompt.RelativePath)))
		if err != nil {
			t.Fatal(err)
		}
		program, err := ParseStandardAuthoringCodexTurnProgramAsset(promptRaw)
		if err != nil {
			t.Fatalf("parse prompt program for %q: %v", registration.Stage.Key, err)
		}
		if program.ID != entry.Prompt.ID || program.Version != entry.Prompt.Version || len(program.TurnPrompts) != payload.MaxTurns {
			t.Fatalf("prompt program for %q does not match manifest/payload", registration.Stage.Key)
		}
		joined := strings.Join(program.TurnPrompts, "\n")
		for _, required := range []string{"context.contract.content", "context.contract.digest", "harbor_submit_stage_output"} {
			if !strings.Contains(joined, required) {
				t.Fatalf("v2 prompt %q omits root-contract boundary %q", registration.Stage.Key, required)
			}
		}

		schemaRaw, err := os.ReadFile(filepath.Join(deploymentRoot, filepath.FromSlash(entry.Schema.RelativePath)))
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateStandardAuthoringCodexOutputSchemaAssetForTemplateStage(catalog.Template(), registration.Stage.Key, schemaRaw); err != nil {
			t.Fatalf("validate schema for %q: %v", registration.Stage.Key, err)
		}
		usesFixedFile := registration.Stage.Key == workflowkit.StageKey(workflowadapter.SolveGen) || registration.Stage.Key == workflowkit.StageKey(workflowadapter.TestGen)
		if usesFixedFile != standardAuthoringCodexUsesFixedFileOutputSchema(catalog.Template(), registration.Stage.Key) {
			t.Fatalf("fixed-file selection for %q diverged from v2 contract", registration.Stage.Key)
		}
		if usesFixedFile && entry.Schema.RelativePath != "schemas/codex-fixed-file-submit.schema.json" {
			t.Fatalf("fixed-file stage %q schema path = %q", registration.Stage.Key, entry.Schema.RelativePath)
		}
	}
	if agentStages != 11 {
		t.Fatalf("Codex agent stages = %d, want 11", agentStages)
	}
}

func TestStandardAuthoringV2ManifestRejectsUninstalledTemplate(t *testing.T) {
	deploymentRoot := filepath.Join(standardAuthoringDeploymentRepositoryRoot(t), "deployments", "standard-authoring")
	raw, err := os.ReadFile(filepath.Join(deploymentRoot, "contract-assets.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := ParseStandardAuthoringContractAssetManifestJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Template = workflowadapter.TemplateReference{ID: manifest.Template.ID, Version: "1.9.9"}
	if err := manifest.Validate(); err == nil {
		t.Fatal("manifest accepted an uninstalled Standard authoring template")
	}
}

func TestStandardAuthoringV2TaskHandoffSchemaMatchesRuntimeContract(t *testing.T) {
	deploymentRoot := filepath.Join(standardAuthoringDeploymentRepositoryRoot(t), "deployments", "standard-authoring")
	raw, err := os.ReadFile(filepath.Join(deploymentRoot, "schemas", "authoring-task-handoff.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			Format struct {
				Const string `json:"const"`
			} `json:"format"`
			Version struct {
				Const string `json:"const"`
			} `json:"version"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("decode task handoff schema: %v", err)
	}
	if schema.Properties.Format.Const != workflowadapter.StandardAuthoringTaskHandoffFormat || schema.Properties.Version.Const != workflowadapter.StandardAuthoringTaskHandoffVersion {
		t.Fatalf("task handoff schema contract = %q@%q, want %q@%q", schema.Properties.Format.Const, schema.Properties.Version.Const, workflowadapter.StandardAuthoringTaskHandoffFormat, workflowadapter.StandardAuthoringTaskHandoffVersion)
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
