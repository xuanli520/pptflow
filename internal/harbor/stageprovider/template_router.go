package stageprovider

import (
	"fmt"
	"sort"

	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// TemplateWorkflowkitProviderRegistration installs one independently
// catalog-lock-attested provider bundle for one exact, closed workflow
// template.  It intentionally does not offer a default bundle: a Stage
// operation must carry the enclosing RunExecutionSpec.Template and select the
// same bundle at admission and execution time.
type TemplateWorkflowkitProviderRegistration struct {
	Template workflowadapter.TemplateReference
	Resolver *CatalogLockAttestedWorkflowkitProviderOperationResolver
}

// TemplateWorkflowkitProviderOperationResolver is the process-level
// multi-template provider boundary.  It is deliberately a router over fully
// independent CatalogLockAttested resolvers rather than a merged operation
// table: each child continues to verify its own catalog receipt, lock, and
// runtime attestation immediately before an external effect.
//
// In particular, equal stage keys, provider IDs, or operation IDs across
// templates never provide routing authority.  Only the template frozen in the
// RunExecutionSpec does.
type TemplateWorkflowkitProviderOperationResolver struct {
	byTemplate map[workflowadapter.TemplateReference]*CatalogLockAttestedWorkflowkitProviderOperationResolver
}

// NewTemplateWorkflowkitProviderOperationResolver validates the complete
// static deployment graph before a lifecycle service can admit a Run.  A
// catalog/lock resolver whose receipt names another template is rejected at
// composition time rather than becoming a dangerous fallback later.
func NewTemplateWorkflowkitProviderOperationResolver(registrations []TemplateWorkflowkitProviderRegistration) (*TemplateWorkflowkitProviderOperationResolver, error) {
	if len(registrations) == 0 {
		return nil, fmt.Errorf("%w: no template provider bundles are configured", ErrProviderUnavailable)
	}
	router := &TemplateWorkflowkitProviderOperationResolver{
		byTemplate: make(map[workflowadapter.TemplateReference]*CatalogLockAttestedWorkflowkitProviderOperationResolver, len(registrations)),
	}
	for index, registration := range registrations {
		if err := registration.Template.Validate(); err != nil {
			return nil, fmt.Errorf("validate template provider registration %d: %w", index, err)
		}
		if registration.Resolver == nil {
			return nil, fmt.Errorf("%w: template provider %s@%s is not configured", ErrProviderUnavailable, registration.Template.ID, registration.Template.Version)
		}
		if _, duplicate := router.byTemplate[registration.Template]; duplicate {
			return nil, fmt.Errorf("%w: duplicate template provider %s@%s", ErrInvalidStageOperation, registration.Template.ID, registration.Template.Version)
		}
		receipt := registration.Resolver.Receipt()
		if err := receipt.Validate(); err != nil {
			return nil, fmt.Errorf("validate template provider %s@%s receipt: %w", registration.Template.ID, registration.Template.Version, err)
		}
		if !receipt.Template.Equal(registration.Template) {
			return nil, fmt.Errorf("%w: template provider %s@%s receipt names %s@%s", ErrInvalidStageOperation, registration.Template.ID, registration.Template.Version, receipt.Template.ID, receipt.Template.Version)
		}
		identity := registration.Resolver.LockIdentity()
		if err := identity.Validate(); err != nil {
			return nil, fmt.Errorf("validate template provider %s@%s lock identity: %w", registration.Template.ID, registration.Template.Version, err)
		}
		if err := registration.Resolver.VerifyLockIdentity(identity); err != nil {
			return nil, fmt.Errorf("verify template provider %s@%s lock identity: %w", registration.Template.ID, registration.Template.Version, err)
		}
		router.byTemplate[registration.Template] = registration.Resolver
	}
	return router, nil
}

// Templates returns the installed closed template identities in canonical
// order.  It is useful for diagnostics and worker registry setup only; it
// cannot mutate the router or select a fallback template.
func (resolver *TemplateWorkflowkitProviderOperationResolver) Templates() []workflowadapter.TemplateReference {
	if resolver == nil {
		return nil
	}
	templates := make([]workflowadapter.TemplateReference, 0, len(resolver.byTemplate))
	for template := range resolver.byTemplate {
		templates = append(templates, template)
	}
	sort.Slice(templates, func(left, right int) bool {
		if templates[left].ID != templates[right].ID {
			return templates[left].ID < templates[right].ID
		}
		return templates[left].Version < templates[right].Version
	})
	return templates
}

func (resolver *TemplateWorkflowkitProviderOperationResolver) resolverFor(resolution workflowadapter.StageOperationResolution) (*CatalogLockAttestedWorkflowkitProviderOperationResolver, error) {
	if resolver == nil {
		return nil, fmt.Errorf("%w: template provider router is not configured", ErrProviderUnavailable)
	}
	if err := resolution.Template.Validate(); err != nil {
		return nil, fmt.Errorf("%w: stage operation template: %v", ErrInvalidStageOperation, err)
	}
	bundle, found := resolver.byTemplate[resolution.Template]
	if !found || bundle == nil {
		return nil, fmt.Errorf("%w: no provider bundle is installed for template %s@%s", ErrProviderUnavailable, resolution.Template.ID, resolution.Template.Version)
	}
	return bundle, nil
}

// ValidateStageOperation routes a prepare-time resolution only to the
// catalog/lock/provider bundle named by its frozen template.  The operation
// itself remains subject to the bundle's own exact catalog validation.
func (resolver *TemplateWorkflowkitProviderOperationResolver) ValidateStageOperation(resolution workflowadapter.StageOperationResolution) error {
	bundle, err := resolver.resolverFor(resolution)
	if err != nil {
		return err
	}
	return bundle.ValidateStageOperation(resolution.Clone())
}

// ResolveWorkflowkitStageOperation routes a worker-time request identically
// to admission.  The returned executor retains the chosen bundle's runtime
// attestor, so retries and reconciliation cannot cross a template boundary.
func (resolver *TemplateWorkflowkitProviderOperationResolver) ResolveWorkflowkitStageOperation(resolution workflowadapter.StageOperationResolution) (workflowkit.StageExecutor, error) {
	bundle, err := resolver.resolverFor(resolution)
	if err != nil {
		return nil, err
	}
	return bundle.ResolveWorkflowkitStageOperation(resolution.Clone())
}

var _ WorkflowkitProviderOperationResolver = (*TemplateWorkflowkitProviderOperationResolver)(nil)
