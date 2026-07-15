package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	managedRunInputsDirectory     = "run-inputs"
	runStartInputBundleFormat     = "harbor.run-start-input-bundle.v1"
	runStartInputProfileFileName  = "execution-profile.json"
	runStartInputSpecFileName     = "run-execution-spec.json"
	runStartInputManifestFileName = "manifest.json"
)

// PreparedStartRun describes the immutable input bundle created by the first
// StartRun confirmation. The bundle is addressed by the caller's UUIDv7 key,
// so a retry never needs to reopen the original profile or execution-spec
// paths.
type PreparedStartRun struct {
	InputBundleID            string `json:"input_bundle_id"`
	ProfileFingerprint       string `json:"profile_fingerprint"`
	ExecutionSpecFingerprint string `json:"execution_spec_fingerprint"`
}

type runStartInputBundle struct {
	Format                        string                                                `json:"format"`
	Action                        LifecycleMutationAction                               `json:"action"`
	IdempotencyKey                string                                                `json:"idempotency_key"`
	Actor                         string                                                `json:"actor"`
	Reason                        string                                                `json:"reason"`
	Trigger                       string                                                `json:"trigger"`
	ParentRunID                   string                                                `json:"parent_run_id,omitempty"`
	Expected                      LifecycleMutationCheckpoint                           `json:"expected"`
	ProfileFingerprint            workflowkit.Fingerprint                               `json:"profile_fingerprint"`
	ExecutionSpecFingerprint      workflowkit.Fingerprint                               `json:"execution_spec_fingerprint"`
	DeploymentCatalogReceipt      json.RawMessage                                       `json:"deployment_catalog_receipt,omitempty"`
	DeploymentCatalogLockIdentity *stageprovider.DeploymentOperationCatalogLockIdentity `json:"deployment_catalog_lock_identity,omitempty"`
	CreatedAt                     time.Time                                             `json:"created_at"`
}

type frozenRunStartInputs struct {
	Bundle                        runStartInputBundle
	Profile                       workflowadapter.ExecutionProfile
	ExecutionSpec                 workflowadapter.RunExecutionSpec
	ProfileCanonicalJSON          []byte
	ExecutionSpecCanonicalJSON    []byte
	DeploymentCatalogReceipt      []byte
	DeploymentCatalogLockIdentity *stageprovider.DeploymentOperationCatalogLockIdentity
}

func (layout managedLayout) runStartInputsRoot() string {
	return filepath.Join(layout.root, managedRunInputsDirectory)
}

func (layout managedLayout) runStartInputDirectory(idempotencyKey string) string {
	return filepath.Join(layout.runStartInputsRoot(), idempotencyKey)
}

// PrepareStartRun canonicalizes the two explicit inputs and publishes them in
// managed storage. It performs no lifecycle Run mutation; callers use its
// result to render a final confirmation before StartRun consumes the bundle.
func (service *LifecycleMutationService) PrepareStartRun(ctx context.Context, command StartRunLifecycleCommand) (PreparedStartRun, error) {
	inputs, err := service.prepareStartRunInputs(ctx, command)
	if err != nil {
		return PreparedStartRun{}, err
	}
	return preparedStartRunResult(inputs), nil
}

func (service *LifecycleMutationService) prepareStartRunInputs(ctx context.Context, command StartRunLifecycleCommand) (frozenRunStartInputs, error) {
	return service.prepareRunStartInputs(ctx, LifecycleMutationStartRun, command, func() (workflowadapter.ExecutionProfile, workflowadapter.RunExecutionSpec, error) {
		profile, err := ReadExecutionProfileFile(command.ProfilePath)
		if err != nil {
			return workflowadapter.ExecutionProfile{}, workflowadapter.RunExecutionSpec{}, err
		}
		specification, err := ReadRunExecutionSpecFile(command.ExecutionSpecPath)
		if err != nil {
			return workflowadapter.ExecutionProfile{}, workflowadapter.RunExecutionSpec{}, err
		}
		return profile, specification, nil
	})
}

