package stageprovider

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/authoringharness"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// StandardAuthoringCodexAppServerOperationAttestor is the narrow capability an
// authoring agent-turn bridge needs from the deployment attestor. It returns a
// secret-free invocation only after it has rechecked the immutable operation,
// runtime files, CLI capability, and Standard authoring prompt/schema assets.
//
// It deliberately does not expose a general environment, a model client, or a
// way to execute an operation. StandardAuthoringRuntimeAttestor implements it.
type StandardAuthoringCodexAppServerOperationAttestor interface {
	AttestCodexAppServerOperation(context.Context, DeploymentOperationRuntimeAttestation) (CodexAppServerInvocation, error)
}

// StandardAuthoringCodexDeploymentAttestor is the additional read-only
// capability required when a bridge loads the prompt and output schema from
// immutable deployment assets at effect time. It never exposes an asset path.
type StandardAuthoringCodexDeploymentAttestor interface {
	StandardAuthoringCodexAppServerOperationAttestor
	ReadStandardAuthoringContractAssets(context.Context, DeploymentOperationRuntimeAttestation) (StandardAuthoringContractAssets, error)
}

// StandardAuthoringAttestedAgentTurnBridgeConfig binds a caller-prevalidated
// program map to a static operation verifier and dynamic runtime attestor. It
// is useful for controlled tests or a composition that has already validated
// immutable prompt/schema assets through a separate deployment boundary.
//
// Normal production composition should use
// NewStandardAuthoringAttestedAgentTurnBridgeFromDeployment instead. That
// constructor reads and validates the assets against each real frozen
// resolution, avoiding any need to fabricate checkout revision facts at
// process startup.
type StandardAuthoringAttestedAgentTurnBridgeConfig struct {
	Verifier         DeploymentOperationCatalogLockVerifier
	Attestor         StandardAuthoringCodexAppServerOperationAttestor
	WorkspaceRoot    string
	WorkspaceMode    StandardAuthoringCodexWorkspaceMode
	SourceVerifier   StandardAuthoringCodexFrozenSourceVerifier
	HarnessValidator authoringharness.Validator
	ProgramByStage   map[workflowkit.StageKey]StandardAuthoringCodexTurnProgram
	RuntimeFactory   StandardAuthoringCodexRuntimeFactory
	Now              func() time.Time
}

// StandardAuthoringAttestedAgentTurnBridgeDeploymentConfig is the production
// bootstrap shape. It has no prompt text, schema text, executable path,
// environment, secret, model endpoint, or checkout revision input. The bridge
// obtains all dynamic facts from the frozen StageOperationInvocation at effect
// time and reads the lock-bound assets through Attestor.
type StandardAuthoringAttestedAgentTurnBridgeDeploymentConfig struct {
	Verifier         DeploymentOperationCatalogLockVerifier
	Attestor         StandardAuthoringCodexDeploymentAttestor
	WorkspaceRoot    string
	WorkspaceMode    StandardAuthoringCodexWorkspaceMode
	SourceVerifier   StandardAuthoringCodexFrozenSourceVerifier
	HarnessValidator authoringharness.Validator
	RuntimeFactory   StandardAuthoringCodexRuntimeFactory
	Now              func() time.Time
}

type standardAuthoringCodexProgramFactory func(context.Context, StageOperationInvocation, workflowadapter.AgentTurnOperationPayload) (StandardAuthoringCodexTurnProgram, error)

// StandardAuthoringAttestedAgentTurnBridge is an AgentTurnOperationExecutor
// that obtains a new, attested Codex invocation for every effect. It holds no
// executable path, controlled environment, model client, prior invocation, or
// deployment asset path. The generic catalog-lock wrapper remains installed
// around it as a separate defense-in-depth attestation boundary.
type StandardAuthoringAttestedAgentTurnBridge struct {
	verifier         DeploymentOperationCatalogLockVerifier
	attestor         StandardAuthoringCodexAppServerOperationAttestor
	workspaceRoot    string
	workspaceMode    StandardAuthoringCodexWorkspaceMode
	sourceVerifier   StandardAuthoringCodexFrozenSourceVerifier
	harnessValidator authoringharness.Validator
	runtimeFactory   StandardAuthoringCodexRuntimeFactory
	now              func() time.Time
	programForEffect standardAuthoringCodexProgramFactory
}

