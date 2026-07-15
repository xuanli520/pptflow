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

// StandardAuthoringStageCatalog defines the complete closed pre-materialize
// topology. It intentionally reuses the same typed stage bindings and
// artifact names as the corresponding Standard stages, except that
// materialize_task emits a mandatory immutable task-handoff receipt.
func StandardAuthoringStageCatalog() StageCatalog {
	return StageCatalog{
		Template: StandardAuthoringTemplateReference(),
		ID:       standardAuthoringCatalogID,
		Version:  standardAuthoringCatalogVersion,
		Stages: []StageDefinition{
			stage(RepoPrepare, StageSourcePrepare, nil, "harborfactory.repo_prepare", []workflowkit.ResourceKey{resourceSourceRepository}, []workflowkit.ResourceKey{resourceSourceSnapshot, resourceEvidenceRepoPrepare}, workflowkit.EffectEvidenceOnly, 1, passOnly(), artifactOutput("repo_prepared")),
			stage(RepoAnalyze, StageTaskAnalysis, []string{RepoPrepare}, "harborfactory.repo_analyze", []workflowkit.ResourceKey{resourceSourceSnapshot}, []workflowkit.ResourceKey{resourceAnalysisRepository}, workflowkit.EffectEvidenceOnly, 3, passOnly(), artifactInput("repo_prepared"), artifactOutput("repo_analysis")),
			stage(TaskDesign, StageTaskDesign, []string{RepoAnalyze}, "harborfactory.task_design", []workflowkit.ResourceKey{resourceSourceSnapshot, resourceAnalysisRepository}, []workflowkit.ResourceKey{resourceTaskDesign}, workflowkit.EffectContentProducer, 3, contentVerdicts(), artifactInput("repo_prepared"), artifactInput("repo_analysis"), artifactOutput("task_proposal")),
			gateStage(TaskReview, StageTaskDesign, []string{TaskDesign}, ReviewTaskDirection, []workflowkit.ResourceKey{resourceAnalysisRepository, resourceTaskDesign}, []workflowkit.ResourceKey{resourceReviewTaskDirection}, artifactInput("repo_analysis"), artifactInput("task_proposal")),
			stage(GenerateTaskFiles, StageTaskGeneration, []string{TaskReview}, "harborfactory.generate_task_files", []workflowkit.ResourceKey{resourceSourceSnapshot, resourceAnalysisRepository, resourceTaskDesign, resourceReviewTaskDirection}, []workflowkit.ResourceKey{resourceTaskGeneratedFiles}, workflowkit.EffectContentProducer, 3, contentVerdicts(), artifactInput("repo_prepared"), artifactInput("repo_analysis"), artifactInput("task_proposal"), reviewDecisionInput("task_review_decision"), artifactOutput("generated_task_files")),
			stage(InstructionGen, StageTaskGeneration, []string{GenerateTaskFiles}, "harborfactory.instruction_generate", []workflowkit.ResourceKey{resourceTaskGeneratedFiles}, []workflowkit.ResourceKey{resourceTaskInstruction}, workflowkit.EffectContentProducer, 1, contentVerdicts(), artifactInput("generated_task_files"), artifactOutput("instruction")),
			stage(TaskTOMLGen, StageTaskGeneration, []string{GenerateTaskFiles}, "harborfactory.task_toml_generate", []workflowkit.ResourceKey{resourceTaskGeneratedFiles, resourceTaskDesign}, []workflowkit.ResourceKey{resourceTaskMetadata}, workflowkit.EffectContentProducer, 1, contentVerdicts(), artifactInput("generated_task_files"), artifactInput("task_proposal"), artifactOutput("task_toml")),
			stage(DockerfileGen, StageTaskGeneration, []string{GenerateTaskFiles}, "harborfactory.dockerfile_generate", []workflowkit.ResourceKey{resourceSourceSnapshot, resourceTaskDesign}, []workflowkit.ResourceKey{resourceTaskEnvironment}, workflowkit.EffectContentProducer, 1, contentVerdicts(), artifactInput("repo_prepared"), artifactInput("task_proposal"), artifactOutput("dockerfile")),
			gateStage(ContentReview, StageTaskGeneration, []string{InstructionGen, TaskTOMLGen, DockerfileGen}, ReviewContent, []workflowkit.ResourceKey{resourceTaskInstruction, resourceTaskMetadata, resourceTaskEnvironment}, []workflowkit.ResourceKey{resourceReviewContent}, artifactInput("instruction"), artifactInput("task_toml"), artifactInput("dockerfile")),
			stage(SolveGen, StageTaskGeneration, []string{ContentReview}, "harborfactory.solve_generate", []workflowkit.ResourceKey{resourceTaskGeneratedFiles}, []workflowkit.ResourceKey{resourceTaskSolution}, workflowkit.EffectContentProducer, 1, contentVerdicts(), artifactInput("generated_task_files"), artifactOutput("solve_script")),
			stage(TestGen, StageTaskGeneration, []string{ContentReview}, "harborfactory.test_generate", []workflowkit.ResourceKey{resourceTaskGeneratedFiles}, []workflowkit.ResourceKey{resourceTaskTests}, workflowkit.EffectContentProducer, 1, contentVerdicts(), artifactInput("generated_task_files"), artifactOutput("test_script")),
			stage(TestsAnalysis, StageTaskGeneration, []string{ContentReview}, "harborfactory.tests_analysis", []workflowkit.ResourceKey{resourceTaskGeneratedFiles, resourceTaskDesign}, []workflowkit.ResourceKey{resourceTaskTestsAnalysis}, workflowkit.EffectContentProducer, 1, contentVerdicts(), artifactInput("generated_task_files"), artifactInput("task_proposal"), artifactOutput("tests_analysis")),
			gateStage(SolutionReview, StageTaskGeneration, []string{SolveGen, TestGen, TestsAnalysis}, ReviewSolutionVerifier, []workflowkit.ResourceKey{resourceTaskInstruction, resourceTaskSolution, resourceTaskTests, resourceTaskTestsAnalysis}, []workflowkit.ResourceKey{resourceReviewSolutionVerifier}, artifactInput("instruction"), artifactInput("solve_script"), artifactInput("test_script"), artifactInput("tests_analysis")),
			stage(MaterializeTask, StageTaskGeneration, []string{SolutionReview}, "harborfactory.materialize_task", []workflowkit.ResourceKey{resourceTaskInstruction, resourceTaskMetadata, resourceTaskEnvironment, resourceTaskSolution, resourceTaskTests, resourceTaskTestsAnalysis, resourceReviewSolutionVerifier}, []workflowkit.ResourceKey{resourceTaskSnapshot, resourceTaskDigest, resourceTaskInstruction, resourceTaskMetadata, resourceTaskEnvironment, resourceTaskSolution, resourceTaskTests, resourceTaskTestsAnalysis, resourceAuthoringTaskHandoff}, workflowkit.EffectContentMutator, 1, contentVerdicts(), artifactInput("instruction"), artifactInput("task_toml"), artifactInput("dockerfile"), artifactInput("solve_script"), artifactInput("test_script"), artifactInput("tests_analysis"), reviewDecisionInput("solution_review_decision"), artifactOutput("task_snapshot"), artifactOutput("task_digest"), artifactOutputWithSchema(StandardAuthoringTaskHandoffArtifact, StandardAuthoringTaskHandoffSchemaVersion)),
		},
	}
}
