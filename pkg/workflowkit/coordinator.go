package workflowkit

import (
	"errors"
	"fmt"
	"sort"
)

// ErrInvalidCoordinatorInput marks a coordinator snapshot that cannot safely
// drive a durable workflow decision.  A caller must repair or reload an
// invalid snapshot instead of guessing which node is current.
var ErrInvalidCoordinatorInput = errors.New("workflowkit: invalid coordinator input")

// CoordinatorNodeStatus is the domain-neutral projection of one frozen node's
// current execution state.  It deliberately does not carry a Verdict: domain
// adapters decide whether a completed report is acceptable before projecting
// it as succeeded here.
type CoordinatorNodeStatus string

const (
	// CoordinatorNodePending has no admitted durable work for the target
	// generation yet.
	CoordinatorNodePending CoordinatorNodeStatus = "pending"
	// CoordinatorNodeQueued has durable work admitted but not yet started.
	CoordinatorNodeQueued CoordinatorNodeStatus = "queued"
	// CoordinatorNodeRunning has an active executor.
	CoordinatorNodeRunning CoordinatorNodeStatus = "running"
	// CoordinatorNodeWaiting has active work waiting on an approved external
	// condition.  It is not a terminal success.
	CoordinatorNodeWaiting CoordinatorNodeStatus = "waiting"
	// CoordinatorNodeSucceeded proves the target generation completed and may
	// satisfy dependent nodes.
	CoordinatorNodeSucceeded CoordinatorNodeStatus = "succeeded"
	// CoordinatorNodeFailed is a terminal non-success decided by the domain
	// adapter; the generic coordinator must not dispatch downstream work.
	CoordinatorNodeFailed CoordinatorNodeStatus = "failed"
	// CoordinatorNodeBlocked records a durable domain/policy block.
	CoordinatorNodeBlocked CoordinatorNodeStatus = "blocked"
	// CoordinatorNodeInDoubt records a possible external side effect whose
	// outcome still requires a provider-specific reconciliation path.
	CoordinatorNodeInDoubt CoordinatorNodeStatus = "in_doubt"
	// CoordinatorNodeCanceled records a terminal canceled target generation.
	CoordinatorNodeCanceled CoordinatorNodeStatus = "canceled"
	// CoordinatorNodePreserved proves a continuation retained valid immutable
	// output from an earlier generation.
	CoordinatorNodePreserved CoordinatorNodeStatus = "preserved"
	// CoordinatorNodeInvalidated proves a continuation retired this node's
	// prior output instead of allowing it to satisfy a successor.
	CoordinatorNodeInvalidated CoordinatorNodeStatus = "invalidated"
)

func (status CoordinatorNodeStatus) valid() bool {
	switch status {
	case CoordinatorNodePending, CoordinatorNodeQueued, CoordinatorNodeRunning,
		CoordinatorNodeWaiting, CoordinatorNodeSucceeded, CoordinatorNodeFailed,
		CoordinatorNodeBlocked, CoordinatorNodeInDoubt, CoordinatorNodeCanceled,
		CoordinatorNodePreserved, CoordinatorNodeInvalidated:
		return true
	default:
		return false
	}
}

// CoordinatorNodeState is one complete, storage-independent node projection
// for the target generation selected by a frozen execution or continuation.
// Node IDs and generations are compared exactly with the frozen descriptor
// and optional NodeTransition records before any scheduling decision is made.
type CoordinatorNodeState struct {
	NodeID     NodeID                `json:"node_id"`
	Generation int                   `json:"generation"`
	Status     CoordinatorNodeStatus `json:"status"`
}

func (state CoordinatorNodeState) validate() error {
	if err := validateRequired("coordinator node id", string(state.NodeID), ErrInvalidCoordinatorInput); err != nil {
		return err
	}
	if state.Generation < 0 {
		return fmt.Errorf("%w: node %q generation cannot be negative", ErrInvalidCoordinatorInput, state.NodeID)
	}
	if !state.Status.valid() {
		return fmt.Errorf("%w: node %q has unsupported status %q", ErrInvalidCoordinatorInput, state.NodeID, state.Status)
	}
	return nil
}

// CoordinatorDecisionKind is the complete set of generic coordinator actions.
// The decision contains no execution command: a durable backend owns job
// creation, fencing, and provider-specific reconciliation.
type CoordinatorDecisionKind string

