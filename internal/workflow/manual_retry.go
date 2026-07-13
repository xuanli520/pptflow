package workflow

import (
	"context"
	"fmt"
	"strings"
)

// PlanManualRetry is a read-only DAG operation. It resolves a skipped node to
// the failed or canceled ancestors that actually block it, then includes every
// downstream dependant in the invalidation set.
func PlanManualRetry(definition WorkflowDefinition, prior map[string]NodeRun, revision int, nodeID string) (ManualRetryPlan, error) {
	nodes, err := orderedWorkflowNodes(definition)
	if err != nil {
		return ManualRetryPlan{}, err
	}
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" {
		return ManualRetryPlan{}, fmt.Errorf("manual retry node ID is required")
	}
	byID := make(map[string]NodeSpec, len(nodes))
	indexByID := make(map[string]int, len(nodes))
	for index, spec := range nodes {
		byID[spec.ID] = spec
		indexByID[spec.ID] = index
	}
	requested, ok := byID[nodeID]
	if !ok {
		return ManualRetryPlan{}, fmt.Errorf("manual retry targets unknown node %q", nodeID)
	}
	run, ok := prior[nodeID]
	if !ok {
		return ManualRetryPlan{}, fmt.Errorf("manual retry node %q has no durable state", nodeID)
	}
	switch run.Status {
	case NodeFailed, NodeCanceled:
	case NodeSkipped:
	default:
		return ManualRetryPlan{}, fmt.Errorf("manual retry requires failed, canceled, or skipped node; %s is %s", nodeID, run.Status)
	}

	roots := map[string]bool{}
	if run.Status == NodeSkipped {
		visiting := map[string]bool{}
		if err := collectBlockingRetryRoots(requested, byID, prior, roots, visiting); err != nil {
			return ManualRetryPlan{}, err
		}
		if len(roots) == 0 {
			return ManualRetryPlan{}, fmt.Errorf("skipped node %q has no failed or canceled blocking ancestor", nodeID)
		}
	} else {
		roots[nodeID] = true
	}

	affectedSet := make(map[string]bool, len(nodes))
	for root := range roots {
		affectedSet[root] = true
	}
	for changed := true; changed; {
		changed = false
		for _, spec := range nodes {
			if affectedSet[spec.ID] {
				continue
			}
			for _, dependency := range spec.DependsOn {
				if affectedSet[dependency] {
					affectedSet[spec.ID] = true
					changed = true
					break
				}
			}
		}
	}

	plan := ManualRetryPlan{RequestedNodeID: nodeID, CurrentRevision: revision, NextRevision: revision + 1}
	for _, spec := range nodes {
		if roots[spec.ID] {
			plan.RetryRoots = append(plan.RetryRoots, spec.ID)
		}
		if affectedSet[spec.ID] {
			plan.AffectedNodes = append(plan.AffectedNodes, spec.ID)
			continue
		}
		if existing, exists := prior[spec.ID]; exists {
			if existing.Status == NodeSucceeded {
				plan.ReusedUpstream = append(plan.ReusedUpstream, spec.ID)
			} else {
				plan.PreservedNodes = append(plan.PreservedNodes, spec.ID)
			}
		}
	}
	if len(plan.RetryRoots) == 0 {
		return ManualRetryPlan{}, fmt.Errorf("manual retry node %q has no executable retry root", nodeID)
	}
	plan.RestartNodeID = plan.RetryRoots[0]
	for _, root := range plan.RetryRoots[1:] {
		if indexByID[root] < indexByID[plan.RestartNodeID] {
			plan.RestartNodeID = root
		}
	}
	return plan, nil
}

func collectBlockingRetryRoots(spec NodeSpec, byID map[string]NodeSpec, prior map[string]NodeRun, roots, visiting map[string]bool) error {
	if visiting[spec.ID] {
		return fmt.Errorf("manual retry dependency cycle at %q", spec.ID)
	}
	visiting[spec.ID] = true
	defer delete(visiting, spec.ID)
	for _, dependency := range spec.DependsOn {
		depSpec, exists := byID[dependency]
		if !exists {
			return fmt.Errorf("manual retry dependency %q is not in workflow", dependency)
		}
		run, exists := prior[dependency]
		if !exists {
			return fmt.Errorf("skipped node %q is blocked by dependency %q without durable state", spec.ID, dependency)
		}
		switch run.Status {
		case NodeSucceeded:
			continue
		case NodeFailed, NodeCanceled:
			roots[dependency] = true
		case NodeSkipped:
			if err := collectBlockingRetryRoots(depSpec, byID, prior, roots, visiting); err != nil {
				return err
			}
		default:
			return fmt.Errorf("skipped node %q is blocked by non-terminal dependency %q (%s)", spec.ID, dependency, run.Status)
		}
	}
	return nil
}

func applyManualRetry(ctx context.Context, store ArtifactStore, prior map[string]NodeRun, changed map[string]bool, plan ManualRetryPlan) error {
	if len(plan.AffectedNodes) == 0 {
		return fmt.Errorf("manual retry affected node set is empty")
	}
	return invalidateRevision(ctx, store, prior, changed, plan.AffectedNodes)
}

func manualRetryAffected(plan *ManualRetryPlan) map[string]bool {
	if plan == nil {
		return nil
	}
	affected := make(map[string]bool, len(plan.AffectedNodes))
	for _, nodeID := range plan.AffectedNodes {
		affected[nodeID] = true
	}
	return affected
}
