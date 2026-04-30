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
