package cmd

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/codeedge"
	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// codeEdgePhase1ProductionCompositionConfig contains only parent deployment
// assets and their linker-bound identity. The parent executor obtains its
// metadata mapping, protected environment names, executable paths and timing
// from the verified lock; this configuration deliberately has no caller
// profile, Docker image, command, workspace, or secret knob.
type codeEdgePhase1ProductionCompositionConfig struct {
	CatalogPath               string
	LockPath                  string
	ManagedRoot               string
	HarborFlowBuild           stageprovider.HarborFlowBuildIdentity
	CatalogReceiptFingerprint workflowkit.Fingerprint
	LockIdentity              stageprovider.DeploymentOperationCatalogLockIdentity
}

// codeEdgePhase1ProductionComposition is the independently attested parent
// capability bundle. Root composition combines it with Standard authoring and
// the evaluator child through the template router; it cannot operate as a
// substitute for either bundle.
type codeEdgePhase1ProductionComposition struct {
	Resolver                *stageprovider.CatalogLockAttestedWorkflowkitProviderOperationResolver
	CatalogBinding          app.TemplateDeploymentCatalogResolver
	Admission               codeedge.TaskAdmissionContract
	AuthoringDockerCommands []stageprovider.LocalExecutableLock
}

func newCodeEdgePhase1ProductionComposition(config codeEdgePhase1ProductionCompositionConfig) (*codeEdgePhase1ProductionComposition, error) {
	if strings.TrimSpace(config.ManagedRoot) == "" {
		return nil, fmt.Errorf("managed root is required for CodeEdge Phase-1 production composition")
	}
	binding := codeEdgePhase1ProductionBuildBinding{
		HarborFlowBuild:           config.HarborFlowBuild,
		CatalogReceiptFingerprint: config.CatalogReceiptFingerprint,
		LockIdentity:              config.LockIdentity,
	}
	if err := binding.Validate(); err != nil {
		return nil, fmt.Errorf("CodeEdge Phase-1 production build binding is unavailable or invalid: %w", err)
	}
	catalogPath, err := requireCodeEdgeProductionFile("CodeEdge Phase-1 catalog", config.CatalogPath)
	if err != nil {
		return nil, err
	}
	lockPath, err := requireCodeEdgeProductionFile("CodeEdge Phase-1 lock", config.LockPath)
	if err != nil {
		return nil, err
	}
	catalogRaw, err := readCodeEdgeProductionFile(catalogPath)
	if err != nil {
		return nil, fmt.Errorf("read CodeEdge Phase-1 catalog: %w", err)
	}
	catalogDocument, err := stageprovider.ParseDeploymentOperationCatalogJSON(catalogRaw)
	if err != nil {
		return nil, fmt.Errorf("load CodeEdge Phase-1 catalog: %w", err)
	}
	catalog, err := stageprovider.NewDeploymentOperationCatalogResolver(catalogDocument)
	if err != nil {
		return nil, fmt.Errorf("load CodeEdge Phase-1 catalog: %w", err)
	}
	if !catalog.Template().Equal(workflowadapter.CodeEdgePhase1TemplateReference()) {
		return nil, fmt.Errorf("load CodeEdge Phase-1 catalog: expected only template %s@%s", workflowadapter.CodeEdgePhase1WorkflowTemplateID, workflowadapter.CodeEdgePhase1WorkflowTemplateVersion)
	}
	lockRaw, err := readCodeEdgeProductionFile(lockPath)
	if err != nil {
		return nil, fmt.Errorf("read CodeEdge Phase-1 lock: %w", err)
	}
	lock, err := stageprovider.ParseDeploymentOperationCatalogLockJSON(lockRaw)
	if err != nil {
		return nil, fmt.Errorf("load CodeEdge Phase-1 lock: %w", err)
	}
	composition, err := codeEdgePhase1CompositionFromVerifiedAssets(config.ManagedRoot, binding, catalog, lock)
	if err != nil {
		return nil, err
	}
	return composition, nil
}

