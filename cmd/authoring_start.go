package cmd

import (
	"context"
	"fmt"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/spf13/cobra"
)

// newAuthoringStartCommand exposes the source-session half of Standard task
// creation. Repository URL and full Git commit are caller-selected immutable
// coordinates; archive format, profile, catalog, provider, model, prompt,
// secret, and execution settings remain deployment-owned.
func newAuthoringStartCommand(config *lifecycleCLIConfig) *cobra.Command {
	var repositoryURL, commitSHA, slug, title, metadataJSON, idempotencyKey, reason string
	command := &cobra.Command{
		Use:   "start",
		Short: "Capture an immutable Git source and start Standard authoring",
		Long: `Capture one HTTPS or SSH Git repository at an exact full commit as a managed immutable source object,
create a revision-free draft Task and AuthoringSession, then queue the closed
Standard authoring Run. The generated TaskRevision and later CodeEdge workflow
begin only after the authoring materialization handoff.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			actor, reason, err := lifecycleActorAndReason(config, reason)
			if err != nil {
				return err
			}
			if _, err := requiredText("slug", slug); err != nil {
				return err
			}
			if _, err := requiredText("title", title); err != nil {
				return err
			}
			if _, err := requiredText("repository-url", repositoryURL); err != nil {
				return err
			}
			if _, err := requiredText("commit-sha", commitSHA); err != nil {
				return err
			}
			idempotencyKey, err = requiredLifecycleIdempotencyKey(idempotencyKey)
			if err != nil {
				return err
			}
			return executeLifecycleCommand(cmd, config, func(ctx context.Context, services *app.LifecycleServices) (any, error) {
				if services == nil || services.AuthoringLaunches == nil {
					return nil, fmt.Errorf("Standard authoring launch service is not configured")
				}
				return services.AuthoringLaunches.Start(ctx, app.StandardAuthoringLaunchCommand{
					LifecycleMutationCommandBase: app.LifecycleMutationCommandBase{IdempotencyKey: idempotencyKey, Actor: actor, Reason: reason},
					RepositoryURL:                repositoryURL,
					CommitSHA:                    commitSHA,
					Slug:                         slug,
					Title:                        title,
					MetadataJSON:                 metadataJSON,
				})
			})
		},
	}
	command.Flags().StringVar(&repositoryURL, "repository-url", "", "HTTPS or SSH Git repository URL")
	command.Flags().StringVar(&commitSHA, "commit-sha", "", "Full immutable Git commit SHA")
	command.Flags().StringVar(&slug, "slug", "", "Human-readable task slug")
	command.Flags().StringVar(&title, "title", "", "Task title")
	command.Flags().StringVar(&metadataJSON, "metadata-json", "{}", "Draft task metadata JSON")
	command.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Client-generated UUIDv7 idempotency key")
	command.Flags().StringVar(&reason, "reason", "", "Audit reason")
	return command
}
