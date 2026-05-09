package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/xuanli520/p2r_tui/internal/db"
	"github.com/xuanli520/p2r_tui/internal/scanner"
)

func newScanCommand() *cobra.Command {
	var scanPath string
	var pruneArtifacts bool
	command := &cobra.Command{
		Use:   "scan",
		Short: "Scan for prompt2repo delivery packages",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(scanPath)
			if err != nil {
				return err
			}
			result, err := scanner.Scan(cfg.ScanPath)
			if err != nil {
				return err
			}
			store, err := openStore(cfg)
			if err != nil {
				return err
			}
			defer store.Close()
			ctx := cmd.Context()
			if err := store.UpsertProjects(ctx, result.Projects); err != nil {
				return err
			}
			var pruneResult db.ArtifactPruneResult
			if pruneArtifacts {
				pruned, err := store.PruneArtifactProjects(ctx, cfg.ScanPath)
				if err != nil {
					return err
				}
				pruneResult = pruned
			}
			fmt.Printf("[scan] scanning %s\n", result.Root)
			fmt.Printf("[scan] visited %d candidate directories, recognized %d p2r projects\n", result.VisitedDirs, len(result.Projects))
			if len(result.Projects) > 0 {
				fmt.Print("[scan] indexed:")
				for _, project := range result.Projects {
					fmt.Printf(" %s/%s", project.Batch, project.TaskID)
				}
				fmt.Println()
			}
			if pruneArtifacts {
				printPruneResult(pruneResult)
			}
			return nil
		},
	}
	command.Flags().StringVar(&scanPath, "path", "", "directory to scan")
	command.Flags().BoolVar(&pruneArtifacts, "prune-artifacts", false, "remove DB project rows that point at p2r artifact directories and have no runs")
	return command
}

func printPruneResult(result db.ArtifactPruneResult) {
	fmt.Printf("[scan] pruned artifact projects: removed=%d skipped=%d\n", len(result.Removed), len(result.Skipped))
	for _, item := range result.Removed {
		fmt.Printf("[scan] pruned %s path=%s\n", item.TaskID, item.Path)
	}
	for _, item := range result.Skipped {
		fmt.Printf("[scan] skipped %s path=%s blocked_by_runs=%d\n", item.TaskID, item.Path, item.Runs)
	}
}
