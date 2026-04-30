package codex_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/xuanli520/p2r_tui/internal/codex"
)

func TestBuildExecArgsAllowsMissingOptionalApproval(t *testing.T) {
	capability := codex.Capability{
		Path:                "codex",
		HasSandbox:          true,
		HasConfig:           true,
		HasCDLong:           true,
		HasEphemeral:        true,
		HasSkipGitRepoCheck: true,
		HasFullAuto:         true,
	}
	args, err := codex.BuildExecArgs(capability, "/repo", []string{"--model", "gpt-5.4"})
	if err != nil {
		t.Fatal(err)
	}
	joined := joinArgs(args)
	for _, absent := range []string{"--ask-for-approval", "--full-auto"} {
		if containsArg(args, absent) {
			t.Fatalf("args should not contain unavailable optional flag %s: %#v", absent, args)
		}
	}
	for _, want := range []string{"exec", "--skip-git-repo-check", "--sandbox", "read-only", "-c", `approval_policy="never"`, "--cd", "/repo", "--ephemeral", "--model", "gpt-5.4", "-"} {
		if !containsArg(args, want) {
			t.Fatalf("args missing %s in %s", want, joined)
		}
	}
}

func TestApplyExecHelpDetectsFullAutoForDiagnosticsOnly(t *testing.T) {
	var capability codex.Capability
	codex.ApplyExecHelp(&capability, `
Usage: codex exec [OPTIONS] [PROMPT]
  --sandbox <MODE>
  --ask-for-approval <POLICY>
  -c, --config <KEY=VALUE>
  --cd <DIR>
  --full-auto
`)
	if !capability.HasFullAuto {
		t.Fatal("expected --full-auto to be detected")
	}
	args, err := codex.BuildExecArgs(codex.Capability{
		Path:              "codex",
		HasSandbox:        true,
		HasAskForApproval: true,
		HasCDLong:         true,
		HasFullAuto:       true,
	}, "/repo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if containsArg(args, "--full-auto") {
		t.Fatalf("BuildExecArgs must not use --full-auto: %#v", args)
	}
}

func TestBuildExecArgsSupportsShortCD(t *testing.T) {
	args, err := codex.BuildExecArgs(codex.Capability{Path: "codex", HasSandbox: true, HasConfig: true, HasCDShort: true}, "/repo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !containsArg(args, "-C") || containsArg(args, "--cd") {
		t.Fatalf("expected short cd only, got %#v", args)
	}
}

func TestBuildExecArgsRejectsMissingSandbox(t *testing.T) {
	if _, err := codex.BuildExecArgs(codex.Capability{Path: "codex", HasCDLong: true}, "/repo", nil); err == nil {
		t.Fatal("expected missing sandbox to be rejected")
	}
}

func TestBuildExecArgsRejectsWhenApprovalPolicyCannotBeForced(t *testing.T) {
	if _, err := codex.BuildExecArgs(codex.Capability{Path: "codex", HasSandbox: true, HasCDLong: true}, "/repo", nil); err == nil {
		t.Fatal("expected missing approval-policy controls to be rejected")
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

func joinArgs(args []string) string {
	out := ""
	for _, arg := range args {
		out += arg + " "
	}
	return out
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}
