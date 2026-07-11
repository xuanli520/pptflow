package gen

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/nodes"
	"github.com/purplevoid/harbor-factory/internal/harbor/repourl"
	"github.com/purplevoid/harbor-factory/internal/workflow"
)

type Options struct {
	RepoPrepared        domain.RepoPrepared
	Workspace           string
	TaskOutputDir       string
	TaskName            string
	Model               string
	ReasoningEffort     string
	AgentTimeoutSeconds int
	Agent               workflow.AgentRuntime
	Progress            func(nodeID, status, message, path string)
	TaskReview          func(analysis domain.RepoAnalysis, proposal domain.TaskProposal, proposalPath string) error
}

func Run(ctx context.Context, opts Options) (domain.GenReport, error) {
	if opts.Agent == nil {
		return domain.GenReport{}, fmt.Errorf("agent runtime is required")
	}
	if strings.TrimSpace(opts.RepoPrepared.SourcePath) == "" {
		return domain.GenReport{}, fmt.Errorf("prepared source path is required")
	}
	if strings.TrimSpace(opts.RepoPrepared.RepoURL) == "" || strings.TrimSpace(opts.RepoPrepared.ResolvedCommit) == "" {
		return domain.GenReport{}, fmt.Errorf("prepared repo URL and resolved commit are required")
	}
	if err := repourl.RejectCredentials(opts.RepoPrepared.RepoURL); err != nil {
		return domain.GenReport{}, err
	}
	workspace := strings.TrimSpace(opts.Workspace)
	if workspace == "" {
		workspace = filepath.Join(".harbor-factory", "workspace")
	}
	opts.Workspace = workspace
	timeout := opts.AgentTimeoutSeconds
	if timeout <= 0 {
		timeout = 600
	}

	progress(opts, nodes.RepoAnalyze, "running", "analyzing prepared repository", "")
	repoAnalysis, repoAnalysisPath, repoAnalysisReused, err := runRepoAnalyze(ctx, opts, workspace, timeout)
	if err != nil {
		progress(opts, nodes.RepoAnalyze, "failed", err.Error(), repoAnalysisPath)
		return domain.GenReport{}, err
	}
	progress(opts, nodes.RepoAnalyze, "succeeded", reuseStatusMessage(repoAnalysisReused, "repo analysis generated", "reused existing repo analysis"), repoAnalysisPath)

	progress(opts, nodes.TaskDesign, "running", "designing CodeEdge task", "")
	taskProposal, taskProposalPath, taskProposalReused, err := runTaskDesign(ctx, opts, workspace, timeout, repoAnalysis)
	if err != nil {
		progress(opts, nodes.TaskDesign, "failed", err.Error(), taskProposalPath)
		return domain.GenReport{}, err
	}
	progress(opts, nodes.TaskDesign, "succeeded", reuseStatusMessage(taskProposalReused, "task proposal generated", "reused existing task proposal"), taskProposalPath)
	if strings.TrimSpace(opts.TaskName) != "" {
		taskProposal.TaskName = opts.TaskName
	}
	applyProposalDefaults(&taskProposal, opts.RepoPrepared)
	if err := writeJSON(taskProposalPath, taskProposal); err != nil {
		progress(opts, nodes.TaskDesign, "failed", err.Error(), taskProposalPath)
		return domain.GenReport{}, err
	}
	if opts.TaskReview != nil {
		progress(opts, nodes.TaskReview, "running", "reviewing selected task proposal", taskProposalPath)
		if err := opts.TaskReview(repoAnalysis, taskProposal, taskProposalPath); err != nil {
			progress(opts, nodes.TaskReview, "failed", err.Error(), taskProposalPath)
			return domain.GenReport{}, err
		}
		progress(opts, nodes.TaskReview, "succeeded", "task proposal approved", taskProposalPath)
	}

	progress(opts, nodes.GenerateTaskFiles, "running", "generating Harbor task files", "")
	taskFiles, taskFilesPath, taskFilesReused, err := runTaskFiles(ctx, opts, workspace, timeout, repoAnalysis, taskProposal)
	if err != nil {
		progress(opts, nodes.GenerateTaskFiles, "failed", err.Error(), taskFilesPath)
		return domain.GenReport{}, err
	}
	progress(opts, nodes.GenerateTaskFiles, "succeeded", reuseStatusMessage(taskFilesReused, "task file content generated", "reused existing task file content"), taskFilesPath)

	taskDir := strings.TrimSpace(opts.TaskOutputDir)
	if taskDir == "" {
		taskDir = filepath.Join(workspace, "phase2", "task", safeTaskDir(taskProposal.TaskName))
	}
	testsAnalysisPath := nodes.TestsAnalysisPath(workspace)
	if err := materializeTask(opts, taskDir, testsAnalysisPath, opts.RepoPrepared, taskProposal, taskFiles); err != nil {
		progress(opts, nodes.MaterializeTask, "failed", err.Error(), taskDir)
		return domain.GenReport{}, err
	}
	progress(opts, nodes.MaterializeTask, "succeeded", "Harbor task directory materialized", taskDir)

	report := domain.GenReport{
		SchemaVersion:     "harbor.gen_report.v1",
		TaskDir:           taskDir,
		TestsAnalysisPath: testsAnalysisPath,
		RepoAnalysisPath:  repoAnalysisPath,
		TaskProposalPath:  taskProposalPath,
		TaskFilesPath:     taskFilesPath,
		RepoAnalysis:      repoAnalysis,
		TaskProposal:      taskProposal,
		CreatedAt:         time.Now().UTC(),
		Passed:            true,
	}
	reportPath := nodes.GenReportPath(workspace)
	if err := writeJSON(reportPath, report); err != nil {
		return report, err
	}
	return report, nil
}

