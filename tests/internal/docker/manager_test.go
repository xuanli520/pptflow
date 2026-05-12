package docker_test

import (
	"strings"
	"testing"

	"github.com/xuanli520/p2r_tui/internal/config"
	dockermgr "github.com/xuanli520/p2r_tui/internal/docker"
)

func TestComposeProjectNamePreservesHashWhenTruncated(t *testing.T) {
	name := dockermgr.ComposeProjectName("p2rqa", strings.Repeat("TASK-LONG-", 10), "run-20260430-123456-999999")
	if len(name) > 63 {
		t.Fatalf("compose project name too long: %d %s", len(name), name)
	}
	parts := strings.Split(name, "_")
	if len(parts[len(parts)-1]) != 8 {
		t.Fatalf("hash suffix not preserved: %s", name)
	}
}

func TestCleanupComposeArgsRespectConfiguredDestructiveOptions(t *testing.T) {
	cfg := config.Default().Docker
	cfg.CleanupImages = false
	cfg.CleanupVolumes = false

	args := dockermgr.CleanupComposeArgs(cfg, "compose file.yml", "p2r_test")
	joined := strings.Join(args, " ")
	for _, forbidden := range []string{"-v", "--rmi"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("cleanup args should respect disabled %s option: %#v", forbidden, args)
		}
	}
	if !containsArg(args, "--remove-orphans") {
		t.Fatalf("cleanup args should still remove orphans: %#v", args)
	}
}

func TestCommandLineShellQuotesUnsafeArguments(t *testing.T) {
	line := dockermgr.CommandLine("docker", []string{"compose", "-f", "compose file.yml", "-p", "p2r;rm", "down"})
	for _, want := range []string{"'compose file.yml'", "'p2r;rm'"} {
		if !strings.Contains(line, want) {
			t.Fatalf("command line missing quoted arg %q: %s", want, line)
		}
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}
