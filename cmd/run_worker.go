package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/spf13/cobra"
)

type runWorkerRuntimeFactory func(*app.LifecycleServices) (app.DurableJobHandler, error)

type runWorkerChildLauncher interface {
	LaunchDetachedRunWorker(context.Context, detachedRunWorkerRequest) (detachedRunWorkerHandoff, error)
}

type runWorkerCommandDependencies struct {
	newRuntime runWorkerRuntimeFactory
	newSession func(app.RunWorkerSessionConfig) (*app.RunWorkerSession, error)
}

func defaultRunWorkerRuntimeFactory(services *app.LifecycleServices) (app.DurableJobHandler, error) {
	// A worker registers every closed template compiled into this Harbor build,
	// but each plugin executor still dispatches only by the template frozen in
	// the claimed RunExecutionSpec. A catalog-lock-attested resolver installed
	// by the common lifecycle composition is reused exactly here. Without one,
	// the registry is an explicit rejector; it never revives the retired
	// app-specific executor, stage-name, PATH, or model-default fallback.
	options := stageprovider.WorkflowkitRegistryOptions{Templates: workflowadapter.BuiltinTemplateReferences()}
	if resolver := services.WorkflowkitProviderOperationResolver(); resolver != nil {
		options.Providers = resolver
	}
	registry, err := stageprovider.NewWorkflowkitStageExecutorRegistry(options)
	if err != nil {
		return nil, fmt.Errorf("create Harbor V2 workflowkit registry: %w", err)
	}
	runtime, err := app.NewFrozenExecutionRuntime(app.FrozenExecutionRuntimeConfig{Services: services, WorkflowkitRegistry: registry})
	if err != nil {
		return nil, err
	}
	return runtime, nil
}

func defaultRunWorkerCommandDependencies() runWorkerCommandDependencies {
	return runWorkerCommandDependencies{
		newRuntime: defaultRunWorkerRuntimeFactory,
		newSession: app.NewRunWorkerSession,
	}
}

// newRunWorkerCommand is an implementation endpoint used by a controlled
// child process. Normal operators use run start/attach/reconcile and the TUI;
// they do not hand-write a stage executor or mutable scheduler configuration.
func newRunWorkerCommand(config *lifecycleCLIConfig) *cobra.Command {
	return newRunWorkerCommandWithDependencies(config, defaultRunWorkerCommandDependencies())
}

func newRunWorkerCommandWithDependencies(config *lifecycleCLIConfig, dependencies runWorkerCommandDependencies) *cobra.Command {
	var runID, owner, reason, handoffOperationID, handoffLogPath string
	command := &cobra.Command{
		Use:    "worker",
		Short:  "Run a controlled V2 durable worker for exactly one Run",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			actor, reason, err := lifecycleActorAndReason(config, reason)
			if err != nil {
				return err
			}
			if _, err := requiredText("run", runID); err != nil {
				return err
			}
			if _, err := requiredText("owner", owner); err != nil {
				return err
			}
			if strings.TrimSpace(handoffOperationID) != "" {
				if err := store.ValidateUUIDv7(strings.TrimSpace(handoffOperationID)); err != nil {
					return fmt.Errorf("handoff operation: %w", err)
				}
				if _, err := requiredText("handoff-log", handoffLogPath); err != nil {
					return err
				}
			}
			if dependencies.newRuntime == nil || dependencies.newSession == nil {
				return fmt.Errorf("controlled V2 run-worker factory is not configured")
			}
			return executeLifecycleCommand(cmd, config, func(ctx context.Context, services *app.LifecycleServices) (any, error) {
				handler, err := dependencies.newRuntime(services)
				if err != nil {
					return nil, err
				}
				sessionConfig := app.RunWorkerSessionConfig{
					Services: services, RunID: runID, Owner: owner, Actor: actor, Reason: reason, Handler: handler,
				}
				if strings.TrimSpace(handoffOperationID) != "" {
					sessionConfig.HandoffOperationID = strings.TrimSpace(handoffOperationID)
					sessionConfig.HandoffProcessID = os.Getpid()
					sessionConfig.HandoffLogPath = handoffLogPath
				}
				session, err := dependencies.newSession(sessionConfig)
				if err != nil {
					return nil, err
				}
				signals := make(chan os.Signal, 2)
				signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
				defer signal.Stop(signals)
				result, runErr := runWorkerSessionWithSignals(ctx, session, signals)
				if runErr != nil {
					return nil, runErr
				}
				// A detached worker's stdout is appended to runs/<run>/worker.log,
				// so whatever this command prints becomes that log's content. The
				// full session result carries the frozen run manifest and the claimed
				// job payload, which repeat identically on every handoff and made up
				// the overwhelming majority of each record's bytes. Print the compact
				// projection instead; RunWorkerSessionResult stays the in-process
				// return value, so no caller or test contract on the Go structure
				// changes.
				return app.NewRunWorkerLogRecord(result), nil
			})
		},
	}
	command.Flags().StringVar(&runID, "run", "", "Run UUIDv7")
	command.Flags().StringVar(&owner, "owner", "", "Controlled worker owner identity")
	command.Flags().StringVar(&reason, "reason", "", "Audit reason")
	command.Flags().StringVar(&handoffOperationID, "handoff", "", "Reserved run-worker handoff UUIDv7")
	command.Flags().StringVar(&handoffLogPath, "handoff-log", "", "Managed child worker log path")
	return command
}

