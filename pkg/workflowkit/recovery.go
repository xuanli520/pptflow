package workflowkit

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var ErrInvalidRecovery = errors.New("workflowkit: invalid recovery subject")

// RecoveryAction is a conservative generic decision after a process or worker
// interruption. It never directly starts work; a lifecycle adapter performs
// the resulting durable action.
type RecoveryAction string

const (
	RecoveryNoAction            RecoveryAction = "no_action"
	RecoveryResumeCheckpoint    RecoveryAction = "resume_checkpoint"
	RecoveryScheduleNewAttempt  RecoveryAction = "schedule_new_attempt"
	RecoveryMarkInterrupted     RecoveryAction = "mark_interrupted"
	RecoveryReconcile           RecoveryAction = "reconcile"
	RecoveryAwaitContinuation   RecoveryAction = "await_continuation"
	RecoveryAwaitControlOutcome RecoveryAction = "await_control_outcome"
)

func (action RecoveryAction) valid() bool {
	switch action {
	case RecoveryNoAction, RecoveryResumeCheckpoint, RecoveryScheduleNewAttempt, RecoveryMarkInterrupted, RecoveryReconcile, RecoveryAwaitContinuation, RecoveryAwaitControlOutcome:
		return true
	default:
		return false
	}
}

// RecoveryReason is a stable machine-readable basis for a decision. Domain
// adapters add human-facing explanation outside the generic kernel.
type RecoveryReason string

const (
	RecoveryReasonLeaseActive           RecoveryReason = "lease_active"
	RecoveryReasonLeaseExpired          RecoveryReason = "lease_expired"
	RecoveryReasonCheckpointReusable    RecoveryReason = "checkpoint_reusable"
	RecoveryReasonCheckpointUnavailable RecoveryReason = "checkpoint_unavailable"
	RecoveryReasonInputsChanged         RecoveryReason = "inputs_changed"
	RecoveryReasonDefinitionChanged     RecoveryReason = "definition_changed"
	RecoveryReasonUnknownSideEffect     RecoveryReason = "unknown_side_effect"
	RecoveryReasonExplicitTerminate     RecoveryReason = "explicit_terminate"
	RecoveryReasonControlInFlight       RecoveryReason = "control_in_flight"
	RecoveryReasonTerminalExecution     RecoveryReason = "terminal_execution"
	RecoveryReasonCanceledExecution     RecoveryReason = "canceled_execution"
	RecoveryReasonInterruptedExecution  RecoveryReason = "interrupted_execution"
)

// RecoverySubject supplies only observed recovery facts. A caller must record
// an explicit control intent separately from an unexpected worker loss; the
// reconciler never guesses one from a canceled context.
type RecoverySubject struct {
	SubjectID              string          `json:"subject_id"`
	Status                 ExecutionStatus `json:"status"`
	LeaseExpiresAt         time.Time       `json:"lease_expires_at,omitempty"`
	ObservedAt             time.Time       `json:"observed_at"`
	ControlAction          ControlAction   `json:"control_action,omitempty"`
	ControlStatus          ControlStatus   `json:"control_status,omitempty"`
	CheckpointRecoverable  bool            `json:"checkpoint_recoverable"`
	InputsUnchanged        bool            `json:"inputs_unchanged"`
	DefinitionUnchanged    bool            `json:"definition_unchanged"`
	UnknownExternalOutcome bool            `json:"unknown_external_outcome"`
}

// LeaseExpired reports whether the observed worker lease is no longer valid.
func (subject RecoverySubject) LeaseExpired() bool {
	return !subject.LeaseExpiresAt.IsZero() && !subject.ObservedAt.Before(subject.LeaseExpiresAt)
}

func (subject RecoverySubject) validate() error {
	if err := validateRequired("recovery subject id", subject.SubjectID, ErrInvalidRecovery); err != nil {
		return err
	}
	if !subject.Status.valid() {
		return fmt.Errorf("%w: unsupported execution status %q", ErrInvalidRecovery, subject.Status)
	}
	if subject.ObservedAt.IsZero() {
		return fmt.Errorf("%w: recovery observation time is required", ErrInvalidRecovery)
	}
	if subject.ControlAction != "" && !subject.ControlAction.valid() {
		return fmt.Errorf("%w: unsupported control action %q", ErrInvalidRecovery, subject.ControlAction)
	}
	if subject.ControlStatus != "" && !subject.ControlStatus.valid() {
		return fmt.Errorf("%w: unsupported control status %q", ErrInvalidRecovery, subject.ControlStatus)
	}
	if requiresLeaseFact(subject.Status) && subject.LeaseExpiresAt.IsZero() {
		return fmt.Errorf("%w: active execution status %q requires a lease expiration", ErrInvalidRecovery, subject.Status)
	}
	return nil
}

