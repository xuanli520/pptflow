package stageprovider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
)

const (
	// CodexAppServerOperationLockFormat identifies the closed runtime
	// attestation used when a generic agent.turn is deliberately implemented
	// by the local Codex App Server. It is a deployment-lock extension, not a
	// generic agent payload or a mutable provider configuration map.
	CodexAppServerOperationLockFormat  = "harbor.codex-app-server-operation.v1"
	CodexAppServerOperationLockVersion = "1"

	// The command IDs distinguish the two independently pinned filesystem
	// objects. The Codex npm shim may be a symbolic link; production locks
	// deliberately pin the real JavaScript launcher instead, so both paths are
	// required to be regular files with no symbolic-link component.
	CodexAppServerJavaScriptLauncherCommandID = "codex.app-server.javascript-launcher"
	CodexAppServerNodeExecutableCommandID     = "codex.app-server.node"
	// CodexAppServerProductionAgentID, CodexAppServerProductionModelID, and
	// CodexAppServerProductionReasoningEffort are the explicitly approved
	// agent configuration for this deployment profile. Other generic agent
	// runtimes remain available through AgentModelLock without this
	// Codex-specific extension.
	CodexAppServerProductionAgentID         = "codex-app-server"
	CodexAppServerProductionModelID         = "gpt-5.6-terra"
	CodexAppServerProductionReasoningEffort = workflowadapter.AgentReasoningEffortXHigh

	// The selected production profile permits edits only inside the controlled
	// workspace and never gives the App Server network access. A future policy
	// needs a new typed lock revision rather than a permissive string value.
	CodexAppServerSandboxModeWorkspaceWrite   = "workspace-write"
	CodexAppServerSandboxPolicyWorkspaceWrite = "workspaceWrite"

	codexAppServerProbeTimeout         = 15 * time.Second
	codexAppServerProbeOutputLimit     = 64 * 1024
	codexAppServerShebangLimit         = 4 * 1024
	codexAppServerSystemPathFirst      = "/usr/local/bin"
	codexAppServerSystemPathSecond     = "/usr/bin"
	codexAppServerSystemPathThird      = "/bin"
	codexAppServerExpectedVersionLabel = "codex-cli"
)

// CodexAppServerOperationLock is the host-specific half of a controlled
// Codex agent.turn. AgentModelLock continues to pin the logical agent/model;
// this type pins the concrete JavaScript launcher, Node runtime, controlled
// CODEX_HOME, and the app-server policy required to realize that logical
// selection. It contains no endpoint, credential, prompt, or environment
// value other than the non-secret CODEX_HOME directory.
type CodexAppServerOperationLock struct {
	Format             string              `json:"format"`
	Version            string              `json:"version"`
	JavaScriptLauncher LocalExecutableLock `json:"javascript_launcher"`
	NodeExecutable     LocalExecutableLock `json:"node_executable"`
	CodexHomeDirectory string              `json:"codex_home_directory"`
	CLIVersionOutput   string              `json:"cli_version_output"`
	SandboxMode        string              `json:"sandbox_mode"`
	SandboxPolicy      string              `json:"sandbox_policy"`
	NetworkAccess      bool                `json:"network_access"`
}

// Clone returns an independently owned copy. All fields are scalar values,
// but retaining this method makes the record cloning boundary explicit and
// future-proof if the closed lock grows a typed collection.
func (lock CodexAppServerOperationLock) Clone() CodexAppServerOperationLock {
	return lock
}

