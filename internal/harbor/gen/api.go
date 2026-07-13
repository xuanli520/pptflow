package gen

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/nodes"
	"github.com/purplevoid/harbor-factory/internal/workflow"
)

// ConversationOptions contains the runtime-only settings shared by one
// isolated generation node. Each Generate* function opens and closes its own
// multi-turn conversation.
type ConversationOptions struct {
	Workspace       string
	CheckpointRoot  string
	RunID           string
	Attempt         int
	ForceFresh      bool
	Model           string
	ReasoningEffort string
	TimeoutSeconds  int
	Agent           workflow.AgentRuntime
}

func GenerateRepoAnalysis(ctx context.Context, opts ConversationOptions, prepared domain.RepoPrepared) (domain.RepoAnalysis, error) {
	prompt, err := repoAnalyzePrompt(prepared)
	if err != nil {
		return domain.RepoAnalysis{}, fmt.Errorf("render repo analysis prompt: %w", err)
	}
	internal := conversationRunOptions(opts, prepared)
	var analysis domain.RepoAnalysis
	err = runJSONConversation(ctx, internal, conversationTimeout(opts), nodes.RepoAnalyze, prompt, &analysis, func() error {
		analysis.SchemaVersion = defaultString(analysis.SchemaVersion, "harbor.repo_analysis.v1")
		analysis.RepoURL = defaultString(analysis.RepoURL, prepared.RepoURL)
		analysis.CommitSHA = defaultString(analysis.CommitSHA, prepared.ResolvedCommit)
		return validateRepoAnalysis(analysis)
	})
	return analysis, err
}

func GenerateTaskProposal(ctx context.Context, opts ConversationOptions, prepared domain.RepoPrepared, analysis domain.RepoAnalysis) (domain.TaskProposal, error) {
	analysisJSON, err := marshalCanonicalJSON(analysis)
	if err != nil {
		return domain.TaskProposal{}, err
	}
	prompt, err := taskDesignPrompt(analysisJSON)
	if err != nil {
		return domain.TaskProposal{}, fmt.Errorf("render task design prompt: %w", err)
	}
	internal := conversationRunOptions(opts, prepared)
	var proposal domain.TaskProposal
	err = runJSONConversation(ctx, internal, conversationTimeout(opts), nodes.TaskDesign, prompt, &proposal, func() error {
		proposal.SchemaVersion = defaultString(proposal.SchemaVersion, "harbor.task_proposal.v1")
		applyProposalDefaults(&proposal, prepared)
		return validateTaskProposal(proposal)
	})
	return proposal, err
}

func GenerateTaskFiles(ctx context.Context, opts ConversationOptions, prepared domain.RepoPrepared, analysis domain.RepoAnalysis, proposal domain.TaskProposal) (domain.GeneratedTaskFiles, error) {
	analysisJSON, err := marshalCanonicalJSON(analysis)
	if err != nil {
		return domain.GeneratedTaskFiles{}, err
	}
	proposalJSON, err := marshalCanonicalJSON(proposal)
	if err != nil {
		return domain.GeneratedTaskFiles{}, err
	}
	prompt, err := taskFilesPrompt(analysisJSON, proposalJSON)
	if err != nil {
		return domain.GeneratedTaskFiles{}, fmt.Errorf("render task files prompt: %w", err)
	}
	internal := conversationRunOptions(opts, prepared)
	var files domain.GeneratedTaskFiles
	err = runJSONConversation(ctx, internal, conversationTimeout(opts), nodes.GenerateTaskFiles, prompt, &files, func() error {
		files.SchemaVersion = defaultString(files.SchemaVersion, "harbor.generated_task_files.v1")
		stampTaskFilesProvenance(&files, prepared, proposal)
		return validateTaskFiles(files)
	})
	return files, err
}

func Instruction(files domain.GeneratedTaskFiles) string {
	return ensureFinalNewline(files.InstructionMD)
}

