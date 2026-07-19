// Command codeedge-evaluator-lock-build generates the immutable CodeEdge
// evaluator-child deployment lock. It accepts only a clean Git snapshot,
// source-controlled contracts, explicit local runtime paths, and the three
// approved evaluator environment names. It never starts a Harbor job or
// writes endpoint or credential values to disk or output.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	modulePath               = "github.com/purplevoid/harbor-factory"
	maxAssetBytes      int64 = 4 * 1024 * 1024
	maxProbeBytes            = 64 * 1024
	maxSourceTreeBytes       = 64 * 1024 * 1024
	maxShebangBytes    int64 = 4 * 1024
	probeTimeout             = 30 * time.Second

	evaluatorDeploymentDirectory        = "deployments/codeedge-evaluator-child"
	evaluatorCatalogRelative            = evaluatorDeploymentDirectory + "/operation-catalog.v1.json"
	evaluatorAssetManifestRelative      = evaluatorDeploymentDirectory + "/contract-assets.v1.json"
	evaluatorProfileRelative            = evaluatorDeploymentDirectory + "/execution-profile.v1.json"
	evaluatorInvocationContractRelative = evaluatorDeploymentDirectory + "/contracts/harbor-pass-at-four.v0.18.json"
	evaluatorResultSchemaRelative       = evaluatorDeploymentDirectory + "/schemas/harbor-run-bundle.v0.18.json"
	evaluatorLockRelative               = evaluatorDeploymentDirectory + "/operation-catalog.lock.json"

	evaluatorContractAssetManifestFormat  = "harbor.codeedge-evaluator-contract-assets.v1"
	evaluatorContractAssetManifestVersion = "1"
	evaluatorInvocationAssetFormat        = "harbor.codeedge-evaluator-invocation-contract.v1"
	evaluatorInvocationAssetVersion       = "1"
	evaluatorResultSchemaAssetFormat      = "harbor.codeedge-evaluator-result-schema.v1"
	evaluatorResultSchemaAssetVersion     = "1"

	qwenEndpointEnvironment   = "QWEN_HARBOR_BASE_URL"
	opusEndpointEnvironment   = "OPUS_HARBOR_BASE_URL"
	credentialEnvironment     = "ANTHROPIC_AUTH_TOKEN"
	dockerServerVersionFormat = `{{.Server.Version}}`
	dockerComposePathFormat   = `{{range .ClientInfo.Plugins}}{{if eq .Name "compose"}}{{.Path}}{{end}}{{end}}`
	dockerBuildxPathFormat    = `{{range .ClientInfo.Plugins}}{{if eq .Name "buildx"}}{{.Path}}{{end}}{{end}}`
)

// generatedProductionLocks are omitted from the source manifest signed by
// every deployment lock. Including any one would make the three independent
// bundles sign different source identities or create a hash cycle.
var generatedProductionLocks = map[string]struct{}{
	"deployments/standard-authoring/operation-catalog.lock.json":       {},
	"deployments/codeedge-phase1/operation-catalog.lock.json":          {},
	"deployments/codeedge-evaluator-child/operation-catalog.lock.json": {},
}

type buildConfig struct {
	sourceRoot          string
	catalogPath         string
	assetManifest       string
	profilePath         string
	contractRoot        string
	outputPath          string
	buildVersion        string
	lockID              string
	lockVersion         string
	gitExecutable       string
	harborLauncher      string
	pythonInterpreter   string
	pythonSourceTree    string
	dockerCLI           string
	dockerComposePlugin string
	dockerBuildxPlugin  string
	lookupEnvironment   func(string) (string, bool)
}

type evaluatorAssetReference struct {
	ID           string `json:"id"`
	Version      string `json:"version"`
	RelativePath string `json:"relative_path"`
}

type evaluatorAssetManifestEntry struct {
	StageKey string                  `json:"stage_key"`
	Prompt   evaluatorAssetReference `json:"prompt"`
	Schema   evaluatorAssetReference `json:"schema"`
}

type evaluatorAssetManifest struct {
	Format     string                            `json:"format"`
	Version    string                            `json:"version"`
	Template   workflowadapter.TemplateReference `json:"template"`
	Operations []evaluatorAssetManifestEntry     `json:"operations"`
}

type evaluatorInvocationAsset struct {
	Format          string                            `json:"format"`
	Version         string                            `json:"version"`
	Template        workflowadapter.TemplateReference `json:"template"`
	HarborCLI       evaluatorInvocationHarborCLI      `json:"harbor_cli"`
	CallerArguments string                            `json:"caller_arguments"`
	Locality        evaluatorInvocationLocality       `json:"locality"`
	TrialPolicy     evaluatorInvocationTrialPolicy    `json:"trial_policy"`
	EvaluatorOrder  []evaluatorInvocationOrderEntry   `json:"evaluator_order"`
}

type evaluatorInvocationHarborCLI struct {
	Version    string `json:"version"`
	Subcommand string `json:"subcommand"`
}

type evaluatorInvocationLocality struct {
	RemoteUpload         string `json:"remote_upload"`
	RemoteReconciliation string `json:"remote_reconciliation"`
	ManagedJobsDirectory string `json:"managed_jobs_directory"`
}

type evaluatorInvocationTrialPolicy struct {
	Attempts                             int  `json:"n_attempts"`
	Concurrent                           int  `json:"n_concurrent"`
	MaxRetries                           int  `json:"max_retries"`
	TechnicalRetriesPreserveLogicalTrial bool `json:"technical_retries_preserve_logical_trial"`
}

type evaluatorInvocationOrderEntry struct {
	StageKey        string `json:"stage_key"`
	CommandID       string `json:"command_id"`
	AgentID         string `json:"agent_id"`
	AgentVersion    string `json:"agent_version"`
	ModelID         string `json:"model_id"`
	EndpointEnvName string `json:"endpoint_env_name"`
}

type evaluatorResultSchemaAsset struct {
	Format           string                             `json:"format"`
	Version          string                             `json:"version"`
	Bundle           evaluatorResultSchemaBundle        `json:"bundle"`
	RequiredEvidence []string                           `json:"required_evidence"`
	TrialContract    evaluatorResultSchemaTrialContract `json:"trial_contract"`
}

type evaluatorResultSchemaBundle struct {
	Format  string `json:"format"`
	Version string `json:"version"`
}

type evaluatorResultSchemaTrialContract struct {
	LogicalTrialCount               int    `json:"logical_trial_count"`
	RequireSingleTerminalScreenshot bool   `json:"require_single_terminal_screenshot"`
	ResultParser                    string `json:"result_parser"`
}

type evaluatorRuntime struct {
	LauncherFingerprint        workflowkit.Fingerprint
	HarborVersion              string
	PythonFingerprint          workflowkit.Fingerprint
	PythonVersion              string
	SourceFingerprint          workflowkit.Fingerprint
	DockerFingerprint          workflowkit.Fingerprint
	DockerVersion              string
	DockerServerVersion        string
	DockerComposeFingerprint   workflowkit.Fingerprint
	DockerComposeVersion       string
	DockerComposeVersionOutput string
	DockerBuildxFingerprint    workflowkit.Fingerprint
	DockerBuildxVersion        string
	DockerBuildxVersionOutput  string
	launcherSnapshot           evaluatorExecutableSnapshot
	pythonSnapshot             evaluatorExecutableSnapshot
	sourceSnapshot             evaluatorSourceTreeSnapshot
	dockerSnapshot             evaluatorExecutableSnapshot
	dockerComposeSnapshot      evaluatorExecutableSnapshot
	dockerBuildxSnapshot       evaluatorExecutableSnapshot
}

