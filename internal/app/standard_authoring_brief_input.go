package app

import (
	"fmt"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// standardAuthoringBriefInput is the immutable bridge from the launch/session
// contract to normal workflow artifact bindings.
type standardAuthoringBriefInput struct {
	ArtifactID    workflowkit.ArtifactID
	Brief         workflowadapter.StandardAuthoringBrief
	CanonicalJSON []byte
	ContentDigest workflowkit.Fingerprint
}

func newStandardAuthoringBriefInput(artifactID string, brief workflowadapter.StandardAuthoringBrief) (standardAuthoringBriefInput, error) {
	if err := store.ValidateUUIDv7(artifactID); err != nil {
		return standardAuthoringBriefInput{}, fmt.Errorf("Standard authoring brief artifact ID: %w", err)
	}
	canonicalBrief, err := brief.Canonical()
	if err != nil {
		return standardAuthoringBriefInput{}, fmt.Errorf("canonicalize Standard authoring brief: %w", err)
	}
	canonical, err := canonicalBrief.CanonicalJSON()
	if err != nil {
		return standardAuthoringBriefInput{}, fmt.Errorf("encode Standard authoring brief: %w", err)
	}
	digest, err := canonicalBrief.ContentDigest()
	if err != nil {
		return standardAuthoringBriefInput{}, fmt.Errorf("fingerprint Standard authoring brief: %w", err)
	}
	return standardAuthoringBriefInput{
		ArtifactID: workflowkit.ArtifactID(artifactID), Brief: canonicalBrief, CanonicalJSON: canonical, ContentDigest: digest,
	}, nil
}

func (input standardAuthoringBriefInput) artifactReference() workflowadapter.ArtifactReference {
	return workflowadapter.ArtifactReference{
		ID: input.ArtifactID, ContentDigest: input.ContentDigest, SchemaVersion: workflowadapter.StandardAuthoringBriefSchemaVersion,
	}
}

func (input standardAuthoringBriefInput) artifactBinding() workflowkit.ArtifactBinding {
	return workflowkit.ArtifactBinding{
		Name: workflowadapter.StandardAuthoringBriefArtifact, ArtifactID: input.ArtifactID,
		ContentDigest: input.ContentDigest, SchemaVersion: workflowadapter.StandardAuthoringBriefSchemaVersion,
	}
}
