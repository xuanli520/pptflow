package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
)

const authoringReviewGateResolutionPayloadFormat = "harbor.authoring-review-gate-resolution.v1"

// AuthoringReviewGateSnapshot is the complete durable operator-review view
// for one pre-materialization AuthoringSession gate. It deliberately exposes
// no TaskRevision because that subject does not exist yet.
type AuthoringReviewGateSnapshot struct {
	Request      store.AuthoringReviewRequest
	Binding      store.AuthoringReviewGateBinding
	Run          store.WorkflowRun
	StageAttempt store.StageAttempt
	Decisions    []store.AuthoringReviewDecision
	Resolution   *store.AuthoringReviewGateResolution
	State        store.AuthoringReviewGateState
}

// AuthoringReviewCheckpoint freezes every source/session coordinate and both
// mutable CAS versions that an operator inspected before deciding a gate.
// The Store repeats these checks atomically at mutation time.
type AuthoringReviewCheckpoint struct {
	ReviewRequestID        string
	BindingID              string
	RunID                  string
	AuthoringSessionID     string
	AuthoringSourceID      string
	SourceSnapshotDigest   string
	DefinitionHash         string
	StageAttemptID         string
	InputFingerprint       string
	EvidenceManifestDigest string
	RunVersion             int64
	StageAttemptVersion    int64
}

// DecideAuthoringReviewRequest is the application-level command for one
// durable authoring review decision. IdempotencyKey is a client-generated
// UUIDv7; optional IDs are useful only to controlled callers that require
// their own UUIDv7 allocation.
type DecideAuthoringReviewRequest struct {
	ID              string
	ResolutionJobID string
	IdempotencyKey  string
	Action          store.ReviewDecisionAction
	Actor           string
	Reason          string
	Expected        AuthoringReviewCheckpoint
}

// AuthoringReviewService owns source/session review inspection and decisions.
// It must not be merged into ReviewService: ReviewService only accepts real
// TaskRevision review facts and lifecycle mutations.
type AuthoringReviewService struct{ core *lifecycleServiceCore }

// Inspect returns the complete durable authoring gate state after verifying
// that its binding still matches the frozen AuthoringSource/Session Run.
func (service *AuthoringReviewService) Inspect(ctx context.Context, reviewRequestID string) (AuthoringReviewGateSnapshot, error) {
	if service == nil || service.core == nil || service.core.store == nil {
		return AuthoringReviewGateSnapshot{}, fmt.Errorf("authoring review service is not configured")
	}
	reviewRequestID = strings.TrimSpace(reviewRequestID)
	if err := store.ValidateUUIDv7(reviewRequestID); err != nil {
		return AuthoringReviewGateSnapshot{}, fmt.Errorf("authoring review request ID: %w", err)
	}
	request, err := service.core.store.GetAuthoringReviewRequest(ctx, reviewRequestID)
	if err != nil {
		return AuthoringReviewGateSnapshot{}, err
	}
	if request == nil {
		return AuthoringReviewGateSnapshot{}, fmt.Errorf("%w: authoring review request %s", ErrLifecycleNotFound, reviewRequestID)
	}
	binding, err := service.core.store.GetAuthoringReviewGateBindingByRequest(ctx, reviewRequestID)
	if err != nil {
		return AuthoringReviewGateSnapshot{}, err
	}
	if binding == nil {
		return AuthoringReviewGateSnapshot{}, fmt.Errorf("%w: authoring review binding for request %s", ErrLifecycleNotFound, reviewRequestID)
	}
	if err := validateAuthoringReviewRequestBinding(*request, *binding); err != nil {
		return AuthoringReviewGateSnapshot{}, err
	}
	run, err := service.core.store.GetWorkflowRun(ctx, binding.RunID)
	if err != nil {
		return AuthoringReviewGateSnapshot{}, err
	}
	if run == nil {
		return AuthoringReviewGateSnapshot{}, fmt.Errorf("%w: authoring review Run %s", ErrLifecycleNotFound, binding.RunID)
	}
	if !isCurrentStandardAuthoringRun(*run) {
		return AuthoringReviewGateSnapshot{}, fmt.Errorf("authoring review Run is not %s@%s", workflowadapter.StandardAuthoringWorkflowTemplateID, workflowadapter.StandardAuthoringTestsAnalysisInputTemplateVersion)
	}
	subject, err := service.core.resolveWorkflowRunSubject(ctx, *run)
	if err != nil {
		return AuthoringReviewGateSnapshot{}, err
	}
	if !subject.isAuthoringSession() || subject.AuthoringSession.ID != binding.AuthoringSessionID ||
		subject.AuthoringSource.ID != binding.AuthoringSourceID || subject.subjectDigest() != binding.SourceSnapshotDigest ||
		run.DefinitionHash != binding.DefinitionHash {
		return AuthoringReviewGateSnapshot{}, fmt.Errorf("%w: authoring review binding differs from frozen source/session Run", store.ErrImmutable)
	}
	stage, err := service.core.store.GetStageAttempt(ctx, binding.StageAttemptID)
	if err != nil {
		return AuthoringReviewGateSnapshot{}, err
	}
	if stage == nil {
		return AuthoringReviewGateSnapshot{}, fmt.Errorf("%w: authoring review StageAttempt %s", ErrLifecycleNotFound, binding.StageAttemptID)
	}
	if stage.RunID != run.ID || stage.StageKey != binding.StageKey || stage.InputFingerprint != binding.InputFingerprint {
		return AuthoringReviewGateSnapshot{}, fmt.Errorf("%w: authoring review StageAttempt differs from immutable binding", store.ErrImmutable)
	}
	decisions, err := service.core.store.ListAuthoringReviewDecisionsForRequest(ctx, request.ID)
	if err != nil {
		return AuthoringReviewGateSnapshot{}, err
	}
	if len(decisions) > 1 {
		return AuthoringReviewGateSnapshot{}, fmt.Errorf("%w: authoring review request %s has %d decisions", store.ErrImmutable, request.ID, len(decisions))
	}
	state, err := service.core.store.GetAuthoringReviewGateState(ctx, request.ID)
	if err != nil {
		return AuthoringReviewGateSnapshot{}, err
	}
	resolution, err := service.core.store.GetAuthoringReviewGateResolution(ctx, request.ID)
	if err != nil {
		return AuthoringReviewGateSnapshot{}, err
	}
	return AuthoringReviewGateSnapshot{
		Request: *request, Binding: *binding, Run: *run, StageAttempt: *stage,
		Decisions: append([]store.AuthoringReviewDecision(nil), decisions...), Resolution: resolution, State: state,
	}, nil
}

