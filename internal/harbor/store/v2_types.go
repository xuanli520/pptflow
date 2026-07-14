package store

import (
	"errors"
	"time"
)

// V2 errors are intentionally distinguishable so application services can
// turn durable-store conflicts into explicit user-facing decisions.
var (
	ErrNotFound              = errors.New("store: record not found")
	ErrIdentityCollision     = errors.New("store: immutable identity collision")
	ErrOptimisticLock        = errors.New("store: optimistic version conflict")
	ErrImmutable             = errors.New("store: immutable record")
	ErrInvalidTransition     = errors.New("store: invalid state transition")
	ErrLeaseHeld             = errors.New("store: resource lease is held")
	ErrFencingToken          = errors.New("store: invalid lease fencing token")
	ErrIdempotencyConflict   = errors.New("store: idempotency key conflicts with an existing job")
	ErrTaskPurgeInProgress   = errors.New("store: task purge is in progress")
	ErrTaskPurgeFilesystem   = errors.New("store: task purge filesystem action failed")
	ErrTaskPurged            = errors.New("store: task has been irreversibly purged")
	ErrCandidateGCInProgress = errors.New("store: candidate garbage collection is in progress")
	ErrCandidateGCFilesystem = errors.New("store: candidate garbage collection filesystem action failed")
	ErrReviewApprovalNeeded  = errors.New("store: approved review decision is required")
	ErrRevisionNotValidated  = errors.New("store: revision must be validated before promotion")
	ErrInvalidUUIDv7Identity = errors.New("store: identity must be a UUIDv7")
)

type TaskLifecycleState string

const (
	TaskLifecycleDraft     TaskLifecycleState = "draft"
	TaskLifecycleReady     TaskLifecycleState = "ready"
	TaskLifecyclePublished TaskLifecycleState = "published"
	TaskLifecycleArchived  TaskLifecycleState = "archived"
	TaskLifecycleDeleted   TaskLifecycleState = "deleted"
)

type RevisionOrigin string

const (
	RevisionOriginGenerated RevisionOrigin = "generated"
	RevisionOriginImported  RevisionOrigin = "imported"
	RevisionOriginManual    RevisionOrigin = "manual"
	RevisionOriginRepair    RevisionOrigin = "repair"
	RevisionOriginFork      RevisionOrigin = "fork"
	RevisionOriginRollback  RevisionOrigin = "rollback"
)

type RevisionState string

const (
	RevisionStateSealed     RevisionState = "sealed"
	RevisionStateValidated  RevisionState = "validated"
	RevisionStateReleased   RevisionState = "released"
	RevisionStateSuperseded RevisionState = "superseded"
)

type WorkflowRunStatus string

const (
	WorkflowRunQueued              WorkflowRunStatus = "queued"
	WorkflowRunRunning             WorkflowRunStatus = "running"
	WorkflowRunPauseRequested      WorkflowRunStatus = "pause_requested"
	WorkflowRunPausing             WorkflowRunStatus = "pausing"
	WorkflowRunPaused              WorkflowRunStatus = "paused"
	WorkflowRunResumeRequested     WorkflowRunStatus = "resume_requested"
	WorkflowRunWaitingReview       WorkflowRunStatus = "waiting_review"
	WorkflowRunWaitingContinuation WorkflowRunStatus = "waiting_continuation"
	WorkflowRunSucceeded           WorkflowRunStatus = "succeeded"
	WorkflowRunFailedRecoverable   WorkflowRunStatus = "failed_recoverable"
	WorkflowRunFailedTerminal      WorkflowRunStatus = "failed_terminal"
	WorkflowRunCancelRequested     WorkflowRunStatus = "cancel_requested"
	WorkflowRunStopRequested       WorkflowRunStatus = "stop_requested"
	WorkflowRunCanceling           WorkflowRunStatus = "canceling"
	WorkflowRunCanceled            WorkflowRunStatus = "canceled"
	WorkflowRunInterrupted         WorkflowRunStatus = "interrupted"
	WorkflowRunInDoubt             WorkflowRunStatus = "in_doubt"
)

type StageExecutionStatus string

