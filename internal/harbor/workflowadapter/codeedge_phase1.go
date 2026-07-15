package workflowadapter

import "github.com/purplevoid/harbor-factory/pkg/workflowkit"

const (
	// CodeEdgePhase1WorkflowTemplateID and Version identify the independent,
	// closed production descriptor selected for CodeEdge Phase-1. It does not
	// mutate or reinterpret the complete Standard lifecycle template.
	CodeEdgePhase1WorkflowTemplateID      = "harbor.codeedge-phase1"
	CodeEdgePhase1WorkflowTemplateVersion = "2.2.0"
	// CodeEdgeSubmissionReportSchemaVersion is the typed submission report
	// consumed by final compliance, the result review gate, and packaging.
	// Changing this schema changes the frozen workflow contract.
	CodeEdgeSubmissionReportSchemaVersion = "codeedge.submission-report.v1"

	codeEdgePhase1CatalogID      = "harbor.codeedge-phase1-stage-catalog"
	codeEdgePhase1CatalogVersion = "2.2.0"
)

// CodeEdgePhase1TemplateReference returns the immutable identity used by
// CodeEdge Phase-1 profiles and execution specifications.
func CodeEdgePhase1TemplateReference() TemplateReference {
	return TemplateReference{ID: CodeEdgePhase1WorkflowTemplateID, Version: CodeEdgePhase1WorkflowTemplateVersion}
}

var codeEdgePhase1StageOrder = []workflowkit.StageKey{
	workflowkit.StageKey(RepoPrepare),
	workflowkit.StageKey(RepoAnalyze),
	workflowkit.StageKey(CodeEdgeLint),
	workflowkit.StageKey(DockerBuild),
	workflowkit.StageKey(InitialVerify),
	workflowkit.StageKey(OracleVerify),
	workflowkit.StageKey(TestsAnalysis),
	workflowkit.StageKey(SolutionReview),
	workflowkit.StageKey(QualityCheck),
	workflowkit.StageKey(SimilarityCheck),
	workflowkit.StageKey(FinalReview),
	workflowkit.StageKey(EvaluatorEvidenceHandoff),
	workflowkit.StageKey(SubmissionLint),
	workflowkit.StageKey(ResultReview),
	workflowkit.StageKey(Package),
}

var codeEdgePhase1Groups = []StageGroup{
	StageSourcePrepare,
	StageTaskAnalysis,
	StageRuntimeVerify,
	StageQuality,
	StageSimilarity,
	StageFinalReview,
	StageEvaluation,
	StageSubmission,
	StageDelivery,
}

// CodeEdgePhase1StageOrder returns the fixed dependency-aware declaration
// order used by the Phase-1 descriptor. It is useful for read-only UI
// presentation, but execution still follows the frozen DAG dependencies.
func CodeEdgePhase1StageOrder() []workflowkit.StageKey {
	return append([]workflowkit.StageKey(nil), codeEdgePhase1StageOrder...)
}

func codeEdgePhase1StageGroups() []StageGroup {
	return append([]StageGroup(nil), codeEdgePhase1Groups...)
}

func codeEdgePhase1Dependencies() map[workflowkit.StageKey][]workflowkit.StageKey {
	return map[workflowkit.StageKey][]workflowkit.StageKey{
		workflowkit.StageKey(RepoPrepare):     nil,
		workflowkit.StageKey(RepoAnalyze):     {workflowkit.StageKey(RepoPrepare)},
		workflowkit.StageKey(CodeEdgeLint):    {workflowkit.StageKey(RepoPrepare)},
		workflowkit.StageKey(DockerBuild):     {workflowkit.StageKey(RepoAnalyze), workflowkit.StageKey(CodeEdgeLint)},
		workflowkit.StageKey(InitialVerify):   {workflowkit.StageKey(DockerBuild)},
		workflowkit.StageKey(OracleVerify):    {workflowkit.StageKey(InitialVerify)},
		workflowkit.StageKey(TestsAnalysis):   {workflowkit.StageKey(OracleVerify)},
		workflowkit.StageKey(SolutionReview):  {workflowkit.StageKey(TestsAnalysis)},
		workflowkit.StageKey(QualityCheck):    {workflowkit.StageKey(SolutionReview)},
		workflowkit.StageKey(SimilarityCheck): {workflowkit.StageKey(QualityCheck)},
		workflowkit.StageKey(FinalReview):     {workflowkit.StageKey(SimilarityCheck)},
		// The evaluator child owns the real Qwen and Opus external effects. The
		// parent waits for a separately verified, immutable handoff rather than
		// calling either model or copying their artifacts into parent lineage.
		workflowkit.StageKey(EvaluatorEvidenceHandoff): {workflowkit.StageKey(FinalReview)},
		workflowkit.StageKey(SubmissionLint):           {workflowkit.StageKey(EvaluatorEvidenceHandoff)},
		workflowkit.StageKey(ResultReview):             {workflowkit.StageKey(SubmissionLint)},
		// The confirmed 3A policy creates the unique local package only after
		// both evaluations and final compliance review have completed.
		workflowkit.StageKey(Package): {workflowkit.StageKey(ResultReview)},
	}
}

