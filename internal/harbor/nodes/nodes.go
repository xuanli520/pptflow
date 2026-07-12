package nodes

import (
	"fmt"
	"path/filepath"
	"strings"
)

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
	PublishTask       = "publish_task"
	TaskRepair        = "task_repair"
	RuntimeSelfCheck  = "runtime_self_check"
	SolutionReview    = "solution_review"
	// The five review gates are explicit workflow nodes so every approval has
	// durable inputs, a stable artifact and an Engine-visible transition.
	ContentReview   = "content_review"
	CodeEdgeLint    = "codeedge_lint"
	HarborVerify    = "harbor_verify"
	DockerBuild     = "docker_build"
	InitialVerify   = "initial_verify"
	OracleVerify    = "oracle_verify"
	QualityCheck    = "quality_check"
	SimilarityCheck = "similarity_check"
	HarborRunQwen   = "harbor_run_qwen"
	HarborRunOpus   = "harbor_run_opus"
	SubmissionLint  = "submission_lint"
	ResultReview    = "result_review"
	FinalReview     = "final_review"
	Package         = "package"
)

func Order() []string {
	return []string{
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
		PublishTask,
		Package,
	}
}

func DefaultWorkspace(workspace string) string {
	if strings.TrimSpace(workspace) == "" {
		return filepath.Join(".harbor-factory", "workspace")
	}
	return workspace
}

func ArtifactPaths(workspace, nodeID string) []string {
	workspace = DefaultWorkspace(workspace)
	switch nodeID {
	case RepoPrepare:
		return []string{
			RepoPreparedPath(workspace),
			RepoPrepareCommandLogPath(workspace),
		}
	case RepoAnalyze:
		return []string{RepoAnalysisPath(workspace)}
	case TaskDesign:
		return []string{TaskProposalPath(workspace)}
	case TaskReview:
		return []string{ReviewDecisionPath(workspace, "phase1", TaskReview)}
	case GenerateTaskFiles:
		return []string{TaskFilesPath(workspace), GenReportPath(workspace)}
	case InstructionGen:
		return []string{InstructionPath(workspace)}
	case TaskTOMLGen:
		return []string{TaskTOMLPath(workspace)}
	case DockerfileGen:
		return []string{DockerfilePath(workspace)}
	case SolveGen:
		return []string{SolvePath(workspace)}
	case TestGen:
		return []string{TestPath(workspace)}
	case TestsAnalysis, MaterializeTask:
		return []string{TestsAnalysisPath(workspace)}
	case PublishTask:
		return []string{TaskPublishReceiptPath(workspace)}
	case RuntimeSelfCheck:
		return []string{AgentLogPath(workspace, RuntimeSelfCheck)}
	case ContentReview:
		return []string{ReviewDecisionPath(workspace, "phase1", ContentReview)}
	case SolutionReview:
		return []string{ReviewDecisionPath(workspace, "phase2", SolutionReview)}
	case CodeEdgeLint:
		return []string{CodeEdgeLintReportPath(workspace)}
	case SubmissionLint:
		return []string{SubmissionLintReportPath(workspace)}
	case DockerBuild:
		return []string{DockerBuildResultPath(workspace)}
	case InitialVerify:
		return []string{InitialVerifyResultPath(workspace)}
	case OracleVerify:
		return []string{OracleVerifyResultPath(workspace)}
	case HarborVerify:
		return []string{VerifyReportPath(workspace)}
	case QualityCheck:
		return []string{QualityReportPath(workspace)}
	case SimilarityCheck:
		return []string{SimilarityReportPath(workspace)}
	case ResultReview:
		return []string{ReviewDecisionPath(workspace, "phase3", ResultReview)}
	case FinalReview:
		return []string{ReviewDecisionPath(workspace, "phase2", FinalReview)}
	case HarborRunQwen:
		return HarborRunArtifactPaths(workspace, HarborRunQwen)
	case HarborRunOpus:
		return HarborRunArtifactPaths(workspace, HarborRunOpus)
	default:
		return nil
	}
}

func PrimaryArtifactPath(workspace, nodeID string) string {
	paths := ArtifactPaths(workspace, nodeID)
	if len(paths) == 0 {
		return ""
	}
	return paths[0]
}

func RepoPreparedPath(workspace string) string {
	return filepath.Join(DefaultWorkspace(workspace), "phase0", "repo_prepared.json")
}

func RunOptionsPath(workspace string) string {
	return filepath.Join(DefaultWorkspace(workspace), "run_options.json")
}

func RepoSourcePath(workspace string) string {
	return filepath.Join(DefaultWorkspace(workspace), "phase0", "source")
}

func RepoPrepareCommandLogDir(workspace, name string) string {
	return filepath.Join(DefaultWorkspace(workspace), "phase0", "command_logs", name)
}

func RepoPrepareCommandLogPath(workspace string) string {
	return filepath.Join(DefaultWorkspace(workspace), "phase0", "command_logs", "repo_prepare.json")
}

func RepoAnalysisPath(workspace string) string {
	return filepath.Join(DefaultWorkspace(workspace), "phase1", "artifacts", RepoAnalyze, "repo_analysis.json")
}

func TaskProposalPath(workspace string) string {
	return filepath.Join(DefaultWorkspace(workspace), "phase1", "artifacts", TaskDesign, "task_proposal.json")
}

