package db_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/xuanli520/p2r_tui/internal/db"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/scanner"
)

func TestFindingsAreScopedByRun(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.UpsertProjects(ctx, []scanner.Project{{TaskID: "TASK-1", Batch: "b", Path: t.TempDir()}}); err != nil {
		t.Fatal(err)
	}
	for _, runID := range []string{"run-1", "run-2"} {
		if err := store.CreateRun(ctx, model.RunRecord{RunID: runID, TaskID: "TASK-1", Status: model.RunRunning, ManualVerdict: model.ManualUnset, ArtifactRoot: t.TempDir()}); err != nil {
			t.Fatal(err)
		}
	}
	finding := model.Finding{ID: "P2R-A-BLK-001", Stage: "A", Severity: "Blocker", Title: "first", DoneCriteria: "done"}
	if err := store.InsertFindings(ctx, "run-1", []model.Finding{finding}); err != nil {
		t.Fatal(err)
	}
	finding.Title = "second"
	if err := store.InsertFindings(ctx, "run-2", []model.Finding{finding}); err != nil {
		t.Fatal(err)
	}
	finding.Title = "first updated"
	if err := store.InsertFindings(ctx, "run-1", []model.Finding{finding}); err != nil {
		t.Fatal(err)
	}
	run1, err := store.Findings(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	run2, err := store.Findings(ctx, "run-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(run1) != 1 || run1[0].Title != "first updated" || run1[0].DoneCriteria != "done" {
		t.Fatalf("unexpected run1 findings: %#v", run1)
	}
	if len(run2) != 1 || run2[0].Title != "second" {
		t.Fatalf("unexpected run2 findings: %#v", run2)
	}
}

func TestProjectOnlyRunCanPersistRuntimeStage(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	projectPath := t.TempDir()
	if err := store.UpsertProjects(ctx, []scanner.Project{{TaskID: "TASK-PROJECT", Batch: "batch-1", Path: projectPath}}); err != nil {
		t.Fatal(err)
	}
	run := model.RunRecord{
		RunID:         "run-project-only",
		TaskID:        "TASK-PROJECT",
		StartedAt:     time.Now().UTC().Format(time.RFC3339),
		Status:        model.RunRunning,
		ManualVerdict: model.ManualUnset,
		ArtifactRoot:  t.TempDir(),
	}
	if err := store.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	stage := model.StageRecord{Stage: "B", Name: "Docker runtime evidence", Status: model.StageDone}
	meta := model.ComposeMeta{Project: "p2rqa_project", ComposeFiles: []string{"compose.yml"}, WorkDir: projectPath}
	if err := store.PutStageAndRecordTaskRuntime(ctx, run.RunID, stage, run.TaskID, "http://127.0.0.1:32770", true, meta); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkTaskDockerStopped(ctx, run.TaskID); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, run.RunID, run.TaskID, model.RunCompletedClean, time.Second); err != nil {
		t.Fatal(err)
	}
	stages, err := store.Stages(ctx, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) != 1 || stages[0].Stage != "B" || stages[0].Status != model.StageDone {
		t.Fatalf("stages = %#v", stages)
	}
	summaries, err := store.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].HasTask || summaries[0].RunCount != 1 || summaries[0].LastRunID != run.RunID {
		t.Fatalf("project summary = %#v", summaries)
	}
}

