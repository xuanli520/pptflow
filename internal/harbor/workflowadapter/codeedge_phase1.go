package workflowadapter

import "github.com/purplevoid/harbor-factory/pkg/workflowkit"

const (
	// CodeEdgePhase1WorkflowTemplateID and Version identify the independent,
	// closed production descriptor selected for CodeEdge Phase-1. It does not
	// mutate or reinterpret the complete Standard lifecycle template.
	CodeEdgePhase1WorkflowTemplateID      = "harbor.codeedge-phase1"
	CodeEdgePhase1WorkflowTemplateVersion = "1.0.0"

	codeEdgePhase1CatalogID      = "harbor.codeedge-phase1-stage-catalog"
	codeEdgePhase1CatalogVersion = "1.0.0"
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
	workflowkit.StageKey(HarborRunQwen),
	workflowkit.StageKey(HarborRunOpus),
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
		workflowkit.StageKey(HarborRunQwen):   {workflowkit.StageKey(FinalReview)},
		// The two evaluator operations retain separate frozen bindings and
		// independent four-trial receipts. The dependency serializes their
		// externally visible evidence sequence; it does not pass Qwen output
		// into the Opus evaluator or make its result a model input.
		workflowkit.StageKey(HarborRunOpus):  {workflowkit.StageKey(HarborRunQwen)},
		workflowkit.StageKey(SubmissionLint): {workflowkit.StageKey(HarborRunQwen), workflowkit.StageKey(HarborRunOpus)},
		workflowkit.StageKey(ResultReview):   {workflowkit.StageKey(SubmissionLint)},
		// The confirmed 3A policy creates the unique local package only after
		// both evaluations and final compliance review have completed.
		workflowkit.StageKey(Package): {workflowkit.StageKey(ResultReview)},
	}
}

// CodeEdgePhase1WorkflowTemplate returns Harbor's versioned CodeEdge
// Phase-1 descriptor. It reuses only existing sealed Harbor stage binding
// types; changing the set requires an additive typed binding/parser revision,
// never an opaque stage-name or map payload escape hatch.
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
// Oracle verification, tests-analysis review, quality/similarity review,
// Qwen then Opus four-trial evaluation, final compliance review, and only
// then the one immutable local package. Real executable/image/model values
// remain outside this descriptor in the deployment operation catalog.
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
			codeEdgeEvaluationStage(HarborRunQwen, []string{FinalReview}, "harborfactory.harbor_run_qwen", []workflowkit.ResourceKey{resourceTaskSnapshot, resourceReviewFinalQuality}, []workflowkit.ResourceKey{resourceEvidenceEvaluationQwen}, artifactInput("task_snapshot"), reviewDecisionInput("final_review_decision"), artifactOutput("qwen_trial_result"), artifactOutput("qwen_pass4_evidence")),
			codeEdgeEvaluationStage(HarborRunOpus, []string{HarborRunQwen}, "harborfactory.harbor_run_opus", []workflowkit.ResourceKey{resourceTaskSnapshot, resourceReviewFinalQuality}, []workflowkit.ResourceKey{resourceEvidenceEvaluationOpus}, artifactInput("task_snapshot"), reviewDecisionInput("final_review_decision"), artifactOutput("opus_trial_result"), artifactOutput("opus_pass4_evidence")),
			stage(SubmissionLint, StageSubmission, []string{HarborRunQwen, HarborRunOpus}, "harborfactory.codeedge_lint", []workflowkit.ResourceKey{resourceTaskSnapshot, resourceEvidenceEvaluationQwen, resourceEvidenceEvaluationOpus}, []workflowkit.ResourceKey{resourceEvidenceSubmissionLint}, workflowkit.EffectEvidenceOnly, 1, checkVerdicts(), artifactInput("task_snapshot"), artifactInput("qwen_trial_result"), artifactInput("opus_trial_result"), artifactOutput("submission_lint_report")),
			gateStage(ResultReview, StageSubmission, []string{SubmissionLint}, ReviewModelResult, []workflowkit.ResourceKey{resourceEvidenceEvaluationQwen, resourceEvidenceEvaluationOpus, resourceEvidenceSubmissionLint}, []workflowkit.ResourceKey{resourceReviewModelResult}, artifactInput("qwen_trial_result"), artifactInput("opus_trial_result"), artifactInput("submission_lint_report")),
			stage(Package, StageDelivery, []string{ResultReview}, "harborfactory.local_package", []workflowkit.ResourceKey{resourceTaskSnapshot, resourceEvidenceSubmissionLint, resourceReviewModelResult}, []workflowkit.ResourceKey{resourceDeliveryPackage}, workflowkit.EffectExternalSideEffect, 1, deliveryVerdicts(), artifactInput("task_snapshot"), artifactInput("submission_lint_report"), reviewDecisionInput("model_result_decision"), artifactOutput("package_bundle")),
		},
	}
}

// codeEdgeEvaluationStage marks a Harbor evaluator as an external side
// effect and deliberately disables generic stage retries. One invocation
// creates the confirmed four logical samples, so retrying the whole stage on
// a transient/process failure could produce a second, incomparable group of
// four. The durable TrialExecution reconciler owns technical retry beneath
// the same logical trial identity instead.
func codeEdgeEvaluationStage(key string, dependencies []string, pluginID string, reads, writes []workflowkit.ResourceKey, artifacts ...stageArtifact) StageDefinition {
	definition := stage(key, StageEvaluation, dependencies, pluginID, reads, writes, workflowkit.EffectExternalSideEffect, 1, evaluationVerdicts(), artifacts...)
	definition.Retry = workflowkit.RetryPolicy{}
	definition.Reuse = workflowkit.ReuseNever
	return definition
}
