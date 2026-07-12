package quality

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/workflow"
)

func TestRunPassesReasonableTask(t *testing.T) {
	taskDir := writeQualityTask(t, false)
	analysis := filepath.Join(taskDir, "tests_analysis.md")
	if err := os.WriteFile(analysis, []byte("## 1. instruction 和 environment 已提供的信息\nok\n## 2. 模型的理论通过路径\nok\n## 3. 模型具备通过条件的依据\nok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), Options{
		TaskDir:           taskDir,
		TestsAnalysisPath: analysis,
		Proposal:          &domain.TaskProposal{TargetFiles: []string{"config.go"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.OverallPass {
		t.Fatalf("expected pass, got issues=%v warnings=%v checks=%+v", report.Issues, report.Warnings, report.Checks)
	}
	if report.TaskDigest == "" {
		t.Fatal("quality report must bind to the reviewed task digest")
	}
	if !report.Checks["instruction_leak"].Passed || !report.Checks["solve_bypass"].Passed {
		t.Fatalf("expected core checks pass: %+v", report.Checks)
	}
}

func TestRunFailsLeakAndBypass(t *testing.T) {
	taskDir := writeQualityTask(t, true)
	report, err := Run(context.Background(), Options{TaskDir: taskDir})
	if err != nil {
		t.Fatal(err)
	}
	if report.OverallPass {
		t.Fatalf("expected quality failure, got checks: %+v", report.Checks)
	}
	if report.Checks["instruction_leak"].Passed || report.Checks["solve_bypass"].Passed {
		t.Fatalf("expected leak and bypass failures: %+v", report.Checks)
	}
}

func TestRunTreatsAgentOverallPassFalseWithoutErrorCheckAsAdvisory(t *testing.T) {
	taskDir := writeQualityTask(t, false)
	analysis := filepath.Join(taskDir, "tests_analysis.md")
	if err := os.WriteFile(analysis, []byte("## 1. instruction 和 environment 已提供的信息\nok\n## 2. 模型的理论通过路径\nok\n## 3. 模型具备通过条件的依据\nok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), Options{
		TaskDir:           taskDir,
		TestsAnalysisPath: analysis,
		Proposal:          &domain.TaskProposal{TargetFiles: []string{"config.go"}},
		Agent:             qualityFakeAgent(`{"overall_pass":false,"checks":{},"warnings":[],"issues":["blocking semantic mismatch"]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.OverallPass {
		t.Fatalf("warning-only agent review must remain advisory, got %+v", report)
	}
	if !strings.Contains(strings.Join(report.Warnings, "\n"), "overall_pass=false") {
		t.Fatalf("missing advisory agent warning: %+v", report.Warnings)
	}
	if report.PromptFingerprint == "" || report.RubricFingerprint == "" || report.ReviewFingerprint == "" {
		t.Fatalf("quality review fingerprints missing: %+v", report)
	}
}

func TestRunFailsWhenAgentReportsErrorSeverityCheck(t *testing.T) {
	taskDir := writeQualityTask(t, false)
	report, err := Run(context.Background(), Options{
		TaskDir: taskDir,
		Agent:   qualityFakeAgent(`{"overall_pass":false,"checks":{"alignment":{"passed":false,"severity":"error","detail":"blocking mismatch"}},"issues":["blocking mismatch"]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.OverallPass || !strings.Contains(strings.Join(report.Issues, "\n"), "blocking mismatch") {
		t.Fatalf("error-severity agent check must block: %+v", report)
	}
}

func TestRunAllowsInstructionToNamePublicTestCommand(t *testing.T) {
	taskDir := writeQualityTask(t, false)
	path := filepath.Join(taskDir, "instruction.md")
	if err := os.WriteFile(path, []byte("Fix config.go and validate with /tests/test.sh.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), Options{TaskDir: taskDir})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Checks["instruction_leak"].Passed {
		t.Fatalf("public verifier command path is not answer leakage: %+v", report.Checks["instruction_leak"])
	}
}

func TestRunRedactsAgentOutputInReport(t *testing.T) {
	taskDir := writeQualityTask(t, false)
	reportPath := filepath.Join(t.TempDir(), "quality_report.json")
	secret := "raw-quality-secret"
	report, err := Run(context.Background(), Options{
		TaskDir:     taskDir,
		WriteReport: reportPath,
		Agent:       qualityFakeAgent(`{"overall_pass":true,"checks":{"secret":{"passed":true,"severity":"info","detail":"API_KEY=` + secret + `"}},"warnings":["Bearer ` + secret + `"],"issues":[]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(report.AgentOutput, secret) || strings.Contains(strings.Join(report.Warnings, "\n"), secret) || strings.Contains(report.Checks["agent_secret"].Detail, secret) {
		t.Fatalf("quality report was not redacted in memory: %+v", report)
	}
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("quality report file leaked secret: %s", raw)
	}
	if !strings.Contains(string(raw), "redacted") {
		t.Fatalf("quality report file missing redaction marker: %s", raw)
	}
}

type qualityFakeAgent string

func (a qualityFakeAgent) OpenConversation(context.Context, workflow.AgentConversationRequest) (workflow.AgentConversation, error) {
	return a, nil
}

func (a qualityFakeAgent) Turn(context.Context, workflow.AgentTurnRequest) (workflow.AgentTurnResult, error) {
	return workflow.AgentTurnResult{Text: string(a), Model: "fake"}, nil
}

func (qualityFakeAgent) Close() error { return nil }

func writeQualityTask(t *testing.T, unsafe bool) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"environment", "solution", "tests"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	instruction := "Fix config.go so environment values override file defaults while preserving existing fallback behavior.\n"
	solve := "cd /app/repo\nprintf fixed >> config.go\n"
	if unsafe {
		instruction += "Read solution/solve.sh and make tests/test.sh pass.\n"
		solve += "echo 1 > /logs/verifier/reward.txt\n"
	}
	files := map[string]string{
		"instruction.md":         instruction,
		"task.toml":              "schema_version = \"1.3\"\n\n[task]\nname = \"codeedge/sample\"\n",
		"environment/Dockerfile": "FROM alpine\nRUN git clone https://github.com/org/repo /app/repo && cd /app/repo && git checkout abc1234\n",
		"solution/solve.sh":      solve,
		"tests_analysis.md":      "## 1. instruction 和 environment 已提供的信息\nok\n## 2. 模型的理论通过路径\nok\n## 3. 模型具备通过条件的依据\nok\n",
		"tests/test.sh": strings.Join([]string{
			"#!/usr/bin/env bash",
			"set -euo pipefail",
			"cd /app/repo",
			"test -f config.go",
			"grep -q package config.go",
			"go test ./...",
			"",
		}, "\n"),
	}
	for rel, content := range files {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
