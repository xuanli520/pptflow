package app

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/nodes"
	"github.com/purplevoid/harbor-factory/internal/harbor/packager"
	"github.com/purplevoid/harbor-factory/internal/workflow"
)

const harborWorkflowID = "harbor-factory"

func buildWorkflowDefinition(opts RunnerOptions) (workflow.WorkflowDefinition, error) {
	workspace := nodes.DefaultWorkspace(opts.Workspace)
	taskDir := strings.TrimSpace(opts.TaskDir)
	publishDestination := ""
	if opts.Generate {
		taskDir = filepath.Join(workspace, "phase2", "task", "generated-task")
		publishDestination = strings.TrimSpace(opts.TaskOutputDir)
	}
	if taskDir == "" {
		return workflow.WorkflowDefinition{}, fmt.Errorf("task directory is required")
	}

	definition := workflow.WorkflowDefinition{ID: harborWorkflowID, Name: "Harbor Task Factory", Policy: workflow.Policy{MaxNodes: 40, MaxRevisions: 5}}
	add := func(spec workflow.NodeSpec) {
		definition.Nodes = append(definition.Nodes, spec)
	}
	chain := func(id, kind string, depends []string, timeout, attempts int, config map[string]any, inputs ...workflow.ArtifactRef) {
		add(workflow.NodeSpec{
			ID: id, Kind: kind, PluginID: kind, Name: id, DependsOn: depends, Inputs: inputs, Config: config,
			Policy: workflow.NodePolicy{TimeoutSeconds: timeout, MaxAttempts: attempts, RetryBackoffMS: 500, RetryMaxBackoffMS: 5000, Retryable: []workflow.FailureKind{workflow.FailureTransient, workflow.FailureTimeout, workflow.FailureRateLimit, workflow.FailureNetwork}},
		})
	}
	artifact := func(name, artifactType, producer string) workflow.ArtifactRef {
		return workflow.ArtifactRef{Name: filepath.ToSlash(name), Type: artifactType, Producer: producer}
	}
	gate := func(id, phase, name, message, restartFrom string, depends []string, inputs ...workflow.ArtifactRef) {
		chain(id, "harborfactory.human_gate", depends, 86400, 1, map[string]any{
			"phase": phase, "gate_id": id, "gate_name": name, "message": message,
			"artifact_name": filepath.ToSlash(filepath.Join(phase, "artifacts", "reviews", id, "decision.json")),
			"restart_from":  restartFrom, "task_dir": taskDir, "model": opts.Model,
			"reasoning_effort": opts.Reasoning, "agent_timeout_seconds": opts.AgentTimeout,
			"qwen_model": defaultString(opts.QwenModel, domain.DefaultQwenModel), "opus_model": defaultString(opts.OpusModel, domain.DefaultOpusModel),
			"harbor_agent": defaultString(opts.HarborAgent, domain.DefaultHarborAgent), "qwen_screenshot": opts.QwenScreenshot, "opus_screenshot": opts.OpusScreenshot,
		}, inputs...)
	}

	materializeDependency := ""
	if opts.Generate {
		chain(nodes.RepoPrepare, "harborfactory.repo_prepare", nil, 600, 1, map[string]any{
			"repo_url": opts.RepoURL, "commit": opts.Commit, "allow_local_repo": opts.AllowLocalRepo,
			"artifact_name": "phase0/repo_prepared.json", "max_network_attempts": 3, "retry_delay_ms": 1000,
		})
		prepared := artifact("phase0/repo_prepared.json", "repo_prepared", nodes.RepoPrepare)
		chain(nodes.RepoAnalyze, "harborfactory.repo_analyze", []string{nodes.RepoPrepare}, 600, 3, agentConfig(opts, "phase1/artifacts/repo_analyze/repo_analysis.json"), prepared)
		analysis := artifact("phase1/artifacts/repo_analyze/repo_analysis.json", "repo_analysis", nodes.RepoAnalyze)
		chain(nodes.TaskDesign, "harborfactory.task_design", []string{nodes.RepoAnalyze}, 600, 3, agentConfig(opts, "phase1/artifacts/task_design/task_proposal.json"), prepared, analysis)
		proposal := artifact("phase1/artifacts/task_design/task_proposal.json", "task_proposal", nodes.TaskDesign)
		gate(nodes.TaskReview, "phase1", "Task Direction Gate", "Confirm that the task is a real, non-trivial CodeEdge engineering scenario before generating deliverables.", nodes.TaskDesign, []string{nodes.TaskDesign}, analysis, proposal)
		taskDecision := artifact("phase1/artifacts/reviews/task_review/decision.json", "gate_decision", nodes.TaskReview)
		chain(nodes.GenerateTaskFiles, "harborfactory.generate_task_files", []string{nodes.TaskReview}, 600, 3, agentConfig(opts, "phase1/artifacts/generate_task_files/task_files.json"), prepared, analysis, proposal, taskDecision)
		files := artifact("phase1/artifacts/generate_task_files/task_files.json", "generated_task_files", nodes.GenerateTaskFiles)

		chain(nodes.InstructionGen, "harborfactory.instruction_generate", []string{nodes.GenerateTaskFiles}, 120, 1, map[string]any{"artifact_name": "phase1/artifacts/instruction_generate/instruction.md"}, files)
		chain(nodes.TaskTOMLGen, "harborfactory.task_toml_generate", []string{nodes.GenerateTaskFiles}, 120, 1, map[string]any{"artifact_name": "phase1/artifacts/task_toml_generate/task.toml"}, proposal)
		chain(nodes.DockerfileGen, "harborfactory.dockerfile_generate", []string{nodes.GenerateTaskFiles}, 120, 1, map[string]any{"artifact_name": "phase1/artifacts/dockerfile_generate/Dockerfile"}, prepared, proposal)
		instruction := artifact("phase1/artifacts/instruction_generate/instruction.md", "instruction", nodes.InstructionGen)
		taskTOML := artifact("phase1/artifacts/task_toml_generate/task.toml", "task_toml", nodes.TaskTOMLGen)
		dockerfile := artifact("phase1/artifacts/dockerfile_generate/Dockerfile", "dockerfile", nodes.DockerfileGen)
		gate(nodes.ContentReview, "phase1", "Content Gate", "Review instruction, task metadata and environment isolation before generating the oracle and verifier.", nodes.GenerateTaskFiles, []string{nodes.InstructionGen, nodes.TaskTOMLGen, nodes.DockerfileGen}, instruction, taskTOML, dockerfile)

		chain(nodes.SolveGen, "harborfactory.solve_generate", []string{nodes.ContentReview}, 120, 1, map[string]any{"artifact_name": "phase2/artifacts/solve_generate/solve.sh"}, files)
		chain(nodes.TestGen, "harborfactory.test_generate", []string{nodes.ContentReview}, 120, 1, map[string]any{"artifact_name": "phase2/artifacts/test_generate/test.sh"}, files)
		chain(nodes.TestsAnalysis, "harborfactory.tests_analysis", []string{nodes.ContentReview}, 120, 1, map[string]any{"artifact_name": "phase3/artifacts/tests_analysis/tests_analysis.md"}, files, proposal)
		solve := artifact("phase2/artifacts/solve_generate/solve.sh", "solve_script", nodes.SolveGen)
		test := artifact("phase2/artifacts/test_generate/test.sh", "test_script", nodes.TestGen)
		testsAnalysis := artifact("phase3/artifacts/tests_analysis/tests_analysis.md", "tests_analysis", nodes.TestsAnalysis)
		gate(nodes.SolutionReview, "phase2", "Solution and Verifier Gate", "Confirm that the oracle is reproducible and the verifier is aligned, discriminating and free of hidden requirements.", nodes.GenerateTaskFiles, []string{nodes.SolveGen, nodes.TestGen, nodes.TestsAnalysis}, instruction, solve, test, testsAnalysis)

		chain(nodes.MaterializeTask, "harborfactory.materialize_task", []string{nodes.SolutionReview}, 120, 1, map[string]any{"task_dir": taskDir}, instruction, taskTOML, dockerfile, solve, test, testsAnalysis)
		materializeDependency = nodes.MaterializeTask
	}

	if guidance := strings.TrimSpace(opts.RepairGuidance); guidance != "" {
		source := normalizedRepairSource(opts.RepairSource)
		chain(nodes.TaskRepair, "harborfactory.task_repair", compactDependency(materializeDependency), 600, 1, map[string]any{
			"task_dir": taskDir, "guidance": guidance, "source": source,
			"model": opts.Model, "reasoning_effort": opts.Reasoning, "agent_timeout_seconds": opts.AgentTimeout,
			"artifact_name":     fmt.Sprintf("phase2/artifacts/task_repair/%s/repair-001.json", source),
			"log_artifact_name": fmt.Sprintf("phase2/artifacts/task_repair/%s/repair-001-agent.log", source),
		})
		materializeDependency = nodes.TaskRepair
	}
	if opts.Generate {
		chain(nodes.RuntimeSelfCheck, "harborfactory.runtime_self_check", compactDependency(materializeDependency), 1800, 1, mergeConfig(agentConfig(opts, "phase2/artifacts/runtime_self_check/agent.log"), map[string]any{"task_dir": taskDir}))
		materializeDependency = nodes.RuntimeSelfCheck
	}
	baseDeps := compactDependency(materializeDependency)
	forceVerify := opts.VerifyDocker || opts.Package
	lastChecks := append([]string(nil), baseDeps...)
	verifyProducer := nodes.OracleVerify
	if forceVerify {
		verifyPath := nodes.VerifyReportPath(workspace)
		if packager.ValidateVerifyReport(verifyPath, taskDir) == nil {
			chain(nodes.HarborVerify, "harborfactory.verify_report_import", baseDeps, 60, 1, map[string]any{
				"task_dir": taskDir, "report_path": verifyPath, "artifact_name": "phase2/artifacts/verify/verify_report.json",
			})
			verifyProducer = nodes.HarborVerify
			lastChecks = []string{nodes.HarborVerify}
		} else {
			chain(nodes.DockerBuild, "harborfactory.docker_build", baseDeps, 600, 3, map[string]any{"task_dir": taskDir, "workspace": workspace})
			chain(nodes.InitialVerify, "harborfactory.initial_verify", []string{nodes.DockerBuild}, 600, 1, map[string]any{"task_dir": taskDir, "workspace": workspace})
			chain(nodes.OracleVerify, "harborfactory.oracle_verify", []string{nodes.InitialVerify}, 600, 1, map[string]any{"task_dir": taskDir, "workspace": workspace})
			lastChecks = []string{nodes.OracleVerify}
		}
	}

	testsAnalysisPath := defaultString(strings.TrimSpace(opts.TestsAnalysis), filepath.Join(taskDir, "tests_analysis.md"))
	chain(nodes.CodeEdgeLint, "harborfactory.codeedge_lint", lastChecks, 60, 1, map[string]any{
		"task_dir": taskDir, "repo_url": opts.RepoURL, "commit": opts.Commit, "tests_analysis": testsAnalysisPath,
		"strict_submission": false, "defer_failure_to_gate": true, "artifact_name": "phase2/artifacts/codeedge_lint/lint_report.json",
	})
	finalEvidence := []workflow.ArtifactRef{artifact("phase2/artifacts/codeedge_lint/lint_report.json", "lint_report", nodes.CodeEdgeLint)}
	if forceVerify {
		finalEvidence = append(finalEvidence, artifact("phase2/artifacts/verify/verify_report.json", "verify_report", verifyProducer))
	}
	lastQuality := nodes.CodeEdgeLint
	if opts.QualityCheck {
		chain(nodes.QualityCheck, "harborfactory.quality_check", []string{lastQuality}, 300, 3, mergeConfig(agentConfig(opts, "phase2/artifacts/quality_check/quality_report.json"), map[string]any{
			"task_dir": taskDir, "workspace": workspace, "repo_url": opts.RepoURL, "commit": opts.Commit,
			"tests_analysis": testsAnalysisPath, "agent_timeout_seconds": opts.AgentTimeout, "agent_enabled": opts.QualityAgent,
		}))
		lastQuality = nodes.QualityCheck
		finalEvidence = append(finalEvidence, artifact("phase2/artifacts/quality_check/quality_report.json", "quality_report", nodes.QualityCheck))
	}
	if opts.SimilarityCheck || opts.Package {
		similarityPath := nodes.SimilarityReportPath(workspace)
		if _, reuseErr := packager.ValidateSimilarityReport(similarityPath, taskDir); reuseErr == nil {
			chain(nodes.SimilarityCheck, "harborfactory.similarity_report_import", []string{lastQuality}, 60, 1, map[string]any{
				"task_dir": taskDir, "report_path": similarityPath, "artifact_name": "phase2/artifacts/similarity_check/similarity_report.json",
			})
		} else {
			chain(nodes.SimilarityCheck, "harborfactory.similarity_check", []string{lastQuality}, 300, 2, map[string]any{
				"task_dir": taskDir, "repo_url": opts.RepoURL, "tests_analysis": testsAnalysisPath,
				"history_dirs": opts.SimilarityHistoryDirs, "tb3_dirs": opts.SimilarityTB3Dirs,
				"enable_github": opts.SimilarityGitHub, "threshold": opts.SimilarityThreshold,
				"artifact_name": "phase2/artifacts/similarity_check/similarity_report.json",
			})
		}
		lastQuality = nodes.SimilarityCheck
		finalEvidence = append(finalEvidence, artifact("phase2/artifacts/similarity_check/similarity_report.json", "similarity_report", nodes.SimilarityCheck))
	}
	gate(nodes.FinalReview, "phase2", "Final Quality Gate", "Review deterministic lint, runtime verification, semantic quality and similarity evidence before model trials.", nodes.CodeEdgeLint, []string{lastQuality}, finalEvidence...)

	runQwen, runOpus, err := harborModelSelection(opts.HarborModels)
	if err != nil {
		return workflow.WorkflowDefinition{}, err
	}
	runHarbor := opts.RunHarbor || opts.Package
	qwenProvided := strings.TrimSpace(opts.QwenResult) != ""
	opusProvided := strings.TrimSpace(opts.OpusResult) != ""
	resultDeps := []string{nodes.FinalReview}
	resultEvidence := []workflow.ArtifactRef{}
	if qwenProvided || (runHarbor && runQwen) {
		config := harborConfig(opts, taskDir, nodes.HarborRunQwen, defaultString(opts.QwenModel, domain.DefaultQwenModel))
		config["result_path"] = strings.TrimSpace(opts.QwenResult)
		chain(nodes.HarborRunQwen, "harborfactory.harbor_run_qwen", []string{nodes.FinalReview}, opts.HarborTimeout, 1, config)
		resultDeps = append(resultDeps, nodes.HarborRunQwen)
		resultEvidence = append(resultEvidence, artifact("phase3/artifacts/harbor_run_qwen/qwen_result.json", "trial_result", nodes.HarborRunQwen))
	}
	if opusProvided || (runHarbor && runOpus) {
		config := harborConfig(opts, taskDir, nodes.HarborRunOpus, defaultString(opts.OpusModel, domain.DefaultOpusModel))
		config["result_path"] = strings.TrimSpace(opts.OpusResult)
		chain(nodes.HarborRunOpus, "harborfactory.harbor_run_opus", []string{nodes.FinalReview}, opts.HarborTimeout, 1, config)
		resultDeps = append(resultDeps, nodes.HarborRunOpus)
		resultEvidence = append(resultEvidence, artifact("phase3/artifacts/harbor_run_opus/opus_result.json", "trial_result", nodes.HarborRunOpus))
	}
	needResultReview := runHarbor || qwenProvided || opusProvided || opts.Package
	submissionDependency := nodes.FinalReview
	if needResultReview {
		resultRestart := nodes.FinalReview
		if runHarbor && runQwen {
			resultRestart = nodes.HarborRunQwen
		} else if runHarbor && runOpus {
			resultRestart = nodes.HarborRunOpus
		}
		gate(nodes.ResultReview, "phase3", "Model Result Gate", "Confirm model identity, four independent trials, task digest, pass@4 screenshots and average-turn evidence.", resultRestart, dedupeDependencies(resultDeps), resultEvidence...)
		submissionDependency = nodes.ResultReview
	}

	qwenResult := strings.TrimSpace(opts.QwenResult)
	if qwenResult == "" && runHarbor && runQwen {
		qwenResult = nodes.QwenResultPath(workspace)
	}
	opusResult := strings.TrimSpace(opts.OpusResult)
	if opusResult == "" && runHarbor && runOpus {
		opusResult = nodes.OpusResultPath(workspace)
	}
	packageDependency := submissionDependency
	if needResultReview || opts.StrictSubmission || opts.Package {
		chain(nodes.SubmissionLint, "harborfactory.codeedge_lint", []string{submissionDependency}, 60, 1, map[string]any{
			"task_dir": taskDir, "repo_url": opts.RepoURL, "commit": opts.Commit, "tests_analysis": testsAnalysisPath,
			"qwen_result": qwenResult, "opus_result": opusResult, "qwen_screenshot": opts.QwenScreenshot, "opus_screenshot": opts.OpusScreenshot,
			"strict_submission": opts.StrictSubmission || opts.Package, "artifact_name": "phase2/artifacts/submission_lint/lint_report.json",
		})
		packageDependency = nodes.SubmissionLint
	}
	if opts.Generate && publishDestination != "" && !sameWorkflowPath(taskDir, publishDestination) {
		chain(nodes.PublishTask, "harborfactory.publish_task", []string{packageDependency}, 120, 1, map[string]any{
			"task_dir": taskDir, "destination_dir": publishDestination,
			"artifact_name": "phase3/artifacts/task_publish/publish_receipt.json",
		})
		packageDependency = nodes.PublishTask
	}
	if opts.Package {
		chain(nodes.Package, "harborfactory.package", []string{packageDependency}, 60, 1, map[string]any{
			"task_dir": taskDir, "output_dir": opts.OutputDir, "task_name": opts.TaskName, "code_lang": opts.CodeLang,
			"task_type": opts.TaskType, "application": opts.Application, "aht": opts.AHT, "description": opts.Description,
			"is_zero_to_one": opts.IsZeroToOne, "github_url": opts.RepoURL, "commit_id": opts.Commit,
			"tests_analysis": testsAnalysisPath, "verify_report": nodes.VerifyReportPath(workspace),
			"quality_report": nodes.QualityReportPath(workspace), "similarity_report": nodes.SimilarityReportPath(workspace),
			"qwen_result": qwenResult, "opus_result": opusResult, "qwen_screenshot": opts.QwenScreenshot, "opus_screenshot": opts.OpusScreenshot,
		})
	}
	return definition, nil
}

