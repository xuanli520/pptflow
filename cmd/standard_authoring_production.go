package cmd

import (
	"fmt"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// Linker variables are intentionally blank in developer builds. A final
// package must inject a Standard-specific catalog receipt and lock identity;
// sharing the CodeEdge evaluator binding would make one template authorize a
// different template, so it is prohibited even when both locks describe the
// same Harbor Flow source build.
var (
	standardAuthoringProductionBuildModule                    string
	standardAuthoringProductionBuildVersion                   string
	standardAuthoringProductionBuildCommit                    string
	standardAuthoringProductionBuildContentSHA256             string
	standardAuthoringProductionBuildCatalogReceiptFingerprint string
	standardAuthoringProductionBuildLockID                    string
	standardAuthoringProductionBuildLockVersion               string
	standardAuthoringProductionBuildLockFingerprint           string
)

type standardAuthoringProductionBuildBinding struct {
	HarborFlowBuild           stageprovider.HarborFlowBuildIdentity
	CatalogReceiptFingerprint workflowkit.Fingerprint
	LockIdentity              stageprovider.DeploymentOperationCatalogLockIdentity
}

func (binding standardAuthoringProductionBuildBinding) Validate() error {
	if err := binding.HarborFlowBuild.Validate(); err != nil {
		return fmt.Errorf("Harbor Flow build identity: %w", err)
	}
	if err := binding.CatalogReceiptFingerprint.Validate(); err != nil {
		return fmt.Errorf("Standard authoring catalog receipt fingerprint: %w", err)
	}
	if err := binding.LockIdentity.Validate(); err != nil {
		return fmt.Errorf("Standard authoring operation catalog lock identity: %w", err)
	}
	return nil
}

// standardAuthoringProductionCompositionConfig contains only deployment-owned
// capability wiring. In particular, HostCommand is a required typed Git stage
// implementation; this helper never synthesizes a shell command, PATH
// fallback, or no-op repo_prepare executor.
type standardAuthoringProductionCompositionConfig struct {
	CatalogPath               string
	LockPath                  string
	ContractRoot              string
	CodexWorkspaceRoot        string
	ManagedRoot               string
	Store                     *store.Store
	Profile                   workflowadapter.ExecutionProfile
	HarborFlowBuild           stageprovider.HarborFlowBuildIdentity
	CatalogReceiptFingerprint workflowkit.Fingerprint
	LockIdentity              stageprovider.DeploymentOperationCatalogLockIdentity
	HostCommand               stageprovider.LocalCommandOperationExecutor
}

// standardAuthoringProductionComposition is a template-keyed capability
// bundle. Root composition combines it with the separate CodeEdge Phase-1 and
// evaluator-child bundles; this helper deliberately does not mutate or choose
// the process-wide default factory by itself.
type standardAuthoringProductionComposition struct {
	Resolver       *stageprovider.CatalogLockAttestedWorkflowkitProviderOperationResolver
	CatalogBinding app.TemplateDeploymentCatalogResolver
	SourceCapturer app.StandardAuthoringSourceCapturer
	Definitions    app.StandardAuthoringRunDefinitionProvider
}

func newStandardAuthoringProductionComposition(config standardAuthoringProductionCompositionConfig) (*standardAuthoringProductionComposition, error) {
	binding := standardAuthoringProductionBuildBinding{
		HarborFlowBuild: config.HarborFlowBuild, CatalogReceiptFingerprint: config.CatalogReceiptFingerprint, LockIdentity: config.LockIdentity,
	}
	if err := binding.Validate(); err != nil {
		return nil, fmt.Errorf("Standard authoring production build binding is unavailable or invalid: %w", err)
	}
	if config.Store == nil || strings.TrimSpace(config.ManagedRoot) == "" {
		return nil, fmt.Errorf("Standard authoring production managed root and Store are required")
	}
	if !config.Profile.Template.Equal(workflowadapter.StandardAuthoringTemplateReference()) {
		return nil, fmt.Errorf("Standard authoring production execution profile is required")
	}
	if err := config.Profile.Validate(); err != nil {
		return nil, fmt.Errorf("validate Standard authoring production execution profile: %w", err)
	}

	bundle, err := stageprovider.LoadStandardAuthoringDeploymentAssetBundle(config.CatalogPath, config.LockPath, config.ContractRoot)
	if err != nil {
		return nil, fmt.Errorf("load Standard authoring production deployment assets: %w", err)
	}
	if bundle.Verifier.HarborFlowBuild() != binding.HarborFlowBuild {
		return nil, fmt.Errorf("Standard authoring production build identity does not match the installed operation lock")
	}
	receiptFingerprint, err := bundle.Verifier.CatalogReceipt().Fingerprint()
	if err != nil {
		return nil, fmt.Errorf("fingerprint installed Standard authoring catalog receipt: %w", err)
	}
	if receiptFingerprint != binding.CatalogReceiptFingerprint || bundle.Verifier.LockIdentity() != binding.LockIdentity {
		return nil, fmt.Errorf("Standard authoring production catalog/lock does not match the installed binary binding")
	}
	attestor, err := stageprovider.NewStandardAuthoringRuntimeAttestor(stageprovider.StandardAuthoringRuntimeAttestorConfig{
		HarborFlowBuild: binding.HarborFlowBuild, ContractRoot: bundle.ContractRoot,
	})
	if err != nil {
		return nil, fmt.Errorf("construct Standard authoring runtime attestor: %w", err)
	}
	lockedGit, err := standardAuthoringLockedGit(bundle.Lock)
	if err != nil {
		return nil, err
	}
	capturer, err := app.NewLockedStandardAuthoringGitArchiveSourceCapturer(lockedGit)
	if err != nil {
		return nil, fmt.Errorf("construct Standard authoring source capturer: %w", err)
	}
	materializer, err := app.NewStandardAuthoringMaterializeExecutor(app.StandardAuthoringMaterializeExecutorConfig{ManagedRoot: config.ManagedRoot, Store: config.Store})
	if err != nil {
		return nil, fmt.Errorf("construct Standard authoring materializer: %w", err)
	}
	providers, err := stageprovider.NewStandardAuthoringProviderComposition(stageprovider.StandardAuthoringProviderCompositionConfig{
		Template: workflowadapter.StandardAuthoringTemplateReference(), Catalog: bundle.Catalog, Lock: bundle.Lock, Attestor: attestor,
		Handlers:           stageprovider.StandardAuthoringOperationHandlers{HostCommand: config.HostCommand, HarborBuiltin: materializer},
		CodexWorkspaceRoot: config.CodexWorkspaceRoot,
	})
	if err != nil {
		return nil, fmt.Errorf("construct Standard authoring provider composition: %w", err)
	}
	definitions, err := app.NewCatalogStandardAuthoringRunDefinitionProvider(bundle.Catalog, config.Profile)
	if err != nil {
		return nil, fmt.Errorf("construct Standard authoring run definition provider: %w", err)
	}
	return &standardAuthoringProductionComposition{
		Resolver: providers.Resolver,
		CatalogBinding: app.TemplateDeploymentCatalogResolver{
			Template: workflowadapter.StandardAuthoringTemplateReference(), Resolver: providers.Resolver,
		},
		SourceCapturer: capturer,
		Definitions:    definitions,
	}, nil
}

func standardAuthoringLockedGit(lock stageprovider.DeploymentOperationCatalogLock) (stageprovider.LocalExecutableLock, error) {
	var found *stageprovider.LocalExecutableLock
	for _, record := range lock.Operations {
		if record.Stage.Key != workflowkit.StageKey(workflowadapter.RepoPrepare) {
			continue
		}
		payload, isLocalCommand := record.Operation.Payload.(workflowadapter.LocalCommandOperationPayload)
		if !isLocalCommand || payload.CommandID != stageprovider.StandardAuthoringGitSnapshotCommandID || len(payload.Arguments) != 0 || record.LocalExecutable == nil {
			return stageprovider.LocalExecutableLock{}, fmt.Errorf("Standard authoring repo_prepare lock does not bind the approved Git snapshot command")
		}
		candidate := *record.LocalExecutable
		if found != nil && *found != candidate {
			return stageprovider.LocalExecutableLock{}, fmt.Errorf("Standard authoring lock has conflicting Git snapshot executables")
		}
		found = &candidate
	}
	if found == nil {
		return stageprovider.LocalExecutableLock{}, fmt.Errorf("Standard authoring lock has no Git snapshot executable")
	}
	return *found, nil
}

func linkedStandardAuthoringProductionBuildBinding() (standardAuthoringProductionBuildBinding, error) {
	binding := standardAuthoringProductionBuildBinding{
		HarborFlowBuild: stageprovider.HarborFlowBuildIdentity{
			Module: standardAuthoringProductionBuildModule, Version: standardAuthoringProductionBuildVersion,
			Commit: standardAuthoringProductionBuildCommit, ContentSHA256: workflowkitFingerprint(standardAuthoringProductionBuildContentSHA256),
		},
		CatalogReceiptFingerprint: workflowkitFingerprint(standardAuthoringProductionBuildCatalogReceiptFingerprint),
		LockIdentity: stageprovider.DeploymentOperationCatalogLockIdentity{
			LockID: standardAuthoringProductionBuildLockID, LockVersion: standardAuthoringProductionBuildLockVersion,
			Fingerprint: workflowkitFingerprint(standardAuthoringProductionBuildLockFingerprint),
		},
	}
	if err := binding.Validate(); err != nil {
		return standardAuthoringProductionBuildBinding{}, fmt.Errorf("Standard authoring production build binding is unavailable; build with a generated Standard operation lock and linker binding: %w", err)
	}
	return binding, nil
}
