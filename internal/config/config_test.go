package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAppliesFileThenOverrides(t *testing.T) {
	dir := t.TempDir()
	content := []byte(`scan_path: "./from-file"
db_path: "./from-file/.qa-control/index.db"
pipeline:
  static_only: true
  stage_timeouts:
    B: 9
tui:
  refresh_interval_ms: 250
`)
	if err := os.WriteFile(filepath.Join(dir, ".p2r.yaml"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir, Overrides{ScanPath: "./from-flag"})
	if err != nil {
		t.Fatal(err)
	}
	if got := filepath.Base(cfg.ScanPath); got != "from-flag" {
		t.Fatalf("scan path override not applied: %s", cfg.ScanPath)
	}
	if !cfg.Pipeline.StaticOnly {
		t.Fatal("expected static_only from file")
	}
	if cfg.Pipeline.StageTimeouts["B"] != 9 {
		t.Fatalf("expected B timeout 9, got %d", cfg.Pipeline.StageTimeouts["B"])
	}
	if cfg.TUI.RefreshIntervalMS != 250 {
		t.Fatalf("expected TUI refresh 250, got %d", cfg.TUI.RefreshIntervalMS)
	}
}
