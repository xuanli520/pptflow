package cmd

import (
	"github.com/spf13/cobra"
	dockermgr "github.com/xuanli520/p2r_tui/internal/docker"
	"github.com/xuanli520/p2r_tui/internal/executor"
	"github.com/xuanli520/p2r_tui/internal/maintenance"
)

func newAdminDockerGCCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "docker-gc",
		Short: "Run conservative Docker garbage collection",
	}
	command.AddCommand(newAdminDockerGCStatusCommand())
	command.AddCommand(newAdminDockerGCRunCommand())
	return command
}

func newAdminDockerGCStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show Docker GC state",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig("")
			if err != nil {
				return err
			}
			state, summary := maintenance.Status(cfg)
			return printJSON(cmd, map[string]any{"state": state, "last_summary": summary})
		},
	}
}

func newAdminDockerGCRunCommand() *cobra.Command {
	var dryRun bool
	var yes bool
	var allowGlobal bool
	command := &cobra.Command{
		Use:   "run",
		Short: "Run Docker GC",
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRun && yes {
				return errFlag("--dry-run and --yes are mutually exclusive")
			}
			cfg, err := loadConfig("")
			if err != nil {
				return err
			}
			summary, err := dockermgr.RunGC(cmd.Context(), dockermgr.GCRunRequest{
				ScanPath:    cfg.ScanPath,
				Config:      cfg.Docker,
				Exec:        executor.New(),
				DryRun:      dryRun,
				Yes:         yes,
				AllowGlobal: allowGlobal,
				Trigger:     "admin",
			})
			_ = printJSON(cmd, summary)
			return err
		},
	}
	command.Flags().BoolVar(&dryRun, "dry-run", false, "plan deletions without deleting")
	command.Flags().BoolVar(&yes, "yes", false, "confirm Docker deletions")
	command.Flags().BoolVar(&allowGlobal, "allow-global", false, "allow non p2r-only global cleanup when configured")
	return command
}

type errFlag string

func (e errFlag) Error() string {
	return string(e)
}
