package tui_test

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	tuiapp "github.com/xuanli520/p2r_tui/internal/tui"
)

func TestExecutionLayoutBreakpoints(t *testing.T) {
	cases := map[int]string{120: "wide", 100: "medium", 80: "stacked", 70: "minimal"}
	for width, want := range cases {
		if got := tuiapp.ExecutionLayoutModeForTest(width, 30); got != want {
			t.Fatalf("%d columns mode = %s, want %s", width, got, want)
		}
	}
}

func TestOverviewColumnsKeepCoreSignalsOnNarrowWidth(t *testing.T) {
	titles := titleSet(tuiapp.OverviewColumnTitlesForTest(70))
	for _, title := range []string{"任务ID", "状态", "失败", "阻断", "严重"} {
		if !titles[title] {
			t.Fatalf("narrow columns missing core title %s", title)
		}
	}
	if titles["文档"] || titles["清理"] || titles["模式"] {
		t.Fatalf("extreme narrow columns should hide lower-priority titles: %#v", titles)
	}
}

func TestOverviewColumnsHideModeBeforeCoreAtMediumWidth(t *testing.T) {
	titles := titleSet(tuiapp.OverviewColumnTitlesForTest(100))
	if titles["模式"] {
		t.Fatalf("medium columns should hide mode: %#v", titles)
	}
	for _, title := range []string{"任务ID", "质检", "状态", "失败", "阻断", "严重", "完成", "文档", "清理"} {
		if !titles[title] {
			t.Fatalf("medium columns missing %s: %#v", title, titles)
		}
	}
}

func TestOverviewWideShowsCompletionCountColumn(t *testing.T) {
	titles := titleSet(tuiapp.OverviewColumnTitlesForTest(120))
	if !titles["完成"] {
		t.Fatalf("wide columns should show completion count: %#v", titles)
	}
}

func TestOverviewLegacyRowsShowTaskID(t *testing.T) {
	columns := tuiapp.OverviewColumnsForTest(120)
	taskIDIndex := -1
	for index, column := range columns {
		if column.Key == "task_id" {
			taskIDIndex = index
			break
		}
	}
	if taskIDIndex < 0 {
		t.Fatalf("task_id column missing: %#v", columns)
	}
	row := tuiapp.OverviewLegacyRowForTest(120)
	if row[taskIDIndex] != "TASK-LEGACY" {
		t.Fatalf("legacy overview row task id = %q, want TASK-LEGACY; row=%#v", row[taskIDIndex], row)
	}
}

func TestOverviewColumnsHideLastRunWhenWidthInsufficient(t *testing.T) {
	titles := titleSet(tuiapp.OverviewColumnTitlesForTest(100))
	if titles["最后运行"] {
		t.Fatalf("medium columns should hide last_run instead of truncating it: %#v", titles)
	}
}

func TestOverviewMediumDoesNotShowTwelveColumnLastRun(t *testing.T) {
	for _, column := range tuiapp.OverviewColumnsForTest(100) {
		if column.Key == "last_run" && column.Width == 12 {
			t.Fatalf("last_run should never be shown at 12 columns: %#v", column)
		}
	}
}

func TestOverviewLastRunColumnIsNeverTruncatedWhenVisible(t *testing.T) {
	columns := tuiapp.OverviewColumnsForTest(120)
	lastRunIndex := -1
	lastRunWidth := 0
	for index, column := range columns {
		if column.Key == "last_run" {
			lastRunIndex = index
			lastRunWidth = column.Width
		}
	}
	if lastRunIndex < 0 || lastRunWidth != 16 {
		t.Fatalf("wide layout should show last_run at width 16: %#v", columns)
	}
	row := tuiapp.OverviewRowForTest("2026-05-07T05:36:18Z", 120)
	value := row[lastRunIndex]
	if strings.Contains(value, "…") {
		t.Fatalf("last_run should not be truncated: %q", value)
	}
	if got := lipgloss.Width(value); got > lastRunWidth {
		t.Fatalf("last_run width = %d, want <= %d for %q", got, lastRunWidth, value)
	}
}

func TestShortTimeConvertsUTCToShanghai(t *testing.T) {
	if got := tuiapp.ShortTimeForTest("2026-05-07T05:36:18Z"); got != "2026-05-07 13:36" {
		t.Fatalf("shortTime = %q", got)
	}
}

func TestShortTimeInvalidInputIsWidthSafe(t *testing.T) {
	got := tuiapp.ShortTimeForTest("not-a-valid-rfc3339-value")
	if lipgloss.Width(got) > 16 {
		t.Fatalf("shortTime fallback width = %d for %q", lipgloss.Width(got), got)
	}
}

