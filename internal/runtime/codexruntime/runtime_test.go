package codexruntime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteAutomationCodexConfigDefaultsToBuiltInProvider(t *testing.T) {
	t.Setenv("CODEX_MODEL_PROVIDER", "")
	t.Setenv("CODEX_MODEL_BASE_URL", "")
	t.Setenv("OPENAI_BASE_URL", "")
	sourceHome := t.TempDir()
	targetHome := t.TempDir()
	if err := writeAutomationCodexConfig(sourceHome, targetHome, "/tmp/project"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(targetHome, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	config := string(data)
	if !strings.Contains(config, `model_provider = "openai"`) {
		t.Fatalf("expected built-in openai provider fallback, got:\n%s", config)
	}
	if strings.Contains(config, "new-api.metalics.cn") {
		t.Fatalf("automation Codex config must not hardcode image endpoint:\n%s", config)
	}
}

func TestWriteAutomationCodexConfigCustomProviderUsesExplicitBaseURL(t *testing.T) {
	t.Setenv("CODEX_MODEL_PROVIDER", "custom")
	t.Setenv("CODEX_MODEL_BASE_URL", "https://codex-provider.example/v1")
	t.Setenv("OPENAI_BASE_URL", "")
	sourceHome := t.TempDir()
	targetHome := t.TempDir()
	if err := writeAutomationCodexConfig(sourceHome, targetHome, "/tmp/project"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(targetHome, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	config := string(data)
	for _, want := range []string{
		`model_provider = "custom"`,
		`[model_providers.custom]`,
		`base_url = "https://codex-provider.example/v1"`,
	} {
		if !strings.Contains(config, want) {
			t.Fatalf("missing %q in config:\n%s", want, config)
		}
	}
}
