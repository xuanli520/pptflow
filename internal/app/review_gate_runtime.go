package app

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// openReviewGate persists the external-decision wait already validated and
// committed by workflowkit.Engine. It intentionally does not resolve or call
// an app-specific executor: the public Engine is the sole execution boundary
// for both normal stages and durable review gates.
func (runtime *FrozenExecutionRuntime) openReviewGate(ctx context.Context, job store.DurableJob, run store.WorkflowRun, revision store.TaskRevision, frozen frozenRunDefinition, payload frozenStageExecutionPayload, stage workflowkit.StageDescriptor, attempt store.StageAttempt, inputs []workflowkit.ArtifactBinding, review workflowadapter.ReviewStage) (store.JobState, error) {
	frozenReview, found := frozen.ReviewStage(stage.Key)
	if !found || frozenReview.ReviewKind != review.ReviewKind || frozenReview.DecisionArtifact != review.DecisionArtifact {
		return runtime.failMalformedJob(ctx, job, fmt.Errorf("%w: review gate %q does not match its frozen review metadata", ErrFrozenExecutionPayload, stage.Key))
	}

	encodedInputs, err := json.Marshal(inputs)
	if err != nil {
		return runtime.failMalformedJob(ctx, job, fmt.Errorf("encode frozen review gate inputs: %w", err))
	}
	evidenceDigest, err := workflowkit.FingerprintParts("harbor.review-gate-evidence.v1", []workflowkit.FingerprintPart{
		{Name: "definition", Value: []byte(run.DefinitionHash)},
		{Name: "inputs", Value: encodedInputs},
		{Name: "review_kind", Value: []byte(review.ReviewKind)},
		{Name: "revision", Value: []byte(revision.TaskDigest)},
		{Name: "stage", Value: []byte(stage.Key)},
	})
	if err != nil {
		return runtime.failMalformedJob(ctx, job, fmt.Errorf("fingerprint frozen review gate evidence: %w", err))
	}
	opened, err := runtime.core.store.OpenReviewGate(ctx, store.OpenReviewGateRequest{
		RunID: run.ID, ExpectedRunVersion: run.Version, RevisionID: revision.ID, RevisionDigest: revision.TaskDigest,
		DefinitionHash: run.DefinitionHash, StageAttemptID: attempt.ID, ExpectedStageAttemptVersion: attempt.Version,
		StageKey: string(stage.Key), ReviewKind: string(review.ReviewKind), NodeGeneration: payload.Generation, NodeAttempt: 1,
		InputBindingsJSON: string(encodedInputs), InputFingerprint: attempt.InputFingerprint, EvidenceManifestDigest: string(evidenceDigest),
		Actor: job.CreatedBy, Reason: "activate frozen durable review gate",
	})
	if err != nil {
		return runtime.failMalformedJob(ctx, job, fmt.Errorf("open durable review gate: %w", err))
	}
	if opened.Binding.RunID != run.ID || opened.Binding.StageAttemptID != attempt.ID || opened.StageAttempt.ExecutionStatus != store.StageExecutionWaiting || opened.Run.Status != store.WorkflowRunWaitingReview {
		return runtime.failMalformedJob(ctx, job, fmt.Errorf("%w: review gate open returned inconsistent waiting state", ErrFrozenExecutionPayload))
	}
	return store.JobSucceeded, nil
}
