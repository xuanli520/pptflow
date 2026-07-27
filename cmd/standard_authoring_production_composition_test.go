package cmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/codeedge"
	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestStandardAuthoringProductionCompositionBuildsItsOwnLockedRepoPrepareCapability(t *testing.T) {
	root := t.TempDir()
	database, err := store.OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	deploymentRoot, catalog, lock := standardAuthoringProductionTestDeployment(t)
	receiptFingerprint, err := lock.CatalogReceipt.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	lockFingerprint, err := lock.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	lockIdentity := stageprovider.DeploymentOperationCatalogLockIdentity{
		LockID: lock.LockID, LockVersion: lock.LockVersion, Fingerprint: lockFingerprint,
	}
	admission := codeedge.TaskAdmissionContract{
		ID: "codeedge-phase1-composition-test", Version: "1.0.0", Profile: codeEdgePhase1DefinitionProviderPreflightProfile(t),
	}
	if err := admission.Validate(); err != nil {
		t.Fatalf("construct CodeEdge admission fixture: %v", err)
	}
	composition, err := newStandardAuthoringProductionComposition(standardAuthoringProductionCompositionConfig{
		CatalogPath:               filepath.Join(deploymentRoot, "operation-catalog.v1.json"),
		LockPath:                  filepath.Join(deploymentRoot, "operation-catalog.lock.json"),
		ContractRoot:              deploymentRoot,
		ManagedRoot:               root,
		Store:                     database,
		HarborFlowBuild:           lock.HarborFlowBuild,
		CatalogReceiptFingerprint: receiptFingerprint,
		LockIdentity:              lockIdentity,
		AdmissionContract:         &admission,
	})
	if err != nil {
		t.Fatalf("construct Standard authoring production composition: %v", err)
	}
	if composition == nil || composition.Resolver == nil || composition.SourceCapturer == nil || composition.Definitions == nil ||
		!composition.CatalogBinding.Template.Equal(catalog.Template()) {
		t.Fatalf("incomplete Standard authoring composition: %+v", composition)
	}
	if composition.CatalogBinding.Resolver != composition.Resolver {
		t.Fatal("Standard authoring catalog binding and operation resolver must share one attested lock snapshot")
	}
	if composition.Resolver.LockIdentity() != lockIdentity {
		t.Fatal("Standard authoring composition operation resolver lost its generated lock identity")
	}
	workspaceRoot, err := app.StandardAuthoringCodexWorkspaceRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(workspaceRoot); err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		t.Fatalf("managed Standard authoring workspace root = %v, %v", info, err)
	}

	definition, err := composition.Definitions.StandardAuthoringRunDefinition(context.Background(), app.StandardAuthoringRunDefinitionSubject{
		SourceID: "018f0a73-3b49-7000-8000-0000000000e1", AuthoringSessionID: "018f0a73-3b49-7000-8000-0000000000e2",
		TargetTaskID: "018f0a73-3b49-7000-8000-0000000000e3", RepositoryURL: "https://github.com/example/fixture-repository.git",
		CommitSHA: "0123456789abcdef0123456789abcdef01234567", SourceSnapshotDigest: workflowkit.SubjectDigest("sha256:" + strings.Repeat("a", 64)),
		SourceSnapshotSchema: app.StandardAuthoringSourceSnapshotSchemaVersion,
	})
	if err != nil {
		t.Fatalf("build locked Standard authoring definition: %v", err)
	}
	lockedProfile, err := lock.StandardAuthoringProfile()
	if err != nil {
		t.Fatal(err)
	}
	definitionProfileFingerprint, err := definition.Profile.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	lockedProfileFingerprint, err := lockedProfile.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if definitionProfileFingerprint != lockedProfileFingerprint {
		t.Fatal("Standard authoring definition did not use the execution profile frozen in the deployment lock")
	}
	for _, stageKey := range workflowadapter.StandardAuthoringStageOrder() {
		resolution, err := definition.ExecutionSpec.ResolveStageOperation(stageKey)
		if err != nil {
			t.Fatal(err)
		}
		if err := composition.Resolver.ValidateStageOperation(resolution); err != nil {
			t.Fatalf("locked Standard stage %q is not resolved by its composition: %v", stageKey, err)
		}
	}
	if !catalog.Template().Equal(workflowadapter.StandardAuthoringCurrentTemplateReference()) {
		t.Fatalf("test catalog template = %+v", catalog.Template())
	}
}

func TestStandardAuthoringCandidateCommandTimeoutUsesHostCandidateVerifyAttemptBudget(t *testing.T) {
	template := workflowadapter.StandardAuthoringCurrentWorkflowTemplate()
	profile := standardAuthoringProductionTestProfile(t, template.Reference())
	for index := range profile.Stages {
		if profile.Stages[index].StageKey == workflowkit.StageKey(workflowadapter.HostCandidateVerify) {
			profile.Stages[index].Budget.AttemptTimeout = 2*time.Hour + 10*time.Minute
			profile.Stages[index].Budget.MaxElapsed = 2*time.Hour + 10*time.Minute
			break
		}
	}

	got, err := standardAuthoringCandidateCommandTimeout(profile)
	if err != nil {
		t.Fatalf("derive Standard authoring candidate command timeout: %v", err)
	}
	if want := 2*time.Hour + 10*time.Minute; got != want {
		t.Fatalf("candidate command timeout = %s, want %s", got, want)
	}
}

