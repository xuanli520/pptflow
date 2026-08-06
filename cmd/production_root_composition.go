package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// harborFlowProductionCompositionConfig names the Standard authoring
// capability bundle that makes up one local Harbor Flow package. A Run has no
// access to these locations or identities: routing happens only through its
// already-frozen TemplateReference.
type harborFlowProductionCompositionConfig struct {
	Paths             productionDeploymentPaths
	StandardBinding   standardAuthoringProductionBuildBinding
	LookupEnvironment func(string) (string, bool)
}

// newHarborFlowProductionLifecycleServices is the sole production factory
// used by CLI, TUI, foreground workers, and detached workers. It composes the
// Standard authoring bundle as the only executable production template.
func newHarborFlowProductionLifecycleServices(root string, dataStore *store.Store) (*app.LifecycleServices, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("managed root is required for Harbor Flow production composition")
	}
	standardBinding, err := linkedStandardAuthoringProductionBuildBinding()
	if err != nil {
		return nil, err
	}
	paths, err := defaultProductionDeploymentPaths()
	if err != nil {
		return nil, err
	}
	return newHarborFlowProductionLifecycleServicesWithConfig(root, dataStore, harborFlowProductionCompositionConfig{
		Paths:             paths,
		StandardBinding:   standardBinding,
		LookupEnvironment: os.LookupEnv,
	})
}

// preflightHarborFlowProductionLifecycleServices proves the linker binding
// and immutable package layout before a CLI/TUI command opens its mutable
// Store. The full factory repeats catalog/lock verification while binding
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
	if err := standardBinding.Validate(); err != nil {
		return fmt.Errorf("Standard authoring production binding: %w", err)
	}
	paths, err := defaultProductionDeploymentPaths()
	if err != nil {
		return err
	}
	return preflightHarborFlowProductionDeploymentBundles(paths, standardBinding)
}

// preflightHarborFlowProductionDeploymentBundles verifies every static catalog,
// lock, asset, and linker binding before a CLI/TUI caller opens the mutable
// Store. It deliberately constructs no executor, attestor, workspace, or
// lifecycle service, so a stale package is rejected without control-plane
// filesystem side effects.
func preflightHarborFlowProductionDeploymentBundles(paths productionDeploymentPaths, standardBinding standardAuthoringProductionBuildBinding) error {
	if err := standardBinding.Validate(); err != nil {
		return fmt.Errorf("Standard authoring production binding: %w", err)
	}
	standard, err := stageprovider.LoadStandardAuthoringDeploymentAssetBundle(paths.StandardCatalog, paths.StandardLock, paths.StandardContractRoot)
	if err != nil {
		return fmt.Errorf("load Standard authoring production deployment assets: %w", err)
	}
	if err := verifyHarborFlowProductionBundleBinding("Standard authoring", standard.Verifier, standardBinding.HarborFlowBuild, standardBinding.CatalogReceiptFingerprint, standardBinding.LockIdentity); err != nil {
		return err
	}
	return nil
}

func verifyHarborFlowProductionBundleBinding(label string, verifier stageprovider.DeploymentOperationCatalogLockVerifier, build stageprovider.HarborFlowBuildIdentity, expectedReceipt workflowkit.Fingerprint, expectedLock stageprovider.DeploymentOperationCatalogLockIdentity) error {
	if verifier == nil {
		return fmt.Errorf("%s deployment lock verifier is required", label)
	}
	if verifier.HarborFlowBuild() != build {
		return fmt.Errorf("%s production build identity does not match the installed operation lock", label)
	}
	receipt, err := verifier.CatalogReceipt().Fingerprint()
	if err != nil {
		return fmt.Errorf("fingerprint installed %s catalog receipt: %w", label, err)
	}
	if receipt != expectedReceipt || verifier.LockIdentity() != expectedLock {
		return fmt.Errorf("%s production catalog/lock does not match the installed binary binding", label)
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
	legacyRuns, err := dataStore.ListActiveLegacyTemplateRuns(context.Background(), workflowadapter.StandardAuthoringWorkflowTemplateID, workflowadapter.StandardAuthoringContractTemplateVersion)
	if err != nil {
		return nil, fmt.Errorf("preflight active retired Standard authoring runs: %w", err)
	}
	if len(legacyRuns) != 0 {
		ids := make([]string, 0, len(legacyRuns))
		for _, run := range legacyRuns {
			ids = append(ids, run.ID)
		}
		return nil, fmt.Errorf("Standard authoring 3.0 deployment is blocked by active retired runs %s; an operator must explicitly terminate them", strings.Join(ids, ", "))
	}

	standard, err := newStandardAuthoringProductionComposition(standardAuthoringProductionCompositionConfig{
		CatalogPath: config.Paths.StandardCatalog, LockPath: config.Paths.StandardLock, ContractRoot: config.Paths.StandardContractRoot,
		ManagedRoot: root, Store: dataStore, HarborFlowBuild: config.StandardBinding.HarborFlowBuild,
		CatalogReceiptFingerprint: config.StandardBinding.CatalogReceiptFingerprint, LockIdentity: config.StandardBinding.LockIdentity,
		LookupEnvironment: config.LookupEnvironment,
	})
	if err != nil {
		return nil, err
	}

	router, err := stageprovider.NewTemplateWorkflowkitProviderOperationResolver([]stageprovider.TemplateWorkflowkitProviderRegistration{
		{Template: standard.CatalogBinding.Template, Resolver: standard.Resolver},
	})
	if err != nil {
		return nil, fmt.Errorf("construct template-scoped production provider router: %w", err)
	}

	return app.NewLifecycleServicesWithOptions(root, dataStore, app.LifecycleServicesOptions{
		OperationResolver:                      router,
		DeploymentCatalogResolvers:             []app.TemplateDeploymentCatalogResolver{standard.CatalogBinding},
		RequireDeploymentCatalog:               true,
		StandardAuthoringSourceCapturer:        standard.SourceCapturer,
		StandardAuthoringRunDefinitionProvider: standard.Definitions,
		RunWorkerHandoffLauncher:               executableRunWorkerLauncher{},
	})
}