func main() {
	var config buildConfig
	flag.StringVar(&config.sourceRoot, "source-root", "", "clean Git worktree root")
	flag.StringVar(&config.catalogPath, "catalog", "", "CodeEdge evaluator child operation catalog")
	flag.StringVar(&config.assetManifest, "asset-manifest", "", "CodeEdge evaluator contract asset manifest")
	flag.StringVar(&config.profilePath, "execution-profile", "", "source-controlled evaluator child execution profile")
	flag.StringVar(&config.contractRoot, "contract-root", "", "directory containing evaluator contract assets")
	flag.StringVar(&config.outputPath, "output", "", "new operation catalog lock output path")
	flag.StringVar(&config.buildVersion, "build-version", "", "immutable Harbor Flow build version")
	flag.StringVar(&config.lockID, "lock-id", "codeedge-evaluator-child-production-lock", "immutable deployment lock id")
	flag.StringVar(&config.lockVersion, "lock-version", "", "immutable deployment lock version")
	flag.StringVar(&config.gitExecutable, "git-executable", "", "absolute Git executable used to prove source identity")
	flag.StringVar(&config.harborLauncher, "harbor-launcher", "", "absolute frozen Harbor launcher")
	flag.StringVar(&config.pythonInterpreter, "python-interpreter", "", "absolute frozen Python interpreter named by the Harbor launcher")
	flag.StringVar(&config.pythonSourceTree, "python-source-tree", "", "absolute Harbor Python package root")
	flag.StringVar(&config.dockerCLI, "docker-cli", "", "absolute frozen Docker CLI")
	flag.StringVar(&config.dockerComposePlugin, "docker-compose-plugin", "", "absolute frozen Docker Compose CLI plugin")
	flag.StringVar(&config.dockerBuildxPlugin, "docker-buildx-plugin", "", "absolute frozen Docker Buildx CLI plugin")
	flag.Parse()
	if flag.NArg() != 0 {
		fail("unexpected positional arguments")
	}
	var runtime evaluatorRuntime
	lock, err := buildLock(config, &runtime)
	if err != nil {
		fail(err.Error())
	}
	canonical, err := lock.CanonicalJSON()
	if err != nil {
		fail("canonicalize generated lock: " + err.Error())
	}
	if err := verifyEvaluatorRuntimeUnchanged(config, runtime); err != nil {
		fail("final evaluator runtime identity verification: " + err.Error())
	}
	if err := writeNewRegularFile(config.outputPath, canonical); err != nil {
		fail(err.Error())
	}
	fingerprint, err := lock.Fingerprint()
	if err != nil {
		fail("fingerprint generated lock: " + err.Error())
	}
	fmt.Printf("wrote %s\nlock_fingerprint=%s\n", config.outputPath, fingerprint)
}

func build(config buildConfig) (stageprovider.DeploymentOperationCatalogLock, error) {
	return buildLock(config, nil)
}

func buildLock(config buildConfig, observedRuntime *evaluatorRuntime) (stageprovider.DeploymentOperationCatalogLock, error) {
	if err := validateConfig(&config); err != nil {
		return stageprovider.DeploymentOperationCatalogLock{}, err
	}
	if err := requireGitRoot(config.sourceRoot, config.gitExecutable); err != nil {
		return stageprovider.DeploymentOperationCatalogLock{}, err
	}
	if err := requireCleanGitWorktree(config.sourceRoot, config.gitExecutable); err != nil {
		return stageprovider.DeploymentOperationCatalogLock{}, err
	}
	commit, sourceManifest, err := sourceBuildIdentity(config.sourceRoot, config.gitExecutable)
	if err != nil {
		return stageprovider.DeploymentOperationCatalogLock{}, err
	}
	if err := requireCommittedInputs(config.sourceRoot, config.gitExecutable, commit, []string{
		evaluatorCatalogRelative,
		evaluatorAssetManifestRelative,
		evaluatorProfileRelative,
		evaluatorInvocationContractRelative,
		evaluatorResultSchemaRelative,
	}); err != nil {
		return stageprovider.DeploymentOperationCatalogLock{}, err
	}

	catalogRaw, err := readRegularFile(config.catalogPath, maxAssetBytes)
	if err != nil {
		return stageprovider.DeploymentOperationCatalogLock{}, fmt.Errorf("read catalog: %w", err)
	}
	catalogDocument, err := stageprovider.ParseDeploymentOperationCatalogJSON(catalogRaw)
	if err != nil {
		return stageprovider.DeploymentOperationCatalogLock{}, fmt.Errorf("parse catalog: %w", err)
	}
	catalog, err := stageprovider.NewDeploymentOperationCatalogResolver(catalogDocument)
	if err != nil {
		return stageprovider.DeploymentOperationCatalogLock{}, fmt.Errorf("resolve catalog: %w", err)
	}
	if !catalog.Template().Equal(workflowadapter.CodeEdgeEvaluatorChildTemplateReference()) {
		return stageprovider.DeploymentOperationCatalogLock{}, errors.New("catalog must bind harbor.codeedge-evaluator@1.0.0")
	}
	registrations, err := requiredEvaluatorRegistrations(catalog)
	if err != nil {
		return stageprovider.DeploymentOperationCatalogLock{}, err
	}
	profile, err := readEvaluatorExecutionProfile(config.profilePath)
	if err != nil {
		return stageprovider.DeploymentOperationCatalogLock{}, err
	}
	assets, err := readEvaluatorContractAssets(config.assetManifest, config.contractRoot, registrations)
	if err != nil {
		return stageprovider.DeploymentOperationCatalogLock{}, err
	}
	if err := validateEvaluatorEnvironment(config.lookupEnvironment, registrations); err != nil {
		return stageprovider.DeploymentOperationCatalogLock{}, err
	}
	runtime, err := discoverEvaluatorRuntime(config)
	if err != nil {
		return stageprovider.DeploymentOperationCatalogLock{}, err
	}

	operations := make([]stageprovider.DeploymentOperationCatalogLockRecord, 0, len(registrations))
	for _, registration := range registrations {
		contract := registration.HarborEvaluator.Clone()
		asset, found := assets[registration.Stage.Key]
		if !found {
			return stageprovider.DeploymentOperationCatalogLock{}, fmt.Errorf("catalog stage %q has no contract assets", registration.Stage.Key)
		}
		payload, ok := registration.Operation.Payload.(workflowadapter.LocalCommandOperationPayload)
		if !ok {
			return stageprovider.DeploymentOperationCatalogLock{}, fmt.Errorf("catalog stage %q is not a local command", registration.Stage.Key)
		}
		launcher := stageprovider.LocalExecutableLock{
			CommandID: payload.CommandID, AbsolutePath: config.harborLauncher,
			Version: runtime.HarborVersion + "-launcher", ContentSHA256: runtime.LauncherFingerprint,
		}
		typedLock := stageprovider.HarborEvaluatorOperationLock{
			Contract: contract, Launcher: launcher,
			PythonInterpreter: stageprovider.LocalExecutableLock{
				CommandID: stageprovider.HarborEvaluatorPythonCommandID, AbsolutePath: config.pythonInterpreter,
				Version: runtime.PythonVersion, ContentSHA256: runtime.PythonFingerprint,
			},
			PythonSourceTree: stageprovider.HarborPythonSourceTreeLock{
				AbsolutePath: config.pythonSourceTree, PythonFilesSHA256: runtime.SourceFingerprint,
			},
			DockerCLI: stageprovider.LocalExecutableLock{
				CommandID: stageprovider.HarborEvaluatorDockerCommandID, AbsolutePath: config.dockerCLI,
				Version: runtime.DockerVersion, ContentSHA256: runtime.DockerFingerprint,
			},
			DockerServerVersion: runtime.DockerServerVersion,
			DockerComposePlugin: stageprovider.LocalExecutableLock{
				CommandID: stageprovider.HarborEvaluatorDockerComposeCommandID, AbsolutePath: config.dockerComposePlugin,
				Version: runtime.DockerComposeVersion, ContentSHA256: runtime.DockerComposeFingerprint,
			},
			DockerComposeVersionOutput: runtime.DockerComposeVersionOutput,
			DockerBuildxPlugin: stageprovider.LocalExecutableLock{
				CommandID: stageprovider.HarborEvaluatorDockerBuildxCommandID, AbsolutePath: config.dockerBuildxPlugin,
				Version: runtime.DockerBuildxVersion, ContentSHA256: runtime.DockerBuildxFingerprint,
			},
			DockerBuildxVersionOutput: runtime.DockerBuildxVersionOutput,
			HarborVersionOutput:       runtime.HarborVersion,
		}
		operations = append(operations, stageprovider.DeploymentOperationCatalogLockRecord{
			Stage: registration.Stage, Provider: registration.Provider, Operation: registration.Operation.Clone(),
			Runtime: registration.Runtime, Checkout: registration.Checkout,
			Secrets:                  cloneEvaluatorSecrets(registration.Secrets),
			PromptContentFingerprint: asset.promptFingerprint, SchemaContentFingerprint: asset.schemaFingerprint,
			ExecutionKind: registration.Operation.Payload.Kind(), LocalExecutable: &launcher, HarborEvaluator: &typedLock,
		})
	}

	profileLock := stageprovider.CodeEdgeEvaluatorChildExecutionProfileLock{Profile: profile}
	lock := stageprovider.DeploymentOperationCatalogLock{
		Format: stageprovider.DeploymentOperationCatalogLockFormat, Version: stageprovider.DeploymentOperationCatalogLockVersion,
		LockID: config.lockID, LockVersion: config.lockVersion, CatalogReceipt: catalog.Receipt(),
		HarborFlowBuild: stageprovider.HarborFlowBuildIdentity{
			Module: modulePath, Version: config.buildVersion, Commit: commit, ContentSHA256: sourceManifest,
		},
		CodeEdgeEvaluatorChildExecutionProfile: &profileLock,
		Operations:                             operations,
	}
	if _, err := stageprovider.NewDeploymentOperationCatalogLockResolver(catalog, lock); err != nil {
		return stageprovider.DeploymentOperationCatalogLock{}, fmt.Errorf("validate generated catalog lock: %w", err)
	}
	if err := verifyEvaluatorRuntimeUnchanged(config, runtime); err != nil {
		return stageprovider.DeploymentOperationCatalogLock{}, fmt.Errorf("final evaluator runtime identity verification: %w", err)
	}
	if observedRuntime != nil {
		*observedRuntime = runtime
	}
	return lock, nil
}

