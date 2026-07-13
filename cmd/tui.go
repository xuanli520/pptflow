package cmd

import (
	"fmt"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/tui"
	"github.com/spf13/cobra"
)

func newTUICommand() *cobra.Command {
	var opts app.RunnerOptions
	var workspaceRoot string
	var rescan bool
	var taskConcurrency int
	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Open the interactive Harbor factory TUI",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := loadHarborEnvironment(); err != nil {
				return err
			}
			applyRunnerEnvironmentDefaults(&opts)
			if cmd.Flags().Changed("auto-approve") {
				return fmt.Errorf("tui requires manual review gates; use run --auto-approve for headless automation")
			}
			opts.AutoApprove = false
			return tui.RunWithOptions(cmd.Context(), opts, tui.RunOptions{
				WorkspaceRoot:     workspaceRoot,
				WorkspaceExplicit: cmd.Flags().Changed("workspace"),
				Rescan:            rescan,
				TaskConcurrency:   taskConcurrency,
			})
		},
	}
	addRunnerFlags(cmd, &opts)
	cmd.Flags().StringVar(&workspaceRoot, "workspace-root", ".harbor-factory", "Root directory for workspace history and the Task Hub index")
	cmd.Flags().BoolVar(&rescan, "rescan", false, "Rebuild the Task Hub index from workspace files before opening")
	cmd.Flags().IntVar(&taskConcurrency, "task-concurrency", app.MaxTaskConcurrency, "Maximum parallel TUI tasks (1-10)")
	if flag := cmd.Flags().Lookup("auto-approve"); flag != nil {
		flag.Hidden = true
	}
	return cmd
}
