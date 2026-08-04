package app

import (
	"bytes"
	"context"
	"fmt"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/workflowruntime"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// standardAuthoringContractInputFromSession reads the sole root contract from
// its content-addressed object. The session manifest stores only its immutable
// binding, never a second embedded copy of the contract bytes.
func standardAuthoringContractInputFromSession(ctx context.Context, objects *workflowruntime.ArtifactObjectStore, session store.AuthoringSession) (standardAuthoringContractInput, error) {
	if objects == nil {
		return standardAuthoringContractInput{}, fmt.Errorf("Standard authoring contract object store is not configured")
	}
	var manifest standardAuthoringSessionManifest
	if err := decodeStrictJSON(session.SessionManifestJSON, &manifest); err != nil {
		return standardAuthoringContractInput{}, fmt.Errorf("decode Standard authoring session manifest: %w", err)
	}
	if manifest.Format != standardAuthoringLaunchSessionManifestFormat || manifest.Version != standardAuthoringLaunchSessionManifestVersion ||
		manifest.AuthoringSessionID != session.ID || manifest.SourceID != session.SourceID || manifest.TargetTaskID != session.TargetTaskID ||
		!isAdmissibleStandardAuthoringSession(session) || manifest.ContractSizeBytes <= 0 {
		return standardAuthoringContractInput{}, fmt.Errorf("Standard authoring session manifest has no valid root contract binding")
	}
	if err := manifest.ContractDigest.Validate(); err != nil {
		return standardAuthoringContractInput{}, fmt.Errorf("Standard authoring session contract digest: %w", err)
	}
	raw, err := objects.ReadAll(ctx, workflowruntime.ObjectRef{Digest: manifest.ContractDigest, SizeBytes: manifest.ContractSizeBytes})
	if err != nil {
		return standardAuthoringContractInput{}, fmt.Errorf("read Standard authoring contract object: %w", err)
	}
	contract, err := workflowadapter.ParseAuthoringContractJSON(raw)
	if err != nil {
		return standardAuthoringContractInput{}, err
	}
	input, err := newStandardAuthoringContractInput(manifest.ContractArtifactID, contract)
	if err != nil {
		return standardAuthoringContractInput{}, err
	}
	if input.ContentDigest != manifest.ContractDigest || !bytes.Equal(input.CanonicalJSON, raw) {
		return standardAuthoringContractInput{}, fmt.Errorf("Standard authoring contract object is not canonical")
	}
	return input, nil
}

// isAdmissibleStandardAuthoringSession admits the current Standard authoring
// template family (same major version). The root contract is content
// addressed and digest-bound by the session manifest, so an exact version
// equality is not a security boundary; the family check prevents legacy
// major contracts from being re-executed under the current policy.
func isAdmissibleStandardAuthoringSession(session store.AuthoringSession) bool {
	if session.WorkflowTemplateID != workflowadapter.StandardAuthoringWorkflowTemplateID {
		return false
	}
	current, ok := standardAuthoringTemplateFamilyVersion(workflowadapter.StandardAuthoringCurrentTemplateReference().Version)
	if !ok {
		return false
	}
	actual, ok := standardAuthoringTemplateFamilyVersion(session.WorkflowTemplateVersion)
	return ok && actual == current
}

// validateStandardAuthoringContractBindings proves the frozen execution
// specification references the session root contract on every contract-bound
// stage. The descriptor is the caller's frozen source of truth (the current
// catalog for new Runs, the frozen run manifest for continuation/restart), so
// an upgraded binary can still admit same-major Runs whose stage input shapes
// were already verified against the current catalog.
func validateStandardAuthoringContractBindings(workflow workflowkit.WorkflowDescriptor, specification workflowadapter.RunExecutionSpec, contract standardAuthoringContractInput) error {
	if err := specification.Validate(); err != nil {
		return fmt.Errorf("validate Standard authoring execution specification: %w", err)
	}
	if workflow.ID != specification.Template.ID || workflow.Version != specification.Template.Version {
		return fmt.Errorf("root contract binding descriptor does not match the execution specification template")
	}
	if err := workflow.Validate(); err != nil {
		return fmt.Errorf("validate Standard authoring descriptor: %w", err)
	}
	reference := contract.artifactReference()
	foundReference := false
	for _, candidate := range specification.References.Artifacts {
		if candidate.ID != reference.ID {
			continue
		}
		if candidate != reference {
			return fmt.Errorf("Standard authoring contract artifact reference differs from the session contract")
		}
		foundReference = true
	}
	if !foundReference {
		return fmt.Errorf("Standard authoring execution specification does not reference the root contract")
	}
	for _, stage := range workflow.Stages {
		resolution, err := specification.ResolveStageOperation(stage.Key)
		if err != nil {
			return err
		}
		found := false
		for _, binding := range resolution.ArtifactInputs {
			if binding.Port != workflowadapter.AuthoringContractArtifact {
				continue
			}
			if found || binding.ArtifactID != reference.ID {
				return fmt.Errorf("Standard authoring stage %q root contract binding differs from the session contract", stage.Key)
			}
			found = true
		}
		if !found {
			return fmt.Errorf("Standard authoring stage %q does not bind the root contract", stage.Key)
		}
	}
	return nil
}
