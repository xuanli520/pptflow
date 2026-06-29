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
		Use:   "pptflow",
		Short: "PPTflow — AI-driven visual presentation generator",
		Long: `PPTflow generates beautiful, editable PowerPoint presentations using a pipeline of:
  Codex (prompt optimization + content generation)
  Image2 (slide image generation with consistent styling)
  Codex (resource extraction + PPTX assembly)`,
	}
	root.AddCommand(newPromptFlowCommand())
	return root
}
