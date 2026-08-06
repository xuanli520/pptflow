package app

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const reviewGateDecisionArtifactFormat = "harbor.review-gate-decision.v1"

// reviewGateDecisionArtifact is the typed evidence emitted only after the
// operator decision has been durably recorded. It deliberately contains no
// mutable task path or provider reference.
type reviewGateDecisionArtifact struct {
	Format                 string                     `json:"format"`
	ReviewRequestID        string                     `json:"review_request_id"`
	ReviewDecisionID       string                     `json:"review_decision_id"`
	Action                 store.ReviewDecisionAction `json:"action"`
	RevisionID             string                     `json:"revision_id"`
	RevisionDigest         string                     `json:"revision_digest"`
	ReviewKind             string                     `json:"review_kind"`
	EvidenceManifestDigest string                     `json:"evidence_manifest_digest"`
	InputFingerprint       string                     `json:"input_fingerprint"`
	DecisionActor          string                     `json:"decision_actor"`
	DecisionReason         string                     `json:"decision_reason"`
}

func (runtime *FrozenExecutionRuntime) handleReviewGateResolution(ctx context.Context, _ DurableJobExecution, job store.DurableJob) (store.JobState, error) {
	var payload reviewGateResolutionPayload
	if err := decodeStrictJSON(job.PayloadJSON, &payload); err != nil {
		return runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: decode review gate resolution payload: %w", ErrFrozenExecutionPayload, err))
	}
	if err := payload.validate(); err != nil {
		return runtime.failRuntimeJob(ctx, job, err)
	}
	if job.CommandType != store.ReviewGateResolutionCommandType || job.EntityType != "stage_attempt" ||
		job.EntityID != payload.StageAttemptID || job.RunID != payload.RunID || job.StageAttemptID != payload.StageAttemptID {
		return runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: review gate resolution job does not bind its payload", ErrFrozenExecutionPayload))
	}
	binding, err := runtime.core.store.GetReviewGateBindingByReviewRequest(ctx, payload.ReviewRequestID)
	if err != nil {
		return runtime.failRuntimeJob(ctx, job, err)
	}
	if binding == nil || binding.RunID != payload.RunID || binding.StageAttemptID != payload.StageAttemptID {
		return runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: review gate resolution payload does not match a frozen gate", ErrFrozenExecutionPayload))
	}
	decision, err := runtime.reviewGateDecision(ctx, binding.ReviewRequestID, payload.ReviewDecisionID)
	if err != nil {
		return runtime.failRuntimeJob(ctx, job, err)
	}
	run, err := runtime.core.store.GetWorkflowRun(ctx, binding.RunID)
	if err != nil {
		return runtime.failRuntimeJob(ctx, job, err)
	}
	if run == nil {
		return runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: review gate run %s", ErrLifecycleNotFound, binding.RunID))
	}
	frozen, err := decodeFrozenRunDefinition(*run)
	if err != nil {
		return runtime.failRuntimeJob(ctx, job, err)
	}
	stage, found := frozen.Workflow.Stage(workflowkit.StageKey(binding.StageKey))
	if !found {
		return runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: frozen workflow omits review gate stage %q", ErrFrozenExecutionPayload, binding.StageKey))
	}
	review, found := frozen.ReviewStage(stage.Key)
	if !found || string(review.ReviewKind) != binding.ReviewKind {
		return runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: frozen review metadata differs from durable gate binding", ErrFrozenExecutionPayload))
	}
	revision, err := runtime.core.store.GetTaskRevision(ctx, binding.RevisionID)
	if err != nil {
		return runtime.failRuntimeJob(ctx, job, err)
	}
	if revision == nil || revision.TaskID != run.TaskID || revision.TaskDigest != binding.RevisionDigest {
		return runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: review gate revision is unavailable or changed", ErrFrozenExecutionPayload))
	}
	attempt, err := runtime.core.store.GetStageAttempt(ctx, binding.StageAttemptID)
	if err != nil {
		return runtime.failRuntimeJob(ctx, job, err)
	}
	if attempt == nil {
		return runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: review gate stage attempt %s", ErrLifecycleNotFound, binding.StageAttemptID))
	}
	node, err := runtime.reviewGateNodeAttempt(ctx, binding.StageAttemptID, binding.NodeAttemptID)
	if err != nil {
		return runtime.failRuntimeJob(ctx, job, err)
	}
	inputs, err := decodeReviewGateInputs(binding)
	if err != nil {
		return runtime.failRuntimeJob(ctx, job, err)
	}
	if attempt.ExecutionStatus == store.StageExecutionWaiting {
		artifact, err := json.Marshal(reviewGateDecisionArtifact{
			Format: reviewGateDecisionArtifactFormat, ReviewRequestID: binding.ReviewRequestID, ReviewDecisionID: decision.ID,
			Action: decision.Action, RevisionID: binding.RevisionID, RevisionDigest: binding.RevisionDigest,
			ReviewKind: binding.ReviewKind, EvidenceManifestDigest: binding.EvidenceManifestDigest,
			InputFingerprint: binding.InputFingerprint, DecisionActor: decision.Actor, DecisionReason: decision.Reason,
		})
		if err != nil {
			return runtime.failRuntimeJob(ctx, job, fmt.Errorf("encode review gate decision evidence: %w", err))
		}
		manifest, _, err := persistStageArtifacts(ctx, runtime.core, *run, *revision, *attempt, node, stage, inputs, []StageArtifact{{
			Key: review.DecisionArtifact.Name, SchemaVersion: review.DecisionArtifact.SchemaVersion, Content: artifact,
		}}, job.CreatedBy, "persist immutable review gate decision")
		if err != nil {
			return runtime.failRuntimeJob(ctx, job, err)
		}
		resolved, err := runtime.core.store.CompleteReviewGateResolution(ctx, store.CompleteReviewGateResolutionRequest{
			ReviewRequestID: binding.ReviewRequestID, ReviewDecisionID: decision.ID, RunID: run.ID, StageAttemptID: attempt.ID,
			ExpectedRunVersion: run.Version, ExpectedStageAttemptVersion: attempt.Version, ExpectedNodeAttemptVersion: node.Version,
			ArtifactManifestID: manifest.ID, Actor: job.CreatedBy, Reason: "materialize frozen review gate decision",
		})
		if err != nil {
			return runtime.failRuntimeJob(ctx, job, err)
		}
		attempt = &resolved.StageAttempt
	}
	if attempt.ExecutionStatus != store.StageExecutionCompleted {
		return runtime.failRuntimeJob(ctx, job, fmt.Errorf("%w: review gate resolution did not complete stage %s", ErrFrozenExecutionPayload, attempt.ID))
	}
	sourceJob, sourcePayload, err := runtime.reviewGateSourceStageJob(ctx, binding)
	if err != nil {
		return runtime.failRuntimeJob(ctx, job, err)
	}
	return runtime.projectResolvedReviewGate(ctx, job, *run, frozen, sourceJob, sourcePayload, *attempt, decision)
}

