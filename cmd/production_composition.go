package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/evaluator"
	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	// This is the evaluator-child bundle, not the Phase-1 parent bundle. The
	// parent now owns deployments/codeedge-phase1; sharing that directory would
	// let a child-only lock be mistaken for parent authority.
	codeEdgeProductionDeploymentDirectory = "codeedge-evaluator-child"
	codeEdgeProductionCatalogFile         = "operation-catalog.v1.json"
	codeEdgeProductionLockFile            = "operation-catalog.lock.json"
)

// These fields are intentionally blank in ordinary developer builds. The
// reproducible production build target injects the build, catalog receipt, and
// complete lock identities with ldflags after it verifies the source manifest
// against the source-controlled operation lock. An unlabelled binary cannot
// operate the real evaluator.
var (
	codeEdgeProductionBuildModule                    string
	codeEdgeProductionBuildVersion                   string
	codeEdgeProductionBuildCommit                    string
	codeEdgeProductionBuildContentSHA256             string
	codeEdgeProductionBuildCatalogReceiptFingerprint string
	codeEdgeProductionBuildLockID                    string
	codeEdgeProductionBuildLockVersion               string
	codeEdgeProductionBuildLockFingerprint           string
)

// codeEdgeProductionBuildBinding is the linker-injected identity of the
// deployment materials a production binary is permitted to load. The catalog
// receipt and complete lock identity close the gap where a self-consistent but
// different catalog/lock pair could otherwise claim the same source build.
type codeEdgeProductionBuildBinding struct {
	HarborFlowBuild           stageprovider.HarborFlowBuildIdentity
	CatalogReceiptFingerprint workflowkit.Fingerprint
	LockIdentity              stageprovider.DeploymentOperationCatalogLockIdentity
}

func (binding codeEdgeProductionBuildBinding) Validate() error {
	if err := binding.HarborFlowBuild.Validate(); err != nil {
		return fmt.Errorf("Harbor Flow build identity: %w", err)
	}
	if err := binding.CatalogReceiptFingerprint.Validate(); err != nil {
		return fmt.Errorf("catalog receipt fingerprint: %w", err)
	}
	if err := binding.LockIdentity.Validate(); err != nil {
		return fmt.Errorf("operation catalog lock identity: %w", err)
	}
	return nil
}

// codeEdgeProductionCompositionConfig describes only deployment-owned wiring.
// Catalog and lock locations are supplied by the local package layout or by
// focused tests; a Run, profile, TUI action, or worker job can never replace
// them. LookupEnvironment returns values only to the attestor/executor and is
// never serialized by this composition.
type codeEdgeProductionCompositionConfig struct {
	CatalogPath               string
	LockPath                  string
	WorkspaceRoot             string
	HarborFlowBuild           stageprovider.HarborFlowBuildIdentity
	CatalogReceiptFingerprint workflowkit.Fingerprint
	LockIdentity              stageprovider.DeploymentOperationCatalogLockIdentity
	LookupEnvironment         func(string) (string, bool)
}

// codeEdgeEvaluatorProductionComposition is the one closed capability bundle
// for the evaluator child.  It intentionally does not construct lifecycle
// services itself: root production composition combines this independent
// child bundle with the distinct Standard-authoring and CodeEdge parent
// bundles through the template router.
type codeEdgeEvaluatorProductionComposition struct {
	Resolver       *stageprovider.CatalogLockAttestedWorkflowkitProviderOperationResolver
	CatalogBinding app.TemplateDeploymentCatalogResolver
	Definitions    app.EvaluatorRunDefinitionProvider
	Observer       app.CodeEdgeEvaluatorCompletedObserver
}

// newCodeEdgeProductionLifecycleServices is the sole real-provider service
// composition used by the CLI, Task Hub, foreground worker, and detached
// worker. It is deliberately narrow: this installation approves only the
// closed CodeEdge evaluator child template, not arbitrary local commands or a
// generic model/PATH fallback.
func newCodeEdgeProductionLifecycleServices(root string, dataStore *store.Store) (*app.LifecycleServices, error) {
	binding, err := linkedCodeEdgeProductionBuildBinding()
	if err != nil {
		return nil, err
	}
	catalogPath, lockPath, err := defaultCodeEdgeProductionDeploymentPaths()
	if err != nil {
		return nil, err
	}
	return newCodeEdgeProductionLifecycleServicesWithConfig(root, dataStore, codeEdgeProductionCompositionConfig{
		CatalogPath:               catalogPath,
		LockPath:                  lockPath,
		HarborFlowBuild:           binding.HarborFlowBuild,
		CatalogReceiptFingerprint: binding.CatalogReceiptFingerprint,
		LockIdentity:              binding.LockIdentity,
	})
}

