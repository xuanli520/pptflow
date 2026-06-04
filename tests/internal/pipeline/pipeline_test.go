package pipeline_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xuanli520/p2r_tui/internal/codex"
	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/db"
	"github.com/xuanli520/p2r_tui/internal/executor"
	pipelinepkg "github.com/xuanli520/p2r_tui/internal/pipeline"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/scanner"
	"github.com/xuanli520/p2r_tui/internal/taskdocs"
)

type portMapping = pipelinepkg.TestPortMapping

func selectedStages(opts pipelinepkg.RunOptions, staticOnly bool) map[string]bool {
	return pipelinepkg.SelectedStagesForTest(opts, staticOnly)
}

func assignFindingIDs(stage string, findings []model.Finding) []model.Finding {
	return pipelinepkg.AssignFindingIDsForTest(stage, findings)
}

func shortComment(stageStatuses map[string]string, findings []model.Finding) string {
	return pipelinepkg.ShortCommentForTest(stageStatuses, findings)
}

func splitStageFCodexReport(report string) (string, string) {
	return pipelinepkg.SplitStageFCodexReportForTest(report)
}

func readmeComposeCommand(repoPath string) []string {
	return pipelinepkg.ReadmeComposeCommandForTest(repoPath)
}

func extractFindingsFromReport(stage, report, sourcePath string) []model.Finding {
	return pipelinepkg.ExtractFindingsFromReportForTest(stage, report, sourcePath)
}

func staticReviewFindingsFromReport(stage, report, sourcePath string) ([]model.Finding, error) {
	return pipelinepkg.StaticReviewFindingsFromReportForTest(stage, report, sourcePath)
}

func normalizeStaticReviewReport(report string) (string, error) {
	return pipelinepkg.NormalizeStaticReviewReportForTest(report)
}

func truncateStaticReviewReport(report string, limit int) string {
	return pipelinepkg.TruncateStaticReviewReportForTest(report, limit)
}

func staticUnavailableReport(stage, profile, projectPath, reason string) string {
	return pipelinepkg.StaticUnavailableReportForTest(stage, profile, projectPath, reason)
}

func acceptanceFindings(path string) []model.Finding {
	return pipelinepkg.AcceptanceFindingsForTest(path)
}

func acceptanceScriptArgs(outputs map[string]string, projectTypeArgs []string) []string {
	return pipelinepkg.AcceptanceScriptArgsForTest(outputs, projectTypeArgs)
}

func validationScriptArgs(outputs map[string]string, projectTypeArgs []string) []string {
	return pipelinepkg.ValidationScriptArgsForTest(outputs, projectTypeArgs)
}

func runArtifactRoot(scanPath string, project scanner.Project, runID string) string {
	return pipelinepkg.RunArtifactRootForTest(scanPath, project, runID)
}

func copyPackageSnapshot(source, dest string) error {
	return pipelinepkg.CopyPackageSnapshotForTest(source, dest)
}

func terminalScreenshotLines(text string) []string {
	return pipelinepkg.TerminalScreenshotLinesForTest(text)
}

func safeCodexExtraArgs(args []string) ([]string, error) {
	return pipelinepkg.SafeCodexExtraArgsForTest(args)
}

func capabilitySummary(capability codex.Capability) string {
	return pipelinepkg.CapabilitySummaryForTest(capability)
}

func runCodexReviewSessionWithGuidance(ctx context.Context, session pipelinepkg.CodexReviewSession, request pipelinepkg.CodexReviewRequest, deadlines []pipelinepkg.CodexGuidanceDeadline) (pipelinepkg.CodexReviewResult, error) {
	return pipelinepkg.RunCodexReviewSessionWithGuidanceForTest(ctx, session, request, deadlines)
}

func codexGuidanceSchedule(timeout time.Duration, stage string) []pipelinepkg.CodexGuidanceDeadline {
	return pipelinepkg.CodexGuidanceScheduleForTest(timeout, stage)
}

func parseComposePS(raw string) (map[string][]portMapping, []string) {
	return pipelinepkg.ParseComposePSForTest(raw)
}

func parseDockerPort(service, raw string) []portMapping {
	return pipelinepkg.ParseDockerPortForTest(service, raw)
}

func TestSelectedStagesStaticOnly(t *testing.T) {
	selected := selectedStages(pipelinepkg.RunOptions{StaticOnly: true}, true)
	for _, stage := range []string{"A", "D", "E", "F"} {
		if !selected[stage] {
			t.Fatalf("expected %s selected", stage)
		}
	}
	for _, stage := range []string{"B", "G", "C"} {
		if selected[stage] {
			t.Fatalf("expected %s skipped", stage)
		}
	}
}

func TestSelectedStagesDefaultIncludesStageE(t *testing.T) {
	selected := selectedStages(pipelinepkg.RunOptions{}, false)
	for _, stage := range model.AllStages() {
		if !selected[stage] {
			t.Fatalf("expected %s selected", stage)
		}
	}
}

func TestSplitStageFCodexReportUsesAnyReport2Line(t *testing.T) {
	report1, report2 := splitStageFCodexReport(`# Repair Summary

Report 1 body.

This line contains Report 2: repair verification issues

Report 2 body.
`)
	if !strings.Contains(report1, "Report 1 body.") || strings.Contains(report1, "Report 2 body.") {
		t.Fatalf("unexpected report 1 split:\n%s", report1)
	}
	if !strings.Contains(report2, "This line contains Report 2") || !strings.Contains(report2, "Report 2 body.") {
		t.Fatalf("unexpected report 2 split:\n%s", report2)
	}
}

func TestSplitStageFCodexReportMarkerPreferred(t *testing.T) {
	result := pipelinepkg.SplitStageFCodexReportFullForTest(`# Repair Summary

## Report 1: Repair Verification Requirements and Fit
Requirement mapping content here.

<!-- p2r:report-split -->

## Report 2: Repair Verification Issues
Issue list content here.
`)
	if result.Kind != "marker" {
		t.Fatalf("expected marker split, got %s", result.Kind)
	}
	if !strings.Contains(result.Report1, "Requirement mapping content") || strings.Contains(result.Report1, "Issue list content") {
		t.Fatalf("marker split report1 incorrect:\n%s", result.Report1)
	}
	if !strings.Contains(result.Report2, "## Report 2: Repair Verification Issues") || !strings.Contains(result.Report2, "Issue list content") {
		t.Fatalf("marker split report2 incorrect:\n%s", result.Report2)
	}
}

func TestSplitStageFCodexReportHeadingPreferredOverLine(t *testing.T) {
	result := pipelinepkg.SplitStageFCodexReportFullForTest(`# Repair Summary

## Report 1 content here.
Some body text that mentions Report 2 in passing.

## Report 2: Repair Verification Issues
Issue list content here.
`)
	if result.Kind != "heading" {
		t.Fatalf("expected heading split, got %s", result.Kind)
	}
	if !strings.Contains(result.Report1, "mentions Report 2 in passing") || strings.Contains(result.Report1, "Issue list content") {
		t.Fatalf("heading split report1 incorrect:\n%s", result.Report1)
	}
	if !strings.Contains(result.Report2, "## Report 2: Repair Verification Issues") {
		t.Fatalf("heading split report2 should start at heading:\n%s", result.Report2)
	}
}

