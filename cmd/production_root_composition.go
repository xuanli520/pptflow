package cmd

import (
	"fmt"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
)

// harborFlowProductionCompositionConfig names the three independently
// attested capability bundles that make up one local Harbor Flow package.
// A Run has no access to these locations or identities: routing happens only
// through its already-frozen TemplateReference.
type harborFlowProductionCompositionConfig struct {
	Paths                 productionDeploymentPaths
	StandardBinding       standardAuthoringProductionBuildBinding
	CodeEdgePhase1Binding codeEdgePhase1ProductionBuildBinding
	EvaluatorBinding      codeEdgeProductionBuildBinding
	LookupEnvironment     func(string) (string, bool)
}

// newHarborFlowProductionLifecycleServices is the sole production factory
// used by CLI, TUI, foreground workers, and detached workers.  It composes
// the Standard authoring, CodeEdge Phase-1 parent, and evaluator-child
// bundles as peers; none is a fallback for another template.
func newHarborFlowProductionLifecycleServices(root string, dataStore *store.Store) (*app.LifecycleServices, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("managed root is required for Harbor Flow production composition")
	}
	standardBinding, err := linkedStandardAuthoringProductionBuildBinding()
	if err != nil {
		return nil, err
	}
	parentBinding, err := linkedCodeEdgePhase1ProductionBuildBinding()
	if err != nil {
		return nil, err
	}
	evaluatorBinding, err := linkedCodeEdgeProductionBuildBinding()
	if err != nil {
		return nil, err
	}
	paths, err := defaultProductionDeploymentPaths()
	if err != nil {
		return nil, err
	}
	return newHarborFlowProductionLifecycleServicesWithConfig(root, dataStore, harborFlowProductionCompositionConfig{
		Paths:                 paths,
		StandardBinding:       standardBinding,
		CodeEdgePhase1Binding: parentBinding,
		EvaluatorBinding:      evaluatorBinding,
	})
}

// preflightHarborFlowProductionLifecycleServices proves the linker bindings
// and immutable package layout before a CLI/TUI command opens its mutable
// Store.  The full factory repeats catalog/lock verification while binding
// the Store; this early check exists solely to prevent invalid packages from
// creating control-plane state as a side effect of discovery.
func preflightHarborFlowProductionLifecycleServices(root string) error {
	if strings.TrimSpace(root) == "" {
		return fmt.Errorf("managed root is required for Harbor Flow production composition")
	}
	standardBinding, err := linkedStandardAuthoringProductionBuildBinding()
	if err != nil {
		return err
	}
	parentBinding, err := linkedCodeEdgePhase1ProductionBuildBinding()
	if err != nil {
		return err
	}
	evaluatorBinding, err := linkedCodeEdgeProductionBuildBinding()
	if err != nil {
		return err
	}
	if err := validateHarborFlowProductionBuildIdentity(standardBinding.HarborFlowBuild, parentBinding.HarborFlowBuild, evaluatorBinding.HarborFlowBuild); err != nil {
		return err
	}
	if _, err := defaultProductionDeploymentPaths(); err != nil {
		return err
	}
	return nil
}

