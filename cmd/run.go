package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/xuanli520/p2r_tui/internal/pipeline"
)

func newRunCommand() *cobra.Command {
	var stage string
	var from string
	var staticOnly bool
	var mode string
	var refRun string
	var extraDocs []string
	var keepRuntime bool
	command := &cobra.Command{
		Use:   "run <task-id>",
		Short: "Run the p2r QA pipeline",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			stage = strings.ToUpper(stage)
			from = strings.ToUpper(from)
			if stage != "" && from != "" {
				return fmt.Errorf("--stage and --from are mutually exclusive")
			}
			if stage != "" && !validStage(stage) {
				return fmt.Errorf("invalid --stage %q; expected A..F", stage)
			}
			if from != "" && !validStage(from) {
				return fmt.Errorf("invalid --from %q; expected A..F", from)
			}
			cfg, err := loadConfig("")
			if err != nil {
				return err
			}
			store, err := openStore(cfg)
			if err != nil {
				return err
			}
			defer store.Close()
			runner := pipeline.NewRunner(store, cfg)
			result, err := runner.Run(context.Background(), args[0], pipeline.RunOptions{Stage: stage, From: from, StaticOnly: staticOnly, Mode: mode, RefRun: refRun, ExtraDocs: extraDocs, KeepRuntime: keepRuntime})
			if err != nil {
				return err
			}
			fmt.Printf("[run] task=%s run_id=%s\n", result.Run.TaskID, result.Run.RunID)
			for _, item := range result.Stages {
				fmt.Printf("[%s] %-36s %s", item.Stage, item.Name, item.Status)
				if item.ErrorSummary != "" {
					fmt.Printf(" (%s)", item.ErrorSummary)
				}
				fmt.Println()
			}
			fmt.Printf("[run] %s, output: %s\n", result.Run.Status, result.Run.ArtifactRoot)
			return nil
		},
	}
	command.Flags().StringVar(&stage, "stage", "", "run only one stage (A..F)")
	command.Flags().StringVar(&from, "from", "", "run from one stage through F (A..F)")
	command.Flags().BoolVar(&staticOnly, "static-only", false, "run only A, D, E, and F")
	command.Flags().StringVar(&mode, "mode", "initial", "QA mode: initial or recheck")
	command.Flags().StringVar(&refRun, "ref-run", "", "reference run id for --mode recheck")
	command.Flags().StringSliceVar(&extraDocs, "extra-docs", nil, "comma-separated extra document paths for --mode recheck")
	command.Flags().BoolVar(&keepRuntime, "keep-runtime", false, "keep the current Docker runtime after B/C for debugging")
	return command
}

func validStage(stage string) bool {
	switch stage {
	case "A", "B", "C", "D", "E", "F":
		return true
	default:
		return false
	}
}