const (
	StageExecutionQueued      StageExecutionStatus = "queued"
	StageExecutionRunning     StageExecutionStatus = "running"
	StageExecutionWaiting     StageExecutionStatus = "waiting"
	StageExecutionCompleted   StageExecutionStatus = "completed"
	StageExecutionInfraFailed StageExecutionStatus = "infra_failed"
	StageExecutionInterrupted StageExecutionStatus = "interrupted"
	StageExecutionInDoubt     StageExecutionStatus = "in_doubt"
	StageExecutionReconciling StageExecutionStatus = "reconciling"
	StageExecutionCanceled    StageExecutionStatus = "canceled"
)

type Verdict string

const (
	VerdictPass        Verdict = "pass"
	VerdictNeedsRepair Verdict = "needs_repair"
	VerdictReject      Verdict = "reject"
	VerdictAdvisory    Verdict = "advisory"
)

type JobState string

const (
	JobQueued          JobState = "queued"
	JobRunning         JobState = "running"
	JobPauseRequested  JobState = "pause_requested"
	JobCancelRequested JobState = "cancel_requested"
	JobStopRequested   JobState = "stop_requested"
	JobPaused          JobState = "paused"
	JobCanceled        JobState = "canceled"
	JobSucceeded       JobState = "succeeded"
	JobFailed          JobState = "failed"
	JobInterrupted     JobState = "interrupted"
	JobInDoubt         JobState = "in_doubt"
)

type LeaseState string

const (
	LeaseActive   LeaseState = "active"
	LeaseReleased LeaseState = "released"
	LeaseExpired  LeaseState = "expired"
)

type ReviewDecisionAction string

const (
	ReviewDecisionApprove        ReviewDecisionAction = "approve"
	ReviewDecisionRequestChanges ReviewDecisionAction = "request_changes"
	ReviewDecisionRejectTerminal ReviewDecisionAction = "reject_terminal"
)

type WorkspaceState string

const (
	WorkspaceActive   WorkspaceState = "active"
	WorkspaceReleased WorkspaceState = "released"
	WorkspaceTrash    WorkspaceState = "trash"
	WorkspacePurged   WorkspaceState = "purged"
)

type RunAttemptStatus string

const (
	RunAttemptQueued      RunAttemptStatus = "queued"
	RunAttemptRunning     RunAttemptStatus = "running"
	RunAttemptSucceeded   RunAttemptStatus = "succeeded"
	RunAttemptFailed      RunAttemptStatus = "failed"
	RunAttemptCanceled    RunAttemptStatus = "canceled"
	RunAttemptInterrupted RunAttemptStatus = "interrupted"
)

type NodeAttemptStatus string

const (
	NodeAttemptQueued      NodeAttemptStatus = "queued"
	NodeAttemptRunning     NodeAttemptStatus = "running"
	NodeAttemptWaiting     NodeAttemptStatus = "waiting"
	NodeAttemptCompleted   NodeAttemptStatus = "completed"
	NodeAttemptInfraFailed NodeAttemptStatus = "infra_failed"
	NodeAttemptInterrupted NodeAttemptStatus = "interrupted"
	NodeAttemptInDoubt     NodeAttemptStatus = "in_doubt"
	NodeAttemptCanceled    NodeAttemptStatus = "canceled"
)

type TurnCheckpointStatus string

const (
	TurnCheckpointStarted     TurnCheckpointStatus = "started"
	TurnCheckpointCompleted   TurnCheckpointStatus = "completed"
	TurnCheckpointInterrupted TurnCheckpointStatus = "interrupted"
	TurnCheckpointFailed      TurnCheckpointStatus = "failed"
)

type OutboxState string

const (
	OutboxPending   OutboxState = "pending"
	OutboxLeased    OutboxState = "leased"
	OutboxPublished OutboxState = "published"
)

type DeletionRecordState string

const (
	DeletionRequested DeletionRecordState = "requested"
	DeletionBlocked   DeletionRecordState = "blocked"
	DeletionCompleted DeletionRecordState = "completed"
	DeletionCanceled  DeletionRecordState = "canceled"
)

// TaskV2 is the stable, path-independent task identity.
type TaskV2 struct {
	ID                string
	Slug              string
	Title             string
	MetadataJSON      string
	SourceRepo        string
	SourceCommit      string
	LifecycleState    TaskLifecycleState
	CurrentRevisionID string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	DeletedAt         *time.Time
	Version           int64
}

