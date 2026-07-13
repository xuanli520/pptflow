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
	harborgen "github.com/purplevoid/harbor-factory/internal/harbor/gen"
	"github.com/purplevoid/harbor-factory/internal/workflow"
)

func TestGenerationPluginsUseThreeIsolatedMultiTurnConversations(t *testing.T) {
	workspace := t.TempDir()
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "SOURCE_ONLY"), []byte("must not reach downstream workspace"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := workflow.NewFileArtifactStore(workspace)
	if err != nil {
		t.Fatal(err)
	}
	prepared := domain.RepoPrepared{SchemaVersion: "harbor.repo_prepared.v1", RepoURL: "https://github.com/org/repo", RequestedCommit: "abc1234", ResolvedCommit: "abc1234", TreeHash: "tree123", SourcePath: source}
	preparedRef, err := store.PutJSON(context.Background(), "phase0/repo_prepared.json", "repo_prepared", "repo_prepare", prepared)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &scriptedRuntime{outputs: []string{
		jsonText(t, domain.RepoAnalysis{SchemaVersion: "harbor.repo_analysis.v1", RepoURL: prepared.RepoURL, CommitSHA: prepared.ResolvedCommit, Language: "go", BuildSystem: "go modules", TestFramework: "go test", EntryPoints: []string{"canonical-analysis-marker"}}),
		jsonText(t, domain.TaskProposal{SchemaVersion: "harbor.task_proposal.v1", TaskName: "codeedge/sample-task", OneLineDescription: "Fix canonical proposal behavior.", CodeLang: "go", TaskType: "bug-fix", Application: "backend", GitHubLink: prepared.RepoURL, CommitSHA: prepared.ResolvedCommit, EstimatedAHTMinutes: 45, DifficultyRationale: "cross-package behavior", SuggestedVerification: "go test ./...", BoundaryConditions: []string{"canonical-proposal-marker"}}),
		jsonText(t, domain.GeneratedTaskFiles{SchemaVersion: "harbor.generated_task_files.v1", InstructionMD: "Fix the canonical behavior and run go test ./...", SolveSH: "printf fixed > result.txt", TestSH: "test -f result.txt", TestsAnalysis: substantiveTestsAnalysis()}),
	}}

	repoResult, err := (RepoAnalyzePlugin{}).Execute(context.Background(), nodeRequest(workspace, store, runtime, "repo_analyze", RepoAnalyzeKind, []workflow.ArtifactRef{preparedRef}))
	if err != nil {
		t.Fatal(err)
	}
	designResult, err := (TaskDesignPlugin{}).Execute(context.Background(), nodeRequest(workspace, store, runtime, "task_design", TaskDesignKind, []workflow.ArtifactRef{preparedRef, repoResult.Artifacts[0]}))
	if err != nil {
		t.Fatal(err)
	}
	filesResult, err := (GenerateTaskFilesPlugin{}).Execute(context.Background(), nodeRequest(workspace, store, runtime, "generate_task_files", GenerateTaskFilesKind, []workflow.ArtifactRef{preparedRef, repoResult.Artifacts[0], designResult.Artifacts[0]}))
	if err != nil {
		t.Fatal(err)
	}

	if len(runtime.conversations) != 3 {
		t.Fatalf("conversation count=%d, want 3", len(runtime.conversations))
	}
	for i, conversation := range runtime.conversations {
		if len(conversation.prompts) < 2 {
			t.Fatalf("conversation %d turns=%d, want at least 2", i, len(conversation.prompts))
		}
	}
	if runtime.conversations[0].request.ProjectPath != source {
		t.Fatalf("repo_analyze must inspect prepared source: %+v", runtime.conversations[0].request)
	}
	for i, conversation := range runtime.conversations[1:] {
		if conversation.request.ProjectPath == source || !strings.HasPrefix(conversation.request.ProjectPath, filepath.Join(workspace, ".agent-workspaces")) {
			t.Fatalf("downstream conversation %d was not isolated: %+v", i+1, conversation.request)
		}
		if _, err := os.Stat(filepath.Join(conversation.request.ProjectPath, "SOURCE_ONLY")); !os.IsNotExist(err) {
			t.Fatalf("downstream conversation %d can see source-only file", i+1)
		}
	}
	if !strings.Contains(runtime.conversations[1].prompts[0], "canonical-analysis-marker") || strings.Contains(runtime.conversations[1].prompts[0], source) {
		t.Fatalf("task_design prompt must contain only canonical analysis, got:\n%s", runtime.conversations[1].prompts[0])
	}
	if !strings.Contains(runtime.conversations[2].prompts[0], "canonical-analysis-marker") || !strings.Contains(runtime.conversations[2].prompts[0], "canonical-proposal-marker") || strings.Contains(runtime.conversations[2].prompts[0], source) {
		t.Fatalf("task_files prompt must contain canonical JSON only, got:\n%s", runtime.conversations[2].prompts[0])
	}
	if filesResult.Artifacts[0].Type != "generated_task_files" {
		t.Fatalf("unexpected task files artifact: %+v", filesResult)
	}
}

