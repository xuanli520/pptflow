package workflowkit

import (
	"fmt"
	"time"
)

// RetryDecision is the complete, immutable-kernel answer after one terminal
// node attempt. Domain adapters persist attempts and perform the wait, but do
// not invent retryability, ordinal, or backoff semantics themselves.
type RetryDecision struct {
	Retry       bool          `json:"retry"`
	NextAttempt int           `json:"next_attempt,omitempty"`
	Delay       time.Duration `json:"delay,omitempty"`
}

// DecideStageRetry applies the retry policy already frozen in a stage
// descriptor. Only classified infrastructure failures can be retried; content
// verdicts, interruption, cancellation, and unknown side effects are never
// converted into another attempt by this generic rule.
func DecideStageRetry(stage StageDescriptor, completedAttempt int, outcome Outcome) (RetryDecision, error) {
	if err := stage.Validate(); err != nil {
		return RetryDecision{}, fmt.Errorf("%w: retry stage: %v", ErrInvalidExecution, err)
	}
	if completedAttempt < 1 || completedAttempt > stage.Budget.MaxAttempts {
		return RetryDecision{}, fmt.Errorf("%w: completed attempt %d is outside stage %q budget 1..%d", ErrInvalidExecution, completedAttempt, stage.Key, stage.Budget.MaxAttempts)
	}
	if err := outcome.Validate(); err != nil {
		return RetryDecision{}, fmt.Errorf("%w: retry outcome: %v", ErrInvalidExecution, err)
	}
	if outcome.Status != StatusInfraFailed || !stage.Retry.Allows(outcome.Failure) || completedAttempt >= stage.Budget.MaxAttempts {
		return RetryDecision{}, nil
	}
	nextAttempt := completedAttempt + 1
	delay, err := stage.Budget.Backoff.RetryDelayBefore(nextAttempt)
	if err != nil {
		return RetryDecision{}, fmt.Errorf("%w: retry backoff for stage %q: %v", ErrInvalidExecution, stage.Key, err)
	}
	return RetryDecision{Retry: true, NextAttempt: nextAttempt, Delay: delay}, nil
}
