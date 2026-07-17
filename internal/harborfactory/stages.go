// Package harborfactory is the unified Harbor Factory domain package. It
// consolidates workflow definitions, stage types, and catalog construction
// that were previously split across workflowadapter and stageprovider.
package harborfactory

import "github.com/purplevoid/harbor-factory/pkg/workflowkit"

// Stage name constants — canonical keys for all Harbor Factory pipeline stages.
const (
	RepoPrepare              = "repo_prepare"
	RepoAnalyze              = "repo_analyze"
	TaskDesign               = "task_design"
	TaskReview               = "task_review"
	GenerateTaskFiles        = "generate_task_files"
	InstructionGen           = "instruction_generate"
	TaskTOMLGen              = "task_toml_generate"
	DockerfileGen            = "dockerfile_generate"
	ContentReview            = "content_review"
	SolveGen                 = "solve_generate"
	TestGen                  = "test_generate"
	TestsAnalysis            = "tests_analysis"
	CodeEdgePackageAdmission = "codeedge_package_admission"
	SolutionReview           = "solution_review"
	MaterializeTask          = "materialize_task"
	TaskRepair               = "task_repair"
	RuntimeSelfCheck         = "runtime_self_check"
	HarborVerify             = "harbor_verify"
	DockerBuild              = "docker_build"
	InitialVerify            = "initial_verify"
	OracleVerify             = "oracle_verify"
	CodeEdgeLint             = "codeedge_lint"
	QualityCheck             = "quality_check"
	SimilarityCheck          = "similarity_check"
	FinalReview              = "final_review"
	HarborRunQwen            = "harbor_run_qwen"
	HarborRunOpus            = "harbor_run_opus"
	EvaluatorEvidenceHandoff = "evaluator_evidence_handoff"
	ResultReview             = "result_review"
	SubmissionLint           = "submission_lint"
	Package                  = "package"
)

// Stage groups.
const (
	StageSourcePrepare  = "source_prepare"
	StageTaskAnalysis   = "task_analysis"
	StageTaskDesign     = "task_design"
	StageTaskGeneration = "task_generation"
	StageRuntimeVerify  = "runtime_verify"
	StageQuality        = "quality"
	StageSimilarity     = "similarity"
	StageFinalReview    = "final_review"
	StageEvaluation     = "evaluation"
	StageSubmission     = "submission"
	StageDelivery       = "delivery"
)

// StandardAuthoringStageOrder is the dependency-ordered stage list for the
// pre-materialization authoring workflow.
var StandardAuthoringStageOrder = []workflowkit.StageKey{
	RepoPrepare, RepoAnalyze, TaskDesign, TaskReview,
	GenerateTaskFiles, InstructionGen, TaskTOMLGen, DockerfileGen,
	ContentReview, SolveGen, TestGen, TestsAnalysis,
	SolutionReview, MaterializeTask,
}

// CodeEdgePhase1StageOrder is the dependency-ordered stage list for
// CodeEdge Phase-1 compliance and evaluation.
var CodeEdgePhase1StageOrder = []workflowkit.StageKey{
	RepoPrepare, RepoAnalyze, CodeEdgeLint,
	DockerBuild, InitialVerify, OracleVerify,
	TestsAnalysis, SolutionReview,
	QualityCheck, SimilarityCheck,
	FinalReview, EvaluatorEvidenceHandoff,
	SubmissionLint, ResultReview, Package,
}
