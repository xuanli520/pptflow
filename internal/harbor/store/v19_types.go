package store

import "time"

// TrialExecutionStatus is the aggregate lifecycle of one logical evaluator
// sample. It deliberately records technical execution health only; pass/fail
// semantics and evaluator-specific result schemas remain outside the store.
type TrialExecutionStatus string

const (
	TrialExecutionQueued      TrialExecutionStatus = "queued"
	TrialExecutionRunning     TrialExecutionStatus = "running"
	TrialExecutionWaiting     TrialExecutionStatus = "waiting"
	TrialExecutionCompleted   TrialExecutionStatus = "completed"
	TrialExecutionInfraFailed TrialExecutionStatus = "infra_failed"
	TrialExecutionInterrupted TrialExecutionStatus = "interrupted"
	TrialExecutionInDoubt     TrialExecutionStatus = "in_doubt"
	TrialExecutionReconciling TrialExecutionStatus = "reconciling"
	TrialExecutionCanceled    TrialExecutionStatus = "canceled"
)

// TrialAttemptStatus is the lifecycle of one technical try for an existing
// TrialExecution. A retry creates another TrialAttempt with the same logical
// TrialExecution ID; it never creates a new logical sample.
type TrialAttemptStatus string

const (
	TrialAttemptQueued      TrialAttemptStatus = "queued"
	TrialAttemptRunning     TrialAttemptStatus = "running"
	TrialAttemptWaiting     TrialAttemptStatus = "waiting"
	TrialAttemptCompleted   TrialAttemptStatus = "completed"
	TrialAttemptInfraFailed TrialAttemptStatus = "infra_failed"
	TrialAttemptInterrupted TrialAttemptStatus = "interrupted"
	TrialAttemptInDoubt     TrialAttemptStatus = "in_doubt"
	TrialAttemptReconciling TrialAttemptStatus = "reconciling"
	TrialAttemptCanceled    TrialAttemptStatus = "canceled"
)

// TrialExecution is one immutable logical sample bound to exactly one Run and
// StageAttempt. Ordinal is local to that evaluator stage; policy decides how
// many logical samples exist, while the store only prevents duplicate logical
// coordinates.
type TrialExecution struct {
	ID             string               `json:"id"`
	RunID          string               `json:"run_id"`
	StageAttemptID string               `json:"stage_attempt_id"`
	StageKey       string               `json:"stage_key"`
	Ordinal        int                  `json:"ordinal"`
	Status         TrialExecutionStatus `json:"status"`
	CreatedAt      time.Time            `json:"created_at"`
	UpdatedAt      time.Time            `json:"updated_at"`
	StartedAt      *time.Time           `json:"started_at,omitempty"`
	FinishedAt     *time.Time           `json:"finished_at,omitempty"`
	Version        int64                `json:"version"`
}

// CreateTrialExecutionRequest creates one logical evaluator sample. The
// caller may allocate ID first to bind durable work, but Run/Stage/ordinal
// remain the idempotency-safe logical coordinate and are checked against the
// existing StageAttempt.
type CreateTrialExecutionRequest struct {
	ID             string
	RunID          string
	StageAttemptID string
	StageKey       string
	Ordinal        int
	Actor          string
	Reason         string
}

// TransitionTrialExecutionRequest changes only the aggregate lifecycle of a
// logical sample. It cannot rebind the Run, StageAttempt, stage key, ordinal,
// or a technical TrialAttempt.
type TransitionTrialExecutionRequest struct {
	TrialExecutionID string
	ExpectedVersion  int64
	Status           TrialExecutionStatus
	Actor            string
	Reason           string
}

// TrialAttempt is one append-only technical attempt to produce the associated
// logical TrialExecution. RetryOfTrialAttemptID forms a linear retry chain.
type TrialAttempt struct {
	ID                    string             `json:"id"`
	TrialExecutionID      string             `json:"trial_execution_id"`
	RetryOfTrialAttemptID string             `json:"retry_of_trial_attempt_id,omitempty"`
	Ordinal               int                `json:"ordinal"`
	Status                TrialAttemptStatus `json:"status"`
	ErrorText             string             `json:"error_text,omitempty"`
	FailureClass          string             `json:"failure_class,omitempty"`
	CreatedAt             time.Time          `json:"created_at"`
	UpdatedAt             time.Time          `json:"updated_at"`
	StartedAt             *time.Time         `json:"started_at,omitempty"`
	FinishedAt            *time.Time         `json:"finished_at,omitempty"`
	Version               int64              `json:"version"`
}

// CreateTrialAttemptRequest appends a technical attempt to an existing
// logical sample. Ordinal one has no predecessor; later ordinals must directly
// retry the preceding eligible TrialAttempt for the same TrialExecution.
type CreateTrialAttemptRequest struct {
	ID                    string
	TrialExecutionID      string
	RetryOfTrialAttemptID string
	Ordinal               int
	Actor                 string
	Reason                string
}

// TransitionTrialAttemptRequest records only technical lifecycle and
// diagnostics. Evaluator-specific result contents stay in immutable artifacts
// and receipts, not in this generic durable store API.
type TransitionTrialAttemptRequest struct {
	TrialAttemptID  string
	ExpectedVersion int64
	Status          TrialAttemptStatus
	ErrorText       string
	FailureClass    string
	Actor           string
	Reason          string
}
