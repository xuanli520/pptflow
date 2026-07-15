package workflowadapter

import "github.com/purplevoid/harbor-factory/pkg/workflowkit"

const (
	// StandardAuthoringWorkflowTemplateID and Version identify the closed
	// pre-materialization workflow. Its subject is an immutable
	// AuthoringSession/source snapshot, not an empty TaskRevision. The final
	// materialize_task stage atomically creates the first task revision and
	// emits an explicit handoff for a separate task-bound CodeEdge Phase-1 Run.
	StandardAuthoringWorkflowTemplateID      = "harbor.standard-authoring"
	StandardAuthoringWorkflowTemplateVersion = "1.0.0"

	standardAuthoringCatalogID      = "harbor.standard-authoring-stage-catalog"
	standardAuthoringCatalogVersion = "1.0.0"

	// StandardAuthoringTaskHandoffArtifact is emitted only by materialize_task.
	// It is a receipt for a newly sealed task revision, not a mutable workspace
	// path and not an authorization to continue task-bound work in the source
	// AuthoringSession Run.
	StandardAuthoringTaskHandoffArtifact      = "authoring_task_handoff"
	StandardAuthoringTaskHandoffSchemaVersion = "harbor.authoring-task-handoff.v1"
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

// IsStandardAuthoringWorkflowTemplate reports whether a Run is bound to the
// immutable-source authoring half of the lifecycle.
func IsStandardAuthoringWorkflowTemplate(reference TemplateReference) bool {
	return reference.Equal(StandardAuthoringTemplateReference())
}

// StandardAuthoringStageOrder returns the dependency-aware closed stage list.
func StandardAuthoringStageOrder() []workflowkit.StageKey {
	return append([]workflowkit.StageKey(nil), standardAuthoringStageOrder...)
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

// StandardAuthoringStageCatalog derives from the canonical StandardStageCatalog.
// The first 13 stages (repo_prepare through solution_review) are identical to the
// Standard catalog; only materialize_task is overridden to emit the mandatory
// immutable task-handoff receipt.
func StandardAuthoringStageCatalog() StageCatalog {
	canonical := StandardStageCatalog()
	stages := make([]StageDefinition, 0, len(canonical.Stages))
	stages = append(stages, canonical.Stages[:13]...)
	stages = append(stages,
		stage(MaterializeTask, StageTaskGeneration, []string{SolutionReview}, "harborfactory.materialize_task",
			[]workflowkit.ResourceKey{resourceTaskInstruction, resourceTaskMetadata, resourceTaskEnvironment, resourceTaskSolution, resourceTaskTests, resourceTaskTestsAnalysis, resourceReviewSolutionVerifier},
			[]workflowkit.ResourceKey{resourceTaskSnapshot, resourceTaskDigest, resourceTaskInstruction, resourceTaskMetadata, resourceTaskEnvironment, resourceTaskSolution, resourceTaskTests, resourceTaskTestsAnalysis, resourceAuthoringTaskHandoff},
			workflowkit.EffectContentMutator, 1, contentVerdicts(),
			artifactInput("instruction"), artifactInput("task_toml"), artifactInput("dockerfile"),
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
