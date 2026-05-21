package tui

import (
	"fmt"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/xuanli520/p2r_tui/internal/scheduler"
)

func renderPipelineBar(m app) string {
	return renderPipelineBarSnapshot(pipelineBarSnapshot(m), time.Now())
}

func pipelineBarHeight(m app) int {
	return pipelineBarSnapshotHeight(pipelineBarSnapshot(m))
}

func jobStateIcon(state scheduler.JobState) string {
	switch state {
	case scheduler.JobQueued:
		return mutedStyle.Render("○")
	case scheduler.JobRunning:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#4488FF")).Render("▶")
	case scheduler.JobDone:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#00CC66")).Render("✓")
	case scheduler.JobCancelled:
		return mutedStyle.Render("×")
	case scheduler.JobFailed:
		return errorStyle.Render("✗")
	default:
		return "?"
	}
}

func shortDuration(job scheduler.JobSnapshot) string {
	return shortDurationAt(job, time.Now())
}

func shortDurationAt(job scheduler.JobSnapshot, now time.Time) string {
	if job.State == scheduler.JobQueued {
		return "排队中"
	}
	start := job.StartedAt
	if start.IsZero() {
		start = job.SubmittedAt
	}
	if start.IsZero() {
		return "00:00"
	}
	end := now
	if !job.FinishedAt.IsZero() {
		end = job.FinishedAt
	}
	elapsed := end.Sub(start)
	if elapsed < 0 {
		elapsed = 0
	}
	minutes := int(elapsed.Minutes())
	seconds := int(elapsed.Seconds()) % 60
	return fmt.Sprintf("%02d:%02d", minutes, seconds)
}