// cloneEvaluatorSecrets retains an explicit empty JSON array. A nil slice
// would serialize as null and incorrectly weaken the deployment lock's secret
// allow-list representation.
func cloneEvaluatorSecrets(values []workflowadapter.SecretReference) []workflowadapter.SecretReference {
	cloned := make([]workflowadapter.SecretReference, len(values))
	copy(cloned, values)
	return cloned
}

type requiredEvaluatorRegistration struct {
	registration stageprovider.DeploymentOperationRegistration
	stageKey     workflowkit.StageKey
	commandID    string
	operationID  string
	modelID      string
	endpointName string
}

func requiredEvaluatorRegistrations(catalog *stageprovider.DeploymentOperationCatalogResolver) ([]stageprovider.DeploymentOperationRegistration, error) {
	if catalog == nil {
		return nil, errors.New("evaluator catalog is required")
	}
	definitions := []requiredEvaluatorRegistration{
		{stageKey: workflowadapter.HarborRunQwen, commandID: stageprovider.HarborEvaluatorQwenCommandID, operationID: "codeedge.qwen.pass-at-four", modelID: "qwen3.7-max", endpointName: qwenEndpointEnvironment},
		{stageKey: workflowadapter.HarborRunOpus, commandID: stageprovider.HarborEvaluatorOpusCommandID, operationID: "codeedge.opus.pass-at-four", modelID: "claude-opus-4-6", endpointName: opusEndpointEnvironment},
	}
	operations := catalog.Catalog().Operations
	if len(operations) != len(definitions) {
		return nil, errors.New("evaluator catalog must contain exactly Qwen and Opus pass@4 operations")
	}
	byCommand := make(map[string]stageprovider.DeploymentOperationRegistration, len(operations))
	for _, registration := range operations {
		payload, ok := registration.Operation.Payload.(workflowadapter.LocalCommandOperationPayload)
		if !ok || registration.HarborEvaluator == nil {
			return nil, errors.New("evaluator catalog must contain only typed local Harbor evaluator operations")
		}
		if _, duplicate := byCommand[payload.CommandID]; duplicate {
			return nil, errors.New("evaluator catalog has duplicate local command")
		}
		byCommand[payload.CommandID] = registration.Clone()
	}
	ordered := make([]stageprovider.DeploymentOperationRegistration, 0, len(definitions))
	for _, expected := range definitions {
		registration, present := byCommand[expected.commandID]
		if !present {
			return nil, fmt.Errorf("evaluator catalog is missing %s", expected.commandID)
		}
		if registration.Stage.Key != expected.stageKey || registration.Operation.OperationID != expected.operationID || registration.Operation.Version != "1.0.0" {
			return nil, fmt.Errorf("evaluator catalog operation %q does not match the frozen child stage", expected.commandID)
		}
		contract := registration.HarborEvaluator.Clone()
		if contract.AgentID != "claude-code" || contract.AgentVersion != "2.1.207" || contract.ModelID != expected.modelID || contract.ModelVersion != "2026.07.14" ||
			contract.EndpointEnvName != expected.endpointName || contract.EndpointChildEnvKey != "ANTHROPIC_BASE_URL" ||
			contract.Attempts != stageprovider.HarborEvaluatorTrialCount || contract.ConcurrentTrials != stageprovider.HarborEvaluatorConcurrentTrials ||
			contract.MaxRetries != stageprovider.HarborEvaluatorMaxRetries || !contract.RequireTrajectory {
			return nil, fmt.Errorf("evaluator catalog operation %q does not match the approved model or pass@4 contract", expected.commandID)
		}
		if len(registration.Secrets) != 1 || registration.Secrets[0] != (workflowadapter.SecretReference{ID: "anthropic-auth-token", Provider: "environment", Version: "2026.07.14"}) ||
			len(contract.SecretEnvTemplates) != 1 {
			return nil, fmt.Errorf("evaluator catalog operation %q does not have the approved secret reference", expected.commandID)
		}
		secret := contract.SecretEnvTemplates[0]
		if secret.Secret != registration.Secrets[0] || secret.HostEnvKey != credentialEnvironment || secret.ChildEnvKey != credentialEnvironment || secret.Template != stageprovider.HarborEvaluatorSecretValueTemplate {
			return nil, fmt.Errorf("evaluator catalog operation %q does not have the approved secret environment mapping", expected.commandID)
		}
		ordered = append(ordered, registration)
	}
	return ordered, nil
}

func readEvaluatorExecutionProfile(file string) (workflowadapter.ExecutionProfile, error) {
	raw, err := readRegularFile(file, maxAssetBytes)
	if err != nil {
		return workflowadapter.ExecutionProfile{}, fmt.Errorf("read execution profile: %w", err)
	}
	profile, err := workflowadapter.ParseExecutionProfileJSON(raw)
	if err != nil {
		return workflowadapter.ExecutionProfile{}, fmt.Errorf("parse evaluator child execution profile: %w", err)
	}
	if !profile.Template.Equal(workflowadapter.CodeEdgeEvaluatorChildTemplateReference()) {
		return workflowadapter.ExecutionProfile{}, errors.New("execution profile must bind harbor.codeedge-evaluator@1.0.0")
	}
	return profile.Clone(), nil
}

type evaluatorContractAssets struct {
	promptFingerprint workflowkit.Fingerprint
	schemaFingerprint workflowkit.Fingerprint
}

func readEvaluatorContractAssets(manifestPath, root string, registrations []stageprovider.DeploymentOperationRegistration) (map[workflowkit.StageKey]evaluatorContractAssets, error) {
	raw, err := readRegularFile(manifestPath, maxAssetBytes)
	if err != nil {
		return nil, fmt.Errorf("read evaluator contract asset manifest: %w", err)
	}
	manifest, err := parseEvaluatorAssetManifest(raw)
	if err != nil {
		return nil, err
	}
	entries, err := validateEvaluatorAssetManifest(manifest, registrations)
	if err != nil {
		return nil, err
	}
	if len(entries) != 2 {
		return nil, errors.New("evaluator contract asset manifest must cover exactly two stages")
	}
	promptPath, err := evaluatorContractAssetPath(root, entries[workflowadapter.HarborRunQwen].Prompt.RelativePath)
	if err != nil {
		return nil, err
	}
	promptRaw, err := readRegularFile(promptPath, maxAssetBytes)
	if err != nil {
		return nil, fmt.Errorf("read evaluator invocation contract: %w", err)
	}
	if err := validateEvaluatorInvocationAsset(promptRaw, registrations); err != nil {
		return nil, err
	}
	schemaPath, err := evaluatorContractAssetPath(root, entries[workflowadapter.HarborRunQwen].Schema.RelativePath)
	if err != nil {
		return nil, err
	}
	schemaRaw, err := readRegularFile(schemaPath, maxAssetBytes)
	if err != nil {
		return nil, fmt.Errorf("read evaluator result schema: %w", err)
	}
	if err := validateEvaluatorResultSchemaAsset(schemaRaw); err != nil {
		return nil, err
	}
	assets := make(map[workflowkit.StageKey]evaluatorContractAssets, len(entries))
	for stage, entry := range entries {
		entryPromptPath, err := evaluatorContractAssetPath(root, entry.Prompt.RelativePath)
		if err != nil {
			return nil, err
		}
		entryPrompt, err := readRegularFile(entryPromptPath, maxAssetBytes)
		if err != nil {
			return nil, fmt.Errorf("read evaluator prompt asset for %q: %w", stage, err)
		}
		entrySchemaPath, err := evaluatorContractAssetPath(root, entry.Schema.RelativePath)
		if err != nil {
			return nil, err
		}
		entrySchema, err := readRegularFile(entrySchemaPath, maxAssetBytes)
		if err != nil {
			return nil, fmt.Errorf("read evaluator schema asset for %q: %w", stage, err)
		}
		assets[stage] = evaluatorContractAssets{promptFingerprint: workflowkit.SHA256Fingerprint(entryPrompt), schemaFingerprint: workflowkit.SHA256Fingerprint(entrySchema)}
	}
	return assets, nil
}

