package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os/user"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
	"github.com/spf13/cobra"
)

// lifecycleCLIConfig is shared by the V2 command groups. It intentionally
// contains only local control-plane settings: remote provider configuration
// and package upload targets are not part of this command surface.
type lifecycleCLIConfig struct {
	root                string
	newLifecycleService lifecycleServicesFactory
}

// lifecycleServicesFactory is the one command-layer composition boundary for
// lifecycle services. CLI commands, the Task Hub, and controlled child workers
// must open the same deployment-bound service graph rather than each silently
// constructing a different resolver set.
type lifecycleServicesFactory func(root string, dataStore *store.Store) (*app.LifecycleServices, error)

func (config *lifecycleCLIConfig) openLifecycleServices(dataStore *store.Store) (*app.LifecycleServices, error) {
	if config == nil {
		return nil, fmt.Errorf("lifecycle configuration is required")
	}
	factory := config.newLifecycleService
	if factory == nil {
		factory = app.NewLifecycleServices
	}
	return factory(config.root, dataStore)
}

func defaultLifecycleActor() string {
	if current, err := user.Current(); err == nil && strings.TrimSpace(current.Username) != "" {
		return strings.TrimSpace(current.Username)
	}
	return ""
}

func executeLifecycleCommand(cmd *cobra.Command, config *lifecycleCLIConfig, action func(context.Context, *app.LifecycleServices) (any, error)) error {
	return executeLifecycleCommandWithStore(cmd, config, store.Open, action)
}

// executeLifecycleReadOnlyCommand is reserved for previews whose public
// contract guarantees that no durable lifecycle state is created or updated.
func executeLifecycleReadOnlyCommand(cmd *cobra.Command, config *lifecycleCLIConfig, action func(context.Context, *app.LifecycleServices) (any, error)) error {
	return executeLifecycleCommandWithStore(cmd, config, store.OpenReadOnly, action)
}

func executeLifecycleCommandWithStore(cmd *cobra.Command, config *lifecycleCLIConfig, openStore func(string) (*store.Store, error), action func(context.Context, *app.LifecycleServices) (any, error)) error {
	if config == nil || strings.TrimSpace(config.root) == "" {
		return fmt.Errorf("managed root is required")
	}
	if openStore == nil {
		return fmt.Errorf("lifecycle store opener is required")
	}
	database, err := openStore(config.root)
	if err != nil {
		return fmt.Errorf("open lifecycle control plane: %w", err)
	}
	defer database.Close()
	services, err := config.openLifecycleServices(database)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	result, err := action(ctx, services)
	if err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("encode lifecycle result: %w", err)
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), string(encoded))
	return err
}

func lifecycleActorAndReason(config *lifecycleCLIConfig, reason string) (string, string, error) {
	if config == nil {
		return "", "", fmt.Errorf("local OS actor is required")
	}
	actor := defaultLifecycleActor()
	if strings.TrimSpace(actor) == "" {
		return "", "", fmt.Errorf("local OS actor is required")
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "", "", fmt.Errorf("reason is required for lifecycle mutations")
	}
	return actor, reason, nil
}

func requiredText(name, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
}

// requiredLifecycleIdempotencyKey keeps every CLI mutation on the V12
// lifecycle-operation ledger. Callers retain the supplied UUIDv7 when a
// response is lost, so the command can return the original immutable receipt.
func requiredLifecycleIdempotencyKey(value string) (string, error) {
	key, err := requiredText("idempotency-key", value)
	if err != nil {
		return "", err
	}
	if err := store.ValidateUUIDv7(key); err != nil {
		return "", fmt.Errorf("idempotency-key must be a UUIDv7: %w", err)
	}
	return key, nil
}

func lifecycleMutationBase(idempotencyKey, actor, reason string, expected app.LifecycleMutationCheckpoint) app.LifecycleMutationCommandBase {
	return app.LifecycleMutationCommandBase{
		IdempotencyKey: idempotencyKey,
		Actor:          actor,
		Reason:         reason,
		Expected:       expected,
	}
}

func replayCompletedLifecycleMutation(ctx context.Context, services *app.LifecycleServices, action app.LifecycleMutationAction, idempotencyKey string) (*app.LifecycleMutationReceipt, error) {
	if services == nil || services.Mutations == nil {
		return nil, fmt.Errorf("lifecycle mutation services are not configured")
	}
	receipt, replayed, err := services.Mutations.ReplayCompleted(ctx, action, idempotencyKey)
	if err != nil {
		return nil, err
	}
	if !replayed {
		return nil, nil
	}
	return &receipt, nil
}

// lifecycleMutationCheckpointForKey returns the original full checkpoint for
// a retry before any mutable entity is reread. This is necessary because a
// successful mutation normally advances the very version the CLI initially
// confirmed. New commands receive no stored checkpoint and must capture one
// immediately before they call the typed application service.
func lifecycleMutationCheckpointForKey(ctx context.Context, services *app.LifecycleServices, action app.LifecycleMutationAction, idempotencyKey string) (app.LifecycleMutationCheckpoint, bool, error) {
	if services == nil || services.Store() == nil {
		return app.LifecycleMutationCheckpoint{}, false, fmt.Errorf("lifecycle mutation services are not configured")
	}
	operation, err := services.Store().GetLifecycleOperationByIdempotencyKey(ctx, idempotencyKey)
	if err != nil {
		return app.LifecycleMutationCheckpoint{}, false, err
	}
	if operation == nil {
		return app.LifecycleMutationCheckpoint{}, false, nil
	}
	if operation.Action != string(action) {
		return app.LifecycleMutationCheckpoint{}, false, fmt.Errorf("%w: lifecycle operation key %s", store.ErrIdempotencyConflict, idempotencyKey)
	}
	if operation.State == store.LifecycleOperationPrepared && strings.TrimSpace(operation.ExpectedTaskID) == "" {
		return app.LifecycleMutationCheckpoint{}, false, fmt.Errorf("%w: prepared legacy lifecycle operation %s has no persisted expected checkpoint identities", store.ErrIdempotencyConflict, operation.ID)
	}
	checkpoint := app.LifecycleMutationCheckpoint{
		TaskID:                           operation.ExpectedTaskID,
		TaskVersion:                      operation.ExpectedTaskVersion,
		RevisionID:                       operation.ExpectedRevisionID,
		RevisionStateVersion:             operation.ExpectedRevisionStateVersion,
		RevisionDigest:                   operation.ExpectedRevisionDigest,
		RunID:                            operation.ExpectedRunID,
		RunVersion:                       operation.ExpectedRunVersion,
		RunExecutionEpoch:                operation.ExpectedRunExecutionEpoch,
		RunDefinitionHash:                operation.ExpectedRunDefinitionHash,
		CodeEdgeComplianceRecordID:       operation.ExpectedCodeEdgeComplianceRecordID,
		CodeEdgeAuthorizationFingerprint: operation.ExpectedCodeEdgeAuthorizationFingerprint,
		ReleaseID:                        operation.ExpectedReleaseID,
		ReleaseRecordVersion:             operation.ExpectedReleaseRecordVersion,
		ReviewRequestID:                  operation.ExpectedReviewRequestID,
		ReviewRevisionID:                 operation.ExpectedReviewRevisionID,
		ReviewState:                      operation.ExpectedReviewState,
		ReviewEvidenceDigest:             operation.ExpectedReviewEvidenceDigest,
	}
	return checkpoint, true, nil
}

