package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func tempDB(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenAndMigrate(t *testing.T) {
	s := tempDB(t)
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM tasks").Scan(&count); err != nil {
		t.Fatalf("tasks table query: %v", err)
	}
	if err := s.db.QueryRow("SELECT COUNT(*) FROM runs").Scan(&count); err != nil {
		t.Fatalf("runs table query: %v", err)
	}
	if err := s.db.QueryRow("SELECT COUNT(*) FROM schema_version").Scan(&count); err != nil {
		t.Fatalf("schema_version query: %v", err)
	}
	if count == 0 {
		t.Error("expected schema_version row")
	}
}

func TestOpenReadOnlyPreservesControlPlaneAndRejectsMutations(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writable, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	task, err := writable.CreateTaskV2(ctx, CreateTaskV2Request{Slug: "read-only-fixture", Actor: "tester", Reason: "fixture"})
	if err != nil {
		_ = writable.Close()
		t.Fatal(err)
	}
	if err := writable.Close(); err != nil {
		t.Fatal(err)
	}
	before := snapshotStoreFiles(t, root)

	readOnly, err := OpenReadOnly(root)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := readOnly.GetTaskV2(ctx, task.ID)
	if err != nil || loaded == nil || loaded.ID != task.ID {
		_ = readOnly.Close()
		t.Fatalf("read-only task lookup = %+v, %v", loaded, err)
	}
	if _, err := readOnly.CreateTaskV2(ctx, CreateTaskV2Request{Slug: "must-not-write", Actor: "tester", Reason: "mutation rejection"}); !errors.Is(err, ErrReadOnly) {
		_ = readOnly.Close()
		t.Fatalf("read-only mutation error = %v, want ErrReadOnly", err)
	}
	if err := readOnly.Close(); err != nil {
		t.Fatal(err)
	}
	after := snapshotStoreFiles(t, root)
	if !reflect.DeepEqual(after, before) {
		t.Fatal("read-only store changed control-plane or backup files")
	}
}

func TestOpenReadOnlyDoesNotInitializeMissingStore(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent-control-plane")
	if _, err := OpenReadOnly(root); err == nil {
		t.Fatal("read-only open initialized a missing control plane")
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only open created root %q: %v", root, err)
	}
}

