package db_test

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/xuanli520/p2r_tui/internal/db"
	"github.com/xuanli520/p2r_tui/internal/scanner"
)

func TestOpenHandlesQuestionMarkInDatabaseFilename(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("question mark is not a valid Windows filename")
	}
	path := filepath.Join(t.TempDir(), "index?compat.db")
	store, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if err := store.UpsertProjects(context.Background(), []scanner.Project{{TaskID: "TASK-1", Batch: "batch", Path: t.TempDir()}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetProject(context.Background(), "TASK-1"); err != nil {
		t.Fatal(err)
	}
}
