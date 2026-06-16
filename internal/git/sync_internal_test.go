package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xuanli520/p2r_tui/internal/config"
)

func TestRecoverCleanPermissionFailureRetriesAfterChmod(t *testing.T) {
	requireGitForInternalTest(t)
	taskID := "TASK-20260616-CHMOD"
	batchID := "batch-1"
	remotePath, _ := createInternalRemoteRepo(t, taskID, "v1")
	basePath := filepath.Join(t.TempDir(), "projects-qa")
	syncer := NewSyncer(basePath, config.GitConfig{CloneTimeout: 10 * time.Second})

	first, err := syncer.Sync(context.Background(), taskID, batchID, remotePath, nil)
	if err != nil {
		t.Fatal(err)
	}
	localPath := filepath.Join(first.RepoPath, "local.tmp")
	if err := os.WriteFile(localPath, []byte("local-only"), 0o644); err != nil {
		t.Fatal(err)
	}

	var events []SyncProgress
	recloned, result, err := syncer.recoverCleanFailure(
		context.Background(),
		filepath.Join(basePath, batchID, taskID),
		first.ClonePath,
		first.RepoPath,
		filepath.Join(basePath, batchID, taskID, cloneDoneMarker),
		remotePath,
		cleanPermissionError(first.ClonePath, "failed to remove 'local.tmp': Permission denied"),
		func(progress SyncProgress) { events = append(events, progress) },
	)
	if err != nil {
		t.Fatal(err)
	}
	if recloned || result != nil {
		t.Fatalf("chmod retry should not fresh clone: recloned=%v result=%#v", recloned, result)
	}
	if _, err := os.Stat(localPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("local file should be removed by retry clean, stat err=%v", err)
	}
	if !internalHasPhase(events, "clean") {
		t.Fatalf("expected clean recovery progress, got %#v", events)
	}
}

func TestRecoverCleanPermissionFailureQuarantinesAndFreshClones(t *testing.T) {
	requireGitForInternalTest(t)
	taskID := "TASK-20260616-RECLON"
	batchID := "batch-1"
	remotePath, workPath := createInternalRemoteRepo(t, taskID, "v1")
	basePath := filepath.Join(t.TempDir(), "projects-qa")
	syncer := NewSyncer(basePath, config.GitConfig{CloneTimeout: 10 * time.Second})

	first, err := syncer.Sync(context.Background(), taskID, batchID, remotePath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first.RepoPath, "local.tmp"), []byte("local-only"), 0o644); err != nil {
		t.Fatal(err)
	}
	updateInternalRemoteRepo(t, workPath, taskID, "v2")

	oldChmod := chmodUserWritableTreeFunc
	chmodUserWritableTreeFunc = func(string) error { return errors.New("chmod denied") }
	t.Cleanup(func() { chmodUserWritableTreeFunc = oldChmod })

	taskPath := filepath.Join(basePath, batchID, taskID)
	resultRecloned, result, err := syncer.recoverCleanFailure(
		context.Background(),
		taskPath,
		first.ClonePath,
		first.RepoPath,
		filepath.Join(taskPath, cloneDoneMarker),
		remotePath,
		cleanPermissionError(first.ClonePath, "failed to remove 'local.tmp': Permission denied"),
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !resultRecloned || result == nil || result.Operation != "reclone" {
		t.Fatalf("expected fresh clone recovery, recloned=%v result=%#v", resultRecloned, result)
	}
	assertInternalFileContains(t, filepath.Join(result.RepoPath, "metadata.json"), "v2")
	if _, err := os.Stat(filepath.Join(result.RepoPath, "local.tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("local contamination should not exist after fresh clone, stat err=%v", err)
	}
	entries, err := os.ReadDir(filepath.Join(basePath, ".qa-control", "git-sync-quarantine"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), taskID+"-") {
		t.Fatalf("unexpected quarantine entries: %#v", entries)
	}
}

func TestRecoverCleanPermissionFailureReportsManualCleanup(t *testing.T) {
	basePath := filepath.Join(t.TempDir(), "projects-qa")
	syncer := NewSyncer(basePath, config.GitConfig{})
	taskID := "TASK-20260616-MANUAL"
	taskPath := filepath.Join(basePath, "batch-1", taskID)
	clonePath := filepath.Join(taskPath, taskID)
	if err := os.MkdirAll(clonePath, 0o755); err != nil {
		t.Fatal(err)
	}

	oldChmod := chmodUserWritableTreeFunc
	oldRename := renamePathFunc
	chmodUserWritableTreeFunc = func(string) error { return errors.New("chmod denied") }
	renamePathFunc = func(string, string) error { return errors.New("rename denied") }
	t.Cleanup(func() {
		chmodUserWritableTreeFunc = oldChmod
		renamePathFunc = oldRename
	})

	_, _, err := syncer.recoverCleanFailure(
		context.Background(),
		taskPath,
		clonePath,
		clonePath,
		filepath.Join(taskPath, cloneDoneMarker),
		"https://gitlab.example/repo.git",
		cleanPermissionError(clonePath, "failed to remove 'storage/logs/app.log': Permission denied"),
		nil,
	)
	if err == nil {
		t.Fatal("expected manual cleanup error")
	}
	message := err.Error()
	for _, want := range []string{"Permission denied", taskPath, "manual cleanup", "rename denied"} {
		if !strings.Contains(message, want) {
			t.Fatalf("error %q missing %q", message, want)
		}
	}
}

func cleanPermissionError(dir, stderr string) error {
	return &CommandError{
		Dir:    dir,
		Args:   []string{"clean", "-fdx"},
		Stderr: stderr,
		Err:    errors.New("exit status 1"),
	}
}

func requireGitForInternalTest(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available in PATH")
	}
}

