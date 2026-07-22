package stageprovider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	harborEvaluatorVersionOutputLimit = 4 * 1024
	harborEvaluatorShebangLimit       = 4 * 1024
	harborEvaluatorDockerServerFormat = `{{.Server.Version}}`
	harborEvaluatorComposePathFormat  = `{{range .ClientInfo.Plugins}}{{if eq .Name "compose"}}{{.Path}}{{end}}{{end}}`
	harborEvaluatorBuildxPathFormat   = `{{range .ClientInfo.Plugins}}{{if eq .Name "buildx"}}{{.Path}}{{end}}{{end}}`
)

// HarborEvaluatorRuntimeAttestorConfig supplies deployment-owned facts only.
// EnvironmentLookup exists for controlled composition and tests; its returned
// values are used solely to compare a fingerprint and never enter a receipt,
// error, log, catalog, lock, or invocation config.
type HarborEvaluatorRuntimeAttestorConfig struct {
	HarborFlowBuild   HarborFlowBuildIdentity
	EnvironmentLookup func(string) (string, bool)
}

// HarborEvaluatorRuntimeAttestor proves the local Harbor 0.18 installation
// before an evaluator provider can execute. Its bounded, credential-free
// probes cover Harbor, Docker, the exact Compose and Buildx plugins, and the
// local daemon version. It never starts a Harbor job, resolves a secret,
// builds child environment values, or contacts a model endpoint.
type HarborEvaluatorRuntimeAttestor struct {
	harborFlowBuild   HarborFlowBuildIdentity
	lookupEnvironment func(string) (string, bool)
}

// HarborEvaluatorInvocationPrelaunchAttestor is the executor-facing boundary
// that re-proves a frozen invocation in the exact isolated home used to launch
// Harbor.
type HarborEvaluatorInvocationPrelaunchAttestor interface {
	AttestHarborEvaluatorInvocationBeforeLaunch(context.Context, HarborEvaluatorInvocation, string) ([]string, error)
}

// HarborEvaluatorInvocation is the secret-free immutable input a controlled
// provider may use after it has passed runtime attestation. It intentionally
// carries only environment variable names, references, and fingerprints. A
// provider must resolve approved secret references privately at child-process
// launch and must never persist the resulting values.
type HarborEvaluatorInvocation struct {
	CommandID                      string                             `json:"command_id"`
	LauncherPath                   string                             `json:"launcher_path"`
	LauncherVersion                string                             `json:"launcher_version"`
	LauncherContentSHA256          workflowkit.Fingerprint            `json:"launcher_content_sha256"`
	ClaudeCodeExecutablePath       string                             `json:"claude_code_executable_path"`
	ClaudeCodeVersion              string                             `json:"claude_code_version"`
	ClaudeCodeContentSHA256        workflowkit.Fingerprint            `json:"claude_code_content_sha256"`
	PythonInterpreterPath          string                             `json:"python_interpreter_path"`
	PythonInterpreterVersion       string                             `json:"python_interpreter_version"`
	PythonInterpreterContentSHA256 workflowkit.Fingerprint            `json:"python_interpreter_content_sha256"`
	PythonSourceTreePath           string                             `json:"python_source_tree_path"`
	PythonSourceFilesSHA256        workflowkit.Fingerprint            `json:"python_source_files_sha256"`
	DockerCLIPath                  string                             `json:"docker_cli_path"`
	DockerCLIContentSHA256         workflowkit.Fingerprint            `json:"docker_cli_content_sha256"`
	DockerPATH                     string                             `json:"docker_path"`
	DockerVersion                  string                             `json:"docker_version"`
	DockerServerVersion            string                             `json:"docker_server_version"`
	DockerComposePluginPath        string                             `json:"docker_compose_plugin_path"`
	DockerComposeContentSHA256     workflowkit.Fingerprint            `json:"docker_compose_content_sha256"`
	DockerComposeVersion           string                             `json:"docker_compose_version"`
	DockerComposeVersionOutput     string                             `json:"docker_compose_version_output"`
	DockerBuildxPluginPath         string                             `json:"docker_buildx_plugin_path"`
	DockerBuildxContentSHA256      workflowkit.Fingerprint            `json:"docker_buildx_content_sha256"`
	DockerBuildxVersion            string                             `json:"docker_buildx_version"`
	DockerBuildxVersionOutput      string                             `json:"docker_buildx_version_output"`
	HarborVersion                  string                             `json:"harbor_version"`
	ResultABIFormat                string                             `json:"result_abi_format"`
	ResultABIVersion               string                             `json:"result_abi_version"`
	TaskArtifactPort               string                             `json:"task_artifact_port"`
	TaskArtifactSchema             string                             `json:"task_artifact_schema"`
	AgentID                        string                             `json:"agent_id"`
	AgentVersion                   string                             `json:"agent_version"`
	ModelID                        string                             `json:"model_id"`
	ModelVersion                   string                             `json:"model_version"`
	EndpointEnvName                string                             `json:"endpoint_env_name"`
	EndpointChildEnvKey            string                             `json:"endpoint_child_env_key"`
	EndpointFingerprint            workflowkit.Fingerprint            `json:"endpoint_fingerprint"`
	SecretEnvTemplates             []HarborEvaluatorSecretEnvTemplate `json:"secret_env_templates"`
	Attempts                       int                                `json:"attempts"`
	ConcurrentTrials               int                                `json:"concurrent_trials"`
	MaxRetries                     int                                `json:"max_retries"`
	RequireTrajectory              bool                               `json:"require_trajectory"`
	ScreenshotRenderer             HarborEvaluatorScreenshotRenderer  `json:"screenshot_renderer"`
}

