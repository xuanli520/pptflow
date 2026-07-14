package workflowkit

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// CheckpointRef is the immutable compare-and-swap basis for a continuation.
// It binds a plan to one subject revision and one frozen workflow definition.
type CheckpointRef struct {
	Sequence            uint64        `json:"sequence"`
	ExecutionEpoch      int           `json:"execution_epoch"`
	SubjectVersion      int64         `json:"subject_version"`
	SubjectID           string        `json:"subject_id"`
	SubjectRevisionID   string        `json:"subject_revision_id"`
	SubjectDigest       SubjectDigest `json:"subject_digest"`
	WorkflowFingerprint Fingerprint   `json:"workflow_fingerprint"`
}

func (checkpoint CheckpointRef) validate() error {
	if checkpoint.ExecutionEpoch < 0 {
		return fmt.Errorf("%w: checkpoint execution epoch cannot be negative", ErrInvalidContinuationPlan)
	}
	if checkpoint.SubjectVersion < 0 {
		return fmt.Errorf("%w: checkpoint subject version cannot be negative", ErrInvalidContinuationPlan)
	}
	if err := validateRequired("checkpoint subject id", checkpoint.SubjectID, ErrInvalidContinuationPlan); err != nil {
		return err
	}
	if err := validateRequired("checkpoint subject revision id", checkpoint.SubjectRevisionID, ErrInvalidContinuationPlan); err != nil {
		return err
	}
	if err := checkpoint.SubjectDigest.Validate(); err != nil {
		return err
	}
	if err := checkpoint.WorkflowFingerprint.Validate(); err != nil {
		return err
	}
	return nil
}

// ContinuationStrategy is planner explanation and audit classification only.
// The execution runtime consumes frozen node transitions, never a mode switch.
type ContinuationStrategy string

const (
	StrategyRetryAttempt  ContinuationStrategy = "retry_attempt"
	StrategyRecompute     ContinuationStrategy = "recompute"
	StrategyReviseSubject ContinuationStrategy = "revise_subject"
)

func (strategy ContinuationStrategy) valid() bool {
	switch strategy {
	case StrategyRetryAttempt, StrategyRecompute, StrategyReviseSubject:
		return true
	default:
		return false
	}
}

// RunRelation declares whether execution continues inside the same run attempt
// or must create a child run due to a changed subject or frozen definition.
type RunRelation string

const (
	RelationSameRunAttempt RunRelation = "same_run_attempt"
	RelationChildRun       RunRelation = "child_run"
)

func (relation RunRelation) valid() bool {
	return relation == RelationSameRunAttempt || relation == RelationChildRun
}

// NodeDisposition assigns exactly one executable fate to each compiled stage.
type NodeDisposition string

const (
	DispositionPreserve   NodeDisposition = "preserve"
	DispositionSchedule   NodeDisposition = "schedule"
	DispositionInvalidate NodeDisposition = "invalidate"
	// DispositionOperatorOnly records a frozen stage that remains visible in a
	// continuation's complete topology but is exclusively owned by a separate
	// domain lifecycle operation. It can never be placed in a worker schedule.
	DispositionOperatorOnly NodeDisposition = "operator_only"
)

func (disposition NodeDisposition) valid() bool {
	switch disposition {
	case DispositionPreserve, DispositionSchedule, DispositionInvalidate, DispositionOperatorOnly:
		return true
	default:
		return false
	}
}

// PlanReason is a stable explanatory code selected by a planner or domain
// adapter. Textual user-facing explanation belongs outside the kernel.
type PlanReason string

// NodeTransition is a fully expanded transition for one compiled stage. It
// intentionally lacks selectors, restart-from fields, or an untyped mode.
type NodeTransition struct {
	NodeID                   NodeID            `json:"node_id"`
	FromGeneration           int               `json:"from_generation"`
	ToGeneration             int               `json:"to_generation"`
	Disposition              NodeDisposition   `json:"disposition"`
	ReasonCodes              []PlanReason      `json:"reason_codes"`
	ExpectedInputFingerprint Fingerprint       `json:"expected_input_fingerprint"`
	InputBindings            []ArtifactBinding `json:"input_bindings"`
}

