package repoprep

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
)

func TestPrepareClonesCommitAndWritesMetadata(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init")
	run(t, repo, "git", "config", "user.email", "test@example.com")
	run(t, repo, "git", "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, repo, "git", "add", "README.md")
	run(t, repo, "git", "commit", "-m", "initial")
	commit := output(t, repo, "git", "rev-parse", "HEAD")

	workspace := t.TempDir()
	prepared, err := Prepare(context.Background(), Options{RepoURL: repo, Commit: commit, Workspace: workspace, AllowLocal: true})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.ResolvedCommit != commit {
		t.Fatalf("resolved commit = %s, want %s", prepared.ResolvedCommit, commit)
	}
	if prepared.TreeHash == "" {
		t.Fatal("tree hash is empty")
	}
	if _, err := os.Stat(filepath.Join(workspace, "phase0", "repo_prepared.json")); err != nil {
		t.Fatal(err)
	}
	if len(prepared.CommandLogs) < 4 {
		t.Fatalf("expected command logs for clone/checkout/rev-parse, got %+v", prepared.CommandLogs)
	}
	commandLogPath := filepath.Join(workspace, "phase0", "command_logs", "repo_prepare.json")
	raw, err := os.ReadFile(commandLogPath)
	if err != nil {
		t.Fatal(err)
	}
	var commandLogs []domain.CommandRun
	if err := json.Unmarshal(raw, &commandLogs); err != nil {
		t.Fatal(err)
	}
	if len(commandLogs) != len(prepared.CommandLogs) {
		t.Fatalf("command log file len = %d, prepared len = %d", len(commandLogs), len(prepared.CommandLogs))
	}
	if !strings.Contains(string(raw), `"cwd"`) {
		t.Fatalf("command log should use cwd field: %s", raw)
	}
	for _, run := range commandLogs {
		if run.Dir == "" || len(run.Argv) == 0 || len(run.Env) == 0 || run.Attempt != 1 || run.DurationMS < 0 {
			t.Fatalf("missing command audit fields: %+v", run)
		}
		if run.StdoutPath == "" || run.StderrPath == "" {
			t.Fatalf("missing command output paths: %+v", run)
		}
		if _, err := os.Stat(run.StdoutPath); err != nil {
			t.Fatalf("missing stdout artifact %s: %v", run.StdoutPath, err)
		}
	}
	if _, err := os.Stat(filepath.Join(prepared.SourcePath, "README.md")); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareWritesPartialCommandLogOnFailure(t *testing.T) {
	workspace := t.TempDir()
	_, err := Prepare(context.Background(), Options{
		RepoURL:    filepath.Join(t.TempDir(), "missing-repo"),
		Commit:     "deadbeef",
		Workspace:  workspace,
		AllowLocal: true,
	})
	if err == nil {
		t.Fatal("expected error")
	}
	raw, readErr := os.ReadFile(filepath.Join(workspace, "phase0", "command_logs", "repo_prepare.json"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	var commandLogs []domain.CommandRun
	if err := json.Unmarshal(raw, &commandLogs); err != nil {
		t.Fatal(err)
	}
	if len(commandLogs) != 1 || commandLogs[0].Passed || commandLogs[0].ExitCode == 0 || commandLogs[0].FailureClass == "" {
		t.Fatalf("partial command log = %+v", commandLogs)
	}
}

func TestPrepareRejectsNonGitHubByDefault(t *testing.T) {
	_, err := Prepare(context.Background(), Options{
		RepoURL:   filepath.Join(t.TempDir(), "local-repo"),
		Commit:    "deadbeef",
		Workspace: t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "public GitHub") {
		t.Fatalf("expected non-GitHub rejection, got %v", err)
	}
}

func TestPrepareRejectsCredentialedRepoURL(t *testing.T) {
	workspace := t.TempDir()
	_, err := Prepare(context.Background(), Options{
		RepoURL:   "https://token@github.com/org/repo.git",
		Commit:    "deadbeef",
		Workspace: workspace,
	})
	if err == nil || !strings.Contains(err.Error(), "credentials") {
		t.Fatalf("expected credentialed repo URL failure, got %v", err)
	}
	if _, readErr := os.Stat(filepath.Join(workspace, "phase0", "repo_prepared.json")); !os.IsNotExist(readErr) {
		t.Fatalf("credentialed repo URL should not write repo metadata, stat err=%v", readErr)
	}
}

func TestPrepareNormalizesGitHubSCPToPublicHTTPS(t *testing.T) {
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "git-args.txt")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$@" >> %q
last=
for arg in "$@"; do
  last="$arg"
done
case "$*" in
  *"ls-remote"*)
    echo "abc123	refs/heads/main"
    exit 0
    ;;
  *"clone"*)
    /bin/mkdir -p "$last"
    exit 0
    ;;
  *"checkout"*)
    exit 0
    ;;
  *"HEAD^{tree}"*)
    echo "tree456"
    exit 0
    ;;
  *"rev-parse"*)
    echo "abc123"
    exit 0
    ;;
