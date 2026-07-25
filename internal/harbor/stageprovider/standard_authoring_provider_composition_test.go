package stageprovider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/testsupport"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestStandardAuthoringProviderCompositionBindsBuiltinAndReviewWithoutFallback(t *testing.T) {
	catalog, lock, resolutions := standardAuthoringProviderCompositionFixture(t)
	contractRoot := t.TempDir()
	writeStandardAuthoringContractAssets(t, contractRoot, lock)
	attestor, err := NewStandardAuthoringRuntimeAttestor(StandardAuthoringRuntimeAttestorConfig{HarborFlowBuild: lock.HarborFlowBuild, ContractRoot: contractRoot})
	if err != nil {
		t.Fatal(err)
	}
	called := false
	composition, err := NewStandardAuthoringProviderComposition(StandardAuthoringProviderCompositionConfig{
		Template: workflowadapter.StandardAuthoringCurrentTemplateReference(), Catalog: catalog, Lock: lock, Attestor: attestor,
		Handlers: StandardAuthoringOperationHandlers{
			HarborBuiltin: HarborBuiltinOperationExecutorFunc(func(_ context.Context, invocation StageOperationInvocation, payload workflowadapter.HarborBuiltinOperationPayload) (workflowkit.StageExecutionResult, error) {
				called = invocation.Resolution.StageKey == workflowadapter.MaterializeTask && payload.HandlerID == "standard-authoring.materialize-task"
				return workflowkit.StageExecutionResult{}, nil
			}),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !composition.Catalog.Template().Equal(workflowadapter.StandardAuthoringCurrentTemplateReference()) || !composition.Verifier.CatalogReceipt().Template.Equal(workflowadapter.StandardAuthoringCurrentTemplateReference()) {
		t.Fatalf("composition did not retain exact Standard authoring template")
	}
	for _, resolution := range resolutions {
		if err := composition.Resolver.ValidateStageOperation(resolution); err != nil {
			t.Fatalf("validate stage %q: %v", resolution.StageKey, err)
		}
	}
	builtinExecutor, err := composition.Resolver.ResolveWorkflowkitStageOperation(resolutions[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := builtinExecutor.ExecuteStage(context.Background(), workflowkit.StageExecutionRequest{Stage: workflowkit.StageDescriptor{Key: resolutions[0].StageKey, Plugin: resolutions[0].Plugin}}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("catalog-locked built-in handler was not invoked")
	}

	drifted := resolutions[0].Clone()
	drifted.Operation.Payload = workflowadapter.HarborBuiltinOperationPayload{HandlerID: "standard-authoring.other"}
	if err := composition.Resolver.ValidateStageOperation(drifted); err == nil {
		t.Fatal("drifted built-in handler was accepted")
	}
}

func TestStandardAuthoringProviderCompositionRejectsDifferentTemplateAndContainerOperation(t *testing.T) {
	catalog, lock, _ := standardAuthoringProviderCompositionFixture(t)
	contractRoot := t.TempDir()
	writeStandardAuthoringContractAssets(t, contractRoot, lock)
	attestor, err := NewStandardAuthoringRuntimeAttestor(StandardAuthoringRuntimeAttestorConfig{HarborFlowBuild: lock.HarborFlowBuild, ContractRoot: contractRoot})
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewStandardAuthoringProviderComposition(StandardAuthoringProviderCompositionConfig{
		Template: workflowadapter.CodeEdgePhase1TemplateReference(), Catalog: catalog, Lock: lock, Attestor: attestor,
		Handlers: StandardAuthoringOperationHandlers{HarborBuiltin: HarborBuiltinOperationExecutorFunc(func(context.Context, StageOperationInvocation, workflowadapter.HarborBuiltinOperationPayload) (workflowkit.StageExecutionResult, error) {
			return workflowkit.StageExecutionResult{}, nil
		})},
	})
	if err == nil {
		t.Fatal("different configured template was accepted")
	}
}

func TestStandardAuthoringProviderCompositionInjectsAttestedCodexBridgeWhenNoAgentHandlerIsSupplied(t *testing.T) {
	catalog, lock, resolution := standardAuthoringAgentOnlyCompositionFixture(t)
	contractRoot := t.TempDir()
	writeStandardAuthoringContractAssets(t, contractRoot, lock)
	attestor, err := NewStandardAuthoringRuntimeAttestor(StandardAuthoringRuntimeAttestorConfig{HarborFlowBuild: lock.HarborFlowBuild, ContractRoot: contractRoot})
	if err != nil {
		t.Fatal(err)
	}
	composition, err := NewStandardAuthoringProviderComposition(StandardAuthoringProviderCompositionConfig{
		Template: workflowadapter.StandardAuthoringCurrentTemplateReference(), Catalog: catalog, Lock: lock, Attestor: attestor,
		CodexWorkspaceRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("compose deployment-owned Codex bridge: %v", err)
	}
	if err := composition.Resolver.ValidateStageOperation(resolution); err != nil {
		t.Fatalf("auto-injected Codex bridge did not validate frozen agent operation: %v", err)
	}

	// A caller-provided handler remains a deliberate test/deployment seam. Its
	// presence must prevent composition from requiring a Codex workspace for
	// the automatic bridge, rather than silently constructing two handlers.
	_, err = NewStandardAuthoringProviderComposition(StandardAuthoringProviderCompositionConfig{
		Template: workflowadapter.StandardAuthoringCurrentTemplateReference(), Catalog: catalog, Lock: lock, Attestor: attestor,
		Handlers: StandardAuthoringOperationHandlers{AgentTurn: AgentTurnOperationExecutorFunc(func(context.Context, StageOperationInvocation, workflowadapter.AgentTurnOperationPayload) (workflowkit.StageExecutionResult, error) {
			return workflowkit.StageExecutionResult{}, nil
		})},
	})
	if err != nil {
		t.Fatalf("explicit agent handler should remain an injection seam: %v", err)
	}
}

func TestStandardAuthoringProviderCompositionRejectsDriftingCodexConfigurationAfterAuditLoad(t *testing.T) {
	catalog, lock, _ := standardAuthoringAgentOnlyCompositionFixture(t)
	legacyDocument := catalog.Catalog()
	legacyPayload := legacyDocument.Operations[0].Operation.Payload.(workflowadapter.AgentTurnOperationPayload)
	legacyPayload.ModelID = "gpt-5.5"
	legacyPayload.ReasoningEffort = ""
	legacyDocument.Operations[0].Operation.Payload = legacyPayload
	legacyCatalog, err := NewDeploymentOperationCatalogResolver(legacyDocument)
	if err != nil {
		t.Fatalf("load historical Standard catalog: %v", err)
	}
	legacyLock := lock.Clone()
	legacyLock.CatalogReceipt = legacyCatalog.Receipt()
	legacyLock.Operations[0].Operation.Payload = legacyPayload
	legacyLock.Operations[0].AgentModel.ModelID = legacyPayload.ModelID
	if _, err := NewDeploymentOperationCatalogLockResolver(legacyCatalog, legacyLock); err != nil {
		t.Fatalf("load historical Standard lock for audit: %v", err)
	}

	contractRoot := t.TempDir()
	writeStandardAuthoringContractAssets(t, contractRoot, legacyLock)
	attestor, err := NewStandardAuthoringRuntimeAttestor(StandardAuthoringRuntimeAttestorConfig{HarborFlowBuild: legacyLock.HarborFlowBuild, ContractRoot: contractRoot})
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewStandardAuthoringProviderComposition(StandardAuthoringProviderCompositionConfig{
		Template: workflowadapter.StandardAuthoringCurrentTemplateReference(), Catalog: legacyCatalog, Lock: legacyLock, Attestor: attestor,
		Handlers: StandardAuthoringOperationHandlers{AgentTurn: AgentTurnOperationExecutorFunc(func(context.Context, StageOperationInvocation, workflowadapter.AgentTurnOperationPayload) (workflowkit.StageExecutionResult, error) {
			return workflowkit.StageExecutionResult{}, nil
		})},
	})
	if err == nil || !errors.Is(err, ErrDeploymentOperationCatalogDrift) {
		t.Fatalf("legacy Standard configuration composition error = %v, want catalog drift", err)
	}
}

func standardAuthoringAgentOnlyCompositionFixture(t *testing.T) (*DeploymentOperationCatalogResolver, DeploymentOperationCatalogLock, workflowadapter.StageOperationResolution) {
	t.Helper()
	fixture := newCodexAppServerAttestationFixture(t)
	specification := testsupport.CompleteRunExecutionSpec(
		"018f0a73-3b49-7000-8000-0000000000a1",
		"018f0a73-3b49-7000-8000-0000000000a2",
		"harbor.task.v2:sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	)
	resolution, err := specification.ResolveStageOperation(workflowadapter.RepoAnalyze)
	if err != nil {
		t.Fatal(err)
	}
	definition, found := workflowadapter.StandardAuthoringContractStageCatalog().Stage(workflowadapter.RepoAnalyze)
	if !found {
		t.Fatal("missing Standard authoring repo_analyze stage")
	}
	provider := workflowadapter.ProviderReference{ID: StandardAuthoringProviderID, Kind: StandardAuthoringProviderKind, Version: StandardAuthoringProviderVersion}
	payload := workflowadapter.AgentTurnOperationPayload{
		AgentID: "codex-app-server", ModelID: CodexAppServerProductionModelID,
		ReasoningEffort: CodexAppServerProductionReasoningEffort, MaxTurns: 3,
	}
	resolution.Provider = provider
	resolution.Operation = workflowadapter.StageOperationBinding{ProviderID: provider.ID, OperationID: "standard-authoring.codex.repo-analyze", Version: "1.0.0", Payload: payload}
	registration := DeploymentOperationRegistration{
		Stage:    DeploymentStageContract{Key: resolution.StageKey, Type: resolution.StageType, Group: definition.Group, Plugin: resolution.Plugin},
		Provider: resolution.Provider, Operation: resolution.Operation.Clone(), Runtime: resolution.Runtime,
		Checkout: DeploymentCheckoutContract{ID: resolution.Checkout.ID, Purpose: "authoring-source-snapshot"}, Secrets: append([]workflowadapter.SecretReference{}, resolution.Secrets...),
	}
	catalog, err := NewDeploymentOperationCatalogResolver(DeploymentOperationCatalog{
		Format: DeploymentOperationCatalogFormat, Version: DeploymentOperationCatalogVersion,
		CatalogID: "standard-authoring-agent-only-composition-test", CatalogVersion: "test-v1",
		Template: workflowadapter.StandardAuthoringCurrentTemplateReference(), Operations: []DeploymentOperationRegistration{registration},
	})
	if err != nil {
		t.Fatal(err)
	}
	codex := fixture.lock.Clone()
	lock := DeploymentOperationCatalogLock{
		Format: DeploymentOperationCatalogLockFormat, Version: DeploymentOperationCatalogLockVersion,
		LockID: "standard-authoring-agent-only-composition-test-lock", LockVersion: "test-v1", CatalogReceipt: catalog.Receipt(), HarborFlowBuild: fixture.attestation.HarborFlowBuild,
		StandardAuthoringExecutionProfile: &StandardAuthoringExecutionProfileLock{Profile: standardAuthoringTestExecutionProfile(t)},
		StandardAuthoringSSHTransport:     standardAuthoringSSHTransportTestLock(t, []byte(standardAuthoringSSHTransportTestKnownHosts)),
		Operations: []DeploymentOperationCatalogLockRecord{{
			Stage: registration.Stage, Provider: registration.Provider, Operation: registration.Operation.Clone(), Runtime: registration.Runtime,
			Checkout: registration.Checkout, Secrets: append([]workflowadapter.SecretReference{}, registration.Secrets...),
			PromptContentFingerprint: workflowkit.SHA256Fingerprint([]byte("agent-prompt")), SchemaContentFingerprint: workflowkit.SHA256Fingerprint([]byte("agent-schema")),
			ExecutionKind: workflowadapter.StageOperationPayloadAgentTurn,
			AgentModel: &AgentModelLock{
				AgentID: payload.AgentID, AgentVersion: "0.133.0", ModelID: payload.ModelID,
				ModelVersion: "gpt-5.6-terra",
			},
			CodexAppServer: &codex,
			StandardAuthoringContract: &StandardAuthoringContractLock{Format: StandardAuthoringContractLockFormat, Version: StandardAuthoringContractLockVersion,
				Prompt: StandardAuthoringContractAssetReference{ID: "standard-authoring.repo-analyze.prompt", Version: "1.0.0", RelativePath: "prompts/repo-analyze.json"},
				Schema: StandardAuthoringContractAssetReference{ID: "standard-authoring.codex-stage-output-schema", Version: "1.0.0", RelativePath: "schemas/codex-stage-output.schema.json"}},
		}},
	}
	return catalog, lock, resolution
}

func standardAuthoringProviderCompositionFixture(t *testing.T) (*DeploymentOperationCatalogResolver, DeploymentOperationCatalogLock, []workflowadapter.StageOperationResolution) {
	t.Helper()
	specification := testsupport.CompleteRunExecutionSpec(
		"018f0a73-3b49-7000-8000-000000000091",
		"018f0a73-3b49-7000-8000-000000000092",
		"harbor.task.v2:sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	)
	builtin, err := specification.ResolveStageOperation(workflowadapter.MaterializeTask)
	if err != nil {
		t.Fatal(err)
	}
	review, err := specification.ResolveStageOperation(workflowadapter.TaskReview)
	if err != nil {
		t.Fatal(err)
	}
	provider := workflowadapter.ProviderReference{ID: StandardAuthoringProviderID, Kind: StandardAuthoringProviderKind, Version: StandardAuthoringProviderVersion}
	builtin.Provider, review.Provider = provider, provider
	builtin.Operation.ProviderID, review.Operation.ProviderID = provider.ID, provider.ID
	builtin.Operation.Payload = workflowadapter.HarborBuiltinOperationPayload{HandlerID: "standard-authoring.materialize-task"}

	standard := workflowadapter.StandardAuthoringContractStageCatalog()
	registrations := make([]DeploymentOperationRegistration, 0, 2)
	for _, resolution := range []workflowadapter.StageOperationResolution{builtin, review} {
		definition, found := standard.Stage(resolution.StageKey)
		if !found {
			t.Fatalf("standard stage %q is missing", resolution.StageKey)
		}
		registrations = append(registrations, DeploymentOperationRegistration{
			Stage:    DeploymentStageContract{Key: resolution.StageKey, Type: resolution.StageType, Group: definition.Group, Plugin: resolution.Plugin},
			Provider: resolution.Provider, Operation: resolution.Operation.Clone(), Runtime: resolution.Runtime,
			Checkout: DeploymentCheckoutContract{ID: resolution.Checkout.ID, Purpose: "authoring-source-snapshot"},
			Secrets:  append([]workflowadapter.SecretReference{}, resolution.Secrets...),
		})
	}
	document := DeploymentOperationCatalog{
		Format: DeploymentOperationCatalogFormat, Version: DeploymentOperationCatalogVersion,
		CatalogID: "standard-authoring-provider-composition-test", CatalogVersion: "test-v1",
		Template: workflowadapter.StandardAuthoringCurrentTemplateReference(), Operations: registrations,
	}
	catalog, err := NewDeploymentOperationCatalogResolver(document)
	if err != nil {
		t.Fatal(err)
	}
	lock := DeploymentOperationCatalogLock{
		Format: DeploymentOperationCatalogLockFormat, Version: DeploymentOperationCatalogLockVersion,
		LockID: "standard-authoring-provider-composition-test-lock", LockVersion: "test-v1", CatalogReceipt: catalog.Receipt(),
		HarborFlowBuild: HarborFlowBuildIdentity{
			Module: "github.com/purplevoid/harbor-factory", Version: "v2.0.0", Commit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			ContentSHA256: workflowkit.SHA256Fingerprint([]byte("standard-authoring-provider-composition")),
		},
		StandardAuthoringExecutionProfile: &StandardAuthoringExecutionProfileLock{Profile: standardAuthoringTestExecutionProfile(t)},
		StandardAuthoringSSHTransport:     standardAuthoringSSHTransportTestLock(t, []byte(standardAuthoringSSHTransportTestKnownHosts)),
		Operations: []DeploymentOperationCatalogLockRecord{
			{
				Stage: builtinStageContract(t, builtin), Provider: builtin.Provider, Operation: builtin.Operation.Clone(), Runtime: builtin.Runtime,
				Checkout: DeploymentCheckoutContract{ID: builtin.Checkout.ID, Purpose: "authoring-source-snapshot"}, Secrets: append([]workflowadapter.SecretReference{}, builtin.Secrets...),
				PromptContentFingerprint: workflowkit.SHA256Fingerprint([]byte("builtin-prompt")), SchemaContentFingerprint: workflowkit.SHA256Fingerprint([]byte("builtin-schema")),
				ExecutionKind:     workflowadapter.StageOperationPayloadHarborBuiltin,
				HarborFlowBuiltin: &HarborFlowBuiltinOperationLock{Format: HarborFlowBuiltinOperationLockFormat, Version: HarborFlowBuiltinOperationLockVersion, HandlerID: "standard-authoring.materialize-task", HandlerVersion: "1.0.0"},
				StandardAuthoringContract: &StandardAuthoringContractLock{
					Format: StandardAuthoringContractLockFormat, Version: StandardAuthoringContractLockVersion,
					Prompt: StandardAuthoringContractAssetReference{ID: "standard-authoring.materialize-task.prompt", Version: "1.0.0", RelativePath: "prompts/materialize-task.md"},
					Schema: StandardAuthoringContractAssetReference{ID: "standard-authoring.materialize-task.schema", Version: "1.0.0", RelativePath: "schemas/materialize-task.json"},
				},
			},
			{
				Stage: builtinStageContract(t, review), Provider: review.Provider, Operation: review.Operation.Clone(), Runtime: review.Runtime,
				Checkout: DeploymentCheckoutContract{ID: review.Checkout.ID, Purpose: "authoring-source-snapshot"}, Secrets: append([]workflowadapter.SecretReference{}, review.Secrets...),
				PromptContentFingerprint: workflowkit.SHA256Fingerprint([]byte("review-prompt")), SchemaContentFingerprint: workflowkit.SHA256Fingerprint([]byte("review-schema")),
				ExecutionKind:       workflowadapter.StageOperationPayloadDurableReview,
				DurableReviewPolicy: &DurableReviewPolicyLock{PolicyID: review.Operation.Payload.(workflowadapter.DurableReviewOperationPayload).PolicyID, Version: "1.0.0"},
				StandardAuthoringContract: &StandardAuthoringContractLock{
					Format: StandardAuthoringContractLockFormat, Version: StandardAuthoringContractLockVersion,
					Prompt: StandardAuthoringContractAssetReference{ID: "standard-authoring.task-review.prompt", Version: "1.0.0", RelativePath: "prompts/task-review.md"},
					Schema: StandardAuthoringContractAssetReference{ID: "standard-authoring.task-review.schema", Version: "1.0.0", RelativePath: "schemas/task-review.json"},
				},
			},
		},
	}
	return catalog, lock, []workflowadapter.StageOperationResolution{builtin, review}
}

func writeStandardAuthoringContractAssets(t *testing.T, root string, lock DeploymentOperationCatalogLock) {
	t.Helper()
	for _, record := range lock.Operations {
		if record.StandardAuthoringContract == nil {
			continue
		}
		contract := record.StandardAuthoringContract.Clone()
		for _, asset := range []struct {
			reference StandardAuthoringContractAssetReference
			contents  []byte
		}{
			{reference: contract.Prompt, contents: []byte("builtin-prompt")},
			{reference: contract.Schema, contents: []byte("builtin-schema")},
		} {
			if record.Stage.Key == workflowadapter.TaskReview {
				if asset.reference == contract.Prompt {
					asset.contents = []byte("review-prompt")
				} else {
					asset.contents = []byte("review-schema")
				}
			}
			path := filepath.Join(root, filepath.FromSlash(asset.reference.RelativePath))
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, asset.contents, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func builtinStageContract(t *testing.T, resolution workflowadapter.StageOperationResolution) DeploymentStageContract {
	t.Helper()
	definition, found := workflowadapter.StandardAuthoringContractStageCatalog().Stage(resolution.StageKey)
	if !found {
		t.Fatalf("stage %q is missing", resolution.StageKey)
	}
	return DeploymentStageContract{Key: resolution.StageKey, Type: resolution.StageType, Group: definition.Group, Plugin: resolution.Plugin}
}

func standardAuthoringTestExecutionProfile(t *testing.T) workflowadapter.ExecutionProfile {
	return standardAuthoringTestExecutionProfileForTemplate(t, workflowadapter.StandardAuthoringCurrentTemplateReference())
}

func standardAuthoringTestExecutionProfileForTemplate(t *testing.T, reference workflowadapter.TemplateReference) workflowadapter.ExecutionProfile {
	t.Helper()
	template, err := workflowadapter.ResolveWorkflowTemplate(reference)
	if err != nil {
		t.Fatalf("resolve Standard authoring test template %s@%s: %v", reference.ID, reference.Version, err)
	}
	profile := workflowadapter.ExecutionProfile{
		Template:                template.Reference(),
		ID:                      "standard-authoring-lock-test",
		Version:                 "1.0.0",
		ContinuationPlanTTL:     workflowadapter.RequiredContinuationPlanTTL,
		ControlGracePeriod:      time.Minute,
		CandidateProviderBudget: workflowadapter.CandidateProviderBudget{AttemptTimeout: 5 * time.Minute},
		Stages:                  make([]workflowadapter.StageBudget, 0, len(template.Catalog.Stages)),
	}
	for _, stage := range template.Catalog.Stages {
		turns := stage.RequiredTurns
		if turns < 1 {
			turns = 1
		}
		attempt := time.Duration(turns) * time.Minute
		profile.Stages = append(profile.Stages, workflowadapter.StageBudget{
			StageKey: stage.Key,
			Budget: workflowkit.ExecutionBudget{
				TurnTimeout:    time.Minute,
				MaxTurns:       turns,
				AttemptTimeout: attempt,
				MaxAttempts:    1,
				MaxElapsed:     attempt,
			},
		})
	}
	if err := profile.Validate(); err != nil {
		t.Fatalf("build complete Standard authoring test execution profile: %v", err)
	}
	return profile
}
