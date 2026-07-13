package tui

import (
	"fmt"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
)

func renderChecklist(items []domain.ChecklistItem) string {
	if len(items) == 0 {
		return subtleStyle.Render("暂无检查项")
	}
	lines := make([]string, 0, len(items))
	for _, item := range items {
		status := "failed"
		if item.Passed {
			status = "succeeded"
		}
		critical := ""
		if item.Critical {
			critical = " " + warnStyle.Render("[严重]")
		}
		lines = append(lines, fmt.Sprintf("%s %s%s", statusIcon(status), redactSingleLineUI(item.Label), critical))
	}
	return strings.Join(lines, "\n")
}

func filterChecklist(items []domain.ChecklistItem, filter string) []domain.ChecklistItem {
	if strings.TrimSpace(filter) == "" {
		return items
	}
	filtered := make([]domain.ChecklistItem, 0, len(items))
	for _, item := range items {
		if matchesFilter(filter, item.ID, item.Label) {
			filtered = append(filtered, item)
		}
	}
	return filtered
}
