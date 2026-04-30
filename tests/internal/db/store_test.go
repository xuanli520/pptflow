package db_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

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

func TestMigratesLegacyGlobalFindingPrimaryKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	handle, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = handle.Exec(`CREATE TABLE findings (
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