func requireLifecycleCheckpointVersion(name string, supplied, captured int64) error {
	if supplied != captured {
		return fmt.Errorf("%w: %s checkpoint version %d does not match current version %d", store.ErrOptimisticLock, name, supplied, captured)
	}
	return nil
}

func requireLifecycleCheckpointDigest(supplied, captured string) error {
	if strings.TrimSpace(supplied) != captured {
		return fmt.Errorf("%w: expected revision digest does not match the captured revision", store.ErrOptimisticLock)
	}
	return nil
}

func requireLifecycleCheckpointIdentity(name, supplied, captured string) error {
	if strings.TrimSpace(supplied) != captured {
		return fmt.Errorf("%w: %s does not match the captured lifecycle checkpoint", store.ErrIdempotencyConflict, name)
	}
	return nil
}

func requiredPositive(name string, value int64) error {
	if value <= 0 {
		return fmt.Errorf("%s must be positive", name)
	}
	return nil
}

func newTaskCommand(config *lifecycleCLIConfig) *cobra.Command {
	command := &cobra.Command{Use: "task", Short: "Manage stable Harbor tasks", Args: cobra.NoArgs, RunE: showCommandGroupHelp}
	command.AddCommand(
		newTaskListCommand(config),
		newTaskShowCommand(config),
		newTaskCreateCommand(config),
		newTaskImportCommand(config),
		newTaskForkCommand(config),
		newTaskArchiveCommand(config),
		newTaskDeleteCommand(config),
		newTaskRestoreCommand(config),
		newTaskContinueCommand(config),
		newTaskPurgeCommand(config),
	)
	return command
}

func newTaskListCommand(config *lifecycleCLIConfig) *cobra.Command {
	var includeDeleted bool
	return &cobra.Command{
		Use:   "list",
		Short: "List tasks by stable identity",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return executeLifecycleCommand(cmd, config, func(ctx context.Context, services *app.LifecycleServices) (any, error) {
				return services.Tasks.List(ctx, includeDeleted)
			})
		},
		Args: cobra.NoArgs,
	}
}

func newTaskShowCommand(config *lifecycleCLIConfig) *cobra.Command {
	var taskID string
	command := &cobra.Command{
		Use:   "show",
		Short: "Show one task",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			id, err := requiredText("task", taskID)
			if err != nil {
				return err
			}
			return executeLifecycleCommand(cmd, config, func(ctx context.Context, services *app.LifecycleServices) (any, error) {
				return services.Tasks.Get(ctx, id)
			})
		},
	}
	command.Flags().StringVar(&taskID, "task", "", "Task UUIDv7")
	return command
}

func newTaskCreateCommand(config *lifecycleCLIConfig) *cobra.Command {
	var slug, title, metadata, repo, commit, idempotencyKey, reason string
	command := &cobra.Command{
		Use:   "create",
		Short: "Create a draft task",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			actor, reason, err := lifecycleActorAndReason(config, reason)
			if err != nil {
				return err
			}
			if _, err := requiredText("slug", slug); err != nil {
				return err
			}
			idempotencyKey, err := requiredLifecycleIdempotencyKey(idempotencyKey)
			if err != nil {
				return err
			}
			return executeLifecycleCommand(cmd, config, func(ctx context.Context, services *app.LifecycleServices) (any, error) {
				return services.Mutations.CreateDraft(ctx, app.CreateDraftLifecycleCommand{
					LifecycleMutationCommandBase: lifecycleMutationBase(idempotencyKey, actor, reason, app.LifecycleMutationCheckpoint{}),
					Slug:                         slug,
					Title:                        title,
					MetadataJSON:                 metadata,
					SourceRepo:                   repo,
					SourceCommit:                 commit,
				})
			})
		},
	}
	command.Flags().StringVar(&slug, "slug", "", "Human-readable task slug")
	command.Flags().StringVar(&title, "title", "", "Task title")
	command.Flags().StringVar(&metadata, "metadata-json", "", "Task metadata JSON")
	command.Flags().StringVar(&repo, "repo", "", "Source repository")
	command.Flags().StringVar(&commit, "commit", "", "Source commit")
	command.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Client-generated UUIDv7 lifecycle idempotency key")
	command.Flags().StringVar(&reason, "reason", "", "Audit reason")
	return command
}

func newTaskImportCommand(config *lifecycleCLIConfig) *cobra.Command {
	var source, slug, title, metadata, repo, commit, proposalDigest, summary, idempotencyKey, reason string
	command := &cobra.Command{
		Use:   "import",
		Short: "Import a strict Harbor task snapshot as an immutable revision",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			actor, reason, err := lifecycleActorAndReason(config, reason)
			if err != nil {
				return err
			}
			if _, err := requiredText("source", source); err != nil {
				return err
			}
			if _, err := requiredText("slug", slug); err != nil {
				return err
			}
			idempotencyKey, err := requiredLifecycleIdempotencyKey(idempotencyKey)
			if err != nil {
				return err
			}
			return executeLifecycleCommand(cmd, config, func(ctx context.Context, services *app.LifecycleServices) (any, error) {
				return services.Mutations.Import(ctx, app.ImportLifecycleCommand{
					LifecycleMutationCommandBase: lifecycleMutationBase(idempotencyKey, actor, reason, app.LifecycleMutationCheckpoint{}),
					Slug:                         slug,
					Title:                        title,
					MetadataJSON:                 metadata,
					SourceRepo:                   repo,
					SourceCommit:                 commit,
					SourcePath:                   source,
					ProposalDigest:               proposalDigest,
					ChangeSummary:                summary,
				})
			})
		},
	}
	command.Flags().StringVar(&source, "source", "", "Source task directory")
	command.Flags().StringVar(&slug, "slug", "", "Human-readable task slug")
	command.Flags().StringVar(&title, "title", "", "Task title")
	command.Flags().StringVar(&metadata, "metadata-json", "", "Task metadata JSON")
	command.Flags().StringVar(&repo, "repo", "", "Source repository")
	command.Flags().StringVar(&commit, "commit", "", "Source commit")
	command.Flags().StringVar(&proposalDigest, "proposal-digest", "", "Optional proposal digest")
	command.Flags().StringVar(&summary, "change-summary", "", "Revision change summary")
	command.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Client-generated UUIDv7 lifecycle idempotency key")
	command.Flags().StringVar(&reason, "reason", "", "Audit reason")
	return command
}