// CaptureCheckpoint reads the exact gate facts that must still hold when an
// operator confirms a decision. A decided or completed gate may be captured
// as well so a lost response can replay the same idempotency key safely.
func (service *AuthoringReviewService) CaptureCheckpoint(ctx context.Context, reviewRequestID string) (AuthoringReviewCheckpoint, error) {
	snapshot, err := service.Inspect(ctx, reviewRequestID)
	if err != nil {
		return AuthoringReviewCheckpoint{}, err
	}
	return authoringReviewCheckpoint(snapshot), nil
}

// Decide records one immutable operator decision and queues its local
// resolution job. The Store repeats the binding and CAS checks in one
// transaction, so this read-before-write is only an early, useful rejection.
func (service *AuthoringReviewService) Decide(ctx context.Context, request DecideAuthoringReviewRequest) (store.AuthoringReviewGateDecisionResult, error) {
	if service == nil || service.core == nil || service.core.store == nil {
		return store.AuthoringReviewGateDecisionResult{}, fmt.Errorf("authoring review service is not configured")
	}
	if err := validateDecideAuthoringReviewRequest(request); err != nil {
		return store.AuthoringReviewGateDecisionResult{}, err
	}
	snapshot, err := service.Inspect(ctx, request.Expected.ReviewRequestID)
	if err != nil {
		return store.AuthoringReviewGateDecisionResult{}, err
	}
	if err := request.Expected.matches(snapshot); err != nil {
		return store.AuthoringReviewGateDecisionResult{}, err
	}
	if snapshot.State == store.AuthoringReviewGateOpen && (snapshot.Run.Status != store.WorkflowRunWaitingReview ||
		snapshot.StageAttempt.ExecutionStatus != store.StageExecutionWaiting) {
		return store.AuthoringReviewGateDecisionResult{}, fmt.Errorf("%w: authoring review gate is not waiting for an operator decision", store.ErrInvalidTransition)
	}
	if snapshot.State != store.AuthoringReviewGateOpen && len(snapshot.Decisions) != 1 {
		return store.AuthoringReviewGateDecisionResult{}, fmt.Errorf("%w: authoring review gate state %s has no immutable decision", store.ErrImmutable, snapshot.State)
	}
	payload, err := newAuthoringReviewGateResolutionPayload(snapshot.Binding, request.Expected)
	if err != nil {
		return store.AuthoringReviewGateDecisionResult{}, err
	}
	return service.core.store.DecideAuthoringReviewGate(ctx, store.DecideAuthoringReviewGateRequest{
		ID: request.ID, ResolutionJobID: request.ResolutionJobID, IdempotencyKey: strings.TrimSpace(request.IdempotencyKey),
		ReviewRequestID: request.Expected.ReviewRequestID, BindingID: request.Expected.BindingID,
		RunID: request.Expected.RunID, AuthoringSessionID: request.Expected.AuthoringSessionID,
		AuthoringSourceID: request.Expected.AuthoringSourceID, SourceSnapshotDigest: request.Expected.SourceSnapshotDigest,
		DefinitionHash: request.Expected.DefinitionHash, StageAttemptID: request.Expected.StageAttemptID,
		InputFingerprint: request.Expected.InputFingerprint, EvidenceManifestDigest: request.Expected.EvidenceManifestDigest,
		ExpectedRunVersion: request.Expected.RunVersion, ExpectedStageAttemptVersion: request.Expected.StageAttemptVersion,
		Action: request.Action, ResolutionPayloadJSON: payload, Actor: strings.TrimSpace(request.Actor), Reason: strings.TrimSpace(request.Reason),
	})
}

