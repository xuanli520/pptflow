package workflowadapter

import (
	"fmt"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	// StandardAuthoringWorkflowTemplateID and Version identify the closed
	// pre-materialization workflow. Its subject is an immutable
	// AuthoringSession/source snapshot, not an empty TaskRevision. The final
	// materialize_task stage atomically creates the first task revision and
	// emits an explicit handoff for a separate task-bound CodeEdge Phase-1 Run.
	StandardAuthoringWorkflowTemplateID      = "harbor.standard-authoring"
	StandardAuthoringWorkflowTemplateVersion = "1.2.0"

	// StandardAuthoringTaskAdmissionTemplateVersion is reserved for the
	// additive Authoring contract release. Keeping it distinct from 1.2.0
	// prevents a new production binary from reinterpreting a frozen legacy Run.
	StandardAuthoringTaskAdmissionTemplateVersion = "1.3.0"

	standardAuthoringCatalogID      = "harbor.standard-authoring-stage-catalog"
	standardAuthoringCatalogVersion = "1.2.0"

	// StandardAuthoringTaskDesignMaxTurns bounds the task-design Codex
	// conversation in this source-session template. It intentionally does not
	// alter the task-bound Standard catalog, whose task_design stage remains a
	// separate workflow contract.
	StandardAuthoringTaskDesignMaxTurns = 30

	// StandardAuthoringTaskHandoffArtifact is emitted only by materialize_task.
	// It is a receipt for a newly sealed task revision, not a mutable workspace
	// path and not an authorization to continue task-bound work in the source
	// AuthoringSession Run.
	StandardAuthoringTaskHandoffArtifact               = "authoring_task_handoff"
	StandardAuthoringTaskHandoffSchemaVersion          = "harbor.authoring-task-handoff.v1"
	StandardAuthoringTaskAdmissionHandoffSchemaVersion = "harbor.authoring-task-handoff.v2"
)

var standardAuthoringStageOrder = []workflowkit.StageKey{
	workflowkit.StageKey(RepoPrepare),
	workflowkit.StageKey(RepoAnalyze),
	workflowkit.StageKey(TaskDesign),
	workflowkit.StageKey(TaskReview),
	workflowkit.StageKey(GenerateTaskFiles),
	workflowkit.StageKey(InstructionGen),
	workflowkit.StageKey(TaskTOMLGen),
	workflowkit.StageKey(DockerfileGen),
	workflowkit.StageKey(ContentReview),
	workflowkit.StageKey(SolveGen),
	workflowkit.StageKey(TestGen),
	workflowkit.StageKey(TestsAnalysis),
	workflowkit.StageKey(SolutionReview),
	workflowkit.StageKey(MaterializeTask),
}

var standardAuthoringGroups = []StageGroup{
	StageSourcePrepare,
	StageTaskAnalysis,
	StageTaskDesign,
	StageTaskGeneration,
}

// StandardAuthoringTemplateReference returns the exact closed pre-task
// authoring template. It must never be treated as a partial spelling of the
// full Standard lifecycle template.
func StandardAuthoringTemplateReference() TemplateReference {
	return TemplateReference{ID: StandardAuthoringWorkflowTemplateID, Version: StandardAuthoringWorkflowTemplateVersion}
}

func StandardAuthoringTaskAdmissionTemplateReference() TemplateReference {
	return TemplateReference{ID: StandardAuthoringWorkflowTemplateID, Version: StandardAuthoringTaskAdmissionTemplateVersion}
}

// IsStandardAuthoringWorkflowTemplate reports whether a Run is bound to the
// immutable-source authoring half of the lifecycle.
func IsStandardAuthoringWorkflowTemplate(reference TemplateReference) bool {
	return reference.Equal(StandardAuthoringTemplateReference()) ||
		reference.Equal(StandardAuthoringTaskAdmissionTemplateReference())
}

// StandardAuthoringStageOrder returns the dependency-aware closed stage list.
func StandardAuthoringStageOrder() []workflowkit.StageKey {
	return append([]workflowkit.StageKey(nil), standardAuthoringStageOrder...)
}

// StandardAuthoringTaskAdmissionStageOrder is the additive 1.3.0 topology.
// Keep this distinct from the legacy order so old deployment catalogs cannot
// accidentally acquire a new required operation.
func StandardAuthoringTaskAdmissionStageOrder() []workflowkit.StageKey {
	order := StandardAuthoringStageOrder()
	for index, key := range order {
		if key == workflowkit.StageKey(SolutionReview) {
			return append(order[:index], append([]workflowkit.StageKey{workflowkit.StageKey(CodeEdgePackageAdmission)}, order[index:]...)...)
		}
	}
	panic("Standard authoring stage order omits solution_review")
}

// StandardAuthoringStageOrderForTemplate exposes the exact closed operation
// set for each installed Authoring template version.
func StandardAuthoringStageOrderForTemplate(reference TemplateReference) ([]workflowkit.StageKey, error) {
	switch {
	case reference.Equal(StandardAuthoringTemplateReference()):
		return StandardAuthoringStageOrder(), nil
	case reference.Equal(StandardAuthoringTaskAdmissionTemplateReference()):
		return StandardAuthoringTaskAdmissionStageOrder(), nil
	default:
		return nil, fmt.Errorf("Standard authoring template %s@%s is not installed", reference.ID, reference.Version)
	}
}

