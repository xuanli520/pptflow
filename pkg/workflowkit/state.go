package workflowkit

import "fmt"

// ExecutionStatus records execution mechanics independently from a completed
// stage's domain verdict. This prevents a trustworthy report that finds a
// repairable issue from being represented as an infrastructure failure.
type ExecutionStatus string

const (
	StatusQueued          ExecutionStatus = "queued"
	StatusRunning         ExecutionStatus = "running"
	StatusWaiting         ExecutionStatus = "waiting"
	StatusPauseRequested  ExecutionStatus = "pause_requested"
	StatusPausing         ExecutionStatus = "pausing"
	StatusPaused          ExecutionStatus = "paused"
	StatusCancelRequested ExecutionStatus = "cancel_requested"
	StatusStopRequested   ExecutionStatus = "stop_requested"
	StatusCanceling       ExecutionStatus = "canceling"
	StatusCompleted       ExecutionStatus = "completed"
	StatusInfraFailed     ExecutionStatus = "infra_failed"
	StatusInterrupted     ExecutionStatus = "interrupted"
	StatusInDoubt         ExecutionStatus = "in_doubt"
	StatusReconciling     ExecutionStatus = "reconciling"
	StatusCanceled        ExecutionStatus = "canceled"
)

func (status ExecutionStatus) valid() bool {
	switch status {
	case StatusQueued, StatusRunning, StatusWaiting, StatusPauseRequested, StatusPausing, StatusPaused,
		StatusCancelRequested, StatusStopRequested, StatusCanceling, StatusCompleted, StatusInfraFailed,
		StatusInterrupted, StatusInDoubt, StatusReconciling, StatusCanceled:
		return true
	default:
		return false
	}
}

// IsTerminal reports whether a status closes this particular durable attempt.
// A new continuation after a terminal state must append a new attempt rather
// than mutate this one.
func (status ExecutionStatus) IsTerminal() bool {
	switch status {
	case StatusCompleted, StatusInfraFailed, StatusInterrupted, StatusCanceled:
		return true
	default:
		return false
	}
}

// RequiresReconcile reports statuses from which a continuation cannot run
// until a recovery adapter has established the outcome of possible side
// effects.
func (status ExecutionStatus) RequiresReconcile() bool {
	return status == StatusInDoubt || status == StatusReconciling
}

// CanTransitionExecution reports whether the generic execution state machine
// permits a single durable transition. It deliberately does not permit
// reopening terminal states.
func CanTransitionExecution(from, to ExecutionStatus) bool {
	if !from.valid() || !to.valid() || from == to || from.IsTerminal() {
		return false
	}
	switch from {
	case StatusQueued:
		return to == StatusRunning || to == StatusCancelRequested
	case StatusRunning:
		switch to {
		case StatusWaiting, StatusPauseRequested, StatusCancelRequested, StatusStopRequested,
			StatusCompleted, StatusInfraFailed, StatusInterrupted, StatusInDoubt:
			return true
		default:
			return false
		}
	case StatusWaiting:
		switch to {
		case StatusRunning, StatusCompleted, StatusCancelRequested, StatusStopRequested, StatusInDoubt:
			return true
		default:
			return false
		}
	case StatusPauseRequested:
		return to == StatusPausing || to == StatusPaused || to == StatusInDoubt
	case StatusPausing:
		return to == StatusPaused || to == StatusInDoubt || to == StatusCanceling
	case StatusPaused:
		return to == StatusRunning || to == StatusCancelRequested || to == StatusStopRequested
	case StatusCancelRequested:
		return to == StatusCanceling || to == StatusCanceled || to == StatusInDoubt
	case StatusStopRequested:
		return to == StatusCanceling || to == StatusInDoubt
	case StatusCanceling:
		return to == StatusCanceled || to == StatusInDoubt
	case StatusInDoubt:
		return to == StatusReconciling
	case StatusReconciling:
		return to == StatusCompleted || to == StatusInfraFailed || to == StatusCanceled
	default:
		return false
	}
}

