package stageprovider

import (
	"context"
	"fmt"

	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	// CodeEdgePhase1ProviderID is the sole provider identity accepted by the
	// parent production catalog. It is intentionally distinct from both the
	// Standard authoring provider and the evaluator-child provider.
	CodeEdgePhase1ProviderID      = "harbor-codeedge-phase1"
	CodeEdgePhase1ProviderKind    = "codeedge-phase1"
	CodeEdgePhase1ProviderVersion = "1.0.0"
)

// CodeEdgePhase1OperationHandlers is the narrow injection boundary for the
// parent template. Commands are selected only by an already locked payload;
// callers cannot pass executable paths, argv, image names, model settings, or
// arbitrary stage configuration.
type CodeEdgePhase1OperationHandlers struct {
	LocalCommand  LocalCommandOperationExecutor
	HarborBuiltin HarborBuiltinOperationExecutor
	DurableReview workflowkit.StageExecutor
}

// CodeEdgePhase1ProviderCompositionConfig contains static parent deployment
// materials and the typed local implementations selected by higher-level
// application composition.
type CodeEdgePhase1ProviderCompositionConfig struct {
	Template workflowadapter.TemplateReference
	Catalog  *DeploymentOperationCatalogResolver
	Lock     DeploymentOperationCatalogLock
	Attestor *CodeEdgePhase1RuntimeAttestor
	Handlers CodeEdgePhase1OperationHandlers
}

// CodeEdgePhase1ProviderComposition exposes the immutable resolver chain for
// one exact parent template.
type CodeEdgePhase1ProviderComposition struct {
	Catalog  *DeploymentOperationCatalogResolver
	Verifier *DeploymentOperationCatalogLockResolver
	Resolver *CatalogLockAttestedWorkflowkitProviderOperationResolver
}

// NewCodeEdgePhase1ProviderComposition builds the strict parent chain:
// catalog allow-list -> typed handlers -> catalog/lock verifier -> runtime
// attestation. It rejects evaluator-child-only payload kinds at construction
// time, rather than leaving an accidental fallback reachable at execution.
func NewCodeEdgePhase1ProviderComposition(config CodeEdgePhase1ProviderCompositionConfig) (*CodeEdgePhase1ProviderComposition, error) {
	if err := config.Template.Validate(); err != nil {
		return nil, fmt.Errorf("validate CodeEdge Phase-1 template: %w", err)
	}
	if !config.Template.Equal(workflowadapter.CodeEdgePhase1TemplateReference()) {
		return nil, fmt.Errorf("%w: CodeEdge Phase-1 composition requires template %s@%s", ErrDeploymentOperationCatalogDrift, workflowadapter.CodeEdgePhase1WorkflowTemplateID, workflowadapter.CodeEdgePhase1WorkflowTemplateVersion)
	}
	if config.Catalog == nil {
		return nil, ErrDeploymentOperationCatalogUnavailable
	}
	if !config.Catalog.Template().Equal(config.Template) {
		return nil, fmt.Errorf("%w: CodeEdge Phase-1 catalog names another template", ErrDeploymentOperationCatalogDrift)
	}
	if config.Attestor == nil {
		return nil, ErrDeploymentOperationRuntimeAttestationUnavailable
	}
	verifier, err := NewDeploymentOperationCatalogLockResolver(config.Catalog, config.Lock)
	if err != nil {
		return nil, err
	}
	if !verifier.CatalogReceipt().Template.Equal(config.Template) {
		return nil, fmt.Errorf("%w: CodeEdge Phase-1 lock receipt names another template", ErrDeploymentOperationCatalogLockDrift)
	}
	if verifier.HarborFlowBuild() != config.Attestor.HarborFlowBuild() {
		return nil, fmt.Errorf("%w: CodeEdge Phase-1 attestor build identity differs from deployment lock", ErrDeploymentOperationRuntimeAttestationFailed)
	}

	operations := verifier.Lock().Operations
	if len(operations) != len(workflowadapter.CodeEdgePhase1StageOrder()) {
		return nil, fmt.Errorf("%w: CodeEdge Phase-1 lock has %d operations, want exact parent stage coverage %d", ErrStageOperationUnavailable, len(operations), len(workflowadapter.CodeEdgePhase1StageOrder()))
	}
	provider := workflowadapter.ProviderReference{ID: CodeEdgePhase1ProviderID, Kind: CodeEdgePhase1ProviderKind, Version: CodeEdgePhase1ProviderVersion}
	typedRegistrations := make([]TypedWorkflowkitStageOperationRegistration, 0, len(operations))
	reviewRegistrations := make([]WorkflowkitStaticStageOperationRegistration, 0, len(operations))
	reviewExecutor := config.Handlers.DurableReview
	if isNilWorkflowkitStageExecutor(reviewExecutor) {
		reviewExecutor = codeEdgePhase1ReviewResolutionOnlyExecutor{}
	}
	seenStages := make(map[workflowkit.StageKey]struct{}, len(operations))
	for _, record := range operations {
		if err := record.Validate(); err != nil {
			return nil, fmt.Errorf("validate CodeEdge Phase-1 lock operation: %w", err)
		}
		if record.Provider != provider {
			return nil, fmt.Errorf("%w: CodeEdge Phase-1 catalog must use only provider %s (%s@%s)", ErrDeploymentOperationCatalogDrift, CodeEdgePhase1ProviderID, CodeEdgePhase1ProviderKind, CodeEdgePhase1ProviderVersion)
		}
		if _, duplicate := seenStages[record.Stage.Key]; duplicate {
			return nil, fmt.Errorf("%w: CodeEdge Phase-1 lock duplicates stage %q", ErrInvalidStageOperation, record.Stage.Key)
		}
		seenStages[record.Stage.Key] = struct{}{}
		switch record.Operation.Payload.(type) {
		case workflowadapter.LocalCommandOperationPayload, workflowadapter.HarborBuiltinOperationPayload:
			typedRegistrations = append(typedRegistrations, TypedWorkflowkitStageOperationRegistration{StageKey: record.Stage.Key, Operation: record.Operation.Clone()})
		case workflowadapter.DurableReviewOperationPayload:
			reviewRegistrations = append(reviewRegistrations, WorkflowkitStaticStageOperationRegistration{StageKey: record.Stage.Key, Operation: record.Operation.Clone(), Executor: reviewExecutor})
		case workflowadapter.AgentTurnOperationPayload, workflowadapter.ContainerCommandOperationPayload:
			return nil, fmt.Errorf("%w: CodeEdge Phase-1 parent does not authorize %s", ErrDeploymentOperationRuntimeAttestationUnavailable, record.ExecutionKind)
		default:
			return nil, fmt.Errorf("%w: unsupported CodeEdge Phase-1 operation payload %T", ErrStageOperationUnavailable, record.Operation.Payload)
		}
	}
	for _, stageKey := range workflowadapter.CodeEdgePhase1StageOrder() {
		if _, found := seenStages[stageKey]; !found {
			return nil, fmt.Errorf("%w: CodeEdge Phase-1 lock omits stage %q", ErrStageOperationUnavailable, stageKey)
		}
	}
	typed, err := NewTypedWorkflowkitStageOperationProvider(TypedWorkflowkitStageOperationProviderConfig{
		Handlers: TypedWorkflowkitOperationHandlers{
			LocalCommand:  config.Handlers.LocalCommand,
			HarborBuiltin: config.Handlers.HarborBuiltin,
		},
		Operations: typedRegistrations,
	})
	if err != nil {
		return nil, fmt.Errorf("construct CodeEdge Phase-1 typed provider: %w", err)
	}
	reviews, err := NewStaticWorkflowkitStageOperationProvider(reviewRegistrations)
	if err != nil {
		return nil, fmt.Errorf("construct CodeEdge Phase-1 durable review provider: %w", err)
	}
	providers, err := NewControlledWorkflowkitProviderRegistry([]WorkflowkitProviderRegistration{{
		Provider: provider, Adapter: codeEdgePhase1OperationProvider{typed: typed, reviews: reviews},
	}})
	if err != nil {
		return nil, fmt.Errorf("register CodeEdge Phase-1 provider: %w", err)
	}
	catalogBound, err := NewCatalogBoundWorkflowkitProviderOperationResolver(config.Catalog, providers)
	if err != nil {
		return nil, fmt.Errorf("bind CodeEdge Phase-1 provider to catalog: %w", err)
	}
	resolver, err := NewCatalogLockAttestedWorkflowkitProviderOperationResolver(verifier, catalogBound, config.Attestor)
	if err != nil {
		return nil, fmt.Errorf("bind CodeEdge Phase-1 provider to catalog lock: %w", err)
	}
	return &CodeEdgePhase1ProviderComposition{Catalog: config.Catalog, Verifier: verifier, Resolver: resolver}, nil
}

