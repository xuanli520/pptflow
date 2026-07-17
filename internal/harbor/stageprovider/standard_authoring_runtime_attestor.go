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

	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	// StandardAuthoringGitSnapshotCommandID is the only local.command identity
	// this attestor accepts for a source-snapshot preparation operation. The
	// matching absolute Git file and version live in LocalExecutableLock.
	StandardAuthoringGitSnapshotCommandID = "standard-authoring.git-snapshot"
	// StandardAuthoringDockerCommandID is the one controlled Docker client
	// identity shared by Docker-dependent authoring verification stages. It
	// does not approve an image: a task-specific image policy remains a
	// separate immutable handler contract.
	StandardAuthoringDockerCommandID = "standard-authoring.docker"

	standardAuthoringHostProbeTimeout     = 15 * time.Second
	standardAuthoringHostProbeOutputLimit = 64 * 1024
	// A prompt/schema deployment asset is configuration, not a task artifact.
	// Bound reads keep a replaced file from turning runtime attestation into an
	// unbounded memory allocation. Larger assets require a reviewed contract
	// revision rather than silently changing this operational envelope.
	standardAuthoringContractAssetReadLimit = 4 * 1024 * 1024
)

// StandardAuthoringHostCommandVersionProber is the narrow, controlled probe
// port used by StandardAuthoringRuntimeAttestor. The default implementation
// invokes only a locked absolute executable with `--version`, a bounded
// timeout/output budget, and a fixed secret-free environment. It exists as a
// test seam; production composition must use the default or an equivalently
// strict deployment-owned implementation.
type StandardAuthoringHostCommandVersionProber interface {
	ProbeHostCommandVersion(context.Context, LocalExecutableLock) (string, error)
	ProbeDockerDaemonVersion(context.Context, LocalExecutableLock) (clientVersion string, serverVersion string, err error)
}

// StandardAuthoringRuntimeAttestorConfig supplies the process identity shared
// by the controlled authoring capabilities and the immutable deployment root
// which contains the prompt/schema contract assets. ContractRoot is a
// deployment-owned absolute directory, never a Run workspace path. The typed
// lock supplies only safe relative paths below it and every effect rechecks
// that containment plus the locked raw SHA-256 values.
type StandardAuthoringRuntimeAttestorConfig struct {
	HarborFlowBuild  HarborFlowBuildIdentity
	HostCommandProbe StandardAuthoringHostCommandVersionProber
	ContractRoot     string
}

// StandardAuthoringRuntimeAttestor routes an already static-verified Standard
// authoring operation to the correct typed proof:
//
//   - local.command: locked filesystem bytes plus a bounded Git/Docker version
//     probe;
//   - agent.turn: the existing closed Codex App Server attestor;
//   - harbor.builtin: the exact versioned handler and linked Harbor Flow build;
//   - durable.review: a checked policy identity with no external process.
//
// container.command is deliberately unavailable. No Docker image digest or
// container ABI has been approved by this generic authoring contract, so it
// must never become a fallback for a generated task Dockerfile.
type StandardAuthoringRuntimeAttestor struct {
	harborFlowBuild HarborFlowBuildIdentity
	local           *LocalFilesystemRuntimeAttestor
	codex           *CodexAppServerRuntimeAttestor
	hostProbe       StandardAuthoringHostCommandVersionProber
	contractRoot    string
}

// NewStandardAuthoringRuntimeAttestor constructs the fail-closed multi-kind
// attestor. It creates both child attestors eagerly so a partially configured
// composition cannot discover a missing capability only after a worker has
// acquired an external-effect claim.
func NewStandardAuthoringRuntimeAttestor(config StandardAuthoringRuntimeAttestorConfig) (*StandardAuthoringRuntimeAttestor, error) {
	if err := config.HarborFlowBuild.Validate(); err != nil {
		return nil, fmt.Errorf("%w: configured Harbor Flow build identity is invalid: %w", ErrDeploymentOperationRuntimeAttestationFailed, err)
	}
	if err := validateStandardAuthoringContractRoot(config.ContractRoot); err != nil {
		return nil, err
	}
	local, err := NewLocalFilesystemRuntimeAttestor(LocalFilesystemRuntimeAttestorConfig{HarborFlowBuild: config.HarborFlowBuild})
	if err != nil {
		return nil, err
	}
	codex, err := NewCodexAppServerRuntimeAttestor(CodexAppServerRuntimeAttestorConfig{HarborFlowBuild: config.HarborFlowBuild})
	if err != nil {
		return nil, err
	}
	probe := config.HostCommandProbe
	if probe == nil {
		probe = defaultStandardAuthoringHostCommandVersionProber{}
	}
	return &StandardAuthoringRuntimeAttestor{
		harborFlowBuild: config.HarborFlowBuild, local: local, codex: codex, hostProbe: probe, contractRoot: config.ContractRoot,
	}, nil
}

