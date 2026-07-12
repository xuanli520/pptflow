package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/spf13/cobra"
)

func newRunCommand() *cobra.Command {
	var opts app.RunnerOptions
	opts.AutoApprove = true
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the Harbor factory workflow",
		RunE: func(cmd *cobra.Command, args []string) error {
			runner := app.NewRunner(opts)
			summary, err := runner.Run(cmd.Context())
			data, marshalErr := json.MarshalIndent(summary, "", "  ")
			if marshalErr != nil {
				return marshalErr
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(data))
			if err != nil {
				return err
			}
			if !summary.Passed {
				return fmt.Errorf("run failed")
			}
			return nil
		},
	}
	addRunnerFlags(cmd, &opts)
	return cmd
}

func addRunnerFlags(cmd *cobra.Command, opts *app.RunnerOptions) {
	applyRunnerEnvironmentDefaults(opts)
	cmd.Flags().StringVar(&opts.RepoURL, "repo", "", "GitHub repository URL")
	cmd.Flags().StringVar(&opts.Commit, "commit", "", "Concrete commit SHA")
	cmd.Flags().StringVar(&opts.TaskDir, "task", "", "Harbor task directory")
	cmd.Flags().BoolVar(&opts.Generate, "generate", false, "Generate a Harbor task from --repo and --commit before linting")
	cmd.Flags().StringVar(&opts.TaskOutputDir, "task-output", "", "Directory for generated Harbor task files")
	cmd.Flags().StringVar(&opts.Workspace, "workspace", ".harbor-factory/workspace", "Workspace directory")
	cmd.Flags().StringVar(&opts.TestsAnalysis, "tests-analysis", "", "tests analysis markdown path")
	cmd.Flags().StringVar(&opts.QwenResult, "qwen-result", "", "Qwen harbor run result JSON")
	cmd.Flags().StringVar(&opts.OpusResult, "opus-result", "", "Opus harbor run result JSON")
	cmd.Flags().BoolVar(&opts.VerifyDocker, "verify-docker", false, "Run Docker build, initial verification, and oracle verification")
	cmd.Flags().BoolVar(&opts.QualityCheck, "quality-check", false, "Run CodeEdge semantic quality checks")
	cmd.Flags().BoolVar(&opts.QualityAgent, "quality-agent", false, "Use Codex agent for optional semantic quality review")
	cmd.Flags().BoolVar(&opts.SimilarityCheck, "similarity-check", false, "Run issue/TB3/history similarity checks")
	cmd.Flags().BoolVar(&opts.SimilarityGitHub, "similarity-github", false, "Search GitHub issues and PRs for similarity")
	cmd.Flags().StringArrayVar(&opts.SimilarityHistoryDirs, "history-dir", nil, "Local history task directory for similarity scan")
	cmd.Flags().StringArrayVar(&opts.SimilarityTB3Dirs, "tb3-dir", nil, "Local TB3/dataset directory for similarity scan")
	cmd.Flags().Float64Var(&opts.SimilarityThreshold, "similarity-threshold", 0.42, "Similarity threshold that fails the run")
	cmd.Flags().StringVar(&opts.GitHubToken, "github-token", "", "GitHub token for similarity search")
	cmd.Flags().BoolVar(&opts.RunHarbor, "run-harbor", false, "Run Harbor pass@4 for Qwen and Opus before linting")
	cmd.Flags().StringVar(&opts.HarborModels, "harbor-models", "qwen,opus", "Comma-separated Harbor model stages to run: qwen, opus, or both")
	cmd.Flags().StringVar(&opts.HarborAgent, "harbor-agent", "claude-code", "Harbor agent name")
	cmd.Flags().StringArrayVar(&opts.HarborAgentEnv, "harbor-agent-env", opts.HarborAgentEnv, "Agent environment passed to harbor run as --ae KEY=VALUE")
	cmd.Flags().StringVar(&opts.QwenModel, "qwen-model", "qwen3.7-max", "Qwen model for Harbor pass@4")
	cmd.Flags().StringVar(&opts.OpusModel, "opus-model", "claude-opus-4-8", "Opus model for Harbor pass@4")
	cmd.Flags().StringVar(&opts.QwenHarborBaseURL, "qwen-harbor-base-url", opts.QwenHarborBaseURL, "ANTHROPIC_BASE_URL used only for the Qwen Harbor stage")
	cmd.Flags().StringVar(&opts.OpusHarborBaseURL, "opus-harbor-base-url", opts.OpusHarborBaseURL, "ANTHROPIC_BASE_URL used only for the Opus Harbor stage")
	cmd.Flags().IntVar(&opts.HarborTimeout, "harbor-timeout", 7200, "Harbor run timeout per model in seconds")
	cmd.Flags().IntVar(&opts.HarborSetupTimeout, "harbor-setup-timeout", 1200, "Harbor preflight/setup timeout in seconds")
	cmd.Flags().StringVar(&opts.HarborAgentCacheDir, "harbor-agent-cache", app.DefaultHarborAgentCacheDir(), "Host cache for pre-fetched Harbor agent binaries")
	cmd.Flags().BoolVar(&opts.HarborPreflight, "harbor-preflight", true, "Warm and validate the Harbor task before pass@4")
	cmd.Flags().IntVar(&opts.HarborConcurrency, "harbor-concurrency", 1, "Harbor concurrent trials per model (1 avoids cold setup races)")
	cmd.Flags().IntVar(&opts.HarborAttempts, "harbor-attempts", 4, "Harbor trial attempts per model")
	cmd.Flags().IntVar(&opts.HarborInfraRetries, "harbor-infra-retries", 1, "Retries for Harbor RuntimeError trials")
	cmd.Flags().BoolVar(&opts.Package, "package", false, "Package the Harbor task and write submission_report.json")
	cmd.Flags().StringVar(&opts.OutputDir, "output", ".harbor-factory/output", "Output directory for packaged task")
	cmd.Flags().BoolVar(&opts.StrictSubmission, "strict-submission", false, "Require CodeEdge submission artifacts during lint (automatically enabled by --package)")
	cmd.Flags().BoolVar(&opts.AutoApprove, "auto-approve", opts.AutoApprove, "Auto-approve review gates in headless run")
	cmd.Flags().StringVar(&opts.TaskName, "task-name", "", "Task name for package root and zip filename")
	cmd.Flags().StringVar(&opts.CodeLang, "code-lang", "", "CodeEdge claim field: primary code language")
	cmd.Flags().StringVar(&opts.TaskType, "task-type", "", "CodeEdge claim field: task type")
	cmd.Flags().StringVar(&opts.Application, "application", "", "CodeEdge claim field: application domain")
	cmd.Flags().StringVar(&opts.AHT, "aht", "", "Estimated human completion time")
	cmd.Flags().StringVar(&opts.Description, "description", "", "One-line task description")
	cmd.Flags().BoolVar(&opts.IsZeroToOne, "zero-to-one", false, "Whether the task is 0-1 code generation")
	cmd.Flags().StringVar(&opts.QwenScreenshot, "qwen-screenshot", "", "Qwen pass@4 screenshot path")
	cmd.Flags().StringVar(&opts.OpusScreenshot, "opus-screenshot", "", "Opus pass@4 screenshot path")
	cmd.Flags().StringVar(&opts.Model, "model", "", "Codex model for generation")
	cmd.Flags().StringVar(&opts.Reasoning, "reasoning", "", "Codex reasoning effort for generation")
	cmd.Flags().StringVar(&opts.CodexPath, "codex-path", "", "Path to Codex CLI")
	cmd.Flags().IntVar(&opts.AgentTimeout, "agent-timeout", 600, "Agent turn timeout in seconds")
}

// applyRunnerEnvironmentDefaults converts process credentials into references
// that Harbor can safely expand for trial containers. Secret values remain in
// the process environment and are never embedded in flags or run snapshots.
func applyRunnerEnvironmentDefaults(opts *app.RunnerOptions) {
	if opts == nil {
		return
	}
	if len(opts.HarborAgentEnv) == 0 {
		for _, key := range []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"} {
			if strings.TrimSpace(os.Getenv(key)) != "" {
				opts.HarborAgentEnv = []string{key + "=${" + key + "}"}
				break
			}
		}
	}
	fallbackBaseURL := strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL"))
	if strings.TrimSpace(opts.QwenHarborBaseURL) == "" {
		opts.QwenHarborBaseURL = environmentOrDefault("QWEN_HARBOR_BASE_URL", fallbackBaseURL)
	}
	if strings.TrimSpace(opts.OpusHarborBaseURL) == "" {
		opts.OpusHarborBaseURL = environmentOrDefault("OPUS_HARBOR_BASE_URL", fallbackBaseURL)
	}
}

func environmentOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
