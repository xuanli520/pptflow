package docker_test

import (
	"strings"
	"testing"

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