func TestSplitStageFCodexReportNoMatch(t *testing.T) {
	result := pipelinepkg.SplitStageFCodexReportFullForTest(`# Repair Summary

Some content here.

More content about repairs.

Final conclusion.
`)
	if result.Kind != "none" {
		t.Fatalf("expected none split, got %s", result.Kind)
	}
	if result.Report1 == "" {
		t.Fatalf("report1 should contain full content when no split found")
	}
	if result.Report2 != "" {
		t.Fatalf("report2 should be empty when no split found, got:\n%s", result.Report2)
	}
}

func TestValidateStageFSplitNoMatchEmitsFinding(t *testing.T) {
	result := pipelinepkg.SplitStageFCodexReportFullForTest("# Report\nContent only.\n")
	findings := pipelinepkg.ValidateStageFSplitForTest(result, "# Report\nContent only.\n")
	if len(findings) == 0 {
		t.Fatalf("expected at least one finding for no-match split")
	}
	if findings[0].Severity != "Medium" {
		t.Fatalf("expected Medium severity for no-match split, got %s", findings[0].Severity)
	}
}

func TestValidateStageFSplitOverlappingContentEmitsFinding(t *testing.T) {
	shared := strings.Repeat("This is a shared report with substantial overlapping content across both segments. ", 10)
	split := pipelinepkg.StageFSplitResultForTest{
		Report1: shared,
		Report2: shared + "\nResolved: issue fixed.",
		Kind:    "marker",
	}
	report := shared + "\n<!-- p2r:report-split -->\n" + shared + "\nResolved: issue fixed."
	findings := pipelinepkg.ValidateStageFSplitForTest(split, report)
	hasOverlap := false
	for _, f := range findings {
		if strings.Contains(f.Title, "overlapping") {
			hasOverlap = true
		}
	}
	if !hasOverlap {
		t.Fatalf("expected overlapping content finding, got: %#v", findings)
	}
}

func TestValidateStageFSplitLineKindEmitsLowFinding(t *testing.T) {
	split := pipelinepkg.StageFSplitResultForTest{
		Report1: "Report 1 distinct content with unique patterns abcdef.",
		Report2: "Report 2 distinct content with different patterns xyz123.",
		Kind:    "line",
	}
	findings := pipelinepkg.ValidateStageFSplitForTest(split, "Report 1 distinct content with unique patterns abcdef.\nThis line mentions Report 2 boundary here.\nReport 2 distinct content with different patterns xyz123.")
	hasLow := false
	for _, f := range findings {
		if f.Severity == "Low" && strings.Contains(f.Title, "weak boundary") {
			hasLow = true
		}
	}
	if !hasLow {
		t.Fatalf("expected Low severity finding for line split, got: %#v", findings)
	}
}

func TestSelectedStagesFrom(t *testing.T) {
	selected := selectedStages(pipelinepkg.RunOptions{From: "C"}, false)
	for _, stage := range []string{"A", "D", "E", "F", "B", "G"} {
		if selected[stage] {
			t.Fatalf("expected %s not selected", stage)
		}
	}
	for _, stage := range []string{"C"} {
		if !selected[stage] {
			t.Fatalf("expected %s selected", stage)
		}
	}
}

func TestSelectedStagesFromBIncludesRuntimeChain(t *testing.T) {
	selected := selectedStages(pipelinepkg.RunOptions{From: "B"}, false)
	for _, stage := range []string{"B", "G", "C"} {
		if !selected[stage] {
			t.Fatalf("expected %s selected", stage)
		}
	}
	for _, stage := range []string{"A", "D", "E", "F"} {
		if selected[stage] {
			t.Fatalf("expected %s not selected", stage)
		}
	}
}

func TestSelectedStagesSingleStageStillRunsSummary(t *testing.T) {
	selected := selectedStages(pipelinepkg.RunOptions{Stage: "D"}, false)
	if !selected["D"] || !selected["F"] {
		t.Fatalf("expected D and F selected, got %#v", selected)
	}
	for _, stage := range []string{"A", "B", "G", "C", "E"} {
		if selected[stage] {
			t.Fatalf("expected %s not selected", stage)
		}
	}
}

func TestSelectedStagesExplicitDependencyChain(t *testing.T) {
	selected := selectedStages(pipelinepkg.RunOptions{Stages: []string{"A", "B", "G", "C"}}, false)
	for _, stage := range []string{"A", "B", "G", "C"} {
		if !selected[stage] {
			t.Fatalf("expected %s selected", stage)
		}
	}
	if selected["D"] || selected["E"] || selected["F"] {
		t.Fatalf("D/E/F should not be selected unless explicitly requested")
	}
}

