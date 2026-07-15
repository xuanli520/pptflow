package stageprovider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// LocalFilesystemRuntimeAttestorConfig supplies the trusted identity of the
// Harbor Flow process that is about to execute a controlled local command.
// The identity must come from deployment-controlled build metadata, not from
// a Run, the command environment, or an executable discovered through PATH.
type LocalFilesystemRuntimeAttestorConfig struct {
	HarborFlowBuild HarborFlowBuildIdentity
}

// LocalFilesystemRuntimeAttestor proves the local-command portion of a
// deployment operation lock immediately before an executor runs. It performs
// no process execution, PATH lookup, environment/secret lookup, network I/O,
// image lookup, or provider default selection.
//
// This attestor deliberately has no implicit verifier for container.command,
// agent.turn, or durable.review. Those payloads remain unavailable until a
// separate, explicit controlled verifier is installed in the worker
// composition; accepting them here would turn a local filesystem check into a
// production fallback.
type LocalFilesystemRuntimeAttestor struct {
	harborFlowBuild HarborFlowBuildIdentity
}

// NewLocalFilesystemRuntimeAttestor creates an immutable local runtime
// attestor. A malformed or unversioned build identity is rejected before the
// attestor can be installed in an execution composition.
func NewLocalFilesystemRuntimeAttestor(config LocalFilesystemRuntimeAttestorConfig) (*LocalFilesystemRuntimeAttestor, error) {
	if err := config.HarborFlowBuild.Validate(); err != nil {
		return nil, fmt.Errorf("%w: configured Harbor Flow build identity is invalid: %w", ErrDeploymentOperationRuntimeAttestationFailed, err)
	}
	return &LocalFilesystemRuntimeAttestor{harborFlowBuild: config.HarborFlowBuild}, nil
}

// HarborFlowBuild returns the immutable trusted process identity configured
// for this attestor. HarborFlowBuildIdentity has no mutable reference fields.
func (attestor *LocalFilesystemRuntimeAttestor) HarborFlowBuild() HarborFlowBuildIdentity {
	if attestor == nil {
		return HarborFlowBuildIdentity{}
	}
	return attestor.harborFlowBuild
}

