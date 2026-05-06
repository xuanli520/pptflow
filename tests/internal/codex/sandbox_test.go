package codex_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/xuanli520/p2r_tui/internal/codex"
)

func TestSandboxEnvPreservesUserCodexHome(t *testing.T) {
	artifactRoot := t.TempDir()
	sandbox, err := codex.NewSandbox("/repo", artifactRoot, "D")
	if err != nil {
		t.Fatal(err)
	}
	env := sandbox.EnvWithNode([]string{
		"HOME=/home/user",
		"USERPROFILE=/home/user",
		"CODEX_HOME=/home/user/.codex",
		"PATH=/usr/bin",
		"P2R_SECRET=should-not-leak",
	}, nil, filepath.Join(t.TempDir(), "node"))

	assertEnvValue(t, env, "HOME", "/home/user")
	assertEnvValue(t, env, "USERPROFILE", "/home/user")
	assertEnvValue(t, env, "CODEX_HOME", "/home/user/.codex")
	assertEnvMissing(t, env, "P2R_SECRET")
}

func TestSandboxEnvAllowsConfiguredCodexHomeOverride(t *testing.T) {
	artifactRoot := t.TempDir()
	sandbox, err := codex.NewSandbox("/repo", artifactRoot, "D")
	if err != nil {
		t.Fatal(err)
	}
	env := sandbox.EnvWithNode([]string{
		"HOME=/home/user",
		"CODEX_HOME=/home/user/.codex",
		"PATH=/usr/bin",
	}, map[string]string{
		"CODEX_HOME":     "/tmp/custom-codex-home",
		"OPENAI_API_KEY": "secret",
	}, "")

	assertEnvValue(t, env, "HOME", "/home/user")
	assertEnvValue(t, env, "CODEX_HOME", "/tmp/custom-codex-home")
	assertEnvValue(t, env, "OPENAI_API_KEY", "secret")
}

func assertEnvValue(t *testing.T, env []string, key, want string) {
	t.Helper()
	prefix := key + "="
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			if got := strings.TrimPrefix(item, prefix); got != want {
				t.Fatalf("%s = %q, want %q in %#v", key, got, want, env)
			}
			return
		}
	}
	t.Fatalf("%s missing in %#v", key, env)
}

func assertEnvMissing(t *testing.T, env []string, key string) {
	t.Helper()
	prefix := key + "="
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			t.Fatalf("%s should not be present in %#v", key, env)
		}
	}
}