type CreateTaskV2Request struct {
	ID             string
	Slug           string
	Title          string
	MetadataJSON   string
	SourceRepo     string
	SourceCommit   string
	LifecycleState TaskLifecycleState
	Actor          string
	Reason         string
}

type UpdateTaskV2Request struct {
	TaskID          string
	ExpectedVersion int64
	Slug            string
	Title           string
	MetadataJSON    string
	LifecycleState  TaskLifecycleState
	Actor           string
	Reason          string
}

// TaskRevision is sealed at creation. No repository method mutates one.
type TaskRevision struct {
	ID                         string
	TaskID                     string
	VersionNumber              int
	ParentRevisionID           string
	Origin                     RevisionOrigin
	TaskDigest                 string
	ProposalDigest             string
	ManifestID                 string
	State                      RevisionState
	ValidationEvidenceManifest string
	StateVersion               int64
	StateUpdatedBy             string
	StateUpdatedAt             time.Time
	ChangeSummary              string
	MetadataJSON               string
	CreatedBy                  string
	CreatedAt                  time.Time
}

type CreateTaskRevisionRequest struct {
	ID               string
	TaskID           string
	ParentRevisionID string
	Origin           RevisionOrigin
	TaskDigest       string
	ProposalDigest   string
	ManifestID       string
	State            RevisionState
	ChangeSummary    string
	MetadataJSON     string
	Actor            string
	Reason           string
}

// CreateTaskWithRevisionRequest atomically persists a stable Task and its
// initial sealed revision after the caller has materialized and digested the
// managed snapshot. It intentionally has no filesystem behavior.
type CreateTaskWithRevisionRequest struct {
	Task     CreateTaskV2Request
	Revision CreateTaskRevisionRequest
}

type CreateTaskWithRevisionResult struct {
	Task     TaskV2
	Revision TaskRevision
}

// TransitionTaskRevisionStateRequest can only change revision lifecycle
// metadata. Task content, digest, provenance, and manifest identity remain
// immutable after creation.
type TransitionTaskRevisionStateRequest struct {
	RevisionID                 string
	ExpectedStateVersion       int64
	State                      RevisionState
	ValidationEvidenceManifest string
	Actor                      string
	Reason                     string
}

type WorkflowRun struct {
	ID                      string
	TaskID                  string
	RevisionID              string
	WorkflowTemplateID      string
	WorkflowTemplateVersion string
	ResolvedProfileHash     string
	DefinitionHash          string
	RunManifestJSON         string
	ParentRunID             string
	Trigger                 string
	ExecutionEpoch          int
	Status                  WorkflowRunStatus
	CreatedBy               string
	CreatedAt               time.Time
	StartedAt               *time.Time
	FinishedAt              *time.Time
	Version                 int64
}

type CreateWorkflowRunRequest struct {
	ID                      string
	TaskID                  string
	RevisionID              string
	WorkflowTemplateID      string
	WorkflowTemplateVersion string
	ResolvedProfileHash     string
	DefinitionHash          string
	RunManifestJSON         string
	ParentRunID             string
	Trigger                 string
	ExecutionEpoch          int
	Actor                   string
	Reason                  string
	Dispatch                *WorkflowRunDispatchRequest
}

// WorkflowRunDispatchRequest optionally creates the initial durable worker
// job in the same transaction as a newly frozen run. A queued WorkflowRun
// with dispatch configured must never become visible without its job/outbox
// record, while lower-level store fixtures may still create a run alone.
type WorkflowRunDispatchRequest struct {
	CommandType    string
	PayloadJSON    string
	IdempotencyKey string
	Priority       int
}

type TransitionWorkflowRunRequest struct {
	RunID           string
	ExpectedVersion int64
	Status          WorkflowRunStatus
	Actor           string
	Reason          string
}

// StageAttempt is append-only once it reaches a terminal execution state.
// A retry must be represented as a new record linked by RetryOfStageAttemptID.
type StageAttempt struct {
	ID                    string
	RunID                 string
	RetryOfStageAttemptID string
	StageKey              string
	StageGroup            string
	Ordinal               int
	InputFingerprint      string
	ExecutionStatus       StageExecutionStatus
	Verdict               Verdict
	BudgetSnapshotJSON    string
	RetrySnapshotJSON     string
	ArtifactManifestID    string
	ErrorText             string
	FailureClass          string
	CreatedAt             time.Time
	StartedAt             *time.Time
	FinishedAt            *time.Time
	Version               int64
}