func parseEvaluatorAssetManifest(raw []byte) (evaluatorAssetManifest, error) {
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return evaluatorAssetManifest{}, fmt.Errorf("decode evaluator contract asset manifest: %w", err)
	}
	var manifest evaluatorAssetManifest
	if err := decodeStrictJSON(raw, &manifest); err != nil {
		return evaluatorAssetManifest{}, fmt.Errorf("decode evaluator contract asset manifest: %w", err)
	}
	return manifest, nil
}

func validateEvaluatorAssetManifest(manifest evaluatorAssetManifest, registrations []stageprovider.DeploymentOperationRegistration) (map[workflowkit.StageKey]evaluatorAssetManifestEntry, error) {
	if manifest.Format != evaluatorContractAssetManifestFormat || manifest.Version != evaluatorContractAssetManifestVersion || !manifest.Template.Equal(workflowadapter.CodeEdgeEvaluatorChildTemplateReference()) {
		return nil, errors.New("evaluator contract asset manifest has an unsupported identity")
	}
	if manifest.Operations == nil || len(manifest.Operations) != len(registrations) {
		return nil, errors.New("evaluator contract asset manifest does not have exact stage coverage")
	}
	expected := make(map[workflowkit.StageKey]struct{}, len(registrations))
	for _, registration := range registrations {
		expected[registration.Stage.Key] = struct{}{}
	}
	entries := make(map[workflowkit.StageKey]evaluatorAssetManifestEntry, len(manifest.Operations))
	for _, entry := range manifest.Operations {
		key := workflowkit.StageKey(entry.StageKey)
		if _, present := expected[key]; !present {
			return nil, fmt.Errorf("evaluator contract asset manifest has unknown stage %q", entry.StageKey)
		}
		if _, duplicate := entries[key]; duplicate {
			return nil, fmt.Errorf("evaluator contract asset manifest has duplicate stage %q", entry.StageKey)
		}
		if entry.Prompt != (evaluatorAssetReference{ID: "codeedge-harbor-pass-at-four-invocation", Version: "0.18.0", RelativePath: "contracts/harbor-pass-at-four.v0.18.json"}) ||
			entry.Schema != (evaluatorAssetReference{ID: "harbor-run-bundle", Version: "0.18.0", RelativePath: "schemas/harbor-run-bundle.v0.18.json"}) {
			return nil, fmt.Errorf("evaluator contract asset manifest stage %q does not bind the approved invocation and result assets", entry.StageKey)
		}
		entries[key] = entry
	}
	return entries, nil
}

func evaluatorContractAssetPath(root, relative string) (string, error) {
	if relative == "" || relative != strings.TrimSpace(relative) || strings.Contains(relative, "\\") || path.Clean(relative) != relative || filepath.IsAbs(relative) || strings.HasPrefix(relative, "../") || relative == ".." {
		return "", errors.New("evaluator contract asset path is invalid")
	}
	result := filepath.Join(root, filepath.FromSlash(relative))
	if !pathWithin(root, result) || result == root {
		return "", errors.New("evaluator contract asset escapes contract root")
	}
	return result, nil
}

func validateEvaluatorInvocationAsset(raw []byte, registrations []stageprovider.DeploymentOperationRegistration) error {
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return fmt.Errorf("decode evaluator invocation contract: %w", err)
	}
	var asset evaluatorInvocationAsset
	if err := decodeStrictJSON(raw, &asset); err != nil {
		return fmt.Errorf("decode evaluator invocation contract: %w", err)
	}
	if asset.Format != evaluatorInvocationAssetFormat || asset.Version != evaluatorInvocationAssetVersion || !asset.Template.Equal(workflowadapter.CodeEdgeEvaluatorChildTemplateReference()) ||
		asset.HarborCLI.Version != stageprovider.HarborEvaluatorHarborVersion || asset.HarborCLI.Subcommand != "run" || asset.CallerArguments != "forbidden" ||
		asset.Locality.RemoteUpload != "forbidden" || asset.Locality.RemoteReconciliation != "forbidden" || asset.Locality.ManagedJobsDirectory != "required" ||
		asset.TrialPolicy.Attempts != stageprovider.HarborEvaluatorTrialCount || asset.TrialPolicy.Concurrent != stageprovider.HarborEvaluatorConcurrentTrials || asset.TrialPolicy.MaxRetries != stageprovider.HarborEvaluatorMaxRetries || !asset.TrialPolicy.TechnicalRetriesPreserveLogicalTrial ||
		len(asset.EvaluatorOrder) != len(registrations) {
		return errors.New("evaluator invocation contract does not match the frozen Harbor pass@4 policy")
	}
	for index, registration := range registrations {
		payload, ok := registration.Operation.Payload.(workflowadapter.LocalCommandOperationPayload)
		if !ok || registration.HarborEvaluator == nil {
			return errors.New("evaluator catalog registration cannot bind invocation contract")
		}
		entry := asset.EvaluatorOrder[index]
		contract := registration.HarborEvaluator
		if entry.StageKey != string(registration.Stage.Key) || entry.CommandID != payload.CommandID || entry.AgentID != contract.AgentID ||
			entry.AgentVersion != contract.AgentVersion || entry.ModelID != contract.ModelID || entry.EndpointEnvName != contract.EndpointEnvName {
			return fmt.Errorf("evaluator invocation contract order entry %d does not match catalog", index)
		}
	}
	return nil
}

func validateEvaluatorResultSchemaAsset(raw []byte) error {
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return fmt.Errorf("decode evaluator result schema: %w", err)
	}
	var asset evaluatorResultSchemaAsset
	if err := decodeStrictJSON(raw, &asset); err != nil {
		return fmt.Errorf("decode evaluator result schema: %w", err)
	}
	wantEvidence := []string{"job.result.json", "job.lock.json", "trial.result.json", "trajectory.json", "terminal.png"}
	if asset.Format != evaluatorResultSchemaAssetFormat || asset.Version != evaluatorResultSchemaAssetVersion ||
		asset.Bundle.Format != stageprovider.HarborEvaluatorResultABIFormat || asset.Bundle.Version != stageprovider.HarborEvaluatorResultABIVersion ||
		asset.TrialContract.LogicalTrialCount != stageprovider.HarborEvaluatorTrialCount || !asset.TrialContract.RequireSingleTerminalScreenshot || asset.TrialContract.ResultParser != "harbor-factory.codeedge.v0.18" ||
		len(asset.RequiredEvidence) != len(wantEvidence) {
		return errors.New("evaluator result schema does not match the frozen Harbor run-bundle contract")
	}
	for index, value := range wantEvidence {
		if asset.RequiredEvidence[index] != value {
			return errors.New("evaluator result schema required evidence differs from the frozen contract")
		}
	}
	return nil
}