func TestGenerationContentPluginsAndMaterializeUseCanonicalArtifacts(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	store, err := workflow.NewFileArtifactStore(workspace)
	if err != nil {
		t.Fatal(err)
	}
	prepared := domain.RepoPrepared{RepoURL: "https://github.com/org/repo", ResolvedCommit: "abc1234"}
	proposal := domain.TaskProposal{TaskName: "codeedge/sample-task", OneLineDescription: "Fix behavior", CodeLang: "go", TaskType: "bug-fix", Application: "backend", GitHubLink: prepared.RepoURL, CommitSHA: prepared.ResolvedCommit, EstimatedAHTMinutes: 45}
	files := domain.GeneratedTaskFiles{InstructionMD: "Fix behavior.\n", SolveSH: "printf fixed > result.txt", TestSH: "test -f result.txt", TestsAnalysis: substantiveTestsAnalysis()}
	preparedRef := putJSONRef(t, store, "prepared.json", "repo_prepared", prepared)
	proposalRef := putJSONRef(t, store, "proposal.json", "task_proposal", proposal)
	filesRef := putJSONRef(t, store, "files.json", "generated_task_files", files)

	instruction := executeOne(t, InstructionPlugin{}, nodeRequest(workspace, store, nil, "instruction_generate", InstructionKind, []workflow.ArtifactRef{filesRef}))
	taskTOML := executeOne(t, TaskTOMLPlugin{}, nodeRequest(workspace, store, nil, "task_toml_generate", TaskTOMLKind, []workflow.ArtifactRef{proposalRef}))
	dockerfile := executeOne(t, DockerfilePlugin{}, nodeRequest(workspace, store, nil, "dockerfile_generate", DockerfileKind, []workflow.ArtifactRef{preparedRef, proposalRef}))
	solve := executeOne(t, SolvePlugin{}, nodeRequest(workspace, store, nil, "solve_generate", SolveKind, []workflow.ArtifactRef{filesRef}))
	testScript := executeOne(t, TestPlugin{}, nodeRequest(workspace, store, nil, "test_generate", TestKind, []workflow.ArtifactRef{filesRef}))
	testsAnalysis := executeOne(t, TestsAnalysisPlugin{}, nodeRequest(workspace, store, nil, "tests_analysis", TestsAnalysisKind, []workflow.ArtifactRef{filesRef, proposalRef}))

	taskDir := filepath.Join(workspace, "phase2", "task", "sample-task")
	materializeReq := nodeRequest(workspace, store, nil, "materialize_task", MaterializeKind, []workflow.ArtifactRef{instruction, taskTOML, dockerfile, solve, testScript, testsAnalysis})
	materializeReq.Spec.Config["task_dir"] = taskDir
	result, err := (MaterializePlugin{}).Execute(ctx, materializeReq)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Artifacts) != 6 {
		t.Fatalf("materialized artifacts=%d, want 6", len(result.Artifacts))
	}
	for _, rel := range []string{"instruction.md", "task.toml", "environment/Dockerfile", "solution/solve.sh", "tests/test.sh", "tests_analysis.md"} {
		if _, err := os.Stat(filepath.Join(taskDir, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("missing materialized %s: %v", rel, err)
		}
	}

}

func TestGenerationPluginsValidateAndFailWithoutCanonicalInput(t *testing.T) {
	plugins := []workflow.Plugin{RepoAnalyzePlugin{}, TaskDesignPlugin{}, GenerateTaskFilesPlugin{}, InstructionPlugin{}, TaskTOMLPlugin{}, DockerfilePlugin{}, SolvePlugin{}, TestPlugin{}, TestsAnalysisPlugin{}, MaterializePlugin{}, RuntimeSelfCheckPlugin{}}
	for _, plugin := range plugins {
		manifest := plugin.Manifest()
		spec := workflow.NodeSpec{ID: manifest.ID, Kind: manifest.Kinds[0], Config: map[string]any{}}
		if manifest.ID == MaterializeKind || manifest.ID == RuntimeSelfCheckKind {
			if err := plugin.Validate(spec); err == nil {
				t.Fatalf("%s validation accepted missing task_dir", manifest.ID)
			}
			spec.Config["task_dir"] = "/task"
		}
		if err := plugin.Validate(spec); err != nil {
			t.Fatalf("%s validation failed: %v", manifest.ID, err)
		}
	}
	store, _ := workflow.NewFileArtifactStore(t.TempDir())
	_, err := (InstructionPlugin{}).Execute(context.Background(), workflow.NodeRequest{Spec: workflow.NodeSpec{ID: "instruction", Kind: InstructionKind}, Store: store})
	if err == nil || !strings.Contains(err.Error(), "canonical generated_task_files") {
		t.Fatalf("expected missing canonical input failure, got %v", err)
	}
}