const (
	CoordinatorScheduleNextBatch CoordinatorDecisionKind = "schedule_next_batch"
	CoordinatorWait              CoordinatorDecisionKind = "wait"
	CoordinatorComplete          CoordinatorDecisionKind = "complete"
	CoordinatorBlocked           CoordinatorDecisionKind = "blocked"
)

// CoordinatorBlockReason is a stable, domain-neutral explanation for why the
// generic coordinator cannot advance a frozen topology.
type CoordinatorBlockReason string

const (
	CoordinatorBlockFailed                 CoordinatorBlockReason = "node_failed"
	CoordinatorBlockBlocked                CoordinatorBlockReason = "node_blocked"
	CoordinatorBlockRequiresReconciliation CoordinatorBlockReason = "requires_reconciliation"
	CoordinatorBlockCanceled               CoordinatorBlockReason = "node_canceled"
	CoordinatorBlockPreservationUnproven   CoordinatorBlockReason = "preservation_unproven"
	CoordinatorBlockInvalidationUnproven   CoordinatorBlockReason = "invalidation_unproven"
	CoordinatorBlockIncompatibleNodeState  CoordinatorBlockReason = "incompatible_node_state"
	CoordinatorBlockDependencyNotSatisfied CoordinatorBlockReason = "dependency_not_satisfied"
)

// CoordinatorBlock identifies one frozen node that prevents advancement.
// It is deliberately factual rather than a user-facing error string.
type CoordinatorBlock struct {
	NodeID NodeID                 `json:"node_id"`
	Reason CoordinatorBlockReason `json:"reason"`
}

// CoordinatorDecision is the pure result of evaluating a frozen workflow
// descriptor, plan and node-state snapshot.  NextBatch is meaningful only for
// CoordinatorScheduleNextBatch; WaitingNodeIDs only for CoordinatorWait; and
// Blocks only for CoordinatorBlocked.
type CoordinatorDecision struct {
	Kind           CoordinatorDecisionKind `json:"kind"`
	NextBatch      ScheduleBatch           `json:"next_batch,omitempty"`
	WaitingNodeIDs []NodeID                `json:"waiting_node_ids,omitempty"`
	Blocks         []CoordinatorBlock      `json:"blocks,omitempty"`
}

// Clone returns independently owned slice data.
func (decision CoordinatorDecision) Clone() CoordinatorDecision {
	decision.NextBatch = decision.NextBatch.Clone()
	decision.WaitingNodeIDs = append([]NodeID(nil), decision.WaitingNodeIDs...)
	decision.Blocks = append([]CoordinatorBlock(nil), decision.Blocks...)
	return decision
}

// CoordinatorScheduleMode selects which frozen scheduling representation the
// coordinator consumes. Continuations use CoordinatorScheduleTransitionSubset
// because their frozen schedule contains only nodes with DispositionSchedule.
type CoordinatorScheduleMode string

const (
	CoordinatorScheduleExecutionPlan    CoordinatorScheduleMode = "execution_plan"
	CoordinatorScheduleTransitionSubset CoordinatorScheduleMode = "transition_subset"
)

func (mode CoordinatorScheduleMode) valid() bool {
	switch mode {
	case CoordinatorScheduleExecutionPlan, CoordinatorScheduleTransitionSubset:
		return true
	default:
		return false
	}
}

// CoordinatorInput is the entire immutable/persisted view needed for one
// coordinator decision. In CoordinatorScheduleExecutionPlan mode, Plan must
// be the complete frozen ExecutionPlan. Transitions are optional there: when
// absent, every automatically dispatchable stage is implicitly scheduled at
// generation zero and operator-only stages stay outside worker execution.
//
// In CoordinatorScheduleTransitionSubset mode, Schedule is a frozen subset
// that must exactly cover every transition with DispositionSchedule.  It may
// omit preserved, invalidated and operator-only nodes, which is the shape of a
// real ContinuationPlanSnapshot.Schedule.  Transitions must then cover every
// descriptor stage exactly once.  This keeps continuation planning generic
// without importing a domain planner or storage-specific run type.
type CoordinatorInput struct {
	Workflow     WorkflowDescriptor      `json:"workflow"`
	ScheduleMode CoordinatorScheduleMode `json:"schedule_mode,omitempty"`
	Plan         ExecutionPlan           `json:"plan"`
	Schedule     []ScheduleBatch         `json:"schedule,omitempty"`
	Transitions  []NodeTransition        `json:"transitions,omitempty"`
	Nodes        []CoordinatorNodeState  `json:"nodes"`
}

