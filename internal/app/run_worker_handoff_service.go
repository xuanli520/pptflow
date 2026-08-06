package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// RunWorkerHandoffService owns the application boundary for handing one Run
// to a controlled local child worker. It deliberately does not spawn a
// process: CLI/TUI composition supplies that platform-specific action after a
// durable reserve has succeeded.
type RunWorkerHandoffService struct{ core *lifecycleServiceCore }

// RunWorkerHandoffCheckpoint is the full Run CAS basis captured by a client
// before asking to launch a child. A stale UI must be rejected rather than
// refreshed silently into a different execution epoch or definition.
type RunWorkerHandoffCheckpoint struct {
	RunVersion     int64
	ExecutionEpoch int
	DefinitionHash string
}

// ReserveRunWorkerHandoffCommand persists a one-time launch authority. Both
// ID and IdempotencyKey are UUIDv7 identities; retries must reuse the key.
type ReserveRunWorkerHandoffCommand struct {
	ID             string
	IdempotencyKey string
	RunID          string
	Expected       RunWorkerHandoffCheckpoint
	Owner          string
	Actor          string
	Reason         string
	LaunchTTL      time.Duration
}

// RunWorkerHandoffLaunchRequest is the narrow process-launch input exposed to
// CLI/TUI composition. The managed root is supplied by the application
// service, so callers cannot redirect a durable worker at an unrelated local
// control plane.
type RunWorkerHandoffLaunchRequest struct {
	ManagedRoot        string
	RunID              string
	Owner              string
	Reason             string
	HandoffOperationID string
}

// RunWorkerHandoffLaunchReceipt is the parent-side process receipt. Durable
// ownership still begins only when the child claims the reserved handoff.
type RunWorkerHandoffLaunchReceipt struct {
	RunID     string
	Owner     string
	ProcessID int
	LogPath   string
}

// RunWorkerHandoffLauncher is injected by a local composition root. It may
// start a child process or a test double, but it must not mutate lifecycle
// state directly.
type RunWorkerHandoffLauncher interface {
	LaunchRunWorker(context.Context, RunWorkerHandoffLaunchRequest) (RunWorkerHandoffLaunchReceipt, error)
}

// LaunchRunWorkerHandoff combines the parent-side durable protocol: reserve
// first, launch second, then record the immutable spawn receipt. A failed
// launch is recorded against the already-reserved operation; a replay never
// launches another child.
func (service *RunWorkerHandoffService) LaunchRunWorkerHandoff(ctx context.Context, command ReserveRunWorkerHandoffCommand, launcher RunWorkerHandoffLauncher) (store.RunWorkerHandoff, error) {
	if launcher == nil {
		return store.RunWorkerHandoff{}, fmt.Errorf("run-worker handoff launcher is required")
	}
	reserved, err := service.ReserveRunWorkerHandoff(ctx, command)
	if err != nil {
		return store.RunWorkerHandoff{}, err
	}
	if !reserved.Launch {
		return reserved.Handoff, nil
	}
	receipt, launchErr := launcher.LaunchRunWorker(ctx, RunWorkerHandoffLaunchRequest{
		ManagedRoot: service.core.layout.root, RunID: reserved.Handoff.RunID, Owner: reserved.Handoff.Owner,
		Reason: reserved.Handoff.Reason, HandoffOperationID: reserved.Handoff.ID,
	})
	if launchErr != nil {
		_, _ = service.FailRunWorkerHandoff(context.Background(), reserved.Handoff.ID, launchErr.Error(), reserved.Handoff.Actor, reserved.Handoff.Reason)
		return store.RunWorkerHandoff{}, launchErr
	}
	if receipt.RunID != reserved.Handoff.RunID || receipt.Owner != reserved.Handoff.Owner {
		cause := fmt.Errorf("controlled child launch receipt does not match reserved handoff")
		_, _ = service.FailRunWorkerHandoff(context.Background(), reserved.Handoff.ID, cause.Error(), reserved.Handoff.Actor, reserved.Handoff.Reason)
		return store.RunWorkerHandoff{}, cause
	}
	return service.RecordRunWorkerHandoffSpawned(ctx, reserved.Handoff.ID, receipt.ProcessID, receipt.LogPath, reserved.Handoff.Actor, reserved.Handoff.Reason)
}

