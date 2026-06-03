package tui_test

import (
	"strings"
	"testing"
	"time"

	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	tuiapp "github.com/xuanli520/p2r_tui/internal/tui"
)

func TestTaskCardShowsBatch(t *testing.T) {
	card := tuiapp.TaskCardForTest(tuiapp.TaskProject{
		ID:        "TASK-20260521-ABCDEF",
		BatchID:   "batch-007",
		TaskState: model.TaskInspecting,
	}, 64, time.Time{})
	if !strings.Contains(card, "TASK-20260521-ABCDEF [batch-007]") {
		t.Fatalf("task card should show batch:\n%s", card)
	}
}

func TestTaskCardShowsGitProgressAndRetryFailure(t *testing.T) {
	progress := tuiapp.TaskCardForTest(tuiapp.TaskProject{
		ID:          "TASK-20260521-ABCDEF",
		TaskState:   model.TaskInspecting,
		SyncPhase:   "clone",
		SyncPercent: 45,
	}, 34, time.Time{})
	if !strings.Contains(progress, "[Git 同步中] clone: 45%") {
		t.Fatalf("git progress card missing progress:\n%s", progress)
	}

	failed := tuiapp.TaskCardForTest(tuiapp.TaskProject{
		ID:        "TASK-20260521-ABCDEF",
		TaskState: model.TaskInspecting,
		SyncError: "network timeout",
	}, 34, time.Time{})
	if !strings.Contains(failed, "[Git 同步失败]") || !strings.Contains(failed, "Ctrl+W 重试") {
		t.Fatalf("git failure card missing retry affordance:\n%s", failed)
	}

	completedRetry := tuiapp.TaskCardForTest(tuiapp.TaskProject{
		ID:          "TASK-20260521-ABCDEF",
		TaskState:   model.TaskCompleted,
		SyncPhase:   "fetch",
		SyncPercent: -1,
	}, 34, time.Time{})
	if !strings.Contains(completedRetry, "[Git 同步中] fetch") || strings.Contains(completedRetry, "累计完成") {
		t.Fatalf("completed reinspection git progress should take over card:\n%s", completedRetry)
	}

	completedFailed := tuiapp.TaskCardForTest(tuiapp.TaskProject{
		ID:        "TASK-20260521-ABCDEF",
		TaskState: model.TaskCompleted,
		SyncError: "auth failed",
	}, 34, time.Time{})
	if !strings.Contains(completedFailed, "[Git 同步失败]") || !strings.Contains(completedFailed, "Ctrl+W 重试") {
		t.Fatalf("completed reinspection git failure should be retryable:\n%s", completedFailed)
	}
}

func TestTaskCardShowsGitErrorLogPathSeparately(t *testing.T) {
	card := tuiapp.TaskCardForTest(tuiapp.TaskProject{
		ID:        "TASK-20260521-ABCDEF",
		TaskState: model.TaskInspecting,
		SyncError: "git clone: exit status 128: fatal auth failed; 日志: /tmp/projects-qa/.qa-control/git-sync/TASK-20260521-ABCDEF.log",
	}, 54, time.Time{})
	if !strings.Contains(card, "[Git 同步失败]") || !strings.Contains(card, "日志:") {
		t.Fatalf("git failure card should expose error summary and log path:\n%s", card)
	}
	if got := len(strings.Split(card, "\n")); got != 4 {
		t.Fatalf("git failure card line count = %d, want 4:\n%s", got, card)
	}
}

func TestTaskCardShowsRunningProgressAndFailedStage(t *testing.T) {
	running := tuiapp.TaskCardForTest(tuiapp.TaskProject{
		ID:            "TASK-20260521-ABCDEF",
		TaskState:     model.TaskInspecting,
		RunStatus:     model.RunRunning,
		CurrentStage:  "E",
		CurrentStatus: model.StageRunning,
	}, 38, time.Time{})
	if !strings.Contains(running, "E: 静态验收审计") || !strings.Contains(running, "%") {
		t.Fatalf("running card missing stage/progress:\n%s", running)
	}

	failed := tuiapp.TaskCardForTest(tuiapp.TaskProject{
		ID:            "TASK-20260521-ABCDEF",
		TaskState:     model.TaskInspecting,
		FailedStage:   "D",
		FailedSummary: "codex unavailable",
		CurrentStatus: model.StageFailed,
	}, 38, time.Time{})
	if !strings.Contains(failed, "D: 测试有效性静态审查") || !strings.Contains(failed, "✗ 失败: Codex 不可用") {
		t.Fatalf("failed card missing stage detail:\n%s", failed)
	}
}