// Clone returns independently owned coordinator input.
func (input CoordinatorInput) Clone() CoordinatorInput {
	input.Workflow = input.Workflow.Clone()
	input.Plan = input.Plan.Clone()
	input.Schedule = cloneCoordinatorScheduleBatches(input.Schedule)
	input.Transitions = cloneCoordinatorTransitions(input.Transitions)
	input.Nodes = append([]CoordinatorNodeState(nil), input.Nodes...)
	return input
}

type coordinatorDirective struct {
	disposition NodeDisposition
	generation  int
}

// DecideCoordinator evaluates only frozen generic data.  It never starts a
// process, reads an artifact store, retries work, or converts a provider
// outcome into a domain verdict.  The caller must persist the returned action
// atomically with its durable job/state mutation before invoking it again.
func DecideCoordinator(input CoordinatorInput) (CoordinatorDecision, error) {
	directives, states, batches, err := validateCoordinatorInput(input)
	if err != nil {
		return CoordinatorDecision{}, err
	}

	blocks := coordinatorStateBlocks(input.Workflow, directives, states)
	if len(blocks) != 0 {
		return blockedCoordinatorDecision(blocks), nil
	}

	for _, frozenBatch := range batches {
		pending := make([]NodeID, 0, len(frozenBatch.NodeIDs))
		waiting := make([]NodeID, 0, len(frozenBatch.NodeIDs))
		for _, nodeID := range frozenBatch.NodeIDs {
			directive := directives[nodeID]
			if directive.disposition != DispositionSchedule {
				continue
			}
			state := states[nodeID]
			switch state.Status {
			case CoordinatorNodeSucceeded:
				continue
			case CoordinatorNodePending:
				if dependencyBlocks := coordinatorDependencyBlocks(input.Workflow, directives, states, nodeID); len(dependencyBlocks) != 0 {
					return blockedCoordinatorDecision(dependencyBlocks), nil
				}
				pending = append(pending, nodeID)
			case CoordinatorNodeQueued, CoordinatorNodeRunning, CoordinatorNodeWaiting:
				waiting = append(waiting, nodeID)
			default:
				// validateCoordinatorInput and coordinatorStateBlocks make this
				// unreachable for a schedule transition.  Keep a fail-closed
				// result if a future status is added without updating this switch.
				return blockedCoordinatorDecision([]CoordinatorBlock{{NodeID: nodeID, Reason: CoordinatorBlockIncompatibleNodeState}}), nil
			}
		}
		if len(pending) != 0 {
			// Do not re-emit siblings already admitted by a prior coordinator
			// delivery.  The returned subset is still one frozen batch and all
			// of its dependencies are proven terminal above.
			return CoordinatorDecision{
				Kind:      CoordinatorScheduleNextBatch,
				NextBatch: ScheduleBatch{ID: frozenBatch.ID, NodeIDs: pending},
			}, nil
		}
		if len(waiting) != 0 {
			return CoordinatorDecision{Kind: CoordinatorWait, WaitingNodeIDs: waiting}, nil
		}
	}

	return CoordinatorDecision{Kind: CoordinatorComplete}, nil
}