func authoringReviewCheckpoint(snapshot AuthoringReviewGateSnapshot) AuthoringReviewCheckpoint {
	return AuthoringReviewCheckpoint{
		ReviewRequestID: snapshot.Request.ID, BindingID: snapshot.Binding.ID, RunID: snapshot.Binding.RunID,
		AuthoringSessionID: snapshot.Binding.AuthoringSessionID, AuthoringSourceID: snapshot.Binding.AuthoringSourceID,
		SourceSnapshotDigest: snapshot.Binding.SourceSnapshotDigest, DefinitionHash: snapshot.Binding.DefinitionHash,
		StageAttemptID: snapshot.Binding.StageAttemptID, InputFingerprint: snapshot.Binding.InputFingerprint,
		EvidenceManifestDigest: snapshot.Binding.EvidenceManifestDigest, RunVersion: snapshot.Run.Version,
		StageAttemptVersion: snapshot.StageAttempt.Version,
	}
}

func validateAuthoringReviewRequestBinding(request store.AuthoringReviewRequest, binding store.AuthoringReviewGateBinding) error {
	if request.ID != binding.ReviewRequestID || request.RunID != binding.RunID ||
		request.AuthoringSessionID != binding.AuthoringSessionID || request.AuthoringSourceID != binding.AuthoringSourceID ||
		request.SourceSnapshotDigest != binding.SourceSnapshotDigest || request.DefinitionHash != binding.DefinitionHash ||
		request.EvidenceManifestDigest != binding.EvidenceManifestDigest {
		return fmt.Errorf("%w: authoring review request and binding differ", store.ErrImmutable)
	}
	return nil
}

func validateDecideAuthoringReviewRequest(request DecideAuthoringReviewRequest) error {
	if err := store.ValidateUUIDv7(strings.TrimSpace(request.IdempotencyKey)); err != nil {
		return fmt.Errorf("authoring review idempotency key: %w", err)
	}
	for _, identity := range []struct {
		name  string
		value string
	}{
		{"authoring review decision ID", request.ID},
		{"authoring review resolution job ID", request.ResolutionJobID},
	} {
		if strings.TrimSpace(identity.value) == "" {
			continue
		}
		if err := store.ValidateUUIDv7(strings.TrimSpace(identity.value)); err != nil {
			return fmt.Errorf("%s: %w", identity.name, err)
		}
	}
	if strings.TrimSpace(request.Actor) == "" || strings.TrimSpace(request.Reason) == "" {
		return fmt.Errorf("authoring review decision actor and reason are required")
	}
	if !validAuthoringReviewDecisionAction(request.Action) {
		return fmt.Errorf("invalid authoring review decision action %q", request.Action)
	}
	return request.Expected.validate()
}

func validAuthoringReviewDecisionAction(action store.ReviewDecisionAction) bool {
	switch action {
	case store.ReviewDecisionApprove, store.ReviewDecisionRequestChanges, store.ReviewDecisionRejectTerminal:
		return true
	default:
		return false
	}
}

