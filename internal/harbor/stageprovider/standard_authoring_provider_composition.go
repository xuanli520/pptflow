package stageprovider

import (
	"context"
	"fmt"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	// StandardAuthoringProviderID is the one production provider identity
	// reserved for the future immutable authoring-source workflow. It has no
	// relationship to the CodeEdge evaluator provider, whose catalog is owned
	// by the closed child template.
	StandardAuthoringProviderID      = "harbor-standard-authoring"
	StandardAuthoringProviderKind    = "authoring"
	StandardAuthoringProviderVersion = "3.0.0"
)

// StandardAuthoringOperationHandlers is the complete injection boundary for
// a deployment-owned authoring provider. It deliberately exposes typed
// executors only: callers cannot supply a command string, image name, model,
// prompt, checkout path, secret, or opaque configuration map.
//
// DurableReview is optional because the public Harbor workflow adapter turns a
// resolved durable.review operation into a StageWait without executing its
// provider executor. If it is absent, composition installs a fail-closed
// resolution-only executor.
type StandardAuthoringOperationHandlers struct {
	HostCommand   LocalCommandOperationExecutor
	AgentTurn     AgentTurnOperationExecutor
	HarborBuiltin HarborBuiltinOperationExecutor
	DurableReview workflowkit.StageExecutor
}

// StandardAuthoringProviderCompositionConfig is intentionally independent of
// app, store, CLI, and TUI packages. The caller supplies a static parsed
// catalog and lock plus handlers assembled by a deployment composition. The
// exact Template field prevents a future closed authoring template from
// accidentally reusing a different template's resolver as a fallback.
type StandardAuthoringProviderCompositionConfig struct {
	Template            workflowadapter.TemplateReference
	Catalog             *DeploymentOperationCatalogResolver
	Lock                DeploymentOperationCatalogLock
	Attestor            *StandardAuthoringRuntimeAttestor
	Handlers            StandardAuthoringOperationHandlers
	CodexWorkspaceRoot  string
	CodexWorkspaceMode  StandardAuthoringCodexWorkspaceMode
	CodexSourceVerifier StandardAuthoringCodexFrozenSourceVerifier
	CodexRuntimeFactory StandardAuthoringCodexRuntimeFactory
	CandidateValidator  StandardAuthoringCandidateValidationTool
	CodexNow            func() time.Time
}

// StandardAuthoringProviderComposition is the immutable provider/resolver
// bundle a higher layer can install under one exact template key. All exposed
// values are static resolver snapshots; handler objects remain private behind
// Resolver and cannot be replaced by a Run input.
type StandardAuthoringProviderComposition struct {
	Catalog  *DeploymentOperationCatalogResolver
	Verifier *DeploymentOperationCatalogLockResolver
	Resolver *CatalogLockAttestedWorkflowkitProviderOperationResolver
}

