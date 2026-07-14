package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

type detachedRunWorkerRequest struct {
	Root               string
	RunID              string
	Owner              string
	Reason             string
	HandoffOperationID string
}

// detachedRunWorkerHandoff is the parent-side receipt for a local child
// process. Durable ownership starts only when the child acquires its
// RunWorkerLeaseResourceType lease; PID is diagnostic data, not authority.
type detachedRunWorkerHandoff struct {
	RunID     string    `json:"run_id"`
	Owner     string    `json:"owner"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
	LogPath   string    `json:"log_path"`
}

type executableRunWorkerLauncher struct{}

// LaunchRunWorker adapts the application handoff port to the same controlled
// detached-child launcher used by `run detach`. The application service owns
// the reserve/receipt transaction; this method performs only the local spawn.
func (launcher executableRunWorkerLauncher) LaunchRunWorker(ctx context.Context, request app.RunWorkerHandoffLaunchRequest) (app.RunWorkerHandoffLaunchReceipt, error) {
	handoff, err := launcher.LaunchDetachedRunWorker(ctx, detachedRunWorkerRequest{
		Root:               request.ManagedRoot,
		RunID:              request.RunID,
		Owner:              request.Owner,
		Reason:             request.Reason,
		HandoffOperationID: request.HandoffOperationID,
	})
	if err != nil {
		return app.RunWorkerHandoffLaunchReceipt{}, err
	}
	return app.RunWorkerHandoffLaunchReceipt{
		RunID:     handoff.RunID,
		Owner:     handoff.Owner,
		ProcessID: handoff.PID,
		LogPath:   handoff.LogPath,
	}, nil
}

func (executableRunWorkerLauncher) LaunchDetachedRunWorker(ctx context.Context, request detachedRunWorkerRequest) (detachedRunWorkerHandoff, error) {
	if err := ctx.Err(); err != nil {
		return detachedRunWorkerHandoff{}, err
	}
	root := strings.TrimSpace(request.Root)
	owner := strings.TrimSpace(request.Owner)
	reason := strings.TrimSpace(request.Reason)
	if root == "" || owner == "" || reason == "" {
		return detachedRunWorkerHandoff{}, fmt.Errorf("controlled child root, owner, and reason are required")
	}
	if err := store.ValidateUUIDv7(strings.TrimSpace(request.RunID)); err != nil {
		return detachedRunWorkerHandoff{}, fmt.Errorf("controlled child run ID: %w", err)
	}
	if err := store.ValidateUUIDv7(strings.TrimSpace(request.HandoffOperationID)); err != nil {
		return detachedRunWorkerHandoff{}, fmt.Errorf("controlled child handoff operation ID: %w", err)
	}
	executable, err := os.Executable()
	if err != nil {
		return detachedRunWorkerHandoff{}, fmt.Errorf("resolve current executable for controlled child: %w", err)
	}
	logDirectory := filepath.Join(root, "runs", request.RunID)
	if err := os.MkdirAll(logDirectory, 0o750); err != nil {
		return detachedRunWorkerHandoff{}, fmt.Errorf("create controlled child log directory: %w", err)
	}
	logPath := filepath.Join(logDirectory, "worker.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return detachedRunWorkerHandoff{}, fmt.Errorf("open controlled child log: %w", err)
	}
	defer logFile.Close()
	command := exec.Command(executable,
		"--root", root,
		"run", "worker",
		"--run", request.RunID,
		"--owner", owner,
		"--reason", reason,
		"--handoff", strings.TrimSpace(request.HandoffOperationID),
		"--handoff-log", logPath,
	)
	command.Stdout = logFile
	command.Stderr = logFile
	configureDetachedRunWorkerProcess(command)
	if err := command.Start(); err != nil {
		return detachedRunWorkerHandoff{}, fmt.Errorf("start controlled child worker: %w", err)
	}
	handoff := detachedRunWorkerHandoff{
		RunID: request.RunID, Owner: owner, PID: command.Process.Pid, StartedAt: time.Now().UTC(), LogPath: logPath,
	}
	// This parent intentionally does not wait for the detached worker. Durable
	// run-worker and job-dispatch leases, rather than a parent process handle,
	// remain the authority for ownership and recovery.
	if err := command.Process.Release(); err != nil {
		// Start already transferred execution to the child. Returning an error
		// here would make a caller retry and launch another process for the same
		// Run, even though the durable supervisor lease correctly allows only
		// one of them to work. Keep the truthful handoff and leave diagnostics
		// beside the child output.
		_, _ = fmt.Fprintf(logFile, "controlled child parent handle release warning: %v\n", err)
	}
	return handoff, nil
}
