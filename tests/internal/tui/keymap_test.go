package tui_test

import (
	"os"
	"path/filepath"
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

func TestCtrlCStartsCleanupAwareQuit(t *testing.T) {
	next, result := tuiapp.NewTestHarness(config.Default()).Press("ctrl+c")
	if result.Quit || result.CmdCount == 0 || !strings.Contains(next.Message(), "Docker 运行状态") {
		t.Fatalf("ctrl+c should start cleanup-aware quit, quit=%v cmds=%d message=%q", result.Quit, result.CmdCount, next.Message())
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

func TestSettingsOverlayKeepsDockerAsSettingsItemAndSavesProjectConfig(t *testing.T) {
	cfg := config.Default()
	root := t.TempDir()
	cfg.ProjectConfigPath = filepath.Join(root, ".p2r.yaml")
	cfg.Docker.DaemonMirrors.BackupDir = filepath.Join(root, "backups")
	if err := os.MkdirAll(cfg.Docker.DaemonMirrors.BackupDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.Docker.DaemonMirrors.BackupDir, "daemon-20260519T120000Z-abcd1234.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := tuiapp.NewTestHarness(cfg).SeedOverview("TASK-1").SetFocus("overview-table")
	h, _ = h.Press("ctrl+?")

	view := h.View()
	if !strings.Contains(view, "设置") || !strings.Contains(view, "> Docker 镜像源") || !strings.Contains(view, "目标配置") || !strings.Contains(view, "备份") || !strings.Contains(view, "1 个") {
		t.Fatalf("settings overlay should keep Docker as settings item:\n%s", view)
	}

	h, _ = h.Press("s")
	content, err := os.ReadFile(cfg.ProjectConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "daemon_mirrors") || !strings.Contains(h.Message(), cfg.ProjectConfigPath) {
		t.Fatalf("save should write project .p2r.yaml and report target, message=%q content=\n%s", h.Message(), content)
	}
}

func TestSettingsArrowKeysMoveDockerSettingFocus(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).SeedOverview("TASK-1").SetFocus("overview-table")
	h, _ = h.Press("ctrl+?")

	view := h.View()
	if !strings.Contains(view, "> 启用") {
		t.Fatalf("settings should start on enabled field:\n%s", view)
	}

	h, _ = h.Press("down")
	view = h.View()
	if !strings.Contains(view, "> daemon.json 路径") || strings.Contains(view, "[x]") {
		t.Fatalf("down should move focus to daemon.json field and use checkmark switches:\n%s", view)
	}

	h, _ = h.Press("down")
	view = h.View()
	if !strings.Contains(view, "> 备份目录") {
		t.Fatalf("second down should move focus to backup dir field:\n%s", view)
	}

	h, _ = h.Press("up")
	view = h.View()
	if !strings.Contains(view, "> daemon.json 路径") {
		t.Fatalf("up should move focus back to daemon.json field:\n%s", view)
	}
}

func TestSettingsOverlayTabCyclesFieldsAndQCloses(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).SeedOverview("TASK-1").SetFocus("overview-table")
	h, _ = h.Press("ctrl+?")
	view := h.View()
	if !strings.Contains(view, "> 启用") {
		t.Fatalf("settings overlay should start on enabled field:\n%s", view)
	}

	next, _ := h.Press("tab")
	if next.TabName() != "overview" || next.FocusName() != "overview-table" {
		t.Fatalf("tab in overlay should not switch page, tab=%s focus=%s", next.TabName(), next.FocusName())
	}
	if !strings.Contains(next.View(), "> daemon.json 路径") {
		t.Fatalf("tab should cycle settings fields:\n%s", next.View())
	}
	next, result := next.Press("q")
	if result.Quit || strings.Contains(next.View(), "目标配置") {
		t.Fatalf("q should close settings overlay without quitting, quit=%v view=\n%s", result.Quit, next.View())
	}
}

func TestSettingsOverlayInterceptsPageSwitchKeys(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).SeedOverview("TASK-1").SetFocus("overview-table")
	h, _ = h.Press("ctrl+?")

	h, _ = h.Press("ctrl+right")
	if h.TabName() != "overview" || !strings.Contains(h.View(), "目标配置") {
		t.Fatalf("ctrl+right should be intercepted by settings overlay, tab=%s view=\n%s", h.TabName(), h.View())
	}
}

func TestSettingsOverlayInterceptsQuitShortcuts(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).SeedOverview("TASK-1").SetFocus("overview-table")
	h, _ = h.Press("ctrl+?")

	next, result := h.Press("ctrl+c")
	if result.Quit || result.CmdCount != 0 || next.TabName() != "overview" || !strings.Contains(next.View(), "目标配置") {
		t.Fatalf("ctrl+c should be intercepted by settings overlay, quit=%v cmds=%d tab=%s view=\n%s", result.Quit, result.CmdCount, next.TabName(), next.View())
	}
	if !strings.Contains(next.Message(), "关闭设置") {
		t.Fatalf("intercept message should explain close key, got %q", next.Message())
	}
}

