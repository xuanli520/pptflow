package cmd

import (
	"fmt"

	"github.com/purplevoid/harbor-factory/internal/harbor/lint"
	"github.com/spf13/cobra"
)

func newLintCommand() *cobra.Command {
	var opts lint.Options
	opts.StrictSubmission = true
	cmd := &cobra.Command{
		Use:   "lint",
		Short: "Run deterministic CodeEdge checks against a Harbor task",
		RunE: func(cmd *cobra.Command, args []string) error {
			report, err := lint.Run(cmd.Context(), opts)
			if err != nil {
				return err
			}
			data, err := lint.MarshalReport(report)
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), string(data))
			if !report.Passed {
				return fmt.Errorf("lint failed")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.TaskDir, "task", "", "Harbor task directory")
	cmd.Flags().StringVar(&opts.ZipPath, "zip", "", "Harbor task zip path")
	cmd.Flags().StringVar(&opts.RepoURL, "repo", "", "Expected GitHub repository URL")
	cmd.Flags().StringVar(&opts.Commit, "commit", "", "Expected commit SHA")
	cmd.Flags().StringVar(&opts.QwenResult, "qwen-result", "", "Qwen harbor run result JSON")
	cmd.Flags().StringVar(&opts.OpusResult, "opus-result", "", "Opus harbor run result JSON")
	cmd.Flags().StringVar(&opts.QwenScreenshot, "qwen-screenshot", "", "Qwen pass@4 screenshot path")
	cmd.Flags().StringVar(&opts.OpusScreenshot, "opus-screenshot", "", "Opus pass@4 screenshot path")
	cmd.Flags().StringVar(&opts.TestsAnalysis, "tests-analysis", "", "tests analysis markdown path")
	cmd.Flags().BoolVar(&opts.StrictSubmission, "strict-submission", opts.StrictSubmission, "Require CodeEdge submission artifacts such as tests analysis, Harbor results, and screenshots")
	cmd.Flags().StringVar(&opts.WriteReport, "write-report", "", "Optional path to write lint report JSON")
	return cmd
}
