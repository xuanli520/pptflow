package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const lockedDockerCommandOutputLimit = 128 << 10

// lockedDockerCommand is a direct, already-resolved process invocation. It
// deliberately has no shell text or inherited environment surface.
type lockedDockerCommand struct {
	Path string
	Args []string
	Dir  string
	Env  []string
}

// lockedDockerCommandResult preserves only bounded process output for a
// transient in-memory classification. The executor writes fingerprints, not
// raw output, into durable task evidence.
type lockedDockerCommandResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

// lockedDockerCommandRunner is the narrow local process boundary. Production
// uses lockedDockerDirectCommandRunner; tests can provide a deterministic
// runner without requiring Docker.
type lockedDockerCommandRunner interface {
	Run(context.Context, lockedDockerCommand) (lockedDockerCommandResult, error)
}

// lockedDockerDirectCommandRunner invokes a locked executable using direct
// argv. It never invokes a shell or consults PATH to resolve the executable.
type lockedDockerDirectCommandRunner struct{}

func (lockedDockerDirectCommandRunner) Run(ctx context.Context, command lockedDockerCommand) (lockedDockerCommandResult, error) {
	if ctx == nil {
		return lockedDockerCommandResult{}, errors.New("locked Docker command context is required")
	}
	process := exec.CommandContext(ctx, command.Path, command.Args...)
	process.Dir = command.Dir
	process.Env = append([]string(nil), command.Env...)
	stdout := &lockedDockerLimitedBuffer{limit: lockedDockerCommandOutputLimit}
	stderr := &lockedDockerLimitedBuffer{limit: lockedDockerCommandOutputLimit}
	process.Stdout = stdout
	process.Stderr = stderr
	err := process.Run()
	result := lockedDockerCommandResult{
		ExitCode: 0,
		Stdout:   append([]byte(nil), stdout.Bytes()...),
		Stderr:   append([]byte(nil), stderr.Bytes()...),
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		result.ExitCode = exitError.ExitCode()
		return result, nil
	}
	if err != nil {
		return result, err
	}
	return result, nil
}

type lockedDockerLimitedBuffer struct {
	limit int
	bytes.Buffer
}

func (buffer *lockedDockerLimitedBuffer) Write(value []byte) (int, error) {
	if buffer == nil || buffer.limit < 0 || buffer.Len()+len(value) > buffer.limit {
		return 0, errors.New("locked Docker command output exceeds the configured limit")
	}
	return buffer.Buffer.Write(value)
}

type lockedDockerExecutableAttestor func(context.Context, stageprovider.LocalExecutableLock) error

// lockedDockerRuntime is the shared host process boundary used by Standard
// authoring. A caller selects only one of the three deployment-lock command
// IDs and supplies direct Docker argv; executable path, environment, output
// bound and timeout remain host-owned.
type lockedDockerRuntime struct {
	commands map[string]stageprovider.LocalExecutableLock
	runner   lockedDockerCommandRunner
	timeout  time.Duration
	attest   lockedDockerExecutableAttestor
}

func newLockedDockerRuntime(commands map[string]stageprovider.LocalExecutableLock, runner lockedDockerCommandRunner, timeout time.Duration, attestor lockedDockerExecutableAttestor) (*lockedDockerRuntime, error) {
	if len(commands) != 3 {
		return nil, errors.New("locked Docker runtime requires exactly three command locks")
	}
	cloned := make(map[string]stageprovider.LocalExecutableLock, len(commands))
	for _, commandID := range []string{
		stageprovider.StandardAuthoringDockerBuildCommandID,
		stageprovider.StandardAuthoringInitialVerifyCommandID,
		stageprovider.StandardAuthoringOracleVerifyCommandID,
	} {
		lock, found := commands[commandID]
		if !found || lock.CommandID != commandID {
			return nil, fmt.Errorf("locked Docker runtime command %q is unavailable", commandID)
		}
		cloned[commandID] = lock
	}
	if runner == nil {
		runner = lockedDockerDirectCommandRunner{}
	}
	if timeout <= 0 {
		timeout = 20 * time.Minute
	}
	return &lockedDockerRuntime{commands: cloned, runner: runner, timeout: timeout, attest: attestor}, nil
}