func validateEvaluatorEnvironment(lookup func(string) (string, bool), registrations []stageprovider.DeploymentOperationRegistration) error {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	for _, registration := range registrations {
		contract := registration.HarborEvaluator
		endpoint, present := lookup(contract.EndpointEnvName)
		if !present || endpoint == "" {
			return fmt.Errorf("approved evaluator endpoint environment %q is unavailable", contract.EndpointEnvName)
		}
		fingerprint, err := stageprovider.CanonicalHarborEvaluatorEndpointFingerprint(endpoint)
		if err != nil || fingerprint != contract.EndpointFingerprint {
			return fmt.Errorf("approved evaluator endpoint environment %q does not match the source-controlled catalog", contract.EndpointEnvName)
		}
	}
	secret, present := lookup(credentialEnvironment)
	if !present || secret == "" || strings.ContainsAny(secret, "\r\n\x00") {
		return errors.New("approved evaluator credential environment is unavailable or malformed")
	}
	return nil
}

func discoverEvaluatorRuntime(config buildConfig) (evaluatorRuntime, error) {
	if _, err := fingerprintRegularFile(config.gitExecutable); err != nil {
		return evaluatorRuntime{}, fmt.Errorf("Git executable: %w", err)
	}
	launcherSnapshot, err := snapshotEvaluatorExecutableWithContents(config.harborLauncher)
	if err != nil {
		return evaluatorRuntime{}, fmt.Errorf("Harbor launcher: %w", err)
	}
	pythonSnapshot, err := snapshotEvaluatorExecutable(config.pythonInterpreter)
	if err != nil {
		return evaluatorRuntime{}, fmt.Errorf("Python interpreter: %w", err)
	}
	sourceSnapshot, err := snapshotEvaluatorSourceTree(config.pythonSourceTree)
	if err != nil {
		return evaluatorRuntime{}, fmt.Errorf("Harbor Python source tree: %w", err)
	}
	if err := verifyHarborLauncherInterpreter(launcherSnapshot, config.pythonInterpreter, pythonSnapshot); err != nil {
		return evaluatorRuntime{}, err
	}
	harborVersion, err := probe(config.harborLauncher, nil, "--version")
	if err != nil || harborVersion != stageprovider.HarborEvaluatorHarborVersion {
		return evaluatorRuntime{}, errors.New("locked Harbor launcher --version probe does not match the approved release")
	}
	if err := verifyHarborRuntimeInputsUnchanged(config, launcherSnapshot, pythonSnapshot, sourceSnapshot); err != nil {
		return evaluatorRuntime{}, fmt.Errorf("locked Harbor runtime changed during launcher version probing: %w", err)
	}
	pythonVersionOutput, err := probe(config.pythonInterpreter, nil, "--version")
	if err != nil || !strings.HasPrefix(pythonVersionOutput, "Python ") {
		return evaluatorRuntime{}, errors.New("locked Python interpreter --version probe failed")
	}
	pythonVersion := strings.TrimPrefix(pythonVersionOutput, "Python ")
	if pythonVersion == "" || strings.ContainsAny(pythonVersion, " \t\r\n") {
		return evaluatorRuntime{}, errors.New("locked Python interpreter version is invalid")
	}
	if err := verifyHarborRuntimeInputsUnchanged(config, launcherSnapshot, pythonSnapshot, sourceSnapshot); err != nil {
		return evaluatorRuntime{}, fmt.Errorf("locked Harbor runtime changed during interpreter version probing: %w", err)
	}
	dockerSnapshot, err := snapshotEvaluatorExecutable(config.dockerCLI)
	if err != nil {
		return evaluatorRuntime{}, fmt.Errorf("Docker CLI: %w", err)
	}
	dockerComposeSnapshot, err := snapshotEvaluatorExecutable(config.dockerComposePlugin)
	if err != nil {
		return evaluatorRuntime{}, fmt.Errorf("Docker Compose plugin: %w", err)
	}
	dockerBuildxSnapshot, err := snapshotEvaluatorExecutable(config.dockerBuildxPlugin)
	if err != nil {
		return evaluatorRuntime{}, fmt.Errorf("Docker Buildx plugin: %w", err)
	}
	dockerPATH, err := stageprovider.HarborEvaluatorDockerPATH(config.dockerCLI)
	if err != nil {
		return evaluatorRuntime{}, errors.New("locked Docker CLI cannot define the controlled Harbor PATH")
	}
	resolvedDocker, resolvedDockerInfo, err := resolveGeneratorPATHExecutable(dockerPATH, "docker")
	if err != nil || resolvedDocker != config.dockerCLI || !os.SameFile(resolvedDockerInfo, dockerSnapshot.info) {
		return evaluatorRuntime{}, errors.New("controlled Harbor PATH does not resolve the locked Docker CLI")
	}
	dockerEnvironment, cleanupDockerEnvironment, err := evaluatorDockerProbeEnvironment(dockerPATH)
	if err != nil {
		return evaluatorRuntime{}, err
	}
	defer cleanupDockerEnvironment()
	dockerOutput, err := probe(config.dockerCLI, dockerEnvironment, "--version")
	dockerVersion, parsed := dockerVersion(dockerOutput)
	if err != nil || !parsed || dockerVersion != stageprovider.HarborEvaluatorDockerVersion {
		return evaluatorRuntime{}, errors.New("locked Docker CLI --version probe does not match the approved release")
	}
	dockerServerVersion, err := probe(config.dockerCLI, dockerEnvironment, "version", "--format", dockerServerVersionFormat)
	if err != nil || dockerServerVersion != stageprovider.HarborEvaluatorDockerServerVersion {
		return evaluatorRuntime{}, errors.New("locked Docker daemon version probe does not match the approved release")
	}
	composePath, err := probe(config.dockerCLI, dockerEnvironment, "info", "--format", dockerComposePathFormat)
	if err != nil {
		return evaluatorRuntime{}, errors.New("locked Docker Compose plugin resolution probe failed")
	}
	resolvedComposeInfo, resolvedComposeErr := inspectNoSymlinkPath(composePath)
	if resolvedComposeErr != nil || composePath != config.dockerComposePlugin || !resolvedComposeInfo.Mode().IsRegular() || resolvedComposeInfo.Mode()&0o111 == 0 || !os.SameFile(resolvedComposeInfo, dockerComposeSnapshot.info) {
		return evaluatorRuntime{}, errors.New("Docker does not resolve the frozen Compose plugin")
	}
	buildxPath, err := probe(config.dockerCLI, dockerEnvironment, "info", "--format", dockerBuildxPathFormat)
	if err != nil {
		return evaluatorRuntime{}, errors.New("locked Docker Buildx plugin resolution probe failed")
	}
	resolvedBuildxInfo, resolvedBuildxErr := inspectNoSymlinkPath(buildxPath)
	if resolvedBuildxErr != nil || buildxPath != config.dockerBuildxPlugin || !resolvedBuildxInfo.Mode().IsRegular() || resolvedBuildxInfo.Mode()&0o111 == 0 || !os.SameFile(resolvedBuildxInfo, dockerBuildxSnapshot.info) {
		return evaluatorRuntime{}, errors.New("Docker does not resolve the frozen Buildx plugin")
	}
	dockerComposeOutput, err := probe(config.dockerCLI, dockerEnvironment, "compose", "version")
	dockerComposeVersion, parsed := dockerComposeVersion(dockerComposeOutput)
	if err != nil || !parsed || dockerComposeVersion != stageprovider.HarborEvaluatorDockerComposeVersion || dockerComposeOutput != stageprovider.HarborEvaluatorDockerComposeVersionOutput {
		return evaluatorRuntime{}, errors.New("locked Docker Compose version probe does not match the approved release")
	}
	dockerBuildxOutput, err := probe(config.dockerCLI, dockerEnvironment, "buildx", "version")
	dockerBuildxVersion, parsed := dockerBuildxVersion(dockerBuildxOutput)
	if err != nil || !parsed || dockerBuildxVersion != stageprovider.HarborEvaluatorDockerBuildxVersion || dockerBuildxOutput != stageprovider.HarborEvaluatorDockerBuildxVersionOutput {
		return evaluatorRuntime{}, errors.New("locked Docker Buildx version probe does not match the approved release")
	}
	runtime := evaluatorRuntime{
		LauncherFingerprint: launcherSnapshot.fingerprint, HarborVersion: harborVersion,
		PythonFingerprint: pythonSnapshot.fingerprint, PythonVersion: pythonVersion, SourceFingerprint: sourceSnapshot.fingerprint,
		DockerFingerprint: dockerSnapshot.fingerprint, DockerVersion: dockerVersion, DockerServerVersion: dockerServerVersion,
		DockerComposeFingerprint: dockerComposeSnapshot.fingerprint, DockerComposeVersion: dockerComposeVersion, DockerComposeVersionOutput: dockerComposeOutput,
		DockerBuildxFingerprint: dockerBuildxSnapshot.fingerprint, DockerBuildxVersion: dockerBuildxVersion, DockerBuildxVersionOutput: dockerBuildxOutput,
		launcherSnapshot: launcherSnapshot, pythonSnapshot: pythonSnapshot, sourceSnapshot: sourceSnapshot,
		dockerSnapshot: dockerSnapshot, dockerComposeSnapshot: dockerComposeSnapshot, dockerBuildxSnapshot: dockerBuildxSnapshot,
	}
	if err := verifyEvaluatorRuntimeUnchanged(config, runtime); err != nil {
		return evaluatorRuntime{}, fmt.Errorf("locked evaluator runtime changed during controlled probing: %w", err)
	}
	return runtime, nil
}

