package codexruntime

import (
	"context"
	"io"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/agent"
	"github.com/purplevoid/harbor-factory/internal/codex/appserver"
	"github.com/purplevoid/harbor-factory/internal/executor"
)

type fakeConversationSession struct {
	turns       []appserver.TurnRequest
	guidance    []string
	closed      bool
	closeCalls  int
	turnResult  appserver.Result
	turnErr     error
	guidanceErr error
	onTurn      func(context.Context, appserver.TurnRequest)
}

func (s *fakeConversationSession) Start(context.Context, appserver.Request) error { return nil }

func (s *fakeConversationSession) Turn(ctx context.Context, request appserver.TurnRequest) (appserver.Result, error) {
	s.turns = append(s.turns, request)
	if s.onTurn != nil {
		s.onTurn(ctx, request)
	}
	if s.turnResult.Warnings == nil {
		s.turnResult.Warnings = []appserver.Warning{{Error: "warning"}}
	}
	return s.turnResult, s.turnErr
}

func (s *fakeConversationSession) SendGuidance(_ context.Context, message string) error {
	s.guidance = append(s.guidance, message)
	return s.guidanceErr
}

func (s *fakeConversationSession) Close() error {
	s.closed = true
	s.closeCalls++
	return nil
}

func TestConversationAppliesDefaultsAcrossTurnsAndClosesSession(t *testing.T) {
	session := &fakeConversationSession{}
	conversation := &conversation{
		session: session,
		model:   "test-model",
		defaults: agent.ConversationRequest{
			TimeoutSeconds: 17,
			MaxOutputBytes: 2048,
			LogPath:        "/tmp/default.log",
		},
	}
	result, err := conversation.Turn(context.Background(), agent.TurnRequest{Prompt: "first"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Model != "test-model" || len(result.Warnings) != 1 {
		t.Fatalf("unexpected turn result: %+v", result)
	}
	if len(session.turns) != 1 || session.turns[0].Timeout != 17*time.Second || session.turns[0].MaxOutputBytes != 2048 || session.turns[0].LogPath != "/tmp/default.log" {
		t.Fatalf("conversation defaults not applied: %+v", session.turns)
	}
	if _, err := conversation.Turn(context.Background(), agent.TurnRequest{Prompt: "second", TimeoutSeconds: 3, MaxOutputBytes: 512, LogPath: "/tmp/second.log"}); err != nil {
		t.Fatal(err)
	}
	if len(session.turns) != 2 || session.turns[1].Timeout != 3*time.Second || session.turns[1].MaxOutputBytes != 512 || session.turns[1].LogPath != "/tmp/second.log" {
		t.Fatalf("second turn settings were not forwarded: %+v", session.turns)
	}
	if err := conversation.Close(); err != nil {
		t.Fatal(err)
	}
	if err := conversation.Close(); err != nil {
		t.Fatal(err)
	}
	if !session.closed || session.closeCalls != 1 {
		t.Fatalf("app-server close lifecycle = closed=%t calls=%d, want true/1", session.closed, session.closeCalls)
	}
}

func TestConversationForwardsStreamingUpdatesAndGuidance(t *testing.T) {
	session := &fakeConversationSession{
		turnResult: appserver.Result{Result: executor.Result{Stdout: "final"}},
		onTurn: func(_ context.Context, request appserver.TurnRequest) {
			if request.OnDelta == nil {
				t.Fatal("streaming turn did not install an update callback")
			}
			request.OnDelta(appserver.Update{TurnID: "turn-1", ItemID: "item-1", Delta: "partial", Text: "partial", Truncated: true})
			request.OnDelta(appserver.Update{TurnID: "turn-1", ItemID: "item-1", Text: "complete", Done: true, Truncated: true})
		},
	}
	conversation := &conversation{session: session, model: "test-model", defaults: agent.ConversationRequest{TimeoutSeconds: 11, MaxOutputBytes: 4096, LogPath: "/tmp/stream.log"}}
	var updates []agent.TurnUpdate
	result, err := conversation.TurnStream(context.Background(), agent.TurnRequest{Prompt: "stream", TimeoutSeconds: 2}, func(update agent.TurnUpdate) {
		updates = append(updates, update)
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "final" || result.Model != "test-model" {
		t.Fatalf("streamed turn result = %+v", result)
	}
	if len(updates) != 2 {
		t.Fatalf("streaming updates = %+v, want two", updates)
	}
	if got := updates[0]; got.TurnID != "turn-1" || got.ItemID != "item-1" || got.Delta != "partial" || got.Text != "partial" || !got.Truncated || got.Done {
		t.Fatalf("first update = %+v", got)
	}
	if got := updates[1]; got.TurnID != "turn-1" || got.ItemID != "item-1" || got.Text != "complete" || !got.Done || !got.Truncated {
		t.Fatalf("final update = %+v", got)
	}
	if len(session.turns) != 1 || session.turns[0].Timeout != 2*time.Second {
		t.Fatalf("streaming timeout was not forwarded: %+v", session.turns)
	}
	if err := conversation.Steer(context.Background(), "focus on tests"); err != nil {
		t.Fatal(err)
	}
	if len(session.guidance) != 1 || session.guidance[0] != "focus on tests" {
		t.Fatalf("guidance forwarding = %#v", session.guidance)
	}
}

func TestOpenConversationRejectsAmbientConfigurationAndDiscovery(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	runner := &probeRunner{}
	request := agent.ConversationRequest{ProjectPath: t.TempDir(), Model: "explicit-model"}

	_, err := New(runner, "", nil).OpenConversation(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "explicit CODEX_HOME") {
		t.Fatalf("ambient CODEX_HOME error = %v, want explicit configuration failure", err)
	}
	if runner.lookPathCalls != 0 || len(runner.runs) != 0 {
		t.Fatalf("runtime unexpectedly probed a command before explicit configuration: lookPath=%d runs=%v", runner.lookPathCalls, runner.runs)
	}

	_, err = New(runner, "", map[string]string{"CODEX_HOME": home}).OpenConversation(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "executable path is required") {
		t.Fatalf("empty explicit command path error = %v", err)
	}
	if runner.lookPathCalls != 0 {
		t.Fatalf("controlled runtime must not discover Codex on PATH: LookPath calls=%d", runner.lookPathCalls)
	}
}

func TestOpenConversationRequiresExplicitModel(t *testing.T) {
	commandPath := writeFakeCodexProgram(t)
	runner := &probeRunner{}
	_, err := New(runner, commandPath, map[string]string{"CODEX_HOME": t.TempDir()}).OpenConversation(context.Background(), agent.ConversationRequest{
		ProjectPath: t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "explicit model") {
		t.Fatalf("missing model error = %v", err)
	}
	if runner.lookPathCalls != 0 {
		t.Fatalf("controlled runtime must not call LookPath: %d", runner.lookPathCalls)
	}
}

func TestConfiguredCodexHomeUsesCanonicalEnvironmentName(t *testing.T) {
	if _, ok := configuredEnvValue(map[string]string{"codex_home": "/not-used"}, "CODEX_HOME"); ok {
		t.Fatal("CODEX_HOME lookup must not accept a differently cased variable name")
	}
}

func TestOpenConversationUsesOnlyExplicitRuntimeInputs(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("fake app-server program uses a POSIX shell")
	}
	commandPath := writeFakeCodexProgram(t)
	configuredHome := t.TempDir()
	ambientHome := t.TempDir()
	envLog := filepath.Join(t.TempDir(), "received-env")
	t.Setenv("CODEX_HOME", ambientHome)
	t.Setenv("OPENAI_API_KEY", "ambient-secret-must-not-leak")
	runner := &probeRunner{}
	runtime := New(runner, commandPath, map[string]string{
		"CODEX_HOME":         configuredHome,
		"FAKE_CODEX_ENV_LOG": envLog,
	})
	conversation, err := runtime.OpenConversation(context.Background(), agent.ConversationRequest{
		ProjectPath:    t.TempDir(),
		Model:          "explicit-model",
		TimeoutSeconds: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conversation.Close() })
	result, err := conversation.Turn(context.Background(), agent.TurnRequest{Prompt: "hello", TimeoutSeconds: 5})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "answer" || result.Model != "explicit-model" {
		t.Fatalf("unexpected controlled turn result: %+v", result)
	}
	if err := conversation.Close(); err != nil {
		t.Fatal(err)
	}
	received, err := os.ReadFile(envLog)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(received), configuredHome+"|"; got != want {
		t.Fatalf("Codex process inherited ambient configuration: got %q want %q", got, want)
	}
	if runner.lookPathCalls != 0 {
		t.Fatalf("controlled runtime must not call LookPath: %d", runner.lookPathCalls)
	}
}

type probeRunner struct {
	lookPathCalls int
	runs          [][]string
}

func (r *probeRunner) LookPath(string) (string, error) {
	r.lookPathCalls++
	return "", os.ErrNotExist
}

func (r *probeRunner) Run(_ context.Context, _ time.Duration, _ string, _ []string, _ string, args ...string) executor.Result {
	r.runs = append(r.runs, append([]string(nil), args...))
	switch strings.Join(args, " ") {
	case "--version":
		return executor.Result{Stdout: "codex 99.0.0\n"}
	case "app-server --help":
		return executor.Result{Stdout: "--listen\n--config\n"}
	default:
		return executor.Result{}
	}
}

func (r *probeRunner) RunStreamingWithOutput(context.Context, time.Duration, string, []string, io.Writer, executor.OutputCallback, string, ...string) executor.Result {
	return executor.Result{}
}

func writeFakeCodexProgram(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-codex")
	program := `#!/bin/sh
printf '%s|%s' "$CODEX_HOME" "${OPENAI_API_KEY-}" > "$FAKE_CODEX_ENV_LOG"
while IFS= read -r line; do
  id=$(printf '%s\n' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
      ;;
    *'"method":"thread/start"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"thread-1"}}}\n' "$id"
      ;;
    *'"method":"turn/start"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"turn":{"id":"turn-1"}}}\n' "$id"
      printf '{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"thread-1","turnId":"turn-1","item":{"id":"item-1","type":"agentMessage","text":"answer"}}}\n'
      printf '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1","items":[],"status":"completed"}}}\n'
      ;;
  esac
done
`
	if err := os.WriteFile(path, []byte(program), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
