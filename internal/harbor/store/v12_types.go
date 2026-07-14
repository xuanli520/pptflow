package store

import "time"

// LifecycleOperationState describes a receipt that is either awaiting its
// application-layer result or is safe to replay verbatim.
type LifecycleOperationState string

const (
	LifecycleOperationPrepared  LifecycleOperationState = "prepared"
	LifecycleOperationCompleted LifecycleOperationState = "completed"
)

// LifecycleOperation is deliberately action-agnostic. The application layer
// stores typed result JSON after a domain mutation succeeds, but the exact
// immutable target identities and CAS facts remain queryable for recovery.
type LifecycleOperation struct {
	ID                           string
	IdempotencyKey               string
	Action                       string
	RequestFingerprint           string
	TaskID                       string
	RevisionID                   string
	RunID                        string
	ReviewRequestID              string
	ReleaseID                    string
	DeletionRecordID             string
	TargetLifecycleState         TaskLifecycleState
	ExpectedTaskID               string
	ExpectedRevisionID           string
	ExpectedRunID                string
	ExpectedReleaseID            string
	ExpectedReviewRequestID      string
	ExpectedTaskVersion          int64
	ExpectedRevisionStateVersion int64
	ExpectedRevisionDigest       string
	ExpectedRunVersion           int64
	ExpectedRunExecutionEpoch    int
	ExpectedRunDefinitionHash    string
	ExpectedReleaseRecordVersion int64
	ExpectedReviewRevisionID     string
	ExpectedReviewState          string
	ExpectedReviewEvidenceDigest string
	Actor                        string
	Reason                       string
	State                        LifecycleOperationState
	ResultJSON                   string
	CreatedAt                    time.Time
	UpdatedAt                    time.Time
	CompletedAt                  *time.Time
	Version                      int64
}

// BeginLifecycleOperationRequest persists an action's stable target IDs before
// it touches the domain. Target IDs can refer to a new entity that does not
// exist yet, so this record intentionally uses no foreign keys for them.
type BeginLifecycleOperationRequest struct {
	ID                           string
	IdempotencyKey               string
	Action                       string
	RequestFingerprint           string
	TaskID                       string
	RevisionID                   string
	RunID                        string
	ReviewRequestID              string
	ReleaseID                    string
	DeletionRecordID             string
	TargetLifecycleState         TaskLifecycleState
	ExpectedTaskID               string
	ExpectedRevisionID           string
	ExpectedRunID                string
	ExpectedReleaseID            string
	ExpectedReviewRequestID      string
	ExpectedTaskVersion          int64
	ExpectedRevisionStateVersion int64
	ExpectedRevisionDigest       string
	ExpectedRunVersion           int64
	ExpectedRunExecutionEpoch    int
	ExpectedRunDefinitionHash    string
	ExpectedReleaseRecordVersion int64
	ExpectedReviewRevisionID     string
	ExpectedReviewState          string
	ExpectedReviewEvidenceDigest string
	Actor                        string
	Reason                       string
}

type BeginLifecycleOperationResult struct {
	Operation LifecycleOperation
	Replayed  bool
}

type CompleteLifecycleOperationRequest struct {
	OperationID     string
	ExpectedVersion int64
	ResultJSON      string
}

// ExecuteLifecycleTaskTransitionRequest consumes a prepared V12 operation
// into one task-state transition. The operation itself contains all target and
// checkpoint facts; callers only provide the immutable operation identity and
// the version they observed after BeginLifecycleOperation.
type ExecuteLifecycleTaskTransitionRequest struct {
	OperationID     string
	ExpectedVersion int64
}

type ExecuteLifecycleTaskTransitionResult struct {
	Operation      LifecycleOperation
	Task           TaskV2
	DeletionRecord *DeletionRecord
}
