package pipeline_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	_ "unsafe"

	pipelinepkg "github.com/xuanli520/p2r_tui/internal/pipeline"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
)

// Linknames keep mirrored tests out of source directories while preserving
// coverage for package-private pipeline helpers.
//
//go:linkname selectedStages github.com/xuanli520/p2r_tui/internal/pipeline.selectedStages
func selectedStages(opts pipelinepkg.RunOptions, staticOnly bool) map[string]bool

//go:linkname assignFindingIDs github.com/xuanli520/p2r_tui/internal/pipeline.assignFindingIDs
func assignFindingIDs(stage string, findings []model.Finding) []model.Finding

//go:linkname shortComment github.com/xuanli520/p2r_tui/internal/pipeline.shortComment
func shortComment(stageStatuses map[string]string, findings []model.Finding) string

//go:linkname readmeComposeCommand github.com/xuanli520/p2r_tui/internal/pipeline.readmeComposeCommand
func readmeComposeCommand(repoPath string) []string

//go:linkname extractFindingsFromReport github.com/xuanli520/p2r_tui/internal/pipeline.extractFindingsFromReport
func extractFindingsFromReport(stage, report, sourcePath string) []model.Finding

//go:linkname acceptanceFindings github.com/xuanli520/p2r_tui/internal/pipeline.acceptanceFindings
func acceptanceFindings(path string) []model.Finding

//go:linkname acceptanceScriptArgs github.com/xuanli520/p2r_tui/internal/pipeline.acceptanceScriptArgs
func acceptanceScriptArgs(outputs map[string]string, projectTypeArgs []string) []string

//go:linkname copyPackageSnapshot github.com/xuanli520/p2r_tui/internal/pipeline.copyPackageSnapshot
func copyPackageSnapshot(source, dest string) error

//go:linkname terminalScreenshotLines github.com/xuanli520/p2r_tui/internal/pipeline.terminalScreenshotLines
func terminalScreenshotLines(text string) []string

//go:linkname safeCodexExtraArgs github.com/xuanli520/p2r_tui/internal/pipeline.safeCodexExtraArgs
func safeCodexExtraArgs(args []string) ([]string, error)

type portMapping struct {
	Service   string
	URL       string
	Host      int
	Container int
	Protocol  string
}

//go:linkname parseComposePS github.com/xuanli520/p2r_tui/internal/pipeline.parseComposePS
func parseComposePS(raw string) (map[string][]portMapping, []string)

//go:linkname parseDockerPort github.com/xuanli520/p2r_tui/internal/pipeline.parseDockerPort
func parseDockerPort(service, raw string) []portMapping

func TestSelectedStagesStaticOnly(t *testing.T) {
	selected := selectedStages(pipelinepkg.RunOptions{StaticOnly: true}, true)
	for _, stage := range []string{"A", "D", "E", "F"} {
		if !selected[stage] {
			t.Fatalf("expected %s selected", stage)
		}
	}
	for _, stage := range []string{"B", "C"} {
		if selected[stage] {
			t.Fatalf("expected %s skipped", stage)
		}
	}
}

func TestSelectedStagesFrom(t *testing.T) {
	selected := selectedStages(pipelinepkg.RunOptions{From: "C"}, false)
	for _, stage := range []string{"A", "B"} {
		if selected[stage] {
			t.Fatalf("expected %s not selected", stage)
		}
	}
	for _, stage := range []string{"C", "D", "E", "F"} {
		if !selected[stage] {
			t.Fatalf("expected %s selected", stage)
		}
	}
}

func TestSelectedStagesSingleStageStillRunsSummary(t *testing.T) {
	selected := selectedStages(pipelinepkg.RunOptions{Stage: "D"}, false)
	if !selected["D"] || !selected["F"] {
		t.Fatalf("expected D and F selected, got %#v", selected)
	}
	for _, stage := range []string{"A", "B", "C", "E"} {
		if selected[stage] {
			t.Fatalf("expected %s not selected", stage)
		}
	}
}

func TestSelectedStagesExplicitDependencyChain(t *testing.T) {
	selected := selectedStages(pipelinepkg.RunOptions{Stages: []string{"A", "B", "C"}}, false)
	for _, stage := range []string{"A", "B", "C", "F"} {
		if !selected[stage] {
			t.Fatalf("expected %s selected", stage)
		}
	}
	if selected["D"] || selected["E"] {
		t.Fatalf("D/E should not be selected for A dependency rerun")
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
	if _, err := safeCodexExtraArgs([]string{"--sandbox", "workspace-write"}); err == nil {
		t.Fatal("expected --sandbox to be rejected")
	}
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
	report := "# Verdict\n\n- Blocker: missing auth guard\n- High: run_tests does not hit API\nNo blocker in unrelated sentence\n"
	findings := extractFindingsFromReport("E", report, "report.md")
	if len(findings) != 2 {
		t.Fatalf("expected 2 findings, got %d: %#v", len(findings), findings)
	}
	if findings[0].Severity != "Blocker" || findings[1].Severity != "High" {
		t.Fatalf("unexpected severities: %#v", findings)
	}
}

func TestAcceptanceFindingsMapRealScriptPayload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "acceptance.json")
	payload := `{
  "blocking_issues": [{"issue_id":"required-artifacts-missing","severity":"blocking","rule":"3.2.1","evidence":"missing docs/design.md","repair_action":"add it","done_criteria":"check passes"}],
  "non_blocking_issues": [
    {"issue_id":"test-structure-gap","severity":"major","rule":"3.3.4","evidence":"weak tests","repair_action":"add tests","done_criteria":"tests pass"},
    {"issue_id":"runtime-verification-missing","severity":"major","rule":"3.1.1","evidence":"run_acceptance.py was executed without --runtime-command","repair_action":"run B/C","done_criteria":"runtime evidence exists"}
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
		"acceptance": "acceptance.json",
		"validation": "validation_report.md",
	}
	got := acceptanceScriptArgs(outputs, []string{"--project-type", "fullstack"})
	want := []string{"--output-json", "acceptance.json", "--output-md", "validation_report.md", "--project-type", "fullstack"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %#v, want %#v", got, want)
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
