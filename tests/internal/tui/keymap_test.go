package tui_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/db"
	"github.com/xuanli520/p2r_tui/internal/pipeline"
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

func TestTaskInputQStartsCleanupAwareQuit(t *testing.T) {
	next, result := tuiapp.NewTestHarness(config.Default()).SetFocus("task-input").Press("q")
	if result.Quit || result.CmdCount == 0 || !strings.Contains(next.Message(), "Docker 运行状态") {
		t.Fatalf("q in task input should start cleanup-aware quit, quit=%v cmds=%d message=%q", result.Quit, result.CmdCount, next.Message())
	}
}

func TestTaskInputEscapeRestoresPreviousFocus(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).SeedOverview("TASK-1").SetExecutionPanel().SetFocus("detail-viewport")
	h, _ = h.Press("/")
	if h.FocusName() != "task-input" {
		t.Fatalf("/ should focus task input, got %s", h.FocusName())
	}
	h, _ = h.Press("esc")
	if h.FocusName() != "detail-viewport" || h.TabName() != "execution" {
		t.Fatalf("esc should restore previous focus, tab=%s focus=%s", h.TabName(), h.FocusName())
	}
}

func TestOverviewSlashFocusesSearch(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).SeedOverview("TASK-1").SetFocus("overview-table")

	next, result := h.Press("/")
	if result.Quit || next.FocusName() != "search" {
		t.Fatalf("/ on overview should focus search, quit=%v focus=%s", result.Quit, next.FocusName())
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
	h, _ = h.Press("ctrl+/")

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
	h, _ = h.Press("ctrl+/")

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
	h, _ = h.Press("ctrl+/")
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

func TestSettingsOverlayCtrlSlashToggles(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).SeedOverview("TASK-1").SetFocus("overview-table")
	opened, _ := h.Press("ctrl+/")
	if !strings.Contains(opened.View(), "目标配置") {
		t.Fatalf("ctrl+/ should open settings overlay:\n%s", opened.View())
	}

	closed, result := opened.Press("ctrl+/")
	if result.Quit || strings.Contains(closed.View(), "目标配置") {
		t.Fatalf("ctrl+/ should close settings overlay without quitting, quit=%v view=\n%s", result.Quit, closed.View())
	}
}

func TestSettingsOverlayInterceptsPageSwitchKeys(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).SeedOverview("TASK-1").SetFocus("overview-table")
	h, _ = h.Press("ctrl+/")

	h, _ = h.Press("ctrl+right")
	if h.TabName() != "overview" || !strings.Contains(h.View(), "目标配置") {
		t.Fatalf("ctrl+right should be intercepted by settings overlay, tab=%s view=\n%s", h.TabName(), h.View())
	}
}

func TestSettingsOverlayInterceptsQuitShortcuts(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).SeedOverview("TASK-1").SetFocus("overview-table")
	h, _ = h.Press("ctrl+/")

	next, result := h.Press("ctrl+c")
	if result.Quit || result.CmdCount != 0 || next.TabName() != "overview" || !strings.Contains(next.View(), "目标配置") {
		t.Fatalf("ctrl+c should be intercepted by settings overlay, quit=%v cmds=%d tab=%s view=\n%s", result.Quit, result.CmdCount, next.TabName(), next.View())
	}
	if !strings.Contains(next.Message(), "关闭设置") {
		t.Fatalf("intercept message should explain close key, got %q", next.Message())
	}
}

func TestSettingsOverlayDoesNotIncreaseViewportHeight(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).SeedOverview("TASK-1").SetFocus("overview-table").SetSize(90, 12)
	closedHeight := lipgloss.Height(h.View())
	h, _ = h.Press("ctrl+/")
	view := h.View()
	if got := lipgloss.Height(view); got > closedHeight {
		t.Fatalf("settings overlay should not add rows, height=%d closed=%d\n%s", got, closedHeight, view)
	}
	if got := lipgloss.Width(view); got > 90 {
		t.Fatalf("settings overlay width=%d, want <=90\n%s", got, view)
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
	cfg := config.Default()
	cfg.ScanPath = t.TempDir()
	taskID := "TASK-20260521-ABCDEF"
	writeTUIDropboxDoc(t, cfg.ScanPath, taskID)
	h := tuiapp.NewTestHarness(cfg).SeedOverview(taskID).SetFocus("overview-table")

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

func TestCtrlROnOverviewTaskOpensRunConfig(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).
		SeedOverviewTask("TASK-20260521-ABCDEF", model.TaskCompleted).
		SetFocus("overview-table")

	next, result := h.Press("ctrl+r")
	if !next.Confirm() || result.CmdCount != 0 || !strings.Contains(next.View(), "运行配置: TASK-20260521-ABCDEF") {
		t.Fatalf("overview task Ctrl+R should open run config, confirm=%v cmds=%d message=%q\n%s", next.Confirm(), result.CmdCount, next.Message(), next.View())
	}
}

