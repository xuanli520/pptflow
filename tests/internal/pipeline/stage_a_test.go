package pipeline_test

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xuanli520/p2r_tui/internal/config"
	pipelinepkg "github.com/xuanli520/p2r_tui/internal/pipeline"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/projectlayout"
	"github.com/xuanli520/p2r_tui/internal/scanner"
)

func structuralFindings(project scanner.Project, required map[string]bool) []model.Finding {
	return pipelinepkg.StructuralFindingsForTest(project, required)
}

func TestStageAAcceptsAlternativeOriginalSessionMarkers(t *testing.T) {
	for _, marker := range []string{filepath.Join("docs", "original-session"), filepath.Join("docs", "original_sessions")} {
		root := writeStageAPackage(t, marker, `{"task_id":"TASK-1","prompt":"build it"}`)
		ok, _ := projectlayout.HasOriginalSessionMarker(root)
		required := map[string]bool{
			"docs":                    pathIsDir(filepath.Join(root, "docs")),
			"repo":                    pathIsDir(filepath.Join(root, "repo")),
			"original_session_marker": ok,
			"metadata.json":           pathIsFile(filepath.Join(root, "metadata.json")),
		}
		findings := structuralFindings(scanner.Project{TaskID: "TASK-1", Path: root}, required)
		if hasFindingTitle(findings, "Missing required delivery artifact: original_session_marker") {
			t.Fatalf("marker %s should not produce original session finding: %#v", marker, findings)
		}
	}
}

func TestStageAReportsMetadataTaskIDMismatch(t *testing.T) {
	root := writeStageAPackage(t, "original_sessions", `{"task_id":"TASK-METADATA","prompt":"build it"}`)
	required := map[string]bool{
		"docs":                    true,
		"repo":                    true,
		"original_session_marker": true,
		"metadata.json":           true,
	}
	findings := structuralFindings(scanner.Project{TaskID: "TASK-DIR", Path: root}, required)
	var mismatch model.Finding
	for _, finding := range findings {
		if strings.Contains(finding.Title, "task_id does not match") {
			mismatch = finding
			break
		}
	}
	if mismatch.Title == "" || mismatch.Severity != "Blocker" || !strings.Contains(mismatch.Evidence, "TASK-DIR") || !strings.Contains(mismatch.Evidence, "TASK-METADATA") {
		t.Fatalf("metadata mismatch finding not found or incomplete: %#v", findings)
	}
}

func TestStageAWritesRenamedReportsAndTrajectoryArchive(t *testing.T) {
	root := writeStageAPackage(t, "original_sessions", `{"task_id":"TASK-1","prompt":"build it"}`)
	writeStageAZip(t, filepath.Join(root, "original_sessions", "-home-purplevoid88-projects-TASK-20260327-D3040D.zip"), map[string]string{
		"docs/README.md":      "# docs",
		"original/prompt.txt": "build it",
		"repo/package.json":   "{}",
	})
	if err := os.MkdirAll(filepath.Join(root, "tmp-cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scratch.txt"), []byte("dirty"), 0o644); err != nil {
		t.Fatal(err)
	}
	artifactRoot := filepath.Join(t.TempDir(), "run")
	scanPath := t.TempDir()
	writeStageAScript(t, scanPath, "run_acceptance.py", `import json, sys
args=sys.argv
json_path=args[args.index("--output-json")+1]
md_path=args[args.index("--output-md")+1]
open(json_path,"w").write(json.dumps({"blocking_issues":[],"non_blocking_issues":[]}))
open(md_path,"w").write("# Acceptance\n")
`)
	writeStageAScript(t, scanPath, "run_validate.py", `import os, sys
args=sys.argv
root=args[1]
md_path=args[args.index("--output-md")+1]
for dirty in ("scratch.txt", "tmp-cache"):
    if os.path.exists(os.path.join(root, dirty)):
        raise SystemExit("validation root is dirty: " + dirty)
for required in ("docs", "repo", "original_sessions", "metadata.json"):
    if not os.path.exists(os.path.join(root, required)):
        raise SystemExit("validation root missing required entry: " + required)
open(md_path,"w").write("# Validation\n")
`)
	record := pipelinepkg.NewRunner(nil, configWithScanPath(scanPath)).StageAForTest(
		context.Background(),
		model.RunRecord{RunID: "run-1", TaskID: "TASK-1", ArtifactRoot: artifactRoot},
		scanner.Project{TaskID: "TASK-1", Path: root},
	)
	for _, name := range []string{"validation_report.md", "acceptance_report.md", "trajectory_archive.png"} {
		if _, err := os.Stat(filepath.Join(artifactRoot, name)); err != nil {
			t.Fatalf("expected Stage A artifact %s: %v; record=%#v", name, err, record)
		}
	}
	acceptanceReport := readStageAArtifact(t, artifactRoot, "acceptance_report.md")
	if strings.Contains(acceptanceReport, "Validation") || !strings.Contains(acceptanceReport, "Acceptance") {
		t.Fatalf("acceptance report should be produced by run_acceptance.py:\n%s", acceptanceReport)
	}
	validationReport := readStageAArtifact(t, artifactRoot, "validation_report.md")
	if strings.Contains(validationReport, "Acceptance") || !strings.Contains(validationReport, "Validation") {
		t.Fatalf("validation report should be produced by run_validate.py:\n%s", validationReport)
	}
	for _, original := range []string{"scratch.txt", "tmp-cache"} {
		if _, err := os.Stat(filepath.Join(root, original)); err != nil {
			t.Fatalf("validation cleanup should not mutate original package entry %s: %v", original, err)
		}
	}
}

