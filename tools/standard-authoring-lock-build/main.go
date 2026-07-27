// Command standard-authoring-lock-build generates the one immutable Standard
// authoring deployment lock from a clean Git snapshot, the source-controlled
// catalog/asset manifest, and explicit local runtime identities. It never
// reads model credentials, endpoints, or other secret environment values.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	modulePath                 = "github.com/purplevoid/harbor-factory"
	maxAssetBytes        int64 = 4 * 1024 * 1024
	maxProbeBytes              = 64 * 1024
	maxSourceTreeBytes         = 64 * 1024 * 1024
	probeTimeout               = 30 * time.Second
	standardLockRelative       = "deployments/standard-authoring/operation-catalog.lock.json"
)

// generatedProductionLocks are omitted from the source manifest signed by
// every deployment lock. Including even one lock would create a hash cycle;
// excluding only the lock currently being generated would make the three
// independent bundles sign different source identities.
var generatedProductionLocks = map[string]struct{}{
	"deployments/standard-authoring/operation-catalog.lock.json":       {},
	"deployments/codeedge-phase1/operation-catalog.lock.json":          {},
	"deployments/codeedge-evaluator-child/operation-catalog.lock.json": {},
}

type buildConfig struct {
	sourceRoot        string
	catalogPath       string
	manifestPath      string
	profilePath       string
	contractRoot      string
	outputPath        string
	buildVersion      string
	lockID            string
	lockVersion       string
	gitExecutable     string
	sshExecutable     string
	sshWrapperShell   string
	sshKnownHosts     string
	codexNode         string
	codexLauncher     string
	codexHome         string
	codexModelVersion string
}

