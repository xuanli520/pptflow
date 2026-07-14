package codexruntime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/agent"
	"github.com/purplevoid/harbor-factory/internal/codex/appserver"
)

var _ agent.Runtime = Runtime{}

type fakeConversationSession struct {
	turns  []appserver.TurnRequest
	closed bool
}

func (s *fakeConversationSession) Start(context.Context, appserver.Request) error { return nil }

func (s *fakeConversationSession) Turn(_ context.Context, request appserver.TurnRequest) (appserver.Result, error) {
	s.turns = append(s.turns, request)
	return appserver.Result{Warnings: []appserver.Warning{{Error: "warning"}}}, nil
}

func (s *fakeConversationSession) SendGuidance(context.Context, string) error { return nil }

func (s *fakeConversationSession) Close() error {
	s.closed = true
	return nil
}

func TestConversationAppliesDefaultsAcrossTurnsAndCleansUp(t *testing.T) {
	session := &fakeConversationSession{}
	cleanup := filepath.Join(t.TempDir(), "codex-home")
	if err := os.MkdirAll(cleanup, 0o700); err != nil {
		t.Fatal(err)
	}
	conversation := &conversation{
		session: session,
		model:   "test-model",
		cleanup: cleanup,
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
	if err := conversation.Close(); err != nil {
		t.Fatal(err)
	}
	if !session.closed {
		t.Fatal("app-server session was not closed")
	}
	if _, err := os.Stat(cleanup); !os.IsNotExist(err) {
		t.Fatalf("temporary CODEX_HOME was not removed: %v", err)
	}
}

func TestWriteAutomationCodexConfigDefaultsToBuiltInProvider(t *testing.T) {
	t.Setenv("CODEX_MODEL_PROVIDER", "")
	t.Setenv("CODEX_MODEL_BASE_URL", "")
	t.Setenv("OPENAI_BASE_URL", "")
	sourceHome := t.TempDir()
	targetHome := t.TempDir()
	if err := writeAutomationCodexConfig(sourceHome, targetHome, "/tmp/project"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(targetHome, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	config := string(data)
	if !strings.Contains(config, `model_provider = "openai"`) {
		t.Fatalf("expected built-in openai provider fallback, got:\n%s", config)
	}
	if strings.Contains(config, "new-api.metalics.cn") {
		t.Fatalf("automation Codex config must not hardcode image endpoint:\n%s", config)
	}
}

func TestWriteAutomationCodexConfigCustomProviderUsesExplicitBaseURL(t *testing.T) {
	t.Setenv("CODEX_MODEL_PROVIDER", "custom")
	t.Setenv("CODEX_MODEL_BASE_URL", "https://codex-provider.example/v1")
	t.Setenv("OPENAI_BASE_URL", "")
	sourceHome := t.TempDir()
	targetHome := t.TempDir()
	if err := writeAutomationCodexConfig(sourceHome, targetHome, "/tmp/project"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(targetHome, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	config := string(data)
	for _, want := range []string{
		`model_provider = "custom"`,
		`[model_providers.custom]`,
		`base_url = "https://codex-provider.example/v1"`,
	} {
		if !strings.Contains(config, want) {
			t.Fatalf("missing %q in config:\n%s", want, config)
		}
	}
}
