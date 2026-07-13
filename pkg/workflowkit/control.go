package workflowkit

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var ErrInvalidControl = errors.New("workflowkit: invalid execution control")

// ControlAction identifies one target-scoped, durable execution intent.
type ControlAction string

const (
	ControlPause       ControlAction = "pause"
	ControlCancelStage ControlAction = "cancel_stage"
	ControlTerminate   ControlAction = "terminate"
)

func (action ControlAction) valid() bool {
	switch action {
	case ControlPause, ControlCancelStage, ControlTerminate:
		return true
	default:
		return false
	}
}

// RequestedExecutionStatus maps a durable control action to the generic
// execution-state transition it requests. A runtime acknowledgement performs
// the later terminal or paused transition.
func RequestedExecutionStatus(action ControlAction) (ExecutionStatus, error) {
	switch action {
	case ControlPause:
		return StatusPauseRequested, nil
	case ControlCancelStage:
		return StatusCancelRequested, nil
	case ControlTerminate:
		return StatusStopRequested, nil
	default:
		return "", fmt.Errorf("%w: unsupported action %q", ErrInvalidControl, action)
	}
}

// ValidateControlExecutionTransition proves that a control request can apply
// to a current execution status without reopening a terminal attempt.
func ValidateControlExecutionTransition(current ExecutionStatus, action ControlAction) error {
	target, err := RequestedExecutionStatus(action)
	if err != nil {
		return err
	}
	if !CanTransitionExecution(current, target) {
		return fmt.Errorf("%w: action %q cannot transition execution %q to %q", ErrInvalidControl, action, current, target)
	}
	return nil
}

// ControlStatus tracks propagation and acknowledgement separately from the
// execution state. An acknowledged operation is a durable fact, not merely an
// optimistic user-interface notification.
type ControlStatus string

const (
	ControlRequested         ControlStatus = "requested"
	ControlPropagating       ControlStatus = "propagating"
	ControlAcknowledged      ControlStatus = "acknowledged"
	ControlReconcileRequired ControlStatus = "reconcile_required"
	ControlFailed            ControlStatus = "failed"
)

func (status ControlStatus) valid() bool {
	switch status {
	case ControlRequested, ControlPropagating, ControlAcknowledged, ControlReconcileRequired, ControlFailed:
		return true
	default:
		return false
	}
}

// IsTerminal reports whether no further transition may mutate a control
// operation. Reconciliation itself is represented by the transition from
// reconcile_required to acknowledged or failed.
func (status ControlStatus) IsTerminal() bool {
	return status == ControlAcknowledged || status == ControlFailed
}

// CanTransitionControl validates the generic durable control state machine.
func CanTransitionControl(from, to ControlStatus) bool {
	if !from.valid() || !to.valid() || from == to || from.IsTerminal() {
		return false
	}
	switch from {
	case ControlRequested:
		return to == ControlPropagating || to == ControlAcknowledged || to == ControlReconcileRequired || to == ControlFailed
	case ControlPropagating:
		return to == ControlAcknowledged || to == ControlReconcileRequired || to == ControlFailed
	case ControlReconcileRequired:
		return to == ControlAcknowledged || to == ControlFailed
	default:
		return false
	}
}

// ValidateControlTransition returns an explicit error for an illegal durable
// control state transition.
func ValidateControlTransition(from, to ControlStatus) error {
	if CanTransitionControl(from, to) {
		return nil
	}
	return fmt.Errorf("%w: control transition %q -> %q is not allowed", ErrInvalidControl, from, to)
}

// ExecutionControlCommand records the intent before any runtime context is
// signaled. RunID is generic execution identity; stage cancellation has a
// narrower StageAttemptID target.
type ExecutionControlCommand struct {
	OperationKey   string        `json:"operation_key"`
	Action         ControlAction `json:"action"`
	RunID          string        `json:"run_id"`
	StageAttemptID string        `json:"stage_attempt_id,omitempty"`
	Expected       CheckpointRef `json:"expected"`
	Actor          string        `json:"actor"`
	Reason         string        `json:"reason"`
	GracePeriod    time.Duration `json:"grace_period"`
}

