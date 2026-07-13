package gen

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/workflow"
)

func TestGenerateRepoAnalysisResumesFromDraftCheckpoint(t *testing.T) {
	source := t.TempDir()
	checkpointRoot := t.TempDir()
	prepared := domain.RepoPrepared{
		RepoURL:        "https://github.com/org/repo",
		ResolvedCommit: "abc1234",
		SourcePath:     source,
	}
	draft := repoAnalysisJSON(t, prepared)
	first := &checkpointRuntime{scripts: [][]checkpointTurn{{
		{text: draft},
		{err: errors.New("simulated self-review interruption")},
	}}}
	opts := ConversationOptions{
		Workspace:      t.TempDir(),
		CheckpointRoot: checkpointRoot,
		RunID:          "run-checkpoint",
		Attempt:        1,
		Model:          "test-model",
		Agent:          first,
	}
	if _, err := GenerateRepoAnalysis(context.Background(), opts, prepared); err == nil {
		t.Fatal("first attempt unexpectedly succeeded")
	}
	if got := first.turnCount(); got != 2 {
		t.Fatalf("first attempt turns=%d, want draft plus interrupted self-review", got)
	}
	checkpoint := filepath.Join(checkpointRoot, "runs", "run-checkpoint", "stages", "repo_analyze", "nodes", "repo_analyze", "attempt-001", "turn-001", "checkpoint.json")
	if _, err := os.Stat(checkpoint); err != nil {
		t.Fatalf("draft checkpoint was not persisted: %v", err)
	}

	second := &checkpointRuntime{scripts: [][]checkpointTurn{{{text: draft}}}}
	opts.Attempt = 2
	opts.Agent = second
	analysis, err := GenerateRepoAnalysis(context.Background(), opts, prepared)
	if err != nil {
		t.Fatalf("resume from draft checkpoint: %v", err)
	}
	if got := second.turnCount(); got != 1 {
		t.Fatalf("resume turns=%d, want only self-review after reused draft", got)
	}
	if analysis.Language != "go" || analysis.CommitSHA != prepared.ResolvedCommit {
		t.Fatalf("resumed analysis=%+v", analysis)
	}
}

