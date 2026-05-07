package projectlayout_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/xuanli520/p2r_tui/internal/projectlayout"
)

func TestBatchAndTaskIDPatterns(t *testing.T) {
	if !projectlayout.IsBatchDir("batch-2") || !projectlayout.IsBatchDir("batch-prod_1") {
		t.Fatal("expected batch-* names to be accepted")
	}
	for _, name := range []string{"batch", "Batch-1", "result", "TASK-1"} {
		if projectlayout.IsBatchDir(name) {
			t.Fatalf("batch name %q should be rejected", name)
		}
	}
	if !projectlayout.IsTaskID("TASK-20260318-3CC794") {
		t.Fatal("expected TASK-* id to be accepted")
	}
	for _, name := range []string{"TASK", "task-1", "TASK/1", "TASK-"} {
		if projectlayout.IsTaskID(name) {
			t.Fatalf("task id %q should be rejected", name)
		}
	}
}

func TestHasOriginalSessionMarkerAcceptsEquivalentLocations(t *testing.T) {
	for _, marker := range projectlayout.OriginalSessionMarkers() {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, marker), 0o755); err != nil {
			t.Fatal(err)
		}
		ok, got := projectlayout.HasOriginalSessionMarker(root)
		if !ok || got != marker {
			t.Fatalf("marker %q detected as ok=%v marker=%q", marker, ok, got)
		}
	}
}

func TestMetadataTaskIDReadsMetadataWithoutCanonicalizing(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "metadata.json"), []byte(`{"task_id":"TASK-METADATA"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := projectlayout.MetadataTaskID(root); got != "TASK-METADATA" {
		t.Fatalf("metadata task id = %q", got)
	}
}

func TestSafePathSegmentFallsBackForUnsafeValues(t *testing.T) {
	cases := map[string]string{
		"":          "fallback",
		".":         "fallback",
		"..":        "fallback",
		"../TASK-1": "fallback",
		"TASK/1":    "fallback",
		"TASK\x001": "fallback",
		"TASK 1":    "TASK-1",
	}
	for input, want := range cases {
		if got := projectlayout.SafePathSegment(input, "fallback"); got != want {
			t.Fatalf("SafePathSegment(%q) = %q, want %q", input, got, want)
		}
	}
}
