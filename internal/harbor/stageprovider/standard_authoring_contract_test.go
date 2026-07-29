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

func TestStandardAuthoringContractLockStrictlyMapsAssetsToExistingFingerprints(t *testing.T) {
	_, lock, _ := standardAuthoringProviderCompositionFixture(t)
	if err := lock.Validate(); err != nil {
		t.Fatalf("valid Standard authoring contract lock: %v", err)
	}

	canonical, err := lock.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseDeploymentOperationCatalogLockJSON(canonical)
	if err != nil {
		t.Fatalf("parse canonical Standard authoring lock: %v", err)
	}
	if parsed.Operations[0].StandardAuthoringContract == nil {
		t.Fatal("parsed Standard authoring contract was lost")
	}
	var direct DeploymentOperationCatalogLock
	if err := json.Unmarshal(canonical, &direct); err != nil {
		t.Fatalf("direct strict parse: %v", err)
	}

	withAdditional := lock.Clone()
	additional := StandardAuthoringContractAdditionalSchemaLock{
		StandardAuthoringContractAssetReference: StandardAuthoringContractAssetReference{ID: "standard-authoring.additional-schema", Version: "1.0.0", RelativePath: "schemas/additional-schema.json"},
		ContentSHA256:                           workflowkit.SHA256Fingerprint([]byte("additional schema")),
	}
	withAdditional.Operations[0].StandardAuthoringContract.AdditionalSchemas = []StandardAuthoringContractAdditionalSchemaLock{additional}
	if err := withAdditional.Validate(); err != nil {
		t.Fatalf("valid additional schema contract lock: %v", err)
	}

	unknownNested := []byte(strings.Replace(string(canonical), `"relative_path":"prompts/materialize-task.md"`, `"relative_path":"prompts/materialize-task.md","unexpected":true`, 1))
	if _, err := ParseDeploymentOperationCatalogLockJSON(unknownNested); err == nil || !errors.Is(err, ErrInvalidDeploymentOperationCatalogLock) {
		t.Fatalf("unknown Standard authoring contract field = %v, want invalid lock", err)
	}

	for name, mutate := range map[string]func(*DeploymentOperationCatalogLock){
		"missing contract": func(candidate *DeploymentOperationCatalogLock) {
			candidate.Operations[0].StandardAuthoringContract = nil
		},
		"path traversal": func(candidate *DeploymentOperationCatalogLock) {
			candidate.Operations[0].StandardAuthoringContract.Prompt.RelativePath = "../outside.md"
		},
		"noncanonical asset id": func(candidate *DeploymentOperationCatalogLock) {
			candidate.Operations[0].StandardAuthoringContract.Prompt.ID = "Standard-Authoring.Prompt"
		},
		"unversioned asset": func(candidate *DeploymentOperationCatalogLock) {
			candidate.Operations[0].StandardAuthoringContract.Schema.Version = "latest"
		},
		"same asset conflicts with hash": func(candidate *DeploymentOperationCatalogLock) {
			candidate.Operations[1].StandardAuthoringContract.Prompt = candidate.Operations[0].StandardAuthoringContract.Prompt.Clone()
			candidate.Operations[1].PromptContentFingerprint = workflowkit.SHA256Fingerprint([]byte("different prompt bytes"))
		},
		"additional schema conflicts with hash": func(candidate *DeploymentOperationCatalogLock) {
			candidate.Operations[0].StandardAuthoringContract.AdditionalSchemas = []StandardAuthoringContractAdditionalSchemaLock{additional}
			conflict := additional.Clone()
			conflict.ContentSHA256 = workflowkit.SHA256Fingerprint([]byte("different additional schema"))
			candidate.Operations[1].StandardAuthoringContract.AdditionalSchemas = []StandardAuthoringContractAdditionalSchemaLock{conflict}
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := lock.Clone()
			mutate(&candidate)
			if err := candidate.Validate(); err == nil || !errors.Is(err, ErrInvalidDeploymentOperationCatalogLock) {
				t.Fatalf("contract mutation validation = %v, want invalid lock", err)
			}
		})
	}

	_, nonStandard, _ := operationCatalogLockFixture(t, workflowadapter.RepoPrepare)
	contract := lock.Operations[0].StandardAuthoringContract.Clone()
	nonStandard.Operations[0].StandardAuthoringContract = &contract
	if err := nonStandard.Validate(); err == nil || !errors.Is(err, ErrInvalidDeploymentOperationCatalogLock) {
		t.Fatalf("non-Standard lock accepted Standard authoring contract = %v", err)
	}
}

