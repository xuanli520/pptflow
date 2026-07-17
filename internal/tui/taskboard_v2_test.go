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
				LogPath: "/managed/logs/run-current.log", HasLog: true,
			},
			{ID: "run-previous", Status: "succeeded"},
		},
	})
	rendered := ansi.Strip(detail.View(132, 40))
	for _, expected := range []string{"来源", "当前运行", "失败原因", "运行记录", "/managed/logs/run-current.log", "stage.source_unavailable", "The source could not be read safely."} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("detail missing %q:\n%s", expected, rendered)
		}
	}
}

func TestDetailShowsDurableFailureRecordAndOnlyEligibleRedrive(t *testing.T) {
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
			FailureRecoveryAction: app.TaskBoardFailureRecoveryRedriveAuthoringHandoff,
			CanRedrive:            true,
		}},
	})
	rendered := ansi.Strip(detail.View(132, 40))
	for _, expected := range []string{"失败阶段", "错误码", "handoff.definition_unavailable", "Job ID", "job-handoff", "Artifact ID", "artifact-handoff", "记录时间", "恢复操作", "显式 redrive"} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("durable failure detail missing %q:\n%s", expected, rendered)
		}
	}

	detail.task.Runs[0].Status = "failed_terminal"
	detail.task.Runs[0].FailureCode = "handoff.artifact_lineage_invalid"
	detail.task.Runs[0].FailureRecoveryAction = app.TaskBoardFailureRecoveryRepairOrNewRun
	detail.task.Runs[0].CanRedrive = false
	rendered = ansi.Strip(detail.View(132, 40))
	if strings.Contains(rendered, "redrive") || !strings.Contains(rendered, "修复或新建运行") {
		t.Fatalf("terminal failure recovery detail =\n%s", rendered)
	}
}

func TestDetailDoesNotRenderLegacyRawFailureReason(t *testing.T) {
	detail := newDetailModel(&TaskItem{
		Name: "Task one", Slug: "task-one", RunID: "run-current",
		Runs: []TaskRunItem{{
			ID: "run-current", Status: "in_doubt", FailureStage: "materialize_task",
			FailureReason: "provider output from /private/handoff with sk-sensitive-token",
		}},
	})
	rendered := ansi.Strip(detail.View(100, 28))
	if strings.Contains(rendered, "private/handoff") || strings.Contains(rendered, "sk-sensitive-token") {
		t.Fatalf("detail rendered legacy raw failure reason:\n%s", rendered)
	}
}

func TestDetailLabelsAuthoringRecoveryWithoutCallingItGenericRetry(t *testing.T) {
	detail := newDetailModel(&TaskItem{
		Name: "Task one", Slug: "task-one", RunID: "run-current",
		Runs: []TaskRunItem{{
			ID: "run-current", Status: "failed_recoverable", CanRetry: true,
			RetryStrategy: app.TaskBoardRetryStrategyAuthoringRecovery,
		}},
	})
	rendered := ansi.Strip(detail.View(100, 28))
	if !strings.Contains(rendered, "恢复/重试") {
		t.Fatalf("authoring recovery label missing:\n%s", rendered)
	}
}

func TestLogModelScrollsBoundedContent(t *testing.T) {
	logs := newLogModel(&TaskItem{Name: "Task one"}, app.TaskBoardLog{
		RunID: "run-1", Path: "/managed/logs/run-1.log", Content: strings.Repeat("log line\n", 32),
	})
	logs.MoveDown(80, 14)
	if logs.offset != 1 {
		t.Fatalf("log offset after down = %d, want 1", logs.offset)
	}
	logs.PageDown(80, 14)
	if logs.offset <= 1 {
		t.Fatalf("log page down did not advance: %d", logs.offset)
	}
	logs.GoToEnd(80, 14)
	if logs.offset == 0 {
		t.Fatal("log end did not move through multi-line content")
	}
	rendered := ansi.Strip(logs.View(80, 14))
	if !strings.Contains(rendered, "/managed/logs/run-1.log") || !strings.Contains(rendered, "行 ") {
		t.Fatalf("log view lacks path or scroll position:\n%s", rendered)
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
