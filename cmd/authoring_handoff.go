package cmd

import (
	"context"
	"fmt"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/spf13/cobra"
)

// authoringHandoffRedriveOutput is intentionally narrower than DurableJob.
// The CLI confirms only the durable delivery identity and state; its raw
// payload remains an application/store contract rather than terminal output.
type authoringHandoffRedriveOutput struct {
	JobID          string `json:"job_id"`
	CommandType    string `json:"command_type"`
	State          string `json:"state"`
	AuthoringRunID string `json:"authoring_run_id"`
}

func newAuthoringHandoffCommand(config *lifecycleCLIConfig) *cobra.Command {
	command := &cobra.Command{
		Use:   "handoff",
		Short: "Inspect and explicitly redrive Standard authoring handoffs",
		Args:  cobra.NoArgs,
		RunE:  showCommandGroupHelp,
	}
	command.AddCommand(newAuthoringHandoffRedriveCommand(config))
	return command
}

// newAuthoringHandoffRedriveCommand republishes only an existing in_doubt
// delivery. It never rebuilds a Phase-1 definition from flags or accesses a
// mutable authoring workspace; the lifecycle service reuses the original
// immutable payload and preallocated child Run identity.
func newAuthoringHandoffRedriveCommand(config *lifecycleCLIConfig) *cobra.Command {
	var authoringRunID, idempotencyKey, reason string
	command := &cobra.Command{
		Use:   "redrive",
		Short: "Explicitly redrive an in_doubt Standard-to-Phase-1 handoff",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			actor, reason, err := lifecycleActorAndReason(config, reason)
			if err != nil {
				return err
			}
			authoringRunID, err = requiredText("authoring-run", authoringRunID)
			if err != nil {
				return err
			}
			if err := store.ValidateUUIDv7(authoringRunID); err != nil {
				return fmt.Errorf("authoring-run must be a UUIDv7: %w", err)
			}
			idempotencyKey, err = requiredLifecycleIdempotencyKey(idempotencyKey)
			if err != nil {
				return err
			}
			return executeLifecycleCommand(cmd, config, func(ctx context.Context, services *app.LifecycleServices) (any, error) {
				if services == nil || services.StandardAuthoringHandoffs == nil {
					return nil, fmt.Errorf("Standard authoring handoff service is not configured")
				}
				job, err := services.StandardAuthoringHandoffs.Redrive(ctx, app.RedriveStandardAuthoringHandoffCommand{
					AuthoringRunID: authoringRunID, IdempotencyKey: idempotencyKey, Actor: actor, Reason: reason,
				})
				if err != nil {
					return nil, err
				}
				return authoringHandoffRedriveOutput{JobID: job.ID, CommandType: job.CommandType, State: string(job.State), AuthoringRunID: job.RunID}, nil
			})
		},
	}
	command.Flags().StringVar(&authoringRunID, "authoring-run", "", "Standard authoring Run UUIDv7")
	command.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Client-generated UUIDv7 redrive idempotency key")
	command.Flags().StringVar(&reason, "reason", "", "Audit reason")
	return command
}
