package store

import "time"

// ArtifactManifest is an immutable, versioned evidence manifest. Its payload
// is opaque structured JSON; callers own artifact schemas and vocabulary.
type ArtifactManifest struct {
	ID                  string
	SubjectRevisionID   string
	SubjectDigest       string
	WorkflowFingerprint string
	ManifestJSON        string
	ManifestFingerprint string
	IdempotencyKey      string
	CreatedBy           string
	CreatedAt           time.Time
}

type CreateArtifactManifestRequest struct {
	ID                  string
	SubjectRevisionID   string
	SubjectDigest       string
	WorkflowFingerprint string
	ManifestJSON        string
	ManifestFingerprint string
	IdempotencyKey      string
	Actor               string
	Reason              string
}

// ArtifactRef is immutable typed artifact lineage bound to an ArtifactManifest.
type ArtifactRef struct {
	ID                  string
	ManifestID          string
	ArtifactKey         string
	ContentDigest       string
	SchemaVersion       string
	RunID               string
	StageKey            string
	AttemptID           string
	TurnOrdinal         int
	SubjectRevisionID   string
	SubjectDigest       string
	WorkflowFingerprint string
	InputBindingsJSON   string
	InputFingerprint    string
	ProducerVersion     string
	IdempotencyKey      string
	CreatedAt           time.Time
}

type CreateArtifactRefRequest struct {
	ID                  string
	ManifestID          string
	ArtifactKey         string
	ContentDigest       string
	SchemaVersion       string
	RunID               string
	StageKey            string
	AttemptID           string
	TurnOrdinal         int
	SubjectRevisionID   string
	SubjectDigest       string
	WorkflowFingerprint string
	InputBindingsJSON   string
	InputFingerprint    string
	ProducerVersion     string
	IdempotencyKey      string
	Actor               string
	Reason              string
}

// ContinuationCommand is an immutable record of user intent. CommandKey is a
// caller-issued idempotency key; replaying it with another payload is rejected.
type ContinuationCommand struct {
	ID            string
	CommandKey    string
	SubjectID     string
	RunID         string
	PayloadJSON   string
	PayloadDigest string
	Actor         string
	Reason        string
	CreatedAt     time.Time
}

type CreateContinuationCommandRequest struct {
	ID          string
	CommandKey  string
	SubjectID   string
	RunID       string
	PayloadJSON string
	Actor       string
	Reason      string
}

type RepairSessionState string

const (
	RepairSessionOpen       RepairSessionState = "open"
	RepairSessionNeedsHuman RepairSessionState = "needs_human"
	RepairSessionCompleted  RepairSessionState = "completed"
	RepairSessionCanceled   RepairSessionState = "canceled"
)

// RepairSession is a CAS-protected durable envelope for bounded repair work.
// Actual changes and receipts are immutable children rather than mutable logs.
type RepairSession struct {
	ID             string
	CommandID      string
	SubjectID      string
	BaseRevisionID string
	MaxRounds      int
	Status         RepairSessionState
	FindingsJSON   string
	PolicyJSON     string
	IdempotencyKey string
	CreatedBy      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	Version        int64
}

type CreateRepairSessionRequest struct {
	ID             string
	CommandID      string
	SubjectID      string
	BaseRevisionID string
	MaxRounds      int
	FindingsJSON   string
	PolicyJSON     string
	IdempotencyKey string
	Actor          string
	Reason         string
}

type TransitionRepairSessionRequest struct {
	RepairSessionID string
	ExpectedVersion int64
	Status          RepairSessionState
	Actor           string
	Reason          string
}

// PreparedChange is immutable normalized change intent and observed result.
// OperationKey is persisted before invoking a mutator and is globally unique.
type PreparedChange struct {
	ID                  string
	CommandID           string
	RepairSessionID     string
	RoundOrdinal        int
	ProviderID          string
	OperationKey        string
	PayloadJSON         string
	ObservedChangesJSON string
	BeforeDigest        string
	AfterDigest         string
	CreatedBy           string
	CreatedAt           time.Time
}

