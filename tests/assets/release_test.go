package assets_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/xuanli520/p2r_tui/assets"
)

func TestReleasePreservesExistingPromptProfiles(t *testing.T) {
	controlDir := t.TempDir()
	profilePath := filepath.Join(controlDir, "prompt_profiles", "frontend_e2e_browser.md")
	if err := os.MkdirAll(filepath.Dir(profilePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(profilePath, []byte("manual browser profile"), 0o644); err != nil {
		t.Fatal(err)
	}

	released, err := assets.Release(controlDir)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "manual browser profile" {
		t.Fatalf("manual prompt profile was overwritten: %q", string(content))
	}
	templatePath := filepath.Join(controlDir, "prompt_profiles", "frontend_e2e_browser_action_prompt.md")
	if _, err := os.Stat(templatePath); err != nil {
		t.Fatalf("missing newly released browser action prompt template: %v", err)
	}
	if len(released) == 0 {
		t.Fatal("expected released files to be reported")
	}
}
