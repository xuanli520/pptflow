package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/scanner"
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