// preflightCodeEdgeProductionLifecycleServices performs every static
// deployment check that is available before the mutable control-plane Store is
// opened. The full composition repeats these checks while binding the Store,
// so this is a fail-closed ordering guard rather than a second configuration
// source. It intentionally does not inspect environment values or execute a
// provider.
func preflightCodeEdgeProductionLifecycleServices(root string) error {
	if strings.TrimSpace(root) == "" {
		return fmt.Errorf("managed root is required for CodeEdge production composition")
	}
	if _, err := linkedCodeEdgeProductionBuildBinding(); err != nil {
		return err
	}
	if _, _, err := defaultCodeEdgeProductionDeploymentPaths(); err != nil {
		return err
	}
	return nil
}

func newCodeEdgeProductionLifecycleServicesWithConfig(root string, dataStore *store.Store, config codeEdgeProductionCompositionConfig) (*app.LifecycleServices, error) {
	composition, err := newCodeEdgeEvaluatorProductionCompositionWithConfig(root, dataStore, config)
	if err != nil {
		return nil, err
	}
	return app.NewLifecycleServicesWithOptions(root, dataStore, app.LifecycleServicesOptions{
		OperationResolver:              composition.Resolver,
		DeploymentCatalogResolver:      composition.Resolver,
		RequireDeploymentCatalog:       true,
		RequireDeploymentLock:          true,
		EvaluatorRunDefinitionProvider: composition.Definitions,
		CodeEdgeEvaluatorObserver:      composition.Observer,
	})
}

