package tui_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tuiapp "github.com/xuanli520/p2r_tui/internal/tui"
)

func TestStageLogPreviewTailsConfiguredLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "A_validate.log")
	if err := os.WriteFile(path, []byte("line1\nline2\nline3\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	preview := tuiapp.StageLogPreviewForTest(path, 2)
	if !strings.Contains(preview, "Log: "+path) {
		t.Fatalf("preview missing log path: %s", preview)
	}
	if strings.Contains(preview, "line1") {
		t.Fatalf("preview should tail the log, got: %s", preview)
	}
	for _, want := range []string{"line2", "line3"} {
		if !strings.Contains(preview, want) {
			t.Fatalf("preview missing %q: %s", want, preview)
		}
	}
}