func TestCtrlROnOverviewWaitingTaskDoesNotOpenRunConfig(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).
		SeedOverviewTask("TASK-20260521-ABCDEF", model.TaskWaitingManual).
		SetFocus("overview-table")

	next, result := h.Press("ctrl+r")
	if next.Confirm() || result.CmdCount != 0 || !strings.Contains(next.Message(), "请选择可重跑质检任务") {
		t.Fatalf("waiting task Ctrl+R should be blocked, confirm=%v cmds=%d message=%q", next.Confirm(), result.CmdCount, next.Message())
	}
}

func TestTaskInputCtrlROpensRunConfigForTypedTaskID(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).SetFocus("task-input")
	h, _ = h.Press("TASK-20260521-ABCDEF")

	next, result := h.Press("ctrl+r")
	if !next.TaskTypePrompt() || next.Confirm() || result.CmdCount != 0 || !strings.Contains(next.View(), "确认题型 TASK-20260521-ABCDEF") {
		t.Fatalf("typed new task Ctrl+R should open task type prompt, prompt=%v confirm=%v cmds=%d message=%q\n%s", next.TaskTypePrompt(), next.Confirm(), result.CmdCount, next.Message(), next.View())
	}
}

func TestTaskInputEnterOpensTaskTypePromptBeforeRunConfig(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).ApplyTaskInputSubmitForTest("TASK-20260521-ABCDEF")
	if !h.TaskTypePrompt() || h.Confirm() || !strings.Contains(h.View(), "确认题型 TASK-20260521-ABCDEF") {
		t.Fatalf("task input submit should open task type prompt before run config:\n%s", h.View())
	}

	h, _ = h.Press("2")
	next, result := h.Press("enter")
	if next.TaskTypePrompt() || !next.Confirm() || result.CmdCount != 0 || !strings.Contains(next.View(), "题型: 纯后端") {
		t.Fatalf("task type confirmation should open run config with selected type, prompt=%v confirm=%v cmds=%d\n%s", next.TaskTypePrompt(), next.Confirm(), result.CmdCount, next.View())
	}
}

func TestTaskInputEnterForExistingTaskOpensRunConfigWithoutTypePrompt(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.ScanPath = t.TempDir()
	store, err := db.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	taskID := "TASK-20260521-ABCDEF"
	gitURL := "https://gitlab.mindflow.com.cn/Prompt2Repo/web/" + taskID
	if _, err := store.CreateTaskWithBatch(ctx, taskID, gitURL, cfg.ScanPath); err != nil {
		t.Fatal(err)
	}

	h := tuiapp.NewTestHarnessWithStore(store, cfg).ApplyTaskInputSubmitForTest(taskID)
	view := h.View()
	if h.TaskTypePrompt() || !h.Confirm() || !strings.Contains(view, "运行配置: "+taskID) {
		t.Fatalf("existing task submit should open run config without task type prompt:\n%s", view)
	}
	if !strings.Contains(view, "重置题型: 保持当前 (纯前端)") {
		t.Fatalf("run config should show current type reset option:\n%s", view)
	}
}

func TestRunConfigProjectTypeResetCyclesAtBottom(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.ScanPath = t.TempDir()
	store, err := db.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	taskID := "TASK-20260521-FEDCBA"
	gitURL := "https://gitlab.mindflow.com.cn/Prompt2Repo/fullstack/" + taskID
	if _, err := store.CreateTaskWithBatch(ctx, taskID, gitURL, cfg.ScanPath); err != nil {
		t.Fatal(err)
	}

	h := tuiapp.NewTestHarnessWithStore(store, cfg).ApplyTaskInputSubmitForTest(taskID)
	for i := 0; i < 5; i++ {
		h, _ = h.Press("tab")
	}
	for i := 0; i < 3; i++ {
		h, _ = h.Press(" ")
	}
	if !strings.Contains(h.View(), "重置题型: 纯前端") {
		t.Fatalf("project type reset should cycle to pure frontend at the bottom:\n%s", h.View())
	}
}