type evaluatorExecutableSnapshot struct {
	info        os.FileInfo
	fingerprint workflowkit.Fingerprint
	contents    []byte
}

func snapshotEvaluatorExecutable(path string) (evaluatorExecutableSnapshot, error) {
	snapshot, err := snapshotEvaluatorExecutableWithContents(path)
	if err != nil {
		return evaluatorExecutableSnapshot{}, err
	}
	snapshot.contents = nil
	return snapshot, nil
}

func snapshotEvaluatorExecutableWithContents(path string) (evaluatorExecutableSnapshot, error) {
	initial, err := inspectNoSymlinkPath(path)
	if err != nil || !initial.Mode().IsRegular() || initial.Mode()&0o111 == 0 {
		return evaluatorExecutableSnapshot{}, errors.New("executable path cannot be inspected")
	}
	contents, err := readRegularFile(path, -1)
	if err != nil {
		return evaluatorExecutableSnapshot{}, err
	}
	final, err := inspectNoSymlinkPath(path)
	if err != nil || !final.Mode().IsRegular() || !os.SameFile(initial, final) {
		return evaluatorExecutableSnapshot{}, errors.New("executable changed while being fingerprinted")
	}
	return evaluatorExecutableSnapshot{info: final, fingerprint: workflowkit.SHA256Fingerprint(contents), contents: contents}, nil
}

func verifyEvaluatorExecutableUnchanged(path string, initial evaluatorExecutableSnapshot) error {
	final, err := snapshotEvaluatorExecutable(path)
	if err != nil || !os.SameFile(initial.info, final.info) || initial.fingerprint != final.fingerprint {
		return errors.New("executable identity changed")
	}
	return nil
}

type evaluatorSourceTreeSnapshot struct {
	rootInfo    os.FileInfo
	fingerprint workflowkit.Fingerprint
	identities  map[string]os.FileInfo
}

func snapshotEvaluatorSourceTree(root string) (evaluatorSourceTreeSnapshot, error) {
	initialRoot, err := inspectNoSymlinkPath(root)
	if err != nil || !initialRoot.IsDir() {
		return evaluatorSourceTreeSnapshot{}, errors.New("source tree path cannot be inspected")
	}
	initialIdentities, err := inspectEvaluatorSourceTreeIdentities(root)
	if err != nil {
		return evaluatorSourceTreeSnapshot{}, err
	}
	fingerprint, err := stageprovider.ComputeHarborPythonSourceTreeFingerprint(context.Background(), root)
	if err != nil {
		return evaluatorSourceTreeSnapshot{}, err
	}
	finalIdentities, err := inspectEvaluatorSourceTreeIdentities(root)
	if err != nil {
		return evaluatorSourceTreeSnapshot{}, err
	}
	finalRoot, err := inspectNoSymlinkPath(root)
	if err != nil || !finalRoot.IsDir() || !os.SameFile(initialRoot, finalRoot) || !sameEvaluatorSourceTreeIdentities(initialIdentities, finalIdentities) {
		return evaluatorSourceTreeSnapshot{}, errors.New("source tree identity changed while being fingerprinted")
	}
	return evaluatorSourceTreeSnapshot{rootInfo: finalRoot, fingerprint: fingerprint, identities: finalIdentities}, nil
}

