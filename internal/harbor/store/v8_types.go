package store

import "time"

// RevisionCandidateState separates a mutable isolated checkout from every
// immutable TaskRevision. Only a fenced candidate may receive a provider
// write; committing creates a new sealed TaskRevision rather than modifying
// the candidate or its base revision.
type RevisionCandidateState string

const (
	RevisionCandidateReady             RevisionCandidateState = "ready"
	RevisionCandidateApplying          RevisionCandidateState = "applying"
	RevisionCandidatePrepared          RevisionCandidateState = "prepared"
	RevisionCandidateNoOp              RevisionCandidateState = "no_op"
	RevisionCandidateReconcileRequired RevisionCandidateState = "reconcile_required"
	RevisionCandidateDiscarded         RevisionCandidateState = "discarded"
	RevisionCandidateCommitting        RevisionCandidateState = "committing"
	RevisionCandidateCommitted         RevisionCandidateState = "committed"
)

type ChangeOperationState string

const (
	ChangeOperationPrepared  ChangeOperationState = "prepared"
	ChangeOperationRunning   ChangeOperationState = "running"
	ChangeOperationSucceeded ChangeOperationState = "succeeded"
	ChangeOperationFailed    ChangeOperationState = "failed"
	ChangeOperationUnknown   ChangeOperationState = "unknown"
)

// RevisionCandidate is the durable record for a checkout that can later
// become a revision. TargetRevisionID and TargetRunID are globally reserved
// UUIDv7 identities, not live rows, until CommitRevisionCandidateContinuation
// consumes them atomically.
type RevisionCandidate struct {
	ID                   string
	TaskID               string
	SourceRunID          string
	CommandID            string
	RepairSessionID      string
	RoundOrdinal         int
	BaseRevisionID       string
	BaseDigest           string
	TargetRevisionID     string
	TargetRunID          string
	ExpectedTaskVersion  int64
	ProviderID           string
	CheckoutRelpath      string
	FindingsJSON         string
	State                RevisionCandidateState
	AfterDigest          string
	ObservedChangesJSON  string
	PreparedChangeID     string
	MutationReceiptID    string
	FrozenPlanID         string
	FinalManifestID      string
	ChildRunManifestJSON string
	LeaseID              string
	LeaseOwner           string
	LeaseFencingToken    uint64
	LeaseVersion         int64
	CreatedBy            string
	Reason               string
	CreatedAt            time.Time
	UpdatedAt            time.Time
	RetainUntil          *time.Time
	CheckoutTombstonedAt *time.Time
	CheckoutTombstonedBy string
	Version              int64
}

type CreateRevisionCandidateRequest struct {
	ID                  string
	TaskID              string
	SourceRunID         string
	CommandID           string
	RepairSessionID     string
	RoundOrdinal        int
	BaseRevisionID      string
	BaseDigest          string
	TargetRevisionID    string
	TargetRunID         string
	ExpectedTaskVersion int64
	ProviderID          string
	CheckoutRelpath     string
	FindingsJSON        string
	LeaseID             string
	LeaseOwner          string
	LeaseFencingToken   uint64
	LeaseVersion        int64
	Actor               string
	Reason              string
}

type ChangeOperation struct {
	ID            string
	CandidateID   string
	ProviderID    string
	OperationKey  string
	PayloadJSON   string
	PayloadDigest string
	State         ChangeOperationState
	ReceiptID     string
	CreatedBy     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	Version       int64
}

type CreateChangeOperationRequest struct {
	ID           string
	CandidateID  string
	ProviderID   string
	OperationKey string
	PayloadJSON  string
	Actor        string
	Reason       string
}

type StartChangeOperationRequest struct {
	OperationID       string
	ExpectedVersion   int64
	LeaseID           string
	LeaseOwner        string
	LeaseFencingToken uint64
	LeaseVersion      int64
	Actor             string
	Reason            string
}

// FinalizeChangeOperationRequest records the observed provider result before
// any frozen plan may reference it. The prepared change and mutation receipt
// are inserted with the operation/candidate projection in one transaction.
type FinalizeChangeOperationRequest struct {
	OperationID               string
	ExpectedVersion           int64
	LeaseID                   string
	LeaseOwner                string
	LeaseFencingToken         uint64
	LeaseVersion              int64
	Outcome                   MutationReceiptOutcome
	AfterDigest               string
	ObservedChangesJSON       string
	PreparedChangeID          string
	PreparedChangePayloadJSON string
	MutationReceiptID         string
	MutationReceiptJSON       string
	MutationReceiptKey        string
	Actor                     string
	Reason                    string
}

// CreateAndBindRevisionCandidatePlanRequest commits the immutable plan and
// its candidate binding in one SQLite transaction. It deliberately carries
// the final filesystem facts produced before the transaction, so a crash can
// leave unreferenced files but never an executable unbound plan.
type CreateAndBindRevisionCandidatePlanRequest struct {
	Plan                     CreateFrozenPlanRequest
	CandidateID              string
	ExpectedCandidateVersion int64
	FinalManifestID          string
	ChildRunManifestJSON     string
	Actor                    string
	Reason                   string
}

type DiscardRevisionCandidateRequest struct {
	CandidateID     string
	ExpectedVersion int64
	Actor           string
	Reason          string
}

type ExpireRevisionCandidateRequest struct {
	CandidateID     string
	ExpectedVersion int64
	Actor           string
	Reason          string
}

// CommitRevisionCandidateContinuationRequest is the one transactional
// boundary for the content-change execution handoff. It consumes a frozen
// revised plan and its candidate into a sealed child revision, child run,
// continuation execution, durable job, and outbox event.
type CommitRevisionCandidateContinuationRequest struct {
	ID             string
	PlanID         string
	IdempotencyKey string
	PayloadJSON    string
	Expected       ControlCheckpointRef
	// ChildRunInputs are immutable, pre-materialized object bindings for the
	// target Run. The commit derives run/task/revision identity itself and
	// inserts these records in the same transaction as the child Run.
	ChildRunInputs []CreateRunInputArtifactRequest
	Actor          string
	Reason         string
	Priority       int
}

type RevisionCandidateContinuationCommit struct {
	Candidate RevisionCandidate
	Revision  TaskRevision
	Run       WorkflowRun
	Execution ContinuationExecution
	Job       DurableJob
}