func snapshotStoreFiles(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files := make(map[string][]byte)
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		if path == filepath.Join(root, dbFileName+"-wal") || path == filepath.Join(root, dbFileName+"-shm") {
			return nil
		}
		if !info.Mode().IsRegular() {
			return errors.New("store snapshot encountered non-regular file")
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(relative)] = content
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestUpsertTask(t *testing.T) {
	s := tempDB(t)
	id, err := s.UpsertTask(Task{
		TaskDir:     "/tmp/test-task",
		TaskName:    "test-task",
		CodeLang:    "go",
		TaskType:    "web",
		Application: "test",
		RepoURL:     "https://github.com/example/repo",
		CommitSHA:   "abc123",
	})
	if err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	if id == 0 {
		t.Error("expected non-zero id")
	}

	// Upsert again with same TaskDir should update and return same id
	id2, err := s.UpsertTask(Task{
		TaskDir:  "/tmp/test-task",
		TaskName: "updated-name",
	})
	if err != nil {
		t.Fatalf("second UpsertTask: %v", err)
	}
	if id2 != id {
		t.Errorf("expected same id on upsert, got %d != %d", id2, id)
	}

	// Verify update preserved fields that were not overwritten
	got, err := s.GetTaskByDir("/tmp/test-task")
	if err != nil {
		t.Fatalf("GetTaskByDir: %v", err)
	}
	if got == nil {
		t.Fatal("task not found")
	}
	if got.CodeLang != "go" {
		t.Errorf("expected CodeLang='go', got %q", got.CodeLang)
	}
	if got.RepoURL != "https://github.com/example/repo" {
		t.Errorf("expected RepoURL preserved, got %q", got.RepoURL)
	}
	tasks, err := s.SearchTasks("updated")
	if err != nil || len(tasks) != 1 || tasks[0].ID != id {
		t.Fatalf("SearchTasks returned %+v err=%v", tasks, err)
	}
}

func TestUpsertRun(t *testing.T) {
	s := tempDB(t)
	taskID, _ := s.UpsertTask(Task{TaskDir: "/tmp/t", TaskName: "t"})

	now := time.Now().UTC()
	err := s.UpsertRun(Run{
		TaskID:        taskID,
		WorkspacePath: "/tmp/ws1",
		RunID:         "run-1",
		Status:        "succeeded",
		Passed:        true,
		StartedAt:     now.Add(-1 * time.Hour),
		FinishedAt:    now,
		SizeBytes:     1024,
	})
	if err != nil {
		t.Fatalf("UpsertRun: %v", err)
	}

	got, err := s.GetRunByWorkspace("/tmp/ws1")
	if err != nil {
		t.Fatalf("GetRunByWorkspace: %v", err)
	}
	if got == nil {
		t.Fatal("run not found")
	}
	if got.Status != "succeeded" {
		t.Errorf("expected status='succeeded', got %q", got.Status)
	}
	if got.SizeBytes != 1024 {
		t.Errorf("expected size=1024, got %d", got.SizeBytes)
	}

	// Upsert again updates
	err = s.UpsertRun(Run{
		TaskID:        taskID,
		WorkspacePath: "/tmp/ws1",
		RunID:         "run-1",
		Status:        "failed",
		Passed:        false,
		SizeBytes:     0, // zero should preserve old value
	})
	if err != nil {
		t.Fatalf("second UpsertRun: %v", err)
	}
	got, err = s.GetRunByWorkspace("/tmp/ws1")
	if err != nil {
		t.Fatalf("GetRunByWorkspace after update: %v", err)
	}
	if got.Status != "failed" {
		t.Errorf("expected updated status='failed', got %q", got.Status)
	}
	if got.SizeBytes != 1024 {
		t.Errorf("expected size preserved at 1024, got %d", got.SizeBytes)
	}
}

func TestListRuns(t *testing.T) {
	s := tempDB(t)
	tid1, _ := s.UpsertTask(Task{TaskDir: "/tmp/a", TaskName: "alpha", CodeLang: "py"})
	tid2, _ := s.UpsertTask(Task{TaskDir: "/tmp/b", TaskName: "beta", CodeLang: "go"})

	now := time.Now().UTC()
	s.UpsertRun(Run{TaskID: tid1, WorkspacePath: "/tmp/ws-a1", RunID: "r1", Status: "succeeded", StartedAt: now.Add(-2 * time.Hour)})
	s.UpsertRun(Run{TaskID: tid2, WorkspacePath: "/tmp/ws-b1", RunID: "r2", Status: "failed", StartedAt: now.Add(-1 * time.Hour)})

	runs, err := s.ListRuns(SortByStartedAt, false, "")
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Errorf("expected 2 runs, got %d", len(runs))
	}
	if runs[0].Task.TaskName != "beta" { // descending by started_at
		t.Errorf("expected beta first, got %s", runs[0].Task.TaskName)
	}

	// Filter by language
	runs, err = s.ListRuns(SortByStartedAt, false, "py")
	if err != nil {
		t.Fatalf("ListRuns with filter: %v", err)
	}
	if len(runs) != 1 {
		t.Errorf("expected 1 run with filter 'py', got %d", len(runs))
	}
	if runs[0].Task.CodeLang != "py" {
		t.Errorf("expected py, got %s", runs[0].Task.CodeLang)
	}

	runs, err = s.SearchRuns("失败")
	if err != nil {
		t.Fatalf("SearchRuns with Chinese status: %v", err)
	}
	if len(runs) != 1 || runs[0].Run.Status != "failed" {
		t.Fatalf("Chinese status search returned %+v", runs)
	}
	taskRuns, err := s.ListRunsByTask(tid1)
	if err != nil || len(taskRuns) != 1 || taskRuns[0].RunID != "r1" {
		t.Fatalf("ListRunsByTask returned %+v err=%v", taskRuns, err)
	}
}