func progress(opts Options, nodeID, status, message, path string) {
	if opts.Progress != nil {
		opts.Progress(nodeID, status, message, path)
	}
}

func reuseStatusMessage(reused bool, generated, reusedMessage string) string {
	if reused {
		return reusedMessage
	}
	return generated
}

func runRepoAnalyze(ctx context.Context, opts Options, workspace string, timeout int) (domain.RepoAnalysis, string, bool, error) {
	path := nodes.RepoAnalysisPath(workspace)
	if analysis, ok := loadReusableRepoAnalysis(path, opts.RepoPrepared); ok {
		return analysis, path, true, nil
	}
	prompt := repoAnalyzePrompt(opts.RepoPrepared)
	var analysis domain.RepoAnalysis
	if err := runJSONTurn(ctx, opts, timeout, nodes.RepoAnalyze, prompt, &analysis); err != nil {
		return domain.RepoAnalysis{}, path, false, err
	}
	analysis.SchemaVersion = defaultString(analysis.SchemaVersion, "harbor.repo_analysis.v1")
	analysis.RepoURL = defaultString(analysis.RepoURL, opts.RepoPrepared.RepoURL)
	analysis.CommitSHA = defaultString(analysis.CommitSHA, opts.RepoPrepared.ResolvedCommit)
	if err := validateRepoAnalysis(analysis); err != nil {
		return domain.RepoAnalysis{}, path, false, err
	}
	if err := writeJSON(path, analysis); err != nil {
		return domain.RepoAnalysis{}, path, false, err
	}
	return analysis, path, false, nil
}

func runTaskDesign(ctx context.Context, opts Options, workspace string, timeout int, analysis domain.RepoAnalysis) (domain.TaskProposal, string, bool, error) {
	path := nodes.TaskProposalPath(workspace)
	if proposal, ok := loadReusableTaskProposal(path, opts.RepoPrepared); ok {
		return proposal, path, true, nil
	}
	analysisJSON, _ := json.MarshalIndent(analysis, "", "  ")
	prompt := taskDesignPrompt(string(analysisJSON))
	var proposal domain.TaskProposal
	if err := runJSONTurn(ctx, opts, timeout, nodes.TaskDesign, prompt, &proposal); err != nil {
		return domain.TaskProposal{}, path, false, err
	}
	proposal.SchemaVersion = defaultString(proposal.SchemaVersion, "harbor.task_proposal.v1")
	applyProposalDefaults(&proposal, opts.RepoPrepared)
	if err := validateTaskProposal(proposal); err != nil {
		return domain.TaskProposal{}, path, false, err
	}
	if err := writeJSON(path, proposal); err != nil {
		return domain.TaskProposal{}, path, false, err
	}
	return proposal, path, false, nil
}

