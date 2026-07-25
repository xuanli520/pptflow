// Command codeedge-phase1-lock-build generates the immutable CodeEdge Phase-1
// parent deployment lock. It accepts only the fixed source-controlled parent
// assets, a clean committed Git worktree, and one explicitly resolved Docker
// executable. It never reads provider endpoints, credentials, or ambient
// executable paths.
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
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/codeedge"
	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	modulePath               = "github.com/purplevoid/harbor-factory"
	maxAssetBytes      int64 = 4 * 1024 * 1024
	maxProbeBytes            = 64 * 1024
	maxSourceTreeBytes       = 64 * 1024 * 1024
	probeTimeout             = 30 * time.Second

	parentDeploymentDirectory = "deployments/codeedge-phase1"
	parentCatalogRelative     = parentDeploymentDirectory + "/operation-catalog.v1.json"
	parentProfileRelative     = parentDeploymentDirectory + "/execution-profile.v1.json"
	parentPreflightRelative   = parentDeploymentDirectory + "/preflight-profile.v1.json"
	parentPolicyRelative      = parentDeploymentDirectory + "/final-compliance-policy.v1.json"
	parentLockRelative        = parentDeploymentDirectory + "/operation-catalog.lock.json"
)

var generatedLockRelatives = []string{
	"deployments/standard-authoring/operation-catalog.lock.json",
	parentLockRelative,
	"deployments/codeedge-evaluator-child/operation-catalog.lock.json",
}

type buildConfig struct {
	sourceRoot    string
	catalogPath   string
	profilePath   string
	preflightPath string
	policyPath    string
	outputPath    string
	buildVersion  string
	lockID        string
	lockVersion   string
	gitExecutable string
	dockerPath    string
}

