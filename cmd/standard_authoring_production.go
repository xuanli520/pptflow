package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/codeedge"
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
// static inputs. The repo_prepare executor is constructed here from the
// generated lock and managed root; callers cannot inject a shell command,
// PATH fallback, no-op, or alternate workspace implementation.
type standardAuthoringProductionCompositionConfig struct {
	CatalogPath               string
	LockPath                  string
	ContractRoot              string
	ManagedRoot               string
	Store                     *store.Store
	HarborFlowBuild           stageprovider.HarborFlowBuildIdentity
	CatalogReceiptFingerprint workflowkit.Fingerprint
	LockIdentity              stageprovider.DeploymentOperationCatalogLockIdentity
	LookupEnvironment         func(string) (string, bool)
	AdmissionContract         *codeedge.TaskAdmissionContract
	CandidateHarness          *app.StandardAuthoringDockerHarness
	CandidateDockerCommands   []stageprovider.LocalExecutableLock
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
	profile, err := bundle.Lock.StandardAuthoringProfile()
	if err != nil {
		return nil, fmt.Errorf("load lock-owned Standard authoring execution profile: %w", err)
	}
	candidateHarness := config.CandidateHarness
	if candidateHarness == nil && len(config.CandidateDockerCommands) != 0 {
		commandTimeout, err := standardAuthoringCandidateCommandTimeout(profile)
		if err != nil {
			return nil, err
		}
		candidateHarness, err = app.NewStandardAuthoringDockerHarness(app.StandardAuthoringDockerHarnessConfig{
			ManagedRoot: config.ManagedRoot, LockedCommands: config.CandidateDockerCommands, CommandTimeout: commandTimeout,
		})
		if err != nil {
			return nil, fmt.Errorf("construct Standard authoring Docker harness: %w", err)
		}
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
	sshTransport, err := bundle.Lock.StandardAuthoringSSHTransportLock()
	if err != nil {
		return nil, fmt.Errorf("load Standard authoring locked SSH transport: %w", err)
	}
	capturer, err := app.NewLockedStandardAuthoringGitArchiveSourceCapturerWithSSHTransport(lockedGit, app.StandardAuthoringSSHSourceCaptureTransportConfig{
		ContractRoot: bundle.ContractRoot, Transport: sshTransport, LookupEnvironment: config.LookupEnvironment,
	})
	if err != nil {
		return nil, fmt.Errorf("construct Standard authoring source capturer: %w", err)
	}
	workspaceRoot, err := app.StandardAuthoringCodexWorkspaceRoot(config.ManagedRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve Standard authoring Codex workspace root: %w", err)
	}
	repoPrepare, err := app.NewStandardAuthoringRepoPrepareExecutor(app.StandardAuthoringRepoPrepareExecutorConfig{
		ManagedRoot: config.ManagedRoot, Store: config.Store, LockedGit: lockedGit, WorkspaceRoot: workspaceRoot,
	})
	if err != nil {
		return nil, fmt.Errorf("construct Standard authoring repo_prepare executor: %w", err)
	}
	materializer, err := app.NewStandardAuthoringMaterializeExecutor(app.StandardAuthoringMaterializeExecutorConfig{ManagedRoot: config.ManagedRoot, Store: config.Store, Admission: config.AdmissionContract, CandidateHarness: candidateHarness})
	if err != nil {
		return nil, fmt.Errorf("construct Standard authoring materializer: %w", err)
	}
	providers, err := stageprovider.NewStandardAuthoringProviderComposition(stageprovider.StandardAuthoringProviderCompositionConfig{
		Template: bundle.Catalog.Template(), Catalog: bundle.Catalog, Lock: bundle.Lock, Attestor: attestor,
		Handlers:            stageprovider.StandardAuthoringOperationHandlers{HostCommand: repoPrepare, HarborBuiltin: materializer},
		CodexWorkspaceRoot:  workspaceRoot,
		CodexWorkspaceMode:  stageprovider.StandardAuthoringCodexWorkspaceRunScoped,
		CodexSourceVerifier: repoPrepare,
		CandidateValidator:  candidateHarness,
	})
	if err != nil {
		return nil, fmt.Errorf("construct Standard authoring provider composition: %w", err)
	}
	definitions, err := app.NewCatalogStandardAuthoringRunDefinitionProvider(bundle.Catalog, profile)
	if err != nil {
		return nil, fmt.Errorf("construct Standard authoring run definition provider: %w", err)
	}
	composition := &standardAuthoringProductionComposition{
		Resolver: providers.Resolver,
		CatalogBinding: app.TemplateDeploymentCatalogResolver{
			Template: bundle.Catalog.Template(), Resolver: providers.Resolver,
		},
		SourceCapturer: capturer,
		Definitions:    definitions,
	}
	return composition, nil
}

// standardAuthoringCandidateCommandTimeout uses the owning template's frozen
// host verification attempt budget. The outer stage context remains the final
// deadline, while the Docker runtime must not impose the shorter CodeEdge
// parent-command ceiling on a potentially long environment build.
func standardAuthoringCandidateCommandTimeout(profile workflowadapter.ExecutionProfile) (time.Duration, error) {
	if err := profile.Validate(); err != nil {
		return 0, err
	}
	budget, found := profile.Budget(workflowkit.StageKey(workflowadapter.HostCandidateVerify))
	if !found || budget.AttemptTimeout <= 0 {
		return 0, fmt.Errorf("Standard authoring profile omits a usable command budget for %q", workflowadapter.HostCandidateVerify)
	}
	return budget.AttemptTimeout, nil
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
		return standardAuthoringProductionBuildBinding{}, fmt.Errorf("Standard authoring production build binding is unavailable; build with scripts/build-codeedge-production.sh: %w", err)
	}
	return binding, nil
}