func main() {
	var config buildConfig
	flag.StringVar(&config.sourceRoot, "source-root", "", "clean Git worktree root")
	flag.StringVar(&config.catalogPath, "catalog", "", "Standard authoring operation catalog")
	flag.StringVar(&config.manifestPath, "asset-manifest", "", "Standard authoring contract asset manifest")
	flag.StringVar(&config.profilePath, "execution-profile", "", "source-controlled complete Standard authoring execution profile")
	flag.StringVar(&config.contractRoot, "contract-root", "", "directory containing prompt/schema assets")
	flag.StringVar(&config.outputPath, "output", "", "new operation catalog lock output path")
	flag.StringVar(&config.buildVersion, "build-version", "", "immutable Harbor Flow build version")
	flag.StringVar(&config.lockID, "lock-id", "standard-authoring-production-lock", "immutable deployment lock id")
	flag.StringVar(&config.lockVersion, "lock-version", "", "immutable deployment lock version")
	flag.StringVar(&config.gitExecutable, "git-executable", "", "absolute locked Git executable")
	flag.StringVar(&config.sshExecutable, "ssh-executable", "", "absolute locked OpenSSH executable for source capture")
	flag.StringVar(&config.sshWrapperShell, "ssh-wrapper-shell", "", "absolute locked POSIX shell for the generated SSH wrapper")
	flag.StringVar(&config.sshKnownHosts, "ssh-known-hosts", "", "lock-bound deployment-relative OpenSSH known_hosts asset")
	flag.StringVar(&config.codexNode, "codex-node", "", "absolute locked Node executable")
	flag.StringVar(&config.codexLauncher, "codex-launcher", "", "absolute locked Codex JavaScript launcher")
	flag.StringVar(&config.codexHome, "codex-home", "", "absolute controlled CODEX_HOME directory")
	flag.StringVar(&config.codexModelVersion, "codex-model-version", "", "approved immutable gpt-5.6-terra model version")
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
	if !workflowadapter.IsStandardAuthoringWorkflowTemplate(catalog.Template()) {
		return stageprovider.DeploymentOperationCatalogLock{}, fmt.Errorf("catalog must bind an installed Standard authoring template")
	}
	profile, err := readStandardAuthoringExecutionProfile(config.profilePath)
	if err != nil {
		return stageprovider.DeploymentOperationCatalogLock{}, err
	}
	manifestRaw, err := readRegularFile(config.manifestPath, maxAssetBytes)
	if err != nil {
		return stageprovider.DeploymentOperationCatalogLock{}, fmt.Errorf("read asset manifest: %w", err)
	}
	manifest, err := stageprovider.ParseStandardAuthoringContractAssetManifestJSON(manifestRaw)
	if err != nil {
		return stageprovider.DeploymentOperationCatalogLock{}, fmt.Errorf("parse asset manifest: %w", err)
	}
	if err := validateStandardAuthoringTemplateBundle(catalog.Template(), profile.Template, manifest.Template); err != nil {
		return stageprovider.DeploymentOperationCatalogLock{}, err
	}
	assets := make(map[workflowkit.StageKey]stageprovider.StandardAuthoringContractAssetManifestEntry, len(manifest.Operations))
	for _, entry := range manifest.Operations {
		assets[entry.StageKey] = entry.Clone()
	}

	gitLock, err := discoverGitLock(config.gitExecutable)
	if err != nil {
		return stageprovider.DeploymentOperationCatalogLock{}, err
	}
	sshTransport, err := discoverStandardAuthoringSSHTransport(config)
	if err != nil {
		return stageprovider.DeploymentOperationCatalogLock{}, err
	}
	codex, err := discoverCodexLock(config)
	if err != nil {
		return stageprovider.DeploymentOperationCatalogLock{}, err
	}
	commit, manifestFingerprint, err := sourceBuildIdentity(config.sourceRoot, config.gitExecutable)
	if err != nil {
		return stageprovider.DeploymentOperationCatalogLock{}, err
	}
	build := stageprovider.HarborFlowBuildIdentity{Module: modulePath, Version: config.buildVersion, Commit: commit, ContentSHA256: manifestFingerprint}

	registrations := catalog.Catalog().Operations
	operations := make([]stageprovider.DeploymentOperationCatalogLockRecord, 0, len(registrations))
	for _, registration := range registrations {
		entry, present := assets[registration.Stage.Key]
		if !present {
			return stageprovider.DeploymentOperationCatalogLock{}, fmt.Errorf("catalog stage %q has no asset manifest entry", registration.Stage.Key)
		}
		prompt, err := fingerprintContractAsset(config.contractRoot, entry.Prompt)
		if err != nil {
			return stageprovider.DeploymentOperationCatalogLock{}, fmt.Errorf("stage %q prompt asset: %w", registration.Stage.Key, err)
		}
		schema, err := fingerprintContractAsset(config.contractRoot, entry.Schema)
		if err != nil {
			return stageprovider.DeploymentOperationCatalogLock{}, fmt.Errorf("stage %q schema asset: %w", registration.Stage.Key, err)
		}
		secrets := make([]workflowadapter.SecretReference, len(registration.Secrets))
		copy(secrets, registration.Secrets)
		record := stageprovider.DeploymentOperationCatalogLockRecord{
			Stage:                    registration.Stage,
			Provider:                 registration.Provider,
			Operation:                registration.Operation.Clone(),
			Runtime:                  registration.Runtime,
			Checkout:                 registration.Checkout,
			Secrets:                  secrets,
			PromptContentFingerprint: prompt,
			SchemaContentFingerprint: schema,
			ExecutionKind:            registration.Operation.Payload.Kind(),
			StandardAuthoringContract: &stageprovider.StandardAuthoringContractLock{
				Format: stageprovider.StandardAuthoringContractLockFormat, Version: stageprovider.StandardAuthoringContractLockVersion,
				Prompt: entry.Prompt, Schema: entry.Schema,
			},
		}
		switch payload := registration.Operation.Payload.(type) {
		case workflowadapter.LocalCommandOperationPayload:
			if payload.CommandID != stageprovider.StandardAuthoringGitSnapshotCommandID || len(payload.Arguments) != 0 {
				return stageprovider.DeploymentOperationCatalogLock{}, fmt.Errorf("stage %q has an unapproved local command", registration.Stage.Key)
			}
			local := gitLock
			record.LocalExecutable = &local
		case workflowadapter.AgentTurnOperationPayload:
			if !stageprovider.IsCodexAppServerProductionPayload(payload) {
				return stageprovider.DeploymentOperationCatalogLock{}, fmt.Errorf("stage %q must pin the approved Codex agent profile", registration.Stage.Key)
			}
			if err := validateCodexStageAssets(config.contractRoot, catalog.Template(), registration.Stage.Key, entry, payload); err != nil {
				return stageprovider.DeploymentOperationCatalogLock{}, fmt.Errorf("stage %q Codex assets: %w", registration.Stage.Key, err)
			}
			record.AgentModel = &stageprovider.AgentModelLock{
				AgentID: payload.AgentID, AgentVersion: codex.JavaScriptLauncher.Version,
				ModelID: payload.ModelID, ModelVersion: config.codexModelVersion,
			}
			copyCodex := codex
			record.CodexAppServer = &copyCodex
		case workflowadapter.DurableReviewOperationPayload:
			record.DurableReviewPolicy = &stageprovider.DurableReviewPolicyLock{PolicyID: payload.PolicyID, Version: "1.0.0"}
		case workflowadapter.HarborBuiltinOperationPayload:
			record.HarborFlowBuiltin = &stageprovider.HarborFlowBuiltinOperationLock{Format: stageprovider.HarborFlowBuiltinOperationLockFormat, Version: stageprovider.HarborFlowBuiltinOperationLockVersion, HandlerID: payload.HandlerID, HandlerVersion: "1.0.0"}
		default:
			return stageprovider.DeploymentOperationCatalogLock{}, fmt.Errorf("stage %q has unsupported payload %T", registration.Stage.Key, payload)
		}
		operations = append(operations, record)
	}
	lock := stageprovider.DeploymentOperationCatalogLock{
		Format: stageprovider.DeploymentOperationCatalogLockFormat, Version: stageprovider.DeploymentOperationCatalogLockVersion,
		LockID: config.lockID, LockVersion: config.lockVersion, CatalogReceipt: catalog.Receipt(), HarborFlowBuild: build,
		StandardAuthoringExecutionProfile: &stageprovider.StandardAuthoringExecutionProfileLock{Profile: profile},
		StandardAuthoringSSHTransport:     &sshTransport,
		Operations:                        operations,
	}
	if _, err := stageprovider.NewDeploymentOperationCatalogLockResolver(catalog, lock); err != nil {
		return stageprovider.DeploymentOperationCatalogLock{}, fmt.Errorf("validate generated catalog lock: %w", err)
	}
	return lock, nil
}

