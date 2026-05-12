package pipeline_test

import (
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
	artifactRoot := filepath.Join(t.TempDir(), "run")
	scanPath := t.TempDir()
	writeStageAScript(t, scanPath, "run_acceptance.py", `import json, sys
args=sys.argv
json_path=args[args.index("--output-json")+1]
md_path=args[args.index("--output-md")+1]
open(json_path,"w").write(json.dumps({"blocking_issues":[],"non_blocking_issues":[]}))
open(md_path,"w").write("# Acceptance\n")
`)
	writeStageAScript(t, scanPath, "run_validate.py", `import sys
args=sys.argv
md_path=args[args.index("--output-md")+1]
open(md_path,"w").write("# Validate\n")
`)
	record := pipelinepkg.NewRunner(nil, configWithScanPath(scanPath)).StageAForTest(
		context.Background(),
		model.RunRecord{RunID: "run-1", TaskID: "TASK-1", ArtifactRoot: artifactRoot},
		scanner.Project{TaskID: "TASK-1", Path: root},
	)
	for _, name := range []string{"QA_validate_report.md", "QA_acceptance_report.md", "QA_trajectory_archive.png"} {
		if _, err := os.Stat(filepath.Join(artifactRoot, name)); err != nil {
			t.Fatalf("expected Stage A artifact %s: %v; record=%#v", name, err, record)
		}
	}
	if _, err := os.Stat(filepath.Join(artifactRoot, "QA_validation_report.md")); !os.IsNotExist(err) {
		t.Fatalf("old validation report name should not be emitted, stat err: %v", err)
	}
}

func TestSubmitArtifactNamesMatchInitialAndRecheckContracts(t *testing.T) {
	initial := strings.Join(pipelinepkg.SubmitArtifactNamesForTest("initial"), "\n")
	for _, want := range []string{
		"QA_codex_report.md",
		"QA_validate_report.md",
		"QA_operator_prompt_requirements_verification.md",
		"QA_operator_codex_report_issues_verification.md",
		"QA_test_effectiveness_report.md",
		"QA_docker_startup.png",
		"QA_run_tests_screenshot.png",
		"QA_trajectory_archive.png",
	} {
		if !strings.Contains(initial, want) {
			t.Fatalf("initial submit names missing %s:\n%s", want, initial)
		}
	}
	recheck := strings.Join(pipelinepkg.SubmitArtifactNamesForTest("recheck"), "\n")
	for _, want := range []string{
		"QA_codex_report_verification.md",
		"QA_prompt_requirements_verification.md",
		"QA_codex_report_issues_verification.md",
		"QA_test_effectiveness_verification.md",
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