// newCodeEdgeEvaluatorProductionCompositionWithConfig constructs only the
// independently attested evaluator-child capability.  It has no parent
// fallback and never installs a lifecycle service with this catalog alone.
func newCodeEdgeEvaluatorProductionCompositionWithConfig(root string, dataStore *store.Store, config codeEdgeProductionCompositionConfig) (*codeEdgeEvaluatorProductionComposition, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("managed root is required for CodeEdge evaluator production composition")
	}
	if dataStore == nil {
		return nil, fmt.Errorf("lifecycle store is required for CodeEdge evaluator production composition")
	}
	binding := codeEdgeProductionBuildBinding{
		HarborFlowBuild:           config.HarborFlowBuild,
		CatalogReceiptFingerprint: config.CatalogReceiptFingerprint,
		LockIdentity:              config.LockIdentity,
	}
	if err := binding.Validate(); err != nil {
		return nil, fmt.Errorf("CodeEdge production build binding is unavailable or invalid: %w", err)
	}
	catalogPath, err := requireCodeEdgeProductionFile("catalog", config.CatalogPath)
	if err != nil {
		return nil, err
	}
	lockPath, err := requireCodeEdgeProductionFile("lock", config.LockPath)
	if err != nil {
		return nil, err
	}

	// The static allow-list is loaded and frozen before any environment lookup,
	// filesystem attestation, provider construction, or durable mutation.
	catalogRaw, err := readCodeEdgeProductionFile(catalogPath)
	if err != nil {
		return nil, fmt.Errorf("read CodeEdge production catalog: %w", err)
	}
	catalogDocument, err := stageprovider.ParseDeploymentOperationCatalogJSON(catalogRaw)
	if err != nil {
		return nil, fmt.Errorf("load CodeEdge production catalog: %w", err)
	}
	catalog, err := stageprovider.NewDeploymentOperationCatalogResolver(catalogDocument)
	if err != nil {
		return nil, fmt.Errorf("load CodeEdge production catalog: %w", err)
	}
	if !catalog.Template().Equal(workflowadapter.CodeEdgeEvaluatorChildTemplateReference()) {
		return nil, fmt.Errorf("load CodeEdge production catalog: expected only template %s@%s", workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateID, workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateVersion)
	}
	lockRaw, err := readCodeEdgeProductionFile(lockPath)
	if err != nil {
		return nil, fmt.Errorf("read CodeEdge production lock: %w", err)
	}
	lock, err := stageprovider.ParseDeploymentOperationCatalogLockJSON(lockRaw)
	if err != nil {
		return nil, fmt.Errorf("load CodeEdge production lock: %w", err)
	}
	verifier, err := stageprovider.NewDeploymentOperationCatalogLockResolver(catalog, lock)
	if err != nil {
		return nil, fmt.Errorf("bind CodeEdge production catalog and lock: %w", err)
	}
	if binding.HarborFlowBuild != verifier.HarborFlowBuild() {
		return nil, fmt.Errorf("CodeEdge production build identity does not match the installed operation lock")
	}
	catalogReceiptFingerprint, err := verifier.CatalogReceipt().Fingerprint()
	if err != nil {
		return nil, fmt.Errorf("fingerprint installed CodeEdge production catalog receipt: %w", err)
	}
	if binding.CatalogReceiptFingerprint != catalogReceiptFingerprint {
		return nil, fmt.Errorf("CodeEdge production catalog receipt does not match the installed binary binding")
	}
	if binding.LockIdentity != verifier.LockIdentity() {
		return nil, fmt.Errorf("CodeEdge production operation catalog lock does not match the installed binary binding")
	}

	// The attestor is installed before a provider exists. It owns the dynamic
	// proof of the pinned launcher's bytes/version, Python source tree, Docker,
	// and endpoint fingerprints immediately before each external side effect.
	attestor, err := stageprovider.NewHarborEvaluatorRuntimeAttestor(stageprovider.HarborEvaluatorRuntimeAttestorConfig{
		HarborFlowBuild: binding.HarborFlowBuild, EnvironmentLookup: config.LookupEnvironment,
	})
	if err != nil {
		return nil, fmt.Errorf("construct CodeEdge Harbor runtime attestor: %w", err)
	}
	invocations, registrations, providerReference, err := codeEdgeEvaluatorProviderDefinition(verifier.Lock())
	if err != nil {
		return nil, err
	}
	evaluatorDefinitions, err := newCodeEdgeEvaluatorRunDefinitionProvider(verifier.Lock())
	if err != nil {
		return nil, err
	}
	workspaceRoot := strings.TrimSpace(config.WorkspaceRoot)
	if workspaceRoot == "" {
		workspaceRoot = filepath.Join(root, "external-evaluators")
	}
	executor, err := evaluator.NewHarborEvaluatorLocalCommandExecutor(evaluator.HarborEvaluatorLocalCommandExecutorConfig{
		WorkspaceRoot: workspaceRoot, Invocations: invocations, LookupEnv: config.LookupEnvironment,
	})
	if err != nil {
		return nil, fmt.Errorf("construct CodeEdge Harbor evaluator executor: %w", err)
	}
	observer, err := newCodeEdgeEvaluatorCompletedObserver(verifier, attestor, executor)
	if err != nil {
		return nil, err
	}
	typedProvider, err := stageprovider.NewTypedWorkflowkitStageOperationProvider(stageprovider.TypedWorkflowkitStageOperationProviderConfig{
		Handlers:   stageprovider.TypedWorkflowkitOperationHandlers{LocalCommand: executor},
		Operations: registrations,
	})
	if err != nil {
		return nil, fmt.Errorf("construct CodeEdge typed evaluator provider: %w", err)
	}
	providers, err := stageprovider.NewControlledWorkflowkitProviderRegistry([]stageprovider.WorkflowkitProviderRegistration{{
		Provider: providerReference, Adapter: typedProvider,
	}})
	if err != nil {
		return nil, fmt.Errorf("register CodeEdge typed evaluator provider: %w", err)
	}
	catalogBound, err := stageprovider.NewCatalogBoundWorkflowkitProviderOperationResolver(catalog, providers)
	if err != nil {
		return nil, fmt.Errorf("bind CodeEdge provider to deployment catalog: %w", err)
	}
	resolver, err := stageprovider.NewCatalogLockAttestedWorkflowkitProviderOperationResolver(verifier, catalogBound, attestor)
	if err != nil {
		return nil, fmt.Errorf("bind CodeEdge provider to deployment lock and attestor: %w", err)
	}
	return &codeEdgeEvaluatorProductionComposition{
		Resolver: resolver,
		CatalogBinding: app.TemplateDeploymentCatalogResolver{
			Template: workflowadapter.CodeEdgeEvaluatorChildTemplateReference(), Resolver: resolver,
		},
		Definitions: evaluatorDefinitions,
		Observer:    observer,
	}, nil
}

