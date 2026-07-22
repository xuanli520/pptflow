package taskpolicy

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestHarborTaskConfigParsesStandardAuthoringTaskTOML(t *testing.T) {
	python, sourceParent := standardAuthoringHarborPython(t)
	command := exec.Command(python, "-c", `import sys
from harbor.models.task.config import TaskConfig
config = TaskConfig.model_validate_toml(sys.stdin.read())
assert config.task is not None
assert config.task.name == "tower-rs/tower-http-request-header-count-limit"
assert config.environment.network_mode.value == "no-network"
assert config.environment.workdir == "/workspace/source"
assert config.verifier.timeout_sec == 1800.0
`)
	command.Stdin = bytes.NewReader(standardAuthoringCurrentHarborTaskTOMLFixture())
	command.Env = standardAuthoringHarborPythonEnvironment(sourceParent)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Harbor 0.18 TaskConfig rejected the Standard Authoring fixture: %v\n%s", err, output)
	}
}

func standardAuthoringHarborPython(t *testing.T) (string, string) {
	t.Helper()
	python := strings.TrimSpace(os.Getenv("HARBOR_FACTORY_HARBOR_PYTHON_INTERPRETER"))
	sourceTree := strings.TrimSpace(os.Getenv("HARBOR_FACTORY_HARBOR_PYTHON_SOURCE_TREE"))
	if python == "" || sourceTree == "" {
		t.Skip("Harbor runtime unavailable: HARBOR_FACTORY_HARBOR_PYTHON_INTERPRETER and HARBOR_FACTORY_HARBOR_PYTHON_SOURCE_TREE are required")
	}
	if info, statErr := os.Stat(python); statErr != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		t.Skipf("Harbor runtime unavailable: configured Python interpreter: %v", statErr)
	}
	if info, statErr := os.Stat(sourceTree); statErr != nil || !info.IsDir() {
		t.Skipf("Harbor runtime unavailable: configured Harbor Python source tree: %v", statErr)
	}
	sourceParent := filepath.Dir(sourceTree)
	probe := exec.Command(python, "-c", "from harbor.models.task.config import TaskConfig")
	probe.Env = standardAuthoringHarborPythonEnvironment(sourceParent)
	if output, probeErr := probe.CombinedOutput(); probeErr != nil {
		t.Skipf("Harbor runtime unavailable: import TaskConfig: %v: %s", probeErr, output)
	}
	return python, sourceParent
}

func standardAuthoringHarborPythonEnvironment(sourceParent string) []string {
	environment := make([]string, 0, len(os.Environ())+1)
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "PYTHONPATH=") {
			environment = append(environment, item)
		}
	}
	return append(environment, "PYTHONPATH="+sourceParent)
}
