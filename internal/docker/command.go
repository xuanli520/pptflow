package docker

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/xuanli520/p2r_tui/internal/executor"
)

type ProgressEvent struct {
	Line   string
	Source string
	Done   bool
}

type RuntimeTimeouts struct {
	Pull   time.Duration
	Build  time.Duration
	Up     time.Duration
	Health time.Duration
	Port   time.Duration
}

type commandContext struct {
	Exec     executor.CommandRunner
	WorkDir  string
	Env      []string
	Log      io.Writer
	Progress func(ProgressEvent)
}

func (c commandContext) runStreaming(ctx context.Context, step string, timeout time.Duration, args []string, required bool) executor.Result {
	startLine := fmt.Sprintf("=== %s start ===", step)
	c.logLine(startLine, "p2r", false)
	if len(args) == 0 {
		c.logLine(step+" skipped", "p2r", false)
		c.logLine(fmt.Sprintf("=== %s end: skipped ===", step), "p2r", true)
		return executor.Result{}
	}
	onOutput := func(line string, source string) {
		if c.Progress != nil {
			c.Progress(ProgressEvent{Line: RedactLogText(line), Source: source})
		}
	}
	logWriter := newRedactingWriter(c.Log)
	result := c.Exec.RunStreamingWithOutput(ctx, timeout, c.WorkDir, c.Env, logWriter, onOutput, "docker", args...)
	if err := logWriter.Flush(); err != nil && result.Err == nil {
		result.Err = err
	}
	endLine := fmt.Sprintf("=== %s end: exit=%d timeout=%t err=%v ===", step, result.ExitCode, result.Timeout, result.Err)
	c.logLine("", "p2r", false)
	c.logLine(endLine, "p2r", true)
	return result
}

func (c commandContext) run(ctx context.Context, timeout time.Duration, args []string) executor.Result {
	return c.Exec.Run(ctx, timeout, c.WorkDir, c.Env, "docker", args...)
}

func (c commandContext) logLine(line, source string, done bool) {
	if c.Log != nil {
		if line == "" {
			_, _ = fmt.Fprintln(c.Log)
		} else {
			_, _ = fmt.Fprintln(c.Log, RedactLogText(line))
		}
	}
	if c.Progress != nil && line != "" {
		c.Progress(ProgressEvent{Line: RedactLogText(line), Source: source, Done: done})
	}
}