func standardAuthoringStageGroups() []StageGroup {
	return append([]StageGroup(nil), standardAuthoringGroups...)
}

func standardAuthoringDependencies() map[workflowkit.StageKey][]workflowkit.StageKey {
	return map[workflowkit.StageKey][]workflowkit.StageKey{
		workflowkit.StageKey(RepoPrepare):       nil,
		workflowkit.StageKey(RepoAnalyze):       {workflowkit.StageKey(RepoPrepare)},
		workflowkit.StageKey(TaskDesign):        {workflowkit.StageKey(RepoAnalyze)},
		workflowkit.StageKey(TaskReview):        {workflowkit.StageKey(TaskDesign)},
		workflowkit.StageKey(GenerateTaskFiles): {workflowkit.StageKey(TaskReview)},
		workflowkit.StageKey(InstructionGen):    {workflowkit.StageKey(GenerateTaskFiles)},
		workflowkit.StageKey(TaskTOMLGen):       {workflowkit.StageKey(GenerateTaskFiles)},
		workflowkit.StageKey(DockerfileGen):     {workflowkit.StageKey(GenerateTaskFiles)},
		workflowkit.StageKey(ContentReview): {
			workflowkit.StageKey(InstructionGen), workflowkit.StageKey(TaskTOMLGen), workflowkit.StageKey(DockerfileGen),
		},
		workflowkit.StageKey(SolveGen):      {workflowkit.StageKey(ContentReview)},
		workflowkit.StageKey(TestGen):       {workflowkit.StageKey(ContentReview)},
		workflowkit.StageKey(TestsAnalysis): {workflowkit.StageKey(ContentReview)},
		workflowkit.StageKey(SolutionReview): {
			workflowkit.StageKey(SolveGen), workflowkit.StageKey(TestGen), workflowkit.StageKey(TestsAnalysis),
		},
		workflowkit.StageKey(MaterializeTask): {workflowkit.StageKey(SolutionReview)},
	}
}

func standardAuthoringTaskAdmissionDependencies() map[workflowkit.StageKey][]workflowkit.StageKey {
	dependencies := standardAuthoringDependencies()
	dependencies[workflowkit.StageKey(CodeEdgePackageAdmission)] = []workflowkit.StageKey{
		workflowkit.StageKey(SolveGen), workflowkit.StageKey(TestGen), workflowkit.StageKey(TestsAnalysis),
	}
	dependencies[workflowkit.StageKey(SolutionReview)] = []workflowkit.StageKey{
		workflowkit.StageKey(SolveGen), workflowkit.StageKey(TestGen), workflowkit.StageKey(CodeEdgePackageAdmission),
	}
	return dependencies
}

// StandardAuthoringWorkflowTemplate returns the source-session half of task
// creation. It intentionally ends at materialize_task: verification, model
// evaluation, compliance, and packaging must run in a fresh task-bound child
// Run after the immutable handoff has been persisted.
func StandardAuthoringWorkflowTemplate() WorkflowTemplate {
	return WorkflowTemplate{
		ID:          StandardAuthoringWorkflowTemplateID,
		Version:     StandardAuthoringWorkflowTemplateVersion,
		Catalog:     StandardAuthoringStageCatalog(),
		QuotaPolicy: StandardAuthoringQuotaPolicy(),
	}
}

func StandardAuthoringTaskAdmissionWorkflowTemplate() WorkflowTemplate {
	return WorkflowTemplate{ID: StandardAuthoringWorkflowTemplateID, Version: StandardAuthoringTaskAdmissionTemplateVersion, Catalog: StandardAuthoringTaskAdmissionStageCatalog(), QuotaPolicy: StandardAuthoringTaskAdmissionQuotaPolicy()}
}