// prepareRunStartInputs owns the common first-confirmation protocol for all
// application-owned Run launchers. The supplied definition callback is not
// evaluated until an existing immutable input bundle has been ruled out, so a
// replay cannot silently read mutable files or rebuild a changed deployment
// definition.
func (service *LifecycleMutationService) prepareRunStartInputs(ctx context.Context, action LifecycleMutationAction, command StartRunLifecycleCommand, definition func() (workflowadapter.ExecutionProfile, workflowadapter.RunExecutionSpec, error)) (frozenRunStartInputs, error) {
	if service == nil || service.core == nil || service.core.store == nil {
		return frozenRunStartInputs{}, fmt.Errorf("lifecycle mutation service is not configured")
	}
	if definition == nil {
		return frozenRunStartInputs{}, fmt.Errorf("Run start definition provider is required")
	}
	if err := validateStartRunInputCommand(command); err != nil {
		return frozenRunStartInputs{}, err
	}
	if err := service.validateCheckpoint(ctx, command.Expected); err != nil {
		return frozenRunStartInputs{}, err
	}

	existing, err := service.core.store.GetLifecycleOperationByIdempotencyKey(ctx, command.IdempotencyKey)
	if err != nil {
		return frozenRunStartInputs{}, err
	}
	if existing != nil {
		if existing.Action != string(action) {
			return frozenRunStartInputs{}, fmt.Errorf("%w: lifecycle operation key %s", store.ErrIdempotencyConflict, command.IdempotencyKey)
		}
		return service.readFrozenRunStartInputs(action, command)
	}

	if directoryExists(service.core.layout.runStartInputDirectory(command.IdempotencyKey)) {
		return service.readFrozenRunStartInputs(action, command)
	}
	profile, specification, err := definition()
	if err != nil {
		return frozenRunStartInputs{}, err
	}
	return service.freezeRunStartInputs(ctx, action, command, profile, specification)
}

func validateStartRunInputCommand(command StartRunLifecycleCommand) error {
	if err := store.ValidateUUIDv7(strings.TrimSpace(command.IdempotencyKey)); err != nil {
		return err
	}
	if strings.TrimSpace(command.Actor) == "" || strings.TrimSpace(command.Reason) == "" {
		return fmt.Errorf("lifecycle mutation actor and reason are required")
	}
	if strings.TrimSpace(command.Trigger) == "" {
		return fmt.Errorf("run trigger is required")
	}
	if parentRunID := strings.TrimSpace(command.ParentRunID); parentRunID != "" {
		if err := store.ValidateUUIDv7(parentRunID); err != nil {
			return fmt.Errorf("StartRun parent Run ID: %w", err)
		}
	}
	expected := command.Expected
	if err := store.ValidateUUIDv7(strings.TrimSpace(expected.TaskID)); err != nil {
		return fmt.Errorf("StartRun expected Task ID: %w", err)
	}
	if err := store.ValidateUUIDv7(strings.TrimSpace(expected.RevisionID)); err != nil {
		return fmt.Errorf("StartRun expected revision ID: %w", err)
	}
	if expected.TaskVersion <= 0 || expected.RevisionStateVersion <= 0 || strings.TrimSpace(expected.RevisionDigest) == "" {
		return fmt.Errorf("StartRun requires a complete TaskRevision checkpoint")
	}
	return nil
}

