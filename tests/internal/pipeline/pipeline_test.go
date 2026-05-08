package pipeline_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	_ "unsafe"

	"github.com/xuanli520/p2r_tui/internal/codex"
	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/db"
	"github.com/xuanli520/p2r_tui/internal/executor"
	pipelinepkg "github.com/xuanli520/p2r_tui/internal/pipeline"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/scanner"
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

//go:linkname staticReviewFindingsFromReport github.com/xuanli520/p2r_tui/internal/pipeline.staticReviewFindingsFromReport
func staticReviewFindingsFromReport(stage, report, sourcePath string) ([]model.Finding, error)

//go:linkname staticUnavailableReport github.com/xuanli520/p2r_tui/internal/pipeline.staticUnavailableReport
func staticUnavailableReport(stage, profile, projectPath, reason string) string

//go:linkname acceptanceFindings github.com/xuanli520/p2r_tui/internal/pipeline.acceptanceFindings
func acceptanceFindings(path string) []model.Finding

//go:linkname acceptanceScriptArgs github.com/xuanli520/p2r_tui/internal/pipeline.acceptanceScriptArgs
func acceptanceScriptArgs(outputs map[string]string, projectTypeArgs []string) []string

//go:linkname validationScriptArgs github.com/xuanli520/p2r_tui/internal/pipeline.validationScriptArgs
func validationScriptArgs(outputs map[string]string, projectTypeArgs []string) []string

//go:linkname runArtifactRoot github.com/xuanli520/p2r_tui/internal/pipeline.runArtifactRoot
func runArtifactRoot(scanPath string, project scanner.Project, runID string) string

//go:linkname copyPackageSnapshot github.com/xuanli520/p2r_tui/internal/pipeline.copyPackageSnapshot
func copyPackageSnapshot(source, dest string) error

//go:linkname terminalScreenshotLines github.com/xuanli520/p2r_tui/internal/pipeline.terminalScreenshotLines
func terminalScreenshotLines(text string) []string

//go:linkname safeCodexExtraArgs github.com/xuanli520/p2r_tui/internal/pipeline.safeCodexExtraArgs
func safeCodexExtraArgs(args []string) ([]string, error)

//go:linkname capabilitySummary github.com/xuanli520/p2r_tui/internal/pipeline.capabilitySummary
func capabilitySummary(capability codex.Capability) string

//go:linkname runCodexReviewSessionWithGuidance github.com/xuanli520/p2r_tui/internal/pipeline.runCodexReviewSessionWithGuidance
func runCodexReviewSessionWithGuidance(ctx context.Context, session pipelinepkg.CodexReviewSession, request pipelinepkg.CodexReviewRequest, deadlines []pipelinepkg.CodexGuidanceDeadline) (pipelinepkg.CodexReviewResult, error)

//go:linkname codexGuidanceSchedule github.com/xuanli520/p2r_tui/internal/pipeline.codexGuidanceSchedule
func codexGuidanceSchedule(timeout time.Duration, stage string) []pipelinepkg.CodexGuidanceDeadline

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
	for _, stage := range []string{"A", "B", "C"} {
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
	store, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertProjects(context.Background(), []scanner.Project{{TaskID: "TASK-STALE", Batch: "batch-1", Path: outer}}); err != nil {
		t.Fatal(err)
	}

	_, err = pipelinepkg.NewRunner(store, cfg).Run(context.Background(), "TASK-STALE", pipelinepkg.RunOptions{Stages: []string{"Z"}})
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
	if !strings.Contains(string(content), `"project_path": "`+canonical+`"`) || !strings.Contains(string(content), `"type": "stale_project_path"`) {
		t.Fatalf("manifest should contain canonical path and stale warning:\n%s", content)
	}
	if _, err := os.Stat(filepath.Join(run.ArtifactRoot, "logs", "path_warnings.log")); err != nil {
		t.Fatalf("path warning log should be written: %v", err)
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
			"Do not return a prose-only summary.",
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

func TestPromptProfilesUseFinalResponseContract(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "../../.."))
	for _, name := range []string{"tests_coverage_report.md", "static_acceptance_audit.md", "annotator_fix.md"} {
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
	}
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
	return projectPath
}