func linkedCodeEdgeProductionBuildBinding() (codeEdgeProductionBuildBinding, error) {
	binding := codeEdgeProductionBuildBinding{
		HarborFlowBuild: stageprovider.HarborFlowBuildIdentity{
			Module: codeEdgeProductionBuildModule, Version: codeEdgeProductionBuildVersion,
			Commit: codeEdgeProductionBuildCommit, ContentSHA256: workflowkitFingerprint(codeEdgeProductionBuildContentSHA256),
		},
		CatalogReceiptFingerprint: workflowkitFingerprint(codeEdgeProductionBuildCatalogReceiptFingerprint),
		LockIdentity: stageprovider.DeploymentOperationCatalogLockIdentity{
			LockID: codeEdgeProductionBuildLockID, LockVersion: codeEdgeProductionBuildLockVersion,
			Fingerprint: workflowkitFingerprint(codeEdgeProductionBuildLockFingerprint),
		},
	}
	if err := binding.Validate(); err != nil {
		return codeEdgeProductionBuildBinding{}, fmt.Errorf("CodeEdge production build binding is unavailable or invalid; build with scripts/build-codeedge-production.sh: %w", err)
	}
	return binding, nil
}

// workflowkitFingerprint avoids making this command-layer composition depend
// on a general-purpose build metadata type while preserving the typed lock
// identity at the package boundary.
func workflowkitFingerprint(value string) workflowkit.Fingerprint {
	return workflowkit.Fingerprint(value)
}

func defaultCodeEdgeProductionDeploymentPaths() (string, string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", "", fmt.Errorf("locate CodeEdge production executable: %w", err)
	}
	return codeEdgeProductionDeploymentPathsBesideExecutable(executable)
}

