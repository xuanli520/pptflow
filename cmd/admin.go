package cmd

import "github.com/spf13/cobra"

func newAdminCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "admin",
		Short: "Administrative maintenance commands",
	}
	command.AddCommand(newAdminDockerMirrorCommand())
	command.AddCommand(newAdminDockerGCCommand())
	return command
}
