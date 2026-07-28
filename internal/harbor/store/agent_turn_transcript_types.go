package store

import "time"

// AgentTurnTranscriptRetention is the fixed period for controlled access to
// raw Agent responses and dynamic-tool payloads. Expiry removes that raw
// material while preserving the diagnostic and audit facts.
const AgentTurnTranscriptRetention = 30 * 24 * time.Hour

const agentTurnTranscriptCompletedSubstep = "turn_completed"

type AgentTurnSubmissionStatus string

const (
	AgentTurnSubmissionNotSubmitted AgentTurnSubmissionStatus = "not_submitted"
	AgentTurnSubmissionAccepted     AgentTurnSubmissionStatus = "accepted"
	AgentTurnSubmissionRejected     AgentTurnSubmissionStatus = "rejected"
	AgentTurnSubmissionRuntimeError AgentTurnSubmissionStatus = "runtime_error"
)

// AgentTurnTranscript is the durable record of one real model turn. Its
// node-attempt and turn coordinate is immutable. ResponseText and the raw
// fields of child submissions are deliberately cleared after retention, but
// digest, byte count, state, and failure facts remain available for audit and
// operator-facing summaries.
type AgentTurnTranscript struct {
	ID                    string                    `json:"id"`
	NodeAttemptID         string                    `json:"node_attempt_id"`
	Turn                  int                       `json:"turn"`
	ResponseText          string                    `json:"response_text,omitempty"`
	ResponseSHA256        string                    `json:"response_sha256"`
	ResponseBytes         int64                     `json:"response_bytes"`
	ModelID               string                    `json:"model_id"`
	SubmissionStatus      AgentTurnSubmissionStatus `json:"submission_status"`
	ProtocolRejectionCode string                    `json:"protocol_rejection_code,omitempty"`
	FailureCode           string                    `json:"failure_code,omitempty"`
	CreatedAt             time.Time                 `json:"created_at"`
	ExpiresAt             time.Time                 `json:"expires_at"`
	ExpiredAt             *time.Time                `json:"expired_at,omitempty"`
	Version               int64                     `json:"version"`
}

// AgentTurnTranscriptSubmission preserves every dynamic-tool attempt made in
// a model turn. Raw payloads can be intentionally malformed because their
// purpose is diagnosis, not a second submission authority.
type AgentTurnTranscriptSubmission struct {
	ID             string                    `json:"id"`
	TranscriptID   string                    `json:"transcript_id"`
	Ordinal        int                       `json:"ordinal"`
	Status         AgentTurnSubmissionStatus `json:"status"`
	RawRequestJSON string                    `json:"raw_request_json,omitempty"`
	ValidationJSON string                    `json:"validation_json,omitempty"`
	ReceiptJSON    string                    `json:"receipt_json,omitempty"`
	RejectionCode  string                    `json:"rejection_code,omitempty"`
	CreatedAt      time.Time                 `json:"created_at"`
	ExpiredAt      *time.Time                `json:"expired_at,omitempty"`
	Version        int64                     `json:"version"`
}

type CreateAgentTurnTranscriptSubmissionRequest struct {
	ID             string
	Ordinal        int
	Status         AgentTurnSubmissionStatus
	RawRequestJSON string
	ValidationJSON string
	ReceiptJSON    string
	RejectionCode  string
}

type CreateAgentTurnTranscriptRequest struct {
	ID                    string
	NodeAttemptID         string
	Turn                  int
	ResponseText          string
	ModelID               string
	SubmissionStatus      AgentTurnSubmissionStatus
	Submissions           []CreateAgentTurnTranscriptSubmissionRequest
	ProtocolRejectionCode string
	FailureCode           string
	OccurredAt            time.Time
	Actor                 string
	Reason                string
}

// AgentTurnTranscriptCheckpoint is the full checkpoint creation intent for a
// completed Agent turn. It is intentionally not an existing checkpoint CAS:
// RecordAgentTurnTranscriptWithCheckpoint creates and completes it in the
// same Store transaction as the transcript.
type AgentTurnTranscriptCheckpoint struct {
	ID            string
	NodeAttemptID string
	Turn          int
	Substep       string
	InputDigest   string
	ArtifactID    string
	PayloadJSON   string
}

type RecordAgentTurnTranscriptWithCheckpointRequest struct {
	Transcript CreateAgentTurnTranscriptRequest
	Checkpoint AgentTurnTranscriptCheckpoint
	Actor      string
	Reason     string
}

type RecordAgentTurnTranscriptWithCheckpointResult struct {
	Transcript AgentTurnTranscript
	Checkpoint TurnCheckpoint
	Replayed   bool
}

type AgentTurnTranscriptExpiryBlock string

const (
	AgentTurnTranscriptExpiryBlockedActiveAttempt AgentTurnTranscriptExpiryBlock = "active_attempt"
	AgentTurnTranscriptExpiryBlockedActiveWorker  AgentTurnTranscriptExpiryBlock = "active_worker"
	AgentTurnTranscriptExpiryBlockedLegalHold     AgentTurnTranscriptExpiryBlock = "legal_hold"
)

type ExpireAgentTurnTranscriptRequest struct {
	TranscriptID    string
	ExpectedVersion int64
	Actor           string
	Reason          string
}

type ExpireAgentTurnTranscriptResult struct {
	Transcript AgentTurnTranscript
	Expired    bool
	Replayed   bool
	Block      AgentTurnTranscriptExpiryBlock
}

type SweepExpiredAgentTurnTranscriptsRequest struct {
	Limit  int
	Actor  string
	Reason string
}

type SweepExpiredAgentTurnTranscriptsResult struct {
	Expired []ExpireAgentTurnTranscriptResult
	Blocked []ExpireAgentTurnTranscriptResult
}

// AgentTurnTranscriptLegalHold is a legal-retention binding. Releasing a
// hold preserves its original fact and audit trail instead of deleting it.
type AgentTurnTranscriptLegalHold struct {
	ID            string     `json:"id"`
	TranscriptID  string     `json:"transcript_id"`
	HoldKey       string     `json:"hold_key"`
	Reason        string     `json:"reason"`
	CreatedBy     string     `json:"created_by"`
	CreatedAt     time.Time  `json:"created_at"`
	ReleasedBy    string     `json:"released_by,omitempty"`
	ReleaseReason string     `json:"release_reason,omitempty"`
	ReleasedAt    *time.Time `json:"released_at,omitempty"`
	Version       int64      `json:"version"`
}

type CreateAgentTurnTranscriptLegalHoldRequest struct {
	ID           string
	TranscriptID string
	HoldKey      string
	Actor        string
	Reason       string
}

type ReleaseAgentTurnTranscriptLegalHoldRequest struct {
	HoldID          string
	ExpectedVersion int64
	Actor           string
	Reason          string
}