// NewStandardAuthoringProviderComposition builds the exact provider chain:
// catalog allow-list -> one local typed provider -> catalog/lock verifier ->
// runtime attestation wrapper. It accepts only local.command, agent.turn,
// harbor.builtin, and durable.review. A container.command operation is denied
// at construction because this authoring contract has no approved container
// image/ABI attestor.
func NewStandardAuthoringProviderComposition(config StandardAuthoringProviderCompositionConfig) (*StandardAuthoringProviderComposition, error) {
	if err := config.Template.Validate(); err != nil {
		return nil, fmt.Errorf("validate Standard authoring template: %w", err)
	}
	if !workflowadapter.IsStandardAuthoringWorkflowTemplate(config.Template) {
		return nil, fmt.Errorf("%w: Standard authoring composition requires an installed Standard authoring template", ErrDeploymentOperationCatalogDrift)
	}
	if config.Catalog == nil {
		return nil, ErrDeploymentOperationCatalogUnavailable
	}
	if !config.Catalog.Template().Equal(config.Template) {
		return nil, fmt.Errorf("%w: Standard authoring catalog template %s@%s does not match configured template %s@%s", ErrDeploymentOperationCatalogDrift, config.Catalog.Template().ID, config.Catalog.Template().Version, config.Template.ID, config.Template.Version)
	}
	if config.Attestor == nil {
		return nil, ErrDeploymentOperationRuntimeAttestationUnavailable
	}
	verifier, err := NewDeploymentOperationCatalogLockResolver(config.Catalog, config.Lock)
	if err != nil {
		return nil, err
	}
	if !verifier.CatalogReceipt().Template.Equal(config.Template) {
		return nil, fmt.Errorf("%w: Standard authoring lock receipt names another template", ErrDeploymentOperationCatalogLockDrift)
	}
	if verifier.HarborFlowBuild() != config.Attestor.HarborFlowBuild() {
		return nil, fmt.Errorf("%w: Standard authoring attestor build identity differs from deployment lock", ErrDeploymentOperationRuntimeAttestationFailed)
	}

	operations := verifier.Lock().Operations
	if len(operations) == 0 {
		return nil, fmt.Errorf("%w: Standard authoring catalog has no operation registrations", ErrStageOperationUnavailable)
	}
	provider := operations[0].Provider
	if provider.ID != StandardAuthoringProviderID || provider.Kind != StandardAuthoringProviderKind || provider.Version != StandardAuthoringProviderVersion {
		return nil, fmt.Errorf("%w: Standard authoring catalog provider must be %s (%s@%s)", ErrDeploymentOperationCatalogDrift, StandardAuthoringProviderID, StandardAuthoringProviderKind, StandardAuthoringProviderVersion)
	}

	typedRegistrations := make([]TypedWorkflowkitStageOperationRegistration, 0, len(operations))
	reviewRegistrations := make([]WorkflowkitStaticStageOperationRegistration, 0, len(operations))
	requiresAgentTurn := false
	reviewExecutor := config.Handlers.DurableReview
	if isNilWorkflowkitStageExecutor(reviewExecutor) {
		reviewExecutor = standardAuthoringReviewResolutionOnlyExecutor{}
	}
	for _, record := range operations {
		if record.Provider != provider {
			return nil, fmt.Errorf("%w: Standard authoring catalog contains more than one provider", ErrDeploymentOperationCatalogDrift)
		}
		switch record.Operation.Payload.(type) {
		case workflowadapter.LocalCommandOperationPayload, workflowadapter.HarborBuiltinOperationPayload:
			typedRegistrations = append(typedRegistrations, TypedWorkflowkitStageOperationRegistration{StageKey: record.Stage.Key, Operation: record.Operation.Clone()})
		case workflowadapter.AgentTurnOperationPayload:
			payload := record.Operation.Payload.(workflowadapter.AgentTurnOperationPayload)
			if !IsCodexAppServerProductionPayload(payload) {
				return nil, fmt.Errorf("%w: Standard authoring Codex agent.turn must pin model %q with reasoning effort %q", ErrDeploymentOperationCatalogDrift, CodexAppServerProductionModelID, CodexAppServerProductionReasoningEffort)
			}
			requiresAgentTurn = true
			typedRegistrations = append(typedRegistrations, TypedWorkflowkitStageOperationRegistration{StageKey: record.Stage.Key, Operation: record.Operation.Clone()})
		case workflowadapter.DurableReviewOperationPayload:
			reviewRegistrations = append(reviewRegistrations, WorkflowkitStaticStageOperationRegistration{StageKey: record.Stage.Key, Operation: record.Operation.Clone(), Executor: reviewExecutor})
		case workflowadapter.ContainerCommandOperationPayload:
			return nil, fmt.Errorf("%w: Standard authoring container.command is not approved", ErrDeploymentOperationRuntimeAttestationUnavailable)
		default:
			return nil, fmt.Errorf("%w: unsupported Standard authoring operation payload %T", ErrStageOperationUnavailable, record.Operation.Payload)
		}
	}
	agentTurn := config.Handlers.AgentTurn
	if requiresAgentTurn && isNilAgentTurnOperationExecutor(agentTurn) {
		bridge, err := NewStandardAuthoringAttestedAgentTurnBridgeFromDeployment(StandardAuthoringAttestedAgentTurnBridgeDeploymentConfig{
			Verifier: verifier, Attestor: config.Attestor, WorkspaceRoot: config.CodexWorkspaceRoot,
			WorkspaceMode: config.CodexWorkspaceMode, SourceVerifier: config.CodexSourceVerifier,
			RuntimeFactory: config.CodexRuntimeFactory, CandidateValidator: config.CandidateValidator, Now: config.CodexNow,
		})
		if err != nil {
			return nil, fmt.Errorf("construct Standard authoring attested Codex agent-turn bridge: %w", err)
		}
		agentTurn = bridge
	}
	typed, err := NewTypedWorkflowkitStageOperationProvider(TypedWorkflowkitStageOperationProviderConfig{
		Handlers: TypedWorkflowkitOperationHandlers{
			LocalCommand:  config.Handlers.HostCommand,
			AgentTurn:     agentTurn,
			HarborBuiltin: config.Handlers.HarborBuiltin,
		},
		Operations: typedRegistrations,
	})
	if err != nil {
		return nil, fmt.Errorf("construct Standard authoring typed provider: %w", err)
	}
	reviews, err := NewStaticWorkflowkitStageOperationProvider(reviewRegistrations)
	if err != nil {
		return nil, fmt.Errorf("construct Standard authoring durable review provider: %w", err)
	}
	adapter := standardAuthoringOperationProvider{typed: typed, reviews: reviews}
	providers, err := NewControlledWorkflowkitProviderRegistry([]WorkflowkitProviderRegistration{{Provider: provider, Adapter: adapter}})
	if err != nil {
		return nil, fmt.Errorf("register Standard authoring provider: %w", err)
	}
	catalogBound, err := NewCatalogBoundWorkflowkitProviderOperationResolver(config.Catalog, providers)
	if err != nil {
		return nil, fmt.Errorf("bind Standard authoring provider to catalog: %w", err)
	}
	resolver, err := NewCatalogLockAttestedWorkflowkitProviderOperationResolver(verifier, catalogBound, config.Attestor)
	if err != nil {
		return nil, fmt.Errorf("bind Standard authoring provider to catalog lock: %w", err)
	}
	return &StandardAuthoringProviderComposition{Catalog: config.Catalog, Verifier: verifier, Resolver: resolver}, nil
}