// HarborFlowBuild returns the immutable process identity selected at
// composition. Returning a value copy prevents a caller from altering an
// installed attestor after construction.
func (attestor *StandardAuthoringRuntimeAttestor) HarborFlowBuild() HarborFlowBuildIdentity {
	if attestor == nil {
		return HarborFlowBuildIdentity{}
	}
	return attestor.harborFlowBuild
}

// ContractRoot returns the immutable deployment root selected at composition.
// It is only a path identity; callers still receive no authority to bypass
// the runtime no-symlink and content-hash checks.
func (attestor *StandardAuthoringRuntimeAttestor) ContractRoot() string {
	if attestor == nil {
		return ""
	}
	return attestor.contractRoot
}

// AttestDeploymentOperation implements the generic catalog-lock boundary.
// It intentionally returns no executable/path/configuration to a provider;
// an agent handler that needs the secret-free Codex invocation may call the
// dedicated AttestCodexAppServerOperation method with the same frozen
// attestation record.
func (attestor *StandardAuthoringRuntimeAttestor) AttestDeploymentOperation(ctx context.Context, attestation DeploymentOperationRuntimeAttestation) error {
	if err := attestor.validateBase(ctx, attestation); err != nil {
		return err
	}
	if err := attestor.attestStandardAuthoringContract(ctx, attestation); err != nil {
		return err
	}
	switch payload := attestation.Record.Operation.Payload.(type) {
	case workflowadapter.LocalCommandOperationPayload:
		if err := attestor.local.AttestDeploymentOperation(ctx, attestation); err != nil {
			return err
		}
		return attestor.attestHostCommand(ctx, payload, *attestation.Record.LocalExecutable)
	case workflowadapter.AgentTurnOperationPayload:
		_, err := attestor.attestCodexAppServerOperationAfterBase(ctx, attestation)
		return err
	case workflowadapter.HarborBuiltinOperationPayload:
		if attestation.Record.HarborFlowBuiltin == nil || attestation.Record.HarborFlowBuiltin.HandlerID != payload.HandlerID {
			return fmt.Errorf("%w: Harbor built-in handler lock is inconsistent", ErrDeploymentOperationRuntimeAttestationFailed)
		}
		// validateBase has already compared the enclosing build identity and
		// Record.Validate has checked the typed handler/version lock. There is
		// intentionally no executable process to probe for a built-in handler.
		return nil
	case workflowadapter.DurableReviewOperationPayload:
		if attestation.Record.DurableReviewPolicy == nil || attestation.Record.DurableReviewPolicy.PolicyID != payload.PolicyID {
			return fmt.Errorf("%w: durable review policy lock is inconsistent", ErrDeploymentOperationRuntimeAttestationFailed)
		}
		return nil
	case workflowadapter.ContainerCommandOperationPayload:
		return fmt.Errorf("%w: Standard authoring has no approved container.command attestor", ErrDeploymentOperationRuntimeAttestationUnavailable)
	default:
		return fmt.Errorf("%w: unsupported Standard authoring operation payload", ErrDeploymentOperationRuntimeAttestationUnavailable)
	}
}

