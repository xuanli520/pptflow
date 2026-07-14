package workflowadapter

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestParseExecutionProfileJSONUsesReadableDurationsAndRequiresCompleteCatalogAtCompile(t *testing.T) {
	catalog := StandardStageCatalog()
	document := map[string]any{"template": catalog.Template, "id": "explicit", "version": "1", "continuation_plan_ttl": "24h", "control_grace_period": "30s", "stages": make([]any, 0, len(catalog.Stages))}
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
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := ParseExecutionProfileJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if profile.ID != "explicit" || profile.ContinuationPlanTTL != RequiredContinuationPlanTTL || profile.ControlGracePeriod != 30*time.Second || len(profile.Stages) != len(catalog.Stages) {
		t.Fatalf("unexpected parsed profile: %+v", profile)
	}
	if _, err := StandardWorkflowTemplate().Compile(profile); err != nil {
		t.Fatalf("parsed complete profile did not compile: %v", err)
	}
}

func TestParseExecutionProfileJSONRejectsAmbiguousOrMalformedInput(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte(`{"id":"x","version":"1","continuation_plan_ttl":"24h","control_grace_period":"0s","stages":[],"unknown":true}`),
		[]byte(`{"id":"x","version":"1","continuation_plan_ttl":"24h","control_grace_period":"0s","stages":[{"stage_key":"a","budget":{"turn_timeout":"not-a-duration"}}]}`),
		[]byte(`{"id":"x","version":"1","continuation_plan_ttl":"24h","control_grace_period":"0s","stages":[]} trailing`),
		[]byte(`{"id":"x","version":"1","continuation_plan_ttl":"23h","control_grace_period":"0s","stages":[]}`),
		[]byte(`{"id":"x","version":"1","continuation_plan_ttl":"24h","stages":[]}`),
	} {
		if _, err := ParseExecutionProfileJSON(raw); err == nil {
			t.Fatalf("profile %s was accepted", raw)
		}
	}
}