// Clone returns an independent transition copy.
func (transition NodeTransition) Clone() NodeTransition {
	transition.ReasonCodes = append([]PlanReason(nil), transition.ReasonCodes...)
	transition.InputBindings = append([]ArtifactBinding(nil), transition.InputBindings...)
	return transition
}

func (transition NodeTransition) validate() error {
	if err := validateRequired("transition node id", string(transition.NodeID), ErrInvalidContinuationPlan); err != nil {
		return err
	}
	if transition.FromGeneration < 0 || transition.ToGeneration < 0 {
		return fmt.Errorf("%w: transition generations cannot be negative", ErrInvalidContinuationPlan)
	}
	if transition.ToGeneration < transition.FromGeneration {
		return fmt.Errorf("%w: transition generation cannot decrease for node %q", ErrInvalidContinuationPlan, transition.NodeID)
	}
	if !transition.Disposition.valid() {
		return fmt.Errorf("%w: unsupported node disposition %q", ErrInvalidContinuationPlan, transition.Disposition)
	}
	if (transition.Disposition == DispositionPreserve || transition.Disposition == DispositionOperatorOnly) && transition.ToGeneration != transition.FromGeneration {
		return fmt.Errorf("%w: non-scheduled node %q cannot change generation", ErrInvalidContinuationPlan, transition.NodeID)
	}
	if len(transition.ReasonCodes) == 0 {
		return fmt.Errorf("%w: transition node %q needs an explanatory reason", ErrInvalidContinuationPlan, transition.NodeID)
	}
	if err := validateUniqueStrings("transition reason code", transition.ReasonCodes, ErrInvalidContinuationPlan); err != nil {
		return err
	}
	if err := transition.ExpectedInputFingerprint.Validate(); err != nil {
		return err
	}
	expected, err := FingerprintArtifactBindings(transition.InputBindings)
	if err != nil {
		return err
	}
	if expected != transition.ExpectedInputFingerprint {
		return fmt.Errorf("%w: transition node %q input fingerprint does not match bindings", ErrInvalidContinuationPlan, transition.NodeID)
	}
	return nil
}

// ScheduleBatch contains stages that may be dispatched concurrently. Batches
// are ordered, and dependencies may only point to an earlier batch or a
// preserved predecessor.
type ScheduleBatch struct {
	ID      string   `json:"id"`
	NodeIDs []NodeID `json:"node_ids"`
}

// Clone returns an independent batch copy.
func (batch ScheduleBatch) Clone() ScheduleBatch {
	batch.NodeIDs = append([]NodeID(nil), batch.NodeIDs...)
	return batch
}

// PlanAssertionKind identifies a non-domain-specific assertion that an
// execution service must enforce while committing a frozen plan.
type PlanAssertionKind string

const (
	AssertionCheckpointCurrent    PlanAssertionKind = "checkpoint_current"
	AssertionArtifactBindingAlive PlanAssertionKind = "artifact_binding_active"
	AssertionQuotaAdmitted        PlanAssertionKind = "quota_admitted"
)

func (kind PlanAssertionKind) valid() bool {
	switch kind {
	case AssertionCheckpointCurrent, AssertionArtifactBindingAlive, AssertionQuotaAdmitted:
		return true
	default:
		return false
	}
}

// PlanAssertion is a typed execution precondition. Domain-specific assertions
// should be represented by a frozen domain artifact bound in the plan.
type PlanAssertion struct {
	Kind     PlanAssertionKind `json:"kind"`
	Subject  string            `json:"subject"`
	Expected Fingerprint       `json:"expected"`
}

func (assertion PlanAssertion) validate() error {
	if !assertion.Kind.valid() {
		return fmt.Errorf("%w: unsupported plan assertion %q", ErrInvalidContinuationPlan, assertion.Kind)
	}
	if err := validateRequired("plan assertion subject", assertion.Subject, ErrInvalidContinuationPlan); err != nil {
		return err
	}
	if err := assertion.Expected.Validate(); err != nil {
		return err
	}
	return nil
}