func standardAuthoringProductionTestDeployment(t *testing.T) (string, *stageprovider.DeploymentOperationCatalogResolver, stageprovider.DeploymentOperationCatalogLock) {
	t.Helper()
	deploymentRoot := t.TempDir()
	sourceRoot := standardAuthoringProductionRepositoryRoot(t)
	copyStandardAuthoringDeploymentTree(t, filepath.Join(sourceRoot, "deployments", "standard-authoring"), deploymentRoot)
	catalogRaw, err := os.ReadFile(filepath.Join(deploymentRoot, "operation-catalog.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	document, err := stageprovider.ParseDeploymentOperationCatalogJSON(catalogRaw)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := stageprovider.NewDeploymentOperationCatalogResolver(document)
	if err != nil {
		t.Fatal(err)
	}
	manifestRaw, err := os.ReadFile(filepath.Join(deploymentRoot, "contract-assets.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := stageprovider.ParseStandardAuthoringContractAssetManifestJSON(manifestRaw)
	if err != nil {
		t.Fatal(err)
	}
	assets := make(map[workflowkit.StageKey]stageprovider.StandardAuthoringContractAssetManifestEntry, len(manifest.Operations))
	for _, entry := range manifest.Operations {
		assets[entry.StageKey] = entry
	}
	lockedGit := standardAuthoringProductionTestGit(t)
	operations := make([]stageprovider.DeploymentOperationCatalogLockRecord, 0, len(catalog.Catalog().Operations))
	for _, registration := range catalog.Catalog().Operations {
		asset, found := assets[registration.Stage.Key]
		if !found {
			t.Fatalf("catalog stage %q has no contract asset", registration.Stage.Key)
		}
		prompt, err := os.ReadFile(filepath.Join(deploymentRoot, filepath.FromSlash(asset.Prompt.RelativePath)))
		if err != nil {
			t.Fatal(err)
		}
		schema, err := os.ReadFile(filepath.Join(deploymentRoot, filepath.FromSlash(asset.Schema.RelativePath)))
		if err != nil {
			t.Fatal(err)
		}
		record := stageprovider.DeploymentOperationCatalogLockRecord{
			Stage: registration.Stage, Provider: registration.Provider, Operation: registration.Operation.Clone(), Runtime: registration.Runtime,
			Checkout: registration.Checkout, Secrets: append([]workflowadapter.SecretReference{}, registration.Secrets...),
			PromptContentFingerprint: workflowkit.SHA256Fingerprint(prompt), SchemaContentFingerprint: workflowkit.SHA256Fingerprint(schema),
			ExecutionKind: registration.Operation.Payload.Kind(),
			StandardAuthoringContract: &stageprovider.StandardAuthoringContractLock{
				Format: stageprovider.StandardAuthoringContractLockFormat, Version: stageprovider.StandardAuthoringContractLockVersion,
				Prompt: asset.Prompt, Schema: asset.Schema,
			},
		}
		switch payload := registration.Operation.Payload.(type) {
		case workflowadapter.LocalCommandOperationPayload:
			git := lockedGit
			record.LocalExecutable = &git
		case workflowadapter.AgentTurnOperationPayload:
			record.AgentModel = &stageprovider.AgentModelLock{
				AgentID: payload.AgentID, AgentVersion: "0.133.0", ModelID: payload.ModelID,
				ModelVersion: "gpt-5.6-terra",
			}
		case workflowadapter.DurableReviewOperationPayload:
			record.DurableReviewPolicy = &stageprovider.DurableReviewPolicyLock{PolicyID: payload.PolicyID, Version: "1.0.0"}
		case workflowadapter.HarborBuiltinOperationPayload:
			record.HarborFlowBuiltin = &stageprovider.HarborFlowBuiltinOperationLock{Format: stageprovider.HarborFlowBuiltinOperationLockFormat, Version: stageprovider.HarborFlowBuiltinOperationLockVersion, HandlerID: payload.HandlerID, HandlerVersion: "1.0.0"}
		default:
			t.Fatalf("unsupported Standard authoring payload %T", payload)
		}
		operations = append(operations, record)
	}
	lock := stageprovider.DeploymentOperationCatalogLock{
		Format: stageprovider.DeploymentOperationCatalogLockFormat, Version: stageprovider.DeploymentOperationCatalogLockVersion,
		LockID: "standard-authoring-composition-test", LockVersion: "1.0.0", CatalogReceipt: catalog.Receipt(),
		HarborFlowBuild:                   stageprovider.HarborFlowBuildIdentity{Module: "github.com/purplevoid/harbor-factory", Version: "v2.0.0", Commit: strings.Repeat("a", 40), ContentSHA256: workflowkit.SHA256Fingerprint([]byte("standard-authoring-composition-test"))},
		StandardAuthoringExecutionProfile: &stageprovider.StandardAuthoringExecutionProfileLock{Profile: standardAuthoringProductionTestProfile(t, catalog.Template())},
		StandardAuthoringSSHTransport:     standardAuthoringProductionTestSSHTransport(t, deploymentRoot),
		Operations:                        operations,
	}
	canonical, err := lock.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deploymentRoot, "operation-catalog.lock.json"), canonical, 0o600); err != nil {
		t.Fatal(err)
	}
	return deploymentRoot, catalog, lock
}

func standardAuthoringProductionTestProfile(t *testing.T, reference workflowadapter.TemplateReference) workflowadapter.ExecutionProfile {
	t.Helper()
	template, err := workflowadapter.ResolveWorkflowTemplate(reference)
	if err != nil {
		t.Fatal(err)
	}
	profile := workflowadapter.ExecutionProfile{
		Template: template.Reference(), ID: "standard-authoring-composition-test", Version: "1.0.0",
		ContinuationPlanTTL: workflowadapter.RequiredContinuationPlanTTL, ControlGracePeriod: 30 * time.Second,
		CandidateProviderBudget: workflowadapter.CandidateProviderBudget{AttemptTimeout: time.Minute},
		Stages:                  make([]workflowadapter.StageBudget, 0, len(template.Catalog.Stages)),
	}
	for _, stage := range template.Catalog.Stages {
		turns := max(1, stage.RequiredTurns)
		attempt := time.Duration(turns) * time.Second
		profile.Stages = append(profile.Stages, workflowadapter.StageBudget{StageKey: stage.Key, Budget: workflowkit.ExecutionBudget{
			TurnTimeout: time.Second, MaxTurns: turns, AttemptTimeout: attempt, MaxAttempts: 1, MaxElapsed: attempt,
		}})
	}
	if err := profile.Validate(); err != nil {
		t.Fatalf("Standard authoring test profile: %v", err)
	}
	return profile
}

func standardAuthoringProductionTestGit(t *testing.T) stageprovider.LocalExecutableLock {
	t.Helper()
	path, err := exec.LookPath("git")
	if err != nil {
		t.Skip("Git is unavailable")
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command(path, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	version := strings.TrimSpace(strings.TrimPrefix(string(output), "git version "))
	if version == "" || version == string(output) {
		t.Fatalf("unexpected Git version %q", output)
	}
	return stageprovider.LocalExecutableLock{CommandID: stageprovider.StandardAuthoringGitSnapshotCommandID, AbsolutePath: path, Version: version, ContentSHA256: workflowkit.SHA256Fingerprint(contents)}
}

func standardAuthoringProductionTestSSHTransport(t *testing.T, deploymentRoot string) *stageprovider.StandardAuthoringSSHTransportLock {
	t.Helper()
	knownHosts, err := os.ReadFile(filepath.Join(deploymentRoot, filepath.FromSlash(stageprovider.StandardAuthoringSSHKnownHostsRelativePath)))
	if err != nil {
		t.Fatal(err)
	}
	sshContent := workflowkit.SHA256Fingerprint([]byte("standard-authoring-test-ssh"))
	shellContent := workflowkit.SHA256Fingerprint([]byte("standard-authoring-test-shell"))
	return &stageprovider.StandardAuthoringSSHTransportLock{
		Format:  stageprovider.StandardAuthoringSSHTransportLockFormat,
		Version: stageprovider.StandardAuthoringSSHTransportLockVersion,
		SSHExecutable: stageprovider.LocalExecutableLock{
			CommandID: stageprovider.StandardAuthoringSSHTransportCommandID, AbsolutePath: "/opt/standard-authoring-test/ssh", Version: "OpenSSH_10.0p2", ContentSHA256: sshContent,
		},
		WrapperShell: stageprovider.LocalExecutableLock{
			CommandID: stageprovider.StandardAuthoringSSHWrapperShellCommandID, AbsolutePath: "/opt/standard-authoring-test/dash", Version: string(shellContent), ContentSHA256: shellContent,
		},
		KnownHosts:                 stageprovider.StandardAuthoringSSHKnownHostsLock{Format: stageprovider.StandardAuthoringSSHKnownHostsLockFormat, Version: stageprovider.StandardAuthoringSSHKnownHostsLockVersion, RelativePath: stageprovider.StandardAuthoringSSHKnownHostsRelativePath, ContentSHA256: workflowkit.SHA256Fingerprint(knownHosts)},
		AgentSocketEnvironmentName: stageprovider.StandardAuthoringSSHAgentSocketEnvironment,
	}
}

func standardAuthoringProductionRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate Standard authoring composition test")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), ".."))
}

func copyStandardAuthoringDeploymentTree(t *testing.T, source, destination string) {
	t.Helper()
	err := filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		info, err := entry.Info()
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return os.ErrInvalid
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, contents, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}
