package workflowkit

import (
	"fmt"
	"math"
	"time"
)

// BackoffPolicy contains one explicit delay before each retry. Keeping the
// delays explicit prevents a runtime default or an implementation-specific
// exponential formula from changing a frozen execution profile.
type BackoffPolicy struct {
	RetryDelays []time.Duration `json:"retry_delays"`
}

// Clone returns an independent copy of the policy.
func (p BackoffPolicy) Clone() BackoffPolicy {
	p.RetryDelays = append([]time.Duration(nil), p.RetryDelays...)
	return p
}

// Validate verifies that the policy gives one non-negative delay for every
// retry after the first attempt.
func (p BackoffPolicy) Validate(maxAttempts int) error {
	if maxAttempts < 1 {
		return fmt.Errorf("%w: max attempts must be at least one", ErrInvalidBudget)
	}
	want := maxAttempts - 1
	if len(p.RetryDelays) != want {
		return fmt.Errorf("%w: retry delays has length %d; want %d for %d attempts", ErrInvalidBudget, len(p.RetryDelays), want, maxAttempts)
	}
	for index, delay := range p.RetryDelays {
		if delay < 0 {
			return fmt.Errorf("%w: retry delay %d is negative", ErrInvalidBudget, index)
		}
	}
	return nil
}

// TotalDelay returns the worst-case total wait imposed by the retry policy.
func (p BackoffPolicy) TotalDelay(maxAttempts int) (time.Duration, error) {
	if err := p.Validate(maxAttempts); err != nil {
		return 0, err
	}
	var total time.Duration
	for _, delay := range p.RetryDelays {
		if delay > 0 && total > time.Duration(math.MaxInt64)-delay {
			return 0, fmt.Errorf("%w: retry delay total overflows duration", ErrInvalidBudget)
		}
		total += delay
	}
	return total, nil
}

// RetryDelayBefore returns the frozen delay before the supplied one-based
// attempt ordinal. Attempt one has no predecessor and therefore has zero
// delay. A caller cannot obtain an implicit exponential/default backoff from
// this helper.
func (p BackoffPolicy) RetryDelayBefore(attempt int) (time.Duration, error) {
	if attempt < 1 {
		return 0, fmt.Errorf("%w: retry attempt must be positive", ErrInvalidBudget)
	}
	if attempt == 1 {
		return 0, nil
	}
	index := attempt - 2
	if index >= len(p.RetryDelays) {
		return 0, fmt.Errorf("%w: retry delay is unavailable for attempt %d", ErrInvalidBudget, attempt)
	}
	delay := p.RetryDelays[index]
	if delay < 0 {
		return 0, fmt.Errorf("%w: retry delay %d is negative", ErrInvalidBudget, index)
	}
	return delay, nil
}

// ExecutionBudget is a fully resolved execution envelope. Zero IdleTimeout
// explicitly disables idle detection; all other limits required to admit work
// must be positive.
type ExecutionBudget struct {
	TurnTimeout    time.Duration `json:"turn_timeout"`
	MaxTurns       int           `json:"max_turns"`
	AttemptTimeout time.Duration `json:"attempt_timeout"`
	MaxAttempts    int           `json:"max_attempts"`
	MaxElapsed     time.Duration `json:"max_elapsed"`
	IdleTimeout    time.Duration `json:"idle_timeout"`
	StartupGrace   time.Duration `json:"startup_grace"`
	ShutdownGrace  time.Duration `json:"shutdown_grace"`
	Backoff        BackoffPolicy `json:"backoff"`
}

// Clone returns an independent copy of the budget.
func (b ExecutionBudget) Clone() ExecutionBudget {
	b.Backoff = b.Backoff.Clone()
	return b
}

// MinimumAttemptDuration returns the time needed to execute all turns and
// leave the configured startup and shutdown margins.
func (b ExecutionBudget) MinimumAttemptDuration() (time.Duration, error) {
	if b.MaxTurns < 1 || b.TurnTimeout <= 0 {
		return 0, fmt.Errorf("%w: turn timeout and max turns must be positive", ErrInvalidBudget)
	}
	if b.StartupGrace < 0 || b.ShutdownGrace < 0 {
		return 0, fmt.Errorf("%w: grace durations cannot be negative", ErrInvalidBudget)
	}
	turns, ok := multiplyDuration(b.TurnTimeout, b.MaxTurns)
	if !ok {
		return 0, fmt.Errorf("%w: turn budget overflows duration", ErrInvalidBudget)
	}
	minimum, ok := addDuration(turns, b.StartupGrace)
	if !ok {
		return 0, fmt.Errorf("%w: attempt budget overflows duration", ErrInvalidBudget)
	}
	minimum, ok = addDuration(minimum, b.ShutdownGrace)
	if !ok {
		return 0, fmt.Errorf("%w: attempt budget overflows duration", ErrInvalidBudget)
	}
	return minimum, nil
}