func newTaskForkCommand(config *lifecycleCLIConfig) *cobra.Command {
	var sourceTask, sourceRevision, slug, title, metadata, idempotencyKey, reason string
	command := &cobra.Command{
		Use:   "fork",
		Short: "Fork an immutable task revision into a new task",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			actor, reason, err := lifecycleActorAndReason(config, reason)
			if err != nil {
				return err
			}
			if _, err := requiredText("source-task", sourceTask); err != nil {
				return err
			}
			if _, err := requiredText("source-revision", sourceRevision); err != nil {
				return err
			}
			if _, err := requiredText("slug", slug); err != nil {
				return err
			}
			idempotencyKey, err := requiredLifecycleIdempotencyKey(idempotencyKey)
			if err != nil {
				return err
			}
			return executeLifecycleCommand(cmd, config, func(ctx context.Context, services *app.LifecycleServices) (any, error) {
				if receipt, err := replayCompletedLifecycleMutation(ctx, services, app.LifecycleMutationFork, idempotencyKey); err != nil {
					return nil, err
				} else if receipt != nil {
					return *receipt, nil
				}
				checkpoint, replayed, err := lifecycleMutationCheckpointForKey(ctx, services, app.LifecycleMutationFork, idempotencyKey)
				if err != nil {
					return nil, err
				}
				if !replayed {
					checkpoint, err = services.Mutations.CaptureCheckpoint(ctx, sourceTask, sourceRevision, "", "")
					if err != nil {
						return nil, err
					}
				}
				return services.Mutations.Fork(ctx, app.ForkLifecycleCommand{
					LifecycleMutationCommandBase: lifecycleMutationBase(idempotencyKey, actor, reason, checkpoint),
					Slug:                         slug,
					Title:                        title,
					MetadataJSON:                 metadata,
				})
			})
		},
	}
	command.Flags().StringVar(&sourceTask, "source-task", "", "Source task UUIDv7")
	command.Flags().StringVar(&sourceRevision, "source-revision", "", "Source immutable revision UUIDv7")
	command.Flags().StringVar(&slug, "slug", "", "Fork task slug")
	command.Flags().StringVar(&title, "title", "", "Fork task title")
	command.Flags().StringVar(&metadata, "metadata-json", "", "Task metadata JSON")
	command.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Client-generated UUIDv7 lifecycle idempotency key")
	command.Flags().StringVar(&reason, "reason", "", "Audit reason")
	return command
}

func newTaskArchiveCommand(config *lifecycleCLIConfig) *cobra.Command {
	return newTaskTransitionCommand(config, "archive", "Archive a task", store.TaskLifecycleArchived)
}

func newTaskDeleteCommand(config *lifecycleCLIConfig) *cobra.Command {
	return newTaskTransitionCommand(config, "delete", "Soft-delete a task", store.TaskLifecycleDeleted)
}

func newTaskTransitionCommand(config *lifecycleCLIConfig, use, short string, state store.TaskLifecycleState) *cobra.Command {
	var taskID, idempotencyKey, reason string
	var expectedVersion int64
	command := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			actor, reason, err := lifecycleActorAndReason(config, reason)
			if err != nil {
				return err
			}
			if _, err := requiredText("task", taskID); err != nil {
				return err
			}
			if err := requiredPositive("expected-version", expectedVersion); err != nil {
				return err
			}
			idempotencyKey, err := requiredLifecycleIdempotencyKey(idempotencyKey)
			if err != nil {
				return err
			}
			return executeLifecycleCommand(cmd, config, func(ctx context.Context, services *app.LifecycleServices) (any, error) {
				action := app.LifecycleMutationArchive
				if state == store.TaskLifecycleDeleted {
					action = app.LifecycleMutationSoftDelete
				}
				if receipt, err := replayCompletedLifecycleMutation(ctx, services, action, idempotencyKey); err != nil {
					return nil, err
				} else if receipt != nil {
					return *receipt, nil
				}
				checkpoint, replayed, err := lifecycleMutationCheckpointForKey(ctx, services, action, idempotencyKey)
				if err != nil {
					return nil, err
				}
				if !replayed {
					checkpoint, err = services.Mutations.CaptureCheckpoint(ctx, taskID, "", "", "")
					if err != nil {
						return nil, err
					}
				}
				if err := requireLifecycleCheckpointIdentity("task", taskID, checkpoint.TaskID); err != nil {
					return nil, err
				}
				if err := requireLifecycleCheckpointVersion("task", expectedVersion, checkpoint.TaskVersion); err != nil {
					return nil, err
				}
				base := lifecycleMutationBase(idempotencyKey, actor, reason, checkpoint)
				if state == store.TaskLifecycleDeleted {
					return services.Mutations.SoftDelete(ctx, base)
				}
				return services.Mutations.Archive(ctx, base)
			})
		},
	}
	command.Flags().StringVar(&taskID, "task", "", "Task UUIDv7")
	command.Flags().Int64Var(&expectedVersion, "expected-version", 0, "Current task version")
	command.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Client-generated UUIDv7 lifecycle idempotency key")
	command.Flags().StringVar(&reason, "reason", "", "Audit reason")
	return command
}

func newTaskRestoreCommand(config *lifecycleCLIConfig) *cobra.Command {
	var taskID, state, idempotencyKey, reason string
	var expectedVersion int64
	command := &cobra.Command{
		Use:   "restore",
		Short: "Restore a soft-deleted task",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			actor, reason, err := lifecycleActorAndReason(config, reason)
			if err != nil {
				return err
			}
			if _, err := requiredText("task", taskID); err != nil {
				return err
			}
			if _, err := requiredText("state", state); err != nil {
				return err
			}
			if err := requiredPositive("expected-version", expectedVersion); err != nil {
				return err
			}
			idempotencyKey, err := requiredLifecycleIdempotencyKey(idempotencyKey)
			if err != nil {
				return err
			}
			return executeLifecycleCommand(cmd, config, func(ctx context.Context, services *app.LifecycleServices) (any, error) {
				if receipt, err := replayCompletedLifecycleMutation(ctx, services, app.LifecycleMutationRestore, idempotencyKey); err != nil {
					return nil, err
				} else if receipt != nil {
					return *receipt, nil
				}
				checkpoint, replayed, err := lifecycleMutationCheckpointForKey(ctx, services, app.LifecycleMutationRestore, idempotencyKey)
				if err != nil {
					return nil, err
				}
				if !replayed {
					checkpoint, err = services.Mutations.CaptureCheckpoint(ctx, taskID, "", "", "")
					if err != nil {
						return nil, err
					}
				}
				if err := requireLifecycleCheckpointIdentity("task", taskID, checkpoint.TaskID); err != nil {
					return nil, err
				}
				if err := requireLifecycleCheckpointVersion("task", expectedVersion, checkpoint.TaskVersion); err != nil {
					return nil, err
				}
				return services.Mutations.Restore(ctx, app.RestoreLifecycleCommand{
					LifecycleMutationCommandBase: lifecycleMutationBase(idempotencyKey, actor, reason, checkpoint),
					RestoreState:                 store.TaskLifecycleState(state),
				})
			})
		},
	}
	command.Flags().StringVar(&taskID, "task", "", "Task UUIDv7")
	command.Flags().StringVar(&state, "state", "", "Target lifecycle state: draft, ready, published, or archived")
	command.Flags().Int64Var(&expectedVersion, "expected-version", 0, "Current task version")
	command.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Client-generated UUIDv7 lifecycle idempotency key")
	command.Flags().StringVar(&reason, "reason", "", "Audit reason")
	return command
}