// ExternalEffectConfirmation records explicit authorization to schedule a
// stage that can repeat a non-idempotent external effect. The idempotency key
// is immutable plan data rather than a runtime default.
type ExternalEffectConfirmation struct {
	NodeID         NodeID    `json:"node_id"`
	IdempotencyKey string    `json:"idempotency_key"`
	Actor          string    `json:"actor"`
	ConfirmedAt    time.Time `json:"confirmed_at"`
}

func (confirmation ExternalEffectConfirmation) validate() error {
	if err := validateRequired("external effect node id", string(confirmation.NodeID), ErrInvalidContinuationPlan); err != nil {
		return err
	}
	if err := validateRequired("external effect idempotency key", confirmation.IdempotencyKey, ErrInvalidContinuationPlan); err != nil {
		return err
	}
	if err := validateRequired("external effect actor", confirmation.Actor, ErrInvalidContinuationPlan); err != nil {
		return err
	}
	if confirmation.ConfirmedAt.IsZero() {
		return fmt.Errorf("%w: external effect confirmation time is required", ErrInvalidContinuationPlan)
	}
	return nil
}

// ContinuationPlanSnapshot is the serializable representation of a frozen
// plan. Construct a ContinuationPlan through FreezeContinuationPlan instead
// of executing a mutable snapshot directly.
type ContinuationPlanSnapshot struct {
	PlanID                      string                       `json:"plan_id"`
	CommandID                   string                       `json:"command_id"`
	Strategy                    ContinuationStrategy         `json:"strategy"`
	BaseCheckpoint              CheckpointRef                `json:"base_checkpoint"`
	NextExecutionEpoch          int                          `json:"next_execution_epoch"`
	SourceRunID                 string                       `json:"source_run_id"`
	TargetRunRelation           RunRelation                  `json:"target_run_relation"`
	PreparedChangeID            string                       `json:"prepared_change_id,omitempty"`
	SubjectRevisionID           string                       `json:"subject_revision_id"`
	SubjectDigest               SubjectDigest                `json:"subject_digest"`
	CandidateRevisionID         string                       `json:"candidate_revision_id,omitempty"`
	Nodes                       []NodeTransition             `json:"nodes"`
	RetireArtifacts             []ArtifactID                 `json:"retire_artifacts,omitempty"`
	Schedule                    []ScheduleBatch              `json:"schedule"`
	Assertions                  []PlanAssertion              `json:"assertions,omitempty"`
	ExternalEffectConfirmations []ExternalEffectConfirmation `json:"external_effect_confirmations,omitempty"`
	ExpiresAt                   time.Time                    `json:"expires_at"`
}

// Clone returns an independent snapshot copy.
func (snapshot ContinuationPlanSnapshot) Clone() ContinuationPlanSnapshot {
	nodes := snapshot.Nodes
	snapshot.Nodes = make([]NodeTransition, len(nodes))
	for index, transition := range nodes {
		snapshot.Nodes[index] = transition.Clone()
	}
	snapshot.RetireArtifacts = append([]ArtifactID(nil), snapshot.RetireArtifacts...)
	schedule := snapshot.Schedule
	snapshot.Schedule = make([]ScheduleBatch, len(schedule))
	for index, batch := range schedule {
		snapshot.Schedule[index] = batch.Clone()
	}
	snapshot.Assertions = append([]PlanAssertion(nil), snapshot.Assertions...)
	snapshot.ExternalEffectConfirmations = append([]ExternalEffectConfirmation(nil), snapshot.ExternalEffectConfirmations...)
	return snapshot
}

// ContinuationPlan holds a validated, private snapshot. It provides value
// semantics: callers receive copies and cannot mutate a plan after it has been
// frozen and fingerprinted.
type ContinuationPlan struct {
	snapshot    ContinuationPlanSnapshot
	fingerprint Fingerprint
}

// FreezeContinuationPlan validates and deep-copies a fully expanded plan for
// one frozen workflow descriptor.
func FreezeContinuationPlan(snapshot ContinuationPlanSnapshot, workflow WorkflowDescriptor) (ContinuationPlan, error) {
	if err := validateContinuationPlan(snapshot, workflow); err != nil {
		return ContinuationPlan{}, err
	}
	copySnapshot := snapshot.Clone()
	fingerprint, err := fingerprintContinuationPlan(copySnapshot)
	if err != nil {
		return ContinuationPlan{}, err
	}
	return ContinuationPlan{snapshot: copySnapshot, fingerprint: fingerprint}, nil
}

