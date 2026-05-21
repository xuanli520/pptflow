package git_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xuanli520/p2r_tui/internal/config"
	gitsync "github.com/xuanli520/p2r_tui/internal/git"
)

func TestSyncCloneCreatesExpectedPackagePathAndMarker(t *testing.T) {
	requireGit(t)
	taskID := "TASK-20260521-CLONE"
	batchID := "batch-1"
	remotePath, _ := createRemoteRepo(t, taskID, "v1")
	basePath := filepath.Join(t.TempDir(), "projects-qa")
	syncer := gitsync.NewSyncer(basePath, config.GitConfig{CloneTimeout: 10 * time.Second})

	var events []gitsync.SyncProgress
	result, err := syncer.Sync(context.Background(), taskID, batchID, remotePath, func(progress gitsync.SyncProgress) {
		events = append(events, progress)
	})
	if err != nil {
		t.Fatal(err)
	}

	wantRepoPath := filepath.Join(basePath, batchID, taskID, taskID)
	if result.Operation != "clone" || result.Commit == "" {
		t.Fatalf("unexpected clone result: %#v", result)
	}
	if result.RepoPath != wantRepoPath {
		t.Fatalf("repo path = %s, want %s", result.RepoPath, wantRepoPath)
	}
	assertFileContains(t, filepath.Join(result.RepoPath, "metadata.json"), "v1")
	assertFileContains(t, filepath.Join(basePath, batchID, taskID, ".qa-clone-done"), "commit="+result.Commit)
	if !hasPhase(events, "clone") {
		t.Fatalf("expected clone progress event, got %#v", events)
	}
}

