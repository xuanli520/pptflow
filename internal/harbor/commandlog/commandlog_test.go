package commandlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedactTextRemovesSecrets(t *testing.T) {
	apiStyleKey := "sk-" + "secretvalue"
	input := strings.Join([]string{
		"harbor --ae ANTHROPIC_AUTH_TOKEN=secret",
		"Bearer abc.def",
		apiStyleKey,
		"https://token@github.com/org/repo",
		"ssh://git:secret@github.com/org/repo",
		`{"OPENAI_API_KEY":"json-secret"}`,
		"GITHUB_TOKEN: ghp_unitsecretvalue",
		"github_pat_unitsecretvalue123456",
		"AKIAUNITSECRET123456",
	}, " ")
	got := RedactText(input)
	for _, secret := range []string{"secret", "abc.def", apiStyleKey, "token@github", "git:secret@github", "json-secret", "ghp_unitsecretvalue", "github_pat_unitsecretvalue", "AKIAUNITSECRET"} {
		if strings.Contains(got, secret) {
			t.Fatalf("secret %q leaked in %q", secret, got)
		}
	}
	if !strings.Contains(got, "ANTHROPIC_AUTH_TOKEN=<redacted>") {
		t.Fatalf("missing env redaction: %s", got)
	}
}

func TestWriteOutputFiles(t *testing.T) {
	dir := t.TempDir()
	stdout, stderr, err := WriteOutputFiles(dir, "out", "err")
	if err != nil {
		t.Fatal(err)
	}
	if stdout != filepath.Join(dir, "stdout.txt") || stderr != filepath.Join(dir, "stderr.txt") {
		t.Fatalf("unexpected output paths: %s %s", stdout, stderr)
	}
	data, err := os.ReadFile(stdout)
	if err != nil || string(data) != "out" {
		t.Fatalf("stdout file = %q err=%v", data, err)
	}
}

func TestWriteOutputFilesRedactsSecretsDefensively(t *testing.T) {
	dir := t.TempDir()
	stdoutPath, stderrPath, err := WriteOutputFiles(dir, "OPENAI_API_KEY=raw-api-value", "Bearer raw-token-value")
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := os.ReadFile(stdoutPath)
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.ReadFile(stderrPath)
	if err != nil {
		t.Fatal(err)
	}
	combined := string(stdout) + "\n" + string(stderr)
	for _, secret := range []string{"raw-api-value", "raw-token-value"} {
		if strings.Contains(combined, secret) {
			t.Fatalf("secret %q leaked in command output files: %s", secret, combined)
		}
	}
	if !strings.Contains(combined, "<redacted>") {
		t.Fatalf("redaction marker missing: %s", combined)
	}
}

func TestRedactArgvPreservesNonSecretAssignments(t *testing.T) {
	got := RedactArgv([]string{
		"docker",
		"compose",
		"--project-directory=/tmp/task",
		"FOO=bar",
		"https://user:pass@example.com/repo",
	})
	joined := strings.Join(got, " ")
	for _, want := range []string{"--project-directory=/tmp/task", "FOO=bar", "https://<redacted>@example.com/repo"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("argv = %q, want %q", joined, want)
		}
	}
}

func TestRedactArgvRemovesSecrets(t *testing.T) {
	apiStyleKey := "sk-" + "secretvalue"
	got := RedactArgv([]string{
		"harbor",
		"run",
		"--api-key=raw-api-value",
		"--client-secret",
		"raw-client-value",
		"--ae",
		"ANTHROPIC_MODEL=qwen3.7-max",
		"--ae",
		"ANTHROPIC_AUTH_TOKEN=token",
		"--ae=ANTHROPIC_AUTH_TOKEN=token",
		"Bearer abc.def",
		apiStyleKey,
	})
	joined := strings.Join(got, " ")
	for _, secret := range []string{"raw-api-value", "raw-client-value", "token", "abc.def", apiStyleKey} {
		if strings.Contains(joined, secret) {
			t.Fatalf("secret %q leaked in %q", secret, joined)
		}
	}
	for _, want := range []string{"--api-key=<redacted>", "--client-secret <redacted>", "ANTHROPIC_MODEL=qwen3.7-max", "ANTHROPIC_AUTH_TOKEN=<redacted>", "--ae=ANTHROPIC_AUTH_TOKEN=<redacted>"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("argv = %q, want %q", joined, want)
		}
	}
}

func TestClassifyFailure(t *testing.T) {
	if got := ClassifyFailure(-1, false, "", "executable file not found"); got != "missing_tool_or_path" {
		t.Fatalf("classification = %s", got)
	}
	if got := ClassifyFailure(1, true, "", ""); got != "timeout" {
		t.Fatalf("classification = %s", got)
	}
}
