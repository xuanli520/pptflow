package repair

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/workflow"
)

type repairAgent struct {
	edit bool
	req  workflow.AgentTurnRequest
}

func (a *repairAgent) Turn(_ context.Context, req workflow.AgentTurnRequest) (workflow.AgentTurnResult, error) {
	a.req = req
	if a.edit {
		if err := os.WriteFile(filepath.Join(req.ProjectPath, "instruction.md"), []byte("repaired instruction\n"), 0o644); err != nil {
			return workflow.AgentTurnResult{}, err
		}
	}
	return workflow.AgentTurnResult{Text: "updated task", Model: "fake-codex"}, nil
}

func TestRunRepairsTaskWithWorkspaceWriteAndPersistsDigest(t *testing.T) {
	taskDir := writeRepairTask(t)
	agent := &repairAgent{edit: true}
	reportPath := filepath.Join(t.TempDir(), "repair.json")
	report, err := Run(context.Background(), Options{
		TaskDir:     taskDir,
		Guidance:    "Follow external reviewer feedback",
		Findings:    []string{"instruction and tests disagree"},
		Source:      "external_review",
		Round:       1,
		Agent:       agent,
		WriteReport: reportPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Changed || report.BeforeDigest == report.AfterDigest || report.AgentModel != "fake-codex" {
		t.Fatalf("unexpected repair report: %+v", report)
	}
	if agent.req.SandboxMode != "workspace-write" || agent.req.SandboxPolicy != "workspace-write" || len(agent.req.WorkspaceRoots) != 1 || agent.req.WorkspaceRoots[0] != taskDir {
		t.Fatalf("repair agent did not receive scoped write access: %+v", agent.req)
	}
	if !strings.Contains(agent.req.Prompt, "external reviewer feedback") || !strings.Contains(agent.req.Prompt, "instruction and tests disagree") {
		t.Fatalf("repair prompt missing operator/reviewer context: %s", agent.req.Prompt)
	}
	if _, err := os.Stat(reportPath); err != nil {
		t.Fatalf("repair report missing: %v", err)
	}
}

func TestRunRejectsNoOpRepair(t *testing.T) {
	_, err := Run(context.Background(), Options{TaskDir: writeRepairTask(t), Agent: &repairAgent{}})
	if err == nil || !strings.Contains(err.Error(), "without changing task files") {
		t.Fatalf("expected no-op repair failure, got %v", err)
	}
}

func writeRepairTask(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"instruction.md":         "fix task\n",
		"task.toml":              "schema_version = \"1.3\"\n",
		"tests_analysis.md":      "analysis\n",
		"environment/Dockerfile": "FROM scratch\n",
		"solution/solve.sh":      "true\n",
		"tests/test.sh":          "true\n",
	}
	for rel, content := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
