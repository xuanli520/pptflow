package stageprovider

import (
	"fmt"

	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// CatalogBoundWorkflowkitProviderOperationResolver composes the immutable
// deployment allow-list with the process-local typed handler registry. The
// catalog answers "may this frozen operation run in this deployment?"; the
// handler registry answers "is this exact provider implementation installed in
// this worker?". Both answers are required, in that order.
//
// This composition deliberately does not register missing operations from a
// caller RunExecutionSpec. A catalog entry without a matching local provider
// handler is a safe StartRun preflight rejection, not a fallback path.
type CatalogBoundWorkflowkitProviderOperationResolver struct {
	catalog   *DeploymentOperationCatalogResolver
	providers *ControlledWorkflowkitProviderRegistry
}

// NewCatalogBoundWorkflowkitProviderOperationResolver creates a pure
// preflight/execution resolver over one frozen catalog and one controlled
// process-local provider registry. An absent handler registry is retained as
// an explicit unavailable state so callers receive the same exact rejection at
// StartRun preflight that they would receive at worker execution.
func NewCatalogBoundWorkflowkitProviderOperationResolver(catalog *DeploymentOperationCatalogResolver, providers *ControlledWorkflowkitProviderRegistry) (*CatalogBoundWorkflowkitProviderOperationResolver, error) {
	if catalog == nil {
		return nil, ErrDeploymentOperationCatalogUnavailable
	}
	return &CatalogBoundWorkflowkitProviderOperationResolver{catalog: catalog, providers: providers}, nil
}

// ResolveWorkflowkitStageOperation validates the static catalog contract
// before it gives the resolution to a handler provider. In particular, a
// payload/runtime/checkout/secret drift never reaches a delegate that could
// otherwise interpret it as a real capability request.
func (resolver *CatalogBoundWorkflowkitProviderOperationResolver) ResolveWorkflowkitStageOperation(resolution workflowadapter.StageOperationResolution) (workflowkit.StageExecutor, error) {
	if resolver == nil || resolver.catalog == nil {
		return nil, ErrDeploymentOperationCatalogUnavailable
	}
	if _, err := resolver.catalog.ResolveStageOperation(resolution); err != nil {
		return nil, err
	}
	if resolver.providers == nil {
		return nil, fmt.Errorf("%w: controlled workflowkit provider registry is not configured", ErrProviderUnavailable)
	}
	return resolver.providers.ResolveWorkflowkitStageOperation(resolution)
}

// ValidateStageOperation implements workflowadapter.StageOperationResolver
// for StartRun admission. Provider resolution is still side-effect free by
// contract and proves both catalog authorization and local handler presence.
func (resolver *CatalogBoundWorkflowkitProviderOperationResolver) ValidateStageOperation(resolution workflowadapter.StageOperationResolution) error {
	_, err := resolver.ResolveWorkflowkitStageOperation(resolution)
	return err
}

var _ WorkflowkitProviderOperationResolver = (*CatalogBoundWorkflowkitProviderOperationResolver)(nil)
