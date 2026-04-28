package cmd

import (
	"github.com/spf13/cobra"

	tuiapp "github.com/xuanli520/p2r_tui/internal/tui"
)

func newTUICommand() *cobra.Command {
	var scanPath string
	command := &cobra.Command{
		Use:   "tui",
		Short: "Open the p2r terminal UI",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(scanPath)
			if err != nil {
				return err
			}
			store, err := openStore(cfg)
			if err != nil {
				return err
			}
			defer store.Close()
			return tuiapp.Start(store, cfg)
		},
	}
	command.Flags().StringVar(&scanPath, "path", "", "project scan path")
	return command
}
