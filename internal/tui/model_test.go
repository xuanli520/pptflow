package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/evidence"
)

func writeTUITestFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeTUITestScreenshot(t *testing.T, path, slot, model string, passCount int) {
	t.Helper()
	runs := make([]domain.TrialRun, domain.RequiredTrialCount)
	for i := range runs {
		runs[i] = domain.TrialRun{Trial: i + 1, Passed: i < passCount, Turns: 20 + i}
	}
	pngData, err := evidence.RenderPassAt4PNG(slot, domain.TrialResult{
		Model: model, Trials: domain.RequiredTrialCount, PassCount: passCount,
		PassAt4: float64(passCount) / domain.RequiredTrialCount, AverageTurns: 21.5, Runs: runs,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pngData, 0o644); err != nil {
		t.Fatal(err)
	}
}

func submitStartForm(m model) (tea.Model, tea.Cmd) {
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		return updated, cmd
	}
	next := updated.(model)
	if next.startStep != startStepAdvanced || next.err != nil {
		return updated, nil
	}
	return next.Update(tea.KeyMsg{Type: tea.KeyEnter})
}

func TestModelRendersRunnerEvents(t *testing.T) {
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{Workspace: "workspace", TaskDir: "task"})
	m.width = 100
	updated, _ := m.Update(runnerEventMsg(domain.RunnerEvent{
		Type:      "node_succeeded",
		NodeID:    "codeedge_lint",
		Status:    "succeeded",
		Message:   "lint passed",
		CreatedAt: time.Now(),
	}))
	rendered := updated.(model).View()
	if !strings.Contains(rendered, "codeedge_lint") || !strings.Contains(rendered, "lint passed") {
		t.Fatalf("rendered view missing event: %s", rendered)
	}
	if !strings.Contains(rendered, "运行控制") {
		t.Fatalf("TUI footer is missing the run control action: %s", rendered)
	}
}

func TestStartViewRendersWorkflowForm(t *testing.T) {
	m := initialStartModel(context.Background(), func() {}, app.RunnerOptions{})
	m.width = 100
	rendered := m.View()
	for _, want := range []string{"启动工作流", "基本配置", "高级选项", "运行已有任务", "任务路径", "工作区路径", "Enter 进入高级选项"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("start view missing %q: %s", want, rendered)
		}
	}
	if strings.Contains(rendered, "Harbor 超时") || strings.Contains(rendered, "质量检查") {
		t.Fatalf("basic step should not render advanced fields: %s", rendered)
	}
}

func TestStartViewRedactsSecretLikeInput(t *testing.T) {
	secret := "raw-start-secret"
	m := initialStartModel(context.Background(), func() {}, app.RunnerOptions{
		RepoURL:   "ssh://git:secret@github.com/org/repo",
		Workspace: "API_KEY=" + secret,
	})
	m.width = 100
	m.startMode = startGenerateTask
	rendered := m.View()
	if strings.Contains(rendered, secret) || strings.Contains(rendered, "git:secret@github") {
		t.Fatalf("start view leaked secret-like input: %s", rendered)
	}
	if !strings.Contains(rendered, "redacted") {
		t.Fatalf("start view missing redaction marker: %s", rendered)
	}
}

func TestStartViewRedactsValidationErrors(t *testing.T) {
	m := initialStartModel(context.Background(), func() {}, app.RunnerOptions{})
	m.width = 100
	m.err = errors.New("API_KEY=raw-start-error ssh://git:secret@github.com/org/repo")
	rendered := m.View()
	for _, leaked := range []string{"raw-start-error", "git:secret@github"} {
		if strings.Contains(rendered, leaked) {
			t.Fatalf("start view error leaked %q: %s", leaked, rendered)
		}
	}
	if !strings.Contains(rendered, "redacted") {
		t.Fatalf("start view error missing redaction marker: %s", rendered)
	}
}

func TestStartFormDoesNotExposeOrPreserveAutoApprove(t *testing.T) {
	m := initialStartModel(context.Background(), func() {}, app.RunnerOptions{
		Workspace:   t.TempDir(),
		TaskDir:     t.TempDir(),
		AutoApprove: true,
	})
	m.width = 100
	if rendered := m.View(); strings.Contains(rendered, "Auto approve") {
		t.Fatalf("start view should not expose auto approve: %s", rendered)
	}
	updated, cmd := submitStartForm(m)
	if cmd == nil {
		t.Fatal("valid start form should launch workflow")
	}
	if updated.(model).opts.AutoApprove {
		t.Fatalf("TUI start should force manual review gates: %+v", updated.(model).opts)
	}
}

func TestStartFormRejectsGenerateWithoutRepoCommit(t *testing.T) {
	m := initialStartModel(context.Background(), func() {}, app.RunnerOptions{})
	m.startMode = startGenerateTask
	updated, cmd := submitStartForm(m)
	if cmd != nil {
		t.Fatal("invalid start form should not launch workflow")
	}
	started := updated.(model)
	if started.runner != nil {
		t.Fatal("invalid start form created runner")
	}
	if started.err == nil || !strings.Contains(started.err.Error(), "仓库地址和提交哈希") {
		t.Fatalf("expected validation error, got %v", started.err)
	}
}

func TestStartFormLaunchesExistingTaskRunner(t *testing.T) {
	taskDir := t.TempDir()
	m := initialStartModel(context.Background(), func() {}, app.RunnerOptions{Workspace: t.TempDir()})
	m.opts.TaskDir = taskDir
	updated, cmd := submitStartForm(m)
	if cmd == nil {
		t.Fatal("valid start form should launch workflow")
	}
	started := updated.(model)
	if started.runner == nil {
		t.Fatal("valid start form did not create runner")
	}
	if started.view != viewOverview || started.opts.TaskDir != taskDir || started.opts.Generate {
		t.Fatalf("unexpected started model: view=%v opts=%+v", started.view, started.opts)
	}
}

func TestStartFormRejectsPackageWithoutEvidence(t *testing.T) {
	taskDir := t.TempDir()
	m := initialStartModel(context.Background(), func() {}, app.RunnerOptions{Workspace: t.TempDir(), Package: true})
	m.opts.TaskDir = taskDir
	updated, cmd := submitStartForm(m)
	if cmd != nil {
		t.Fatal("package form without evidence should not launch workflow")
	}
	blocked := updated.(model)
	if blocked.runner != nil {
		t.Fatal("invalid package evidence created runner")
	}
	if blocked.err == nil || !strings.Contains(blocked.err.Error(), "测试分析") {
		t.Fatalf("expected tests analysis validation error, got %v", blocked.err)
	}
}

