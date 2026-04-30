package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"time"
)

type Result struct {
	Command  string
	Stdout   string
	Stderr   string
	ExitCode int
	Err      error
	Timeout  bool
}

type Runner struct{}

func New() Runner {
	return Runner{}
}

func (Runner) LookPath(name string) (string, error) {
	return exec.LookPath(name)
}

func (Runner) Run(ctx context.Context, timeout time.Duration, dir string, env []string, name string, args ...string) Result {
	return runCommand(ctx, timeout, dir, env, nil, nil, name, args...)
}

func (Runner) RunStreaming(ctx context.Context, timeout time.Duration, dir string, env []string, writer io.Writer, name string, args ...string) Result {
	return runCommand(ctx, timeout, dir, env, nil, writer, name, args...)
}

func (Runner) RunWithInput(ctx context.Context, timeout time.Duration, dir string, env []string, input io.Reader, name string, args ...string) Result {
	return runCommand(ctx, timeout, dir, env, input, nil, name, args...)
}

func (Runner) RunWithInputStreaming(ctx context.Context, timeout time.Duration, dir string, env []string, input io.Reader, writer io.Writer, name string, args ...string) Result {
	return runCommand(ctx, timeout, dir, env, input, writer, name, args...)
}

func runCommand(ctx context.Context, timeout time.Duration, dir string, env []string, input io.Reader, writer io.Writer, name string, args ...string) Result {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, name, args...)
	prepareCommand(cmd)
	cmd.Cancel = func() error {
		return terminateCommand(cmd)
	}
	cmd.WaitDelay = 5 * time.Second
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = env
	}
	if input != nil {
		cmd.Stdin = input
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if writer != nil {
		cmd.Stdout = io.MultiWriter(&stdout, writer)
		cmd.Stderr = io.MultiWriter(&stderr, writer)
	} else {
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
	}
	err := cmd.Run()
	result := Result{
		Command: strings.Join(append([]string{name}, args...), " "),
		Stdout:  stdout.String(),
		Stderr:  stderr.String(),
		Err:     err,
	}
	if ctx.Err() != nil {
		result.Timeout = true
		result.Err = ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
	}
	return result
}
