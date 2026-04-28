package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

func newStatusCommand() *cobra.Command {
	var runID string
	command := &cobra.Command{
		Use:   "status <task-id>",
		Short: "Show run history and stage status",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig("")
			if err != nil {
				return err
			}
			store, err := openStore(cfg)
			if err != nil {
				return err
			}
			defer store.Close()
			ctx := context.Background()
			run, err := store.LatestRunForTask(ctx, args[0])
			if runID != "" {
				run, err = store.GetRun(ctx, runID)
			}
			if err != nil {
				return err
			}
			stages, err := store.Stages(ctx, run.RunID)
			if err != nil {
				return err
			}
			findings, err := store.Findings(ctx, run.RunID)
			if err != nil {
				return err
			}
			blocker, high := 0, 0
			for _, finding := range findings {
				switch finding.Severity {
				case "Blocker":
					blocker++
				case "High":
					high++
				}
			}
			fmt.Printf("task: %s\nrun: %s\nstatus: %s\nmanual_verdict: %s\nartifact_root: %s\n", run.TaskID, run.RunID, run.Status, run.ManualVerdict, run.ArtifactRoot)
			fmt.Printf("findings: Blocker=%d High=%d Total=%d\n", blocker, high, len(findings))
			for _, stage := range stages {
				fmt.Printf("[%s] %s", stage.Stage, stage.Status)
				if stage.ErrorSummary != "" {
					fmt.Printf(" - %s", stage.ErrorSummary)
				}
				fmt.Println()
			}
			return nil
		},
	}
	command.Flags().StringVar(&runID, "run", "", "specific run id")
	return command
}
