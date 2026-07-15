package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

const reviewGateResolutionPayloadFormat = "harbor.review-gate-resolution.v1"

// reviewGateResolutionPayload is written with the immutable decision job. It
// contains identities only; the worker re-reads the binding, decision, and
// frozen run before it publishes the decision artifact.
type reviewGateResolutionPayload struct {
	Format           string `json:"format"`
	ReviewRequestID  string `json:"review_request_id"`
	ReviewDecisionID string `json:"review_decision_id"`
	RunID            string `json:"run_id"`
	StageAttemptID   string `json:"stage_attempt_id"`
}

func (payload reviewGateResolutionPayload) validate() error {
	if payload.Format != reviewGateResolutionPayloadFormat {
		return fmt.Errorf("%w: unsupported review gate resolution payload format %q", ErrFrozenExecutionPayload, payload.Format)
	}
	for _, identity := range []struct {
		name  string
		value string
	}{
		{"review request", payload.ReviewRequestID},
		{"review decision", payload.ReviewDecisionID},
		{"run", payload.RunID},
		{"stage attempt", payload.StageAttemptID},
	} {
		if err := store.ValidateUUIDv7(identity.value); err != nil {
			return fmt.Errorf("%w: %s ID: %v", ErrFrozenExecutionPayload, identity.name, err)
		}
	}
	return nil
}

func newReviewGateResolutionPayload(binding store.ReviewGateBinding, decisionID string) (string, error) {
	payload := reviewGateResolutionPayload{
		Format: reviewGateResolutionPayloadFormat, ReviewRequestID: binding.ReviewRequestID,
		ReviewDecisionID: decisionID, RunID: binding.RunID, StageAttemptID: binding.StageAttemptID,
	}
	if err := payload.validate(); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode review gate resolution payload: %w", err)
	}
	return string(encoded), nil
}

func decodeReviewGateResolutionPayload(raw string) (reviewGateResolutionPayload, error) {
	var payload reviewGateResolutionPayload
	if err := decodeStrictJSON(raw, &payload); err != nil {
		return reviewGateResolutionPayload{}, fmt.Errorf("decode review gate resolution payload: %w", err)
	}
	if err := payload.validate(); err != nil {
		return reviewGateResolutionPayload{}, err
	}
	return payload, nil
}

func (service *ReviewService) decideReviewGate(ctx context.Context, binding store.ReviewGateBinding, request DecideReviewRequest) (store.ReviewDecision, error) {
	if request.ReviewRequestID != binding.ReviewRequestID || request.RevisionID != binding.RevisionID ||
		strings.TrimSpace(request.ExpectedRevisionDigest) != binding.RevisionDigest {
		return store.ReviewDecision{}, fmt.Errorf("review decision does not match immutable review gate binding")
	}
	// The evaluator-evidence gate is special only at the Harbor application
	// boundary: its approval is meaningful solely when a verified child-to-
	// parent handoff exists. Recheck before both a new decision and idempotent
	// replay; workflowkit itself remains entirely domain-neutral.
	if _, err := service.core.verifyCodeEdgeEvaluatorEvidenceHandoffGate(ctx, binding); err != nil {
		return store.ReviewDecision{}, err
	}

	// Direct application callers without a client idempotency key still get a
	// stable replay when the gate already has the exact immutable decision.
	existing, err := service.core.store.ListReviewDecisionsForRequest(ctx, binding.ReviewRequestID)
	if err != nil {
		return store.ReviewDecision{}, err
	}
	if len(existing) > 1 {
		return store.ReviewDecision{}, fmt.Errorf("review gate %s has multiple decisions", binding.StageAttemptID)
	}
	if len(existing) == 1 {
		decision := existing[0]
		if (strings.TrimSpace(request.ID) != "" && request.ID != decision.ID) || decision.RevisionID != request.RevisionID ||
			decision.Action != request.Action || decision.ExpectedRevisionDigest != request.ExpectedRevisionDigest ||
			decision.Actor != strings.TrimSpace(request.Actor) || decision.Reason != strings.TrimSpace(request.Reason) {
			return store.ReviewDecision{}, fmt.Errorf("%w: review gate decision %s", store.ErrIdempotencyConflict, binding.ReviewRequestID)
		}
		return decision, nil
	}

	decisionID := strings.TrimSpace(request.ID)
	if decisionID == "" {
		decisionID, err = store.NewUUIDv7()
		if err != nil {
			return store.ReviewDecision{}, fmt.Errorf("allocate review gate decision identity: %w", err)
		}
	}
	if err := store.ValidateUUIDv7(decisionID); err != nil {
		return store.ReviewDecision{}, err
	}
	resolutionJobID, err := store.NewUUIDv7()
	if err != nil {
		return store.ReviewDecision{}, fmt.Errorf("allocate review gate resolution job identity: %w", err)
	}
	payload, err := newReviewGateResolutionPayload(binding, decisionID)
	if err != nil {
		return store.ReviewDecision{}, err
	}
	run, err := service.core.store.GetWorkflowRun(ctx, binding.RunID)
	if err != nil {
		return store.ReviewDecision{}, err
	}
	if run == nil {
		return store.ReviewDecision{}, fmt.Errorf("%w: workflow run %s", ErrLifecycleNotFound, binding.RunID)
	}
	stage, err := service.core.store.GetStageAttempt(ctx, binding.StageAttemptID)
	if err != nil {
		return store.ReviewDecision{}, err
	}
	if stage == nil {
		return store.ReviewDecision{}, fmt.Errorf("%w: stage attempt %s", ErrLifecycleNotFound, binding.StageAttemptID)
	}
	result, err := service.core.store.RecordReviewGateDecision(ctx, store.RecordReviewGateDecisionRequest{
		ID: decisionID, ReviewRequestID: binding.ReviewRequestID, RunID: binding.RunID, RevisionID: binding.RevisionID,
		StageAttemptID: binding.StageAttemptID, ExpectedRevisionDigest: binding.RevisionDigest,
		ExpectedRunVersion: run.Version, ExpectedStageAttemptVersion: stage.Version, Action: request.Action,
		ResolutionJobID: resolutionJobID, ResolutionPayloadJSON: payload, Actor: request.Actor, Reason: request.Reason,
	})
	if err != nil {
		return store.ReviewDecision{}, err
	}
	return result.Decision, nil
}