type CreateStageAttemptRequest struct {
	ID                    string
	RunID                 string
	RetryOfStageAttemptID string
	StageKey              string
	StageGroup            string
	Ordinal               int
	InputFingerprint      string
	BudgetSnapshotJSON    string
	RetrySnapshotJSON     string
	Actor                 string
	Reason                string
}

type TransitionStageAttemptRequest struct {
	StageAttemptID     string
	ExpectedVersion    int64
	ExecutionStatus    StageExecutionStatus
	Verdict            Verdict
	ArtifactManifestID string
	ErrorText          string
	FailureClass       string
	Actor              string
	Reason             string
}

type DurableJob struct {
	ID             string
	CommandType    string
	EntityType     string
	EntityID       string
	RunID          string
	StageAttemptID string
	State          JobState
	Priority       int
	PayloadJSON    string
	IdempotencyKey string
	CreatedBy      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	StartedAt      *time.Time
	FinishedAt     *time.Time
	Version        int64
}

type CreateDurableJobRequest struct {
	ID             string
	CommandType    string
	EntityType     string
	EntityID       string
	RunID          string
	StageAttemptID string
	Priority       int
	PayloadJSON    string
	IdempotencyKey string
	Actor          string
	Reason         string
}

type TransitionDurableJobRequest struct {
	JobID           string
	ExpectedVersion int64
	State           JobState
	Actor           string
	Reason          string
}

type Lease struct {
	ID           string
	ResourceType string
	ResourceID   string
	Owner        string
	JobID        string
	ExpiresAt    time.Time
	FencingToken uint64
	State        LeaseState
	CreatedAt    time.Time
	UpdatedAt    time.Time
	Version      int64
}

type AcquireLeaseRequest struct {
	ID           string
	ResourceType string
	ResourceID   string
	Owner        string
	JobID        string
	TTL          time.Duration
	Actor        string
	Reason       string
}

type HeartbeatLeaseRequest struct {
	LeaseID         string
	Owner           string
	FencingToken    uint64
	ExpectedVersion int64
	TTL             time.Duration
	Actor           string
	Reason          string
}

type ReleaseLeaseRequest struct {
	LeaseID         string
	Owner           string
	FencingToken    uint64
	ExpectedVersion int64
	Actor           string
	Reason          string
}

type ReviewRequest struct {
	ID                     string
	RevisionID             string
	EvidenceManifestDigest string
	State                  string
	CreatedBy              string
	CreatedAt              time.Time
	ClosedAt               *time.Time
}

type CreateReviewRequest struct {
	ID                     string
	RevisionID             string
	EvidenceManifestDigest string
	Actor                  string
	Reason                 string
}

type ReviewDecision struct {
	ID                     string
	ReviewRequestID        string
	RevisionID             string
	Action                 ReviewDecisionAction
	ExpectedRevisionDigest string
	Actor                  string
	Reason                 string
	CreatedAt              time.Time
}

type RecordReviewDecisionRequest struct {
	ID                     string
	ReviewRequestID        string
	RevisionID             string
	Action                 ReviewDecisionAction
	ExpectedRevisionDigest string
	Actor                  string
	Reason                 string
}

type PromoteCurrentRevisionRequest struct {
	TaskID          string
	RevisionID      string
	ExpectedVersion int64
	Actor           string
	Reason          string
}

// ManagedWorkspace is a disposable execution checkout. It never supplies a
// Task identity; stable Task and Revision references are persisted separately.
type ManagedWorkspace struct {
	ID         string
	RootURI    string
	Purpose    string
	TaskID     string
	RevisionID string
	RunID      string
	State      WorkspaceState
	CreatedAt  time.Time
	UpdatedAt  time.Time
	Version    int64
}

type CreateManagedWorkspaceRequest struct {
	ID         string
	RootURI    string
	Purpose    string
	TaskID     string
	RevisionID string
	RunID      string
	Actor      string
	Reason     string
}