// AttestCodexAppServerOperation performs the same base checks as the generic
// boundary and then returns the secret-free, lock-derived Codex invocation.
// Provider compositions can use it to construct an App Server runtime without
// discovering a path or merging ambient environment values.
func (attestor *StandardAuthoringRuntimeAttestor) AttestCodexAppServerOperation(ctx context.Context, attestation DeploymentOperationRuntimeAttestation) (CodexAppServerInvocation, error) {
	if err := attestor.validateBase(ctx, attestation); err != nil {
		return CodexAppServerInvocation{}, err
	}
	if err := attestor.attestStandardAuthoringContract(ctx, attestation); err != nil {
		return CodexAppServerInvocation{}, err
	}
	return attestor.attestCodexAppServerOperationAfterBase(ctx, attestation)
}

func (attestor *StandardAuthoringRuntimeAttestor) attestCodexAppServerOperationAfterBase(ctx context.Context, attestation DeploymentOperationRuntimeAttestation) (CodexAppServerInvocation, error) {
	if _, ok := attestation.Record.Operation.Payload.(workflowadapter.AgentTurnOperationPayload); !ok {
		return CodexAppServerInvocation{}, fmt.Errorf("%w: operation is not an agent.turn", ErrDeploymentOperationRuntimeAttestationUnavailable)
	}
	if attestor.codex == nil {
		return CodexAppServerInvocation{}, ErrDeploymentOperationRuntimeAttestationUnavailable
	}
	return attestor.codex.AttestCodexAppServerOperation(ctx, attestation)
}