func newHarborFlowProductionLifecycleServicesWithConfig(root string, dataStore *store.Store, config harborFlowProductionCompositionConfig) (*app.LifecycleServices, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("managed root is required for Harbor Flow production composition")
	}
	if dataStore == nil {
		return nil, fmt.Errorf("lifecycle store is required for Harbor Flow production composition")
	}
	if err := config.StandardBinding.Validate(); err != nil {
		return nil, fmt.Errorf("Standard authoring production binding: %w", err)
	}
	if err := config.CodeEdgePhase1Binding.Validate(); err != nil {
		return nil, fmt.Errorf("CodeEdge Phase-1 production binding: %w", err)
	}
	if err := config.EvaluatorBinding.Validate(); err != nil {
		return nil, fmt.Errorf("CodeEdge evaluator production binding: %w", err)
	}
	if err := validateHarborFlowProductionBuildIdentity(config.StandardBinding.HarborFlowBuild, config.CodeEdgePhase1Binding.HarborFlowBuild, config.EvaluatorBinding.HarborFlowBuild); err != nil {
		return nil, err
	}

	standard, err := newStandardAuthoringProductionComposition(standardAuthoringProductionCompositionConfig{
		CatalogPath:               config.Paths.StandardCatalog,
		LockPath:                  config.Paths.StandardLock,
		ContractRoot:              config.Paths.StandardContractRoot,
		ManagedRoot:               root,
		Store:                     dataStore,
		HarborFlowBuild:           config.StandardBinding.HarborFlowBuild,
		CatalogReceiptFingerprint: config.StandardBinding.CatalogReceiptFingerprint,
		LockIdentity:              config.StandardBinding.LockIdentity,
	})
	if err != nil {
		return nil, err
	}
	parent, err := newCodeEdgePhase1ProductionComposition(codeEdgePhase1ProductionCompositionConfig{
		CatalogPath:               config.Paths.ParentCatalog,
		LockPath:                  config.Paths.ParentLock,
		ManagedRoot:               root,
		HarborFlowBuild:           config.CodeEdgePhase1Binding.HarborFlowBuild,
		CatalogReceiptFingerprint: config.CodeEdgePhase1Binding.CatalogReceiptFingerprint,
		LockIdentity:              config.CodeEdgePhase1Binding.LockIdentity,
	})
	if err != nil {
		return nil, err
	}
	evaluator, err := newCodeEdgeEvaluatorProductionCompositionWithConfig(root, dataStore, codeEdgeProductionCompositionConfig{
		CatalogPath:               config.Paths.EvaluatorCatalog,
		LockPath:                  config.Paths.EvaluatorLock,
		HarborFlowBuild:           config.EvaluatorBinding.HarborFlowBuild,
		CatalogReceiptFingerprint: config.EvaluatorBinding.CatalogReceiptFingerprint,
		LockIdentity:              config.EvaluatorBinding.LockIdentity,
		LookupEnvironment:         config.LookupEnvironment,
	})
	if err != nil {
		return nil, err
	}

	router, err := stageprovider.NewTemplateWorkflowkitProviderOperationResolver([]stageprovider.TemplateWorkflowkitProviderRegistration{
		{Template: workflowadapter.StandardAuthoringTemplateReference(), Resolver: standard.Resolver},
		{Template: workflowadapter.CodeEdgePhase1TemplateReference(), Resolver: parent.Resolver},
		{Template: workflowadapter.CodeEdgeEvaluatorChildTemplateReference(), Resolver: evaluator.Resolver},
	})
	if err != nil {
		return nil, fmt.Errorf("construct template-scoped production provider router: %w", err)
	}

	return app.NewLifecycleServicesWithOptions(root, dataStore, app.LifecycleServicesOptions{
		OperationResolver:                      router,
		DeploymentCatalogResolvers:             []app.TemplateDeploymentCatalogResolver{standard.CatalogBinding, parent.CatalogBinding, evaluator.CatalogBinding},
		RequireDeploymentCatalog:               true,
		RequireDeploymentLock:                  true,
		StandardAuthoringSourceCapturer:        standard.SourceCapturer,
		StandardAuthoringRunDefinitionProvider: standard.Definitions,
		CodeEdgePhase1RunDefinitionProvider:    parent.Definitions,
		EvaluatorRunDefinitionProvider:         evaluator.Definitions,
		CodeEdgeEvaluatorObserver:              evaluator.Observer,
	})
}

func validateHarborFlowProductionBuildIdentity(standard, parent, evaluator stageprovider.HarborFlowBuildIdentity) error {
	for _, binding := range []struct {
		label string
		value stageprovider.HarborFlowBuildIdentity
	}{
		{label: "Standard authoring", value: standard},
		{label: "CodeEdge Phase-1", value: parent},
		{label: "CodeEdge evaluator child", value: evaluator},
	} {
		if err := binding.value.Validate(); err != nil {
			return fmt.Errorf("%s Harbor Flow build identity: %w", binding.label, err)
		}
	}
	if standard != parent || standard != evaluator {
		return fmt.Errorf("all three production deployment locks must bind the same Harbor Flow build identity")
	}
	return nil
}
