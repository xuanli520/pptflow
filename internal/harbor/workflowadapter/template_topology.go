package workflowadapter

import (
	"fmt"
	"reflect"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// catalogTemplatePolicy is intentionally code-only. A catalog may customize
// versioned plugin and quota policy values for tests or a future source
// revision, but it cannot change which Harbor stages constitute a template or
// silently alter a closed template's required ordering.
type catalogTemplatePolicy struct {
	catalogID                   string
	catalogVersion              string
	stageOrder                  []workflowkit.StageKey
	groups                      []StageGroup
	gates                       []workflowkit.StageKey
	dependencies                map[workflowkit.StageKey][]workflowkit.StageKey
	requiresOperatorOnlyPackage bool
	validateStages              func(map[workflowkit.StageKey]StageDefinition) error
}

func catalogPolicyFor(reference TemplateReference) (catalogTemplatePolicy, error) {
	if err := reference.Validate(); err != nil {
		return catalogTemplatePolicy{}, err
	}
	switch {
	case reference.Equal(StandardTemplateReference()):
		return catalogTemplatePolicy{
			catalogID:                   standardCatalogID,
			catalogVersion:              standardCatalogVersion,
			stageOrder:                  standardCatalogStageOrder(),
			groups:                      StandardStageGroups(),
			requiresOperatorOnlyPackage: true,
			gates: []workflowkit.StageKey{
				workflowkit.StageKey(TaskReview),
				workflowkit.StageKey(ContentReview),
				workflowkit.StageKey(SolutionReview),
				workflowkit.StageKey(FinalReview),
				workflowkit.StageKey(ResultReview),
			},
		}, nil
	case reference.Equal(StandardAuthoringContractTemplateReference()):
		return catalogTemplatePolicy{
			catalogID:                   standardAuthoringCatalogID,
			catalogVersion:              standardAuthoringCatalogVersion,
			stageOrder:                  StandardAuthoringStageOrder(),
			groups:                      standardAuthoringStageGroups(),
			requiresOperatorOnlyPackage: false,
			gates: []workflowkit.StageKey{
				workflowkit.StageKey(TaskReview), workflowkit.StageKey(ContentReview), workflowkit.StageKey(SolutionReview),
			},
			dependencies:   standardAuthoringDependencies(),
			validateStages: validateStandardAuthoringV3CatalogStages,
		}, nil
	case reference.Equal(CodeEdgePhase1TemplateReference()):
		return catalogTemplatePolicy{
			catalogID:                   codeEdgePhase1CatalogID,
			catalogVersion:              codeEdgePhase1CatalogVersion,
			stageOrder:                  CodeEdgePhase1StageOrder(),
			groups:                      codeEdgePhase1StageGroups(),
			requiresOperatorOnlyPackage: true,
			gates: []workflowkit.StageKey{
				workflowkit.StageKey(SolutionReview),
				workflowkit.StageKey(FinalReview),
				workflowkit.StageKey(EvaluatorEvidenceHandoff),
				workflowkit.StageKey(ResultReview),
			},
			dependencies: codeEdgePhase1Dependencies(),
		}, nil
	case reference.Equal(CodeEdgeEvaluatorChildTemplateReference()):
		return catalogTemplatePolicy{
			catalogID:      codeEdgeEvaluatorChildCatalogID,
			catalogVersion: codeEdgeEvaluatorChildCatalogVersion,
			stageOrder:     CodeEdgeEvaluatorChildStageOrder(),
			groups:         codeEdgeEvaluatorChildStageGroups(),
			gates:          []workflowkit.StageKey{},
			dependencies:   codeEdgeEvaluatorChildDependencies(),
			validateStages: validateCodeEdgeEvaluatorChildCatalogStages,
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

func (policy catalogTemplatePolicy) validateStageDefinitions(stages map[workflowkit.StageKey]StageDefinition) error {
	if policy.validateStages == nil {
		return nil
	}
	return policy.validateStages(stages)
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

// validateStandardAuthoringV3CatalogStages compares the complete direct 3.0
// descriptor, not only its graph. This freezes role authority, workspace
// isolation, candidate validation, finding repair, and package admission as
// one immutable topology.
func validateStandardAuthoringV3CatalogStages(stages map[workflowkit.StageKey]StageDefinition) error {
	expected := StandardAuthoringContractStageCatalog()
	if len(stages) != len(expected.Stages) {
		return fmt.Errorf("%w: Standard authoring 3.0 stage count %d does not match frozen descriptor", errInvalidCatalog, len(stages))
	}
	for _, definition := range expected.Stages {
		actual, present := stages[definition.Key]
		actual = actual.Clone()
		definition = definition.Clone()
		if !present || !reflect.DeepEqual(actual, definition) {
			return fmt.Errorf("%w: Standard authoring 3.0 stage %q does not match frozen descriptor", errInvalidCatalog, definition.Key)
		}
	}
	return nil
}

// validateCodeEdgeEvaluatorChildCatalogStages freezes more than the DAG for
// the evaluator child. It has only two externally billable operations, so an
// altered screenshot schema, extra artifact, relaxed effect, or generic retry
// would silently change the evidence contract even if the two stage keys and
// their dependency still looked valid.
func validateCodeEdgeEvaluatorChildCatalogStages(stages map[workflowkit.StageKey]StageDefinition) error {
	expected := codeEdgeEvaluatorChildStageDefinitions()
	if len(stages) != len(expected) {
		return fmt.Errorf("%w: CodeEdge evaluator child stage count %d does not match frozen descriptor", errInvalidCatalog, len(stages))
	}
	for _, definition := range expected {
		actual, present := stages[definition.Key]
		actual = actual.Clone()
		definition = definition.Clone()
		if !present || !reflect.DeepEqual(actual, definition) {
			return fmt.Errorf("%w: CodeEdge evaluator child stage %q does not match frozen descriptor", errInvalidCatalog, definition.Key)
		}
	}
	return nil
}
