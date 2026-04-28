package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func Execute() {
	root := NewRootCommand()
	if err := root.Execute(); err != nil {
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
	root.AddCommand(newStatusCommand())
	root.AddCommand(newTUICommand())
	return root
}