func newTaskPurgeCommand(config *lifecycleCLIConfig) *cobra.Command {
	var taskID, idempotencyKey, reason string
	var expectedVersion int64
	var dryRun, yes bool
	command := &cobra.Command{
		Use:   "purge",
		Short: "Preview or irreversibly purge a soft-deleted task",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := requiredText("task", taskID); err != nil {
				return err
			}
			if dryRun {
				if yes {
					return fmt.Errorf("--yes cannot be combined with --dry-run")
				}
				return executeLifecycleReadOnlyCommand(cmd, config, func(ctx context.Context, services *app.LifecycleServices) (any, error) {
					return services.Deletion.PreviewPurgeTask(ctx, taskID)
				})
			}
			if !yes {
				return fmt.Errorf("--yes is required to irreversibly purge a task")
			}
			actor, reason, err := lifecycleActorAndReason(config, reason)
			if err != nil {
				return err
			}
			if err := requiredPositive("expected-version", expectedVersion); err != nil {
				return err
			}
			if _, err := requiredText("idempotency-key", idempotencyKey); err != nil {
				return err
			}
			return executeLifecycleCommand(cmd, config, func(ctx context.Context, services *app.LifecycleServices) (any, error) {
				return services.Deletion.PurgeTask(ctx, app.PurgeTaskRequest{
					TaskID:              taskID,
					ExpectedTaskVersion: expectedVersion,
					IdempotencyKey:      idempotencyKey,
					Actor:               actor,
					Reason:              reason,
				})
			})
		},
	}
	command.Flags().StringVar(&taskID, "task", "", "Task UUIDv7")
	command.Flags().Int64Var(&expectedVersion, "expected-version", 0, "Current soft-deleted task version")
	command.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Client-generated purge idempotency key")
	command.Flags().StringVar(&reason, "reason", "", "Audit reason for irreversible purge")
	command.Flags().BoolVar(&dryRun, "dry-run", false, "Show lifecycle and dependency blockers without creating a purge operation")
	command.Flags().BoolVar(&yes, "yes", false, "Confirm irreversible task purge")
	return command
}

type taskContinuationPlanOutput struct {
	Plan        workflowkit.ContinuationPlanSnapshot `json:"plan"`
	Fingerprint workflowkit.Fingerprint              `json:"fingerprint"`
	Persisted   bool                                 `json:"persisted"`
	Executable  bool                                 `json:"executable"`
}

func taskContinuationPlanResult(plan workflowkit.ContinuationPlan, persisted bool) taskContinuationPlanOutput {
	return taskContinuationPlanOutput{
		Plan:        plan.Snapshot(),
		Fingerprint: plan.Fingerprint(),
		Persisted:   persisted,
		Executable:  persisted,
	}
}

// newTaskContinueCommand exposes the single public continuation entry point.
// A normal invocation freezes a durable plan; --dry-run is intentionally a
// pure preview, while --plan plus --yes consumes only a previously persisted
// frozen plan.
func newTaskContinueCommand(config *lifecycleCLIConfig) *cobra.Command {
	var runID, planID, idempotencyKey, reason, scope string
	var stageGroups []string
	var dryRun, yes bool
	command := &cobra.Command{
		Use:   "continue",
		Short: "Plan or execute one durable task continuation",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if scope != "affected" {
				return fmt.Errorf("scope must be affected")
			}
			if strings.TrimSpace(planID) != "" {
				if dryRun {
					return fmt.Errorf("dry-run cannot execute an existing continuation plan")
				}
				if !yes {
					return fmt.Errorf("--yes is required to execute an existing continuation plan")
				}
				if strings.TrimSpace(runID) != "" || strings.TrimSpace(idempotencyKey) != "" || strings.TrimSpace(reason) != "" || len(stageGroups) != 0 {
					return fmt.Errorf("--plan cannot be combined with run planning inputs")
				}
				return executeLifecycleCommand(cmd, config, func(ctx context.Context, services *app.LifecycleServices) (any, error) {
					return services.Continuations.ExecuteTaskContinuation(ctx, planID)
				})
			}
			if yes {
				return fmt.Errorf("--yes requires --plan")
			}
			actor, reason, err := lifecycleActorAndReason(config, reason)
			if err != nil {
				return err
			}
			if _, err := requiredText("run", runID); err != nil {
				return err
			}
			if _, err := requiredText("idempotency-key", idempotencyKey); err != nil {
				return err
			}
			action := func(ctx context.Context, services *app.LifecycleServices) (any, error) {
				checkpoint, err := services.Continuations.CurrentCheckpoint(ctx, runID)
				if err != nil {
					return nil, err
				}
				continuation := app.ContinueTaskCommand{
					CommandKey:        idempotencyKey,
					TaskID:            checkpoint.SubjectID,
					RunID:             runID,
					Expected:          checkpoint,
					TargetStageGroups: append([]string(nil), stageGroups...),
					ForceSelected:     len(stageGroups) > 0,
					Actor:             actor,
					Reason:            reason,
				}
				if dryRun {
					plan, err := services.Continuations.PreviewTaskContinuation(ctx, continuation)
					if err != nil {
						return nil, err
					}
					return taskContinuationPlanResult(plan, false), nil
				}
				plan, err := services.Continuations.PlanTaskContinuation(ctx, continuation)
				if err != nil {
					return nil, err
				}
				return taskContinuationPlanResult(plan, true), nil
			}
			if dryRun {
				return executeLifecycleReadOnlyCommand(cmd, config, action)
			}
			return executeLifecycleCommand(cmd, config, action)
		},
	}
	command.Flags().StringVar(&runID, "run", "", "Source Run UUIDv7 for planning")
	command.Flags().StringVar(&planID, "plan", "", "Persisted continuation plan UUIDv7 to execute")
	command.Flags().StringSliceVar(&stageGroups, "from-stage", nil, "Frozen stage group to recompute; repeat for multiple groups")
	command.Flags().StringVar(&scope, "scope", "affected", "Invalidation scope; only affected is supported")
	command.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Client-generated continuation idempotency key")
	command.Flags().StringVar(&reason, "reason", "", "Audit reason for planning")
	command.Flags().BoolVar(&dryRun, "dry-run", false, "Preview a continuation plan without persisting command, plan, or audit state")
	command.Flags().BoolVar(&yes, "yes", false, "Execute the persisted continuation plan named by --plan")
	command.Flags().Bool("json", true, "Emit JSON output (the lifecycle CLI always emits JSON)")
	return command
}