func (checkpoint AuthoringReviewCheckpoint) validate() error {
	for _, identity := range []struct {
		name  string
		value string
	}{
		{"authoring review request ID", checkpoint.ReviewRequestID},
		{"authoring review binding ID", checkpoint.BindingID},
		{"authoring review Run ID", checkpoint.RunID},
		{"authoring session ID", checkpoint.AuthoringSessionID},
		{"authoring source ID", checkpoint.AuthoringSourceID},
		{"authoring review StageAttempt ID", checkpoint.StageAttemptID},
	} {
		if err := store.ValidateUUIDv7(strings.TrimSpace(identity.value)); err != nil {
			return fmt.Errorf("%s: %w", identity.name, err)
		}
	}
	if strings.TrimSpace(checkpoint.SourceSnapshotDigest) == "" || strings.TrimSpace(checkpoint.DefinitionHash) == "" ||
		strings.TrimSpace(checkpoint.InputFingerprint) == "" || strings.TrimSpace(checkpoint.EvidenceManifestDigest) == "" {
		return fmt.Errorf("authoring review checkpoint has incomplete immutable binding")
	}
	if checkpoint.RunVersion <= 0 || checkpoint.StageAttemptVersion <= 0 {
		return fmt.Errorf("authoring review checkpoint requires positive Run and StageAttempt versions")
	}
	return nil
}

func (checkpoint AuthoringReviewCheckpoint) matches(snapshot AuthoringReviewGateSnapshot) error {
	current := authoringReviewCheckpoint(snapshot)
	if checkpoint.ReviewRequestID != current.ReviewRequestID || checkpoint.BindingID != current.BindingID ||
		checkpoint.RunID != current.RunID || checkpoint.AuthoringSessionID != current.AuthoringSessionID ||
		checkpoint.AuthoringSourceID != current.AuthoringSourceID || checkpoint.SourceSnapshotDigest != current.SourceSnapshotDigest ||
		checkpoint.DefinitionHash != current.DefinitionHash || checkpoint.StageAttemptID != current.StageAttemptID ||
		checkpoint.InputFingerprint != current.InputFingerprint || checkpoint.EvidenceManifestDigest != current.EvidenceManifestDigest {
		return fmt.Errorf("%w: authoring review checkpoint does not match immutable source/session gate", store.ErrImmutable)
	}
	if checkpoint.RunVersion != current.RunVersion || checkpoint.StageAttemptVersion != current.StageAttemptVersion {
		return fmt.Errorf("%w: authoring review Run or StageAttempt has changed", store.ErrOptimisticLock)
	}
	return nil
}

type authoringReviewGateResolutionPayload struct {
	Format                 string `json:"format"`
	ReviewRequestID        string `json:"review_request_id"`
	BindingID              string `json:"binding_id"`
	RunID                  string `json:"run_id"`
	StageAttemptID         string `json:"stage_attempt_id"`
	AuthoringSessionID     string `json:"authoring_session_id"`
	AuthoringSourceID      string `json:"authoring_source_id"`
	SourceSnapshotDigest   string `json:"source_snapshot_digest"`
	DefinitionHash         string `json:"definition_hash"`
	InputFingerprint       string `json:"input_fingerprint"`
	EvidenceManifestDigest string `json:"evidence_manifest_digest"`
}

func newAuthoringReviewGateResolutionPayload(binding store.AuthoringReviewGateBinding, checkpoint AuthoringReviewCheckpoint) (string, error) {
	if binding.ID != checkpoint.BindingID || binding.ReviewRequestID != checkpoint.ReviewRequestID || binding.RunID != checkpoint.RunID ||
		binding.AuthoringSessionID != checkpoint.AuthoringSessionID || binding.AuthoringSourceID != checkpoint.AuthoringSourceID ||
		binding.SourceSnapshotDigest != checkpoint.SourceSnapshotDigest || binding.DefinitionHash != checkpoint.DefinitionHash ||
		binding.StageAttemptID != checkpoint.StageAttemptID || binding.InputFingerprint != checkpoint.InputFingerprint ||
		binding.EvidenceManifestDigest != checkpoint.EvidenceManifestDigest {
		return "", fmt.Errorf("%w: authoring review resolution payload binding mismatch", store.ErrImmutable)
	}
	payload, err := json.Marshal(authoringReviewGateResolutionPayload{
		Format: authoringReviewGateResolutionPayloadFormat, ReviewRequestID: checkpoint.ReviewRequestID,
		BindingID: checkpoint.BindingID, RunID: checkpoint.RunID, StageAttemptID: checkpoint.StageAttemptID,
		AuthoringSessionID: checkpoint.AuthoringSessionID, AuthoringSourceID: checkpoint.AuthoringSourceID,
		SourceSnapshotDigest: checkpoint.SourceSnapshotDigest, DefinitionHash: checkpoint.DefinitionHash,
		InputFingerprint: checkpoint.InputFingerprint, EvidenceManifestDigest: checkpoint.EvidenceManifestDigest,
	})
	if err != nil {
		return "", fmt.Errorf("encode authoring review resolution payload: %w", err)
	}
	return string(payload), nil
}