type TransitionManagedWorkspaceRequest struct {
	WorkspaceID     string
	ExpectedVersion int64
	State           WorkspaceState
	Actor           string
	Reason          string
}

type RunAttempt struct {
	ID         string
	RunID      string
	Ordinal    int
	Trigger    string
	ResumeFrom string
	Status     RunAttemptStatus
	CreatedAt  time.Time
	StartedAt  *time.Time
	FinishedAt *time.Time
	Version    int64
}

type CreateRunAttemptRequest struct {
	ID         string
	RunID      string
	Ordinal    int
	Trigger    string
	ResumeFrom string
	Actor      string
	Reason     string
}

type TransitionRunAttemptRequest struct {
	RunAttemptID    string
	ExpectedVersion int64
	Status          RunAttemptStatus
	Actor           string
	Reason          string
}

type NodeAttempt struct {
	ID             string
	StageAttemptID string
	NodeID         string
	Generation     int
	Attempt        int
	Status         NodeAttemptStatus
	IdempotencyKey string
	ErrorText      string
	CreatedAt      time.Time
	StartedAt      *time.Time
	FinishedAt     *time.Time
	Version        int64
}

type CreateNodeAttemptRequest struct {
	ID             string
	StageAttemptID string
	NodeID         string
	Generation     int
	Attempt        int
	IdempotencyKey string
	Actor          string
	Reason         string
}

type TransitionNodeAttemptRequest struct {
	NodeAttemptID   string
	ExpectedVersion int64
	Status          NodeAttemptStatus
	ErrorText       string
	Actor           string
	Reason          string
}

type TurnCheckpoint struct {
	ID            string
	NodeAttemptID string
	Turn          int
	Substep       string
	Status        TurnCheckpointStatus
	InputDigest   string
	ArtifactID    string
	PayloadJSON   string
	CreatedAt     time.Time
	FinishedAt    *time.Time
	Version       int64
}

type CreateTurnCheckpointRequest struct {
	ID            string
	NodeAttemptID string
	Turn          int
	Substep       string
	InputDigest   string
	ArtifactID    string
	PayloadJSON   string
	Actor         string
	Reason        string
}

type TransitionTurnCheckpointRequest struct {
	CheckpointID    string
	ExpectedVersion int64
	Status          TurnCheckpointStatus
	ArtifactID      string
	PayloadJSON     string
	Actor           string
	Reason          string
}

type OutboxEvent struct {
	ID                string
	Topic             string
	EntityType        string
	EntityID          string
	PayloadJSON       string
	IdempotencyKey    string
	State             OutboxState
	AvailableAt       time.Time
	LeaseOwner        string
	LeaseExpiresAt    *time.Time
	LeaseFencingToken uint64
	DeliveryCount     int
	LastError         string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	PublishedAt       *time.Time
	Version           int64
}

type CreateOutboxEventRequest struct {
	ID             string
	Topic          string
	EntityType     string
	EntityID       string
	PayloadJSON    string
	IdempotencyKey string
	AvailableAt    time.Time
	Actor          string
	Reason         string
}

// LocalPackageRelease records a locally produced package and its immutable
// evidence. It does not upload, invoke a provider, or model remote state.
type LocalPackageRelease struct {
	ID             string
	ReleaseVersion string
	RevisionID     string
	TaskID         string
	TaskDigest     string
	PackageRef     string
	EvidenceRef    string
	PublishedAt    time.Time
	WithdrawnAt    *time.Time
	WithdrawnBy    string
	CreatedBy      string
	RecordVersion  int64
}

type CreateLocalPackageReleaseRequest struct {
	ID string
	// IdempotencyKey is optional for callers that reconcile only by immutable
	// release version. When supplied it must be UUIDv7 and becomes the release
	// identity, so a retry is protected by the global entity identity registry.
	IdempotencyKey string
	ReleaseVersion string
	RevisionID     string
	TaskID         string
	TaskDigest     string
	PackageRef     string
	EvidenceRef    string
	Actor          string
	Reason         string
}

type ReleaseChannel struct {
	Channel   string
	ReleaseID string
	UpdatedAt time.Time
	UpdatedBy string
	Version   int64
}

