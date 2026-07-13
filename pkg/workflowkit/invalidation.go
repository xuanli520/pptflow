package workflowkit

import (
	"fmt"
	"sort"
)

// ResourceAny is the only built-in wildcard. Domain adapters can supply a
// richer ResourceMatcher when their resource vocabulary needs hierarchy or
// aliases; the kernel never infers that from filesystem paths.
const ResourceAny ResourceKey = "**"

// ResourceMatcher decides whether a declared read resource is affected by an
// observed changed resource.
type ResourceMatcher func(declared, changed ResourceKey) bool

// ExactResourceMatch is the conservative default matcher. It only treats
// identical logical keys, or an explicit ResourceAny, as overlapping.
func ExactResourceMatch(declared, changed ResourceKey) bool {
	return declared == changed || declared == ResourceAny || changed == ResourceAny
}

// ResourceChange is a typed observed change. Reason is optional explanatory
// metadata and does not affect matching semantics.
type ResourceChange struct {
	Key    ResourceKey `json:"key"`
	Reason PlanReason  `json:"reason,omitempty"`
}

// StageReuseState is the artifact-store observation needed to decide whether a
// previously completed stage can be preserved. Present distinguishes a missing
// historic result from an observed result that has become damaged.
type StageReuseState struct {
	NodeID                   NodeID            `json:"node_id"`
	Present                  bool              `json:"present"`
	ArtifactsIntact          bool              `json:"artifacts_intact"`
	ExpectedInputFingerprint Fingerprint       `json:"expected_input_fingerprint,omitempty"`
	CurrentInputs            []ArtifactBinding `json:"current_inputs,omitempty"`
}

func (state StageReuseState) Clone() StageReuseState {
	state.CurrentInputs = append([]ArtifactBinding(nil), state.CurrentInputs...)
	return state
}

func (state StageReuseState) validate() error {
	if err := validateRequired("stage reuse node id", string(state.NodeID), ErrInvalidArtifact); err != nil {
		return err
	}
	if !state.Present {
		if state.ArtifactsIntact || state.ExpectedInputFingerprint != "" || len(state.CurrentInputs) > 0 {
			return fmt.Errorf("%w: absent reuse state %q cannot claim artifacts or inputs", ErrInvalidArtifact, state.NodeID)
		}
		return nil
	}
	if err := state.ExpectedInputFingerprint.Validate(); err != nil {
		return err
	}
	if _, err := FingerprintArtifactBindings(state.CurrentInputs); err != nil {
		return err
	}
	return nil
}

// InvalidationImpact is the safe result for one stage. Confirmation-required
// means an external side effect is stale but must not be repeated implicitly.
type InvalidationImpact string

const (
	ImpactPreserve             InvalidationImpact = "preserve"
	ImpactInvalidate           InvalidationImpact = "invalidate"
	ImpactRequiresConfirmation InvalidationImpact = "requires_confirmation"
)

// InvalidationReason is a stable, machine-readable explanation for an impact.
type InvalidationReason string

const (
	InvalidationChangedResource       InvalidationReason = "changed_resource"
	InvalidationRecomputeRequested    InvalidationReason = "recompute_requested"
	InvalidationDependencyInvalidated InvalidationReason = "dependency_invalidated"
	InvalidationArtifactUnavailable   InvalidationReason = "artifact_unavailable"
	InvalidationInputFingerprintDrift InvalidationReason = "input_fingerprint_drift"
	InvalidationReuseForbidden        InvalidationReason = "reuse_forbidden"
)

// InvalidationEntry is the result for one compiled stage.
type InvalidationEntry struct {
	NodeID  NodeID               `json:"node_id"`
	Impact  InvalidationImpact   `json:"impact"`
	Reasons []InvalidationReason `json:"reasons"`
}

func (entry InvalidationEntry) Clone() InvalidationEntry {
	entry.Reasons = append([]InvalidationReason(nil), entry.Reasons...)
	return entry
}

