package cmd

import (
	"encoding/json"
	"fmt"

	harborstatus "github.com/purplevoid/harbor-factory/internal/harbor/status"
	"github.com/spf13/cobra"
)

func newStatusCommand() *cobra.Command {
	var workspace string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Inspect a Harbor factory workspace state",
		RunE: func(cmd *cobra.Command, args []string) error {
			report, err := harborstatus.ReadWorkspace(workspace)
			data, marshalErr := json.MarshalIndent(report, "", "  ")
			if marshalErr != nil {
				return marshalErr
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(data))
			return err
		},
	}
	cmd.Flags().StringVar(&workspace, "workspace", ".harbor-factory/workspace", "Workspace directory")
	return cmd
}