func TestSyncForcePullResetsAndCleansExistingClone(t *testing.T) {
	requireGit(t)
	taskID := "TASK-20260521-PULL"
	batchID := "batch-1"
	remotePath, workPath := createRemoteRepo(t, taskID, "v1")
	basePath := filepath.Join(t.TempDir(), "projects-qa")
	syncer := gitsync.NewSyncer(basePath, config.GitConfig{CloneTimeout: 10 * time.Second})

	first, err := syncer.Sync(context.Background(), taskID, batchID, remotePath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Operation != "clone" {
		t.Fatalf("first operation = %q, want clone", first.Operation)
	}
	if err := os.WriteFile(filepath.Join(first.RepoPath, "local.tmp"), []byte("local-only"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first.RepoPath, "metadata.json"), []byte(`{"version":"local"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	updateRemoteRepo(t, workPath, taskID, "v2")

	var events []gitsync.SyncProgress
	second, err := syncer.Sync(context.Background(), taskID, batchID, remotePath, func(progress gitsync.SyncProgress) {
		events = append(events, progress)
	})
	if err != nil {
		t.Fatal(err)
	}

	if second.Operation != "force-pull" || second.Commit == first.Commit {
		t.Fatalf("unexpected force-pull result: first=%#v second=%#v", first, second)
	}
	assertFileContains(t, filepath.Join(second.RepoPath, "metadata.json"), "v2")
	if _, err := os.Stat(filepath.Join(second.RepoPath, "local.tmp")); !os.IsNotExist(err) {
		t.Fatalf("local untracked file should be cleaned, stat err=%v", err)
	}
	assertFileContains(t, filepath.Join(basePath, batchID, taskID, ".qa-clone-done"), "commit="+second.Commit)
	for _, phase := range []string{"fetch", "reset", "clean", "force-pull"} {
		if !hasPhase(events, phase) {
			t.Fatalf("missing progress phase %q in %#v", phase, events)
		}
	}
}

func TestSyncForcePullsExistingCloneWithoutMarker(t *testing.T) {
	requireGit(t)
	taskID := "TASK-20260521-NOMARK"
	batchID := "batch-1"
	remotePath, workPath := createRemoteRepo(t, taskID, "v1")
	basePath := filepath.Join(t.TempDir(), "projects-qa")
	syncer := gitsync.NewSyncer(basePath, config.GitConfig{CloneTimeout: 10 * time.Second})

	first, err := syncer.Sync(context.Background(), taskID, batchID, remotePath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(basePath, batchID, taskID, ".qa-clone-done")); err != nil {
		t.Fatal(err)
	}
	updateRemoteRepo(t, workPath, taskID, "v2")

	second, err := syncer.Sync(context.Background(), taskID, batchID, remotePath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.Operation != "force-pull" || second.Commit == first.Commit {
		t.Fatalf("unexpected sync result without marker: first=%#v second=%#v", first, second)
	}
	assertFileContains(t, filepath.Join(second.RepoPath, "metadata.json"), "v2")
	assertFileContains(t, filepath.Join(basePath, batchID, taskID, ".qa-clone-done"), "commit="+second.Commit)
}

func TestSyncRejectsPathTraversal(t *testing.T) {
	requireGit(t)
	taskID := "TASK-20260521-SAFE"
	remotePath, _ := createRemoteRepo(t, taskID, "v1")
	basePath := filepath.Join(t.TempDir(), "projects-qa")
	syncer := gitsync.NewSyncer(basePath, config.GitConfig{CloneTimeout: 10 * time.Second})

	if _, err := syncer.Sync(context.Background(), "../"+taskID, "batch-1", remotePath, nil); err == nil {
		t.Fatal("expected taskID traversal to be rejected")
	}
	if _, err := syncer.Sync(context.Background(), taskID, "../batch-1", remotePath, nil); err == nil {
		t.Fatal("expected batchID traversal to be rejected")
	}
	repoPath := syncer.RepoPath("../batch-1", "../"+taskID)
	rel, err := filepath.Rel(basePath, repoPath)
	if err != nil {
		t.Fatal(err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		t.Fatalf("RepoPath escaped base path: %s", repoPath)
	}
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available in PATH")
	}
}

func createRemoteRepo(t *testing.T, taskID, version string) (string, string) {
	t.Helper()
	root := t.TempDir()
	workPath := filepath.Join(root, "work")
	remotePath := filepath.Join(root, "remote.git")
	if err := os.MkdirAll(workPath, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, workPath, "init")
	runGit(t, workPath, "config", "user.name", "P2R Test")
	runGit(t, workPath, "config", "user.email", "p2r-test@example.com")
	runGit(t, workPath, "checkout", "-b", "main")
	writePackageVersion(t, workPath, taskID, version)
	runGit(t, workPath, "add", ".")
	runGit(t, workPath, "commit", "-m", "initial package")
	runGit(t, root, "init", "--bare", remotePath)
	runGit(t, remotePath, "symbolic-ref", "HEAD", "refs/heads/main")
	runGit(t, workPath, "remote", "add", "origin", remotePath)
	runGit(t, workPath, "push", "-u", "origin", "main")
	return remotePath, workPath
}

func updateRemoteRepo(t *testing.T, workPath, taskID, version string) {
	t.Helper()
	writePackageVersion(t, workPath, taskID, version)
	runGit(t, workPath, "add", ".")
	runGit(t, workPath, "commit", "-m", "update package "+version)
	runGit(t, workPath, "push", "origin", "main")
}

func writePackageVersion(t *testing.T, workPath, taskID, version string) {
	t.Helper()
	packagePath := filepath.Join(workPath, taskID)
	if err := os.MkdirAll(packagePath, 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte(`{"version":"` + version + `"}` + "\n")
	if err := os.WriteFile(filepath.Join(packagePath, "metadata.json"), content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed in %s: %v\n%s", strings.Join(args, " "), dir, err, string(output))
	}
}

func assertFileContains(t *testing.T, path, want string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), want) {
		t.Fatalf("%s = %q, want substring %q", path, string(content), want)
	}
}

func hasPhase(events []gitsync.SyncProgress, phase string) bool {
	for _, event := range events {
		if event.Phase == phase {
			return true
		}
	}
	return false
}
