package harborruntime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/harborrun"
	"github.com/purplevoid/harbor-factory/internal/workflow"
)

func TestEvaluateMapsRequestAndPreservesPartialEvidence(t *testing.T) {
	wantErr := errors.New("partial Harbor failure")
	runtime := Runtime{Run: func(_ context.Context, opts harborrun.Options) (domain.TrialResult, domain.CommandRun, error) {
		if opts.TaskPath != "/task" || opts.Model != "qwen-model" || opts.Attempts != 4 || opts.Concurrency != 2 {
			t.Fatalf("evaluation request was not mapped: %+v", opts)
		}
		return domain.TrialResult{
			SchemaVersion: "harbor.trial_result.v1", Model: opts.Model, Trials: 4,
			Runs: []domain.TrialRun{{Trial: 1, Turns: 9}}, RawTrialResults: []domain.ResultFileEvidence{{Path: "/raw/1.json", SHA256: "abc"}},
		}, domain.CommandRun{Name: "harbor_run", ExitCode: -1, Timeout: true, StartedAt: time.Now().UTC()}, wantErr
	}}
	result, err := runtime.Evaluate(context.Background(), workflow.EvaluationRequest{
		TaskPath: "/task", Model: "qwen-model", Attempts: 4, Concurrency: 2, AgentEnv: []string{"TOKEN=${TOKEN}"},
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected runtime error, got %v", err)
	}
	if result.TrialResult.Model != "qwen-model" || len(result.TrialResult.Runs) != 1 || len(result.TrialResult.RawTrialResults) != 1 {
		t.Fatalf("partial trial evidence was lost: %+v", result.TrialResult)
	}
	if !result.CommandRun.Timeout || result.CommandRun.ExitCode != -1 {
		t.Fatalf("partial command evidence was lost: %+v", result.CommandRun)
	}
}

var _ workflow.EvaluationRuntime = Runtime{}