func TestMaterializeCanonicalTaskPreservesPreviousOutputWhenValidationFails(t *testing.T) {
	taskDir := filepath.Join(t.TempDir(), "task")
	if err := os.MkdirAll(filepath.Join(taskDir, "solution"), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := "#!/bin/sh\necho previous\n"
	if err := os.WriteFile(filepath.Join(taskDir, "solution", "solve.sh"), []byte(previous), 0o755); err != nil {
		t.Fatal(err)
	}

	err := MaterializeCanonicalTask(taskDir, CanonicalTask{
		Instruction:   "Fix the config loader.\n",
		TaskTOML:      "[task]\nname = \"sample\"\n",
		Dockerfile:    "FROM ubuntu:24.04\n",
		SolveScript:   "#!/bin/sh\n# promptflow legacy residue\n",
		TestScript:    "#!/bin/sh\nexit 0\n",
		TestsAnalysis: "Valid analysis.\n",
	})
	if err == nil || !strings.Contains(err.Error(), "legacy non-Harbor") {
		t.Fatalf("expected legacy validation failure, got %v", err)
	}
	data, err := os.ReadFile(filepath.Join(taskDir, "solution", "solve.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != previous {
		t.Fatalf("previous output changed after failed materialization:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(taskDir, "instruction.md")); !os.IsNotExist(err) {
		t.Fatalf("failed materialization leaked a partial file: %v", err)
	}
}

func TestNormalizeTestScriptIsolatesGeneratedExitTrap(t *testing.T) {
	script := normalizeTestScript("cleanup() { rm -f temporary-test; }\ntrap cleanup EXIT\nprintf ok\n")
	rewardTrap := strings.Index(script, "trap finish EXIT")
	subshell := strings.Index(script, "\n(\n")
	cleanupTrap := strings.Index(script, "trap cleanup EXIT")
	if rewardTrap < 0 || subshell < 0 || cleanupTrap < 0 {
		t.Fatalf("normalized script is missing required traps or subshell:\n%s", script)
	}
	if !(rewardTrap < subshell && subshell < cleanupTrap) {
		t.Fatalf("generated cleanup trap must be isolated below the parent reward trap:\n%s", script)
	}
	closingSubshell := strings.LastIndex(script, "\n)\n")
	if closingSubshell < 0 || cleanupTrap > closingSubshell || !strings.Contains(script[cleanupTrap:closingSubshell], "printf ok") {
		t.Fatalf("generated body must execute completely inside the subshell:\n%s", script)
	}
}

func TestRenderDockerfileFiltersRepositoryLifecycleAndBuildTimeTests(t *testing.T) {
	dockerfile := renderDockerfile(domain.RepoPrepared{
		RepoURL:        "https://github.com/org/repo.git",
		ResolvedCommit: "abc1234",
	}, domain.TaskProposal{
		CodeLang: "rust",
		SetupCommands: []string{
			"git clone https://github.com/org/repo.git",
			"cd repo",
			"git checkout abc1234",
			"cargo fetch",
			" cargo   fetch ",
			"cargo fetch --locked",
			"cargo test --all-features",
		},
	})
	if strings.Count(dockerfile, "git clone") != 1 || strings.Count(dockerfile, "git checkout") != 1 {
		t.Fatalf("Dockerfile must contain exactly one system-owned repo bootstrap:\n%s", dockerfile)
	}
	if strings.Contains(dockerfile, "RUN cd /app/repo && cd repo") || strings.Contains(dockerfile, "cargo test") {
		t.Fatalf("Dockerfile retained invalid setup commands:\n%s", dockerfile)
	}
	guardedFetch := "RUN cd /app/repo && if [ -f Cargo.lock ]; then cargo fetch --locked; else cargo fetch; fi"
	if strings.Count(dockerfile, guardedFetch) != 1 {
		t.Fatalf("Dockerfile did not retain and deduplicate dependency preparation:\n%s", dockerfile)
	}
	if !strings.HasPrefix(dockerfile, "FROM "+rustBaseImage+"\n") || !strings.Contains(dockerfile, "RUN rustup component add rustfmt clippy") {
		t.Fatalf("Rust Dockerfile did not use the pinned toolchain:\n%s", dockerfile)
	}
}

type checkpointTurn struct {
	text string
	err  error
}

type checkpointRuntime struct {
	scripts       [][]checkpointTurn
	conversations []*checkpointConversation
}

func (runtime *checkpointRuntime) OpenConversation(_ context.Context, _ workflow.AgentConversationRequest) (workflow.AgentConversation, error) {
	if len(runtime.scripts) == 0 {
		return nil, errors.New("no scripted conversation")
	}
	conversation := &checkpointConversation{turns: append([]checkpointTurn(nil), runtime.scripts[0]...)}
	runtime.scripts = runtime.scripts[1:]
	runtime.conversations = append(runtime.conversations, conversation)
	return conversation, nil
}

func (runtime *checkpointRuntime) turnCount() int {
	count := 0
	for _, conversation := range runtime.conversations {
		count += conversation.turnCount
	}
	return count
}

type checkpointConversation struct {
	turns     []checkpointTurn
	turnCount int
}

func (conversation *checkpointConversation) Turn(_ context.Context, _ workflow.AgentTurnRequest) (workflow.AgentTurnResult, error) {
	conversation.turnCount++
	if len(conversation.turns) == 0 {
		return workflow.AgentTurnResult{}, errors.New("unexpected agent turn")
	}
	turn := conversation.turns[0]
	conversation.turns = conversation.turns[1:]
	if turn.err != nil {
		return workflow.AgentTurnResult{}, turn.err
	}
	return workflow.AgentTurnResult{Text: turn.text, Model: "test-model"}, nil
}

func (*checkpointConversation) Close() error { return nil }

func repoAnalysisJSON(t *testing.T, prepared domain.RepoPrepared) string {
	t.Helper()
	data, err := json.Marshal(domain.RepoAnalysis{
		SchemaVersion: "harbor.repo_analysis.v1",
		RepoURL:       prepared.RepoURL,
		CommitSHA:     prepared.ResolvedCommit,
		Language:      "go",
		BuildSystem:   "go modules",
		TestFramework: "go test",
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
