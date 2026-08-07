package tui

import (
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
)

func TestDisplayStageNameLocalizesAuthoringHarnessStages(t *testing.T) {
	for stage, want := range map[string]string{
		"dockerfile_build_validate": "Dockerfile 构建验证",
		"authoring_harness":         "Authoring harness 修复验证",
	} {
		if got := displayStageName(stage); got != want {
			t.Fatalf("displayStageName(%q) = %q, want %q", stage, got, want)
		}
	}
	if got := displayStageName("future_stage"); got != "future_stage" {
		t.Fatalf("unknown stage display = %q, want raw key", got)
	}
}

// TestDisplayStageNameCoversLiveAuthoringGraph is the guard against the rot this
// table had accumulated: every stage of the executable Standard Authoring graph
// was reachable in a running Run, but ten of them had no translation and showed
// their raw key while the table still carried entries nobody checked.
//
// Asserting against the compiled stage order rather than a hand-written list
// means adding a stage to the graph without a label fails here.
func TestDisplayStageNameCoversLiveAuthoringGraph(t *testing.T) {
	for _, stage := range workflowadapter.StandardAuthoringStageOrder() {
		key := string(stage)
		if got := displayStageName(key); got == key {
			t.Errorf("live authoring stage %q has no display name", key)
		}
	}
}

// TestDisplayStageNameKeepsUnknownKeysVerbatim pins the fallback. A stage key
// this build does not know must stay readable so an operator can still correlate
// it with a durable record instead of seeing a blank or a guess.
func TestDisplayStageNameKeepsUnknownKeysVerbatim(t *testing.T) {
	for _, key := range []string{"", "some_future_stage", "repo_prepare_v9"} {
		if got := displayStageName(key); got != key {
			t.Errorf("displayStageName(%q) = %q, want the raw key", key, got)
		}
	}
}
