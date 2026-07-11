package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/purplevoid/harbor-factory/internal/harbor/repoprep"
	"github.com/spf13/cobra"
)

func newRepoPrepareCommand() *cobra.Command {
	var repoURL, commit, workspace string
	cmd := &cobra.Command{
		Use:   "repo-prepare",
		Short: "Clone a repository and pin it to a concrete commit",
		RunE: func(cmd *cobra.Command, args []string) error {
			prepared, err := repoprep.Prepare(cmd.Context(), repoprep.Options{
				RepoURL:   repoURL,
				Commit:    commit,
				Workspace: workspace,
			})
			if err != nil {
				return err
			}
			data, err := json.MarshalIndent(prepared, "", "  ")
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(data))
			return nil
		},
	}
	cmd.Flags().StringVar(&repoURL, "repo", "", "GitHub repository URL")
	cmd.Flags().StringVar(&commit, "commit", "", "Concrete commit SHA")
	cmd.Flags().StringVar(&workspace, "workspace", ".harbor-factory/workspace", "Workspace directory")
	_ = cmd.MarkFlagRequired("repo")
	_ = cmd.MarkFlagRequired("commit")
	return cmd
}
