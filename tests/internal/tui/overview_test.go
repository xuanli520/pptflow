package tui_test

import (
	"strings"
	"testing"

	"github.com/xuanli520/p2r_tui/internal/config"
	tuiapp "github.com/xuanli520/p2r_tui/internal/tui"
)

func TestOverviewSearchDebounceIgnoresStaleMessages(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).SetFocus("search")
	h, _ = h.Press("T")
	staleSeq := h.SearchSeq()
	h, _ = h.Press("A")

	next, hasCmd := h.ApplySearchDebounceForTest(staleSeq, "T")
	if hasCmd {
		t.Fatal("stale debounce should not issue a load")
	}
	if next.OverviewSeq() != h.OverviewSeq() {
		t.Fatal("stale debounce should not mutate load seq")
	}

	next, hasCmd = h.ApplySearchDebounceForTest(h.SearchSeq(), "TA")
	if !hasCmd || next.PageCurrent() != 1 || next.OverviewSeq() != h.OverviewSeq()+1 {
		t.Fatalf("latest debounce should reset page and issue load, cmd=%v page=%d seq=%d", hasCmd, next.PageCurrent(), next.OverviewSeq())
	}
}

func TestOverviewSortKeysCycleAndReverse(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).SeedOverview("TASK-1").SetFocus("overview-table")

	next, result := h.Press("s")
	if result.CmdCount == 0 || next.SortName() != "status" || next.SortAsc() {
		t.Fatalf("s should switch to status desc, cmd=%d sort=%s asc=%v", result.CmdCount, next.SortName(), next.SortAsc())
	}

	next, result = next.Press("S")
	if result.CmdCount == 0 || next.SortName() != "status" || !next.SortAsc() {
		t.Fatalf("S should reverse status only, cmd=%d sort=%s asc=%v", result.CmdCount, next.SortName(), next.SortAsc())
	}
}

func TestOverviewPaginationKeysAndBoundaryNoops(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).
		SeedOverview("TASK-1", "TASK-2").
		SetOverviewPage(1, 10, 25).
		SetFocus("overview-table")

	next, result := h.Press("pgdown")
	if result.CmdCount == 0 || next.PageCurrent() != 2 {
		t.Fatalf("pgdown should load page 2, cmd=%d page=%d", result.CmdCount, next.PageCurrent())
	}
	next, result = next.Press("pgup")
	if result.CmdCount == 0 || next.PageCurrent() != 1 {
		t.Fatalf("pgup should load page 1, cmd=%d page=%d", result.CmdCount, next.PageCurrent())
	}
	next, result = next.Press("pgup")
	if result.CmdCount != 0 || next.PageCurrent() != 1 {
		t.Fatalf("pgup on first page should be no-op, cmd=%d page=%d", result.CmdCount, next.PageCurrent())
	}
}

func TestOverviewArrowKeysTurnPagesAtEdges(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).
		SeedOverview("TASK-11", "TASK-12").
		SetOverviewPage(2, 10, 25).
		SetOverviewCursor(0).
		SetFocus("overview-table")

	next, result := h.Press("up")
	if result.CmdCount == 0 || next.PageCurrent() != 1 {
		t.Fatalf("up at first row should load previous page, cmd=%d page=%d", result.CmdCount, next.PageCurrent())
	}

	h = tuiapp.NewTestHarness(config.Default()).
		SeedOverview("TASK-11", "TASK-12").
		SetOverviewPage(2, 10, 25).
		SetOverviewCursor(1).
		SetFocus("overview-table")
	next, result = h.Press("down")
	if result.CmdCount == 0 || next.PageCurrent() != 3 {
		t.Fatalf("down at last row should load next page, cmd=%d page=%d", result.CmdCount, next.PageCurrent())
	}
}

func TestOverviewIgnoresStaleResultAndClampsOutOfRangePage(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).
		SeedOverview("TASK-CURRENT").
		SetOverviewPage(3, 10, 30).
		SetFocus("overview-table")
	h, _ = h.ApplyOverviewRefreshForTest()
	currentSeq := h.OverviewSeq()

	next, hasCmd := h.ApplyOverviewResultForTest(currentSeq-1, 30, "TASK-STALE")
	if hasCmd || next.SelectedTaskID() != "TASK-CURRENT" {
		t.Fatalf("stale result should be ignored, cmd=%v selected=%s", hasCmd, next.SelectedTaskID())
	}

	next, hasCmd = h.ApplyOverviewResultForTest(currentSeq, 5)
	if !hasCmd || next.PageCurrent() != 1 {
		t.Fatalf("out-of-range result should clamp and reload, cmd=%v page=%d", hasCmd, next.PageCurrent())
	}
}

func TestOverviewSearchFocusKeepsCommandRunesAsInput(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).SetFocus("search")
	for _, key := range []string{"s", "S", "z"} {
		var result tuiapp.TestKeyResult
		h, result = h.Press(key)
		if result.CmdCount == 0 {
			t.Fatalf("%s in search should still go through text input debounce", key)
		}
	}
	if got := h.SearchValue(); got != "sSz" {
		t.Fatalf("search value = %q, want sSz", got)
	}
}

func TestOverviewFooterMentionsSortingAndPaging(t *testing.T) {
	footer := tuiapp.FooterForTest("overview-table", false)
	for _, want := range []string{"s排序", "PgUp/PgDn翻页", "z条数"} {
		if !strings.Contains(footer, want) {
			t.Fatalf("footer missing %q: %s", want, footer)
		}
	}
}
