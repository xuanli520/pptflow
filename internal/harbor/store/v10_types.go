package store

import "time"

// RevisionCandidateRetention is the fixed policy interval for terminal
// candidate checkout material. The durable candidate and all immutable
// evidence remain after this interval; only its managed temporary directory
// may be tombstoned.
const RevisionCandidateRetention = 7 * 24 * time.Hour

func revisionCandidateRetentionDeadline(now time.Time) *time.Time {
	deadline := now.UTC().Add(RevisionCandidateRetention)
	return &deadline
}

type CandidateGarbageCollectionState string

const (
	CandidateGarbageCollectionInProgress CandidateGarbageCollectionState = "in_progress"
	CandidateGarbageCollectionCompleted  CandidateGarbageCollectionState = "completed"
)

// CandidateGarbageCollectionOperation persists cleanup across the SQLite to
// filesystem boundary. A completed operation is a permanent tombstone for
// candidate material, not for the candidate identity or immutable evidence.
type CandidateGarbageCollectionOperation struct {
	ID                       string
	CandidateID              string
	IdempotencyKey           string
	ExpectedCandidateVersion int64
	Actor                    string
	Reason                   string
	State                    CandidateGarbageCollectionState
	LastError                string
	CreatedAt                time.Time
	UpdatedAt                time.Time
	CompletedAt              *time.Time
	Version                  int64
}

type PrepareCandidateGarbageCollectionRequest struct {
	ID                       string
	CandidateID              string
	ExpectedCandidateVersion int64
	IdempotencyKey           string
	Actor                    string
	Reason                   string
}

type PrepareCandidateGarbageCollectionResult struct {
	Candidate RevisionCandidate
	Operation CandidateGarbageCollectionOperation
}

// FinalizeCandidateGarbageCollectionRequest keeps the managed filesystem
// action inside the SQLite writer transaction. RemoveDirectory must tolerate
// a missing directory because a process can stop after deletion but before
// the transaction commits.
type FinalizeCandidateGarbageCollectionRequest struct {
	OperationID     string
	ExpectedVersion int64
	Actor           string
	Reason          string
	RemoveDirectory func() error
}

type FinalizeCandidateGarbageCollectionResult struct {
	Candidate RevisionCandidate
	Operation CandidateGarbageCollectionOperation
	Collected bool
}

type RecordCandidateGarbageCollectionFailureRequest struct {
	OperationID     string
	ExpectedVersion int64
	Actor           string
	Reason          string
	ErrorText       string
}

// ReleaseWithdrawOperation is the durable command projection for one local
// release withdrawal. It is separate from the immutable Release record so
// retries can return the original outcome and receipt without another write.
type ReleaseWithdrawOperationState string

const (
	ReleaseWithdrawCompleted ReleaseWithdrawOperationState = "completed"
)

type ReleaseWithdrawOperation struct {
	ID                     string
	ReleaseID              string
	IdempotencyKey         string
	ExpectedReleaseVersion int64
	Actor                  string
	Reason                 string
	State                  ReleaseWithdrawOperationState
	ReceiptID              string
	ResultReleaseVersion   int64
	CreatedAt              time.Time
	CompletedAt            time.Time
}

// ReleaseWithdrawReceipt is an immutable confirmation of the exact release
// record version that was withdrawn by a durable operation.
type ReleaseWithdrawReceipt struct {
	ID                    string
	OperationID           string
	ReleaseID             string
	ReleaseVersion        string
	ExpectedRecordVersion int64
	ResultRecordVersion   int64
	ReceiptJSON           string
	ReceiptDigest         string
	CreatedBy             string
	CreatedAt             time.Time
}

type ExecuteReleaseWithdrawRequest struct {
	ID                     string
	ReleaseID              string
	ExpectedReleaseVersion int64
	IdempotencyKey         string
	Actor                  string
	Reason                 string
}

type ReleaseWithdrawResult struct {
	Release   LocalPackageRelease
	Operation ReleaseWithdrawOperation
	Receipt   ReleaseWithdrawReceipt
}