func (runtime *FrozenExecutionRuntime) reviewGateDecision(ctx context.Context, reviewRequestID, decisionID string) (store.ReviewDecision, error) {
	decisions, err := runtime.core.store.ListReviewDecisionsForRequest(ctx, reviewRequestID)
	if err != nil {
		return store.ReviewDecision{}, err
	}
	for _, decision := range decisions {
		if decision.ID == decisionID {
			return decision, nil
		}
	}
	return store.ReviewDecision{}, fmt.Errorf("%w: review gate decision %s", ErrLifecycleNotFound, decisionID)
}

func (runtime *FrozenExecutionRuntime) reviewGateNodeAttempt(ctx context.Context, stageAttemptID, nodeAttemptID string) (store.NodeAttempt, error) {
	attempts, err := runtime.core.store.ListNodeAttempts(ctx, stageAttemptID)
	if err != nil {
		return store.NodeAttempt{}, err
	}
	for _, attempt := range attempts {
		if attempt.ID == nodeAttemptID {
			return attempt, nil
		}
	}
	return store.NodeAttempt{}, fmt.Errorf("%w: review gate node attempt %s", ErrLifecycleNotFound, nodeAttemptID)
}

func decodeReviewGateInputs(binding *store.ReviewGateBinding) ([]workflowkit.ArtifactBinding, error) {
	if binding == nil {
		return nil, fmt.Errorf("review gate binding is required")
	}
	var inputs []workflowkit.ArtifactBinding
	if err := decodeStrictJSON(binding.InputBindingsJSON, &inputs); err != nil {
		return nil, fmt.Errorf("decode review gate input bindings: %w", err)
	}
	fingerprint, err := workflowkit.FingerprintArtifactBindings(inputs)
	if err != nil {
		return nil, fmt.Errorf("fingerprint review gate input bindings: %w", err)
	}
	if string(fingerprint) != binding.InputFingerprint {
		return nil, fmt.Errorf("%w: review gate input binding fingerprint drift", ErrFrozenExecutionPayload)
	}
	return inputs, nil
}

