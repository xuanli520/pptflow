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
	case reference.Equal(StandardAuthoringTestsAnalysisInputTemplateReference()):
		return catalogTemplatePolicy{
			catalogID:                   standardAuthoringCatalogID,
			catalogVersion:              StandardAuthoringTestsAnalysisInputTemplateVersion,
			stageOrder:                  StandardAuthoringTaskAdmissionStageOrder(),
			groups:                      standardAuthoringStageGroups(),
			requiresOperatorOnlyPackage: false,
			gates: []workflowkit.StageKey{
				workflowkit.StageKey(TaskReview),
				workflowkit.StageKey(ContentReview),
				workflowkit.StageKey(SolutionReview),
			},
			dependencies:   standardAuthoringTestsAnalysisInputDependencies(),
			validateStages: validateStandardAuthoringTestsAnalysisInputContract,
		}, nil
	case reference.Equal(StandardAuthoringHarnessTemplateReference()):
		return catalogTemplatePolicy{
			catalogID:                   standardAuthoringCatalogID,
			catalogVersion:              StandardAuthoringHarnessTemplateVersion,
			stageOrder:                  StandardAuthoringHarnessStageOrder(),
			groups:                      standardAuthoringStageGroups(),
			requiresOperatorOnlyPackage: false,
			gates: []workflowkit.StageKey{
				workflowkit.StageKey(TaskReview),
				workflowkit.StageKey(ContentReview),
				workflowkit.StageKey(SolutionReview),
			},
			dependencies:   standardAuthoringHarnessDependencies(),
			validateStages: validateStandardAuthoringHarnessContract,
		}, nil
	case reference.Equal(StandardAuthoringFixedFileTemplateReference()):
		return catalogTemplatePolicy{
			catalogID:                   standardAuthoringCatalogID,
			catalogVersion:              StandardAuthoringFixedFileTemplateVersion,
			stageOrder:                  StandardAuthoringFixedFileStageOrder(),
			groups:                      standardAuthoringStageGroups(),
			requiresOperatorOnlyPackage: false,
			gates: []workflowkit.StageKey{
				workflowkit.StageKey(TaskReview),
				workflowkit.StageKey(ContentReview),
				workflowkit.StageKey(SolutionReview),
			},
			dependencies:   standardAuthoringHarnessDependencies(),
			validateStages: validateStandardAuthoringFixedFileContract,
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

func validateStandardAuthoringTestsAnalysisInputContract(stages map[workflowkit.StageKey]StageDefinition) error {
	if err := validateStandardAuthoringRepairFeedbackContract(stages); err != nil {
		return err
	}
	for _, contract := range []struct {
		stage    workflowkit.StageKey
		artifact string
		resource workflowkit.ResourceKey
	}{
		{stage: workflowkit.StageKey(SolveGen), artifact: "dockerfile", resource: resourceTaskEnvironment},
		{stage: workflowkit.StageKey(TestGen), artifact: "dockerfile", resource: resourceTaskEnvironment},
		{stage: workflowkit.StageKey(TestsAnalysis), artifact: "test_script", resource: resourceTaskTests},
	} {
		stage, present := stages[contract.stage]
		if !present || !stageHasArtifact(stage.Inputs, contract.artifact, "harbor.artifact.v1") || stageCatalogResourceCount(stage.ReadSet, contract.resource) != 1 {
			return fmt.Errorf("%w: Standard authoring 1.6.0 stage %q must consume current %q", errInvalidCatalog, contract.stage, contract.artifact)
		}
	}
	analysis, present := stages[workflowkit.StageKey(TestsAnalysis)]
	if !present || !reflect.DeepEqual(analysis.Verdicts, passOnly()) {
		return fmt.Errorf("%w: Standard authoring 1.6.0 tests_analysis must submit pass-only evidence", errInvalidCatalog)
	}
	return nil
}

func validateStandardAuthoringHarnessContract(stages map[workflowkit.StageKey]StageDefinition) error {
	if err := validateStandardAuthoringRepairFeedbackContract(stages); err != nil {
		return err
	}

	build, present := stages[workflowkit.StageKey(DockerfileBuildValidate)]
	if !present || build.Effect != workflowkit.EffectContentMutator || !reflect.DeepEqual(build.Verdicts, passOnly()) ||
		!stageHasArtifact(build.Inputs, "dockerfile", "harbor.artifact.v1") ||
		!stageHasArtifact(build.Outputs, StandardAuthoringValidatedDockerfileArtifact, "harbor.artifact.v1") ||
		!stageHasArtifact(build.Outputs, StandardAuthoringDockerfileBuildReportArtifact, StandardAuthoringDockerfileBuildReportSchemaVersion) ||
		stageCatalogResourceCount(build.ReadSet, resourceTaskEnvironment) != 1 ||
		stageCatalogResourceCount(build.WriteSet, resourceTaskValidatedEnvironment) != 1 ||
		stageCatalogResourceCount(build.WriteSet, resourceEvidenceAuthoringDockerBuild) != 1 {
		return fmt.Errorf("%w: Standard authoring 1.7.0 Dockerfile build validator contract is invalid", errInvalidCatalog)
	}
	if inputs, exact := stageCatalogEnvironmentPolicyInputCounts(build.Inputs); inputs != 1 || exact != 1 || stageCatalogResourceCount(build.ReadSet, resourceAuthoringEnvironmentPolicy) != 1 {
		return fmt.Errorf("%w: Standard authoring 1.7.0 Dockerfile build validator must consume the frozen environment policy", errInvalidCatalog)
	}
	if inputs, exact := stageCatalogBriefInputCounts(build.Inputs); inputs != 1 || exact != 1 || stageCatalogResourceCount(build.ReadSet, resourceAuthoringBrief) != 1 {
		return fmt.Errorf("%w: Standard authoring 1.7.0 Dockerfile build validator must consume the frozen brief", errInvalidCatalog)
	}

	harness, present := stages[workflowkit.StageKey(AuthoringHarness)]
	if !present || harness.Effect != workflowkit.EffectContentMutator || !reflect.DeepEqual(harness.Verdicts, passOnly()) ||
		!stageHasArtifact(harness.Inputs, StandardAuthoringValidatedDockerfileArtifact, "harbor.artifact.v1") ||
		!stageHasArtifact(harness.Inputs, "solve_script", "harbor.artifact.v1") ||
		!stageHasArtifact(harness.Inputs, "test_script", "harbor.artifact.v1") ||
		!stageHasArtifact(harness.Inputs, StandardAuthoringDockerfileBuildReportArtifact, StandardAuthoringDockerfileBuildReportSchemaVersion) ||
		!stageHasArtifact(harness.Outputs, StandardAuthoringValidatedSolveScriptArtifact, "harbor.artifact.v1") ||
		!stageHasArtifact(harness.Outputs, StandardAuthoringValidatedTestScriptArtifact, "harbor.artifact.v1") ||
		!stageHasArtifact(harness.Outputs, StandardAuthoringHarnessReportArtifact, StandardAuthoringHarnessReportSchemaVersion) ||
		stageCatalogResourceCount(harness.WriteSet, resourceTaskValidatedSolution) != 1 ||
		stageCatalogResourceCount(harness.WriteSet, resourceTaskValidatedTests) != 1 ||
		stageCatalogResourceCount(harness.WriteSet, resourceEvidenceAuthoringHarness) != 1 {
		return fmt.Errorf("%w: Standard authoring 1.7.0 authoring harness contract is invalid", errInvalidCatalog)
	}
	if inputs, exact := stageCatalogEnvironmentPolicyInputCounts(harness.Inputs); inputs != 1 || exact != 1 || stageCatalogResourceCount(harness.ReadSet, resourceAuthoringEnvironmentPolicy) != 1 {
		return fmt.Errorf("%w: Standard authoring 1.7.0 authoring harness must consume the frozen environment policy", errInvalidCatalog)
	}
	if inputs, exact := stageCatalogBriefInputCounts(harness.Inputs); inputs != 1 || exact != 1 || stageCatalogResourceCount(harness.ReadSet, resourceAuthoringBrief) != 1 {
		return fmt.Errorf("%w: Standard authoring 1.7.0 authoring harness must consume the frozen brief", errInvalidCatalog)
	}

	for _, stageKey := range []workflowkit.StageKey{workflowkit.StageKey(DockerfileBuildValidate), workflowkit.StageKey(AuthoringHarness)} {
		stage := stages[stageKey]
		for _, feedback := range standardAuthoringRepairFeedbackForStage(stageKey) {
			actual, found := stageArtifactSpec(stage.Inputs, feedback.input.Name)
			if !found || actual != feedback.input || stageCatalogResourceCount(stage.ReadSet, feedback.resource) != 1 {
				return fmt.Errorf("%w: Standard authoring 1.7.0 stage %q repair feedback is incomplete", errInvalidCatalog, stageKey)
			}
		}
	}

	for _, contract := range []struct {
		stage    workflowkit.StageKey
		artifact string
		resource workflowkit.ResourceKey
	}{
		{stage: workflowkit.StageKey(ContentReview), artifact: StandardAuthoringValidatedDockerfileArtifact, resource: resourceTaskValidatedEnvironment},
		{stage: workflowkit.StageKey(SolveGen), artifact: StandardAuthoringValidatedDockerfileArtifact, resource: resourceTaskValidatedEnvironment},
		{stage: workflowkit.StageKey(TestGen), artifact: StandardAuthoringValidatedDockerfileArtifact, resource: resourceTaskValidatedEnvironment},
		{stage: workflowkit.StageKey(TestsAnalysis), artifact: StandardAuthoringValidatedTestScriptArtifact, resource: resourceTaskValidatedTests},
		{stage: workflowkit.StageKey(CodeEdgePackageAdmission), artifact: StandardAuthoringValidatedDockerfileArtifact, resource: resourceTaskValidatedEnvironment},
		{stage: workflowkit.StageKey(CodeEdgePackageAdmission), artifact: StandardAuthoringValidatedSolveScriptArtifact, resource: resourceTaskValidatedSolution},
		{stage: workflowkit.StageKey(CodeEdgePackageAdmission), artifact: StandardAuthoringValidatedTestScriptArtifact, resource: resourceTaskValidatedTests},
		{stage: workflowkit.StageKey(SolutionReview), artifact: StandardAuthoringValidatedSolveScriptArtifact, resource: resourceTaskValidatedSolution},
		{stage: workflowkit.StageKey(SolutionReview), artifact: StandardAuthoringValidatedTestScriptArtifact, resource: resourceTaskValidatedTests},
		{stage: workflowkit.StageKey(MaterializeTask), artifact: StandardAuthoringValidatedDockerfileArtifact, resource: resourceTaskValidatedEnvironment},
		{stage: workflowkit.StageKey(MaterializeTask), artifact: StandardAuthoringValidatedSolveScriptArtifact, resource: resourceTaskValidatedSolution},
		{stage: workflowkit.StageKey(MaterializeTask), artifact: StandardAuthoringValidatedTestScriptArtifact, resource: resourceTaskValidatedTests},
	} {
		stage, found := stages[contract.stage]
		if !found || !stageHasArtifact(stage.Inputs, contract.artifact, "harbor.artifact.v1") || stageCatalogResourceCount(stage.ReadSet, contract.resource) != 1 {
			return fmt.Errorf("%w: Standard authoring 1.7.0 stage %q must consume %q", errInvalidCatalog, contract.stage, contract.artifact)
		}
	}

	for _, key := range []workflowkit.StageKey{
		workflowkit.StageKey(ContentReview), workflowkit.StageKey(SolveGen), workflowkit.StageKey(TestGen),
		workflowkit.StageKey(TestsAnalysis), workflowkit.StageKey(CodeEdgePackageAdmission),
		workflowkit.StageKey(SolutionReview), workflowkit.StageKey(MaterializeTask),
	} {
		stage := stages[key]
		for _, legacy := range []string{"dockerfile", "solve_script", "test_script"} {
			if _, found := stageArtifactSpec(stage.Inputs, legacy); found {
				return fmt.Errorf("%w: Standard authoring 1.7.0 stage %q retained ambiguous legacy input %q", errInvalidCatalog, key, legacy)
			}
		}
	}

	analysis := stages[workflowkit.StageKey(TestsAnalysis)]
	if !reflect.DeepEqual(analysis.Verdicts, passOnly()) || !stageHasArtifact(analysis.Inputs, StandardAuthoringHarnessReportArtifact, StandardAuthoringHarnessReportSchemaVersion) || stageCatalogResourceCount(analysis.ReadSet, resourceEvidenceAuthoringHarness) != 1 {
		return fmt.Errorf("%w: Standard authoring 1.7.0 tests_analysis must consume pass-only harness evidence", errInvalidCatalog)
	}
	for _, key := range []workflowkit.StageKey{workflowkit.StageKey(CodeEdgePackageAdmission), workflowkit.StageKey(MaterializeTask)} {
		stage := stages[key]
		if !stageHasArtifact(stage.Inputs, StandardAuthoringDockerfileBuildReportArtifact, StandardAuthoringDockerfileBuildReportSchemaVersion) ||
			!stageHasArtifact(stage.Inputs, StandardAuthoringHarnessReportArtifact, StandardAuthoringHarnessReportSchemaVersion) {
			return fmt.Errorf("%w: Standard authoring 1.7.0 stage %q must bind both validation reports", errInvalidCatalog, key)
		}
	}
	return nil
}

// validateStandardAuthoringFixedFileContract deliberately layers the 1.8.0
// submission boundary onto the unchanged 1.7.0 ReAct/Docker topology. Stage
// descriptors do not carry a workspace path; the pass-only solve/test policy
// is the catalog-level proof that their host-owned fixed-file submission has
// no model-authored reject/repair payload variant.
func validateStandardAuthoringFixedFileContract(stages map[workflowkit.StageKey]StageDefinition) error {
	if err := validateStandardAuthoringHarnessContract(stages); err != nil {
		return err
	}
	for _, contract := range []struct {
		key    workflowkit.StageKey
		output string
	}{
		{key: workflowkit.StageKey(SolveGen), output: "solve_script"},
		{key: workflowkit.StageKey(TestGen), output: "test_script"},
	} {
		stage, present := stages[contract.key]
		if !present || !reflect.DeepEqual(stage.Verdicts, passOnly()) || len(stage.Outputs) != 1 ||
			stage.Outputs[0].Name != contract.output || !stage.Outputs[0].Required || stage.Outputs[0].SchemaVersion != "harbor.artifact.v1" {
			return fmt.Errorf("%w: Standard authoring 1.8.0 fixed-file stage %q contract is invalid", errInvalidCatalog, contract.key)
		}
	}
	return nil
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
