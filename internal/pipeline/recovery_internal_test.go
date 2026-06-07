package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/db"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/scanner"
)

func TestRecoverOrphanedRunsLeavesLiveLockedRunAlone(t *testing.T) {
	root := t.TempDir()
	projectPath := filepath.Join(root, "batch", "TASK-1")
	artifactRoot := filepath.Join(projectPath, "qa", "runs", "run-live-lock")
	if err := os.MkdirAll(filepath.Join(projectPath, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(artifactRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.ScanPath = root
	cfg.DBPath = filepath.Join(root, ".qa-control", "index.db")
	store, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := store.UpsertProjects(ctx, []scanner.Project{{TaskID: "TASK-1", Batch: "batch", Path: projectPath}}); err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC().Format(time.RFC3339)
	run := model.RunRecord{
		RunID:         "run-live-lock",
		TaskID:        "TASK-1",
		StartedAt:     started,
		Status:        model.RunRunning,
		ManualVerdict: model.ManualUnset,
		ArtifactRoot:  artifactRoot,
	}
	if err := store.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	lockPath := taskRunLockPath(cfg.ScanPath, run.TaskID)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	lock := fmt.Sprintf("task_id=%s\npid=%d\ncreated_at=%s\n", run.TaskID, os.Getpid(), started)
	if err := os.WriteFile(lockPath, []byte(lock), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := RecoverOrphanedRuns(ctx, store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if result.Count() != 0 {
		t.Fatalf("live lock should not be recovered: %#v", result)
	}
	got, err := store.GetRun(ctx, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.RunRunning {
		t.Fatalf("run status = %s, want running", got.Status)
	}
}
