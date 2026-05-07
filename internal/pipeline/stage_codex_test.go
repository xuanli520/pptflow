package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/scanner"
	"github.com/xuanli520/p2r_tui/internal/taskdocs"
)

func TestCodexReviewPathPrefersScriptInputSnapshot(t *testing.T) {
	root := t.TempDir()
	projectPath := filepath.Join(root, "project")
	artifactRoot := filepath.Join(projectPath, "qa", "runs", "run-1")
	snapshot := filepath.Join(artifactRoot, "script_input_snapshot")
	if err := os.MkdirAll(snapshot, 0o755); err != nil {
		t.Fatal(err)
	}

	got := codexReviewPath(model.RunRecord{ArtifactRoot: artifactRoot}, projectPath)
	if got != snapshot {
		t.Fatalf("review path = %q, want snapshot %q", got, snapshot)
	}
}

func TestCodexReviewPathFallsBackToProjectPath(t *testing.T) {
	root := t.TempDir()
	projectPath := filepath.Join(root, "project")
	artifactRoot := filepath.Join(projectPath, "qa", "runs", "run-1")

	got := codexReviewPath(model.RunRecord{ArtifactRoot: artifactRoot}, projectPath)
	if got != projectPath {
		t.Fatalf("review path = %q, want project path %q", got, projectPath)
	}
}

func TestStageDCodexContextDoesNotRequireSelfTestReport(t *testing.T) {
	root := t.TempDir()
	projectPath := filepath.Join(root, "batch", "TASK-1")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectPath, "metadata.json"), []byte(`{"task_id":"TASK-1"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	runner := Runner{cfg: config.Default()}
	contextText, err := runner.codexContext(
		scanner.Project{TaskID: "TASK-1", Path: projectPath},
		RunOptions{},
		"D",
	)
	if err != nil {
		t.Fatalf("D context should not require self-test report: %v", err)
	}
	if strings.Contains(contextText, "self-test report unavailable") {
		t.Fatalf("D context should not mention missing self-test report: %s", contextText)
	}
	if !strings.Contains(contextText, "metadata.json") {
		t.Fatalf("D context should still include available metadata: %s", contextText)
	}
}

func TestCodexContextOnlyExposesUploadedDocsToStageF(t *testing.T) {
	root := t.TempDir()
	projectPath := filepath.Join(root, "batch", "TASK-1")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectPath, "metadata.json"), []byte(`{"task_id":"TASK-1","prompt":"build it"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	selfTest := filepath.Join(t.TempDir(), "标注员自测报告.md")
	if err := os.WriteFile(selfTest, []byte("SELF TEST CLAIM: checkout flow passes"), 0o644); err != nil {
		t.Fatal(err)
	}
	notes := filepath.Join(t.TempDir(), "补充说明.md")
	if err := os.WriteFile(notes, []byte("SUPPLEMENTAL CLAIM: admin flow exists"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.ScanPath = root
	cfg.Docs.StageInlineMaxBytes = 10
	if _, err := taskdocs.Attach(root, "TASK-1", selfTest, "self-test", "test", cfg.Docs); err != nil {
		t.Fatal(err)
	}
	if _, err := taskdocs.Attach(root, "TASK-1", notes, "notes", "test", cfg.Docs); err != nil {
		t.Fatal(err)
	}
	runner := Runner{cfg: cfg}
	project := scanner.Project{TaskID: "TASK-1", Path: projectPath}

	stageD, err := runner.codexContext(project, RunOptions{}, "D")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stageD, "SELF TEST CLAIM") || strings.Contains(stageD, "SUPPLEMENTAL CLAIM") || strings.Contains(stageD, "Uploaded/attached docs") {
		t.Fatalf("Stage D should not see uploaded docs:\n%s", stageD)
	}

	stageF, err := runner.codexContext(project, RunOptions{}, "F")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Stage F uploaded-document requirement",
		"Uploaded/attached docs available to Stage F: 2",
		"SELF TEST CLAIM",
		"SUPPLEMENTAL CLAIM",
	} {
		if !strings.Contains(stageF, want) {
			t.Fatalf("Stage F context missing %q:\n%s", want, stageF)
		}
	}
}

func TestStageCodexRecheckDoesNotEmitSelfTestCompatibilityAlias(t *testing.T) {
	root := t.TempDir()
	artifactRoot := filepath.Join(root, "TASK-1", "qa", "runs", "run-1")
	cfg := config.Default()
	cfg.Codex.PromptProfilesDir = filepath.Join(root, "missing-profiles")
	runner := Runner{cfg: cfg}

	record := runner.stageCodex(
		context.Background(),
		model.RunRecord{RunID: "run-1", TaskID: "TASK-1", ArtifactRoot: artifactRoot},
		scanner.Project{TaskID: "TASK-1", Path: filepath.Join(root, "batch", "TASK-1")},
		RunOptions{Mode: "recheck"},
		"D",
		"tests_coverage_report.md",
		"4_测试有效性报告_api端点真实性_确认修复报告.md",
	)

	deprecatedPath := filepath.Join(artifactRoot, "自测报告确认修复报告.md")
	if _, err := os.Stat(deprecatedPath); !os.IsNotExist(err) {
		t.Fatalf("deprecated self-test compatibility alias should not be emitted, stat err: %v", err)
	}
	for _, path := range record.ArtifactPaths {
		if filepath.Base(path) == "自测报告确认修复报告.md" {
			t.Fatalf("deprecated self-test compatibility alias should not be recorded: %#v", record.ArtifactPaths)
		}
	}
	if _, err := os.Stat(filepath.Join(artifactRoot, "4_测试有效性报告_api端点真实性_确认修复报告.md")); err != nil {
		t.Fatalf("canonical recheck report missing: %v", err)
	}
}
