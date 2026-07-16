package tui

import (
	"strings"
	"testing"

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
				FailureStage: "repo_prepare", FailureClass: "network", FailureReason: "network timeout while fetching source",
				LogPath: "/managed/logs/run-current.log", HasLog: true,
			},
			{ID: "run-previous", Status: "succeeded"},
		},
	})
	rendered := ansi.Strip(detail.View(132, 40))
	for _, expected := range []string{"来源", "当前运行", "失败原因", "运行记录", "/managed/logs/run-current.log", "network timeout"} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("detail missing %q:\n%s", expected, rendered)
		}
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
			FailureReason: "a failure message that should wrap instead of stretching the terminal layout", LogPath: "/managed/logs/run-current.log",
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
