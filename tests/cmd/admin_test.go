package cmd_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xuanli520/p2r_tui/cmd"
)

func TestAdminDockerMirrorStatusIsReadOnlyAndApplyRequiresYes(t *testing.T) {
	root := t.TempDir()
	daemonPath := filepath.Join(root, "daemon.json")
	backupDir := filepath.Join(root, "backups")
	scanPath := filepath.Join(root, "scan")
	if err := os.WriteFile(daemonPath, []byte(`{"registry-mirrors":["https://old.example"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, ".p2r.yaml")
	configText := fmt.Sprintf(`scan_path: %q
db_path: %q
docker:
  daemon_mirrors:
    daemon_json: %q
    backup_dir: %q
    registry_mirrors:
      - "https://mirror.example"
    require_manual_apply: true
`, scanPath, filepath.Join(scanPath, ".qa-control", "index.db"), daemonPath, backupDir)
	if err := os.WriteFile(configPath, []byte(configText), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("P2R_CONFIG", configPath)

	status := cmd.NewRootCommand()
	var statusOut bytes.Buffer
	status.SetOut(&statusOut)
	status.SetArgs([]string{"admin", "docker-mirror", "status"})
	if err := status.Execute(); err != nil {
		t.Fatalf("status failed: %v", err)
	}
	if !strings.Contains(statusOut.String(), `"operation": "status"`) || !strings.Contains(statusOut.String(), `"changed": true`) {
		t.Fatalf("unexpected status output: %s", statusOut.String())
	}
	if _, err := os.Stat(filepath.Join(scanPath, ".qa-control", "daemon_mirror_summary.json")); !os.IsNotExist(err) {
		t.Fatalf("status should not write daemon mirror summary, stat err=%v", err)
	}

	apply := cmd.NewRootCommand()
	var applyOut bytes.Buffer
	apply.SetOut(&applyOut)
	apply.SetArgs([]string{"admin", "docker-mirror", "apply"})
	err := apply.Execute()
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("apply without --yes should fail with confirmation error, got %v", err)
	}
	summary, readErr := os.ReadFile(filepath.Join(scanPath, ".qa-control", "daemon_mirror_summary.json"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(summary), `"ok": false`) || !strings.Contains(string(summary), "--yes") {
		t.Fatalf("apply failure summary missing confirmation evidence:\n%s", summary)
	}
}
