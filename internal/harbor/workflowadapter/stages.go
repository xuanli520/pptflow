package workflowadapter

// Harbor stage keys belong to the code-versioned Harbor workflow policy. They
// describe frozen workflow semantics, not paths in a mutable workspace.
const (
	RepoPrepare       = "repo_prepare"
	RepoAnalyze       = "repo_analyze"
	TaskDesign        = "task_design"
	TaskReview        = "task_review"
	GenerateTaskFiles = "generate_task_files"
	InstructionGen    = "instruction_generate"
	TaskTOMLGen       = "task_toml_generate"
	DockerfileGen     = "dockerfile_generate"
	SolveGen          = "solve_generate"
	TestGen           = "test_generate"
	TestsAnalysis     = "tests_analysis"
	MaterializeTask   = "materialize_task"
	TaskRepair        = "task_repair"
	RuntimeSelfCheck  = "runtime_self_check"
	SolutionReview    = "solution_review"
	ContentReview     = "content_review"
	CodeEdgeLint      = "codeedge_lint"
	HarborVerify      = "harbor_verify"
	DockerBuild       = "docker_build"
	InitialVerify     = "initial_verify"
	OracleVerify      = "oracle_verify"
	QualityCheck      = "quality_check"
	SimilarityCheck   = "similarity_check"
	HarborRunQwen     = "harbor_run_qwen"
	HarborRunOpus     = "harbor_run_opus"
	// EvaluatorEvidenceHandoff is the parent-side durable gate that adopts
	// externally executed evaluator child evidence. It never executes a model
	// or re-labels child artifacts as outputs of the parent Run.
	EvaluatorEvidenceHandoff = "evaluator_evidence_handoff"
	SubmissionLint           = "submission_lint"
	ResultReview             = "result_review"
	FinalReview              = "final_review"
	Package                  = "package"
)

var standardStageOrder = []string{
	RepoPrepare,
	RepoAnalyze,
	TaskDesign,
	TaskReview,
	GenerateTaskFiles,
	InstructionGen,
	TaskTOMLGen,
	DockerfileGen,
	ContentReview,
	SolveGen,
	TestGen,
	TestsAnalysis,
	SolutionReview,
	MaterializeTask,
	TaskRepair,
	RuntimeSelfCheck,
	HarborVerify,
	DockerBuild,
	InitialVerify,
	OracleVerify,
	CodeEdgeLint,
	QualityCheck,
	SimilarityCheck,
	FinalReview,
	HarborRunQwen,
	HarborRunOpus,
	ResultReview,
	SubmissionLint,
	Package,
}

func lifecycleNodeOrder() []string {
	return append([]string(nil), standardStageOrder...)
}
