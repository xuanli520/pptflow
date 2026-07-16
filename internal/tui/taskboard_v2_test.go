package tui

import (
	"strings"
	"testing"
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