func newRevisionCommand(config *lifecycleCLIConfig) *cobra.Command {
	command := &cobra.Command{Use: "revision", Short: "Inspect and create immutable task revisions", Args: cobra.NoArgs, RunE: showCommandGroupHelp}
	command.AddCommand(
		newRevisionListCommand(config),
		newRevisionShowCommand(config),
		newRevisionCreateCommand(config),
		newRevisionDiffCommand(config),
		newRevisionValidateCommand(config),
		newRevisionRollbackCommand(config),
	)
	return command
}

func newRevisionListCommand(config *lifecycleCLIConfig) *cobra.Command {
	var taskID string
	command := &cobra.Command{
		Use:   "list",
		Short: "List immutable revisions for a task",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			id, err := requiredText("task", taskID)
			if err != nil {
				return err
			}
			return executeLifecycleCommand(cmd, config, func(ctx context.Context, services *app.LifecycleServices) (any, error) {
				return services.Revisions.List(ctx, id)
			})
		},
	}
	command.Flags().StringVar(&taskID, "task", "", "Task UUIDv7")
	return command
}

func newRevisionShowCommand(config *lifecycleCLIConfig) *cobra.Command {
	var revisionID string
	command := &cobra.Command{
		Use:   "show",
		Short: "Show an immutable revision",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			id, err := requiredText("revision", revisionID)
			if err != nil {
				return err
			}
			return executeLifecycleCommand(cmd, config, func(ctx context.Context, services *app.LifecycleServices) (any, error) {
				return services.Revisions.Get(ctx, id)
			})
		},
	}
	command.Flags().StringVar(&revisionID, "revision", "", "Revision UUIDv7")
	return command
}

func newRevisionCreateCommand(config *lifecycleCLIConfig) *cobra.Command {
	var id, taskID, parentID, source, origin, proposalDigest, summary, metadata, reason string
	command := &cobra.Command{
		Use:   "create",
		Short: "Create a sealed revision from a strict candidate snapshot",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			actor, reason, err := lifecycleActorAndReason(config, reason)
			if err != nil {
				return err
			}
			if _, err := requiredText("task", taskID); err != nil {
				return err
			}
			if _, err := requiredText("source", source); err != nil {
				return err
			}
			if _, err := requiredText("origin", origin); err != nil {
				return err
			}
			return executeLifecycleCommand(cmd, config, func(ctx context.Context, services *app.LifecycleServices) (any, error) {
				return services.Revisions.CreateFromSnapshot(ctx, app.CreateRevisionFromSnapshotRequest{
					ID: id, TaskID: taskID, ParentRevisionID: parentID, Origin: store.RevisionOrigin(origin),
					SourceDirectory: source, ProposalDigest: proposalDigest, ChangeSummary: summary, MetadataJSON: metadata,
					Actor: actor, Reason: reason,
				})
			})
		},
	}
	command.Flags().StringVar(&id, "id", "", "Optional UUIDv7 revision identity")
	command.Flags().StringVar(&taskID, "task", "", "Task UUIDv7")
	command.Flags().StringVar(&parentID, "parent", "", "Optional parent revision UUIDv7")
	command.Flags().StringVar(&source, "source", "", "Strict candidate task directory")
	command.Flags().StringVar(&origin, "origin", "", "Revision origin: generated, imported, manual, repair, fork, or rollback")
	command.Flags().StringVar(&proposalDigest, "proposal-digest", "", "Optional proposal digest")
	command.Flags().StringVar(&summary, "change-summary", "", "Change summary")
	command.Flags().StringVar(&metadata, "metadata-json", "", "Revision metadata JSON")
	command.Flags().StringVar(&reason, "reason", "", "Audit reason")
	return command
}

func newRevisionDiffCommand(config *lifecycleCLIConfig) *cobra.Command {
	var left, right string
	command := &cobra.Command{
		Use:   "diff",
		Short: "Compare two immutable revisions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			leftID, err := requiredText("left", left)
			if err != nil {
				return err
			}
			rightID, err := requiredText("right", right)
			if err != nil {
				return err
			}
			return executeLifecycleCommand(cmd, config, func(ctx context.Context, services *app.LifecycleServices) (any, error) {
				return services.Revisions.Diff(ctx, leftID, rightID)
			})
		},
	}
	command.Flags().StringVar(&left, "left", "", "Left revision UUIDv7")
	command.Flags().StringVar(&right, "right", "", "Right revision UUIDv7")
	return command
}

func newRevisionValidateCommand(config *lifecycleCLIConfig) *cobra.Command {
	var revisionID, evidence, reason string
	var expectedVersion int64
	command := &cobra.Command{
		Use:   "validate",
		Short: "Record completed blocking validation evidence",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			actor, reason, err := lifecycleActorAndReason(config, reason)
			if err != nil {
				return err
			}
			if _, err := requiredText("revision", revisionID); err != nil {
				return err
			}
			if _, err := requiredText("evidence", evidence); err != nil {
				return err
			}
			if err := requiredPositive("expected-version", expectedVersion); err != nil {
				return err
			}
			return executeLifecycleCommand(cmd, config, func(ctx context.Context, services *app.LifecycleServices) (any, error) {
				return services.Revisions.MarkValidated(ctx, revisionID, expectedVersion, evidence, actor, reason)
			})
		},
	}
	command.Flags().StringVar(&revisionID, "revision", "", "Revision UUIDv7")
	command.Flags().StringVar(&evidence, "evidence", "", "Immutable evidence manifest digest")
	command.Flags().Int64Var(&expectedVersion, "expected-version", 0, "Current revision state version")
	command.Flags().StringVar(&reason, "reason", "", "Audit reason")
	return command
}

func newRevisionRollbackCommand(config *lifecycleCLIConfig) *cobra.Command {
	var taskID, target, parent, summary, metadata, reason string
	command := &cobra.Command{
		Use:   "rollback",
		Short: "Create a new revision copied from an earlier revision",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			actor, reason, err := lifecycleActorAndReason(config, reason)
			if err != nil {
				return err
			}
			if _, err := requiredText("task", taskID); err != nil {
				return err
			}
			if _, err := requiredText("target", target); err != nil {
				return err
			}
			return executeLifecycleCommand(cmd, config, func(ctx context.Context, services *app.LifecycleServices) (any, error) {
				return services.Revisions.CreateRollbackRevision(ctx, app.CreateRollbackRevisionRequest{
					TaskID: taskID, TargetRevisionID: target, ParentRevisionID: parent, ChangeSummary: summary,
					MetadataJSON: metadata, Actor: actor, Reason: reason,
				})
			})
		},
	}
	command.Flags().StringVar(&taskID, "task", "", "Task UUIDv7")
	command.Flags().StringVar(&target, "target", "", "Revision to copy")
	command.Flags().StringVar(&parent, "parent", "", "Optional new revision parent")
	command.Flags().StringVar(&summary, "change-summary", "", "Change summary")
	command.Flags().StringVar(&metadata, "metadata-json", "", "Revision metadata JSON")
	command.Flags().StringVar(&reason, "reason", "", "Audit reason")
	return command
}