func TestTruncateDisplayRespectsChineseWidth(t *testing.T) {
	got := tuiapp.TruncateDisplayForTest("阶段 B - Docker运行时证据", 10)
	if width := lipgloss.Width(got); width > 10 {
		t.Fatalf("truncated width = %d, want <= 10 for %q", width, got)
	}
}

func TestLocalizationCoversCoreValues(t *testing.T) {
	cases := map[string]string{
		tuiapp.LocalizeRunStatusForTest(model.RunCompletedClean):        "通过",
		tuiapp.LocalizeRunStatusForTest(model.RunCompletedWithFindings): "有发现",
		tuiapp.LocalizeStageStatusForTest(model.StageBlocked):           "已阻塞",
		tuiapp.LocalizeManualVerdictForTest(model.ManualRework):         "返工",
		tuiapp.LocalizeSeverityForTest("Blocker"):                       "阻断",
		tuiapp.LocalizeStageNameForTest("F", ""):                        "标注员修复静态审查",
		tuiapp.LocalizeCleanupStatusForTest("none"):                     "未生成",
		tuiapp.LocalizeSummaryForTest("Not selected for this run."):     "本次未选择",
		tuiapp.LocalizeSummaryForTest("3 validation finding(s)"):        "3 个验证发现",
		tuiapp.LocalizeSummaryForTest("3 acceptance finding(s)"):        "3 个验收发现",
	}
	for got, want := range cases {
		if got != want {
			t.Fatalf("localized value = %s, want %s", got, want)
		}
	}
}

func TestFooterChangesWithFocus(t *testing.T) {
	search := tuiapp.FooterForTest("search", false)
	detail := tuiapp.FooterForTest("detail-viewport", false)
	if search == "" || search == detail {
		t.Fatalf("footer should be focus-specific, search=%q detail=%q", search, detail)
	}
	taskBoard := tuiapp.FooterForTest("task-board", false)
	if !strings.Contains(taskBoard, "Ctrl+E") || !strings.Contains(taskBoard, "Ctrl+/") || !strings.Contains(taskBoard, "Q 退出") {
		t.Fatalf("task board footer should expose global workflow keys, got %q", taskBoard)
	}
	taskInput := tuiapp.FooterForTest("task-input", false)
	if !strings.Contains(taskInput, "Enter 开始质检") || !strings.Contains(taskInput, "Ctrl+/") || !strings.Contains(taskInput, "Q 退出") {
		t.Fatalf("task input footer should expose submit/global keys, got %q", taskInput)
	}
	if got := tuiapp.FooterForTest("search", true); got != "Tab 切换  Space 选择  Enter 确认  Esc 取消" {
		t.Fatalf("confirm footer = %q", got)
	}
	if got := tuiapp.CancelFooterForTest(); got != "y/Enter 确认终止  n/Esc 取消" {
		t.Fatalf("cancel footer = %q", got)
	}
}

func TestExecutionRenderDoesNotExceedViewportWidth(t *testing.T) {
	for _, width := range []int{120, 100, 80, 70} {
		view := tuiapp.NewTestHarness(config.Default()).
			SeedExecutionDetail("TASK-1").
			SetSize(width, 30).
			View()
		if got := lipgloss.Width(view); got > width {
			t.Fatalf("render width at %d columns = %d, want <= %d\n%s", width, got, width, view)
		}
	}
	for _, size := range []struct {
		width  int
		height int
	}{
		{120, 12},
		{100, 12},
		{90, 12},
	} {
		view := tuiapp.NewTestHarness(config.Default()).
			SeedExecutionDetail("TASK-1").
			SetSize(size.width, size.height).
			View()
		if got := lipgloss.Width(view); got > size.width {
			t.Fatalf("render width at %dx%d = %d, want <= %d\n%s", size.width, size.height, got, size.width, view)
		}
		if got := lipgloss.Height(view); got > size.height {
			t.Fatalf("render height at %dx%d = %d, want <= %d\n%s", size.width, size.height, got, size.height, view)
		}
	}
}

func TestTaskBoardColumnsUseSeparatorsAndFixedTitles(t *testing.T) {
	view := tuiapp.TaskBoardViewForTest(120, 18,
		[]tuiapp.TaskProject{{ID: "TASK-20260521-AAAAAA", TaskState: model.TaskInspecting}},
		[]tuiapp.TaskProject{{ID: "TASK-20260521-BBBBBB", TaskState: model.TaskWaitingManual}},
		[]tuiapp.TaskProject{{ID: "TASK-20260521-CCCCCC", TaskState: model.TaskCompleted, CompletionCount: 1}},
	)
	if !strings.Contains(view, "│") || !strings.Contains(view, "─── 开始质检 (1) ───") {
		t.Fatalf("task board should show column separators and fixed titles:\n%s", view)
	}
	if got := lipgloss.Width(view); got > 120 {
		t.Fatalf("task board width = %d, want <= 120\n%s", got, view)
	}
}

