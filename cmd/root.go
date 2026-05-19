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
		Use:   "p2r",
		Short: "prompt2repo QA orchestration CLI",
	}
	root.AddCommand(newScanCommand())
	root.AddCommand(newRunCommand())
	root.AddCommand(newAttachCommand())
	root.AddCommand(newDocsCommand())
	root.AddCommand(newStatusCommand())
	root.AddCommand(newTUICommand())
	root.AddCommand(newAdminCommand())
	root.AddCommand(newVersionCommand())
	return root
}
