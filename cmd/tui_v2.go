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
	return newLifecycleTUICommandWithRunner(config, tui.RunWithLifecycle)
}

type lifecycleTUIRunner func(context.Context, tui.TaskHubLifecycleService) error

func newLifecycleTUICommandWithRunner(config *lifecycleCLIConfig, runner lifecycleTUIRunner) *cobra.Command {
	return &cobra.Command{
		Use:   "tui",
		Short: "Open the V2 Task Hub",
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
			return runner(ctx, newLifecycleTUIAdapter(services))
		},
	}
}

// newLifecycleTUIAdapter keeps TUI composition explicit: the Task Hub can
// offer per-Run exit handoff only when its application adapter is supplied a
// controlled child-worker launcher. The launcher shares the `run detach`
// process boundary and never lets the TUI mutate worker state directly.
func newLifecycleTUIAdapter(services *app.LifecycleServices) *tui.AppTaskHubLifecycleAdapter {
	return tui.NewAppTaskHubLifecycleAdapterWithRunWorkerHandoffLauncher(services, executableRunWorkerLauncher{})
}
