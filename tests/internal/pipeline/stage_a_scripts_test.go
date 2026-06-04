package pipeline_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestRequiredArtifactScriptsAcceptAlternativeOriginalSessionMarkers(t *testing.T) {
	python := ""
	for _, name := range []string{"python3", "python"} {
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if err := exec.Command(path, "--version").Run(); err == nil {
			python = path
			break
		}
	}
	if python == "" {
		t.Skip("python not available")
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "../../.."))
	script := filepath.Join(repoRoot, "assets", "scripts", "check_required_artifacts.py")
	for _, marker := range []string{filepath.Join("docs", "original-session"), filepath.Join("docs", "original_sessions")} {
		root := t.TempDir()
		for _, dir := range []string{"docs", "repo", marker, filepath.Join("repo", "unit_tests"), filepath.Join("repo", "API_tests")} {
			if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		files := map[string]string{
			"metadata.json":       `{"prompt":"build it","project_type":"pure_frontend"}`,
			"docs/design.md":      "# Design\n",
			"docs/questions.md":   "# Questions\n",
			"repo/README.md":      "# App\n",
			"repo/run_tests.sh":   "#!/usr/bin/env bash\nexit 0\n",
			"repo/unit_tests/.go": "placeholder\n",
			"repo/API_tests/.go":  "placeholder\n",
		}
		for rel, content := range files {
			if err := os.WriteFile(filepath.Join(root, rel), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command(python, script, root, "--project-type", "pure_frontend")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("marker %s should pass required artifact script: %v\n%s", marker, err, output)
		}
	}
}
