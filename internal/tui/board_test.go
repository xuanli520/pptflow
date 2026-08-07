package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/purplevoid/harbor-factory/internal/app"
)

func TestRenderColumnFitsWholeCardsWithinItsHeight(t *testing.T) {
	items := make([]TaskItem, 8)
	for index := range items {
		items[index] = TaskItem{Name: "Task", State: TaskPending}
	}
	const height = 20
	rendered := renderColumn(items, 30, height, true, 7)
	if lines := strings.Count(rendered, "\n") + 1; lines > height {
		t.Fatalf("rendered %d lines into a %d-line column:\n%s", lines, height, rendered)
	}
}

func TestRenderColumnSupportsNarrowTerminalWidths(t *testing.T) {
	items := []TaskItem{{Name: "Task", RepoURL: "https://example.invalid/repository", CommitSHA: "abcdef", State: TaskPending}}
	if rendered := renderColumn(items, 4, 6, true, 0); rendered == "" {
		t.Fatal("narrow column rendered no task")
	}
}

func TestThreeColumnHeadersAlignWithCardGridAndEmptyColumnsStayBlank(t *testing.T) {
	for _, width := range []int{90, 120, 180} {
		board := NewTaskBoardModel()
		board.SetTasks([]TaskItem{{Name: "Task", State: TaskPending}}, nil, nil)
		rendered := strings.Split(ansi.Strip(board.View(width, 16)), "\n")
		if len(rendered) < 2 {
			t.Fatalf("width %d rendered too few lines: %q", width, rendered)
		}
		headers := separatorColumns(rendered[0])
		body := separatorColumns(rendered[1])
		if len(headers) != 2 || len(body) != 2 {
			t.Fatalf("width %d separators: headers=%v body=%v\n%s", width, headers, body, strings.Join(rendered, "\n"))
		}
		if headers[0] != body[0] || headers[1] != body[1] {
			t.Fatalf("width %d header separators %v do not align with body %v", width, headers, body)
		}
		if strings.Contains(strings.Join(rendered, "\n"), "暂无题目") {
			t.Fatalf("width %d rendered text in an empty column", width)
		}
	}
}

func TestDetailGroupsCurrentRunHistoryFailureAndLogPath(t *testing.T) {
	detail := newDetailModel(&TaskItem{
		Name:      "Task one",
		Slug:      "task-one",
		RepoURL:   "https://example.invalid/repo.git",
		CommitSHA: "abcdef0123456789",
		RunID:     "run-current",
		Runs: []TaskRunItem{
			{
				ID: "run-current", Status: "failed_recoverable", CurrentStage: "repo_prepare",
				FailureStage: "repo_prepare", FailureCode: "stage.source_unavailable", FailureSummary: "The source could not be read safely.",
				LogPath: "/managed/logs/run-current.log",
			},
			{ID: "run-previous", Status: "succeeded"},
		},
	})
	// The default body carries decision-necessary facts only: that a log exists
	// and how to open it. The managed path itself is a diagnostic identifier and
	// lives behind [e], so a short terminal spends no rows on it.
	rendered := ansi.Strip(detail.View(132, 40))
	for _, expected := range []string{"来源", "当前运行", "失败原因", "运行记录", "可读取（按 l）", "stage.source_unavailable", "The source could not be read safely."} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("detail missing %q:\n%s", expected, rendered)
		}
	}
	if strings.Contains(rendered, "/managed/logs/run-current.log") {
		t.Fatalf("collapsed detail spent rows on the managed log path:\n%s", rendered)
	}

	// Expanding must still reach it: collapsing is a layout decision, never a
	// reason for a durable identifier to become unreachable from the terminal.
	detail.ToggleEvidence()
	expanded := ansi.Strip(detail.View(132, 40))
	if !strings.Contains(expanded, "/managed/logs/run-current.log") {
		t.Fatalf("expanded detail lost the managed log path:\n%s", expanded)
	}
}

func TestDetailShowsBoundedAuthoringEvidence(t *testing.T) {
	detail := newDetailModel(&TaskItem{
		Name: "Task one", Slug: "task-one", RunID: "run-current",
		Runs: []TaskRunItem{{
			ID: "run-current", Status: "running",
			AuthoringEvidence: &app.TaskBoardAuthoringEvidence{
				Contract: app.TaskBoardAuthoringContract{
					Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
					Title:  "Task one", Slug: "task-one", CodeLang: "go", TaskType: "bugfix", Application: "backend",
					RepositoryURL: "https://example.invalid/repo.git", CommitSHA: "abcdef0123456789",
					SnapshotDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
					BaseImage:      "docker.io/library/golang:1.26@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
					Objective:      "Repair the selected behavior.",
				},
				Claims:  []app.TaskBoardAuthoringClaim{{ArtifactKey: "task_specification", State: "match"}},
				Lineage: []app.TaskBoardAuthoringArtifact{{ArtifactKey: "candidate_snapshot", ArtifactID: "artifact-candidate", Digest: "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}},
			},
		}},
	})
	// Evidence is audit data, not decision data, so the default body omits it.
	if collapsed := ansi.Strip(detail.View(180, 60)); strings.Contains(collapsed, "根契约") {
		t.Fatalf("collapsed detail rendered contract evidence:\n%s", collapsed)
	}
	detail.ToggleEvidence()
	rendered := ansi.Strip(detail.View(180, 60))
	for _, expected := range []string{"根契约", "声明比对", "最终谱系", "Task one", "task_specification"} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("detail missing %q:\n%s", expected, rendered)
		}
	}
}

