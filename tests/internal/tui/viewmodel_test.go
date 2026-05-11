package tui_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/db"
	"github.com/xuanli520/p2r_tui/internal/pipeline"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/scanner"
	"github.com/xuanli520/p2r_tui/internal/scheduler"
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

func TestExecutionViewModelPrioritizesCodexFinalReportWarningsAndGuidance(t *testing.T) {
	store, cfg, projectPath, artifactRoot := tuiStore(t)
	ctx := context.Background()
	run := model.RunRecord{RunID: "run-1", TaskID: "TASK-1", StartedAt: "2026-04-30T00:00:00Z", Status: model.RunCompletedClean, ManualVerdict: model.ManualUnset, ArtifactRoot: artifactRoot}
	if err := store.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(artifactRoot, "QA_tests_coverage_report.md")
	if err := os.WriteFile(reportPath, []byte("# Final Codex Response\n\nConfirmed findings.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(artifactRoot, "logs", "D_tests_coverage_static.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("=== codex guidance events ===\n20m guidance sent at 2026-05-08T00:20:00Z\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := `{
  "project_path": "` + filepath.ToSlash(projectPath) + `",
  "path_warnings": [
    {"type":"stale_project_path","db_path":"` + filepath.ToSlash(filepath.Dir(projectPath)) + `","canonical_path":"` + filepath.ToSlash(projectPath) + `"}
  ]
}`
	if err := os.WriteFile(filepath.Join(artifactRoot, "run_manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.PutStage(ctx, run.RunID, model.StageRecord{Stage: "D", Status: model.StageDone, LogPath: logPath, ArtifactPaths: []string{reportPath}}); err != nil {
		t.Fatal(err)
	}

	probe, err := tuiapp.BuildExecutionProbeForTest(ctx, store, cfg, "TASK-1", "D", 90)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"最终报告", "Final Codex Response", "路径警告", "20m guidance sent"} {
		if !strings.Contains(probe.DetailContent, want) {
			t.Fatalf("detail content missing %q:\n%s", want, probe.DetailContent)
		}
	}
}

func TestExecutionViewModelOnlyIncludesUsableCompletedRefRuns(t *testing.T) {
	store, cfg, _, artifactRoot := tuiStore(t)
	ctx := context.Background()
	completed := model.RunRecord{RunID: "run-completed", TaskID: "TASK-1", StartedAt: "2026-04-30T00:03:00Z", Status: model.RunCompletedClean, ManualVerdict: model.ManualUnset, ArtifactRoot: artifactRoot}
	runningRoot := filepath.Join(filepath.Dir(artifactRoot), "run-running")
	if err := os.MkdirAll(runningRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	crashedRoot := filepath.Join(filepath.Dir(artifactRoot), "run-crashed")
	if err := os.MkdirAll(crashedRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	running := model.RunRecord{RunID: "run-running", TaskID: "TASK-1", StartedAt: "2026-04-30T00:01:00Z", Status: model.RunRunning, ManualVerdict: model.ManualUnset, ArtifactRoot: runningRoot}
	crashed := model.RunRecord{RunID: "run-crashed", TaskID: "TASK-1", StartedAt: "2026-04-30T00:02:00Z", Status: model.RunCrashed, ManualVerdict: model.ManualUnset, ArtifactRoot: crashedRoot}
	missing := model.RunRecord{RunID: "run-missing-artifacts", TaskID: "TASK-1", StartedAt: "2026-04-30T00:00:00Z", Status: model.RunCompletedWithFindings, ManualVerdict: model.ManualUnset, ArtifactRoot: filepath.Join(filepath.Dir(artifactRoot), "missing")}
	for _, run := range []model.RunRecord{completed, running, crashed, missing} {
		if err := store.CreateRun(ctx, run); err != nil {
			t.Fatal(err)
		}
	}

	probe, err := tuiapp.BuildExecutionProbeForTest(ctx, store, cfg, "TASK-1", "A", 80)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(probe.RefRunIDs, ",") != "run-completed" {
		t.Fatalf("ref runs should exclude unusable runs, got %#v", probe.RefRunIDs)
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

func TestExecutionDetailMergesRunningCodexStream(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).
		SeedExecutionRun("TASK-1", "run-1", []model.StageRecord{
			{Stage: "D", Status: model.StageRunning},
		}, "D").
		ApplySchedulerJobsForTest([]scheduler.JobSnapshot{{
			JobID:  "job-1",
			RunID:  "run-1",
			TaskID: "TASK-1",
			State:  scheduler.JobRunning,
			StreamByStage: map[string]pipeline.StreamUpdate{
				"D": {Stage: "D", Mode: pipeline.StreamModeCumulative, ItemID: "item-1", Text: "正在生成 Codex 审查正文", Truncated: true},
			},
		}})

	view := h.View()
	for _, want := range []string{"正在生成 Codex 审查正文", "预览已截断", "运行证据"} {
		if !strings.Contains(view, want) {
			t.Fatalf("stream detail missing %q:\n%s", want, view)
		}
	}
}

func TestExecutionDetailRendersAppendStreamAndClearsAfterReload(t *testing.T) {
	h := tuiapp.NewTestHarness(config.Default()).
		SeedExecutionRun("TASK-1", "run-1", []model.StageRecord{
			{Stage: "B", Status: model.StageRunning},
		}, "B").
		ApplySchedulerJobsForTest([]scheduler.JobSnapshot{{
			JobID:  "job-1",
			RunID:  "run-1",
			TaskID: "TASK-1",
			State:  scheduler.JobRunning,
			StreamByStage: map[string]pipeline.StreamUpdate{
				"B": {
					Stage: "B",
					Mode:  pipeline.StreamModeAppend,
					Lines: []pipeline.StreamLine{
						{Source: "stdout", Text: "container healthy"},
						{Source: "stderr", Text: "warning from docker"},
					},
				},
			},
		}})
	if view := h.View(); !strings.Contains(view, "container healthy") || !strings.Contains(view, "[stderr] warning from docker") {
		t.Fatalf("append stream not rendered:\n%s", view)
	}

	h = h.SeedExecutionRun("TASK-1", "run-1", []model.StageRecord{
		{Stage: "B", Status: model.StageDone},
	}, "B").ApplySchedulerJobsForTest([]scheduler.JobSnapshot{{
		JobID:  "job-1",
		RunID:  "run-1",
		TaskID: "TASK-1",
		State:  scheduler.JobRunning,
		StreamByStage: map[string]pipeline.StreamUpdate{
			"B": {Stage: "B", Mode: pipeline.StreamModeAppend, Lines: []pipeline.StreamLine{{Source: "stdout", Text: "old output"}}},
		},
	}})
	if view := h.View(); strings.Contains(view, "old output") {
		t.Fatalf("stream should not render once DB state marks stage non-running:\n%s", view)
	}
}

func TestExecutionDetailKeepsFinishedJobStreamUntilDBConfirmsStageDone(t *testing.T) {
	finishedJob := scheduler.JobSnapshot{
		JobID:  "job-1",
		RunID:  "run-1",
		TaskID: "TASK-1",
		State:  scheduler.JobDone,
		StreamByStage: map[string]pipeline.StreamUpdate{
			"C": {
				Stage: "C",
				Mode:  pipeline.StreamModeAppend,
				Done:  true,
				Lines: []pipeline.StreamLine{
					{Source: "stdout", Text: "last observed test output"},
				},
			},
		},
	}
	h := tuiapp.NewTestHarness(config.Default()).
		SeedExecutionRun("TASK-1", "run-1", []model.StageRecord{
			{Stage: "C", Status: model.StageRunning},
		}, "C").
		ApplySchedulerJobsForTest([]scheduler.JobSnapshot{finishedJob})
	if view := h.View(); !strings.Contains(view, "last observed test output") {
		t.Fatalf("finished job stream should remain while DB still says running:\n%s", view)
	}

	h = h.SeedExecutionRun("TASK-1", "run-1", []model.StageRecord{
		{Stage: "C", Status: model.StageDone},
	}, "C").ApplySchedulerJobsForTest([]scheduler.JobSnapshot{finishedJob})
	if view := h.View(); strings.Contains(view, "last observed test output") {
		t.Fatalf("stream should clear after DB confirms stage done:\n%s", view)
	}
}

func TestRunningStreamStaysPinnedToPrimaryContent(t *testing.T) {
	lines := make([]pipeline.StreamLine, 0, 40)
	for i := 0; i < 40; i++ {
		lines = append(lines, pipeline.StreamLine{Source: "stdout", Text: fmt.Sprintf("line-%02d", i)})
	}
	h := tuiapp.NewTestHarness(config.Default()).
		SetSize(100, 24).
		SeedExecutionRun("TASK-1", "run-1", []model.StageRecord{
			{Stage: "C", Status: model.StageRunning},
		}, "C").
		SetFocus("detail-viewport").
		ApplySchedulerJobsForTest([]scheduler.JobSnapshot{{
			JobID:  "job-1",
			RunID:  "run-1",
			TaskID: "TASK-1",
			State:  scheduler.JobRunning,
			StreamByStage: map[string]pipeline.StreamUpdate{
				"C": {Stage: "C", Mode: pipeline.StreamModeAppend, Lines: lines},
			},
		}})
	if h.DetailYOffset() != 0 {
		t.Fatalf("running stream should stay pinned to primary content, offset=%d", h.DetailYOffset())
	}
	view := h.View()
	if !strings.Contains(view, "line-39") || !strings.Contains(view, "运行证据") {
		t.Fatalf("running stream should show recent stream and evidence header, got:\n%s", view)
	}
	h, _ = h.Press("pgup")
	offsetAfterPageUp := h.DetailYOffset()
	if h.DetailFollowTail() {
		t.Fatal("PageUp should disable follow-tail")
	}
	h = h.ApplySchedulerJobsForTest([]scheduler.JobSnapshot{{
		JobID:  "job-1",
		RunID:  "run-1",
		TaskID: "TASK-1",
		State:  scheduler.JobRunning,
		StreamByStage: map[string]pipeline.StreamUpdate{
			"C": {Stage: "C", Mode: pipeline.StreamModeAppend, Lines: append(lines, pipeline.StreamLine{Source: "stdout", Text: "new line"})},
		},
	}})
	if h.DetailYOffset() != offsetAfterPageUp {
		t.Fatalf("PageUp should disable follow-tail, offset %d -> %d", offsetAfterPageUp, h.DetailYOffset())
	}
	h, _ = h.Press("home")
	if h.DetailFollowTail() {
		t.Fatal("Home should keep follow-tail disabled")
	}
	h, _ = h.Press("end")
	if !h.DetailFollowTail() {
		t.Fatal("End should restore follow-tail")
	}
	h = h.ApplySchedulerJobsForTest([]scheduler.JobSnapshot{{
		JobID:  "job-1",
		RunID:  "run-1",
		TaskID: "TASK-1",
		State:  scheduler.JobRunning,
		StreamByStage: map[string]pipeline.StreamUpdate{
			"C": {Stage: "C", Mode: pipeline.StreamModeAppend, Lines: append(lines, pipeline.StreamLine{Source: "stdout", Text: "another line"})},
		},
	}})
	if h.DetailYOffset() != 0 {
		t.Fatalf("restored follow-tail should return to primary content, offset=%d", h.DetailYOffset())
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