func TestStartupDockerCleanupPromptCanBeSkipped(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).ApplyStartupDockerCheckForTest(2, nil)
	if !h.StartupDockerCleanupConfirm() || !strings.Contains(h.View(), "遗留 Docker 资源") {
		t.Fatalf("startup docker check should open cleanup prompt:\n%s", h.View())
	}
	next, result := h.Press("n")
	if result.CmdCount != 0 || next.StartupDockerCleanupConfirm() || !strings.Contains(next.Message(), "已跳过") {
		t.Fatalf("n should skip startup cleanup, confirm=%v cmds=%d message=%q", next.StartupDockerCleanupConfirm(), result.CmdCount, next.Message())
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

func TestCtrlROnOverviewTaskUsesReinspect(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).
		SeedOverviewTask("TASK-20260521-ABCDEF", model.TaskCompleted).
		SetFocus("overview-table")

	next, result := h.Press("ctrl+r")
	if next.Confirm() || result.CmdCount == 0 || !strings.Contains(next.Message(), "重新质检") {
		t.Fatalf("overview task Ctrl+R should submit reinspection, confirm=%v cmds=%d message=%q", next.Confirm(), result.CmdCount, next.Message())
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

func TestCtrlXCancelQueuedJobUsesSeparateConfirmation(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).
		SeedOverview("TASK-1").
		SetFocus("overview-table").
		ApplySchedulerJobsForTest([]scheduler.JobSnapshot{{
			JobID:  "job-1",
			TaskID: "TASK-1",
			State:  scheduler.JobQueued,
		}})

	next, _ := h.Press("ctrl+x")
	if !next.CancelConfirm() || next.Confirm() {
		t.Fatalf("ctrl+x should open cancel confirmation only, cancel=%v run=%v", next.CancelConfirm(), next.Confirm())
	}
	if !strings.Contains(next.View(), "确认终止 TASK-1 的 job-1") {
		t.Fatalf("cancel prompt missing from view:\n%s", next.View())
	}
	next, result := next.Press("enter")
	if next.CancelConfirm() || result.CmdCount == 0 || !strings.Contains(next.Message(), "正在发送终止请求") {
		t.Fatalf("enter should submit cancel request, cancel=%v cmds=%d message=%q", next.CancelConfirm(), result.CmdCount, next.Message())
	}
}

func TestCancelMessageTakesPriorityOverPendingJobFailure(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).
		SeedOverview("TASK-1").
		ApplyRunSubmitForTest("job-1").
		ApplyCancelRequestForTest("TASK-1", "job-1", nil)

	h = h.ApplySchedulerJobsForTest([]scheduler.JobSnapshot{{
		JobID:           "job-1",
		TaskID:          "TASK-1",
		State:           scheduler.JobCancelled,
		Err:             scheduler.ErrJobCancelledByUser.Error(),
		CancelRequested: true,
	}})
	if got := h.Message(); got != "已终止 TASK-1 的运行" {
		t.Fatalf("cancel message should win over generic job failure, got %q", got)
	}
}

func TestCtrlXWithoutActiveJobAndRunConfigPriority(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).SeedOverview("TASK-1").SetFocus("overview-table")
	next, _ := h.Press("ctrl+x")
	if next.CancelConfirm() || next.Message() != "该任务没有排队或运行中的作业" {
		t.Fatalf("ctrl+x without active job message = %q cancel=%v", next.Message(), next.CancelConfirm())
	}

	h, _ = h.Press("ctrl+r")
	next, result := h.Press("ctrl+x")
	if !next.Confirm() || result.CmdCount != 0 || next.Message() != "请先关闭运行配置再终止作业" {
		t.Fatalf("ctrl+x should not pass through run config, confirm=%v cmds=%d message=%q", next.Confirm(), result.CmdCount, next.Message())
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
	if got := next.SelectedStageKey(); got != "D" {
		t.Fatalf("selected stage = %s, want D", got)
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