func sameWorkflowPath(left, right string) bool {
	leftAbs, leftErr := filepath.Abs(filepath.Clean(left))
	rightAbs, rightErr := filepath.Abs(filepath.Clean(right))
	return leftErr == nil && rightErr == nil && leftAbs == rightAbs
}

func agentConfig(opts RunnerOptions, artifactName string) map[string]any {
	timeout := opts.AgentTimeout
	if timeout <= 0 {
		timeout = 600
	}
	return map[string]any{
		"model": opts.Model, "reasoning_effort": opts.Reasoning, "timeout_seconds": timeout,
		"artifact_name": artifactName,
	}
}

func harborConfig(opts RunnerOptions, taskDir, nodeID, model string) map[string]any {
	agentEnv := append([]string(nil), opts.HarborAgentEnv...)
	for _, key := range []string{
		"CLAUDE_CODE_SUBAGENT_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL",
		"ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL", "ANTHROPIC_MODEL",
	} {
		agentEnv = upsertEnv(agentEnv, key, model)
	}
	if nodeID == nodes.HarborRunQwen {
		agentEnv = upsertEnv(agentEnv, "ANTHROPIC_BASE_URL", opts.QwenHarborBaseURL)
	} else {
		agentEnv = upsertEnv(agentEnv, "ANTHROPIC_BASE_URL", opts.OpusHarborBaseURL)
	}
	return map[string]any{
		"task_dir": taskDir, "node_id": nodeID, "model": model, "agent": defaultString(opts.HarborAgent, domain.DefaultHarborAgent),
		"agent_env":       agentEnv,
		"timeout_seconds": opts.HarborTimeout, "setup_timeout_seconds": opts.HarborSetupTimeout,
		"agent_cache_dir": opts.HarborAgentCacheDir, "preflight": opts.HarborPreflight,
		"concurrency": opts.HarborConcurrency, "attempts": domain.RequiredTrialCount, "infra_retries": opts.HarborInfraRetries,
	}
}

func mergeConfig(base, extra map[string]any) map[string]any {
	result := make(map[string]any, len(base)+len(extra))
	for key, value := range base {
		result[key] = value
	}
	for key, value := range extra {
		result[key] = value
	}
	return result
}

func compactDependency(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return []string{value}
}

func dedupeDependencies(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}