func requiresLeaseFact(status ExecutionStatus) bool {
	switch status {
	case StatusQueued, StatusRunning, StatusWaiting, StatusPauseRequested, StatusPausing, StatusCancelRequested, StatusStopRequested, StatusCanceling:
		return true
	default:
		return false
	}
}

// RecoveryDecision is an immutable policy result. It does not itself mutate
// any job, lease, quota, or attempt record.
type RecoveryDecision struct {
	SubjectID string           `json:"subject_id"`
	Action    RecoveryAction   `json:"action"`
	Reasons   []RecoveryReason `json:"reasons"`
}

// Clone returns an independent decision snapshot.
func (decision RecoveryDecision) Clone() RecoveryDecision {
	decision.Reasons = append([]RecoveryReason(nil), decision.Reasons...)
	return decision
}

func newRecoveryDecision(subject RecoverySubject, action RecoveryAction, reasons ...RecoveryReason) RecoveryDecision {
	return RecoveryDecision{SubjectID: subject.SubjectID, Action: action, Reasons: append([]RecoveryReason(nil), reasons...)}
}

// DecideRecovery applies the generic conservative policy. In particular,
// unknown side effects reconcile first and an explicit terminate request never
// turns into an automatic resume after restart.
func DecideRecovery(subject RecoverySubject) (RecoveryDecision, error) {
	if err := subject.validate(); err != nil {
		return RecoveryDecision{}, err
	}
	if subject.UnknownExternalOutcome || subject.Status.RequiresReconcile() || subject.ControlStatus == ControlReconcileRequired {
		return newRecoveryDecision(subject, RecoveryReconcile, RecoveryReasonUnknownSideEffect), nil
	}
	if subject.ControlAction == ControlTerminate && !subject.ControlStatus.IsTerminal() {
		return newRecoveryDecision(subject, RecoveryAwaitControlOutcome, RecoveryReasonExplicitTerminate, RecoveryReasonControlInFlight), nil
	}
	if requiresLeaseFact(subject.Status) && !subject.LeaseExpired() {
		return newRecoveryDecision(subject, RecoveryNoAction, RecoveryReasonLeaseActive), nil
	}
	switch subject.Status {
	case StatusPaused:
		if subject.CheckpointRecoverable && subject.InputsUnchanged && subject.DefinitionUnchanged {
			return newRecoveryDecision(subject, RecoveryResumeCheckpoint, RecoveryReasonCheckpointReusable), nil
		}
		reasons := []RecoveryReason{RecoveryReasonCheckpointUnavailable}
		if !subject.InputsUnchanged {
			reasons = append(reasons, RecoveryReasonInputsChanged)
		}
		if !subject.DefinitionUnchanged {
			reasons = append(reasons, RecoveryReasonDefinitionChanged)
		}
		return newRecoveryDecision(subject, RecoveryScheduleNewAttempt, reasons...), nil
	case StatusQueued, StatusRunning, StatusWaiting, StatusPauseRequested, StatusPausing, StatusCancelRequested, StatusStopRequested, StatusCanceling:
		if subject.ControlStatus == ControlRequested || subject.ControlStatus == ControlPropagating {
			return newRecoveryDecision(subject, RecoveryReconcile, RecoveryReasonLeaseExpired, RecoveryReasonControlInFlight), nil
		}
		return newRecoveryDecision(subject, RecoveryMarkInterrupted, RecoveryReasonLeaseExpired, RecoveryReasonInterruptedExecution), nil
	case StatusCanceled:
		return newRecoveryDecision(subject, RecoveryAwaitContinuation, RecoveryReasonCanceledExecution), nil
	case StatusInterrupted:
		return newRecoveryDecision(subject, RecoveryAwaitContinuation, RecoveryReasonInterruptedExecution), nil
	case StatusCompleted, StatusInfraFailed:
		return newRecoveryDecision(subject, RecoveryNoAction, RecoveryReasonTerminalExecution), nil
	default:
		return RecoveryDecision{}, fmt.Errorf("%w: no policy for status %q", ErrInvalidRecovery, subject.Status)
	}
}

// RecoveryReconciler is the generic recovery contract used by lifecycle
// adapters after they load durable job, lease, and control-operation facts.
type RecoveryReconciler interface {
	Reconcile(context.Context, RecoverySubject) (RecoveryDecision, error)
}

// DefaultRecoveryReconciler applies DecideRecovery without domain branches.
type DefaultRecoveryReconciler struct{}

// Reconcile returns the deterministic generic policy result.
func (DefaultRecoveryReconciler) Reconcile(ctx context.Context, subject RecoverySubject) (RecoveryDecision, error) {
	if err := contextError(ctx); err != nil {
		return RecoveryDecision{}, err
	}
	return DecideRecovery(subject)
}