// Validate proves the Codex extension is a complete, bounded production
// attestation. It intentionally accepts neither a launcher discovered on
// PATH nor a policy that enables unrestricted network or host access.
func (lock CodexAppServerOperationLock) Validate() error {
	if lock.Format != CodexAppServerOperationLockFormat {
		return fmt.Errorf("%w: unsupported Codex App Server lock format %q", ErrInvalidDeploymentOperationCatalogLock, lock.Format)
	}
	if lock.Version != CodexAppServerOperationLockVersion {
		return fmt.Errorf("%w: unsupported Codex App Server lock version %q", ErrInvalidDeploymentOperationCatalogLock, lock.Version)
	}
	if err := validateLocalExecutableLock(lock.JavaScriptLauncher); err != nil {
		return err
	}
	if lock.JavaScriptLauncher.CommandID != CodexAppServerJavaScriptLauncherCommandID {
		return fmt.Errorf("%w: Codex JavaScript launcher command id must be %q", ErrInvalidDeploymentOperationCatalogLock, CodexAppServerJavaScriptLauncherCommandID)
	}
	if err := validateCodexAppServerVersion("Codex JavaScript launcher", lock.JavaScriptLauncher.Version, false); err != nil {
		return err
	}
	if err := validateLocalExecutableLock(lock.NodeExecutable); err != nil {
		return err
	}
	if lock.NodeExecutable.CommandID != CodexAppServerNodeExecutableCommandID {
		return fmt.Errorf("%w: Codex Node executable command id must be %q", ErrInvalidDeploymentOperationCatalogLock, CodexAppServerNodeExecutableCommandID)
	}
	if filepath.Base(lock.NodeExecutable.AbsolutePath) != "node" {
		return fmt.Errorf("%w: Codex Node executable must be named %q for the locked launcher shebang", ErrInvalidDeploymentOperationCatalogLock, "node")
	}
	if err := validateCodexAppServerEnvironmentPath("Codex Node executable path", lock.NodeExecutable.AbsolutePath); err != nil {
		return err
	}
	if err := validateCodexAppServerVersion("Codex Node executable", lock.NodeExecutable.Version, true); err != nil {
		return err
	}
	if lock.JavaScriptLauncher.AbsolutePath == lock.NodeExecutable.AbsolutePath {
		return fmt.Errorf("%w: Codex JavaScript launcher and Node executable must be distinct files", ErrInvalidDeploymentOperationCatalogLock)
	}
	if err := validateCodexAppServerDirectoryPath(lock.CodexHomeDirectory); err != nil {
		return err
	}
	if err := validateCodexAppServerCLIVersionOutput(lock.CLIVersionOutput, lock.JavaScriptLauncher.Version); err != nil {
		return err
	}
	if lock.SandboxMode != CodexAppServerSandboxModeWorkspaceWrite {
		return fmt.Errorf("%w: Codex sandbox mode must be %q", ErrInvalidDeploymentOperationCatalogLock, CodexAppServerSandboxModeWorkspaceWrite)
	}
	if lock.SandboxPolicy != CodexAppServerSandboxPolicyWorkspaceWrite {
		return fmt.Errorf("%w: Codex sandbox policy must be %q", ErrInvalidDeploymentOperationCatalogLock, CodexAppServerSandboxPolicyWorkspaceWrite)
	}
	if lock.NetworkAccess {
		return fmt.Errorf("%w: Codex App Server network access must be disabled", ErrInvalidDeploymentOperationCatalogLock)
	}
	return nil
}

// CodexAppServerRuntimeAttestorConfig supplies only the process identity that
// owns the deployment. Runtime file paths, version output, model selection,
// sandbox policy, and CODEX_HOME all come from the immutable lock.
type CodexAppServerRuntimeAttestorConfig struct {
	HarborFlowBuild HarborFlowBuildIdentity
}

// CodexAppServerRuntimeAttestor checks a typed Codex App Server lock before a
// controlled agent.turn delegate runs. It does not launch an App Server turn,
// resolve a secret, inspect ambient environment variables, log probe output,
// or perform network I/O.
type CodexAppServerRuntimeAttestor struct {
	harborFlowBuild HarborFlowBuildIdentity
}