func (runtime *FrozenExecutionRuntime) reviewGateSourceStageJob(ctx context.Context, binding *store.ReviewGateBinding) (store.DurableJob, frozenStageExecutionPayload, error) {
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
	return store.DurableJob{}, frozenStageExecutionPayload{}, fmt.Errorf("%w: source stage execution job for review gate %s", ErrLifecycleNotFound, binding.StageAttemptID)
}

func (runtime *FrozenExecutionRuntime) projectResolvedReviewGate(ctx context.Context, resolutionJob store.DurableJob, run store.WorkflowRun, frozen frozenRunDefinition, sourceJob store.DurableJob, sourcePayload frozenStageExecutionPayload, attempt store.StageAttempt, decision store.ReviewDecision) (store.JobState, error) {
	switch decision.Action {
	case store.ReviewDecisionApprove:
		if err := runtime.transitionRunToRunning(ctx, run, resolutionJob.CreatedBy, "approved durable review gate"); err != nil {
			return runtime.failRuntimeJob(ctx, resolutionJob, err)
		}
		if err := runtime.enqueueNextCoordinator(ctx, sourceJob, run, frozen, sourcePayload); err != nil {
			return runtime.failRuntimeJob(ctx, resolutionJob, err)
		}
		return store.JobSucceeded, nil
	case store.ReviewDecisionRequestChanges:
		if err := runtime.finishRunWithStatus(ctx, run.ID, store.WorkflowRunWaitingContinuation, resolutionJob.CreatedBy, "review gate requested task changes"); err != nil {
			return store.JobFailed, err
		}
		return runtime.finishContinuationForRunOutcome(ctx, sourcePayload.ContinuationExecutionID, store.ContinuationExecutionCompleted, resolutionJob.CreatedBy, "review gate requested task changes")
	case store.ReviewDecisionRejectTerminal:
		if err := runtime.finishRunWithStatus(ctx, run.ID, store.WorkflowRunFailedTerminal, resolutionJob.CreatedBy, "review gate rejected task"); err != nil {
			return store.JobFailed, err
		}
		return runtime.finishContinuationForRunOutcome(ctx, sourcePayload.ContinuationExecutionID, store.ContinuationExecutionFailed, resolutionJob.CreatedBy, "review gate rejected task")
	default:
		return runtime.failRuntimeJob(ctx, resolutionJob, fmt.Errorf("%w: unsupported review gate action %q", ErrFrozenExecutionPayload, decision.Action))
	}
}

// An expired resolution job is never re-issued with a new decision. The
// materialization operation is idempotent, so recovery may safely finish the
// same frozen work and project the run outcome from its durable facts.
func (runtime *FrozenExecutionRuntime) reconcileRecoveredReviewGateResolution(ctx context.Context, job store.DurableJob) error {
	if job.CommandType != store.ReviewGateResolutionCommandType {
		return nil
	}
	_, err := runtime.handleReviewGateResolution(ctx, DurableJobExecution{}, job)
	return err
}
