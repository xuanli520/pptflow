package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/workflowruntime"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	runExecutionProfileFileName = "execution-profile.json"
	runExecutionSpecFileName    = "run-execution-spec.json"
)

// verifyRunManagedExecutionInputs proves the managed companion files, the
// embedded manifest values, and the durable generic subject selection all
// name the same immutable execution inputs. It is called before worker
// admission; a missing or substituted file is an integrity failure rather
// than a reason to read a caller path or recompute from current configuration.
func (core *lifecycleServiceCore) verifyRunManagedExecutionInputs(ctx context.Context, run store.WorkflowRun) (workflowadapter.ExecutionProfile, workflowadapter.RunExecutionSpec, error) {
	if core == nil || core.store == nil {
		return workflowadapter.ExecutionProfile{}, workflowadapter.RunExecutionSpec{}, fmt.Errorf("managed execution input verifier is not configured")
	}
	manifest, err := decodeRunManifest(run)
	if err != nil {
		return workflowadapter.ExecutionProfile{}, workflowadapter.RunExecutionSpec{}, fmt.Errorf("decode run manifest execution inputs: %w", err)
	}
	specification, canonicalSpec, specificationFingerprint, err := canonicalFrozenRunExecutionSpec(manifest, run)
	if err != nil {
		return workflowadapter.ExecutionProfile{}, workflowadapter.RunExecutionSpec{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	subject, err := core.resolveWorkflowRunSubject(ctx, run)
	if err != nil {
		return workflowadapter.ExecutionProfile{}, workflowadapter.RunExecutionSpec{}, err
	}
	if binding, bindingErr := specification.Selection.SubjectBinding(); bindingErr != nil || binding != subject.Binding {
		return workflowadapter.ExecutionProfile{}, workflowadapter.RunExecutionSpec{}, fmt.Errorf("execution specification selection does not match immutable workflow subject")
	}
	if err := core.verifyWorkflowRunSubjectInputs(ctx, run, subject, manifest, specification); err != nil {
		return workflowadapter.ExecutionProfile{}, workflowadapter.RunExecutionSpec{}, err
	}

	runDirectory := core.layout.runDirectory(run.ID)
	profileRaw, err := readManagedRunExecutionInputFile(filepath.Join(runDirectory, runExecutionProfileFileName), "execution profile")
	if err != nil {
		return workflowadapter.ExecutionProfile{}, workflowadapter.RunExecutionSpec{}, err
	}
	profile, err := workflowadapter.ParseExecutionProfileJSON(profileRaw)
	if err != nil {
		return workflowadapter.ExecutionProfile{}, workflowadapter.RunExecutionSpec{}, fmt.Errorf("parse managed execution profile: %w", err)
	}
	canonicalProfile, err := profile.CanonicalJSON()
	if err != nil || !bytes.Equal(profileRaw, canonicalProfile) {
		return workflowadapter.ExecutionProfile{}, workflowadapter.RunExecutionSpec{}, fmt.Errorf("managed execution profile is not canonical")
	}
	profileFingerprint, err := profile.Fingerprint()
	if err != nil || profileFingerprint != manifest.Inputs.ProfileFingerprint || profileFingerprint != manifest.Resolved.ExecutionProfileFingerprint {
		return workflowadapter.ExecutionProfile{}, workflowadapter.RunExecutionSpec{}, fmt.Errorf("managed execution profile fingerprint does not match run manifest")
	}
	if _, err := resolveFrozenRunTemplate(profile, specification); err != nil {
		return workflowadapter.ExecutionProfile{}, workflowadapter.RunExecutionSpec{}, fmt.Errorf("managed execution profile and specification templates differ: %w", err)
	}

	specRaw, err := readManagedRunExecutionInputFile(filepath.Join(runDirectory, runExecutionSpecFileName), "execution specification")
	if err != nil {
		return workflowadapter.ExecutionProfile{}, workflowadapter.RunExecutionSpec{}, err
	}
	managedSpecification, err := workflowadapter.ParseRunExecutionSpecJSON(specRaw)
	if err != nil {
		return workflowadapter.ExecutionProfile{}, workflowadapter.RunExecutionSpec{}, fmt.Errorf("parse managed execution specification: %w", err)
	}
	managedCanonicalSpec, err := managedSpecification.CanonicalJSON()
	if err != nil || !bytes.Equal(specRaw, managedCanonicalSpec) || !bytes.Equal(managedCanonicalSpec, canonicalSpec) {
		return workflowadapter.ExecutionProfile{}, workflowadapter.RunExecutionSpec{}, fmt.Errorf("managed execution specification differs from run manifest")
	}
	managedFingerprint, err := managedSpecification.Fingerprint()
	if err != nil || managedFingerprint != specificationFingerprint {
		return workflowadapter.ExecutionProfile{}, workflowadapter.RunExecutionSpec{}, fmt.Errorf("managed execution specification fingerprint does not match run manifest")
	}
	return profile, specification, nil
}

// verifyWorkflowRunSubjectInputs keeps subject-specific intrinsic inputs at
// the Harbor boundary.  The generic kernel sees only verified artifact
// bindings; it does not learn what a TaskRevision or AuthoringSource is.
func (core *lifecycleServiceCore) verifyWorkflowRunSubjectInputs(ctx context.Context, run store.WorkflowRun, subject workflowRunSubject, manifest runManifest, specification workflowadapter.RunExecutionSpec) error {
	switch {
	case subject.isTaskRevision():
		return verifyManagedRunInputs(ctx, core, run, *subject.Revision, manifest, specification)
	case subject.isAuthoringSession():
		return core.verifyAuthoringSourceInput(ctx, run, subject, manifest, specification)
	default:
		return fmt.Errorf("workflow Run subject is not executable")
	}
}

// verifyAuthoringSourceInput proves both intrinsic authoring-session inputs:
// source_snapshot stays available only to the controlled repo adapter, while
// environment_policy and authoring_brief are exposed as normal immutable
// artifact bindings to the stages that declare them. Neither path accepts a
// caller filesystem path or a mutable run-input record.
func (core *lifecycleServiceCore) verifyAuthoringSourceInput(ctx context.Context, run store.WorkflowRun, subject workflowRunSubject, manifest runManifest, specification workflowadapter.RunExecutionSpec) error {
	if core == nil || core.store == nil || core.objects == nil || !subject.isAuthoringSession() {
		return fmt.Errorf("authoring source input verifier is not configured")
	}
	if manifest.Inputs == nil || len(manifest.Inputs.ManagedInputs) != 0 {
		return fmt.Errorf("authoring Run manifest cannot declare task-revision managed inputs")
	}
	if err := validateCurrentStandardAuthoringFrozenContract(run, manifest, specification); err != nil {
		return err
	}
	environmentPolicy, brief, err := standardAuthoringSessionIntrinsicInputs(*subject.AuthoringSession)
	if err != nil {
		return fmt.Errorf("verify authoring session environment policy: %w", err)
	}
	if err := validateStandardAuthoringEnvironmentPolicyBindings(specification, environmentPolicy); err != nil {
		return fmt.Errorf("verify authoring execution environment policy binding: %w", err)
	}
	if err := validateStandardAuthoringBriefBindings(specification, brief); err != nil {
		return fmt.Errorf("verify authoring execution brief binding: %w", err)
	}
	input, err := core.store.GetAuthoringRunInputArtifactForPort(ctx, run.ID, "source_snapshot")
	if err != nil {
		return err
	}
	if input == nil || input.RunID != run.ID || input.SessionID != subject.AuthoringSession.ID || input.SourceID != subject.AuthoringSource.ID ||
		input.SourceFingerprint != subject.AuthoringSource.SourceFingerprint || input.SnapshotArtifactRef != subject.AuthoringSource.SnapshotArtifactRef ||
		input.ContentDigest != subject.AuthoringSource.SnapshotContentDigest || input.SchemaVersion != subject.AuthoringSource.SnapshotSchemaVersion {
		return fmt.Errorf("authoring Run source_snapshot input does not match immutable source subject")
	}
	// The current closed Standard descriptor does not surface source_snapshot
	// as a generic stage artifact port. Prove nevertheless that the source
	// object exists and hashes to the persisted digest before a provider can
	// resolve its controlled checkout.
	file, err := core.objects.Open(ctx, workflowruntime.ObjectRef{Digest: workflowkit.Fingerprint(subject.Binding.Digest)})
	if err != nil {
		return fmt.Errorf("open authoring source snapshot: %w", err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return fmt.Errorf("read authoring source snapshot: %w", err)
	}
	if actual := "sha256:" + hex.EncodeToString(hash.Sum(nil)); actual != string(subject.Binding.Digest) {
		return fmt.Errorf("authoring source snapshot object digest differs from immutable subject")
	}
	return nil
}

func readManagedRunExecutionInputFile(path, label string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect managed %s: %w", label, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("managed %s is not a regular file", label)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read managed %s: %w", label, err)
	}
	return raw, nil
}
