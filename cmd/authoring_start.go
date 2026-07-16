package cmd

import (
	"context"
	"fmt"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/spf13/cobra"
)

// newAuthoringStartCommand exposes the source-session half of Standard task
// creation. Repository URL, full Git commit, and digest-pinned base image are
// caller-selected immutable task-contract inputs; archive format, profile,
// catalog, provider, model, prompt, secret, and execution settings remain
// deployment-owned.
func newAuthoringStartCommand(config *lifecycleCLIConfig) *cobra.Command {
	var repositoryURL, commitSHA, baseImage, slug, title, metadataJSON, idempotencyKey, reason string
	command := &cobra.Command{
		Use:   "start",
		Short: "Capture an immutable Git source and start Standard authoring",
		Long: `Capture one HTTPS or SSH Git repository at an exact full commit as a managed immutable source object,
create a revision-free draft Task and AuthoringSession, then queue the closed
Standard authoring Run. The generated TaskRevision and later CodeEdge workflow
begin only after the authoring materialization handoff. The caller must provide
one fully-qualified base image pinned by a SHA-256 digest; that image is frozen
into the AuthoringSession task contract.`,
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
			baseImage, err = requiredText("base-image", baseImage)
			if err != nil {
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
				receipt, err := services.AuthoringLaunches.Start(ctx, app.StandardAuthoringLaunchCommand{
					LifecycleMutationCommandBase: app.LifecycleMutationCommandBase{IdempotencyKey: idempotencyKey, Actor: actor, Reason: reason},
					RepositoryURL:                repositoryURL,
					CommitSHA:                    commitSHA,
					BaseImage:                    baseImage,
					Slug:                         slug,
					Title:                        title,
					MetadataJSON:                 metadataJSON,
				})
				if err != nil {
					return nil, err
				}
				if services.RunActivations != nil && services.RunActivations.Available() {
					if err := services.RunActivations.Drain(ctx); err != nil {
						return nil, fmt.Errorf("activate queued Standard Run: %w", err)
					}
				}
				return receipt, nil
			})
		},
	}
	command.Flags().StringVar(&repositoryURL, "repository-url", "", "HTTPS or SSH Git repository URL")
	command.Flags().StringVar(&commitSHA, "commit-sha", "", "Full immutable Git commit SHA")
	command.Flags().StringVar(&baseImage, "base-image", "", "Immutable OCI base image pinned by a SHA-256 digest")
	command.Flags().StringVar(&slug, "slug", "", "Human-readable task slug")
	command.Flags().StringVar(&title, "title", "", "Task title")
	command.Flags().StringVar(&metadataJSON, "metadata-json", "{}", "Draft task metadata JSON")
	command.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Client-generated UUIDv7 idempotency key")
	command.Flags().StringVar(&reason, "reason", "", "Audit reason")
	return command
}