// CodexAppServerInvocation is the secret-free, immutable process contract a
// controlled agent-turn adapter may consume after attestation. Environment
// returns a fresh explicit map containing only CODEX_HOME and the fixed PATH
// necessary for the pinned JavaScript launcher to reach the pinned Node.
type CodexAppServerInvocation struct {
	AgentID                string                               `json:"agent_id"`
	AgentVersion           string                               `json:"agent_version"`
	ModelID                string                               `json:"model_id"`
	ModelVersion           string                               `json:"model_version"`
	ReasoningEffort        workflowadapter.AgentReasoningEffort `json:"reasoning_effort"`
	JavaScriptLauncherPath string                               `json:"javascript_launcher_path"`
	NodeExecutablePath     string                               `json:"node_executable_path"`
	CodexHomeDirectory     string                               `json:"codex_home_directory"`
	CLIVersionOutput       string                               `json:"cli_version_output"`
	SandboxMode            string                               `json:"sandbox_mode"`
	SandboxPolicy          string                               `json:"sandbox_policy"`
	NetworkAccess          bool                                 `json:"network_access"`
}

// Environment returns the only process values supplied by this lock. It
// intentionally does not merge os.Environ, so an ambient CODEX_HOME, model
// endpoint, proxy, token, or API key cannot affect the capability probes or
// subsequent controlled invocation.
func (invocation CodexAppServerInvocation) Environment() map[string]string {
	return map[string]string{
		"CODEX_HOME": invocation.CodexHomeDirectory,
		"PATH":       codexAppServerControlledPATH(invocation.NodeExecutablePath),
	}
}

// NewCodexAppServerRuntimeAttestor creates an immutable, fail-closed Codex
// runtime verifier. A malformed build identity is rejected before the
// attestor can be placed behind the generic catalog-lock wrapper.
func NewCodexAppServerRuntimeAttestor(config CodexAppServerRuntimeAttestorConfig) (*CodexAppServerRuntimeAttestor, error) {
	if err := config.HarborFlowBuild.Validate(); err != nil {
		return nil, fmt.Errorf("%w: configured Harbor Flow build identity is invalid: %w", ErrDeploymentOperationRuntimeAttestationFailed, err)
	}
	return &CodexAppServerRuntimeAttestor{harborFlowBuild: config.HarborFlowBuild}, nil
}

// HarborFlowBuild returns the immutable process identity selected when the
// attestor was composed.
func (attestor *CodexAppServerRuntimeAttestor) HarborFlowBuild() HarborFlowBuildIdentity {
	if attestor == nil {
		return HarborFlowBuildIdentity{}
	}
	return attestor.harborFlowBuild
}

// AttestDeploymentOperation implements the generic runtime-attestation
// boundary. Consumers that need the controlled process settings should use
// AttestCodexAppServerOperation and retain only its secret-free invocation.
func (attestor *CodexAppServerRuntimeAttestor) AttestDeploymentOperation(ctx context.Context, attestation DeploymentOperationRuntimeAttestation) error {
	_, err := attestor.AttestCodexAppServerOperation(ctx, attestation)
	return err
}

