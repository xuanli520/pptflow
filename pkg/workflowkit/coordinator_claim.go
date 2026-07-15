package workflowkit

import (
	"encoding/json"
	"fmt"
	"reflect"
)

const frozenCoordinatorScheduleFingerprintDomain = "workflowkit.frozen-coordinator-schedule.v1"

// FrozenCoordinatorSchedule is the immutable scheduling contract carried by a
// claimed coordinator.  FrozenExecution.Plan remains the initial full plan;
// this value also represents a continuation's transition subset, so a domain
// adapter cannot hide its real continuation topology behind private runtime
// state.
//
// Node statuses do not belong here. They are current durable facts loaded by
// DurableBackend immediately before Engine makes a decision.
type FrozenCoordinatorSchedule struct {
	ExecutionKey        string                  `json:"execution_key"`
	WorkflowFingerprint Fingerprint             `json:"workflow_fingerprint"`
	Mode                CoordinatorScheduleMode `json:"mode"`
	Plan                ExecutionPlan           `json:"plan,omitempty"`
	Schedule            []ScheduleBatch         `json:"schedule,omitempty"`
	Transitions         []NodeTransition        `json:"transitions,omitempty"`
	Fingerprint         Fingerprint             `json:"fingerprint"`
}

// Clone returns independently owned schedule data.
func (schedule FrozenCoordinatorSchedule) Clone() FrozenCoordinatorSchedule {
	schedule.Plan = schedule.Plan.Clone()
	schedule.Schedule = cloneCoordinatorScheduleBatches(schedule.Schedule)
	schedule.Transitions = cloneCoordinatorTransitions(schedule.Transitions)
	return schedule
}

// FreezeCoordinatorSchedule validates and fingerprints a coordinator's exact
// immutable schedule. Initial executions use CoordinatorScheduleExecutionPlan;
// continuations use CoordinatorScheduleTransitionSubset with complete node
// transitions and a subset schedule.
func FreezeCoordinatorSchedule(workflow WorkflowDescriptor, executionKey string, mode CoordinatorScheduleMode, plan ExecutionPlan, batches []ScheduleBatch, transitions []NodeTransition) (FrozenCoordinatorSchedule, error) {
	schedule := FrozenCoordinatorSchedule{
		ExecutionKey: executionKey, Mode: mode, Plan: plan.Clone(),
		Schedule: cloneCoordinatorScheduleBatches(batches), Transitions: cloneCoordinatorTransitions(transitions),
	}
	workflowFingerprint, err := workflow.Fingerprint()
	if err != nil {
		return FrozenCoordinatorSchedule{}, err
	}
	schedule.WorkflowFingerprint = workflowFingerprint
	if err := schedule.validateStructure(workflow); err != nil {
		return FrozenCoordinatorSchedule{}, err
	}
	fingerprint, err := frozenCoordinatorScheduleFingerprint(schedule)
	if err != nil {
		return FrozenCoordinatorSchedule{}, err
	}
	schedule.Fingerprint = fingerprint
	return schedule, nil
}

// Validate proves a frozen schedule still matches its workflow and canonical
// fingerprint. It does not make a runtime scheduling decision.
func (schedule FrozenCoordinatorSchedule) Validate(workflow WorkflowDescriptor) error {
	if err := schedule.validateStructure(workflow); err != nil {
		return err
	}
	if err := schedule.Fingerprint.Validate(); err != nil {
		return fmt.Errorf("%w: frozen coordinator schedule fingerprint: %v", ErrInvalidJobClaim, err)
	}
	expected, err := frozenCoordinatorScheduleFingerprint(schedule)
	if err != nil {
		return err
	}
	if expected != schedule.Fingerprint {
		return fmt.Errorf("%w: frozen coordinator schedule fingerprint does not match its content", ErrInvalidJobClaim)
	}
	return nil
}

// ValidateInput proves that a just-loaded mutable status snapshot is using
// exactly this claimed schedule. It deliberately compares only scheduling
// facts; node statuses remain free to reflect current durable execution.
func (schedule FrozenCoordinatorSchedule) ValidateInput(input CoordinatorInput) error {
	if err := schedule.Validate(input.Workflow); err != nil {
		return err
	}
	if input.ScheduleMode != schedule.Mode || input.Plan.Fingerprint != schedule.Plan.Fingerprint || !reflect.DeepEqual(input.Schedule, schedule.Schedule) || !reflect.DeepEqual(input.Transitions, schedule.Transitions) {
		return fmt.Errorf("%w: coordinator input differs from its frozen schedule", ErrInvalidJobClaim)
	}
	if _, _, _, err := validateCoordinatorInput(input); err != nil {
		return err
	}
	return nil
}

