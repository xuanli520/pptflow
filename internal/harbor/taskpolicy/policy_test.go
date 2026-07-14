package taskpolicy

import (
	"os"
	"testing"
)

func TestIsAllowedFileUsesCanonicalHarborFileSet(t *testing.T) {
	for _, path := range []string{
		"instruction.md",
		"task.toml",
		"tests_analysis.md",
		"environment/Dockerfile",
		"environment/docker-compose.yaml",
		"solution/solve.sh",
		"tests/test.sh",
	} {
		if !IsAllowedFile(path) {
			t.Errorf("expected allowed file %q", path)
		}
	}
	if IsAllowedFile("environment/promptflow_runner.py") {
		t.Fatal("unexpected legacy file allowed")
	}
}

func TestCanonicalFilesDefinesExactV2PolicyAndFixedModes(t *testing.T) {
	want := map[string]struct {
		mode        os.FileMode
		required    bool
		environment bool
	}{
		"instruction.md":                  {mode: 0o644, required: true},
		"task.toml":                       {mode: 0o644, required: true},
		"tests_analysis.md":               {mode: 0o644, required: true},
		"environment/Dockerfile":          {mode: 0o644, environment: true},
		"environment/docker-compose.yaml": {mode: 0o644, environment: true},
		"solution/solve.sh":               {mode: 0o755, required: true},
		"tests/test.sh":                   {mode: 0o755, required: true},
	}
	files := CanonicalFiles()
	if len(files) != len(want) {
		t.Fatalf("canonical policy has %d files, want %d: %+v", len(files), len(want), files)
	}
	for _, file := range files {
		expected, ok := want[file.Path]
		if !ok {
			t.Fatalf("unexpected canonical policy file: %+v", file)
		}
		if file.Mode != expected.mode || file.Required != expected.required || file.Environment != expected.environment {
			t.Fatalf("canonical policy %s = %+v, want mode=%#o required=%t environment=%t", file.Path, file, expected.mode, expected.required, expected.environment)
		}
		mode, ok := CanonicalMode(file.Path)
		if !ok || mode != expected.mode {
			t.Fatalf("CanonicalMode(%q) = (%#o, %t), want (%#o, true)", file.Path, mode, ok, expected.mode)
		}
		delete(want, file.Path)
	}
	if len(want) != 0 {
		t.Fatalf("canonical policy omitted files: %+v", want)
	}
}
