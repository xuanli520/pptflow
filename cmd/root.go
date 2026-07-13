package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

func Execute() {
	root := NewRootCommand()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func NewRootCommand() *cobra.Command {
	lifecycle := &lifecycleCLIConfig{
		root: ".harbor-factory",
	}
	root := &cobra.Command{
		Use:          "harbor-factory",
		Short:        "Harbor Task Factory lifecycle control plane",
		SilenceUsage: true,
		Long: `Harbor Task Factory manages immutable Harbor task revisions,
frozen workflow runs, reviews, local package releases, and disposable
workspaces through its durable local control plane.`,
	}
	root.PersistentFlags().StringVar(&lifecycle.root, "root", lifecycle.root, "Managed Harbor Factory root directory")
	root.AddCommand(
		newTaskCommand(lifecycle),
		newRevisionCommand(lifecycle),
		newRunCommandV2(lifecycle),
		newReviewCommand(lifecycle),
		newReleaseCommand(lifecycle),
		newBudgetCommand(lifecycle),
		newWorkspaceCommand(lifecycle),
		newLifecycleTUICommand(lifecycle),
		newDoctorCommand(),
	)
	return root
}

// showCommandGroupHelp keeps invoking a V2 command group without a child
// convenient while making the group runnable so Cobra validates stray
// positional arguments instead of silently accepting removed subcommands.
func showCommandGroupHelp(command *cobra.Command, _ []string) error {
	return command.Help()
}