func TestRunReportsProgressWhenArtifactRootCannotBeCreated(t *testing.T) {
	scanPath := t.TempDir()
	projectPath := writePipelinePackage(t, scanPath, "batch-1", "TASK-1")
	if err := os.WriteFile(filepath.Join(scanPath, "result"), []byte("blocks artifact directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.ScanPath = scanPath
	cfg.DBPath = filepath.Join(t.TempDir(), "index.db")
	store, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.UpsertProjects(ctx, []scanner.Project{{TaskID: "TASK-1", Batch: "batch-1", Path: projectPath}}); err != nil {
		t.Fatal(err)
	}

	var updates []pipelinepkg.RunProgress
	_, err = pipelinepkg.NewRunner(store, cfg).Run(ctx, "TASK-1", pipelinepkg.RunOptions{
		Progress: func(update pipelinepkg.RunProgress) {
			updates = append(updates, update)
		},
	})
	if err == nil {
		t.Fatal("expected artifact directory creation to fail")
	}
	if len(updates) == 0 {
		t.Fatal("expected progress update for early run failure")
	}
	last := updates[len(updates)-1]
	if last.Event != "run_crashed" || !last.Done || last.Err == nil || last.RunID == "" {
		t.Fatalf("last progress = %#v, want run_crashed done with run id and error", last)
	}
}

func TestRunCanonicalizesStaleDBProjectPath(t *testing.T) {
	root := t.TempDir()
	canonical := writePipelinePackage(t, root, "batch-1", "TASK-STALE")
	outer := filepath.Dir(canonical)

	cfg := config.Default()
	cfg.ScanPath = root
	cfg.DBPath = filepath.Join(t.TempDir(), "index.db")
	cfg.Codex.PromptProfilesDir = filepath.Join(t.TempDir(), "missing-prompt-profiles")
	store, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertProjects(context.Background(), []scanner.Project{{TaskID: "TASK-STALE", Batch: "batch-1", Path: outer}}); err != nil {
		t.Fatal(err)
	}

	_, err = pipelinepkg.NewRunner(store, cfg).Run(context.Background(), "TASK-STALE", pipelinepkg.RunOptions{Stages: []string{"D"}})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.LatestRunForTask(context.Background(), "TASK-STALE")
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(run.ArtifactRoot, "run_manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		ProjectPath  string `json:"project_path"`
		PathWarnings []struct {
			Type string `json:"type"`
		} `json:"path_warnings"`
	}
	if err := json.Unmarshal(content, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.ProjectPath != canonical || len(manifest.PathWarnings) != 1 || manifest.PathWarnings[0].Type != "stale_project_path" {
		t.Fatalf("manifest should contain canonical path and stale warning: %#v", manifest)
	}
	if _, err := os.Stat(filepath.Join(run.ArtifactRoot, "logs", "path_warnings.log")); err != nil {
		t.Fatalf("path warning log should be written: %v", err)
	}
}

func TestRunSubmitManifestMarksUnselectedArtifactsWithoutWarnings(t *testing.T) {
	root := t.TempDir()
	projectPath := writePipelinePackage(t, root, "batch-1", "TASK-WARN")
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

	cfg := config.Default()
	cfg.ScanPath = root
	cfg.DBPath = filepath.Join(t.TempDir(), "index.db")
	cfg.Codex.PromptProfilesDir = profiles
	store, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.UpsertProjects(ctx, []scanner.Project{{TaskID: "TASK-WARN", Batch: "batch-1", Path: projectPath}}); err != nil {
		t.Fatal(err)
	}
	submitDir := filepath.Join(root, "result", "batch-1", "TASK-WARN", "submit")
	if err := os.MkdirAll(filepath.Join(submitDir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldValidationReportName := "stale_validation_report.md"
	for _, stale := range []string{oldValidationReportName, "unexpected.txt", filepath.Join("nested", "old.txt")} {
		if err := os.WriteFile(filepath.Join(submitDir, stale), []byte("stale"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	result, err := pipelinepkg.NewRunner(store, cfg).Run(ctx, "TASK-WARN", pipelinepkg.RunOptions{Stages: []string{"D"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, stale := range []string{oldValidationReportName, "unexpected.txt", filepath.Join("nested", "old.txt")} {
		if _, err := os.Stat(filepath.Join(submitDir, stale)); !os.IsNotExist(err) {
			t.Fatalf("submit reset should remove stale %s, stat err: %v", stale, err)
		}
	}
	if _, err := os.Stat(filepath.Join(submitDir, "test_effectiveness_report.md")); err != nil {
		t.Fatalf("submit reset should still copy current selected artifact: %v", err)
	}
	warnings, err := os.ReadFile(filepath.Join(result.Run.ArtifactRoot, "artifact_warnings.json"))
	if err == nil && strings.Contains(string(warnings), "submit_copy") {
		t.Fatalf("unselected submit files should not become artifact warnings:\n%s", warnings)
	}
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("unexpected artifact_warnings.json read error: %v", err)
	}
	manifest, err := os.ReadFile(filepath.Join(result.Run.ArtifactRoot, "submit_manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifest), "test_effectiveness_report.md") || !strings.Contains(string(manifest), `"not_selected": true`) {
		t.Fatalf("submit manifest missing expected file records:\n%s", manifest)
	}
}

func TestAggregateSubmitArtifactsAllowsMissingOptionalCodexReport(t *testing.T) {
	root := t.TempDir()
	artifactRoot := filepath.Join(root, "run")
	submitDir := filepath.Join(root, "submit")
	if err := os.MkdirAll(artifactRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	copies, err := pipelinepkg.AggregateSubmitArtifactsForTest(artifactRoot, submitDir, "initial", map[string]bool{"E": true})
	if err != nil {
		t.Fatal(err)
	}
	var codexReport pipelinepkg.TestSubmitArtifactCopy
	for _, copy := range copies {
		if copy.Name == "codex_report.md" {
			codexReport = copy
			break
		}
	}
	if !codexReport.Optional || codexReport.OK || codexReport.NotSelected || codexReport.Error != "" {
		t.Fatalf("missing optional codex_report should be recorded without copy error: %#v", codexReport)
	}
}

func TestRunMaterializesStaticArtifactsWhenCodexPreflightBlocked(t *testing.T) {
	root := t.TempDir()
	projectPath := writePipelinePackage(t, root, "batch-1", "TASK-PREFLIGHT")
	fakeBin := filepath.Join(root, "bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(fakeBin, "codex"), fakeCodexAppServerScript())
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := config.Default()
	cfg.ScanPath = root
	cfg.DBPath = filepath.Join(t.TempDir(), "index.db")
	cfg.Codex.PromptProfilesDir = filepath.Join(root, ".qa-control", "prompt_profiles")
	cfg.Codex.ExtraArgs = []string{"--search"}
	store, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.UpsertProjects(ctx, []scanner.Project{{TaskID: "TASK-PREFLIGHT", Batch: "batch-1", Path: projectPath}}); err != nil {
		t.Fatal(err)
	}

	result, err := pipelinepkg.NewRunner(store, cfg).Run(ctx, "TASK-PREFLIGHT", pipelinepkg.RunOptions{Stages: []string{"D", "E", "F"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"D", "E", "F"} {
		record := stageByName(result.Stages, stage)
		if record.Status != model.StageFailed {
			t.Fatalf("stage %s status = %s, want failed unavailable materialization; record=%#v", stage, record.Status, record)
		}
		if record.ErrorSummary == "" {
			t.Fatalf("stage %s should keep an unavailable error summary: %#v", stage, record)
		}
	}
	for _, name := range []string{
		"test_effectiveness_report.md",
		"codex_report.md",
		"operator_prompt_requirements_verification.md",
	} {
		content, err := os.ReadFile(filepath.Join(result.Run.ArtifactRoot, name))
		if err != nil {
			t.Fatalf("expected preflight-blocked static report %s: %v", name, err)
		}
		if !strings.Contains(string(content), "Manual Verification Required") {
			t.Fatalf("artifact %s should be an unavailable-review report:\n%s", name, content)
		}
	}
	for _, name := range []string{
		"operator_codex_report_issues_verification.md",
		"repair_summary.json",
	} {
		if _, err := os.Stat(filepath.Join(result.Run.ArtifactRoot, name)); err != nil {
			t.Fatalf("expected preflight-blocked static artifact %s: %v", name, err)
		}
	}
}

func TestRunRejectsUnknownStageBeforeCreatingRun(t *testing.T) {
	root := t.TempDir()
	projectPath := writePipelinePackage(t, root, "batch-1", "TASK-BAD-STAGE")

	cfg := config.Default()
	cfg.ScanPath = root
	cfg.DBPath = filepath.Join(t.TempDir(), "index.db")
	store, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.UpsertProjects(ctx, []scanner.Project{{TaskID: "TASK-BAD-STAGE", Batch: "batch-1", Path: projectPath}}); err != nil {
		t.Fatal(err)
	}

	_, err = pipelinepkg.NewRunner(store, cfg).Run(ctx, "TASK-BAD-STAGE", pipelinepkg.RunOptions{Stages: []string{"Z"}})
	if err == nil || !strings.Contains(err.Error(), `invalid stage "Z"`) {
		t.Fatalf("expected invalid stage error, got %v", err)
	}
	if _, err := store.LatestRunForTask(ctx, "TASK-BAD-STAGE"); err == nil {
		t.Fatal("invalid stage should not create a run")
	}
}

func TestRunInitialRequiresSupplementalDocsBeforeCreatingRun(t *testing.T) {
	root := t.TempDir()
	projectPath := writePipelinePackage(t, root, "batch-1", "TASK-NODOCS")
	removeSupplementalDocs(t, root, "TASK-NODOCS")

	cfg := config.Default()
	cfg.ScanPath = root
	cfg.DBPath = filepath.Join(t.TempDir(), "index.db")
	store, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.UpsertProjects(ctx, []scanner.Project{{TaskID: "TASK-NODOCS", Batch: "batch-1", Path: projectPath}}); err != nil {
		t.Fatal(err)
	}

	_, err = pipelinepkg.NewRunner(store, cfg, pipelinepkg.WithCommandRunner(lifecycleCommandRunner{})).Run(ctx, "TASK-NODOCS", pipelinepkg.RunOptions{Stages: []string{"A"}})
	if err == nil || !strings.Contains(err.Error(), "至少需要一个补充文档") {
		t.Fatalf("expected initial docs gate error, got %v", err)
	}
	if _, err := store.LatestRunForTask(ctx, "TASK-NODOCS"); err == nil {
		t.Fatal("docs gate should not create a run")
	}
}

func TestRunInitialImportsDropboxBeforeDocsGate(t *testing.T) {
	root := t.TempDir()
	projectPath := writePipelinePackage(t, root, "batch-1", "TASK-DROPBOX")
	removeSupplementalDocs(t, root, "TASK-DROPBOX")
	dropbox := filepath.Join(root, "task-docs", "TASK-DROPBOX")
	if err := os.MkdirAll(dropbox, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dropbox, "notes.md"), []byte("extra context"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.ScanPath = root
	cfg.DBPath = filepath.Join(t.TempDir(), "index.db")
	store, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.UpsertProjects(ctx, []scanner.Project{{TaskID: "TASK-DROPBOX", Batch: "batch-1", Path: projectPath}}); err != nil {
		t.Fatal(err)
	}

	result, err := pipelinepkg.NewRunner(store, cfg, pipelinepkg.WithCommandRunner(lifecycleCommandRunner{})).Run(ctx, "TASK-DROPBOX", pipelinepkg.RunOptions{Stages: []string{"A"}})
	if err != nil {
		t.Fatalf("dropbox docs should satisfy initial docs gate: %v", err)
	}
	if result.Run.RunID == "" {
		t.Fatal("expected run to be created after importing dropbox docs")
	}
	manifest, err := taskdocs.ReadManifest(root, "TASK-DROPBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Docs) != 1 || manifest.Docs[0].OriginalName != "notes.md" {
		t.Fatalf("dropbox doc should be imported before gate: %#v", manifest.Docs)
	}
}

func TestRunInjectsConfiguredDefaultStages(t *testing.T) {
	root := t.TempDir()
	projectPath := writePipelinePackage(t, root, "batch-1", "TASK-DEFAULT")
	doc := filepath.Join(t.TempDir(), "notes.md")
	if err := os.WriteFile(doc, []byte("extra context"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.ScanPath = root
	cfg.DBPath = filepath.Join(t.TempDir(), "index.db")
	cfg.Pipeline.DefaultStages = map[string][]string{"initial": {"A"}}
	if _, err := taskdocs.Attach(root, "TASK-DEFAULT", doc, "", "tester", cfg.Docs); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.UpsertProjects(ctx, []scanner.Project{{TaskID: "TASK-DEFAULT", Batch: "batch-1", Path: projectPath}}); err != nil {
		t.Fatal(err)
	}

	result, err := pipelinepkg.NewRunner(store, cfg, pipelinepkg.WithCommandRunner(lifecycleCommandRunner{})).Run(ctx, "TASK-DEFAULT", pipelinepkg.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if stageByName(result.Stages, "A").Status == model.StageSkipped {
		t.Fatalf("default stage A should run: %#v", result.Stages)
	}
	for _, stage := range []string{"D", "E", "F", "B", "C"} {
		if got := stageByName(result.Stages, stage); got.Status != model.StageSkipped {
			t.Fatalf("stage %s status = %s, want skipped after default_stages injection", stage, got.Status)
		}
	}
	content, err := os.ReadFile(filepath.Join(result.Run.ArtifactRoot, "run_manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), `"stages": [
    "A"
  ]`) {
		t.Fatalf("run manifest should record injected stages:\n%s", content)
	}
}

func TestStaticOnlyExplicitRuntimeStageFailsBeforeCreatingRun(t *testing.T) {
	root := t.TempDir()
	projectPath := writePipelinePackage(t, root, "batch-1", "TASK-STATIC")
	cfg := config.Default()
	cfg.ScanPath = root
	cfg.DBPath = filepath.Join(t.TempDir(), "index.db")
	cfg.Pipeline.StaticOnly = true
	store, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.UpsertProjects(ctx, []scanner.Project{{TaskID: "TASK-STATIC", Batch: "batch-1", Path: projectPath}}); err != nil {
		t.Fatal(err)
	}

	_, err = pipelinepkg.NewRunner(store, cfg).Run(ctx, "TASK-STATIC", pipelinepkg.RunOptions{Stages: []string{"B"}})
	if err == nil || !strings.Contains(err.Error(), "static-only") || !strings.Contains(err.Error(), "B") {
		t.Fatalf("expected explicit runtime stage static-only error, got %v", err)
	}
	if _, err := store.LatestRunForTask(ctx, "TASK-STATIC"); err == nil {
		t.Fatal("static-only stage validation should not create a run")
	}
}

func TestArtifactWriterRejectsPathsOutsideRoot(t *testing.T) {
	root := t.TempDir()
	writer := pipelinepkg.NewArtifactWriter(root)
	if err := writer.RequiredText("logs/ok.txt", "ok"); err != nil {
		t.Fatalf("relative artifact write failed: %v", err)
	}
	for _, path := range []string{"../escape.txt", filepath.Join(t.TempDir(), "escape.txt")} {
		if err := writer.RequiredText(path, "nope"); err == nil {
			t.Fatalf("artifact writer should reject path outside root: %s", path)
		}
	}
}

func TestArtifactWriterAllowsAbsolutePathWithinRoot(t *testing.T) {
	root := t.TempDir()
	writer := pipelinepkg.NewArtifactWriter(root)
	inside := filepath.Join(root, "logs", "ok.txt")
	if err := writer.RequiredText(inside, "ok"); err != nil {
		t.Fatalf("absolute path within artifact root should be allowed: %v", err)
	}
}

func TestRunFailsWhenCanonicalPackageRootInvalid(t *testing.T) {
	root := t.TempDir()
	outer := filepath.Join(root, "batch-1", "TASK-BAD")
	if err := os.MkdirAll(outer, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.ScanPath = root
	cfg.DBPath = filepath.Join(t.TempDir(), "index.db")
	store, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertProjects(context.Background(), []scanner.Project{{TaskID: "TASK-BAD", Batch: "batch-1", Path: outer}}); err != nil {
		t.Fatal(err)
	}

	_, err = pipelinepkg.NewRunner(store, cfg).Run(context.Background(), "TASK-BAD", pipelinepkg.RunOptions{Stages: []string{"Z"}})
	if err == nil || !strings.Contains(err.Error(), "indexed project path is invalid or stale") || strings.Contains(err.Error(), ".git") {
		t.Fatalf("expected explicit canonical package root failure without generic repo fallback, got %v", err)
	}
}

func TestRecheckRejectsCrashedReferenceRun(t *testing.T) {
	root := t.TempDir()
	projectPath := writePipelinePackage(t, root, "batch-1", "TASK-1")
	refRoot := filepath.Join(root, "refs", "run-crashed")
	if err := os.MkdirAll(refRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.ScanPath = root
	cfg.DBPath = filepath.Join(t.TempDir(), "index.db")
	store, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.UpsertProjects(ctx, []scanner.Project{{TaskID: "TASK-1", Batch: "batch-1", Path: projectPath}}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRun(ctx, model.RunRecord{
		RunID:         "run-crashed",
		TaskID:        "TASK-1",
		StartedAt:     "2026-04-30T00:00:00Z",
		Status:        model.RunCrashed,
		ManualVerdict: model.ManualUnset,
		ArtifactRoot:  refRoot,
	}); err != nil {
		t.Fatal(err)
	}

	_, err = pipelinepkg.NewRunner(store, cfg).Run(ctx, "TASK-1", pipelinepkg.RunOptions{Mode: "recheck", RefRun: "run-crashed", Stage: "F"})
	if err == nil || !strings.Contains(err.Error(), "requires a completed reference run") {
		t.Fatalf("expected completed ref-run validation error, got %v", err)
	}
}

func TestAssignFindingIDs(t *testing.T) {
	findings := assignFindingIDs("E", []model.Finding{
		{Severity: "Blocker", Title: "one"},
		{Severity: "High", Title: "two"},
		{Severity: "High", Title: "three"},
	})
	want := []string{"P2R-E-BLK-001", "P2R-E-HIGH-001", "P2R-E-HIGH-002"}
	for i, id := range want {
		if findings[i].ID != id {
			t.Fatalf("finding %d id = %s, want %s", i, findings[i].ID, id)
		}
	}
}

func TestShortCommentKeepsManualVerdictUnchecked(t *testing.T) {
	text := shortComment(map[string]string{"B": "skipped", "C": "skipped"}, nil)
	if want := "<[ ] PASS  [ ] REWORK  [ ] FAIL>"; !strings.Contains(text, want) {
		t.Fatalf("short comment missing manual verdict line: %s", text)
	}
}

func TestShortCommentDoesNotExposeDoneCriteriaAsRisk(t *testing.T) {
	text := shortComment(map[string]string{"B": "done", "C": "done"}, []model.Finding{{
		ID:           "P2R-A-BLK-001",
		Severity:     "Blocker",
		Title:        "missing auth",
		Rule:         "rule-1",
		DoneCriteria: "acceptance passes after adding auth",
	}})
	if strings.Contains(text, "acceptance passes") {
		t.Fatalf("short comment exposed done criteria: %s", text)
	}
	if !strings.Contains(text, "rule-1") {
		t.Fatalf("short comment should include risk rule/evidence context: %s", text)
	}
}

func TestTerminalScreenshotUsesTailSinglePageInput(t *testing.T) {
	var builder strings.Builder
	for i := 1; i <= 90; i++ {
		builder.WriteString("line ")
		builder.WriteString(fmt.Sprint(i))
		builder.WriteString("\n")
	}
	lines := terminalScreenshotLines("\x1b[31m" + builder.String())
	if len(lines) != 80 {
		t.Fatalf("expected 80 tail lines, got %d", len(lines))
	}
	if lines[0] != "line 11" || lines[len(lines)-1] != "line 90" {
		t.Fatalf("unexpected tail lines: first=%q last=%q", lines[0], lines[len(lines)-1])
	}
}

func TestSafeCodexExtraArgsRejectsBoundaryFlags(t *testing.T) {
	if _, err := safeCodexExtraArgs([]string{"--model", "gpt-5.4"}); err != nil {
		t.Fatalf("safe args rejected: %v", err)
	}
	for _, flag := range []string{"--sandbox", "--full-auto", "--search", "--dangerously-bypass-approvals-and-sandbox"} {
		if _, err := safeCodexExtraArgs([]string{flag}); err == nil {
			t.Fatalf("expected %s to be rejected", flag)
		}
	}
	if _, err := safeCodexExtraArgs([]string{"--full-auto=true"}); err == nil {
		t.Fatal("expected --full-auto=... to be rejected")
	}
}

func TestCapabilitySummaryIncludesAppServerDiagnostic(t *testing.T) {
	summary := capabilitySummary(codex.Capability{Path: "codex", HasAppServer: true})
	if !strings.Contains(summary, "app_server=true") {
		t.Fatalf("summary missing app-server diagnostic: %s", summary)
	}
}

func TestCodexGuidanceMessagesRestateStaticReviewContract(t *testing.T) {
	deadlines := codexGuidanceSchedule(45*time.Minute, "E")
	if len(deadlines) != 3 {
		t.Fatalf("guidance deadlines = %d, want 3", len(deadlines))
	}
	for _, deadline := range deadlines {
		for _, want := range []string{
			"<!-- p2r:static-review-json:start -->",
			`"schema_version": "p2r.static_review.v1"`,
			`"stage": "E"`,
			`"findings": []`,
			"Begin immediately with the report's first heading",
			"Do not return a prose-only summary.",
			"Do not put any text after the JSON end marker.",
		} {
			if !strings.Contains(deadline.Message, want) {
				t.Fatalf("%s guidance missing %q:\n%s", deadline.Label, want, deadline.Message)
			}
		}
	}
}

func TestStaticUnavailableReportIncludesMachineReadableContract(t *testing.T) {
	report := staticUnavailableReport("E", "static_acceptance_audit.md", "/tmp/project", "static review report schema invalid: missing marker")
	if !strings.Contains(report, "Manual Verification Required") {
		t.Fatalf("unavailable report missing manual-verification text:\n%s", report)
	}
	findings, err := staticReviewFindingsFromReport("E", report, "/tmp/report.md")
	if err != nil {
		t.Fatalf("unavailable report should satisfy static review schema: %v\n%s", err, report)
	}
	if len(findings) != 1 || findings[0].Severity != "High" || !strings.Contains(findings[0].Evidence, "missing marker") {
		t.Fatalf("unexpected unavailable findings: %#v", findings)
	}
}

func TestNormalizeStaticReviewReportRemovesPreambleAndMovesContractToEnd(t *testing.T) {
	report := `I will keep this strictly static and inspect files only.

Tool note: rg is unavailable, so I am using find.

1. **Verdict**

Overall conclusion: **Partial Pass**

<!-- p2r:static-review-json:start -->
{
  "schema_version": "p2r.static_review.v1",
  "stage": "E",
  "findings": []
}
<!-- p2r:static-review-json:end -->

2. **Scope and Static Verification Boundary**

Reviewed repository files only.
`
	normalized, err := normalizeStaticReviewReport(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(normalized, "I will keep this strictly static") || strings.Contains(normalized, "Tool note") {
		t.Fatalf("preamble should be removed:\n%s", normalized)
	}
	if !strings.HasPrefix(normalized, "1. **Verdict**") {
		t.Fatalf("normalized report should start with first report section:\n%s", normalized)
	}
	jsonStart := strings.Index(normalized, "<!-- p2r:static-review-json:start -->")
	scope := strings.Index(normalized, "2. **Scope and Static Verification Boundary**")
	if jsonStart < 0 || scope < 0 || jsonStart < scope {
		t.Fatalf("JSON contract should be moved after the report body:\n%s", normalized)
	}
	if !strings.HasSuffix(strings.TrimSpace(normalized), "<!-- p2r:static-review-json:end -->") {
		t.Fatalf("JSON contract should be the final block:\n%s", normalized)
	}
}

func TestNormalizeStaticReviewReportCollapsesMultipleContractBlocks(t *testing.T) {
	report := `# Repair Verification

Report 1 body.

<!-- p2r:static-review-json:start -->
{
  "schema_version": "p2r.static_review.v1",
  "stage": "F",
  "findings": [
    {
      "severity": "Low",
      "title": "earlier block",
      "rule": "earlier duplicate block should not remain authoritative",
      "evidence": "repo/old.py:1",
      "impact": "duplicate blocks make report layout invalid",
      "minimum_fix": "keep one final contract block"
    }
  ]
}
<!-- p2r:static-review-json:end -->

## Report 2

Report 2 body.

<!-- p2r:static-review-json:start -->
{
  "schema_version": "p2r.static_review.v1",
  "stage": "F",
  "findings": [
    {
      "severity": "High",
      "title": "final block",
      "rule": "the final contract block is the authoritative machine-readable result",
      "evidence": "repo/new.py:2",
      "impact": "p2r can parse the final reviewer conclusion",
      "minimum_fix": "collapse duplicate blocks before schema parsing"
    }
  ]
}
<!-- p2r:static-review-json:end -->
`
	normalized, err := normalizeStaticReviewReport(report)
	if err != nil {
		t.Fatal(err)
	}
	startMarker := "<!-- p2r:static-review-json:start -->"
	endMarker := "<!-- p2r:static-review-json:end -->"
	if strings.Count(normalized, startMarker) != 1 || strings.Count(normalized, endMarker) != 1 {
		t.Fatalf("normalized report should contain exactly one contract block:\n%s", normalized)
	}
	if !strings.Contains(normalized, "Report 1 body.") || !strings.Contains(normalized, "Report 2 body.") {
		t.Fatalf("normalized report should preserve human-readable report bodies:\n%s", normalized)
	}
	findings, err := staticReviewFindingsFromReport("F", normalized, "/tmp/report.md")
	if err != nil {
		t.Fatalf("collapsed report should satisfy schema: %v\n%s", err, normalized)
	}
	if len(findings) != 2 || findings[0].Title != "earlier block" || findings[1].Title != "final block" {
		t.Fatalf("expected final contract findings to be authoritative, got %#v", findings)
	}
}

func TestTruncateStaticReviewReportPreservesContract(t *testing.T) {
	var body strings.Builder
	body.WriteString("# Static Review\n\n")
	for i := 0; i < 200; i++ {
		body.WriteString("Long evidence line that can be shortened without losing the JSON contract.\n")
	}
	report := body.String() + `
<!-- p2r:static-review-json:start -->
{
  "schema_version": "p2r.static_review.v1",
  "stage": "E",
  "findings": [
    {
      "severity": "High",
      "title": "kept finding",
      "rule": "contract must survive truncation",
      "evidence": "repo/file.go:12",
      "impact": "findings remain classifiable after truncation",
      "minimum_fix": "preserve the contract block when shortening report bodies"
    }
  ]
}
<!-- p2r:static-review-json:end -->
`
	truncated := truncateStaticReviewReport(report, 900)
	if !strings.Contains(truncated, "[truncated]") {
		t.Fatalf("expected truncation marker:\n%s", truncated)
	}
	if !strings.HasSuffix(strings.TrimSpace(truncated), "<!-- p2r:static-review-json:end -->") {
		t.Fatalf("contract should remain the final block:\n%s", truncated)
	}
	findings, err := staticReviewFindingsFromReport("E", truncated, "/tmp/report.md")
	if err != nil {
		t.Fatalf("truncated report should still satisfy schema: %v\n%s", err, truncated)
	}
	if len(findings) != 1 || findings[0].Title != "kept finding" {
		t.Fatalf("unexpected findings after truncation: %#v", findings)
	}
}

func TestCodexGuidanceSendsDeadlinesUntilFinalResult(t *testing.T) {
	session := &fakeCodexSession{waitDelay: 35 * time.Millisecond}
	deadlines := []pipelinepkg.CodexGuidanceDeadline{
		{Label: "first", After: 10 * time.Millisecond, Message: "soft"},
		{Label: "second", After: 20 * time.Millisecond, Message: "deadline"},
		{Label: "third", After: 80 * time.Millisecond, Message: "too late"},
	}
	result, err := runCodexReviewSessionWithGuidance(context.Background(), session, pipelinepkg.CodexReviewRequest{}, deadlines)
	if err != nil {
		t.Fatal(err)
	}
	if result.Result.Stdout != "final" {
		t.Fatalf("result stdout = %q", result.Result.Stdout)
	}
	if got := strings.Join(session.guidanceMessages(), ","); got != "soft,deadline" {
		t.Fatalf("guidance messages = %q", got)
	}
	if len(result.GuidanceEvents) != 2 || result.GuidanceEvents[0].Label != "first" || result.GuidanceEvents[1].Label != "second" {
		t.Fatalf("guidance events = %#v", result.GuidanceEvents)
	}
}

func TestCodexGuidanceDoesNotSendAfterFinalResult(t *testing.T) {
	session := &fakeCodexSession{waitDelay: 5 * time.Millisecond}
	deadlines := []pipelinepkg.CodexGuidanceDeadline{{Label: "first", After: 40 * time.Millisecond, Message: "late"}}
	result, err := runCodexReviewSessionWithGuidance(context.Background(), session, pipelinepkg.CodexReviewRequest{}, deadlines)
	if err != nil {
		t.Fatal(err)
	}
	if result.Result.Stdout != "final" {
		t.Fatalf("result stdout = %q", result.Result.Stdout)
	}
	if got := session.guidanceMessages(); len(got) != 0 {
		t.Fatalf("guidance should not be sent after final result, got %#v", got)
	}
}

func TestCodexGuidanceStopsPromptlyWhenContextCancelled(t *testing.T) {
	session := &fakeCodexSession{waitDelay: time.Hour}
	deadlines := []pipelinepkg.CodexGuidanceDeadline{{Label: "late", After: time.Hour, Message: "late"}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	result, err := runCodexReviewSessionWithGuidance(ctx, session, pipelinepkg.CodexReviewRequest{}, deadlines)
	if err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("expected context deadline error, got err=%v result=%#v", err, result)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("guidance runner did not stop promptly after context cancellation: %s", elapsed)
	}
	if got := session.guidanceMessages(); len(got) != 0 {
		t.Fatalf("guidance should not be sent after context cancellation, got %#v", got)
	}
}

func TestCodexGuidancePreservesDiagnosticsAfterContextCancelled(t *testing.T) {
	session := &cancelDiagnosticCodexSession{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	result, err := runCodexReviewSessionWithGuidance(ctx, session, pipelinepkg.CodexReviewRequest{}, nil)
	if err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("expected context deadline error, got err=%v result=%#v", err, result)
	}
	if !strings.Contains(result.Result.Stderr, "codex stderr after cancel") {
		t.Fatalf("cancel diagnostics were not preserved: %#v", result.Result)
	}
}

func TestCodexStartFailurePreservesSessionDiagnostics(t *testing.T) {
	startErr := errors.New("initialize failed")
	session := &startFailureCodexSession{
		startErr: startErr,
		result: pipelinepkg.CodexReviewResult{Result: executor.Result{
			Command: "codex app-server --listen stdio://",
			Stderr:  "not authenticated",
			Err:     startErr,
		}},
	}
	result, err := runCodexReviewSessionWithGuidance(context.Background(), session, pipelinepkg.CodexReviewRequest{}, nil)
	if !errors.Is(err, startErr) {
		t.Fatalf("start error = %v, want %v", err, startErr)
	}
	if result.Result.Command == "" || !strings.Contains(result.Result.Stderr, "not authenticated") {
		t.Fatalf("session diagnostics were not preserved: %#v", result.Result)
	}
}

func TestPromptProfilesUseFinalResponseContract(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "../../.."))
	for _, name := range []string{"tests_coverage_report.md", "static_acceptance_audit.md"} {
		content, err := os.ReadFile(filepath.Join(repoRoot, "assets", "prompt_profiles", name))
		if err != nil {
			t.Fatal(err)
		}
		text := string(content)
		if strings.Contains(text, "./.tmp") {
			t.Fatalf("%s still asks Codex to write a .tmp report", name)
		}
		if !strings.Contains(text, "final Codex response") || !strings.Contains(text, "Do not write files") {
			t.Fatalf("%s does not state the p2r final-response contract", name)
		}
		if !strings.Contains(text, "Do not include progress updates") || !strings.Contains(text, "JSON end marker") {
			t.Fatalf("%s does not forbid preamble text and require final JSON placement", name)
		}
	}
	content, err := os.ReadFile(filepath.Join(repoRoot, "assets", "prompt_profiles", "annotator_fix.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	if strings.Contains(text, "./.tmp") {
		t.Fatal("annotator_fix.md still asks Codex to write a .tmp report")
	}
	for _, want := range []string{
		"two independent Markdown reports, separated by an exact split marker",
		"# Repair Summary",
		"## Report 1: Repair Verification Requirements and Fit",
		"<!-- p2r:report-split -->",
		"## Report 2: Repair Verification Issues",
		"Do not write files",
		"Do not include progress updates",
		"JSON end marker",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("annotator_fix.md missing %q", want)
		}
	}
}

type cancelDiagnosticCodexSession struct{}

func (s *cancelDiagnosticCodexSession) Start(ctx context.Context, request pipelinepkg.CodexReviewRequest) error {
	return nil
}

func (s *cancelDiagnosticCodexSession) SendGuidance(ctx context.Context, message string) error {
	return nil
}

func (s *cancelDiagnosticCodexSession) Wait(ctx context.Context) (pipelinepkg.CodexReviewResult, error) {
	<-ctx.Done()
	time.Sleep(25 * time.Millisecond)
	return pipelinepkg.CodexReviewResult{Result: executor.Result{
		Command: "codex app-server --listen stdio://",
		Stderr:  "codex stderr after cancel",
		Err:     ctx.Err(),
	}}, ctx.Err()
}

type startFailureCodexSession struct {
	startErr error
	result   pipelinepkg.CodexReviewResult
}

func (s *startFailureCodexSession) Start(ctx context.Context, request pipelinepkg.CodexReviewRequest) error {
	return s.startErr
}

func (s *startFailureCodexSession) SendGuidance(ctx context.Context, message string) error {
	return nil
}

func (s *startFailureCodexSession) Wait(ctx context.Context) (pipelinepkg.CodexReviewResult, error) {
	return s.result, s.startErr
}

type fakeCodexSession struct {
	waitDelay time.Duration
	done      chan struct{}
	mu        sync.Mutex
	guidance  []string
}

func (s *fakeCodexSession) Start(ctx context.Context, request pipelinepkg.CodexReviewRequest) error {
	s.done = make(chan struct{})
	go func() {
		timer := time.NewTimer(s.waitDelay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-timer.C:
		}
		close(s.done)
	}()
	return nil
}

func (s *fakeCodexSession) SendGuidance(ctx context.Context, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.guidance = append(s.guidance, message)
	return nil
}

func (s *fakeCodexSession) Wait(ctx context.Context) (pipelinepkg.CodexReviewResult, error) {
	select {
	case <-ctx.Done():
		return pipelinepkg.CodexReviewResult{Result: executor.Result{Err: ctx.Err()}}, ctx.Err()
	case <-s.done:
		return pipelinepkg.CodexReviewResult{Result: executor.Result{Stdout: "final"}}, nil
	}
}

func (s *fakeCodexSession) guidanceMessages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.guidance...)
}

func TestParseComposePSExtractsMappings(t *testing.T) {
	raw := `{"Service":"web","Publishers":[{"URL":"0.0.0.0","TargetPort":3000,"PublishedPort":34152,"Protocol":"tcp"}]}`
	mappings, services := parseComposePS(raw)
	if len(services) != 1 || services[0] != "web" {
		t.Fatalf("unexpected services: %#v", services)
	}
	if got := mappings["web"][0].Host; got != 34152 {
		t.Fatalf("host port = %d, want 34152", got)
	}
}

func TestReadmeDockerComposeCommandAcceptsStandaloneSpelling(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("README.md", "```sh\ndocker-compose up --build\n```\n")
	fields := readmeComposeCommand(dir)
	if strings.Join(fields[:2], " ") != "docker compose" {
		t.Fatalf("expected docker-compose normalized to docker compose, got %#v", fields)
	}
}

func TestParseDockerPortFallback(t *testing.T) {
	mappings := parseDockerPort("web", "80/tcp -> 0.0.0.0:34152\n443/tcp -> [::]:34153\n")
	if len(mappings) != 2 {
		t.Fatalf("expected 2 mappings, got %#v", mappings)
	}
	if mappings[0].Container != 80 || mappings[0].Host != 34152 {
		t.Fatalf("unexpected first mapping: %#v", mappings[0])
	}
}

func TestExtractFindingsFromReport(t *testing.T) {
	report := `# Verdict

- Blocker: missing auth guard
- High: run_tests does not hit API

<!-- p2r:static-review-json:start -->
{
  "schema_version": "p2r.static_review.v1",
  "stage": "E",
  "findings": [
    {
      "severity": "Blocker",
      "title": "missing auth guard",
      "rule": "Acceptance requires protected routes to enforce auth.",
      "evidence": ["repo/server.js:42 lacks auth middleware", "repo/routes.js:12 is reachable without a guard"],
      "impact": "Unauthorized users can reach protected behavior.",
      "minimum_fix": "Add auth middleware and tests around protected routes."
    },
    {
      "severity": "High",
      "title": "run_tests does not hit API",
      "rule": "Self tests must exercise the delivered API.",
      "evidence": "repo/run_tests.sh:7 only checks process startup",
      "impact": "Endpoint regressions can pass self-test.",
      "minimum_fix": "Add API assertions to run_tests.sh."
    }
  ]
}
<!-- p2r:static-review-json:end -->
`
	findings := extractFindingsFromReport("E", report, "report.md")
	if len(findings) != 2 {
		t.Fatalf("expected 2 findings, got %d: %#v", len(findings), findings)
	}
	if findings[0].Severity != "Blocker" || findings[1].Severity != "High" {
		t.Fatalf("unexpected severities: %#v", findings)
	}
	if findings[0].Rule == "" || findings[0].Evidence == "" || findings[0].MinimumFix == "" {
		t.Fatalf("structured finding details were not preserved: %#v", findings[0])
	}
}

func TestStaticReviewFindingsRequireContract(t *testing.T) {
	report := "# Verdict\n\n- High: this old text format should not be parsed\n"
	findings, err := staticReviewFindingsFromReport("E", report, "report.md")
	if err == nil {
		t.Fatalf("expected missing contract to fail, got findings %#v", findings)
	}
	if len(extractFindingsFromReport("E", report, "report.md")) != 0 {
		t.Fatal("legacy keyword extraction should not produce findings")
	}
}

func TestAcceptanceFindingsMapRealScriptPayload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "acceptance.json")
	payload := `{
  "blocking_issues": [{"issue_id":"required-artifacts-missing","severity":"blocking","rule":"3.2.1","evidence":"missing docs/design.md","repair_action":"add it","done_criteria":"check passes"}],
  "non_blocking_issues": [
    {"issue_id":"test-structure-gap","severity":"major","rule":"3.3.4","evidence":"weak tests","repair_action":"add tests","done_criteria":"tests pass"},
    {"issue_id":"runtime-verification-missing","severity":"major","rule":"3.1.1","evidence":"run_acceptance.py was executed without --runtime-command","repair_action":"run B/G/C","done_criteria":"runtime evidence exists"}
  ]
}`
	if err := os.WriteFile(path, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	findings := acceptanceFindings(path)
	if len(findings) != 2 {
		t.Fatalf("expected 2 findings, got %#v", findings)
	}
	if findings[0].Severity != "Blocker" || findings[1].Severity != "High" {
		t.Fatalf("unexpected severities: %#v", findings)
	}
	if findings[0].Title != "required-artifacts-missing" || findings[0].SourcePath != path {
		t.Fatalf("unexpected first finding: %#v", findings[0])
	}
}

func TestAcceptanceScriptArgsMatchRealScriptContract(t *testing.T) {
	outputs := map[string]string{
		"acceptance":    "acceptance.json",
		"acceptance_md": "acceptance_report.md",
	}
	got := acceptanceScriptArgs(outputs, []string{"--project-type", "fullstack"})
	want := []string{"--output-json", "acceptance.json", "--output-md", "acceptance_report.md", "--project-type", "fullstack"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestValidationScriptArgsOwnValidationReport(t *testing.T) {
	outputs := map[string]string{
		"validation_md": "validation_report.md",
	}
	got := validationScriptArgs(outputs, []string{"--project-type", "fullstack"})
	want := []string{"--output-md", "validation_report.md"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestRunArtifactRootUsesResultBatchTask(t *testing.T) {
	root := t.TempDir()
	projectPath := filepath.Join(root, "batch-1", "TASK-1", "TASK-1")
	got := runArtifactRoot(root, scanner.Project{TaskID: "TASK-1", Batch: "batch-1", Path: projectPath}, "run-1")
	want := filepath.Join(root, "result", "batch-1", "TASK-1", "run-1")
	if got != want {
		t.Fatalf("artifact root = %q, want %q", got, want)
	}
	if strings.HasPrefix(got, projectPath+string(filepath.Separator)) {
		t.Fatalf("artifact root should not be under original package: %s", got)
	}
}

func TestRunArtifactRootFallsBackWhenTaskFolderIsOriginalPackage(t *testing.T) {
	root := t.TempDir()
	projectPath := root
	got := runArtifactRoot(root, scanner.Project{TaskID: "TASK-1", Batch: "batch-1", Path: projectPath}, "run-1")
	want := filepath.Join(root, ".qa-control", "runs", "batch-1", "TASK-1", "run-1")
	if got != want {
		t.Fatalf("artifact root = %q, want %q", got, want)
	}
}

func TestRunArtifactRootFallsBackToUnbatchedForLegacyProject(t *testing.T) {
	root := t.TempDir()
	projectPath := filepath.Join(root, "legacy", "TASK-1")
	got := runArtifactRoot(root, scanner.Project{TaskID: "TASK-1", Path: projectPath}, "run-1")
	want := filepath.Join(root, "result", "unbatched", "TASK-1", "run-1")
	if got != want {
		t.Fatalf("artifact root = %q, want %q", got, want)
	}
}

func TestRunArtifactRootCleansUnsafeSegments(t *testing.T) {
	root := t.TempDir()
	projectPath := filepath.Join(root, "batch-1", "TASK-1", "TASK-1")
	got := runArtifactRoot(root, scanner.Project{TaskID: "../TASK-1", Batch: "batch/1", Path: projectPath}, "../run-1")
	want := filepath.Join(root, "result", "unbatched", "TASK-UNKNOWN", "run-unknown")
	if got != want {
		t.Fatalf("artifact root = %q, want %q", got, want)
	}
	if strings.Contains(got, "..") {
		t.Fatalf("artifact root contains parent traversal: %s", got)
	}
}

func TestCopyPackageSnapshotExcludesPriorQAArtifacts(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"docs", "repo", "original_sessions", filepath.Join("qa", "runs", "old")} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "metadata.json"), []byte(`{"prompt":"build it"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "qa", "runs", "old", "short_comment.txt"), []byte("old generated artifact"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(root, "qa", "runs", "new", "script_input_snapshot")
	if err := copyPackageSnapshot(root, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "metadata.json")); err != nil {
		t.Fatalf("metadata should be copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "qa")); !os.IsNotExist(err) {
		t.Fatalf("qa artifacts should be excluded, stat err: %v", err)
	}
}

func TestCopyPackageSnapshotExcludesResultArtifacts(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"docs", "repo", "original_sessions", filepath.Join("result", "batch-1", "TASK-1", "run-1")} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "metadata.json"), []byte(`{"prompt":"build it"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "result", "batch-1", "TASK-1", "run-1", "run_manifest.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "snapshot")
	if err := copyPackageSnapshot(root, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "result")); !os.IsNotExist(err) {
		t.Fatalf("result artifacts should be excluded, stat err: %v", err)
	}
}

func TestCopyPackageSnapshotExcludesTaskDocsControlDir(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"docs", "repo", "original_sessions", filepath.Join("task-docs", "TASK-1")} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "metadata.json"), []byte(`{"prompt":"build it"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "snapshot")
	if err := copyPackageSnapshot(root, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "task-docs")); !os.IsNotExist(err) {
		t.Fatalf("task-docs control dir should be excluded, stat err: %v", err)
	}
}

func writePipelinePackage(t *testing.T, root, batch, taskID string) string {
	t.Helper()
	projectPath := filepath.Join(root, batch, taskID, taskID)
	for _, dir := range []string{"docs", "repo", "original_sessions"} {
		if err := os.MkdirAll(filepath.Join(projectPath, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(projectPath, "metadata.json"), []byte(`{"task_id":"`+taskID+`","prompt":"build it"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSupplementalDoc(t, root, taskID)
	return projectPath
}

func writeSupplementalDoc(t *testing.T, root, taskID string) {
	t.Helper()
	dropbox := filepath.Join(root, "task-docs", taskID)
	if err := os.MkdirAll(dropbox, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dropbox, "notes.md"), []byte("extra context"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func removeSupplementalDocs(t *testing.T, root, taskID string) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(root, "task-docs", taskID)); err != nil {
		t.Fatal(err)
	}
}
