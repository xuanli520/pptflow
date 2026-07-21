package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

type lockedDockerExecutableAttestor func(context.Context, stageprovider.LocalExecutableLock) error

// lockedDockerRuntime is the shared host process boundary used by CodeEdge
// Phase-1 and Standard authoring. A caller selects only one of the three
// deployment-lock command IDs and supplies direct Docker argv; executable
// path, environment, output bound and timeout remain host-owned.
type lockedDockerRuntime struct {
	commands map[string]stageprovider.LocalExecutableLock
	runner   CodeEdgePhase1CommandRunner
	timeout  time.Duration
	attest   lockedDockerExecutableAttestor
}

func newLockedDockerRuntime(commands map[string]stageprovider.LocalExecutableLock, runner CodeEdgePhase1CommandRunner, timeout time.Duration, attestor lockedDockerExecutableAttestor) (*lockedDockerRuntime, error) {
	if len(commands) != 3 {
		return nil, errors.New("locked Docker runtime requires exactly three command locks")
	}
	cloned := make(map[string]stageprovider.LocalExecutableLock, len(commands))
	for _, commandID := range []string{
		stageprovider.CodeEdgePhase1DockerBuildCommandID,
		stageprovider.CodeEdgePhase1InitialVerifyCommandID,
		stageprovider.CodeEdgePhase1OracleVerifyCommandID,
	} {
		lock, found := commands[commandID]
		if !found || lock.CommandID != commandID {
			return nil, fmt.Errorf("locked Docker runtime command %q is unavailable", commandID)
		}
		cloned[commandID] = lock
	}
	if runner == nil {
		runner = CodeEdgePhase1DirectCommandRunner{}
	}
	if timeout <= 0 {
		timeout = 20 * time.Minute
	}
	return &lockedDockerRuntime{commands: cloned, runner: runner, timeout: timeout, attest: attestor}, nil
}

func (runtime *lockedDockerRuntime) command(commandID string) (stageprovider.LocalExecutableLock, error) {
	if runtime == nil || runtime.runner == nil {
		return stageprovider.LocalExecutableLock{}, errors.New("locked Docker runtime is not configured")
	}
	lock, found := runtime.commands[commandID]
	if !found {
		return stageprovider.LocalExecutableLock{}, fmt.Errorf("locked Docker command %q is not installed", commandID)
	}
	return lock, nil
}

func (runtime *lockedDockerRuntime) run(ctx context.Context, commandID string, args []string, directory string) (CodeEdgePhase1CommandResult, workflowkit.Fingerprint, error) {
	if ctx == nil {
		return CodeEdgePhase1CommandResult{}, "", errors.New("locked Docker command context is required")
	}
	if err := ctx.Err(); err != nil {
		return CodeEdgePhase1CommandResult{}, "", err
	}
	lock, err := runtime.command(commandID)
	if err != nil {
		return CodeEdgePhase1CommandResult{}, "", err
	}
	if runtime.attest != nil {
		if err := runtime.attest(ctx, lock); err != nil {
			return CodeEdgePhase1CommandResult{}, "", fmt.Errorf("attest locked Docker executable: %w", err)
		}
	}
	if err := ensureLockedDockerCommandDirectory(directory); err != nil {
		return CodeEdgePhase1CommandResult{}, "", err
	}
	for _, name := range []string{"command-home", "command-tmp"} {
		if err := ensureLockedDockerPrivateDirectory(filepath.Join(directory, name)); err != nil {
			return CodeEdgePhase1CommandResult{}, "", err
		}
	}
	command := CodeEdgePhase1Command{
		Path: lock.AbsolutePath,
		Args: append([]string(nil), args...),
		Dir:  directory,
		Env:  lockedDockerCommandEnvironment(directory),
	}
	commandCtx, cancel := context.WithTimeout(ctx, runtime.timeout)
	defer cancel()
	result, runErr := runtime.runner.Run(commandCtx, command)
	if commandCtx.Err() != nil && runErr == nil {
		runErr = commandCtx.Err()
	}
	fingerprint, fingerprintErr := workflowkit.FingerprintParts("harbor.codeedge-phase1.command-output.v1", []workflowkit.FingerprintPart{
		{Name: "exit_code", Value: []byte(fmt.Sprintf("%d", result.ExitCode))},
		{Name: "stdout", Value: result.Stdout},
		{Name: "stderr", Value: result.Stderr},
	})
	if fingerprintErr != nil {
		return CodeEdgePhase1CommandResult{}, "", fingerprintErr
	}
	return result, fingerprint, runErr
}

func (runtime *lockedDockerRuntime) inspectImage(ctx context.Context, workspace, commandID, imageTag string) (string, CodeEdgePhase1CommandResult, workflowkit.Fingerprint, error) {
	result, fingerprint, err := runtime.run(ctx, commandID, []string{"image", "inspect", "--format", "{{.Id}}", imageTag}, workspace)
	if err != nil {
		return "", result, fingerprint, err
	}
	if result.ExitCode != 0 {
		return "", result, fingerprint, errors.New("controlled Docker image inspection failed")
	}
	imageID, err := codeEdgePhase1DockerImageID(result.Stdout)
	if err != nil {
		return "", result, fingerprint, err
	}
	return imageID, result, fingerprint, nil
}

func lockedDockerCommandEnvironment(workspace string) []string {
	return []string{
		"HOME=" + filepath.Join(workspace, "command-home"),
		"TMPDIR=" + filepath.Join(workspace, "command-tmp"),
		"LANG=C.UTF-8",
		"PATH=/nonexistent",
	}
}

func ensureLockedDockerCommandDirectory(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil || filepath.Clean(absolute) != path || absolute == string(os.PathSeparator) {
		return errors.New("locked Docker command directory is invalid")
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("locked Docker command directory is unavailable or unsafe")
	}
	return nil
}

func ensureLockedDockerPrivateDirectory(path string) error {
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("locked Docker private command directory is unavailable or unsafe")
	}
	return nil
}
