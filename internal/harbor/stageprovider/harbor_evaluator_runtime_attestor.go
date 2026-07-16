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
// before an evaluator provider can execute. It only performs the bounded,
// credential-free `harbor --version` identity probe. It never starts a Harbor
// job, resolves a secret, builds child environment values, or contacts a model
// endpoint.
type HarborEvaluatorRuntimeAttestor struct {
	harborFlowBuild   HarborFlowBuildIdentity
	lookupEnvironment func(string) (string, bool)
}

// HarborEvaluatorInvocation is the secret-free immutable input a controlled
// provider may use after it has passed runtime attestation. It intentionally
// carries only environment variable names, references, and fingerprints. A
// provider must resolve approved secret references privately at child-process
// launch and must never persist the resulting values.
type HarborEvaluatorInvocation struct {
	CommandID             string                             `json:"command_id"`
	LauncherPath          string                             `json:"launcher_path"`
	LauncherContentSHA256 workflowkit.Fingerprint            `json:"launcher_content_sha256"`
	PythonInterpreterPath string                             `json:"python_interpreter_path"`
	PythonSourceTreePath  string                             `json:"python_source_tree_path"`
	DockerCLIPath         string                             `json:"docker_cli_path"`
	DockerVersion         string                             `json:"docker_version"`
	HarborVersion         string                             `json:"harbor_version"`
	ResultABIFormat       string                             `json:"result_abi_format"`
	ResultABIVersion      string                             `json:"result_abi_version"`
	TaskArtifactPort      string                             `json:"task_artifact_port"`
	TaskArtifactSchema    string                             `json:"task_artifact_schema"`
	AgentID               string                             `json:"agent_id"`
	AgentVersion          string                             `json:"agent_version"`
	ModelID               string                             `json:"model_id"`
	ModelVersion          string                             `json:"model_version"`
	EndpointEnvName       string                             `json:"endpoint_env_name"`
	EndpointChildEnvKey   string                             `json:"endpoint_child_env_key"`
	EndpointFingerprint   workflowkit.Fingerprint            `json:"endpoint_fingerprint"`
	SecretEnvTemplates    []HarborEvaluatorSecretEnvTemplate `json:"secret_env_templates"`
	Attempts              int                                `json:"attempts"`
	ConcurrentTrials      int                                `json:"concurrent_trials"`
	MaxRetries            int                                `json:"max_retries"`
	RequireTrajectory     bool                               `json:"require_trajectory"`
	ScreenshotRenderer    HarborEvaluatorScreenshotRenderer  `json:"screenshot_renderer"`
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
	if err := attestLockedRegularFile(ctx, evaluator.Launcher); err != nil {
		return HarborEvaluatorInvocation{}, err
	}
	if err := attestLockedRegularFile(ctx, evaluator.PythonInterpreter); err != nil {
		return HarborEvaluatorInvocation{}, err
	}
	if err := attestHarborEvaluatorLauncherInterpreter(evaluator.Launcher, evaluator.PythonInterpreter); err != nil {
		return HarborEvaluatorInvocation{}, err
	}
	if err := attestHarborPythonSourceTree(ctx, evaluator.PythonSourceTree); err != nil {
		return HarborEvaluatorInvocation{}, err
	}
	if err := attestLockedRegularFile(ctx, evaluator.DockerCLI); err != nil {
		return HarborEvaluatorInvocation{}, err
	}
	if err := attestHarborEvaluatorDockerVersion(ctx, evaluator.DockerCLI); err != nil {
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
	if err := attestHarborEvaluatorVersion(ctx, evaluator); err != nil {
		return HarborEvaluatorInvocation{}, err
	}
	return harborEvaluatorInvocationFromLock(payload.CommandID, evaluator), nil
}

func harborEvaluatorInvocationFromLock(commandID string, evaluator HarborEvaluatorOperationLock) HarborEvaluatorInvocation {
	contract := evaluator.Contract.canonicalized()
	return HarborEvaluatorInvocation{
		CommandID: commandID, LauncherPath: evaluator.Launcher.AbsolutePath, LauncherContentSHA256: evaluator.Launcher.ContentSHA256, PythonInterpreterPath: evaluator.PythonInterpreter.AbsolutePath,
		PythonSourceTreePath: evaluator.PythonSourceTree.AbsolutePath, DockerCLIPath: evaluator.DockerCLI.AbsolutePath, DockerVersion: evaluator.DockerCLI.Version,
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

func attestHarborEvaluatorDockerVersion(ctx context.Context, docker LocalExecutableLock) error {
	if err := contextRuntimeAttestationError(ctx); err != nil {
		return err
	}
	command := exec.CommandContext(ctx, docker.AbsolutePath, "--version")
	command.Dir = filepath.Dir(docker.AbsolutePath)
	command.Env = []string{}
	output := &harborEvaluatorLimitedBuffer{limit: harborEvaluatorVersionOutputLimit}
	command.Stdout = output
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return fmt.Errorf("%w: locked Docker --version probe failed", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	version, ok := normalizedHarborEvaluatorDockerVersionOutput(output.Bytes())
	if !ok || version != docker.Version {
		return fmt.Errorf("%w: locked Docker --version output does not match the immutable lock", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	return contextRuntimeAttestationError(ctx)
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

// ComputeHarborPythonSourceTreeFingerprint implements the source-tree digest
// documented by HARBOR_CLI_0_18_OBSERVED_CONTRACT.md: sorted sha256sum-style
// records for every regular *.py file beneath the exact absolute package root.
// The function rejects symlinks and non-regular Python files so a replacement
// cannot become an implicit redirect during runtime attestation.
func ComputeHarborPythonSourceTreeFingerprint(ctx context.Context, root string) (workflowkit.Fingerprint, error) {
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
	return fingerprint, nil
}

var _ DeploymentOperationRuntimeAttestor = (*HarborEvaluatorRuntimeAttestor)(nil)