func (schedule FrozenCoordinatorSchedule) validateStructure(workflow WorkflowDescriptor) error {
	if err := validateRequired("coordinator execution key", schedule.ExecutionKey, ErrInvalidJobClaim); err != nil {
		return err
	}
	if !schedule.Mode.valid() || schedule.Mode == "" {
		return fmt.Errorf("%w: unsupported frozen coordinator schedule mode %q", ErrInvalidJobClaim, schedule.Mode)
	}
	workflowFingerprint, err := workflow.Fingerprint()
	if err != nil {
		return err
	}
	if err := schedule.WorkflowFingerprint.Validate(); err != nil || schedule.WorkflowFingerprint != workflowFingerprint {
		return fmt.Errorf("%w: frozen coordinator schedule does not bind its workflow", ErrInvalidJobClaim)
	}
	input := CoordinatorInput{
		Workflow: workflow.Clone(), ScheduleMode: schedule.Mode, Plan: schedule.Plan.Clone(),
		Schedule: cloneCoordinatorScheduleBatches(schedule.Schedule), Transitions: cloneCoordinatorTransitions(schedule.Transitions),
	}
	if schedule.Mode == CoordinatorScheduleExecutionPlan && len(schedule.Transitions) != 0 {
		return fmt.Errorf("%w: initial frozen coordinator schedule cannot carry continuation transitions", ErrInvalidJobClaim)
	}
	directives, err := coordinatorDirectives(input.Workflow, input.Transitions)
	if err != nil {
		return fmt.Errorf("%w: frozen coordinator directives: %v", ErrInvalidJobClaim, err)
	}
	input.Nodes = make([]CoordinatorNodeState, 0, len(input.Workflow.Stages))
	for _, stage := range input.Workflow.Stages {
		directive := directives[stage.Key]
		status := CoordinatorNodePending
		switch directive.disposition {
		case DispositionPreserve:
			status = CoordinatorNodePreserved
		case DispositionInvalidate:
			status = CoordinatorNodeInvalidated
		case DispositionSchedule, DispositionOperatorOnly:
		default:
			return fmt.Errorf("%w: frozen coordinator schedule has unsupported disposition %q", ErrInvalidJobClaim, directive.disposition)
		}
		input.Nodes = append(input.Nodes, CoordinatorNodeState{NodeID: stage.Key, Generation: directive.generation, Status: status})
	}
	if _, _, _, err := validateCoordinatorInput(input); err != nil {
		return fmt.Errorf("%w: frozen coordinator schedule: %v", ErrInvalidJobClaim, err)
	}
	return nil
}

func frozenCoordinatorScheduleFingerprint(schedule FrozenCoordinatorSchedule) (Fingerprint, error) {
	canonical := struct {
		ExecutionKey        string                  `json:"execution_key"`
		WorkflowFingerprint Fingerprint             `json:"workflow_fingerprint"`
		Mode                CoordinatorScheduleMode `json:"mode"`
		Plan                ExecutionPlan           `json:"plan,omitempty"`
		Schedule            []ScheduleBatch         `json:"schedule,omitempty"`
		Transitions         []NodeTransition        `json:"transitions,omitempty"`
	}{
		ExecutionKey: schedule.ExecutionKey, WorkflowFingerprint: schedule.WorkflowFingerprint, Mode: schedule.Mode,
		Plan: schedule.Plan.Clone(), Schedule: cloneCoordinatorScheduleBatches(schedule.Schedule), Transitions: cloneCoordinatorTransitions(schedule.Transitions),
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("%w: encode frozen coordinator schedule: %v", ErrInvalidJobClaim, err)
	}
	return FingerprintBytes(frozenCoordinatorScheduleFingerprintDomain, encoded)
}

func cloneCoordinatorScheduleBatches(batches []ScheduleBatch) []ScheduleBatch {
	if len(batches) == 0 {
		return nil
	}
	cloned := make([]ScheduleBatch, len(batches))
	for index, batch := range batches {
		cloned[index] = batch.Clone()
	}
	return cloned
}

func cloneCoordinatorTransitions(transitions []NodeTransition) []NodeTransition {
	if len(transitions) == 0 {
		return nil
	}
	cloned := make([]NodeTransition, len(transitions))
	for index, transition := range transitions {
		cloned[index] = transition.Clone()
	}
	return cloned
}

// CoordinatorClaim carries the immutable scheduling contract for a claimed
// coordinator job.
type CoordinatorClaim struct {
	Schedule FrozenCoordinatorSchedule `json:"schedule"`
}

// Clone returns independently owned claim data.
func (claim CoordinatorClaim) Clone() CoordinatorClaim {
	claim.Schedule = claim.Schedule.Clone()
	return claim
}
