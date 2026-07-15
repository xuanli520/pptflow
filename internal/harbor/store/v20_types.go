package store

import "time"

// CodeEdgeComplianceStatus is the immutable package-eligibility outcome for
// one CodeEdge Phase-1 Run. It deliberately records the aggregate decision,
// not a mutable review workflow: a changed task, catalog, or evidence binding
// must create a new child Run and therefore a new record.
type CodeEdgeComplianceStatus string

const (
	CodeEdgeComplianceApproved CodeEdgeComplianceStatus = "approved"
	CodeEdgeComplianceRejected CodeEdgeComplianceStatus = "rejected"
)

// CodeEdgeComplianceRecord persists the exact evaluator, submission, final
// decision, and (when approved) package authorization documents that a local
// CodeEdge package is allowed to consume. JSON remains domain-owned and is
// validated by the CodeEdge application service before it reaches this
// durable immutable record.
type CodeEdgeComplianceRecord struct {
	ID                                  string
	RunID                               string
	TaskID                              string
	RevisionID                          string
	TaskDigest                          string
	Status                              CodeEdgeComplianceStatus
	EvaluatorEvidenceHandoffID          string
	EvaluatorEvidenceHandoffFingerprint string
	QwenReceiptJSON                     string
	OpusReceiptJSON                     string
	SubmissionReceiptJSON               string
	DecisionJSON                        string
	DecisionFingerprint                 string
	AuthorizationJSON                   string
	AuthorizationFingerprint            string
	IdempotencyKey                      string
	CreatedBy                           string
	CreatedAt                           time.Time
}

// CreateCodeEdgeComplianceRecordRequest creates one write-once final
// compliance result. IdempotencyKey is required and is also the UUIDv7 record
// identity, so a lost response cannot issue a second authorization.
type CreateCodeEdgeComplianceRecordRequest struct {
	ID                                  string
	RunID                               string
	TaskID                              string
	RevisionID                          string
	TaskDigest                          string
	Status                              CodeEdgeComplianceStatus
	EvaluatorEvidenceHandoffID          string
	EvaluatorEvidenceHandoffFingerprint string
	QwenReceiptJSON                     string
	OpusReceiptJSON                     string
	SubmissionReceiptJSON               string
	DecisionJSON                        string
	DecisionFingerprint                 string
	AuthorizationJSON                   string
	AuthorizationFingerprint            string
	IdempotencyKey                      string
	Actor                               string
	Reason                              string
}