func validateStandardAuthoringTemplateBundle(catalog, profile, manifest workflowadapter.TemplateReference) error {
	if !profile.Equal(catalog) {
		return fmt.Errorf("execution profile template %s@%s does not match catalog template %s@%s", profile.ID, profile.Version, catalog.ID, catalog.Version)
	}
	if !manifest.Equal(catalog) {
		return fmt.Errorf("asset manifest template %s@%s does not match catalog template %s@%s", manifest.ID, manifest.Version, catalog.ID, catalog.Version)
	}
	return nil
}

func readStandardAuthoringExecutionProfile(path string) (workflowadapter.ExecutionProfile, error) {
	raw, err := readRegularFile(path, maxAssetBytes)
	if err != nil {
		return workflowadapter.ExecutionProfile{}, fmt.Errorf("read execution profile: %w", err)
	}
	profile, err := workflowadapter.ParseExecutionProfileJSON(raw)
	if err != nil {
		return workflowadapter.ExecutionProfile{}, fmt.Errorf("parse Standard authoring execution profile: %w", err)
	}
	if !workflowadapter.IsStandardAuthoringWorkflowTemplate(profile.Template) {
		return workflowadapter.ExecutionProfile{}, fmt.Errorf("execution profile must bind an installed Standard authoring template")
	}
	return profile.Clone(), nil
}