func TestStageADoesNotUseAcceptanceOutputAsValidationReport(t *testing.T) {
	root := writeStageAPackage(t, "original_sessions", `{"task_id":"TASK-1","prompt":"build it"}`)
	writeStageAZip(t, filepath.Join(root, "original_sessions", "-home-purplevoid88-projects-TASK-20260327-D3040D.zip"), map[string]string{
		"repo/package.json": "{}",
	})
	artifactRoot := filepath.Join(t.TempDir(), "run")
	scanPath := t.TempDir()
	writeStageAScript(t, scanPath, "run_acceptance.py", `import json, os, sys
args=sys.argv
json_path=args[args.index("--output-json")+1]
md_path=args[args.index("--output-md")+1]
open(json_path,"w").write(json.dumps({"blocking_issues":[],"non_blocking_issues":[]}))
open(md_path,"w").write("# Acceptance\n")
open(os.path.join(os.path.dirname(md_path), "validation_report.md"),"w").write("# Acceptance masquerading as validation\n")
`)
	writeStageAScript(t, scanPath, "run_validate.py", `import sys
sys.exit(1)
`)

	record := pipelinepkg.NewRunner(nil, configWithScanPath(scanPath)).StageAForTest(
		context.Background(),
		model.RunRecord{RunID: "run-1", TaskID: "TASK-1", ArtifactRoot: artifactRoot},
		scanner.Project{TaskID: "TASK-1", Path: root},
	)
	if _, err := os.Stat(filepath.Join(artifactRoot, "validation_report.md")); !os.IsNotExist(err) {
		t.Fatalf("validation report must not be kept from run_acceptance.py; stat err: %v; record=%#v", err, record)
	}
	if !hasFindingTitle(record.Findings, "run_validate.py did not emit validation_report.md") {
		t.Fatalf("missing validation report finding not recorded: %#v", record.Findings)
	}
}

func TestStageATrajectoryArchiveSummarizesZipInternals(t *testing.T) {
	root := writeStageAPackage(t, "original_sessions", `{"task_id":"TASK-1","prompt":"build it"}`)
	writeStageAZip(t, filepath.Join(root, "original_sessions", "-home-purplevoid88-projects-TASK-20260327-D3040D.zip"), map[string]string{
		"docs/original-prompt.md": "# Prompt",
		"repo/package.json":       "{}",
		"repo/src/main.go":        "package main",
	})

	summary := pipelinepkg.PackageTrajectorySummaryForTest(root, root, nil)
	for _, want := range []string{
		"Stage A trajectory archive internal structure",
		"archive: original_sessions/-home-purplevoid88-projects-TASK-20260327-D3040D.zip",
		"repo/",
		"src/",
		"main.go",
		"docs/",
		"original-prompt.md",
	} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary missing %q:\n%s", want, summary)
		}
	}
	if strings.Contains(summary, "Top-level delivery structure") {
		t.Fatalf("summary should describe zip internals, not the package root:\n%s", summary)
	}
}

func TestSubmitArtifactNamesMatchInitialAndRecheckContracts(t *testing.T) {
	initial := strings.Join(pipelinepkg.SubmitArtifactNamesForTest("initial"), "\n")
	for _, want := range []string{
		"codex_report.md",
		"validation_report.md",
		"operator_prompt_requirements_verification.md",
		"operator_codex_report_issues_verification.md",
		"test_effectiveness_report.md",
		"docker_startup.png",
		"run_tests_screenshot.png",
		"trajectory_archive.png",
	} {
		if !strings.Contains(initial, want) {
			t.Fatalf("initial submit names missing %s:\n%s", want, initial)
		}
	}
	recheck := strings.Join(pipelinepkg.SubmitArtifactNamesForTest("recheck"), "\n")
	for _, want := range []string{
		"codex_report_verification.md",
		"validation_report.md",
		"prompt_requirements_verification.md",
		"codex_report_issues_verification.md",
		"test_effectiveness_verification.md",
	} {
		if !strings.Contains(recheck, want) {
			t.Fatalf("recheck submit names missing %s:\n%s", want, recheck)
		}
	}
}

func writeStageAPackage(t *testing.T, marker, metadata string) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"docs", "repo", marker} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "metadata.json"), []byte(metadata), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func configWithScanPath(scanPath string) config.Config {
	cfg := config.Default()
	cfg.ScanPath = scanPath
	cfg.Pipeline.StageTimeouts["A"] = 1
	return cfg
}

func writeStageAScript(t *testing.T, scanPath, name, content string) {
	t.Helper()
	dir := filepath.Join(scanPath, ".qa-control", "scripts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeStageAZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	writer := zip.NewWriter(file)
	defer writer.Close()
	for name, content := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
}

func readStageAArtifact(t *testing.T, artifactRoot, name string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(artifactRoot, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func pathIsDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func pathIsFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func hasFindingTitle(findings []model.Finding, title string) bool {
	for _, finding := range findings {
		if finding.Title == title {
			return true
		}
	}
	return false
}