// ReserveRunWorkerHandoff reserves a durable child-launch operation. It is
// safe to invoke before process spawning and validates the full observed Run
// checkpoint at the application boundary as well as in SQLite.
func (service *RunWorkerHandoffService) ReserveRunWorkerHandoff(ctx context.Context, command ReserveRunWorkerHandoffCommand) (store.ReserveRunWorkerHandoffResult, error) {
	if service == nil || service.core == nil || service.core.store == nil {
		return store.ReserveRunWorkerHandoffResult{}, fmt.Errorf("run-worker handoff service is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	runID := strings.TrimSpace(command.RunID)
	if err := store.ValidateUUIDv7(runID); err != nil {
		return store.ReserveRunWorkerHandoffResult{}, err
	}
	if strings.TrimSpace(command.ID) != "" {
		if err := store.ValidateUUIDv7(command.ID); err != nil {
			return store.ReserveRunWorkerHandoffResult{}, err
		}
	}
	if err := store.ValidateUUIDv7(strings.TrimSpace(command.IdempotencyKey)); err != nil {
		return store.ReserveRunWorkerHandoffResult{}, err
	}
	if command.Expected.RunVersion <= 0 || command.Expected.ExecutionEpoch < 0 || strings.TrimSpace(command.Expected.DefinitionHash) == "" {
		return store.ReserveRunWorkerHandoffResult{}, fmt.Errorf("run-worker handoff expected Run checkpoint is required")
	}
	owner, err := requiredRunWorkerHandoffText("owner", command.Owner)
	if err != nil {
		return store.ReserveRunWorkerHandoffResult{}, err
	}
	actor, err := requiredRunWorkerHandoffText("actor", command.Actor)
	if err != nil {
		return store.ReserveRunWorkerHandoffResult{}, err
	}
	reason, err := requiredRunWorkerHandoffText("reason", command.Reason)
	if err != nil {
		return store.ReserveRunWorkerHandoffResult{}, err
	}
	if command.LaunchTTL < 0 {
		return store.ReserveRunWorkerHandoffResult{}, fmt.Errorf("run-worker handoff launch TTL cannot be negative")
	}
	run, err := service.core.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return store.ReserveRunWorkerHandoffResult{}, err
	}
	if run == nil {
		return store.ReserveRunWorkerHandoffResult{}, fmt.Errorf("%w: run %s", ErrLifecycleNotFound, runID)
	}
	if run.Version != command.Expected.RunVersion || run.ExecutionEpoch != command.Expected.ExecutionEpoch || run.DefinitionHash != command.Expected.DefinitionHash {
		return store.ReserveRunWorkerHandoffResult{}, fmt.Errorf("%w: run-worker handoff checkpoint for run %s", store.ErrOptimisticLock, runID)
	}
	fingerprint, err := workflowkit.FingerprintParts("harbor.run-worker-handoff-request.v1", []workflowkit.FingerprintPart{
		{Name: "run_id", Value: []byte(runID)},
		{Name: "run_version", Value: []byte(fmt.Sprintf("%d", command.Expected.RunVersion))},
		{Name: "execution_epoch", Value: []byte(fmt.Sprintf("%d", command.Expected.ExecutionEpoch))},
		{Name: "definition_hash", Value: []byte(command.Expected.DefinitionHash)},
		{Name: "owner", Value: []byte(owner)},
		{Name: "actor", Value: []byte(actor)},
		{Name: "reason", Value: []byte(reason)},
		{Name: "launch_ttl", Value: []byte(command.LaunchTTL.String())},
	})
	if err != nil {
		return store.ReserveRunWorkerHandoffResult{}, err
	}
	return service.core.store.ReserveRunWorkerHandoff(ctx, store.ReserveRunWorkerHandoffRequest{
		ID:                        strings.TrimSpace(command.ID),
		IdempotencyKey:            strings.TrimSpace(command.IdempotencyKey),
		RequestFingerprint:        string(fingerprint),
		RunID:                     runID,
		ExpectedRunVersion:        command.Expected.RunVersion,
		ExpectedRunExecutionEpoch: command.Expected.ExecutionEpoch,
		ExpectedRunDefinitionHash: command.Expected.DefinitionHash,
		Owner:                     owner,
		Actor:                     actor,
		Reason:                    reason,
		LaunchTTL:                 command.LaunchTTL,
	})
}

// RecordRunWorkerHandoffSpawned records a child-process receipt after a
// successful local exec.Start. The receipt is separate from child ownership;
// the child must still claim the durable handoff fence.
func (service *RunWorkerHandoffService) RecordRunWorkerHandoffSpawned(ctx context.Context, operationID string, processID int, logPath, actor, reason string) (store.RunWorkerHandoff, error) {
	if service == nil || service.core == nil || service.core.store == nil {
		return store.RunWorkerHandoff{}, fmt.Errorf("run-worker handoff service is not configured")
	}
	return service.core.store.RecordRunWorkerHandoffSpawned(ctx, store.RecordRunWorkerHandoffSpawnedRequest{
		OperationID: operationID, ProcessID: processID, LogPath: logPath, Actor: actor, Reason: reason,
	})
}

// FailRunWorkerHandoff records a proven parent-side spawn failure. It never
// rewrites an operation that may already have a live child claim.
func (service *RunWorkerHandoffService) FailRunWorkerHandoff(ctx context.Context, operationID, failure, actor, reason string) (store.RunWorkerHandoff, error) {
	if service == nil || service.core == nil || service.core.store == nil {
		return store.RunWorkerHandoff{}, fmt.Errorf("run-worker handoff service is not configured")
	}
	return service.core.store.FailRunWorkerHandoff(ctx, store.FailRunWorkerHandoffRequest{
		OperationID: operationID, Failure: failure, Actor: actor, Reason: reason,
	})
}

// ClaimRunWorkerHandoff is the child-side application boundary. It consumes
// the reserved operation and returns the exact supervisor lease the worker
// must heartbeat and later release; it must not acquire a second lease.
func (service *RunWorkerHandoffService) ClaimRunWorkerHandoff(ctx context.Context, operationID, runID, owner string, processID int, logPath, actor, reason string, leaseTTL time.Duration) (store.RunWorkerHandoffClaim, error) {
	if service == nil || service.core == nil || service.core.store == nil {
		return store.RunWorkerHandoffClaim{}, fmt.Errorf("run-worker handoff service is not configured")
	}
	return service.core.store.ClaimRunWorkerHandoff(ctx, store.ClaimRunWorkerHandoffRequest{
		OperationID: operationID, RunID: runID, Owner: owner, ProcessID: processID, LogPath: logPath,
		LeaseTTL: leaseTTL, Actor: actor, Reason: reason,
	})
}

// ReleaseRunWorkerHandoff releases the consumed supervisor lease and closes
// the durable handoff receipt in one scoped store operation.
func (service *RunWorkerHandoffService) ReleaseRunWorkerHandoff(ctx context.Context, operationID string, lease store.Lease, actor, reason string) (store.RunWorkerHandoff, error) {
	if service == nil || service.core == nil || service.core.store == nil {
		return store.RunWorkerHandoff{}, fmt.Errorf("run-worker handoff service is not configured")
	}
	return service.core.store.ReleaseRunWorkerHandoff(ctx, store.ReleaseRunWorkerHandoffRequest{
		OperationID: operationID, WorkerLease: lease, Actor: actor, Reason: reason,
	})
}

func requiredRunWorkerHandoffText(label, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("run-worker handoff %s is required", label)
	}
	return value, nil
}
