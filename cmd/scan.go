package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/xuanli520/p2r_tui/internal/scanner"
)

func newScanCommand() *cobra.Command {
	var scanPath string
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
			if err := store.UpsertProjects(context.Background(), result.Projects); err != nil {
				return err
			}
			fmt.Printf("[scan] scanning %s\n", result.Root)
			fmt.Printf("[scan] visited %d directories, recognized %d p2r projects\n", result.VisitedDirs, len(result.Projects))
			if len(result.Projects) > 0 {
				fmt.Print("[scan] indexed:")
				for _, project := range result.Projects {
					fmt.Printf(" %s", project.TaskID)
				}
				fmt.Println()
			}
			return nil
		},
	}
	command.Flags().StringVar(&scanPath, "path", "", "directory to scan")
	return command
}
