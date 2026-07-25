package workflowadapter

import (
	"fmt"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	// StandardAuthoringWorkflowTemplateID identifies the closed source-session
	// workflow. It materializes one immutable task revision before handing off to
	// the separate task-bound CodeEdge workflow.
	StandardAuthoringWorkflowTemplateID = "harbor.standard-authoring"

	// StandardAuthoringContractTemplateVersion is the only supported Standard
	// Authoring template. The immutable root contract is the sole source of
	// caller-selected task direction, source identity, and environment policy.
	StandardAuthoringContractTemplateVersion = "2.0.0"

	standardAuthoringCatalogID      = "harbor.standard-authoring-stage-catalog"
	standardAuthoringCatalogVersion = StandardAuthoringContractTemplateVersion

	// StandardAuthoringTaskDesignMaxTurns bounds the task-design Codex
	// conversation in the source-session workflow. It intentionally does not
	// alter the task-bound Standard catalog.
	StandardAuthoringTaskDesignMaxTurns = 30

	// StandardAuthoringTaskHandoffArtifact is emitted only by materialize_task.
	// It is a receipt for a sealed task revision, never a mutable workspace path
	// or authority to continue task-bound work under the source session.
	StandardAuthoringTaskHandoffArtifact      = "authoring_task_handoff"
	StandardAuthoringTaskHandoffSchemaVersion = "harbor.authoring-task-handoff.v2"

	StandardAuthoringValidatedDockerfileArtifact        = "validated_dockerfile"
	StandardAuthoringDockerfileBuildReportArtifact      = "dockerfile_build_report"
	StandardAuthoringDockerfileBuildReportSchemaVersion = "harbor.standard-authoring-dockerfile-build-report.v1"
	StandardAuthoringValidatedSolveScriptArtifact       = "validated_solve_script"
	StandardAuthoringValidatedTestScriptArtifact        = "validated_test_script"
	StandardAuthoringHarnessReportArtifact              = "authoring_harness_report"
	StandardAuthoringHarnessReportSchemaVersion         = "harbor.standard-authoring-harness-report.v1"

	standardAuthoringPackageAdmissionReportArtifact      = "codeedge_package_admission_report"
	standardAuthoringPackageAdmissionReportSchemaVersion = "harbor.standard-authoring-task-package-admission.v1"
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
	workflowkit.StageKey(DockerfileBuildValidate),
	workflowkit.StageKey(ContentReview),
	workflowkit.StageKey(SolveGen),
	workflowkit.StageKey(TestGen),
	workflowkit.StageKey(AuthoringHarness),
	workflowkit.StageKey(TestsAnalysis),
	workflowkit.StageKey(CodeEdgePackageAdmission),
	workflowkit.StageKey(SolutionReview),
	workflowkit.StageKey(MaterializeTask),
}

var standardAuthoringGroups = []StageGroup{
	StageSourcePrepare,
	StageTaskAnalysis,
	StageTaskDesign,
	StageTaskGeneration,
}

func StandardAuthoringContractTemplateReference() TemplateReference {
	return TemplateReference{ID: StandardAuthoringWorkflowTemplateID, Version: StandardAuthoringContractTemplateVersion}
}

// StandardAuthoringCurrentTemplateReference returns the only executable
// Standard Authoring template.
func StandardAuthoringCurrentTemplateReference() TemplateReference {
	return StandardAuthoringContractTemplateReference()
}

// IsStandardAuthoringWorkflowTemplate reports whether a Run is bound to the
// immutable-source authoring half of the lifecycle.
func IsStandardAuthoringWorkflowTemplate(reference TemplateReference) bool {
	return reference.Equal(StandardAuthoringContractTemplateReference())
}

// StandardAuthoringStageOrder returns the dependency-aware v2 stage list.
func StandardAuthoringStageOrder() []workflowkit.StageKey {
	return append([]workflowkit.StageKey(nil), standardAuthoringStageOrder...)
}

