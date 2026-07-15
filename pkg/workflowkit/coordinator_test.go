package workflowkit

import (
	"errors"
	"reflect"
	"testing"
)

func TestDecideCoordinatorSchedulesWaitsAndCompletesFrozenBatches(t *testing.T) {
	workflow := testWorkflow(t)
	plan, err := CompileDependencyExecutionPlan(workflow)
	if err != nil {
		t.Fatalf("compile plan: %v", err)
	}
	states := coordinatorStates(workflow, map[NodeID]CoordinatorNodeStatus{
		"source":  CoordinatorNodePending,
		"verify":  CoordinatorNodePending,
		"deliver": CoordinatorNodePending,
	})

	decision := mustCoordinatorDecision(t, CoordinatorInput{Workflow: workflow, ScheduleMode: CoordinatorScheduleExecutionPlan, Plan: plan, Nodes: states})
	assertCoordinatorSchedule(t, decision, plan.Batches[0].ID, []NodeID{"source"})

	states = coordinatorStates(workflow, map[NodeID]CoordinatorNodeStatus{
		"source":  CoordinatorNodeRunning,
		"verify":  CoordinatorNodePending,
		"deliver": CoordinatorNodePending,
	})
	decision = mustCoordinatorDecision(t, CoordinatorInput{Workflow: workflow, ScheduleMode: CoordinatorScheduleExecutionPlan, Plan: plan, Nodes: states})
	if decision.Kind != CoordinatorWait || !reflect.DeepEqual(decision.WaitingNodeIDs, []NodeID{"source"}) {
		t.Fatalf("running first batch decision = %#v, want wait for source", decision)
	}

	states = coordinatorStates(workflow, map[NodeID]CoordinatorNodeStatus{
		"source":  CoordinatorNodeSucceeded,
		"verify":  CoordinatorNodePending,
		"deliver": CoordinatorNodePending,
	})
	decision = mustCoordinatorDecision(t, CoordinatorInput{Workflow: workflow, ScheduleMode: CoordinatorScheduleExecutionPlan, Plan: plan, Nodes: states})
	assertCoordinatorSchedule(t, decision, plan.Batches[1].ID, []NodeID{"verify"})

	states = coordinatorStates(workflow, map[NodeID]CoordinatorNodeStatus{
		"source":  CoordinatorNodeSucceeded,
		"verify":  CoordinatorNodeSucceeded,
		"deliver": CoordinatorNodeSucceeded,
	})
	decision = mustCoordinatorDecision(t, CoordinatorInput{Workflow: workflow, ScheduleMode: CoordinatorScheduleExecutionPlan, Plan: plan, Nodes: states})
	if decision.Kind != CoordinatorComplete {
		t.Fatalf("completed workflow decision = %#v, want complete", decision)
	}
}

func TestCoordinatorRequiresExplicitScheduleMode(t *testing.T) {
	workflow := testWorkflow(t)
	plan, err := CompileDependencyExecutionPlan(workflow)
	if err != nil {
		t.Fatalf("compile plan: %v", err)
	}
	states := coordinatorStates(workflow, map[NodeID]CoordinatorNodeStatus{
		"source": CoordinatorNodePending, "verify": CoordinatorNodePending, "deliver": CoordinatorNodePending,
	})
	if _, err := DecideCoordinator(CoordinatorInput{Workflow: workflow, Plan: plan, Nodes: states}); !errors.Is(err, ErrInvalidCoordinatorInput) {
		t.Fatalf("implicit coordinator mode error = %v, want invalid coordinator input", err)
	}
	if _, err := FreezeCoordinatorSchedule(workflow, "initial", "", plan, nil, nil); !errors.Is(err, ErrInvalidJobClaim) {
		t.Fatalf("implicit frozen coordinator mode error = %v, want invalid job claim", err)
	}
	schedule, err := FreezeCoordinatorSchedule(workflow, "initial", CoordinatorScheduleExecutionPlan, plan, nil, nil)
	if err != nil {
		t.Fatalf("freeze explicit coordinator schedule: %v", err)
	}
	if err := schedule.ValidateInput(CoordinatorInput{Workflow: workflow, Plan: plan, Nodes: states}); !errors.Is(err, ErrInvalidJobClaim) {
		t.Fatalf("implicit coordinator input mode against frozen schedule error = %v, want invalid job claim", err)
	}
}

