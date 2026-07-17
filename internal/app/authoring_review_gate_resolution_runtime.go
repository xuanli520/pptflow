package app

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const authoringReviewGateDecisionArtifactFormat = "harbor.authoring-review-gate-decision.v1"

// authoringReviewGateDecisionArtifact is the session-subject counterpart of
// the task-revision decision evidence.  It deliberately records source and
// session coordinates, never an invented TaskRevision.
type authoringReviewGateDecisionArtifact struct {
	Format                 string                     `json:"format"`
	ReviewRequestID        string                     `json:"review_request_id"`
	ReviewDecisionID       string                     `json:"review_decision_id"`
	Action                 store.ReviewDecisionAction `json:"action"`
	AuthoringSourceID      string                     `json:"authoring_source_id"`
	AuthoringSessionID     string                     `json:"authoring_session_id"`
	SourceSnapshotDigest   string                     `json:"source_snapshot_digest"`
	ReviewKind             string                     `json:"review_kind"`
	EvidenceManifestDigest string                     `json:"evidence_manifest_digest"`
	InputFingerprint       string                     `json:"input_fingerprint"`
	DecisionActor          string                     `json:"decision_actor"`
	DecisionReason         string                     `json:"decision_reason"`
}

func (runtime *FrozenExecutionRuntime) handleAuthoringReviewGateResolution(ctx context.Context, _ DurableJobExecution, job store.DurableJob) (store.JobState, error) {
	if job.CommandType != store.AuthoringReviewGateResolutionCommandType || job.RunID == "" || job.StageAttemptID == "" {
		return runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: authoring review resolution job does not bind a Run and StageAttempt", ErrFrozenExecutionPayload))
	}
	binding, err := runtime.core.store.GetAuthoringReviewGateBindingByStageAttempt(ctx, job.StageAttemptID)
	if err != nil {
		return runtime.failRuntimeJob(ctx, job, err)
	}
	if binding == nil || binding.RunID != job.RunID || binding.StageAttemptID != job.StageAttemptID {
		return runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: authoring review resolution job does not match a frozen gate", ErrFrozenExecutionPayload))
	}
	decisions, err := runtime.core.store.ListAuthoringReviewDecisionsForRequest(ctx, binding.ReviewRequestID)
	if err != nil {
		return runtime.failRuntimeJob(ctx, job, err)
	}
	if len(decisions) != 1 {
		return runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: authoring review gate has %d decisions", ErrFrozenExecutionPayload, len(decisions)))
	}
	decision := decisions[0]
	run, err := runtime.core.store.GetWorkflowRun(ctx, binding.RunID)
	if err != nil {
		return runtime.failRuntimeJob(ctx, job, err)
	}
	if run == nil {
		return runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: authoring review Run %s", ErrLifecycleNotFound, binding.RunID))
	}
	if !isCurrentStandardAuthoringRun(*run) {
		return runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: authoring review Run is not bound to the current Standard authoring template", ErrFrozenExecutionPayload))
	}
	if err := runtime.core.verifyRunDeploymentCatalogReceipt(*run); err != nil {
		return runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: verify authoring review Run deployment catalog receipt: %v", ErrFrozenExecutionPayload, err))
	}
	subject, err := runtime.core.resolveWorkflowRunSubject(ctx, *run)
	if err != nil {
		return runtime.failRuntimeJob(ctx, job, err)
	}
	if !subject.isAuthoringSession() || subject.AuthoringSession.ID != binding.AuthoringSessionID || subject.AuthoringSource.ID != binding.AuthoringSourceID || subject.subjectDigest() != binding.SourceSnapshotDigest {
		return runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: authoring review binding differs from frozen Run subject", ErrFrozenExecutionPayload))
	}
	frozen, err := decodeFrozenRunDefinition(*run)
	if err != nil {
		return runtime.failRuntimeJob(ctx, job, err)
	}
	stage, found := frozen.Workflow.Stage(workflowkit.StageKey(binding.StageKey))
	if !found {
		return runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: frozen authoring workflow omits gate %q", ErrFrozenExecutionPayload, binding.StageKey))
	}
	review, found := frozen.ReviewStage(stage.Key)
	if !found || string(review.ReviewKind) != binding.ReviewKind {
		return runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: frozen authoring review metadata differs from durable binding", ErrFrozenExecutionPayload))
	}
	attempt, err := runtime.core.store.GetStageAttempt(ctx, binding.StageAttemptID)
	if err != nil {
		return runtime.failRuntimeJob(ctx, job, err)
	}
	if attempt == nil || attempt.RunID != run.ID || attempt.StageKey != binding.StageKey {
		return runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: authoring review StageAttempt is unavailable", ErrFrozenExecutionPayload))
	}
	node, err := runtime.reviewGateNodeAttempt(ctx, binding.StageAttemptID, binding.NodeAttemptID)
	if err != nil {
		return runtime.failRuntimeJob(ctx, job, err)
	}
	inputs, err := decodeAuthoringReviewGateInputs(binding)
	if err != nil {
		return runtime.failRuntimeJob(ctx, job, err)
	}
	if attempt.ExecutionStatus == store.StageExecutionWaiting {
		artifact, err := json.Marshal(authoringReviewGateDecisionArtifact{
			Format: authoringReviewGateDecisionArtifactFormat, ReviewRequestID: binding.ReviewRequestID, ReviewDecisionID: decision.ID,
			Action: decision.Action, AuthoringSourceID: binding.AuthoringSourceID, AuthoringSessionID: binding.AuthoringSessionID,
			SourceSnapshotDigest: binding.SourceSnapshotDigest, ReviewKind: binding.ReviewKind,
			EvidenceManifestDigest: binding.EvidenceManifestDigest, InputFingerprint: binding.InputFingerprint,
			DecisionActor: decision.Actor, DecisionReason: decision.Reason,
		})
		if err != nil {
			return runtime.failRuntimeJob(ctx, job, fmt.Errorf("encode authoring review decision evidence: %w", err))
		}
		manifest, _, err := persistStageArtifactsForSubject(ctx, runtime.core, *run, subject, *attempt, node, stage, inputs, []StageArtifact{{
			Key: review.DecisionArtifact.Name, SchemaVersion: review.DecisionArtifact.SchemaVersion, Content: artifact,
		}}, job.CreatedBy, "persist immutable authoring review gate decision")
		if err != nil {
			return runtime.failRuntimeJob(ctx, job, err)
		}
		completed, err := runtime.core.store.CompleteAuthoringReviewGateResolution(ctx, store.CompleteAuthoringReviewGateResolutionRequest{
			IdempotencyKey:  "complete-authoring-review-gate:" + binding.ID + ":" + decision.ID,
			ReviewRequestID: binding.ReviewRequestID, BindingID: binding.ID, DecisionID: decision.ID,
			RunID: run.ID, AuthoringSessionID: binding.AuthoringSessionID, AuthoringSourceID: binding.AuthoringSourceID,
			SourceSnapshotDigest: binding.SourceSnapshotDigest, DefinitionHash: binding.DefinitionHash,
			StageAttemptID: attempt.ID, NodeAttemptID: node.ID, InputFingerprint: binding.InputFingerprint,
			EvidenceManifestDigest: binding.EvidenceManifestDigest, ExpectedRunVersion: run.Version,
			ExpectedStageAttemptVersion: attempt.Version, ExpectedNodeAttemptVersion: node.Version,
			ArtifactManifestID:       manifest.ID,
			ResolutionEvidenceDigest: string(workflowkit.SHA256Fingerprint(artifact)),
			ResolutionPayloadJSON:    mustJSON(map[string]any{"format": authoringReviewGateDecisionArtifactFormat, "artifact_manifest_id": manifest.ID}),
			Actor:                    job.CreatedBy, Reason: "materialize frozen authoring review decision",
		})
		if err != nil {
			return runtime.failRuntimeJob(ctx, job, err)
		}
		attempt = &completed.StageAttempt
		*run = completed.Run
	}
	if attempt.ExecutionStatus != store.StageExecutionCompleted {
		return runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: authoring review resolution did not complete stage %s", ErrFrozenExecutionPayload, attempt.ID))
	}
	sourceJob, sourcePayload, err := runtime.authoringReviewGateSourceStageJob(ctx, *binding)
	if err != nil {
		return runtime.failRuntimeJob(ctx, job, err)
	}
	return runtime.projectResolvedAuthoringReviewGate(ctx, job, *run, frozen, sourceJob, sourcePayload, decision)
}