// InvalidationPlan is a deterministic resource- and lineage-based result.
// It is input to a domain continuation planner, not an executable plan by
// itself: external effects still require explicit confirmation in a frozen
// ContinuationPlan.
type InvalidationPlan struct {
	ChangedResources []ResourceKey       `json:"changed_resources"`
	Entries          []InvalidationEntry `json:"entries"`
}

// Clone returns an independent plan copy.
func (plan InvalidationPlan) Clone() InvalidationPlan {
	plan.ChangedResources = append([]ResourceKey(nil), plan.ChangedResources...)
	entries := plan.Entries
	plan.Entries = make([]InvalidationEntry, len(entries))
	for index, entry := range entries {
		plan.Entries[index] = entry.Clone()
	}
	return plan
}

// Entry returns a copy of the impact for nodeID.
func (plan InvalidationPlan) Entry(nodeID NodeID) (InvalidationEntry, bool) {
	for _, entry := range plan.Entries {
		if entry.NodeID == nodeID {
			return entry.Clone(), true
		}
	}
	return InvalidationEntry{}, false
}

// InvalidationRequest supplies only already-observed facts. It has no bare
// restart point or workspace path: resource changes, explicit recomputes, and
// artifact lineage are enough to derive the affected closure.
type InvalidationRequest struct {
	ChangedResources []ResourceChange  `json:"changed_resources"`
	RecomputeNodes   []NodeID          `json:"recompute_nodes"`
	ReuseStates      []StageReuseState `json:"reuse_states"`
	Matcher          ResourceMatcher   `json:"-"`
}