func TestCtrlWRetriesGitSync(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).SeedTaskBoardForTest([]tuiapp.TaskProject{
		{ID: "TASK-20260521-AAAAAA", TaskState: model.TaskInspecting, SyncError: "auth failed"},
	}).SetFocus("task-board")
	view := h.View()
	if !strings.Contains(view, "Ctrl+W 重试Git") {
		t.Fatalf("task board footer should expose Ctrl+W retry, got:\n%s", view)
	}
	next, result := h.Press("ctrl+w")
	if result.CmdCount == 0 || !strings.Contains(next.Message(), "正在重试 Git 同步") {
		t.Fatalf("ctrl+w should submit retry, cmds=%d message=%q", result.CmdCount, next.Message())
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

func TestCtrlERequiresManualVerdictBeforeCompletingTask(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).
		SeedTaskBoardForTest([]tuiapp.TaskProject{
			{ID: "TASK-20260521-AAAAAA", TaskState: model.TaskWaitingManual},
		}).
		SetFocus("task-board")

	next, result := h.Press("ctrl+e")
	if result.CmdCount != 0 || !strings.Contains(next.View(), "结束质检前判定 TASK-20260521-AAAAAA") {
		t.Fatalf("ctrl+e should open verdict prompt, cmd=%d view=\n%s", result.CmdCount, next.View())
	}

	next, _ = next.Press("3")
	next, result = next.Press("enter")
	if result.CmdCount == 0 || !strings.Contains(next.Message(), "判定: 不通过") {
		t.Fatalf("enter should submit completion with selected verdict, cmd=%d message=%q", result.CmdCount, next.Message())
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

func TestCtrlXWithPersistedRunningRunStartsOrphanRecovery(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).
		SeedExecutionRun("TASK-1", "run-1", []model.StageRecord{{Stage: "C", Status: model.StageRunning}}, "C").
		WithOrphanRunRecoveryForTest(func(context.Context, string) (pipeline.RecoveryResult, error) {
			return pipeline.RecoveryResult{}, nil
		})

	next, result := h.Press("ctrl+x")
	if next.CancelConfirm() || result.CmdCount != 1 || next.Message() != "正在检查失联运行 TASK-1" {
		t.Fatalf("ctrl+x should start orphan recovery, cancel=%v cmds=%d message=%q", next.CancelConfirm(), result.CmdCount, next.Message())
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

func TestTaskBoardSelectionWraps(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).SeedTaskBoardForTest([]tuiapp.TaskProject{
		{ID: "TASK-20260521-AAAAAA", TaskState: model.TaskInspecting},
		{ID: "TASK-20260521-BBBBBB", TaskState: model.TaskInspecting},
	}).SetFocus("task-board")

	next, _ := h.Press("up")
	if got := next.SelectedTaskID(); got != "TASK-20260521-BBBBBB" {
		t.Fatalf("up should wrap to last task, got %s", got)
	}
	next, _ = next.Press("down")
	if got := next.SelectedTaskID(); got != "TASK-20260521-AAAAAA" {
		t.Fatalf("down should wrap to first task, got %s", got)
	}
}

func TestTaskBoardDownKeepsListWindowFilled(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).SeedTaskBoardForTest([]tuiapp.TaskProject{
		{ID: "TASK-20260521-AAAAAA", TaskState: model.TaskInspecting, SyncError: "auth failed"},
		{ID: "TASK-20260521-BBBBBB", TaskState: model.TaskInspecting, SyncError: "network timeout"},
		{ID: "TASK-20260521-CCCCCC", TaskState: model.TaskInspecting, SyncError: "clone failed"},
	}).SetFocus("task-board").SetSize(82, 35)

	h, _ = h.Press("down")
	view := h.View()
	if got := h.SelectedTaskID(); got != "TASK-20260521-BBBBBB" {
		t.Fatalf("selected task = %s, want TASK-20260521-BBBBBB", got)
	}
	for _, want := range []string{"TASK-20260521-AAAAAA", "TASK-20260521-BBBBBB", "TASK-20260521-CCCCCC"} {
		if !strings.Contains(view, want) {
			t.Fatalf("task board should keep surrounding rows visible after down, missing %s:\n%s", want, view)
		}
	}
}

func TestTaskBoardEnterLocksExecutionTaskAcrossOverviewRefresh(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).
		SeedTaskBoardForTest([]tuiapp.TaskProject{
			{ID: "TASK-20260521-AAAAAA", TaskState: model.TaskCompleted},
		}).
		SeedOverview("TASK-20260521-BBBBBB").
		SetFocus("task-board")

	next, result := h.Press("enter")
	if result.CmdCount == 0 || next.TabName() != "execution" || next.SelectedTaskID() != "TASK-20260521-AAAAAA" {
		t.Fatalf("enter should open selected task detail, cmd=%d tab=%s selected=%s", result.CmdCount, next.TabName(), next.SelectedTaskID())
	}

	next, _ = next.ApplyOverviewResultForTest(next.OverviewSeq(), 1, "TASK-20260521-BBBBBB")
	if got := next.SelectedTaskID(); got != "TASK-20260521-AAAAAA" {
		t.Fatalf("execution task should survive overview refresh, got %s", got)
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