// standardAuthoringLockedCommands validates the three Standard authoring
// Docker command locks supplied by the deployment boundary.
func standardAuthoringLockedCommands(locks []stageprovider.LocalExecutableLock) (map[string]stageprovider.LocalExecutableLock, error) {
	if len(locks) != 3 {
		return nil, errors.New("Standard authoring candidate harness requires exactly three locked Docker commands")
	}
	commands := make(map[string]stageprovider.LocalExecutableLock, len(locks))
	for _, lock := range locks {
		switch lock.CommandID {
		case stageprovider.StandardAuthoringDockerBuildCommandID,
			stageprovider.StandardAuthoringInitialVerifyCommandID,
			stageprovider.StandardAuthoringOracleVerifyCommandID:
		default:
			return nil, fmt.Errorf("Standard authoring candidate harness does not authorize local command %q", lock.CommandID)
		}
		if lock.CommandID == "" || strings.TrimSpace(lock.Version) == "" || !filepath.IsAbs(lock.AbsolutePath) ||
			filepath.Clean(lock.AbsolutePath) != lock.AbsolutePath || lock.AbsolutePath == string(os.PathSeparator) {
			return nil, errors.New("Standard authoring local executable lock is incomplete")
		}
		if err := lock.ContentSHA256.Validate(); err != nil {
			return nil, fmt.Errorf("Standard authoring local executable fingerprint: %w", err)
		}
		if _, duplicate := commands[lock.CommandID]; duplicate {
			return nil, fmt.Errorf("Standard authoring local command %q is duplicated", lock.CommandID)
		}
		commands[lock.CommandID] = lock
	}
	for _, commandID := range []string{
		stageprovider.StandardAuthoringDockerBuildCommandID,
		stageprovider.StandardAuthoringInitialVerifyCommandID,
		stageprovider.StandardAuthoringOracleVerifyCommandID,
	} {
		if _, found := commands[commandID]; !found {
			return nil, fmt.Errorf("Standard authoring local command %q is missing", commandID)
		}
	}
	return commands, nil
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

func (runtime *lockedDockerRuntime) run(ctx context.Context, commandID string, args []string, directory string) (lockedDockerCommandResult, workflowkit.Fingerprint, error) {
	if ctx == nil {
		return lockedDockerCommandResult{}, "", errors.New("locked Docker command context is required")
	}
	if err := ctx.Err(); err != nil {
		return lockedDockerCommandResult{}, "", err
	}
	lock, err := runtime.command(commandID)
	if err != nil {
		return lockedDockerCommandResult{}, "", err
	}
	if runtime.attest != nil {
		if err := runtime.attest(ctx, lock); err != nil {
			return lockedDockerCommandResult{}, "", fmt.Errorf("attest locked Docker executable: %w", err)
		}
	}
	if err := ensureLockedDockerCommandDirectory(directory); err != nil {
		return lockedDockerCommandResult{}, "", err
	}
	for _, name := range []string{"command-home", "command-tmp"} {
		if err := ensureLockedDockerPrivateDirectory(filepath.Join(directory, name)); err != nil {
			return lockedDockerCommandResult{}, "", err
		}
	}
	command := lockedDockerCommand{
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
	fingerprint, fingerprintErr := workflowkit.FingerprintParts("harbor.standard-authoring.command-output.v1", []workflowkit.FingerprintPart{
		{Name: "exit_code", Value: []byte(fmt.Sprintf("%d", result.ExitCode))},
		{Name: "stdout", Value: result.Stdout},
		{Name: "stderr", Value: result.Stderr},
	})
	if fingerprintErr != nil {
		return lockedDockerCommandResult{}, "", fingerprintErr
	}
	return result, fingerprint, runErr
}

func (runtime *lockedDockerRuntime) inspectImage(ctx context.Context, workspace, commandID, imageTag string) (string, lockedDockerCommandResult, workflowkit.Fingerprint, error) {
	result, fingerprint, err := runtime.run(ctx, commandID, []string{"image", "inspect", "--format", "{{.Id}}", imageTag}, workspace)
	if err != nil {
		return "", result, fingerprint, err
	}
	if result.ExitCode != 0 {
		return "", result, fingerprint, errors.New("controlled Docker image inspection failed")
	}
	imageID, err := standardAuthoringDockerImageID(result.Stdout)
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