func newReviewCommand(config *lifecycleCLIConfig) *cobra.Command {
	command := &cobra.Command{Use: "review", Short: "Record review requests and decisions", Args: cobra.NoArgs, RunE: showCommandGroupHelp}
	command.AddCommand(newReviewRequestCommand(config), newReviewDecideCommand(config), newReviewPromoteCommand(config))
	return command
}

func newReviewRequestCommand(config *lifecycleCLIConfig) *cobra.Command {
	var revisionID, evidence, reason string
	command := &cobra.Command{
		Use:   "request",
		Short: "Open a review request for one immutable revision",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			actor, reason, err := lifecycleActorAndReason(config, reason)
			if err != nil {
				return err
			}
			if _, err := requiredText("revision", revisionID); err != nil {
				return err
			}
			if _, err := requiredText("evidence", evidence); err != nil {
				return err
			}
			return executeLifecycleCommand(cmd, config, func(ctx context.Context, services *app.LifecycleServices) (any, error) {
				return services.Reviews.Request(ctx, revisionID, evidence, actor, reason)
			})
		},
	}
	command.Flags().StringVar(&revisionID, "revision", "", "Revision UUIDv7")
	command.Flags().StringVar(&evidence, "evidence", "", "Evidence manifest digest")
	command.Flags().StringVar(&reason, "reason", "", "Audit reason")
	return command
}

func newReviewDecideCommand(config *lifecycleCLIConfig) *cobra.Command {
	var requestID, revisionID, action, digest, idempotencyKey, reason string
	command := &cobra.Command{
		Use:   "decide",
		Short: "Record an approve, request-changes, or terminal-reject decision",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			actor, reason, err := lifecycleActorAndReason(config, reason)
			if err != nil {
				return err
			}
			if _, err := requiredText("request", requestID); err != nil {
				return err
			}
			if _, err := requiredText("revision", revisionID); err != nil {
				return err
			}
			if _, err := requiredText("action", action); err != nil {
				return err
			}
			if _, err := requiredText("expected-digest", digest); err != nil {
				return err
			}
			idempotencyKey, err := requiredLifecycleIdempotencyKey(idempotencyKey)
			if err != nil {
				return err
			}
			return executeLifecycleCommand(cmd, config, func(ctx context.Context, services *app.LifecycleServices) (any, error) {
				if receipt, err := replayCompletedLifecycleMutation(ctx, services, app.LifecycleMutationReview, idempotencyKey); err != nil {
					return nil, err
				} else if receipt != nil {
					return *receipt, nil
				}
				checkpoint, replayed, err := lifecycleMutationCheckpointForKey(ctx, services, app.LifecycleMutationReview, idempotencyKey)
				if err != nil {
					return nil, err
				}
				if !replayed {
					checkpoint, err = services.Mutations.CaptureReviewCheckpoint(ctx, "", revisionID, requestID)
					if err != nil {
						return nil, err
					}
				}
				if err := requireLifecycleCheckpointIdentity("revision", revisionID, checkpoint.RevisionID); err != nil {
					return nil, err
				}
				if err := requireLifecycleCheckpointIdentity("review request", requestID, checkpoint.ReviewRequestID); err != nil {
					return nil, err
				}
				if err := requireLifecycleCheckpointDigest(digest, checkpoint.RevisionDigest); err != nil {
					return nil, err
				}
				return services.Mutations.DecideReview(ctx, app.DecideReviewLifecycleCommand{
					LifecycleMutationCommandBase: lifecycleMutationBase(idempotencyKey, actor, reason, checkpoint),
					Decision:                     store.ReviewDecisionAction(action),
				})
			})
		},
	}
	command.Flags().StringVar(&requestID, "request", "", "Review request UUIDv7")
	command.Flags().StringVar(&revisionID, "revision", "", "Revision UUIDv7")
	command.Flags().StringVar(&action, "action", "", "approve, request_changes, or reject_terminal")
	command.Flags().StringVar(&digest, "expected-digest", "", "Approved revision digest")
	command.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Client-generated UUIDv7 lifecycle idempotency key")
	command.Flags().StringVar(&reason, "reason", "", "Audit reason")
	return command
}

func newReviewPromoteCommand(config *lifecycleCLIConfig) *cobra.Command {
	var taskID, revisionID, reason string
	var expectedVersion int64
	command := &cobra.Command{
		Use:   "promote",
		Short: "Promote an approved validated revision to current",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			actor, reason, err := lifecycleActorAndReason(config, reason)
			if err != nil {
				return err
			}
			if _, err := requiredText("task", taskID); err != nil {
				return err
			}
			if _, err := requiredText("revision", revisionID); err != nil {
				return err
			}
			if err := requiredPositive("expected-version", expectedVersion); err != nil {
				return err
			}
			return executeLifecycleCommand(cmd, config, func(ctx context.Context, services *app.LifecycleServices) (any, error) {
				return services.Reviews.PromoteCurrent(ctx, taskID, revisionID, expectedVersion, actor, reason)
			})
		},
	}
	command.Flags().StringVar(&taskID, "task", "", "Task UUIDv7")
	command.Flags().StringVar(&revisionID, "revision", "", "Revision UUIDv7")
	command.Flags().Int64Var(&expectedVersion, "expected-version", 0, "Current task version")
	command.Flags().StringVar(&reason, "reason", "", "Audit reason")
	return command
}

func newRunCommandV2(config *lifecycleCLIConfig) *cobra.Command {
	command := &cobra.Command{Use: "run", Short: "Create and inspect frozen workflow runs", Args: cobra.NoArgs, RunE: showCommandGroupHelp}
	command.AddCommand(
		newRunStartCommand(config),
		newRunShowCommand(config),
		newRunListCommand(config),
		newRunPauseCommand(config),
		newRunCancelStageCommand(config),
		newRunTerminateCommand(config),
		newRunControlShowCommand(config),
		newRunAttachCommand(config),
		newRunReconcileCommand(config),
		newRunDetachCommand(config),
		newRunWorkerCommand(config),
	)
	return command
}

