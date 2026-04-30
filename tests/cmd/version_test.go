package cmd_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xuanli520/p2r_tui/cmd"
)

func TestVersionCommand(t *testing.T) {
	command := cmd.NewRootCommand()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetArgs([]string{"version"})

	if err := command.Execute(); err != nil {
		t.Fatalf("version command failed: %v", err)
	}

	got := output.String()
	for _, want := range []string{"p2r ", "commit:", "built:"} {
		if !strings.Contains(got, want) {
			t.Fatalf("version output %q does not contain %q", got, want)
		}
	}
}

func TestStatusEmptyRunIsFriendly(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "TASK-001")
	for _, dir := range []string{"docs", "repo", "original_sessions"} {
		if err := os.MkdirAll(filepath.Join(project, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(project, "metadata.json"), []byte(`{"task_id":"TASK-001","prompt":"build it"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, ".p2r.yaml")
	if err := os.WriteFile(configPath, []byte("scan_path: \"./\"\ndb_path: \"./.qa-control/index.db\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("P2R_CONFIG", configPath)
	scan := cmd.NewRootCommand()
	scan.SetArgs([]string{"scan"})
	if err := scan.Execute(); err != nil {
		t.Fatalf("scan failed: %v", err)
	}
	status := cmd.NewRootCommand()
	var output bytes.Buffer
	status.SetOut(&output)
	status.SetArgs([]string{"status", "TASK-001"})
	if err := status.Execute(); err != nil {
		t.Fatalf("status failed: %v", err)
	}
	got := output.String()
	if !strings.Contains(got, "项目已索引但尚无 run") {
		t.Fatalf("expected friendly empty-run message, got %q", got)
	}
	if strings.Contains(got, "sql: no rows") {
		t.Fatalf("raw sql error leaked: %q", got)
	}
}