func runTaskFiles(ctx context.Context, opts Options, workspace string, timeout int, analysis domain.RepoAnalysis, proposal domain.TaskProposal) (domain.GeneratedTaskFiles, string, bool, error) {
	path := nodes.TaskFilesPath(workspace)
	if files, ok := loadReusableTaskFiles(path, opts.RepoPrepared, proposal); ok {
		return files, path, true, nil
	}
	analysisJSON, _ := json.MarshalIndent(analysis, "", "  ")
	proposalJSON, _ := json.MarshalIndent(proposal, "", "  ")
	prompt := taskFilesPrompt(string(analysisJSON), string(proposalJSON))
	var files domain.GeneratedTaskFiles
	if err := runJSONTurn(ctx, opts, timeout, nodes.GenerateTaskFiles, prompt, &files); err != nil {
		return domain.GeneratedTaskFiles{}, path, false, err
	}
	files.SchemaVersion = defaultString(files.SchemaVersion, "harbor.generated_task_files.v1")
	stampTaskFilesProvenance(&files, opts.RepoPrepared, proposal)
	if err := validateTaskFiles(files); err != nil {
		return domain.GeneratedTaskFiles{}, path, false, err
	}
	if err := writeJSON(path, files); err != nil {
		return domain.GeneratedTaskFiles{}, path, false, err
	}
	return files, path, false, nil
}

func loadReusableRepoAnalysis(path string, prepared domain.RepoPrepared) (domain.RepoAnalysis, bool) {
	var analysis domain.RepoAnalysis
	if !readReusableJSON(path, &analysis) {
		return domain.RepoAnalysis{}, false
	}
	analysis.SchemaVersion = defaultString(analysis.SchemaVersion, "harbor.repo_analysis.v1")
	if err := validateRepoAnalysis(analysis); err != nil {
		return domain.RepoAnalysis{}, false
	}
	if strings.TrimSpace(analysis.RepoURL) != strings.TrimSpace(prepared.RepoURL) {
		return domain.RepoAnalysis{}, false
	}
	if strings.TrimSpace(analysis.CommitSHA) != strings.TrimSpace(prepared.ResolvedCommit) {
		return domain.RepoAnalysis{}, false
	}
	return analysis, true
}

func loadReusableTaskProposal(path string, prepared domain.RepoPrepared) (domain.TaskProposal, bool) {
	var proposal domain.TaskProposal
	if !readReusableJSON(path, &proposal) {
		return domain.TaskProposal{}, false
	}
	proposal.SchemaVersion = defaultString(proposal.SchemaVersion, "harbor.task_proposal.v1")
	applyProposalDefaults(&proposal, prepared)
	if err := validateTaskProposal(proposal); err != nil {
		return domain.TaskProposal{}, false
	}
	if strings.TrimSpace(proposal.GitHubLink) != strings.TrimSpace(prepared.RepoURL) {
		return domain.TaskProposal{}, false
	}
	if strings.TrimSpace(proposal.CommitSHA) != strings.TrimSpace(prepared.ResolvedCommit) {
		return domain.TaskProposal{}, false
	}
	return proposal, true
}