func TestRuntimeSelfCheckPluginSuccessAndFailure(t *testing.T) {
	workspace := t.TempDir()
	store, _ := workflow.NewFileArtifactStore(workspace)
	spec := workflow.NodeSpec{ID: "runtime_self_check", Kind: RuntimeSelfCheckKind, Config: map[string]any{"task_dir": filepath.Join(workspace, "task")}}
	runtime := &scriptedRuntime{}
	passing := RuntimeSelfCheckPlugin{Check: func(_ context.Context, _ harborgen.ConversationOptions, taskDir, logPath string) error {
		if taskDir != spec.Config["task_dir"] {
			t.Fatalf("unexpected task dir %s", taskDir)
		}
		if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
			return err
		}
		return os.WriteFile(logPath, []byte("checked\n"), 0o600)
	}}
	result, err := passing.Execute(context.Background(), workflow.NodeRequest{Spec: spec, WorkspaceRoot: workspace, Store: store, Runtimes: workflow.Runtimes{Agent: runtime}})
	if err != nil || len(result.Artifacts) != 1 {
		t.Fatalf("unexpected runtime self-check success: %+v, %v", result, err)
	}
	want := errors.New("self-check failed")
	failing := RuntimeSelfCheckPlugin{Check: func(context.Context, harborgen.ConversationOptions, string, string) error { return want }}
	_, err = failing.Execute(context.Background(), workflow.NodeRequest{Spec: spec, WorkspaceRoot: workspace, Store: store, Runtimes: workflow.Runtimes{Agent: runtime}})
	if !errors.Is(err, want) {
		t.Fatalf("expected runtime self-check failure, got %v", err)
	}
}

type scriptedRuntime struct {
	outputs       []string
	conversations []*scriptedConversation
}

func (r *scriptedRuntime) OpenConversation(_ context.Context, request workflow.AgentConversationRequest) (workflow.AgentConversation, error) {
	if len(r.outputs) == 0 {
		return nil, errors.New("no scripted conversation output")
	}
	conversation := &scriptedConversation{request: request, output: r.outputs[0]}
	r.outputs = r.outputs[1:]
	r.conversations = append(r.conversations, conversation)
	return conversation, nil
}

type scriptedConversation struct {
	request workflow.AgentConversationRequest
	output  string
	prompts []string
}

func (c *scriptedConversation) Turn(_ context.Context, request workflow.AgentTurnRequest) (workflow.AgentTurnResult, error) {
	c.prompts = append(c.prompts, request.Prompt)
	return workflow.AgentTurnResult{Text: c.output, Model: c.request.Model}, nil
}

func (*scriptedConversation) Close() error { return nil }

func nodeRequest(workspace string, store workflow.ArtifactStore, runtime workflow.AgentRuntime, id, kind string, inputs []workflow.ArtifactRef) workflow.NodeRequest {
	return workflow.NodeRequest{RunID: "run-1", WorkspaceRoot: workspace, Store: store, Inputs: inputs, Spec: workflow.NodeSpec{ID: id, Kind: kind, Config: map[string]any{"model": "test-model", "timeout_seconds": 60}}, Runtimes: workflow.Runtimes{Agent: runtime}}
}

func executeOne(t *testing.T, plugin workflow.Plugin, req workflow.NodeRequest) workflow.ArtifactRef {
	t.Helper()
	result, err := plugin.Execute(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Artifacts) != 1 {
		t.Fatalf("%s artifacts=%d, want 1", req.Spec.ID, len(result.Artifacts))
	}
	return result.Artifacts[0]
}

func putJSONRef(t *testing.T, store workflow.ArtifactStore, name, artifactType string, value any) workflow.ArtifactRef {
	t.Helper()
	ref, err := store.PutJSON(context.Background(), name, artifactType, "fixture", value)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func jsonText(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func substantiveTestsAnalysis() string {
	return "## 1. instruction 和 environment 已提供的信息\n- instruction exposes the target behavior and Dockerfile pins the repository.\n\n## 2. 模型的理论通过路径\n- Inspect the implementation, repair behavior, and run public tests.\n\n## 3. 模型具备通过条件的依据\n- The verifier checks only requirements stated by instruction and environment.\n"
}