// newRunDetachCommand is the operator-facing local child handoff. The child
// itself invokes the hidden run worker endpoint so foreground and detached
// workers share one frozen-runtime and durable lease protocol.
func newRunDetachCommand(config *lifecycleCLIConfig) *cobra.Command {
	return newRunDetachCommandWithLauncher(config, executableRunWorkerLauncher{})
}

func newRunDetachCommandWithLauncher(config *lifecycleCLIConfig, launcher runWorkerChildLauncher) *cobra.Command {
	var runID, owner, reason, idempotencyKey string
	command := &cobra.Command{
		Use:   "detach",
		Short: "Launch a controlled local worker for one Run",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			actor, reason, err := lifecycleActorAndReason(config, reason)
			if err != nil {
				return err
			}
			idempotencyKey, err = requiredLifecycleIdempotencyKey(idempotencyKey)
			if err != nil {
				return err
			}
			if _, err := requiredText("run", runID); err != nil {
				return err
			}
			if _, err := requiredText("owner", owner); err != nil {
				return err
			}
			if launcher == nil {
				return fmt.Errorf("controlled child-worker launcher is not configured")
			}
			return executeLifecycleCommand(cmd, config, func(ctx context.Context, services *app.LifecycleServices) (any, error) {
				run, err := services.Runs.Get(ctx, runID)
				if err != nil {
					return nil, err
				}
				reserved, err := services.WorkerHandoffs.ReserveRunWorkerHandoff(ctx, app.ReserveRunWorkerHandoffCommand{
					IdempotencyKey: idempotencyKey, RunID: run.ID,
					Expected: app.RunWorkerHandoffCheckpoint{RunVersion: run.Version, ExecutionEpoch: run.ExecutionEpoch, DefinitionHash: run.DefinitionHash},
					Owner:    owner, Actor: actor, Reason: reason,
				})
				if err != nil {
					return nil, err
				}
				if !reserved.Launch {
					return reserved.Handoff, nil
				}
				receipt, launchErr := launcher.LaunchDetachedRunWorker(ctx, detachedRunWorkerRequest{
					Root: config.root, RunID: run.ID, Owner: owner, Reason: reason, HandoffOperationID: reserved.Handoff.ID,
				})
				if launchErr != nil {
					_, _ = services.WorkerHandoffs.FailRunWorkerHandoff(context.Background(), reserved.Handoff.ID, launchErr.Error(), actor, reason)
					return nil, launchErr
				}
				if receipt.RunID != run.ID || receipt.Owner != owner {
					cause := fmt.Errorf("controlled child receipt does not match reserved handoff")
					_, _ = services.WorkerHandoffs.FailRunWorkerHandoff(context.Background(), reserved.Handoff.ID, cause.Error(), actor, reason)
					return nil, cause
				}
				handoff, err := services.WorkerHandoffs.RecordRunWorkerHandoffSpawned(ctx, reserved.Handoff.ID, receipt.PID, receipt.LogPath, actor, reason)
				if err != nil {
					return nil, err
				}
				return handoff, nil
			})
		},
	}
	command.Flags().StringVar(&runID, "run", "", "Run UUIDv7")
	command.Flags().StringVar(&owner, "owner", "", "Controlled worker owner identity")
	command.Flags().StringVar(&reason, "reason", "", "Audit reason")
	command.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Client-generated UUIDv7 run-worker handoff idempotency key")
	return command
}

// runWorkerSessionWithSignals keeps a process alive after a signal has been
// converted into a durable control operation. It never maps SIGINT/SIGTERM to
// context cancellation: the frozen runtime must acknowledge pause/terminate
// through the Run's durable control state first.
func runWorkerSessionWithSignals(ctx context.Context, session *app.RunWorkerSession, signals <-chan os.Signal) (app.RunWorkerSessionResult, error) {
	if session == nil {
		return app.RunWorkerSessionResult{}, fmt.Errorf("controlled run worker session is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	done := make(chan runWorkerExecutionResult, 1)
	go func() {
		result, err := session.Run(ctx)
		done <- runWorkerExecutionResult{result: result, err: err}
	}()
	for {
		select {
		case completed := <-done:
			return completed.result, completed.err
		case received, open := <-signals:
			if !open {
				signals = nil
				continue
			}
			action, handled := signalControlAction(received)
			if !handled {
				continue
			}
			if _, err := session.RequestSignalControl(context.Background(), action); err != nil {
				return app.RunWorkerSessionResult{}, fmt.Errorf("persist %s signal control: %w", action, err)
			}
		}
	}
}

type runWorkerExecutionResult struct {
	result app.RunWorkerSessionResult
	err    error
}

func signalControlAction(received os.Signal) (store.ControlAction, bool) {
	switch received {
	case os.Interrupt:
		return store.ControlActionPause, true
	case syscall.SIGTERM:
		return store.ControlActionTerminate, true
	default:
		return "", false
	}
}