func validateConfig(config *buildConfig) error {
	if config == nil {
		return errors.New("build configuration is required")
	}
	var err error
	for _, field := range []*string{&config.sourceRoot, &config.catalogPath, &config.manifestPath, &config.profilePath, &config.contractRoot, &config.outputPath, &config.gitExecutable, &config.sshExecutable, &config.sshWrapperShell, &config.sshKnownHosts, &config.codexNode, &config.codexLauncher, &config.codexHome} {
		*field, err = cleanAbsolutePath(*field)
		if err != nil {
			return err
		}
	}
	for label, value := range map[string]string{"build version": config.buildVersion, "lock id": config.lockID, "lock version": config.lockVersion, "Codex model version": config.codexModelVersion} {
		if strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) || strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("%s is required and must be canonical", label)
		}
	}
	if filepath.ToSlash(filepath.Clean(filepath.Join(config.sourceRoot, standardLockRelative))) != filepath.ToSlash(config.outputPath) {
		return fmt.Errorf("output must be %s below source root", standardLockRelative)
	}
	if _, err := os.Lstat(config.outputPath); err == nil || !errors.Is(err, os.ErrNotExist) {
		return errors.New("output lock must not already exist")
	}
	for label, path := range map[string]string{"catalog": config.catalogPath, "asset manifest": config.manifestPath, "execution profile": config.profilePath} {
		if !pathWithin(config.sourceRoot, path) {
			return fmt.Errorf("%s must be below source root", label)
		}
	}
	if !pathWithin(config.sourceRoot, config.contractRoot) {
		return errors.New("contract root must be below source root")
	}
	if err := requireNonSymlinkDirectory(config.contractRoot); err != nil {
		return fmt.Errorf("contract root: %w", err)
	}
	if err := requireNonSymlinkDirectory(config.codexHome); err != nil {
		return fmt.Errorf("Codex home: %w", err)
	}
	expectedKnownHosts := filepath.Join(config.contractRoot, filepath.FromSlash(stageprovider.StandardAuthoringSSHKnownHostsRelativePath))
	if config.sshKnownHosts != expectedKnownHosts {
		return fmt.Errorf("SSH known_hosts must be %s below contract root", stageprovider.StandardAuthoringSSHKnownHostsRelativePath)
	}
	return nil
}

func discoverGitLock(path string) (stageprovider.LocalExecutableLock, error) {
	content, err := fingerprintRegularFile(path)
	if err != nil {
		return stageprovider.LocalExecutableLock{}, fmt.Errorf("Git executable: %w", err)
	}
	output, err := probe(path, []string{"PATH=/usr/local/bin:/usr/bin:/bin"}, "--version")
	if err != nil || !strings.HasPrefix(output, "git version ") {
		return stageprovider.LocalExecutableLock{}, errors.New("locked Git --version probe failed")
	}
	version := strings.TrimPrefix(output, "git version ")
	if version == "" || strings.ContainsAny(version, " \t\r\n") {
		return stageprovider.LocalExecutableLock{}, errors.New("locked Git version is invalid")
	}
	return stageprovider.LocalExecutableLock{CommandID: stageprovider.StandardAuthoringGitSnapshotCommandID, AbsolutePath: path, Version: version, ContentSHA256: content}, nil
}

