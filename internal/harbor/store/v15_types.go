package store

import "time"

const (
	// ReviewGateResolutionCommandType is consumed only by the controlled V2
	// worker after an immutable ReviewDecision has been recorded.
	ReviewGateResolutionCommandType = "stage_attempt.review_gate_resolution"
	reviewGateResolutionEntityType  = "stage_attempt"
)

// ReviewGateBinding is the immutable relation between one durable review
// StageAttempt and one generic ReviewRequest. It deliberately has no separate
// identity: the stage attempt is its stable primary key, which avoids adding a
// second lifecycle identity to the global UUIDv7 namespace.
type ReviewGateBinding struct {
	StageAttemptID         string
	ReviewRequestID        string
	RunID                  string
	RevisionID             string
	RevisionDigest         string
	DefinitionHash         string
	StageKey               string
	ReviewKind             string
	NodeAttemptID          string
	InputBindingsJSON      string
	InputFingerprint       string
	EvidenceManifestDigest string
	CreatedAt              time.Time
}

// OpenReviewGateRequest freezes the evidence observed by a gate before it
// enters durable waiting. Optional review/node IDs are allocated by Store when
// omitted; a supplied ID is retained for deterministic integration fixtures.
type OpenReviewGateRequest struct {
	ReviewRequestID string
	NodeAttemptID   string

	RunID                       string
	ExpectedRunVersion          int64
	RevisionID                  string
	RevisionDigest              string
	DefinitionHash              string
	StageAttemptID              string
	ExpectedStageAttemptVersion int64
	StageKey                    string
	ReviewKind                  string
	NodeGeneration              int
	NodeAttempt                 int
	InputBindingsJSON           string
	InputFingerprint            string
	EvidenceManifestDigest      string
	Actor                       string
	Reason                      string
}

// ReviewGateOpenResult is the coherent post-transaction state of opening a
// review gate. The Run and Stage remain non-terminal while the worker lease is
// released, so a UI may exit without canceling the human review.
type ReviewGateOpenResult struct {
	Binding      ReviewGateBinding
	Review       ReviewRequest
	Run          WorkflowRun
	StageAttempt StageAttempt
	NodeAttempt  NodeAttempt
}

// RecordReviewGateDecisionRequest closes one bound ReviewRequest and atomically
// queues the local resolution job. The job payload is opaque to Store but its
// identity and scope are derived here rather than from a caller-provided key.
type RecordReviewGateDecisionRequest struct {
	ID string

	ReviewRequestID             string
	RunID                       string
	RevisionID                  string
	StageAttemptID              string
	ExpectedRevisionDigest      string
	ExpectedRunVersion          int64
	ExpectedStageAttemptVersion int64
	Action                      ReviewDecisionAction
	ResolutionJobID             string
	ResolutionPayloadJSON       string
	ResolutionPriority          int
	Actor                       string
	Reason                      string
}

// ReviewGateDecisionResult exposes both immutable decision facts and the one
// durable job responsible for materializing its review-decision artifact.
type ReviewGateDecisionResult struct {
	Binding       ReviewGateBinding
	Decision      ReviewDecision
	ResolutionJob DurableJob
}

// CompleteReviewGateResolutionRequest proves that the worker materialized the
// immutable decision artifact for exactly the review binding it claimed.
type CompleteReviewGateResolutionRequest struct {
	ReviewRequestID             string
	ReviewDecisionID            string
	RunID                       string
	StageAttemptID              string
	ExpectedRunVersion          int64
	ExpectedStageAttemptVersion int64
	ExpectedNodeAttemptVersion  int64
	ArtifactManifestID          string
	Actor                       string
	Reason                      string
}

// ReviewGateResolutionResult is the terminal StageAttempt projection. The
// runtime uses its mapped Verdict to enqueue a coordinator or project the
// appropriate continuation outcome.
type ReviewGateResolutionResult struct {
	Binding      ReviewGateBinding
	Decision     ReviewDecision
	Run          WorkflowRun
	StageAttempt StageAttempt
	NodeAttempt  NodeAttempt
}

// ReviewGateResolutionJobKey is stable across lost responses and process
// restarts. It contains no user-controlled component beyond existing UUIDv7
// identities already bound by the store transaction.
func ReviewGateResolutionJobKey(stageAttemptID, decisionID string) string {
	return "review-gate-resolution:" + stageAttemptID + ":" + decisionID
}