esac
exit 0
`, logPath)
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	prepared, err := Prepare(context.Background(), Options{
		RepoURL:   "git@github.com:org/repo.git",
		Commit:    "abc123",
		Workspace: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.RepoURL != "https://github.com/org/repo.git" {
		t.Fatalf("expected canonical public repo URL, got %q", prepared.RepoURL)
	}
	args, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(args)
	if !strings.Contains(text, "ls-remote") || !strings.Contains(text, "clone") || !strings.Contains(text, "https://github.com/org/repo.git") {
		t.Fatalf("expected public probe and clone against HTTPS URL, got %s", text)
	}
	if strings.Contains(text, "git@github.com") {
		t.Fatalf("git commands should not use SSH repo URL: %s", text)
	}
}

func TestGitPublicProbeUsesCredentiallessEnv(t *testing.T) {
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "git-args.txt")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$@" > %q
echo "abc123	refs/heads/main"
`, logPath)
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	t.Setenv("GITHUB_TOKEN", "raw-token-value")
	_, run, err := runGitPublicProbe(context.Background(), "https://github.com/org/repo.git")
	if err != nil {
		t.Fatal(err)
	}
	if run.Name != "git_public_probe" || !run.Passed {
		t.Fatalf("unexpected public probe run: %+v", run)
	}
	args, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"credential.helper=", "ls-remote", "https://github.com/org/repo.git", "HEAD"} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("public probe args missing %q: %s", want, args)
		}
	}
	envText := strings.Join(run.Env, "\n")
	if strings.Contains(envText, "GITHUB_TOKEN") || strings.Contains(envText, "raw-token-value") {
		t.Fatalf("public probe env should not include GitHub token: %s", envText)
	}
	if !strings.Contains(envText, "GIT_TERMINAL_PROMPT=<set>") || !strings.Contains(envText, "GIT_CONFIG_GLOBAL=<set>") {
		t.Fatalf("public probe env missing noninteractive config: %s", envText)
	}
}

func TestGitCommandRetryRecoversTransientTLSFailure(t *testing.T) {
	binDir := t.TempDir()
	counter := filepath.Join(t.TempDir(), "attempts")
	script := fmt.Sprintf(`#!/bin/sh
count=0
if [ -f %q ]; then count=$(/bin/cat %q); fi
count=$((count + 1))
printf '%%s' "$count" > %q
if [ "$count" -lt 3 ]; then
  echo 'fatal: GnuTLS recv error (-110): The TLS connection was non-properly terminated.' >&2
  exit 128
fi
echo 'abc123 refs/heads/main'
`, counter, counter, counter)
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	_, runs, err := runGitCommandWithRetry(context.Background(), "git_public_probe", publicGitEnv(), "", 3, time.Millisecond, nil, "ls-remote", "https://github.com/org/repo.git", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 3 || runs[0].Attempt != 1 || runs[2].Attempt != 3 || !runs[2].Passed {
		t.Fatalf("unexpected retry audit: %+v", runs)
	}
}

func run(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, out)
	}
}

func output(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %v: %v", name, args, err)
	}
	return string(bytesTrimSpace(out))
}

func bytesTrimSpace(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ' || b[len(b)-1] == '\t') {
		b = b[:len(b)-1]
	}
	for len(b) > 0 && (b[0] == '\n' || b[0] == '\r' || b[0] == ' ' || b[0] == '\t') {
		b = b[1:]
	}
	return b
}