func TaskTOML(proposal domain.TaskProposal) string {
	return renderTaskTOML(proposal)
}

func Dockerfile(prepared domain.RepoPrepared, proposal domain.TaskProposal) string {
	return renderDockerfile(prepared, proposal)
}

func SolveScript(files domain.GeneratedTaskFiles) string {
	return normalizeShellScript(files.SolveSH)
}

func TestScript(files domain.GeneratedTaskFiles) string {
	return normalizeTestScript(files.TestSH)
}

func TestsAnalysis(files domain.GeneratedTaskFiles, proposal domain.TaskProposal) string {
	return ensureTestsAnalysis(files.TestsAnalysis, proposal)
}

type CanonicalTask struct {
	Instruction   string
	TaskTOML      string
	Dockerfile    string
	SolveScript   string
	TestScript    string
	TestsAnalysis string
}

// MaterializeCanonicalTask atomically publishes exactly the canonical text
// artifacts selected by the workflow, preserving executable script modes.
func MaterializeCanonicalTask(taskDir string, files CanonicalTask) error {
	taskDir = filepath.Clean(strings.TrimSpace(taskDir))
	if taskDir == "" || taskDir == "." {
		return fmt.Errorf("task directory is required")
	}
	writes := []generatedFileWrite{
		{taskPath: filepath.Join(taskDir, "instruction.md"), content: files.Instruction, mode: 0o644},
		{taskPath: filepath.Join(taskDir, "task.toml"), content: files.TaskTOML, mode: 0o644},
		{taskPath: filepath.Join(taskDir, "environment", "Dockerfile"), content: files.Dockerfile, mode: 0o644},
		{taskPath: filepath.Join(taskDir, "solution", "solve.sh"), content: files.SolveScript, mode: 0o755},
		{taskPath: filepath.Join(taskDir, "tests", "test.sh"), content: files.TestScript, mode: 0o755},
		{taskPath: filepath.Join(taskDir, "tests_analysis.md"), content: files.TestsAnalysis, mode: 0o644},
	}
	return publishCanonicalTask(taskDir, writes)
}

func RuntimeSelfCheck(ctx context.Context, opts ConversationOptions, taskDir, logPath string) error {
	if strings.TrimSpace(logPath) == "" {
		logPath = nodes.AgentLogPath(opts.Workspace, nodes.RuntimeSelfCheck)
	}
	prompt, err := runtimeSelfCheckPrompt()
	if err != nil {
		return err
	}
	_, err = workflow.RunAgentTurn(ctx, opts.Agent, workflow.AgentTurnRequest{
		ProjectPath: taskDir, Prompt: prompt, Model: opts.Model, ReasoningEffort: opts.ReasoningEffort,
		SandboxMode: "danger-full-access", SandboxPolicy: "danger-full-access", NetworkAccess: true,
		WorkspaceRoots: []string{taskDir}, TimeoutSeconds: max(conversationTimeout(opts), 1800), MaxOutputBytes: 2 << 20, LogPath: logPath,
	})
	if err != nil {
		return fmt.Errorf("runtime self-check agent turn: %w", err)
	}
	return validateMaterializedTaskDir(taskDir)
}

func conversationRunOptions(opts ConversationOptions, prepared domain.RepoPrepared) conversationRuntimeOptions {
	return conversationRuntimeOptions{Workspace: opts.Workspace, CheckpointRoot: opts.CheckpointRoot, RunID: opts.RunID, Attempt: opts.Attempt, ForceFresh: opts.ForceFresh, Model: opts.Model, ReasoningEffort: opts.ReasoningEffort, Agent: opts.Agent, RepoPrepared: prepared}
}

func conversationTimeout(opts ConversationOptions) int {
	if opts.TimeoutSeconds > 0 {
		return opts.TimeoutSeconds
	}
	return 600
}

func marshalCanonicalJSON(value any) (string, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal canonical generation input: %w", err)
	}
	return string(data), nil
}