type CreatePreparedChangeRequest struct {
	ID                  string
	CommandID           string
	RepairSessionID     string
	RoundOrdinal        int
	ProviderID          string
	OperationKey        string
	PayloadJSON         string
	ObservedChangesJSON string
	BeforeDigest        string
	AfterDigest         string
	Actor               string
	Reason              string
}

type MutationReceiptOutcome string

const (
	MutationReceiptApplied   MutationReceiptOutcome = "applied"
	MutationReceiptNoOp      MutationReceiptOutcome = "no_op"
	MutationReceiptUncertain MutationReceiptOutcome = "uncertain"
	MutationReceiptFailed    MutationReceiptOutcome = "failed"
)

// MutationReceipt records an immutable provider outcome. A later reconcile
// creates a new receipt that may explicitly supersede this one.
type MutationReceipt struct {
	ID                  string
	PreparedChangeID    string
	OperationKey        string
	Outcome             MutationReceiptOutcome
	ReceiptJSON         string
	ReceiptDigest       string
	SupersedesReceiptID string
	IdempotencyKey      string
	CreatedBy           string
	CreatedAt           time.Time
}

type CreateMutationReceiptRequest struct {
	ID                  string
	PreparedChangeID    string
	OperationKey        string
	Outcome             MutationReceiptOutcome
	ReceiptJSON         string
	SupersedesReceiptID string
	IdempotencyKey      string
	Actor               string
	Reason              string
}

// FrozenPlan is a write-once executable continuation plan. PlanFingerprint is
// supplied by the planner; PayloadDigest independently detects any payload
// conflict during idempotent storage or readback validation.
type FrozenPlan struct {
	ID                  string
	CommandID           string
	PreparedChangeID    string
	SubjectID           string
	SubjectRevisionID   string
	SubjectDigest       string
	WorkflowFingerprint string
	PlanFingerprint     string
	PayloadJSON         string
	PayloadDigest       string
	ExpiresAt           time.Time
	CreatedBy           string
	CreatedAt           time.Time
}

type CreateFrozenPlanRequest struct {
	ID                  string
	CommandID           string
	PreparedChangeID    string
	SubjectID           string
	SubjectRevisionID   string
	SubjectDigest       string
	WorkflowFingerprint string
	PlanFingerprint     string
	PayloadJSON         string
	ExpiresAt           time.Time
	Actor               string
	Reason              string
}

type ContinuationExecutionState string

const (
	ContinuationExecutionQueued            ContinuationExecutionState = "queued"
	ContinuationExecutionRunning           ContinuationExecutionState = "running"
	ContinuationExecutionCompleted         ContinuationExecutionState = "completed"
	ContinuationExecutionFailed            ContinuationExecutionState = "failed"
	ContinuationExecutionCanceled          ContinuationExecutionState = "canceled"
	ContinuationExecutionReconcileRequired ContinuationExecutionState = "reconcile_required"
)

// ContinuationExecution carries mutable execution projection only; its plan,
// parent, payload, and idempotency identity remain immutable by schema trigger.
type ContinuationExecution struct {
	ID                string
	PlanID            string
	ParentExecutionID string
	RunID             string
	IdempotencyKey    string
	State             ContinuationExecutionState
	PayloadJSON       string
	CreatedBy         string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	FinishedAt        *time.Time
	Version           int64
}

type CreateContinuationExecutionRequest struct {
	ID                string
	PlanID            string
	ParentExecutionID string
	RunID             string
	IdempotencyKey    string
	PayloadJSON       string
	Actor             string
	Reason            string
}

type TransitionContinuationExecutionRequest struct {
	ContinuationExecutionID string
	ExpectedVersion         int64
	State                   ContinuationExecutionState
	Actor                   string
	Reason                  string
}