func decodeAuthoringReviewGateInputs(binding *store.AuthoringReviewGateBinding) ([]workflowkit.ArtifactBinding, error) {
	if binding == nil {
		return nil, fmt.Errorf("authoring review gate binding is required")
	}
	var inputs []workflowkit.ArtifactBinding
	if err := decodeStrictJSON(binding.InputBindingsJSON, &inputs); err != nil {
		return nil, fmt.Errorf("decode authoring review gate inputs: %w", err)
	}
	fingerprint, err := workflowkit.FingerprintArtifactBindings(inputs)
	if err != nil {
		return nil, fmt.Errorf("fingerprint authoring review gate inputs: %w", err)
	}
	if string(fingerprint) != binding.InputFingerprint {
		return nil, fmt.Errorf("%w: authoring review gate input binding fingerprint drift", ErrFrozenExecutionPayload)
	}
	return inputs, nil
}

func (runtime *FrozenExecutionRuntime) authoringReviewGateSourceStageJob(ctx context.Context, binding store.AuthoringReviewGateBinding) (store.DurableJob, frozenStageExecutionPayload, error) {
	jobs, err := runtime.core.store.ListDurableJobsForRun(ctx, binding.RunID)
	if err != nil {
		return store.DurableJob{}, frozenStageExecutionPayload{}, err
	}
	for _, job := range jobs {
		if job.CommandType != "stage_attempt.execute" || job.StageAttemptID != binding.StageAttemptID || job.EntityID != binding.StageAttemptID {
			continue
		}
		var payload frozenStageExecutionPayload
		if err := decodeStrictJSON(job.PayloadJSON, &payload); err != nil {
			return store.DurableJob{}, frozenStageExecutionPayload{}, err
		}
		if payload.Format == frozenStageExecutionPayloadFormat && payload.RunID == binding.RunID && payload.StageAttemptID == binding.StageAttemptID && payload.StageKey == workflowkit.StageKey(binding.StageKey) {
			return job, payload, nil
		}
	}
	return store.DurableJob{}, frozenStageExecutionPayload{}, fmt.Errorf("%w: source stage execution job for authoring review gate %s", ErrLifecycleNotFound, binding.StageAttemptID)
}

