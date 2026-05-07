package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/projectlayout"
	"github.com/xuanli520/p2r_tui/internal/scanner"
)

func TestStageAAcceptsAlternativeOriginalSessionMarkers(t *testing.T) {
	for _, marker := range []string{filepath.Join("docs", "original-session"), filepath.Join("docs", "original_sessions")} {
		root := writeStageAPackage(t, marker, `{"task_id":"TASK-1","prompt":"build it"}`)
		ok, _ := projectlayout.HasOriginalSessionMarker(root)
		required := map[string]bool{
			"docs":                    dirExists(filepath.Join(root, "docs")),
			"repo":                    dirExists(filepath.Join(root, "repo")),
			"original_session_marker": ok,
			"metadata.json":           fileExists(filepath.Join(root, "metadata.json")),
		}
		findings := structuralFindings(scanner.Project{TaskID: "TASK-1", Path: root}, required)
		if hasFindingTitle(findings, "Missing required delivery artifact: original_session_marker") {
			t.Fatalf("marker %s should not produce original session finding: %#v", marker, findings)
		}
	}
}

func TestStageAReportsMetadataTaskIDMismatch(t *testing.T) {
	root := writeStageAPackage(t, "original_sessions", `{"task_id":"TASK-METADATA","prompt":"build it"}`)
	required := map[string]bool{
		"docs":                    true,
		"repo":                    true,
		"original_session_marker": true,
		"metadata.json":           true,
	}
	findings := structuralFindings(scanner.Project{TaskID: "TASK-DIR", Path: root}, required)
	var mismatch model.Finding
	for _, finding := range findings {
		if strings.Contains(finding.Title, "task_id does not match") {
			mismatch = finding
			break
		}
	}
	if mismatch.Title == "" || mismatch.Severity != "Blocker" || !strings.Contains(mismatch.Evidence, "TASK-DIR") || !strings.Contains(mismatch.Evidence, "TASK-METADATA") {
		t.Fatalf("metadata mismatch finding not found or incomplete: %#v", findings)
	}
}

func writeStageAPackage(t *testing.T, marker, metadata string) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"docs", "repo", marker} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "metadata.json"), []byte(metadata), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func hasFindingTitle(findings []model.Finding, title string) bool {
	for _, finding := range findings {
		if finding.Title == title {
			return true
		}
	}
	return false
}
