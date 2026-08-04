package workflowadapter

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestParseExecutionProfileJSONUsesReadableDurationsAndRequiresCompleteCatalogAtCompile(t *testing.T) {
	raw := marshalCompleteExecutionProfileDocument(t, completeExecutionProfileDocument())
	profile, err := ParseExecutionProfileJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	catalog := StandardStageCatalog()
	if profile.ID != "explicit" || profile.ContinuationPlanTTL != RequiredContinuationPlanTTL || profile.ControlGracePeriod != 30*time.Second || len(profile.Stages) != len(catalog.Stages) {
		t.Fatalf("unexpected parsed profile: %+v", profile)
	}
	if profile.CandidateProviderBudget != (CandidateProviderBudget{AttemptTimeout: 10 * time.Second, StartupGrace: 2 * time.Second, ShutdownGrace: 3 * time.Second}) {
		t.Fatalf("candidate provider budget = %+v", profile.CandidateProviderBudget)
	}
	if _, err := StandardWorkflowTemplate().Compile(profile); err != nil {
		t.Fatalf("parsed complete profile did not compile: %v", err)
	}

	canonical, err := profile.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var canonicalDocument map[string]any
	if err := json.Unmarshal(canonical, &canonicalDocument); err != nil {
		t.Fatal(err)
	}
	candidate, ok := canonicalDocument["candidate_provider_budget"].(map[string]any)
	if !ok {
		t.Fatalf("canonical profile candidate provider budget = %#v", canonicalDocument["candidate_provider_budget"])
	}
	if candidate["attempt_timeout"] != "10s" || candidate["startup_grace"] != "2s" || candidate["shutdown_grace"] != "3s" {
		t.Fatalf("canonical candidate provider budget = %#v", candidate)
	}
	if _, present := candidate["timeout"]; present {
		t.Fatalf("canonical candidate provider budget retained legacy timeout field: %#v", candidate)
	}
	if _, present := candidate["lease_ttl"]; present {
		t.Fatalf("canonical candidate provider budget retained legacy lease TTL field: %#v", candidate)
	}
}

func TestParseExecutionProfileJSONRejectsAmbiguousOrMalformedInput(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(map[string]any)
		trailing   string
		errorMatch string
	}{
		{
			name:       "unknown root field",
			mutate:     func(document map[string]any) { document["unknown"] = true },
			errorMatch: "unknown field",
		},
		{
			name: "malformed stage duration",
			mutate: func(document map[string]any) {
				stages := document["stages"].([]any)
				budget := stages[0].(map[string]any)["budget"].(map[string]any)
				budget["turn_timeout"] = "not-a-duration"
			},
			errorMatch: "turn_timeout must be a duration",
		},
		{
			name:       "trailing JSON value",
			mutate:     func(map[string]any) {},
			trailing:   " {}",
			errorMatch: "unexpected trailing JSON value",
		},
		{
			name:       "continuation TTL over limit",
			mutate:     func(document map[string]any) { document["continuation_plan_ttl"] = "168h1m0s" },
			errorMatch: "continuation plan TTL",
		},
		{
			name:       "missing control grace",
			mutate:     func(document map[string]any) { delete(document, "control_grace_period") },
			errorMatch: "control_grace_period is required",
		},
		{
			name:       "missing candidate policy",
			mutate:     func(document map[string]any) { delete(document, "candidate_provider_budget") },
			errorMatch: "candidate_provider_budget.attempt_timeout is required",
		},
		{
			name: "legacy candidate policy fields",
			mutate: func(document map[string]any) {
				candidate := document["candidate_provider_budget"].(map[string]any)
				candidate["timeout"] = "10s"
			},
			errorMatch: "unknown field",
		},
		{
			name: "candidate grace consumes attempt",
			mutate: func(document map[string]any) {
				candidate := document["candidate_provider_budget"].(map[string]any)
				candidate["startup_grace"] = "7s"
				candidate["shutdown_grace"] = "3s"
			},
			errorMatch: "must exceed startup and shutdown grace",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			document := completeExecutionProfileDocument()
			test.mutate(document)
			raw := append(marshalCompleteExecutionProfileDocument(t, document), test.trailing...)
			if _, err := ParseExecutionProfileJSON(raw); err == nil || !strings.Contains(err.Error(), test.errorMatch) {
				t.Fatalf("ParseExecutionProfileJSON(%s) error = %v, want %q", raw, err, test.errorMatch)
			}
		})
	}
}