// AttestCodexAppServerOperation proves that an exact static agent.turn is
// bound to a real JavaScript Codex launcher and Node executable, both still
// match their file hashes, and expose the expected --version/app-server
// capability under an explicit environment. Probe output is bounded, used
// only for exact comparison, and never persisted or included in an error.
func (attestor *CodexAppServerRuntimeAttestor) AttestCodexAppServerOperation(ctx context.Context, attestation DeploymentOperationRuntimeAttestation) (CodexAppServerInvocation, error) {
	if attestor == nil {
		return CodexAppServerInvocation{}, ErrDeploymentOperationRuntimeAttestationUnavailable
	}
	if err := contextRuntimeAttestationError(ctx); err != nil {
		return CodexAppServerInvocation{}, err
	}
	if err := validateLocalFilesystemRuntimeAttestation(attestation); err != nil {
		return CodexAppServerInvocation{}, err
	}
	if attestation.HarborFlowBuild != attestor.harborFlowBuild {
		return CodexAppServerInvocation{}, fmt.Errorf("%w: Harbor Flow build identity does not match the installed process identity", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	payload, ok := attestation.Record.Operation.Payload.(workflowadapter.AgentTurnOperationPayload)
	if !ok || attestation.Record.CodexAppServer == nil || attestation.Record.AgentModel == nil {
		return CodexAppServerInvocation{}, fmt.Errorf("%w: no typed Codex App Server lock is installed for this operation", ErrDeploymentOperationRuntimeAttestationUnavailable)
	}
	if attestation.Record.AgentModel.AgentID != payload.AgentID || attestation.Record.AgentModel.ModelID != payload.ModelID {
		return CodexAppServerInvocation{}, fmt.Errorf("%w: locked Codex agent configuration does not match the frozen operation", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	if !IsCodexAppServerProductionPayload(payload) {
		return CodexAppServerInvocation{}, fmt.Errorf("%w: Codex App Server operation must pin model %q with reasoning effort %q", ErrDeploymentOperationRuntimeAttestationFailed, CodexAppServerProductionModelID, CodexAppServerProductionReasoningEffort)
	}
	lock := attestation.Record.CodexAppServer.Clone()
	if err := lock.Validate(); err != nil {
		return CodexAppServerInvocation{}, fmt.Errorf("%w: typed Codex App Server lock is invalid", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	if err := attestLockedRegularFile(ctx, lock.JavaScriptLauncher); err != nil {
		return CodexAppServerInvocation{}, err
	}
	if err := attestLockedExecutableFile(lock.JavaScriptLauncher); err != nil {
		return CodexAppServerInvocation{}, err
	}
	if err := attestCodexJavaScriptLauncher(lock.JavaScriptLauncher); err != nil {
		return CodexAppServerInvocation{}, err
	}
	if err := attestLockedRegularFile(ctx, lock.NodeExecutable); err != nil {
		return CodexAppServerInvocation{}, err
	}
	if err := attestLockedExecutableFile(lock.NodeExecutable); err != nil {
		return CodexAppServerInvocation{}, err
	}
	if err := attestCodexHomeDirectory(lock.CodexHomeDirectory); err != nil {
		return CodexAppServerInvocation{}, err
	}
	environment := codexAppServerEnvironment(lock.CodexHomeDirectory, lock.NodeExecutable.AbsolutePath)
	if err := attestCodexNodeVersion(ctx, lock.NodeExecutable, environment); err != nil {
		return CodexAppServerInvocation{}, err
	}
	if err := attestCodexCLICapability(ctx, lock, environment); err != nil {
		return CodexAppServerInvocation{}, err
	}
	return codexAppServerInvocationFromLock(lock, *attestation.Record.AgentModel, payload.ReasoningEffort), nil
}

func codexAppServerInvocationFromLock(lock CodexAppServerOperationLock, agent AgentModelLock, reasoningEffort workflowadapter.AgentReasoningEffort) CodexAppServerInvocation {
	return CodexAppServerInvocation{
		AgentID:                agent.AgentID,
		AgentVersion:           agent.AgentVersion,
		ModelID:                agent.ModelID,
		ModelVersion:           agent.ModelVersion,
		ReasoningEffort:        reasoningEffort,
		JavaScriptLauncherPath: lock.JavaScriptLauncher.AbsolutePath,
		NodeExecutablePath:     lock.NodeExecutable.AbsolutePath,
		CodexHomeDirectory:     lock.CodexHomeDirectory,
		CLIVersionOutput:       lock.CLIVersionOutput,
		SandboxMode:            lock.SandboxMode,
		SandboxPolicy:          lock.SandboxPolicy,
		NetworkAccess:          lock.NetworkAccess,
	}
}

// IsCodexAppServerProductionPayload verifies the one approved Standard
// authoring agent profile. The catalog, generator, attestor, and executor use
// the same predicate while still enforcing it at their own trust boundaries.
func IsCodexAppServerProductionPayload(payload workflowadapter.AgentTurnOperationPayload) bool {
	return payload.AgentID == CodexAppServerProductionAgentID && payload.ModelID == CodexAppServerProductionModelID && payload.ReasoningEffort == CodexAppServerProductionReasoningEffort
}

func validateCodexAppServerDirectoryPath(value string) error {
	if !filepath.IsAbs(value) || filepath.Clean(value) != value || value == string(filepath.Separator) {
		return fmt.Errorf("%w: Codex CODEX_HOME must be a clean non-root absolute directory", ErrInvalidDeploymentOperationCatalogLock)
	}
	if err := validateCodexAppServerEnvironmentPath("Codex CODEX_HOME", value); err != nil {
		return err
	}
	return validateOperationCatalogLockString("Codex CODEX_HOME", value)
}

func validateCodexAppServerEnvironmentPath(label, value string) error {
	if strings.ContainsRune(value, os.PathListSeparator) || strings.Contains(value, "=") {
		return fmt.Errorf("%w: %s cannot be encoded safely in the controlled environment", ErrInvalidDeploymentOperationCatalogLock, label)
	}
	return nil
}

func validateCodexAppServerCLIVersionOutput(value, launcherVersion string) error {
	if value == "" || value != strings.TrimSpace(value) || strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("%w: Codex CLI version output must be one exact non-empty line", ErrInvalidDeploymentOperationCatalogLock)
	}
	fields := strings.Fields(value)
	if len(fields) != 2 || fields[0] != codexAppServerExpectedVersionLabel || fields[1] != launcherVersion {
		return fmt.Errorf("%w: Codex CLI version output must exactly bind %s to the pinned launcher version", ErrInvalidDeploymentOperationCatalogLock, codexAppServerExpectedVersionLabel)
	}
	return nil
}

func validateCodexAppServerVersion(label, value string, requireLeadingV bool) error {
	if err := validateOperationCatalogLockVersion(label, value); err != nil {
		return err
	}
	if requireLeadingV {
		if !strings.HasPrefix(value, "v") {
			return fmt.Errorf("%w: %s version must use Node's v-prefixed semantic version form", ErrInvalidDeploymentOperationCatalogLock, label)
		}
		value = strings.TrimPrefix(value, "v")
	} else if strings.HasPrefix(value, "v") {
		return fmt.Errorf("%w: %s version must use Codex CLI's unprefixed semantic version form", ErrInvalidDeploymentOperationCatalogLock, label)
	}
	if !validCodexAppServerSemver(value) {
		return fmt.Errorf("%w: %s version is not a concrete semantic version", ErrInvalidDeploymentOperationCatalogLock, label)
	}
	return nil
}

func validCodexAppServerSemver(value string) bool {
	if value == "" || strings.ContainsAny(value, "+ \t\r\n") {
		return false
	}
	core, prerelease, hasPrerelease := strings.Cut(value, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, character := range part {
			if character < '0' || character > '9' {
				return false
			}
		}
	}
	if !hasPrerelease {
		return true
	}
	if prerelease == "" {
		return false
	}
	for _, character := range prerelease {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '.' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func attestLockedExecutableFile(locked LocalExecutableLock) error {
	info, err := inspectLockedLocalExecutablePath(locked.AbsolutePath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return fmt.Errorf("%w: locked Codex runtime file is not an executable regular file", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	return nil
}

func attestCodexJavaScriptLauncher(launcher LocalExecutableLock) error {
	file, err := os.Open(launcher.AbsolutePath)
	if err != nil {
		return fmt.Errorf("%w: locked Codex JavaScript launcher cannot be opened for shebang verification", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	defer file.Close()
	line, err := readBoundedShebangLine(file, codexAppServerShebangLimit)
	if err != nil {
		return fmt.Errorf("%w: locked Codex JavaScript launcher shebang cannot be read", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	if !bytes.Equal(line, []byte("#!/usr/bin/env node")) {
		return fmt.Errorf("%w: locked Codex JavaScript launcher must use the strict /usr/bin/env node shebang", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	return nil
}

func attestCodexHomeDirectory(home string) error {
	info, err := inspectLockedLocalExecutablePath(home)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("%w: locked Codex CODEX_HOME is not an existing non-symlink directory", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	return nil
}

func attestCodexNodeVersion(ctx context.Context, node LocalExecutableLock, environment []string) error {
	output, err := runCodexAppServerProbe(ctx, node.AbsolutePath, environment, "--version")
	if err != nil {
		return err
	}
	version, ok := normalizeCodexAppServerProbeLine(output)
	if !ok || version != node.Version {
		return fmt.Errorf("%w: locked Codex Node --version output does not match the immutable lock", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	return nil
}

func attestCodexCLICapability(ctx context.Context, lock CodexAppServerOperationLock, environment []string) error {
	versionOutput, err := runCodexAppServerProbe(ctx, lock.JavaScriptLauncher.AbsolutePath, environment, "--version")
	if err != nil {
		return err
	}
	version, ok := normalizeCodexAppServerProbeLine(versionOutput)
	if !ok || version != lock.CLIVersionOutput {
		return fmt.Errorf("%w: locked Codex CLI --version output does not match the immutable lock", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	help, err := runCodexAppServerProbe(ctx, lock.JavaScriptLauncher.AbsolutePath, environment, "app-server", "--help")
	if err != nil {
		return err
	}
	helpText := string(help)
	if !strings.Contains(helpText, "--listen") || (!strings.Contains(helpText, "--config") && !strings.Contains(helpText, "-c,")) {
		return fmt.Errorf("%w: locked Codex CLI does not expose the required app-server capability", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	return contextRuntimeAttestationError(ctx)
}

func runCodexAppServerProbe(ctx context.Context, commandPath string, environment []string, arguments ...string) ([]byte, error) {
	if err := contextRuntimeAttestationError(ctx); err != nil {
		return nil, err
	}
	probeContext, cancel := context.WithTimeout(ctx, codexAppServerProbeTimeout)
	defer cancel()
	command := exec.CommandContext(probeContext, commandPath, arguments...)
	command.Dir = filepath.Dir(commandPath)
	command.Env = append([]string(nil), environment...)
	output := &codexAppServerLimitedBuffer{limit: codexAppServerProbeOutputLimit}
	command.Stdout = output
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		if probeContext.Err() != nil {
			return nil, fmt.Errorf("%w: Codex capability probe context ended: %w", ErrDeploymentOperationRuntimeAttestationFailed, probeContext.Err())
		}
		return nil, fmt.Errorf("%w: locked Codex capability probe failed", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	if err := contextRuntimeAttestationError(ctx); err != nil {
		return nil, err
	}
	return append([]byte(nil), output.Bytes()...), nil
}

type codexAppServerLimitedBuffer struct {
	limit int
	bytes.Buffer
}

func (buffer *codexAppServerLimitedBuffer) Write(value []byte) (int, error) {
	if buffer == nil || buffer.limit < 0 || buffer.Len()+len(value) > buffer.limit {
		return 0, errors.New("Codex capability probe output exceeds bounded attestation buffer")
	}
	return buffer.Buffer.Write(value)
}

func normalizeCodexAppServerProbeLine(raw []byte) (string, bool) {
	value := string(raw)
	value = strings.TrimSuffix(value, "\n")
	value = strings.TrimSuffix(value, "\r")
	if value == "" || value != strings.TrimSpace(value) || strings.ContainsAny(value, "\r\n") {
		return "", false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return "", false
		}
	}
	return value, true
}

func codexAppServerEnvironment(home, nodePath string) []string {
	return []string{
		"CODEX_HOME=" + home,
		"PATH=" + codexAppServerControlledPATH(nodePath),
	}
}

func codexAppServerControlledPATH(nodePath string) string {
	return strings.Join([]string{
		filepath.Dir(nodePath),
		codexAppServerSystemPathFirst,
		codexAppServerSystemPathSecond,
		codexAppServerSystemPathThird,
	}, string(os.PathListSeparator))
}

var _ DeploymentOperationRuntimeAttestor = (*CodexAppServerRuntimeAttestor)(nil)