// ValidateExecutionTransition returns an explicit validation error for an
// illegal state transition.
func ValidateExecutionTransition(from, to ExecutionStatus) error {
	if CanTransitionExecution(from, to) {
		return nil
	}
	return fmt.Errorf("%w: execution transition %q -> %q is not allowed", ErrInvalidAttemptRecord, from, to)
}

// Verdict is a domain outcome emitted only after execution completed and a
// report is trustworthy.
type Verdict string

const (
	VerdictNone        Verdict = ""
	VerdictPass        Verdict = "pass"
	VerdictNeedsRepair Verdict = "needs_repair"
	VerdictReject      Verdict = "reject"
	VerdictAdvisory    Verdict = "advisory"
)

func (verdict Verdict) valid() bool {
	switch verdict {
	case VerdictNone, VerdictPass, VerdictNeedsRepair, VerdictReject, VerdictAdvisory:
		return true
	default:
		return false
	}
}

// FailureClass describes a failed execution without embedding a particular
// product's check or policy vocabulary.
type FailureClass string

const (
	FailureNone        FailureClass = ""
	FailureTransient   FailureClass = "transient"
	FailureTimeout     FailureClass = "timeout"
	FailureRateLimited FailureClass = "rate_limited"
	FailureNetwork     FailureClass = "network"
	FailureProcess     FailureClass = "process"
	FailurePermanent   FailureClass = "permanent"
	FailurePolicy      FailureClass = "policy"
	FailureUnknown     FailureClass = "unknown"
)

func (class FailureClass) valid() bool {
	switch class {
	case FailureNone, FailureTransient, FailureTimeout, FailureRateLimited, FailureNetwork, FailureProcess, FailurePermanent, FailurePolicy, FailureUnknown:
		return true
	default:
		return false
	}
}

// Outcome is the terminal two-channel result for an attempt. Completed work
// carries a verdict; all non-completed terminal statuses carry no verdict.
type Outcome struct {
	Status  ExecutionStatus `json:"status"`
	Verdict Verdict         `json:"verdict,omitempty"`
	Failure FailureClass    `json:"failure,omitempty"`
}

// Validate verifies that an outcome cannot conflate report verdicts with
// execution infrastructure state.
func (outcome Outcome) Validate() error {
	if !outcome.Status.IsTerminal() {
		return fmt.Errorf("%w: outcome status %q is not terminal", ErrInvalidAttemptRecord, outcome.Status)
	}
	if !outcome.Verdict.valid() {
		return fmt.Errorf("%w: unsupported verdict %q", ErrInvalidAttemptRecord, outcome.Verdict)
	}
	if !outcome.Failure.valid() {
		return fmt.Errorf("%w: unsupported failure class %q", ErrInvalidAttemptRecord, outcome.Failure)
	}
	switch outcome.Status {
	case StatusCompleted:
		if outcome.Verdict == VerdictNone {
			return fmt.Errorf("%w: completed outcome requires a verdict", ErrInvalidAttemptRecord)
		}
		if outcome.Failure != FailureNone {
			return fmt.Errorf("%w: completed outcome cannot carry a failure class", ErrInvalidAttemptRecord)
		}
	case StatusInfraFailed:
		if outcome.Verdict != VerdictNone {
			return fmt.Errorf("%w: infrastructure failure cannot carry a verdict", ErrInvalidAttemptRecord)
		}
		if outcome.Failure == FailureNone {
			return fmt.Errorf("%w: infrastructure failure requires a failure class", ErrInvalidAttemptRecord)
		}
	case StatusInterrupted, StatusCanceled:
		if outcome.Verdict != VerdictNone {
			return fmt.Errorf("%w: %s outcome cannot carry a verdict", ErrInvalidAttemptRecord, outcome.Status)
		}
		if outcome.Status == StatusCanceled && outcome.Failure != FailureNone {
			return fmt.Errorf("%w: canceled outcome cannot carry a failure class", ErrInvalidAttemptRecord)
		}
	}
	return nil
}