func TestTaskBoardShortViewportDoesNotOverflow(t *testing.T) {
	tasks := []tuiapp.TaskProject{
		{ID: "TASK-20260521-AAAAAA", TaskState: model.TaskInspecting, SyncError: "network timeout"},
		{ID: "TASK-20260521-BBBBBB", TaskState: model.TaskInspecting, SyncPhase: "clone", SyncPercent: 45},
		{ID: "TASK-20260521-CCCCCC", TaskState: model.TaskInspecting, RunStatus: model.RunRunning, CurrentStage: "E"},
	}
	view := tuiapp.TaskBoardViewForTest(120, 8, tasks, tasks, tasks)
	if got := lipgloss.Height(view); got > 8 {
		t.Fatalf("task board height = %d, want <= 8\n%s", got, view)
	}
	if got := lipgloss.Width(view); got > 120 {
		t.Fatalf("task board width = %d, want <= 120\n%s", got, view)
	}
}

func TestTaskBoardLongSelectedGitErrorDoesNotOverflow(t *testing.T) {
	view := tuiapp.TaskBoardViewForTest(120, 18,
		[]tuiapp.TaskProject{{
			ID:        "TASK-20260508-771E08",
			TaskState: model.TaskInspecting,
			SyncError: "verify delivery package /home/purplevoid88/projects-qa/batch-1/TASK-20260508-771E08/TASK-20260508-771E08: stat /home/purplevoid88/projects-qa/batch-1/TASK-20260508-771E08/TASK-20260508-771E08: no such file or directory",
		}},
		nil,
		nil,
	)
	if got := lipgloss.Width(view); got > 120 {
		t.Fatalf("task board width = %d, want <= 120\n%s", got, view)
	}
	if got := lipgloss.Height(view); got > 18 {
		t.Fatalf("task board height = %d, want <= 18\n%s", got, view)
	}
}

func TestRunConfigDialogFitsNarrowShortViewport(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).
		SeedExecutionDetail("TASK-1").
		SetSize(70, 12)
	h, _ = h.Press("ctrl+r")
	view := h.View()
	if got := lipgloss.Width(view); got > 70 {
		t.Fatalf("run config width = %d, want <= 70\n%s", got, view)
	}
	if got := lipgloss.Height(view); got > 12 {
		t.Fatalf("run config height = %d, want <= 12\n%s", got, view)
	}
	if !strings.Contains(view, "运行配置") {
		t.Fatalf("run config dialog should remain visible:\n%s", view)
	}
}

func TestExecutionLeftKeepsSelectedStageVisibleOnSmallHeight(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).
		SeedExecutionDetail("TASK-1").
		SeedStages([]model.StageRecord{
			{Stage: "A", Status: model.StageDone},
			{Stage: "B", Status: model.StageDone},
			{Stage: "C", Status: model.StageDone},
			{Stage: "D", Status: model.StageDone},
			{Stage: "E", Status: model.StageDone},
			{Stage: "F", Status: model.StageFailed},
		}, "F").
		SetSize(120, 12)
	view := h.View()
	if got := lipgloss.Height(view); got > 12 {
		t.Fatalf("render height = %d, want <= 12\n%s", got, view)
	}
	if !strings.Contains(view, "↑") || !strings.Contains(view, "F") {
		t.Fatalf("small left panel should show scroll marker and selected Stage F:\n%s", view)
	}
}

func TestExecutionLeftCropsRefRunsAndKeepsSelectionVisible(t *testing.T) {
	runIDs := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		runIDs = append(runIDs, "run-"+string(rune('A'+i)))
	}
	h := tuiapp.NewTestHarness(config.Default()).
		SeedExecutionDetail("TASK-1").
		SeedRefRuns(runIDs...).
		SetFocus("ref-run-list").
		SetSize(120, 12)
	for i := 0; i < 18; i++ {
		h, _ = h.Press("down")
	}
	view := h.View()
	if got := lipgloss.Height(view); got > 12 {
		t.Fatalf("render height = %d, want <= 12\n%s", got, view)
	}
	if !strings.Contains(view, "run-S") {
		t.Fatalf("selected ref run should remain visible:\n%s", view)
	}
}

func titleSet(titles []string) map[string]bool {
	result := map[string]bool{}
	for _, title := range titles {
		result[title] = true
	}
	return result
}
