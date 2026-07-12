package cmd

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/app"
)

func TestTUICommandRejectsAutoApprove(t *testing.T) {
	cmd := newTUICommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--auto-approve"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected tui --auto-approve to fail")
	}
	if !strings.Contains(err.Error(), "manual review gates") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunnerEnvironmentDefaultsUseSafeCredentialTemplateAndStageRoutes(t *testing.T) {
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "relay-secret")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_BASE_URL", "https://fallback.example")
	t.Setenv("QWEN_HARBOR_BASE_URL", "https://qwen.example")
	t.Setenv("OPUS_HARBOR_BASE_URL", "https://opus.example")

	opts := app.RunnerOptions{}
	applyRunnerEnvironmentDefaults(&opts)
	want := []string{"ANTHROPIC_AUTH_TOKEN=${ANTHROPIC_AUTH_TOKEN}"}
	if !reflect.DeepEqual(opts.HarborAgentEnv, want) {
		t.Fatalf("agent environment=%q, want safe template %q", opts.HarborAgentEnv, want)
	}
	if opts.QwenHarborBaseURL != "https://qwen.example" || opts.OpusHarborBaseURL != "https://opus.example" {
		t.Fatalf("stage routes not loaded: qwen=%q opus=%q", opts.QwenHarborBaseURL, opts.OpusHarborBaseURL)
	}

	cmd := newTUICommand()
	agentEnv, err := cmd.Flags().GetStringArray("harbor-agent-env")
	if err != nil {
		t.Fatal(err)
	}
	qwenURL, err := cmd.Flags().GetString("qwen-harbor-base-url")
	if err != nil {
		t.Fatal(err)
	}
	opusURL, err := cmd.Flags().GetString("opus-harbor-base-url")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(agentEnv, want) || qwenURL != "https://qwen.example" || opusURL != "https://opus.example" {
		t.Fatalf("TUI flags did not inherit environment defaults: env=%q qwen=%q opus=%q", agentEnv, qwenURL, opusURL)
	}
}

func TestRunnerEnvironmentDefaultsUseGenericBaseURLFallback(t *testing.T) {
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_BASE_URL", "https://fallback.example")
	t.Setenv("QWEN_HARBOR_BASE_URL", "")
	t.Setenv("OPUS_HARBOR_BASE_URL", "")

	opts := app.RunnerOptions{}
	applyRunnerEnvironmentDefaults(&opts)
	if len(opts.HarborAgentEnv) != 0 || opts.QwenHarborBaseURL != "https://fallback.example" || opts.OpusHarborBaseURL != "https://fallback.example" {
		t.Fatalf("unexpected fallback defaults: %+v", opts)
	}
}
