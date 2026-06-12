package pipeline_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func testPython(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"python3", "python"} {
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if err := exec.Command(path, "--version").Run(); err == nil {
			return path
		}
	}
	t.Skip("python not available")
	return ""
}

func repoRootForTest(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../../.."))
}

func TestRequiredArtifactScriptsAcceptAlternativeOriginalSessionMarkers(t *testing.T) {
	python := testPython(t)
	repoRoot := repoRootForTest(t)
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

func TestLocalDependencyScriptIgnoresLegitimateLocalhostContexts(t *testing.T) {
	python := testPython(t)
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("metadata.json", `{"cwd":"/home/runner/work/package"}`)
	write(filepath.Join("repo", "run_tests.sh"), "curl http://localhost:3000/health\n")
	write(filepath.Join("repo", "docker-compose.yml"), "services:\n  web:\n    healthcheck:\n      test: ['CMD', 'curl', 'http://127.0.0.1:3000/health']\n")
	write(filepath.Join("repo", "unit_tests", "auth_test.py"), "BASE='http://127.0.0.1:8000'\n")

	script := filepath.Join(repoRootForTest(t), "assets", "scripts", "check_local_dependency.py")
	output, err := exec.Command(python, script, root).CombinedOutput()
	if err != nil {
		t.Fatalf("legitimate localhost contexts should pass: %v\n%s", err, output)
	}
}

func TestLocalDependencyScriptReportsHostDependencies(t *testing.T) {
	python := testPython(t)
	root := t.TempDir()
	path := filepath.Join(root, "repo", "src", "config.ts")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("export const API='http://localhost:5432';\nexport const DB='host.docker.internal';\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	script := filepath.Join(repoRootForTest(t), "assets", "scripts", "check_local_dependency.py")
	output, err := exec.Command(python, script, root).CombinedOutput()
	if err == nil {
		t.Fatalf("expected host dependency report, got success:\n%s", output)
	}
	text := string(output)
	if !strings.Contains(text, "localhost_reference") || !strings.Contains(text, "host_docker_internal") {
		t.Fatalf("unexpected output:\n%s", text)
	}
}

func TestLocalDependencyScriptIncludeMarkdownReportsLocalhost(t *testing.T) {
	python := testPython(t)
	root := t.TempDir()
	path := filepath.Join(root, "repo", "README.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("Requires http://localhost:4567 from my laptop.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	script := filepath.Join(repoRootForTest(t), "assets", "scripts", "check_local_dependency.py")
	if output, err := exec.Command(python, script, root).CombinedOutput(); err != nil {
		t.Fatalf("markdown should be ignored by default: %v\n%s", err, output)
	}
	output, err := exec.Command(python, script, root, "--include-markdown").CombinedOutput()
	if err == nil {
		t.Fatalf("expected markdown localhost report with --include-markdown, got success:\n%s", output)
	}
	if !strings.Contains(string(output), "localhost_reference") {
		t.Fatalf("unexpected output:\n%s", output)
	}
}

func TestLocalDependencyScriptReportsHostDockerInternalInTests(t *testing.T) {
	python := testPython(t)
	root := t.TempDir()
	path := filepath.Join(root, "repo", "unit_tests", "mock_test.py")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("HOST='host.docker.internal'\nLOOPBACK='http://127.0.0.1:8000'\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	script := filepath.Join(repoRootForTest(t), "assets", "scripts", "check_local_dependency.py")
	output, err := exec.Command(python, script, root).CombinedOutput()
	if err == nil {
		t.Fatalf("expected host.docker.internal report in tests, got success:\n%s", output)
	}
	text := string(output)
	if !strings.Contains(text, "host_docker_internal") {
		t.Fatalf("unexpected output:\n%s", output)
	}
	if strings.Contains(text, "loopback_reference") {
		t.Fatalf("test mock loopback should remain allowed:\n%s", output)
	}
}
