package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/xuanli520/p2r_tui/internal/config"
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
	cfg, err := config.Load(dir, config.Overrides{ScanPath: "./from-flag"})
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

func TestLoadUsesExplicitConfigRelativeToConfigDirectory(t *testing.T) {
	cwd := t.TempDir()
	configDir := t.TempDir()
	t.Setenv(config.EnvConfig, filepath.Join(configDir, "p2r.yaml"))

	content := []byte(`scan_path: "./packages"
db_path: "./state/index.db"
codex:
  prompt_profiles_dir: "./profiles"
`)
	if err := os.WriteFile(filepath.Join(configDir, "p2r.yaml"), content, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(cwd, config.Overrides{})
	if err != nil {
		t.Fatal(err)
	}

	wantScanPath := filepath.Join(configDir, "packages")
	if cfg.ScanPath != wantScanPath {
		t.Fatalf("expected scan path %s, got %s", wantScanPath, cfg.ScanPath)
	}
	wantDBPath := filepath.Join(configDir, "state", "index.db")
	if cfg.DBPath != wantDBPath {
		t.Fatalf("expected db path %s, got %s", wantDBPath, cfg.DBPath)
	}
	wantProfilesPath := filepath.Join(configDir, "profiles")
	if cfg.Codex.PromptProfilesDir != wantProfilesPath {
		t.Fatalf("expected prompt profiles path %s, got %s", wantProfilesPath, cfg.Codex.PromptProfilesDir)
	}
}

func TestLoadAppliesEnvironmentThenOverrides(t *testing.T) {
	dir := t.TempDir()
	content := []byte(`scan_path: "./from-file"
db_path: "./from-file/index.db"
`)
	if err := os.WriteFile(filepath.Join(dir, ".p2r.yaml"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.EnvScanPath, "./from-env")
	t.Setenv(config.EnvDBPath, "./from-env/index.db")

	cfg, err := config.Load(dir, config.Overrides{ScanPath: "./from-flag"})
	if err != nil {
		t.Fatal(err)
	}

	wantScanPath := filepath.Join(dir, "from-flag")
	if cfg.ScanPath != wantScanPath {
		t.Fatalf("expected flag scan path %s, got %s", wantScanPath, cfg.ScanPath)
	}
	wantDBPath := filepath.Join(dir, "from-env", "index.db")
	if cfg.DBPath != wantDBPath {
		t.Fatalf("expected env db path %s, got %s", wantDBPath, cfg.DBPath)
	}
}

func TestLoadUsesUserConfigWhenNoLocalConfigExists(t *testing.T) {
	cwd := t.TempDir()
	configRoot := t.TempDir()
	t.Setenv("AppData", configRoot)
	t.Setenv("XDG_CONFIG_HOME", configRoot)
	t.Setenv("HOME", t.TempDir())
	userConfigDir := filepath.Join(configRoot, "p2r")
	if err := os.MkdirAll(userConfigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte(`scan_path: "./global-packages"
`)
	if err := os.WriteFile(filepath.Join(userConfigDir, "config.yaml"), content, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(cwd, config.Overrides{})
	if err != nil {
		t.Fatal(err)
	}

	wantScanPath := filepath.Join(userConfigDir, "global-packages")
	if cfg.ScanPath != wantScanPath {
		t.Fatalf("expected global scan path %s, got %s", wantScanPath, cfg.ScanPath)
	}
}

func TestLoadReturnsErrorForMissingExplicitConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.EnvConfig, filepath.Join(dir, "missing.yaml"))

	if _, err := config.Load(dir, config.Overrides{}); err == nil {
		t.Fatal("expected error for missing explicit config")
	}
}

func TestLoadParsesCodexEnvExtraArgsAndSelfTestPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OPENAI_API_KEY", "secret")
	content := []byte(`pipeline:
  self_test_report_path: "repo/custom_self_test.md"
  stage_timeouts:
    b_pull: 12
codex:
  env:
    OPENAI_API_KEY: "${OPENAI_API_KEY}"
  extra_args:
    - "--model"
    - "gpt-5.4"
`)
	if err := os.WriteFile(filepath.Join(dir, ".p2r.yaml"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(dir, config.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Pipeline.SelfTestReportPath != "repo/custom_self_test.md" {
		t.Fatalf("unexpected self test path: %s", cfg.Pipeline.SelfTestReportPath)
	}
	if cfg.Pipeline.StageTimeouts["B_PULL"] != 12 {
		t.Fatalf("B_PULL timeout not normalized: %#v", cfg.Pipeline.StageTimeouts)
	}
	if cfg.Codex.Env["OPENAI_API_KEY"] != "secret" {
		t.Fatalf("env expansion failed: %#v", cfg.Codex.Env)
	}
	if len(cfg.Codex.ExtraArgs) != 2 || cfg.Codex.ExtraArgs[1] != "gpt-5.4" {
		t.Fatalf("extra args not parsed: %#v", cfg.Codex.ExtraArgs)
	}
}

func TestLoadErrorsForMissingEnvReference(t *testing.T) {
	dir := t.TempDir()
	content := []byte(`codex:
  env:
    OPENAI_API_KEY: "${MISSING_P2R_TEST_ENV}"
`)
	if err := os.WriteFile(filepath.Join(dir, ".p2r.yaml"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(dir, config.Overrides{}); err == nil {
		t.Fatal("expected missing env reference error")
	}
}
