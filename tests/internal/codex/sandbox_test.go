package codex_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/xuanli520/p2r_tui/internal/codex"
)

func TestSandboxEnvPreservesUserCodexHomeByDefault(t *testing.T) {
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
	}, nil, filepath.Join(t.TempDir(), "node"))

	assertEnvValue(t, env, "HOME", "/home/user")
	assertEnvValue(t, env, "USERPROFILE", "/home/user")
	assertEnvValue(t, env, "CODEX_HOME", "/home/user/.codex")
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
	}, map[string]string{"CODEX_HOME": "/tmp/custom-codex-home"}, "")

	assertEnvValue(t, env, "HOME", "/home/user")
	assertEnvValue(t, env, "CODEX_HOME", "/tmp/custom-codex-home")
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
