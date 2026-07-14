package store

import "time"

const (
	// RunWorkerSupervisorLeaseResourceType is the durable, run-scoped fence
	// held by one controlled child worker.
	RunWorkerSupervisorLeaseResourceType = "workflow_run_worker"
)

type RunWorkerHandoffState string

const (
	RunWorkerHandoffLaunching RunWorkerHandoffState = "launching"
	RunWorkerHandoffHandedOff RunWorkerHandoffState = "handed_off"
	RunWorkerHandoffReleased  RunWorkerHandoffState = "released"
	RunWorkerHandoffFailed    RunWorkerHandoffState = "failed"
	RunWorkerHandoffExpired   RunWorkerHandoffState = "expired"
)

// RunWorkerHandoff is the durable operation and immutable process receipt for
// one controlled child launch. A row stays active until the claimed worker
// lease is normally released or a scoped reconciliation proves it expired.
type RunWorkerHandoff struct {
	ID                        string                `json:"operation_id"`
	IdempotencyKey            string                `json:"idempotency_key"`
	RequestFingerprint        string                `json:"request_fingerprint"`
	RunID                     string                `json:"run_id"`
	ExpectedRunVersion        int64                 `json:"expected_run_version"`
	ExpectedRunExecutionEpoch int                   `json:"expected_run_execution_epoch"`
	ExpectedRunDefinitionHash string                `json:"expected_run_definition_hash"`
	Owner                     string                `json:"owner"`
	Actor                     string                `json:"actor"`
	Reason                    string                `json:"reason"`
	State                     RunWorkerHandoffState `json:"state"`
	LaunchDeadlineAt          time.Time             `json:"launch_deadline_at"`
	WorkerLeaseID             string                `json:"worker_lease_id,omitempty"`
	WorkerLeaseOwner          string                `json:"worker_lease_owner,omitempty"`
	WorkerLeaseFencingToken   uint64                `json:"worker_lease_fencing_token,omitempty"`
	WorkerLeaseVersion        int64                 `json:"worker_lease_version,omitempty"`
	ProcessID                 int                   `json:"pid,omitempty"`
	LogPath                   string                `json:"log_path,omitempty"`
	ReceiptJSON               string                `json:"receipt_json,omitempty"`
	FailureReason             string                `json:"failure_reason,omitempty"`
	CreatedAt                 time.Time             `json:"created_at"`
	UpdatedAt                 time.Time             `json:"updated_at"`
	SpawnedAt                 *time.Time            `json:"spawned_at,omitempty"`
	HandedOffAt               *time.Time            `json:"handed_off_at,omitempty"`
	ReleasedAt                *time.Time            `json:"released_at,omitempty"`
	Version                   int64                 `json:"version"`
}

type ReserveRunWorkerHandoffRequest struct {
	ID                        string
	IdempotencyKey            string
	RequestFingerprint        string
	RunID                     string
	ExpectedRunVersion        int64
	ExpectedRunExecutionEpoch int
	ExpectedRunDefinitionHash string
	Owner                     string
	Actor                     string
	Reason                    string
	LaunchTTL                 time.Duration
}

type ReserveRunWorkerHandoffResult struct {
	Handoff  RunWorkerHandoff
	Replayed bool
	Launch   bool
}

type RecordRunWorkerHandoffSpawnedRequest struct {
	OperationID string
	ProcessID   int
	LogPath     string
	Actor       string
	Reason      string
}

type FailRunWorkerHandoffRequest struct {
	OperationID string
	Failure     string
	Actor       string
	Reason      string
}

type ClaimRunWorkerHandoffRequest struct {
	OperationID string
	RunID       string
	Owner       string
	ProcessID   int
	LogPath     string
	LeaseTTL    time.Duration
	Actor       string
	Reason      string
}

type RunWorkerHandoffClaim struct {
	Handoff     RunWorkerHandoff
	WorkerLease Lease
}

type ReleaseRunWorkerHandoffRequest struct {
	OperationID string
	WorkerLease Lease
	Actor       string
	Reason      string
}

type ReconcileRunWorkerHandoffsRequest struct {
	RunID  string
	Actor  string
	Reason string
}
