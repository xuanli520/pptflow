package cmd

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/xuanli520/pptflow/internal/app"
)

func newPhase0Command() *cobra.Command {
	var scenario string
	var fixture string
	var template string
	var artifacts string
	var workspace string
	command := &cobra.Command{
		Use:   "phase0",
		Short: "Run PPTflow phase 0 locally",
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := app.RunPhase0(cmd.Context(), app.Phase0Options{
				Scenario:      scenario,
				FixturePath:   absPath(fixture),
				TemplatePath:  absPath(template),
				ArtifactRoot:  artifacts,
				WorkspaceRoot: workspace,
			})
			if err != nil {
				return err
			}
			fmt.Printf("run_id=%s\n", result.RunID)
			fmt.Printf("status=%s\n", result.Status)
			fmt.Printf("artifact_root=%s\n", result.ArtifactRoot)
			return nil
		},
	}
	command.Flags().StringVar(&scenario, "scenario", "performance_review", "phase 0 scenario")
	command.Flags().StringVar(&fixture, "fixture", "", "requirements fixture json")
	command.Flags().StringVar(&template, "template", "", "template pptx path")
	command.Flags().StringVar(&artifacts, "artifacts", "artifacts", "artifact root")
	command.Flags().StringVar(&workspace, "workspace", "workspace", "workspace root")
	return command
}

func absPath(path string) string {
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
}
