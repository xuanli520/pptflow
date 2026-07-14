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

// CatalogIdentity exposes the installed immutable catalog binding for Run
// manifest freezing. It is intentionally delegated from the catalog resolver;
// the provider registry itself never changes the catalog identity.
func (resolver *CatalogBoundWorkflowkitProviderOperationResolver) CatalogIdentity() DeploymentOperationCatalogIdentity {
	if resolver == nil || resolver.catalog == nil {
		return DeploymentOperationCatalogIdentity{}
	}
	return resolver.catalog.CatalogIdentity()
}

// Template returns the exact closed template installed with the catalog. It
// is deliberately forwarded from the static catalog rather than inferred from
// a provider or a stage key.
func (resolver *CatalogBoundWorkflowkitProviderOperationResolver) Template() workflowadapter.TemplateReference {
	if resolver == nil || resolver.catalog == nil {
		return workflowadapter.TemplateReference{}
	}
	return resolver.catalog.Template()
}

// ValidateExecutionSpec validates a complete frozen specification against the
// exact catalog template and then proves each operation has a local typed
// provider handler. It closes the template gap that a bare
// StageOperationResolution intentionally cannot carry.
func (resolver *CatalogBoundWorkflowkitProviderOperationResolver) ValidateExecutionSpec(specification workflowadapter.RunExecutionSpec) error {
	if resolver == nil || resolver.catalog == nil {
		return ErrDeploymentOperationCatalogUnavailable
	}
	if err := resolver.catalog.verifyExecutionSpecTemplate(specification); err != nil {
		return err
	}
	if err := specification.ValidateWithOperationResolver(resolver); err != nil {
		return fmt.Errorf("validate frozen execution specification against catalog-bound providers: %w", err)
	}
	return nil
}

// ResolveExecutionSpecStageOperation resolves a typed public-engine executor
// only after the enclosing RunExecutionSpec has been proven template-bound to
// this catalog.
func (resolver *CatalogBoundWorkflowkitProviderOperationResolver) ResolveExecutionSpecStageOperation(specification workflowadapter.RunExecutionSpec, key workflowkit.StageKey) (workflowkit.StageExecutor, error) {
	if resolver == nil || resolver.catalog == nil {
		return nil, ErrDeploymentOperationCatalogUnavailable
	}
	if err := resolver.catalog.verifyExecutionSpecTemplate(specification); err != nil {
		return nil, err
	}
	resolution, err := specification.ResolveStageOperation(key)
	if err != nil {
		return nil, err
	}
	return resolver.ResolveWorkflowkitStageOperation(resolution)
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
