package workflowadapter

import (
	"fmt"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// catalogTemplatePolicy is intentionally code-only. A catalog may customize
// versioned plugin and quota policy values for tests or a future source
// revision, but it cannot change which Harbor stages constitute a template or
// silently alter a closed template's required ordering.
type catalogTemplatePolicy struct {
	catalogID      string
	catalogVersion string
	stageOrder     []workflowkit.StageKey
	groups         []StageGroup
	gates          []workflowkit.StageKey
	dependencies   map[workflowkit.StageKey][]workflowkit.StageKey
}

func catalogPolicyFor(reference TemplateReference) (catalogTemplatePolicy, error) {
	if err := reference.Validate(); err != nil {
		return catalogTemplatePolicy{}, err
	}
	switch {
	case reference.Equal(StandardTemplateReference()):
		return catalogTemplatePolicy{
			catalogID:      standardCatalogID,
			catalogVersion: standardCatalogVersion,
			stageOrder:     standardCatalogStageOrder(),
			groups:         StandardStageGroups(),
			gates: []workflowkit.StageKey{
				workflowkit.StageKey(TaskReview),
				workflowkit.StageKey(ContentReview),
				workflowkit.StageKey(SolutionReview),
				workflowkit.StageKey(FinalReview),
				workflowkit.StageKey(ResultReview),
			},
		}, nil
	case reference.Equal(CodeEdgePhase1TemplateReference()):
		return catalogTemplatePolicy{
			catalogID:      codeEdgePhase1CatalogID,
			catalogVersion: codeEdgePhase1CatalogVersion,
			stageOrder:     CodeEdgePhase1StageOrder(),
			groups:         codeEdgePhase1StageGroups(),
			gates: []workflowkit.StageKey{
				workflowkit.StageKey(SolutionReview),
				workflowkit.StageKey(FinalReview),
				workflowkit.StageKey(ResultReview),
			},
			dependencies: codeEdgePhase1Dependencies(),
		}, nil
	default:
		return catalogTemplatePolicy{}, fmt.Errorf("%w: workflow template %s@%s has no catalog policy", errInvalidCatalog, reference.ID, reference.Version)
	}
}

func standardCatalogStageOrder() []workflowkit.StageKey {
	keys := lifecycleNodeOrder()
	result := make([]workflowkit.StageKey, len(keys))
	for index, key := range keys {
		result[index] = workflowkit.StageKey(key)
	}
	return result
}

func (policy catalogTemplatePolicy) validateTopology(stages map[workflowkit.StageKey]StageDefinition) error {
	if len(policy.dependencies) == 0 {
		return nil
	}
	for key, expected := range policy.dependencies {
		stage, present := stages[key]
		if !present {
			return fmt.Errorf("%w: required template topology stage %q is missing", errInvalidCatalog, key)
		}
		if !sameStageKeySet(stage.Dependencies, expected) {
			return fmt.Errorf("%w: stage %q dependencies %v do not match frozen template topology %v", errInvalidCatalog, key, stage.Dependencies, expected)
		}
	}
	return nil
}

func sameStageKeySet(left, right []workflowkit.StageKey) bool {
	if len(left) != len(right) {
		return false
	}
	values := make(map[workflowkit.StageKey]struct{}, len(left))
	for _, value := range left {
		values[value] = struct{}{}
	}
	if len(values) != len(left) {
		return false
	}
	for _, value := range right {
		if _, present := values[value]; !present {
			return false
		}
	}
	return true
}
