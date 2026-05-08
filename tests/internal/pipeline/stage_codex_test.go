package pipeline_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	_ "unsafe"

	"github.com/xuanli520/p2r_tui/internal/config"
	pipelinepkg "github.com/xuanli520/p2r_tui/internal/pipeline"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/scanner"
	"github.com/xuanli520/p2r_tui/internal/taskdocs"
)

//go:linkname codexReviewPath github.com/xuanli520/p2r_tui/internal/pipeline.codexReviewPath
func codexReviewPath(run model.RunRecord, projectPath string) string

//go:linkname runnerCodexContext github.com/xuanli520/p2r_tui/internal/pipeline.Runner.codexContext
func runnerCodexContext(r pipelinepkg.Runner, ctx context.Context, project scanner.Project, opts pipelinepkg.RunOptions, stage string) (string, error)

//go:linkname runnerStageCodex github.com/xuanli520/p2r_tui/internal/pipeline.Runner.stageCodex
func runnerStageCodex(r pipelinepkg.Runner, ctx context.Context, run model.RunRecord, project scanner.Project, opts pipelinepkg.RunOptions, stage, profile, output string, compat ...string) model.StageRecord

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

	runner := pipelinepkg.NewRunner(nil, config.Default())
	contextText, err := runnerCodexContext(
		runner,
		context.Background(),
		scanner.Project{TaskID: "TASK-1", Path: projectPath},
		pipelinepkg.RunOptions{},
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
	runner := pipelinepkg.NewRunner(nil, cfg)
	project := scanner.Project{TaskID: "TASK-1", Path: projectPath}

	stageD, err := runnerCodexContext(runner, context.Background(), project, pipelinepkg.RunOptions{}, "D")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stageD, "SELF TEST CLAIM") || strings.Contains(stageD, "SUPPLEMENTAL CLAIM") || strings.Contains(stageD, "Uploaded/attached docs") {
		t.Fatalf("Stage D should not see uploaded docs:\n%s", stageD)
	}

	stageF, err := runnerCodexContext(runner, context.Background(), project, pipelinepkg.RunOptions{}, "F")
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
	runner := pipelinepkg.NewRunner(nil, cfg)

	record := runnerStageCodex(
		runner,
		context.Background(),
		model.RunRecord{RunID: "run-1", TaskID: "TASK-1", ArtifactRoot: artifactRoot},
		scanner.Project{TaskID: "TASK-1", Path: filepath.Join(root, "batch", "TASK-1")},
		pipelinepkg.RunOptions{Mode: "recheck"},
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

func TestStageCodexUnsafeExtraArgsProducesFindingAndContract(t *testing.T) {
	root := t.TempDir()
	fakeBin := filepath.Join(root, "bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(fakeBin, "codex"), fakeCodexAppServerScript())
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	profiles := filepath.Join(root, "profiles")
	if err := os.MkdirAll(profiles, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profiles, "tests_coverage_report.md"), []byte("profile"), 0o644); err != nil {
		t.Fatal(err)
	}

	projectPath := filepath.Join(root, "batch", "TASK-1")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	artifactRoot := filepath.Join(root, "TASK-1", "qa", "runs", "run-1")
	cfg := config.Default()
	cfg.Codex.PromptProfilesDir = profiles
	cfg.Codex.ExtraArgs = []string{"--search"}
	record := runnerStageCodex(
		pipelinepkg.NewRunner(nil, cfg),
		context.Background(),
		model.RunRecord{RunID: "run-1", TaskID: "TASK-1", ArtifactRoot: artifactRoot},
		scanner.Project{TaskID: "TASK-1", Path: projectPath},
		pipelinepkg.RunOptions{},
		"D",
		"tests_coverage_report.md",
		"tests_coverage_report.md",
	)

	if record.Status != model.StageFailed || record.ErrorSummary != "unsafe codex extra_args" {
		t.Fatalf("stage record = %#v, want unsafe extra_args failure", record)
	}
	if len(record.Findings) != 1 || !strings.Contains(record.Findings[0].Evidence, "--search") {
		t.Fatalf("expected structured extra_args finding, got %#v", record.Findings)
	}
	content, err := os.ReadFile(filepath.Join(artifactRoot, "tests_coverage_report.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Manual Verification Required", "<!-- p2r:static-review-json:start -->", `"stage": "D"`} {
		if !strings.Contains(string(content), want) {
			t.Fatalf("unavailable report missing %q:\n%s", want, content)
		}
	}
}
