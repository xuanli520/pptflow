package cmd

import (
	"context"
	"time"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/spf13/cobra"
)

type executionControlPreview struct {
	Action         store.ControlAction   `json:"action"`
	RunID          string                `json:"run_id"`
	StageAttemptID string                `json:"stage_attempt_id,omitempty"`
	OperationKey   string                `json:"operation_key"`
	Expected       app.ControlCheckpoint `json:"expected_checkpoint"`
	GracePeriod    time.Duration         `json:"grace_period"`
	WillMutate     bool                  `json:"will_mutate"`
}

func newRunPauseCommand(config *lifecycleCLIConfig) *cobra.Command {
	return newRunControlCommand(config, "pause", "Request a durable pause for a running workflow run", store.ControlActionPause)
}

func newRunCancelStageCommand(config *lifecycleCLIConfig) *cobra.Command {
	return newRunControlCommand(config, "cancel-stage", "Request a durable cancellation for one stage attempt", store.ControlActionCancelStage)
}

func newRunTerminateCommand(config *lifecycleCLIConfig) *cobra.Command {
	return newRunControlCommand(config, "terminate", "Request durable termination for a workflow run", store.ControlActionTerminate)
}

func newRunControlCommand(config *lifecycleCLIConfig, use, short string, action store.ControlAction) *cobra.Command {
	var runID, stageAttemptID, operationKey, reason string
	var dryRun bool
	command := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			actor, reason, err := lifecycleActorAndReason(config, reason)
			if err != nil {
				return err
			}
			if _, err := requiredText("run", runID); err != nil {
				return err
			}
			if _, err := requiredText("operation-key", operationKey); err != nil {
				return err
			}
			if action == store.ControlActionCancelStage {
				if _, err := requiredText("stage-attempt", stageAttemptID); err != nil {
					return err
				}
			}
			return executeLifecycleCommand(cmd, config, func(ctx context.Context, services *app.LifecycleServices) (any, error) {
				checkpoint, err := services.Control.CurrentCheckpoint(ctx, runID)
				if err != nil {
					return nil, err
				}
				grace, err := services.Control.FrozenGracePeriod(ctx, runID)
				if err != nil {
					return nil, err
				}
				preview := executionControlPreview{
					Action: action, RunID: runID, StageAttemptID: stageAttemptID, OperationKey: operationKey,
					Expected: checkpoint, GracePeriod: grace, WillMutate: !dryRun,
				}
				if dryRun {
					return preview, nil
				}
				return services.Control.Request(ctx, app.RequestExecutionControlRequest{
					OperationKey: operationKey, Action: action, RunID: runID, StageAttemptID: stageAttemptID,
					Expected: checkpoint, Actor: actor, Reason: reason,
				})
			})
		},
	}
	command.Flags().StringVar(&runID, "run", "", "Run UUIDv7")
	if action == store.ControlActionCancelStage {
		command.Flags().StringVar(&stageAttemptID, "stage-attempt", "", "Target non-terminal stage attempt UUIDv7")
	}
	command.Flags().StringVar(&operationKey, "operation-key", "", "Client-generated idempotency key")
	command.Flags().StringVar(&reason, "reason", "", "Audit reason")
	command.Flags().BoolVar(&dryRun, "dry-run", false, "Show the targeted checkpoint without creating a control operation")
	return command
}

func newRunControlShowCommand(config *lifecycleCLIConfig) *cobra.Command {
	var operationID string
	command := &cobra.Command{
		Use:   "control-show",
		Short: "Show one durable execution-control operation",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := requiredText("operation", operationID); err != nil {
				return err
			}
			return executeLifecycleCommand(cmd, config, func(ctx context.Context, services *app.LifecycleServices) (any, error) {
				return services.Control.Get(ctx, operationID)
			})
		},
	}
	command.Flags().StringVar(&operationID, "operation", "", "Control operation UUIDv7")
	return command
}

func newBudgetCommand(config *lifecycleCLIConfig) *cobra.Command {
	command := &cobra.Command{Use: "budget", Short: "Inspect and grant explicit task budgets", Args: cobra.NoArgs, RunE: showCommandGroupHelp}
	command.AddCommand(newBudgetShowCommand(config), newBudgetGrantCommand(config))
	return command
}

func newBudgetShowCommand(config *lifecycleCLIConfig) *cobra.Command {
	var runID string
	command := &cobra.Command{
		Use:   "show",
		Short: "Show configured task quota accounts for a run",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := requiredText("run", runID); err != nil {
				return err
			}
			return executeLifecycleCommand(cmd, config, func(ctx context.Context, services *app.LifecycleServices) (any, error) {
				return services.Budgets.ListRunBudgets(ctx, runID)
			})
		},
	}
	command.Flags().StringVar(&runID, "run", "", "Run UUIDv7")
	return command
}

type budgetGrantPreview struct {
	TaskID          string `json:"task_id"`
	RunID           string `json:"run_id"`
	Dimension       string `json:"dimension"`
	DeltaUnits      int64  `json:"delta_units"`
	ExpectedVersion int64  `json:"expected_version"`
	IdempotencyKey  string `json:"idempotency_key"`
	WillMutate      bool   `json:"will_mutate"`
}

func newBudgetGrantCommand(config *lifecycleCLIConfig) *cobra.Command {
	var runID, dimension, idempotencyKey, reason string
	var delta, expectedVersion int64
	var dryRun bool
	command := &cobra.Command{
		Use:   "grant",
		Short: "Grant additional task budget as the task owner",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			actor, reason, err := lifecycleActorAndReason(config, reason)
			if err != nil {
				return err
			}
			if _, err := requiredText("run", runID); err != nil {
				return err
			}
			if _, err := requiredText("dimension", dimension); err != nil {
				return err
			}
			if _, err := requiredText("idempotency-key", idempotencyKey); err != nil {
				return err
			}
			if err := requiredPositive("delta", delta); err != nil {
				return err
			}
			if err := requiredPositive("expected-version", expectedVersion); err != nil {
				return err
			}
			return executeLifecycleCommand(cmd, config, func(ctx context.Context, services *app.LifecycleServices) (any, error) {
				if dryRun {
					taskID, err := services.Budgets.QuotaTaskIDForRun(ctx, runID)
					if err != nil {
						return nil, err
					}
					preview := budgetGrantPreview{
						TaskID: taskID, RunID: runID, Dimension: dimension, DeltaUnits: delta,
						ExpectedVersion: expectedVersion, IdempotencyKey: idempotencyKey,
					}
					return preview, nil
				}
				return services.Budgets.GrantRunBudget(ctx, app.GrantRunBudgetRequest{
					RunID: runID, Dimension: dimension, DeltaUnits: delta, ExpectedVersion: expectedVersion,
					IdempotencyKey: idempotencyKey, Actor: actor, Reason: reason,
				})
			})
		},
	}
	command.Flags().StringVar(&runID, "run", "", "Run UUIDv7")
	command.Flags().StringVar(&dimension, "dimension", "", "Quota dimension")
	command.Flags().Int64Var(&delta, "delta", 0, "Positive quota units to grant")
	command.Flags().Int64Var(&expectedVersion, "expected-version", 0, "Current quota account version")
	command.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Client-generated idempotency key")
	command.Flags().StringVar(&reason, "reason", "", "Audit reason")
	command.Flags().BoolVar(&dryRun, "dry-run", false, "Show the resolved task budget grant without mutating quota")
	return command
}
