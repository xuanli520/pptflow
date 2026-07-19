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
	case reference.Equal(StandardAuthoringTemplateReference()):
		return catalogTemplatePolicy{
			catalogID:                   standardAuthoringCatalogID,
			catalogVersion:              standardAuthoringCatalogVersion,
			stageOrder:                  StandardAuthoringStageOrder(),
			groups:                      standardAuthoringStageGroups(),
			requiresOperatorOnlyPackage: false,
			gates: []workflowkit.StageKey{
				workflowkit.StageKey(TaskReview),
				workflowkit.StageKey(ContentReview),
				workflowkit.StageKey(SolutionReview),
			},
			dependencies: standardAuthoringDependencies(),
			validateStages: func(stages map[workflowkit.StageKey]StageDefinition) error {
				return validateStandardAuthoringEnvironmentPolicyContract(stages)
			},
		}, nil
	case reference.Equal(StandardAuthoringTaskAdmissionTemplateReference()):
		return catalogTemplatePolicy{
			catalogID:                   standardAuthoringCatalogID,
			catalogVersion:              StandardAuthoringTaskAdmissionTemplateVersion,
			stageOrder:                  StandardAuthoringTaskAdmissionStageOrder(),
			groups:                      standardAuthoringStageGroups(),
			requiresOperatorOnlyPackage: false,
			gates: []workflowkit.StageKey{
				workflowkit.StageKey(TaskReview),
				workflowkit.StageKey(ContentReview),
				workflowkit.StageKey(SolutionReview),
			},
			dependencies:   standardAuthoringTaskAdmissionDependencies(),
			validateStages: validateStandardAuthoringTaskAdmissionContract,
		}, nil
	case reference.Equal(StandardAuthoringBriefTemplateReference()):
		return catalogTemplatePolicy{
			catalogID:                   standardAuthoringCatalogID,
			catalogVersion:              StandardAuthoringBriefTemplateVersion,
			stageOrder:                  StandardAuthoringTaskAdmissionStageOrder(),
			groups:                      standardAuthoringStageGroups(),
			requiresOperatorOnlyPackage: false,
			gates: []workflowkit.StageKey{
				workflowkit.StageKey(TaskReview),
				workflowkit.StageKey(ContentReview),
				workflowkit.StageKey(SolutionReview),
			},
			dependencies:   standardAuthoringTaskAdmissionDependencies(),
			validateStages: validateStandardAuthoringBriefContract,
		}, nil
	case reference.Equal(StandardAuthoringRepairFeedbackTemplateReference()):
		return catalogTemplatePolicy{
			catalogID:                   standardAuthoringCatalogID,
			catalogVersion:              StandardAuthoringRepairFeedbackTemplateVersion,
			stageOrder:                  StandardAuthoringTaskAdmissionStageOrder(),
			groups:                      standardAuthoringStageGroups(),
			requiresOperatorOnlyPackage: false,
			gates: []workflowkit.StageKey{
				workflowkit.StageKey(TaskReview),
				workflowkit.StageKey(ContentReview),
				workflowkit.StageKey(SolutionReview),
			},
			dependencies:   standardAuthoringTaskAdmissionDependencies(),
			validateStages: validateStandardAuthoringRepairFeedbackContract,
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
		// StageDefinition.Clone intentionally canonicalizes empty slices to nil,
		// because registry resolution returns independent snapshots. Compare those
		// owned copies so nil versus empty declaration storage cannot masquerade as
		// a semantic descriptor drift.
		actual = actual.Clone()
		definition = definition.Clone()
		if !present || !reflect.DeepEqual(actual, definition) {
			return fmt.Errorf("%w: CodeEdge evaluator child stage %q does not match frozen descriptor", errInvalidCatalog, definition.Key)
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

// validateStandardAuthoringEnvironmentPolicyContract freezes the source-session
// base-image boundary. Every consumer must declare exactly one required policy
// input and read the corresponding resource; no other stage may consume it.
func validateStandardAuthoringEnvironmentPolicyContract(stages map[workflowkit.StageKey]StageDefinition) error {
	for _, key := range StandardAuthoringStageOrder() {
		stage, present := stages[key]
		if !present {
			return fmt.Errorf("%w: required Standard authoring stage %q is missing", errInvalidCatalog, key)
		}
		wantsPolicy := standardAuthoringStageUsesEnvironmentPolicy(key) || key == workflowkit.StageKey(MaterializeTask)
		inputCount, exactInputCount := stageCatalogEnvironmentPolicyInputCounts(stage.Inputs)
		resourceCount := stageCatalogResourceCount(stage.ReadSet, resourceAuthoringEnvironmentPolicy)
		if (wantsPolicy && (inputCount != 1 || exactInputCount != 1 || resourceCount != 1)) ||
			(!wantsPolicy && (inputCount != 0 || resourceCount != 0)) {
			return fmt.Errorf("%w: Standard authoring template %q stage %q environment-policy contract does not match its version", errInvalidCatalog, stage.Key, key)
		}
	}
	return nil
}

func validateStandardAuthoringTaskAdmissionContract(stages map[workflowkit.StageKey]StageDefinition) error {
	if err := validateStandardAuthoringEnvironmentPolicyContract(stages); err != nil {
		return err
	}
	admission, present := stages[workflowkit.StageKey(CodeEdgePackageAdmission)]
	if !present {
		return fmt.Errorf("%w: task-admission stage is missing", errInvalidCatalog)
	}
	if inputs, exact := stageCatalogEnvironmentPolicyInputCounts(admission.Inputs); inputs != 1 || exact != 1 || stageCatalogResourceCount(admission.ReadSet, resourceAuthoringEnvironmentPolicy) != 1 {
		return fmt.Errorf("%w: task-admission stage must consume the frozen environment policy", errInvalidCatalog)
	}
	if !stageHasArtifact(admission.Outputs, "codeedge_package_admission_report", "harbor.standard-authoring-task-package-admission.v1") {
		return fmt.Errorf("%w: task-admission stage must emit the admission report", errInvalidCatalog)
	}
	for _, key := range []workflowkit.StageKey{workflowkit.StageKey(SolutionReview), workflowkit.StageKey(MaterializeTask)} {
		if !stageHasArtifact(stages[key].Inputs, "codeedge_package_admission_report", "harbor.standard-authoring-task-package-admission.v1") {
			return fmt.Errorf("%w: stage %q must consume the admission report", errInvalidCatalog, key)
		}
	}
	return nil
}

func validateStandardAuthoringBriefContract(stages map[workflowkit.StageKey]StageDefinition) error {
	if err := validateStandardAuthoringBriefIntrinsicContract(stages); err != nil {
		return err
	}
	return validateStandardAuthoringFeedbackShape(stages, StandardAuthoringBriefStageCatalog())
}

func validateStandardAuthoringBriefIntrinsicContract(stages map[workflowkit.StageKey]StageDefinition) error {
	if err := validateStandardAuthoringTaskAdmissionContract(stages); err != nil {
		return err
	}
	for _, key := range StandardAuthoringTaskAdmissionStageOrder() {
		stage, present := stages[key]
		if !present {
			return fmt.Errorf("%w: required Standard authoring brief stage %q is missing", errInvalidCatalog, key)
		}
		wantsBrief := standardAuthoringStageUsesBrief(key)
		inputCount, exactInputCount := stageCatalogBriefInputCounts(stage.Inputs)
		resourceCount := stageCatalogResourceCount(stage.ReadSet, resourceAuthoringBrief)
		if (wantsBrief && (inputCount != 1 || exactInputCount != 1 || resourceCount != 1)) ||
			(!wantsBrief && (inputCount != 0 || resourceCount != 0)) {
			return fmt.Errorf("%w: Standard authoring brief template stage %q brief contract does not match its version", errInvalidCatalog, key)
		}
	}
	return nil
}

func validateStandardAuthoringRepairFeedbackContract(stages map[workflowkit.StageKey]StageDefinition) error {
	if err := validateStandardAuthoringBriefIntrinsicContract(stages); err != nil {
		return err
	}
	return validateStandardAuthoringFeedbackShape(stages, StandardAuthoringRepairFeedbackStageCatalog())
}

func validateStandardAuthoringFeedbackShape(stages map[workflowkit.StageKey]StageDefinition, expectedCatalog StageCatalog) error {
	expectedStages := make(map[workflowkit.StageKey]StageDefinition, len(expectedCatalog.Stages))
	for _, stage := range expectedCatalog.Stages {
		expectedStages[stage.Key] = stage
	}
	for _, key := range StandardAuthoringTaskAdmissionStageOrder() {
		stage, present := stages[key]
		if !present {
			return fmt.Errorf("%w: required Standard authoring feedback stage %q is missing", errInvalidCatalog, key)
		}
		expectedStage := expectedStages[key]
		for _, contract := range standardAuthoringRepairFeedbackArtifactContracts() {
			expected, expectsFeedback := stageArtifactSpec(expectedStage.Inputs, contract.name)
			actual, present := stageArtifactSpec(stage.Inputs, contract.name)
			if present != expectsFeedback || (present && actual != expected) {
				return fmt.Errorf("%w: Standard authoring feedback stage %q input %q does not match its version", errInvalidCatalog, key, contract.name)
			}
			expectedResource := stageCatalogResourceCount(expectedStage.ReadSet, contract.resource)
			if stageCatalogResourceCount(stage.ReadSet, contract.resource) != expectedResource {
				return fmt.Errorf("%w: Standard authoring feedback stage %q resource %q does not match its input", errInvalidCatalog, key, contract.resource)
			}
		}
	}
	return nil
}

type standardAuthoringRepairFeedbackArtifactContract struct {
	name     string
	resource workflowkit.ResourceKey
}

func standardAuthoringRepairFeedbackArtifactContracts() []standardAuthoringRepairFeedbackArtifactContract {
	return []standardAuthoringRepairFeedbackArtifactContract{
		{name: "task_review_decision", resource: resourceReviewTaskDirection},
		{name: "content_review_decision", resource: resourceReviewContent},
		{name: "solution_review_decision", resource: resourceReviewSolutionVerifier},
		{name: "codeedge_package_admission_report", resource: resourceAuthoringTaskAdmission},
	}
}

func stageArtifactSpec(artifacts []workflowkit.ArtifactSpec, name string) (workflowkit.ArtifactSpec, bool) {
	for _, artifact := range artifacts {
		if artifact.Name == name {
			return artifact, true
		}
	}
	return workflowkit.ArtifactSpec{}, false
}

func stageHasArtifact(artifacts []workflowkit.ArtifactSpec, name, schemaVersion string) bool {
	for _, artifact := range artifacts {
		if artifact.Name == name && artifact.SchemaVersion == schemaVersion && artifact.Required {
			return true
		}
	}
	return false
}

func stageCatalogEnvironmentPolicyInputCounts(inputs []workflowkit.ArtifactSpec) (total, exact int) {
	for _, input := range inputs {
		if input.Name != StandardAuthoringEnvironmentPolicyArtifact {
			continue
		}
		total++
		if input.SchemaVersion == StandardAuthoringEnvironmentPolicySchemaVersion && input.Required {
			exact++
		}
	}
	return total, exact
}

func stageCatalogBriefInputCounts(inputs []workflowkit.ArtifactSpec) (total, exact int) {
	for _, input := range inputs {
		if input.Name != StandardAuthoringBriefArtifact {
			continue
		}
		total++
		if input.SchemaVersion == StandardAuthoringBriefSchemaVersion && input.Required {
			exact++
		}
	}
	return total, exact
}

func stageCatalogResourceCount(resources []workflowkit.ResourceKey, target workflowkit.ResourceKey) int {
	count := 0
	for _, resource := range resources {
		if resource == target {
			count++
		}
	}
	return count
}
