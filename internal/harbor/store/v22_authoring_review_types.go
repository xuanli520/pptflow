package store

import "time"

const (
	// AuthoringReviewGateResolutionCommandType is consumed by the local worker
	// after an operator has decided an AuthoringSession-bound review gate.
	// It is intentionally distinct from the task-revision review-gate command.
	AuthoringReviewGateResolutionCommandType = "authoring_stage_attempt.review_gate_resolution"
	authoringReviewGateResolutionEntityType  = "authoring_review_gate"
)

// AuthoringReviewGateState is derived from immutable request, decision, and
// resolution facts. No mutable review-request state is needed for an
// AuthoringSession subject.
type AuthoringReviewGateState string

const (
	AuthoringReviewGateOpen      AuthoringReviewGateState = "open"
	AuthoringReviewGateDecided   AuthoringReviewGateState = "decided"
	AuthoringReviewGateCompleted AuthoringReviewGateState = "completed"
)

// AuthoringReviewRequest is an immutable operator-review envelope for one
// source/session Run. It is deliberately not ReviewRequest: that type is
// reserved for a real TaskRevision lifecycle review.
type AuthoringReviewRequest struct {
	ID                     string
	RunID                  string
	AuthoringSessionID     string
	AuthoringSourceID      string
	SourceSnapshotDigest   string
	DefinitionHash         string
	EvidenceManifestDigest string
	RequestFingerprint     string
	IdempotencyKey         string
	CreatedBy              string
	CreatedAt              time.Time
}

// AuthoringReviewGateBinding freezes one StageAttempt's complete source/session
// lineage before a human operator can decide it. SourceFingerprint remains a
// source provenance fact outside this generic subject coordinate; the durable
// subject is SourceID, SessionID, and SourceSnapshotDigest.
type AuthoringReviewGateBinding struct {
	ID                     string
	ReviewRequestID        string
	RunID                  string
	AuthoringSessionID     string
	AuthoringSourceID      string
	SourceSnapshotDigest   string
	DefinitionHash         string
	StageAttemptID         string
	StageKey               string
	NodeAttemptID          string
	NodeGeneration         int
	NodeAttemptOrdinal     int
	ReviewKind             string
	InputBindingsJSON      string
	InputFingerprint       string
	EvidenceManifestDigest string
	BindingFingerprint     string
	CreatedAt              time.Time
}

// AuthoringReviewDecision is one immutable operator action for a bound
// AuthoringSession review gate. It never references a TaskRevision.
type AuthoringReviewDecision struct {
	ID                  string
	ReviewRequestID     string
	BindingID           string
	Action              ReviewDecisionAction
	DecisionFingerprint string
	IdempotencyKey      string
	Actor               string
	Reason              string
	CreatedAt           time.Time
}

// AuthoringReviewGateResolution is the immutable worker receipt that maps an
// operator action to a StageAttempt verdict and preserves result evidence.
type AuthoringReviewGateResolution struct {
	ID                       string
	ReviewRequestID          string
	BindingID                string
	DecisionID               string
	Verdict                  Verdict
	ArtifactManifestID       string
	ResolutionEvidenceDigest string
	ResolutionPayloadJSON    string
	ResolutionFingerprint    string
	IdempotencyKey           string
	CreatedBy                string
	CreatedAt                time.Time
}

// OpenAuthoringReviewGateRequest freezes a source/session review gate and
// atomically moves its Run and StageAttempt into durable waiting state.
// Optional IDs are allocated by Store when omitted; IdempotencyKey is required
// because it is the replay boundary used by TUI and worker callers.
type OpenAuthoringReviewGateRequest struct {
	ReviewRequestID string
	BindingID       string
	NodeAttemptID   string

	IdempotencyKey string

	RunID                       string
	AuthoringSessionID          string
	AuthoringSourceID           string
	SourceSnapshotDigest        string
	ExpectedRunVersion          int64
	DefinitionHash              string
	StageAttemptID              string
	ExpectedStageAttemptVersion int64
	StageKey                    string
	ReviewKind                  string
	NodeGeneration              int
	NodeAttemptOrdinal          int
	InputBindingsJSON           string
	InputFingerprint            string
	EvidenceManifestDigest      string
	Actor                       string
	Reason                      string
}

// AuthoringReviewGateOpenResult returns the coherent source/session gate state
// after an atomic open or a verified replay.
type AuthoringReviewGateOpenResult struct {
	Request      AuthoringReviewRequest
	Binding      AuthoringReviewGateBinding
	Run          WorkflowRun
	StageAttempt StageAttempt
	NodeAttempt  NodeAttempt
}

// DecideAuthoringReviewGateRequest records one operator action and queues the
// local resolution job. All subject and input coordinates must reproduce the
// frozen binding; callers cannot decide another Run or source session by ID.
type DecideAuthoringReviewGateRequest struct {
	ID                          string
	ResolutionJobID             string
	IdempotencyKey              string
	ReviewRequestID             string
	BindingID                   string
	RunID                       string
	AuthoringSessionID          string
	AuthoringSourceID           string
	SourceSnapshotDigest        string
	DefinitionHash              string
	StageAttemptID              string
	InputFingerprint            string
	EvidenceManifestDigest      string
	ExpectedRunVersion          int64
	ExpectedStageAttemptVersion int64
	Action                      ReviewDecisionAction
	ResolutionPayloadJSON       string
	ResolutionPriority          int
	Actor                       string
	Reason                      string
}

// AuthoringReviewGateDecisionResult contains the immutable decision and its
// one durable local resolution job.
type AuthoringReviewGateDecisionResult struct {
	Request       AuthoringReviewRequest
	Binding       AuthoringReviewGateBinding
	Decision      AuthoringReviewDecision
	ResolutionJob DurableJob
}

// CompleteAuthoringReviewGateResolutionRequest proves that the worker applied
// exactly one decision to its frozen source/session gate. The result evidence
// is opaque but immutable and does not pretend to be a TaskRevision artifact.
type CompleteAuthoringReviewGateResolutionRequest struct {
	ID                          string
	IdempotencyKey              string
	ReviewRequestID             string
	BindingID                   string
	DecisionID                  string
	RunID                       string
	AuthoringSessionID          string
	AuthoringSourceID           string
	SourceSnapshotDigest        string
	DefinitionHash              string
	StageAttemptID              string
	NodeAttemptID               string
	InputFingerprint            string
	EvidenceManifestDigest      string
	ExpectedRunVersion          int64
	ExpectedStageAttemptVersion int64
	ExpectedNodeAttemptVersion  int64
	ArtifactManifestID          string
	ResolutionEvidenceDigest    string
	ResolutionPayloadJSON       string
	Actor                       string
	Reason                      string
}

// AuthoringReviewGateResolutionResult is the terminal gate projection. The
// Run remains waiting_review so the generic runtime can choose the next
// durable transition after inspecting the completed review fact.
type AuthoringReviewGateResolutionResult struct {
	Request      AuthoringReviewRequest
	Binding      AuthoringReviewGateBinding
	Decision     AuthoringReviewDecision
	Resolution   AuthoringReviewGateResolution
	Run          WorkflowRun
	StageAttempt StageAttempt
	NodeAttempt  NodeAttempt
}

// AuthoringReviewGateResolutionJobKey is stable across lost responses and
// process restarts. It contains only previously validated UUIDv7 identities.
func AuthoringReviewGateResolutionJobKey(bindingID, decisionID string) string {
	return "authoring-review-gate-resolution:" + bindingID + ":" + decisionID
}
