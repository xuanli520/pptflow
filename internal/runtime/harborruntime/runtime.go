package harborruntime

import (
	"context"

	"github.com/purplevoid/harbor-factory/internal/executor"
	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/harborrun"
	"github.com/purplevoid/harbor-factory/internal/workflow"
)

type RunFunc func(context.Context, harborrun.Options) (domain.TrialResult, domain.CommandRun, error)

type Runtime struct {
	Exec      executor.CommandRunner
	CacheExec executor.CommandRunner
	Run       RunFunc
}

func New(exec, cacheExec executor.CommandRunner) Runtime {
	return Runtime{Exec: exec, CacheExec: cacheExec}
}

func (r Runtime) Evaluate(ctx context.Context, request workflow.EvaluationRequest) (workflow.EvaluationResult, error) {
	run := r.Run
	if run == nil {
		run = harborrun.Run
	}
	trial, command, err := run(ctx, harborrun.Options{
		TaskPath:            request.TaskPath,
		Model:               request.Model,
		Agent:               request.Agent,
		AgentEnv:            append([]string(nil), request.AgentEnv...),
		OutputDir:           request.OutputDir,
		TimeoutSeconds:      request.TimeoutSeconds,
		SetupTimeoutSeconds: request.SetupTimeoutSeconds,
		AgentCacheDir:       request.AgentCacheDir,
		Preflight:           request.Preflight,
		Concurrency:         request.Concurrency,
		Attempts:            request.Attempts,
		InfraRetries:        request.InfraRetries,
		Progress:            request.Progress,
		Env:                 append([]string(nil), request.Env...),
		Exec:                r.Exec,
		CacheExec:           r.CacheExec,
	})
	return workflow.EvaluationResult{
		TrialResult: trialToWorkflow(trial),
		CommandRun:  commandToWorkflow(command),
	}, err
}

func trialToWorkflow(input domain.TrialResult) workflow.EvaluationTrialResult {
	runs := make([]workflow.EvaluationTrialRun, 0, len(input.Runs))
	for _, run := range input.Runs {
		runs = append(runs, workflow.EvaluationTrialRun{
			Trial: run.Trial, Passed: run.Passed, Turns: run.Turns,
			DurationSeconds: run.DurationSeconds, Reward: run.Reward, FailureReason: run.FailureReason,
		})
	}
	raw := make([]workflow.EvaluationFileEvidence, 0, len(input.RawTrialResults))
	for _, evidence := range input.RawTrialResults {
		raw = append(raw, workflow.EvaluationFileEvidence{Path: evidence.Path, SHA256: evidence.SHA256})
	}
	return workflow.EvaluationTrialResult{
		SchemaVersion: input.SchemaVersion, Model: input.Model, Agent: input.Agent,
		Trials: input.Trials, PassCount: input.PassCount, PassAt4: input.PassAt4,
		AverageTurns: input.AverageTurns, Runs: runs, ResultPath: input.ResultPath,
		RawResultPath: input.RawResultPath, RawResultSHA256: input.RawResultSHA256,
		RawTrialResults: raw, TaskDigest: input.TaskDigest, HarborTaskChecksum: input.HarborTaskChecksum,
		TaskPath: input.TaskPath, CommandRunPath: input.CommandRunPath,
		SchemaPreflightPath: input.SchemaPreflightPath, PreflightRunPath: input.PreflightRunPath,
		PreflightResultPath: input.PreflightResultPath, AgentCacheManifest: input.AgentCacheManifest,
		RetryEvidence: input.RetryEvidence, Screenshot: input.Screenshot, CreatedAt: input.CreatedAt,
	}
}

func commandToWorkflow(input domain.CommandRun) workflow.CommandEvidence {
	return workflow.CommandEvidence{
		Name: input.Name, Command: input.Command, Argv: append([]string(nil), input.Argv...),
		Dir: input.Dir, Env: append([]string(nil), input.Env...), Attempt: input.Attempt,
		ExitCode: input.ExitCode, Stdout: input.Stdout, Stderr: input.Stderr,
		StdoutPath: input.StdoutPath, StderrPath: input.StderrPath, Timeout: input.Timeout,
		FailureClass: input.FailureClass, StartedAt: input.StartedAt, FinishedAt: input.FinishedAt,
		DurationMS: input.DurationMS, Passed: input.Passed,
	}
}

var _ workflow.EvaluationRuntime = Runtime{}
