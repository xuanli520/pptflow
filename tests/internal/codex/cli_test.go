package codex_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/xuanli520/p2r_tui/internal/codex"
	"github.com/xuanli520/p2r_tui/internal/executor"
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

func TestValidateAppServerExtraArgsOnlyAllowsModelSelection(t *testing.T) {
	got, err := codex.ValidateAppServerExtraArgs([]string{"--model", "gpt-5.4"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1] != "gpt-5.4" {
		t.Fatalf("validated args were not preserved: %#v", got)
	}
	for _, args := range [][]string{
		{"--sandbox", "read-only"},
		{"--search"},
		{"--dangerously-bypass-approvals-and-sandbox"},
		{"--model", ""},
		{"--model", "--search"},
		{"--model=--search"},
		{"--model", "gpt-5.4", "--model", "gpt-5.5"},
	} {
		if _, err := codex.ValidateAppServerExtraArgs(args); err == nil {
			t.Fatalf("expected args to be rejected: %#v", args)
		}
	}
}

func TestDetectCLIUsesCodexLocalNodeForVersionAndHelp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based local node fixture is unix-only")
	}
	dir := t.TempDir()
	codexPath := filepath.Join(dir, "codex")
	nodePath := filepath.Join(dir, "node")
	if err := os.WriteFile(codexPath, []byte("#!/usr/bin/env node\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nodePath, []byte(`#!/bin/sh
shift
if [ "$1" = "--version" ]; then
  echo "codex-cli local-node"
  exit 0
fi
if [ "$1" = "app-server" ] && [ "$2" = "--help" ]; then
  echo "Usage: codex app-server [OPTIONS]"
  echo "  -c, --config <KEY=VALUE>"
  echo "      --listen <URL>"
  exit 0
fi
exit 3
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "")

	capability := codex.DetectCLI(context.Background(), executor.New(), codexPath)
	if capability.Version != "codex-cli local-node" || !capability.HasAppServer || !capability.HasConfig {
		t.Fatalf("expected local node to power Codex capability detection: %#v", capability)
	}
	if capability.NodePath != nodePath || !capability.PathPrependedForNode {
		t.Fatalf("expected Codex-local node to be detected and prepended: %#v", capability)
	}
}

func TestAppServerModelFromArgs(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--model", "gpt-5.4"}, "gpt-5.4"},
		{[]string{"-m=gpt-5.5"}, "gpt-5.5"},
		{[]string{"--model="}, ""},
		{nil, ""},
	} {
		if got := codex.AppServerModelFromArgs(tc.args); got != tc.want {
			t.Fatalf("AppServerModelFromArgs(%#v) = %q, want %q", tc.args, got, tc.want)
		}
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
