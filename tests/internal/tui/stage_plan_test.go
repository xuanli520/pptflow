package tui_test

import (
	"os"
	"path/filepath"
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

func TestStagePlanFromAndExplicitStagesAreMutuallyExclusive(t *testing.T) {
	plan := tuiapp.StagePlanWithOptionsForTest("initial", "A", false, []string{"D"}, "C")
	if !strings.Contains(plan.BlockedReason, "不能同时使用") {
		t.Fatalf("expected mutual exclusion error, got %#v", plan)
	}
}

func TestStagePlanFromStageRunsThroughF(t *testing.T) {
	plan := tuiapp.StagePlanWithOptionsForTest("initial", "A", false, nil, "C")
	want := []string{"C", "D", "E", "F"}
	if !slices.Equal(plan.DisplayStages, want) || plan.RunStages != nil {
		t.Fatalf("plan = run %#v display %#v, want display %v and nil run stages", plan.RunStages, plan.DisplayStages, want)
	}
}

func TestStagePlanExplicitStagesAppendF(t *testing.T) {
	plan := tuiapp.StagePlanWithOptionsForTest("initial", "A", false, []string{"D"}, "")
	want := []string{"D", "F"}
	if !slices.Equal(plan.RunStages, want) || !slices.Equal(plan.DisplayStages, want) {
		t.Fatalf("plan = run %#v display %#v, want %v", plan.RunStages, plan.DisplayStages, want)
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

func TestRunConfigModeTogglePreservesFromStageWithoutConflict(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).
		SeedOverview("TASK-1").
		SetExecutionPanel().
		SetFocus("stage-list")

	h, _ = h.Press("ctrl+r")
	h, _ = h.Press("tab")
	h, _ = h.Press("tab")
	h, _ = h.Press(" ")
	h, _ = h.Press("shift+tab")
	h, _ = h.Press("shift+tab")
	h, _ = h.Press(" ")

	view := h.View()
	if strings.Contains(view, "起始阶段和阶段多选不能同时使用") {
		t.Fatalf("mode toggle should not create a stage-source conflict:\n%s", view)
	}
	if !strings.Contains(view, "起始阶段: A") {
		t.Fatalf("from stage should be preserved after mode toggle:\n%s", view)
	}
}

func TestRunConfigAttachedDocCountUsesManagedManifest(t *testing.T) {
	root := t.TempDir()
	docPath := filepath.Join(root, "extra.md")
	if err := os.WriteFile(docPath, []byte("extra context"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.ScanPath = root
	h := tuiapp.NewTestHarness(cfg).
		SeedOverview("TASK-1").
		SetExecutionPanel().
		SetFocus("stage-list")

	h, _ = h.Press("ctrl+r")
	for i := 0; i < 4; i++ {
		h, _ = h.Press("tab")
	}
	h, _ = h.Press(docPath)
	h, _ = h.Press("enter")
	h, _ = h.Press(docPath)
	h, _ = h.Press("enter")

	view := h.View()
	if !strings.Contains(view, "补充文档: 已托管附件 1 个") {
		t.Fatalf("attached count should come from managed manifest, got:\n%s", view)
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