// StandardAuthoringStageOrderForTemplate exposes the exact closed operation
// set for the single installed Authoring template.
func StandardAuthoringStageOrderForTemplate(reference TemplateReference) ([]workflowkit.StageKey, error) {
	if reference.Equal(StandardAuthoringContractTemplateReference()) {
		return StandardAuthoringStageOrder(), nil
	}
	return nil, fmt.Errorf("Standard authoring template %s@%s is not installed", reference.ID, reference.Version)
}

func standardAuthoringStageGroups() []StageGroup {
	return append([]StageGroup(nil), standardAuthoringGroups...)
}

func standardAuthoringDependencies() map[workflowkit.StageKey][]workflowkit.StageKey {
	return map[workflowkit.StageKey][]workflowkit.StageKey{
		workflowkit.StageKey(RepoPrepare):             nil,
		workflowkit.StageKey(RepoAnalyze):             {workflowkit.StageKey(RepoPrepare)},
		workflowkit.StageKey(TaskDesign):              {workflowkit.StageKey(RepoAnalyze)},
		workflowkit.StageKey(TaskReview):              {workflowkit.StageKey(TaskDesign)},
		workflowkit.StageKey(GenerateTaskFiles):       {workflowkit.StageKey(TaskReview)},
		workflowkit.StageKey(InstructionGen):          {workflowkit.StageKey(GenerateTaskFiles)},
		workflowkit.StageKey(TaskTOMLGen):             {workflowkit.StageKey(GenerateTaskFiles)},
		workflowkit.StageKey(DockerfileGen):           {workflowkit.StageKey(GenerateTaskFiles)},
		workflowkit.StageKey(DockerfileBuildValidate): {workflowkit.StageKey(DockerfileGen)},
		workflowkit.StageKey(ContentReview): {
			workflowkit.StageKey(InstructionGen), workflowkit.StageKey(TaskTOMLGen), workflowkit.StageKey(DockerfileBuildValidate),
		},
		workflowkit.StageKey(SolveGen):         {workflowkit.StageKey(ContentReview)},
		workflowkit.StageKey(TestGen):          {workflowkit.StageKey(ContentReview)},
		workflowkit.StageKey(AuthoringHarness): {workflowkit.StageKey(SolveGen), workflowkit.StageKey(TestGen)},
		workflowkit.StageKey(TestsAnalysis):    {workflowkit.StageKey(AuthoringHarness)},
		workflowkit.StageKey(CodeEdgePackageAdmission): {
			workflowkit.StageKey(AuthoringHarness), workflowkit.StageKey(TestsAnalysis),
		},
		workflowkit.StageKey(SolutionReview): {
			workflowkit.StageKey(AuthoringHarness), workflowkit.StageKey(TestsAnalysis), workflowkit.StageKey(CodeEdgePackageAdmission),
		},
		workflowkit.StageKey(MaterializeTask): {workflowkit.StageKey(SolutionReview)},
	}
}

// StandardAuthoringContractWorkflowTemplate is the complete v2 source-session
// workflow. It directly defines its topology rather than inheriting a retired
// 1.x catalog, so a v2 change cannot reinterpret any historical descriptor.
func StandardAuthoringContractWorkflowTemplate() WorkflowTemplate {
	return WorkflowTemplate{
		ID:          StandardAuthoringWorkflowTemplateID,
		Version:     StandardAuthoringContractTemplateVersion,
		Catalog:     StandardAuthoringContractStageCatalog(),
		QuotaPolicy: StandardAuthoringContractQuotaPolicy(),
	}
}

func StandardAuthoringCurrentWorkflowTemplate() WorkflowTemplate {
	return StandardAuthoringContractWorkflowTemplate()
}