func StandardAuthoringTaskAdmissionStageCatalog() StageCatalog {
	base := StandardAuthoringStageCatalog()
	stages := make([]StageDefinition, 0, len(base.Stages)+1)
	for _, definition := range base.Stages {
		definition = definition.Clone()
		if definition.Key == workflowkit.StageKey(SolutionReview) {
			stages = append(stages, stage(CodeEdgePackageAdmission, StageTaskGeneration, []string{SolveGen, TestGen, TestsAnalysis}, "harborfactory.codeedge_package_admission", []workflowkit.ResourceKey{resourceTaskInstruction, resourceTaskMetadata, resourceTaskEnvironment, resourceTaskSolution, resourceTaskTests, resourceTaskTestsAnalysis, resourceAuthoringEnvironmentPolicy}, []workflowkit.ResourceKey{resourceAuthoringTaskAdmission}, workflowkit.EffectEvidenceOnly, 1, contentVerdicts(), artifactInput("instruction"), artifactInput("task_toml"), artifactInput("dockerfile"), standardAuthoringEnvironmentPolicyInput(), artifactInput("solve_script"), artifactInput("test_script"), artifactInput("tests_analysis"), artifactOutputWithSchema("codeedge_package_admission_report", "harbor.standard-authoring-task-package-admission.v1")))
			definition.Dependencies = []workflowkit.StageKey{workflowkit.StageKey(SolveGen), workflowkit.StageKey(TestGen), workflowkit.StageKey(CodeEdgePackageAdmission)}
			definition.Inputs = append(definition.Inputs, artifactInputWithSchema("codeedge_package_admission_report", "harbor.standard-authoring-task-package-admission.v1").spec)
			definition.ReadSet = append(definition.ReadSet, resourceAuthoringTaskAdmission)
		}
		if definition.Key == workflowkit.StageKey(MaterializeTask) {
			definition.Inputs = append(definition.Inputs, artifactInputWithSchema("codeedge_package_admission_report", "harbor.standard-authoring-task-package-admission.v1").spec)
			definition.ReadSet = append(definition.ReadSet, resourceAuthoringTaskAdmission)
			for index := range definition.Outputs {
				if definition.Outputs[index].Name == StandardAuthoringTaskHandoffArtifact {
					definition.Outputs[index].SchemaVersion = StandardAuthoringTaskAdmissionHandoffSchemaVersion
				}
			}
		}
		stages = append(stages, definition)
	}
	return StageCatalog{Template: StandardAuthoringTaskAdmissionTemplateReference(), ID: standardAuthoringCatalogID, Version: StandardAuthoringTaskAdmissionTemplateVersion, Stages: stages}
}

func StandardAuthoringTaskHandoffSchemaForTemplate(reference TemplateReference) (string, error) {
	switch {
	case reference.Equal(StandardAuthoringTemplateReference()):
		return StandardAuthoringTaskHandoffSchemaVersion, nil
	case reference.Equal(StandardAuthoringTaskAdmissionTemplateReference()):
		return StandardAuthoringTaskAdmissionHandoffSchemaVersion, nil
	default:
		return "", fmt.Errorf("Standard authoring handoff schema has no template %s@%s", reference.ID, reference.Version)
	}
}

// StandardAuthoringStageCatalog derives the source-session catalog from the
// canonical StandardStageCatalog. It overlays the required immutable
// environment-policy input before materialize_task emits the handoff receipt.
func StandardAuthoringStageCatalog() StageCatalog {
	canonical := StandardStageCatalog()
	stages := make([]StageDefinition, 0, len(canonical.Stages))
	for _, definition := range canonical.Stages[:13] {
		definition = definition.Clone()
		if definition.Key == workflowkit.StageKey(TaskDesign) {
			definition.RequiredTurns = StandardAuthoringTaskDesignMaxTurns
		}
		if standardAuthoringStageUsesEnvironmentPolicy(definition.Key) {
			definition.Inputs = append(definition.Inputs, standardAuthoringEnvironmentPolicyInput().spec)
			definition.ReadSet = append(definition.ReadSet, resourceAuthoringEnvironmentPolicy)
		}
		stages = append(stages, definition)
	}
	stages = append(stages,
		stage(MaterializeTask, StageTaskGeneration, []string{SolutionReview}, "harborfactory.materialize_task",
			[]workflowkit.ResourceKey{resourceTaskInstruction, resourceTaskMetadata, resourceTaskEnvironment, resourceTaskSolution, resourceTaskTests, resourceTaskTestsAnalysis, resourceReviewSolutionVerifier, resourceAuthoringEnvironmentPolicy},
			[]workflowkit.ResourceKey{resourceTaskSnapshot, resourceTaskDigest, resourceTaskInstruction, resourceTaskMetadata, resourceTaskEnvironment, resourceTaskSolution, resourceTaskTests, resourceTaskTestsAnalysis, resourceAuthoringTaskHandoff},
			workflowkit.EffectContentMutator, 1, contentVerdicts(),
			artifactInput("instruction"), artifactInput("task_toml"), artifactInput("dockerfile"), standardAuthoringEnvironmentPolicyInput(),
			artifactInput("solve_script"), artifactInput("test_script"), artifactInput("tests_analysis"),
			reviewDecisionInput("solution_review_decision"),
			artifactOutput("task_snapshot"), artifactOutput("task_digest"),
			artifactOutputWithSchema(StandardAuthoringTaskHandoffArtifact, StandardAuthoringTaskHandoffSchemaVersion),
		),
	)
	return StageCatalog{
		Template: StandardAuthoringTemplateReference(),
		ID:       standardAuthoringCatalogID,
		Version:  standardAuthoringCatalogVersion,
		Stages:   stages,
	}
}

func standardAuthoringStageUsesEnvironmentPolicy(key workflowkit.StageKey) bool {
	switch key {
	case workflowkit.StageKey(TaskDesign), workflowkit.StageKey(GenerateTaskFiles), workflowkit.StageKey(DockerfileGen), workflowkit.StageKey(ContentReview):
		return true
	default:
		return false
	}
}

func standardAuthoringEnvironmentPolicyInput() stageArtifact {
	return artifactInputWithSchema(StandardAuthoringEnvironmentPolicyArtifact, StandardAuthoringEnvironmentPolicySchemaVersion)
}