func validateCoordinatorInput(input CoordinatorInput) (map[NodeID]coordinatorDirective, map[NodeID]CoordinatorNodeState, []ScheduleBatch, error) {
	if err := input.Workflow.Validate(); err != nil {
		return nil, nil, nil, fmt.Errorf("%w: workflow: %v", ErrInvalidCoordinatorInput, err)
	}
	if !input.ScheduleMode.valid() {
		return nil, nil, nil, fmt.Errorf("%w: unsupported coordinator schedule mode %q", ErrInvalidCoordinatorInput, input.ScheduleMode)
	}
	directives, err := coordinatorDirectives(input.Workflow, input.Transitions)
	if err != nil {
		return nil, nil, nil, err
	}
	batches, err := coordinatorBatches(input, directives)
	if err != nil {
		return nil, nil, nil, err
	}
	states := make(map[NodeID]CoordinatorNodeState, len(input.Nodes))
	if len(input.Nodes) != len(input.Workflow.Stages) {
		return nil, nil, nil, fmt.Errorf("%w: node state coverage is %d, want %d descriptor stages", ErrInvalidCoordinatorInput, len(input.Nodes), len(input.Workflow.Stages))
	}
	for _, state := range input.Nodes {
		if err := state.validate(); err != nil {
			return nil, nil, nil, err
		}
		if _, found := input.Workflow.Stage(StageKey(state.NodeID)); !found {
			return nil, nil, nil, fmt.Errorf("%w: node state refers to unknown stage %q", ErrInvalidCoordinatorInput, state.NodeID)
		}
		if _, duplicate := states[state.NodeID]; duplicate {
			return nil, nil, nil, fmt.Errorf("%w: duplicate node state for %q", ErrInvalidCoordinatorInput, state.NodeID)
		}
		directive := directives[state.NodeID]
		if state.Generation != directive.generation {
			return nil, nil, nil, fmt.Errorf("%w: node %q generation %d does not match frozen target generation %d", ErrInvalidCoordinatorInput, state.NodeID, state.Generation, directive.generation)
		}
		states[state.NodeID] = state
	}
	for _, stage := range input.Workflow.Stages {
		if _, found := states[stage.Key]; !found {
			return nil, nil, nil, fmt.Errorf("%w: missing node state for %q", ErrInvalidCoordinatorInput, stage.Key)
		}
	}
	return directives, states, batches, nil
}

func coordinatorDirectives(workflow WorkflowDescriptor, transitions []NodeTransition) (map[NodeID]coordinatorDirective, error) {
	directives := make(map[NodeID]coordinatorDirective, len(workflow.Stages))
	if len(transitions) == 0 {
		for _, stage := range workflow.Stages {
			disposition := DispositionSchedule
			if stage.OperatorOnly() {
				disposition = DispositionOperatorOnly
			}
			directives[stage.Key] = coordinatorDirective{disposition: disposition, generation: 0}
		}
		return directives, nil
	}

	validated, err := validateTransitionCoverage(transitions, workflow)
	if err != nil {
		return nil, fmt.Errorf("%w: node transitions: %v", ErrInvalidCoordinatorInput, err)
	}
	for nodeID, transition := range validated {
		directives[nodeID] = coordinatorDirective{disposition: transition.Disposition, generation: transition.ToGeneration}
	}
	return directives, nil
}

func coordinatorBatches(input CoordinatorInput, directives map[NodeID]coordinatorDirective) ([]ScheduleBatch, error) {
	switch input.ScheduleMode {
	case CoordinatorScheduleExecutionPlan:
		if len(input.Schedule) != 0 {
			return nil, fmt.Errorf("%w: execution-plan mode cannot also carry a transition schedule", ErrInvalidCoordinatorInput)
		}
		if err := input.Plan.Validate(input.Workflow); err != nil {
			return nil, fmt.Errorf("%w: execution plan: %v", ErrInvalidCoordinatorInput, err)
		}
		batches := make([]ScheduleBatch, len(input.Plan.Batches))
		for index, batch := range input.Plan.Batches {
			batches[index] = batch.Clone()
		}
		if err := validateCoordinatorBatchDependencies(input.Workflow, directives, batches, false); err != nil {
			return nil, err
		}
		return batches, nil
	case CoordinatorScheduleTransitionSubset:
		if len(input.Transitions) == 0 {
			return nil, fmt.Errorf("%w: transition-subset mode requires complete node transitions", ErrInvalidCoordinatorInput)
		}
		if len(input.Plan.Batches) != 0 || input.Plan.Fingerprint != "" {
			return nil, fmt.Errorf("%w: transition-subset mode cannot also carry an execution plan", ErrInvalidCoordinatorInput)
		}
		batches := make([]ScheduleBatch, len(input.Schedule))
		for index, batch := range input.Schedule {
			batches[index] = batch.Clone()
		}
		if err := validateCoordinatorBatchDependencies(input.Workflow, directives, batches, true); err != nil {
			return nil, err
		}
		return batches, nil
	default:
		return nil, fmt.Errorf("%w: unsupported coordinator schedule mode %q", ErrInvalidCoordinatorInput, input.ScheduleMode)
	}
}