func (attestor *StandardAuthoringRuntimeAttestor) validateBase(ctx context.Context, attestation DeploymentOperationRuntimeAttestation) error {
	if attestor == nil || attestor.local == nil || attestor.hostProbe == nil || attestor.contractRoot == "" {
		return ErrDeploymentOperationRuntimeAttestationUnavailable
	}
	if err := contextRuntimeAttestationError(ctx); err != nil {
		return err
	}
	if err := validateLocalFilesystemRuntimeAttestation(attestation); err != nil {
		return err
	}
	if attestation.HarborFlowBuild != attestor.harborFlowBuild {
		return fmt.Errorf("%w: Harbor Flow build identity does not match the installed process identity", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	return nil
}

// attestStandardAuthoringContract reopens both immutable prompt and schema
// files before every external effect for the only template that is allowed to
// carry this extension. It is intentionally invoked by the direct Codex
// attestation entry point too, so a handler cannot bypass drift detection by
// using the secret-free invocation helper rather than the generic wrapper.
func (attestor *StandardAuthoringRuntimeAttestor) attestStandardAuthoringContract(ctx context.Context, attestation DeploymentOperationRuntimeAttestation) error {
	if !workflowadapter.IsStandardAuthoringWorkflowTemplate(attestation.CatalogReceipt.Template) {
		if attestation.Record.StandardAuthoringContract != nil {
			return fmt.Errorf("%w: Standard authoring contract is bound to a different template", ErrDeploymentOperationRuntimeAttestationFailed)
		}
		return nil
	}
	_, err := attestor.ReadStandardAuthoringContractAssets(ctx, attestation)
	return err
}

// StandardAuthoringContractAssetContents is the verified in-memory content of
// one immutable deployment asset. It exposes its canonical identity and raw
// content SHA-256 but deliberately never exposes the deployment path; a
// handler can parse the bytes without gaining a filesystem escape hatch.
type StandardAuthoringContractAssetContents struct {
	ID            string                  `json:"id"`
	Version       string                  `json:"version"`
	Content       []byte                  `json:"-"`
	ContentSHA256 workflowkit.Fingerprint `json:"content_sha256"`
}

// Clone returns independently owned asset bytes.
func (contents StandardAuthoringContractAssetContents) Clone() StandardAuthoringContractAssetContents {
	contents.Content = append([]byte(nil), contents.Content...)
	return contents
}

// StandardAuthoringContractAssets is the pair of verified prompt and schema
// assets for one frozen Standard authoring operation.
type StandardAuthoringContractAssets struct {
	Prompt StandardAuthoringContractAssetContents `json:"prompt"`
	Schema StandardAuthoringContractAssetContents `json:"schema"`
}

// Clone returns independently owned asset bytes.
func (assets StandardAuthoringContractAssets) Clone() StandardAuthoringContractAssets {
	assets.Prompt = assets.Prompt.Clone()
	assets.Schema = assets.Schema.Clone()
	return assets
}

// ReadStandardAuthoringContractAssets safely opens, bounds, fingerprints, and
// returns the two exact deployment assets for one frozen authoring operation.
// It is intended for deployment composition to decode a prevalidated Codex
// prompt program or another typed handler contract. It is not a Run-input
// file API: the root and relative paths both remain lock-controlled, every
// path component is non-symlinked, and no path is returned to the caller.
func (attestor *StandardAuthoringRuntimeAttestor) ReadStandardAuthoringContractAssets(ctx context.Context, attestation DeploymentOperationRuntimeAttestation) (StandardAuthoringContractAssets, error) {
	if err := attestor.validateBase(ctx, attestation); err != nil {
		return StandardAuthoringContractAssets{}, err
	}
	if !workflowadapter.IsStandardAuthoringWorkflowTemplate(attestation.CatalogReceipt.Template) {
		return StandardAuthoringContractAssets{}, fmt.Errorf("%w: operation is not bound to the Standard authoring template", ErrDeploymentOperationRuntimeAttestationUnavailable)
	}
	if attestation.Record.StandardAuthoringContract == nil {
		return StandardAuthoringContractAssets{}, fmt.Errorf("%w: Standard authoring operation lacks typed prompt/schema contract", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	contract := attestation.Record.StandardAuthoringContract.Clone()
	if err := contract.Validate(); err != nil {
		return StandardAuthoringContractAssets{}, fmt.Errorf("%w: Standard authoring prompt/schema contract is invalid", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	prompt, err := readStandardAuthoringContractAsset(ctx, attestor.contractRoot, contract.Prompt, attestation.Record.PromptContentFingerprint)
	if err != nil {
		return StandardAuthoringContractAssets{}, err
	}
	schema, err := readStandardAuthoringContractAsset(ctx, attestor.contractRoot, contract.Schema, attestation.Record.SchemaContentFingerprint)
	if err != nil {
		return StandardAuthoringContractAssets{}, err
	}
	return StandardAuthoringContractAssets{Prompt: prompt, Schema: schema}.Clone(), nil
}

func validateStandardAuthoringContractRoot(root string) error {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || root == string(filepath.Separator) {
		return fmt.Errorf("%w: Standard authoring contract root must be a clean non-root absolute path", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	if err := validateOperationCatalogLockString("Standard authoring contract root", root); err != nil {
		return fmt.Errorf("%w: Standard authoring contract root is invalid", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	info, err := inspectStandardAuthoringContractPath(root)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("%w: Standard authoring contract root is not an existing non-symlink directory", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	return nil
}

func readStandardAuthoringContractAsset(ctx context.Context, root string, reference StandardAuthoringContractAssetReference, expected workflowkit.Fingerprint) (StandardAuthoringContractAssetContents, error) {
	if err := contextRuntimeAttestationError(ctx); err != nil {
		return StandardAuthoringContractAssetContents{}, err
	}
	if err := reference.Validate(); err != nil {
		return StandardAuthoringContractAssetContents{}, fmt.Errorf("%w: Standard authoring contract asset reference is invalid", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	if err := expected.Validate(); err != nil {
		return StandardAuthoringContractAssetContents{}, fmt.Errorf("%w: Standard authoring contract asset fingerprint is invalid", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	if err := validateStandardAuthoringContractRoot(root); err != nil {
		return StandardAuthoringContractAssetContents{}, err
	}
	assetPath, err := standardAuthoringContractAssetPath(root, reference.RelativePath)
	if err != nil {
		return StandardAuthoringContractAssetContents{}, err
	}
	initial, err := inspectStandardAuthoringContractPath(assetPath)
	if err != nil || !initial.Mode().IsRegular() {
		return StandardAuthoringContractAssetContents{}, fmt.Errorf("%w: Standard authoring contract asset cannot be inspected", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	if initial.Size() < 0 || initial.Size() > standardAuthoringContractAssetReadLimit {
		return StandardAuthoringContractAssetContents{}, fmt.Errorf("%w: Standard authoring contract asset exceeds the fixed read limit", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	file, err := os.Open(assetPath)
	if err != nil {
		return StandardAuthoringContractAssetContents{}, fmt.Errorf("%w: Standard authoring contract asset cannot be opened", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(initial, opened) {
		return StandardAuthoringContractAssetContents{}, fmt.Errorf("%w: Standard authoring contract asset changed while opening", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	if opened.Size() < 0 || opened.Size() > standardAuthoringContractAssetReadLimit {
		return StandardAuthoringContractAssetContents{}, fmt.Errorf("%w: Standard authoring contract asset exceeds the fixed read limit", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	contents, err := io.ReadAll(io.LimitReader(file, standardAuthoringContractAssetReadLimit+1))
	if err != nil || len(contents) > standardAuthoringContractAssetReadLimit {
		return StandardAuthoringContractAssetContents{}, fmt.Errorf("%w: Standard authoring contract asset cannot be read", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	if err := contextRuntimeAttestationError(ctx); err != nil {
		return StandardAuthoringContractAssetContents{}, err
	}
	finalFile, err := file.Stat()
	if err != nil || !finalFile.Mode().IsRegular() || !os.SameFile(opened, finalFile) || finalFile.Size() != opened.Size() {
		return StandardAuthoringContractAssetContents{}, fmt.Errorf("%w: Standard authoring contract asset changed while reading", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	finalPath, err := inspectStandardAuthoringContractPath(assetPath)
	if err != nil || !finalPath.Mode().IsRegular() || !os.SameFile(opened, finalPath) || finalPath.Size() != opened.Size() {
		return StandardAuthoringContractAssetContents{}, fmt.Errorf("%w: Standard authoring contract asset path changed while reading", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	fingerprint := workflowkit.SHA256Fingerprint(contents)
	if fingerprint != expected {
		return StandardAuthoringContractAssetContents{}, fmt.Errorf("%w: Standard authoring contract asset content fingerprint does not match", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	return StandardAuthoringContractAssetContents{ID: reference.ID, Version: reference.Version, Content: append([]byte(nil), contents...), ContentSHA256: fingerprint}, nil
}

func standardAuthoringContractAssetPath(root, relativePath string) (string, error) {
	if err := validateStandardAuthoringContractRelativePath(relativePath); err != nil {
		return "", fmt.Errorf("%w: Standard authoring contract asset path is invalid", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	assetPath := filepath.Join(root, filepath.FromSlash(relativePath))
	relative, err := filepath.Rel(root, assetPath)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", fmt.Errorf("%w: Standard authoring contract asset escapes contract root", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	return assetPath, nil
}

// inspectStandardAuthoringContractPath walks every component from filesystem
// root to the requested path. Checking only the leaf would allow a symlinked
// deployment directory to redirect a correctly named prompt/schema file.
func inspectStandardAuthoringContractPath(absolutePath string) (os.FileInfo, error) {
	components := make([]string, 0, 8)
	for current := absolutePath; ; {
		components = append(components, current)
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	var finalInfo os.FileInfo
	for index := len(components) - 1; index >= 0; index-- {
		info, err := os.Lstat(components[index])
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("invalid Standard authoring contract path component")
		}
		if index > 0 && !info.IsDir() {
			return nil, errors.New("non-directory Standard authoring contract path component")
		}
		if index == 0 {
			finalInfo = info
		}
	}
	return finalInfo, nil
}

func (attestor *StandardAuthoringRuntimeAttestor) attestHostCommand(ctx context.Context, payload workflowadapter.LocalCommandOperationPayload, locked LocalExecutableLock) error {
	if attestor == nil || attestor.hostProbe == nil {
		return ErrDeploymentOperationRuntimeAttestationUnavailable
	}
	if len(payload.Arguments) != 0 {
		return fmt.Errorf("%w: Standard authoring host command must not accept caller argv", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	version, err := attestor.hostProbe.ProbeHostCommandVersion(ctx, locked)
	if err != nil {
		return fmt.Errorf("%w: locked host command version probe failed", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	switch payload.CommandID {
	case StandardAuthoringGitSnapshotCommandID:
		if version != "git version "+locked.Version {
			return fmt.Errorf("%w: locked Git version does not match", ErrDeploymentOperationRuntimeAttestationFailed)
		}
	case StandardAuthoringDockerCommandID:
		if !strings.HasPrefix(version, "Docker version "+locked.Version+", build ") || len(strings.TrimPrefix(version, "Docker version "+locked.Version+", build ")) == 0 {
			return fmt.Errorf("%w: locked Docker client version does not match", ErrDeploymentOperationRuntimeAttestationFailed)
		}
		clientVersion, serverVersion, err := attestor.hostProbe.ProbeDockerDaemonVersion(ctx, locked)
		if err != nil || clientVersion != locked.Version || serverVersion != locked.Version {
			return fmt.Errorf("%w: locked Docker daemon version does not match", ErrDeploymentOperationRuntimeAttestationFailed)
		}
	default:
		return fmt.Errorf("%w: local command %q is not approved for Standard authoring", ErrDeploymentOperationRuntimeAttestationUnavailable, payload.CommandID)
	}
	return nil
}

type defaultStandardAuthoringHostCommandVersionProber struct{}

func (defaultStandardAuthoringHostCommandVersionProber) ProbeHostCommandVersion(ctx context.Context, locked LocalExecutableLock) (string, error) {
	value, err := runStandardAuthoringHostProbe(ctx, locked, "--version")
	if err != nil {
		return "", err
	}
	if value == "" || strings.Contains(value, "\r") {
		return "", errors.New("locked host command version output is invalid")
	}
	if strings.Contains(value, "\n") {
		return "", errors.New("locked host command version output is not one line")
	}
	return value, nil
}

func (defaultStandardAuthoringHostCommandVersionProber) ProbeDockerDaemonVersion(ctx context.Context, locked LocalExecutableLock) (clientVersion string, serverVersion string, err error) {
	value, err := runStandardAuthoringHostProbe(ctx, locked, "version", "--format", "{{.Client.Version}} {{.Server.Version}}")
	if err != nil || value == "" || strings.ContainsAny(value, "\r\n") {
		return "", "", errors.New("locked Docker daemon version output is invalid")
	}
	parts := strings.Fields(value)
	if len(parts) != 2 {
		return "", "", errors.New("locked Docker daemon version output is incomplete")
	}
	return parts[0], parts[1], nil
}

func runStandardAuthoringHostProbe(ctx context.Context, locked LocalExecutableLock, arguments ...string) (string, error) {
	if err := contextRuntimeAttestationError(ctx); err != nil {
		return "", err
	}
	probeCtx, cancel := context.WithTimeout(ctx, standardAuthoringHostProbeTimeout)
	defer cancel()
	command := exec.CommandContext(probeCtx, locked.AbsolutePath, arguments...)
	command.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin"}
	var stdout, stderr standardAuthoringBoundedBuffer
	stdout.limit = standardAuthoringHostProbeOutputLimit
	stderr.limit = standardAuthoringHostProbeOutputLimit
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil || stdout.exceeded || stderr.exceeded || probeCtx.Err() != nil {
		return "", errors.New("locked host command version probe failed")
	}
	value := string(stdout.bytes())
	value = strings.TrimSuffix(value, "\n")
	return value, nil
}

type standardAuthoringBoundedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
}

func (buffer *standardAuthoringBoundedBuffer) Write(value []byte) (int, error) {
	if buffer == nil || buffer.limit <= 0 {
		return 0, errors.New("bounded output buffer is unavailable")
	}
	remaining := buffer.limit - buffer.buffer.Len()
	if remaining <= 0 {
		buffer.exceeded = true
		return len(value), nil
	}
	if len(value) > remaining {
		_, _ = buffer.buffer.Write(value[:remaining])
		buffer.exceeded = true
		return len(value), nil
	}
	return buffer.buffer.Write(value)
}

func (buffer *standardAuthoringBoundedBuffer) bytes() []byte {
	if buffer == nil {
		return nil
	}
	return append([]byte(nil), buffer.buffer.Bytes()...)
}

var _ DeploymentOperationRuntimeAttestor = (*StandardAuthoringRuntimeAttestor)(nil)
var _ StandardAuthoringHostCommandVersionProber = defaultStandardAuthoringHostCommandVersionProber{}