// Clone returns an independently owned invocation configuration.
func (invocation HarborEvaluatorInvocation) Clone() HarborEvaluatorInvocation {
	invocation.SecretEnvTemplates = append([]HarborEvaluatorSecretEnvTemplate(nil), invocation.SecretEnvTemplates...)
	return invocation
}

// NewHarborEvaluatorRuntimeAttestor creates a strict attestor for only the
// typed CodeEdge evaluator lock. Generic local commands remain the responsibility
// of LocalFilesystemRuntimeAttestor or another explicit composition.
func NewHarborEvaluatorRuntimeAttestor(config HarborEvaluatorRuntimeAttestorConfig) (*HarborEvaluatorRuntimeAttestor, error) {
	if err := config.HarborFlowBuild.Validate(); err != nil {
		return nil, fmt.Errorf("%w: configured Harbor Flow build identity is invalid: %w", ErrDeploymentOperationRuntimeAttestationFailed, err)
	}
	lookup := config.EnvironmentLookup
	if lookup == nil {
		lookup = os.LookupEnv
	}
	return &HarborEvaluatorRuntimeAttestor{harborFlowBuild: config.HarborFlowBuild, lookupEnvironment: lookup}, nil
}

// HarborFlowBuild returns the immutable trusted Harbor Flow identity selected
// at composition time.
func (attestor *HarborEvaluatorRuntimeAttestor) HarborFlowBuild() HarborFlowBuildIdentity {
	if attestor == nil {
		return HarborFlowBuildIdentity{}
	}
	return attestor.harborFlowBuild
}

// AttestDeploymentOperation satisfies the generic catalog-lock boundary. A
// caller that needs the secret-free invocation configuration can call
// AttestHarborEvaluatorOperation directly after the same static lock check.
func (attestor *HarborEvaluatorRuntimeAttestor) AttestDeploymentOperation(ctx context.Context, attestation DeploymentOperationRuntimeAttestation) error {
	_, err := attestor.AttestHarborEvaluatorOperation(ctx, attestation)
	return err
}

// AttestHarborEvaluatorInvocationBeforeLaunch implements the executor-facing
// launch-time runtime attestation boundary.
func (attestor *HarborEvaluatorRuntimeAttestor) AttestHarborEvaluatorInvocationBeforeLaunch(ctx context.Context, invocation HarborEvaluatorInvocation, home string) ([]string, error) {
	if attestor == nil {
		return nil, ErrDeploymentOperationRuntimeAttestationUnavailable
	}
	return AttestHarborEvaluatorInvocationBeforeLaunch(ctx, invocation, home)
}

