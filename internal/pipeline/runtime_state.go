package pipeline

import (
	"encoding/json"
	"os"

	dockermgr "github.com/xuanli520/p2r_tui/internal/docker"
)

type RuntimeState = dockermgr.RuntimeState
type portMapping = dockermgr.PortMapping
type probeResult = dockermgr.ProbeResult

func readRuntimeState(path string) (RuntimeState, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return RuntimeState{}, err
	}
	var state RuntimeState
	if err := json.Unmarshal(content, &state); err != nil {
		return RuntimeState{}, err
	}
	state.Normalize()
	return state, nil
}

type runtimeEvidence = RuntimeState

func readRuntimeEvidence(path string) (runtimeEvidence, error) {
	return readRuntimeState(path)
}