func TestDecideCoordinatorReturnsOnlyUnadmittedIndependentSiblings(t *testing.T) {
	workflow := WorkflowDescriptor{
		ID: "parallel-coordinator", Version: "1",
		Stages: []StageDescriptor{
			testStage("left", nil, EffectEvidenceOnly, nil, []ResourceKey{"evidence/left"}),
			testStage("right", nil, EffectEvidenceOnly, nil, []ResourceKey{"evidence/right"}),
			testStage("join", []StageKey{"left", "right"}, EffectEvidenceOnly, []ResourceKey{"evidence/left", "evidence/right"}, []ResourceKey{"evidence/join"}),
		},
	}
	if err := workflow.Validate(); err != nil {
		t.Fatalf("validate workflow: %v", err)
	}
	plan, err := CompileDependencyExecutionPlan(workflow)
	if err != nil {
		t.Fatalf("compile plan: %v", err)
	}
	decision := mustCoordinatorDecision(t, CoordinatorInput{Workflow: workflow, ScheduleMode: CoordinatorScheduleExecutionPlan, Plan: plan, Nodes: coordinatorStates(workflow, map[NodeID]CoordinatorNodeStatus{
		"left":  CoordinatorNodeQueued,
		"right": CoordinatorNodePending,
		"join":  CoordinatorNodePending,
	})})
	assertCoordinatorSchedule(t, decision, plan.Batches[0].ID, []NodeID{"right"})
}

func TestDecideCoordinatorBlocksInDoubtWithoutDispatchingDependents(t *testing.T) {
	workflow := testWorkflow(t)
	plan, err := CompileDependencyExecutionPlan(workflow)
	if err != nil {
		t.Fatalf("compile plan: %v", err)
	}
	decision := mustCoordinatorDecision(t, CoordinatorInput{Workflow: workflow, ScheduleMode: CoordinatorScheduleExecutionPlan, Plan: plan, Nodes: coordinatorStates(workflow, map[NodeID]CoordinatorNodeStatus{
		"source":  CoordinatorNodeInDoubt,
		"verify":  CoordinatorNodePending,
		"deliver": CoordinatorNodePending,
	})})
	if decision.Kind != CoordinatorBlocked || !reflect.DeepEqual(decision.Blocks, []CoordinatorBlock{{NodeID: "source", Reason: CoordinatorBlockRequiresReconciliation}}) {
		t.Fatalf("in_doubt decision = %#v, want reconciliation block", decision)
	}
}

func TestDecideCoordinatorAppliesFrozenTransitionsWithoutDomainImports(t *testing.T) {
	workflow := testWorkflow(t)
	plan, err := CompileDependencyExecutionPlan(workflow)
	if err != nil {
		t.Fatalf("compile plan: %v", err)
	}
	transitions := []NodeTransition{
		coordinatorTransition(t, "source", DispositionPreserve, 2, 2),
		coordinatorTransition(t, "verify", DispositionSchedule, 2, 3),
		coordinatorTransition(t, "deliver", DispositionInvalidate, 2, 3),
	}
	states := []CoordinatorNodeState{
		{NodeID: "source", Generation: 2, Status: CoordinatorNodePreserved},
		{NodeID: "verify", Generation: 3, Status: CoordinatorNodePending},
		{NodeID: "deliver", Generation: 3, Status: CoordinatorNodeInvalidated},
	}
	input := CoordinatorInput{
		Workflow: workflow, ScheduleMode: CoordinatorScheduleTransitionSubset,
		Schedule: []ScheduleBatch{plan.Batches[1]}, Transitions: transitions, Nodes: states,
	}
	decision := mustCoordinatorDecision(t, input)
	assertCoordinatorSchedule(t, decision, plan.Batches[1].ID, []NodeID{"verify"})

	states[1].Status = CoordinatorNodeSucceeded
	input.Nodes = states
	decision = mustCoordinatorDecision(t, input)
	if decision.Kind != CoordinatorComplete {
		t.Fatalf("transition-complete decision = %#v, want complete", decision)
	}
}