// MinimumElapsedDuration returns the time needed to spend every attempt plus
// every explicitly configured inter-attempt delay.
func (b ExecutionBudget) MinimumElapsedDuration() (time.Duration, error) {
	if b.MaxAttempts < 1 || b.AttemptTimeout <= 0 {
		return 0, fmt.Errorf("%w: attempt timeout and max attempts must be positive", ErrInvalidBudget)
	}
	attempts, ok := multiplyDuration(b.AttemptTimeout, b.MaxAttempts)
	if !ok {
		return 0, fmt.Errorf("%w: total attempt budget overflows duration", ErrInvalidBudget)
	}
	backoff, err := b.Backoff.TotalDelay(b.MaxAttempts)
	if err != nil {
		return 0, err
	}
	minimum, ok := addDuration(attempts, backoff)
	if !ok {
		return 0, fmt.Errorf("%w: elapsed budget overflows duration", ErrInvalidBudget)
	}
	return minimum, nil
}

// Validate proves the budget hierarchy before a workflow starts:
//
//	AttemptTimeout >= MaxTurns*TurnTimeout + StartupGrace + ShutdownGrace
//	MaxElapsed >= MaxAttempts*AttemptTimeout + all configured retry delays
func (b ExecutionBudget) Validate() error {
	if b.TurnTimeout <= 0 {
		return fmt.Errorf("%w: turn timeout must be positive", ErrInvalidBudget)
	}
	if b.MaxTurns < 1 {
		return fmt.Errorf("%w: max turns must be at least one", ErrInvalidBudget)
	}
	if b.AttemptTimeout <= 0 {
		return fmt.Errorf("%w: attempt timeout must be positive", ErrInvalidBudget)
	}
	if b.MaxAttempts < 1 {
		return fmt.Errorf("%w: max attempts must be at least one", ErrInvalidBudget)
	}
	if b.MaxElapsed <= 0 {
		return fmt.Errorf("%w: max elapsed must be positive", ErrInvalidBudget)
	}
	if b.IdleTimeout < 0 || b.StartupGrace < 0 || b.ShutdownGrace < 0 {
		return fmt.Errorf("%w: idle timeout and grace durations cannot be negative", ErrInvalidBudget)
	}
	minimumAttempt, err := b.MinimumAttemptDuration()
	if err != nil {
		return err
	}
	if b.AttemptTimeout < minimumAttempt {
		return fmt.Errorf("%w: attempt timeout %s is less than required %s", ErrInvalidBudget, b.AttemptTimeout, minimumAttempt)
	}
	minimumElapsed, err := b.MinimumElapsedDuration()
	if err != nil {
		return err
	}
	if b.MaxElapsed < minimumElapsed {
		return fmt.Errorf("%w: max elapsed %s is less than required %s", ErrInvalidBudget, b.MaxElapsed, minimumElapsed)
	}
	return nil
}

// ValidateNestedBudget verifies a child execution envelope can finish inside
// a caller-supplied remaining parent duration. The caller supplies remaining
// time deliberately; this helper does not guess whether a child is nested in
// a turn, an attempt, or an entire run.
func ValidateNestedBudget(parentRemaining time.Duration, child ExecutionBudget) error {
	if parentRemaining <= 0 {
		return fmt.Errorf("%w: parent remaining duration must be positive", ErrInvalidBudget)
	}
	if err := child.Validate(); err != nil {
		return err
	}
	if child.MaxElapsed > parentRemaining {
		return fmt.Errorf("%w: child max elapsed %s exceeds parent remaining duration %s", ErrInvalidBudget, child.MaxElapsed, parentRemaining)
	}
	return nil
}

func multiplyDuration(value time.Duration, count int) (time.Duration, bool) {
	if count < 0 {
		return 0, false
	}
	if count == 0 || value == 0 {
		return 0, true
	}
	if value > 0 && int64(value) > math.MaxInt64/int64(count) {
		return 0, false
	}
	if value < 0 && int64(value) < math.MinInt64/int64(count) {
		return 0, false
	}
	return value * time.Duration(count), true
}

func addDuration(left, right time.Duration) (time.Duration, bool) {
	if right > 0 && left > time.Duration(math.MaxInt64)-right {
		return 0, false
	}
	if right < 0 && left < time.Duration(math.MinInt64)-right {
		return 0, false
	}
	return left + right, true
}