func discoverStandardAuthoringSSHTransport(config buildConfig) (stageprovider.StandardAuthoringSSHTransportLock, error) {
	sshContent, err := fingerprintRegularFile(config.sshExecutable)
	if err != nil {
		return stageprovider.StandardAuthoringSSHTransportLock{}, fmt.Errorf("SSH executable: %w", err)
	}
	sshVersion, err := probeSSHVersion(config.sshExecutable)
	if err != nil {
		return stageprovider.StandardAuthoringSSHTransportLock{}, errors.New("locked SSH -V probe failed")
	}
	shellContent, err := fingerprintRegularFile(config.sshWrapperShell)
	if err != nil {
		return stageprovider.StandardAuthoringSSHTransportLock{}, fmt.Errorf("SSH wrapper shell: %w", err)
	}
	if strings.ContainsAny(config.sshWrapperShell, " \t\r\n") {
		return stageprovider.StandardAuthoringSSHTransportLock{}, errors.New("SSH wrapper shell path is not shebang-safe")
	}
	knownHosts, err := readRegularFile(config.sshKnownHosts, stageprovider.StandardAuthoringSSHKnownHostsMaxBytes)
	if err != nil {
		return stageprovider.StandardAuthoringSSHTransportLock{}, fmt.Errorf("SSH known_hosts: %w", err)
	}
	if err := stageprovider.ValidateStandardAuthoringSSHKnownHostsAsset(knownHosts); err != nil {
		return stageprovider.StandardAuthoringSSHTransportLock{}, fmt.Errorf("SSH known_hosts: %w", err)
	}
	transport := stageprovider.StandardAuthoringSSHTransportLock{
		Format:  stageprovider.StandardAuthoringSSHTransportLockFormat,
		Version: stageprovider.StandardAuthoringSSHTransportLockVersion,
		SSHExecutable: stageprovider.LocalExecutableLock{
			CommandID: stageprovider.StandardAuthoringSSHTransportCommandID, AbsolutePath: config.sshExecutable,
			Version: sshVersion, ContentSHA256: sshContent,
		},
		WrapperShell: stageprovider.LocalExecutableLock{
			CommandID: stageprovider.StandardAuthoringSSHWrapperShellCommandID, AbsolutePath: config.sshWrapperShell,
			Version: string(shellContent), ContentSHA256: shellContent,
		},
		KnownHosts: stageprovider.StandardAuthoringSSHKnownHostsLock{
			Format: stageprovider.StandardAuthoringSSHKnownHostsLockFormat, Version: stageprovider.StandardAuthoringSSHKnownHostsLockVersion,
			RelativePath: stageprovider.StandardAuthoringSSHKnownHostsRelativePath, ContentSHA256: workflowkit.SHA256Fingerprint(knownHosts),
		},
		AgentSocketEnvironmentName: stageprovider.StandardAuthoringSSHAgentSocketEnvironment,
	}
	if err := transport.Validate(); err != nil {
		return stageprovider.StandardAuthoringSSHTransportLock{}, err
	}
	return transport, nil
}

func probeSSHVersion(command string) (string, error) {
	probeContext, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	var stdout, stderr limitedBuffer
	stdout.limit = maxProbeBytes
	stderr.limit = maxProbeBytes
	process := exec.CommandContext(probeContext, command, "-V")
	process.Dir = filepath.Dir(command)
	process.Env = []string{"HOME=" + filepath.Dir(command), "PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C", "LC_ALL=C", "SSH_ASKPASS_REQUIRE=never"}
	process.Stdout = &stdout
	process.Stderr = &stderr
	if err := process.Run(); err != nil || stdout.exceeded || stderr.exceeded || probeContext.Err() != nil {
		return "", errors.New("controlled probe failed")
	}
	value := strings.TrimSpace(string(append(append([]byte(nil), stdout.buffer.Bytes()...), stderr.buffer.Bytes()...)))
	if value == "" || strings.ContainsAny(value, "\r\n\x00") {
		return "", errors.New("SSH version output is invalid")
	}
	fields := strings.Fields(value)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "OpenSSH_") {
		return "", errors.New("SSH version output is invalid")
	}
	return fields[0], nil
}