// SetReleaseChannelRequest uses ExpectedVersion=0 only to create a previously
// absent channel. Existing pointers require their current version.
type SetReleaseChannelRequest struct {
	Channel         string
	ReleaseID       string
	ExpectedVersion int64
	Actor           string
	Reason          string
}

type DeletionRecord struct {
	ID          string
	EntityType  string
	EntityID    string
	Action      string
	State       DeletionRecordState
	Actor       string
	Reason      string
	CreatedAt   time.Time
	CompletedAt *time.Time
	Version     int64
}

type CreateDeletionRecordRequest struct {
	ID         string
	EntityType string
	EntityID   string
	Action     string
	Actor      string
	Reason     string
}

type TransitionDeletionRecordRequest struct {
	DeletionRecordID string
	ExpectedVersion  int64
	State            DeletionRecordState
	Actor            string
	Reason           string
}

type PurgeDependencyQuery struct {
	EntityType string
	EntityID   string
}

type PurgeDependencyReport struct {
	TaskID              string
	ActiveWorkspaceIDs  []string
	ActiveRunIDs        []string
	ActiveJobIDs        []string
	ActiveLeaseIDs      []string
	PendingOutboxIDs    []string
	ReleaseIDs          []string
	ArtifactManifestIDs []string
	ArtifactRefIDs      []string
	PackageRefs         []string
	EvidenceRefs        []string
}

func (r PurgeDependencyReport) HasBlockers() bool {
	return len(r.ActiveWorkspaceIDs) > 0 || len(r.ActiveRunIDs) > 0 || len(r.ActiveJobIDs) > 0 || len(r.ActiveLeaseIDs) > 0 || len(r.PendingOutboxIDs) > 0 || len(r.ReleaseIDs) > 0 || len(r.ArtifactManifestIDs) > 0 || len(r.ArtifactRefIDs) > 0
}

type TaskPurgeOperationState string

const (
	TaskPurgeInProgress TaskPurgeOperationState = "in_progress"
	TaskPurgeBlocked    TaskPurgeOperationState = "blocked"
	TaskPurgeCompleted  TaskPurgeOperationState = "completed"
)

// TaskPurgeOperation persists one irreversible-purge intent across the
// SQLite/filesystem boundary. Its lease fields are immutable for one fencing
// epoch and change only when a crashed operation is safely reclaimed.
type TaskPurgeOperation struct {
	ID                  string
	TaskID              string
	IdempotencyKey      string
	ExpectedTaskVersion int64
	Actor               string
	Reason              string
	State               TaskPurgeOperationState
	LeaseID             string
	LeaseOwner          string
	LeaseFencingToken   uint64
	LeaseVersion        int64
	Dependencies        PurgeDependencyReport
	LastError           string
	CreatedAt           time.Time
	UpdatedAt           time.Time
	CompletedAt         *time.Time
	Version             int64
}

type PrepareTaskPurgeRequest struct {
	ID                  string
	TaskID              string
	ExpectedTaskVersion int64
	IdempotencyKey      string
	Actor               string
	Reason              string
	LeaseTTL            time.Duration
}

type PrepareTaskPurgeResult struct {
	Task      TaskV2
	Operation TaskPurgeOperation
	Acquired  bool
}

// FinalizeTaskPurgeRequest keeps the filesystem action inside the SQLite
// write transaction after the operation and lease are revalidated. The action
// must be idempotent because a process may fail after removal and before the
// transaction commits.
type FinalizeTaskPurgeRequest struct {
	OperationID     string
	ExpectedVersion int64
	Actor           string
	Reason          string
	RemoveDirectory func() error
}

type FinalizeTaskPurgeResult struct {
	Operation    TaskPurgeOperation
	Dependencies PurgeDependencyReport
	Purged       bool
}

type RecordTaskPurgeFailureRequest struct {
	OperationID     string
	ExpectedVersion int64
	Actor           string
	Reason          string
	ErrorText       string
}

type AuditEvent struct {
	ID           string
	Actor        string
	EntityType   string
	EntityID     string
	Action       string
	Reason       string
	PayloadJSON  string
	OperationKey string
	CreatedAt    time.Time
}

type ListAuditEventsRequest struct {
	EntityType string
	EntityID   string
	Limit      int
}