func TestDecideCoordinatorBlocksUnprovenPreservation(t *testing.T) {
	workflow := testWorkflow(t)
	plan, err := CompileDependencyExecutionPlan(workflow)
	if err != nil {
		t.Fatalf("compile plan: %v", err)
	}
	transitions := []NodeTransition{
		coordinatorTransition(t, "source", DispositionPreserve, 1, 1),
		coordinatorTransition(t, "verify", DispositionSchedule, 1, 2),
		coordinatorTransition(t, "deliver", DispositionInvalidate, 1, 2),
	}
	decision := mustCoordinatorDecision(t, CoordinatorInput{
		Workflow: workflow, ScheduleMode: CoordinatorScheduleTransitionSubset,
		Schedule: []ScheduleBatch{plan.Batches[1]}, Transitions: transitions,
		Nodes: []CoordinatorNodeState{
			{NodeID: "source", Generation: 1, Status: CoordinatorNodePending},
			{NodeID: "verify", Generation: 2, Status: CoordinatorNodePending},
			{NodeID: "deliver", Generation: 2, Status: CoordinatorNodeInvalidated},
		},
	})
	if decision.Kind != CoordinatorBlocked || !reflect.DeepEqual(decision.Blocks, []CoordinatorBlock{{NodeID: "source", Reason: CoordinatorBlockPreservationUnproven}}) {
		t.Fatalf("unproven preserve decision = %#v, want preservation block", decision)
	}
}

func TestDecideCoordinatorRejectsIncompleteOrStaleAbstractState(t *testing.T) {
	workflow := testWorkflow(t)
	plan, err := CompileDependencyExecutionPlan(workflow)
	if err != nil {
		t.Fatalf("compile plan: %v", err)
	}
	_, err = DecideCoordinator(CoordinatorInput{Workflow: workflow, ScheduleMode: CoordinatorScheduleExecutionPlan, Plan: plan, Nodes: []CoordinatorNodeState{{NodeID: "source", Generation: 0, Status: CoordinatorNodePending}}})
	if !errors.Is(err, ErrInvalidCoordinatorInput) {
		t.Fatalf("incomplete state error = %v, want invalid coordinator input", err)
	}

	transitions := []NodeTransition{
		coordinatorTransition(t, "source", DispositionSchedule, 0, 2),
		coordinatorTransition(t, "verify", DispositionSchedule, 0, 2),
		coordinatorTransition(t, "deliver", DispositionSchedule, 0, 2),
	}
	states := coordinatorStates(workflow, map[NodeID]CoordinatorNodeStatus{
		"source":  CoordinatorNodePending,
		"verify":  CoordinatorNodePending,
		"deliver": CoordinatorNodePending,
	})
	_, err = DecideCoordinator(CoordinatorInput{Workflow: workflow, ScheduleMode: CoordinatorScheduleExecutionPlan, Plan: plan, Transitions: transitions, Nodes: states})
	if !errors.Is(err, ErrInvalidCoordinatorInput) {
		t.Fatalf("stale generation error = %v, want invalid coordinator input", err)
	}
}

