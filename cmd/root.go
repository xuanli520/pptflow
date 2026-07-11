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
	root := &cobra.Command{
		Use:          "harbor-factory",
		Short:        "Harbor Task Factory — CodeEdge task generation and validation",
		SilenceUsage: true,
		Long: `Harbor Task Factory prepares, validates, and packages Harbor benchmark tasks.

The fork removes non-Harbor domain code and keeps reusable
workflow, executor, Codex, and command runtime infrastructure for Harbor tasks.`,
	}
	root.AddCommand(
		newRepoPrepareCommand(),
		newLintCommand(),
		newRunCommand(),
		newTUICommand(),
		newStatusCommand(),
		newDoctorCommand(),
	)
	return root
}
