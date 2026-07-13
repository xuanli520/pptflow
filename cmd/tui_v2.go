package cmd

import (
	"context"
	"fmt"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/tui"
	"github.com/spf13/cobra"
)

// newLifecycleTUICommand opens only the V2 service-backed Task Hub. The old
// workspace-runner TUI is deliberately not registered after the hard cutover:
// task identity, lifecycle mutations, and plans must come from the control
// plane rather than an arbitrary workspace path.
func newLifecycleTUICommand(config *lifecycleCLIConfig) *cobra.Command {
	return &cobra.Command{
		Use:   "tui",
		Short: "Open the V2 Task Hub",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if config == nil {
				return fmt.Errorf("lifecycle configuration is required")
			}
			dataStore, err := store.Open(config.root)
			if err != nil {
				return fmt.Errorf("open lifecycle control plane: %w", err)
			}
			defer dataStore.Close()
			services, err := app.NewLifecycleServices(config.root, dataStore)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			return tui.RunWithLifecycle(ctx, app.RunnerOptions{}, tui.NewAppTaskHubLifecycleAdapter(services))
		},
	}
}