func (service *LifecycleMutationService) freezeRunStartInputs(ctx context.Context, action LifecycleMutationAction, command StartRunLifecycleCommand, profile workflowadapter.ExecutionProfile, specification workflowadapter.RunExecutionSpec) (frozenRunStartInputs, error) {
	if err := validateRunExecutionSpecSelection(specification, command.Expected); err != nil {
		return frozenRunStartInputs{}, err
	}
	if err := validateRunExecutionSpecOperationResolver(specification, service.core.operationResolver); err != nil {
		return frozenRunStartInputs{}, err
	}
	if err := service.core.validateDeploymentCatalogExecutionSpec(specification); err != nil {
		return frozenRunStartInputs{}, err
	}
	profileCanonical, err := profile.CanonicalJSON()
	if err != nil {
		return frozenRunStartInputs{}, fmt.Errorf("canonicalize execution profile: %w", err)
	}
	specificationCanonical, err := specification.CanonicalJSON()
	if err != nil {
		return frozenRunStartInputs{}, fmt.Errorf("canonicalize execution specification: %w", err)
	}
	profileFingerprint, err := profile.Fingerprint()
	if err != nil {
		return frozenRunStartInputs{}, fmt.Errorf("fingerprint execution profile: %w", err)
	}
	specificationFingerprint, err := specification.Fingerprint()
	if err != nil {
		return frozenRunStartInputs{}, fmt.Errorf("fingerprint execution specification: %w", err)
	}
	catalogReceipt, err := service.core.frozenDeploymentCatalogReceipt(specification.Template)
	if err != nil {
		return frozenRunStartInputs{}, fmt.Errorf("freeze deployment catalog receipt: %w", err)
	}
	if err := service.core.verifyDeploymentCatalogReceipt(specification.Template, catalogReceipt); err != nil {
		return frozenRunStartInputs{}, fmt.Errorf("verify deployment catalog receipt before freezing StartRun inputs: %w", err)
	}
	lockIdentity, err := service.core.frozenDeploymentCatalogLockIdentity(specification.Template)
	if err != nil {
		return frozenRunStartInputs{}, fmt.Errorf("freeze deployment catalog lock identity: %w", err)
	}
	bundle := runStartInputBundle{
		Format:                        runStartInputBundleFormat,
		Action:                        action,
		IdempotencyKey:                strings.TrimSpace(command.IdempotencyKey),
		Actor:                         strings.TrimSpace(command.Actor),
		Reason:                        strings.TrimSpace(command.Reason),
		Trigger:                       strings.TrimSpace(command.Trigger),
		ParentRunID:                   strings.TrimSpace(command.ParentRunID),
		Expected:                      command.Expected,
		ProfileFingerprint:            profileFingerprint,
		ExecutionSpecFingerprint:      specificationFingerprint,
		DeploymentCatalogReceipt:      append(json.RawMessage(nil), catalogReceipt...),
		DeploymentCatalogLockIdentity: cloneDeploymentCatalogLockIdentity(lockIdentity),
		CreatedAt:                     service.core.now().UTC(),
	}
	inputs := frozenRunStartInputs{
		Bundle:                        bundle,
		Profile:                       profile,
		ExecutionSpec:                 specification,
		ProfileCanonicalJSON:          profileCanonical,
		ExecutionSpecCanonicalJSON:    specificationCanonical,
		DeploymentCatalogReceipt:      append([]byte(nil), catalogReceipt...),
		DeploymentCatalogLockIdentity: cloneDeploymentCatalogLockIdentity(lockIdentity),
	}
	if err := service.publishFrozenRunStartInputs(ctx, inputs); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return frozenRunStartInputs{}, err
		}
		return service.readFrozenRunStartInputs(action, command)
	}
	return inputs, nil
}

func (service *LifecycleMutationService) publishFrozenRunStartInputs(ctx context.Context, inputs frozenRunStartInputs) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := service.core.layout.ensureRoot(); err != nil {
		return err
	}
	parent := service.core.layout.runStartInputsRoot()
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return fmt.Errorf("create managed run-input root: %w", err)
	}
	finalDirectory := service.core.layout.runStartInputDirectory(inputs.Bundle.IdempotencyKey)
	if directoryExists(finalDirectory) {
		return os.ErrExist
	}
	stagingDirectory, err := os.MkdirTemp(parent, "."+inputs.Bundle.IdempotencyKey+".")
	if err != nil {
		return fmt.Errorf("create run-input staging directory: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(stagingDirectory)
		}
	}()
	if err := writeNewBytes(filepath.Join(stagingDirectory, runStartInputProfileFileName), inputs.ProfileCanonicalJSON); err != nil {
		return fmt.Errorf("write frozen execution profile: %w", err)
	}
	if err := writeNewBytes(filepath.Join(stagingDirectory, runStartInputSpecFileName), inputs.ExecutionSpecCanonicalJSON); err != nil {
		return fmt.Errorf("write frozen execution specification: %w", err)
	}
	if len(inputs.DeploymentCatalogReceipt) != 0 {
		if err := writeNewBytes(filepath.Join(stagingDirectory, deploymentCatalogReceiptFileName), inputs.DeploymentCatalogReceipt); err != nil {
			return fmt.Errorf("write frozen deployment catalog receipt: %w", err)
		}
	}
	if inputs.DeploymentCatalogLockIdentity != nil {
		canonicalLockIdentity, err := canonicalDeploymentCatalogLockIdentity(*inputs.DeploymentCatalogLockIdentity)
		if err != nil {
			return fmt.Errorf("canonicalize frozen deployment catalog lock identity: %w", err)
		}
		if err := writeNewBytes(filepath.Join(stagingDirectory, deploymentCatalogLockIdentityFileName), canonicalLockIdentity); err != nil {
			return fmt.Errorf("write frozen deployment catalog lock identity: %w", err)
		}
	}
	if err := writeNewJSON(filepath.Join(stagingDirectory, runStartInputManifestFileName), inputs.Bundle); err != nil {
		return fmt.Errorf("write frozen run-input manifest: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(stagingDirectory, finalDirectory); err != nil {
		if directoryExists(finalDirectory) {
			return os.ErrExist
		}
		return fmt.Errorf("publish frozen run inputs: %w", err)
	}
	committed = true
	return nil
}

