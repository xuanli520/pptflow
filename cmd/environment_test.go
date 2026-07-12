package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/app"
)

func TestLoadHarborEnvironmentLoadsAllowedValuesWithoutOverridingProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env")
	raw := []byte("export ANTHROPIC_AUTH_TOKEN=file-token\nQWEN_HARBOR_BASE_URL=https://qwen.example\nIGNORED=value\n")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARBOR_FACTORY_ENV_FILE", path)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "process-token")
	t.Setenv("QWEN_HARBOR_BASE_URL", "")
	if err := loadHarborEnvironment(); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("ANTHROPIC_AUTH_TOKEN"); got != "process-token" {
		t.Fatalf("process value was overridden: %q", got)
	}
	if got := os.Getenv("QWEN_HARBOR_BASE_URL"); got != "https://qwen.example" {
		t.Fatalf("file value was not loaded: %q", got)
	}
	opts := app.RunnerOptions{}
	applyRunnerEnvironmentDefaults(&opts)
	if len(opts.HarborAgentEnv) != 1 || opts.HarborAgentEnv[0] != "ANTHROPIC_AUTH_TOKEN=${ANTHROPIC_AUTH_TOKEN}" || opts.QwenHarborBaseURL != "https://qwen.example" {
		t.Fatalf("loaded environment did not reach runner defaults: %+v", opts)
	}
}

func TestLoadHarborEnvironmentRejectsInsecurePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions are not applicable")
	}
	path := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(path, []byte("ANTHROPIC_AUTH_TOKEN=token\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARBOR_FACTORY_ENV_FILE", path)
	if err := loadHarborEnvironment(); err == nil {
		t.Fatal("expected insecure environment file to be rejected")
	}
}