func TestConcurrentWritesAreSerialized(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	projectPath := t.TempDir()
	if err := store.UpsertProjects(ctx, []scanner.Project{{TaskID: "TASK-1", Batch: "b", Path: projectPath}}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 3)
	artifactRoots := []string{t.TempDir(), t.TempDir(), t.TempDir()}
	for i := 0; i < 3; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			runID := "run-concurrent-" + string(rune('A'+i))
			run := model.RunRecord{
				RunID:         runID,
				TaskID:        "TASK-1",
				StartedAt:     time.Now().UTC().Format(time.RFC3339),
				Status:        model.RunRunning,
				ManualVerdict: model.ManualUnset,
				ArtifactRoot:  artifactRoots[i],
			}
			if err := store.CreateRun(ctx, run); err != nil {
				errCh <- err
				return
			}
			if err := store.PutStage(ctx, runID, model.StageRecord{Stage: "A", Status: model.StageDone}); err != nil {
				errCh <- err
				return
			}
			if err := store.InsertFindings(ctx, runID, []model.Finding{{ID: "F-1", Stage: "A", Severity: "High", Title: "finding"}}); err != nil {
				errCh <- err
				return
			}
			if err := store.FinishRun(ctx, runID, "TASK-1", model.RunCompletedWithFindings, time.Second); err != nil {
				errCh <- err
				return
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	runs, err := store.ListRunsForTask(ctx, "TASK-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 3 {
		t.Fatalf("run count = %d, want 3", len(runs))
	}
}

func TestCreateRunMakesRunningRunLatest(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.UpsertProjects(ctx, []scanner.Project{{TaskID: "TASK-1", Batch: "b", Path: t.TempDir()}}); err != nil {
		t.Fatal(err)
	}
	old := model.RunRecord{RunID: "run-old", TaskID: "TASK-1", StartedAt: "2026-04-30T00:00:00Z", Status: model.RunCompletedClean, ManualVerdict: model.ManualUnset, ArtifactRoot: t.TempDir()}
	if err := store.CreateRun(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, old.RunID, old.TaskID, model.RunCompletedClean, 0); err != nil {
		t.Fatal(err)
	}
	running := model.RunRecord{RunID: "run-running", TaskID: "TASK-1", StartedAt: "2026-04-30T00:01:00Z", Status: model.RunRunning, ManualVerdict: model.ManualUnset, ArtifactRoot: t.TempDir()}
	if err := store.CreateRun(ctx, running); err != nil {
		t.Fatal(err)
	}
	latest, err := store.LatestRunForTask(ctx, "TASK-1")
	if err != nil {
		t.Fatal(err)
	}
	if latest.RunID != running.RunID || latest.Status != model.RunRunning {
		t.Fatalf("latest run = %#v, want running run", latest)
	}
	projects, err := store.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0].LastRunID != running.RunID || projects[0].RunStatus != model.RunRunning {
		t.Fatalf("project summary did not surface running run: %#v", projects)
	}
}

func TestListProjectsPaginatedReturnsPageAndTotal(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	upsertTaskIDs(t, store, "TASK-1", "TASK-2", "TASK-3")

	projects, total, err := store.ListProjectsPaginated(ctx, db.ProjectQuery{Sort: db.ProjectSortTaskID, Asc: true, Limit: 2, Offset: 1})
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}
	if got := projectIDs(projects); !slices.Equal(got, []string{"TASK-2", "TASK-3"}) {
		t.Fatalf("page IDs = %#v", got)
	}
}

func TestListProjectsPaginatedNormalizesInvalidLimit(t *testing.T) {
	store, _ := newStore(t)
	for i := 0; i < 25; i++ {
		upsertTaskIDs(t, store, "TASK-"+string(rune('A'+i)))
	}

	projects, total, err := store.ListProjectsPaginated(context.Background(), db.ProjectQuery{Limit: 7})
	if err != nil {
		t.Fatal(err)
	}
	if total != 25 || len(projects) != 20 {
		t.Fatalf("total/page = %d/%d, want 25/20", total, len(projects))
	}
}