func loadReusableTaskFiles(path string, prepared domain.RepoPrepared, proposal domain.TaskProposal) (domain.GeneratedTaskFiles, bool) {
	var files domain.GeneratedTaskFiles
	if !readReusableJSON(path, &files) {
		return domain.GeneratedTaskFiles{}, false
	}
	files.SchemaVersion = defaultString(files.SchemaVersion, "harbor.generated_task_files.v1")
	if err := validateTaskFiles(files); err != nil {
		return domain.GeneratedTaskFiles{}, false
	}
	if strings.TrimSpace(files.RepoURL) != strings.TrimSpace(prepared.RepoURL) {
		return domain.GeneratedTaskFiles{}, false
	}
	if strings.TrimSpace(files.CommitSHA) != strings.TrimSpace(prepared.ResolvedCommit) {
		return domain.GeneratedTaskFiles{}, false
	}
	if strings.TrimSpace(files.TaskProposalDigest) != taskProposalDigest(proposal) {
		return domain.GeneratedTaskFiles{}, false
	}
	return files, true
}

func readReusableJSON(path string, target any) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return json.Unmarshal(raw, target) == nil
}

func runJSONTurn(ctx context.Context, opts Options, timeout int, nodeID, prompt string, target any) error {
	result, err := opts.Agent.Turn(ctx, workflow.AgentTurnRequest{
		ProjectPath:     opts.RepoPrepared.SourcePath,
		Prompt:          prompt,
		Model:           opts.Model,
		ReasoningEffort: opts.ReasoningEffort,
		SandboxMode:     "read-only",
		SandboxPolicy:   "readOnly",
		NetworkAccess:   false,
		WorkspaceRoots:  []string{opts.RepoPrepared.SourcePath},
		TimeoutSeconds:  timeout,
		MaxOutputBytes:  2 << 20,
		LogPath:         nodes.AgentLogPath(opts.Workspace, nodeID),
	})
	if err != nil {
		return fmt.Errorf("%s agent turn: %w", nodeID, err)
	}
	raw, err := extractJSONObject(result.Text)
	if err != nil {
		return fmt.Errorf("%s parse JSON: %w", nodeID, err)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("%s decode JSON: %w", nodeID, err)
	}
	return nil
}

func materializeTask(opts Options, taskDir, testsAnalysisPath string, prepared domain.RepoPrepared, proposal domain.TaskProposal, files domain.GeneratedTaskFiles) error {
	workspace := strings.TrimSpace(opts.Workspace)
	if workspace == "" {
		workspace = filepath.Join(".harbor-factory", "workspace")
	}
	for _, dir := range []string{
		taskDir,
		filepath.Join(taskDir, "environment"),
		filepath.Join(taskDir, "solution"),
		filepath.Join(taskDir, "tests"),
		filepath.Dir(testsAnalysisPath),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	writes := []struct {
		nodeID       string
		taskPath     string
		artifactPath string
		content      string
		mode         os.FileMode
	}{
		{nodes.InstructionGen, filepath.Join(taskDir, "instruction.md"), nodes.InstructionPath(workspace), ensureFinalNewline(files.InstructionMD), 0o644},
		{nodes.TaskTOMLGen, filepath.Join(taskDir, "task.toml"), nodes.TaskTOMLPath(workspace), renderTaskTOML(proposal), 0o644},
		{nodes.DockerfileGen, filepath.Join(taskDir, "environment", "Dockerfile"), nodes.DockerfilePath(workspace), renderDockerfile(prepared, proposal), 0o644},
		{nodes.SolveGen, filepath.Join(taskDir, "solution", "solve.sh"), nodes.SolvePath(workspace), normalizeShellScript(files.SolveSH), 0o755},
		{nodes.TestGen, filepath.Join(taskDir, "tests", "test.sh"), nodes.TestPath(workspace), normalizeTestScript(files.TestSH), 0o755},
		{nodes.TestsAnalysis, filepath.Join(taskDir, "tests_analysis.md"), testsAnalysisPath, ensureTestsAnalysis(files.TestsAnalysis, proposal), 0o644},
	}
	for _, write := range writes {
		progress(opts, write.nodeID, "running", "writing "+filepath.Base(write.artifactPath), write.artifactPath)
		if err := writeGeneratedFile(write.artifactPath, write.content, write.mode); err != nil {
			progress(opts, write.nodeID, "failed", err.Error(), write.artifactPath)
			return err
		}
		if write.taskPath != write.artifactPath {
			if err := writeGeneratedFile(write.taskPath, write.content, write.mode); err != nil {
				progress(opts, write.nodeID, "failed", err.Error(), write.taskPath)
				return err
			}
		}
		progress(opts, write.nodeID, "succeeded", filepath.Base(write.artifactPath)+" written", write.artifactPath)
	}
	if err := validateMaterializedTaskDir(taskDir); err != nil {
		return err
	}
	return nil
}

func validateMaterializedTaskDir(taskDir string) error {
	allowed := map[string]bool{
		"instruction.md":                  true,
		"task.toml":                       true,
		"tests_analysis.md":               true,
		"environment/Dockerfile":          true,
		"environment/docker-compose.yaml": true,
		"solution/solve.sh":               true,
		"tests/test.sh":                   true,
	}
	return filepath.WalkDir(taskDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(taskDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink is not allowed in generated Harbor task directory: %s", rel)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("non-regular file is not allowed in generated Harbor task directory: %s", rel)
		}
		if !allowed[rel] {
			return fmt.Errorf("unexpected file in generated Harbor task directory: %s", rel)
		}
		if legacyDomainMatch(rel) {
			return fmt.Errorf("legacy non-Harbor domain file is not allowed in generated task: %s", rel)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if legacyDomainMatch(string(raw)) {
			return fmt.Errorf("legacy non-Harbor domain content is not allowed in generated task: %s", rel)
		}
		return nil
	})
}

func legacyDomainMatch(value string) bool {
	lower := strings.ToLower(value)
	for _, term := range []string{"pptflow", "promptflow", "image2", "powerpoint", "presentation", "slide"} {
		if strings.Contains(lower, term) {
			return true
		}
	}
	return false
}

func writeGeneratedFile(path, content string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), mode)
}

