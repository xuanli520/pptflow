package tui

import "strings"

type stagePlan struct {
	runStages     []string
	displayStages []string
	blockedReason string
}

func stagePlanForMode(mode, stage string, staticOnly bool) stagePlan {
	stage = strings.ToUpper(strings.TrimSpace(stage))
	if stage == "" {
		stage = "A"
	}
	if mode == "recheck" {
		if staticOnly && (stage == "B" || stage == "C") {
			return stagePlan{blockedReason: "static-only 模式不能重跑 runtime 阶段 B/C"}
		}
		stages := affectedStages(stage)
		return stagePlan{runStages: stages, displayStages: stages}
	}
	if staticOnly {
		return stagePlan{displayStages: []string{"A", "D", "E", "F"}}
	}
	return stagePlan{displayStages: []string{"A", "B", "C", "D", "E", "F"}}
}

func (m app) rerunStagePlan() stagePlan {
	return stagePlanForMode(m.qaMode, m.rerunStageKey(), m.cfg.Pipeline.StaticOnly)
}

func (m app) rerunStageKey() string {
	stage := m.selectedStage()
	stageKey := stage.Stage
	if stageKey == "" {
		stageKey = m.selectedStageKey
	}
	if stageKey == "" {
		stageKey = stageLetter(m.stageIndex)
	}
	return stageKey
}
