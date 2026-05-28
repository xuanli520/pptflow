package config_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
docker:
  cleanup_build_cache: true
  build_cache_prune_until: "12h"
docs:
  inline_text_limit_bytes: 2048
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
	if cfg.Pipeline.MaxConcurrent != 10 {
		t.Fatalf("expected default max concurrent 10, got %d", cfg.Pipeline.MaxConcurrent)
	}
	if cfg.TUI.RefreshIntervalMS != 250 {
		t.Fatalf("expected TUI refresh 250, got %d", cfg.TUI.RefreshIntervalMS)
	}
	if !cfg.Docker.CleanupBuildCache || cfg.Docker.BuildCachePruneUntil != "12h" {
		t.Fatalf("docker cleanup config not parsed: %#v", cfg.Docker)
	}
	if cfg.Docs.InlineTextLimitBytes != 2048 {
		t.Fatalf("docs inline limit not parsed: %#v", cfg.Docs)
	}
}

func TestLoadParsesAndNormalizesMaxConcurrent(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want int
	}{
		{"file value", "5", 5},
		{"zero fallback", "0", 10},
		{"cap large", "99", 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			content := []byte("pipeline:\n  max_concurrent: " + tc.raw + "\n")
			if err := os.WriteFile(filepath.Join(dir, ".p2r.yaml"), content, 0o644); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(dir, config.Overrides{})
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Pipeline.MaxConcurrent != tc.want {
				t.Fatalf("max concurrent = %d, want %d", cfg.Pipeline.MaxConcurrent, tc.want)
			}
		})
	}
}

func TestLoadParsesDefaultStages(t *testing.T) {
	dir := t.TempDir()
	content := []byte(`pipeline:
  default_stages:
    initial: [f, A, D, A]
    recheck: [F]
`)
	if err := os.WriteFile(filepath.Join(dir, ".p2r.yaml"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(dir, config.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.Pipeline.DefaultStages["initial"], ","); got != "A,D,F" {
		t.Fatalf("initial default stages = %s, want A,D,F", got)
	}
	if got := strings.Join(cfg.Pipeline.DefaultStages["recheck"], ","); got != "F" {
		t.Fatalf("recheck default stages = %s, want F", got)
	}
}

func TestLoadParsesStageCExecutionConfig(t *testing.T) {
	dir := t.TempDir()
	content := []byte(`pipeline:
  stage_c:
    execution: isolated
    runner_image: golang:1.25
    proxy_image: alpine/socat:latest
    fail_on_unmapped_localhost: false
`)
	if err := os.WriteFile(filepath.Join(dir, ".p2r.yaml"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(dir, config.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Pipeline.StageC.Execution != "isolated" ||
		cfg.Pipeline.StageC.RunnerImage != "golang:1.25" ||
		cfg.Pipeline.StageC.ProxyImage != "alpine/socat:latest" ||
		cfg.Pipeline.StageC.FailOnUnmappedLocalhost {
		t.Fatalf("stage C config not parsed: %#v", cfg.Pipeline.StageC)
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

func TestDefaultStaticCodexTimeoutsAllowFullReviews(t *testing.T) {
	cfg := config.Default()
	if cfg.Pipeline.StageTimeouts["D"] < 900 {
		t.Fatalf("D timeout too short for static Codex review: %d", cfg.Pipeline.StageTimeouts["D"])
	}
	if cfg.Pipeline.StageTimeouts["E"] < 1200 {
		t.Fatalf("E timeout too short for static Codex review: %d", cfg.Pipeline.StageTimeouts["E"])
	}
	if cfg.Pipeline.StageTimeouts["F"] < 900 {
		t.Fatalf("F timeout too short for static Codex review: %d", cfg.Pipeline.StageTimeouts["F"])
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

func TestLoadRejectsInvalidScalarTypes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
	}{
		{
			name: "invalid bool",
			content: `pipeline:
  static_only: maybe
`,
		},
		{
			name: "invalid int",
			content: `pipeline:
  max_concurrent: lots
`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, ".p2r.yaml"), []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := config.Load(dir, config.Overrides{}); err == nil {
				t.Fatal("expected config parse error")
			}
		})
	}
}

func TestLoadRejectsUnknownFieldsAndStageTimeoutKeys(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
	}{
		{
			name: "unknown top level",
			content: `surprise: true
`,
		},
		{
			name: "unknown nested field",
			content: `codex:
  unsafe_extra_args: true
`,
		},
		{
			name: "unknown stage timeout",
			content: `pipeline:
  stage_timeouts:
    Z: 10
`,
		},
		{
			name: "unknown default stage mode",
			content: `pipeline:
  default_stages:
    retry: [A]
`,
		},
		{
			name: "unknown default stage",
			content: `pipeline:
  default_stages:
    initial: [Z]
`,
		},
		{
			name: "empty default stages",
			content: `pipeline:
  default_stages:
    initial: []
`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, ".p2r.yaml"), []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := config.Load(dir, config.Overrides{}); err == nil {
				t.Fatal("expected config validation error")
			}
		})
	}
}

func TestLoadPreservesQuotedHashInScalar(t *testing.T) {
	dir := t.TempDir()
	content := []byte(`scan_path: "./with#hash"
codex:
  env:
    TOKEN: "abc#123"
`)
	if err := os.WriteFile(filepath.Join(dir, ".p2r.yaml"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(dir, config.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ScanPath != filepath.Join(dir, "with#hash") {
		t.Fatalf("quoted hash in scan path was not preserved: %s", cfg.ScanPath)
	}
	if cfg.Codex.Env["TOKEN"] != "abc#123" {
		t.Fatalf("quoted hash in env value was not preserved: %#v", cfg.Codex.Env)
	}
}

func TestLoadRejectsNonPositiveLimits(t *testing.T) {
	dir := t.TempDir()
	content := []byte(`docs:
  max_attachment_bytes: 0
`)
	if err := os.WriteFile(filepath.Join(dir, ".p2r.yaml"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(dir, config.Overrides{}); err == nil {
		t.Fatal("expected non-positive docs limit to be rejected")
	}
}

func TestLoadRejectsUnsafeGitBaseURL(t *testing.T) {
	for _, raw := range []string{
		"http://gitlab.example/fullstack/",
		"ssh://gitlab.example/fullstack/",
		"file:///tmp/repos/",
		"https://user:pass@gitlab.example/fullstack/",
		"https://gitlab.example/fullstack/?token=secret",
		"https://gitlab.example/fullstack/#frag",
	} {
		t.Run(raw, func(t *testing.T) {
			dir := t.TempDir()
			content := []byte("git:\n  base_url: " + strconv.Quote(raw) + "\n")
			if err := os.WriteFile(filepath.Join(dir, ".p2r.yaml"), content, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := config.Load(dir, config.Overrides{}); err == nil {
				t.Fatal("expected unsafe git base url to be rejected")
			}
		})
	}
}

func TestLoadAcceptsGitBaseURLWhenHostIsAllowed(t *testing.T) {
	dir := t.TempDir()
	content := []byte(`git:
  base_url: "https://gitlab.example/fullstack/"
  allowed_hosts:
    - "gitlab.example"
`)
	if err := os.WriteFile(filepath.Join(dir, ".p2r.yaml"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(dir, config.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Git.BaseURL != "https://gitlab.example/fullstack/" || len(cfg.Git.AllowedHosts) != 1 || cfg.Git.AllowedHosts[0] != "gitlab.example" {
		t.Fatalf("git config not loaded as expected: %#v", cfg.Git)
	}
}