func validateRepoAnalysis(analysis domain.RepoAnalysis) error {
	if strings.TrimSpace(analysis.RepoURL) == "" {
		return fmt.Errorf("repo_analysis repo_url is required")
	}
	if strings.TrimSpace(analysis.CommitSHA) == "" {
		return fmt.Errorf("repo_analysis commit_sha is required")
	}
	if strings.TrimSpace(analysis.Language) == "" {
		return fmt.Errorf("repo_analysis language is required")
	}
	if strings.TrimSpace(analysis.BuildSystem) == "" {
		return fmt.Errorf("repo_analysis build_system is required")
	}
	return nil
}

func validateTaskProposal(proposal domain.TaskProposal) error {
	if strings.TrimSpace(proposal.TaskName) == "" {
		return fmt.Errorf("task_proposal task_name is required")
	}
	if strings.TrimSpace(proposal.OneLineDescription) == "" {
		return fmt.Errorf("task_proposal one_line_description is required")
	}
	if strings.TrimSpace(proposal.CodeLang) == "" {
		return fmt.Errorf("task_proposal code_lang is required")
	}
	if strings.TrimSpace(proposal.TaskType) == "" {
		return fmt.Errorf("task_proposal task_type is required")
	}
	if strings.TrimSpace(proposal.Application) == "" {
		return fmt.Errorf("task_proposal application is required")
	}
	if !proposal.IsZeroToOne && strings.TrimSpace(proposal.GitHubLink) == "" {
		return fmt.Errorf("task_proposal github_link is required for non 0-1 task")
	}
	if !commitPattern.MatchString(proposal.CommitSHA) {
		return fmt.Errorf("task_proposal commit_sha must be a concrete 7-40 hex commit")
	}
	return nil
}

func validateTaskFiles(files domain.GeneratedTaskFiles) error {
	if strings.TrimSpace(files.InstructionMD) == "" {
		return fmt.Errorf("generated instruction_md is required")
	}
	if strings.TrimSpace(files.SolveSH) == "" {
		return fmt.Errorf("generated solve_sh is required")
	}
	if strings.TrimSpace(files.TestSH) == "" {
		return fmt.Errorf("generated test_sh is required")
	}
	if strings.TrimSpace(files.TestsAnalysis) == "" {
		return fmt.Errorf("generated tests_analysis_md is required")
	}
	return nil
}

