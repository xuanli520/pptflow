package gen

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/workflow"
)

type fakeAgent struct {
	outputs  []string
	requests []workflow.AgentTurnRequest
}

func (f *fakeAgent) Turn(_ context.Context, req workflow.AgentTurnRequest) (workflow.AgentTurnResult, error) {
	f.requests = append(f.requests, req)
	if len(f.outputs) == 0 {
		return workflow.AgentTurnResult{}, os.ErrInvalid
	}
	out := f.outputs[0]
	f.outputs = f.outputs[1:]
	return workflow.AgentTurnResult{Text: out, Model: req.Model}, nil
}

func TestRunGeneratesHarborTaskFiles(t *testing.T) {
	source := t.TempDir()
	workspace := t.TempDir()
	taskOutput := filepath.Join(t.TempDir(), "task")
	agent := &fakeAgent{outputs: []string{
		jsonText(t, domain.RepoAnalysis{
			SchemaVersion: "harbor.repo_analysis.v1",
			RepoURL:       "https://github.com/org/repo",
			CommitSHA:     "abc1234",
			Language:      "go",
			BuildSystem:   "go modules",
			TestFramework: "go test",
		}),
		jsonText(t, domain.TaskProposal{
			SchemaVersion:         "harbor.task_proposal.v1",
			TaskName:              "codeedge/fix-config-loader",
			OneLineDescription:    "Fix config loader environment override handling.",
			CodeLang:              "go",
			TaskType:              "bug-fix",
			Application:           "backend",
			GitHubLink:            "https://github.com/org/repo",
			CommitSHA:             "abc1234",
			EstimatedAHTMinutes:   60,
			TargetFiles:           []string{"internal/config/loader.go"},
			DifficultyRationale:   "Requires tracing config precedence across packages.",
			SuggestedVerification: "go test ./...",
		}),
		"```json\n" + jsonText(t, domain.GeneratedTaskFiles{
			SchemaVersion: "harbor.generated_task_files.v1",
			InstructionMD: "Fix the config loader so environment overrides win while preserving file defaults.\n\nRun `go test ./...`.",
			SolveSH:       "cd /app/repo\nprintf fixed > internal/config/loader.go\n",
			TestSH:        "cd /app/repo\nprintf '%s' ok | grep ok\n",
			TestsAnalysis: "## 1. instruction 和 environment 已提供的信息\n- Task is visible.\n\n---\n\n## 2. 模型的理论通过路径\n- Inspect config loader.\n\n---\n\n## 3. 模型具备通过条件的依据\n- Checks follow instruction.\n",
		}) + "\n```",
	}}
	var progress []string
	report, err := Run(context.Background(), Options{
		RepoPrepared: domain.RepoPrepared{
			RepoURL:         "https://github.com/org/repo",
			ResolvedCommit:  "abc1234",
			RequestedCommit: "abc1234",
			TreeHash:        "tree123",
			SourcePath:      source,
			PreparedAt:      time.Now(),
		},
		Workspace:     workspace,
		TaskOutputDir: taskOutput,
		Model:         "test-model",
		Agent:         agent,
		Progress: func(nodeID, status, _ string, _ string) {
			progress = append(progress, nodeID+":"+status)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed || report.TaskDir != taskOutput {
		t.Fatalf("unexpected report: %+v", report)
	}
	for _, rel := range []string{"instruction.md", "task.toml", "tests_analysis.md", "environment/Dockerfile", "solution/solve.sh", "tests/test.sh"} {
		if _, err := os.Stat(filepath.Join(taskOutput, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("missing generated %s: %v", rel, err)
		}
	}
	for _, rel := range []string{
		"phase1/artifacts/instruction_generate/instruction.md",
		"phase1/artifacts/task_toml_generate/task.toml",
		"phase1/artifacts/dockerfile_generate/Dockerfile",
		"phase2/artifacts/solve_generate/solve.sh",
		"phase2/artifacts/test_generate/test.sh",
		"phase3/artifacts/tests_analysis/tests_analysis.md",
	} {
		if _, err := os.Stat(filepath.Join(workspace, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("missing split artifact %s: %v", rel, err)
		}
	}
	dockerfile := readFile(t, filepath.Join(taskOutput, "environment", "Dockerfile"))
	if !strings.Contains(dockerfile, "git clone 'https://github.com/org/repo'") || !strings.Contains(dockerfile, "git checkout 'abc1234'") {
		t.Fatalf("Dockerfile is not pinned to repo commit:\n%s", dockerfile)
	}
	taskTOML := readFile(t, filepath.Join(taskOutput, "task.toml"))
	if !strings.Contains(taskTOML, "code_lang = \"go\"") || !strings.Contains(taskTOML, "task_type = \"bug-fix\"") {
		t.Fatalf("task.toml missing metadata:\n%s", taskTOML)
	}
	testScript := readFile(t, filepath.Join(taskOutput, "tests", "test.sh"))
	if !strings.Contains(testScript, "/logs/verifier/reward.txt") {
		t.Fatalf("test script missing reward wrapper:\n%s", testScript)
	}
	testsAnalysis := readFile(t, report.TestsAnalysisPath)
	if !strings.Contains(testsAnalysis, "## 1. instruction 和 environment 已提供的信息") {
		t.Fatalf("tests analysis missing CodeEdge heading:\n%s", testsAnalysis)
	}
	if !strings.Contains(testsAnalysis, "environment 通过 Dockerfile") || !strings.Contains(testsAnalysis, "原始生成备注") {
		t.Fatalf("thin tests analysis was not expanded with structured attribution:\n%s", testsAnalysis)
	}
	if len(agent.requests) != 3 {
		t.Fatalf("agent calls = %d, want 3", len(agent.requests))
	}
	for _, want := range []string{
		"repo_analyze:succeeded",
		"instruction_generate:succeeded",
		"task_toml_generate:succeeded",
		"dockerfile_generate:succeeded",
		"solve_generate:succeeded",
		"test_generate:succeeded",
		"tests_analysis:succeeded",
		"materialize_task:succeeded",
	} {
		if !contains(progress, want) {
			t.Fatalf("progress missing %s: %#v", want, progress)
		}
	}
	if contains(progress, "task_review:succeeded") {
		t.Fatalf("standalone gen should not emit task_review without callback: %#v", progress)
	}
}

func TestRunReusesExistingAgentArtifacts(t *testing.T) {
	source := t.TempDir()
	workspace := t.TempDir()
	taskOutput := filepath.Join(t.TempDir(), "task")
	prepared := domain.RepoPrepared{
		RepoURL:         "https://github.com/org/repo",
		ResolvedCommit:  "abc1234",
		RequestedCommit: "abc1234",
		TreeHash:        "tree123",
		SourcePath:      source,
		PreparedAt:      time.Now(),
	}
	firstAgent := &fakeAgent{outputs: []string{
		jsonText(t, domain.RepoAnalysis{
			SchemaVersion: "harbor.repo_analysis.v1",
			RepoURL:       prepared.RepoURL,
			CommitSHA:     prepared.ResolvedCommit,
			Language:      "go",
			BuildSystem:   "go modules",
			TestFramework: "go test",
		}),
		jsonText(t, domain.TaskProposal{
			SchemaVersion:         "harbor.task_proposal.v1",
			TaskName:              "codeedge/fix-config-loader",
			OneLineDescription:    "Fix config loader environment override handling.",
			CodeLang:              "go",
			TaskType:              "bug-fix",
			Application:           "backend",
			GitHubLink:            prepared.RepoURL,
			CommitSHA:             prepared.ResolvedCommit,
			EstimatedAHTMinutes:   60,
			TargetFiles:           []string{"internal/config/loader.go"},
			DifficultyRationale:   "Requires tracing config precedence across packages.",
			SuggestedVerification: "go test ./...",
		}),
		jsonText(t, domain.GeneratedTaskFiles{
			SchemaVersion: "harbor.generated_task_files.v1",
			InstructionMD: "Fix the config loader.",
			SolveSH:       "cd /app/repo\nprintf fixed > internal/config/loader.go\n",
			TestSH:        "cd /app/repo\ngrep -q fixed internal/config/loader.go\n",
			TestsAnalysis: "## 1. instruction 和 environment 已提供的信息\n- Task is visible.\n\n## 2. 模型的理论通过路径\n- Inspect config loader.\n\n## 3. 模型具备通过条件的依据\n- Public verifier.\n",
		}),
	}}
	if _, err := Run(context.Background(), Options{
		RepoPrepared:  prepared,
		Workspace:     workspace,
		TaskOutputDir: taskOutput,
		Agent:         firstAgent,
	}); err != nil {
		t.Fatal(err)
	}
	if len(firstAgent.requests) != 3 {
		t.Fatalf("initial agent calls = %d, want 3", len(firstAgent.requests))
	}

	secondAgent := &fakeAgent{}
	var progress []string
	report, err := Run(context.Background(), Options{
		RepoPrepared:  prepared,
		Workspace:     workspace,
		TaskOutputDir: taskOutput,
		Agent:         secondAgent,
		Progress: func(nodeID, status, message, _ string) {
			if status == "succeeded" {
				progress = append(progress, nodeID+":"+message)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed || len(secondAgent.requests) != 0 {
		t.Fatalf("reuse run should pass without agent calls: report=%+v calls=%d", report, len(secondAgent.requests))
	}
	for _, want := range []string{
		"repo_analyze:reused existing repo analysis",
		"task_design:reused existing task proposal",
		"generate_task_files:reused existing task file content",
	} {
		if !contains(progress, want) {
			t.Fatalf("progress missing reuse marker %q: %#v", want, progress)
		}
	}
}

func TestRunRegeneratesTaskFilesWhenProposalProvenanceChanges(t *testing.T) {
	source := t.TempDir()
	workspace := t.TempDir()
	taskOutput := filepath.Join(t.TempDir(), "task")
	prepared := domain.RepoPrepared{
		RepoURL:         "https://github.com/org/repo",
		ResolvedCommit:  "abc1234",
		RequestedCommit: "abc1234",
		TreeHash:        "tree123",
		SourcePath:      source,
		PreparedAt:      time.Now(),
	}
	firstAgent := &fakeAgent{outputs: []string{
		jsonText(t, domain.RepoAnalysis{
			SchemaVersion: "harbor.repo_analysis.v1",
			RepoURL:       prepared.RepoURL,
			CommitSHA:     prepared.ResolvedCommit,
			Language:      "go",
			BuildSystem:   "go modules",
			TestFramework: "go test",
		}),
		jsonText(t, domain.TaskProposal{
			SchemaVersion:         "harbor.task_proposal.v1",
			TaskName:              "codeedge/fix-config-loader",
			OneLineDescription:    "Fix config loader environment override handling.",
			CodeLang:              "go",
			TaskType:              "bug-fix",
			Application:           "backend",
			GitHubLink:            prepared.RepoURL,
			CommitSHA:             prepared.ResolvedCommit,
			EstimatedAHTMinutes:   60,
			TargetFiles:           []string{"internal/config/loader.go"},
			DifficultyRationale:   "Requires tracing config precedence across packages.",
			SuggestedVerification: "go test ./...",
		}),
		jsonText(t, domain.GeneratedTaskFiles{
			SchemaVersion: "harbor.generated_task_files.v1",
			InstructionMD: "Fix the config loader.",
			SolveSH:       "cd /app/repo\nprintf fixed > internal/config/loader.go\n",
			TestSH:        "cd /app/repo\ngrep -q fixed internal/config/loader.go\n",
			TestsAnalysis: "## 1. instruction 和 environment 已提供的信息\n- Task is visible.\n\n## 2. 模型的理论通过路径\n- Inspect config loader.\n\n## 3. 模型具备通过条件的依据\n- Public verifier.\n",
		}),
	}}
	if _, err := Run(context.Background(), Options{
		RepoPrepared:  prepared,
		Workspace:     workspace,
		TaskOutputDir: taskOutput,
		Agent:         firstAgent,
	}); err != nil {
		t.Fatal(err)
	}

	var files domain.GeneratedTaskFiles
	raw, err := os.ReadFile(filepath.Join(workspace, "phase1", "artifacts", "generate_task_files", "task_files.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &files); err != nil {
		t.Fatal(err)
	}
	files.TaskProposalDigest = "sha256:stale"
	raw, err = json.Marshal(files)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "phase1", "artifacts", "generate_task_files", "task_files.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	secondAgent := &fakeAgent{outputs: []string{jsonText(t, domain.GeneratedTaskFiles{
		SchemaVersion: "harbor.generated_task_files.v1",
		InstructionMD: "Fix the config loader after proposal provenance changed.",
		SolveSH:       "cd /app/repo\nprintf fixed > internal/config/loader.go\n",
		TestSH:        "cd /app/repo\ngrep -q fixed internal/config/loader.go\n",
		TestsAnalysis: "## 1. instruction 和 environment 已提供的信息\n- Task is visible.\n\n## 2. 模型的理论通过路径\n- Inspect config loader.\n\n## 3. 模型具备通过条件的依据\n- Public verifier.\n",
	})}}
	if _, err := Run(context.Background(), Options{
		RepoPrepared:  prepared,
		Workspace:     workspace,
		TaskOutputDir: taskOutput,
		Agent:         secondAgent,
	}); err != nil {
		t.Fatal(err)
	}
	if len(secondAgent.requests) != 1 || secondAgent.requests[0].LogPath == "" || !strings.Contains(secondAgent.requests[0].LogPath, "generate_task_files") {
		t.Fatalf("expected only task file generation to re-run, got %#v", secondAgent.requests)
	}
}

func TestRunFailsWhenTaskOutputDirContainsUnexpectedFile(t *testing.T) {
	source := t.TempDir()
	workspace := t.TempDir()
	taskOutput := filepath.Join(t.TempDir(), "task")
	if err := os.MkdirAll(filepath.Join(taskOutput, "environment"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskOutput, "environment", "promptflow_runner.py"), []byte("print('legacy')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	agent := &fakeAgent{outputs: []string{
		jsonText(t, domain.RepoAnalysis{
			SchemaVersion: "harbor.repo_analysis.v1",
			RepoURL:       "https://github.com/org/repo",
			CommitSHA:     "abc1234",
			Language:      "go",
			BuildSystem:   "go modules",
			TestFramework: "go test",
		}),
		jsonText(t, domain.TaskProposal{
			SchemaVersion:         "harbor.task_proposal.v1",
			TaskName:              "codeedge/fix-config-loader",
			OneLineDescription:    "Fix config loader environment override handling.",
			CodeLang:              "go",
			TaskType:              "bug-fix",
			Application:           "backend",
			GitHubLink:            "https://github.com/org/repo",
			CommitSHA:             "abc1234",
			EstimatedAHTMinutes:   60,
			TargetFiles:           []string{"internal/config/loader.go"},
			DifficultyRationale:   "Requires tracing config precedence across packages.",
			SuggestedVerification: "go test ./...",
		}),
		jsonText(t, domain.GeneratedTaskFiles{
			SchemaVersion: "harbor.generated_task_files.v1",
			InstructionMD: "Fix the config loader.",
			SolveSH:       "cd /app/repo\nprintf fixed > internal/config/loader.go\n",
			TestSH:        "cd /app/repo\ngrep -q fixed internal/config/loader.go\n",
			TestsAnalysis: "## 1. instruction 和 environment 已提供的信息\n- Task is visible.\n\n## 2. 模型的理论通过路径\n- Inspect config loader.\n\n## 3. 模型具备通过条件的依据\n- Public verifier.\n",
		}),
	}}
	_, err := Run(context.Background(), Options{
		RepoPrepared: domain.RepoPrepared{
			RepoURL:         "https://github.com/org/repo",
			ResolvedCommit:  "abc1234",
			RequestedCommit: "abc1234",
			TreeHash:        "tree123",
			SourcePath:      source,
			PreparedAt:      time.Now(),
		},
		Workspace:     workspace,
		TaskOutputDir: taskOutput,
		Agent:         agent,
	})
	if err == nil || !strings.Contains(err.Error(), "unexpected file") {
		t.Fatalf("expected dirty task output failure, got %v", err)
	}
}

func TestRunRejectsCredentialedPreparedRepoURL(t *testing.T) {
	_, err := Run(context.Background(), Options{
		RepoPrepared: domain.RepoPrepared{
			RepoURL:        "https://token@github.com/org/repo.git",
			ResolvedCommit: "abc1234",
			SourcePath:     t.TempDir(),
		},
		Workspace: t.TempDir(),
		Agent:     &fakeAgent{},
	})
	if err == nil || !strings.Contains(err.Error(), "credentials") {
		t.Fatalf("expected credentialed repo URL failure, got %v", err)
	}
}

func jsonText(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
