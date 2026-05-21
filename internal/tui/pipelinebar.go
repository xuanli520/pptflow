package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/scheduler"
)

type PipelineBarModel struct {
	MaxParallel int
	Jobs        []scheduler.JobSnapshot
	Width       int
}

func pipelineBarSnapshot(m app) PipelineBarModel {
	return PipelineBarModel{
		MaxParallel: normalizedMaxParallel(m.cfg.Pipeline.MaxConcurrent),
		Jobs:        append([]scheduler.JobSnapshot(nil), m.activeJobs...),
		Width:       m.width,
	}
}

func renderPipelineBarSnapshot(snapshot PipelineBarModel, now time.Time) string {
	maxParallel := normalizedMaxParallel(snapshot.MaxParallel)
	active := activeJobSnapshots(snapshot.Jobs)
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
	if len(active) == 0 || snapshot.Width < 72 {
		return mutedStyle.Render(line)
	}
	limit := pipelineJobLineLimit(snapshot)
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
		lines = append(lines, fmt.Sprintf("  %s %s  %s  %s", jobStateIcon(job.State), truncateMiddleDisplay(job.TaskID, 24), label, shortDurationAt(job, now)))
	}
	if len(active) > limit {
		lines = append(lines, mutedStyle.Render(fmt.Sprintf("  另有 %d 个 job 排队/运行", len(active)-limit)))
	}
	return strings.Join(lines, "\n")
}

func pipelineBarSnapshotHeight(snapshot PipelineBarModel) int {
	active := activeJobSnapshots(snapshot.Jobs)
	if len(active) == 0 || snapshot.Width < 72 {
		return 1
	}
	limit := pipelineJobLineLimit(snapshot)
	height := 1 + limit
	if len(active) > limit {
		height++
	}
	return height
}

func pipelineJobLineLimit(snapshot PipelineBarModel) int {
	active := activeJobSnapshots(snapshot.Jobs)
	if len(active) == 0 || snapshot.Width < 72 {
		return 0
	}
	return min(normalizedMaxParallel(snapshot.MaxParallel), len(active))
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