func createInternalRemoteRepo(t *testing.T, taskID, version string) (string, string) {
	t.Helper()
	root := t.TempDir()
	workPath := filepath.Join(root, "work")
	remotePath := filepath.Join(root, "remote.git")
	if err := os.MkdirAll(workPath, 0o755); err != nil {
		t.Fatal(err)
	}
	runInternalGit(t, workPath, "init")
	runInternalGit(t, workPath, "config", "user.name", "P2R Test")
	runInternalGit(t, workPath, "config", "user.email", "p2r-test@example.com")
	runInternalGit(t, workPath, "checkout", "-b", "main")
	writeInternalPackageVersion(t, workPath, taskID, version)
	runInternalGit(t, workPath, "add", ".")
	runInternalGit(t, workPath, "commit", "-m", "initial package")
	runInternalGit(t, root, "init", "--bare", remotePath)
	runInternalGit(t, remotePath, "symbolic-ref", "HEAD", "refs/heads/main")
	runInternalGit(t, workPath, "remote", "add", "origin", remotePath)
	runInternalGit(t, workPath, "push", "-u", "origin", "main")
	return remotePath, workPath
}

func updateInternalRemoteRepo(t *testing.T, workPath, taskID, version string) {
	t.Helper()
	writeInternalPackageVersion(t, workPath, taskID, version)
	runInternalGit(t, workPath, "add", ".")
	runInternalGit(t, workPath, "commit", "-m", "update package "+version)
	runInternalGit(t, workPath, "push", "origin", "main")
}

func writeInternalPackageVersion(t *testing.T, workPath, taskID, version string) {
	t.Helper()
	for _, path := range []string{
		filepath.Join("docs", "readme.txt"),
		filepath.Join("repo", "app.txt"),
		filepath.Join("docs", "original-session", "session.txt"),
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(workPath, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(workPath, path), []byte(version+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	content := []byte(`{"task_id":"` + taskID + `","prompt":"build it","version":"` + version + `"}` + "\n")
	if err := os.WriteFile(filepath.Join(workPath, "metadata.json"), content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func runInternalGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed in %s: %v\n%s", strings.Join(args, " "), dir, err, string(output))
	}
}

func assertInternalFileContains(t *testing.T, path, want string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), want) {
		t.Fatalf("%s = %q, want substring %q", path, string(content), want)
	}
}

func internalHasPhase(events []SyncProgress, phase string) bool {
	for _, event := range events {
		if event.Phase == phase {
			return true
		}
	}
	return false
}