func TestDetailShowsDurableFailureRecordAndRecoveryAction(t *testing.T) {
	recordedAt := time.Date(2026, time.July, 17, 10, 20, 0, 0, time.UTC)
	detail := newDetailModel(&TaskItem{
		Name: "Task one", Slug: "task-one", RunID: "run-current",
		Runs: []TaskRunItem{{
			ID:                    "run-current",
			Status:                "in_doubt",
			FailureStage:          "materialize_task",
			FailureCode:           "handoff.definition_unavailable",
			FailureSummary:        "The approved child definition is unavailable.",
			FailureJobID:          "job-handoff",
			FailureArtifactID:     "artifact-handoff",
			FailureRecordedAt:     &recordedAt,
			FailureRecoveryAction: app.TaskBoardFailureRecoveryReconcile,
		}},
	})
	// The failure section keeps what an operator decides on: which stage failed,
	// the closed failure code, when it was recorded, and the recovery action.
	rendered := ansi.Strip(detail.View(132, 40))
	for _, expected := range []string{"失败阶段", "错误码", "handoff.definition_unavailable", "记录时间", "恢复操作", "显式 reconcile"} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("durable failure detail missing %q:\n%s", expected, rendered)
		}
	}
	// The durable Job and Artifact IDs are correlation identifiers for the store,
	// not decision inputs, so they move behind [e] rather than being dropped.
	detail.ToggleEvidence()
	expanded := ansi.Strip(detail.View(132, 40))
	for _, expected := range []string{"Job ID", "job-handoff", "Artifact ID", "artifact-handoff"} {
		if !strings.Contains(expanded, expected) {
			t.Fatalf("expanded detail missing failure identifier %q:\n%s", expected, expanded)
		}
	}
	detail.ToggleEvidence()

	detail.task.Runs[0].Status = "failed_terminal"
	detail.task.Runs[0].FailureCode = "handoff.artifact_lineage_invalid"
	detail.task.Runs[0].FailureRecoveryAction = app.TaskBoardFailureRecoveryRepairOrNewRun
	rendered = ansi.Strip(detail.View(132, 40))
	if strings.Contains(rendered, "redrive") || !strings.Contains(rendered, "run restart 重跑") {
		t.Fatalf("terminal failure recovery detail =\n%s", rendered)
	}
	detail.task.Runs[0].CanRetry = true
	rendered = ansi.Strip(detail.View(132, 40))
	if !strings.Contains(rendered, "按 t 断点恢复") {
		t.Fatalf("recoverable failure recovery detail =\n%s", rendered)
	}
}

// TestDetailDoesNotRenderLegacyRawFailureReason pins the boundary rather than a
// field: the app-layer Run keeps FailureReason for legacy compatibility, but the
// terminal projection must not carry it across, so raw provider output and any
// credential inside it can never reach a rendered screen.
func TestDetailDoesNotRenderLegacyRawFailureReason(t *testing.T) {
	pending, _, _ := taskItemsForSnapshot(app.TaskBoardSnapshot{Tasks: []app.TaskBoardTask{{
		ID: "task-1", Title: "Task one", Column: app.TaskBoardPending, RunID: "run-current",
		Runs: []app.TaskBoardRun{{
			ID: "run-current", Status: "in_doubt", FailureStage: "materialize_task",
			FailureReason: "provider output from /private/handoff with sk-sensitive-token",
		}},
	}}})
	if len(pending) != 1 {
		t.Fatalf("task items = %+v", pending)
	}
	detail := newDetailModel(&pending[0])
	detail.ToggleEvidence()
	rendered := ansi.Strip(detail.View(100, 28))
	if strings.Contains(rendered, "private/handoff") || strings.Contains(rendered, "sk-sensitive-token") {
		t.Fatalf("detail rendered legacy raw failure reason:\n%s", rendered)
	}
}

func TestDetailOffersTaskContinuationRecovery(t *testing.T) {
	detail := newDetailModel(&TaskItem{
		Name: "Task one", Slug: "task-one", RunID: "run-current",
		Runs: []TaskRunItem{{
			ID: "run-current", Status: "failed_recoverable", CanRetry: true,
			RetryStrategy: app.TaskBoardRetryStrategyTaskContinuation,
			FailureCode:   "stage.transient_failure",
		}},
	})
	rendered := ansi.Strip(detail.View(100, 28))
	if !strings.Contains(rendered, "断点恢复") {
		t.Fatalf("task continuation label missing:\n%s", rendered)
	}
	if footer := detailFooterText(detail); !strings.Contains(footer, "[t] 断点恢复") {
		t.Fatalf("task continuation action missing from footer: %q", footer)
	}
}