func (command ExecutionControlCommand) validate() error {
	if err := validateRequired("control operation key", command.OperationKey, ErrInvalidControl); err != nil {
		return err
	}
	if !command.Action.valid() {
		return fmt.Errorf("%w: unsupported control action %q", ErrInvalidControl, command.Action)
	}
	if err := validateRequired("control run id", command.RunID, ErrInvalidControl); err != nil {
		return err
	}
	if command.Action == ControlCancelStage {
		if err := validateRequired("control stage attempt id", command.StageAttemptID, ErrInvalidControl); err != nil {
			return err
		}
	} else if command.StageAttemptID != "" {
		return fmt.Errorf("%w: action %q cannot include a stage attempt id", ErrInvalidControl, command.Action)
	}
	if err := command.Expected.validate(); err != nil {
		return fmt.Errorf("%w: expected checkpoint: %v", ErrInvalidControl, err)
	}
	if err := validateRequired("control actor", command.Actor, ErrInvalidControl); err != nil {
		return err
	}
	if err := validateRequired("control reason", command.Reason, ErrInvalidControl); err != nil {
		return err
	}
	if command.GracePeriod < 0 {
		return fmt.Errorf("%w: control grace period cannot be negative", ErrInvalidControl)
	}
	return nil
}

// RuntimeTerminationReceipt is one immutable runtime fact observed while
// carrying out a control action. Domain-specific adapters may store opaque
// evidence elsewhere and bind its identity through ReceiptID.
type RuntimeTerminationReceipt struct {
	ReceiptID              string    `json:"receipt_id"`
	RuntimeScopeID         string    `json:"runtime_scope_id"`
	ObservedAt             time.Time `json:"observed_at"`
	Graceful               bool      `json:"graceful"`
	ExternalOutcomeUnknown bool      `json:"external_outcome_unknown"`
}

func (receipt RuntimeTerminationReceipt) validate() error {
	if err := validateRequired("runtime receipt id", receipt.ReceiptID, ErrInvalidControl); err != nil {
		return err
	}
	if err := validateRequired("runtime receipt scope id", receipt.RuntimeScopeID, ErrInvalidControl); err != nil {
		return err
	}
	if receipt.ObservedAt.IsZero() {
		return fmt.Errorf("%w: runtime receipt observation time is required", ErrInvalidControl)
	}
	return nil
}

// ControlOperation is the durable projection of one command and its runtime
// acknowledgements. Version protects state changes from stale writers.
type ControlOperation struct {
	OperationID       string                      `json:"operation_id"`
	OperationKey      string                      `json:"operation_key"`
	Action            ControlAction               `json:"action"`
	RunID             string                      `json:"run_id"`
	StageAttemptID    string                      `json:"stage_attempt_id,omitempty"`
	Expected          CheckpointRef               `json:"expected"`
	Actor             string                      `json:"actor"`
	Reason            string                      `json:"reason"`
	GracePeriod       time.Duration               `json:"grace_period"`
	Status            ControlStatus               `json:"status"`
	RuntimeReceipts   []RuntimeTerminationReceipt `json:"runtime_receipts,omitempty"`
	CheckpointID      string                      `json:"checkpoint_id,omitempty"`
	QuotaSettlementID string                      `json:"quota_settlement_id,omitempty"`
	FailureReason     string                      `json:"failure_reason,omitempty"`
	Version           int64                       `json:"version"`
	CreatedAt         time.Time                   `json:"created_at"`
	UpdatedAt         time.Time                   `json:"updated_at"`
}

// Clone returns an independent operation snapshot.
func (operation ControlOperation) Clone() ControlOperation {
	operation.RuntimeReceipts = append([]RuntimeTerminationReceipt(nil), operation.RuntimeReceipts...)
	return operation
}

// ControlTransition appends a state fact through an optimistic operation
// version. TransitionID makes duplicate runtime acknowledgements harmless.
type ControlTransition struct {
	TransitionID      string                      `json:"transition_id"`
	OperationID       string                      `json:"operation_id"`
	ExpectedVersion   int64                       `json:"expected_version"`
	Status            ControlStatus               `json:"status"`
	RuntimeReceipts   []RuntimeTerminationReceipt `json:"runtime_receipts,omitempty"`
	CheckpointID      string                      `json:"checkpoint_id,omitempty"`
	QuotaSettlementID string                      `json:"quota_settlement_id,omitempty"`
	FailureReason     string                      `json:"failure_reason,omitempty"`
}

// Clone returns an independent transition snapshot.
func (transition ControlTransition) Clone() ControlTransition {
	transition.RuntimeReceipts = append([]RuntimeTerminationReceipt(nil), transition.RuntimeReceipts...)
	return transition
}