func TestFrozenCoordinatorScheduleBindsInitialAndContinuationTopology(t *testing.T) {
	workflow := testWorkflow(t)
	plan, err := CompileDependencyExecutionPlan(workflow)
	if err != nil {
		t.Fatalf("compile plan: %v", err)
	}
	initial, err := FreezeCoordinatorSchedule(workflow, "initial", CoordinatorScheduleExecutionPlan, plan, nil, nil)
	if err != nil {
		t.Fatalf("freeze initial coordinator schedule: %v", err)
	}
	initialInput := CoordinatorInput{
		Workflow: workflow, ScheduleMode: CoordinatorScheduleExecutionPlan, Plan: plan,
		Nodes: coordinatorStates(workflow, map[NodeID]CoordinatorNodeStatus{
			"source": CoordinatorNodePending, "verify": CoordinatorNodePending, "deliver": CoordinatorNodePending,
		}),
	}
	if err := initial.ValidateInput(initialInput); err != nil {
		t.Fatalf("validate initial coordinator input: %v", err)
	}

	transitions := []NodeTransition{
		coordinatorTransition(t, "source", DispositionPreserve, 2, 2),
		coordinatorTransition(t, "verify", DispositionSchedule, 2, 3),
		coordinatorTransition(t, "deliver", DispositionInvalidate, 2, 3),
	}
	continuation, err := FreezeCoordinatorSchedule(workflow, "continuation-plan-1", CoordinatorScheduleTransitionSubset, ExecutionPlan{}, []ScheduleBatch{plan.Batches[1]}, transitions)
	if err != nil {
		t.Fatalf("freeze continuation coordinator schedule: %v", err)
	}
	input := CoordinatorInput{
		Workflow: workflow, ScheduleMode: CoordinatorScheduleTransitionSubset,
		Schedule: []ScheduleBatch{plan.Batches[1]}, Transitions: transitions,
		Nodes: []CoordinatorNodeState{
			{NodeID: "source", Generation: 2, Status: CoordinatorNodePreserved},
			{NodeID: "verify", Generation: 3, Status: CoordinatorNodePending},
			{NodeID: "deliver", Generation: 3, Status: CoordinatorNodeInvalidated},
		},
	}
	if err := continuation.ValidateInput(input); err != nil {
		t.Fatalf("validate continuation coordinator input: %v", err)
	}
	drifted := input.Clone()
	drifted.Schedule[0].NodeIDs = []NodeID{"source"}
	if err := continuation.ValidateInput(drifted); !errors.Is(err, ErrInvalidJobClaim) {
		t.Fatalf("drifted continuation schedule error = %v, want invalid job claim", err)
	}
}

func coordinatorStates(workflow WorkflowDescriptor, statuses map[NodeID]CoordinatorNodeStatus) []CoordinatorNodeState {
	states := make([]CoordinatorNodeState, 0, len(workflow.Stages))
	for _, stage := range workflow.Stages {
		states = append(states, CoordinatorNodeState{NodeID: stage.Key, Generation: 0, Status: statuses[stage.Key]})
	}
	return states
}

func coordinatorTransition(t *testing.T, nodeID NodeID, disposition NodeDisposition, from, to int) NodeTransition {
	t.Helper()
	fingerprint, err := FingerprintArtifactBindings(nil)
	if err != nil {
		t.Fatalf("empty input fingerprint: %v", err)
	}
	return NodeTransition{
		NodeID: nodeID, FromGeneration: from, ToGeneration: to, Disposition: disposition,
		ReasonCodes: []PlanReason{"test_transition"}, ExpectedInputFingerprint: fingerprint,
	}
}

func mustCoordinatorDecision(t *testing.T, input CoordinatorInput) CoordinatorDecision {
	t.Helper()
	decision, err := DecideCoordinator(input)
	if err != nil {
		t.Fatalf("decide coordinator: %v", err)
	}
	return decision
}

func assertCoordinatorSchedule(t *testing.T, decision CoordinatorDecision, batchID string, nodeIDs []NodeID) {
	t.Helper()
	if decision.Kind != CoordinatorScheduleNextBatch || decision.NextBatch.ID != batchID || !reflect.DeepEqual(decision.NextBatch.NodeIDs, nodeIDs) {
		t.Fatalf("decision = %#v, want schedule %s/%#v", decision, batchID, nodeIDs)
	}
}
