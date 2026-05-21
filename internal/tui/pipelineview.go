package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/scheduler"
)

func renderPipelineBar(m app) string {
	maxParallel := normalizedMaxParallel(m.cfg.Pipeline.MaxConcurrent)
	active := activeJobSnapshots(m.activeJobs)
	running := 0
	queued := 0
	for _, job := range active {
		if job.State == scheduler.JobRunning {
			running++
		}
		if job.State == scheduler.JobQueued {
			queued++
		}
	}
	bar := slotBar(running, maxParallel)
	line := fmt.Sprintf("流水线: %s %d/%d", bar, running, maxParallel)
	if queued > 0 {
		line += fmt.Sprintf("  排队: %d", queued)
	}
	if len(active) == 0 || m.width < 72 {
		return mutedStyle.Render(line)
	}
	limit := pipelineJobLineLimit(m)
	lines := []string{mutedStyle.Render(line)}
	for _, job := range active[:limit] {
		label := localizeJobState(job.State)
		if job.CancelRequested {
			label = "终止中"
		} else if job.Kind == scheduler.JobGitSync {
			label = "Git 同步"
			if job.SyncProgress.Phase != "" {
				label = "Git " + job.SyncProgress.Phase
			}
		} else if job.CurrentStage != "" {
			label = "阶段" + job.CurrentStage + " " + localizeStageName(job.CurrentStage, "")
		}
		lines = append(lines, fmt.Sprintf("  %s %s  %s  %s", jobStateIcon(job.State), truncateMiddleDisplay(job.TaskID, 24), label, shortDuration(job)))
	}
	if len(active) > limit {
		lines = append(lines, mutedStyle.Render(fmt.Sprintf("  另有 %d 个 job 排队/运行", len(active)-limit)))
	}
	return strings.Join(lines, "\n")
}

func pipelineBarHeight(m app) int {
	active := activeJobSnapshots(m.activeJobs)
	if len(active) == 0 || m.width < 72 {
		return 1
	}
	limit := pipelineJobLineLimit(m)
	height := 1 + limit
	if len(active) > limit {
		height++
	}
	return height
}

func pipelineJobLineLimit(m app) int {
	active := activeJobSnapshots(m.activeJobs)
	if len(active) == 0 || m.width < 72 {
		return 0
	}
	return min(normalizedMaxParallel(m.cfg.Pipeline.MaxConcurrent), len(active))
}

func activeJobSnapshots(jobs []scheduler.JobSnapshot) []scheduler.JobSnapshot {
	var active []scheduler.JobSnapshot
	for _, job := range jobs {
		if job.State == scheduler.JobQueued || job.State == scheduler.JobRunning {
			active = append(active, job)
		}
	}
	return active
}

func slotBar(running, maxParallel int) string {
	maxParallel = normalizedMaxParallel(maxParallel)
	running = clamp(running, 0, maxParallel)
	var builder strings.Builder
	builder.WriteString("[")
	for i := 0; i < maxParallel; i++ {
		if i < running {
			builder.WriteString("█")
		} else {
			builder.WriteString("░")
		}
	}
	builder.WriteString("]")
	return builder.String()
}

func normalizedMaxParallel(value int) int {
	return config.NormalizeMaxConcurrent(value)
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
	end := time.Now()
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
