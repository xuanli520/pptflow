package tui_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/db"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/scanner"
	tuiapp "github.com/xuanli520/p2r_tui/internal/tui"
)

func TestExecutionViewModelFillsPartialRunsAndMissingDocs(t *testing.T) {
	store, cfg, projectPath, artifactRoot := tuiStore(t)
	ctx := context.Background()
	run := model.RunRecord{RunID: "run-1", TaskID: "TASK-1", StartedAt: "2026-04-30T00:00:00Z", Status: model.RunCompletedWithFindings, ManualVerdict: model.ManualUnset, ArtifactRoot: artifactRoot}
	if err := store.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := store.PutStage(ctx, run.RunID, model.StageRecord{Stage: "D", Status: model.StageFailed, ErrorSummary: "codex unavailable", ArtifactPaths: []string{filepath.Join(artifactRoot, "unavailable-review.md")}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectPath, "repo", "self_test_report.md"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}

	probe, err := tuiapp.BuildExecutionProbeForTest(ctx, store, cfg, "TASK-1", "D", 80)
	if err != nil {
		t.Fatal(err)
	}
	if probe.StageCount != 6 {
		t.Fatalf("stages = %d, want 6", probe.StageCount)
	}
	if probe.FirstStageError != "本次运行未记录该阶段" {
		t.Fatalf("missing stage summary = %q", probe.FirstStageError)
	}
	if probe.DocsManifestExists {
		t.Fatal("missing docs manifest should be visible as not generated")
	}
	if !strings.Contains(probe.DetailContent, "unavailable-review.md") || !strings.Contains(probe.DetailContent, "文档清单") {
		t.Fatalf("detail content missing evidence/docs: %s", probe.DetailContent)
	}
}

func TestExecutionViewModelReportsInvalidCleanupJSON(t *testing.T) {
	store, cfg, _, artifactRoot := tuiStore(t)
	ctx := context.Background()
	run := model.RunRecord{RunID: "run-1", TaskID: "TASK-1", StartedAt: "2026-04-30T00:00:00Z", Status: model.RunCompletedClean, ManualVerdict: model.ManualUnset, ArtifactRoot: artifactRoot}
	if err := store.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifactRoot, "cleanup_summary.json"), []byte("{bad json"), 0o644); err != nil {
		t.Fatal(err)
	}

	probe, err := tuiapp.BuildExecutionProbeForTest(ctx, store, cfg, "TASK-1", "A", 80)
	if err != nil {
		t.Fatal(err)
	}
	if probe.CleanupStatus != "unknown" || !strings.Contains(probe.CleanupText, "读取失败") {
		t.Fatalf("cleanup status/text = %q / %q", probe.CleanupStatus, probe.CleanupText)
	}
}

func TestExecutionViewModelExcludesRunningRefRuns(t *testing.T) {
	store, cfg, _, artifactRoot := tuiStore(t)
	ctx := context.Background()
	completed := model.RunRecord{RunID: "run-completed", TaskID: "TASK-1", StartedAt: "2026-04-30T00:00:00Z", Status: model.RunCompletedClean, ManualVerdict: model.ManualUnset, ArtifactRoot: artifactRoot}
	runningRoot := filepath.Join(filepath.Dir(artifactRoot), "run-running")
	if err := os.MkdirAll(runningRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	running := model.RunRecord{RunID: "run-running", TaskID: "TASK-1", StartedAt: "2026-04-30T00:01:00Z", Status: model.RunRunning, ManualVerdict: model.ManualUnset, ArtifactRoot: runningRoot}
	for _, run := range []model.RunRecord{completed, running} {
		if err := store.CreateRun(ctx, run); err != nil {
			t.Fatal(err)
		}
	}

	probe, err := tuiapp.BuildExecutionProbeForTest(ctx, store, cfg, "TASK-1", "A", 80)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(probe.RefRunIDs, ",") != "run-completed" {
		t.Fatalf("ref runs should exclude running runs, got %#v", probe.RefRunIDs)
	}
}

func TestRefreshRowsKeepsSelectedTaskStable(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).
		SeedOverview("TASK-1", "TASK-2").
		SetSelectedTaskForRefresh("TASK-2").
		ReplaceOverviewForRefresh("TASK-2", "TASK-1")
	if got := h.SelectedTaskID(); got != "TASK-2" {
		t.Fatalf("selected task drifted to %s", got)
	}
}

func TestProjectReloadRequestsDetailRefreshForSameTask(t *testing.T) {
	store, cfg, _, _ := tuiStore(t)
	h := tuiapp.NewTestHarnessWithStore(store, cfg).
		SeedOverview("TASK-1").
		SetSelectedTaskForRefresh("TASK-1")
	_, hasCmd := h.ApplyProjectReloadForTest()
	if !hasCmd {
		t.Fatal("project reload for selected task should request detail refresh")
	}
}

func tuiStore(t *testing.T) (*db.Store, config.Config, string, string) {
	t.Helper()
	root := t.TempDir()
	projectPath := filepath.Join(root, "batch", "TASK-1")
	artifactRoot := filepath.Join(projectPath, "qa", "runs", "run-1")
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
	if err := store.UpsertProjects(context.Background(), []scanner.Project{{TaskID: "TASK-1", Batch: "batch", Path: projectPath}}); err != nil {
		t.Fatal(err)
	}
	return store, cfg, projectPath, artifactRoot
}