// TestLogModelScrollsRawContent covers the fallback path: a log with no parsed
// records still scrolls as plain text and reports a line position.
func TestLogModelScrollsRawContent(t *testing.T) {
	logs := newLogModel(&TaskItem{Name: "Task one"}, app.TaskBoardLog{
		RunID: "run-1", Path: "/managed/logs/run-1.log", Content: strings.Repeat("log line\n", 32),
	})
	// Render once so the pane learns its size before scrolling.
	logs.View(80, 14)
	first := logs.pane.FirstVisibleLine()
	logs.MoveDown()
	logs.View(80, 14)
	if logs.pane.FirstVisibleLine() != first+1 {
		t.Fatalf("raw log first visible line after down = %d, want %d", logs.pane.FirstVisibleLine(), first+1)
	}
	logs.GoToEnd()
	logs.View(80, 14)
	if logs.pane.LastVisibleLine() != logs.pane.LineCount() {
		t.Fatalf("raw log end = %d, want %d", logs.pane.LastVisibleLine(), logs.pane.LineCount())
	}
	rendered := ansi.Strip(logs.View(80, 14))
	if !strings.Contains(rendered, "/managed/logs/run-1.log") || !strings.Contains(rendered, "行 ") {
		t.Fatalf("log view lacks path or scroll position:\n%s", rendered)
	}
}

// TestLogModelRendersRecordSummaries pins the readability fix: a worker log is
// shown as one line per durable record, and the raw JSON stays behind [R].
func TestLogModelRendersRecordSummaries(t *testing.T) {
	recorded := time.Date(2026, 8, 7, 9, 15, 0, 0, time.UTC)
	logs := newLogModel(&TaskItem{Name: "Task one"}, app.TaskBoardLog{
		RunID: "run-1", Path: "/managed/logs/run-1.log",
		Content: `{"format":"harbor.run-worker-log-record.v1"}`,
		Records: []app.TaskBoardLogRecord{
			{Sequence: 1, ObservedAt: &recorded, RunStatus: "running", JobCommandType: "stage_attempt.execute", JobState: "succeeded"},
			{Sequence: 2, ObservedAt: &recorded, RunStatus: "waiting_review", StoppedFor: "waiting_review", CycleEmpty: true},
		},
	})
	rendered := ansi.Strip(logs.View(100, 20))
	for _, want := range []string{"stage_attempt.execute", "stopped_for:waiting_review", "记录 2 / 2"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("record view missing %q:\n%s", want, rendered)
		}
	}
	if strings.Contains(rendered, "harbor.run-worker-log-record.v1") {
		t.Fatalf("record view leaked raw JSON:\n%s", rendered)
	}
	// The newest record is selected so the operator lands on what just happened.
	if logs.selected != 1 {
		t.Fatalf("selected record = %d, want the newest", logs.selected)
	}
	logs.ToggleRaw()
	if raw := ansi.Strip(logs.View(100, 20)); !strings.Contains(raw, "harbor.run-worker-log-record.v1") {
		t.Fatalf("raw fallback did not expose the original bytes:\n%s", raw)
	}
}

func TestDetailAndLogRespectTerminalWidths(t *testing.T) {
	detail := newDetailModel(&TaskItem{
		Name: "A task with a deliberately long title", Slug: "long-task", RepoURL: "https://example.invalid/organization/repository.git",
		CommitSHA: "abcdef0123456789abcdef0123456789abcdef0123456789", RunID: "run-current",
		Runs: []TaskRunItem{{
			ID: "run-current", Status: "failed_recoverable", CurrentStage: "repository_preparation",
			FailureCode: "stage.input_invalid", FailureSummary: "A durable failure summary that should wrap instead of stretching the terminal layout.", LogPath: "/managed/logs/run-current.log",
		}},
	})
	for _, width := range []int{80, 120} {
		assertRenderedWidthAtMost(t, detail.View(width, 32), width)
		logs := newLogModel(detail.task, app.TaskBoardLog{
			RunID: "run-current", Path: "/managed/logs/run-current.log", Content: strings.Repeat("a long log line that must wrap inside the viewport\n", 12),
		})
		assertRenderedWidthAtMost(t, logs.View(width, 18), width)
	}
}

func separatorColumns(line string) []int {
	var columns []int
	for index, value := range line {
		if value == '│' {
			columns = append(columns, lipgloss.Width(line[:index]))
		}
	}
	return columns
}

func assertRenderedWidthAtMost(t *testing.T, rendered string, width int) {
	t.Helper()
	for _, line := range strings.Split(ansi.Strip(rendered), "\n") {
		if actual := lipgloss.Width(line); actual > width {
			t.Fatalf("rendered line width %d exceeds terminal width %d: %q", actual, width, line)
		}
	}
}
