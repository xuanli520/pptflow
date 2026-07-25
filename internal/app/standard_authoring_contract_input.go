package app

import (
	"fmt"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// standardAuthoringContractInput is the only intrinsic model-facing input for
// a v2 AuthoringSession. Its immutable bytes are content addressed by the
// object store and its logical ID is fixed by the launch idempotency key.
type standardAuthoringContractInput struct {
	ArtifactID    workflowkit.ArtifactID
	Contract      workflowadapter.AuthoringContract
	CanonicalJSON []byte
	ContentDigest workflowkit.Fingerprint
}

func newStandardAuthoringContractInput(artifactID string, contract workflowadapter.AuthoringContract) (standardAuthoringContractInput, error) {
	if err := store.ValidateUUIDv7(artifactID); err != nil {
		return standardAuthoringContractInput{}, fmt.Errorf("Standard authoring contract artifact ID: %w", err)
	}
	canonicalContract, err := contract.Canonical()
	if err != nil {
		return standardAuthoringContractInput{}, fmt.Errorf("canonicalize Standard authoring contract: %w", err)
	}
	raw, err := canonicalContract.CanonicalJSON()
	if err != nil {
		return standardAuthoringContractInput{}, fmt.Errorf("encode Standard authoring contract: %w", err)
	}
	digest, err := canonicalContract.ContentDigest()
	if err != nil {
		return standardAuthoringContractInput{}, fmt.Errorf("fingerprint Standard authoring contract: %w", err)
	}
	return standardAuthoringContractInput{
		ArtifactID: workflowkit.ArtifactID(artifactID), Contract: canonicalContract, CanonicalJSON: raw, ContentDigest: digest,
	}, nil
}

func (input standardAuthoringContractInput) artifactReference() workflowadapter.ArtifactReference {
	return workflowadapter.ArtifactReference{
		ID: input.ArtifactID, ContentDigest: input.ContentDigest, SchemaVersion: workflowadapter.AuthoringContractSchemaVersion,
	}
}

func (input standardAuthoringContractInput) artifactBinding() workflowkit.ArtifactBinding {
	return workflowkit.ArtifactBinding{
		Name: workflowadapter.AuthoringContractArtifact, ArtifactID: input.ArtifactID,
		ContentDigest: input.ContentDigest, SchemaVersion: workflowadapter.AuthoringContractSchemaVersion,
	}
}
