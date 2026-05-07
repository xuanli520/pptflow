package tui

import "strings"

type stagePlan struct {
	runStages     []string
	displayStages []string
	fromStage     string
	blockedReason string
}

func stagePlanForMode(mode, stage string, staticOnly bool, explicitStages map[string]bool, fromStage string) stagePlan {
	stage = strings.ToUpper(strings.TrimSpace(stage))
	if stage == "" {
		stage = "A"
	}
	fromStage = strings.ToUpper(strings.TrimSpace(fromStage))
	if len(explicitStages) > 0 && fromStage != "" {
		return stagePlan{blockedReason: "起始阶段和阶段多选不能同时使用"}
	}
	if fromStage != "" {
		stages := stagesFrom(fromStage)
		if len(stages) == 0 {
			return stagePlan{blockedReason: "未知起始阶段: " + fromStage}
		}
		if staticOnly && hasRuntimeStage(stages) {
			return stagePlan{blockedReason: "static-only 模式不能重跑 runtime 阶段 B/C"}
		}
		return stagePlan{displayStages: stages, fromStage: fromStage}
	}
	if len(explicitStages) > 0 {
		stages := selectedStageList(explicitStages)
		if staticOnly && hasRuntimeStage(stages) {
			return stagePlan{blockedReason: "static-only 模式不能重跑 runtime 阶段 B/C"}
		}
		return stagePlan{runStages: stages, displayStages: stages}
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
	if m.runConfig.active {
		return stagePlanForMode(m.runConfig.mode, m.rerunStageKey(), m.cfg.Pipeline.StaticOnly, m.runConfig.stages, m.runConfig.fromStage)
	}
	return stagePlanForMode(m.qaMode, m.rerunStageKey(), m.cfg.Pipeline.StaticOnly, nil, "")
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

func selectedStageList(selected map[string]bool) []string {
	var stages []string
	for _, stage := range []string{"A", "B", "C", "D", "E", "F"} {
		if selected[stage] || stage == "F" {
			stages = append(stages, stage)
		}
	}
	return stages
}

func stagesFrom(fromStage string) []string {
	fromStage = strings.ToUpper(strings.TrimSpace(fromStage))
	include := false
	var stages []string
	for _, stage := range []string{"A", "B", "C", "D", "E", "F"} {
		if stage == fromStage {
			include = true
		}
		if include {
			stages = append(stages, stage)
		}
	}
	return stages
}

func hasRuntimeStage(stages []string) bool {
	for _, stage := range stages {
		if stage == "B" || stage == "C" {
			return true
		}
	}
	return false
}