// Snapshot returns a deep-copy serializable representation.
func (plan ContinuationPlan) Snapshot() ContinuationPlanSnapshot {
	return plan.snapshot.Clone()
}

// ID returns the durable plan identity.
func (plan ContinuationPlan) ID() string { return plan.snapshot.PlanID }

// Fingerprint returns the canonical fingerprint of the frozen plan.
func (plan ContinuationPlan) Fingerprint() Fingerprint { return plan.fingerprint }

// IsExpired reports whether a plan may no longer be executed at now.
func (plan ContinuationPlan) IsExpired(now time.Time) bool {
	return !plan.snapshot.ExpiresAt.After(now)
}

// Validate proves the plan still matches its private fingerprint and the given
// workflow descriptor. It is useful after reading a persisted plan snapshot.
func (plan ContinuationPlan) Validate(workflow WorkflowDescriptor) error {
	if plan.fingerprint == "" {
		return fmt.Errorf("%w: plan has not been frozen", ErrInvalidContinuationPlan)
	}
	if err := validateContinuationPlan(plan.snapshot, workflow); err != nil {
		return err
	}
	actual, err := fingerprintContinuationPlan(plan.snapshot)
	if err != nil {
		return err
	}
	if actual != plan.fingerprint {
		return fmt.Errorf("%w: frozen plan fingerprint does not match its snapshot", ErrInvalidContinuationPlan)
	}
	return nil
}

func validateContinuationPlan(snapshot ContinuationPlanSnapshot, workflow WorkflowDescriptor) error {
	if err := workflow.Validate(); err != nil {
		return fmt.Errorf("%w: workflow: %v", ErrInvalidContinuationPlan, err)
	}
	workflowFingerprint, err := workflow.Fingerprint()
	if err != nil {
		return err
	}
	if err := validateRequired("plan id", snapshot.PlanID, ErrInvalidContinuationPlan); err != nil {
		return err
	}
	if err := validateRequired("plan command id", snapshot.CommandID, ErrInvalidContinuationPlan); err != nil {
		return err
	}
	if !snapshot.Strategy.valid() {
		return fmt.Errorf("%w: unsupported continuation strategy %q", ErrInvalidContinuationPlan, snapshot.Strategy)
	}
	if err := snapshot.BaseCheckpoint.validate(); err != nil {
		return err
	}
	if snapshot.BaseCheckpoint.WorkflowFingerprint != workflowFingerprint {
		return fmt.Errorf("%w: plan checkpoint does not bind the supplied frozen workflow", ErrInvalidContinuationPlan)
	}
	if snapshot.NextExecutionEpoch <= snapshot.BaseCheckpoint.ExecutionEpoch {
		return fmt.Errorf("%w: next execution epoch must advance the base checkpoint", ErrInvalidContinuationPlan)
	}
	if err := validateRequired("plan source run id", snapshot.SourceRunID, ErrInvalidContinuationPlan); err != nil {
		return err
	}
	if !snapshot.TargetRunRelation.valid() {
		return fmt.Errorf("%w: unsupported target run relation %q", ErrInvalidContinuationPlan, snapshot.TargetRunRelation)
	}
	if err := validateRequired("plan subject revision id", snapshot.SubjectRevisionID, ErrInvalidContinuationPlan); err != nil {
		return err
	}
	if err := snapshot.SubjectDigest.Validate(); err != nil {
		return err
	}
	if snapshot.TargetRunRelation == RelationSameRunAttempt && (snapshot.SubjectRevisionID != snapshot.BaseCheckpoint.SubjectRevisionID || snapshot.SubjectDigest != snapshot.BaseCheckpoint.SubjectDigest) {
		return fmt.Errorf("%w: same-run continuation cannot change the subject revision", ErrInvalidContinuationPlan)
	}
	if snapshot.Strategy == StrategyReviseSubject {
		if snapshot.TargetRunRelation != RelationChildRun {
			return fmt.Errorf("%w: subject revision strategy requires a child run", ErrInvalidContinuationPlan)
		}
		if err := validateRequired("prepared change id", snapshot.PreparedChangeID, ErrInvalidContinuationPlan); err != nil {
			return err
		}
		if err := validateRequired("candidate revision id", snapshot.CandidateRevisionID, ErrInvalidContinuationPlan); err != nil {
			return err
		}
	}
	if snapshot.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: plan expiration is required", ErrInvalidContinuationPlan)
	}
	if err := validateUniqueStrings("retired artifact id", snapshot.RetireArtifacts, ErrInvalidContinuationPlan); err != nil {
		return err
	}
	if err := validatePlanAssertions(snapshot.Assertions); err != nil {
		return err
	}
	transitions, err := validateTransitionCoverage(snapshot.Nodes, workflow)
	if err != nil {
		return err
	}
	confirmations, err := validateExternalEffectConfirmations(snapshot.ExternalEffectConfirmations, workflow)
	if err != nil {
		return err
	}
	if err := validateSchedule(snapshot.Schedule, workflow, transitions, confirmations); err != nil {
		return err
	}
	return nil
}