// NewStandardAuthoringAttestedAgentTurnBridge constructs an injectable static
// program bridge. It checks static catalog/lock identity at construction and
// rechecks the exact record and runtime proof immediately before every App
// Server open.
func NewStandardAuthoringAttestedAgentTurnBridge(config StandardAuthoringAttestedAgentTurnBridgeConfig) (*StandardAuthoringAttestedAgentTurnBridge, error) {
	if len(config.ProgramByStage) == 0 {
		return nil, fmt.Errorf("%w: at least one stage prompt program is required", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	programs := make(map[workflowkit.StageKey]StandardAuthoringCodexTurnProgram, len(config.ProgramByStage))
	for stageKey, program := range config.ProgramByStage {
		if err := standardAuthoringCodexToken("program stage key", string(stageKey)); err != nil {
			return nil, err
		}
		if err := program.validate(); err != nil || program.Fingerprint == "" {
			return nil, fmt.Errorf("%w: stage %q prompt program is invalid", ErrStandardAuthoringCodexAgentTurnConfiguration, stageKey)
		}
		programs[stageKey] = program.clone()
	}
	bridge, err := newStandardAuthoringAttestedAgentTurnBridge(config.Verifier, config.Attestor, config.WorkspaceRoot, config.WorkspaceMode, config.SourceVerifier, config.HarnessValidator, config.RuntimeFactory, config.Now)
	if err != nil {
		return nil, err
	}
	bridge.programForEffect = func(_ context.Context, invocation StageOperationInvocation, _ workflowadapter.AgentTurnOperationPayload) (StandardAuthoringCodexTurnProgram, error) {
		program, found := programs[invocation.Request.Stage.Key]
		if !found {
			return StandardAuthoringCodexTurnProgram{}, ErrDeploymentOperationCatalogLockDrift
		}
		return program.clone(), nil
	}
	return bridge, nil
}

// NewStandardAuthoringAttestedAgentTurnBridgeFromDeployment constructs the
// production injectable bridge. Prompt and schema assets are deliberately
// loaded at effect time, after the real frozen resolution is available: a
// deployment lock intentionally does not contain a Run's checkout revision or
// artifact identities, and this helper must never invent them.
func NewStandardAuthoringAttestedAgentTurnBridgeFromDeployment(config StandardAuthoringAttestedAgentTurnBridgeDeploymentConfig) (*StandardAuthoringAttestedAgentTurnBridge, error) {
	if isNilInterface(config.Attestor) {
		return nil, ErrDeploymentOperationRuntimeAttestationUnavailable
	}
	bridge, err := newStandardAuthoringAttestedAgentTurnBridge(config.Verifier, config.Attestor, config.WorkspaceRoot, config.WorkspaceMode, config.SourceVerifier, config.HarnessValidator, config.RuntimeFactory, config.Now)
	if err != nil {
		return nil, err
	}
	bridge.programForEffect = func(ctx context.Context, invocation StageOperationInvocation, payload workflowadapter.AgentTurnOperationPayload) (StandardAuthoringCodexTurnProgram, error) {
		return bridge.loadProgramFromFrozenAssets(ctx, invocation, payload, config.Attestor)
	}
	return bridge, nil
}

func newStandardAuthoringAttestedAgentTurnBridge(verifier DeploymentOperationCatalogLockVerifier, attestor StandardAuthoringCodexAppServerOperationAttestor, workspaceRoot string, workspaceMode StandardAuthoringCodexWorkspaceMode, sourceVerifier StandardAuthoringCodexFrozenSourceVerifier, harnessValidator authoringharness.Validator, runtimeFactory StandardAuthoringCodexRuntimeFactory, now func() time.Time) (*StandardAuthoringAttestedAgentTurnBridge, error) {
	if isNilDeploymentOperationCatalogLockVerifier(verifier) {
		return nil, ErrDeploymentOperationCatalogLockUnavailable
	}
	if isNilInterface(attestor) {
		return nil, ErrDeploymentOperationRuntimeAttestationUnavailable
	}
	if err := verifier.VerifyLockIdentity(verifier.LockIdentity()); err != nil {
		return nil, fmt.Errorf("verify Standard authoring deployment lock identity: %w", err)
	}
	root, err := standardAuthoringCodexWorkspaceRoot(workspaceRoot)
	if err != nil {
		return nil, err
	}
	mode, err := standardAuthoringCodexWorkspaceMode(workspaceMode)
	if err != nil {
		return nil, err
	}
	if mode == StandardAuthoringCodexWorkspaceRunScoped && isNilInterface(sourceVerifier) {
		return nil, fmt.Errorf("%w: RunScoped frozen source verifier is required", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	return &StandardAuthoringAttestedAgentTurnBridge{
		verifier: verifier, attestor: attestor, workspaceRoot: root, workspaceMode: mode, sourceVerifier: sourceVerifier, harnessValidator: harnessValidator, runtimeFactory: runtimeFactory, now: now,
	}, nil
}

// ExecuteAgentTurn implements AgentTurnOperationExecutor. It obtains the
// program from either a prevalidated static map or verified deployment assets,
// creates a one-effect strict executor, and lets that executor re-attest the
// Codex runtime immediately before OpenConversation.
func (bridge *StandardAuthoringAttestedAgentTurnBridge) ExecuteAgentTurn(ctx context.Context, invocation StageOperationInvocation, payload workflowadapter.AgentTurnOperationPayload) (workflowkit.StageExecutionResult, error) {
	if bridge == nil || isNilInterface(bridge.programForEffect) {
		return standardAuthoringCodexFailure(workflowkit.FailurePolicy, standardAuthoringCodexFailureConfiguration), nil
	}
	program, err := bridge.programForEffect(ctx, invocation.clone(), payload)
	if err != nil {
		if contextError(ctx) != nil {
			return standardAuthoringCodexInterrupted(), nil
		}
		return standardAuthoringCodexFailure(workflowkit.FailurePolicy, standardAuthoringCodexFailureConfiguration), nil
	}
	strict, err := NewStandardAuthoringCodexAgentTurnExecutor(StandardAuthoringCodexAgentTurnExecutorConfig{
		InvocationFactory: func(factoryCtx context.Context, factoryInvocation StageOperationInvocation, factoryPayload workflowadapter.AgentTurnOperationPayload) (CodexAppServerInvocation, error) {
			return bridge.attestInvocationForEffect(factoryCtx, factoryInvocation, factoryPayload, program)
		},
		WorkspaceRoot:    bridge.workspaceRoot,
		WorkspaceMode:    bridge.workspaceMode,
		SourceVerifier:   bridge.sourceVerifier,
		HarnessValidator: bridge.harnessValidator,
		ProgramByStage:   map[workflowkit.StageKey]StandardAuthoringCodexTurnProgram{invocation.Request.Stage.Key: program},
		RuntimeFactory:   bridge.runtimeFactory,
		Now:              bridge.now,
	})
	if err != nil {
		return standardAuthoringCodexFailure(workflowkit.FailurePolicy, standardAuthoringCodexFailureConfiguration), nil
	}
	return strict.ExecuteAgentTurn(ctx, invocation, payload)
}

func (bridge *StandardAuthoringAttestedAgentTurnBridge) loadProgramFromFrozenAssets(ctx context.Context, invocation StageOperationInvocation, payload workflowadapter.AgentTurnOperationPayload, reader StandardAuthoringCodexDeploymentAttestor) (StandardAuthoringCodexTurnProgram, error) {
	attestation, err := bridge.frozenEffectAttestation(ctx, invocation, payload)
	if err != nil {
		return StandardAuthoringCodexTurnProgram{}, err
	}
	assets, err := reader.ReadStandardAuthoringContractAssets(ctx, attestation)
	if err != nil {
		return StandardAuthoringCodexTurnProgram{}, stableStandardAuthoringBridgeAttestationError(err)
	}
	if assets.Prompt.ContentSHA256 != attestation.Record.PromptContentFingerprint || assets.Schema.ContentSHA256 != attestation.Record.SchemaContentFingerprint {
		return StandardAuthoringCodexTurnProgram{}, ErrDeploymentOperationRuntimeAttestationFailed
	}
	program, err := ParseStandardAuthoringCodexTurnProgramAsset(assets.Prompt.Content)
	if err != nil {
		return StandardAuthoringCodexTurnProgram{}, ErrDeploymentOperationRuntimeAttestationFailed
	}
	if err := ValidateStandardAuthoringCodexOutputSchemaAssetForTemplateStage(attestation.CatalogReceipt.Template, invocation.Request.Stage.Key, assets.Schema.Content); err != nil {
		return StandardAuthoringCodexTurnProgram{}, ErrDeploymentOperationRuntimeAttestationFailed
	}
	if err := verifyStandardAuthoringLockedProgram(attestation.Record, invocation, payload, program); err != nil {
		return StandardAuthoringCodexTurnProgram{}, err
	}
	contract := attestation.Record.StandardAuthoringContract.Clone()
	if assets.Prompt.ID != contract.Prompt.ID || assets.Prompt.Version != contract.Prompt.Version || assets.Schema.ID != contract.Schema.ID || assets.Schema.Version != contract.Schema.Version {
		return StandardAuthoringCodexTurnProgram{}, ErrDeploymentOperationRuntimeAttestationFailed
	}
	return program.clone(), nil
}

func (bridge *StandardAuthoringAttestedAgentTurnBridge) attestInvocationForEffect(ctx context.Context, invocation StageOperationInvocation, payload workflowadapter.AgentTurnOperationPayload, program StandardAuthoringCodexTurnProgram) (CodexAppServerInvocation, error) {
	attestation, err := bridge.frozenEffectAttestation(ctx, invocation, payload)
	if err != nil {
		return CodexAppServerInvocation{}, err
	}
	if err := verifyStandardAuthoringLockedProgram(attestation.Record, invocation, payload, program); err != nil {
		return CodexAppServerInvocation{}, err
	}
	attestedInvocation, err := bridge.attestor.AttestCodexAppServerOperation(ctx, attestation)
	if err != nil {
		return CodexAppServerInvocation{}, stableStandardAuthoringBridgeAttestationError(err)
	}
	return attestedInvocation, nil
}

func (bridge *StandardAuthoringAttestedAgentTurnBridge) frozenEffectAttestation(ctx context.Context, invocation StageOperationInvocation, payload workflowadapter.AgentTurnOperationPayload) (DeploymentOperationRuntimeAttestation, error) {
	if bridge == nil || isNilDeploymentOperationCatalogLockVerifier(bridge.verifier) || isNilInterface(bridge.attestor) {
		return DeploymentOperationRuntimeAttestation{}, ErrDeploymentOperationRuntimeAttestationUnavailable
	}
	if err := contextError(ctx); err != nil {
		return DeploymentOperationRuntimeAttestation{}, ErrDeploymentOperationRuntimeAttestationFailed
	}
	record, err := bridge.verifier.VerifyStageOperation(invocation.Resolution)
	if err != nil {
		return DeploymentOperationRuntimeAttestation{}, stableStandardAuthoringBridgeVerifierError(err)
	}
	if record.Stage.Key != invocation.Resolution.StageKey || invocation.Request.Stage.Key != invocation.Resolution.StageKey {
		return DeploymentOperationRuntimeAttestation{}, ErrDeploymentOperationCatalogLockDrift
	}
	lockedPayload, ok := record.Operation.Payload.(workflowadapter.AgentTurnOperationPayload)
	if !ok || lockedPayload != payload {
		return DeploymentOperationRuntimeAttestation{}, ErrDeploymentOperationCatalogLockDrift
	}
	return DeploymentOperationRuntimeAttestation{
		CatalogReceipt:  bridge.verifier.CatalogReceipt(),
		LockIdentity:    bridge.verifier.LockIdentity(),
		HarborFlowBuild: bridge.verifier.HarborFlowBuild(),
		Record:          record.Clone(),
		Resolution:      invocation.Resolution.Clone(),
	}, nil
}

// verifyStandardAuthoringLockedProgram proves that a typed agent.turn cannot
// reuse a prompt program registered for another stage or replace the immutable
// prompt asset selected by the lock. Raw prompt/schema bytes are rehashed by
// the deployment attestor before the bridge accepts their semantic form.
func verifyStandardAuthoringLockedProgram(record DeploymentOperationCatalogLockRecord, invocation StageOperationInvocation, payload workflowadapter.AgentTurnOperationPayload, program StandardAuthoringCodexTurnProgram) error {
	if record.StandardAuthoringContract == nil || program.Fingerprint == "" {
		return ErrDeploymentOperationRuntimeAttestationFailed
	}
	if err := program.validate(); err != nil {
		return ErrDeploymentOperationRuntimeAttestationFailed
	}
	contract := record.StandardAuthoringContract.Clone()
	if err := contract.Validate(); err != nil {
		return ErrDeploymentOperationCatalogLockDrift
	}
	if contract.Prompt.ID != program.ID || contract.Prompt.Version != program.Version || record.Stage.Key != invocation.Request.Stage.Key {
		return ErrDeploymentOperationCatalogLockDrift
	}
	lockedPayload, ok := record.Operation.Payload.(workflowadapter.AgentTurnOperationPayload)
	if !ok || lockedPayload != payload {
		return ErrDeploymentOperationCatalogLockDrift
	}
	return nil
}

// The turn executor deliberately returns only a stable durable failure code.
// This bridge likewise does not expose filesystem paths, provider output, or
// environment details from static verification or runtime attestation errors.
func stableStandardAuthoringBridgeVerifierError(err error) error {
	if errors.Is(err, ErrDeploymentOperationCatalogLockUnavailable) {
		return ErrDeploymentOperationCatalogLockUnavailable
	}
	return ErrDeploymentOperationCatalogLockDrift
}

func stableStandardAuthoringBridgeAttestationError(err error) error {
	if errors.Is(err, ErrDeploymentOperationRuntimeAttestationUnavailable) {
		return ErrDeploymentOperationRuntimeAttestationUnavailable
	}
	return ErrDeploymentOperationRuntimeAttestationFailed
}

var _ AgentTurnOperationExecutor = (*StandardAuthoringAttestedAgentTurnBridge)(nil)
var _ StandardAuthoringCodexAppServerOperationAttestor = (*StandardAuthoringRuntimeAttestor)(nil)
var _ StandardAuthoringCodexDeploymentAttestor = (*StandardAuthoringRuntimeAttestor)(nil)