func main() {
	var config buildConfig
	flag.StringVar(&config.sourceRoot, "source-root", "", "clean Git worktree root")
	flag.StringVar(&config.catalogPath, "catalog", "", "CodeEdge Phase-1 parent operation catalog")
	flag.StringVar(&config.profilePath, "execution-profile", "", "source-controlled complete parent execution profile")
	flag.StringVar(&config.preflightPath, "preflight-profile", "", "source-controlled parent preflight profile")
	flag.StringVar(&config.policyPath, "final-compliance-policy", "", "source-controlled parent final compliance policy")
	flag.StringVar(&config.outputPath, "output", "", "new parent operation catalog lock output path")
	flag.StringVar(&config.buildVersion, "build-version", "", "immutable Harbor Flow build version")
	flag.StringVar(&config.lockID, "lock-id", "codeedge-phase1-parent-production-lock", "immutable deployment lock id")
	flag.StringVar(&config.lockVersion, "lock-version", "", "immutable deployment lock version")
	flag.StringVar(&config.gitExecutable, "git-executable", "", "absolute Git executable used to prove the source snapshot")
	flag.StringVar(&config.dockerPath, "docker-executable", "", "absolute Docker executable used by all three parent local commands")
	flag.Parse()
	if flag.NArg() != 0 {
		fail("unexpected positional arguments")
	}
	lock, err := build(config)
	if err != nil {
		fail(err.Error())
	}
	canonical, err := lock.CanonicalJSON()
	if err != nil {
		fail("canonicalize generated lock: " + err.Error())
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
	if err := validateConfig(&config); err != nil {
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
		parentCatalogRelative,
		parentProfileRelative,
		parentPreflightRelative,
		parentPolicyRelative,
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
	if !catalog.Template().Equal(workflowadapter.CodeEdgePhase1TemplateReference()) {
		return stageprovider.DeploymentOperationCatalogLock{}, errors.New("catalog must bind harbor.codeedge-phase1@2.2.0")
	}

	profile, profileCanonical, err := readParentExecutionProfile(config.profilePath)
	if err != nil {
		return stageprovider.DeploymentOperationCatalogLock{}, err
	}
	preflight, preflightCanonical, err := readParentPreflightProfile(config.preflightPath)
	if err != nil {
		return stageprovider.DeploymentOperationCatalogLock{}, err
	}
	policy, policyCanonical, err := readParentFinalCompliancePolicy(config.policyPath)
	if err != nil {
		return stageprovider.DeploymentOperationCatalogLock{}, err
	}

	docker, err := discoverDockerLock(config.dockerPath)
	if err != nil {
		return stageprovider.DeploymentOperationCatalogLock{}, err
	}
	buildIdentity := stageprovider.HarborFlowBuildIdentity{
		Module: modulePath, Version: config.buildVersion, Commit: commit, ContentSHA256: sourceManifest,
	}
	if err := buildIdentity.Validate(); err != nil {
		return stageprovider.DeploymentOperationCatalogLock{}, fmt.Errorf("validate source build identity: %w", err)
	}

	operations, err := lockParentOperations(catalog.Catalog().Operations, docker, profileCanonical, preflightCanonical, policyCanonical)
	if err != nil {
		return stageprovider.DeploymentOperationCatalogLock{}, err
	}
	lock := stageprovider.DeploymentOperationCatalogLock{
		Format:          stageprovider.DeploymentOperationCatalogLockFormat,
		Version:         stageprovider.DeploymentOperationCatalogLockVersion,
		LockID:          config.lockID,
		LockVersion:     config.lockVersion,
		CatalogReceipt:  catalog.Receipt(),
		HarborFlowBuild: buildIdentity,
		CodeEdgePhase1ExecutionProfile: &stageprovider.CodeEdgePhase1ExecutionProfileLock{
			Profile: profile,
		},
		CodeEdgePhase1PreflightProfile: &stageprovider.CodeEdgePhase1PreflightProfileLock{
			Profile: preflight,
		},
		CodeEdgePhase1FinalCompliancePolicy: &stageprovider.CodeEdgePhase1FinalCompliancePolicyLock{
			Policy: policy,
		},
		Operations: operations,
	}
	if _, err := stageprovider.NewDeploymentOperationCatalogLockResolver(catalog, lock); err != nil {
		return stageprovider.DeploymentOperationCatalogLock{}, fmt.Errorf("validate generated catalog lock: %w", err)
	}
	return lock, nil
}

func validateConfig(config *buildConfig) error {
	if config == nil {
		return errors.New("build configuration is required")
	}
	var err error
	for _, field := range []*string{
		&config.sourceRoot,
		&config.catalogPath,
		&config.profilePath,
		&config.preflightPath,
		&config.policyPath,
		&config.outputPath,
		&config.gitExecutable,
		&config.dockerPath,
	} {
		*field, err = cleanAbsolutePath(*field)
		if err != nil {
			return err
		}
	}
	if err := requireNonSymlinkDirectory(config.sourceRoot); err != nil {
		return fmt.Errorf("source root: %w", err)
	}
	if err := requireExecutableRegularFile(config.gitExecutable); err != nil {
		return fmt.Errorf("Git executable: %w", err)
	}
	if err := requireExecutableRegularFile(config.dockerPath); err != nil {
		return fmt.Errorf("Docker executable: %w", err)
	}
	for label, value := range map[string]string{
		"build version": config.buildVersion,
		"lock id":       config.lockID,
		"lock version":  config.lockVersion,
	} {
		if err := validateVersionedText(label, value); err != nil {
			return err
		}
	}
	if err := requireGitTopLevel(config.sourceRoot, config.gitExecutable); err != nil {
		return err
	}

	expected := map[*string]string{
		&config.catalogPath:   parentCatalogRelative,
		&config.profilePath:   parentProfileRelative,
		&config.preflightPath: parentPreflightRelative,
		&config.policyPath:    parentPolicyRelative,
		&config.outputPath:    parentLockRelative,
	}
	for supplied, relative := range expected {
		want := filepath.Clean(filepath.Join(config.sourceRoot, filepath.FromSlash(relative)))
		if *supplied != want {
			return fmt.Errorf("path must be the fixed managed asset %s", relative)
		}
	}
	if _, err := os.Lstat(config.outputPath); err == nil || !errors.Is(err, os.ErrNotExist) {
		return errors.New("output lock must not already exist")
	}
	for label, path := range map[string]string{
		"catalog":                 config.catalogPath,
		"execution profile":       config.profilePath,
		"preflight profile":       config.preflightPath,
		"final compliance policy": config.policyPath,
	} {
		if !pathWithin(config.sourceRoot, path) {
			return fmt.Errorf("%s must be below source root", label)
		}
	}
	return nil
}

func readParentExecutionProfile(path string) (workflowadapter.ExecutionProfile, []byte, error) {
	raw, err := readRegularFile(path, maxAssetBytes)
	if err != nil {
		return workflowadapter.ExecutionProfile{}, nil, fmt.Errorf("read execution profile: %w", err)
	}
	profile, err := workflowadapter.ParseExecutionProfileJSON(raw)
	if err != nil {
		return workflowadapter.ExecutionProfile{}, nil, fmt.Errorf("parse CodeEdge Phase-1 execution profile: %w", err)
	}
	if !profile.Template.Equal(workflowadapter.CodeEdgePhase1TemplateReference()) {
		return workflowadapter.ExecutionProfile{}, nil, errors.New("execution profile must bind harbor.codeedge-phase1@2.2.0")
	}
	for _, key := range workflowadapter.CodeEdgePhase1StageOrder() {
		budget, found := profile.Budget(key)
		if !found || budget.MaxAttempts != 1 || len(budget.Backoff.RetryDelays) != 0 {
			return workflowadapter.ExecutionProfile{}, nil, fmt.Errorf("parent execution profile stage %q must disable generic retries", key)
		}
	}
	canonical, err := profile.CanonicalJSON()
	if err != nil {
		return workflowadapter.ExecutionProfile{}, nil, fmt.Errorf("canonicalize CodeEdge Phase-1 execution profile: %w", err)
	}
	return profile.Clone(), canonical, nil
}

func readParentPreflightProfile(path string) (codeedge.Profile, []byte, error) {
	raw, err := readRegularFile(path, maxAssetBytes)
	if err != nil {
		return codeedge.Profile{}, nil, fmt.Errorf("read preflight profile: %w", err)
	}
	var locked stageprovider.CodeEdgePhase1PreflightProfileLock
	if err := json.Unmarshal(raw, &locked); err != nil {
		return codeedge.Profile{}, nil, fmt.Errorf("parse CodeEdge Phase-1 preflight profile: %w", err)
	}
	profile, err := locked.PreflightProfile()
	if err != nil {
		return codeedge.Profile{}, nil, err
	}
	if !sameStringsAsSet(profile.ProtectedEnvironmentVariables, []string{
		"ANTHROPIC_AUTH_TOKEN",
		"ANTHROPIC_BASE_URL",
		"OPUS_HARBOR_BASE_URL",
		"QWEN_HARBOR_BASE_URL",
	}) {
		return codeedge.Profile{}, nil, errors.New("preflight profile must protect exactly the approved model credential and endpoint environment names")
	}
	canonical, err := json.Marshal(locked)
	if err != nil {
		return codeedge.Profile{}, nil, fmt.Errorf("canonicalize CodeEdge Phase-1 preflight profile: %w", err)
	}
	return profile, canonical, nil
}

func readParentFinalCompliancePolicy(path string) (codeedge.FinalCompliancePolicy, []byte, error) {
	raw, err := readRegularFile(path, maxAssetBytes)
	if err != nil {
		return codeedge.FinalCompliancePolicy{}, nil, fmt.Errorf("read final compliance policy: %w", err)
	}
	var locked stageprovider.CodeEdgePhase1FinalCompliancePolicyLock
	if err := json.Unmarshal(raw, &locked); err != nil {
		return codeedge.FinalCompliancePolicy{}, nil, fmt.Errorf("parse CodeEdge Phase-1 final compliance policy: %w", err)
	}
	policy, err := locked.FinalCompliancePolicy()
	if err != nil {
		return codeedge.FinalCompliancePolicy{}, nil, err
	}
	if policy.QwenPolicy.Evaluator.AgentName != "claude-code" || policy.QwenPolicy.Evaluator.AgentVersion != "2.1.207" || policy.QwenPolicy.Evaluator.ModelName != "qwen3.7-max" ||
		policy.OpusPolicy.Evaluator.AgentName != "claude-code" || policy.OpusPolicy.Evaluator.AgentVersion != "2.1.207" || policy.OpusPolicy.Evaluator.ModelName != "claude-opus-4-6" {
		return codeedge.FinalCompliancePolicy{}, nil, errors.New("final compliance policy must bind the approved Qwen and Opus evaluator identities")
	}
	if policy.QwenPolicy.PassRewardKey != "reward" || policy.OpusPolicy.PassRewardKey != "reward" ||
		policy.SubmissionCheckerID != stageprovider.CodeEdgePhase1SubmissionLintHandlerID ||
		policy.SubmissionCheckerVersion != "1.0.0" ||
		policy.SubmissionReportSchemaVersion != workflowadapter.CodeEdgeSubmissionReportSchemaVersion {
		return codeedge.FinalCompliancePolicy{}, nil, errors.New("final compliance policy does not match the approved parent submission contract")
	}
	canonical, err := policy.CanonicalJSON()
	if err != nil {
		return codeedge.FinalCompliancePolicy{}, nil, fmt.Errorf("canonicalize CodeEdge Phase-1 final compliance policy: %w", err)
	}
	return policy, canonical, nil
}

func lockParentOperations(registrations []stageprovider.DeploymentOperationRegistration, docker stageprovider.LocalExecutableLock, profile, preflight, policy []byte) ([]stageprovider.DeploymentOperationCatalogLockRecord, error) {
	expected := make(map[workflowkit.StageKey]struct{}, len(workflowadapter.CodeEdgePhase1StageOrder()))
	for _, key := range workflowadapter.CodeEdgePhase1StageOrder() {
		expected[key] = struct{}{}
	}
	if len(registrations) != len(expected) {
		return nil, fmt.Errorf("parent catalog has %d operations, want %d", len(registrations), len(expected))
	}
	operations := make([]stageprovider.DeploymentOperationCatalogLockRecord, 0, len(registrations))
	seen := make(map[workflowkit.StageKey]struct{}, len(registrations))
	for _, registration := range registrations {
		if _, found := expected[registration.Stage.Key]; !found {
			return nil, fmt.Errorf("parent catalog contains unexpected stage %q", registration.Stage.Key)
		}
		if _, duplicate := seen[registration.Stage.Key]; duplicate {
			return nil, fmt.Errorf("parent catalog duplicates stage %q", registration.Stage.Key)
		}
		seen[registration.Stage.Key] = struct{}{}

		prompt, schema, err := parentOperationContractFingerprints(registration, profile, preflight, policy)
		if err != nil {
			return nil, fmt.Errorf("stage %q contract fingerprints: %w", registration.Stage.Key, err)
		}
		record := stageprovider.DeploymentOperationCatalogLockRecord{
			Stage:                    registration.Stage,
			Provider:                 registration.Provider,
			Operation:                registration.Operation.Clone(),
			Runtime:                  registration.Runtime,
			Checkout:                 registration.Checkout,
			Secrets:                  cloneParentSecrets(registration.Secrets),
			PromptContentFingerprint: prompt,
			SchemaContentFingerprint: schema,
			ExecutionKind:            registration.Operation.Payload.Kind(),
		}
		switch payload := registration.Operation.Payload.(type) {
		case workflowadapter.LocalCommandOperationPayload:
			if !isApprovedParentLocalCommand(registration.Stage.Key, payload) {
				return nil, fmt.Errorf("stage %q has an unapproved local command", registration.Stage.Key)
			}
			local := docker
			local.CommandID = payload.CommandID
			record.LocalExecutable = &local
		case workflowadapter.HarborBuiltinOperationPayload:
			if !isApprovedParentBuiltin(registration.Stage.Key, payload.HandlerID) {
				return nil, fmt.Errorf("stage %q has an unapproved Harbor builtin", registration.Stage.Key)
			}
			record.HarborFlowBuiltin = &stageprovider.HarborFlowBuiltinOperationLock{
				Format: stageprovider.HarborFlowBuiltinOperationLockFormat, Version: stageprovider.HarborFlowBuiltinOperationLockVersion,
				HandlerID: payload.HandlerID, HandlerVersion: "1.0.0",
			}
		case workflowadapter.DurableReviewOperationPayload:
			if !isApprovedParentReview(registration.Stage.Key, payload.PolicyID) {
				return nil, fmt.Errorf("stage %q has an unapproved durable review policy", registration.Stage.Key)
			}
			record.DurableReviewPolicy = &stageprovider.DurableReviewPolicyLock{PolicyID: payload.PolicyID, Version: "1.0.0"}
		default:
			return nil, fmt.Errorf("stage %q uses unsupported parent payload %T", registration.Stage.Key, payload)
		}
		operations = append(operations, record)
	}
	for key := range expected {
		if _, found := seen[key]; !found {
			return nil, fmt.Errorf("parent catalog omits required stage %q", key)
		}
	}
	sort.Slice(operations, func(left, right int) bool { return operations[left].Stage.Key < operations[right].Stage.Key })
	return operations, nil
}

// cloneParentSecrets preserves the catalog's explicit empty-array contract.
// A nil slice would encode as JSON null and is rejected by the deployment
// lock parser, because an absent secret allow-list must never mean allow-all.
func cloneParentSecrets(values []workflowadapter.SecretReference) []workflowadapter.SecretReference {
	cloned := make([]workflowadapter.SecretReference, len(values))
	copy(cloned, values)
	return cloned
}

// parentOperationContractFingerprints binds the current generic lock fields
// to the parent operation's closed source policy. Parent stages do not carry
// external prompt/schema files: their implementation is the linked Harbor
// Flow build, while the catalog, profile, preflight policy and final policy
// are the reviewed operation template/schema facts. Keeping them in the two
// existing fingerprint fields prevents an empty placeholder from weakening a
// record before a future lock-format revision introduces parent-specific fields.
func parentOperationContractFingerprints(registration stageprovider.DeploymentOperationRegistration, profile, preflight, policy []byte) (workflowkit.Fingerprint, workflowkit.Fingerprint, error) {
	operation, err := json.Marshal(registration)
	if err != nil {
		return "", "", err
	}
	parentPolicy, err := json.Marshal(struct {
		Format    string               `json:"format"`
		StageKey  workflowkit.StageKey `json:"stage_key"`
		Profile   json.RawMessage      `json:"execution_profile"`
		Preflight json.RawMessage      `json:"preflight_profile"`
		Policy    json.RawMessage      `json:"final_compliance_policy"`
	}{
		Format: "harbor.codeedge-phase1.parent-operation-contract.v1", StageKey: registration.Stage.Key,
		Profile: append(json.RawMessage(nil), profile...), Preflight: append(json.RawMessage(nil), preflight...), Policy: append(json.RawMessage(nil), policy...),
	})
	if err != nil {
		return "", "", err
	}
	return workflowkit.SHA256Fingerprint(operation), workflowkit.SHA256Fingerprint(parentPolicy), nil
}

func isApprovedParentLocalCommand(stage workflowkit.StageKey, payload workflowadapter.LocalCommandOperationPayload) bool {
	if len(payload.Arguments) != 0 {
		return false
	}
	return (stage == workflowkit.StageKey(workflowadapter.DockerBuild) && payload.CommandID == stageprovider.CodeEdgePhase1DockerBuildCommandID) ||
		(stage == workflowkit.StageKey(workflowadapter.InitialVerify) && payload.CommandID == stageprovider.CodeEdgePhase1InitialVerifyCommandID) ||
		(stage == workflowkit.StageKey(workflowadapter.OracleVerify) && payload.CommandID == stageprovider.CodeEdgePhase1OracleVerifyCommandID)
}

func isApprovedParentBuiltin(stage workflowkit.StageKey, handlerID string) bool {
	expected := map[workflowkit.StageKey]string{
		workflowkit.StageKey(workflowadapter.RepoPrepare):     stageprovider.CodeEdgePhase1TaskLayoutPreflightHandlerID,
		workflowkit.StageKey(workflowadapter.RepoAnalyze):     stageprovider.CodeEdgePhase1RepoProvenancePreflightHandlerID,
		workflowkit.StageKey(workflowadapter.CodeEdgeLint):    stageprovider.CodeEdgePhase1EnvironmentIsolationPreflightHandlerID,
		workflowkit.StageKey(workflowadapter.TestsAnalysis):   stageprovider.CodeEdgePhase1TestsAnalysisValidateHandlerID,
		workflowkit.StageKey(workflowadapter.QualityCheck):    stageprovider.CodeEdgePhase1QualityCheckHandlerID,
		workflowkit.StageKey(workflowadapter.SimilarityCheck): stageprovider.CodeEdgePhase1SimilarityCheckHandlerID,
		workflowkit.StageKey(workflowadapter.SubmissionLint):  stageprovider.CodeEdgePhase1SubmissionLintHandlerID,
		workflowkit.StageKey(workflowadapter.Package):         stageprovider.CodeEdgePhase1LocalPackageHandlerID,
	}
	return expected[stage] == handlerID && stageprovider.IsCodeEdgePhase1BuiltinHandlerID(handlerID)
}

func isApprovedParentReview(stage workflowkit.StageKey, policyID string) bool {
	expected := map[workflowkit.StageKey]string{
		workflowkit.StageKey(workflowadapter.SolutionReview):           "codeedge-phase1.solution-review.v1",
		workflowkit.StageKey(workflowadapter.FinalReview):              "codeedge-phase1.final-review.v1",
		workflowkit.StageKey(workflowadapter.EvaluatorEvidenceHandoff): "codeedge-phase1.evaluator-evidence.v1",
		workflowkit.StageKey(workflowadapter.ResultReview):             "codeedge-phase1.result-review.v1",
	}
	return expected[stage] == policyID
}

func discoverDockerLock(path string) (stageprovider.LocalExecutableLock, error) {
	content, err := fingerprintExecutable(path)
	if err != nil {
		return stageprovider.LocalExecutableLock{}, fmt.Errorf("Docker executable: %w", err)
	}
	output, err := probe(path, controlledProbeEnvironment(), "--version")
	if err != nil || !strings.HasPrefix(output, "Docker version ") {
		return stageprovider.LocalExecutableLock{}, errors.New("locked Docker --version probe failed")
	}
	version := strings.TrimPrefix(output, "Docker version ")
	if separator := strings.Index(version, ","); separator >= 0 {
		version = version[:separator]
	}
	version = strings.TrimSpace(version)
	if err := validateVersionedText("Docker version", version); err != nil {
		return stageprovider.LocalExecutableLock{}, err
	}
	return stageprovider.LocalExecutableLock{AbsolutePath: path, Version: version, ContentSHA256: content}, nil
}

func requireGitTopLevel(root, gitPath string) error {
	topLevel, err := probeAt(root, gitPath, controlledProbeEnvironment(), "rev-parse", "--show-toplevel")
	if err != nil || topLevel != root {
		return errors.New("source root must be the clean Git worktree top level")
	}
	return nil
}

func requireCleanGitWorktree(root, gitPath string) error {
	status, err := runAt(root, gitPath, controlledProbeEnvironment(), "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil || len(status) != 0 {
		return errors.New("a clean committed Git worktree is required before generating the CodeEdge Phase-1 parent lock")
	}
	return nil
}

func sourceBuildIdentity(root, gitPath string) (string, workflowkit.Fingerprint, error) {
	commit, err := probeAt(root, gitPath, controlledProbeEnvironment(), "rev-parse", "HEAD")
	if err != nil || len(commit) != 40 || strings.ToLower(commit) != commit || !isLowerHex(commit) {
		return "", "", errors.New("resolve source Git commit")
	}
	tree, err := runAtWithLimit(root, gitPath, controlledProbeEnvironment(), maxSourceTreeBytes, "ls-tree", "-r", "--full-tree", commit)
	if err != nil {
		return "", "", errors.New("read source Git tree")
	}
	excluded := make(map[string]struct{}, len(generatedLockRelatives))
	for _, relative := range generatedLockRelatives {
		excluded[relative] = struct{}{}
	}
	var manifest bytes.Buffer
	for _, line := range bytes.SplitAfter(tree, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		parts := bytes.SplitN(bytes.TrimSuffix(line, []byte("\n")), []byte("\t"), 2)
		if len(parts) != 2 {
			return "", "", errors.New("source Git tree contains an invalid entry")
		}
		if _, omit := excluded[string(parts[1])]; omit {
			continue
		}
		_, _ = manifest.Write(line)
	}
	return commit, workflowkit.SHA256Fingerprint(manifest.Bytes()), nil
}

func requireCommittedInputs(root, gitPath, commit string, paths []string) error {
	for _, relative := range paths {
		output, err := runAt(root, gitPath, controlledProbeEnvironment(), "ls-tree", "-r", "--name-only", commit, "--", relative)
		if err != nil || string(output) != relative+"\n" {
			return fmt.Errorf("required parent source asset is not committed at source revision: %s", relative)
		}
	}
	return nil
}

func sameStringsAsSet(values, want []string) bool {
	if len(values) != len(want) {
		return false
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, duplicate := seen[value]; duplicate {
			return false
		}
		seen[value] = struct{}{}
	}
	for _, value := range want {
		if _, found := seen[value]; !found {
			return false
		}
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

func cleanAbsolutePath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value || value == string(filepath.Separator) {
		return "", errors.New("path must be a clean non-root absolute path")
	}
	return value, nil
}

func pathWithin(root, value string) bool {
	relative, err := filepath.Rel(root, value)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func requireNonSymlinkDirectory(path string) error {
	info, err := inspectNoSymlinkPath(path)
	if err != nil || !info.IsDir() {
		return errors.New("must be an existing non-symlink directory")
	}
	return nil
}

func requireExecutableRegularFile(path string) error {
	info, err := inspectNoSymlinkPath(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return errors.New("must be an executable regular file with no symlink path component")
	}
	return nil
}

func fingerprintExecutable(path string) (workflowkit.Fingerprint, error) {
	if err := requireExecutableRegularFile(path); err != nil {
		return "", err
	}
	contents, err := readRegularFile(path, -1)
	if err != nil {
		return "", err
	}
	return workflowkit.SHA256Fingerprint(contents), nil
}

func readRegularFile(path string, limit int64) ([]byte, error) {
	initial, err := inspectNoSymlinkPath(path)
	if err != nil || !initial.Mode().IsRegular() {
		return nil, errors.New("must be a regular file with no symlink path component")
	}
	if limit >= 0 && initial.Size() > limit {
		return nil, errors.New("regular file exceeds read limit")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("open regular file")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(initial, opened) {
		return nil, errors.New("regular file changed while opening")
	}
	reader := io.Reader(file)
	if limit >= 0 {
		reader = io.LimitReader(file, limit+1)
	}
	contents, err := io.ReadAll(reader)
	if err != nil || (limit >= 0 && int64(len(contents)) > limit) {
		return nil, errors.New("read regular file")
	}
	final, err := file.Stat()
	pathInfo, pathErr := inspectNoSymlinkPath(path)
	if err != nil || pathErr != nil || !final.Mode().IsRegular() || !pathInfo.Mode().IsRegular() || !os.SameFile(opened, final) || !os.SameFile(opened, pathInfo) || final.Size() != opened.Size() || pathInfo.Size() != opened.Size() {
		return nil, errors.New("regular file changed while reading")
	}
	return contents, nil
}

func inspectNoSymlinkPath(path string) (os.FileInfo, error) {
	components := make([]string, 0, 8)
	for current := path; ; {
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

func controlledProbeEnvironment() []string {
	return []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LC_ALL=C", "LANG=C"}
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
	probeContext, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	process := exec.CommandContext(probeContext, command, arguments...)
	process.Dir = directory
	process.Env = append([]string(nil), environment...)
	var stdout limitedBuffer
	stdout.limit = limit
	process.Stdout = &stdout
	process.Stderr = io.Discard
	if err := process.Run(); err != nil || stdout.exceeded || probeContext.Err() != nil {
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

func writeNewRegularFile(path string, value []byte) error {
	parent := filepath.Dir(path)
	if err := requireNonSymlinkDirectory(parent); err != nil {
		return fmt.Errorf("output parent: %w", err)
	}
	if _, err := os.Lstat(path); err == nil || !errors.Is(err, os.ErrNotExist) {
		return errors.New("refuse to replace an existing output lock")
	}
	temporary, err := os.CreateTemp(parent, ".codeedge-phase1-lock-*")
	if err != nil {
		return errors.New("create lock staging file")
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o644); err != nil {
		_ = temporary.Close()
		return errors.New("set lock staging permissions")
	}
	if _, err := temporary.Write(value); err != nil || temporary.Sync() != nil || temporary.Close() != nil {
		return errors.New("write lock staging file")
	}
	if err := os.Link(temporaryPath, path); err != nil {
		return errors.New("publish lock without replacing an existing file")
	}
	return nil
}

func isLowerHex(value string) bool {
	for _, character := range value {
		if (character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') {
			continue
		}
		return false
	}
	return true
}

func fail(message string) {
	fmt.Fprintln(os.Stderr, "codeedge-phase1-lock-build:", message)
	os.Exit(1)
}
