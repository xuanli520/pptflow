package workflowadapter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// ParseExecutionProfileJSON decodes the user-facing persisted profile format.
// Durations are Go duration strings instead of encoding/json's raw nanosecond
// representation, making an explicit production profile reviewable and
// stable across CLI and TUI clients.
func ParseExecutionProfileJSON(raw []byte) (ExecutionProfile, error) {
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return ExecutionProfile{}, fmt.Errorf("decode execution profile: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var document executionProfileDocument
	if err := decoder.Decode(&document); err != nil {
		return ExecutionProfile{}, fmt.Errorf("decode execution profile: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return ExecutionProfile{}, fmt.Errorf("decode execution profile: unexpected trailing JSON value")
		}
		return ExecutionProfile{}, fmt.Errorf("decode execution profile trailing data: %w", err)
	}
	continuationPlanTTL, err := parseProfileDuration("continuation_plan_ttl", document.ContinuationPlanTTL)
	if err != nil {
		return ExecutionProfile{}, err
	}
	controlGracePeriod, err := parseProfileDuration("control_grace_period", document.ControlGracePeriod)
	if err != nil {
		return ExecutionProfile{}, err
	}
	profile := ExecutionProfile{
		Template:            document.Template,
		ID:                  document.ID,
		Version:             document.Version,
		ContinuationPlanTTL: continuationPlanTTL,
		ControlGracePeriod:  controlGracePeriod,
		Stages:              make([]StageBudget, 0, len(document.Stages)),
	}
	for _, stage := range document.Stages {
		budget, err := stage.Budget.resolve()
		if err != nil {
			return ExecutionProfile{}, fmt.Errorf("decode stage budget %q: %w", stage.StageKey, err)
		}
		profile.Stages = append(profile.Stages, StageBudget{StageKey: workflowkit.StageKey(stage.StageKey), Budget: budget})
	}
	template, err := ResolveWorkflowTemplate(profile.Template)
	if err != nil {
		return ExecutionProfile{}, fmt.Errorf("decode execution profile template: %w", err)
	}
	if err := profile.ValidateFor(template.Catalog); err != nil {
		return ExecutionProfile{}, err
	}
	return profile, nil
}

// CanonicalJSON returns the stable, human-readable persisted form of a
// validated execution profile. It is deliberately separate from Fingerprint:
// the latter retains its established hash representation while this form is
// used when a caller freezes explicit profile input into managed storage.
func (profile ExecutionProfile) CanonicalJSON() ([]byte, error) {
	if err := profile.Validate(); err != nil {
		return nil, err
	}
	canonical := profile.Clone()
	sort.Slice(canonical.Stages, func(left, right int) bool {
		return canonical.Stages[left].StageKey < canonical.Stages[right].StageKey
	})
	document := executionProfileDocument{
		Template:            canonical.Template,
		ID:                  canonical.ID,
		Version:             canonical.Version,
		ContinuationPlanTTL: canonical.ContinuationPlanTTL.String(),
		ControlGracePeriod:  canonical.ControlGracePeriod.String(),
		Stages:              make([]executionProfileStageEntry, 0, len(canonical.Stages)),
	}
	for _, stage := range canonical.Stages {
		budget := stage.Budget
		retryDelays := make([]string, len(budget.Backoff.RetryDelays))
		for index, delay := range budget.Backoff.RetryDelays {
			retryDelays[index] = delay.String()
		}
		document.Stages = append(document.Stages, executionProfileStageEntry{
			StageKey: string(stage.StageKey),
			Budget: executionProfileBudgetJSON{
				TurnTimeout:    budget.TurnTimeout.String(),
				MaxTurns:       budget.MaxTurns,
				AttemptTimeout: budget.AttemptTimeout.String(),
				MaxAttempts:    budget.MaxAttempts,
				MaxElapsed:     budget.MaxElapsed.String(),
				IdleTimeout:    budget.IdleTimeout.String(),
				StartupGrace:   budget.StartupGrace.String(),
				ShutdownGrace:  budget.ShutdownGrace.String(),
				Backoff:        executionProfileBackoffJSON{RetryDelays: retryDelays},
			},
		})
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode canonical execution profile: %w", err)
	}
	return encoded, nil
}

type executionProfileDocument struct {
	Template            TemplateReference            `json:"template"`
	ID                  string                       `json:"id"`
	Version             string                       `json:"version"`
	ContinuationPlanTTL string                       `json:"continuation_plan_ttl"`
	ControlGracePeriod  string                       `json:"control_grace_period"`
	Stages              []executionProfileStageEntry `json:"stages"`
}

type executionProfileStageEntry struct {
	StageKey string                     `json:"stage_key"`
	Budget   executionProfileBudgetJSON `json:"budget"`
}

type executionProfileBudgetJSON struct {
	TurnTimeout    string                      `json:"turn_timeout"`
	MaxTurns       int                         `json:"max_turns"`
	AttemptTimeout string                      `json:"attempt_timeout"`
	MaxAttempts    int                         `json:"max_attempts"`
	MaxElapsed     string                      `json:"max_elapsed"`
	IdleTimeout    string                      `json:"idle_timeout"`
	StartupGrace   string                      `json:"startup_grace"`
	ShutdownGrace  string                      `json:"shutdown_grace"`
	Backoff        executionProfileBackoffJSON `json:"backoff"`
}

type executionProfileBackoffJSON struct {
	RetryDelays []string `json:"retry_delays"`
}

func (budget executionProfileBudgetJSON) resolve() (workflowkit.ExecutionBudget, error) {
	turnTimeout, err := parseProfileDuration("turn_timeout", budget.TurnTimeout)
	if err != nil {
		return workflowkit.ExecutionBudget{}, err
	}
	attemptTimeout, err := parseProfileDuration("attempt_timeout", budget.AttemptTimeout)
	if err != nil {
		return workflowkit.ExecutionBudget{}, err
	}
	maxElapsed, err := parseProfileDuration("max_elapsed", budget.MaxElapsed)
	if err != nil {
		return workflowkit.ExecutionBudget{}, err
	}
	idleTimeout, err := parseProfileDuration("idle_timeout", budget.IdleTimeout)
	if err != nil {
		return workflowkit.ExecutionBudget{}, err
	}
	startupGrace, err := parseProfileDuration("startup_grace", budget.StartupGrace)
	if err != nil {
		return workflowkit.ExecutionBudget{}, err
	}
	shutdownGrace, err := parseProfileDuration("shutdown_grace", budget.ShutdownGrace)
	if err != nil {
		return workflowkit.ExecutionBudget{}, err
	}
	retryDelays := make([]time.Duration, len(budget.Backoff.RetryDelays))
	for index, value := range budget.Backoff.RetryDelays {
		delay, err := parseProfileDuration(fmt.Sprintf("backoff.retry_delays[%d]", index), value)
		if err != nil {
			return workflowkit.ExecutionBudget{}, err
		}
		retryDelays[index] = delay
	}
	return workflowkit.ExecutionBudget{
		TurnTimeout:    turnTimeout,
		MaxTurns:       budget.MaxTurns,
		AttemptTimeout: attemptTimeout,
		MaxAttempts:    budget.MaxAttempts,
		MaxElapsed:     maxElapsed,
		IdleTimeout:    idleTimeout,
		StartupGrace:   startupGrace,
		ShutdownGrace:  shutdownGrace,
		Backoff:        workflowkit.BackoffPolicy{RetryDelays: retryDelays},
	}, nil
}

func parseProfileDuration(field, value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, fmt.Errorf("%s is required", field)
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration: %w", field, err)
	}
	if duration < 0 {
		return 0, fmt.Errorf("%s cannot be negative", field)
	}
	return duration, nil
}
