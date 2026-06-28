package command

import (
	"context"
	"time"

	"github.com/xuanli520/pptflow/internal/executor"
	"github.com/xuanli520/pptflow/internal/workflow"
)

type Runtime struct {
	exec executor.CommandRunner
}

func New(exec executor.CommandRunner) Runtime {
	if exec == nil {
		exec = executor.New()
	}
	return Runtime{exec: exec}
}

func (r Runtime) Run(ctx context.Context, req workflow.CommandRequest) (workflow.CommandResult, error) {
	timeout := time.Duration(req.TimeoutSeconds) * time.Second
	result := r.exec.Run(ctx, timeout, req.Dir, req.Env, req.Command, req.Args...)
	return workflow.CommandResult{
		Command:  result.Command,
		Stdout:   result.Stdout,
		Stderr:   result.Stderr,
		ExitCode: result.ExitCode,
		Timeout:  result.Timeout,
	}, result.Err
}
