package pipeline

import (
	"encoding/json"
	"os"
	"strings"
)

type RuntimeState struct {
	ComposeProject string                   `json:"compose_project"`
	ComposeFile    string                   `json:"compose_file"`
	WorkDir        string                   `json:"work_dir"`
	Services       []string                 `json:"services"`
	Mappings       map[string][]portMapping `json:"mappings"`
	Probes         []probeResult            `json:"probes"`
}

func (s RuntimeState) HasCleanupTarget() bool {
	return strings.TrimSpace(s.ComposeProject) != ""
}

func (s RuntimeState) HasServiceMappings() bool {
	for _, mappings := range s.Mappings {
		if len(mappings) > 0 {
			return true
		}
	}
	return false
}

func readRuntimeState(path string) (RuntimeState, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return RuntimeState{}, err
	}
	var state RuntimeState
	if err := json.Unmarshal(content, &state); err != nil {
		return RuntimeState{}, err
	}
	return state, nil
}

type runtimeEvidence = RuntimeState

func readRuntimeEvidence(path string) (runtimeEvidence, error) {
	return readRuntimeState(path)
}