func (service *LifecycleMutationService) readFrozenRunStartInputs(action LifecycleMutationAction, command StartRunLifecycleCommand) (frozenRunStartInputs, error) {
	directory := service.core.layout.runStartInputDirectory(command.IdempotencyKey)
	info, err := os.Lstat(directory)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return frozenRunStartInputs{}, fmt.Errorf("frozen StartRun inputs for idempotency key %s are unavailable", command.IdempotencyKey)
		}
		return frozenRunStartInputs{}, fmt.Errorf("inspect frozen StartRun input directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return frozenRunStartInputs{}, fmt.Errorf("frozen StartRun input directory is not a real directory")
	}
	manifestRaw, err := readManagedRunStartInputFile(directory, runStartInputManifestFileName)
	if err != nil {
		return frozenRunStartInputs{}, err
	}
	var bundle runStartInputBundle
	if err := decodeStrictJSON(string(manifestRaw), &bundle); err != nil {
		return frozenRunStartInputs{}, fmt.Errorf("decode frozen StartRun input manifest: %w", err)
	}
	if bundle.Format != runStartInputBundleFormat || bundle.Action != action || !sameRunStartInputCommand(bundle, command) {
		return frozenRunStartInputs{}, fmt.Errorf("%w: frozen StartRun input bundle %s", store.ErrIdempotencyConflict, command.IdempotencyKey)
	}
	profileRaw, err := readManagedRunStartInputFile(directory, runStartInputProfileFileName)
	if err != nil {
		return frozenRunStartInputs{}, err
	}
	profile, err := workflowadapter.ParseExecutionProfileJSON(profileRaw)
	if err != nil {
		return frozenRunStartInputs{}, fmt.Errorf("parse frozen execution profile: %w", err)
	}
	profileCanonical, err := profile.CanonicalJSON()
	if err != nil {
		return frozenRunStartInputs{}, fmt.Errorf("canonicalize frozen execution profile: %w", err)
	}
	if !bytes.Equal(profileRaw, profileCanonical) {
		return frozenRunStartInputs{}, fmt.Errorf("frozen execution profile is not canonical")
	}
	profileFingerprint, err := profile.Fingerprint()
	if err != nil {
		return frozenRunStartInputs{}, fmt.Errorf("fingerprint frozen execution profile: %w", err)
	}
	if profileFingerprint != bundle.ProfileFingerprint {
		return frozenRunStartInputs{}, fmt.Errorf("frozen execution profile fingerprint does not match manifest")
	}
	specificationRaw, err := readManagedRunStartInputFile(directory, runStartInputSpecFileName)
	if err != nil {
		return frozenRunStartInputs{}, err
	}
	specification, err := workflowadapter.ParseRunExecutionSpecJSON(specificationRaw)
	if err != nil {
		return frozenRunStartInputs{}, fmt.Errorf("parse frozen execution specification: %w", err)
	}
	specificationCanonical, err := specification.CanonicalJSON()
	if err != nil {
		return frozenRunStartInputs{}, fmt.Errorf("canonicalize frozen execution specification: %w", err)
	}
	if !bytes.Equal(specificationRaw, specificationCanonical) {
		return frozenRunStartInputs{}, fmt.Errorf("frozen execution specification is not canonical")
	}
	specificationFingerprint, err := specification.Fingerprint()
	if err != nil {
		return frozenRunStartInputs{}, fmt.Errorf("fingerprint frozen execution specification: %w", err)
	}
	if specificationFingerprint != bundle.ExecutionSpecFingerprint {
		return frozenRunStartInputs{}, fmt.Errorf("frozen execution specification fingerprint does not match manifest")
	}
	catalogReceipt, err := canonicalManifestDeploymentCatalogReceipt(runManifest{DeploymentCatalogReceipt: bundle.DeploymentCatalogReceipt})
	if err != nil {
		return frozenRunStartInputs{}, fmt.Errorf("parse frozen deployment catalog receipt: %w", err)
	}
	if err := service.core.verifyDeploymentCatalogReceipt(specification.Template, catalogReceipt); err != nil {
		return frozenRunStartInputs{}, fmt.Errorf("verify frozen deployment catalog receipt: %w", err)
	}
	lockIdentity, err := canonicalManifestDeploymentCatalogLockIdentity(runManifest{DeploymentCatalogLockIdentity: bundle.DeploymentCatalogLockIdentity})
	if err != nil {
		return frozenRunStartInputs{}, fmt.Errorf("parse frozen deployment catalog lock identity: %w", err)
	}
	if err := service.core.verifyDeploymentCatalogLockIdentity(specification.Template, lockIdentity); err != nil {
		return frozenRunStartInputs{}, fmt.Errorf("verify frozen deployment catalog lock identity: %w", err)
	}
	if len(catalogReceipt) != 0 {
		receiptRaw, readErr := readManagedRunStartInputFile(directory, deploymentCatalogReceiptFileName)
		if readErr != nil {
			return frozenRunStartInputs{}, readErr
		}
		if err := service.core.verifyDeploymentCatalogReceipt(specification.Template, receiptRaw); err != nil {
			return frozenRunStartInputs{}, fmt.Errorf("verify managed frozen deployment catalog receipt: %w", err)
		}
		if !bytes.Equal(receiptRaw, catalogReceipt) {
			return frozenRunStartInputs{}, fmt.Errorf("frozen StartRun input manifest and deployment catalog receipt differ")
		}
	}
	if lockIdentity != nil {
		lockRaw, readErr := readManagedRunStartInputFile(directory, deploymentCatalogLockIdentityFileName)
		if readErr != nil {
			return frozenRunStartInputs{}, readErr
		}
		storedLockIdentity, canonicalLockIdentity, lockErr := parseDeploymentCatalogLockIdentityJSON(lockRaw)
		if lockErr != nil {
			return frozenRunStartInputs{}, fmt.Errorf("parse managed frozen deployment catalog lock identity: %w", lockErr)
		}
		if !bytes.Equal(lockRaw, canonicalLockIdentity) || storedLockIdentity != *lockIdentity {
			return frozenRunStartInputs{}, fmt.Errorf("frozen StartRun input manifest and deployment catalog lock identity differ")
		}
	}
	if err := validateRunExecutionSpecSelection(specification, command.Expected); err != nil {
		return frozenRunStartInputs{}, err
	}
	return frozenRunStartInputs{
		Bundle:                        bundle,
		Profile:                       profile,
		ExecutionSpec:                 specification,
		ProfileCanonicalJSON:          profileCanonical,
		ExecutionSpecCanonicalJSON:    specificationCanonical,
		DeploymentCatalogReceipt:      append([]byte(nil), catalogReceipt...),
		DeploymentCatalogLockIdentity: cloneDeploymentCatalogLockIdentity(lockIdentity),
	}, nil
}

