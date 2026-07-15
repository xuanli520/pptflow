package workflowkit

import (
	"testing"
	"time"
)

func TestDecideStageRetryUsesOnlyFrozenInfrastructurePolicy(t *testing.T) {
	stage := testStage("retry", nil, EffectEvidenceOnly, nil, []ResourceKey{"evidence/retry"})
	stage.Retry = RetryPolicy{Retryable: []FailureClass{FailureNetwork, FailureTimeout}}
	stage.Budget.MaxAttempts = 3
	stage.Budget.Backoff = BackoffPolicy{RetryDelays: []time.Duration{2 * time.Second, 5 * time.Second}}
	stage.Budget.MaxElapsed = 3*stage.Budget.AttemptTimeout + 7*time.Second
	if err := stage.Validate(); err != nil {
		t.Fatalf("validate retry stage: %v", err)
	}

	first, err := DecideStageRetry(stage, 1, Outcome{Status: StatusInfraFailed, Failure: FailureNetwork})
	if err != nil {
		t.Fatalf("decide first retry: %v", err)
	}
	if !first.Retry || first.NextAttempt != 2 || first.Delay != 2*time.Second {
		t.Fatalf("first retry = %+v", first)
	}
	second, err := DecideStageRetry(stage, 2, Outcome{Status: StatusInfraFailed, Failure: FailureTimeout})
	if err != nil {
		t.Fatalf("decide second retry: %v", err)
	}
	if !second.Retry || second.NextAttempt != 3 || second.Delay != 5*time.Second {
		t.Fatalf("second retry = %+v", second)
	}
	for _, outcome := range []Outcome{
		{Status: StatusCompleted, Verdict: VerdictPass},
		{Status: StatusInfraFailed, Failure: FailurePolicy},
		{Status: StatusInterrupted},
		{Status: StatusCanceled},
	} {
		decision, decisionErr := DecideStageRetry(stage, 1, outcome)
		if decisionErr != nil || decision.Retry {
			t.Fatalf("outcome %+v retry = %+v, %v; want no retry", outcome, decision, decisionErr)
		}
	}
	last, err := DecideStageRetry(stage, 3, Outcome{Status: StatusInfraFailed, Failure: FailureNetwork})
	if err != nil || last.Retry {
		t.Fatalf("last retry = %+v, %v; want exhausted no retry", last, err)
	}
}

func TestDecideStageRetryRejectsMalformedAttemptOrOutcome(t *testing.T) {
	stage := testStage("retry", nil, EffectEvidenceOnly, nil, []ResourceKey{"evidence/retry"})
	if _, err := DecideStageRetry(stage, 0, Outcome{Status: StatusInfraFailed, Failure: FailureNetwork}); err == nil {
		t.Fatal("zero completed attempt was accepted")
	}
	if _, err := DecideStageRetry(stage, 1, Outcome{Status: StatusCompleted}); err == nil {
		t.Fatal("malformed completed outcome was accepted")
	}
}