func stampTaskFilesProvenance(files *domain.GeneratedTaskFiles, prepared domain.RepoPrepared, proposal domain.TaskProposal) {
	files.RepoURL = strings.TrimSpace(prepared.RepoURL)
	files.CommitSHA = strings.TrimSpace(prepared.ResolvedCommit)
	files.TaskProposalDigest = taskProposalDigest(proposal)
}

func taskProposalDigest(proposal domain.TaskProposal) string {
	data, _ := json.Marshal(proposal)
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func applyProposalDefaults(proposal *domain.TaskProposal, prepared domain.RepoPrepared) {
	proposal.SchemaVersion = defaultString(proposal.SchemaVersion, "harbor.task_proposal.v1")
	proposal.TaskName = safeHarborTaskName(defaultString(proposal.TaskName, "codeedge/generated-task"))
	proposal.GitHubLink = defaultString(proposal.GitHubLink, prepared.RepoURL)
	proposal.CommitSHA = defaultString(proposal.CommitSHA, prepared.ResolvedCommit)
	if proposal.EstimatedAHTMinutes <= 0 {
		proposal.EstimatedAHTMinutes = 45
	}
	if strings.TrimSpace(proposal.CodeLang) == "" {
		proposal.CodeLang = "unknown"
	}
	if strings.TrimSpace(proposal.TaskType) == "" {
		proposal.TaskType = "bug-fix"
	}
	if strings.TrimSpace(proposal.Application) == "" {
		proposal.Application = "software-engineering"
	}
}

func renderTaskTOML(proposal domain.TaskProposal) string {
	keywords := []string{"codeedge", proposal.CodeLang, proposal.TaskType, proposal.Application}
	var b strings.Builder
	b.WriteString("schema_version = \"1.3\"\n\n")
	b.WriteString("[task]\n")
	b.WriteString("name = " + tomlString(proposal.TaskName) + "\n")
	b.WriteString("description = " + tomlString(proposal.OneLineDescription) + "\n")
	b.WriteString("keywords = " + tomlStringList(keywords) + "\n\n")
	b.WriteString("[metadata]\n")
	b.WriteString("code_lang = " + tomlString(proposal.CodeLang) + "\n")
	b.WriteString("task_type = " + tomlString(proposal.TaskType) + "\n")
	b.WriteString("application = " + tomlString(proposal.Application) + "\n")
	b.WriteString("is_0_to_1 = " + strconv.FormatBool(proposal.IsZeroToOne) + "\n")
	b.WriteString("github_url = " + tomlString(proposal.GitHubLink) + "\n")
	b.WriteString("commit_id = " + tomlString(proposal.CommitSHA) + "\n")
	b.WriteString("estimated_aht_minutes = " + strconv.Itoa(proposal.EstimatedAHTMinutes) + "\n")
	b.WriteString("difficulty_explanation = " + tomlString(proposal.DifficultyRationale) + "\n")
	b.WriteString("target_files = " + tomlStringList(proposal.TargetFiles) + "\n")
	b.WriteString("affected_modules = " + tomlStringList(proposal.AffectedModules) + "\n")
	b.WriteString("boundary_conditions = " + tomlStringList(proposal.BoundaryConditions) + "\n\n")
	b.WriteString("[verifier]\n")
	b.WriteString("timeout_sec = 600.0\n\n")
	b.WriteString("[agent]\n")
	b.WriteString("timeout_sec = 1800.0\n\n")
	b.WriteString("[environment]\n")
	b.WriteString("build_timeout_sec = 600.0\n")
	b.WriteString("network_mode = \"public\"\n")
	b.WriteString("os = \"linux\"\n")
	return b.String()
}

func renderDockerfile(prepared domain.RepoPrepared, proposal domain.TaskProposal) string {
	var b strings.Builder
	b.WriteString("FROM ubuntu:24.04\n\n")
	b.WriteString("ENV DEBIAN_FRONTEND=noninteractive\n")
	b.WriteString("RUN apt-get update && apt-get install -y --no-install-recommends ")
	b.WriteString(strings.Join(basePackages(proposal.CodeLang), " "))
	b.WriteString(" && rm -rf /var/lib/apt/lists/*\n\n")
	b.WriteString("WORKDIR /app\n")
	b.WriteString("RUN git clone ")
	b.WriteString(shellQuote(prepared.RepoURL))
	b.WriteString(" /app/repo && cd /app/repo && git checkout ")
	b.WriteString(shellQuote(prepared.ResolvedCommit))
	b.WriteString("\n")
	for _, command := range proposal.SetupCommands {
		command = strings.TrimSpace(command)
		if safeSetupCommand(command) {
			b.WriteString("RUN cd /app/repo && ")
			b.WriteString(command)
			b.WriteString("\n")
		}
	}
	b.WriteString("\nWORKDIR /app/repo\n")
	return b.String()
}

func basePackages(language string) []string {
	packages := []string{"ca-certificates", "git", "bash", "curl", "build-essential"}
	lower := strings.ToLower(language)
	switch {
	case strings.Contains(lower, "go"):
		packages = append(packages, "golang-go")
	case strings.Contains(lower, "python"):
		packages = append(packages, "python3", "python3-pip", "python3-pytest")
	case strings.Contains(lower, "javascript"), strings.Contains(lower, "typescript"), strings.Contains(lower, "node"):
		packages = append(packages, "nodejs", "npm")
	case strings.Contains(lower, "rust"):
		packages = append(packages, "cargo", "rustc")
	case strings.Contains(lower, "java"), strings.Contains(lower, "kotlin"):
		packages = append(packages, "openjdk-17-jdk", "maven", "gradle")
	case strings.Contains(lower, "ruby"):
		packages = append(packages, "ruby", "ruby-dev")
	}
	return packages
}

func normalizeShellScript(content string) string {
	body := cleanScriptBody(content)
	return "#!/usr/bin/env bash\nset -euo pipefail\n\n" + ensureFinalNewline(body)
}

func normalizeTestScript(content string) string {
	body := cleanScriptBody(content)
	if strings.Contains(body, "/logs/verifier/reward") {
		return "#!/usr/bin/env bash\nset -euo pipefail\n\n" + ensureFinalNewline(body)
	}
	return strings.Join([]string{
		"#!/usr/bin/env bash",
		"set -euo pipefail",
		"mkdir -p /logs/verifier",
		"finish() {",
		"  status=$?",
		"  if [ \"$status\" -eq 0 ]; then",
		"    echo 1 > /logs/verifier/reward.txt",
		"  else",
		"    echo 0 > /logs/verifier/reward.txt",
		"  fi",
		"  exit \"$status\"",
		"}",
		"trap finish EXIT",
		"",
		body,
		"",
	}, "\n")
}

func ensureTestsAnalysis(content string, proposal domain.TaskProposal) string {
	content = strings.TrimSpace(content)
	required := []string{
		"## 1. instruction 和 environment 已提供的信息",
		"## 2. 模型的理论通过路径",
		"## 3. 模型具备通过条件的依据",
	}
	ok := true
	for _, heading := range required {
		if !strings.Contains(content, heading) {
			ok = false
			break
		}
	}
	if ok && generatedTestsAnalysisSubstantive(content, required) {
		return ensureFinalNewline(content)
	}
	return fmt.Sprintf(`## 1. instruction 和 environment 已提供的信息
- instruction 描述的任务: %s
- environment 通过 Dockerfile 从 %s clone 固定 commit %s, 任务在 /app/repo 中执行。
- 关键约束: 不泄露标准答案, 不要求模型读取 tests 或 solution, 不依赖私有凭证。

---

## 2. 模型的理论通过路径
- 阅读 instruction 和仓库源码, 定位目标文件或相关模块: %s
- 按任务类型 %s 完成实现, 保持既有行为和边界条件。
- 运行项目测试或 instruction 建议命令, 修正失败并确认行为符合要求。

---

## 3. 模型具备通过条件的依据
- verifier 的核心检查点应来自 instruction、仓库现有行为和 environment 中可见的运行方式。
- 模型可根据任务描述、目标模块和建议验证方式判断交付边界。
- 不存在只能从 tests/test.sh 得知的隐藏业务要求。

原始生成备注:
%s
`, proposal.OneLineDescription, proposal.GitHubLink, proposal.CommitSHA, strings.Join(proposal.TargetFiles, ", "), proposal.TaskType, content)
}

func generatedTestsAnalysisSubstantive(content string, headings []string) bool {
	for i, heading := range headings {
		start := strings.Index(content, heading)
		if start < 0 {
			return false
		}
		start += len(heading)
		end := len(content)
		for _, next := range headings[i+1:] {
			if idx := strings.Index(content[start:], next); idx >= 0 {
				end = start + idx
				break
			}
		}
		if !generatedTestsAnalysisSectionSubstantive(content[start:end]) {
			return false
		}
	}
	return true
}

func generatedTestsAnalysisSectionSubstantive(section string) bool {
	bullets := 0
	chars := 0
	for _, line := range strings.Split(section, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "---"))
		if line == "" {
			continue
		}
		trimmed := strings.TrimLeft(line, "-*0123456789.、) ")
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(line, "-") || strings.HasPrefix(line, "*") || line[0] >= '0' && line[0] <= '9' {
			bullets++
		}
		chars += len([]rune(trimmed))
	}
	return bullets >= 1 && chars >= 40
}

