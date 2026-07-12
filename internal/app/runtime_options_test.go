package app

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/nodes"
)

func TestNewRunnerHydratesRuntimeEnvironmentWithoutCLI(t *testing.T) {
	clearClaudeEnvironment(t)
	t.Setenv("ANTHROPIC_API_KEY", "runtime-key")
	t.Setenv("QWEN_HARBOR_BASE_URL", "https://qwen.runtime")
	t.Setenv("OPUS_HARBOR_BASE_URL", "https://opus.runtime")
	runner := NewRunner(RunnerOptions{})
	wantEnv := []string{"ANTHROPIC_API_KEY=${ANTHROPIC_API_KEY}"}
	if !reflect.DeepEqual(runner.opts.HarborAgentEnv, wantEnv) || runner.opts.QwenHarborBaseURL != "https://qwen.runtime" || runner.opts.OpusHarborBaseURL != "https://opus.runtime" {
		t.Fatalf("runner did not hydrate runtime environment: %+v", runner.opts)
	}
}

func TestLoadRunnerOptionsReplacesStaleCredentialAndRoutes(t *testing.T) {
	clearClaudeEnvironment(t)
	t.Setenv("ANTHROPIC_API_KEY", "current-key")
	t.Setenv("QWEN_HARBOR_BASE_URL", "https://qwen.current")
	t.Setenv("OPUS_HARBOR_BASE_URL", "https://opus.current")
	workspace := t.TempDir()
	snapshot := domain.RunnerOptionsSnapshot{
		SchemaVersion:      runnerOptionsSchemaVersion,
		Workspace:          workspace,
		TaskDir:            "/tmp/task",
		HarborAgentEnvKeys: []string{"ANTHROPIC_AUTH_TOKEN"},
		QwenHarborBaseURL:  "https://qwen.stale",
		OpusHarborBaseURL:  "https://opus.stale",
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nodes.RunOptionsPath(workspace), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := LoadRunnerOptions(workspace)
	if err != nil {
		t.Fatal(err)
	}
	wantEnv := []string{"ANTHROPIC_API_KEY=${ANTHROPIC_API_KEY}"}
	if !reflect.DeepEqual(loaded.HarborAgentEnv, wantEnv) || loaded.QwenHarborBaseURL != "https://qwen.current" || loaded.OpusHarborBaseURL != "https://opus.current" {
		t.Fatalf("stale runtime values survived load: %+v", loaded)
	}
}

func TestMergeRuntimeOptionsPreservesBusinessConfigAndOverridesRuntimeState(t *testing.T) {
	clearClaudeEnvironment(t)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-token")
	historical := RunnerOptions{
		TaskDir:             "/task",
		TaskName:            "task-name",
		HarborAgentEnv:      []string{"ANTHROPIC_AUTH_TOKEN=${ANTHROPIC_AUTH_TOKEN}", "CUSTOM=value"},
		QwenHarborBaseURL:   "https://stale-qwen",
		OpusHarborBaseURL:   "https://stale-opus",
		SimilarityThreshold: 0.37,
	}
	current := RunnerOptions{
		HarborAgentEnv:    []string{"CLAUDE_CODE_OAUTH_TOKEN=${CLAUDE_CODE_OAUTH_TOKEN}"},
		QwenHarborBaseURL: "https://current-qwen",
		OpusHarborBaseURL: "https://current-opus",
	}
	merged := MergeRuntimeOptions(historical, current)
	if merged.TaskDir != historical.TaskDir || merged.TaskName != historical.TaskName || merged.SimilarityThreshold != historical.SimilarityThreshold {
		t.Fatalf("business configuration changed: %+v", merged)
	}
	if merged.QwenHarborBaseURL != current.QwenHarborBaseURL || merged.OpusHarborBaseURL != current.OpusHarborBaseURL || !hasClaudeCredential(merged.HarborAgentEnv) {
		t.Fatalf("runtime configuration was not replaced: %+v", merged)
	}
	for _, value := range merged.HarborAgentEnv {
		if value == "ANTHROPIC_AUTH_TOKEN=${ANTHROPIC_AUTH_TOKEN}" {
			t.Fatalf("unresolved historical credential survived merge: %q", merged.HarborAgentEnv)
		}
	}
}

func clearClaudeEnvironment(t *testing.T) {
	t.Helper()
	for _, key := range append(append([]string(nil), claudeCredentialKeys...), "ANTHROPIC_BASE_URL", "QWEN_HARBOR_BASE_URL", "OPUS_HARBOR_BASE_URL", "GITHUB_TOKEN") {
		t.Setenv(key, "")
	}
}