func TestTaskCardCurrentStageOverridesPreviousFailure(t *testing.T) {
	card := tuiapp.TaskCardForTest(tuiapp.TaskProject{
		ID:            "TASK-20260521-ABCDEF",
		TaskState:     model.TaskInspecting,
		RunStatus:     model.RunRunning,
		CurrentStage:  "D",
		CurrentStatus: model.StageRunning,
		FailedStage:   "A",
		FailedSummary: "run_validate.py exited with code 1",
	}, 38, time.Time{})
	if !strings.Contains(card, "D: 测试有效性静态审查") || strings.Contains(card, "✗ 失败") {
		t.Fatalf("running stage should override previous failure:\n%s", card)
	}
}

func TestTaskCardShowsWaitingDockerVariants(t *testing.T) {
	now := time.Date(2026, 5, 21, 15, 5, 0, 0, time.UTC)
	start := now.Add(-2*time.Minute - 35*time.Second).Format(time.RFC3339)
	running := tuiapp.TaskCardForTest(tuiapp.TaskProject{
		ID:               "TASK-20260521-ABCDEF",
		TaskState:        model.TaskWaitingManual,
		DockerRunning:    true,
		FrontendURL:      "http://localhost:30080",
		EnteredWaitingAt: start,
	}, 42, now)
	if !strings.Contains(running, "http://localhost:30080") || !strings.Contains(running, "等待: 02:35") {
		t.Fatalf("waiting running card incorrect:\n%s", running)
	}

	partial := tuiapp.TaskCardForTest(tuiapp.TaskProject{
		ID:               "TASK-20260521-ABCDEF",
		TaskState:        model.TaskWaitingManual,
		DockerRunning:    true,
		EnteredWaitingAt: start,
	}, 42, now)
	if !strings.Contains(partial, "! Docker 已启动，端口检测失败") {
		t.Fatalf("waiting partial card incorrect:\n%s", partial)
	}

	failed := tuiapp.TaskCardForTest(tuiapp.TaskProject{
		ID:               "TASK-20260521-ABCDEF",
		TaskState:        model.TaskWaitingManual,
		EnteredWaitingAt: start,
	}, 42, now)
	if !strings.Contains(failed, "✗ Docker 启动失败") {
		t.Fatalf("waiting failure card incorrect:\n%s", failed)
	}

	late := tuiapp.TaskCardForTest(tuiapp.TaskProject{
		ID:               "TASK-20260521-ABCDEF",
		TaskState:        model.TaskWaitingManual,
		DockerRunning:    true,
		FrontendURL:      "http://localhost:30080",
		EnteredWaitingAt: now.Add(-31 * time.Minute).Format(time.RFC3339),
	}, 42, now)
	if !strings.Contains(late, "⏱ 等待: 31:00") {
		t.Fatalf("late waiting card should include redundant timer marker:\n%s", late)
	}
}

func TestSelectedTaskCardUsesGradientRule(t *testing.T) {
	card := tuiapp.SelectedTaskCardForTest(tuiapp.TaskProject{
		ID:              "TASK-20260521-ABCDEF",
		TaskState:       model.TaskCompleted,
		CompletionCount: 1,
	}, 42, time.Time{})
	if strings.Contains(card, "█") || !strings.Contains(card, "─") {
		t.Fatalf("selected card should use a gradient rule, not a block bar:\n%s", card)
	}
	if count := strings.Count(card, "─"); count != 16 {
		t.Fatalf("selected card gradient rule width = %d, want 16:\n%s", count, card)
	}
}

func TestCompletedTaskCardScrollsSummaryWhenNarrow(t *testing.T) {
	task := tuiapp.TaskProject{
		ID:              "TASK-20260521-ABCDEF",
		TaskState:       model.TaskCompleted,
		CompletionCount: 3,
		LastCompletedAt: "2026-05-24T12:58:00Z",
	}
	first := tuiapp.TaskCardForTest(task, 30, time.Unix(0, 0))
	next := tuiapp.TaskCardForTest(task, 30, time.Unix(3, 0))
	var frames string
	for second := int64(0); second < 18; second += 3 {
		frames += tuiapp.TaskCardForTest(task, 30, time.Unix(second, 0))
	}

	if strings.Contains(first, "…") || strings.Contains(next, "…") {
		t.Fatalf("completed summary should scroll instead of truncating:\n%s\n---\n%s", first, next)
	}
	if first == next {
		t.Fatalf("completed summary should advance with time:\n%s", first)
	}
	if !strings.Contains(frames, "累计完成: 3 次") || !strings.Contains(frames, "最后: 2026-05-24 20:58") {
		t.Fatalf("scrolling summary should expose both parts:\n%s", frames)
	}
}