func TestCandidateProviderBudgetValidatesAttemptBoundaryAndDerivesRuntimeWindows(t *testing.T) {
	for _, test := range []struct {
		name       string
		budget     CandidateProviderBudget
		errorMatch string
	}{
		{name: "zero attempt", budget: CandidateProviderBudget{}, errorMatch: "attempt timeout must be positive"},
		{name: "negative startup grace", budget: CandidateProviderBudget{AttemptTimeout: time.Second, StartupGrace: -time.Nanosecond}, errorMatch: "grace periods cannot be negative"},
		{name: "negative shutdown grace", budget: CandidateProviderBudget{AttemptTimeout: time.Second, ShutdownGrace: -time.Nanosecond}, errorMatch: "grace periods cannot be negative"},
		{name: "grace equals attempt", budget: CandidateProviderBudget{AttemptTimeout: 10 * time.Second, StartupGrace: 7 * time.Second, ShutdownGrace: 3 * time.Second}, errorMatch: "must exceed startup and shutdown grace"},
		{name: "grace exceeds attempt", budget: CandidateProviderBudget{AttemptTimeout: 10 * time.Second, StartupGrace: 7 * time.Second, ShutdownGrace: 4 * time.Second}, errorMatch: "must exceed startup and shutdown grace"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.budget.Validate(); err == nil || !strings.Contains(err.Error(), test.errorMatch) {
				t.Fatalf("Validate() error = %v, want %q", err, test.errorMatch)
			}
			if _, err := test.budget.ExecutionTimeout(); err == nil || !strings.Contains(err.Error(), test.errorMatch) {
				t.Fatalf("ExecutionTimeout() error = %v, want %q", err, test.errorMatch)
			}
			if _, err := test.budget.LeaseTTL(); err == nil || !strings.Contains(err.Error(), test.errorMatch) {
				t.Fatalf("LeaseTTL() error = %v, want %q", err, test.errorMatch)
			}
		})
	}

	budget := CandidateProviderBudget{AttemptTimeout: 10 * time.Second, StartupGrace: 2 * time.Second, ShutdownGrace: 3 * time.Second}
	executionTimeout, err := budget.ExecutionTimeout()
	if err != nil {
		t.Fatal(err)
	}
	leaseTTL, err := budget.LeaseTTL()
	if err != nil {
		t.Fatal(err)
	}
	if executionTimeout != 5*time.Second || leaseTTL != 10*time.Second {
		t.Fatalf("candidate runtime windows = execution %s, lease %s; want execution 5s, lease 10s", executionTimeout, leaseTTL)
	}
}

func completeExecutionProfileDocument() map[string]any {
	catalog := StandardStageCatalog()
	document := map[string]any{
		"template":                  catalog.Template,
		"id":                        "explicit",
		"version":                   "1",
		"continuation_plan_ttl":     "24h",
		"control_grace_period":      "30s",
		"candidate_provider_budget": map[string]any{"attempt_timeout": "10s", "startup_grace": "2s", "shutdown_grace": "3s"},
		"stages":                    make([]any, 0, len(catalog.Stages)),
	}
	stages := document["stages"].([]any)
	for _, stage := range catalog.Stages {
		stages = append(stages, map[string]any{
			"stage_key": string(stage.Key),
			"budget": map[string]any{
				"turn_timeout":    "1s",
				"max_turns":       stage.RequiredTurns,
				"attempt_timeout": fmt.Sprintf("%ds", stage.RequiredTurns),
				"max_attempts":    1,
				"max_elapsed":     fmt.Sprintf("%ds", stage.RequiredTurns),
				"idle_timeout":    "0s",
				"startup_grace":   "0s",
				"shutdown_grace":  "0s",
				"backoff":         map[string]any{"retry_delays": []any{}},
			},
		})
	}
	document["stages"] = stages
	return document
}

func marshalCompleteExecutionProfileDocument(t *testing.T, document map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