// CodeEdgePhase1WorkflowTemplate returns Harbor's versioned CodeEdge
// Phase-1 descriptor. It uses only sealed Harbor stage binding types;
// changing the set requires an additive typed binding/parser revision, never
// an opaque stage-name or map payload escape hatch.
func CodeEdgePhase1WorkflowTemplate() WorkflowTemplate {
	return WorkflowTemplate{
		ID:          CodeEdgePhase1WorkflowTemplateID,
		Version:     CodeEdgePhase1WorkflowTemplateVersion,
		Catalog:     CodeEdgePhase1StageCatalog(),
		QuotaPolicy: CodeEdgePhase1QuotaPolicy(),
	}
}

// CodeEdgePhase1StageCatalog defines the confirmed production ordering:
// structural/repository/environment preflight, controlled build, initial and
// Oracle verification, tests-analysis review, quality/similarity review, an
// explicit evaluator-child evidence handoff, final compliance review, and
// only then the one immutable local package. The parent never contains a
// Qwen/Opus provider operation; real evaluator execution remains in the
// closed child descriptor.
func CodeEdgePhase1StageCatalog() StageCatalog {
	return StageCatalog{
		Template: CodeEdgePhase1TemplateReference(),
		ID:       codeEdgePhase1CatalogID,
		Version:  codeEdgePhase1CatalogVersion,
		Stages: []StageDefinition{
			stage(RepoPrepare, StageSourcePrepare, nil, "harborfactory.repo_prepare", []workflowkit.ResourceKey{resourceTaskSnapshot}, []workflowkit.ResourceKey{resourceEvidenceTaskLayout}, workflowkit.EffectEvidenceOnly, 1, checkVerdicts(), artifactInput("task_snapshot"), artifactOutput("task_layout_report")),
			stage(RepoAnalyze, StageSourcePrepare, []string{RepoPrepare}, "harborfactory.repo_analyze", []workflowkit.ResourceKey{resourceTaskSnapshot, resourceEvidenceTaskLayout}, []workflowkit.ResourceKey{resourceEvidenceRepoProvenance}, workflowkit.EffectEvidenceOnly, 1, checkVerdicts(), artifactInput("task_snapshot"), artifactInput("task_layout_report"), artifactOutput("repo_provenance_report")),
			stage(CodeEdgeLint, StageSourcePrepare, []string{RepoPrepare}, "harborfactory.codeedge_lint", []workflowkit.ResourceKey{resourceTaskSnapshot, resourceEvidenceTaskLayout}, []workflowkit.ResourceKey{resourceEvidenceEnvironmentIsolation}, workflowkit.EffectEvidenceOnly, 1, checkVerdicts(), artifactInput("task_snapshot"), artifactInput("task_layout_report"), artifactOutput("environment_isolation_report")),
			stage(DockerBuild, StageRuntimeVerify, []string{RepoAnalyze, CodeEdgeLint}, "harborfactory.docker_build", []workflowkit.ResourceKey{resourceTaskSnapshot, resourceEvidenceRepoProvenance, resourceEvidenceEnvironmentIsolation}, []workflowkit.ResourceKey{resourceEvidenceDockerBuild}, workflowkit.EffectEvidenceOnly, 1, checkVerdicts(), artifactInput("task_snapshot"), artifactInput("repo_provenance_report"), artifactInput("environment_isolation_report"), artifactOutput("docker_build_report")),
			stage(InitialVerify, StageRuntimeVerify, []string{DockerBuild}, "harborfactory.initial_verify", []workflowkit.ResourceKey{resourceTaskSnapshot, resourceEvidenceDockerBuild}, []workflowkit.ResourceKey{resourceEvidenceInitialVerify}, workflowkit.EffectEvidenceOnly, 1, checkVerdicts(), artifactInput("task_snapshot"), artifactInput("docker_build_report"), artifactOutput("initial_verify_report")),
			stage(OracleVerify, StageRuntimeVerify, []string{InitialVerify}, "harborfactory.oracle_verify", []workflowkit.ResourceKey{resourceTaskSnapshot, resourceEvidenceInitialVerify}, []workflowkit.ResourceKey{resourceEvidenceOracleVerify}, workflowkit.EffectEvidenceOnly, 1, checkVerdicts(), artifactInput("task_snapshot"), artifactInput("initial_verify_report"), artifactOutput("oracle_verify_report")),
			stage(TestsAnalysis, StageTaskAnalysis, []string{OracleVerify}, "harborfactory.tests_analysis", []workflowkit.ResourceKey{resourceTaskSnapshot, resourceEvidenceOracleVerify}, []workflowkit.ResourceKey{resourceEvidenceTestsAnalysis}, workflowkit.EffectEvidenceOnly, 1, checkVerdicts(), artifactInput("task_snapshot"), artifactInput("oracle_verify_report"), artifactOutput("tests_analysis_report")),
			gateStage(SolutionReview, StageTaskAnalysis, []string{TestsAnalysis}, ReviewSolutionVerifier, []workflowkit.ResourceKey{resourceEvidenceTestsAnalysis}, []workflowkit.ResourceKey{resourceReviewSolutionVerifier}, artifactInput("tests_analysis_report")),
			stage(QualityCheck, StageQuality, []string{SolutionReview}, "harborfactory.quality_check", []workflowkit.ResourceKey{resourceTaskSnapshot, resourceEvidenceTestsAnalysis, resourceReviewSolutionVerifier}, []workflowkit.ResourceKey{resourceEvidenceQuality}, workflowkit.EffectEvidenceOnly, 1, checkVerdicts(), artifactInput("task_snapshot"), artifactInput("tests_analysis_report"), reviewDecisionInput("solution_review_decision"), artifactOutput("quality_report")),
			stage(SimilarityCheck, StageSimilarity, []string{QualityCheck}, "harborfactory.similarity_check", []workflowkit.ResourceKey{resourceTaskSnapshot, resourceEvidenceQuality}, []workflowkit.ResourceKey{resourceEvidenceSimilarity}, workflowkit.EffectEvidenceOnly, 1, similarityVerdicts(), artifactInput("task_snapshot"), artifactInput("quality_report"), artifactOutput("similarity_report")),
			gateStage(FinalReview, StageFinalReview, []string{SimilarityCheck}, ReviewFinalQuality, []workflowkit.ResourceKey{resourceEvidenceQuality, resourceEvidenceSimilarity}, []workflowkit.ResourceKey{resourceReviewFinalQuality}, artifactInput("quality_report"), artifactInput("similarity_report")),
			gateStage(EvaluatorEvidenceHandoff, StageEvaluation, []string{FinalReview}, ReviewEvaluatorEvidence, []workflowkit.ResourceKey{resourceReviewFinalQuality}, []workflowkit.ResourceKey{resourceEvidenceEvaluatorHandoff, resourceReviewEvaluatorEvidence}, reviewDecisionInput("final_review_decision")),
			stage(SubmissionLint, StageSubmission, []string{EvaluatorEvidenceHandoff}, "harborfactory.codeedge_lint", []workflowkit.ResourceKey{resourceTaskSnapshot, resourceEvidenceEvaluatorHandoff, resourceReviewEvaluatorEvidence}, []workflowkit.ResourceKey{resourceEvidenceSubmissionLint}, workflowkit.EffectEvidenceOnly, 1, checkVerdicts(), artifactInput("task_snapshot"), reviewDecisionInput("evaluator_evidence_handoff_decision"), artifactOutputWithSchema("submission_lint_report", CodeEdgeSubmissionReportSchemaVersion)),
			gateStage(ResultReview, StageSubmission, []string{SubmissionLint}, ReviewModelResult, []workflowkit.ResourceKey{resourceEvidenceEvaluatorHandoff, resourceReviewEvaluatorEvidence, resourceEvidenceSubmissionLint}, []workflowkit.ResourceKey{resourceReviewModelResult}, reviewDecisionInput("evaluator_evidence_handoff_decision"), artifactInputWithSchema("submission_lint_report", CodeEdgeSubmissionReportSchemaVersion)),
			operatorOnlyLocalPackageStage([]string{ResultReview}, []workflowkit.ResourceKey{resourceTaskSnapshot, resourceEvidenceSubmissionLint, resourceReviewModelResult}, []workflowkit.ResourceKey{resourceDeliveryPackage}, artifactInput("task_snapshot"), artifactInputWithSchema("submission_lint_report", CodeEdgeSubmissionReportSchemaVersion), reviewDecisionInput("model_result_decision"), artifactOutput("package_bundle")),
		},
	}
}