func (runtime *FrozenExecutionRuntime) projectResolvedAuthoringReviewGate(ctx context.Context, resolutionJob store.DurableJob, run store.WorkflowRun, frozen frozenRunDefinition, sourceJob store.DurableJob, sourcePayload frozenStageExecutionPayload, decision store.AuthoringReviewDecision) (store.JobState, error) {
	switch decision.Action {
	case store.ReviewDecisionApprove:
		if err := runtime.transitionRunToRunning(ctx, run, resolutionJob.CreatedBy, "approved durable authoring review gate"); err != nil {
			return runtime.failRuntimeJob(ctx, resolutionJob, err)
		}
		if err := runtime.enqueueNextCoordinator(ctx, sourceJob, run, frozen, sourcePayload); err != nil {
			return runtime.failRuntimeJob(ctx, resolutionJob, err)
		}
		return store.JobSucceeded, nil
	case store.ReviewDecisionRequestChanges:
		if err := runtime.finishRunWithStatus(ctx, run.ID, store.WorkflowRunWaitingContinuation, resolutionJob.CreatedBy, "authoring review gate requested changes"); err != nil {
			return store.JobFailed, err
		}
		return runtime.finishContinuationForRunOutcome(ctx, sourcePayload.ContinuationExecutionID, store.ContinuationExecutionCompleted, resolutionJob.CreatedBy, "authoring review gate requested changes")
	case store.ReviewDecisionRejectTerminal:
		if err := runtime.finishRunWithStatus(ctx, run.ID, store.WorkflowRunFailedTerminal, resolutionJob.CreatedBy, "authoring review gate rejected task"); err != nil {
			return store.JobFailed, err
		}
		return runtime.finishContinuationForRunOutcome(ctx, sourcePayload.ContinuationExecutionID, store.ContinuationExecutionFailed, resolutionJob.CreatedBy, "authoring review gate rejected task")
	default:
		return runtime.failRuntimeJob(ctx, resolutionJob, fmt.Errorf("%w: unsupported authoring review action %q", ErrFrozenExecutionPayload, decision.Action))
	}
}

func (runtime *FrozenExecutionRuntime) reconcileRecoveredAuthoringReviewGateResolution(ctx context.Context, job store.DurableJob) error {
	if job.CommandType != store.AuthoringReviewGateResolutionCommandType {
		return nil
	}
	_, err := runtime.handleAuthoringReviewGateResolution(ctx, DurableJobExecution{}, job)
	return err
}