// validateCoordinatorBatchDependencies validates either a complete execution
// plan or a continuation's scheduled-node subset.  A preserved dependency is
// valid without a batch; an invalidated dependency can never feed a scheduled
// successor.  The latter is rejected at the frozen-plan boundary rather than
// guessed about at dispatch time.
func validateCoordinatorBatchDependencies(workflow WorkflowDescriptor, directives map[NodeID]coordinatorDirective, batches []ScheduleBatch, subset bool) error {
	batchByNode := make(map[NodeID]int)
	batchIDs := make(map[string]struct{}, len(batches))
	for batchIndex, batch := range batches {
		if err := validateRequired("coordinator schedule batch id", batch.ID, ErrInvalidCoordinatorInput); err != nil {
			return err
		}
		if _, duplicate := batchIDs[batch.ID]; duplicate {
			return fmt.Errorf("%w: duplicate coordinator schedule batch id %q", ErrInvalidCoordinatorInput, batch.ID)
		}
		batchIDs[batch.ID] = struct{}{}
		if len(batch.NodeIDs) == 0 {
			return fmt.Errorf("%w: coordinator schedule batch %q is empty", ErrInvalidCoordinatorInput, batch.ID)
		}
		if err := validateUniqueStrings("coordinator scheduled node id", batch.NodeIDs, ErrInvalidCoordinatorInput); err != nil {
			return err
		}
		for _, nodeID := range batch.NodeIDs {
			stage, found := workflow.Stage(StageKey(nodeID))
			if !found {
				return fmt.Errorf("%w: coordinator schedule refers to unknown stage %q", ErrInvalidCoordinatorInput, nodeID)
			}
			if !stage.AutomaticallyDispatchable() || directives[nodeID].disposition != DispositionSchedule {
				return fmt.Errorf("%w: coordinator schedule may contain only transition-scheduled automatic node %q", ErrInvalidCoordinatorInput, nodeID)
			}
			if _, duplicate := batchByNode[nodeID]; duplicate {
				return fmt.Errorf("%w: coordinator schedule node %q appears in multiple batches", ErrInvalidCoordinatorInput, nodeID)
			}
			batchByNode[nodeID] = batchIndex
		}
		stages := make([]StageDescriptor, 0, len(batch.NodeIDs))
		for _, nodeID := range batch.NodeIDs {
			stage, _ := workflow.Stage(StageKey(nodeID))
			stages = append(stages, stage)
		}
		if err := ValidateConcurrentStages(stages); err != nil {
			return fmt.Errorf("%w: coordinator schedule batch %q: %v", ErrInvalidCoordinatorInput, batch.ID, err)
		}
	}

	for _, stage := range workflow.Stages {
		directive := directives[stage.Key]
		_, scheduled := batchByNode[stage.Key]
		if directive.disposition == DispositionSchedule && !scheduled {
			return fmt.Errorf("%w: transition-scheduled node %q is absent from frozen schedule", ErrInvalidCoordinatorInput, stage.Key)
		}
		if directive.disposition != DispositionSchedule && scheduled {
			return fmt.Errorf("%w: non-scheduled transition node %q appears in frozen schedule", ErrInvalidCoordinatorInput, stage.Key)
		}
		if !subset && stage.AutomaticallyDispatchable() && !scheduled {
			return fmt.Errorf("%w: execution plan omits automatic stage %q", ErrInvalidCoordinatorInput, stage.Key)
		}
	}

	for scheduledNode, batchIndex := range batchByNode {
		stage, _ := workflow.Stage(StageKey(scheduledNode))
		for _, dependency := range stage.Dependencies {
			dependencyDirective := directives[dependency]
			switch dependencyDirective.disposition {
			case DispositionSchedule:
				dependencyBatch, found := batchByNode[dependency]
				if !found || dependencyBatch >= batchIndex {
					return fmt.Errorf("%w: scheduled dependency %q must be in an earlier batch than %q", ErrInvalidCoordinatorInput, dependency, scheduledNode)
				}
			case DispositionPreserve:
				// A preservation has no worker batch, but state validation later
				// proves its immutable output before a dependent is emitted.
			case DispositionInvalidate:
				return fmt.Errorf("%w: scheduled node %q depends on invalidated node %q", ErrInvalidCoordinatorInput, scheduledNode, dependency)
			default:
				return fmt.Errorf("%w: scheduled node %q has unsupported dependency disposition for %q", ErrInvalidCoordinatorInput, scheduledNode, dependency)
			}
		}
	}
	return nil
}