func readManagedRunStartInputFile(directory, name string) ([]byte, error) {
	path := filepath.Join(directory, name)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect frozen StartRun input %s: %w", name, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("frozen StartRun input %s is not a regular file", name)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read frozen StartRun input %s: %w", name, err)
	}
	return raw, nil
}

func writeNewBytes(path string, value []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return err
	}
	if _, err := file.Write(value); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func directoryExists(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSymlink == 0 && info.IsDir()
}

func sameRunStartInputCommand(bundle runStartInputBundle, command StartRunLifecycleCommand) bool {
	return bundle.IdempotencyKey == strings.TrimSpace(command.IdempotencyKey) &&
		bundle.Actor == strings.TrimSpace(command.Actor) &&
		bundle.Reason == strings.TrimSpace(command.Reason) &&
		bundle.Trigger == strings.TrimSpace(command.Trigger) &&
		bundle.ParentRunID == strings.TrimSpace(command.ParentRunID) &&
		bundle.Expected == command.Expected
}

func validateRunExecutionSpecSelection(specification workflowadapter.RunExecutionSpec, expected LifecycleMutationCheckpoint) error {
	if err := specification.Validate(); err != nil {
		return fmt.Errorf("validate execution specification: %w", err)
	}
	selection := specification.Selection
	if selection.TaskID != expected.TaskID || selection.RevisionID != expected.RevisionID || string(selection.RevisionDigest) != expected.RevisionDigest {
		return fmt.Errorf("%w: execution specification selection does not match the confirmed TaskRevision", store.ErrOptimisticLock)
	}
	return nil
}

func preparedStartRunResult(inputs frozenRunStartInputs) PreparedStartRun {
	return PreparedStartRun{
		InputBundleID:            inputs.Bundle.IdempotencyKey,
		ProfileFingerprint:       string(inputs.Bundle.ProfileFingerprint),
		ExecutionSpecFingerprint: string(inputs.Bundle.ExecutionSpecFingerprint),
	}
}