func codeEdgePhase1CompositionFromVerifiedAssets(managedRoot string, binding codeEdgePhase1ProductionBuildBinding, catalog *stageprovider.DeploymentOperationCatalogResolver, lock stageprovider.DeploymentOperationCatalogLock) (*codeEdgePhase1ProductionComposition, error) {
	if err := binding.Validate(); err != nil {
		return nil, err
	}
	if catalog == nil {
		return nil, fmt.Errorf("CodeEdge Phase-1 catalog is required")
	}
	verifier, err := stageprovider.NewDeploymentOperationCatalogLockResolver(catalog, lock)
	if err != nil {
		return nil, fmt.Errorf("bind CodeEdge Phase-1 catalog and lock: %w", err)
	}
	if binding.HarborFlowBuild != verifier.HarborFlowBuild() {
		return nil, fmt.Errorf("CodeEdge Phase-1 build identity does not match the installed operation lock")
	}
	receiptFingerprint, err := verifier.CatalogReceipt().Fingerprint()
	if err != nil {
		return nil, fmt.Errorf("fingerprint installed CodeEdge Phase-1 catalog receipt: %w", err)
	}
	if binding.CatalogReceiptFingerprint != receiptFingerprint {
		return nil, fmt.Errorf("CodeEdge Phase-1 catalog receipt does not match the installed binary binding")
	}
	if binding.LockIdentity != verifier.LockIdentity() {
		return nil, fmt.Errorf("CodeEdge Phase-1 operation catalog lock does not match the installed binary binding")
	}

	locked := verifier.Lock()
	preflight, err := locked.CodeEdgePhase1Preflight()
	if err != nil {
		return nil, fmt.Errorf("load lock-owned CodeEdge Phase-1 preflight profile: %w", err)
	}
	admission := codeedge.TaskAdmissionContract{ID: verifier.LockIdentity().LockID, Version: verifier.LockIdentity().LockVersion, Profile: preflight}
	if err := admission.Validate(); err != nil {
		return nil, fmt.Errorf("construct lock-owned CodeEdge task admission contract: %w", err)
	}
	commands, err := codeEdgePhase1ParentCommandLocks(locked)
	if err != nil {
		return nil, err
	}
	profile, err := locked.CodeEdgePhase1Profile()
	if err != nil {
		return nil, fmt.Errorf("load lock-owned CodeEdge Phase-1 execution profile: %w", err)
	}
	commandTimeout, err := codeEdgePhase1ParentCommandTimeout(profile)
	if err != nil {
		return nil, err
	}
	parentExecutor, err := app.NewCodeEdgePhase1ParentExecutor(app.CodeEdgePhase1ParentExecutorConfig{
		ManagedRoot: managedRoot, PreflightProfile: preflight, LockedCommands: commands, CommandTimeout: commandTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("construct CodeEdge Phase-1 parent executor: %w", err)
	}
	attestor, err := stageprovider.NewCodeEdgePhase1RuntimeAttestor(stageprovider.CodeEdgePhase1RuntimeAttestorConfig{HarborFlowBuild: binding.HarborFlowBuild})
	if err != nil {
		return nil, fmt.Errorf("construct CodeEdge Phase-1 runtime attestor: %w", err)
	}
	providers, err := stageprovider.NewCodeEdgePhase1ProviderComposition(stageprovider.CodeEdgePhase1ProviderCompositionConfig{
		Template: workflowadapter.CodeEdgePhase1TemplateReference(), Catalog: catalog, Lock: locked, Attestor: attestor,
		Handlers: stageprovider.CodeEdgePhase1OperationHandlers{LocalCommand: parentExecutor, HarborBuiltin: parentExecutor},
	})
	if err != nil {
		return nil, fmt.Errorf("construct CodeEdge Phase-1 provider composition: %w", err)
	}
	return &codeEdgePhase1ProductionComposition{
		Resolver: providers.Resolver,
		CatalogBinding: app.TemplateDeploymentCatalogResolver{
			Template: workflowadapter.CodeEdgePhase1TemplateReference(), Resolver: providers.Resolver,
		},
		Admission:               admission,
		AuthoringDockerCommands: append([]stageprovider.LocalExecutableLock(nil), commands...),
	}, nil
}

func codeEdgePhase1ParentCommandLocks(lock stageprovider.DeploymentOperationCatalogLock) ([]stageprovider.LocalExecutableLock, error) {
	expected := map[string]workflowkit.StageKey{
		stageprovider.CodeEdgePhase1DockerBuildCommandID:   workflowkit.StageKey(workflowadapter.DockerBuild),
		stageprovider.CodeEdgePhase1InitialVerifyCommandID: workflowkit.StageKey(workflowadapter.InitialVerify),
		stageprovider.CodeEdgePhase1OracleVerifyCommandID:  workflowkit.StageKey(workflowadapter.OracleVerify),
	}
	byCommand := make(map[string]stageprovider.LocalExecutableLock, len(expected))
	for _, record := range lock.Operations {
		payload, ok := record.Operation.Payload.(workflowadapter.LocalCommandOperationPayload)
		if !ok {
			continue
		}
		expectedStage, allowed := expected[payload.CommandID]
		if !allowed {
			return nil, fmt.Errorf("CodeEdge Phase-1 lock contains unapproved local command %q", payload.CommandID)
		}
		if record.Stage.Key != expectedStage || len(payload.Arguments) != 0 || record.LocalExecutable == nil {
			return nil, fmt.Errorf("CodeEdge Phase-1 lock has an invalid local command binding for %q", payload.CommandID)
		}
		if _, duplicate := byCommand[payload.CommandID]; duplicate {
			return nil, fmt.Errorf("CodeEdge Phase-1 lock duplicates local command %q", payload.CommandID)
		}
		byCommand[payload.CommandID] = *record.LocalExecutable
	}
	commands := make([]stageprovider.LocalExecutableLock, 0, len(expected))
	for commandID := range expected {
		command, found := byCommand[commandID]
		if !found {
			return nil, fmt.Errorf("CodeEdge Phase-1 lock omits local command %q", commandID)
		}
		commands = append(commands, command)
	}
	sort.Slice(commands, func(left, right int) bool { return commands[left].CommandID < commands[right].CommandID })
	return commands, nil
}

// codeEdgePhase1ParentCommandTimeout derives the executor's internal ceiling
// from the frozen local-command budgets. The outer worker context remains the
// authority for an individual stage, but this prevents an unrelated executor
// default from shortening an approved Docker verification stage.
func codeEdgePhase1ParentCommandTimeout(profile workflowadapter.ExecutionProfile) (time.Duration, error) {
	if err := profile.Validate(); err != nil {
		return 0, err
	}
	var timeout time.Duration
	for _, key := range []workflowkit.StageKey{
		workflowkit.StageKey(workflowadapter.DockerBuild),
		workflowkit.StageKey(workflowadapter.InitialVerify),
		workflowkit.StageKey(workflowadapter.OracleVerify),
	} {
		budget, found := profile.Budget(key)
		if !found || budget.AttemptTimeout <= 0 {
			return 0, fmt.Errorf("CodeEdge Phase-1 profile omits a usable command budget for %q", key)
		}
		if budget.AttemptTimeout > timeout {
			timeout = budget.AttemptTimeout
		}
	}
	return timeout, nil
}
