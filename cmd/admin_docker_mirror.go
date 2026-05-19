package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
	dockermgr "github.com/xuanli520/p2r_tui/internal/docker"
)

func newAdminDockerMirrorCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "docker-mirror",
		Short: "Manage Docker daemon registry mirrors",
	}
	command.AddCommand(newAdminDockerMirrorStatusCommand())
	command.AddCommand(newAdminDockerMirrorApplyCommand())
	command.AddCommand(newAdminDockerMirrorRestoreCommand())
	return command
}

func newAdminDockerMirrorStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show Docker daemon mirror drift",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig("")
			if err != nil {
				return err
			}
			return printJSON(cmd, dockermgr.DaemonMirrorStatus(cfg))
		},
	}
}

func newAdminDockerMirrorApplyCommand() *cobra.Command {
	var yes bool
	command := &cobra.Command{
		Use:   "apply",
		Short: "Apply configured Docker daemon mirrors",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig("")
			if err != nil {
				return err
			}
			summary, err := dockermgr.ApplyDaemonMirrors(cfg, yes)
			_ = printJSON(cmd, summary)
			if err == nil && summary.RestartRequired {
				fmt.Fprintln(cmd.OutOrStdout(), "restart required: sudo systemctl restart docker")
			}
			return err
		},
	}
	command.Flags().BoolVar(&yes, "yes", false, "confirm daemon.json write")
	return command
}

func newAdminDockerMirrorRestoreCommand() *cobra.Command {
	var backup string
	var yes bool
	command := &cobra.Command{
		Use:   "restore",
		Short: "Restore Docker daemon config from a backup",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig("")
			if err != nil {
				return err
			}
			summary, err := dockermgr.RestoreDaemonMirrors(cfg, backup, yes)
			_ = printJSON(cmd, summary)
			if err == nil && summary.RestartRequired {
				fmt.Fprintln(cmd.OutOrStdout(), "restart required: sudo systemctl restart docker")
			}
			return err
		},
	}
	command.Flags().StringVar(&backup, "backup", "", "backup file to restore")
	command.Flags().BoolVar(&yes, "yes", false, "confirm daemon.json restore")
	return command
}

func printJSON(cmd *cobra.Command, value any) error {
	content, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), string(content))
	return err
}
