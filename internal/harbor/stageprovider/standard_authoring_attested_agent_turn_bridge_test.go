package stageprovider

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/agent"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestStandardAuthoringAttestedAgentTurnBridgeFromDeploymentLoadsFrozenAssetsPerEffect(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 15, 9, 30, 0, 0, time.UTC)
	stage, program, resolution, payload, verifier := standardAuthoringAttestedBridgeFixture(t)
	prompt, err := json.Marshal(program)
	if err != nil {
		t.Fatal(err)
	}
	schema := standardAuthoringCodexTestOutputSchemaAsset(t)
	contract := verifier.record.StandardAuthoringContract.Clone()
	verifier.record.PromptContentFingerprint = workflowkit.SHA256Fingerprint(prompt)
	verifier.record.SchemaContentFingerprint = workflowkit.SHA256Fingerprint(schema)
	firstInvocation := standardAuthoringCodexTestInvocation(t)
	secondInvocation := standardAuthoringCodexTestInvocation(t)
	secondInvocation.CLIVersionOutput = "codex-cli 0.133.1"
	attestor := &standardAuthoringAttestedBridgeAttestor{
		invocations: []CodexAppServerInvocation{firstInvocation, secondInvocation},
		assets: StandardAuthoringContractAssets{
			Prompt: StandardAuthoringContractAssetContents{ID: contract.Prompt.ID, Version: contract.Prompt.Version, Content: prompt, ContentSHA256: workflowkit.SHA256Fingerprint(prompt)},
			Schema: StandardAuthoringContractAssetContents{ID: contract.Schema.ID, Version: contract.Schema.Version, Content: schema, ContentSHA256: workflowkit.SHA256Fingerprint(schema)},
		},
	}
	firstRuntime := &standardAuthoringCodexRuntimeStub{conversation: &standardAuthoringCodexConversationStub{
		results: []agent.TurnResult{{Model: CodexAppServerProductionModelID, Text: standardAuthoringCodexTestOutput(t, stage, workflowkit.VerdictPass, []byte("first verified deployment asset"))}},
	}}
	secondRuntime := &standardAuthoringCodexRuntimeStub{conversation: &standardAuthoringCodexConversationStub{
		results: []agent.TurnResult{{Model: CodexAppServerProductionModelID, Text: standardAuthoringCodexTestOutput(t, stage, workflowkit.VerdictPass, []byte("second verified deployment asset"))}},
	}}
	runtimes := []agent.Runtime{firstRuntime, secondRuntime}
	runtimeFactoryCalls := 0
	bridge, err := NewStandardAuthoringAttestedAgentTurnBridgeFromDeployment(StandardAuthoringAttestedAgentTurnBridgeDeploymentConfig{
		Verifier: verifier, Attestor: attestor, WorkspaceRoot: t.TempDir(),
		RuntimeFactory: func(received CodexAppServerInvocation) agent.Runtime {
			if received != attestor.invocations[runtimeFactoryCalls] {
				t.Fatalf("runtime invocation = %+v, want attested invocation %+v", received, attestor.invocations[runtimeFactoryCalls])
			}
			runtime := runtimes[runtimeFactoryCalls]
			runtimeFactoryCalls++
			return runtime
		},
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	firstRequest, _, _ := standardAuthoringCodexTestRequest(stage, []byte("first-frozen-input"), now)
	firstResult, err := bridge.ExecuteAgentTurn(context.Background(), StageOperationInvocation{Request: firstRequest, Resolution: resolution}, payload)
	if err != nil {
		t.Fatal(err)
	}
	secondRequest, _, _ := standardAuthoringCodexTestRequest(stage, []byte("second-frozen-input"), now)
	secondResult, err := bridge.ExecuteAgentTurn(context.Background(), StageOperationInvocation{Request: secondRequest, Resolution: resolution}, payload)
	if err != nil {
		t.Fatal(err)
	}
	if firstResult.Outcome.Status != workflowkit.StatusCompleted || secondResult.Outcome.Status != workflowkit.StatusCompleted || string(firstResult.Artifacts[0].Content) != "first verified deployment asset" || string(secondResult.Artifacts[0].Content) != "second verified deployment asset" {
		t.Fatalf("deployment bridge results = first=%+v second=%+v", firstResult, secondResult)
	}
	if len(attestor.assetReads) != 2 || len(attestor.attestations) != 2 || verifier.stageCalls != 4 || runtimeFactoryCalls != 2 || len(firstRuntime.openRequests) != 1 || len(secondRuntime.openRequests) != 1 {
		t.Fatalf("deployment bridge calls: asset=%d attest=%d verify=%d runtime=%d first-opens=%d second-opens=%d", len(attestor.assetReads), len(attestor.attestations), verifier.stageCalls, runtimeFactoryCalls, len(firstRuntime.openRequests), len(secondRuntime.openRequests))
	}
	for _, assetRead := range attestor.assetReads {
		if assetRead.Resolution.StageKey != resolution.StageKey || assetRead.Record.PromptContentFingerprint != verifier.record.PromptContentFingerprint || assetRead.Record.SchemaContentFingerprint != verifier.record.SchemaContentFingerprint {
			t.Fatalf("asset reader did not receive the exact frozen lock evidence: %+v", assetRead)
		}
	}
}

func TestStandardAuthoringAttestedAgentTurnBridgeFromDeploymentRejectsBadSchemaBeforeRuntime(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 15, 9, 30, 0, 0, time.UTC)
	stage, program, resolution, payload, verifier := standardAuthoringAttestedBridgeFixture(t)
	prompt, err := json.Marshal(program)
	if err != nil {
		t.Fatal(err)
	}
	badSchema := []byte(`{"format":"` + StandardAuthoringCodexTurnOutputFormat + `","version":"1","fields":["format"],"fingerprint":"` + string(StandardAuthoringCodexOutputSchemaFingerprint()) + `"}`)
	contract := verifier.record.StandardAuthoringContract.Clone()
	verifier.record.PromptContentFingerprint = workflowkit.SHA256Fingerprint(prompt)
	verifier.record.SchemaContentFingerprint = workflowkit.SHA256Fingerprint(badSchema)
	attestor := &standardAuthoringAttestedBridgeAttestor{
		assets: StandardAuthoringContractAssets{
			Prompt: StandardAuthoringContractAssetContents{ID: contract.Prompt.ID, Version: contract.Prompt.Version, Content: prompt, ContentSHA256: workflowkit.SHA256Fingerprint(prompt)},
			Schema: StandardAuthoringContractAssetContents{ID: contract.Schema.ID, Version: contract.Schema.Version, Content: badSchema, ContentSHA256: workflowkit.SHA256Fingerprint(badSchema)},
		},
	}
	runtimeFactoryCalls := 0
	bridge, err := NewStandardAuthoringAttestedAgentTurnBridgeFromDeployment(StandardAuthoringAttestedAgentTurnBridgeDeploymentConfig{
		Verifier: verifier, Attestor: attestor, WorkspaceRoot: t.TempDir(),
		RuntimeFactory: func(CodexAppServerInvocation) agent.Runtime {
			runtimeFactoryCalls++
			return &standardAuthoringCodexRuntimeStub{}
		},
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	request, _, _ := standardAuthoringCodexTestRequest(stage, []byte("frozen-input"), now)
	result, err := bridge.ExecuteAgentTurn(context.Background(), StageOperationInvocation{Request: request, Resolution: resolution}, payload)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome.Status != workflowkit.StatusInfraFailed || result.ErrorText != standardAuthoringCodexFailureConfiguration || len(attestor.assetReads) != 1 || len(attestor.attestations) != 0 || runtimeFactoryCalls != 0 {
		t.Fatalf("bad schema path = result=%+v asset=%d attest=%d runtime=%d", result, len(attestor.assetReads), len(attestor.attestations), runtimeFactoryCalls)
	}
}

func TestStandardAuthoringCodexContractAssetsRequireCanonicalSelfDescribingDocuments(t *testing.T) {
	t.Parallel()
	program, err := NewStandardAuthoringCodexTurnProgram("standard-authoring.repo-analysis", "1", standardAuthoringCodexTestPrompts(1), 64*1024)
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := json.Marshal(program)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseStandardAuthoringCodexTurnProgramAsset(prompt)
	if err != nil || parsed.Fingerprint != program.Fingerprint {
		t.Fatalf("parse exact prompt asset = %+v, %v", parsed, err)
	}
	promptWithLF := append(append([]byte(nil), prompt...), '\n')
	if parsed, err := ParseStandardAuthoringCodexTurnProgramAsset(promptWithLF); err != nil || parsed.Fingerprint != program.Fingerprint {
		t.Fatalf("parse POSIX-LF prompt asset = %+v, %v", parsed, err)
	}
	for _, malformed := range [][]byte{
		append(append([]byte(nil), prompt...), '\n', '\n'),
		append(append([]byte(nil), prompt...), '\r', '\n'),
		append(append([]byte(nil), prompt...), ' '),
	} {
		if _, err := ParseStandardAuthoringCodexTurnProgramAsset(malformed); err == nil {
			t.Fatal("prompt asset with non-POSIX canonical whitespace was accepted")
		}
	}
	schema := standardAuthoringCodexTestOutputSchemaAsset(t)
	if err := ValidateStandardAuthoringCodexOutputSchemaAsset(schema); err != nil {
		t.Fatalf("validate exact schema asset: %v", err)
	}
	if err := ValidateStandardAuthoringCodexOutputSchemaAsset(append(append([]byte(nil), schema...), '\n')); err != nil {
		t.Fatalf("validate POSIX-LF schema asset: %v", err)
	}
	for _, malformed := range [][]byte{
		append(append([]byte(nil), schema...), '\n', '\n'),
		append(append([]byte(nil), schema...), '\r', '\n'),
		append(append([]byte(nil), schema...), '\t'),
	} {
		if err := ValidateStandardAuthoringCodexOutputSchemaAsset(malformed); err == nil {
			t.Fatal("schema asset with non-POSIX canonical whitespace was accepted")
		}
	}
	if err := ValidateStandardAuthoringCodexOutputSchemaAsset([]byte(`{"format":"` + StandardAuthoringCodexTurnOutputFormat + `","version":"1","fields":["verdict","format","version","artifacts[name,schema_version,content_base64]"],"fingerprint":"` + string(StandardAuthoringCodexOutputSchemaFingerprint()) + `"}`)); err == nil {
		t.Fatal("schema with reordered fields was accepted")
	}
}

func TestStandardAuthoringAttestedAgentTurnBridgeReattestsAndRebuildsRuntimePerEffect(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC)
	stage, program, resolution, payload, verifier := standardAuthoringAttestedBridgeFixture(t)
	firstInvocation := standardAuthoringCodexTestInvocation(t)
	secondInvocation := standardAuthoringCodexTestInvocation(t)
	secondInvocation.CLIVersionOutput = "codex-cli 0.133.1"
	attestor := &standardAuthoringAttestedBridgeAttestor{invocations: []CodexAppServerInvocation{firstInvocation, secondInvocation}}
	firstRuntime := &standardAuthoringCodexRuntimeStub{conversation: &standardAuthoringCodexConversationStub{
		results: []agent.TurnResult{{Model: CodexAppServerProductionModelID, Text: standardAuthoringCodexTestOutput(t, stage, workflowkit.VerdictPass, []byte("first"))}},
	}}
	secondRuntime := &standardAuthoringCodexRuntimeStub{conversation: &standardAuthoringCodexConversationStub{
		results: []agent.TurnResult{{Model: CodexAppServerProductionModelID, Text: standardAuthoringCodexTestOutput(t, stage, workflowkit.VerdictPass, []byte("second"))}},
	}}
	runtimes := []agent.Runtime{firstRuntime, secondRuntime}
	createdFor := []CodexAppServerInvocation{}
	bridge, err := NewStandardAuthoringAttestedAgentTurnBridge(StandardAuthoringAttestedAgentTurnBridgeConfig{
		Verifier: verifier, Attestor: attestor, WorkspaceRoot: t.TempDir(),
		ProgramByStage: map[workflowkit.StageKey]StandardAuthoringCodexTurnProgram{stage.Key: program},
		RuntimeFactory: func(invocation CodexAppServerInvocation) agent.Runtime {
			createdFor = append(createdFor, invocation)
			return runtimes[len(createdFor)-1]
		},
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}

	firstRequest, _, _ := standardAuthoringCodexTestRequest(stage, []byte("frozen-input-one"), now)
	first, err := bridge.ExecuteAgentTurn(context.Background(), StageOperationInvocation{Request: firstRequest, Resolution: resolution}, payload)
	if err != nil {
		t.Fatal(err)
	}
	secondRequest, _, _ := standardAuthoringCodexTestRequest(stage, []byte("frozen-input-two"), now)
	second, err := bridge.ExecuteAgentTurn(context.Background(), StageOperationInvocation{Request: secondRequest, Resolution: resolution}, payload)
	if err != nil {
		t.Fatal(err)
	}

	if string(first.Artifacts[0].Content) != "first" || string(second.Artifacts[0].Content) != "second" {
		t.Fatalf("effect outputs = first=%+v second=%+v, want independent runtimes", first, second)
	}
	if len(attestor.attestations) != 2 || verifier.stageCalls != 2 {
		t.Fatalf("per-effect proofs = attestor %d verifier %d, want 2 each", len(attestor.attestations), verifier.stageCalls)
	}
	if len(createdFor) != 2 || createdFor[0].CodexHomeDirectory == createdFor[1].CodexHomeDirectory || createdFor[0].CLIVersionOutput == createdFor[1].CLIVersionOutput {
		t.Fatalf("runtime factory invocations = %+v, want two distinct fresh invocations", createdFor)
	}
	if len(firstRuntime.openRequests) != 1 || len(secondRuntime.openRequests) != 1 || firstRuntime.openRequests[0].CapabilitySummary != firstInvocation.CLIVersionOutput || secondRuntime.openRequests[0].CapabilitySummary != secondInvocation.CLIVersionOutput {
		t.Fatalf("conversation opens did not receive their own attested invocation: first=%+v second=%+v", firstRuntime.openRequests, secondRuntime.openRequests)
	}
	for _, attestation := range attestor.attestations {
		attestedPayload, isAgentTurn := attestation.Resolution.Operation.Payload.(workflowadapter.AgentTurnOperationPayload)
		if attestation.Record.StandardAuthoringContract == nil || attestation.Record.StandardAuthoringContract.Prompt.ID != program.ID || attestation.Resolution.StageKey != resolution.StageKey || !isAgentTurn || attestedPayload != payload {
			t.Fatalf("bridge attestation did not preserve the frozen record/resolution: %+v", attestation)
		}
	}
}

func TestStandardAuthoringAttestedAgentTurnBridgeBlocksRuntimeOpenWhenAttestationFails(t *testing.T) {
	t.Parallel()
	const providerSecret = "provider-error-must-not-escape"
	now := time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC)
	stage, program, resolution, payload, verifier := standardAuthoringAttestedBridgeFixture(t)
	attestor := &standardAuthoringAttestedBridgeAttestor{err: errors.New(providerSecret)}
	runtimeFactoryCalls := 0
	bridge, err := NewStandardAuthoringAttestedAgentTurnBridge(StandardAuthoringAttestedAgentTurnBridgeConfig{
		Verifier: verifier, Attestor: attestor, WorkspaceRoot: t.TempDir(),
		ProgramByStage: map[workflowkit.StageKey]StandardAuthoringCodexTurnProgram{stage.Key: program},
		RuntimeFactory: func(CodexAppServerInvocation) agent.Runtime {
			runtimeFactoryCalls++
			return &standardAuthoringCodexRuntimeStub{}
		},
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	request, _, _ := standardAuthoringCodexTestRequest(stage, []byte("frozen-input"), now)
	result, err := bridge.ExecuteAgentTurn(context.Background(), StageOperationInvocation{Request: request, Resolution: resolution}, payload)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome.Status != workflowkit.StatusInfraFailed || result.ErrorText != standardAuthoringCodexFailureConfiguration || strings.Contains(result.ErrorText, providerSecret) {
		t.Fatalf("attestation failure result = %+v, want stable pre-open policy failure", result)
	}
	if len(attestor.attestations) != 1 || verifier.stageCalls != 1 || runtimeFactoryCalls != 0 {
		t.Fatalf("attestation failure reached a runtime: attestations=%d verifications=%d runtime-factory=%d", len(attestor.attestations), verifier.stageCalls, runtimeFactoryCalls)
	}
}

func standardAuthoringAttestedBridgeFixture(t *testing.T) (workflowkit.StageDescriptor, StandardAuthoringCodexTurnProgram, workflowadapter.StageOperationResolution, workflowadapter.AgentTurnOperationPayload, *standardAuthoringAttestedBridgeVerifier) {
	t.Helper()
	stage := standardAuthoringCodexTestStage(1)
	program, err := NewStandardAuthoringCodexTurnProgram("standard-authoring.repo-analysis", "1", standardAuthoringCodexTestPrompts(1), 64*1024)
	if err != nil {
		t.Fatal(err)
	}
	payload := workflowadapter.AgentTurnOperationPayload{AgentID: "standard-authoring", ModelID: CodexAppServerProductionModelID, MaxTurns: 1}
	resolution := workflowadapter.StageOperationResolution{
		StageKey:  stage.Key,
		Operation: workflowadapter.StageOperationBinding{Payload: payload},
	}
	record := DeploymentOperationCatalogLockRecord{
		Stage:     DeploymentStageContract{Key: stage.Key},
		Operation: resolution.Operation.Clone(),
		StandardAuthoringContract: &StandardAuthoringContractLock{
			Format: StandardAuthoringContractLockFormat, Version: StandardAuthoringContractLockVersion,
			Prompt: StandardAuthoringContractAssetReference{ID: program.ID, Version: program.Version, RelativePath: "prompts/repo-analysis.json"},
			Schema: StandardAuthoringContractAssetReference{ID: "standard-authoring.repo-analysis.schema", Version: "1", RelativePath: "schemas/repo-analysis.json"},
		},
	}
	return stage, program, resolution, payload, &standardAuthoringAttestedBridgeVerifier{record: record}
}

func standardAuthoringCodexTestOutputSchemaAsset(t *testing.T) []byte {
	t.Helper()
	encoded, err := json.Marshal(standardAuthoringCodexOutputSchemaAsset{
		Format: StandardAuthoringCodexTurnOutputFormat, Version: StandardAuthoringCodexTurnOutputVersion,
		Fields:      []string{"format", "version", "verdict", "artifacts[name,schema_version,content_base64]"},
		Fingerprint: StandardAuthoringCodexOutputSchemaFingerprint(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

type standardAuthoringAttestedBridgeVerifier struct {
	record     DeploymentOperationCatalogLockRecord
	stageCalls int
}

func (verifier *standardAuthoringAttestedBridgeVerifier) CatalogIdentity() DeploymentOperationCatalogIdentity {
	return DeploymentOperationCatalogIdentity{}
}

func (verifier *standardAuthoringAttestedBridgeVerifier) CatalogReceipt() DeploymentOperationCatalogReceipt {
	return DeploymentOperationCatalogReceipt{}
}

func (verifier *standardAuthoringAttestedBridgeVerifier) LockIdentity() DeploymentOperationCatalogLockIdentity {
	return DeploymentOperationCatalogLockIdentity{}
}

func (verifier *standardAuthoringAttestedBridgeVerifier) HarborFlowBuild() HarborFlowBuildIdentity {
	return HarborFlowBuildIdentity{}
}

func (verifier *standardAuthoringAttestedBridgeVerifier) CanonicalLockJSON() []byte { return nil }

func (verifier *standardAuthoringAttestedBridgeVerifier) VerifyCatalogReceipt(DeploymentOperationCatalogReceipt) error {
	return nil
}

func (verifier *standardAuthoringAttestedBridgeVerifier) VerifyLockIdentity(DeploymentOperationCatalogLockIdentity) error {
	return nil
}

func (verifier *standardAuthoringAttestedBridgeVerifier) VerifyStageOperation(workflowadapter.StageOperationResolution) (DeploymentOperationCatalogLockRecord, error) {
	verifier.stageCalls++
	return verifier.record.Clone(), nil
}

type standardAuthoringAttestedBridgeAttestor struct {
	invocations  []CodexAppServerInvocation
	attestations []DeploymentOperationRuntimeAttestation
	assetReads   []DeploymentOperationRuntimeAttestation
	assets       StandardAuthoringContractAssets
	assetErr     error
	err          error
}

func (attestor *standardAuthoringAttestedBridgeAttestor) AttestCodexAppServerOperation(_ context.Context, attestation DeploymentOperationRuntimeAttestation) (CodexAppServerInvocation, error) {
	attestation.Record = attestation.Record.Clone()
	attestation.Resolution = attestation.Resolution.Clone()
	attestor.attestations = append(attestor.attestations, attestation)
	if attestor.err != nil {
		return CodexAppServerInvocation{}, attestor.err
	}
	index := len(attestor.attestations) - 1
	if index >= len(attestor.invocations) {
		return CodexAppServerInvocation{}, errors.New("missing attested invocation")
	}
	return attestor.invocations[index], nil
}

func (attestor *standardAuthoringAttestedBridgeAttestor) ReadStandardAuthoringContractAssets(_ context.Context, attestation DeploymentOperationRuntimeAttestation) (StandardAuthoringContractAssets, error) {
	attestation.Record = attestation.Record.Clone()
	attestation.Resolution = attestation.Resolution.Clone()
	attestor.assetReads = append(attestor.assetReads, attestation)
	if attestor.assetErr != nil {
		return StandardAuthoringContractAssets{}, attestor.assetErr
	}
	return attestor.assets.Clone(), nil
}

var _ DeploymentOperationCatalogLockVerifier = (*standardAuthoringAttestedBridgeVerifier)(nil)
var _ StandardAuthoringCodexAppServerOperationAttestor = (*standardAuthoringAttestedBridgeAttestor)(nil)
var _ StandardAuthoringCodexDeploymentAttestor = (*standardAuthoringAttestedBridgeAttestor)(nil)
