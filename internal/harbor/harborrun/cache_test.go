package harborrun

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareClaudeAgentCacheReusesVerifiedReadOnlyCache(t *testing.T) {
	root := t.TempDir()
	arch, npmArch, err := claudeCacheArchitecture()
	if err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(root, claudeCacheVersion, "linux-"+arch)
	for _, packageName := range []string{"claude-code-linux-" + npmArch, "claude-code-linux-" + npmArch + "-musl"} {
		path := filepath.Join(cacheDir, "node_modules", "@anthropic-ai", packageName, "claude")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture-"+packageName), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := buildClaudeCacheManifest(cacheDir, arch, npmArch)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(cacheDir, "manifest.json")
	if err := os.WriteFile(manifestPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	gotManifest, mount, err := prepareClaudeAgentCache(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if gotManifest != manifestPath || !strings.Contains(mount, cacheDir) || !strings.Contains(mount, `"read_only":true`) || !strings.Contains(mount, claudeCacheMountTarget) {
		t.Fatalf("unexpected cache evidence: manifest=%q mount=%s", gotManifest, mount)
	}
}

func TestCacheProcessEnvDoesNotForwardModelCredentials(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "runtime-secret")
	joined := strings.Join(cacheProcessEnv(), "\n")
	if !strings.Contains(joined, "PATH=/usr/bin") || strings.Contains(joined, "ANTHROPIC") || strings.Contains(joined, "runtime-secret") {
		t.Fatalf("unexpected cache subprocess environment: %s", joined)
	}
}
