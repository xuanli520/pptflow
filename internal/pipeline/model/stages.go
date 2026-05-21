package model

import "strings"

type StageID string

const (
	StageA StageID = "A"
	StageB StageID = "B"
	StageC StageID = "C"
	StageD StageID = "D"
	StageE StageID = "E"
	StageF StageID = "F"
)

type StageSpec struct {
	ID      StageID
	Order   int
	Name    string
	Runtime bool
	Static  bool
	LogName string
}

var stageSpecs = []StageSpec{
	{ID: StageA, Order: 1, Name: "structure and rules check", Static: true, LogName: "A_validate.log"},
	{ID: StageD, Order: 2, Name: "tests effectiveness static review", Static: true, LogName: "D_tests_coverage_static.log"},
	{ID: StageE, Order: 3, Name: "static acceptance audit", Static: true, LogName: "E_static_audit.log"},
	{ID: StageF, Order: 4, Name: "annotator repair static review", Static: true, LogName: "F_repair.log"},
	{ID: StageB, Order: 5, Name: "Docker runtime evidence", Runtime: true, LogName: "B_docker.log"},
	{ID: StageC, Order: 6, Name: "run_tests runtime evidence", Runtime: true, LogName: "C_tests.log"},
}

var stageSpecByID = func() map[StageID]StageSpec {
	result := make(map[StageID]StageSpec, len(stageSpecs))
	for _, spec := range stageSpecs {
		result[spec.ID] = spec
	}
	return result
}()

func AllStageSpecs() []StageSpec {
	return append([]StageSpec(nil), stageSpecs...)
}

func AllStages() []string {
	stages := make([]string, 0, len(stageSpecs))
	for _, spec := range stageSpecs {
		stages = append(stages, string(spec.ID))
	}
	return stages
}

func ParseStageID(stage string) (StageID, bool) {
	id := StageID(strings.ToUpper(strings.TrimSpace(stage)))
	_, ok := stageSpecByID[id]
	return id, ok
}

func NormalizeStage(stage string) (string, bool) {
	id, ok := ParseStageID(stage)
	if !ok {
		return "", false
	}
	return string(id), true
}

func IsStageID(stage string) bool {
	_, ok := ParseStageID(stage)
	return ok
}

func StageDisplayName(stage string) string {
	id, ok := ParseStageID(stage)
	if !ok {
		return "unknown"
	}
	return stageSpecByID[id].Name
}

func StageLogName(stage string) string {
	id, ok := ParseStageID(stage)
	if !ok {
		return strings.ToUpper(strings.TrimSpace(stage)) + "_stage.log"
	}
	return stageSpecByID[id].LogName
}

func IsRuntimeStage(stage string) bool {
	id, ok := ParseStageID(stage)
	return ok && stageSpecByID[id].Runtime
}

func IsStaticStage(stage string) bool {
	id, ok := ParseStageID(stage)
	return ok && stageSpecByID[id].Static
}