func TestStandardAuthoringRuntimeAttestorReattestsLockedAssetsBeforeEveryEffect(t *testing.T) {
	catalog, lock, resolutions := standardAuthoringProviderCompositionFixture(t)
	contractRoot := t.TempDir()
	writeStandardAuthoringContractAssets(t, contractRoot, lock)
	attestor, err := NewStandardAuthoringRuntimeAttestor(StandardAuthoringRuntimeAttestorConfig{
		HarborFlowBuild: lock.HarborFlowBuild,
		ContractRoot:    contractRoot,
	})
	if err != nil {
		t.Fatal(err)
	}

	calls := 0
	composition, err := NewStandardAuthoringProviderComposition(StandardAuthoringProviderCompositionConfig{
		Template: workflowadapter.StandardAuthoringCurrentTemplateReference(), Catalog: catalog, Lock: lock, Attestor: attestor,
		Handlers: StandardAuthoringOperationHandlers{
			HarborBuiltin: HarborBuiltinOperationExecutorFunc(func(context.Context, StageOperationInvocation, workflowadapter.HarborBuiltinOperationPayload) (workflowkit.StageExecutionResult, error) {
				calls++
				return workflowkit.StageExecutionResult{}, nil
			}),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err := composition.Verifier.VerifyStageOperation(resolutions[0])
	if err != nil {
		t.Fatal(err)
	}
	assets, err := attestor.ReadStandardAuthoringContractAssets(context.Background(), DeploymentOperationRuntimeAttestation{
		CatalogReceipt: composition.Verifier.CatalogReceipt(), LockIdentity: composition.Verifier.LockIdentity(), HarborFlowBuild: composition.Verifier.HarborFlowBuild(),
		Record: record, Resolution: resolutions[0],
	})
	if err != nil {
		t.Fatalf("read verified Standard authoring assets: %v", err)
	}
	if assets.Prompt.ID != "standard-authoring.materialize-task.prompt" || string(assets.Prompt.Content) != "builtin-prompt" || string(assets.Schema.Content) != "builtin-schema" {
		t.Fatalf("verified assets = %+v, want exact lock-bound prompt/schema bytes", assets)
	}
	assets.Prompt.Content[0] = 'X'
	secondRead, err := attestor.ReadStandardAuthoringContractAssets(context.Background(), DeploymentOperationRuntimeAttestation{
		CatalogReceipt: composition.Verifier.CatalogReceipt(), LockIdentity: composition.Verifier.LockIdentity(), HarborFlowBuild: composition.Verifier.HarborFlowBuild(),
		Record: record, Resolution: resolutions[0],
	})
	if err != nil || string(secondRead.Prompt.Content) != "builtin-prompt" {
		t.Fatalf("asset read did not return independently owned verified bytes: assets=%+v error=%v", secondRead, err)
	}
	executor, err := composition.Resolver.ResolveWorkflowkitStageOperation(resolutions[0])
	if err != nil {
		t.Fatal(err)
	}
	request := workflowkit.StageExecutionRequest{Stage: workflowkit.StageDescriptor{Key: resolutions[0].StageKey, Plugin: resolutions[0].Plugin}}
	if _, err := executor.ExecuteStage(context.Background(), request); err != nil {
		t.Fatalf("first effect with exact assets: %v", err)
	}
	if calls != 1 {
		t.Fatalf("builtin calls after exact assets = %d, want 1", calls)
	}

	contract := lock.Operations[0].StandardAuthoringContract.Clone()
	promptPath := filepath.Join(contractRoot, filepath.FromSlash(contract.Prompt.RelativePath))
	if err := os.WriteFile(promptPath, []byte("prompt drift"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := executor.ExecuteStage(context.Background(), request); err == nil || !errors.Is(err, ErrDeploymentOperationRuntimeAttestationFailed) {
		t.Fatalf("second effect after prompt drift = %v, want attestation failure", err)
	}
	if calls != 1 {
		t.Fatalf("drifted asset reached builtin handler: calls = %d, want 1", calls)
	}
}

func TestStandardAuthoringRuntimeAttestorRejectsContractRootAndAssetSymlinks(t *testing.T) {
	catalog, lock, resolutions := standardAuthoringProviderCompositionFixture(t)
	contractRoot := t.TempDir()
	writeStandardAuthoringContractAssets(t, contractRoot, lock)
	rootLink := filepath.Join(t.TempDir(), "contract-root-link")
	if err := os.Symlink(contractRoot, rootLink); err != nil {
		t.Skipf("create contract-root symlink: %v", err)
	}
	if _, err := NewStandardAuthoringRuntimeAttestor(StandardAuthoringRuntimeAttestorConfig{HarborFlowBuild: lock.HarborFlowBuild, ContractRoot: rootLink}); err == nil || !errors.Is(err, ErrDeploymentOperationRuntimeAttestationFailed) {
		t.Fatalf("symlink contract root = %v, want attestation failure", err)
	}

	verifier, err := NewDeploymentOperationCatalogLockResolver(catalog, lock)
	if err != nil {
		t.Fatal(err)
	}
	record, err := verifier.VerifyStageOperation(resolutions[0])
	if err != nil {
		t.Fatal(err)
	}
	attestation := DeploymentOperationRuntimeAttestation{
		CatalogReceipt: verifier.CatalogReceipt(), LockIdentity: verifier.LockIdentity(), HarborFlowBuild: verifier.HarborFlowBuild(),
		Record: record, Resolution: resolutions[0],
	}
	attestor, err := NewStandardAuthoringRuntimeAttestor(StandardAuthoringRuntimeAttestorConfig{HarborFlowBuild: lock.HarborFlowBuild, ContractRoot: contractRoot})
	if err != nil {
		t.Fatal(err)
	}
	contract := record.StandardAuthoringContract.Clone()
	schemaPath := filepath.Join(contractRoot, filepath.FromSlash(contract.Schema.RelativePath))
	outside := filepath.Join(t.TempDir(), "schema.json")
	if err := os.WriteFile(outside, []byte("builtin-schema"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(schemaPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, schemaPath); err != nil {
		t.Skipf("create schema symlink: %v", err)
	}
	if err := attestor.AttestDeploymentOperation(context.Background(), attestation); err == nil || !errors.Is(err, ErrDeploymentOperationRuntimeAttestationFailed) {
		t.Fatalf("symlink contract asset = %v, want attestation failure", err)
	}
}
