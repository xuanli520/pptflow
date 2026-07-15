package cmd

import (
	"context"
	"fmt"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/tui"
	"github.com/spf13/cobra"
)

// newLifecycleTUICommand opens the new task-board TUI backed by lifecycle services.
func newLifecycleTUICommand(config *lifecycleCLIConfig) *cobra.Command {
	return newLifecycleTUICommandWithRunner(config, tui.RunNewTUIAdapter)
}

// lifecycleTUIRunner exposes the TUI session so tests can inject a different runner.
type lifecycleTUIRunner func(context.Context, *app.LifecycleServices) error

func newLifecycleTUICommandWithRunner(config *lifecycleCLIConfig, runner lifecycleTUIRunner) *cobra.Command {
	return &cobra.Command{
		Use:   "tui",
		Short: "Open the Harbor Task Factory TUI",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if config == nil {
				return fmt.Errorf("lifecycle configuration is required")
			}
			if runner == nil {
				return fmt.Errorf("lifecycle TUI runner is required")
			}
			if err := config.preflightLifecycleServices(); err != nil {
				return fmt.Errorf("preflight lifecycle deployment: %w", err)
			}
			dataStore, err := store.Open(config.root)
			if err != nil {
				return fmt.Errorf("open lifecycle control plane: %w", err)
			}
			defer dataStore.Close()
			services, err := config.openLifecycleServices(dataStore)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			return runner(ctx, services)
		},
	}
}