type codeEdgePhase1OperationProvider struct {
	typed   *TypedWorkflowkitStageOperationProvider
	reviews *StaticWorkflowkitStageOperationProvider
}

func (provider codeEdgePhase1OperationProvider) ResolveWorkflowkitStageOperation(resolution workflowadapter.StageOperationResolution) (workflowkit.StageExecutor, error) {
	switch resolution.Operation.Payload.(type) {
	case workflowadapter.DurableReviewOperationPayload:
		if provider.reviews == nil {
			return nil, fmt.Errorf("%w: CodeEdge Phase-1 durable review provider is unavailable", ErrStageOperationUnavailable)
		}
		return provider.reviews.ResolveWorkflowkitStageOperation(resolution)
	default:
		if provider.typed == nil {
			return nil, fmt.Errorf("%w: CodeEdge Phase-1 typed provider is unavailable", ErrStageOperationUnavailable)
		}
		return provider.typed.ResolveWorkflowkitStageOperation(resolution)
	}
}

// codeEdgePhase1ReviewResolutionOnlyExecutor must never execute a normal
// gate: workflowkit's Harbor adapter resolves it solely to prove the frozen
// review policy, then projects an external-decision wait.
type codeEdgePhase1ReviewResolutionOnlyExecutor struct{}

func (codeEdgePhase1ReviewResolutionOnlyExecutor) ExecuteStage(context.Context, workflowkit.StageExecutionRequest) (workflowkit.StageExecutionResult, error) {
	return workflowkit.StageExecutionResult{}, fmt.Errorf("%w: CodeEdge Phase-1 durable review must be handled by the Harbor review-gate adapter", ErrInvalidStageOperation)
}

var _ WorkflowkitStageOperationProvider = codeEdgePhase1OperationProvider{}
var _ workflowkit.StageExecutor = codeEdgePhase1ReviewResolutionOnlyExecutor{}
