package tui

import "testing"

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