func TestListProjectsPaginatedSortsStatusSeverityLastRunAndVerdict(t *testing.T) {
	store, path := newStore(t)
	ctx := context.Background()
	upsertTaskIDs(t, store, "TASK-RUNNING", "TASK-CRASHED", "TASK-FINDINGS", "TASK-ABORTED", "TASK-CLEAN", "TASK-NONE")
	createRun(t, store, "run-running", "TASK-RUNNING", "2026-05-07T00:05:00Z", model.RunRunning, model.ManualUnset)
	createRun(t, store, "run-crashed", "TASK-CRASHED", "2026-05-07T00:04:00Z", model.RunCrashed, model.ManualFail)
	createRun(t, store, "run-findings", "TASK-FINDINGS", "2026-05-07T00:03:00Z", model.RunCompletedWithFindings, model.ManualRework)
	createRun(t, store, "run-aborted", "TASK-ABORTED", "2026-05-07T00:02:00Z", model.RunAborted, model.ManualUnset)
	createRun(t, store, "run-clean", "TASK-CLEAN", "2026-05-07T00:01:00Z", model.RunCompletedClean, model.ManualPass)
	insertFinding(t, store, "run-findings", "Blocker", "finding-blocker")
	insertFinding(t, store, "run-clean", "High", "finding-high-1")
	insertFinding(t, store, "run-clean", "High", "finding-high-2")

	statusPage, _, err := store.ListProjectsPaginated(ctx, db.ProjectQuery{Sort: db.ProjectSortStatus, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if got := projectIDs(statusPage); !slices.Equal(got, []string{"TASK-RUNNING", "TASK-CRASHED", "TASK-FINDINGS", "TASK-ABORTED", "TASK-CLEAN", "TASK-NONE"}) {
		t.Fatalf("status order = %#v", got)
	}

	severityPage, _, err := store.ListProjectsPaginated(ctx, db.ProjectQuery{Sort: db.ProjectSortSeverity, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if got := projectIDs(severityPage[:2]); !slices.Equal(got, []string{"TASK-FINDINGS", "TASK-CLEAN"}) {
		t.Fatalf("severity should rank blocker before multiple high findings, got %#v", projectIDs(severityPage))
	}

	verdictPage, _, err := store.ListProjectsPaginated(ctx, db.ProjectQuery{Sort: db.ProjectSortVerdict, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if got := projectIDs(verdictPage[:4]); !slices.Equal(got, []string{"TASK-CRASHED", "TASK-FINDINGS", "TASK-ABORTED", "TASK-NONE"}) {
		t.Fatalf("verdict order = %#v", projectIDs(verdictPage))
	}

	updateProjectLastRun(t, path, "TASK-CLEAN", "run-crashed", "2099-01-01T00:00:00Z")
	lastRunPage, _, err := store.ListProjectsPaginated(ctx, db.ProjectQuery{Sort: db.ProjectSortLastRun, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if got := projectIDs(lastRunPage[:2]); !slices.Equal(got, []string{"TASK-RUNNING", "TASK-CRASHED"}) {
		t.Fatalf("last run order should ignore stale project last_run fields, got %#v", projectIDs(lastRunPage))
	}
}

func TestListProjectsPaginatedSearchesTermsAndEscapesLikeWildcards(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	upsertTaskIDs(t, store, "TASK-1", "TASK-2", "TASK%100", "TASK-100")
	createRun(t, store, "run-pass", "TASK-1", "2026-05-07T00:00:00Z", model.RunCompletedClean, model.ManualPass)
	createRun(t, store, "run-fail", "TASK-2", "2026-05-07T00:01:00Z", model.RunCompletedWithFindings, model.ManualFail)
	if err := store.PutStage(ctx, "run-fail", model.StageRecord{Stage: "D", Status: model.StageFailed}); err != nil {
		t.Fatal(err)
	}

	projects, _, err := store.ListProjectsPaginated(ctx, db.ProjectQuery{
		Limit: 20,
		Search: db.ProjectSearch{Terms: []db.ProjectSearchTerm{
			{Text: "TASK"},
			{Text: "通过", Verdicts: []string{model.ManualPass}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := projectIDs(projects); !slices.Equal(got, []string{"TASK-1"}) {
		t.Fatalf("multi-term search = %#v", got)
	}

	projects, _, err = store.ListProjectsPaginated(ctx, db.ProjectQuery{
		Limit:  20,
		Search: db.ProjectSearch{Terms: []db.ProjectSearchTerm{{Text: "阶段D", FailedStages: []string{"D"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := projectIDs(projects); !slices.Equal(got, []string{"TASK-2"}) {
		t.Fatalf("stage filter search = %#v", got)
	}

	projects, _, err = store.ListProjectsPaginated(ctx, db.ProjectQuery{
		Limit:  20,
		Search: db.ProjectSearch{Terms: []db.ProjectSearchTerm{{Text: "TASK%100"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := projectIDs(projects); !slices.Equal(got, []string{"TASK%100"}) {
		t.Fatalf("escaped wildcard search = %#v", got)
	}
}

func TestListProjectsChoosesRuntimeFailureAsPrimaryFailedStage(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	upsertTaskIDs(t, store, "TASK-RUNTIME")
	createRun(t, store, "run-runtime", "TASK-RUNTIME", "2026-05-07T00:01:00Z", model.RunCompletedWithFindings, model.ManualFail)
	for _, stage := range []model.StageRecord{
		{Stage: "A", Status: model.StageFailed, ErrorSummary: "local-dependency-risk"},
		{Stage: "B", Status: model.StageFailed, ErrorSummary: "docker build failed"},
		{Stage: "G", Status: model.StageBlocked, ErrorSummary: "blocked by B"},
	} {
		if err := store.PutStage(ctx, "run-runtime", stage); err != nil {
			t.Fatal(err)
		}
	}
	projects, _, err := store.ListProjectsPaginated(ctx, db.ProjectQuery{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0].FailedStage != "B" {
		t.Fatalf("primary failed stage = %#v", projects)
	}
}

func TestListProjectsPrimaryFailedStagePriorityMatrix(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stages []model.StageRecord
		want   string
	}{
		{
			name: "G failed beats C failed",
			stages: []model.StageRecord{
				{Stage: "C", Status: model.StageFailed, ErrorSummary: "run_tests failed"},
				{Stage: "G", Status: model.StageFailed, ErrorSummary: "frontend E2E failed"},
			},
			want: "G",
		},
		{
			name: "G failed beats B blocked",
			stages: []model.StageRecord{
				{Stage: "B", Status: model.StageBlocked, ErrorSummary: "docker unavailable"},
				{Stage: "G", Status: model.StageFailed, ErrorSummary: "frontend E2E failed"},
			},
			want: "G",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, _ := newStore(t)
			ctx := context.Background()
			upsertTaskIDs(t, store, "TASK-PRIMARY")
			createRun(t, store, "run-primary", "TASK-PRIMARY", "2026-05-07T00:01:00Z", model.RunCompletedWithFindings, model.ManualFail)
			for _, stage := range tc.stages {
				if err := store.PutStage(ctx, "run-primary", stage); err != nil {
					t.Fatal(err)
				}
			}
			projects, _, err := store.ListProjectsPaginated(ctx, db.ProjectQuery{Limit: 20})
			if err != nil {
				t.Fatal(err)
			}
			if len(projects) != 1 || projects[0].FailedStage != tc.want {
				t.Fatalf("primary failed stage = %#v, want %s", projects, tc.want)
			}
		})
	}
}

func TestListProjectsAndPaginatedSummariesMatch(t *testing.T) {
	store, _ := newStore(t)
	upsertTaskIDs(t, store, "TASK-1", "TASK-2")
	createRun(t, store, "run-1", "TASK-1", "2026-05-07T00:00:00Z", model.RunCompletedWithFindings, "")
	insertFinding(t, store, "run-1", "Blocker", "finding-1")

	all, err := store.ListProjects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	page, total, err := store.ListProjectsPaginated(context.Background(), db.ProjectQuery{Sort: db.ProjectSortTaskID, Asc: true, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if total != len(all) || !reflect.DeepEqual(all, page) {
		t.Fatalf("ListProjects = %#v, paginated total/page = %d/%#v", all, total, page)
	}
	if all[0].ManualVerdict != model.ManualUnset {
		t.Fatalf("empty manual verdict should normalize to unset, got %#v", all[0])
	}
}

func TestScanPruneArtifactsRemovesSnapshotProjectWithoutRuns(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	root := t.TempDir()
	snapshotPath := filepath.Join(root, "result", "batch-1", "TASK-1", "run-1", "script_input_snapshot")
	if err := store.UpsertProjects(ctx, []scanner.Project{{TaskID: "TASK-SNAPSHOT", Batch: "batch-1", Path: snapshotPath}}); err != nil {
		t.Fatal(err)
	}
	pruned, err := store.PruneArtifactProjects(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(pruned.Removed) != 1 || pruned.Removed[0].TaskID != "TASK-SNAPSHOT" {
		t.Fatalf("unexpected prune result: %#v", pruned)
	}
	projects, err := store.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 0 {
		t.Fatalf("artifact project should be removed: %#v", projects)
	}
}

func TestScanPruneArtifactsSkipsProjectWithRuns(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	root := t.TempDir()
	snapshotPath := filepath.Join(root, "TASK-OLD", "qa", "runs", "run-1", "script_input_snapshot")
	if err := store.UpsertProjects(ctx, []scanner.Project{{TaskID: "TASK-SNAPSHOT", Batch: "batch-1", Path: snapshotPath}}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRun(ctx, model.RunRecord{RunID: "run-1", TaskID: "TASK-SNAPSHOT", Status: model.RunRunning, ManualVerdict: model.ManualUnset, ArtifactRoot: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	pruned, err := store.PruneArtifactProjects(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(pruned.Removed) != 0 || len(pruned.Skipped) != 1 || pruned.Skipped[0].Runs != 1 {
		t.Fatalf("unexpected prune result: %#v", pruned)
	}
	projects, err := store.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 {
		t.Fatalf("project with runs should remain: %#v", projects)
	}
}

func TestScanPruneArtifactsDoesNotRemoveNormalProject(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	root := t.TempDir()
	projectPath := filepath.Join(root, "batch-1", "TASK-1", "TASK-1")
	if err := store.UpsertProjects(ctx, []scanner.Project{{TaskID: "TASK-1", Batch: "batch-1", Path: projectPath}}); err != nil {
		t.Fatal(err)
	}
	pruned, err := store.PruneArtifactProjects(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(pruned.Removed) != 0 || len(pruned.Skipped) != 0 {
		t.Fatalf("normal project should not be pruned: %#v", pruned)
	}
	projects, err := store.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 {
		t.Fatalf("normal project should remain: %#v", projects)
	}
}

func TestScanPruneArtifactsRequiresPathUnderScanRoot(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	root := t.TempDir()
	outsidePath := filepath.Join(t.TempDir(), "result", "batch-1", "TASK-1", "run-1", "script_input_snapshot")
	if err := store.UpsertProjects(ctx, []scanner.Project{{TaskID: "TASK-OUTSIDE", Batch: "batch-1", Path: outsidePath}}); err != nil {
		t.Fatal(err)
	}
	pruned, err := store.PruneArtifactProjects(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(pruned.Removed) != 0 || len(pruned.Skipped) != 0 {
		t.Fatalf("outside artifact-looking path should not be pruned: %#v", pruned)
	}
	projects, err := store.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 {
		t.Fatalf("outside project should remain: %#v", projects)
	}
}

func TestMigratesLegacyGlobalFindingPrimaryKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	handle, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = handle.Exec(`CREATE TABLE projects (
			task_id TEXT PRIMARY KEY,
			batch TEXT NOT NULL,
			path TEXT NOT NULL,
			run_count INTEGER DEFAULT 0,
			last_run_id TEXT,
			last_run_at TEXT,
			created_at TEXT DEFAULT (datetime('now'))
		);
		CREATE TABLE runs (
			run_id TEXT PRIMARY KEY,
			task_id TEXT NOT NULL REFERENCES projects(task_id),
			started_at TEXT,
			finished_at TEXT,
			status TEXT DEFAULT 'running',
			manual_verdict TEXT DEFAULT 'unset',
			static_only INTEGER DEFAULT 0,
			duration_ms INTEGER DEFAULT 0,
			artifact_root TEXT NOT NULL,
			tool_versions TEXT,
			prompt_versions TEXT
		);
		CREATE TABLE findings (
			id TEXT PRIMARY KEY,
			run_id TEXT NOT NULL,
			stage TEXT,
			severity TEXT NOT NULL,
		title TEXT NOT NULL,
		rule TEXT,
		evidence TEXT,
		impact TEXT,
			minimum_fix TEXT,
			source_path TEXT
		);
		INSERT INTO projects(task_id, batch, path) VALUES('TASK-LEGACY', 'batch', '/tmp/TASK-LEGACY');
		INSERT INTO runs(run_id, task_id, artifact_root) VALUES('run-legacy', 'TASK-LEGACY', '/tmp/artifacts');
		INSERT INTO findings(id, run_id, stage, severity, title) VALUES('P2R-A-BLK-001', 'run-legacy', 'A', 'Blocker', 'legacy');`)
	if err != nil {
		t.Fatal(err)
	}
	_ = handle.Close()
	store, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	findings, err := store.Findings(context.Background(), "run-legacy")
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].Title != "legacy" {
		t.Fatalf("legacy finding not preserved: %#v", findings)
	}
}

func TestMigratesRunStageNameAndFallsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-stage.db")
	handle, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = handle.Exec(`CREATE TABLE projects (
		task_id TEXT PRIMARY KEY,
		batch TEXT NOT NULL,
		path TEXT NOT NULL,
		run_count INTEGER DEFAULT 0,
		last_run_id TEXT,
		last_run_at TEXT,
		created_at TEXT DEFAULT (datetime('now'))
	);
	CREATE TABLE runs (
		run_id TEXT PRIMARY KEY,
		task_id TEXT NOT NULL,
		started_at TEXT,
		finished_at TEXT,
		status TEXT DEFAULT 'running',
		manual_verdict TEXT DEFAULT 'unset',
		static_only INTEGER DEFAULT 0,
		duration_ms INTEGER DEFAULT 0,
		artifact_root TEXT NOT NULL,
		tool_versions TEXT,
		prompt_versions TEXT
	);
	CREATE TABLE run_stages (
		run_id TEXT NOT NULL,
		stage TEXT NOT NULL,
		status TEXT NOT NULL,
		started_at TEXT,
		finished_at TEXT,
		duration_ms INTEGER DEFAULT 0,
		blocked_by TEXT,
		log_path TEXT,
		artifact_json TEXT,
		error_summary TEXT,
		PRIMARY KEY (run_id, stage)
	);
	CREATE TABLE findings (
		id TEXT NOT NULL,
		run_id TEXT NOT NULL,
		stage TEXT,
		severity TEXT NOT NULL,
		title TEXT NOT NULL,
		rule TEXT,
		evidence TEXT,
		impact TEXT,
		done_criteria TEXT,
		minimum_fix TEXT,
		source_path TEXT,
		PRIMARY KEY (run_id, id)
	);
	INSERT INTO projects(task_id,batch,path,last_run_id) VALUES('TASK-1','b','/tmp/project','run-1');
	INSERT INTO runs(run_id,task_id,status,manual_verdict,artifact_root) VALUES('run-1','TASK-1','completed_clean','unset','/tmp/artifacts');
	INSERT INTO run_stages(run_id,stage,status) VALUES('run-1','D','done');`)
	if err != nil {
		t.Fatal(err)
	}
	_ = handle.Close()
	store, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	stages, err := store.Stages(context.Background(), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) != 1 || stages[0].Name != "tests effectiveness static review" {
		t.Fatalf("stage name fallback failed: %#v", stages)
	}
	if err := store.PutStage(context.Background(), "run-1", model.StageRecord{Stage: "E", Status: model.StageDone}); err != nil {
		t.Fatal(err)
	}
	stages, err = store.Stages(context.Background(), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if stages[1].Name != "static acceptance audit" {
		t.Fatalf("persisted stage name fallback failed: %#v", stages)
	}
}

func TestMigratesReadIndexesForEmptyAndCurrentDatabases(t *testing.T) {
	emptyPath := filepath.Join(t.TempDir(), "empty.db")
	emptyStore, err := db.Open(emptyPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = emptyStore.Close()
	assertReadIndexes(t, emptyPath)

	currentPath := filepath.Join(t.TempDir(), "current.db")
	handle, err := sql.Open("sqlite", currentPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = handle.Exec(`CREATE TABLE projects (
		task_id TEXT PRIMARY KEY,
		batch TEXT NOT NULL,
		path TEXT NOT NULL,
		run_count INTEGER DEFAULT 0,
		last_run_id TEXT,
		last_run_at TEXT,
		created_at TEXT DEFAULT (datetime('now'))
	);
	CREATE TABLE runs (
		run_id TEXT PRIMARY KEY,
		task_id TEXT NOT NULL REFERENCES projects(task_id),
		started_at TEXT,
		finished_at TEXT,
		status TEXT DEFAULT 'running',
		manual_verdict TEXT DEFAULT 'unset',
		static_only INTEGER DEFAULT 0,
		duration_ms INTEGER DEFAULT 0,
		artifact_root TEXT NOT NULL,
		tool_versions TEXT,
		prompt_versions TEXT
	);
	CREATE TABLE run_stages (
		run_id TEXT NOT NULL REFERENCES runs(run_id),
		stage TEXT NOT NULL,
		name TEXT,
		status TEXT NOT NULL,
		started_at TEXT,
		finished_at TEXT,
		duration_ms INTEGER DEFAULT 0,
		blocked_by TEXT,
		log_path TEXT,
		artifact_json TEXT,
		error_summary TEXT,
		PRIMARY KEY (run_id, stage)
	);
	CREATE TABLE findings (
		id TEXT NOT NULL,
		run_id TEXT NOT NULL REFERENCES runs(run_id),
		stage TEXT,
		severity TEXT NOT NULL,
		title TEXT NOT NULL,
		rule TEXT,
		evidence TEXT,
		impact TEXT,
		done_criteria TEXT,
		minimum_fix TEXT,
		source_path TEXT,
		PRIMARY KEY (run_id, id)
	);
	CREATE TABLE schema_version (
		id INTEGER PRIMARY KEY CHECK(id = 1),
		version INTEGER NOT NULL
	);
	INSERT INTO projects(task_id, batch, path) VALUES('TASK-OLD', 'legacy-batch', '/tmp/TASK-OLD');
	INSERT INTO schema_version(id, version) VALUES(1, 4);`)
	if err != nil {
		t.Fatal(err)
	}
	_ = handle.Close()
	currentStore, err := db.Open(currentPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = currentStore.Close()
	assertReadIndexes(t, currentPath)
	assertV5TaskSchema(t, currentPath)
}

func newStore(t *testing.T) (*db.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "index.db")
	store, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, path
}

func upsertTaskIDs(t *testing.T, store *db.Store, taskIDs ...string) {
	t.Helper()
	projects := make([]scanner.Project, 0, len(taskIDs))
	for _, taskID := range taskIDs {
		projects = append(projects, scanner.Project{TaskID: taskID, Batch: "batch", Path: filepath.Join(t.TempDir(), taskID)})
	}
	if err := store.UpsertProjects(context.Background(), projects); err != nil {
		t.Fatal(err)
	}
}

func createRun(t *testing.T, store *db.Store, runID, taskID, startedAt, status, verdict string) {
	t.Helper()
	if err := store.CreateRun(context.Background(), model.RunRecord{
		RunID:         runID,
		TaskID:        taskID,
		StartedAt:     startedAt,
		Status:        status,
		ManualVerdict: verdict,
		ArtifactRoot:  t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
}

func insertFinding(t *testing.T, store *db.Store, runID, severity, id string) {
	t.Helper()
	if err := store.InsertFindings(context.Background(), runID, []model.Finding{{
		ID:       id,
		Stage:    "D",
		Severity: severity,
		Title:    id,
	}}); err != nil {
		t.Fatal(err)
	}
}

func updateProjectLastRun(t *testing.T, path, taskID, runID, lastRunAt string) {
	t.Helper()
	handle, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	if _, err := handle.Exec(`UPDATE projects SET last_run_id = ?, last_run_at = ? WHERE task_id = ?`, runID, lastRunAt, taskID); err != nil {
		t.Fatal(err)
	}
}

func projectIDs(projects []db.ProjectSummary) []string {
	ids := make([]string, 0, len(projects))
	for _, project := range projects {
		ids = append(ids, project.TaskID)
	}
	return ids
}

func assertReadIndexes(t *testing.T, path string) {
	t.Helper()
	handle, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	want := []string{
		"idx_runs_task_started",
		"idx_runs_task_status",
		"idx_runs_task_completion_round",
		"idx_run_stages_run_status_stage",
		"idx_findings_run_severity",
		"idx_projects_batch",
		"idx_tasks_state",
		"idx_tasks_batch",
		"idx_tasks_batch_state",
		"idx_tasks_state_docker",
	}
	for _, index := range want {
		var name string
		err := handle.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`, index).Scan(&name)
		if err != nil {
			t.Fatalf("missing index %s: %v", index, err)
		}
	}
}

func assertV5TaskSchema(t *testing.T, path string) {
	t.Helper()
	handle, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	for table, columns := range map[string][]string{
		"tasks":   {"id", "batch_id", "git_url", "repo_path", "state", "current_run_id", "completion_count", "frontend_url", "docker_running", "compose_meta", "entered_waiting_at", "last_completed_at", "sync_error", "created_at", "updated_at"},
		"batches": {"id", "display_name", "task_count", "max_tasks", "created_at", "is_full"},
		"runs":    {"completion_round"},
	} {
		got := tableColumnSet(t, handle, table)
		for _, column := range columns {
			if !got[column] {
				t.Fatalf("table %s missing column %s; got %#v", table, column, got)
			}
		}
	}
	var batchID string
	if err := handle.QueryRow(`SELECT id FROM batches WHERE id = 'legacy-batch'`).Scan(&batchID); err != nil {
		t.Fatalf("legacy project batch should be backfilled: %v", err)
	}
	rows, err := handle.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign_key_check reported migrated data conflicts")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func tableColumnSet(t *testing.T, handle *sql.DB, table string) map[string]bool {
	t.Helper()
	rows, err := handle.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			t.Fatal(err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return columns
}