func TestStartFormLaunchesPackageWithEvidenceOptions(t *testing.T) {
	taskDir := t.TempDir()
	analysis := filepath.Join(t.TempDir(), "tests_analysis.md")
	qwen := filepath.Join(t.TempDir(), "qwen.json")
	opus := filepath.Join(t.TempDir(), "opus.json")
	qwenShot := filepath.Join(t.TempDir(), "qwen.png")
	opusShot := filepath.Join(t.TempDir(), "opus.png")
	historyA := filepath.Join(t.TempDir(), "history-a")
	historyB := filepath.Join(t.TempDir(), "history-b")
	tb3 := filepath.Join(t.TempDir(), "tb3")
	for _, path := range []string{analysis, qwen, opus, qwenShot, opusShot} {
		if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{historyA, historyB, tb3} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	m := initialStartModel(context.Background(), func() {}, app.RunnerOptions{Workspace: t.TempDir(), Package: true})
	m.opts.TaskDir = taskDir
	m.opts.TestsAnalysis = analysis
	m.opts.QwenResult = qwen
	m.opts.OpusResult = opus
	m.opts.QwenScreenshot = qwenShot
	m.opts.OpusScreenshot = opusShot
	m.opts.SimilarityHistoryDirs = []string{historyA + "," + historyB}
	m.opts.SimilarityTB3Dirs = []string{tb3}
	updated, cmd := submitStartForm(m)
	if cmd == nil {
		t.Fatal("valid package evidence should launch workflow")
	}
	started := updated.(model)
	if started.runner == nil || !started.opts.Package {
		t.Fatalf("package form did not create runner: %+v", started)
	}
	if got := strings.Join(started.opts.SimilarityHistoryDirs, ","); got != historyA+","+historyB {
		t.Fatalf("history dirs not split correctly: %q", got)
	}
	if len(started.opts.SimilarityTB3Dirs) != 1 || started.opts.SimilarityTB3Dirs[0] != tb3 {
		t.Fatalf("tb3 dirs not preserved: %+v", started.opts.SimilarityTB3Dirs)
	}
	if started.opts.TestsAnalysis != analysis || started.opts.QwenResult != qwen || started.opts.OpusResult != opus || started.opts.QwenScreenshot != qwenShot || started.opts.OpusScreenshot != opusShot {
		t.Fatalf("evidence fields not preserved: %+v", started.opts)
	}
}

func TestStartFormLaunchesPackageWithRunHarborWithoutScreenshots(t *testing.T) {
	taskDir := t.TempDir()
	analysis := filepath.Join(t.TempDir(), "tests_analysis.md")
	if err := os.WriteFile(analysis, []byte("analysis"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := initialStartModel(context.Background(), func() {}, app.RunnerOptions{
		Workspace:             t.TempDir(),
		Package:               true,
		RunHarbor:             true,
		TaskDir:               taskDir,
		TestsAnalysis:         analysis,
		SimilarityHistoryDirs: []string{t.TempDir()},
	})
	updated, cmd := submitStartForm(m)
	if cmd == nil {
		t.Fatal("package with RunHarbor should launch without prefilled screenshots")
	}
	started := updated.(model)
	if started.runner == nil || !started.opts.RunHarbor || started.opts.QwenScreenshot != "" || started.opts.OpusScreenshot != "" {
		t.Fatalf("unexpected started model: %+v", started.opts)
	}
}

func TestStartFormLaunchesPackageWithResultScreenshotFallback(t *testing.T) {
	taskDir := t.TempDir()
	resultDir := t.TempDir()
	qwen := filepath.Join(resultDir, "qwen.json")
	opus := filepath.Join(resultDir, "opus.json")
	writeTUITestScreenshot(t, filepath.Join(resultDir, "qwen.png"), "harbor_run_qwen", "qwen3.7-max", 1)
	writeTUITestScreenshot(t, filepath.Join(resultDir, "opus.png"), "harbor_run_opus", "claude-opus-4-6", 3)
	if err := os.WriteFile(qwen, []byte(`{"model":"qwen3.7-max","screenshot":"qwen.png"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(opus, []byte(`{"model":"claude-opus-4-6","pass4_screenshot":"opus.png"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m := initialStartModel(context.Background(), func() {}, app.RunnerOptions{
		Workspace:             t.TempDir(),
		Package:               true,
		TaskDir:               taskDir,
		TestsAnalysis:         writeTUITestFile(t, "tests_analysis.md", "analysis"),
		QwenResult:            qwen,
		OpusResult:            opus,
		SimilarityHistoryDirs: []string{t.TempDir()},
	})
	updated, cmd := submitStartForm(m)
	if cmd == nil {
		t.Fatal("package should launch when result JSON declares screenshots")
	}
	started := updated.(model)
	if started.runner == nil || started.opts.QwenScreenshot != "" || started.opts.OpusScreenshot != "" {
		t.Fatalf("unexpected screenshot fallback handling: %+v", started.opts)
	}
}

func TestStartFormRejectsPackageWithoutScreenshotOrFallback(t *testing.T) {
	resultDir := t.TempDir()
	qwen := filepath.Join(resultDir, "qwen.json")
	opus := filepath.Join(resultDir, "opus.json")
	if err := os.WriteFile(qwen, []byte(`{"model":"qwen3.7-max"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(opus, []byte(`{"model":"claude-opus-4-6"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m := initialStartModel(context.Background(), func() {}, app.RunnerOptions{
		Workspace:             t.TempDir(),
		Package:               true,
		TaskDir:               t.TempDir(),
		TestsAnalysis:         writeTUITestFile(t, "tests_analysis.md", "analysis"),
		QwenResult:            qwen,
		OpusResult:            opus,
		SimilarityHistoryDirs: []string{t.TempDir()},
	})
	updated, cmd := submitStartForm(m)
	if cmd != nil {
		t.Fatal("package without screenshot evidence should not launch workflow")
	}
	blocked := updated.(model)
	if blocked.err == nil || !strings.Contains(blocked.err.Error(), "截图") {
		t.Fatalf("expected screenshot validation error, got %v", blocked.err)
	}
}

func TestStartFormRejectsUnreadablePackageEvidencePaths(t *testing.T) {
	m := initialStartModel(context.Background(), func() {}, app.RunnerOptions{
		Workspace:             t.TempDir(),
		Package:               true,
		TaskDir:               t.TempDir(),
		TestsAnalysis:         filepath.Join(t.TempDir(), "missing-tests-analysis.md"),
		QwenResult:            filepath.Join(t.TempDir(), "missing-qwen.json"),
		OpusResult:            filepath.Join(t.TempDir(), "missing-opus.json"),
		SimilarityHistoryDirs: []string{filepath.Join(t.TempDir(), "missing-history")},
	})
	updated, cmd := submitStartForm(m)
	if cmd != nil {
		t.Fatal("unreadable package evidence should not launch workflow")
	}
	blocked := updated.(model)
	if blocked.err == nil || !strings.Contains(blocked.err.Error(), "测试分析") {
		t.Fatalf("expected readable tests analysis validation error, got %v", blocked.err)
	}
}

func TestStartFormPreservesAdvancedOptions(t *testing.T) {
	m := initialStartModel(context.Background(), func() {}, app.RunnerOptions{
		Workspace:           t.TempDir(),
		TaskDir:             t.TempDir(),
		QualityAgent:        true,
		SimilarityThreshold: 0.37,
		HarborAgent:         "custom-agent",
		QwenModel:           "qwen-custom",
		OpusModel:           "opus-custom",
		QwenHarborBaseURL:   "https://qwen.example/v1",
		OpusHarborBaseURL:   "https://opus.example/v1",
		HarborTimeout:       123,
		HarborSetupTimeout:  321,
		HarborPreflight:     true,
		HarborConcurrency:   2,
		HarborAttempts:      4,
		HarborInfraRetries:  3,
		TaskName:            "sample-task",
		CodeLang:            "go",
		TaskType:            "bugfix",
		Application:         "cli",
		AHT:                 "45m",
		Description:         "Fix config loading",
		IsZeroToOne:         true,
		Model:               "gpt-5.3-codex",
		Reasoning:           "high",
		CodexPath:           "/usr/local/bin/codex",
		AgentTimeout:        77,
	})
	updated, cmd := submitStartForm(m)
	if cmd == nil {
		t.Fatal("valid advanced options should launch workflow")
	}
	opts := updated.(model).opts
	if !opts.QualityAgent || opts.SimilarityThreshold != 0.37 || opts.HarborAgent != "custom-agent" || opts.QwenModel != "qwen-custom" || opts.OpusModel != "opus-custom" || opts.QwenHarborBaseURL != "https://qwen.example/v1" || opts.OpusHarborBaseURL != "https://opus.example/v1" || opts.HarborTimeout != 123 || opts.HarborSetupTimeout != 321 || !opts.HarborPreflight || opts.HarborConcurrency != 2 || opts.HarborAttempts != 4 || opts.HarborInfraRetries != 3 || opts.TaskName != "sample-task" || opts.CodeLang != "go" || opts.TaskType != "bugfix" || opts.Application != "cli" || opts.AHT != "45m" || opts.Description != "Fix config loading" || !opts.IsZeroToOne || opts.Model != "gpt-5.3-codex" || opts.Reasoning != "high" || opts.CodexPath != "/usr/local/bin/codex" || opts.AgentTimeout != 77 {
		t.Fatalf("advanced options not preserved: %+v", opts)
	}
}

func TestModelRendersGateAndSubmitsApprove(t *testing.T) {
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{Workspace: "workspace", TaskDir: "task"})
	m.width = 100
	updated, _ := m.Update(runnerEventMsg(domain.RunnerEvent{
		Type:   "gate_requested",
		NodeID: "codeedge_lint",
		Status: "waiting",
		Gate: &domain.GateRequest{
			RequestID: "req-1",
			GateID:    "final_review",
			GateName:  "Final Review",
			Message:   "Review lint",
			Checklist: []domain.ChecklistItem{{ID: "check", Label: "Check passed", Passed: true, Critical: true}},
			Artifacts: []domain.ArtifactPreview{{Name: "lint_report.json", Path: "/tmp/lint_report.json", Content: "{\"passed\":true}"}},
			CreatedAt: time.Now(),
		},
		CreatedAt: time.Now(),
	}))
	gateModel := updated.(model)
	rendered := gateModel.View()
	if !strings.Contains(rendered, "最终发布") || !strings.Contains(rendered, "lint_report.json") {
		t.Fatalf("rendered gate missing content: %s", rendered)
	}
	approved, cmd := gateModel.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	if cmd == nil {
		t.Fatal("expected submit decision command")
	}
	if approved.(model).activeGate != nil {
		t.Fatal("gate should be cleared after approve")
	}
}

func TestGateViewBlocksApproveWithFailingCriticalCheck(t *testing.T) {
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{Workspace: "workspace", TaskDir: "task"})
	m.width = 100
	m.activeGate = &domain.GateRequest{
		RequestID: "req-1",
		GateID:    "final_review",
		GateName:  "Final Review",
		Message:   "Review lint",
		Checklist: []domain.ChecklistItem{{ID: "critical", Label: "blocking check", Critical: true, Passed: false}},
	}
	m.view = viewGate
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	blocked := updated.(model)
	if cmd != nil {
		t.Fatal("failing critical gate should not submit approve command")
	}
	if blocked.activeGate == nil || blocked.err == nil {
		t.Fatalf("gate should remain active with error: %+v", blocked)
	}
	rendered := blocked.View()
	if !strings.Contains(rendered, "无法批准") || !strings.Contains(rendered, "blocking check") {
		t.Fatalf("gate view missing blocking error: %s", rendered)
	}
}

func TestGateViewReloadsArtifactFromPath(t *testing.T) {
	workspace := t.TempDir()
	artifact := filepath.Join(workspace, "artifact.txt")
	if err := os.WriteFile(artifact, []byte("fresh content"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{Workspace: workspace, TaskDir: t.TempDir()})
	m.width = 100
	updated, _ := m.Update(runnerEventMsg(domain.RunnerEvent{
		Type:   "gate_requested",
		NodeID: "codeedge_lint",
		Status: "waiting",
		Gate: &domain.GateRequest{
			RequestID: "req-1",
			GateID:    "final_review",
			GateName:  "Final Review",
			Message:   "Review lint",
			Artifacts: []domain.ArtifactPreview{{Name: "artifact.txt", Path: artifact, Content: "stale content"}},
			CreatedAt: time.Now(),
		},
		CreatedAt: time.Now(),
	}))
	rendered := updated.(model).View()
	if !strings.Contains(rendered, "fresh content") || strings.Contains(rendered, "stale content") {
		t.Fatalf("gate view did not reload artifact from path: %s", rendered)
	}
}

func TestNodeDetailRendersSelectedNodeArtifactFile(t *testing.T) {
	workspace := t.TempDir()
	reportPath := filepath.Join(workspace, "phase2", "artifacts", "codeedge_lint", "lint_report.json")
	if err := os.MkdirAll(filepath.Dir(reportPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reportPath, []byte(`{"passed":true,"checks":[{"id":"ok"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{Workspace: workspace, TaskDir: "task"})
	m.width = 120
	m.height = 40
	updated, _ := m.Update(runnerEventMsg(domain.RunnerEvent{
		Type:      "node_succeeded",
		NodeID:    "codeedge_lint",
		Status:    "succeeded",
		Message:   "lint passed",
		Path:      reportPath,
		CreatedAt: time.Now(),
	}))
	detail, _ := updated.(model).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	rendered := detail.(model).View()
	if !strings.Contains(rendered, "lint_report.json") || !strings.Contains(rendered, `"passed":true`) {
		t.Fatalf("rendered detail missing artifact content: %s", rendered)
	}
}

func TestNodeDetailUsesActualGeneratedArtifactPaths(t *testing.T) {
	workspace := t.TempDir()
	paths := map[string]string{
		filepath.Join(workspace, "phase1", "artifacts", "repo_analyze", "repo_analysis.json"):         `{"schema_version":"harbor.repo_analysis.v1"}`,
		filepath.Join(workspace, "phase1", "artifacts", "generate_task_files", "task_files.json"):     `{"schema_version":"harbor.generated_task_files.v1"}`,
		filepath.Join(workspace, "phase1", "artifacts", "instruction_generate", "instruction.md"):     `Fix the task.`,
		filepath.Join(workspace, "phase1", "artifacts", "task_toml_generate", "task.toml"):            `schema_version = "1.3"`,
		filepath.Join(workspace, "phase1", "artifacts", "dockerfile_generate", "Dockerfile"):          `FROM ubuntu:24.04`,
		filepath.Join(workspace, "phase2", "artifacts", "solve_generate", "solve.sh"):                 `#!/usr/bin/env bash`,
		filepath.Join(workspace, "phase2", "artifacts", "test_generate", "test.sh"):                   `grep -q expected output.txt`,
		filepath.Join(workspace, "phase3", "artifacts", "tests_analysis", "tests_analysis.md"):        `## tests analysis`,
		filepath.Join(workspace, "phase2", "artifacts", "submission_lint", "lint_report.json"):        `{"passed":true}`,
		filepath.Join(workspace, "phase1", "artifacts", "generate_task_files", "gen_report.json"):     `{"schema_version":"harbor.gen_report.v1"}`,
		filepath.Join(workspace, "phase1", "artifacts", "task_design", "task_proposal.json"):          `{"schema_version":"harbor.task_proposal.v1"}`,
		filepath.Join(workspace, "phase0", "command_logs", "repo_prepare.json"):                       `[]`,
		filepath.Join(workspace, "phase0", "repo_prepared.json"):                                      `{"schema_version":"harbor.repo_prepared.v1"}`,
		filepath.Join(workspace, "phase1", "artifacts", "reviews", "task_review", "decision.json"):    `{"approved":true}`,
		filepath.Join(workspace, "phase1", "artifacts", "reviews", "content_review", "decision.json"): `{"approved":true}`,
		filepath.Join(workspace, "phase2", "artifacts", "reviews", "final_review", "decision.json"):   `{"approved":true}`,
		filepath.Join(workspace, "phase3", "artifacts", "reviews", "result_review", "decision.json"):  `{"approved":true}`,
		filepath.Join(workspace, "phase3", "artifacts", "harbor_run_qwen", "qwen_result.json"):        `{"model":"qwen3.7-max"}`,
		filepath.Join(workspace, "phase3", "artifacts", "harbor_run_qwen", "trial_result.json"):       `{"model":"qwen3.7-max"}`,
		filepath.Join(workspace, "phase3", "artifacts", "harbor_run_opus", "opus_result.json"):        `{"model":"opus"}`,
		filepath.Join(workspace, "phase3", "artifacts", "harbor_run_opus", "trial_result.json"):       `{"model":"opus"}`,
		filepath.Join(workspace, "phase2", "artifacts", "verify", "verify_report.json"):               `{"passed":true}`,
		filepath.Join(workspace, "phase2", "artifacts", "docker_build", "build_result.json"):          `{"name":"docker_build","passed":true}`,
		filepath.Join(workspace, "phase2", "artifacts", "initial_verify", "initial_result.json"):      `{"name":"initial_verify","passed":true}`,
		filepath.Join(workspace, "phase2", "artifacts", "oracle_verify", "oracle_result.json"):        `{"name":"oracle_verify","passed":true}`,
		filepath.Join(workspace, "phase2", "artifacts", "quality_check", "quality_report.json"):       `{"overall_pass":true}`,
		filepath.Join(workspace, "phase2", "artifacts", "similarity_check", "similarity_report.json"): `{"overall_pass":true}`,
		filepath.Join(workspace, "phase2", "artifacts", "codeedge_lint", "lint_report.json"):          `{"passed":true}`,
	}
	for path, content := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{Workspace: workspace, TaskDir: "task"})
	m.width = 120
	m.height = 40
	for _, nodeID := range []string{"repo_analyze", "task_review", "generate_task_files", "instruction_generate", "task_toml_generate", "dockerfile_generate", "solve_generate", "test_generate", "tests_analysis", "materialize_task", "docker_build", "initial_verify", "oracle_verify", "harbor_run_qwen", "harbor_run_opus", "submission_lint", "result_review"} {
		m.nodes[nodeID] = domain.RunnerEvent{NodeID: nodeID, Status: "succeeded", Message: "done"}
		m.selectedNode = nodeID
		m.selectedArtifact = 0
		rendered := m.nodeDetailView()
		if !strings.Contains(rendered, "工件") || strings.Contains(rendered, "内容为空或不可用") {
			t.Fatalf("node %s did not render artifact content: %s", nodeID, rendered)
		}
	}
}

func TestGateDecisionIncludesEditedFileSummary(t *testing.T) {
	artifact := filepath.Join(t.TempDir(), "artifact.txt")
	if err := os.WriteFile(artifact, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshotFile(artifact)
	if err := os.WriteFile(artifact, []byte("after content"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{Workspace: "workspace", TaskDir: "task"})
	m.activeGate = &domain.GateRequest{RequestID: "req-1", GateID: "final_review"}
	updated, _ := m.Update(editorDoneMsg{path: artifact, before: before, after: snapshotFile(artifact)})
	decision := updated.(model).makeGateDecision(true)
	if !decision.Approved || decision.EditedFiles == nil || !strings.Contains(decision.EditedFiles[artifact], "size 6->13") {
		t.Fatalf("decision missing edited file summary: %+v", decision)
	}
}

func TestFinalReviewCanSubmitCodexRepairAction(t *testing.T) {
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{Workspace: "workspace", TaskDir: "task"})
	m.activeGate = &domain.GateRequest{RequestID: "phase2:final_review", GateID: "final_review"}
	m.view = viewGate
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'v'}})
	if cmd == nil {
		t.Fatal("repair action did not submit a decision")
	}
	msg := cmd()
	written, ok := msg.(gateDecisionWrittenMsg)
	if !ok || written.decision.Action != "repair" || written.decision.Approved {
		t.Fatalf("unexpected repair decision: %#v", msg)
	}
	if updated.(model).activeGate != nil {
		t.Fatal("gate should wait for the rerun request after submitting revise")
	}
}

func TestFinalReviewSupportsAutomaticAndManualRepairModes(t *testing.T) {
	for key, action := range map[rune]string{'c': "repair_loop", 'u': "revise"} {
		m := initialModel(context.Background(), func() {}, app.RunnerOptions{Workspace: "workspace", TaskDir: "task"})
		m.activeGate = &domain.GateRequest{RequestID: "phase2:final_review", GateID: "final_review"}
		m.view = viewGate
		updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{key}})
		if cmd == nil {
			t.Fatalf("key %q did not submit %s", key, action)
		}
		written := cmd().(gateDecisionWrittenMsg)
		if written.decision.Action != action || updated.(model).activeGate != nil {
			t.Fatalf("key %q produced unexpected decision: %+v", key, written.decision)
		}
	}
}

func TestGateDecisionRedactsNotesAndEditedFiles(t *testing.T) {
	secret := "raw-gate-secret"
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{Workspace: "workspace", TaskDir: "task"})
	m.activeGate = &domain.GateRequest{RequestID: "req-1", GateID: "final_review"}
	m.gateNotes = "API_KEY=" + secret
	m.editedFiles = map[string]string{"artifact.txt": "Bearer " + secret}
	decision := m.makeGateDecision(true)
	if strings.Contains(decision.Notes, secret) || strings.Contains(decision.EditedFiles["artifact.txt"], secret) {
		t.Fatalf("decision leaked secret-like content: %+v", decision)
	}
	if !strings.Contains(decision.Notes, "redacted") || !strings.Contains(decision.EditedFiles["artifact.txt"], "redacted") {
		t.Fatalf("decision missing redaction marker: %+v", decision)
	}
}

func TestSubmitDecisionRejectsLegacyWorkspaceMutation(t *testing.T) {
	m := initialWorkspaceModel(context.Background(), func() {}, app.RunnerOptions{Workspace: t.TempDir()})
	msg := m.submitDecision(domain.GateDecision{RequestID: "req-1", GateID: "final_review", Approved: true}, nil)()
	written, ok := msg.(gateDecisionWrittenMsg)
	if !ok || !errors.Is(written.err, ErrLegacyWorkspaceTUIUnavailable) {
		t.Fatalf("legacy decision result = %#v, want lifecycle cutover error", msg)
	}
}

func TestSnapshotDecisionRejectsDirectWorkspaceWrite(t *testing.T) {
	workspace := t.TempDir()
	writeWorkspaceSnapshot(t, workspace, domain.RunSummary{RunID: "run-current", Workspace: workspace, Status: "running"}, []domain.RunnerEvent{
		{RunID: "run-current", Type: "gate_requested", NodeID: "final_review", Status: "waiting", Message: "review", Gate: &domain.GateRequest{RequestID: "req-1", GateID: "final_review", GateName: "Final Review", NodeID: "final_review"}},
	})
	m := initialWorkspaceModel(context.Background(), func() {}, app.RunnerOptions{Workspace: workspace})
	m.width = 120
	if m.activeGate == nil {
		t.Fatal("expected active snapshot gate")
	}
	decision := m.makeGateDecision(true)
	msg := m.submitDecision(decision, m.activeGate)()
	updated, _ := m.Update(msg)
	failed := updated.(model)
	if failed.activeGate == nil || !errors.Is(failed.err, ErrLegacyWorkspaceTUIUnavailable) {
		t.Fatalf("legacy decision write did not remain blocked: gate=%+v err=%v", failed.activeGate, failed.err)
	}
}

func TestReadPreviewRedactsSecrets(t *testing.T) {
	secret := "sk-" + "unitsecretvalue"
	path := filepath.Join(t.TempDir(), "artifact.txt")
	content := "API_KEY=raw-preview-secret\nBearer token.value\n" + secret + "\nhttps://user:pass@example.com/repo\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	preview := readPreview(path, 20000)
	for _, leaked := range []string{"raw-preview-secret", "token.value", secret, "user:pass@example"} {
		if strings.Contains(preview, leaked) {
			t.Fatalf("preview leaked %q: %s", leaked, preview)
		}
	}
	if !strings.Contains(preview, "redacted") {
		t.Fatalf("preview missing redaction marker: %s", preview)
	}
}

func TestGateViewClampsArtifactIndex(t *testing.T) {
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{Workspace: t.TempDir(), TaskDir: t.TempDir()})
	m.width = 120
	m.view = viewGate
	m.selectedArtifact = 42
	m.activeGate = &domain.GateRequest{
		RequestID: "req-1",
		GateID:    "final_review",
		GateName:  "Final Review",
		Artifacts: []domain.ArtifactPreview{{Name: "artifact.txt", Content: "preview content"}},
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("gate view panicked on out-of-range artifact index: %v", recovered)
		}
	}()
	rendered := m.View()
	if !strings.Contains(rendered, "preview content") {
		t.Fatalf("gate view did not render clamped artifact: %s", rendered)
	}
}

func TestGateEditRejectsArtifactOutsideAllowedRoots(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{Workspace: t.TempDir(), TaskDir: t.TempDir()})
	m.view = viewGate
	m.activeGate = &domain.GateRequest{
		RequestID: "req-1",
		GateID:    "final_review",
		GateName:  "Final Review",
		Artifacts: []domain.ArtifactPreview{{Name: "outside.txt", Path: outside}},
	}
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	if cmd != nil || updated.(model).err != nil {
		t.Fatalf("retired editor shortcut mutated state: cmd=%v err=%v", cmd, updated.(model).err)
	}
}

func TestGateEditRejectsNonArtifactInsideTaskDir(t *testing.T) {
	taskDir := t.TempDir()
	readme := filepath.Join(taskDir, "README.md")
	if err := os.WriteFile(readme, []byte("notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{Workspace: t.TempDir(), TaskDir: taskDir})
	m.view = viewGate
	m.activeGate = &domain.GateRequest{
		RequestID: "req-1",
		GateID:    "final_review",
		GateName:  "Final Review",
		Artifacts: []domain.ArtifactPreview{{Name: "README.md", Path: readme}},
	}
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	if cmd != nil || updated.(model).err != nil {
		t.Fatalf("retired editor shortcut mutated state: cmd=%v err=%v", cmd, updated.(model).err)
	}
}

func TestGateEditAllowsKnownGeneratedTaskFiles(t *testing.T) {
	taskDir := t.TempDir()
	instruction := filepath.Join(taskDir, "instruction.md")
	if err := os.WriteFile(instruction, []byte("do the task"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{Workspace: t.TempDir(), TaskDir: taskDir})
	m.view = viewGate
	m.activeGate = &domain.GateRequest{
		RequestID: "req-1",
		GateID:    "content_review",
		GateName:  "Content Review",
		Artifacts: []domain.ArtifactPreview{{Name: "instruction.md", Path: instruction}},
	}
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	if cmd != nil || updated.(model).err != nil {
		t.Fatalf("retired editor shortcut mutated state: cmd=%v err=%v", cmd, updated.(model).err)
	}
}

func TestGateEditRejectsWorkspaceGeneratedCopies(t *testing.T) {
	workspace := t.TempDir()
	instruction := filepath.Join(workspace, "phase1", "artifacts", "instruction_generate", "instruction.md")
	if err := os.MkdirAll(filepath.Dir(instruction), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(instruction, []byte("workspace copy"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{Workspace: workspace, TaskDir: t.TempDir()})
	m.view = viewGate
	m.activeGate = &domain.GateRequest{
		RequestID: "req-1",
		GateID:    "content_review",
		GateName:  "Content Review",
		Artifacts: []domain.ArtifactPreview{{Name: "instruction.md", Path: instruction}},
	}
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	if cmd != nil || updated.(model).err != nil {
		t.Fatalf("retired editor shortcut mutated state: cmd=%v err=%v", cmd, updated.(model).err)
	}
}

func TestNodeEditRejectsPollutedTaskDirArtifact(t *testing.T) {
	taskDir := t.TempDir()
	dotenv := filepath.Join(taskDir, ".env")
	if err := os.WriteFile(dotenv, []byte("TOKEN=do-not-edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{Workspace: t.TempDir(), TaskDir: taskDir})
	m.view = viewNodeDetail
	m.selectedNode = "codeedge_lint"
	m.nodes = map[string]domain.RunnerEvent{
		"codeedge_lint": {
			NodeID:    "codeedge_lint",
			Status:    "failed",
			Message:   "polluted event",
			Artifacts: []domain.ArtifactPreview{{Name: ".env", Path: dotenv}},
		},
	}
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	if cmd != nil || updated.(model).err != nil {
		t.Fatalf("retired editor shortcut mutated state: cmd=%v err=%v", cmd, updated.(model).err)
	}
}

func TestNodeArtifactsRejectOutsidePathsAndSymlinkEscapes(t *testing.T) {
	workspace := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(workspace, "inside.txt")
	if err := os.WriteFile(inside, []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(workspace, "link.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{Workspace: workspace, TaskDir: t.TempDir()})
	m.nodes = map[string]domain.RunnerEvent{
		"codeedge_lint": {
			NodeID: "codeedge_lint",
			Artifacts: []domain.ArtifactPreview{
				{Name: "outside.txt", Path: outside},
				{Name: "inside.txt", Path: inside},
				{Name: "link.txt", Path: link},
			},
		},
	}
	artifacts := m.nodeArtifacts("codeedge_lint")
	if len(artifacts) != 1 || !strings.Contains(artifacts[0].Content, "inside") {
		t.Fatalf("expected only safe workspace artifact, got %+v", artifacts)
	}
}

func TestLogsViewRendersEventLogFile(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "event_log.jsonl"), []byte(`{"message":"persisted event"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{Workspace: workspace, TaskDir: "task"})
	m.width = 120
	m.height = 40
	m.view = viewLogs
	rendered := m.View()
	if !strings.Contains(rendered, "event_log.jsonl") || !strings.Contains(rendered, "persisted event") {
		t.Fatalf("rendered logs missing persisted file: %s", rendered)
	}
}

func TestLogArtifactsIndexCommandAndHarborLogs(t *testing.T) {
	workspace := t.TempDir()
	files := map[string]string{
		filepath.Join(workspace, "event_log.jsonl"):                                                               `{"message":"persisted event"}` + "\n",
		filepath.Join(workspace, "phase0", "command_logs", "repo_prepare.json"):                                   `{"argv":["git","clone"]}`,
		filepath.Join(workspace, "phase2", "artifacts", "verify", "command_logs", "initial_verify", "stderr.txt"): "verify stderr",
		filepath.Join(workspace, "phase3", "artifacts", "harbor_run_qwen", "command_run.json"):                    `{"argv":["harbor","run"]}`,
		filepath.Join(workspace, "phase3", "artifacts", "harbor_run_qwen", "stderr.txt"):                          "harbor stderr",
	}
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{Workspace: workspace, TaskDir: "task"})
	artifacts := m.logArtifacts()
	for _, want := range []string{
		"event_log.jsonl",
		"phase0/command_logs/repo_prepare.json",
		"phase2/artifacts/verify/command_logs/initial_verify/stderr.txt",
		"phase3/artifacts/harbor_run_qwen/command_run.json",
		"phase3/artifacts/harbor_run_qwen/stderr.txt",
	} {
		if !artifactNamed(artifacts, want) {
			t.Fatalf("log artifacts missing %q: %+v", want, artifacts)
		}
	}
	m.width = 120
	m.height = 40
	m.view = viewLogs
	for i, artifact := range artifacts {
		if strings.HasSuffix(filepath.ToSlash(artifact.Name), "harbor_run_qwen/stderr.txt") {
			m.selectedLogFile = i
			break
		}
	}
	rendered := m.View()
	if !strings.Contains(rendered, "harbor stderr") || !strings.Contains(rendered, "harbor_run_qwen/stderr.txt") {
		t.Fatalf("logs view missing selected harbor stderr: %s", rendered)
	}
}

func TestLogArtifactsRejectSymlinkEscapes(t *testing.T) {
	workspace := t.TempDir()
	outside := filepath.Join(t.TempDir(), "stderr.txt")
	if err := os.WriteFile(outside, []byte("outside secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(workspace, "phase2", "artifacts", "verify", "command_logs", "initial_verify", "stderr.txt")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{Workspace: workspace, TaskDir: "task"})
	for _, artifact := range m.logArtifacts() {
		if strings.Contains(artifact.Content, "outside secret") || artifact.Path == outside {
			t.Fatalf("log artifacts followed symlink escape: %+v", artifact)
		}
	}
}

func TestLogsViewTailShowsLargeFileEndAndRedacts(t *testing.T) {
	workspace := t.TempDir()
	stderrPath := filepath.Join(workspace, "phase3", "artifacts", "harbor_run_qwen", "stderr.txt")
	if err := os.MkdirAll(filepath.Dir(stderrPath), 0o755); err != nil {
		t.Fatal(err)
	}
	secret := "raw-tail-secret"
	content := strings.Repeat("prefix noise line\n", 6000) + "API_KEY=" + secret + "\nFINAL_HARBOR_ERROR_AT_TAIL\n"
	if err := os.WriteFile(stderrPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{Workspace: workspace, TaskDir: "task"})
	m.width = 120
	m.height = 40
	m.view = viewLogs
	m.selectedLogFile = selectedLogIndex(t, m.logArtifacts(), "phase3/artifacts/harbor_run_qwen/stderr.txt")

	rendered := m.View()
	if strings.Contains(rendered, "FINAL_HARBOR_ERROR_AT_TAIL") {
		t.Fatalf("tail-only error should not appear in initial top preview: %s", rendered)
	}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	tail := updated.(model)
	rendered = tail.View()
	if !strings.Contains(rendered, "FINAL_HARBOR_ERROR_AT_TAIL") || !strings.Contains(rendered, "[尾部跟踪]") {
		t.Fatalf("tail view missing final error: %s", rendered)
	}
	if strings.Contains(rendered, secret) || !strings.Contains(rendered, "redacted") {
		t.Fatalf("tail view did not redact secret: %s", rendered)
	}
}

func TestLogsViewScrollsSelectedFile(t *testing.T) {
	workspace := t.TempDir()
	stderrPath := filepath.Join(workspace, "phase2", "artifacts", "verify", "command_logs", "initial_verify", "stderr.txt")
	if err := os.MkdirAll(filepath.Dir(stderrPath), 0o755); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for i := 0; i < 40; i++ {
		lines = append(lines, fmt.Sprintf("scroll-line-%02d", i))
	}
	if err := os.WriteFile(stderrPath, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{Workspace: workspace, TaskDir: "task"})
	m.width = 120
	m.height = 32
	m.view = viewLogs
	m.selectedLogFile = selectedLogIndex(t, m.logArtifacts(), "phase2/artifacts/verify/command_logs/initial_verify/stderr.txt")

	initial := m.View()
	if !strings.Contains(initial, "scroll-line-00") || strings.Contains(initial, "scroll-line-10") {
		t.Fatalf("initial log window unexpected: %s", initial)
	}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	scrolled := updated.(model)
	rendered := scrolled.View()
	if !strings.Contains(rendered, "scroll-line-07") || strings.Contains(rendered, "scroll-line-00") || !strings.Contains(rendered, "上方还有") {
		t.Fatalf("paged log window did not scroll: %s", rendered)
	}
}

func TestInitialWorkspaceModelLoadsStateAndFiltersRunEvents(t *testing.T) {
	workspace := t.TempDir()
	state := domain.RunSummary{
		RunID:      "run-current",
		Workspace:  workspace,
		Status:     "failed",
		Passed:     false,
		FinishedAt: time.Now(),
	}
	stateRaw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "state.json"), stateRaw, 0o644); err != nil {
		t.Fatal(err)
	}
	eventLog := strings.Join([]string{
		`{"run_id":"run-old","type":"node_succeeded","node_id":"repo_prepare","status":"succeeded","message":"old"}`,
		`{"run_id":"run-current","type":"node_failed","node_id":"codeedge_lint","status":"failed","message":"current failure"}`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(workspace, "event_log.jsonl"), []byte(eventLog), 0o644); err != nil {
		t.Fatal(err)
	}
	m := initialWorkspaceModel(context.Background(), func() {}, app.RunnerOptions{Workspace: workspace})
	m.width = 120
	m.view = viewLogs
	rendered := m.View()
	if !m.done || len(m.events) != 1 || !strings.Contains(rendered, "current failure") || strings.Contains(rendered, "old") {
		t.Fatalf("workspace model did not load/filter state correctly: done=%v events=%+v rendered=%s", m.done, m.events, rendered)
	}
}

func TestWorkspaceModelRefreshesStateAndActiveGate(t *testing.T) {
	workspace := t.TempDir()
	writeWorkspaceSnapshot(t, workspace, domain.RunSummary{RunID: "run-current", Workspace: workspace, Status: "running"}, []domain.RunnerEvent{
		{RunID: "run-current", Type: "node_started", NodeID: "codeedge_lint", Status: "running", Message: "lint running"},
	})
	m := initialWorkspaceModel(context.Background(), func() {}, app.RunnerOptions{Workspace: workspace})
	if m.activeGate != nil || m.selectedNode != "codeedge_lint" {
		t.Fatalf("unexpected initial workspace model: gate=%+v selected=%s", m.activeGate, m.selectedNode)
	}

	writeWorkspaceSnapshot(t, workspace, domain.RunSummary{RunID: "run-current", Workspace: workspace, Status: "running"}, []domain.RunnerEvent{
		{RunID: "run-current", Type: "node_succeeded", NodeID: "codeedge_lint", Status: "succeeded", Message: "lint passed"},
		{RunID: "run-current", Type: "gate_requested", NodeID: "result_review", Status: "waiting", Message: "review", Gate: &domain.GateRequest{RequestID: "req-1", GateID: "result_review", GateName: "Result Review"}},
	})
	summary, events := loadWorkspaceState(workspace)
	updated, _ := m.Update(workspaceRefreshMsg{summary: summary, events: events})
	refreshed := updated.(model)
	if refreshed.activeGate == nil || refreshed.activeGate.GateID != "result_review" {
		t.Fatalf("refresh did not restore active gate: %+v", refreshed.activeGate)
	}
	if refreshed.view != viewGate {
		t.Fatalf("refresh should switch to gate view, got %v", refreshed.view)
	}
	if refreshed.nodes["codeedge_lint"].Status != "succeeded" || refreshed.nodes["result_review"].Status != "waiting" {
		t.Fatalf("refresh did not update node events: %+v", refreshed.nodes)
	}
}

func TestWorkspaceSnapshotGateSwitchResetsLocalReviewState(t *testing.T) {
	workspace := t.TempDir()
	writeWorkspaceSnapshot(t, workspace, domain.RunSummary{RunID: "run-current", Workspace: workspace, Status: "running"}, []domain.RunnerEvent{
		{RunID: "run-current", Type: "gate_requested", NodeID: "final_review", Status: "waiting", Message: "review", Gate: &domain.GateRequest{RequestID: "req-1", GateID: "final_review", GateName: "Final Review", NodeID: "final_review"}},
	})
	m := initialWorkspaceModel(context.Background(), func() {}, app.RunnerOptions{Workspace: workspace})
	m.gateNotes = "old notes"
	m.gateEditingNote = true
	m.editedFiles = map[string]string{"old": "size 1->2"}
	m.selectedArtifact = 7

	writeWorkspaceSnapshot(t, workspace, domain.RunSummary{RunID: "run-current", Workspace: workspace, Status: "running"}, []domain.RunnerEvent{
		{RunID: "run-current", Type: "gate_requested", NodeID: "result_review", Status: "waiting", Message: "review", Gate: &domain.GateRequest{RequestID: "req-2", GateID: "result_review", GateName: "Result Review", NodeID: "result_review"}},
	})
	summary, events := loadWorkspaceState(workspace)
	updated, _ := m.Update(workspaceRefreshMsg{summary: summary, events: events})
	refreshed := updated.(model)
	if refreshed.activeGate == nil || refreshed.activeGate.RequestID != "req-2" {
		t.Fatalf("expected new active gate: %+v", refreshed.activeGate)
	}
	if refreshed.gateNotes != "" || refreshed.gateEditingNote || len(refreshed.editedFiles) != 0 || refreshed.selectedArtifact != 0 {
		t.Fatalf("gate-local state was not reset: notes=%q editing=%v files=%+v artifact=%d", refreshed.gateNotes, refreshed.gateEditingNote, refreshed.editedFiles, refreshed.selectedArtifact)
	}
}

func TestWorkspaceModelDoesNotReviveCompletedGate(t *testing.T) {
	workspace := t.TempDir()
	writeWorkspaceSnapshot(t, workspace, domain.RunSummary{
		RunID:     "run-current",
		Workspace: workspace,
		Status:    "running",
		GateDecisions: []domain.GateDecision{
			{RequestID: "req-1", GateID: "result_review", Approved: true},
		},
	}, []domain.RunnerEvent{
		{RunID: "run-current", Type: "gate_requested", NodeID: "result_review", Status: "waiting", Message: "review", Gate: &domain.GateRequest{RequestID: "req-1", GateID: "result_review", GateName: "Result Review"}},
		{RunID: "run-current", Type: "node_succeeded", NodeID: "result_review", Status: "succeeded", Message: "gate approved"},
	})
	m := initialWorkspaceModel(context.Background(), func() {}, app.RunnerOptions{Workspace: workspace})
	if m.activeGate != nil {
		t.Fatalf("completed gate should not be active: %+v", m.activeGate)
	}
	if m.view == viewGate {
		t.Fatalf("completed gate should not open gate view")
	}
}

func TestWorkspaceModelKeepsNewRequestWithSameGateIDActive(t *testing.T) {
	workspace := t.TempDir()
	writeWorkspaceSnapshot(t, workspace, domain.RunSummary{
		RunID:     "run-current",
		Workspace: workspace,
		Status:    "running",
		GateDecisions: []domain.GateDecision{
			{RequestID: "req-old", GateID: "result_review", Approved: true},
		},
	}, []domain.RunnerEvent{
		{RunID: "run-current", Type: "gate_requested", NodeID: "result_review", Status: "waiting", Message: "review", Gate: &domain.GateRequest{RequestID: "req-new", GateID: "result_review", GateName: "Result Review", NodeID: "result_review"}},
	})
	m := initialWorkspaceModel(context.Background(), func() {}, app.RunnerOptions{Workspace: workspace})
	if m.activeGate == nil || m.activeGate.RequestID != "req-new" {
		t.Fatalf("new request should remain active despite old decision: %+v", m.activeGate)
	}
}

func TestRunnerEventClearsActiveGateOnTerminalNode(t *testing.T) {
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{Workspace: "workspace", TaskDir: "task"})
	m.width = 100
	updated, _ := m.Update(runnerEventMsg(domain.RunnerEvent{
		Type:   "gate_requested",
		NodeID: "final_review",
		Status: "waiting",
		Gate:   &domain.GateRequest{RequestID: "req-1", GateID: "final_review", GateName: "Final Review", NodeID: "final_review"},
	}))
	gateModel := updated.(model)
	if gateModel.activeGate == nil || gateModel.view != viewGate {
		t.Fatalf("expected active gate after request: %+v", gateModel.activeGate)
	}
	updated, _ = gateModel.Update(runnerEventMsg(domain.RunnerEvent{
		Type:   "node_succeeded",
		NodeID: "final_review",
		Status: "succeeded",
	}))
	cleared := updated.(model)
	if cleared.activeGate != nil || cleared.view == viewGate {
		t.Fatalf("terminal event should clear active gate: gate=%+v view=%v", cleared.activeGate, cleared.view)
	}
}

func TestGateDecisionWriteFailureRestoresGate(t *testing.T) {
	gate := &domain.GateRequest{RequestID: "req-1", GateID: "final_review", GateName: "Final Review", NodeID: "final_review"}
	m := initialWorkspaceModel(context.Background(), func() {}, app.RunnerOptions{Workspace: t.TempDir()})
	m.view = viewOverview
	updated, _ := m.Update(gateDecisionWrittenMsg{gate: gate, err: errors.New("write failed")})
	failed := updated.(model)
	if failed.activeGate == nil || failed.activeGate.RequestID != "req-1" || failed.view != viewGate {
		t.Fatalf("failed decision write should restore gate: %+v view=%v", failed.activeGate, failed.view)
	}
}

func TestEditorErrorRendersInDetailAndLogs(t *testing.T) {
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{Workspace: "workspace", TaskDir: "task"})
	m.width = 100
	m.view = viewNodeDetail
	updated, _ := m.Update(editorDoneMsg{path: "instruction.md", err: errors.New("editor failed")})
	failed := updated.(model)
	if failed.err == nil {
		t.Fatal("editor error should be stored on model")
	}
	if rendered := failed.View(); !strings.Contains(rendered, "editor failed") {
		t.Fatalf("node detail view missing editor error: %s", rendered)
	}
	failed.view = viewLogs
	if rendered := failed.View(); !strings.Contains(rendered, "editor failed") {
		t.Fatalf("logs view missing editor error: %s", rendered)
	}
}

func TestDoneViewShowsLastNodeFailure(t *testing.T) {
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{Workspace: "workspace", TaskDir: "task"})
	m.width = 120
	m.view = viewDone
	m.done = true
	m.summary = domain.RunSummary{
		Passed: false,
		Events: []domain.RunnerEvent{
			{Type: "node_failed", NodeID: "package", Status: "failed", Message: "package evidence missing", Path: "/tmp/package"},
			{Type: "run_failed", Status: "failed", Message: "run failed"},
		},
	}
	rendered := m.View()
	if !strings.Contains(rendered, "最后失败：package：package evidence missing") || !strings.Contains(rendered, "/tmp/package") {
		t.Fatalf("done view missing last node failure: %s", rendered)
	}
}

func writeWorkspaceSnapshot(t *testing.T, workspace string, summary domain.RunSummary, events []domain.RunnerEvent) {
	t.Helper()
	stateRaw, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "state.json"), stateRaw, 0o644); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, event := range events {
		raw, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(raw))
	}
	if err := os.WriteFile(filepath.Join(workspace, "event_log.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func artifactNamed(artifacts []domain.ArtifactPreview, name string) bool {
	name = filepath.ToSlash(name)
	for _, artifact := range artifacts {
		if filepath.ToSlash(artifact.Name) == name {
			return true
		}
	}
	return false
}

func selectedLogIndex(t *testing.T, artifacts []domain.ArtifactPreview, name string) int {
	t.Helper()
	name = filepath.ToSlash(name)
	for i, artifact := range artifacts {
		if filepath.ToSlash(artifact.Name) == name {
			return i
		}
	}
	t.Fatalf("log artifact %q not found in %+v", name, artifacts)
	return 0
}