// AttestHarborEvaluatorOperation checks the exact launcher, interpreter,
// source tree, no-argv evaluator contract, endpoint fingerprint, and bounded
// `--version` probe before returning a provider-ready secret-free config.
func (attestor *HarborEvaluatorRuntimeAttestor) AttestHarborEvaluatorOperation(ctx context.Context, attestation DeploymentOperationRuntimeAttestation) (HarborEvaluatorInvocation, error) {
	if attestor == nil {
		return HarborEvaluatorInvocation{}, ErrDeploymentOperationRuntimeAttestationUnavailable
	}
	if err := contextRuntimeAttestationError(ctx); err != nil {
		return HarborEvaluatorInvocation{}, err
	}
	if err := validateLocalFilesystemRuntimeAttestation(attestation); err != nil {
		return HarborEvaluatorInvocation{}, err
	}
	if attestation.HarborFlowBuild != attestor.harborFlowBuild {
		return HarborEvaluatorInvocation{}, fmt.Errorf("%w: Harbor Flow build identity does not match the installed process identity", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	payload, ok := attestation.Record.Operation.Payload.(workflowadapter.LocalCommandOperationPayload)
	if !ok || attestation.Record.HarborEvaluator == nil || !isHarborEvaluatorCommandID(payload.CommandID) {
		return HarborEvaluatorInvocation{}, fmt.Errorf("%w: no typed Harbor evaluator lock is installed for this operation", ErrDeploymentOperationRuntimeAttestationUnavailable)
	}
	if len(payload.Arguments) != 0 {
		return HarborEvaluatorInvocation{}, fmt.Errorf("%w: Harbor evaluator local.command received argv", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	evaluator := attestation.Record.HarborEvaluator.Clone()
	if err := evaluator.Validate(); err != nil {
		return HarborEvaluatorInvocation{}, fmt.Errorf("%w: typed Harbor evaluator lock is invalid", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	if attestation.Record.LocalExecutable == nil || evaluator.Launcher != *attestation.Record.LocalExecutable || evaluator.Launcher.CommandID != payload.CommandID {
		return HarborEvaluatorInvocation{}, fmt.Errorf("%w: typed Harbor evaluator launcher does not match frozen local command", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	invocation, err := NewHarborEvaluatorInvocation(payload.CommandID, evaluator)
	if err != nil {
		return HarborEvaluatorInvocation{}, err
	}
	probeRoot, err := os.MkdirTemp("", "harbor-evaluator-runtime-probe-")
	if err != nil {
		return HarborEvaluatorInvocation{}, fmt.Errorf("%w: isolated Harbor evaluator probe environment is unavailable", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	defer os.RemoveAll(probeRoot)
	probeHome := filepath.Join(probeRoot, "home")
	if err := os.MkdirAll(filepath.Join(probeHome, ".docker"), 0o700); err != nil {
		return HarborEvaluatorInvocation{}, fmt.Errorf("%w: isolated Harbor evaluator probe environment is unavailable", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	if _, err := attestor.AttestHarborEvaluatorInvocationBeforeLaunch(ctx, invocation, probeHome); err != nil {
		return HarborEvaluatorInvocation{}, err
	}
	endpoint, present := attestor.lookupEnvironment(evaluator.Contract.EndpointEnvName)
	if !present {
		return HarborEvaluatorInvocation{}, fmt.Errorf("%w: required Harbor evaluator endpoint environment variable is unavailable", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	endpointFingerprint, err := CanonicalHarborEvaluatorEndpointFingerprint(endpoint)
	if err != nil || endpointFingerprint != evaluator.Contract.EndpointFingerprint {
		return HarborEvaluatorInvocation{}, fmt.Errorf("%w: Harbor evaluator endpoint fingerprint does not match the immutable lock", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	return invocation, nil
}

// NewHarborEvaluatorInvocation projects one validated evaluator lock into the
// complete secret-free runtime identity consumed by production composition and
// launch-time attestation.
func NewHarborEvaluatorInvocation(commandID string, evaluator HarborEvaluatorOperationLock) (HarborEvaluatorInvocation, error) {
	if !isHarborEvaluatorCommandID(commandID) || evaluator.Launcher.CommandID != commandID {
		return HarborEvaluatorInvocation{}, fmt.Errorf("%w: Harbor evaluator command does not match the immutable launcher", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	if err := evaluator.Validate(); err != nil {
		return HarborEvaluatorInvocation{}, fmt.Errorf("%w: typed Harbor evaluator lock is invalid", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	dockerPATH, err := HarborEvaluatorDockerPATH(evaluator.DockerCLI.AbsolutePath)
	if err != nil {
		return HarborEvaluatorInvocation{}, fmt.Errorf("%w: locked Docker PATH contract is invalid", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	return harborEvaluatorInvocationFromLock(commandID, evaluator, dockerPATH), nil
}

func harborEvaluatorInvocationFromLock(commandID string, evaluator HarborEvaluatorOperationLock, dockerPATH string) HarborEvaluatorInvocation {
	contract := evaluator.Contract.canonicalized()
	return HarborEvaluatorInvocation{
		CommandID: commandID, LauncherPath: evaluator.Launcher.AbsolutePath, LauncherVersion: evaluator.Launcher.Version, LauncherContentSHA256: evaluator.Launcher.ContentSHA256,
		ClaudeCodeExecutablePath: evaluator.ClaudeCodeExecutable.AbsolutePath, ClaudeCodeVersion: evaluator.ClaudeCodeExecutable.Version, ClaudeCodeContentSHA256: evaluator.ClaudeCodeExecutable.ContentSHA256,
		PythonInterpreterPath: evaluator.PythonInterpreter.AbsolutePath, PythonInterpreterVersion: evaluator.PythonInterpreter.Version, PythonInterpreterContentSHA256: evaluator.PythonInterpreter.ContentSHA256,
		PythonSourceTreePath: evaluator.PythonSourceTree.AbsolutePath, PythonSourceFilesSHA256: evaluator.PythonSourceTree.PythonFilesSHA256,
		DockerCLIPath: evaluator.DockerCLI.AbsolutePath, DockerCLIContentSHA256: evaluator.DockerCLI.ContentSHA256,
		DockerPATH: dockerPATH, DockerVersion: evaluator.DockerCLI.Version, DockerServerVersion: evaluator.DockerServerVersion,
		DockerComposePluginPath: evaluator.DockerComposePlugin.AbsolutePath, DockerComposeContentSHA256: evaluator.DockerComposePlugin.ContentSHA256,
		DockerComposeVersion: evaluator.DockerComposePlugin.Version, DockerComposeVersionOutput: evaluator.DockerComposeVersionOutput,
		DockerBuildxPluginPath: evaluator.DockerBuildxPlugin.AbsolutePath, DockerBuildxContentSHA256: evaluator.DockerBuildxPlugin.ContentSHA256,
		DockerBuildxVersion: evaluator.DockerBuildxPlugin.Version, DockerBuildxVersionOutput: evaluator.DockerBuildxVersionOutput,
		HarborVersion:   contract.HarborVersion,
		ResultABIFormat: contract.ResultABIFormat, ResultABIVersion: contract.ResultABIVersion,
		TaskArtifactPort: contract.TaskArtifactPort, TaskArtifactSchema: contract.TaskArtifactSchema,
		AgentID: contract.AgentID, AgentVersion: contract.AgentVersion, ModelID: contract.ModelID, ModelVersion: contract.ModelVersion,
		EndpointEnvName: contract.EndpointEnvName, EndpointChildEnvKey: contract.EndpointChildEnvKey, EndpointFingerprint: contract.EndpointFingerprint,
		SecretEnvTemplates: append([]HarborEvaluatorSecretEnvTemplate(nil), contract.SecretEnvTemplates...),
		Attempts:           contract.Attempts, ConcurrentTrials: contract.ConcurrentTrials, MaxRetries: contract.MaxRetries, RequireTrajectory: contract.RequireTrajectory,
		ScreenshotRenderer: contract.ScreenshotRenderer,
	}
}

// AttestHarborEvaluatorInvocationBeforeLaunch re-proves every executable,
// imported Python source, Docker plugin, and daemon identity immediately before
// Harbor starts. The returned environment is the exact environment that was
// used for Docker discovery and must be passed unchanged to the Harbor process.
func AttestHarborEvaluatorInvocationBeforeLaunch(ctx context.Context, invocation HarborEvaluatorInvocation, home string) ([]string, error) {
	if err := contextRuntimeAttestationError(ctx); err != nil {
		return nil, err
	}
	evaluator, err := harborEvaluatorRuntimeLockFromInvocation(invocation)
	if err != nil {
		return nil, err
	}
	environment, err := harborEvaluatorInvocationEnvironment(invocation, home)
	if err != nil {
		return nil, err
	}
	if err := attestLockedRegularFile(ctx, evaluator.Launcher); err != nil {
		return nil, err
	}
	if err := attestLockedRegularFile(ctx, evaluator.ClaudeCodeExecutable); err != nil {
		return nil, err
	}
	if err := attestLockedRegularFile(ctx, evaluator.PythonInterpreter); err != nil {
		return nil, err
	}
	if err := attestHarborEvaluatorLauncherInterpreter(evaluator.Launcher, evaluator.PythonInterpreter); err != nil {
		return nil, err
	}
	if err := attestHarborPythonSourceTree(ctx, evaluator.PythonSourceTree); err != nil {
		return nil, err
	}
	if err := attestHarborEvaluatorDockerRuntime(ctx, evaluator, environment); err != nil {
		return nil, err
	}
	if err := attestHarborEvaluatorVersion(ctx, evaluator); err != nil {
		return nil, err
	}
	if err := attestHarborEvaluatorClaudeCodeVersion(ctx, evaluator.ClaudeCodeExecutable); err != nil {
		return nil, err
	}
	// Re-prove every executable after all subprocess probes. This closes the
	// materialization-to-launch window and catches pathname replacement by a
	// probe or concurrent local change before the caller executes Harbor.
	for _, executable := range []LocalExecutableLock{evaluator.Launcher, evaluator.ClaudeCodeExecutable, evaluator.PythonInterpreter, evaluator.DockerCLI, evaluator.DockerComposePlugin, evaluator.DockerBuildxPlugin} {
		if err := attestLockedRegularFile(ctx, executable); err != nil {
			return nil, err
		}
	}
	if err := attestHarborEvaluatorLauncherInterpreter(evaluator.Launcher, evaluator.PythonInterpreter); err != nil {
		return nil, err
	}
	if err := attestHarborPythonSourceTree(ctx, evaluator.PythonSourceTree); err != nil {
		return nil, err
	}
	return append([]string(nil), environment...), nil
}

func harborEvaluatorRuntimeLockFromInvocation(invocation HarborEvaluatorInvocation) (HarborEvaluatorOperationLock, error) {
	if !isHarborEvaluatorCommandID(invocation.CommandID) || invocation.HarborVersion != HarborEvaluatorHarborVersion {
		return HarborEvaluatorOperationLock{}, fmt.Errorf("%w: Harbor evaluator invocation identity is invalid", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	if invocation.ClaudeCodeVersion != invocation.AgentVersion {
		return HarborEvaluatorOperationLock{}, fmt.Errorf("%w: Harbor evaluator Claude Code invocation version does not match its agent version", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	if invocation.DockerVersion != HarborEvaluatorDockerVersion || invocation.DockerServerVersion != HarborEvaluatorDockerServerVersion ||
		invocation.DockerComposeVersion != HarborEvaluatorDockerComposeVersion || invocation.DockerComposeVersionOutput != HarborEvaluatorDockerComposeVersionOutput ||
		invocation.DockerBuildxVersion != HarborEvaluatorDockerBuildxVersion || invocation.DockerBuildxVersionOutput != HarborEvaluatorDockerBuildxVersionOutput {
		return HarborEvaluatorOperationLock{}, fmt.Errorf("%w: Harbor evaluator Docker invocation identity is invalid", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	dockerPATH, err := HarborEvaluatorDockerPATH(invocation.DockerCLIPath)
	if err != nil || invocation.DockerPATH != dockerPATH {
		return HarborEvaluatorOperationLock{}, fmt.Errorf("%w: Harbor evaluator Docker PATH invocation identity is invalid", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	evaluator := HarborEvaluatorOperationLock{
		Launcher: LocalExecutableLock{
			CommandID: invocation.CommandID, AbsolutePath: invocation.LauncherPath, Version: invocation.LauncherVersion, ContentSHA256: invocation.LauncherContentSHA256,
		},
		ClaudeCodeExecutable: LocalExecutableLock{
			CommandID: HarborEvaluatorClaudeCodeCommandID, AbsolutePath: invocation.ClaudeCodeExecutablePath, Version: invocation.ClaudeCodeVersion, ContentSHA256: invocation.ClaudeCodeContentSHA256,
		},
		PythonInterpreter: LocalExecutableLock{
			CommandID: HarborEvaluatorPythonCommandID, AbsolutePath: invocation.PythonInterpreterPath, Version: invocation.PythonInterpreterVersion, ContentSHA256: invocation.PythonInterpreterContentSHA256,
		},
		PythonSourceTree: HarborPythonSourceTreeLock{AbsolutePath: invocation.PythonSourceTreePath, PythonFilesSHA256: invocation.PythonSourceFilesSHA256},
		DockerCLI: LocalExecutableLock{
			CommandID: HarborEvaluatorDockerCommandID, AbsolutePath: invocation.DockerCLIPath, Version: invocation.DockerVersion, ContentSHA256: invocation.DockerCLIContentSHA256,
		},
		DockerServerVersion: invocation.DockerServerVersion,
		DockerComposePlugin: LocalExecutableLock{
			CommandID: HarborEvaluatorDockerComposeCommandID, AbsolutePath: invocation.DockerComposePluginPath, Version: invocation.DockerComposeVersion, ContentSHA256: invocation.DockerComposeContentSHA256,
		},
		DockerComposeVersionOutput: invocation.DockerComposeVersionOutput,
		DockerBuildxPlugin: LocalExecutableLock{
			CommandID: HarborEvaluatorDockerBuildxCommandID, AbsolutePath: invocation.DockerBuildxPluginPath, Version: invocation.DockerBuildxVersion, ContentSHA256: invocation.DockerBuildxContentSHA256,
		},
		DockerBuildxVersionOutput: invocation.DockerBuildxVersionOutput,
		HarborVersionOutput:       invocation.HarborVersion,
	}
	for _, executable := range []LocalExecutableLock{evaluator.Launcher, evaluator.ClaudeCodeExecutable, evaluator.PythonInterpreter, evaluator.DockerCLI, evaluator.DockerComposePlugin, evaluator.DockerBuildxPlugin} {
		if err := validateLocalExecutableLock(executable); err != nil {
			return HarborEvaluatorOperationLock{}, fmt.Errorf("%w: Harbor evaluator invocation executable identity is invalid", ErrDeploymentOperationRuntimeAttestationFailed)
		}
	}
	if filepath.Base(evaluator.DockerComposePlugin.AbsolutePath) != "docker-compose" || filepath.Base(evaluator.DockerBuildxPlugin.AbsolutePath) != "docker-buildx" {
		return HarborEvaluatorOperationLock{}, fmt.Errorf("%w: Harbor evaluator invocation plugin identity is invalid", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	if err := evaluator.PythonSourceTree.Validate(); err != nil {
		return HarborEvaluatorOperationLock{}, fmt.Errorf("%w: Harbor evaluator invocation source identity is invalid", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	return evaluator, nil
}

func harborEvaluatorInvocationEnvironment(invocation HarborEvaluatorInvocation, home string) ([]string, error) {
	if !filepath.IsAbs(home) || filepath.Clean(home) != home || home == string(filepath.Separator) {
		return nil, fmt.Errorf("%w: Harbor evaluator home is not a clean non-root absolute path", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	dockerConfig := filepath.Join(home, ".docker")
	for _, directory := range []string{home, dockerConfig} {
		info, err := inspectLockedLocalExecutablePath(directory)
		if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("%w: isolated Harbor evaluator directory is unavailable or unsafe", ErrDeploymentOperationRuntimeAttestationFailed)
		}
	}
	return []string{
		"DOCKER_CONFIG=" + dockerConfig,
		"HOME=" + home,
		"LANG=C.UTF-8",
		"PATH=" + invocation.DockerPATH,
	}, nil
}

func attestHarborEvaluatorLauncherInterpreter(launcher, interpreter LocalExecutableLock) error {
	file, err := os.Open(launcher.AbsolutePath)
	if err != nil {
		return fmt.Errorf("%w: locked Harbor launcher cannot be opened for interpreter verification", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	defer file.Close()
	line, err := readHarborEvaluatorShebang(file)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(line) || filepath.Clean(line) != line {
		return fmt.Errorf("%w: locked Harbor launcher shebang is not an absolute interpreter path", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	resolved, err := filepath.EvalSymlinks(line)
	if err != nil || filepath.Clean(resolved) != interpreter.AbsolutePath {
		return fmt.Errorf("%w: locked Harbor launcher shebang does not resolve to the pinned Python interpreter", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	return nil
}

func readHarborEvaluatorShebang(reader io.Reader) (string, error) {
	firstLine, err := readBoundedShebangLine(reader, harborEvaluatorShebangLimit)
	if err != nil {
		return "", fmt.Errorf("%w: locked Harbor launcher shebang cannot be read", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	if !bytes.HasPrefix(firstLine, []byte("#!")) {
		return "", fmt.Errorf("%w: locked Harbor launcher lacks a strict Python shebang", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	interpreter := strings.TrimSuffix(string(firstLine[2:]), "\r")
	parts := strings.Fields(interpreter)
	if len(parts) != 1 || parts[0] != interpreter {
		return "", fmt.Errorf("%w: locked Harbor launcher shebang has unsupported arguments", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	return interpreter, nil
}

func attestHarborEvaluatorVersion(ctx context.Context, evaluator HarborEvaluatorOperationLock) error {
	if err := contextRuntimeAttestationError(ctx); err != nil {
		return err
	}
	command := exec.CommandContext(ctx, evaluator.Launcher.AbsolutePath, "--version")
	command.Dir = filepath.Dir(evaluator.Launcher.AbsolutePath)
	// The identity probe must not inherit endpoint or credential variables.
	command.Env = []string{}
	output := &harborEvaluatorLimitedBuffer{limit: harborEvaluatorVersionOutputLimit}
	command.Stdout = output
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return fmt.Errorf("%w: locked Harbor --version probe failed", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	version, ok := normalizedHarborEvaluatorVersionOutput(output.Bytes())
	if !ok || version != evaluator.HarborVersionOutput {
		return fmt.Errorf("%w: locked Harbor --version output does not match the immutable lock", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	return contextRuntimeAttestationError(ctx)
}

func attestHarborEvaluatorClaudeCodeVersion(ctx context.Context, claude LocalExecutableLock) error {
	if err := contextRuntimeAttestationError(ctx); err != nil {
		return err
	}
	command := exec.CommandContext(ctx, claude.AbsolutePath, "--version")
	command.Dir = filepath.Dir(claude.AbsolutePath)
	// The version probe must not inherit endpoint or credential variables.
	command.Env = []string{}
	output := &harborEvaluatorLimitedBuffer{limit: harborEvaluatorVersionOutputLimit}
	command.Stdout = output
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return fmt.Errorf("%w: locked Claude Code --version probe failed", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	versionOutput, ok := normalizedHarborEvaluatorVersionOutput(output.Bytes())
	if !ok || versionOutput != claude.Version+" (Claude Code)" {
		return fmt.Errorf("%w: locked Claude Code --version output does not match the immutable lock", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	return contextRuntimeAttestationError(ctx)
}

func attestHarborEvaluatorDockerRuntime(ctx context.Context, evaluator HarborEvaluatorOperationLock, environment []string) error {
	if err := attestLockedRegularFile(ctx, evaluator.DockerCLI); err != nil {
		return err
	}
	if err := attestLockedRegularFile(ctx, evaluator.DockerComposePlugin); err != nil {
		return err
	}
	if err := attestLockedRegularFile(ctx, evaluator.DockerBuildxPlugin); err != nil {
		return err
	}
	dockerPATH, err := attestHarborEvaluatorDockerPATH(evaluator.DockerCLI)
	if err != nil {
		return err
	}
	wantPATH := "PATH=" + dockerPATH
	if len(environment) != 4 || environment[3] != wantPATH {
		return fmt.Errorf("%w: Docker probe environment does not match the frozen Harbor invocation", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	if err := attestHarborEvaluatorDockerVersion(ctx, evaluator.DockerCLI, environment); err != nil {
		return err
	}
	if err := attestHarborEvaluatorDockerServerVersion(ctx, evaluator, environment); err != nil {
		return err
	}
	if err := attestHarborEvaluatorDockerPlugin(ctx, evaluator.DockerCLI, evaluator.DockerComposePlugin, environment, "Compose", harborEvaluatorComposePathFormat, evaluator.DockerComposeVersionOutput, normalizedHarborEvaluatorDockerComposeVersionOutput, "compose", "version"); err != nil {
		return err
	}
	if err := attestHarborEvaluatorDockerPlugin(ctx, evaluator.DockerCLI, evaluator.DockerBuildxPlugin, environment, "Buildx", harborEvaluatorBuildxPathFormat, evaluator.DockerBuildxVersionOutput, normalizedHarborEvaluatorDockerBuildxVersionOutput, "buildx", "version"); err != nil {
		return err
	}
	// Rehash all three identities after the subprocess probes so a replacement
	// during discovery cannot survive this attestation pass.
	if err := attestLockedRegularFile(ctx, evaluator.DockerCLI); err != nil {
		return err
	}
	if err := attestLockedRegularFile(ctx, evaluator.DockerComposePlugin); err != nil {
		return err
	}
	if err := attestLockedRegularFile(ctx, evaluator.DockerBuildxPlugin); err != nil {
		return err
	}
	return nil
}

func attestHarborEvaluatorDockerPATH(docker LocalExecutableLock) (string, error) {
	dockerPATH, err := HarborEvaluatorDockerPATH(docker.AbsolutePath)
	if err != nil {
		return "", fmt.Errorf("%w: locked Docker PATH contract is invalid", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	resolvedPath, resolvedInfo, err := resolveHarborEvaluatorPATHExecutable(dockerPATH, "docker")
	if err != nil {
		return "", fmt.Errorf("%w: controlled PATH cannot resolve Docker", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	lockedInfo, err := inspectLockedLocalExecutablePath(docker.AbsolutePath)
	if err != nil || resolvedPath != docker.AbsolutePath || !resolvedInfo.Mode().IsRegular() || !lockedInfo.Mode().IsRegular() || !os.SameFile(resolvedInfo, lockedInfo) {
		return "", fmt.Errorf("%w: controlled PATH Docker does not match the immutable lock", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	return dockerPATH, nil
}

func resolveHarborEvaluatorPATHExecutable(searchPath, basename string) (string, os.FileInfo, error) {
	for _, directory := range filepath.SplitList(searchPath) {
		if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
			return "", nil, errors.New("noncanonical executable search directory")
		}
		candidate := filepath.Join(directory, basename)
		leaf, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", nil, err
		}
		if leaf.Mode()&os.ModeSymlink != 0 {
			return "", nil, errors.New("symbolic link selected from executable search path")
		}
		if !leaf.Mode().IsRegular() || leaf.Mode()&0o111 == 0 {
			continue
		}
		info, err := inspectLockedLocalExecutablePath(candidate)
		if err != nil {
			return "", nil, err
		}
		return candidate, info, nil
	}
	return "", nil, errors.New("executable is unavailable")
}

func attestHarborEvaluatorDockerVersion(ctx context.Context, docker LocalExecutableLock, environment []string) error {
	if err := contextRuntimeAttestationError(ctx); err != nil {
		return err
	}
	output, err := runHarborEvaluatorDockerProbe(ctx, docker.AbsolutePath, environment, "--version")
	if err != nil {
		return fmt.Errorf("%w: locked Docker --version probe failed", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	version, ok := normalizedHarborEvaluatorDockerVersionOutput(output)
	if !ok || version != docker.Version {
		return fmt.Errorf("%w: locked Docker --version output does not match the immutable lock", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	return contextRuntimeAttestationError(ctx)
}

func attestHarborEvaluatorDockerServerVersion(ctx context.Context, evaluator HarborEvaluatorOperationLock, environment []string) error {
	versionRaw, err := runHarborEvaluatorDockerProbe(ctx, evaluator.DockerCLI.AbsolutePath, environment, "version", "--format", harborEvaluatorDockerServerFormat)
	if err != nil {
		return fmt.Errorf("%w: locked Docker daemon version probe failed", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	version, ok := normalizedHarborEvaluatorVersionOutput(versionRaw)
	if !ok || version != evaluator.DockerServerVersion {
		return fmt.Errorf("%w: locked Docker daemon version does not match the immutable lock", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	return contextRuntimeAttestationError(ctx)
}

func attestHarborEvaluatorDockerPlugin(ctx context.Context, docker, plugin LocalExecutableLock, environment []string, label, pathFormat, expectedOutput string, normalizeVersion func([]byte) (string, string, bool), versionArguments ...string) error {
	pluginPathRaw, err := runHarborEvaluatorDockerProbe(ctx, docker.AbsolutePath, environment, "info", "--format", pathFormat)
	if err != nil {
		return fmt.Errorf("%w: locked Docker %s plugin resolution probe failed", ErrDeploymentOperationRuntimeAttestationFailed, label)
	}
	pluginPath, ok := normalizedHarborEvaluatorVersionOutput(pluginPathRaw)
	if !ok || pluginPath != plugin.AbsolutePath {
		return fmt.Errorf("%w: Docker resolved a %s plugin outside the immutable lock", ErrDeploymentOperationRuntimeAttestationFailed, label)
	}
	resolvedInfo, err := inspectLockedLocalExecutablePath(pluginPath)
	if err != nil {
		return fmt.Errorf("%w: resolved Docker %s plugin cannot be inspected", ErrDeploymentOperationRuntimeAttestationFailed, label)
	}
	lockedInfo, err := inspectLockedLocalExecutablePath(plugin.AbsolutePath)
	if err != nil || !resolvedInfo.Mode().IsRegular() || !lockedInfo.Mode().IsRegular() || !os.SameFile(resolvedInfo, lockedInfo) {
		return fmt.Errorf("%w: resolved Docker %s plugin does not match the immutable lock", ErrDeploymentOperationRuntimeAttestationFailed, label)
	}
	versionRaw, err := runHarborEvaluatorDockerProbe(ctx, docker.AbsolutePath, environment, versionArguments...)
	if err != nil {
		return fmt.Errorf("%w: locked Docker %s version probe failed", ErrDeploymentOperationRuntimeAttestationFailed, label)
	}
	versionOutput, version, ok := normalizeVersion(versionRaw)
	if !ok || versionOutput != expectedOutput || version != plugin.Version {
		return fmt.Errorf("%w: locked Docker %s version output does not match the immutable lock", ErrDeploymentOperationRuntimeAttestationFailed, label)
	}
	return contextRuntimeAttestationError(ctx)
}

func runHarborEvaluatorDockerProbe(ctx context.Context, dockerPath string, environment []string, arguments ...string) ([]byte, error) {
	if err := contextRuntimeAttestationError(ctx); err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, dockerPath, arguments...)
	command.Dir = filepath.Dir(dockerPath)
	command.Env = append([]string(nil), environment...)
	output := &harborEvaluatorLimitedBuffer{limit: harborEvaluatorVersionOutputLimit}
	command.Stdout = output
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return nil, err
	}
	return append([]byte(nil), output.Bytes()...), contextRuntimeAttestationError(ctx)
}

type harborEvaluatorLimitedBuffer struct {
	limit int
	bytes.Buffer
}

func (buffer *harborEvaluatorLimitedBuffer) Write(value []byte) (int, error) {
	if buffer == nil || buffer.limit < 0 || buffer.Len()+len(value) > buffer.limit {
		return 0, errors.New("Harbor version output exceeds bounded attestation buffer")
	}
	return buffer.Buffer.Write(value)
}

func normalizedHarborEvaluatorVersionOutput(raw []byte) (string, bool) {
	value := string(raw)
	value = strings.TrimSuffix(value, "\n")
	value = strings.TrimSuffix(value, "\r")
	if value == "" || value != strings.TrimSpace(value) || strings.ContainsAny(value, "\r\n") {
		return "", false
	}
	return value, true
}

func normalizedHarborEvaluatorDockerVersionOutput(raw []byte) (string, bool) {
	value, ok := normalizedHarborEvaluatorVersionOutput(raw)
	if !ok {
		return "", false
	}
	fields := strings.Fields(value)
	if len(fields) < 3 || fields[0] != "Docker" || fields[1] != "version" {
		return "", false
	}
	version := strings.TrimSuffix(fields[2], ",")
	if version == "" || version != strings.TrimSpace(version) {
		return "", false
	}
	return version, true
}

func normalizedHarborEvaluatorDockerComposeVersionOutput(raw []byte) (string, string, bool) {
	output, ok := normalizedHarborEvaluatorVersionOutput(raw)
	if !ok || !strings.HasPrefix(output, "Docker Compose version ") {
		return "", "", false
	}
	version := strings.TrimPrefix(output, "Docker Compose version ")
	if version == "" || version != strings.TrimSpace(version) || strings.ContainsAny(version, " \t\r\n") {
		return "", "", false
	}
	return output, version, true
}

func normalizedHarborEvaluatorDockerBuildxVersionOutput(raw []byte) (string, string, bool) {
	output, ok := normalizedHarborEvaluatorVersionOutput(raw)
	if !ok {
		return "", "", false
	}
	fields := strings.Fields(output)
	if len(fields) != 3 || fields[0] != "github.com/docker/buildx" || fields[1] == "" || !strings.HasPrefix(fields[1], "v") || len(fields[2]) != 40 {
		return "", "", false
	}
	for _, character := range fields[2] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return "", "", false
		}
	}
	return output, fields[1], true
}

// ComputeHarborPythonSourceTreeFingerprint implements the source-tree digest
// documented by HARBOR_CLI_0_18_OBSERVED_CONTRACT.md: sorted sha256sum-style
// records for every regular *.py file beneath the exact absolute package root.
// The function rejects symlinks and non-regular Python files so a replacement
// cannot become an implicit redirect during runtime attestation.
func ComputeHarborPythonSourceTreeFingerprint(ctx context.Context, root string) (workflowkit.Fingerprint, error) {
	if err := validateHarborEvaluatorAbsolutePath("Harbor evaluator Python source tree", root); err != nil {
		return "", fmt.Errorf("%w: Python source tree lock is invalid", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	initial, err := inspectLockedLocalExecutablePath(root)
	if err != nil || !initial.IsDir() {
		return "", fmt.Errorf("%w: locked Harbor Python source tree cannot be inspected", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	first, err := computeHarborPythonSourceTreeFingerprintPass(ctx, root)
	if err != nil {
		return "", err
	}
	second, err := computeHarborPythonSourceTreeFingerprintPass(ctx, root)
	if err != nil {
		return "", err
	}
	final, err := inspectLockedLocalExecutablePath(root)
	if err != nil || !final.IsDir() || !os.SameFile(initial, final) || first != second {
		return "", fmt.Errorf("%w: locked Harbor Python source tree changed while being read", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	return second, nil
}

func computeHarborPythonSourceTreeFingerprintPass(ctx context.Context, root string) (workflowkit.Fingerprint, error) {
	if err := contextRuntimeAttestationError(ctx); err != nil {
		return "", err
	}
	if err := validateHarborEvaluatorAbsolutePath("Harbor evaluator Python source tree", root); err != nil {
		return "", fmt.Errorf("%w: Python source tree lock is invalid", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	if err := inspectHarborEvaluatorSourceTreeRoot(root); err != nil {
		return "", fmt.Errorf("%w: locked Harbor Python source tree cannot be inspected", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	files := make([]string, 0, 64)
	err := filepath.WalkDir(root, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := contextRuntimeAttestationError(ctx); err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("symbolic link in locked Harbor Python source tree")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return errors.New("non-regular file in locked Harbor Python source tree")
		}
		if strings.HasSuffix(entry.Name(), ".py") {
			for _, character := range current {
				if character == '\n' || character == '\r' || character == '\x00' {
					return errors.New("noncanonical Python source path")
				}
			}
			files = append(files, current)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("%w: locked Harbor Python source tree cannot be read", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	if len(files) == 0 {
		return "", fmt.Errorf("%w: locked Harbor Python source tree contains no Python files", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	sort.Strings(files)
	manifest := sha256.New()
	for _, filePath := range files {
		if err := contextRuntimeAttestationError(ctx); err != nil {
			return "", err
		}
		fingerprint, err := fingerprintHarborEvaluatorRegularFile(ctx, filePath)
		if err != nil {
			return "", err
		}
		line := strings.TrimPrefix(string(fingerprint), "sha256:") + "  " + filePath + "\n"
		if _, err := io.WriteString(manifest, line); err != nil {
			return "", fmt.Errorf("%w: cannot construct Harbor Python source manifest", ErrDeploymentOperationRuntimeAttestationFailed)
		}
	}
	return workflowkit.Fingerprint("sha256:" + hex.EncodeToString(manifest.Sum(nil))), nil
}

func attestHarborPythonSourceTree(ctx context.Context, locked HarborPythonSourceTreeLock) error {
	if err := locked.Validate(); err != nil {
		return fmt.Errorf("%w: Harbor Python source tree lock is invalid", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	fingerprint, err := ComputeHarborPythonSourceTreeFingerprint(ctx, locked.AbsolutePath)
	if err != nil {
		return err
	}
	if fingerprint != locked.PythonFilesSHA256 {
		return fmt.Errorf("%w: locked Harbor Python source tree fingerprint does not match", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	return nil
}

func inspectHarborEvaluatorSourceTreeRoot(root string) error {
	components := make([]string, 0, 8)
	for current := root; ; {
		components = append(components, current)
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	for index := len(components) - 1; index >= 0; index-- {
		info, err := os.Lstat(components[index])
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("invalid source tree path component")
		}
		if index > 0 && !info.IsDir() {
			return errors.New("non-directory source tree path component")
		}
	}
	return nil
}

func fingerprintHarborEvaluatorRegularFile(ctx context.Context, filePath string) (workflowkit.Fingerprint, error) {
	initial, err := os.Lstat(filePath)
	if err != nil || initial.Mode()&os.ModeSymlink != 0 || !initial.Mode().IsRegular() {
		return "", fmt.Errorf("%w: locked Harbor Python source file cannot be inspected", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("%w: locked Harbor Python source file cannot be opened", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(initial, opened) {
		return "", fmt.Errorf("%w: locked Harbor Python source file changed while opening", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	fingerprint, err := fingerprintOpenRegularFile(ctx, file)
	if err != nil {
		return "", err
	}
	final, err := file.Stat()
	if err != nil || !final.Mode().IsRegular() || !os.SameFile(opened, final) {
		return "", fmt.Errorf("%w: locked Harbor Python source file changed while reading", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	finalPath, err := inspectLockedLocalExecutablePath(filePath)
	if err != nil || !finalPath.Mode().IsRegular() || !os.SameFile(opened, finalPath) {
		return "", fmt.Errorf("%w: locked Harbor Python source file path changed while reading", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	return fingerprint, nil
}

var _ DeploymentOperationRuntimeAttestor = (*HarborEvaluatorRuntimeAttestor)(nil)