func (transition ControlTransition) validate() error {
	if err := validateRequired("control transition id", transition.TransitionID, ErrInvalidControl); err != nil {
		return err
	}
	if err := validateRequired("control transition operation id", transition.OperationID, ErrInvalidControl); err != nil {
		return err
	}
	if transition.ExpectedVersion <= 0 {
		return fmt.Errorf("%w: control transition expected version must be positive", ErrInvalidControl)
	}
	if !transition.Status.valid() || transition.Status == ControlRequested {
		return fmt.Errorf("%w: unsupported target control status %q", ErrInvalidControl, transition.Status)
	}
	if transition.Status == ControlFailed && transition.FailureReason == "" {
		return fmt.Errorf("%w: failed control transition needs a failure reason", ErrInvalidControl)
	}
	if transition.Status != ControlFailed && transition.FailureReason != "" {
		return fmt.Errorf("%w: only failed control transitions may carry a failure reason", ErrInvalidControl)
	}
	seen := make(map[string]struct{}, len(transition.RuntimeReceipts))
	for _, receipt := range transition.RuntimeReceipts {
		if err := receipt.validate(); err != nil {
			return err
		}
		if _, duplicate := seen[receipt.ReceiptID]; duplicate {
			return fmt.Errorf("%w: duplicate runtime receipt %q", ErrInvalidControl, receipt.ReceiptID)
		}
		seen[receipt.ReceiptID] = struct{}{}
	}
	return nil
}

// ExecutionControl is the generic persistence contract used by UI, worker,
// and runtime adapters. It deliberately never cancels a shared root context.
type ExecutionControl interface {
	RequestControl(context.Context, ExecutionControlCommand) (ControlOperation, error)
	TransitionControl(context.Context, ControlTransition) (ControlOperation, error)
	GetControlOperation(context.Context, string) (ControlOperation, bool, error)
}

type controlTransitionRecord struct {
	transition ControlTransition
	operation  ControlOperation
}

// InMemoryExecutionControl is a deterministic, optimistic in-memory control
// store suitable for generic workflow tests.
type InMemoryExecutionControl struct {
	mu sync.Mutex

	clock Clock

	byOperationID  map[string]ControlOperation
	byOperationKey map[string]string
	transitions    map[string]controlTransitionRecord
}

// NewInMemoryExecutionControl creates an empty durable-style control store.
func NewInMemoryExecutionControl(clock Clock) *InMemoryExecutionControl {
	return &InMemoryExecutionControl{
		clock:          resolveClock(clock),
		byOperationID:  make(map[string]ControlOperation),
		byOperationKey: make(map[string]string),
		transitions:    make(map[string]controlTransitionRecord),
	}
}

