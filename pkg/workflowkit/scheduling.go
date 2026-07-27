package workflowkit

import "fmt"

// ValidateConcurrentStages verifies that every stage in one dispatch batch
// can run at the same time. Resource access and workspace bindings are both
// descriptor facts, so every plan boundary can apply the same rule instead of
// relying on a domain executor to notice a conflict after dispatch.
func ValidateConcurrentStages(stages []StageDescriptor) error {
	for left := 0; left < len(stages); left++ {
		for right := left + 1; right < len(stages); right++ {
			if reason := concurrentStageConflict(stages[left], stages[right]); reason != "" {
				return fmt.Errorf("stages %q and %q cannot share a schedule batch: %s", stages[left].Key, stages[right].Key, reason)
			}
		}
	}
	return nil
}

func concurrentStageConflict(left, right StageDescriptor) string {
	if resource, found := resourceOverlap(left.WriteSet, right.WriteSet); found {
		return fmt.Sprintf("both write resource %q", resource)
	}
	if resource, found := resourceOverlap(left.WriteSet, right.ReadSet); found {
		return fmt.Sprintf("%q writes resource %q read by %q", left.Key, resource, right.Key)
	}
	if resource, found := resourceOverlap(right.WriteSet, left.ReadSet); found {
		return fmt.Sprintf("%q writes resource %q read by %q", right.Key, resource, left.Key)
	}
	leftWorkspace := left.concurrencyPolicy().Workspace.canonical()
	rightWorkspace := right.concurrencyPolicy().Workspace.canonical()
	if leftWorkspace.Key != "" && leftWorkspace.Key == rightWorkspace.Key &&
		(leftWorkspace.Mode == WorkspaceExclusiveWriter || rightWorkspace.Mode == WorkspaceExclusiveWriter) {
		return fmt.Sprintf("workspace key %q has an exclusive writer", leftWorkspace.Key)
	}
	return ""
}

func resourceOverlap(left, right []ResourceKey) (ResourceKey, bool) {
	if len(left) > len(right) {
		left, right = right, left
	}
	values := make(map[ResourceKey]struct{}, len(left))
	for _, value := range left {
		values[value] = struct{}{}
	}
	for _, value := range right {
		if _, found := values[value]; found {
			return value, true
		}
	}
	return "", false
}