// PlanInvalidation computes the dependency and resource-read closure. Any
// absent, damaged, or input-drifted artifact is never preserved. A changed or
// recomputed external-side-effect stage is surfaced as confirmation-required
// rather than silently scheduled.
func PlanInvalidation(workflow WorkflowDescriptor, request InvalidationRequest) (InvalidationPlan, error) {
	if err := workflow.Validate(); err != nil {
		return InvalidationPlan{}, fmt.Errorf("%w: workflow: %v", ErrInvalidContinuationPlan, err)
	}
	matcher := request.Matcher
	if matcher == nil {
		matcher = ExactResourceMatch
	}
	stageByID := make(map[NodeID]StageDescriptor, len(workflow.Stages))
	for _, stage := range workflow.Stages {
		stageByID[stage.Key] = stage
	}
	changed := make(map[ResourceKey]struct{}, len(request.ChangedResources))
	for _, change := range request.ChangedResources {
		if err := validateRequired("changed resource", string(change.Key), ErrInvalidContinuationPlan); err != nil {
			return InvalidationPlan{}, err
		}
		if _, duplicate := changed[change.Key]; duplicate {
			return InvalidationPlan{}, fmt.Errorf("%w: duplicate changed resource %q", ErrInvalidContinuationPlan, change.Key)
		}
		changed[change.Key] = struct{}{}
	}
	recompute := make(map[NodeID]struct{}, len(request.RecomputeNodes))
	for _, nodeID := range request.RecomputeNodes {
		if err := validateRequired("recompute node id", string(nodeID), ErrInvalidContinuationPlan); err != nil {
			return InvalidationPlan{}, err
		}
		if _, exists := stageByID[nodeID]; !exists {
			return InvalidationPlan{}, fmt.Errorf("%w: recompute stage %q does not exist", ErrInvalidContinuationPlan, nodeID)
		}
		if _, duplicate := recompute[nodeID]; duplicate {
			return InvalidationPlan{}, fmt.Errorf("%w: duplicate recompute stage %q", ErrInvalidContinuationPlan, nodeID)
		}
		recompute[nodeID] = struct{}{}
	}
	reuseStates := make(map[NodeID]StageReuseState, len(request.ReuseStates))
	for _, state := range request.ReuseStates {
		if err := state.validate(); err != nil {
			return InvalidationPlan{}, err
		}
		if _, exists := stageByID[state.NodeID]; !exists {
			return InvalidationPlan{}, fmt.Errorf("%w: reuse state refers to unknown stage %q", ErrInvalidContinuationPlan, state.NodeID)
		}
		if _, duplicate := reuseStates[state.NodeID]; duplicate {
			return InvalidationPlan{}, fmt.Errorf("%w: duplicate reuse state for stage %q", ErrInvalidContinuationPlan, state.NodeID)
		}
		reuseStates[state.NodeID] = state.Clone()
	}

	order, err := workflow.TopologicalStages()
	if err != nil {
		return InvalidationPlan{}, err
	}
	reasons := make(map[NodeID]map[InvalidationReason]struct{}, len(workflow.Stages))
	impacts := make(map[NodeID]InvalidationImpact, len(workflow.Stages))
	for _, stage := range workflow.Stages {
		impacts[stage.Key] = ImpactPreserve
	}

	// A recomputation can itself change every declared output resource.
	for nodeID := range recompute {
		for _, resource := range stageByID[nodeID].WriteSet {
			changed[resource] = struct{}{}
		}
	}

	for changedPass := true; changedPass; {
		changedPass = false
		for _, nodeID := range order {
			stage := stageByID[nodeID]
			nodeReasons := reasons[nodeID]
			if nodeReasons == nil {
				nodeReasons = make(map[InvalidationReason]struct{})
				reasons[nodeID] = nodeReasons
			}
			if _, selected := recompute[nodeID]; selected {
				nodeReasons[InvalidationRecomputeRequested] = struct{}{}
			}
			if stage.Reuse == ReuseNever {
				nodeReasons[InvalidationReuseForbidden] = struct{}{}
			}
			state, observed := reuseStates[nodeID]
			if !observed || !state.Present || !state.ArtifactsIntact {
				nodeReasons[InvalidationArtifactUnavailable] = struct{}{}
			} else {
				current, fingerprintErr := FingerprintArtifactBindings(state.CurrentInputs)
				if fingerprintErr != nil {
					return InvalidationPlan{}, fingerprintErr
				}
				if current != state.ExpectedInputFingerprint {
					nodeReasons[InvalidationInputFingerprintDrift] = struct{}{}
				}
			}
			if stageReadsChanges(stage, changed, matcher) {
				nodeReasons[InvalidationChangedResource] = struct{}{}
			}
			for _, dependency := range stage.Dependencies {
				if impacts[dependency] != ImpactPreserve {
					nodeReasons[InvalidationDependencyInvalidated] = struct{}{}
				}
			}
			if len(nodeReasons) == 0 {
				continue
			}
			impact := ImpactInvalidate
			if stage.Effect == EffectExternalSideEffect {
				impact = ImpactRequiresConfirmation
			}
			if impacts[nodeID] != impact {
				impacts[nodeID] = impact
				changedPass = true
			}
			for _, resource := range stage.WriteSet {
				if _, exists := changed[resource]; !exists {
					changed[resource] = struct{}{}
					changedPass = true
				}
			}
		}
	}

	result := InvalidationPlan{ChangedResources: sortedResourceKeys(changed), Entries: make([]InvalidationEntry, 0, len(order))}
	for _, nodeID := range order {
		result.Entries = append(result.Entries, InvalidationEntry{
			NodeID:  nodeID,
			Impact:  impacts[nodeID],
			Reasons: sortedInvalidationReasons(reasons[nodeID]),
		})
	}
	return result, nil
}

func stageReadsChanges(stage StageDescriptor, changed map[ResourceKey]struct{}, matcher ResourceMatcher) bool {
	for _, read := range stage.ReadSet {
		for resource := range changed {
			if matcher(read, resource) {
				return true
			}
		}
	}
	return false
}

func sortedResourceKeys(values map[ResourceKey]struct{}) []ResourceKey {
	result := make([]ResourceKey, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Slice(result, func(left, right int) bool { return result[left] < result[right] })
	return result
}

func sortedInvalidationReasons(values map[InvalidationReason]struct{}) []InvalidationReason {
	result := make([]InvalidationReason, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Slice(result, func(left, right int) bool { return result[left] < result[right] })
	return result
}