// AttestDeploymentOperation verifies that the lock-provided build identity
// exactly matches the installed Harbor Flow process and that a local.command
// still resolves to the exact regular file whose bytes are pinned in the
// operation lock. It rejects symbolic links in every path component and never
// falls back to a command name or PATH lookup.
func (attestor *LocalFilesystemRuntimeAttestor) AttestDeploymentOperation(ctx context.Context, attestation DeploymentOperationRuntimeAttestation) error {
	if attestor == nil {
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

	switch payload := attestation.Record.Operation.Payload.(type) {
	case workflowadapter.LocalCommandOperationPayload:
		if attestation.Record.LocalExecutable == nil || attestation.Record.LocalExecutable.CommandID != payload.CommandID {
			// Record.Validate above should make this unreachable. Keep the
			// execution boundary fail-closed if a future lock type changes.
			return fmt.Errorf("%w: local command attestation record is inconsistent", ErrDeploymentOperationRuntimeAttestationFailed)
		}
		return attestLockedRegularFile(ctx, *attestation.Record.LocalExecutable)
	case workflowadapter.ContainerCommandOperationPayload,
		workflowadapter.AgentTurnOperationPayload,
		workflowadapter.DurableReviewOperationPayload,
		workflowadapter.HarborBuiltinOperationPayload:
		return fmt.Errorf("%w: no explicit controlled verifier is installed for %s", ErrDeploymentOperationRuntimeAttestationUnavailable, attestation.Record.ExecutionKind)
	default:
		return fmt.Errorf("%w: unsupported deployment operation payload", ErrDeploymentOperationRuntimeAttestationFailed)
	}
}

// validateLocalFilesystemRuntimeAttestation rejects malformed or mismatched
// evidence even when this attestor is called directly rather than through the
// catalog-lock wrapper. Error messages intentionally avoid operation paths,
// secret references, and arbitrary lock values.
func validateLocalFilesystemRuntimeAttestation(attestation DeploymentOperationRuntimeAttestation) error {
	if err := attestation.CatalogReceipt.Validate(); err != nil {
		return fmt.Errorf("%w: catalog receipt is invalid", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	if err := attestation.LockIdentity.Validate(); err != nil {
		return fmt.Errorf("%w: lock identity is invalid", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	if err := attestation.HarborFlowBuild.Validate(); err != nil {
		return fmt.Errorf("%w: Harbor Flow build identity is invalid", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	if err := attestation.Record.Validate(); err != nil {
		return fmt.Errorf("%w: deployment operation lock record is invalid", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	if err := validateDeploymentOperationResolution(attestation.Resolution); err != nil {
		return fmt.Errorf("%w: frozen operation resolution is invalid", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	if deploymentCoordinateForLockRecord(attestation.Record) != deploymentCoordinateForResolution(attestation.Resolution) {
		return fmt.Errorf("%w: deployment operation coordinate does not match frozen resolution", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	if attestation.Record.Stage.Key != attestation.Resolution.StageKey ||
		attestation.Record.Stage.Type != attestation.Resolution.StageType ||
		attestation.Record.Stage.Plugin != attestation.Resolution.Plugin ||
		attestation.Record.Provider != attestation.Resolution.Provider ||
		attestation.Record.Runtime != attestation.Resolution.Runtime ||
		attestation.Record.Checkout.ID != attestation.Resolution.Checkout.ID ||
		!sameDeploymentSecrets(attestation.Record.Secrets, attestation.Resolution.Secrets) {
		return fmt.Errorf("%w: deployment operation contract does not match frozen resolution", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	lockedPayload, err := canonicalOperationBindingPayload(attestation.Record.Operation)
	if err != nil {
		return fmt.Errorf("%w: locked operation payload is invalid", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	resolvedPayload, err := canonicalOperationBindingPayload(attestation.Resolution.Operation)
	if err != nil {
		return fmt.Errorf("%w: frozen operation payload is invalid", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	if !bytes.Equal(lockedPayload, resolvedPayload) {
		return fmt.Errorf("%w: locked operation payload does not match frozen resolution", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	return nil
}

// attestLockedRegularFile computes a raw SHA-256 over the exact local file
// addressed by the lock. Lstat rejects symbolic links before opening; the
// opened descriptor is then checked against the original and final path
// metadata so a path replacement cannot become an implicit redirect.
func attestLockedRegularFile(ctx context.Context, locked LocalExecutableLock) error {
	if err := contextRuntimeAttestationError(ctx); err != nil {
		return err
	}
	if err := validateLocalExecutableLock(locked); err != nil {
		return fmt.Errorf("%w: local executable lock is invalid", ErrDeploymentOperationRuntimeAttestationFailed)
	}

	initialInfo, err := inspectLockedLocalExecutablePath(locked.AbsolutePath)
	if err != nil {
		return fmt.Errorf("%w: locked local executable cannot be inspected", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	if !initialInfo.Mode().IsRegular() {
		return fmt.Errorf("%w: locked local executable is not a regular file", ErrDeploymentOperationRuntimeAttestationFailed)
	}

	file, err := os.Open(locked.AbsolutePath)
	if err != nil {
		return fmt.Errorf("%w: locked local executable cannot be opened", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	defer file.Close()

	openedInfo, err := file.Stat()
	if err != nil {
		return fmt.Errorf("%w: opened local executable cannot be inspected", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(initialInfo, openedInfo) {
		return fmt.Errorf("%w: locked local executable changed while being opened", ErrDeploymentOperationRuntimeAttestationFailed)
	}

	fingerprint, err := fingerprintOpenRegularFile(ctx, file)
	if err != nil {
		return err
	}
	finalFileInfo, err := file.Stat()
	if err != nil || !finalFileInfo.Mode().IsRegular() || !os.SameFile(openedInfo, finalFileInfo) {
		return fmt.Errorf("%w: opened local executable changed while being read", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	finalPathInfo, err := inspectLockedLocalExecutablePath(locked.AbsolutePath)
	if err != nil || !finalPathInfo.Mode().IsRegular() || !os.SameFile(openedInfo, finalPathInfo) {
		return fmt.Errorf("%w: locked local executable path changed while being read", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	if fingerprint != locked.ContentSHA256 {
		return fmt.Errorf("%w: locked local executable content fingerprint does not match", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	return nil
}

// inspectLockedLocalExecutablePath walks from the filesystem root to the
// final path with Lstat. Checking only the leaf would allow a mutable release
// directory symlink to redirect an otherwise regular executable. The lock
// validator has already required a clean absolute non-root path, so this walk
// has no caller-controlled traversal semantics.
func inspectLockedLocalExecutablePath(absolutePath string) (os.FileInfo, error) {
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
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("symbolic link in locked local executable path")
		}
		// All but the final component must be a directory. This avoids opening
		// a path whose parent was changed to an unusual filesystem object.
		if index > 0 && !info.IsDir() {
			return nil, errors.New("non-directory component in locked local executable path")
		}
		if index == 0 {
			finalInfo = info
		}
	}
	return finalInfo, nil
}

func fingerprintOpenRegularFile(ctx context.Context, file *os.File) (workflowkit.Fingerprint, error) {
	hasher := sha256.New()
	buffer := make([]byte, 64*1024)
	for {
		if err := contextRuntimeAttestationError(ctx); err != nil {
			return "", err
		}
		count, err := file.Read(buffer)
		if count > 0 {
			if _, writeErr := hasher.Write(buffer[:count]); writeErr != nil {
				return "", fmt.Errorf("%w: cannot fingerprint locked local executable", ErrDeploymentOperationRuntimeAttestationFailed)
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("%w: cannot read locked local executable", ErrDeploymentOperationRuntimeAttestationFailed)
		}
		if count == 0 {
			// A non-empty read buffer from a regular file must make progress or
			// terminate. Refuse a broken/hostile filesystem rather than spinning
			// until an execution deadline expires.
			return "", fmt.Errorf("%w: locked local executable read made no progress", ErrDeploymentOperationRuntimeAttestationFailed)
		}
	}
	if err := contextRuntimeAttestationError(ctx); err != nil {
		return "", err
	}
	return workflowkit.Fingerprint("sha256:" + hex.EncodeToString(hasher.Sum(nil))), nil
}

func contextRuntimeAttestationError(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("%w: runtime attestation context is required", ErrDeploymentOperationRuntimeAttestationFailed)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: runtime attestation context ended: %w", ErrDeploymentOperationRuntimeAttestationFailed, err)
	}
	return nil
}

var _ DeploymentOperationRuntimeAttestor = (*LocalFilesystemRuntimeAttestor)(nil)
