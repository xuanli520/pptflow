package pipeline_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/db"
	pipelinepkg "github.com/xuanli520/p2r_tui/internal/pipeline"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/scanner"
)

func TestRunPersistsRunningStageAndStreamsCodexLog(t *testing.T) {
	root := t.TempDir()
	projectDir := filepath.Join(root, "batch-1", "task-hang")
	for _, dir := range []string{"docs", "repo", "original_sessions"} {
		if err := os.MkdirAll(filepath.Join(projectDir, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(projectDir, "metadata.json"), []byte(`{"task_id":"TASK-HANG","prompt":"build a small app"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "repo", "self_test_report.md"), []byte("self test passed"), 0o644); err != nil {
		t.Fatal(err)
	}

	fakeBin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(fakeBin, "codex"), `#!/usr/bin/env bash
set -euo pipefail
if [ "${1:-}" = "--version" ]; then
  echo "codex-cli 0.999.0"
  exit 0
fi
if [ "${1:-}" = "exec" ] && [ "${2:-}" = "--help" ]; then
  echo "--sandbox --ask-for-approval --cd -C --ephemeral --skip-git-repo-check --ignore-user-config"
  exit 0
fi
if [ "${1:-}" = "exec" ]; then
  echo "fake-codex-start" >&2
  sleep "${FAKE_CODEX_SLEEP:-2}"
  echo "# Fake Report"
  echo "- High: simulated finding from fake codex"
  exit 0
fi
echo "unexpected fake codex args: $*" >&2
exit 2
`)
	writeExecutable(t, filepath.Join(fakeBin, "node"), `#!/usr/bin/env bash
echo "v25.0.0"
`)
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CODEX_SLEEP", "2")

	cfg := config.Default()
	cfg.ScanPath = root
	cfg.DBPath = filepath.Join(root, ".qa-control", "index.db")
	cfg.Codex.PromptProfilesDir = filepath.Join(root, ".qa-control", "prompt_profiles")
	cfg.Pipeline.StageTimeouts["D"] = 10
	cfg.Pipeline.StageTimeouts["F"] = 10
	store, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scan, err := scanner.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertProjects(context.Background(), scan.Projects); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resultCh := make(chan error, 1)
	go func() {
		_, err := pipelinepkg.NewRunner(store, cfg).Run(ctx, "TASK-HANG", pipelinepkg.RunOptions{Stage: "D"})
		resultCh <- err
	}()

	run := waitForLatestRun(t, ctx, store, "TASK-HANG")
	if run.Status != model.RunRunning {
		t.Fatalf("latest run status while command is active = %s, want running", run.Status)
	}
	waitForStageLog(t, ctx, store, run.RunID, "D", "fake-codex-start")

	if err := <-resultCh; err != nil {
		t.Fatal(err)
	}
	finalRun, err := store.GetRun(context.Background(), run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if finalRun.Status == model.RunRunning {
		t.Fatalf("run stayed running after completion: %#v", finalRun)
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func waitForLatestRun(t *testing.T, ctx context.Context, store *db.Store, taskID string) model.RunRecord {
	t.Helper()
	for {
		run, err := store.LatestRunForTask(context.Background(), taskID)
		if err == nil {
			return run
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for latest run: %v", ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func waitForStageLog(t *testing.T, ctx context.Context, store *db.Store, runID, stage, want string) {
	t.Helper()
	for {
		stages, err := store.Stages(context.Background(), runID)
		if err == nil {
			for _, item := range stages {
				if item.Stage != stage || item.Status != model.StageRunning || item.LogPath == "" {
					continue
				}
				content, _ := os.ReadFile(item.LogPath)
				if strings.Contains(string(content), want) {
					return
				}
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for running stage %s log containing %q", stage, want)
		case <-time.After(25 * time.Millisecond):
		}
	}
}
