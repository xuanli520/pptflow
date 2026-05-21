package tui

import "github.com/xuanli520/p2r_tui/internal/scheduler"

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