func discoverCodexLock(config buildConfig) (stageprovider.CodexAppServerOperationLock, error) {
	nodeHash, err := fingerprintRegularFile(config.codexNode)
	if err != nil {
		return stageprovider.CodexAppServerOperationLock{}, fmt.Errorf("Codex Node executable: %w", err)
	}
	launcherHash, err := fingerprintRegularFile(config.codexLauncher)
	if err != nil {
		return stageprovider.CodexAppServerOperationLock{}, fmt.Errorf("Codex JavaScript launcher: %w", err)
	}
	environment := []string{"CODEX_HOME=" + config.codexHome, "PATH=" + strings.Join([]string{filepath.Dir(config.codexNode), "/usr/local/bin", "/usr/bin", "/bin"}, string(os.PathListSeparator))}
	nodeVersion, err := probe(config.codexNode, environment, "--version")
	if err != nil || !strings.HasPrefix(nodeVersion, "v") {
		return stageprovider.CodexAppServerOperationLock{}, errors.New("locked Codex Node --version probe failed")
	}
	cliVersionOutput, err := probe(config.codexLauncher, environment, "--version")
	if err != nil || !strings.HasPrefix(cliVersionOutput, "codex-cli ") {
		return stageprovider.CodexAppServerOperationLock{}, errors.New("locked Codex CLI --version probe failed")
	}
	launcherVersion := strings.TrimPrefix(cliVersionOutput, "codex-cli ")
	if launcherVersion == "" || strings.ContainsAny(launcherVersion, " \t\r\n") {
		return stageprovider.CodexAppServerOperationLock{}, errors.New("locked Codex CLI version is invalid")
	}
	help, err := probeMultiline(config.codexLauncher, environment, "app-server", "--help")
	if err != nil || !strings.Contains(help, "--listen") || (!strings.Contains(help, "--config") && !strings.Contains(help, "-c,")) {
		return stageprovider.CodexAppServerOperationLock{}, errors.New("locked Codex CLI lacks app-server capability")
	}
	return stageprovider.CodexAppServerOperationLock{
		Format: stageprovider.CodexAppServerOperationLockFormatV3, Version: stageprovider.CodexAppServerOperationLockVersionV3,
		JavaScriptLauncher: stageprovider.LocalExecutableLock{CommandID: stageprovider.CodexAppServerJavaScriptLauncherCommandID, AbsolutePath: config.codexLauncher, Version: launcherVersion, ContentSHA256: launcherHash},
		NodeExecutable:     stageprovider.LocalExecutableLock{CommandID: stageprovider.CodexAppServerNodeExecutableCommandID, AbsolutePath: config.codexNode, Version: nodeVersion, ContentSHA256: nodeHash},
		CodexHomeDirectory: config.codexHome, CLIVersionOutput: cliVersionOutput,
		ApprovalPolicy: stageprovider.CodexAppServerApprovalPolicyNever,
		SandboxMode:    stageprovider.CodexAppServerSandboxModeWorkspaceWrite, SandboxPolicy: stageprovider.CodexAppServerSandboxPolicyWorkspaceWrite,
		NetworkAccess: false,
	}, nil
}

func validateCodexStageAssets(root string, template workflowadapter.TemplateReference, stageKey workflowkit.StageKey, entry stageprovider.StandardAuthoringContractAssetManifestEntry, payload workflowadapter.AgentTurnOperationPayload) error {
	promptPath, err := contractAssetPath(root, entry.Prompt.RelativePath)
	if err != nil {
		return err
	}
	prompt, err := readRegularFile(promptPath, maxAssetBytes)
	if err != nil {
		return err
	}
	program, err := stageprovider.ParseStandardAuthoringCodexTurnProgramAsset(prompt)
	if err != nil {
		return err
	}
	if program.ID != entry.Prompt.ID || program.Version != entry.Prompt.Version || len(program.TurnPrompts) != payload.MaxTurns || payload.AgentID != stageprovider.CodexAppServerProductionAgentID || payload.ModelID != stageprovider.CodexAppServerProductionModelID || payload.ReasoningEffort != stageprovider.CodexAppServerProductionReasoningEffort {
		return errors.New("Codex prompt program does not match its frozen stage operation")
	}
	schemaPath, err := contractAssetPath(root, entry.Schema.RelativePath)
	if err != nil {
		return err
	}
	schema, err := readRegularFile(schemaPath, maxAssetBytes)
	if err != nil {
		return err
	}
	return stageprovider.ValidateStandardAuthoringV3AgentOutputSchemaAsset(template, stageKey, schema)
}