// RequestControl persists an idempotent command before any propagation.
func (store *InMemoryExecutionControl) RequestControl(ctx context.Context, command ExecutionControlCommand) (ControlOperation, error) {
	if err := contextError(ctx); err != nil {
		return ControlOperation{}, err
	}
	if err := command.validate(); err != nil {
		return ControlOperation{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if operationID, exists := store.byOperationKey[command.OperationKey]; exists {
		operation := store.byOperationID[operationID]
		if !sameControlCommand(operation, command) {
			return ControlOperation{}, fmt.Errorf("%w: control operation key %q", ErrIdempotencyConflict, command.OperationKey)
		}
		return operation.Clone(), nil
	}
	operationID := "control:" + command.OperationKey
	if _, exists := store.byOperationID[operationID]; exists {
		return ControlOperation{}, fmt.Errorf("%w: control operation id %q", ErrIdempotencyConflict, operationID)
	}
	now := store.now()
	operation := ControlOperation{
		OperationID:    operationID,
		OperationKey:   command.OperationKey,
		Action:         command.Action,
		RunID:          command.RunID,
		StageAttemptID: command.StageAttemptID,
		Expected:       command.Expected,
		Actor:          command.Actor,
		Reason:         command.Reason,
		GracePeriod:    command.GracePeriod,
		Status:         ControlRequested,
		Version:        1,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	store.byOperationID[operationID] = operation
	store.byOperationKey[command.OperationKey] = operationID
	return operation.Clone(), nil
}

// TransitionControl applies an idempotent optimistic state update and retains
// every unique runtime receipt supplied by the runtime adapter.
func (store *InMemoryExecutionControl) TransitionControl(ctx context.Context, transition ControlTransition) (ControlOperation, error) {
	if err := contextError(ctx); err != nil {
		return ControlOperation{}, err
	}
	if err := transition.validate(); err != nil {
		return ControlOperation{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if record, exists := store.transitions[transition.TransitionID]; exists {
		if !sameControlTransition(record.transition, transition) {
			return ControlOperation{}, fmt.Errorf("%w: control transition id %q", ErrIdempotencyConflict, transition.TransitionID)
		}
		return record.operation.Clone(), nil
	}
	operation, exists := store.byOperationID[transition.OperationID]
	if !exists {
		return ControlOperation{}, fmt.Errorf("%w: control operation %q does not exist", ErrInvalidControl, transition.OperationID)
	}
	if operation.Version != transition.ExpectedVersion {
		return ControlOperation{}, fmt.Errorf("%w: expected control version %d, current %d", ErrInvalidControl, transition.ExpectedVersion, operation.Version)
	}
	if err := ValidateControlTransition(operation.Status, transition.Status); err != nil {
		return ControlOperation{}, err
	}
	if err := mergeControlTransition(&operation, transition); err != nil {
		return ControlOperation{}, err
	}
	operation.Status = transition.Status
	operation.Version++
	operation.UpdatedAt = store.now()
	store.byOperationID[operation.OperationID] = operation
	store.transitions[transition.TransitionID] = controlTransitionRecord{transition: transition.Clone(), operation: operation.Clone()}
	return operation.Clone(), nil
}

// GetControlOperation returns a copied durable operation projection.
func (store *InMemoryExecutionControl) GetControlOperation(ctx context.Context, operationID string) (ControlOperation, bool, error) {
	if err := contextError(ctx); err != nil {
		return ControlOperation{}, false, err
	}
	if err := validateRequired("control operation id", operationID, ErrInvalidControl); err != nil {
		return ControlOperation{}, false, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	operation, exists := store.byOperationID[operationID]
	if !exists {
		return ControlOperation{}, false, nil
	}
	return operation.Clone(), true, nil
}

func mergeControlTransition(operation *ControlOperation, transition ControlTransition) error {
	receipts := make(map[string]RuntimeTerminationReceipt, len(operation.RuntimeReceipts)+len(transition.RuntimeReceipts))
	for _, receipt := range operation.RuntimeReceipts {
		receipts[receipt.ReceiptID] = receipt
	}
	for _, receipt := range transition.RuntimeReceipts {
		if existing, duplicate := receipts[receipt.ReceiptID]; duplicate {
			if existing != receipt {
				return fmt.Errorf("%w: runtime receipt %q conflicts with prior fact", ErrInvalidControl, receipt.ReceiptID)
			}
			continue
		}
		operation.RuntimeReceipts = append(operation.RuntimeReceipts, receipt)
		receipts[receipt.ReceiptID] = receipt
	}
	if err := mergeControlFact(&operation.CheckpointID, transition.CheckpointID, "checkpoint id"); err != nil {
		return err
	}
	if err := mergeControlFact(&operation.QuotaSettlementID, transition.QuotaSettlementID, "quota settlement id"); err != nil {
		return err
	}
	if transition.Status == ControlFailed {
		operation.FailureReason = transition.FailureReason
	}
	return nil
}

func mergeControlFact(current *string, incoming, label string) error {
	if incoming == "" {
		return nil
	}
	if *current != "" && *current != incoming {
		return fmt.Errorf("%w: control %s cannot change", ErrInvalidControl, label)
	}
	*current = incoming
	return nil
}

func sameControlCommand(operation ControlOperation, command ExecutionControlCommand) bool {
	return operation.OperationKey == command.OperationKey &&
		operation.Action == command.Action &&
		operation.RunID == command.RunID &&
		operation.StageAttemptID == command.StageAttemptID &&
		operation.Expected == command.Expected &&
		operation.Actor == command.Actor &&
		operation.Reason == command.Reason &&
		operation.GracePeriod == command.GracePeriod
}

func sameControlTransition(left, right ControlTransition) bool {
	if left.TransitionID != right.TransitionID || left.OperationID != right.OperationID || left.ExpectedVersion != right.ExpectedVersion || left.Status != right.Status || left.CheckpointID != right.CheckpointID || left.QuotaSettlementID != right.QuotaSettlementID || left.FailureReason != right.FailureReason || len(left.RuntimeReceipts) != len(right.RuntimeReceipts) {
		return false
	}
	for index := range left.RuntimeReceipts {
		if left.RuntimeReceipts[index] != right.RuntimeReceipts[index] {
			return false
		}
	}
	return true
}

func (store *InMemoryExecutionControl) now() time.Time { return store.clock.Now().UTC() }
