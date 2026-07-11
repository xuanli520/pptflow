package cmd

import (
	"fmt"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/tui"
	"github.com/spf13/cobra"
)

func newTUICommand() *cobra.Command {
	var opts app.RunnerOptions
	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Open the interactive Harbor factory TUI",
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("auto-approve") {
				return fmt.Errorf("tui requires manual review gates; use run --auto-approve for headless automation")
			}
			opts.AutoApprove = false
			return tui.Run(cmd.Context(), opts)
		},
	}
	addRunnerFlags(cmd, &opts)
	if flag := cmd.Flags().Lookup("auto-approve"); flag != nil {
		flag.Hidden = true
	}
	return cmd
}
