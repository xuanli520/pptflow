package executor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"sync"
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

type CommandRunner interface {
	LookPath(name string) (string, error)
	Run(ctx context.Context, timeout time.Duration, dir string, env []string, name string, args ...string) Result
	RunStreamingWithOutput(ctx context.Context, timeout time.Duration, dir string, env []string, writer io.Writer, onOutput OutputCallback, name string, args ...string) Result
}

type OutputCallback func(line string, source string)

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

func (Runner) RunStreamingWithOutput(ctx context.Context, timeout time.Duration, dir string, env []string, writer io.Writer, onOutput OutputCallback, name string, args ...string) Result {
	return runCommandStreamingWithOutput(ctx, timeout, dir, env, writer, onOutput, name, args...)
}

func (Runner) RunWithInput(ctx context.Context, timeout time.Duration, dir string, env []string, input io.Reader, name string, args ...string) Result {
	return runCommand(ctx, timeout, dir, env, input, nil, name, args...)
}

func (Runner) RunWithInputStreaming(ctx context.Context, timeout time.Duration, dir string, env []string, input io.Reader, writer io.Writer, name string, args ...string) Result {
	return runCommand(ctx, timeout, dir, env, input, writer, name, args...)
}

func ConfigureCommand(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	prepareCommand(cmd)
	cmd.Cancel = func() error {
		return terminateCommand(cmd)
	}
	cmd.WaitDelay = 5 * time.Second
}

func runCommand(ctx context.Context, timeout time.Duration, dir string, env []string, input io.Reader, writer io.Writer, name string, args ...string) Result {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, name, args...)
	ConfigureCommand(cmd)
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
	if ctxErr := ctx.Err(); ctxErr != nil {
		result.Timeout = errors.Is(ctxErr, context.DeadlineExceeded)
		result.Err = ctxErr
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
	}
	return result
}

type outputEvent struct {
	line   string
	source string
}

type outputDispatcher struct {
	onOutput OutputCallback
	mu       sync.Mutex
	cond     *sync.Cond
	queue    []outputEvent
	closed   bool
	wg       sync.WaitGroup
}

func newOutputDispatcher(onOutput OutputCallback) *outputDispatcher {
	if onOutput == nil {
		return nil
	}
	dispatcher := &outputDispatcher{onOutput: onOutput}
	dispatcher.cond = sync.NewCond(&dispatcher.mu)
	dispatcher.wg.Add(1)
	go dispatcher.run()
	return dispatcher
}

func (d *outputDispatcher) emit(event outputEvent) {
	if d == nil {
		return
	}
	d.mu.Lock()
	if !d.closed {
		d.queue = append(d.queue, event)
		d.cond.Signal()
	}
	d.mu.Unlock()
}

func (d *outputDispatcher) closeAndWait() {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.closed = true
	d.cond.Signal()
	d.mu.Unlock()
	d.wg.Wait()
}

func (d *outputDispatcher) run() {
	defer d.wg.Done()
	for {
		d.mu.Lock()
		for len(d.queue) == 0 && !d.closed {
			d.cond.Wait()
		}
		if len(d.queue) == 0 && d.closed {
			d.mu.Unlock()
			return
		}
		event := d.queue[0]
		d.queue[0] = outputEvent{}
		d.queue = d.queue[1:]
		d.mu.Unlock()
		d.onOutput(event.line, event.source)
	}
}

func runCommandStreamingWithOutput(ctx context.Context, timeout time.Duration, dir string, env []string, writer io.Writer, onOutput OutputCallback, name string, args ...string) Result {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	command := strings.Join(append([]string{name}, args...), " ")
	cmd := exec.CommandContext(ctx, name, args...)
	ConfigureCommand(cmd)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = env
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return Result{Command: command, Err: err, Stderr: err.Error()}
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return Result{Command: command, Err: err, Stderr: err.Error()}
	}
	if err := cmd.Start(); err != nil {
		return Result{Command: command, Err: err, Stderr: err.Error()}
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	var writerMu sync.Mutex
	var readMu sync.Mutex
	var readErr error
	setReadErr := func(err error) {
		if err == nil {
			return
		}
		readMu.Lock()
		if readErr == nil {
			readErr = err
		}
		readMu.Unlock()
	}

	var readWG sync.WaitGroup
	readWG.Add(2)
	dispatcher := newOutputDispatcher(onOutput)
	go func() {
		defer readWG.Done()
		setReadErr(copyCommandStream(stdoutPipe, "stdout", &stdout, writer, &writerMu, dispatcher))
	}()
	go func() {
		defer readWG.Done()
		setReadErr(copyCommandStream(stderrPipe, "stderr", &stderr, writer, &writerMu, dispatcher))
	}()

	err = cmd.Wait()
	waitCtxErr := ctx.Err()
	readWG.Wait()
	dispatcher.closeAndWait()

	readMu.Lock()
	if err == nil {
		err = readErr
	}
	readMu.Unlock()

	result := Result{
		Command: command,
		Stdout:  stdout.String(),
		Stderr:  stderr.String(),
		Err:     err,
	}
	if ctxErr := waitCtxErr; ctxErr != nil {
		result.Timeout = errors.Is(ctxErr, context.DeadlineExceeded)
		result.Err = ctxErr
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
	}
	return result
}

func copyCommandStream(reader io.Reader, source string, output *bytes.Buffer, writer io.Writer, writerMu *sync.Mutex, dispatcher *outputDispatcher) error {
	buf := bufio.NewReader(reader)
	for {
		line, err := buf.ReadString('\n')
		if len(line) > 0 {
			output.WriteString(line)
			if writer != nil {
				writerMu.Lock()
				_, _ = writer.Write([]byte(line))
				writerMu.Unlock()
			}
			dispatcher.emit(outputEvent{line: line, source: source})
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}
