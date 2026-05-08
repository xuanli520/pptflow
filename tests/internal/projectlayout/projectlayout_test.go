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

func TestValidatePackageRootReportsSharedRequiredMarkers(t *testing.T) {
	root := t.TempDir()
	validation := projectlayout.ValidatePackageRoot(root)
	if validation.Valid {
		t.Fatal("empty package root should be invalid")
	}
	for _, want := range []string{"metadata.json", "docs/", "repo/", "original session marker"} {
		if !contains(validation.Missing, want) {
			t.Fatalf("missing list %#v does not include %q", validation.Missing, want)
		}
	}

	for _, dir := range []string{"docs", "repo", filepath.Join("docs", "original-session")} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "metadata.json"), []byte(`{"prompt":"build it"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	validation = projectlayout.ValidatePackageRoot(root)
	if !validation.Valid || validation.OriginalSessionMarker != filepath.Join("docs", "original-session") || len(validation.Missing) != 0 {
		t.Fatalf("valid package root validation = %#v", validation)
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

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
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
