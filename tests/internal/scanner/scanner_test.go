package scanner_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/xuanli520/p2r_tui/internal/scanner"
)

func TestScanFindsValidPackages(t *testing.T) {
	root := t.TempDir()
	valid := filepath.Join(root, "batch-1", "TASK-001")
	for _, dir := range []string{"docs", "repo", "original_sessions"} {
		if err := os.MkdirAll(filepath.Join(valid, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(valid, "metadata.json"), []byte(`{"task_id":"TASK-001","prompt":"build it"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "batch-1", "not-a-task", "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	result, err := scanner.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Projects) != 1 {
		t.Fatalf("expected 1 project, got %d", len(result.Projects))
	}
	project := result.Projects[0]
	if project.TaskID != "TASK-001" {
		t.Fatalf("unexpected task id: %s", project.TaskID)
	}
	if project.MetadataPromptMissing {
		t.Fatal("prompt should be detected")
	}
}

func TestScanIndexesMissingPrompt(t *testing.T) {
	root := t.TempDir()
	valid := filepath.Join(root, "TASK-002")
	for _, dir := range []string{"docs", "repo", "original_sessions"} {
		if err := os.MkdirAll(filepath.Join(valid, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(valid, "metadata.json"), []byte(`{"task_id":"TASK-002"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := scanner.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Projects) != 1 {
		t.Fatalf("expected 1 project, got %d", len(result.Projects))
	}
	if !result.Projects[0].MetadataPromptMissing {
		t.Fatal("missing prompt should be recorded without blocking indexing")
	}
}

func TestScanMissingRootReturnsError(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	result, err := scanner.Scan(root)
	if err == nil {
		t.Fatalf("expected missing root to fail, got result %#v", result)
	}
}

func TestScanFileRootReturnsError(t *testing.T) {
	root := filepath.Join(t.TempDir(), "not-dir")
	if err := os.WriteFile(root, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := scanner.Scan(root)
	if err == nil {
		t.Fatalf("expected file root to fail, got result %#v", result)
	}
}
