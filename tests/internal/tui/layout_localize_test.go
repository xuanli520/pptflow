package tui_test

import (
	"testing"
	_ "unsafe"

	"github.com/charmbracelet/lipgloss"
	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	tuiapp "github.com/xuanli520/p2r_tui/internal/tui"
)

//go:linkname localizeRunStatus github.com/xuanli520/p2r_tui/internal/tui.localizeRunStatus
func localizeRunStatus(status string) string

//go:linkname localizeStageStatus github.com/xuanli520/p2r_tui/internal/tui.localizeStageStatus
func localizeStageStatus(status string) string

//go:linkname localizeManualVerdict github.com/xuanli520/p2r_tui/internal/tui.localizeManualVerdict
func localizeManualVerdict(verdict string) string

//go:linkname localizeSeverity github.com/xuanli520/p2r_tui/internal/tui.localizeSeverity
func localizeSeverity(severity string) string

//go:linkname localizeStageName github.com/xuanli520/p2r_tui/internal/tui.localizeStageName
func localizeStageName(stage, name string) string

//go:linkname localizeCleanupStatus github.com/xuanli520/p2r_tui/internal/tui.localizeCleanupStatus
func localizeCleanupStatus(status string) string

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
	for _, title := range []string{"任务ID", "状态", "失败", "阻断", "严重", "文档", "清理"} {
		if !titles[title] {
			t.Fatalf("medium columns missing %s: %#v", title, titles)
		}
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
		localizeRunStatus(model.RunCompletedClean):        "通过",
		localizeRunStatus(model.RunCompletedWithFindings): "有发现",
		localizeStageStatus(model.StageBlocked):           "已阻塞",
		localizeManualVerdict(model.ManualRework):         "返工",
		localizeSeverity("Blocker"):                       "阻断",
		localizeStageName("F", ""):                        "标注员修复静态审查",
		localizeCleanupStatus("none"):                     "未生成",
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
	if got := tuiapp.FooterForTest("search", true); got != "Enter/y 确认  Esc/n 取消" {
		t.Fatalf("confirm footer = %q", got)
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
}

func titleSet(titles []string) map[string]bool {
	result := map[string]bool{}
	for _, title := range titles {
		result[title] = true
	}
	return result
}