func inspectEvaluatorSourceTreeIdentities(root string) (map[string]os.FileInfo, error) {
	identities := make(map[string]os.FileInfo)
	pythonFiles := 0
	err := filepath.WalkDir(root, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry == nil || entry.Type()&os.ModeSymlink != 0 {
			return errors.New("source tree entry cannot be inspected")
		}
		info, err := entry.Info()
		if err != nil {
			return errors.New("source tree entry cannot be inspected")
		}
		pathInfo, err := inspectNoSymlinkPath(current)
		if err != nil || !os.SameFile(info, pathInfo) {
			return errors.New("source tree entry changed while being inspected")
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return errors.New("source tree contains a non-regular file")
		}
		if !strings.HasSuffix(entry.Name(), ".py") {
			return nil
		}
		if strings.ContainsAny(current, "\r\n\x00") {
			return errors.New("source tree contains a noncanonical Python path")
		}
		pythonFiles++
		for candidate := current; ; candidate = filepath.Dir(candidate) {
			candidateInfo, err := inspectNoSymlinkPath(candidate)
			if err != nil {
				return errors.New("source tree entry path cannot be inspected")
			}
			if previous, found := identities[candidate]; found && !os.SameFile(previous, candidateInfo) {
				return errors.New("source tree entry path changed while being inspected")
			}
			identities[candidate] = candidateInfo
			if candidate == root {
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if pythonFiles == 0 {
		return nil, errors.New("source tree contains no Python files")
	}
	return identities, nil
}

func sameEvaluatorSourceTreeIdentities(left, right map[string]os.FileInfo) bool {
	if len(left) != len(right) {
		return false
	}
	for path, leftInfo := range left {
		rightInfo, found := right[path]
		if !found || !os.SameFile(leftInfo, rightInfo) {
			return false
		}
	}
	return true
}

func verifyEvaluatorSourceTreeUnchanged(root string, initial evaluatorSourceTreeSnapshot) error {
	final, err := snapshotEvaluatorSourceTree(root)
	if err != nil || !os.SameFile(initial.rootInfo, final.rootInfo) || initial.fingerprint != final.fingerprint || !sameEvaluatorSourceTreeIdentities(initial.identities, final.identities) {
		return errors.New("source tree identity changed")
	}
	return nil
}

func verifyHarborRuntimeInputsUnchanged(config buildConfig, launcher, interpreter evaluatorExecutableSnapshot, source evaluatorSourceTreeSnapshot) error {
	if err := verifyEvaluatorExecutableUnchanged(config.harborLauncher, launcher); err != nil {
		return errors.New("Harbor launcher identity changed")
	}
	if err := verifyEvaluatorExecutableUnchanged(config.pythonInterpreter, interpreter); err != nil {
		return errors.New("Python interpreter identity changed")
	}
	if err := verifyEvaluatorSourceTreeUnchanged(config.pythonSourceTree, source); err != nil {
		return errors.New("Harbor Python source tree identity changed")
	}
	return nil
}

func verifyEvaluatorRuntimeUnchanged(config buildConfig, runtime evaluatorRuntime) error {
	if err := verifyHarborRuntimeInputsUnchanged(config, runtime.launcherSnapshot, runtime.pythonSnapshot, runtime.sourceSnapshot); err != nil {
		return err
	}
	for _, executable := range []struct {
		label    string
		path     string
		snapshot evaluatorExecutableSnapshot
	}{
		{label: "Docker CLI", path: config.dockerCLI, snapshot: runtime.dockerSnapshot},
		{label: "Docker Compose plugin", path: config.dockerComposePlugin, snapshot: runtime.dockerComposeSnapshot},
		{label: "Docker Buildx plugin", path: config.dockerBuildxPlugin, snapshot: runtime.dockerBuildxSnapshot},
	} {
		if err := verifyEvaluatorExecutableUnchanged(executable.path, executable.snapshot); err != nil {
			return fmt.Errorf("%s identity changed", executable.label)
		}
	}
	return nil
}

func evaluatorDockerProbeEnvironment(dockerPATH string) ([]string, func(), error) {
	root, err := os.MkdirTemp("", "harbor-evaluator-lock-docker-probe-")
	if err != nil {
		return nil, nil, errors.New("create isolated Docker probe environment")
	}
	cleanup := func() { _ = os.RemoveAll(root) }
	configDirectory := filepath.Join(root, "config")
	if err := os.Mkdir(configDirectory, 0o700); err != nil {
		cleanup()
		return nil, nil, errors.New("create isolated Docker probe environment")
	}
	return []string{
		"DOCKER_CONFIG=" + configDirectory,
		"HOME=" + root,
		"LANG=C.UTF-8",
		"PATH=" + dockerPATH,
	}, cleanup, nil
}

func resolveGeneratorPATHExecutable(searchPath, basename string) (string, os.FileInfo, error) {
	for _, directory := range filepath.SplitList(searchPath) {
		if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
			return "", nil, errors.New("noncanonical executable search directory")
		}
		candidate := filepath.Join(directory, basename)
		leaf, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || leaf.Mode()&os.ModeSymlink != 0 {
			return "", nil, errors.New("invalid executable search result")
		}
		if !leaf.Mode().IsRegular() || leaf.Mode()&0o111 == 0 {
			continue
		}
		info, err := inspectNoSymlinkPath(candidate)
		if err != nil {
			return "", nil, err
		}
		return candidate, info, nil
	}
	return "", nil, errors.New("executable is unavailable")
}

func verifyHarborLauncherInterpreter(launcher evaluatorExecutableSnapshot, interpreterPath string, interpreter evaluatorExecutableSnapshot) error {
	raw := launcher.contents
	if len(raw) == 0 || int64(len(raw)) > maxShebangBytes {
		return errors.New("locked Harbor launcher cannot be read for interpreter verification")
	}
	first, _, found := bytes.Cut(raw, []byte{'\n'})
	if !found || !bytes.HasPrefix(first, []byte("#!")) {
		return errors.New("locked Harbor launcher lacks a strict Python shebang")
	}
	shebang := strings.TrimSuffix(string(first[2:]), "\r")
	parts := strings.Fields(shebang)
	if len(parts) != 1 || parts[0] != shebang || !filepath.IsAbs(shebang) || filepath.Clean(shebang) != shebang {
		return errors.New("locked Harbor launcher shebang is not an absolute interpreter path")
	}
	resolved, err := filepath.EvalSymlinks(shebang)
	if err != nil || resolved != interpreterPath {
		return errors.New("locked Harbor launcher shebang does not resolve to the pinned Python interpreter")
	}
	resolvedInfo, err := inspectNoSymlinkPath(resolved)
	if err != nil || !os.SameFile(resolvedInfo, interpreter.info) {
		return errors.New("locked Harbor launcher shebang does not resolve to the pinned Python interpreter")
	}
	return nil
}

func dockerVersion(value string) (string, bool) {
	fields := strings.Fields(value)
	if len(fields) < 3 || fields[0] != "Docker" || fields[1] != "version" {
		return "", false
	}
	version := strings.TrimSuffix(fields[2], ",")
	return version, version != "" && version == strings.TrimSpace(version)
}

func dockerComposeVersion(value string) (string, bool) {
	const prefix = "Docker Compose version "
	if !strings.HasPrefix(value, prefix) {
		return "", false
	}
	version := strings.TrimPrefix(value, prefix)
	return version, version != "" && version == strings.TrimSpace(version) && !strings.ContainsAny(version, " \t\r\n")
}

func dockerBuildxVersion(value string) (string, bool) {
	fields := strings.Fields(value)
	if len(fields) != 3 || fields[0] != "github.com/docker/buildx" || !strings.HasPrefix(fields[1], "v") || len(fields[2]) != 40 || !isLowerHex(fields[2]) {
		return "", false
	}
	return fields[1], true
}

func validateConfig(config *buildConfig) error {
	if config == nil {
		return errors.New("build configuration is required")
	}
	var err error
	for _, field := range []*string{
		&config.sourceRoot, &config.catalogPath, &config.assetManifest, &config.profilePath, &config.contractRoot,
		&config.outputPath, &config.gitExecutable, &config.harborLauncher, &config.pythonInterpreter, &config.pythonSourceTree, &config.dockerCLI, &config.dockerComposePlugin, &config.dockerBuildxPlugin,
	} {
		*field, err = cleanAbsolutePath(*field)
		if err != nil {
			return err
		}
	}
	if err := requireNonSymlinkDirectory(config.sourceRoot); err != nil {
		return fmt.Errorf("source root: %w", err)
	}
	for label, executable := range map[string]string{
		"Git executable":        config.gitExecutable,
		"Harbor launcher":       config.harborLauncher,
		"Python interpreter":    config.pythonInterpreter,
		"Docker CLI":            config.dockerCLI,
		"Docker Compose plugin": config.dockerComposePlugin,
		"Docker Buildx plugin":  config.dockerBuildxPlugin,
	} {
		if err := requireExecutableRegularFile(executable); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
	}
	if filepath.Base(config.dockerCLI) != "docker" {
		return errors.New("Docker CLI basename must be docker")
	}
	if filepath.Base(config.dockerComposePlugin) != "docker-compose" {
		return errors.New("Docker Compose plugin basename must be docker-compose")
	}
	if filepath.Base(config.dockerBuildxPlugin) != "docker-buildx" {
		return errors.New("Docker Buildx plugin basename must be docker-buildx")
	}
	for label, value := range map[string]string{"build version": config.buildVersion, "lock id": config.lockID, "lock version": config.lockVersion} {
		if err := validateVersionedText(label, value); err != nil {
			return err
		}
	}
	expected := map[*string]string{
		&config.catalogPath:   evaluatorCatalogRelative,
		&config.assetManifest: evaluatorAssetManifestRelative,
		&config.profilePath:   evaluatorProfileRelative,
		&config.contractRoot:  evaluatorDeploymentDirectory,
		&config.outputPath:    evaluatorLockRelative,
	}
	for supplied, relative := range expected {
		want := filepath.Join(config.sourceRoot, filepath.FromSlash(relative))
		if *supplied != want {
			return fmt.Errorf("path must be the fixed managed asset %s", relative)
		}
	}
	if _, err := os.Lstat(config.outputPath); err == nil || !errors.Is(err, os.ErrNotExist) {
		return errors.New("output lock must not already exist")
	}
	for label, candidate := range map[string]string{"catalog": config.catalogPath, "asset manifest": config.assetManifest, "execution profile": config.profilePath, "contract root": config.contractRoot} {
		if !pathWithin(config.sourceRoot, candidate) {
			return fmt.Errorf("%s must be below source root", label)
		}
	}
	for label, directory := range map[string]string{"source root": config.sourceRoot, "contract root": config.contractRoot, "Harbor Python source tree": config.pythonSourceTree} {
		if err := requireNonSymlinkDirectory(directory); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
	}
	return nil
}

func sourceBuildIdentity(root, gitPath string) (string, workflowkit.Fingerprint, error) {
	commit, err := probeAt(root, gitPath, []string{"PATH=/usr/local/bin:/usr/bin:/bin"}, "rev-parse", "HEAD")
	if err != nil || len(commit) != 40 || strings.ToLower(commit) != commit || !isLowerHex(commit) {
		return "", "", errors.New("resolve source Git commit")
	}
	tree, err := runAtWithLimit(root, gitPath, []string{"PATH=/usr/local/bin:/usr/bin:/bin"}, maxSourceTreeBytes, "ls-tree", "-r", "--full-tree", commit)
	if err != nil {
		return "", "", errors.New("read source Git tree")
	}
	var manifest bytes.Buffer
	for _, line := range bytes.SplitAfter(tree, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		parts := bytes.SplitN(bytes.TrimSuffix(line, []byte("\n")), []byte("\t"), 2)
		if len(parts) != 2 {
			return "", "", errors.New("read source Git tree")
		}
		if _, generated := generatedProductionLocks[string(parts[1])]; !generated {
			_, _ = manifest.Write(line)
		}
	}
	return commit, workflowkit.SHA256Fingerprint(manifest.Bytes()), nil
}

func requireCommittedInputs(root, gitPath, commit string, paths []string) error {
	for _, relative := range paths {
		output, err := runAt(root, gitPath, []string{"PATH=/usr/local/bin:/usr/bin:/bin"}, "ls-tree", "-r", "--name-only", commit, "--", relative)
		if err != nil || string(output) != relative+"\n" {
			return fmt.Errorf("required evaluator source asset is not committed at source revision: %s", relative)
		}
	}
	return nil
}

func requireCleanGitWorktree(root, gitPath string) error {
	status, err := runAt(root, gitPath, []string{"PATH=/usr/local/bin:/usr/bin:/bin"}, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil || len(status) != 0 {
		return errors.New("a clean committed Git worktree is required before generating the evaluator child lock")
	}
	return nil
}

func requireGitRoot(root, gitPath string) error {
	resolved, err := probeAt(root, gitPath, []string{"PATH=/usr/local/bin:/usr/bin:/bin"}, "rev-parse", "--show-toplevel")
	if err != nil || resolved != root {
		return errors.New("source root must be the clean Git worktree root")
	}
	return nil
}

func cleanAbsolutePath(value string) (string, error) {
	if value == "" || value != strings.TrimSpace(value) || !filepath.IsAbs(value) || filepath.Clean(value) != value || value == string(filepath.Separator) {
		return "", errors.New("path must be a clean non-root absolute path")
	}
	return value, nil
}

func pathWithin(root, value string) bool {
	relative, err := filepath.Rel(root, value)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func requireNonSymlinkDirectory(directory string) error {
	info, err := inspectNoSymlinkPath(directory)
	if err != nil || !info.IsDir() {
		return errors.New("must be an existing non-symlink directory")
	}
	return nil
}

func requireExecutableRegularFile(file string) error {
	info, err := inspectNoSymlinkPath(file)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return errors.New("must be an executable regular file with no symlink path component")
	}
	return nil
}

func fingerprintRegularFile(file string) (workflowkit.Fingerprint, error) {
	if err := requireExecutableRegularFile(file); err != nil {
		return "", err
	}
	contents, err := readRegularFile(file, -1)
	if err != nil {
		return "", err
	}
	return workflowkit.SHA256Fingerprint(contents), nil
}

func isLowerHex(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if (character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') {
			continue
		}
		return false
	}
	return true
}

func validateVersionedText(label, value string) error {
	if value == "" || value != strings.TrimSpace(value) || strings.ContainsAny(value, "\r\n\x00") || value == "latest" || value == "unknown" {
		return fmt.Errorf("%s is required and must be a concrete canonical versioned value", label)
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '.' || character == '_' || character == '-' || character == '+' {
			continue
		}
		return fmt.Errorf("%s contains unsupported character %q", label, character)
	}
	return nil
}

func readRegularFile(file string, limit int64) ([]byte, error) {
	initial, err := inspectNoSymlinkPath(file)
	if err != nil || !initial.Mode().IsRegular() {
		return nil, errors.New("must be a regular file with no symlink path component")
	}
	if limit >= 0 && initial.Size() > limit {
		return nil, errors.New("regular file exceeds read limit")
	}
	handle, err := os.Open(file)
	if err != nil {
		return nil, errors.New("open regular file")
	}
	defer handle.Close()
	opened, err := handle.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(initial, opened) {
		return nil, errors.New("regular file changed while opening")
	}
	reader := io.Reader(handle)
	if limit >= 0 {
		reader = io.LimitReader(handle, limit+1)
	}
	contents, err := io.ReadAll(reader)
	if err != nil || (limit >= 0 && int64(len(contents)) > limit) {
		return nil, errors.New("read regular file")
	}
	final, err := handle.Stat()
	pathInfo, pathErr := inspectNoSymlinkPath(file)
	if err != nil || pathErr != nil || !final.Mode().IsRegular() || !pathInfo.Mode().IsRegular() || !os.SameFile(opened, final) || !os.SameFile(opened, pathInfo) || final.Size() != opened.Size() || pathInfo.Size() != opened.Size() {
		return nil, errors.New("regular file changed while reading")
	}
	return contents, nil
}

func inspectNoSymlinkPath(file string) (os.FileInfo, error) {
	components := make([]string, 0, 8)
	for current := file; ; {
		components = append(components, current)
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	var final os.FileInfo
	for index := len(components) - 1; index >= 0; index-- {
		info, err := os.Lstat(components[index])
		if err != nil || info.Mode()&os.ModeSymlink != 0 || (index > 0 && !info.IsDir()) {
			return nil, errors.New("invalid path component")
		}
		if index == 0 {
			final = info
		}
	}
	return final, nil
}

func probe(command string, environment []string, arguments ...string) (string, error) {
	return probeAt(filepath.Dir(command), command, environment, arguments...)
}

func probeAt(directory, command string, environment []string, arguments ...string) (string, error) {
	output, err := runAt(directory, command, environment, arguments...)
	if err != nil {
		return "", err
	}
	value := strings.TrimSuffix(string(output), "\n")
	value = strings.TrimSuffix(value, "\r")
	if value == "" || value != strings.TrimSpace(value) || strings.ContainsAny(value, "\r\n") {
		return "", errors.New("probe output must be one canonical line")
	}
	return value, nil
}

func runAt(directory, command string, environment []string, arguments ...string) ([]byte, error) {
	return runAtWithLimit(directory, command, environment, maxProbeBytes, arguments...)
}

func runAtWithLimit(directory, command string, environment []string, limit int, arguments ...string) ([]byte, error) {
	if limit <= 0 {
		return nil, errors.New("controlled probe output limit is invalid")
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	process := exec.CommandContext(ctx, command, arguments...)
	process.Dir = directory
	process.Env = append([]string(nil), environment...)
	var stdout limitedBuffer
	stdout.limit = limit
	process.Stdout = &stdout
	process.Stderr = io.Discard
	if err := process.Run(); err != nil || stdout.exceeded || ctx.Err() != nil {
		return nil, errors.New("controlled probe failed")
	}
	return append([]byte(nil), stdout.buffer.Bytes()...), nil
}

type limitedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
}

func (buffer *limitedBuffer) Write(value []byte) (int, error) {
	if buffer == nil || buffer.limit <= 0 {
		return 0, errors.New("bounded output unavailable")
	}
	if buffer.buffer.Len()+len(value) > buffer.limit {
		buffer.exceeded = true
		return len(value), nil
	}
	return buffer.buffer.Write(value)
}

func rejectDuplicateJSONKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := walkJSONValue(decoder, "$"); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return fmt.Errorf("decode trailing JSON value: %w", err)
	}
	return nil
}

func walkJSONValue(decoder *json.Decoder, location string) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("object key at %s is not a string", location)
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate object key %q at %s", key, location)
			}
			seen[key] = struct{}{}
			if err := walkJSONValue(decoder, location+"."+key); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return fmt.Errorf("object at %s is not closed", location)
		}
	case '[':
		index := 0
		for decoder.More() {
			if err := walkJSONValue(decoder, fmt.Sprintf("%s[%d]", location, index)); err != nil {
				return err
			}
			index++
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return fmt.Errorf("array at %s is not closed", location)
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q at %s", delimiter, location)
	}
	return nil
}

func decodeStrictJSON(raw []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return fmt.Errorf("decode trailing JSON value: %w", err)
	}
	return nil
}

func writeNewRegularFile(file string, contents []byte) error {
	parent := filepath.Dir(file)
	if err := requireNonSymlinkDirectory(parent); err != nil {
		return fmt.Errorf("output parent: %w", err)
	}
	if _, err := os.Lstat(file); err == nil || !errors.Is(err, os.ErrNotExist) {
		return errors.New("refuse to replace an existing output lock")
	}
	temporary, err := os.CreateTemp(parent, ".codeedge-evaluator-lock-*")
	if err != nil {
		return errors.New("create lock staging file")
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o644); err != nil {
		_ = temporary.Close()
		return errors.New("set lock staging permissions")
	}
	if _, err := temporary.Write(contents); err != nil || temporary.Sync() != nil || temporary.Close() != nil {
		return errors.New("write lock staging file")
	}
	if err := os.Link(temporaryPath, file); err != nil {
		return errors.New("publish lock without replacing an existing file")
	}
	return nil
}

func fail(message string) {
	fmt.Fprintln(os.Stderr, "codeedge-evaluator-lock-build:", message)
	os.Exit(1)
}