func codeEdgeProductionDeploymentPathsBesideExecutable(executable string) (string, string, error) {
	executable = strings.TrimSpace(executable)
	if executable == "" {
		return "", "", fmt.Errorf("CodeEdge production executable path is required")
	}
	absoluteExecutable, err := filepath.Abs(executable)
	if err != nil {
		return "", "", fmt.Errorf("resolve CodeEdge production executable path: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(absoluteExecutable)
	if err != nil {
		return "", "", fmt.Errorf("resolve CodeEdge production executable: %w", err)
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return "", "", fmt.Errorf("inspect CodeEdge production executable: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", "", fmt.Errorf("CodeEdge production executable must be a regular non-symlink file")
	}
	directory := filepath.Dir(resolved)
	deploymentDirectory, err := requireCodeEdgeProductionManagedDirectory("deployment directory", filepath.Join(directory, "deployments"), directory)
	if err != nil {
		return "", "", fmt.Errorf("locate CodeEdge production deployment directory beside executable: %w", err)
	}
	phaseDirectory, err := requireCodeEdgeProductionManagedDirectory("deployment phase directory", filepath.Join(deploymentDirectory, codeEdgeProductionDeploymentDirectory), directory)
	if err != nil {
		return "", "", fmt.Errorf("locate CodeEdge production deployment phase directory beside executable: %w", err)
	}
	catalog, err := requireCodeEdgeProductionFileWithin("catalog", filepath.Join(phaseDirectory, codeEdgeProductionCatalogFile), directory)
	if err != nil {
		return "", "", fmt.Errorf("locate CodeEdge production catalog beside executable: %w", err)
	}
	lock, err := requireCodeEdgeProductionFileWithin("lock", filepath.Join(phaseDirectory, codeEdgeProductionLockFile), directory)
	if err != nil {
		return "", "", fmt.Errorf("locate CodeEdge production lock beside executable: %w", err)
	}
	return catalog, lock, nil
}

// requireCodeEdgeProductionManagedDirectory accepts one deployment directory
// only when it is a real directory below the resolved executable directory.
// Both deployment path components are checked separately so a symlink cannot
// redirect an otherwise regular catalog or lock outside the local package.
func requireCodeEdgeProductionManagedDirectory(label, path, executableDirectory string) (string, error) {
	path = strings.TrimSpace(path)
	executableDirectory = strings.TrimSpace(executableDirectory)
	if path == "" || executableDirectory == "" {
		return "", fmt.Errorf("CodeEdge production %s path is required", label)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve CodeEdge production %s path: %w", label, err)
	}
	if !codeEdgeProductionPathWithin(executableDirectory, absolute) {
		return "", fmt.Errorf("CodeEdge production %s escapes the resolved executable directory", label)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", fmt.Errorf("inspect CodeEdge production %s: %w", label, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("CodeEdge production %s must be a non-symlink directory", label)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve CodeEdge production %s: %w", label, err)
	}
	if filepath.Clean(resolved) != filepath.Clean(absolute) || !codeEdgeProductionPathWithin(executableDirectory, resolved) {
		return "", fmt.Errorf("CodeEdge production %s escapes the resolved executable directory", label)
	}
	return filepath.Clean(absolute), nil
}

// requireCodeEdgeProductionFileWithin retains the regular-file/no-final-
// symlink rule and additionally proves that resolving every path component
// still names a file inside the resolved executable directory.
func requireCodeEdgeProductionFileWithin(label, path, executableDirectory string) (string, error) {
	file, err := requireCodeEdgeProductionFile(label, path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(file)
	if err != nil {
		return "", fmt.Errorf("resolve CodeEdge production %s: %w", label, err)
	}
	if filepath.Clean(resolved) != filepath.Clean(file) || !codeEdgeProductionPathWithin(executableDirectory, resolved) {
		return "", fmt.Errorf("CodeEdge production %s escapes the resolved executable directory", label)
	}
	return file, nil
}

func codeEdgeProductionPathWithin(root, path string) bool {
	root, err := filepath.Abs(strings.TrimSpace(root))
	if err != nil {
		return false
	}
	path, err = filepath.Abs(strings.TrimSpace(path))
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return false
	}
	return true
}

func requireCodeEdgeProductionFile(label, path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("CodeEdge production %s path is required", label)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve CodeEdge production %s path: %w", label, err)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", fmt.Errorf("inspect CodeEdge production %s: %w", label, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("CodeEdge production %s must be a regular non-symlink file", label)
	}
	return filepath.Clean(absolute), nil
}

func readCodeEdgeProductionFile(path string) ([]byte, error) {
	path, err := requireCodeEdgeProductionFile("file", path)
	if err != nil {
		return nil, err
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, fmt.Errorf("CodeEdge production file changed while opening")
	}
	contents, err := io.ReadAll(io.LimitReader(file, 1<<20+1))
	if err != nil {
		return nil, err
	}
	if len(contents) > 1<<20 {
		return nil, fmt.Errorf("CodeEdge production file exceeds size limit")
	}
	after, err := os.Lstat(path)
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(opened, after) {
		return nil, fmt.Errorf("CodeEdge production file changed while reading")
	}
	return contents, nil
}

func codeEdgeEvaluatorProviderDefinition(lock stageprovider.DeploymentOperationCatalogLock) ([]stageprovider.HarborEvaluatorInvocation, []stageprovider.TypedWorkflowkitStageOperationRegistration, workflowadapter.ProviderReference, error) {
	if len(lock.Operations) != 2 {
		return nil, nil, workflowadapter.ProviderReference{}, fmt.Errorf("CodeEdge production lock must contain exactly Qwen and Opus evaluator operations")
	}
	records := append([]stageprovider.DeploymentOperationCatalogLockRecord(nil), lock.Operations...)
	sort.Slice(records, func(left, right int) bool {
		return records[left].Stage.Key < records[right].Stage.Key
	})
	invocations := make([]stageprovider.HarborEvaluatorInvocation, 0, 2)
	registrations := make([]stageprovider.TypedWorkflowkitStageOperationRegistration, 0, 2)
	var provider workflowadapter.ProviderReference
	seenCommands := make(map[string]struct{}, 2)
	for _, record := range records {
		if err := record.Validate(); err != nil {
			return nil, nil, workflowadapter.ProviderReference{}, fmt.Errorf("CodeEdge production evaluator operation is invalid: %w", err)
		}
		payload, ok := record.Operation.Payload.(workflowadapter.LocalCommandOperationPayload)
		if !ok || record.HarborEvaluator == nil || (payload.CommandID != stageprovider.HarborEvaluatorQwenCommandID && payload.CommandID != stageprovider.HarborEvaluatorOpusCommandID) {
			return nil, nil, workflowadapter.ProviderReference{}, fmt.Errorf("CodeEdge production lock contains a non-evaluator operation")
		}
		if _, duplicate := seenCommands[payload.CommandID]; duplicate {
			return nil, nil, workflowadapter.ProviderReference{}, fmt.Errorf("CodeEdge production lock duplicates evaluator command %q", payload.CommandID)
		}
		seenCommands[payload.CommandID] = struct{}{}
		if provider.ID == "" {
			provider = record.Provider
		} else if provider != record.Provider {
			return nil, nil, workflowadapter.ProviderReference{}, fmt.Errorf("CodeEdge production lock has more than one evaluator provider")
		}
		evaluatorLock := record.HarborEvaluator.Clone()
		contract := evaluatorLock.Contract
		invocations = append(invocations, stageprovider.HarborEvaluatorInvocation{
			CommandID: payload.CommandID, LauncherPath: evaluatorLock.Launcher.AbsolutePath, LauncherContentSHA256: evaluatorLock.Launcher.ContentSHA256,
			PythonInterpreterPath: evaluatorLock.PythonInterpreter.AbsolutePath, PythonSourceTreePath: evaluatorLock.PythonSourceTree.AbsolutePath,
			DockerCLIPath: evaluatorLock.DockerCLI.AbsolutePath, DockerVersion: evaluatorLock.DockerCLI.Version,
			HarborVersion: contract.HarborVersion, ResultABIFormat: contract.ResultABIFormat, ResultABIVersion: contract.ResultABIVersion,
			TaskArtifactPort: contract.TaskArtifactPort, TaskArtifactSchema: contract.TaskArtifactSchema,
			AgentID: contract.AgentID, AgentVersion: contract.AgentVersion, ModelID: contract.ModelID, ModelVersion: contract.ModelVersion,
			EndpointEnvName: contract.EndpointEnvName, EndpointChildEnvKey: contract.EndpointChildEnvKey, EndpointFingerprint: contract.EndpointFingerprint,
			SecretEnvTemplates: append([]stageprovider.HarborEvaluatorSecretEnvTemplate(nil), contract.SecretEnvTemplates...),
			Attempts:           contract.Attempts, ConcurrentTrials: contract.ConcurrentTrials, MaxRetries: contract.MaxRetries, RequireTrajectory: contract.RequireTrajectory, ScreenshotRenderer: contract.ScreenshotRenderer,
		})
		registrations = append(registrations, stageprovider.TypedWorkflowkitStageOperationRegistration{StageKey: record.Stage.Key, Operation: record.Operation.Clone()})
	}
	if provider.Kind != "evaluation" || len(seenCommands) != 2 {
		return nil, nil, workflowadapter.ProviderReference{}, fmt.Errorf("CodeEdge production lock does not define the complete evaluator provider")
	}
	for _, commandID := range []string{stageprovider.HarborEvaluatorQwenCommandID, stageprovider.HarborEvaluatorOpusCommandID} {
		if _, found := seenCommands[commandID]; !found {
			return nil, nil, workflowadapter.ProviderReference{}, fmt.Errorf("CodeEdge production lock is missing evaluator command %q", commandID)
		}
	}
	return invocations, registrations, provider, nil
}

// codeEdgeEvaluatorRunDefinitionProvider is the controlled application
// boundary for the lock-owned evaluator child definition. Its profile and
// operation records are copied from one already-validated immutable lock at
// composition time. In particular, it never projects an evaluator budget from
// the parent CodeEdge Run: parent and child have independent frozen envelopes.
type codeEdgeEvaluatorRunDefinitionProvider struct {
	profile workflowadapter.ExecutionProfile
	records map[workflowkit.StageKey]stageprovider.DeploymentOperationCatalogLockRecord
}

func newCodeEdgeEvaluatorRunDefinitionProvider(lock stageprovider.DeploymentOperationCatalogLock) (*codeEdgeEvaluatorRunDefinitionProvider, error) {
	if err := lock.Validate(); err != nil {
		return nil, fmt.Errorf("CodeEdge evaluator definition lock is invalid: %w", err)
	}
	if !lock.CatalogReceipt.Template.Equal(workflowadapter.CodeEdgeEvaluatorChildTemplateReference()) {
		return nil, fmt.Errorf("CodeEdge evaluator definition lock does not bind the evaluator child template")
	}
	if _, _, _, err := codeEdgeEvaluatorProviderDefinition(lock); err != nil {
		return nil, err
	}
	profile, err := lock.CodeEdgeEvaluatorChildProfile()
	if err != nil {
		return nil, fmt.Errorf("CodeEdge evaluator definition lock has no complete child-owned execution profile: %w", err)
	}
	records, err := codeEdgeEvaluatorDefinitionRecords(lock)
	if err != nil {
		return nil, err
	}
	return &codeEdgeEvaluatorRunDefinitionProvider{profile: profile, records: records}, nil
}

func (provider *codeEdgeEvaluatorRunDefinitionProvider) DefinitionForEvaluatorRun(ctx context.Context, request app.EvaluatorRunDefinitionRequest) (app.EvaluatorRunDefinition, error) {
	if provider == nil {
		return app.EvaluatorRunDefinition{}, app.ErrCodeEdgeEvaluatorDefinitionUnavailable
	}
	if ctx == nil {
		return app.EvaluatorRunDefinition{}, fmt.Errorf("CodeEdge evaluator definition context is required")
	}
	if err := ctx.Err(); err != nil {
		return app.EvaluatorRunDefinition{}, err
	}
	specification, err := provider.executionSpec(request)
	if err != nil {
		return app.EvaluatorRunDefinition{}, fmt.Errorf("construct lock-owned CodeEdge evaluator execution specification: %w", err)
	}
	return app.EvaluatorRunDefinition{Profile: provider.profile.Clone(), ExecutionSpec: specification}, nil
}

var _ app.EvaluatorRunDefinitionProvider = (*codeEdgeEvaluatorRunDefinitionProvider)(nil)

func codeEdgeEvaluatorDefinitionRecords(lock stageprovider.DeploymentOperationCatalogLock) (map[workflowkit.StageKey]stageprovider.DeploymentOperationCatalogLockRecord, error) {
	if len(lock.Operations) != len(workflowadapter.CodeEdgeEvaluatorChildStageOrder()) {
		return nil, fmt.Errorf("CodeEdge evaluator definition lock must contain exactly the two child evaluator operations")
	}
	records := make(map[workflowkit.StageKey]stageprovider.DeploymentOperationCatalogLockRecord, len(lock.Operations))
	for _, record := range lock.Operations {
		if err := record.Validate(); err != nil {
			return nil, fmt.Errorf("CodeEdge evaluator definition record: %w", err)
		}
		if _, duplicate := records[record.Stage.Key]; duplicate {
			return nil, fmt.Errorf("CodeEdge evaluator definition lock duplicates stage %q", record.Stage.Key)
		}
		records[record.Stage.Key] = record.Clone()
	}
	for _, stageKey := range workflowadapter.CodeEdgeEvaluatorChildStageOrder() {
		record, found := records[stageKey]
		if !found {
			return nil, fmt.Errorf("CodeEdge evaluator definition lock omits stage %q", stageKey)
		}
		payload, ok := record.Operation.Payload.(workflowadapter.LocalCommandOperationPayload)
		if !ok {
			return nil, fmt.Errorf("CodeEdge evaluator definition stage %q does not use the approved local command", stageKey)
		}
		switch stageKey {
		case workflowkit.StageKey(workflowadapter.HarborRunQwen):
			if record.Stage.Type != workflowadapter.StageBindingHarborRunQwen || payload.CommandID != stageprovider.HarborEvaluatorQwenCommandID {
				return nil, fmt.Errorf("CodeEdge evaluator definition Qwen record is not the approved Qwen stage")
			}
		case workflowkit.StageKey(workflowadapter.HarborRunOpus):
			if record.Stage.Type != workflowadapter.StageBindingHarborRunOpus || payload.CommandID != stageprovider.HarborEvaluatorOpusCommandID {
				return nil, fmt.Errorf("CodeEdge evaluator definition Opus record is not the approved Opus stage")
			}
		default:
			return nil, fmt.Errorf("CodeEdge evaluator definition has unsupported child stage %q", stageKey)
		}
	}
	return records, nil
}

func (provider *codeEdgeEvaluatorRunDefinitionProvider) executionSpec(request app.EvaluatorRunDefinitionRequest) (workflowadapter.RunExecutionSpec, error) {
	if provider == nil {
		return workflowadapter.RunExecutionSpec{}, app.ErrCodeEdgeEvaluatorDefinitionUnavailable
	}
	if err := provider.profile.Validate(); err != nil {
		return workflowadapter.RunExecutionSpec{}, fmt.Errorf("stored child execution profile: %w", err)
	}
	placeholder := workflowadapter.ArtifactReference{
		// The launch service always replaces this lock-owned provisional binding
		// with a managed task snapshot before freezing the child Run. ParentRunID
		// is used only as a valid UUIDv7 placeholder; it is not a workspace path
		// or a caller-selected artifact.
		ID:            workflowkit.ArtifactID(request.ParentRunID),
		ContentDigest: workflowkit.SHA256Fingerprint([]byte("codeedge-evaluator-lock-owned-provisional-snapshot:" + string(request.RevisionDigest) + ":" + request.ParentRunID)),
		SchemaVersion: workflowadapter.CodeEdgeEvaluatorTaskSnapshotSchemaVersion,
	}
	specification := workflowadapter.RunExecutionSpec{
		Format: workflowadapter.RunExecutionSpecFormat, Version: workflowadapter.RunExecutionSpecVersion,
		Template: workflowadapter.CodeEdgeEvaluatorChildTemplateReference(),
		Selection: workflowadapter.RunSelectionReference{
			TaskID: request.TaskID, RevisionID: request.RevisionID, RevisionDigest: request.RevisionDigest,
		},
		References: workflowadapter.ExecutionReferenceSet{
			Artifacts: []workflowadapter.ArtifactReference{placeholder}, Checkouts: []workflowadapter.CheckoutReference{},
			Runtimes: []workflowadapter.RuntimeReference{}, Providers: []workflowadapter.ProviderReference{}, Secrets: []workflowadapter.SecretReference{},
		},
		Stages: make([]workflowadapter.StageExecutionBinding, 0, len(workflowadapter.CodeEdgeEvaluatorChildStageOrder())),
	}
	checkouts := make(map[string]workflowadapter.CheckoutReference)
	runtimes := make(map[string]workflowadapter.RuntimeReference)
	providers := make(map[string]workflowadapter.ProviderReference)
	secrets := make(map[string]workflowadapter.SecretReference)
	for _, stageKey := range workflowadapter.CodeEdgeEvaluatorChildStageOrder() {
		record, found := provider.records[stageKey]
		if !found {
			return workflowadapter.RunExecutionSpec{}, fmt.Errorf("stored child definition omits stage %q", stageKey)
		}
		checkout := workflowadapter.CheckoutReference{ID: record.Checkout.ID, RevisionID: request.RevisionID, RevisionDigest: request.RevisionDigest}
		if existing, present := checkouts[checkout.ID]; present && existing != checkout {
			return workflowadapter.RunExecutionSpec{}, fmt.Errorf("stored child definition has conflicting checkout %q", checkout.ID)
		}
		checkouts[checkout.ID] = checkout
		if existing, present := runtimes[record.Runtime.ID]; present && existing != record.Runtime {
			return workflowadapter.RunExecutionSpec{}, fmt.Errorf("stored child definition has conflicting runtime %q", record.Runtime.ID)
		}
		runtimes[record.Runtime.ID] = record.Runtime
		if existing, present := providers[record.Provider.ID]; present && existing != record.Provider {
			return workflowadapter.RunExecutionSpec{}, fmt.Errorf("stored child definition has conflicting provider %q", record.Provider.ID)
		}
		providers[record.Provider.ID] = record.Provider
		secretIDs := make([]string, 0, len(record.Secrets))
		for _, secret := range record.Secrets {
			if existing, present := secrets[secret.ID]; present && existing != secret {
				return workflowadapter.RunExecutionSpec{}, fmt.Errorf("stored child definition has conflicting secret %q", secret.ID)
			}
			secrets[secret.ID] = secret
			secretIDs = append(secretIDs, secret.ID)
		}
		sort.Strings(secretIDs)
		base := workflowadapter.StageBindingBase{
			Type: record.Stage.Type, StageKey: record.Stage.Key, Plugin: record.Stage.Plugin,
			ArtifactInputs: []workflowadapter.ArtifactInputReference{{Port: workflowadapter.CodeEdgeEvaluatorTaskSnapshotArtifact, ArtifactID: placeholder.ID}},
			CheckoutID:     checkout.ID, RuntimeID: record.Runtime.ID, Operation: record.Operation.Clone(), SecretIDs: secretIDs,
		}
		switch stageKey {
		case workflowkit.StageKey(workflowadapter.HarborRunQwen):
			specification.Stages = append(specification.Stages, workflowadapter.HarborRunQwenBinding{StageBindingBase: base})
		case workflowkit.StageKey(workflowadapter.HarborRunOpus):
			specification.Stages = append(specification.Stages, workflowadapter.HarborRunOpusBinding{StageBindingBase: base})
		default:
			return workflowadapter.RunExecutionSpec{}, fmt.Errorf("stored child definition has unsupported stage %q", stageKey)
		}
	}
	for _, checkout := range checkouts {
		specification.References.Checkouts = append(specification.References.Checkouts, checkout)
	}
	for _, runtime := range runtimes {
		specification.References.Runtimes = append(specification.References.Runtimes, runtime)
	}
	for _, provider := range providers {
		specification.References.Providers = append(specification.References.Providers, provider)
	}
	for _, secret := range secrets {
		specification.References.Secrets = append(specification.References.Secrets, secret)
	}
	sort.Slice(specification.References.Checkouts, func(left, right int) bool {
		return specification.References.Checkouts[left].ID < specification.References.Checkouts[right].ID
	})
	sort.Slice(specification.References.Runtimes, func(left, right int) bool {
		return specification.References.Runtimes[left].ID < specification.References.Runtimes[right].ID
	})
	sort.Slice(specification.References.Providers, func(left, right int) bool {
		return specification.References.Providers[left].ID < specification.References.Providers[right].ID
	})
	sort.Slice(specification.References.Secrets, func(left, right int) bool {
		return specification.References.Secrets[left].ID < specification.References.Secrets[right].ID
	})
	if err := specification.Validate(); err != nil {
		return workflowadapter.RunExecutionSpec{}, err
	}
	return specification, nil
}