func TestSyncFromFilesystem(t *testing.T) {
	s := tempDB(t)

	// Create a mock workspace
	wsDir := filepath.Join(t.TempDir(), "harbor-factory", "workspace")
	if err := os.MkdirAll(wsDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Write state.json
	stateJSON := `{"run_id":"run-test","status":"succeeded","passed":true,"started_at":"2026-01-01T00:00:00Z","finished_at":"2026-01-01T01:00:00Z","workspace":"` + wsDir + `"}`
	if err := os.WriteFile(filepath.Join(wsDir, "state.json"), []byte(stateJSON), 0644); err != nil {
		t.Fatalf("write state.json: %v", err)
	}

	// Write run_options.json
	optsJSON := `{"schema_version":"1","task_dir":"/tmp/my-task","task_name":"my-task","code_lang":"go","generate":false}`
	runOptsPath := filepath.Join(wsDir, "run_options.json")
	if err := os.WriteFile(runOptsPath, []byte(optsJSON), 0644); err != nil {
		t.Fatalf("write run_options.json: %v", err)
	}

	// Sync
	rootDir := filepath.Dir(wsDir) // the harbor-factory dir
	err := s.SyncFromFilesystem(context.Background(), []string{rootDir})
	if err != nil {
		t.Fatalf("SyncFromFilesystem: %v", err)
	}

	runs, err := s.ListRuns(SortByStartedAt, false, "")
	if err != nil {
		t.Fatalf("ListRuns after sync: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 run after sync, got %d", len(runs))
	}
	if runs[0].Task.TaskName != "my-task" {
		t.Errorf("expected task name 'my-task', got %q", runs[0].Task.TaskName)
	}
	if runs[0].Run.Status != "succeeded" {
		t.Errorf("expected status 'succeeded', got %q", runs[0].Run.Status)
	}
}

func TestSyncFromNestedWorkspacesDirectory(t *testing.T) {
	s := tempDB(t)
	root := t.TempDir()
	workspace := filepath.Join(root, "workspaces", "run-1")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	stateJSON := `{"run_id":"nested-run","status":"running","workspace":"` + workspace + `"}`
	if err := os.WriteFile(filepath.Join(workspace, "state.json"), []byte(stateJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "run_options.json"), []byte(`{"task_dir":"/tmp/nested-task","task_name":"nested"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncFromFilesystem(context.Background(), []string{root}); err != nil {
		t.Fatal(err)
	}
	runs, err := s.ListRuns(SortByStartedAt, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Run.WorkspacePath != workspace {
		t.Fatalf("nested workspace was not indexed: %+v", runs)
	}
}

func TestRefreshRunningUpdatesStatusWithoutDroppingSize(t *testing.T) {
	s := tempDB(t)
	root := t.TempDir()
	workspace := filepath.Join(root, "workspaces", "run-1")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	writeState := func(status string, finished bool) {
		summary := `{"run_id":"refresh-run","status":"` + status + `","workspace":"` + workspace + `"`
		if finished {
			summary += `,"finished_at":"2026-01-01T01:00:00Z"`
		}
		summary += `}`
		if err := os.WriteFile(filepath.Join(workspace, "state.json"), []byte(summary), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeState("running", false)
	if err := os.WriteFile(filepath.Join(workspace, "run_options.json"), []byte(`{"task_dir":"/tmp/refresh-task","task_name":"refresh"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "payload"), make([]byte, 128), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncFromFilesystem(context.Background(), []string{root}); err != nil {
		t.Fatal(err)
	}
	before, err := s.GetRunByWorkspace(workspace)
	if err != nil || before == nil || before.SizeBytes == 0 {
		t.Fatalf("initial run not indexed with size: %+v err=%v", before, err)
	}
	writeState("succeeded", true)
	if err := s.RefreshRunning(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := s.GetRunByWorkspace(workspace)
	if err != nil || after.Status != "succeeded" || after.SizeBytes != before.SizeBytes || after.IsResumable {
		t.Fatalf("running refresh failed: before=%+v after=%+v err=%v", before, after, err)
	}
}

func TestResetDatabaseKeepsWorkspaceFiles(t *testing.T) {
	root := t.TempDir()
	workspaceFile := filepath.Join(root, "workspaces", "run-1", "state.json")
	if err := os.MkdirAll(filepath.Dir(workspaceFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workspaceFile, []byte(`{"run_id":"run-1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ResetDatabase(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(workspaceFile); err != nil {
		t.Fatalf("reset removed workspace data: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, dbFileName)); !os.IsNotExist(err) {
		t.Fatalf("database still exists after reset: %v", err)
	}
}

func TestDeleteRunByWorkspace(t *testing.T) {
	s := tempDB(t)
	tid, _ := s.UpsertTask(Task{TaskDir: "/tmp/d", TaskName: "d"})
	s.UpsertRun(Run{TaskID: tid, WorkspacePath: "/tmp/ws-d", RunID: "rd"})

	err := s.DeleteRunByWorkspace("/tmp/ws-d")
	if err != nil {
		t.Fatalf("DeleteRunByWorkspace: %v", err)
	}
	got, _ := s.GetRunByWorkspace("/tmp/ws-d")
	if got != nil {
		t.Error("expected nil after delete")
	}
}

func TestCleanOrphanTasks(t *testing.T) {
	s := tempDB(t)
	tid, _ := s.UpsertTask(Task{TaskDir: "/tmp/orphan", TaskName: "orphan"})
	s.UpsertRun(Run{TaskID: tid, WorkspacePath: "/tmp/ws-o", RunID: "ro"})
	s.DeleteRunByWorkspace("/tmp/ws-o")

	err := s.CleanOrphanTasks()
	if err != nil {
		t.Fatalf("CleanOrphanTasks: %v", err)
	}
	got, _ := s.GetTaskByDir("/tmp/orphan")
	if got != nil {
		t.Error("expected orphan task to be cleaned")
	}
}