func newRunStartCommand(config *lifecycleCLIConfig) *cobra.Command {
	var taskID, revisionID, profilePath, executionSpecPath, trigger, idempotencyKey, reason string
	command := &cobra.Command{
		Use:   "start",
		Short: "Freeze an explicit profile into a queued workflow run",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			actor, reason, err := lifecycleActorAndReason(config, reason)
			if err != nil {
				return err
			}
			if _, err := requiredText("task", taskID); err != nil {
				return err
			}
			if _, err := requiredText("revision", revisionID); err != nil {
				return err
			}
			if _, err := requiredText("profile", profilePath); err != nil {
				return err
			}
			if _, err := requiredText("execution-spec", executionSpecPath); err != nil {
				return err
			}
			if _, err := requiredText("trigger", trigger); err != nil {
				return err
			}
			idempotencyKey, err := requiredLifecycleIdempotencyKey(idempotencyKey)
			if err != nil {
				return err
			}
			return executeLifecycleCommand(cmd, config, func(ctx context.Context, services *app.LifecycleServices) (any, error) {
				if receipt, err := replayCompletedLifecycleMutation(ctx, services, app.LifecycleMutationStartRun, idempotencyKey); err != nil {
					return nil, err
				} else if receipt != nil {
					return *receipt, nil
				}
				checkpoint, replayed, err := lifecycleMutationCheckpointForKey(ctx, services, app.LifecycleMutationStartRun, idempotencyKey)
				if err != nil {
					return nil, err
				}
				if !replayed {
					checkpoint, err = services.Mutations.CaptureCheckpoint(ctx, taskID, revisionID, "", "")
					if err != nil {
						return nil, err
					}
				}
				if err := requireLifecycleCheckpointIdentity("task", taskID, checkpoint.TaskID); err != nil {
					return nil, err
				}
				if err := requireLifecycleCheckpointIdentity("revision", revisionID, checkpoint.RevisionID); err != nil {
					return nil, err
				}
				return services.Mutations.StartRun(ctx, app.StartRunLifecycleCommand{
					LifecycleMutationCommandBase: lifecycleMutationBase(idempotencyKey, actor, reason, checkpoint),
					ProfilePath:                  profilePath,
					ExecutionSpecPath:            executionSpecPath,
					Trigger:                      trigger,
				})
			})
		},
	}
	command.Flags().StringVar(&taskID, "task", "", "Task UUIDv7")
	command.Flags().StringVar(&revisionID, "revision", "", "Revision UUIDv7")
	command.Flags().StringVar(&profilePath, "profile", "", "Complete explicit execution-profile JSON")
	command.Flags().StringVar(&executionSpecPath, "execution-spec", "", "Complete typed run-execution-spec JSON")
	command.Flags().StringVar(&trigger, "trigger", "", "Run trigger")
	command.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Client-generated UUIDv7 lifecycle idempotency key")
	command.Flags().StringVar(&reason, "reason", "", "Audit reason")
	return command
}

func newRunShowCommand(config *lifecycleCLIConfig) *cobra.Command {
	var runID string
	command := &cobra.Command{
		Use:   "show",
		Short: "Show a frozen workflow run",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			id, err := requiredText("run", runID)
			if err != nil {
				return err
			}
			return executeLifecycleCommand(cmd, config, func(ctx context.Context, services *app.LifecycleServices) (any, error) {
				return services.Runs.Get(ctx, id)
			})
		},
	}
	command.Flags().StringVar(&runID, "run", "", "Run UUIDv7")
	return command
}

func newRunListCommand(config *lifecycleCLIConfig) *cobra.Command {
	var taskID string
	command := &cobra.Command{
		Use:   "list",
		Short: "List runs for one task",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			id, err := requiredText("task", taskID)
			if err != nil {
				return err
			}
			return executeLifecycleCommand(cmd, config, func(ctx context.Context, services *app.LifecycleServices) (any, error) {
				return services.Runs.ListForTask(ctx, id)
			})
		},
	}
	command.Flags().StringVar(&taskID, "task", "", "Task UUIDv7")
	return command
}

func newRunAttachCommand(config *lifecycleCLIConfig) *cobra.Command {
	var runID string
	command := &cobra.Command{
		Use:   "attach",
		Short: "Read local durable runtime state for a run without mutating it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := requiredText("run", runID); err != nil {
				return err
			}
			return executeLifecycleReadOnlyCommand(cmd, config, func(ctx context.Context, services *app.LifecycleServices) (any, error) {
				return services.LocalRuntime.AttachRun(ctx, app.AttachRunRequest{RunID: runID})
			})
		},
	}
	command.Flags().StringVar(&runID, "run", "", "Run UUIDv7")
	return command
}

func newRunReconcileCommand(config *lifecycleCLIConfig) *cobra.Command {
	var runID, reason string
	command := &cobra.Command{
		Use:   "reconcile",
		Short: "Recover scoped local jobs, leases, controls, and quota state for a run",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			actor, reason, err := lifecycleActorAndReason(config, reason)
			if err != nil {
				return err
			}
			if _, err := requiredText("run", runID); err != nil {
				return err
			}
			return executeLifecycleCommand(cmd, config, func(ctx context.Context, services *app.LifecycleServices) (any, error) {
				return services.LocalRuntime.ReconcileRun(ctx, app.ReconcileRunRequest{RunID: runID, Actor: actor, Reason: reason})
			})
		},
	}
	command.Flags().StringVar(&runID, "run", "", "Run UUIDv7")
	command.Flags().StringVar(&reason, "reason", "", "Audit reason for local recovery")
	return command
}

func newReleaseCommand(config *lifecycleCLIConfig) *cobra.Command {
	command := &cobra.Command{Use: "release", Short: "Build and manage local immutable package releases", Args: cobra.NoArgs, RunE: showCommandGroupHelp}
	command.AddCommand(newReleasePackageCommand(config), newReleaseListCommand(config), newReleaseWithdrawCommand(config), newReleasePromoteCommand(config))
	return command
}

func newReleasePackageCommand(config *lifecycleCLIConfig) *cobra.Command {
	var revisionID, runID, version, idempotencyKey, reason string
	var expectedStateVersion int64
	command := &cobra.Command{
		Use:   "package",
		Short: "Build a local immutable package; it never uploads or publishes remotely",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			actor, reason, err := lifecycleActorAndReason(config, reason)
			if err != nil {
				return err
			}
			if _, err := requiredText("revision", revisionID); err != nil {
				return err
			}
			if _, err := requiredText("version", version); err != nil {
				return err
			}
			if err := requiredPositive("expected-state-version", expectedStateVersion); err != nil {
				return err
			}
			idempotencyKey, err := requiredLifecycleIdempotencyKey(idempotencyKey)
			if err != nil {
				return err
			}
			return executeLifecycleCommand(cmd, config, func(ctx context.Context, services *app.LifecycleServices) (any, error) {
				if receipt, err := replayCompletedLifecycleMutation(ctx, services, app.LifecycleMutationPackage, idempotencyKey); err != nil {
					return nil, err
				} else if receipt != nil {
					return *receipt, nil
				}
				checkpoint, replayed, err := lifecycleMutationCheckpointForKey(ctx, services, app.LifecycleMutationPackage, idempotencyKey)
				if err != nil {
					return nil, err
				}
				if !replayed {
					checkpoint, err = services.Mutations.CaptureCheckpoint(ctx, "", revisionID, runID, "")
					if err != nil {
						return nil, err
					}
				}
				if err := requireLifecycleCheckpointIdentity("revision", revisionID, checkpoint.RevisionID); err != nil {
					return nil, err
				}
				if err := requireLifecycleCheckpointIdentity("run", runID, checkpoint.RunID); err != nil {
					return nil, err
				}
				if err := requireLifecycleCheckpointVersion("revision state", expectedStateVersion, checkpoint.RevisionStateVersion); err != nil {
					return nil, err
				}
				return services.Mutations.Package(ctx, app.PackageLifecycleCommand{
					LifecycleMutationCommandBase: lifecycleMutationBase(idempotencyKey, actor, reason, checkpoint),
					ReleaseVersion:               version,
				})
			})
		},
	}
	command.Flags().StringVar(&revisionID, "revision", "", "Validated current revision UUIDv7")
	command.Flags().StringVar(&runID, "run", "", "Approved CodeEdge Phase-1 Run UUIDv7 when this revision has CodeEdge execution")
	command.Flags().Int64Var(&expectedStateVersion, "expected-state-version", 0, "Current revision state version")
	command.Flags().StringVar(&version, "version", "", "Globally unique local release version")
	command.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Client-generated UUIDv7 lifecycle idempotency key")
	command.Flags().StringVar(&reason, "reason", "", "Audit reason")
	return command
}

