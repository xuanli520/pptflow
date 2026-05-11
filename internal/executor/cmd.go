package executor

import (
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

type commandStreamWriter struct {
	source     string
	output     *bytes.Buffer
	writer     io.Writer
	writerMu   *sync.Mutex
	dispatcher *outputDispatcher

	mu      sync.Mutex
	pending []byte
}

func newCommandStreamWriter(source string, output *bytes.Buffer, writer io.Writer, writerMu *sync.Mutex, dispatcher *outputDispatcher) *commandStreamWriter {
	return &commandStreamWriter{
		source:     source,
		output:     output,
		writer:     writer,
		writerMu:   writerMu,
		dispatcher: dispatcher,
	}
}

func (w *commandStreamWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.output != nil {
		_, _ = w.output.Write(p)
	}
	w.emitLinesLocked(p)

	if w.writer == nil {
		return len(p), nil
	}
	w.writerMu.Lock()
	n, err := w.writer.Write(p)
	w.writerMu.Unlock()
	if err != nil {
		return n, err
	}
	if n != len(p) {
		return n, io.ErrShortWrite
	}
	return len(p), nil
}

func (w *commandStreamWriter) emitLinesLocked(p []byte) {
	w.pending = append(w.pending, p...)
	for {
		index := bytes.IndexByte(w.pending, '\n')
		if index < 0 {
			return
		}
		line := string(w.pending[:index+1])
		rest := append([]byte(nil), w.pending[index+1:]...)
		w.pending = rest
		w.dispatcher.emit(outputEvent{line: line, source: w.source})
	}
}

func (w *commandStreamWriter) flushPending() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.pending) == 0 {
		return
	}
	line := string(w.pending)
	w.pending = nil
	w.dispatcher.emit(outputEvent{line: line, source: w.source})
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

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	var writerMu sync.Mutex
	dispatcher := newOutputDispatcher(onOutput)
	stdoutWriter := newCommandStreamWriter("stdout", &stdout, writer, &writerMu, dispatcher)
	stderrWriter := newCommandStreamWriter("stderr", &stderr, writer, &writerMu, dispatcher)
	cmd.Stdout = stdoutWriter
	cmd.Stderr = stderrWriter

	err := cmd.Run()
	waitCtxErr := ctx.Err()
	stdoutWriter.flushPending()
	stderrWriter.flushPending()
	dispatcher.closeAndWait()

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