func TaskFilesPath(workspace string) string {
	return filepath.Join(DefaultWorkspace(workspace), "phase1", "artifacts", GenerateTaskFiles, "task_files.json")
}

func GenReportPath(workspace string) string {
	return filepath.Join(DefaultWorkspace(workspace), "phase1", "artifacts", GenerateTaskFiles, "gen_report.json")
}

func AgentLogPath(workspace, nodeID string) string {
	return filepath.Join(DefaultWorkspace(workspace), "phase1", "artifacts", nodeID, "agent.log")
}

func InstructionPath(workspace string) string {
	return filepath.Join(DefaultWorkspace(workspace), "phase1", "artifacts", InstructionGen, "instruction.md")
}

func TaskTOMLPath(workspace string) string {
	return filepath.Join(DefaultWorkspace(workspace), "phase1", "artifacts", TaskTOMLGen, "task.toml")
}

func DockerfilePath(workspace string) string {
	return filepath.Join(DefaultWorkspace(workspace), "phase1", "artifacts", DockerfileGen, "Dockerfile")
}

func SolvePath(workspace string) string {
	return filepath.Join(DefaultWorkspace(workspace), "phase2", "artifacts", SolveGen, "solve.sh")
}

func TestPath(workspace string) string {
	return filepath.Join(DefaultWorkspace(workspace), "phase2", "artifacts", TestGen, "test.sh")
}

func TestsAnalysisPath(workspace string) string {
	return filepath.Join(DefaultWorkspace(workspace), "phase3", "artifacts", TestsAnalysis, "tests_analysis.md")
}

func TaskPublishReceiptPath(workspace string) string {
	return filepath.Join(DefaultWorkspace(workspace), "phase3", "artifacts", PublishTask, "publish_receipt.json")
}

func ReviewDecisionPath(workspace, phase, gateID string) string {
	return filepath.Join(DefaultWorkspace(workspace), phase, "artifacts", "reviews", gateID, "decision.json")
}

func CodeEdgeLintReportPath(workspace string) string {
	return filepath.Join(DefaultWorkspace(workspace), "phase2", "artifacts", CodeEdgeLint, "lint_report.json")
}

func SubmissionLintReportPath(workspace string) string {
	return filepath.Join(DefaultWorkspace(workspace), "phase2", "artifacts", SubmissionLint, "lint_report.json")
}

func VerifyReportPath(workspace string) string {
	return filepath.Join(DefaultWorkspace(workspace), "phase2", "artifacts", "verify", "verify_report.json")
}

func VerifyCommandLogDir(workspace, name string) string {
	return filepath.Join(DefaultWorkspace(workspace), "phase2", "artifacts", "verify", "command_logs", name)
}

func DockerBuildResultPath(workspace string) string {
	return filepath.Join(DefaultWorkspace(workspace), "phase2", "artifacts", DockerBuild, "build_result.json")
}

func InitialVerifyResultPath(workspace string) string {
	return filepath.Join(DefaultWorkspace(workspace), "phase2", "artifacts", InitialVerify, "initial_result.json")
}

func OracleVerifyResultPath(workspace string) string {
	return filepath.Join(DefaultWorkspace(workspace), "phase2", "artifacts", OracleVerify, "oracle_result.json")
}

func QualityReportPath(workspace string) string {
	return filepath.Join(DefaultWorkspace(workspace), "phase2", "artifacts", QualityCheck, "quality_report.json")
}

func QualityAgentLogPath(workspace string) string {
	return filepath.Join(DefaultWorkspace(workspace), "phase2", "artifacts", QualityCheck, "agent.log")
}

func TaskRepairReportPath(workspace, source string, round int) string {
	return filepath.Join(DefaultWorkspace(workspace), "phase2", "artifacts", "task_repair", source, fmt.Sprintf("repair-%03d.json", round))
}

func TaskRepairAgentLogPath(workspace, source string, round int) string {
	return filepath.Join(DefaultWorkspace(workspace), "phase2", "artifacts", "task_repair", source, fmt.Sprintf("repair-%03d-agent.log", round))
}

func SimilarityReportPath(workspace string) string {
	return filepath.Join(DefaultWorkspace(workspace), "phase2", "artifacts", SimilarityCheck, "similarity_report.json")
}

func HarborRunDir(workspace, nodeID string) string {
	return filepath.Join(DefaultWorkspace(workspace), "phase3", "artifacts", nodeID)
}

func HarborRunArtifactPaths(workspace, nodeID string) []string {
	dir := HarborRunDir(workspace, nodeID)
	paths := []string{
		filepath.Join(dir, "trial_result.json"),
		filepath.Join(dir, "command_run.json"),
		filepath.Join(dir, "stdout.txt"),
		filepath.Join(dir, "stderr.txt"),
	}
	switch nodeID {
	case HarborRunQwen:
		paths = append([]string{QwenResultPath(workspace)}, paths...)
	case HarborRunOpus:
		paths = append([]string{OpusResultPath(workspace)}, paths...)
	}
	return paths
}

func TrialResultPath(workspace, nodeID string) string {
	return filepath.Join(HarborRunDir(workspace, nodeID), "trial_result.json")
}

func QwenResultPath(workspace string) string {
	return filepath.Join(HarborRunDir(workspace, HarborRunQwen), "qwen_result.json")
}

func OpusResultPath(workspace string) string {
	return filepath.Join(HarborRunDir(workspace, HarborRunOpus), "opus_result.json")
}
