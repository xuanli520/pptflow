package pipeline_test

import (
	"testing"
	_ "unsafe"
)

type testRuntimeEvidence struct {
	ComposeProject string                   `json:"compose_project"`
	ComposeFile    string                   `json:"compose_file"`
	WorkDir        string                   `json:"work_dir"`
	Services       []string                 `json:"services"`
	Mappings       map[string][]portMapping `json:"mappings"`
	Probes         []testProbeResult        `json:"probes"`
}

type testProbeResult struct {
	Service string `json:"service"`
	URL     string `json:"url"`
	OK      bool   `json:"ok"`
	Status  int    `json:"status,omitempty"`
	Error   string `json:"error,omitempty"`
}

type testStageCCommandEnv struct {
	Env     []string
	Keys    []string
	Values  map[string]string
	Service testServiceURLEnv
}

type testServiceURLEnv struct {
	Env     []string
	Keys    []string
	Mapping map[string]testServiceURL
}

type testServiceURL struct {
	EnvKey string `json:"env_key"`
	URL    string `json:"url"`
}

//go:linkname stageCEnvironment github.com/xuanli520/p2r_tui/internal/pipeline.stageCEnvironment
func stageCEnvironment(evidence testRuntimeEvidence) testStageCCommandEnv

func TestStageCEnvironmentPassesComposeProjectAndFile(t *testing.T) {
	evidence := testRuntimeEvidence{
		ComposeProject: "p2rqa_task_run_hash",
		ComposeFile:    "/tmp/project/repo/compose.yaml",
		Mappings: map[string][]portMapping{
			"api": {{
				Service:   "api",
				URL:       "0.0.0.0",
				Host:      4300,
				Container: 4300,
				Protocol:  "tcp",
			}},
		},
	}

	env := stageCEnvironment(evidence)
	if got := env.Values["API_URL"]; got != "http://localhost:4300" {
		t.Fatalf("API_URL = %q, want host runtime URL", got)
	}
	if got := env.Values["COMPOSE_PROJECT_NAME"]; got != evidence.ComposeProject {
		t.Fatalf("COMPOSE_PROJECT_NAME = %q, want %q", got, evidence.ComposeProject)
	}
	if got := env.Values["COMPOSE_FILE"]; got != evidence.ComposeFile {
		t.Fatalf("COMPOSE_FILE = %q, want %q", got, evidence.ComposeFile)
	}
	assertKeyOrder(t, env.Keys, []string{"API_URL", "COMPOSE_PROJECT_NAME", "COMPOSE_FILE"})
}

func assertKeyOrder(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("keys = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("keys = %#v, want %#v", got, want)
		}
	}
}
