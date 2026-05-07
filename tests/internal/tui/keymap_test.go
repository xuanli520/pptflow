package tui_test

import (
	"strings"
	"testing"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/scheduler"
	tuiapp "github.com/xuanli520/p2r_tui/internal/tui"
)

func TestSearchQDoesNotQuit(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).SetFocus("search")

	next, result := h.Press("q")
	if result.Quit {
		t.Fatal("q in search focus should not quit")
	}
	if got := next.SearchValue(); got != "q" {
		t.Fatalf("search value = %q, want q", got)
	}
}

func TestCtrlCQuits(t *testing.T) {
	_, result := tuiapp.NewTestHarness(config.Default()).Press("ctrl+c")
	if !result.Quit {
		t.Fatal("ctrl+c should quit")
	}
}

func TestModeKeyDoesNotStealSearchInput(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).SetFocus("search")
	next, _ := h.Press("m")
	if next.Mode() != "initial" || next.SearchValue() != "m" {
		t.Fatalf("m in search should be input only, mode=%s search=%q", next.Mode(), next.SearchValue())
	}

	next = next.SeedOverview("TASK-1").SetFocus("overview-table")
	next, _ = next.Press("m")
	if next.Mode() != "recheck" {
		t.Fatalf("m outside input should toggle mode, got %s", next.Mode())
	}
}

func TestSearchMatchesLocalizedStatus(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).SeedOverview("TASK-1").SetFocus("search")
	next, _ := h.Press("通过")
	if next.VisibleCount() != 1 {
		t.Fatalf("localized status search should keep matching row, visible=%d", next.VisibleCount())
	}
}

func TestCtrlROpensConfirmAndConfirmKeys(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).SeedOverview("TASK-1").SetFocus("overview-table")

	next, _ := h.Press("ctrl+r")
	if !next.Confirm() {
		t.Fatal("ctrl+r should open confirmation")
	}
	next, _ = next.Press("esc")
	if next.Confirm() || next.Message() != "已取消重跑" {
		t.Fatalf("esc should cancel confirmation, confirm=%v message=%q", next.Confirm(), next.Message())
	}

	next, _ = h.Press("ctrl+r")
	next, result := next.Press("enter")
	if next.Confirm() || !next.Running() || result.CmdCount == 0 {
		t.Fatalf("enter should confirm and start run, confirm=%v running=%v cmds=%d", next.Confirm(), next.Running(), result.CmdCount)
	}
}

func TestRecheckRequiresRefRunBeforeConfirm(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).
		SeedOverview("TASK-1").
		SetExecutionPanel().
		SetFocus("stage-list")
	h, _ = h.Press("m")
	next, _ := h.Press("ctrl+r")
	if next.Confirm() || next.Message() != "打回重检模式需要选择一个参考运行" {
		t.Fatalf("recheck without ref should be blocked, confirm=%v message=%q", next.Confirm(), next.Message())
	}
}

func TestRunConfigModeToggleToRecheckSubmitsWithAvailableRefRun(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).
		SeedExecutionDetail("TASK-1").
		SeedRefRunsForCurrentMode("run-ref-1").
		SetFocus("stage-list")

	h, _ = h.Press("ctrl+r")
	h, _ = h.Press(" ")
	next, result := h.Press("enter")
	if next.Confirm() || !next.Running() || result.CmdCount == 0 {
		t.Fatalf("recheck mode toggle should submit with available ref run, confirm=%v running=%v cmds=%d message=%q", next.Confirm(), next.Running(), result.CmdCount, next.Message())
	}
}

func TestSubmittedJobFailureUpdatesMessage(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).ApplyRunSubmitForTest("job-1")

	h = h.ApplySchedulerJobsForTest([]scheduler.JobSnapshot{{
		JobID: "job-1",
		State: scheduler.JobFailed,
		Err:   "ref run run-old artifact root is missing",
	}})
	if !strings.Contains(h.Message(), "启动失败") || !strings.Contains(h.Message(), "artifact root is missing") {
		t.Fatalf("message should surface scheduler failure, got %q", h.Message())
	}
}

func TestPipelineBarShowsEachConfiguredRunningSlot(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).
		SetSize(120, 20).
		ApplySchedulerJobsForTest([]scheduler.JobSnapshot{
			{JobID: "job-1", TaskID: "TASK-A", State: scheduler.JobRunning, CurrentStage: "A"},
			{JobID: "job-2", TaskID: "TASK-B", State: scheduler.JobRunning, CurrentStage: "B"},
			{JobID: "job-3", TaskID: "TASK-C", State: scheduler.JobRunning, CurrentStage: "C"},
		})
	view := h.View()
	for _, want := range []string{"TASK-A", "TASK-B", "TASK-C"} {
		if !strings.Contains(view, want) {
			t.Fatalf("pipeline bar should show %s when max_concurrent has a visible slot:\n%s", want, view)
		}
	}
	if strings.Contains(view, "另有 1 个 job") {
		t.Fatalf("pipeline bar should not fold the third running slot:\n%s", view)
	}
}

func TestOverviewTableMovesSelection(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).SeedOverview("TASK-1", "TASK-2").SetFocus("overview-table")

	next, _ := h.Press("down")
	if got := next.SelectedTaskID(); got != "TASK-2" {
		t.Fatalf("selected task = %s, want TASK-2", got)
	}
}

func TestStageListMovesSelectedStage(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).
		SetExecutionPanel().
		SeedStages([]model.StageRecord{
			{Stage: "A", Status: model.StageDone},
			{Stage: "B", Status: model.StageFailed},
		}, "A").
		SetFocus("stage-list")

	next, _ := h.Press("down")
	if got := next.SelectedStageKey(); got != "B" {
		t.Fatalf("selected stage = %s, want B", got)
	}
}

func TestRefRunListMovesSelection(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).
		SeedRefRuns("run-1", "run-2").
		SetFocus("ref-run-list")

	next, _ := h.Press("down")
	if got := next.SelectedRefRun(); got != "run-2" {
		t.Fatalf("selected ref run = %s, want run-2", got)
	}
}

func TestDetailViewportPages(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).
		SetDetailContent(20, 2, "one\ntwo\nthree\nfour\nfive").
		SetFocus("detail-viewport")

	next, _ := h.Press("pgdown")
	if next.DetailYOffset() == 0 {
		t.Fatal("pgdown should scroll the detail viewport")
	}
}

func TestShiftTabSwitchesPanel(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).
		SeedOverview("TASK-1").
		SetExecutionPanel().
		SetFocus("stage-list")

	next, _ := h.Press("shift+tab")
	if next.TabName() != "overview" || next.FocusName() != "overview-table" {
		t.Fatalf("shift+tab should move back to overview table, tab=%s focus=%s", next.TabName(), next.FocusName())
	}
}