func cleanScriptBody(content string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	lines := strings.Split(strings.TrimSpace(content), "\n")
	for len(lines) > 0 && strings.HasPrefix(strings.TrimSpace(lines[0]), "#!") {
		lines = lines[1:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func extractJSONObject(text string) ([]byte, error) {
	data := []byte(text)
	for idx, b := range data {
		if b != '{' {
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(data[idx:]))
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err == nil && len(raw) > 0 {
			return raw, nil
		}
	}
	return nil, fmt.Errorf("no JSON object found")
}

func writeJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func defaultString(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return strings.TrimSpace(fallback)
	}
	return value
}

var (
	commitPattern       = regexp.MustCompile(`(?i)^[0-9a-f]{7,40}$`)
	taskNameChars       = regexp.MustCompile(`[^a-z0-9._/-]+`)
	taskDirChars        = regexp.MustCompile(`[^a-z0-9._-]+`)
	forbiddenSetupToken = regexp.MustCompile(`(?i)(\bcopy\b|\badd\b|tests/|solution/|/logs/verifier|reward\.txt|reward\.json)`)
)

func safeHarborTaskName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.ReplaceAll(name, " ", "-")
	name = taskNameChars.ReplaceAllString(name, "-")
	name = strings.Trim(name, "./-")
	if name == "" {
		return "codeedge/generated-task"
	}
	if !strings.Contains(name, "/") {
		name = "codeedge/" + name
	}
	return name
}

func safeTaskDir(name string) string {
	parts := strings.Split(safeHarborTaskName(name), "/")
	name = parts[len(parts)-1]
	name = taskDirChars.ReplaceAllString(name, "-")
	name = strings.Trim(name, ".-")
	if name == "" {
		return "generated-task"
	}
	return name
}

func safeSetupCommand(command string) bool {
	return command != "" && !forbiddenSetupToken.MatchString(command)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func tomlString(value string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

func tomlStringList(values []string) string {
	if len(values) == 0 {
		return "[]"
	}
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		quoted = append(quoted, tomlString(value))
	}
	if len(quoted) == 0 {
		return "[]"
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func ensureFinalNewline(content string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return "\n"
	}
	return content + "\n"
}
