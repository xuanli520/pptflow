package store

import (
	"errors"
	"time"
)

var (
	ErrQuotaNotConfigured = errors.New("store: quota account is not configured")
	ErrQuotaExhausted     = errors.New("store: quota exhausted")
	ErrQuotaLeaseExpired  = errors.New("store: quota lease expired")
	ErrStaleQuotaGrant    = errors.New("store: stale quota grant")
	ErrInvalidQuota       = errors.New("store: invalid quota request")
	// ErrQuotaPolicyAccountMismatch prevents a newer frozen policy from
	// silently changing an existing task or actor account. The owner must make
	// the intended capacity change through the CAS BudgetGrant path first.
	ErrQuotaPolicyAccountMismatch = errors.New("store: frozen quota policy conflicts with existing account")
	ErrInvalidControl             = errors.New("store: invalid execution control")
	ErrCapacityNotConfigured      = errors.New("store: capacity pool is not configured")
	ErrCapacityExhausted          = errors.New("store: dispatch capacity is exhausted")
	ErrInvalidDispatch            = errors.New("store: invalid durable job dispatch")
)

type QuotaScopeKind string

const (
	QuotaScopeTask  QuotaScopeKind = "task"
	QuotaScopeActor QuotaScopeKind = "actor"
)

type QuotaReclaimPolicy string

const (
	QuotaReclaimUnused QuotaReclaimPolicy = "reclaim_unused"
	QuotaReclaimNever  QuotaReclaimPolicy = "reclaim_never"
)

type DurableQuotaLeaseState string

const (
	DurableQuotaLeaseActive    DurableQuotaLeaseState = "active"
	DurableQuotaLeaseSettled   DurableQuotaLeaseState = "settled"
	DurableQuotaLeaseUncertain DurableQuotaLeaseState = "uncertain"
	DurableQuotaLeaseExpired   DurableQuotaLeaseState = "expired"
)

type QuotaSettlementOutcome string

const (
	QuotaSettlementCompleted QuotaSettlementOutcome = "completed"
	QuotaSettlementCanceled  QuotaSettlementOutcome = "canceled"
	QuotaSettlementUncertain QuotaSettlementOutcome = "uncertain"
)

type QuotaSettlementKind string

const (
	QuotaSettlementDirect    QuotaSettlementKind = "settlement"
	QuotaSettlementReconcile QuotaSettlementKind = "reconcile"
)

type AdmissionReason string

const (
	AdmissionReasonAccepted       AdmissionReason = "accepted"
	AdmissionReasonQuotaExhausted AdmissionReason = "quota_exhausted"
)