type standardAuthoringOperationProvider struct {
	typed   *TypedWorkflowkitStageOperationProvider
	reviews *StaticWorkflowkitStageOperationProvider
}

func (provider standardAuthoringOperationProvider) ResolveWorkflowkitStageOperation(resolution workflowadapter.StageOperationResolution) (workflowkit.StageExecutor, error) {
	switch resolution.Operation.Payload.(type) {
	case workflowadapter.DurableReviewOperationPayload:
		if provider.reviews == nil {
			return nil, fmt.Errorf("%w: Standard authoring durable review provider is unavailable", ErrStageOperationUnavailable)
		}
		return provider.reviews.ResolveWorkflowkitStageOperation(resolution)
	default:
		if provider.typed == nil {
			return nil, fmt.Errorf("%w: Standard authoring typed provider is unavailable", ErrStageOperationUnavailable)
		}
		return provider.typed.ResolveWorkflowkitStageOperation(resolution)
	}
}

// standardAuthoringReviewResolutionOnlyExecutor must never be reached during
// a normal durable gate: workflowkitSpecPluginExecutor consumes the resolved
// operation and emits StageWaitExternalDecision itself. If a caller tries to
// execute this adapter directly, fail rather than accidentally auto-completing
// a human review stage.
type standardAuthoringReviewResolutionOnlyExecutor struct{}

func (standardAuthoringReviewResolutionOnlyExecutor) ExecuteStage(context.Context, workflowkit.StageExecutionRequest) (workflowkit.StageExecutionResult, error) {
	return workflowkit.StageExecutionResult{}, fmt.Errorf("%w: durable review must be handled by the Harbor review-gate adapter", ErrInvalidStageOperation)
}

var _ WorkflowkitStageOperationProvider = standardAuthoringOperationProvider{}
var _ workflowkit.StageExecutor = standardAuthoringReviewResolutionOnlyExecutor{}
