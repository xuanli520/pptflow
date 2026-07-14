package app

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
)

const (
	runExecutionProfileFileName = "execution-profile.json"
	runExecutionSpecFileName    = "run-execution-spec.json"
)

// verifyRunManagedExecutionInputs proves the managed companion files, the
// embedded manifest values, and the durable TaskRevision selection all name
// the same immutable execution inputs. It is called before worker admission;
// a missing or substituted file is an integrity failure rather than a reason
// to read a caller path or recompute from current configuration.
func (core *lifecycleServiceCore) verifyRunManagedExecutionInputs(ctx context.Context, run store.WorkflowRun) (workflowadapter.ExecutionProfile, workflowadapter.RunExecutionSpec, error) {
	if core == nil || core.store == nil {
		return workflowadapter.ExecutionProfile{}, workflowadapter.RunExecutionSpec{}, fmt.Errorf("managed execution input verifier is not configured")
	}
	var manifest runManifest
	if err := decodeStrictJSON(run.RunManifestJSON, &manifest); err != nil {
		return workflowadapter.ExecutionProfile{}, workflowadapter.RunExecutionSpec{}, fmt.Errorf("decode run manifest execution inputs: %w", err)
	}
	if manifest.Format != "harbor.workflow-run-manifest.v2" || manifest.RunID != run.ID || manifest.TaskID != run.TaskID || manifest.Revision != run.RevisionID {
		return workflowadapter.ExecutionProfile{}, workflowadapter.RunExecutionSpec{}, fmt.Errorf("run manifest does not match workflow run")
	}
	specification, canonicalSpec, specificationFingerprint, err := canonicalFrozenRunExecutionSpec(manifest, run)
	if err != nil {
		return workflowadapter.ExecutionProfile{}, workflowadapter.RunExecutionSpec{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	revision, err := core.store.GetTaskRevision(ctx, run.RevisionID)
	if err != nil {
		return workflowadapter.ExecutionProfile{}, workflowadapter.RunExecutionSpec{}, err
	}
	if revision == nil || revision.TaskID != run.TaskID || string(specification.Selection.RevisionDigest) != revision.TaskDigest {
		return workflowadapter.ExecutionProfile{}, workflowadapter.RunExecutionSpec{}, fmt.Errorf("execution specification selection does not match TaskRevision")
	}
	if err := verifyManagedRunInputs(ctx, core, run, *revision, manifest, specification); err != nil {
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