type QuotaAccount struct {
	ID            string
	ScopeKind     QuotaScopeKind
	ScopeID       string
	Dimension     string
	LimitUnits    int64
	ConsumedUnits int64
	ReservedUnits int64
	Version       int64
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

func (account QuotaAccount) AvailableUnits() int64 {
	available := account.LimitUnits - account.ConsumedUnits - account.ReservedUnits
	if available < 0 {
		return 0
	}
	return available
}

type CreateQuotaAccountRequest struct {
	ID         string
	ScopeKind  QuotaScopeKind
	ScopeID    string
	Dimension  string
	LimitUnits int64
	Actor      string
	Reason     string
}

type QuotaLeaseRequest struct {
	ID             string
	IdempotencyKey string
	Owner          string
	ScopeKind      QuotaScopeKind
	ScopeID        string
	Dimension      string
	Units          int64
	ReclaimPolicy  QuotaReclaimPolicy
	TTL            time.Duration
	AdmissionID    string
	Actor          string
	Reason         string
}

type DurableQuotaLease struct {
	ID            string
	AccountID     string
	AdmissionID   string
	Owner         string
	ScopeKind     QuotaScopeKind
	ScopeID       string
	Dimension     string
	ReservedUnits int64
	ConsumedUnits int64
	ReleasedUnits int64
	ReclaimPolicy QuotaReclaimPolicy
	FencingToken  uint64
	TTL           time.Duration
	ExpiresAt     time.Time
	State         DurableQuotaLeaseState
	CreatedAt     time.Time
	UpdatedAt     time.Time
	SettledAt     *time.Time
	Version       int64
}

func (lease DurableQuotaLease) RemainingUnits() int64 {
	remaining := lease.ReservedUnits - lease.ConsumedUnits - lease.ReleasedUnits
	if remaining < 0 {
		return 0
	}
	return remaining
}

type RecordQuotaUsageRequest struct {
	ID           string
	OperationKey string
	LeaseID      string
	FencingToken uint64
	Units        int64
	OccurredAt   time.Time
	Actor        string
	Reason       string
}

type QuotaUsageEvent struct {
	ID           string
	OperationKey string
	LeaseID      string
	FencingToken uint64
	Units        int64
	OccurredAt   time.Time
	Actor        string
	Reason       string
	RecordedAt   time.Time
}

type HeartbeatQuotaLeaseRequest struct {
	ID             string
	IdempotencyKey string
	LeaseID        string
	Owner          string
	FencingToken   uint64
	TTL            time.Duration
	Actor          string
	Reason         string
}

type SettleQuotaLeaseRequest struct {
	ID             string
	IdempotencyKey string
	LeaseID        string
	Owner          string
	FencingToken   uint64
	Outcome        QuotaSettlementOutcome
	Actor          string
	Reason         string
}

type ReconcileQuotaLeaseRequest struct {
	ID             string
	IdempotencyKey string
	LeaseID        string
	Owner          string
	FencingToken   uint64
	Outcome        QuotaSettlementOutcome
	Actor          string
	Reason         string
}

type DurableQuotaSettlement struct {
	ID            string
	LeaseID       string
	Kind          QuotaSettlementKind
	Outcome       QuotaSettlementOutcome
	ConsumedUnits int64
	ReleasedUnits int64
	Actor         string
	SettledAt     time.Time
	Lease         DurableQuotaLease
}

type GrantBudgetRequest struct {
	ID              string
	IdempotencyKey  string
	ScopeKind       QuotaScopeKind
	ScopeID         string
	Dimension       string
	DeltaUnits      int64
	ExpectedVersion int64
	Actor           string
	Reason          string
}

type DurableBudgetGrant struct {
	ID              string
	AccountID       string
	ScopeKind       QuotaScopeKind
	ScopeID         string
	Dimension       string
	DeltaUnits      int64
	PreviousVersion int64
	Version         int64
	LimitUnits      int64
	Actor           string
	Reason          string
	GrantedAt       time.Time
}

type TaskActorQuotaClaim struct {
	Dimension     string
	Units         int64
	ReclaimPolicy QuotaReclaimPolicy
}

// QuotaPolicyBinding identifies the exact code-versioned policy whose
// bootstrap limits and claims were frozen into a durable execution request.
// Store deliberately treats the fingerprint as opaque: domain adapters own
// its algorithm, while SQLite preserves it for account provenance and replay.
type QuotaPolicyBinding struct {
	PolicyID          string
	PolicyVersion     string
	PolicyFingerprint string
}

// QuotaAccountBootstrap is one fully explicit task/actor account limit from a
// frozen policy. Admission creates every listed account on first use; an
// omitted or zero value is never treated as a numeric default.
type QuotaAccountBootstrap struct {
	Dimension       string
	TaskLimitUnits  int64
	ActorLimitUnits int64
}

// QuotaAccountPolicyBinding records the immutable policy that initially
// created or adopted an account. Later policies may use the account only when
// its current limit already equals their requested bootstrap limit, which can
// happen after an explicit CAS grant.
type QuotaAccountPolicyBinding struct {
	AccountID         string
	PolicyID          string
	PolicyVersion     string
	PolicyFingerprint string
	InitialLimitUnits int64
	BoundAt           time.Time
}

type AdmitTaskActorQuotaRequest struct {
	ID                string
	IdempotencyKey    string
	TaskID            string
	Actor             string
	LeaseOwner        string
	LeaseTTL          time.Duration
	Policy            QuotaPolicyBinding
	BootstrapAccounts []QuotaAccountBootstrap
	Claims            []TaskActorQuotaClaim
	Reason            string
}

type DurableAdmissionDecision struct {
	ID             string
	IdempotencyKey string
	TaskID         string
	Actor          string
	Accepted       bool
	Reason         AdmissionReason
	Leases         []DurableQuotaLease
	DecidedAt      time.Time
}

type ControlAction string

const (
	ControlActionPause       ControlAction = "pause"
	ControlActionCancelStage ControlAction = "cancel_stage"
	ControlActionTerminate   ControlAction = "terminate"
)

type ControlOperationStatus string

const (
	ControlOperationRequested         ControlOperationStatus = "requested"
	ControlOperationPropagating       ControlOperationStatus = "propagating"
	ControlOperationAcknowledged      ControlOperationStatus = "acknowledged"
	ControlOperationReconcileRequired ControlOperationStatus = "reconcile_required"
	ControlOperationFailed            ControlOperationStatus = "failed"
)

type ControlCheckpointRef struct {
	Sequence            uint64
	ExecutionEpoch      int
	SubjectVersion      int64
	SubjectID           string
	SubjectRevisionID   string
	SubjectDigest       string
	WorkflowFingerprint string
}

type ExecutionControlCommand struct {
	ID             string
	OperationKey   string
	Action         ControlAction
	RunID          string
	StageAttemptID string
	Expected       ControlCheckpointRef
	Actor          string
	Reason         string
	GracePeriod    time.Duration
}

type DurableControlOperation struct {
	ID                string
	OperationKey      string
	Action            ControlAction
	RunID             string
	StageAttemptID    string
	Expected          ControlCheckpointRef
	Actor             string
	Reason            string
	GracePeriod       time.Duration
	Status            ControlOperationStatus
	CheckpointID      string
	QuotaSettlementID string
	FailureReason     string
	RuntimeReceipts   []RuntimeTerminationReceipt
	CreatedAt         time.Time
	UpdatedAt         time.Time
	Version           int64
}

type RuntimeTerminationReceipt struct {
	ID                     string
	RuntimeScopeID         string
	ObservedAt             time.Time
	Graceful               bool
	ExternalOutcomeUnknown bool
	PayloadJSON            string
}

type TransitionControlOperationRequest struct {
	ID                string
	OperationID       string
	ExpectedVersion   int64
	Status            ControlOperationStatus
	RuntimeReceipts   []RuntimeTerminationReceipt
	CheckpointID      string
	QuotaSettlementID string
	FailureReason     string
	Actor             string
	Reason            string
}

type SideEffectOperationState string

const (
	SideEffectPrepared  SideEffectOperationState = "prepared"
	SideEffectStarted   SideEffectOperationState = "started"
	SideEffectSucceeded SideEffectOperationState = "succeeded"
	SideEffectFailed    SideEffectOperationState = "failed"
	SideEffectUnknown   SideEffectOperationState = "unknown"
)

type SideEffectOperation struct {
	ID                string
	OperationKey      string
	RunID             string
	StageAttemptID    string
	EffectKind        string
	IdempotencyKey    string
	SourceDigest      string
	DestinationDigest string
	ReceiptRef        string
	PayloadJSON       string
	State             SideEffectOperationState
	CreatedAt         time.Time
	UpdatedAt         time.Time
	Version           int64
}

type CreateSideEffectOperationRequest struct {
	ID             string
	OperationKey   string
	RunID          string
	StageAttemptID string
	EffectKind     string
	IdempotencyKey string
	SourceDigest   string
	PayloadJSON    string
	Actor          string
	Reason         string
}

type TransitionSideEffectOperationRequest struct {
	OperationID       string
	ExpectedVersion   int64
	State             SideEffectOperationState
	DestinationDigest string
	ReceiptRef        string
	PayloadJSON       string
	Actor             string
	Reason            string
}

type ReconciliationAttemptState string

const (
	ReconciliationRunning   ReconciliationAttemptState = "running"
	ReconciliationCompleted ReconciliationAttemptState = "completed"
	ReconciliationFailed    ReconciliationAttemptState = "failed"
)

type ReconciliationAttempt struct {
	ID                    string
	OperationKey          string
	SubjectType           string
	SubjectID             string
	SideEffectOperationID string
	ControlOperationID    string
	Ordinal               int
	State                 ReconciliationAttemptState
	ObservedJSON          string
	Resolution            string
	CreatedAt             time.Time
	FinishedAt            *time.Time
	Version               int64
}

type StartReconciliationAttemptRequest struct {
	ID                    string
	OperationKey          string
	SubjectType           string
	SubjectID             string
	SideEffectOperationID string
	ControlOperationID    string
	Ordinal               int
	ObservedJSON          string
	Actor                 string
	Reason                string
}

type CompleteReconciliationAttemptRequest struct {
	AttemptID       string
	ExpectedVersion int64
	State           ReconciliationAttemptState
	ObservedJSON    string
	Resolution      string
	Actor           string
	Reason          string
}

// CapacityPool is a deployment-level, short-lived dispatch capacity. It is
// intentionally distinct from task and actor quota accounts: queue admission
// reserves durable work budget, while a pool slot is held only while a worker
// owns a running job.
type CapacityPool struct {
	ID        string
	PoolKey   string
	Capacity  int
	CreatedAt time.Time
	UpdatedAt time.Time
	Version   int64
}

type ConfigureCapacityPoolRequest struct {
	ID              string
	PoolKey         string
	Capacity        int
	ExpectedVersion int64
	Actor           string
	Reason          string
}

type ListQueuedDurableJobsRequest struct {
	Limit int
}

// ClaimNextDurableJobRequest is idempotent. A blank CapacityPoolKey means the
// caller only needs a per-job dispatch lease; otherwise one pool slot is
// acquired in the same transaction as the queued-to-running transition.
type ClaimNextDurableJobRequest struct {
	ID              string
	IdempotencyKey  string
	Owner           string
	LeaseTTL        time.Duration
	CapacityPoolKey string
	Actor           string
	Reason          string
}

// DurableJobDispatchClaim is the fence a worker must retain while executing
// the returned job. CapacityLease is nil when the caller did not request a
// deployment capacity pool.
type DurableJobDispatchClaim struct {
	ID              string
	IdempotencyKey  string
	Job             *DurableJob
	Owner           string
	LeaseTTL        time.Duration
	DispatchLease   *Lease
	CapacityPoolKey string
	CapacityLease   *Lease
	State           string
	ClaimedAt       time.Time
	UpdatedAt       time.Time
}

// ExpiredDurableJobRecovery is emitted after a worker dispatch lease expires.
// The job is projected to interrupted, all active job leases are released or
// expired, and the listed unfinished control operations are marked for
// reconcile instead of being blindly retried.
type ExpiredDurableJobRecovery struct {
	Claim      DurableJobDispatchClaim
	Job        DurableJob
	Operations []DurableControlOperation
}

type ScanExpiredDurableJobsRequest struct {
	// RunID narrows recovery to one durable Run. An empty value preserves the
	// existing deployment-wide recovery scan used by a worker process.
	RunID  string
	Limit  int
	Actor  string
	Reason string
}