// StandardAuthoringContractStageCatalog directly declares the v2 descriptor.
// Every stage receives the same required immutable root contract. Bounded,
// optional repair evidence is declared only on producers that can be selected
// for a continuation; it is absent during the ordinary happy path.
func StandardAuthoringContractStageCatalog() StageCatalog {
	stages := []StageDefinition{
		stage(RepoPrepare, StageSourcePrepare, nil, "harborfactory.repo_prepare",
			[]workflowkit.ResourceKey{resourceSourceRepository},
			[]workflowkit.ResourceKey{resourceSourceSnapshot, resourceEvidenceRepoPrepare},
			workflowkit.EffectEvidenceOnly, 1, passOnly(), artifactOutput("repo_prepared")),
		stage(RepoAnalyze, StageTaskAnalysis, []string{RepoPrepare}, "harborfactory.repo_analyze",
			[]workflowkit.ResourceKey{resourceSourceSnapshot}, []workflowkit.ResourceKey{resourceAnalysisRepository},
			workflowkit.EffectEvidenceOnly, 3, passOnly(), artifactInput("repo_prepared"), optionalReviewDecisionInput("task_review_decision"), artifactOutput("repo_analysis")),
		stage(TaskDesign, StageTaskDesign, []string{RepoAnalyze}, "harborfactory.task_design",
			[]workflowkit.ResourceKey{resourceSourceSnapshot, resourceAnalysisRepository}, []workflowkit.ResourceKey{resourceTaskDesign},
			workflowkit.EffectContentProducer, StandardAuthoringTaskDesignMaxTurns, contentVerdicts(), artifactInput("repo_prepared"), artifactInput("repo_analysis"), optionalReviewDecisionInput("task_review_decision"), artifactOutput("task_proposal")),
		gateStage(TaskReview, StageTaskDesign, []string{TaskDesign}, ReviewTaskDirection,
			[]workflowkit.ResourceKey{resourceAnalysisRepository, resourceTaskDesign}, []workflowkit.ResourceKey{resourceReviewTaskDirection},
			artifactInput("repo_analysis"), artifactInput("task_proposal")),
		stage(GenerateTaskFiles, StageTaskGeneration, []string{TaskReview}, "harborfactory.generate_task_files",
			[]workflowkit.ResourceKey{resourceSourceSnapshot, resourceAnalysisRepository, resourceTaskDesign, resourceReviewTaskDirection}, []workflowkit.ResourceKey{resourceTaskGeneratedFiles},
			workflowkit.EffectContentProducer, 3, contentVerdicts(), artifactInput("repo_prepared"), artifactInput("repo_analysis"), artifactInput("task_proposal"), reviewDecisionInput("task_review_decision"), artifactOutput("generated_task_files")),
		stage(InstructionGen, StageTaskGeneration, []string{GenerateTaskFiles}, "harborfactory.instruction_generate",
			[]workflowkit.ResourceKey{resourceTaskGeneratedFiles}, []workflowkit.ResourceKey{resourceTaskInstruction},
			workflowkit.EffectContentProducer, 1, contentVerdicts(), artifactInput("generated_task_files"), optionalReviewDecisionInput("content_review_decision"), optionalReviewDecisionInput("solution_review_decision"), optionalArtifactInputWithSchema(standardAuthoringPackageAdmissionReportArtifact, standardAuthoringPackageAdmissionReportSchemaVersion), artifactOutput("instruction")),
		stage(TaskTOMLGen, StageTaskGeneration, []string{GenerateTaskFiles}, "harborfactory.task_toml_generate",
			[]workflowkit.ResourceKey{resourceTaskGeneratedFiles, resourceTaskDesign}, []workflowkit.ResourceKey{resourceTaskMetadata},
			workflowkit.EffectContentProducer, 1, contentVerdicts(), artifactInput("generated_task_files"), artifactInput("task_proposal"), optionalReviewDecisionInput("content_review_decision"), optionalReviewDecisionInput("solution_review_decision"), optionalArtifactInputWithSchema(standardAuthoringPackageAdmissionReportArtifact, standardAuthoringPackageAdmissionReportSchemaVersion), artifactOutput("task_toml")),
		stage(DockerfileGen, StageTaskGeneration, []string{GenerateTaskFiles}, "harborfactory.dockerfile_generate",
			[]workflowkit.ResourceKey{resourceSourceSnapshot, resourceTaskDesign}, []workflowkit.ResourceKey{resourceTaskEnvironment},
			workflowkit.EffectContentProducer, 1, contentVerdicts(), artifactInput("repo_prepared"), artifactInput("task_proposal"), optionalReviewDecisionInput("content_review_decision"), optionalReviewDecisionInput("solution_review_decision"), optionalArtifactInputWithSchema(standardAuthoringPackageAdmissionReportArtifact, standardAuthoringPackageAdmissionReportSchemaVersion), artifactOutput("dockerfile")),
		stage(DockerfileBuildValidate, StageTaskGeneration, []string{DockerfileGen}, "harborfactory.dockerfile_build_validate",
			[]workflowkit.ResourceKey{resourceSourceSnapshot, resourceTaskDesign, resourceTaskEnvironment},
			[]workflowkit.ResourceKey{resourceTaskValidatedEnvironment, resourceEvidenceAuthoringDockerBuild},
			workflowkit.EffectContentMutator, 1, passOnly(),
			artifactInput("repo_prepared"), artifactInput("task_proposal"), artifactInput("dockerfile"),
			artifactOutput(StandardAuthoringValidatedDockerfileArtifact), artifactOutputWithSchema(StandardAuthoringDockerfileBuildReportArtifact, StandardAuthoringDockerfileBuildReportSchemaVersion)),
		gateStage(ContentReview, StageTaskGeneration, []string{InstructionGen, TaskTOMLGen, DockerfileBuildValidate}, ReviewContent,
			[]workflowkit.ResourceKey{resourceTaskInstruction, resourceTaskMetadata, resourceTaskValidatedEnvironment, resourceEvidenceAuthoringDockerBuild}, []workflowkit.ResourceKey{resourceReviewContent},
			artifactInput("instruction"), artifactInput("task_toml"), artifactInput(StandardAuthoringValidatedDockerfileArtifact), artifactInputWithSchema(StandardAuthoringDockerfileBuildReportArtifact, StandardAuthoringDockerfileBuildReportSchemaVersion)),
		stage(SolveGen, StageTaskGeneration, []string{ContentReview}, "harborfactory.solve_generate",
			[]workflowkit.ResourceKey{resourceTaskGeneratedFiles, resourceTaskValidatedEnvironment, resourceEvidenceAuthoringDockerBuild}, []workflowkit.ResourceKey{resourceTaskSolution},
			workflowkit.EffectContentProducer, 1, passOnly(), artifactInput("generated_task_files"), artifactInput(StandardAuthoringValidatedDockerfileArtifact), artifactInputWithSchema(StandardAuthoringDockerfileBuildReportArtifact, StandardAuthoringDockerfileBuildReportSchemaVersion), optionalReviewDecisionInput("solution_review_decision"), optionalArtifactInputWithSchema(standardAuthoringPackageAdmissionReportArtifact, standardAuthoringPackageAdmissionReportSchemaVersion), artifactOutput("solve_script")),
		stage(TestGen, StageTaskGeneration, []string{ContentReview}, "harborfactory.test_generate",
			[]workflowkit.ResourceKey{resourceTaskGeneratedFiles, resourceTaskValidatedEnvironment, resourceEvidenceAuthoringDockerBuild}, []workflowkit.ResourceKey{resourceTaskTests},
			workflowkit.EffectContentProducer, 1, passOnly(), artifactInput("generated_task_files"), artifactInput(StandardAuthoringValidatedDockerfileArtifact), artifactInputWithSchema(StandardAuthoringDockerfileBuildReportArtifact, StandardAuthoringDockerfileBuildReportSchemaVersion), optionalReviewDecisionInput("solution_review_decision"), optionalArtifactInputWithSchema(standardAuthoringPackageAdmissionReportArtifact, standardAuthoringPackageAdmissionReportSchemaVersion), artifactOutput("test_script")),
		stage(AuthoringHarness, StageTaskGeneration, []string{SolveGen, TestGen}, "harborfactory.authoring_harness",
			[]workflowkit.ResourceKey{resourceSourceSnapshot, resourceTaskGeneratedFiles, resourceTaskDesign, resourceTaskInstruction, resourceTaskMetadata, resourceTaskValidatedEnvironment, resourceTaskSolution, resourceTaskTests, resourceEvidenceAuthoringDockerBuild},
			[]workflowkit.ResourceKey{resourceTaskValidatedSolution, resourceTaskValidatedTests, resourceEvidenceAuthoringHarness},
			workflowkit.EffectContentMutator, 1, passOnly(),
			artifactInput("repo_prepared"), artifactInput("generated_task_files"), artifactInput("task_proposal"), artifactInput("instruction"), artifactInput("task_toml"), artifactInput(StandardAuthoringValidatedDockerfileArtifact), artifactInputWithSchema(StandardAuthoringDockerfileBuildReportArtifact, StandardAuthoringDockerfileBuildReportSchemaVersion), artifactInput("solve_script"), artifactInput("test_script"),
			artifactOutput(StandardAuthoringValidatedSolveScriptArtifact), artifactOutput(StandardAuthoringValidatedTestScriptArtifact), artifactOutputWithSchema(StandardAuthoringHarnessReportArtifact, StandardAuthoringHarnessReportSchemaVersion)),
		stage(TestsAnalysis, StageTaskGeneration, []string{AuthoringHarness}, "harborfactory.tests_analysis",
			[]workflowkit.ResourceKey{resourceTaskGeneratedFiles, resourceTaskDesign, resourceTaskValidatedTests, resourceEvidenceAuthoringHarness}, []workflowkit.ResourceKey{resourceTaskTestsAnalysis},
			workflowkit.EffectContentProducer, 1, passOnly(), artifactInput("generated_task_files"), artifactInput("task_proposal"), artifactInput(StandardAuthoringValidatedTestScriptArtifact), artifactInputWithSchema(StandardAuthoringHarnessReportArtifact, StandardAuthoringHarnessReportSchemaVersion), optionalReviewDecisionInput("solution_review_decision"), optionalArtifactInputWithSchema(standardAuthoringPackageAdmissionReportArtifact, standardAuthoringPackageAdmissionReportSchemaVersion), artifactOutput("tests_analysis")),
		stage(CodeEdgePackageAdmission, StageTaskGeneration, []string{AuthoringHarness, TestsAnalysis}, "harborfactory.codeedge_package_admission",
			[]workflowkit.ResourceKey{resourceTaskInstruction, resourceTaskMetadata, resourceTaskValidatedEnvironment, resourceTaskValidatedSolution, resourceTaskValidatedTests, resourceTaskTestsAnalysis, resourceEvidenceAuthoringDockerBuild, resourceEvidenceAuthoringHarness}, []workflowkit.ResourceKey{resourceAuthoringTaskAdmission},
			workflowkit.EffectEvidenceOnly, 1, contentVerdicts(),
			artifactInput("instruction"), artifactInput("task_toml"), artifactInput(StandardAuthoringValidatedDockerfileArtifact), artifactInput(StandardAuthoringValidatedSolveScriptArtifact), artifactInput(StandardAuthoringValidatedTestScriptArtifact), artifactInput("tests_analysis"), artifactInputWithSchema(StandardAuthoringDockerfileBuildReportArtifact, StandardAuthoringDockerfileBuildReportSchemaVersion), artifactInputWithSchema(StandardAuthoringHarnessReportArtifact, StandardAuthoringHarnessReportSchemaVersion), artifactOutputWithSchema(standardAuthoringPackageAdmissionReportArtifact, standardAuthoringPackageAdmissionReportSchemaVersion)),
		gateStage(SolutionReview, StageTaskGeneration, []string{AuthoringHarness, TestsAnalysis, CodeEdgePackageAdmission}, ReviewSolutionVerifier,
			[]workflowkit.ResourceKey{resourceTaskInstruction, resourceTaskMetadata, resourceTaskValidatedEnvironment, resourceTaskValidatedSolution, resourceTaskValidatedTests, resourceTaskTestsAnalysis, resourceEvidenceAuthoringDockerBuild, resourceEvidenceAuthoringHarness, resourceAuthoringTaskAdmission}, []workflowkit.ResourceKey{resourceReviewSolutionVerifier},
			artifactInput("instruction"), artifactInput("task_toml"), artifactInput(StandardAuthoringValidatedDockerfileArtifact), artifactInput(StandardAuthoringValidatedSolveScriptArtifact), artifactInput(StandardAuthoringValidatedTestScriptArtifact), artifactInput("tests_analysis"), artifactInputWithSchema(StandardAuthoringDockerfileBuildReportArtifact, StandardAuthoringDockerfileBuildReportSchemaVersion), artifactInputWithSchema(StandardAuthoringHarnessReportArtifact, StandardAuthoringHarnessReportSchemaVersion), artifactInputWithSchema(standardAuthoringPackageAdmissionReportArtifact, standardAuthoringPackageAdmissionReportSchemaVersion)),
		stage(MaterializeTask, StageTaskGeneration, []string{SolutionReview}, "harborfactory.materialize_task",
			[]workflowkit.ResourceKey{resourceTaskInstruction, resourceTaskMetadata, resourceTaskValidatedEnvironment, resourceTaskValidatedSolution, resourceTaskValidatedTests, resourceTaskTestsAnalysis, resourceReviewSolutionVerifier, resourceEvidenceAuthoringDockerBuild, resourceEvidenceAuthoringHarness, resourceAuthoringTaskAdmission},
			[]workflowkit.ResourceKey{resourceTaskSnapshot, resourceTaskDigest, resourceTaskInstruction, resourceTaskMetadata, resourceTaskEnvironment, resourceTaskSolution, resourceTaskTests, resourceTaskTestsAnalysis, resourceAuthoringTaskHandoff},
			workflowkit.EffectContentMutator, 1, contentVerdicts(),
			artifactInput("instruction"), artifactInput("task_toml"), artifactInput(StandardAuthoringValidatedDockerfileArtifact), artifactInput(StandardAuthoringValidatedSolveScriptArtifact), artifactInput(StandardAuthoringValidatedTestScriptArtifact), artifactInput("tests_analysis"), reviewDecisionInput("solution_review_decision"), artifactInputWithSchema(StandardAuthoringDockerfileBuildReportArtifact, StandardAuthoringDockerfileBuildReportSchemaVersion), artifactInputWithSchema(StandardAuthoringHarnessReportArtifact, StandardAuthoringHarnessReportSchemaVersion), artifactInputWithSchema(standardAuthoringPackageAdmissionReportArtifact, standardAuthoringPackageAdmissionReportSchemaVersion), artifactOutput("task_snapshot"), artifactOutput("task_digest"), artifactOutputWithSchema(StandardAuthoringTaskHandoffArtifact, StandardAuthoringTaskHandoffSchemaVersion)),
	}
	for index := range stages {
		stages[index].Inputs = append(stages[index].Inputs, artifactInputWithSchema(AuthoringContractArtifact, AuthoringContractSchemaVersion).spec)
		stages[index].ReadSet = append(stages[index].ReadSet, resourceAuthoringContract)
	}
	return StageCatalog{
		Template: StandardAuthoringContractTemplateReference(),
		ID:       standardAuthoringCatalogID,
		Version:  standardAuthoringCatalogVersion,
		Stages:   stages,
	}
}

func StandardAuthoringTaskHandoffSchemaForTemplate(reference TemplateReference) (string, error) {
	if reference.Equal(StandardAuthoringContractTemplateReference()) {
		return StandardAuthoringTaskHandoffSchemaVersion, nil
	}
	return "", fmt.Errorf("Standard authoring handoff schema has no template %s@%s", reference.ID, reference.Version)
}
