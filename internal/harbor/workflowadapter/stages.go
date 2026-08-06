package workflowadapter

// Harbor stage keys belong to the code-versioned Harbor workflow policy. They
// describe frozen workflow semantics, not paths in a mutable workspace.
const (
	RepoPrepare              = "repo_prepare"
	RepoAnalyze              = "repo_analyze"
	TaskDesign               = "task_design"
	TaskReview               = "task_review"
	GenerateTaskFiles        = "generate_task_files"
	InstructionGen           = "instruction_generate"
	TaskTOMLGen              = "task_toml_generate"
	DockerfileGen            = "dockerfile_generate"
	DockerfileBuildValidate  = "dockerfile_build_validate"
	SolveGen                 = "solve_generate"
	TestGen                  = "test_generate"
	AuthoringHarness         = "authoring_harness"
	TestsAnalysis            = "tests_analysis"
	CodeEdgePackageAdmission = "codeedge_package_admission"
	MaterializeTask          = "materialize_task"
	TaskRepair               = "task_repair"
	RuntimeSelfCheck         = "runtime_self_check"
	SolutionReview           = "solution_review"
	ContentReview            = "content_review"
	CodeEdgeLint             = "codeedge_lint"
	HarborVerify             = "harbor_verify"
	DockerBuild              = "docker_build"
	InitialVerify            = "initial_verify"
	OracleVerify             = "oracle_verify"
	QualityCheck             = "quality_check"
	SimilarityCheck          = "similarity_check"
	FinalReview              = "final_review"
	Package                  = "package"

	// Standard Authoring 3.0 owns a closed source-session graph. These keys do
	// not reuse retired 2.0 generation nodes, so a frozen legacy operation can
	// never be interpreted as a 3.0 role binding.
	RepoStructureResearch   = "repo_structure_research"
	TestRuntimeResearch     = "test_runtime_research"
	VerifierThreatResearch  = "verifier_threat_research"
	TaskSynthesis           = "task_synthesis"
	AuthoringLoop           = "authoring_loop"
	HostCandidateVerify     = "host_candidate_verify"
	TestQualityCritic       = "test_quality_critic"
	SolutionIntegrityCritic = "solution_integrity_critic"
	AuthoringRepair         = "authoring_repair"
	FinalAttestation        = "final_attestation"
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
	Package,
}

func lifecycleNodeOrder() []string {
	return append([]string(nil), standardStageOrder...)
}