func fingerprintContractAsset(root string, reference stageprovider.StandardAuthoringContractAssetReference) (workflowkit.Fingerprint, error) {
	path, err := contractAssetPath(root, reference.RelativePath)
	if err != nil {
		return "", err
	}
	contents, err := readRegularFile(path, maxAssetBytes)
	if err != nil {
		return "", err
	}
	return workflowkit.SHA256Fingerprint(contents), nil
}

func sourceBuildIdentity(root, gitPath string) (string, workflowkit.Fingerprint, error) {
	commit, err := probeAt(root, gitPath, []string{"PATH=/usr/local/bin:/usr/bin:/bin"}, "rev-parse", "HEAD")
	if err != nil || len(commit) != 40 || strings.ToLower(commit) != commit {
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
		if _, generatedLock := generatedProductionLocks[string(parts[1])]; !generatedLock {
			_, _ = manifest.Write(line)
		}
	}
	return commit, workflowkit.SHA256Fingerprint(manifest.Bytes()), nil
}

func requireCleanGitWorktree(root, gitPath string) error {
	status, err := runAt(root, gitPath, []string{"PATH=/usr/local/bin:/usr/bin:/bin"}, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil || len(status) != 0 {
		return errors.New("a clean committed Git worktree is required before generating the Standard authoring lock")
	}
	return nil
}

func cleanAbsolutePath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("required absolute path is missing")
	}
	abs, err := filepath.Abs(value)
	if err != nil || filepath.Clean(abs) != abs || abs == string(filepath.Separator) {
		return "", errors.New("path must be a clean non-root absolute path")
	}
	return abs, nil
}

func pathWithin(root, value string) bool {
	relative, err := filepath.Rel(root, value)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func contractAssetPath(root, relative string) (string, error) {
	if strings.TrimSpace(relative) == "" || filepath.IsAbs(relative) || strings.Contains(relative, "\\") {
		return "", errors.New("contract asset path is invalid")
	}
	path := filepath.Join(root, filepath.FromSlash(relative))
	if !pathWithin(root, path) || path == root {
		return "", errors.New("contract asset escapes contract root")
	}
	return path, nil
}

func requireNonSymlinkDirectory(path string) error {
	info, err := inspectNoSymlinkPath(path)
	if err != nil || !info.IsDir() {
		return errors.New("must be an existing non-symlink directory")
	}
	return nil
}

func fingerprintRegularFile(path string) (workflowkit.Fingerprint, error) {
	info, err := inspectNoSymlinkPath(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return "", errors.New("must be an executable regular file with no symlink path component")
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

func probe(command string, environment []string, arguments ...string) (string, error) {
	return probeAt(filepath.Dir(command), command, environment, arguments...)
}

// probeMultiline is restricted to static capability help. Version identities
// remain single-line probes above; app-server help is intentionally multi-line
// in current Codex releases and must not be mistaken for an invalid runtime.
func probeMultiline(command string, environment []string, arguments ...string) (string, error) {
	output, err := runAt(filepath.Dir(command), command, environment, arguments...)
	if err != nil {
		return "", err
	}
	value := string(output)
	if value == "" || strings.Contains(value, "\x00") {
		return "", errors.New("capability probe output is invalid")
	}
	return value, nil
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
	temporary, err := os.CreateTemp(parent, ".standard-authoring-lock-*")
	if err != nil {
		return errors.New("create lock staging file")
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o644); err != nil {
		temporary.Close()
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

func fail(message string) {
	fmt.Fprintln(os.Stderr, "standard-authoring-lock-build:", message)
	os.Exit(1)
}
