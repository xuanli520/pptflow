package appserver

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/executor"
)

func TestCloseReturnsAfterCompleteClosesOpenStreams(t *testing.T) {
	stdoutReader, stdoutWriter := io.Pipe()
	stderrReader, stderrWriter := io.Pipe()
	defer stdoutWriter.Close()
	defer stderrWriter.Close()

	session := &appServerCodexReviewSession{
		done:        make(chan struct{}),
		responses:   map[int]chan appServerRPCMessage{},
		deltas:      map[string]string{},
		deltaLogged: map[string]bool{},
		stdoutPipe:  stdoutReader,
		stderrPipe:  stderrReader,
	}
	session.wg.Add(2)
	go func() {
		defer session.wg.Done()
		session.readStdout(stdoutReader)
	}()
	go func() {
		defer session.wg.Done()
		session.readStderr(stderrReader)
	}()

	session.complete(executor.Result{Command: "fake app-server"}, nil)

	done := make(chan error, 1)
	go func() {
		done <- session.Close()
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Wait() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close() did not return after complete closed app-server streams")
	}
}

func TestSessionRunsMultipleTurnsOnOneEphemeralThread(t *testing.T) {
	tempDir := t.TempDir()
	requestLog := filepath.Join(tempDir, "requests.jsonl")
	commandPath := filepath.Join(tempDir, "fake-codex")
	script := `#!/bin/sh
turn=0
while IFS= read -r line; do
  printf '%s\n' "$line" >> "$FAKE_CODEX_REQUEST_LOG"
  id=$(printf '%s\n' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
      ;;
    *'"method":"thread/start"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"thread-1"}}}\n' "$id"
      ;;
    *'"method":"turn/start"'*)
      case "$line" in
        *'"threadId":"thread-1"'*) ;;
        *) exit 42 ;;
      esac
      turn=$((turn + 1))
      printf '{"jsonrpc":"2.0","id":%s,"result":{"turn":{"id":"turn-%s"}}}\n' "$id" "$turn"
      printf '{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"thread-1","turnId":"turn-%s","item":{"id":"item-%s","type":"agentMessage","text":"answer-%s"}}}\n' "$turn" "$turn" "$turn"
      printf '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-%s","items":[],"status":"completed"}}}\n' "$turn"
      ;;
  esac
done
`
	if err := os.WriteFile(commandPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	session := New(nil)
	if err := session.Start(context.Background(), Request{
		ProjectPath:       tempDir,
		LogPath:           filepath.Join(tempDir, "codex.log"),
		Env:               append(os.Environ(), "FAKE_CODEX_REQUEST_LOG="+requestLog),
		CommandPath:       commandPath,
		CapabilitySummary: "fake",
		HasAppServer:      true,
		SandboxMode:       "read-only",
		SandboxPolicy:     "readOnly",
		MaxOutputBytes:    1 << 20,
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })

	first, err := session.Turn(context.Background(), TurnRequest{Prompt: "first", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	second, err := session.Turn(context.Background(), TurnRequest{Prompt: "second", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if first.Result.Stdout != "answer-1" || second.Result.Stdout != "answer-2" {
		t.Fatalf("turn outputs leaked or were lost: first=%q second=%q", first.Result.Stdout, second.Result.Stdout)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(requestLog)
	if err != nil {
		t.Fatal(err)
	}
	requests := string(raw)
	if strings.Count(requests, `"method":"thread/start"`) != 1 {
		t.Fatalf("thread/start count mismatch:\n%s", requests)
	}
	if strings.Count(requests, `"method":"turn/start"`) != 2 {
		t.Fatalf("turn/start count mismatch:\n%s", requests)
	}
	if strings.Count(requests, `"threadId":"thread-1"`) != 2 {
		t.Fatalf("turns did not reuse thread-1:\n%s", requests)
	}
}