func validateTransitionCoverage(transitions []NodeTransition, workflow WorkflowDescriptor) (map[NodeID]NodeTransition, error) {
	if len(transitions) != len(workflow.Stages) {
		return nil, fmt.Errorf("%w: plan has %d transitions for %d compiled stages", ErrInvalidContinuationPlan, len(transitions), len(workflow.Stages))
	}
	known := make(map[NodeID]StageDescriptor, len(workflow.Stages))
	for _, stage := range workflow.Stages {
		known[stage.Key] = stage
	}
	result := make(map[NodeID]NodeTransition, len(transitions))
	for _, transition := range transitions {
		if err := transition.validate(); err != nil {
			return nil, err
		}
		stage, exists := known[transition.NodeID]
		if !exists {
			return nil, fmt.Errorf("%w: transition refers to unknown stage %q", ErrInvalidContinuationPlan, transition.NodeID)
		}
		if _, duplicate := result[transition.NodeID]; duplicate {
			return nil, fmt.Errorf("%w: plan has more than one disposition for stage %q", ErrInvalidContinuationPlan, transition.NodeID)
		}
		if transition.Disposition == DispositionPreserve && stage.Reuse != ReuseWhenInputsMatch {
			return nil, fmt.Errorf("%w: stage %q does not permit reuse", ErrInvalidContinuationPlan, transition.NodeID)
		}
		if stage.OperatorOnly() && transition.Disposition != DispositionOperatorOnly {
			return nil, fmt.Errorf("%w: operator-only stage %q must retain the operator-only disposition", ErrInvalidContinuationPlan, transition.NodeID)
		}
		if !stage.OperatorOnly() && transition.Disposition == DispositionOperatorOnly {
			return nil, fmt.Errorf("%w: automatically dispatchable stage %q cannot use the operator-only disposition", ErrInvalidContinuationPlan, transition.NodeID)
		}
		if err := validateTransitionBindings(transition, stage); err != nil {
			return nil, err
		}
		result[transition.NodeID] = transition.Clone()
	}
	for _, stage := range workflow.Stages {
		if _, exists := result[stage.Key]; !exists {
			return nil, fmt.Errorf("%w: plan is missing a disposition for stage %q", ErrInvalidContinuationPlan, stage.Key)
		}
	}
	return result, nil
}

func validateTransitionBindings(transition NodeTransition, stage StageDescriptor) error {
	specifications := make(map[string]ArtifactSpec, len(stage.Inputs))
	for _, specification := range stage.Inputs {
		specifications[specification.Name] = specification
	}
	bound := make(map[string]struct{}, len(transition.InputBindings))
	for _, binding := range transition.InputBindings {
		specification, exists := specifications[binding.Name]
		if !exists {
			return fmt.Errorf("%w: transition node %q binds undeclared input artifact %q", ErrInvalidContinuationPlan, transition.NodeID, binding.Name)
		}
		if specification.SchemaVersion != binding.SchemaVersion {
			return fmt.Errorf("%w: transition node %q binding %q schema %q does not match descriptor schema %q", ErrInvalidContinuationPlan, transition.NodeID, binding.Name, binding.SchemaVersion, specification.SchemaVersion)
		}
		bound[binding.Name] = struct{}{}
	}
	// A preserved stage must prove every required input is still bound to an
	// immutable artifact. Scheduled and invalidated stages intentionally may
	// have unresolved downstream inputs: their producer is part of this frozen
	// plan, so fabricating a stale binding would be less safe than recording no
	// binding and letting runtime materialize it after its dependency completes.
	if transition.Disposition == DispositionPreserve {
		for _, specification := range stage.Inputs {
			if specification.Required {
				if _, exists := bound[specification.Name]; !exists {
					return fmt.Errorf("%w: preserved transition node %q is missing required input artifact %q", ErrInvalidContinuationPlan, transition.NodeID, specification.Name)
				}
			}
		}
	}
	return nil
}

