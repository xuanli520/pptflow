package scanner_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/xuanli520/p2r_tui/internal/scanner"
)

func TestScanFindsValidPackages(t *testing.T) {
	root := t.TempDir()
	valid := writePackage(t, root, "batch-1", "TASK-001", `{"task_id":"TASK-001","prompt":"build it"}`, "original_sessions")
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
	if project.Batch != "batch-1" {
		t.Fatalf("unexpected batch: %s", project.Batch)
	}
	if project.Path != valid {
		t.Fatalf("unexpected project path: %s", project.Path)
	}
	if project.MetadataPromptMissing {
		t.Fatal("prompt should be detected")
	}
}

func TestScanIndexesMissingPrompt(t *testing.T) {
	root := t.TempDir()
	writePackage(t, root, "batch-1", "TASK-002", `{"task_id":"TASK-002"}`, "original_sessions")
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

func TestScanUsesDirectoryTaskIDWhenMetadataDiffers(t *testing.T) {
	root := t.TempDir()
	writePackage(t, root, "batch-2", "TASK-DIR", `{"task_id":"TASK-METADATA","prompt":"build it"}`, "original_sessions")
	result, err := scanner.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Projects) != 1 {
		t.Fatalf("expected 1 project, got %d", len(result.Projects))
	}
	if result.Projects[0].TaskID != "TASK-DIR" {
		t.Fatalf("canonical task id = %q", result.Projects[0].TaskID)
	}
}

func TestScanRejectsRootLevelTaskPackage(t *testing.T) {
	root := t.TempDir()
	writeLegacyPackage(t, filepath.Join(root, "TASK-ROOT"), `{"task_id":"TASK-ROOT","prompt":"build it"}`, "original_sessions")
	result, err := scanner.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Projects) != 0 {
		t.Fatalf("root-level package should be rejected: %#v", result.Projects)
	}
}

func TestScanRejectsBatchTaskWithoutNestedTask(t *testing.T) {
	root := t.TempDir()
	writeLegacyPackage(t, filepath.Join(root, "batch-1", "TASK-001"), `{"task_id":"TASK-001","prompt":"build it"}`, "original_sessions")
	result, err := scanner.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Projects) != 0 {
		t.Fatalf("non-nested package should be rejected: %#v", result.Projects)
	}
}

func TestScanRejectsNonBatchTopLevelPackage(t *testing.T) {
	root := t.TempDir()
	writePackage(t, root, "release-1", "TASK-001", `{"task_id":"TASK-001","prompt":"build it"}`, "original_sessions")
	result, err := scanner.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Projects) != 0 {
		t.Fatalf("non-batch package should be rejected: %#v", result.Projects)
	}
}

func TestScanSkipsResultArtifacts(t *testing.T) {
	root := t.TempDir()
	writeLegacyPackage(t, filepath.Join(root, "result", "batch-1", "TASK-001", "run-1", "script_input_snapshot"), `{"task_id":"TASK-001","prompt":"build it"}`, "original_sessions")
	writePackage(t, root, "batch-1", "TASK-001", `{"task_id":"TASK-001","prompt":"build it"}`, "original_sessions")
	result, err := scanner.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Projects) != 1 || result.Projects[0].Path != filepath.Join(root, "batch-1", "TASK-001", "TASK-001") {
		t.Fatalf("expected only canonical project, got %#v", result.Projects)
	}
}

func TestScanSkipsQARunSnapshot(t *testing.T) {
	root := t.TempDir()
	writeLegacyPackage(t, filepath.Join(root, "TASK-OLD", "qa", "runs", "run-1", "script_input_snapshot"), `{"task_id":"TASK-OLD","prompt":"build it"}`, "original_sessions")
	result, err := scanner.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Projects) != 0 {
		t.Fatalf("qa run snapshot should be rejected: %#v", result.Projects)
	}
}

func TestScanAcceptsAlternativeOriginalSessionMarkers(t *testing.T) {
	for _, marker := range []string{"docs/original-session", "docs/original_sessions"} {
		root := t.TempDir()
		writePackage(t, root, "batch-1", "TASK-001", `{"task_id":"TASK-001","prompt":"build it"}`, marker)
		result, err := scanner.Scan(root)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Projects) != 1 {
			t.Fatalf("marker %s should be accepted, got %#v", marker, result.Projects)
		}
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

func writePackage(t *testing.T, root, batch, taskID, metadata, marker string) string {
	t.Helper()
	path := filepath.Join(root, batch, taskID, taskID)
	writeLegacyPackage(t, path, metadata, marker)
	return path
}

func writeLegacyPackage(t *testing.T, path, metadata, marker string) {
	t.Helper()
	for _, dir := range []string{"docs", "repo", marker} {
		if err := os.MkdirAll(filepath.Join(path, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(path, "metadata.json"), []byte(metadata), 0o644); err != nil {
		t.Fatal(err)
	}
}
