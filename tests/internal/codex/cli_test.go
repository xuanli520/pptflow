package codex_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/xuanli520/p2r_tui/internal/codex"
)

func TestApplyAppServerHelpDetectsRequiredControls(t *testing.T) {
	var capability codex.Capability
	codex.ApplyAppServerHelp(&capability, `
Usage: codex app-server [OPTIONS] [COMMAND]
  -c, --config <KEY=VALUE>
  --listen <URL>
`)
	if !capability.HasAppServer || !capability.HasConfig {
		t.Fatalf("expected app-server/config controls to be detected: %#v", capability)
	}
	if err := codex.ValidateAppServerCapability(codex.Capability{Path: "codex", HasAppServer: true, HasConfig: true}); err != nil {
		t.Fatal(err)
	}
}

func TestValidateAppServerCapabilityRejectsMissingAppServer(t *testing.T) {
	if err := codex.ValidateAppServerCapability(codex.Capability{Path: "codex", HasConfig: true}); err == nil {
		t.Fatal("expected missing app-server support to be rejected")
	}
}

func TestValidateAppServerCapabilityRejectsMissingConfigOverride(t *testing.T) {
	if err := codex.ValidateAppServerCapability(codex.Capability{Path: "codex", HasAppServer: true}); err == nil {
		t.Fatal("expected missing -c/--config support to be rejected")
	}
}

func TestWithNodeOnPATHPrependsNodeDirectory(t *testing.T) {
	node := filepath.Join(t.TempDir(), "node")
	env := []string{"PATH=/usr/bin"}
	got := codex.WithNodeOnPATH(env, node)
	wantPrefix := filepath.Dir(node) + string(os.PathListSeparator)
	if runtime.GOOS == "windows" {
		wantPrefix = filepath.Dir(node) + string(os.PathListSeparator)
	}
	if got[0][:len("PATH=")+len(wantPrefix)] != "PATH="+wantPrefix {
		t.Fatalf("node dir not prepended: %#v", got)
	}
}