func validatePlanAssertions(assertions []PlanAssertion) error {
	seen := make(map[string]struct{}, len(assertions))
	for _, assertion := range assertions {
		if err := assertion.validate(); err != nil {
			return err
		}
		key := string(assertion.Kind) + "\x00" + assertion.Subject
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("%w: duplicate plan assertion for %q", ErrInvalidContinuationPlan, assertion.Subject)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func validateExternalEffectConfirmations(confirmations []ExternalEffectConfirmation, workflow WorkflowDescriptor) (map[NodeID]ExternalEffectConfirmation, error) {
	known := make(map[NodeID]StageDescriptor, len(workflow.Stages))
	for _, stage := range workflow.Stages {
		known[stage.Key] = stage
	}
	result := make(map[NodeID]ExternalEffectConfirmation, len(confirmations))
	for _, confirmation := range confirmations {
		if err := confirmation.validate(); err != nil {
			return nil, err
		}
		stage, exists := known[confirmation.NodeID]
		if !exists {
			return nil, fmt.Errorf("%w: confirmation refers to unknown stage %q", ErrInvalidContinuationPlan, confirmation.NodeID)
		}
		if stage.Effect != EffectExternalSideEffect {
			return nil, fmt.Errorf("%w: confirmation is only valid for external side-effect stage %q", ErrInvalidContinuationPlan, confirmation.NodeID)
		}
		if stage.OperatorOnly() {
			return nil, fmt.Errorf("%w: operator-only stage %q cannot be authorized through a continuation", ErrInvalidContinuationPlan, confirmation.NodeID)
		}
		if _, duplicate := result[confirmation.NodeID]; duplicate {
			return nil, fmt.Errorf("%w: duplicate external effect confirmation for stage %q", ErrInvalidContinuationPlan, confirmation.NodeID)
		}
		result[confirmation.NodeID] = confirmation
	}
	return result, nil
}

func validateSchedule(batches []ScheduleBatch, workflow WorkflowDescriptor, transitions map[NodeID]NodeTransition, confirmations map[NodeID]ExternalEffectConfirmation) error {
	stageByID := make(map[NodeID]StageDescriptor, len(workflow.Stages))
	for _, stage := range workflow.Stages {
		stageByID[stage.Key] = stage
	}
	batchForNode := make(map[NodeID]int)
	batchIDs := make(map[string]struct{}, len(batches))
	for index, batch := range batches {
		if err := validateRequired("schedule batch id", batch.ID, ErrInvalidContinuationPlan); err != nil {
			return err
		}
		if _, duplicate := batchIDs[batch.ID]; duplicate {
			return fmt.Errorf("%w: duplicate schedule batch id %q", ErrInvalidContinuationPlan, batch.ID)
		}
		batchIDs[batch.ID] = struct{}{}
		if len(batch.NodeIDs) == 0 {
			return fmt.Errorf("%w: schedule batch %q is empty", ErrInvalidContinuationPlan, batch.ID)
		}
		if err := validateUniqueStrings("scheduled node id", batch.NodeIDs, ErrInvalidContinuationPlan); err != nil {
			return err
		}
		for _, nodeID := range batch.NodeIDs {
			transition, exists := transitions[nodeID]
			if !exists {
				return fmt.Errorf("%w: schedule refers to unknown stage %q", ErrInvalidContinuationPlan, nodeID)
			}
			if transition.Disposition != DispositionSchedule {
				return fmt.Errorf("%w: stage %q is scheduled but disposition is %q", ErrInvalidContinuationPlan, nodeID, transition.Disposition)
			}
			if !stageByID[nodeID].AutomaticallyDispatchable() {
				return fmt.Errorf("%w: schedule contains operator-only stage %q", ErrInvalidContinuationPlan, nodeID)
			}
			if _, duplicate := batchForNode[nodeID]; duplicate {
				return fmt.Errorf("%w: stage %q appears in multiple schedule batches", ErrInvalidContinuationPlan, nodeID)
			}
			batchForNode[nodeID] = index
			if stageByID[nodeID].Effect == EffectExternalSideEffect {
				if _, confirmed := confirmations[nodeID]; !confirmed {
					return fmt.Errorf("%w: scheduled external side-effect stage %q lacks explicit confirmation", ErrInvalidContinuationPlan, nodeID)
				}
			}
		}
	}
	for nodeID, transition := range transitions {
		_, scheduled := batchForNode[nodeID]
		if transition.Disposition == DispositionSchedule && !scheduled {
			return fmt.Errorf("%w: scheduled stage %q is missing from schedule batches", ErrInvalidContinuationPlan, nodeID)
		}
		if transition.Disposition != DispositionSchedule && scheduled {
			return fmt.Errorf("%w: non-scheduled stage %q appears in schedule batches", ErrInvalidContinuationPlan, nodeID)
		}
	}
	for nodeID := range confirmations {
		if transitions[nodeID].Disposition != DispositionSchedule {
			return fmt.Errorf("%w: external effect confirmation for non-scheduled stage %q", ErrInvalidContinuationPlan, nodeID)
		}
	}
	for nodeID, currentBatch := range batchForNode {
		stage := stageByID[nodeID]
		for _, dependency := range stage.Dependencies {
			dependencyTransition := transitions[dependency]
			switch dependencyTransition.Disposition {
			case DispositionInvalidate:
				return fmt.Errorf("%w: scheduled stage %q depends on invalidated stage %q", ErrInvalidContinuationPlan, nodeID, dependency)
			case DispositionSchedule:
				if batchForNode[dependency] >= currentBatch {
					return fmt.Errorf("%w: scheduled dependency %q must be in an earlier batch than %q", ErrInvalidContinuationPlan, dependency, nodeID)
				}
			}
		}
	}
	return nil
}

func fingerprintContinuationPlan(snapshot ContinuationPlanSnapshot) (Fingerprint, error) {
	canonical := snapshot.Clone()
	canonical.ExpiresAt = canonical.ExpiresAt.UTC()
	sort.Slice(canonical.Nodes, func(left, right int) bool { return canonical.Nodes[left].NodeID < canonical.Nodes[right].NodeID })
	for index := range canonical.Nodes {
		transition := &canonical.Nodes[index]
		sort.Slice(transition.ReasonCodes, func(left, right int) bool { return transition.ReasonCodes[left] < transition.ReasonCodes[right] })
		sort.Slice(transition.InputBindings, func(left, right int) bool {
			return transition.InputBindings[left].Name < transition.InputBindings[right].Name
		})
	}
	sort.Slice(canonical.RetireArtifacts, func(left, right int) bool { return canonical.RetireArtifacts[left] < canonical.RetireArtifacts[right] })
	for index := range canonical.Schedule {
		sort.Slice(canonical.Schedule[index].NodeIDs, func(left, right int) bool {
			return canonical.Schedule[index].NodeIDs[left] < canonical.Schedule[index].NodeIDs[right]
		})
	}
	sort.Slice(canonical.Assertions, func(left, right int) bool {
		if canonical.Assertions[left].Kind == canonical.Assertions[right].Kind {
			return canonical.Assertions[left].Subject < canonical.Assertions[right].Subject
		}
		return canonical.Assertions[left].Kind < canonical.Assertions[right].Kind
	})
	sort.Slice(canonical.ExternalEffectConfirmations, func(left, right int) bool {
		return canonical.ExternalEffectConfirmations[left].NodeID < canonical.ExternalEffectConfirmations[right].NodeID
	})
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("%w: encode frozen continuation plan: %v", ErrInvalidContinuationPlan, err)
	}
	return FingerprintBytes("workflowkit.continuation-plan.v1", encoded)
}
