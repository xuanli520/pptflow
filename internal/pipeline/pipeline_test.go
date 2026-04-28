package pipeline

import (
	"strings"
	"testing"

	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
)

func TestSelectedStagesStaticOnly(t *testing.T) {
	selected := selectedStages(RunOptions{StaticOnly: true}, true)
	for _, stage := range []string{"A", "D", "E", "F"} {
		if !selected[stage] {
			t.Fatalf("expected %s selected", stage)
		}
	}
	for _, stage := range []string{"B", "C"} {
		if selected[stage] {
			t.Fatalf("expected %s skipped", stage)
		}
	}
}

func TestSelectedStagesFrom(t *testing.T) {
	selected := selectedStages(RunOptions{From: "C"}, false)
	for _, stage := range []string{"A", "B"} {
		if selected[stage] {
			t.Fatalf("expected %s not selected", stage)
		}
	}
	for _, stage := range []string{"C", "D", "E", "F"} {
		if !selected[stage] {
			t.Fatalf("expected %s selected", stage)
		}
	}
}

func TestSelectedStagesSingleStageStillRunsSummary(t *testing.T) {
	selected := selectedStages(RunOptions{Stage: "D"}, false)
	if !selected["D"] || !selected["F"] {
		t.Fatalf("expected D and F selected, got %#v", selected)
	}
	for _, stage := range []string{"A", "B", "C", "E"} {
		if selected[stage] {
			t.Fatalf("expected %s not selected", stage)
		}
	}
}

func TestSelectedStagesExplicitDependencyChain(t *testing.T) {
	selected := selectedStages(RunOptions{Stages: []string{"A", "B", "C"}}, false)
	for _, stage := range []string{"A", "B", "C", "F"} {
		if !selected[stage] {
			t.Fatalf("expected %s selected", stage)
		}
	}
	if selected["D"] || selected["E"] {
		t.Fatalf("D/E should not be selected for A dependency rerun")
	}
}

func TestAssignFindingIDs(t *testing.T) {
	findings := assignFindingIDs("E", []model.Finding{
		{Severity: "Blocker", Title: "one"},
		{Severity: "High", Title: "two"},
		{Severity: "High", Title: "three"},
	})
	want := []string{"P2R-E-BLK-001", "P2R-E-HIGH-001", "P2R-E-HIGH-002"}
	for i, id := range want {
		if findings[i].ID != id {
			t.Fatalf("finding %d id = %s, want %s", i, findings[i].ID, id)
		}
	}
}

func TestShortCommentKeepsManualVerdictUnchecked(t *testing.T) {
	text := shortComment(map[string]string{"B": "skipped", "C": "skipped"}, nil)
	if want := "<[ ] PASS  [ ] REWORK  [ ] FAIL>"; !strings.Contains(text, want) {
		t.Fatalf("short comment missing manual verdict line: %s", text)
	}
}
