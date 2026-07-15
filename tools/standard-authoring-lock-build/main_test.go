package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSourceBuildIdentityExcludesSelfReferentialLock(t *testing.T) {
	git := testGitExecutable(t)
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "tracked.txt"), "stable\n")
	writeTestFile(t, filepath.Join(root, standardLockRelative), "first lock\n")
	testGit(t, root, git, "init")
	testGit(t, root, git, "config", "user.email", "lock-test@example.invalid")
	testGit(t, root, git, "config", "user.name", "Lock Test")
	testGit(t, root, git, "add", ".")
	testGit(t, root, git, "commit", "-m", "first")
	_, first, err := sourceBuildIdentity(root, git, standardLockRelative)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, standardLockRelative), "second lock\n")
	testGit(t, root, git, "add", standardLockRelative)
	testGit(t, root, git, "commit", "-m", "lock only")
	_, second, err := sourceBuildIdentity(root, git, standardLockRelative)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("source manifest changed only because the excluded lock changed: %s != %s", first, second)
	}
}

func TestReadRegularFileRejectsSymlinkAndDetectsPathShape(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	writeTestFile(t, target, "content")
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("create symlink: %v", err)
	}
	if _, err := readRegularFile(link, maxAssetBytes); err == nil {
		t.Fatal("symlink asset was accepted")
	}
	if _, err := contractAssetPath(root, "../escape"); err == nil {
		t.Fatal("asset traversal was accepted")
	}
}

func TestRequireCleanGitWorktreeRejectsUntrackedContent(t *testing.T) {
	git := testGitExecutable(t)
	root := t.TempDir()
	testGit(t, root, git, "init")
	testGit(t, root, git, "config", "user.email", "lock-test@example.invalid")
	testGit(t, root, git, "config", "user.name", "Lock Test")
	writeTestFile(t, filepath.Join(root, "tracked.txt"), "tracked\n")
	testGit(t, root, git, "add", ".")
	testGit(t, root, git, "commit", "-m", "initial")
	if err := requireCleanGitWorktree(root, git); err != nil {
		t.Fatalf("clean worktree rejected: %v", err)
	}
	writeTestFile(t, filepath.Join(root, "untracked.txt"), "untracked\n")
	if err := requireCleanGitWorktree(root, git); err == nil {
		t.Fatal("dirty worktree accepted")
	}
}

func testGitExecutable(t *testing.T) string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is unavailable")
	}
	abs, err := filepath.Abs(git)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func testGit(t *testing.T, root, git string, arguments ...string) {
	t.Helper()
	command := exec.Command(git, arguments...)
	command.Dir = root
	command.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "GIT_AUTHOR_NAME=Lock Test", "GIT_AUTHOR_EMAIL=lock-test@example.invalid", "GIT_COMMITTER_NAME=Lock Test", "GIT_COMMITTER_EMAIL=lock-test@example.invalid"}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(arguments, " "), err, output)
	}
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