func newReleaseListCommand(config *lifecycleCLIConfig) *cobra.Command {
	var taskID string
	command := &cobra.Command{
		Use:   "list",
		Short: "List local package releases for a task",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			id, err := requiredText("task", taskID)
			if err != nil {
				return err
			}
			return executeLifecycleCommand(cmd, config, func(ctx context.Context, services *app.LifecycleServices) (any, error) {
				return services.Releases.List(ctx, id)
			})
		},
	}
	command.Flags().StringVar(&taskID, "task", "", "Task UUIDv7")
	return command
}

func newReleaseWithdrawCommand(config *lifecycleCLIConfig) *cobra.Command {
	var releaseID, idempotencyKey, reason string
	var expectedVersion int64
	command := &cobra.Command{
		Use:   "withdraw",
		Short: "Withdraw a local release record without deleting pinned evidence",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			actor, reason, err := lifecycleActorAndReason(config, reason)
			if err != nil {
				return err
			}
			if _, err := requiredText("release", releaseID); err != nil {
				return err
			}
			if err := requiredPositive("expected-version", expectedVersion); err != nil {
				return err
			}
			idempotencyKey, err := requiredLifecycleIdempotencyKey(idempotencyKey)
			if err != nil {
				return err
			}
			return executeLifecycleCommand(cmd, config, func(ctx context.Context, services *app.LifecycleServices) (any, error) {
				if receipt, err := replayCompletedLifecycleMutation(ctx, services, app.LifecycleMutationWithdraw, idempotencyKey); err != nil {
					return nil, err
				} else if receipt != nil {
					return *receipt, nil
				}
				checkpoint, replayed, err := lifecycleMutationCheckpointForKey(ctx, services, app.LifecycleMutationWithdraw, idempotencyKey)
				if err != nil {
					return nil, err
				}
				if !replayed {
					checkpoint, err = services.Mutations.CaptureCheckpoint(ctx, "", "", "", releaseID)
					if err != nil {
						return nil, err
					}
				}
				if err := requireLifecycleCheckpointIdentity("release", releaseID, checkpoint.ReleaseID); err != nil {
					return nil, err
				}
				if err := requireLifecycleCheckpointVersion("release", expectedVersion, checkpoint.ReleaseRecordVersion); err != nil {
					return nil, err
				}
				return services.Mutations.Withdraw(ctx, lifecycleMutationBase(idempotencyKey, actor, reason, checkpoint))
			})
		},
	}
	command.Flags().StringVar(&releaseID, "release", "", "Release UUIDv7")
	command.Flags().Int64Var(&expectedVersion, "expected-version", 0, "Current release record version")
	command.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Client-generated UUIDv7 withdrawal idempotency key")
	command.Flags().StringVar(&reason, "reason", "", "Audit reason")
	return command
}

func newReleasePromoteCommand(config *lifecycleCLIConfig) *cobra.Command {
	var channel, releaseID, reason string
	var expectedVersion int64
	command := &cobra.Command{
		Use:   "promote",
		Short: "Move a local channel pointer to an immutable release",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			actor, reason, err := lifecycleActorAndReason(config, reason)
			if err != nil {
				return err
			}
			if _, err := requiredText("channel", channel); err != nil {
				return err
			}
			if _, err := requiredText("release", releaseID); err != nil {
				return err
			}
			return executeLifecycleCommand(cmd, config, func(ctx context.Context, services *app.LifecycleServices) (any, error) {
				return services.Releases.PromoteChannel(ctx, channel, releaseID, expectedVersion, actor, reason)
			})
		},
	}
	command.Flags().StringVar(&channel, "channel", "", "Local channel name")
	command.Flags().StringVar(&releaseID, "release", "", "Release UUIDv7")
	command.Flags().Int64Var(&expectedVersion, "expected-version", 0, "Current channel version; zero creates a channel")
	command.Flags().StringVar(&reason, "reason", "", "Audit reason")
	return command
}

func newWorkspaceCommand(config *lifecycleCLIConfig) *cobra.Command {
	command := &cobra.Command{Use: "workspace", Short: "Inspect and retire disposable managed workspaces", Args: cobra.NoArgs, RunE: showCommandGroupHelp}
	command.AddCommand(newWorkspaceListCommand(config), newWorkspaceTransitionCommand(config, "trash", store.WorkspaceTrash), newWorkspaceTransitionCommand(config, "purge", store.WorkspacePurged))
	return command
}

func newWorkspaceListCommand(config *lifecycleCLIConfig) *cobra.Command {
	var taskID string
	var includePurged bool
	command := &cobra.Command{
		Use:   "list",
		Short: "List disposable workspaces for a task",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			id, err := requiredText("task", taskID)
			if err != nil {
				return err
			}
			return executeLifecycleCommand(cmd, config, func(ctx context.Context, services *app.LifecycleServices) (any, error) {
				return services.Store().ListManagedWorkspacesForTask(ctx, id, includePurged)
			})
		},
	}
	command.Flags().StringVar(&taskID, "task", "", "Task UUIDv7")
	command.Flags().BoolVar(&includePurged, "include-purged", false, "Include purged workspace records")
	return command
}

func newWorkspaceTransitionCommand(config *lifecycleCLIConfig, use string, state store.WorkspaceState) *cobra.Command {
	var workspaceID, reason string
	var expectedVersion int64
	command := &cobra.Command{
		Use:   use,
		Short: "Transition a disposable workspace to " + string(state),
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			actor, reason, err := lifecycleActorAndReason(config, reason)
			if err != nil {
				return err
			}
			if _, err := requiredText("workspace", workspaceID); err != nil {
				return err
			}
			if err := requiredPositive("expected-version", expectedVersion); err != nil {
				return err
			}
			return executeLifecycleCommand(cmd, config, func(ctx context.Context, services *app.LifecycleServices) (any, error) {
				return services.Store().TransitionManagedWorkspace(ctx, store.TransitionManagedWorkspaceRequest{
					WorkspaceID: workspaceID, ExpectedVersion: expectedVersion, State: state, Actor: actor, Reason: reason,
				})
			})
		},
	}
	command.Flags().StringVar(&workspaceID, "workspace", "", "Workspace UUIDv7")
	command.Flags().Int64Var(&expectedVersion, "expected-version", 0, "Current workspace version")
	command.Flags().StringVar(&reason, "reason", "", "Audit reason")
	return command
}
