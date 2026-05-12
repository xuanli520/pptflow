package pipeline_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/db"
	pipelinepkg "github.com/xuanli520/p2r_tui/internal/pipeline"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/scanner"
)

func TestRecoverStaleRunsMarksExpiredRunningRunCrashed(t *testing.T) {
	store, cfg, artifactRoot := recoveryStore(t)
	ctx := context.Background()
	started := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
	run := model.RunRecord{
		RunID:         "run-stale",
		TaskID:        "TASK-1",
		StartedAt:     started,
		Status:        model.RunRunning,
		ManualVerdict: model.ManualUnset,
		ArtifactRoot:  artifactRoot,
	}
	if err := store.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := store.PutStage(ctx, run.RunID, model.StageRecord{Stage: "A", Status: model.StageFailed, StartedAt: started, FinishedAt: started, ErrorSummary: "failed"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifactRoot, "run_manifest.json"), []byte(`{"stages":["A","F"],"stage_timeouts":{"A":1,"F":1}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := pipelinepkg.RecoverStaleRuns(ctx, store, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetRun(ctx, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.RunCrashed || got.FinishedAt == "" {
		t.Fatalf("run status = %s finished=%q, want crashed with finish time", got.Status, got.FinishedAt)
	}
	stages, err := store.Stages(ctx, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	stageF := findStage(stages, "F")
	if stageF.Status != model.StageFailed || stageF.ErrorSummary == "" {
		t.Fatalf("stage F not recovered as failed: %#v", stageF)
	}
	if _, err := os.Stat(filepath.Join(artifactRoot, "crash_summary.json")); err != nil {
		t.Fatalf("crash summary missing: %v", err)
	}
}

func TestRecoverStaleRunsLeavesFreshRunningRunAlone(t *testing.T) {
	store, cfg, artifactRoot := recoveryStore(t)
	ctx := context.Background()
	started := time.Now().UTC().Format(time.RFC3339)
	run := model.RunRecord{
		RunID:         "run-fresh",
		TaskID:        "TASK-1",
		StartedAt:     started,
		Status:        model.RunRunning,
		ManualVerdict: model.ManualUnset,
		ArtifactRoot:  artifactRoot,
	}
	if err := store.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifactRoot, "run_manifest.json"), []byte(`{"stages":["A","F"],"stage_timeouts":{"A":600,"F":600}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := pipelinepkg.RecoverStaleRuns(ctx, store, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetRun(ctx, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.RunRunning {
		t.Fatalf("fresh run status = %s, want running", got.Status)
	}
}

func TestRecoverStaleRunsCleansRuntimeFromPortMap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake docker shell script is POSIX-only")
	}
	store, cfg, artifactRoot := recoveryStore(t)
	ctx := context.Background()
	started := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
	run := model.RunRecord{
		RunID:         "run-runtime-stale",
		TaskID:        "TASK-1",
		StartedAt:     started,
		Status:        model.RunRunning,
		ManualVerdict: model.ManualUnset,
		ArtifactRoot:  artifactRoot,
	}
	if err := store.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := store.PutStage(ctx, run.RunID, model.StageRecord{Stage: "B", Status: model.StageRunning, StartedAt: started}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifactRoot, "run_manifest.json"), []byte(`{"stages":["B","C"],"stage_timeouts":{"B":1,"C":1}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(artifactRoot, "port_map.json"), []byte(`{
  "compose_project": "p2r_test",
  "compose_file": "`+filepath.ToSlash(filepath.Join(workDir, "compose file.yml"))+`",
  "work_dir": "`+filepath.ToSlash(workDir)+`",
  "services": ["web"],
  "mappings": {"web": [{"service":"web","url":"0.0.0.0","host":38080,"container":8080,"protocol":"tcp"}]}
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	fakeBin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	dockerLog := filepath.Join(t.TempDir(), "docker.log")
	t.Setenv("DOCKER_LOG", dockerLog)
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(fakeBin, "docker"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$DOCKER_LOG\"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := pipelinepkg.RecoverStaleRuns(ctx, store, cfg); err != nil {
		t.Fatal(err)
	}
	logContent, err := os.ReadFile(dockerLog)
	if err != nil {
		t.Fatalf("docker cleanup was not invoked: %v", err)
	}
	logText := string(logContent)
	for _, want := range []string{"compose", "-p p2r_test", "down"} {
		if !strings.Contains(logText, want) {
			t.Fatalf("docker cleanup log missing %q:\n%s", want, logText)
		}
	}
	summary, err := os.ReadFile(filepath.Join(artifactRoot, "cleanup_summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(summary), `"status": "ok"`) {
		t.Fatalf("cleanup summary should record successful recovery cleanup:\n%s", summary)
	}
}

func recoveryStore(t *testing.T) (*db.Store, config.Config, string) {
	t.Helper()
	root := t.TempDir()
	projectPath := filepath.Join(root, "batch", "TASK-1")
	artifactRoot := filepath.Join(projectPath, "qa", "runs", "run-stale")
	if err := os.MkdirAll(filepath.Join(projectPath, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(artifactRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.ScanPath = root
	cfg.DBPath = filepath.Join(root, ".qa-control", "index.db")
	cfg.Pipeline.StageTimeouts = map[string]int{"A": 1, "F": 1}
	store, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.UpsertProjects(context.Background(), []scanner.Project{{TaskID: "TASK-1", Batch: "batch", Path: projectPath}}); err != nil {
		t.Fatal(err)
	}
	return store, cfg, artifactRoot
}

func findStage(stages []model.StageRecord, stage string) model.StageRecord {
	for _, item := range stages {
		if item.Stage == stage {
			return item
		}
	}
	return model.StageRecord{}
}