func coordinatorStateBlocks(workflow WorkflowDescriptor, directives map[NodeID]coordinatorDirective, states map[NodeID]CoordinatorNodeState) []CoordinatorBlock {
	blocks := make([]CoordinatorBlock, 0)
	for _, stage := range workflow.Stages {
		if stage.OperatorOnly() {
			continue
		}
		directive := directives[stage.Key]
		state := states[stage.Key]
		switch directive.disposition {
		case DispositionSchedule:
			switch state.Status {
			case CoordinatorNodeFailed:
				blocks = append(blocks, CoordinatorBlock{NodeID: stage.Key, Reason: CoordinatorBlockFailed})
			case CoordinatorNodeBlocked:
				blocks = append(blocks, CoordinatorBlock{NodeID: stage.Key, Reason: CoordinatorBlockBlocked})
			case CoordinatorNodeInDoubt:
				blocks = append(blocks, CoordinatorBlock{NodeID: stage.Key, Reason: CoordinatorBlockRequiresReconciliation})
			case CoordinatorNodeCanceled:
				blocks = append(blocks, CoordinatorBlock{NodeID: stage.Key, Reason: CoordinatorBlockCanceled})
			case CoordinatorNodePreserved, CoordinatorNodeInvalidated:
				blocks = append(blocks, CoordinatorBlock{NodeID: stage.Key, Reason: CoordinatorBlockIncompatibleNodeState})
			}
		case DispositionPreserve:
			if state.Status != CoordinatorNodeSucceeded && state.Status != CoordinatorNodePreserved {
				blocks = append(blocks, CoordinatorBlock{NodeID: stage.Key, Reason: CoordinatorBlockPreservationUnproven})
			}
		case DispositionInvalidate:
			if state.Status != CoordinatorNodeInvalidated {
				blocks = append(blocks, CoordinatorBlock{NodeID: stage.Key, Reason: CoordinatorBlockInvalidationUnproven})
			}
		default:
			blocks = append(blocks, CoordinatorBlock{NodeID: stage.Key, Reason: CoordinatorBlockIncompatibleNodeState})
		}
	}
	return normalizeCoordinatorBlocks(blocks)
}

func coordinatorDependencyBlocks(workflow WorkflowDescriptor, directives map[NodeID]coordinatorDirective, states map[NodeID]CoordinatorNodeState, nodeID NodeID) []CoordinatorBlock {
	stage, found := workflow.Stage(StageKey(nodeID))
	if !found {
		return []CoordinatorBlock{{NodeID: nodeID, Reason: CoordinatorBlockDependencyNotSatisfied}}
	}
	blocks := make([]CoordinatorBlock, 0, len(stage.Dependencies))
	for _, dependency := range stage.Dependencies {
		directive := directives[dependency]
		state := states[dependency]
		satisfied := false
		switch directive.disposition {
		case DispositionSchedule:
			satisfied = state.Status == CoordinatorNodeSucceeded
		case DispositionPreserve:
			satisfied = state.Status == CoordinatorNodeSucceeded || state.Status == CoordinatorNodePreserved
		}
		if !satisfied {
			blocks = append(blocks, CoordinatorBlock{NodeID: dependency, Reason: CoordinatorBlockDependencyNotSatisfied})
		}
	}
	return normalizeCoordinatorBlocks(blocks)
}

func blockedCoordinatorDecision(blocks []CoordinatorBlock) CoordinatorDecision {
	return CoordinatorDecision{Kind: CoordinatorBlocked, Blocks: normalizeCoordinatorBlocks(blocks)}
}

func normalizeCoordinatorBlocks(blocks []CoordinatorBlock) []CoordinatorBlock {
	if len(blocks) == 0 {
		return nil
	}
	unique := make(map[CoordinatorBlock]struct{}, len(blocks))
	for _, block := range blocks {
		unique[block] = struct{}{}
	}
	result := make([]CoordinatorBlock, 0, len(unique))
	for block := range unique {
		result = append(result, block)
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].NodeID == result[right].NodeID {
			return result[left].Reason < result[right].Reason
		}
		return result[left].NodeID < result[right].NodeID
	})
	return result
}
