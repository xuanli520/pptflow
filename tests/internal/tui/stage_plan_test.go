package tui_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	tuiapp "github.com/xuanli520/p2r_tui/internal/tui"
)

func TestStagePlanInitialRuntimeDefaults(t *testing.T) {
	plan := tuiapp.StagePlanForTest("initial", "A", false)
	if plan.RunStages != nil {
		t.Fatalf("initial run stages = %#v, want nil", plan.RunStages)
	}
	if !slices.Equal(plan.DisplayStages, []string{"A", "B", "C", "D", "E", "F"}) {
		t.Fatalf("display stages = %#v", plan.DisplayStages)
	}
}

func TestStagePlanInitialStaticOnlyDefaults(t *testing.T) {
	plan := tuiapp.StagePlanForTest("initial", "A", true)
	if plan.RunStages != nil {
		t.Fatalf("initial static-only run stages = %#v, want nil", plan.RunStages)
	}
	if !slices.Equal(plan.DisplayStages, []string{"A", "D", "E", "F"}) {
		t.Fatalf("display stages = %#v", plan.DisplayStages)
	}
}

func TestStagePlanRecheckUsesAffectedStages(t *testing.T) {
	plan := tuiapp.StagePlanForTest("recheck", "A", false)
	want := []string{"A", "F"}
	if !slices.Equal(plan.RunStages, want) || !slices.Equal(plan.DisplayStages, want) {
		t.Fatalf("plan = run %#v display %#v, want %v", plan.RunStages, plan.DisplayStages, want)
	}
}

func TestStagePlanBlocksRuntimeStagesInStaticOnlyRecheck(t *testing.T) {
	for _, stage := range []string{"B", "C"} {
		plan := tuiapp.StagePlanForTest("recheck", stage, true)
		if plan.BlockedReason == "" {
			t.Fatalf("stage %s should be blocked", stage)
		}
		if plan.RunStages != nil || plan.DisplayStages != nil {
			t.Fatalf("blocked plan should not return stages, got run %#v display %#v", plan.RunStages, plan.DisplayStages)
		}
	}
}

func TestRenderConfirmUsesInitialStagePlan(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).
		SeedOverview("TASK-1").
		SetExecutionPanel().
		SeedStages([]model.StageRecord{{Stage: "A", Status: model.StageDone}}, "A").
		SetFocus("stage-list")

	h, _ = h.Press("ctrl+r")
	view := h.View()
	if !strings.Contains(view, "阶段: A, B, C, D, E, F") {
		t.Fatalf("confirm should show initial full plan, got:\n%s", view)
	}
	if strings.Contains(view, "阶段: A, F") {
		t.Fatalf("confirm appears to use affectedStages for initial mode:\n%s", view)
	}
}

func TestStaticOnlyRecheckRuntimeStageDoesNotOpenConfirm(t *testing.T) {
	cfg := config.Default()
	cfg.Pipeline.StaticOnly = true
	h := tuiapp.NewTestHarness(cfg).
		SeedOverview("TASK-1").
		SetExecutionPanel().
		SeedStages([]model.StageRecord{{Stage: "B", Status: model.StageFailed}}, "B").
		SetFocus("stage-list")
	h, _ = h.Press("m")

	next, _ := h.Press("ctrl+r")
	if next.Confirm() {
		t.Fatal("static-only runtime recheck should not open confirmation")
	}
	if !strings.Contains(next.Message(), "static-only 模式不能重跑 runtime 阶段 B/C") {
		t.Fatalf("message = %q", next.Message())
	}
}
