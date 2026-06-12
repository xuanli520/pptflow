package pipeline_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xuanli520/p2r_tui/assets"
	browserpkg "github.com/xuanli520/p2r_tui/internal/browser"
	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/executor"
	pipelinepkg "github.com/xuanli520/p2r_tui/internal/pipeline"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/scanner"
)

func TestBrowserURLCandidatesUseLocalhostAllowlist(t *testing.T) {
	candidates := pipelinepkg.BrowserURLCandidatesForTest(pipelinepkg.TestRuntimeEvidence{
		Services: []string{"web", "api"},
		Mappings: map[string][]pipelinepkg.TestPortMapping{
			"web": {
				{Service: "web", URL: "0.0.0.0", Host: 38080, Container: 3000, Protocol: "tcp"},
				{Service: "web", URL: "localhost", Host: 0, Container: 9999, Protocol: "tcp"},
			},
			"api": {
				{Service: "api", URL: "[::]", Host: 39090, Container: 8080, Protocol: "tcp"},
			},
		},
		Probes: []pipelinepkg.TestProbeResult{
			{Service: "web", URL: "http://127.0.0.1:38080", OK: true, Status: 200},
			{Service: "api", URL: "http://localhost:39090", OK: false, Status: 500, Error: "server error"},
		},
	})
	if len(candidates) != 2 {
		t.Fatalf("candidates = %#v", candidates)
	}
	if candidates[0].URL != "http://127.0.0.1:38080" || candidates[1].URL != "http://127.0.0.1:39090" {
		t.Fatalf("URLs should be normalized to 127.0.0.1: %#v", candidates)
	}
	origins := pipelinepkg.BrowserAllowlistOriginsForTest(candidates)
	if strings.Join(origins, ",") != "http://127.0.0.1:38080,http://127.0.0.1:39090" {
		t.Fatalf("origins = %#v", origins)
	}
}

func TestBrowserURLCandidatesDoNotBorrowProbeAcrossPorts(t *testing.T) {
	candidates := pipelinepkg.BrowserURLCandidatesForTest(pipelinepkg.TestRuntimeEvidence{
		Services: []string{"web"},
		Mappings: map[string][]pipelinepkg.TestPortMapping{
			"web": {
				{Service: "web", URL: "0.0.0.0", Host: 3000, Container: 3000, Protocol: "tcp"},
				{Service: "web", URL: "0.0.0.0", Host: 3001, Container: 3001, Protocol: "tcp"},
			},
		},
		Probes: []pipelinepkg.TestProbeResult{
			{Service: "web", URL: "http://127.0.0.1:3000", OK: true, Status: 200},
		},
	})
	if len(candidates) != 2 {
		t.Fatalf("candidates = %#v", candidates)
	}
	if candidates[0].Source != "probe" || !candidates[0].ProbeOK {
		t.Fatalf("first candidate should carry exact successful probe evidence: %#v", candidates[0])
	}
	if candidates[1].Source != "mapping" || candidates[1].ProbeOK || candidates[1].ProbeStatus != 0 {
		t.Fatalf("second candidate should not borrow first probe evidence: %#v", candidates[1])
	}
}

func TestBrowserURLCandidatesDeduplicateEquivalentMappings(t *testing.T) {
	candidates := pipelinepkg.BrowserURLCandidatesForTest(pipelinepkg.TestRuntimeEvidence{
		Services: []string{"web"},
		Mappings: map[string][]pipelinepkg.TestPortMapping{
			"web": {
				{Service: "web", URL: "0.0.0.0", Host: 38080, Container: 80, Protocol: "tcp"},
				{Service: "web", URL: "0.0.0.0", Host: 38080, Container: 80, Protocol: "tcp"},
				{Service: "web", URL: "0.0.0.0", Host: 39090, Container: 8080, Protocol: "tcp"},
			},
		},
		Probes: []pipelinepkg.TestProbeResult{
			{Service: "web", URL: "http://127.0.0.1:38080", OK: true, Status: 200},
			{Service: "web", URL: "http://127.0.0.1:39090", OK: true, Status: 200},
		},
	})
	if len(candidates) != 2 {
		t.Fatalf("candidates = %#v", candidates)
	}
	if candidates[0].ID != "url_1" || candidates[1].ID != "url_2" {
		t.Fatalf("candidate IDs should remain contiguous after dedupe: %#v", candidates)
	}
	if candidates[0].URL != "http://127.0.0.1:38080" || candidates[1].URL != "http://127.0.0.1:39090" {
		t.Fatalf("unexpected URLs after dedupe: %#v", candidates)
	}
}

func TestStageBBlocksFrontendAndRuntimeDependents(t *testing.T) {
	got := pipelinepkg.BlockedDependentsForTest("B")
	want := []string{"G", "C"}
	if !slices.Equal(got, want) {
		t.Fatalf("blocked dependents = %#v, want %#v", got, want)
	}
}

func TestRunBlocksGAndCWhenStageBPreflightBlocked(t *testing.T) {
	root := t.TempDir()
	taskID := "TASK-20260604-B10C0D"
	projectPath := writePipelinePackage(t, root, "batch-1", taskID)
	cfg := config.Default()
	cfg.ScanPath = root
	ctx := context.Background()
	store := &runtimeBlockStore{project: scanner.Project{TaskID: taskID, Batch: "batch-1", Path: projectPath}}
	result, err := pipelinepkg.NewRunner(store, cfg, pipelinepkg.WithCommandRunner(runtimeBlockedRunner{})).Run(ctx, taskID, pipelinepkg.RunOptions{From: "B"})
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"B", "G", "C"} {
		record := stageByName(result.Stages, stage)
		if record.Status != model.StageBlocked {
			t.Fatalf("stage %s = %#v, want blocked", stage, record)
		}
	}
	for _, name := range []string{"frontend_e2e_summary.json", "frontend_e2e_report.md", "test_runtime_summary.json"} {
		if _, err := os.Stat(filepath.Join(result.Run.ArtifactRoot, name)); err != nil {
			t.Fatalf("expected blocked artifact %s: %v", name, err)
		}
	}
	content, err := os.ReadFile(filepath.Join(result.Run.ArtifactRoot, "frontend_e2e_summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var summary pipelinepkg.TestFrontendE2ESummary
	if err := json.Unmarshal(content, &summary); err != nil {
		t.Fatal(err)
	}
	if len(summary.Findings) != 1 || summary.Findings[0].Title != "Stage G blocked" {
		t.Fatalf("blocked summary findings = %#v", summary.Findings)
	}
}

func TestBrowserCodexEnvDoesNotInheritSecretsOrUserHomes(t *testing.T) {
	env := pipelinepkg.BrowserCodexEnvForTest([]string{
		"PATH=/usr/bin",
		"HOME=/home/user",
		"USERPROFILE=/home/user",
		"CODEX_HOME=/home/user/.codex",
		"CODEX_API_KEY=secret",
		"OPENAI_API_KEY=secret",
		"HTTP_PROXY=http://user:pass@proxy.local:8080",
		"TMPDIR=/tmp",
		"LANG=C.UTF-8",
	}, map[string]string{"P2R_EXPLICIT": "ok"}, "/opt/node/bin/node")
	joined := "\n" + strings.Join(env, "\n") + "\n"
	for _, key := range []string{"\nHOME=", "\nUSERPROFILE=", "\nCODEX_HOME=", "\nCODEX_API_KEY=", "\nOPENAI_API_KEY=", "\nHTTP_PROXY="} {
		if strings.Contains(joined, key) {
			t.Fatalf("browser Codex env leaked %s in %#v", strings.Trim(key, "\n="), env)
		}
	}
	for _, key := range []string{"\nPATH=", "\nTMPDIR=", "\nLANG=", "\nP2R_EXPLICIT="} {
		if !strings.Contains(joined, key) {
			t.Fatalf("browser Codex env missing %s in %#v", strings.Trim(key, "\n="), env)
		}
	}
}

func TestStageGBrowserContextHighlightsReadmeCredentials(t *testing.T) {
	projectPath := t.TempDir()
	repoPath := filepath.Join(projectPath, "repo")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatal(err)
	}
	readme := `# FieldOrder Pro

Demo credentials (also shown on the login screen):

| Username | Password | Role |
|----------|----------|------|
| admin    | admin123 | admin |
| rep1     | rep123   | rep |

End-to-end UI check: sign in as rep1/rep123 and open the catalog.
`
	if err := os.WriteFile(filepath.Join(repoPath, "README.md"), []byte(readme), 0o644); err != nil {
		t.Fatal(err)
	}

	contextText := pipelinepkg.StageGBrowserContextForTest(projectPath)
	for _, want := range []string{
		"BEGIN P2R BROWSER TEST HINTS",
		"Demo credentials",
		"admin123",
		"rep1/rep123",
		"BEGIN UNTRUSTED repo/README.md",
	} {
		if !strings.Contains(contextText, want) {
			t.Fatalf("context missing %q:\n%s", want, contextText)
		}
	}
}

func TestStageGBrowserContextResolvesReadmeReferencedEnvPassword(t *testing.T) {
	projectPath := t.TempDir()
	repoPath := filepath.Join(projectPath, "repo")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatal(err)
	}
	readme := `# TerraSync

### Default Credentials

On first startup the seed script consumes $ADMIN_BOOTSTRAP_PASSWORD from .env.
Seeded accounts include admin, engineer, and viewer.
`
	if err := os.WriteFile(filepath.Join(repoPath, "README.md"), []byte(readme), 0o644); err != nil {
		t.Fatal(err)
	}
	env := `POSTGRES_PASSWORD=do_not_surface
ADMIN_BOOTSTRAP_PASSWORD=ChangeMeOnFirstLogin!   # dev login password
`
	if err := os.WriteFile(filepath.Join(repoPath, ".env"), []byte(env), 0o644); err != nil {
		t.Fatal(err)
	}

	contextText := pipelinepkg.StageGBrowserContextForTest(projectPath)
	if !strings.Contains(contextText, "ADMIN_BOOTSTRAP_PASSWORD=ChangeMeOnFirstLogin!") {
		t.Fatalf("context should surface README-referenced login password:\n%s", contextText)
	}
	if strings.Contains(contextText, "POSTGRES_PASSWORD=do_not_surface") {
		t.Fatalf("context should not surface unrelated database password:\n%s", contextText)
	}
}

func TestBrowserActionPromptTemplateRendersFromPromptProfileAsset(t *testing.T) {
	content, err := assets.FS.ReadFile("prompt_profiles/frontend_e2e_browser_action_prompt.md")
	if err != nil {
		t.Fatal(err)
	}
	contextText := "README-derived browser test hints: rep1/rep123"
	prompt, err := pipelinepkg.BrowserActionPromptForTest(string(content), "profile text", contextText)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Run p2r stage G as a browser E2E planner.",
		"passed conclusions need strong distinct browser evidence",
		"browser action tool errors",
		"Authentication is not a universal success requirement",
		"Do not use fill_input retries",
		"Do not click logout",
		"fill every visible username/email/account field and every password field before submitting",
		"same login, CAPTCHA, or registration state remains",
		"\"id\": \"url_1\"",
		"README-derived browser test hints",
		"rep1/rep123",
		"profile text",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("rendered prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "{{.") {
		t.Fatalf("rendered prompt still contains template placeholders:\n%s", prompt)
	}
}

func TestStageGFinishRequiresMinimumBrowserScreenshots(t *testing.T) {
	root := t.TempDir()
	observations := make([]pipelinepkg.TestBrowserObservation, 0, 5)
	for index := 0; index < 4; index++ {
		observations = append(observations, pipelinepkg.TestBrowserObservation{
			Action:         "snapshot",
			OK:             true,
			CurrentURL:     fmt.Sprintf("http://127.0.0.1:5173/state-%02d", index),
			VisibleText:    fmt.Sprintf("Business state %02d", index),
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "runtime", fmt.Sprintf("shot-%02d.png", index))),
		})
	}
	reason := pipelinepkg.StageGFinishScreenshotBlockReasonForTest(observations)
	if !strings.Contains(reason, "at least 5 browser screenshots") || !strings.Contains(reason, "currently captured 4") {
		t.Fatalf("unexpected finish block reason: %q", reason)
	}
	observations = append(observations, pipelinepkg.TestBrowserObservation{
		Action:         "snapshot",
		OK:             true,
		CurrentURL:     "http://127.0.0.1:5173/state-04",
		VisibleText:    "Business state 04",
		ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "runtime", "shot-04.png")),
	})
	if reason := pipelinepkg.StageGFinishScreenshotBlockReasonForTest(observations); reason != "" {
		t.Fatalf("finish should be allowed after five screenshots, got %q", reason)
	}
}

func TestStageGFinishAllowsLimitedScreenshotsForProductBlocker(t *testing.T) {
	root := t.TempDir()
	loginURL := "http://127.0.0.1:5173/api/auth/login"
	observations := []pipelinepkg.TestBrowserObservation{
		{
			Action:         "open_candidate",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/login",
			VisibleText:    "Login CAPTCHA",
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "open.png")),
		},
		{
			Action:         "click_button",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/login",
			VisibleText:    "Invalid or expired CAPTCHA Login CAPTCHA",
			NetworkIssues:  []browserpkg.NetworkIssue{{URL: loginURL, Status: 400}},
			NetworkEvents:  []browserpkg.NetworkEvent{{URL: loginURL, Method: "POST", Status: 400}},
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "captcha.png")),
		},
	}
	passed := pipelinepkg.TestFrontendE2ESummary{Status: "passed"}
	if reason := pipelinepkg.StageGFinishScreenshotBlockReasonForSummaryForTest(passed, observations); reason == "" {
		t.Fatal("passed summary should still require five key screenshots")
	}
	failed := pipelinepkg.TestFrontendE2ESummary{
		Status: "failed",
		Findings: []pipelinepkg.FrontendE2EFinding{{
			Severity: "High",
			Title:    "CAPTCHA blocks README credentials",
		}},
	}
	if reason := pipelinepkg.StageGFinishScreenshotBlockReasonForSummaryForTest(failed, observations); reason != "" {
		t.Fatalf("failed product blocker should allow limited screenshot finish, got %q", reason)
	}
}

func TestStageGFinishAllowsLimitedScreenshotsForAuthGateStall(t *testing.T) {
	root := t.TempDir()
	observations := []pipelinepkg.TestBrowserObservation{
		{
			Action:         "open_candidate",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/login",
			VisibleText:    "AvatarForge Studio Sign In Email Password",
			Controls:       loginControls(false, false),
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "open.png")),
		},
		{
			Action:      "fill_input",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/login",
			VisibleText: "AvatarForge Studio Sign In Email Password",
			Controls:    loginControls(true, false),
		},
		{
			Action:      "fill_input",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/login",
			VisibleText: "AvatarForge Studio Sign In Email Password",
			Controls:    loginControls(true, true),
		},
		{
			Action:         "click_button",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/login",
			VisibleText:    "AvatarForge Studio Sign In Email Password",
			Controls:       loginControls(true, true),
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "submit-1.png")),
		},
		{
			Action:         "collect_network",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/login",
			VisibleText:    "AvatarForge Studio Sign In Email Password",
			Controls:       loginControls(true, true),
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "network-1.png")),
		},
		{
			Action:         "click_button",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/login",
			VisibleText:    "AvatarForge Studio Sign In Email Password",
			Controls:       loginControls(true, true),
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "submit-2.png")),
		},
	}
	summary := pipelinepkg.TestFrontendE2ESummary{
		Status: "failed",
		Findings: []pipelinepkg.FrontendE2EFinding{{
			Severity: "High",
			Title:    "Login never reaches the dashboard",
		}},
	}
	if reason := pipelinepkg.StageGFinishScreenshotBlockReasonForSummaryForTest(summary, observations); reason != "" {
		t.Fatalf("auth-gate stall should allow limited screenshot finish, got %q", reason)
	}
}

func TestStageGPartialProductBlockerFindingDetectsAuthGate(t *testing.T) {
	loginURL := "http://127.0.0.1:5173/api/auth/login"
	observations := []pipelinepkg.TestBrowserObservation{
		{
			Action:      "open_candidate",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/login",
			VisibleText: "Username Password CAPTCHA Sign In Register",
		},
		{
			Action:        "click_button",
			OK:            true,
			CurrentURL:    "http://127.0.0.1:5173/login",
			VisibleText:   "Invalid or expired CAPTCHA Username Password CAPTCHA Sign In Register",
			NetworkIssues: []browserpkg.NetworkIssue{{URL: loginURL, Status: 400}},
			NetworkEvents: []browserpkg.NetworkEvent{{URL: loginURL, Method: "POST", Status: 400}},
		},
		{
			Action:      "fill_input",
			OK:          false,
			CurrentURL:  "http://127.0.0.1:5173/login",
			VisibleText: "Username Password CAPTCHA Sign In Register",
			Error:       "locator.fill: Timeout 5000ms exceeded",
		},
	}
	finding, ok := pipelinepkg.StageGPartialProductBlockerFindingForTest(observations, "Stage G timeout reached.")
	if !ok {
		t.Fatal("expected auth gate product blocker finding")
	}
	if finding.Title != "Authentication gate prevented browser workflow coverage" || !strings.Contains(finding.Evidence, "status=400") {
		t.Fatalf("unexpected finding: %#v", finding)
	}
	observations = append(observations, pipelinepkg.TestBrowserObservation{
		Action:        "click_button",
		OK:            true,
		CurrentURL:    "http://127.0.0.1:5173/dashboard",
		VisibleText:   "Dashboard",
		NetworkEvents: []browserpkg.NetworkEvent{{URL: loginURL, Method: "POST", Status: 200}},
	})
	if finding, ok := pipelinepkg.StageGPartialProductBlockerFindingForTest(observations, "timeout"); ok {
		t.Fatalf("recovered auth failure should not be a blocker: %#v", finding)
	}
}

func TestStageGNativeDialogBoundarySupportsNonPassedConclusion(t *testing.T) {
	observations := []pipelinepkg.TestBrowserObservation{
		{
			Action:      "click_button",
			OK:          false,
			CurrentURL:  "http://127.0.0.1:5173/editor/1",
			Title:       "SketchPad Studio",
			VisibleText: "Editor + Page Components Preview",
			Error:       "native prompt dismissed because action.value was not supplied",
			Metadata: map[string]string{
				"p2r_dialog_type":           "prompt",
				"p2r_dialog_message":        "Page title?",
				"p2r_dialog_default_value":  "Page 1",
				"p2r_dialog_action":         "dismissed",
				"p2r_dialog_reason":         "missing_action_value",
				"p2r_dialog_value_supplied": "false",
			},
		},
	}
	evidence := pipelinepkg.StageGNativeDialogBoundaryEvidenceForTest(observations)
	if !strings.Contains(evidence, "native prompt dialog") || !strings.Contains(evidence, "Page title?") {
		t.Fatalf("unexpected native dialog evidence: %q", evidence)
	}
	finding, ok := pipelinepkg.StageGPartialProductBlockerFindingForTest(observations, "planner could not infer a safe dialog value")
	if !ok {
		t.Fatal("expected native dialog boundary finding")
	}
	if finding.Title != "Native browser dialog required explicit model input" {
		t.Fatalf("unexpected finding: %#v", finding)
	}
	summary := pipelinepkg.TestFrontendE2ESummary{
		Status: "blocked",
		Findings: []pipelinepkg.FrontendE2EFinding{{
			Severity: "High",
			Title:    finding.Title,
		}},
	}
	if reason := pipelinepkg.StageGFinishScreenshotBlockReasonForSummaryForTest(summary, observations); reason != "" {
		t.Fatalf("native dialog boundary should allow limited screenshot finish, got %q", reason)
	}
}

func TestStageGAuthGateProtectedAPI401DoesNotBecomeProductFailure(t *testing.T) {
	observations := []pipelinepkg.TestBrowserObservation{
		{
			Action:        "open_candidate",
			OK:            true,
			CurrentURL:    "http://127.0.0.1:18080/login",
			Title:         "FreightNest - Warehouse Management",
			VisibleText:   "FreightNest Sign In Email Password",
			ConsoleErrors: []string{"Failed to load resource: the server responded with a status of 401 (Unauthorized)"},
			NetworkIssues: []browserpkg.NetworkIssue{
				{URL: "http://127.0.0.1:18080/api/dashboard", Status: 401},
			},
			NetworkEvents: []browserpkg.NetworkEvent{
				{URL: "http://127.0.0.1:18080/api/dashboard", Method: "GET", Status: 401, ResourceType: "fetch"},
			},
		},
	}
	if finding, ok := pipelinepkg.StageGPartialProductBlockerFindingForTest(observations, "after opening login"); ok {
		t.Fatalf("auth-gate protected API 401 should not be product blocker: %#v", finding)
	}
	if summary, ok := pipelinepkg.StageGPositiveEvidenceOutcomeForTest(nil, observations, nil, "after opening login"); ok {
		t.Fatalf("auth-gate login shell should not pass either: %#v", summary)
	}
}

func TestStageGPositiveEvidenceOutcomePassesAuthenticatedBusinessFlow(t *testing.T) {
	root := t.TempDir()
	candidates := []pipelinepkg.TestBrowserURLCandidate{{ID: "url_1", URL: "http://127.0.0.1:5173", Origin: "http://127.0.0.1:5173", ProbeOK: true}}
	observations := []pipelinepkg.TestBrowserObservation{
		{
			Action:         "open_candidate",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/login",
			Title:          "Sign in",
			VisibleText:    "Sign in Email Password",
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "login.png")),
		},
		{
			Action:      "fill_input",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/login",
			Title:       "Sign in",
			VisibleText: "Sign in Email Password",
			Controls:    loginControls(true, false),
		},
		{
			Action:      "fill_input",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/login",
			Title:       "Sign in",
			VisibleText: "Sign in Email Password",
			Controls:    loginControls(true, true),
		},
		{
			Action:      "click_button",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/admin/dashboard",
			Title:       "Admin Dashboard",
			VisibleText: "Admin Dashboard User Management Analytics Settings",
			NetworkEvents: []browserpkg.NetworkEvent{
				{URL: "http://127.0.0.1:5173/api/login", Method: "POST", Status: 200, ResourceType: "xhr"},
				{URL: "http://127.0.0.1:5173/api/users", Method: "GET", Status: 200, ResourceType: "xhr"},
				{URL: "http://127.0.0.1:5173/api/analytics", Method: "GET", Status: 200, ResourceType: "xhr"},
			},
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "dashboard.png")),
		},
	}
	summary, ok := pipelinepkg.StageGPositiveEvidenceOutcomeForTest(candidates, observations, nil, "planner timeout")
	if !ok {
		t.Fatal("expected authenticated business evidence to finish Stage G")
	}
	notes := strings.Join(summary.Notes, "\n")
	if summary.Status != "passed" || !strings.Contains(notes, "auth_success=true") || !strings.Contains(notes, "business_network_endpoints=2") {
		t.Fatalf("unexpected summary: %#v", summary)
	}
}

func TestStageGPositiveEvidenceOutcomeRequiresTwoBusinessEndpoints(t *testing.T) {
	root := t.TempDir()
	observations := []pipelinepkg.TestBrowserObservation{
		{
			Action:         "open_candidate",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/login",
			VisibleText:    "Sign in Email Password",
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "login.png")),
		},
		{
			Action:      "fill_input",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/login",
			Title:       "Sign in",
			VisibleText: "Sign in Email Password",
		},
		{
			Action:      "click_button",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/account",
			Title:       "Account",
			VisibleText: "Account page",
			NetworkEvents: []browserpkg.NetworkEvent{
				{URL: "http://127.0.0.1:5173/api/login", Method: "POST", Status: 200, ResourceType: "xhr"},
				{URL: "http://127.0.0.1:5173/api/users", Method: "GET", Status: 200, ResourceType: "xhr"},
			},
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "dashboard.png")),
		},
	}
	if summary, ok := pipelinepkg.StageGPositiveEvidenceOutcomeForTest(nil, observations, nil, "planner timeout"); ok {
		t.Fatalf("one business endpoint must not finish Stage G: %#v", summary)
	}
	observations[2].NetworkEvents = append(observations[2].NetworkEvents, browserpkg.NetworkEvent{URL: "http://127.0.0.1:5173/api/logout", Method: "POST", Status: 200, ResourceType: "xhr"})
	if summary, ok := pipelinepkg.StageGPositiveEvidenceOutcomeForTest(nil, observations, nil, "planner timeout"); ok {
		t.Fatalf("logout must not count as business evidence: %#v", summary)
	}
}

func TestStageGPositiveEvidenceOutcomeRejectsPostAuthSessionLoss(t *testing.T) {
	root := t.TempDir()
	observations := []pipelinepkg.TestBrowserObservation{
		{
			Action:         "open_candidate",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/login",
			Title:          "Sign in",
			VisibleText:    "Sign in Email Password",
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "login.png")),
		},
		{
			Action:      "fill_input",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/login",
			Title:       "Sign in",
			VisibleText: "Sign in Email Password",
			Controls:    loginControls(true, false),
		},
		{
			Action:      "fill_input",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/login",
			Title:       "Sign in",
			VisibleText: "Sign in Email Password",
			Controls:    loginControls(true, true),
		},
		{
			Action:      "click_button",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/admin/dashboard",
			Title:       "Admin Dashboard",
			VisibleText: "Admin Dashboard User Management Analytics Settings",
			NetworkEvents: []browserpkg.NetworkEvent{
				{URL: "http://127.0.0.1:5173/api/login", Method: "POST", Status: 200, ResourceType: "xhr"},
				{URL: "http://127.0.0.1:5173/api/users", Method: "GET", Status: 200, ResourceType: "xhr"},
				{URL: "http://127.0.0.1:5173/api/analytics", Method: "GET", Status: 200, ResourceType: "xhr"},
			},
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "dashboard.png")),
		},
		{
			Action:         "click_navigation",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/login",
			Title:          "Sign in",
			VisibleText:    "Sign in Email Password",
			NetworkEvents:  []browserpkg.NetworkEvent{{URL: "http://127.0.0.1:5173/api/logout", Method: "POST", Status: 200, ResourceType: "xhr"}},
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "logout.png")),
		},
	}
	if summary, ok := pipelinepkg.StageGPositiveEvidenceOutcomeForTest(nil, observations, nil, "planner timeout"); ok {
		t.Fatalf("post-auth session loss must not pass Stage G: %#v", summary)
	}
	finding, ok := pipelinepkg.StageGPartialProductBlockerFindingForTest(observations, "planner timeout")
	if !ok || finding.Title != "Authenticated browser session was lost during Stage G" {
		t.Fatalf("expected session-loss finding, got ok=%t finding=%#v", ok, finding)
	}
	if reason := pipelinepkg.StageGObservationStopReasonForTest(observations); !strings.Contains(reason, "authenticated session was lost") {
		t.Fatalf("expected session-loss stop reason, got %q", reason)
	}
}

func TestStageGBusinessEvidenceIgnoresAuthAndSessionUtilityEndpoints(t *testing.T) {
	root := t.TempDir()
	observations := []pipelinepkg.TestBrowserObservation{
		{
			Action:         "open_candidate",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/login",
			VisibleText:    "Sign in Email Password",
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "login.png")),
		},
		{
			Action:      "click_button",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/admin/dashboard",
			VisibleText: "Admin Dashboard Users Modules Settings",
			NetworkEvents: []browserpkg.NetworkEvent{
				{URL: "http://127.0.0.1:5173/api/login", Method: "POST", Status: 200, ResourceType: "xhr"},
				{URL: "http://127.0.0.1:5173/api/auth/me", Method: "GET", Status: 200, ResourceType: "xhr"},
				{URL: "http://127.0.0.1:5173/api/settings/session-timeout", Method: "GET", Status: 200, ResourceType: "xhr"},
				{URL: "http://127.0.0.1:5173/api/users", Method: "GET", Status: 200, ResourceType: "xhr"},
			},
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "dashboard.png")),
		},
		{
			Action:         "snapshot",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/admin/dashboard",
			VisibleText:    "Admin Dashboard Users Modules Settings",
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "dashboard-2.png")),
		},
	}
	if summary, ok := pipelinepkg.StageGPositiveEvidenceOutcomeForTest(nil, observations, nil, "planner timeout"); ok {
		t.Fatalf("auth/session utility endpoints plus one business endpoint must not pass: %#v", summary)
	}
	observations[1].NetworkEvents = append(observations[1].NetworkEvents, browserpkg.NetworkEvent{URL: "http://127.0.0.1:5173/api/modules", Method: "GET", Status: 200, ResourceType: "xhr"})
	summary, ok := pipelinepkg.StageGPositiveEvidenceOutcomeForTest(nil, observations, nil, "planner timeout")
	if !ok {
		t.Fatal("expected two real business endpoints to pass")
	}
	if !strings.Contains(strings.Join(summary.Notes, "\n"), "business_network_endpoints=2") {
		t.Fatalf("unexpected business endpoint count: %#v", summary.Notes)
	}
}

func TestStageGPostAuthSessionLossDetectsReturnedLoginAfterSelectorTimeout(t *testing.T) {
	observations := []pipelinepkg.TestBrowserObservation{
		{
			Action:      "click_button",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/admin",
			Title:       "Admin Dashboard",
			VisibleText: "Admin Dashboard Projects Users",
			NetworkEvents: []browserpkg.NetworkEvent{
				{URL: "http://127.0.0.1:5173/api/v1/auth/login", Method: "POST", Status: 200, ResourceType: "fetch"},
				{URL: "http://127.0.0.1:5173/api/v1/projects", Method: "GET", Status: 200, ResourceType: "fetch"},
				{URL: "http://127.0.0.1:5173/api/v1/users", Method: "GET", Status: 200, ResourceType: "fetch"},
			},
		},
		{
			Action:      "click_navigation",
			OK:          false,
			CurrentURL:  "http://127.0.0.1:5173/login",
			Title:       "Sign In",
			VisibleText: "Sign In Email Password",
			Error:       "locator.click: Timeout 5000ms exceeded",
		},
	}
	finding, ok := pipelinepkg.StageGPartialProductBlockerFindingForTest(observations, "planner timeout")
	if !ok || finding.Title != "Authenticated browser session was lost during Stage G" {
		t.Fatalf("expected returned-login session-loss finding, got ok=%t finding=%#v", ok, finding)
	}
	if reason := pipelinepkg.StageGObservationStopReasonForTest(observations); !strings.Contains(reason, "authenticated session was lost") {
		t.Fatalf("expected session-loss stop reason, got %q", reason)
	}
}

func TestStageGPositiveEvidenceOutcomeUsesSupportScreenshotsWhenKeyStatesDeduplicate(t *testing.T) {
	root := t.TempDir()
	observations := []pipelinepkg.TestBrowserObservation{
		{
			Action:      "click_button",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/admin/dashboard",
			Title:       "Admin Dashboard",
			VisibleText: "Admin Dashboard User Management Analytics Settings",
			NetworkEvents: []browserpkg.NetworkEvent{
				{URL: "http://127.0.0.1:5173/api/login", Method: "POST", Status: 200, ResourceType: "xhr"},
				{URL: "http://127.0.0.1:5173/api/users", Method: "GET", Status: 200, ResourceType: "xhr"},
				{URL: "http://127.0.0.1:5173/api/analytics", Method: "GET", Status: 200, ResourceType: "xhr"},
			},
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "dashboard-1.png")),
		},
		{
			Action:         "snapshot",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/admin/dashboard",
			Title:          "Admin Dashboard",
			VisibleText:    "Admin Dashboard User Management Analytics Settings",
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "dashboard-2.png")),
		},
	}
	if count := len(pipelinepkg.StageGKeyScreenshotObservationIndexesForTest(observations)); count != 1 {
		t.Fatalf("test fixture should deduplicate to one key screenshot, got %d", count)
	}
	summary, ok := pipelinepkg.StageGPositiveEvidenceOutcomeForTest(nil, observations, nil, "planner timeout")
	if !ok {
		t.Fatal("expected support screenshots to allow deterministic pass")
	}
	notes := strings.Join(summary.Notes, "\n")
	if !strings.Contains(notes, "support_screenshots=2") || !strings.Contains(notes, "key_screenshots=1") {
		t.Fatalf("unexpected evidence note: %s", notes)
	}
}

func TestStageGPositiveEvidenceOutcomePassesAuthenticatedBusinessUIWithoutAPIs(t *testing.T) {
	root := t.TempDir()
	observations := []pipelinepkg.TestBrowserObservation{
		{
			Action:         "open_candidate",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/login",
			Title:          "Sign in",
			VisibleText:    "Sign in Email Password",
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "login.png")),
		},
		{
			Action:      "fill_input",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/login",
			Title:       "Sign in",
			VisibleText: "Sign in Email Password",
			Controls:    loginControls(true, false),
		},
		{
			Action:      "fill_input",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/login",
			Title:       "Sign in",
			VisibleText: "Sign in Email Password",
			Controls:    loginControls(true, true),
		},
		{
			Action:      "click_button",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/admin/dashboard",
			Title:       "Admin Dashboard",
			VisibleText: "Admin Dashboard User Management Analytics Settings Reports",
			NetworkEvents: []browserpkg.NetworkEvent{
				{URL: "http://127.0.0.1:5173/_blazor/negotiate?negotiateVersion=1", Method: "POST", Status: 200, ResourceType: "fetch"},
			},
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "dashboard.png")),
		},
		{
			Action:         "snapshot",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/admin/reports",
			Title:          "Admin Reports",
			VisibleText:    "Admin Reports Analytics Settings User Management Export Save",
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "reports.png")),
		},
	}
	summary, ok := pipelinepkg.StageGPositiveEvidenceOutcomeForTest(nil, observations, nil, "planner timeout")
	if !ok {
		t.Fatal("expected authenticated business UI evidence to finish Stage G")
	}
	notes := strings.Join(summary.Notes, "\n")
	if !strings.Contains(notes, "business_network_endpoints=0") || !strings.Contains(notes, "business_ui_signals=") || !strings.Contains(notes, "distinct_states=2") {
		t.Fatalf("unexpected evidence note: %s", notes)
	}
}

func TestStageGPositiveEvidenceOutcomePassesAuthenticatedInteractiveUIWithoutDomainKeywords(t *testing.T) {
	root := t.TempDir()
	observations := []pipelinepkg.TestBrowserObservation{
		{
			Action:         "open_candidate",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/login",
			Title:          "Sign in",
			VisibleText:    "Sign in Email Password",
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "login.png")),
		},
		{
			Action:      "fill_input",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/login",
			Title:       "Sign in",
			VisibleText: "Sign in Email Password",
			Controls:    loginControls(true, false),
		},
		{
			Action:      "fill_input",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/login",
			Title:       "Sign in",
			VisibleText: "Sign in Email Password",
			Controls:    loginControls(true, true),
		},
		{
			Action:      "click_button",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/workspace",
			Title:       "Northwind Desk",
			VisibleText: "Northwind Desk Today Queue Assigned Items Waiting Review Recent Activity Search Filter Owner Priority Due Date",
			Controls: []browserpkg.ControlSummary{
				{Role: "link", Text: "Queue"},
				{Role: "link", Text: "Calendar"},
				{Role: "link", Text: "People"},
				{Role: "button", Text: "Add Item"},
				{Role: "button", Text: "Export"},
				{Role: "input", Placeholder: "Search"},
				{Role: "input", Type: "select", Name: "priority"},
			},
			NetworkEvents:  []browserpkg.NetworkEvent{{URL: "http://127.0.0.1:5173/auth/login", Method: "POST", Status: 200, ResourceType: "fetch"}},
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "workspace.png")),
		},
		{
			Action:         "snapshot",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/workspace/review",
			Title:          "Northwind Desk",
			VisibleText:    "Northwind Desk Review Queue Pending Approval Assigned Items Recent Activity Search Filter Owner Status Due Date",
			Controls:       []browserpkg.ControlSummary{{Role: "link", Text: "Queue"}, {Role: "link", Text: "Review"}, {Role: "button", Text: "Approve"}, {Role: "button", Text: "Export"}, {Role: "input", Placeholder: "Search"}, {Role: "input", Type: "select", Name: "status"}},
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "workspace-review.png")),
		},
	}
	summary, ok := pipelinepkg.StageGPositiveEvidenceOutcomeForTest(nil, observations, nil, "planner timeout")
	if !ok {
		t.Fatal("expected authenticated interactive UI evidence to finish Stage G")
	}
	notes := strings.Join(summary.Notes, "\n")
	if !strings.Contains(notes, "business_network_endpoints=0") || !strings.Contains(notes, "interactive_product_states=2") || !strings.Contains(notes, "product_navigation_changes=1") {
		t.Fatalf("unexpected evidence note: %s", notes)
	}
}

func TestStageGPositiveEvidenceOutcomePassesPublicInteractiveUIWithoutAuth(t *testing.T) {
	root := t.TempDir()
	observations := []pipelinepkg.TestBrowserObservation{
		{
			Action:      "open_candidate",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/workspace",
			Title:       "Northwind Desk",
			VisibleText: "Northwind Desk Today Queue Assigned Items Waiting Review Recent Activity Search Filter Owner Priority Due Date",
			Controls: []browserpkg.ControlSummary{
				{Role: "link", Text: "Queue"},
				{Role: "link", Text: "Calendar"},
				{Role: "link", Text: "People"},
				{Role: "button", Text: "Add Item"},
				{Role: "button", Text: "Export"},
				{Role: "input", Placeholder: "Search"},
				{Role: "input", Type: "select", Name: "priority"},
			},
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "workspace.png")),
		},
		{
			Action:      "click_navigation",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/workspace/review",
			Title:       "Northwind Desk",
			VisibleText: "Northwind Desk Review Queue Pending Approval Assigned Items Recent Activity Search Filter Owner Status Due Date",
			Controls: []browserpkg.ControlSummary{
				{Role: "link", Text: "Queue"},
				{Role: "link", Text: "Review"},
				{Role: "button", Text: "Approve"},
				{Role: "button", Text: "Export"},
				{Role: "input", Placeholder: "Search"},
				{Role: "input", Type: "select", Name: "status"},
			},
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "workspace-review.png")),
		},
	}
	summary, ok := pipelinepkg.StageGPositiveEvidenceOutcomeForTest(nil, observations, nil, "planner timeout")
	if !ok {
		t.Fatal("expected public interactive product UI evidence to finish Stage G without auth")
	}
	notes := strings.Join(summary.Notes, "\n")
	if summary.Status != "passed" || strings.Contains(notes, "auth_success=true") || !strings.Contains(notes, "interactive_product_states=2") {
		t.Fatalf("unexpected public UI evidence summary: %#v", summary)
	}
}

func TestStageGPositiveEvidenceOutcomeRejectsSingleInteractiveUIState(t *testing.T) {
	root := t.TempDir()
	workspace := pipelinepkg.TestBrowserObservation{
		Action:      "click_button",
		OK:          true,
		CurrentURL:  "http://127.0.0.1:5173/workspace",
		Title:       "Northwind Desk",
		VisibleText: "Northwind Desk Today Queue Assigned Items Waiting Review Recent Activity Search Filter Owner Priority Due Date",
		Controls: []browserpkg.ControlSummary{
			{Role: "link", Text: "Queue"},
			{Role: "link", Text: "Calendar"},
			{Role: "link", Text: "People"},
			{Role: "button", Text: "Add Item"},
			{Role: "button", Text: "Export"},
			{Role: "input", Placeholder: "Search"},
			{Role: "input", Type: "select", Name: "priority"},
		},
		NetworkEvents:  []browserpkg.NetworkEvent{{URL: "http://127.0.0.1:5173/auth/login", Method: "POST", Status: 200, ResourceType: "fetch"}},
		ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "workspace.png")),
	}
	workspaceCopy := workspace
	workspaceCopy.Action = "snapshot"
	workspaceCopy.NetworkEvents = nil
	workspaceCopy.ScreenshotPath = writeTinyPNG(t, filepath.Join(root, "workspace-2.png"))
	observations := []pipelinepkg.TestBrowserObservation{
		{
			Action:         "open_candidate",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/login",
			Title:          "Sign in",
			VisibleText:    "Sign in Email Password",
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "login.png")),
		},
		{
			Action:      "fill_input",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/login",
			Title:       "Sign in",
			VisibleText: "Sign in Email Password",
			Controls:    loginControls(true, false),
		},
		{
			Action:      "fill_input",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/login",
			Title:       "Sign in",
			VisibleText: "Sign in Email Password",
			Controls:    loginControls(true, true),
		},
		workspace,
		workspaceCopy,
	}
	if summary, ok := pipelinepkg.StageGPositiveEvidenceOutcomeForTest(nil, observations, nil, "planner timeout"); ok {
		t.Fatalf("single interactive UI state must not finish Stage G: %#v", summary)
	}
}

func TestStageGPositiveEvidenceOutcomeRejectsSingleNamedUIStateWithoutAPIs(t *testing.T) {
	root := t.TempDir()
	dashboard := pipelinepkg.TestBrowserObservation{
		Action:      "click_button",
		OK:          true,
		CurrentURL:  "http://127.0.0.1:5173/admin/dashboard",
		Title:       "Admin Dashboard",
		VisibleText: "Admin Dashboard User Management Analytics Settings Reports",
		NetworkEvents: []browserpkg.NetworkEvent{
			{URL: "http://127.0.0.1:5173/_blazor/negotiate?negotiateVersion=1", Method: "POST", Status: 200, ResourceType: "fetch"},
		},
		ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "dashboard.png")),
	}
	dashboardCopy := dashboard
	dashboardCopy.Action = "snapshot"
	dashboardCopy.NetworkEvents = nil
	dashboardCopy.ScreenshotPath = writeTinyPNG(t, filepath.Join(root, "dashboard-2.png"))
	observations := []pipelinepkg.TestBrowserObservation{
		{
			Action:         "open_candidate",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/login",
			Title:          "Sign in",
			VisibleText:    "Sign in Email Password",
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "login.png")),
		},
		{
			Action:      "fill_input",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/login",
			Title:       "Sign in",
			VisibleText: "Sign in Email Password",
			Controls:    loginControls(true, false),
		},
		{
			Action:      "fill_input",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/login",
			Title:       "Sign in",
			VisibleText: "Sign in Email Password",
			Controls:    loginControls(true, true),
		},
		dashboard,
		dashboardCopy,
	}
	if summary, ok := pipelinepkg.StageGPositiveEvidenceOutcomeForTest(nil, observations, nil, "planner timeout"); ok {
		t.Fatalf("single named UI state without APIs must not finish Stage G: %#v", summary)
	}
}

func TestStageGPositiveEvidenceOutcomeRejectsDashboardAuthFormShell(t *testing.T) {
	root := t.TempDir()
	observations := []pipelinepkg.TestBrowserObservation{
		{
			Action:         "open_candidate",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/login",
			Title:          "Sign in",
			VisibleText:    "Sign in Email Password",
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "login.png")),
		},
		{
			Action:      "fill_input",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/login",
			Title:       "Sign in",
			VisibleText: "Sign in Email Password",
			Controls:    loginControls(true, false),
		},
		{
			Action:      "fill_input",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/login",
			Title:       "Sign in",
			VisibleText: "Sign in Email Password",
			Controls:    loginControls(true, true),
		},
		{
			Action:      "click_button",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/admin/dashboard",
			Title:       "Admin Dashboard",
			VisibleText: "Admin Dashboard User Management Analytics Settings Reports Sign In Email Password",
			Controls:    loginControls(true, true),
			NetworkEvents: []browserpkg.NetworkEvent{
				{URL: "http://127.0.0.1:5173/api/login", Method: "POST", Status: 200, ResourceType: "xhr"},
			},
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "dashboard-shell.png")),
		},
	}
	if summary, ok := pipelinepkg.StageGPositiveEvidenceOutcomeForTest(nil, observations, nil, "planner timeout"); ok {
		t.Fatalf("dashboard auth form shell must not finish Stage G: %#v", summary)
	}
}

func TestStageGPositiveEvidenceOutcomeRejectsFillAttemptsWithoutCredentialState(t *testing.T) {
	root := t.TempDir()
	observations := []pipelinepkg.TestBrowserObservation{
		{
			Action:         "open_candidate",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/login",
			Title:          "Sign in",
			VisibleText:    "Sign in Email Password",
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "login.png")),
		},
		{
			Action:      "fill_input",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/login",
			Title:       "Sign in",
			VisibleText: "Sign in Email Password",
		},
		{
			Action:      "fill_input",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/login",
			Title:       "Sign in",
			VisibleText: "Sign in Email Password",
		},
		{
			Action:         "click_button",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/admin/dashboard",
			Title:          "Admin Dashboard",
			VisibleText:    "Admin Dashboard User Management Analytics Settings Reports",
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "dashboard.png")),
		},
	}
	if summary, ok := pipelinepkg.StageGPositiveEvidenceOutcomeForTest(nil, observations, nil, "planner timeout"); ok {
		t.Fatalf("fill attempts without credential state must not finish Stage G: %#v", summary)
	}
}

func TestStageGPositiveEvidenceOutcomeRejectsUnauthenticatedDashboardShell(t *testing.T) {
	root := t.TempDir()
	observations := []pipelinepkg.TestBrowserObservation{
		{
			Action:         "open_candidate",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/admin/dashboard",
			Title:          "Admin Dashboard",
			VisibleText:    "Admin Dashboard User Management Analytics Settings Reports",
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "dashboard-1.png")),
		},
		{
			Action:         "snapshot",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/admin/dashboard",
			Title:          "Admin Dashboard",
			VisibleText:    "Admin Dashboard User Management Analytics Settings Reports",
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "dashboard-2.png")),
		},
	}
	if summary, ok := pipelinepkg.StageGPositiveEvidenceOutcomeForTest(nil, observations, nil, "planner timeout"); ok {
		t.Fatalf("dashboard shell without auth evidence must not finish Stage G: %#v", summary)
	}
}

func TestStageGFrameworkNetworkNoiseDoesNotCountAsBusinessEvidence(t *testing.T) {
	root := t.TempDir()
	observations := []pipelinepkg.TestBrowserObservation{
		{
			Action:         "open_candidate",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5010/",
			Title:          "ShiftForge Pro",
			VisibleText:    "Welcome to ShiftForge",
			NetworkEvents:  []browserpkg.NetworkEvent{{URL: "http://127.0.0.1:5010/_blazor/negotiate?negotiateVersion=1", Method: "POST", Status: 200, ResourceType: "fetch"}},
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "home.png")),
		},
		{
			Action:         "snapshot",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5010/schedule",
			Title:          "ShiftForge Pro",
			VisibleText:    "Schedule Overview",
			NetworkEvents:  []browserpkg.NetworkEvent{{URL: "http://127.0.0.1:5010/_framework/blazor.server.js", Method: "POST", Status: 200, ResourceType: "fetch"}},
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "schedule.png")),
		},
	}
	if summary, ok := pipelinepkg.StageGPositiveEvidenceOutcomeForTest(nil, observations, nil, "planner timeout"); ok {
		t.Fatalf("framework transport traffic must not finish Stage G: %#v", summary)
	}
}

func TestStageGFrameworkNetworkNoiseDoesNotBlockBusinessEvidence(t *testing.T) {
	root := t.TempDir()
	observations := []pipelinepkg.TestBrowserObservation{
		{
			Action:         "open_candidate",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/login",
			Title:          "Sign in",
			VisibleText:    "Sign in Email Password",
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "login.png")),
		},
		{
			Action:      "fill_input",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/login",
			Title:       "Sign in",
			VisibleText: "Sign in Email Password",
			Controls:    loginControls(true, true),
		},
		{
			Action:      "click_button",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/admin/dashboard",
			Title:       "Admin Dashboard",
			VisibleText: "Admin Dashboard User Management Analytics Settings",
			NetworkEvents: []browserpkg.NetworkEvent{
				{URL: "http://127.0.0.1:5173/api/login", Method: "POST", Status: 200, ResourceType: "xhr"},
				{URL: "http://127.0.0.1:5173/api/users", Method: "GET", Status: 200, ResourceType: "xhr"},
				{URL: "http://127.0.0.1:5173/api/analytics", Method: "GET", Status: 200, ResourceType: "xhr"},
				{URL: "http://127.0.0.1:5173/@vite/client", Method: "GET", Status: 404, ResourceType: "fetch"},
			},
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "dashboard.png")),
		},
		{
			Action:         "snapshot",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/admin/dashboard",
			Title:          "Admin Dashboard",
			VisibleText:    "Admin Dashboard User Management Analytics Settings",
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "dashboard-2.png")),
		},
	}
	if summary, ok := pipelinepkg.StageGPositiveEvidenceOutcomeForTest(nil, observations, nil, "planner timeout"); !ok || summary.Status != "passed" {
		t.Fatalf("framework network noise should not block completed business evidence: ok=%t summary=%#v", ok, summary)
	}
	findings := pipelinepkg.FrontendE2EObservationFindingsForTest(observations, true)
	if len(findings) != 0 {
		t.Fatalf("framework network noise should not produce observation findings: %#v", findings)
	}
}

func TestStageGRejectsPlannerPassedFinishWithoutDeterministicEvidence(t *testing.T) {
	fixture := newStageGReplayFixture(t, "TASK-20260612-WEAKPASS")
	passedSummary := json.RawMessage(`{"schema_version":"p2r.frontend_e2e.v1","status":"passed","reason":"looks good","findings":[]}`)
	actions := []pipelinepkg.TestBrowserAction{
		{Action: "open_candidate", URLID: "url_1", Reason: "open app"},
		{Action: "finish", Reason: "planner weak pass", Summary: passedSummary},
		{Action: "finish", Reason: "planner weak pass", Summary: passedSummary},
		{Action: "finish", Reason: "planner weak pass", Summary: passedSummary},
		{Action: "finish", Reason: "planner weak pass", Summary: passedSummary},
	}
	plannerCalls := 0
	actionCalls := 0
	runner := fixture.NewRunner(
		pipelinepkg.WithStageGBrowserPlannerForTest(func(ctx context.Context, sc pipelinepkg.StageContext, promptTemplate, profile, contextText string, candidates []pipelinepkg.TestBrowserURLCandidate, observations []pipelinepkg.TestBrowserObservation, blocked []pipelinepkg.TestBlockedBrowserAction, round int, timeout time.Duration) (string, []pipelinepkg.ArtifactWarning, error) {
			plannerCalls++
			if plannerCalls > len(actions) {
				t.Fatalf("unexpected planner call %d", plannerCalls)
			}
			content, err := json.Marshal(actions[plannerCalls-1])
			if err != nil {
				t.Fatal(err)
			}
			return string(content), nil, nil
		}),
		pipelinepkg.WithStageGBrowserActionRunnerForTest(func(ctx context.Context, action browserpkg.Action, policy browserpkg.Policy, timeout time.Duration) (pipelinepkg.TestBrowserObservation, error) {
			actionCalls++
			if action.Name != "open_candidate" {
				t.Fatalf("unexpected browser action: %s", action.Name)
			}
			return pipelinepkg.TestBrowserObservation{
				Action:         action.Name,
				OK:             true,
				CurrentURL:     "http://127.0.0.1:5173/login",
				Title:          "Sign in",
				VisibleText:    "Sign in Email Password",
				ScreenshotPath: stageGTestScreenshot(t, policy, filepath.Join(fixture.Root, "login.png")),
			}, nil
		}),
	)
	record := runner.StageGForTest(context.Background(), fixture.Run, fixture.Project, fixture.Runtime)
	if record.Status != model.StageFailed || record.ErrorSummary != "too many unsupported pass summaries" {
		t.Fatalf("Stage G weak pass record = %#v", record)
	}
	if plannerCalls != len(actions) || actionCalls != 1 {
		t.Fatalf("unexpected calls planner=%d action=%d", plannerCalls, actionCalls)
	}
	if len(record.Findings) != 1 || record.Findings[0].Title != "Stage G received too many unsupported pass summaries" {
		t.Fatalf("missing weak-pass finding: %#v", record.Findings)
	}
	summary := readStageGSummaryForTest(t, fixture.ArtifactRoot)
	if summary.Status != "blocked" || len(summary.BlockedActions) != 4 {
		t.Fatalf("unexpected weak-pass summary: %#v", summary)
	}
}

func TestStageGRejectsPlannerFailedFinishWithoutObservationBackedEvidence(t *testing.T) {
	fixture := newStageGReplayFixture(t, "TASK-20260612-WEAKFAIL")
	failedSummary := json.RawMessage(`{"schema_version":"p2r.frontend_e2e.v1","status":"failed","reason":"generic failure","findings":[{"severity":"High","title":"workflow incomplete","evidence":"planner could not finish"}]}`)
	actions := []pipelinepkg.TestBrowserAction{
		{Action: "open_candidate", URLID: "url_1", Reason: "open app"},
		{Action: "snapshot", Reason: "capture state 1"},
		{Action: "snapshot", Reason: "capture state 2"},
		{Action: "snapshot", Reason: "capture state 3"},
		{Action: "snapshot", Reason: "capture state 4"},
		{Action: "finish", Reason: "planner weak failure", Summary: failedSummary},
		{Action: "finish", Reason: "planner weak failure", Summary: failedSummary},
		{Action: "finish", Reason: "planner weak failure", Summary: failedSummary},
		{Action: "finish", Reason: "planner weak failure", Summary: failedSummary},
	}
	plannerCalls := 0
	actionCalls := 0
	runner := fixture.NewRunner(
		pipelinepkg.WithStageGBrowserPlannerForTest(func(ctx context.Context, sc pipelinepkg.StageContext, promptTemplate, profile, contextText string, candidates []pipelinepkg.TestBrowserURLCandidate, observations []pipelinepkg.TestBrowserObservation, blocked []pipelinepkg.TestBlockedBrowserAction, round int, timeout time.Duration) (string, []pipelinepkg.ArtifactWarning, error) {
			plannerCalls++
			if plannerCalls > len(actions) {
				t.Fatalf("unexpected planner call %d", plannerCalls)
			}
			content, err := json.Marshal(actions[plannerCalls-1])
			if err != nil {
				t.Fatal(err)
			}
			return string(content), nil, nil
		}),
		pipelinepkg.WithStageGBrowserActionRunnerForTest(func(ctx context.Context, action browserpkg.Action, policy browserpkg.Policy, timeout time.Duration) (pipelinepkg.TestBrowserObservation, error) {
			actionCalls++
			return pipelinepkg.TestBrowserObservation{
				Action:         action.Name,
				OK:             true,
				CurrentURL:     fmt.Sprintf("http://127.0.0.1:5173/state-%d", actionCalls),
				Title:          "Public Workflow",
				VisibleText:    fmt.Sprintf("Public workflow state %d", actionCalls),
				ScreenshotPath: stageGTestScreenshot(t, policy, filepath.Join(fixture.Root, fmt.Sprintf("state-%d.png", actionCalls))),
			}, nil
		}),
	)
	record := runner.StageGForTest(context.Background(), fixture.Run, fixture.Project, fixture.Runtime)
	if record.Status != model.StageFailed || record.ErrorSummary != "too many unsupported finish summaries" {
		t.Fatalf("Stage G weak failed finish record = %#v", record)
	}
	if plannerCalls != len(actions) || actionCalls != 5 {
		t.Fatalf("unexpected calls planner=%d action=%d", plannerCalls, actionCalls)
	}
	if len(record.Findings) != 1 || record.Findings[0].Title != "Stage G received too many unsupported finish summaries" {
		t.Fatalf("missing weak-failed finding: %#v", record.Findings)
	}
	summary := readStageGSummaryForTest(t, fixture.ArtifactRoot)
	if summary.Status != "blocked" || len(summary.BlockedActions) != 4 {
		t.Fatalf("unexpected weak-failed summary: %#v", summary)
	}
}

func TestStageGPlannerTimeoutRecognizesGenericTimeoutErrors(t *testing.T) {
	fixture := newStageGReplayFixture(t, "TASK-20260612-PLANNER-TIMEOUT")
	runner := fixture.NewRunner(
		pipelinepkg.WithStageGBrowserPlannerForTest(func(ctx context.Context, sc pipelinepkg.StageContext, promptTemplate, profile, contextText string, candidates []pipelinepkg.TestBrowserURLCandidate, observations []pipelinepkg.TestBrowserObservation, blocked []pipelinepkg.TestBlockedBrowserAction, round int, timeout time.Duration) (string, []pipelinepkg.ArtifactWarning, error) {
			return "", nil, errors.New("turn timed out waiting for model response")
		}),
	)
	record := runner.StageGForTest(context.Background(), fixture.Run, fixture.Project, fixture.Runtime)
	if record.Status != model.StageFailed || record.ErrorSummary != "frontend E2E incomplete" {
		t.Fatalf("Stage G planner timeout record = %#v", record)
	}
	if len(record.Findings) != 1 || record.Findings[0].Title != "Stage G browser exploration did not finish" {
		t.Fatalf("expected incomplete exploration finding, got %#v", record.Findings)
	}
	summary := readStageGSummaryForTest(t, fixture.ArtifactRoot)
	if summary.Status != "partial" || !strings.Contains(summary.Reason, "planner returned") {
		t.Fatalf("unexpected planner timeout summary: %#v", summary)
	}
}

func TestStageGActionRunnerErrorFeedsPlannerReactLoop(t *testing.T) {
	fixture := newStageGReplayFixture(t, "TASK-20260612-RUNNER-ERROR")
	open := pipelinepkg.TestBrowserAction{Action: "open_candidate", URLID: "url_1", Reason: "open app"}
	failedSummary := json.RawMessage(`{"schema_version":"p2r.frontend_e2e.v1","status":"failed","reason":"browser tool could not open the app","findings":[{"severity":"High","title":"browser tool failed","evidence":"wrapper launch failed"}]}`)
	plannerCalls := 0
	actionCalls := 0
	runner := fixture.NewRunner(
		pipelinepkg.WithStageGBrowserPlannerForTest(func(ctx context.Context, sc pipelinepkg.StageContext, promptTemplate, profile, contextText string, candidates []pipelinepkg.TestBrowserURLCandidate, observations []pipelinepkg.TestBrowserObservation, blocked []pipelinepkg.TestBlockedBrowserAction, round int, timeout time.Duration) (string, []pipelinepkg.ArtifactWarning, error) {
			plannerCalls++
			if plannerCalls == 1 {
				content, err := json.Marshal(open)
				if err != nil {
					t.Fatal(err)
				}
				return string(content), nil, nil
			}
			if len(observations) != 1 || observations[0].OK || !strings.Contains(observations[0].Error, "wrapper launch failed") {
				t.Fatalf("planner did not receive runner error observation: %#v", observations)
			}
			content, err := json.Marshal(pipelinepkg.TestBrowserAction{Action: "finish", Reason: "conclude from browser tool error", Summary: failedSummary})
			if err != nil {
				t.Fatal(err)
			}
			return string(content), nil, nil
		}),
		pipelinepkg.WithStageGBrowserActionRunnerForTest(func(ctx context.Context, action browserpkg.Action, policy browserpkg.Policy, timeout time.Duration) (pipelinepkg.TestBrowserObservation, error) {
			actionCalls++
			return pipelinepkg.TestBrowserObservation{}, errors.New("wrapper launch failed")
		}),
	)
	record := runner.StageGForTest(context.Background(), fixture.Run, fixture.Project, fixture.Runtime)
	if record.Status != model.StageFailed || record.ErrorSummary != "frontend E2E findings" {
		t.Fatalf("Stage G runner-error record = %#v, want accepted failed agent conclusion", record)
	}
	if plannerCalls != 2 || actionCalls != 1 {
		t.Fatalf("runner error should feed one planner retry, planner=%d action=%d", plannerCalls, actionCalls)
	}
	summary := readStageGSummaryForTest(t, fixture.ArtifactRoot)
	if summary.Status != "failed" || len(summary.Findings) == 0 || summary.Findings[0].Title != "browser tool failed" {
		t.Fatalf("Stage G runner-error summary = %#v", summary)
	}
}

func TestStageGAcceptsModelFinishAfterSuccessfulEvidence(t *testing.T) {
	fixture := newStageGReplayFixture(t, "TASK-20260611-REPLAY")
	repoFile := filepath.Join(fixture.RepoPath, "src", "app.ts")
	if err := os.MkdirAll(filepath.Dir(repoFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(repoFile, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	actions := []pipelinepkg.TestBrowserAction{
		{Action: "open_candidate", URLID: "url_1", Reason: "open app"},
		{Action: "fill_input", Selector: "input[type=\"email\"]", Value: "admin@example.com", Reason: "fill email"},
		{Action: "fill_input", Selector: "input[type=\"password\"]", Value: "password", Reason: "fill password"},
		{Action: "click_button", Selector: "button[type=\"submit\"]", Reason: "submit login"},
		{Action: "finish", Reason: "model concluded from authenticated dashboard evidence", Summary: json.RawMessage(`{"schema_version":"p2r.frontend_e2e.v1","status":"passed","reason":"authenticated dashboard workflow was reached","findings":[]}`)},
	}
	plannerCalls := 0
	actionCalls := 0
	runner := fixture.NewRunner(
		pipelinepkg.WithStageGBrowserPlannerForTest(func(ctx context.Context, sc pipelinepkg.StageContext, promptTemplate, profile, contextText string, candidates []pipelinepkg.TestBrowserURLCandidate, observations []pipelinepkg.TestBrowserObservation, blocked []pipelinepkg.TestBlockedBrowserAction, round int, timeout time.Duration) (string, []pipelinepkg.ArtifactWarning, error) {
			plannerCalls++
			if plannerCalls > len(actions) {
				t.Fatalf("unexpected planner call %d after successful evidence", plannerCalls)
			}
			content, err := json.Marshal(actions[plannerCalls-1])
			if err != nil {
				t.Fatal(err)
			}
			return string(content), nil, nil
		}),
		pipelinepkg.WithStageGBrowserActionRunnerForTest(func(ctx context.Context, action browserpkg.Action, policy browserpkg.Policy, timeout time.Duration) (pipelinepkg.TestBrowserObservation, error) {
			actionCalls++
			observation := pipelinepkg.TestBrowserObservation{
				Action:      action.Name,
				OK:          true,
				CurrentURL:  "http://127.0.0.1:5173/login",
				Title:       "Sign in",
				VisibleText: "Sign in Email Password",
			}
			switch action.Name {
			case "open_candidate":
				observation.ScreenshotPath = stageGTestScreenshot(t, policy, filepath.Join(fixture.Root, "open.png"))
			case "fill_input":
				if actionCalls == 2 {
					observation.Controls = loginControls(true, false)
				} else {
					observation.Controls = loginControls(true, true)
				}
			case "click_button":
				observation.CurrentURL = "http://127.0.0.1:5173/admin/dashboard"
				observation.Title = "Admin Dashboard"
				observation.VisibleText = "Admin Dashboard User Management Analytics Settings"
				observation.Controls = []browserpkg.ControlSummary{
					{Role: "button", Text: "User Management"},
					{Role: "button", Text: "Settings"},
				}
				observation.NetworkEvents = []browserpkg.NetworkEvent{
					{URL: "http://127.0.0.1:5173/api/login", Method: "POST", Status: 200, ResourceType: "xhr"},
					{URL: "http://127.0.0.1:5173/api/users", Method: "GET", Status: 200, ResourceType: "xhr"},
					{URL: "http://127.0.0.1:5173/api/analytics", Method: "GET", Status: 200, ResourceType: "xhr"},
				}
				if err := os.WriteFile(repoFile, []byte("after"), 0o644); err != nil {
					t.Fatal(err)
				}
				observation.ScreenshotPath = stageGTestScreenshot(t, policy, filepath.Join(fixture.Root, "dashboard.png"))
			default:
				t.Fatalf("unexpected browser action after successful evidence: %s", action.Name)
			}
			return observation, nil
		}),
	)
	record := runner.StageGForTest(context.Background(), fixture.Run, fixture.Project, fixture.Runtime)
	if record.Status != model.StageDone || record.ErrorSummary != "" {
		t.Fatalf("Stage G record = %#v, want done without error", record)
	}
	if plannerCalls != 5 || actionCalls != 4 {
		t.Fatalf("Stage G should wait for explicit model finish after successful evidence, planner=%d action=%d", plannerCalls, actionCalls)
	}
	if len(record.Findings) != 1 || record.Findings[0].Title != "Stage G modified repository source files" {
		t.Fatalf("Stage G replay should preserve repo mutation finding, got %#v", record.Findings)
	}
	summaryPath := filepath.Join(fixture.ArtifactRoot, "frontend_e2e_summary.json")
	content, err := os.ReadFile(summaryPath)
	if err != nil {
		t.Fatal(err)
	}
	var summary pipelinepkg.TestFrontendE2ESummary
	if err := json.Unmarshal(content, &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Status != "passed" || !strings.Contains(strings.Join(summary.Notes, "\n"), "business_network_endpoints=2") {
		t.Fatalf("unexpected Stage G summary: %#v", summary)
	}
	if len(summary.Screenshots) < 2 {
		t.Fatalf("expected materialized screenshots in summary: %#v", summary.Screenshots)
	}
	for _, screenshot := range summary.Screenshots {
		if _, err := os.Stat(screenshot); err != nil {
			t.Fatalf("summary screenshot missing: %s err=%v", screenshot, err)
		}
		if !slices.Contains(record.ArtifactPaths, screenshot) {
			t.Fatalf("record artifact paths missing screenshot %s: %#v", screenshot, record.ArtifactPaths)
		}
	}
	if len(summary.Findings) != 1 || !strings.Contains(summary.Findings[0].Evidence, "src") {
		t.Fatalf("summary missing repo mutation finding: %#v", summary.Findings)
	}
}

func TestStageGRepoSnapshotFailureContinuesBrowserExploration(t *testing.T) {
	fixture := newStageGReplayFixture(t, "TASK-20260611-SNAPSHOT")
	if err := os.RemoveAll(fixture.RepoPath); err != nil {
		t.Fatal(err)
	}
	actions := []pipelinepkg.TestBrowserAction{
		{Action: "open_candidate", URLID: "url_1", Reason: "open app"},
		{Action: "fill_input", Selector: "input[type=\"email\"]", Value: "admin@example.com", Reason: "fill email"},
		{Action: "fill_input", Selector: "input[type=\"password\"]", Value: "password", Reason: "fill password"},
		{Action: "click_button", Selector: "button[type=\"submit\"]", Reason: "submit login"},
		{Action: "finish", Reason: "model concluded from authenticated dashboard evidence", Summary: json.RawMessage(`{"schema_version":"p2r.frontend_e2e.v1","status":"passed","reason":"authenticated dashboard workflow was reached","findings":[]}`)},
	}
	plannerCalls := 0
	actionCalls := 0
	runner := fixture.NewRunner(
		pipelinepkg.WithStageGBrowserPlannerForTest(func(ctx context.Context, sc pipelinepkg.StageContext, promptTemplate, profile, contextText string, candidates []pipelinepkg.TestBrowserURLCandidate, observations []pipelinepkg.TestBrowserObservation, blocked []pipelinepkg.TestBlockedBrowserAction, round int, timeout time.Duration) (string, []pipelinepkg.ArtifactWarning, error) {
			plannerCalls++
			if plannerCalls > len(actions) {
				t.Fatalf("unexpected planner call %d after successful evidence", plannerCalls)
			}
			content, err := json.Marshal(actions[plannerCalls-1])
			if err != nil {
				t.Fatal(err)
			}
			return string(content), nil, nil
		}),
		pipelinepkg.WithStageGBrowserActionRunnerForTest(func(ctx context.Context, action browserpkg.Action, policy browserpkg.Policy, timeout time.Duration) (pipelinepkg.TestBrowserObservation, error) {
			actionCalls++
			observation := pipelinepkg.TestBrowserObservation{
				Action:      action.Name,
				OK:          true,
				CurrentURL:  "http://127.0.0.1:5173/login",
				Title:       "Sign in",
				VisibleText: "Sign in Email Password",
			}
			switch action.Name {
			case "open_candidate":
				observation.ScreenshotPath = stageGTestScreenshot(t, policy, filepath.Join(fixture.Root, "open.png"))
			case "fill_input":
				if actionCalls == 2 {
					observation.Controls = loginControls(true, false)
				} else {
					observation.Controls = loginControls(true, true)
				}
			case "click_button":
				observation.CurrentURL = "http://127.0.0.1:5173/admin/dashboard"
				observation.Title = "Admin Dashboard"
				observation.VisibleText = "Admin Dashboard User Management Analytics Settings"
				observation.NetworkEvents = []browserpkg.NetworkEvent{
					{URL: "http://127.0.0.1:5173/api/login", Method: "POST", Status: 200, ResourceType: "xhr"},
					{URL: "http://127.0.0.1:5173/api/users", Method: "GET", Status: 200, ResourceType: "xhr"},
					{URL: "http://127.0.0.1:5173/api/analytics", Method: "GET", Status: 200, ResourceType: "xhr"},
				}
				observation.ScreenshotPath = stageGTestScreenshot(t, policy, filepath.Join(fixture.Root, "dashboard.png"))
			default:
				t.Fatalf("unexpected browser action: %s", action.Name)
			}
			return observation, nil
		}),
	)
	record := runner.StageGForTest(context.Background(), fixture.Run, fixture.Project, fixture.Runtime)
	if record.Status != model.StageDone || record.ErrorSummary != "" {
		t.Fatalf("Stage G snapshot failure record = %#v, want done with finding after browser evidence", record)
	}
	if plannerCalls != 5 || actionCalls != 4 {
		t.Fatalf("Stage G should continue after repo snapshot failure, planner=%d action=%d", plannerCalls, actionCalls)
	}
	if len(record.Findings) == 0 || record.Findings[0].Title != "Stage G repository snapshot failed" {
		t.Fatalf("Stage G missing repo snapshot finding: %#v", record.Findings)
	}
	observationsContent, err := os.ReadFile(filepath.Join(fixture.ArtifactRoot, "frontend_e2e_observations.json"))
	if err != nil {
		t.Fatal(err)
	}
	var observations []pipelinepkg.TestBrowserObservation
	if err := json.Unmarshal(observationsContent, &observations); err != nil {
		t.Fatal(err)
	}
	if len(observations) != 4 {
		t.Fatalf("expected browser observations despite snapshot failure, got %d", len(observations))
	}
	summary := readStageGSummaryForTest(t, fixture.ArtifactRoot)
	if summary.Status != "passed" || len(summary.Findings) == 0 || summary.Findings[0].Title != "Stage G repository snapshot failed" {
		t.Fatalf("Stage G snapshot failure summary = %#v", summary)
	}
}

func TestStageGReplayLoginServerErrorFailsWithProductEvidence(t *testing.T) {
	fixture := newStageGReplayFixture(t, "TASK-20260611-LOGIN500")
	actions := []pipelinepkg.TestBrowserAction{
		{Action: "open_candidate", URLID: "url_1", Reason: "open login"},
		{Action: "fill_input", Selector: "input[type=\"text\"]", Value: "admin", Reason: "fill username"},
		{Action: "fill_input", Selector: "input[type=\"password\"]", Value: "password", Reason: "fill password"},
		{Action: "click_button", Selector: "form button", Reason: "submit login"},
		{Action: "finish", Reason: "model concluded from login server error", Summary: json.RawMessage(`{"schema_version":"p2r.frontend_e2e.v1","status":"failed","reason":"login request returned a server error","findings":[{"severity":"High","title":"Login request returned server error","rule":"Credentialed login should not return 5xx","evidence":"POST /api/auth/login status=500","impact":"Users cannot reach authenticated workflows.","minimum_fix":"Fix the login endpoint and rerun Stage G."}]}`)},
	}
	plannerCalls := 0
	actionCalls := 0
	runner := fixture.NewRunner(
		pipelinepkg.WithStageGBrowserPlannerForTest(func(ctx context.Context, sc pipelinepkg.StageContext, promptTemplate, profile, contextText string, candidates []pipelinepkg.TestBrowserURLCandidate, observations []pipelinepkg.TestBrowserObservation, blocked []pipelinepkg.TestBlockedBrowserAction, round int, timeout time.Duration) (string, []pipelinepkg.ArtifactWarning, error) {
			plannerCalls++
			if plannerCalls > len(actions) {
				return "", nil, context.DeadlineExceeded
			}
			content, err := json.Marshal(actions[plannerCalls-1])
			if err != nil {
				t.Fatal(err)
			}
			return string(content), nil, nil
		}),
		pipelinepkg.WithStageGBrowserActionRunnerForTest(func(ctx context.Context, action browserpkg.Action, policy browserpkg.Policy, timeout time.Duration) (pipelinepkg.TestBrowserObservation, error) {
			actionCalls++
			observation := pipelinepkg.TestBrowserObservation{
				Action:      action.Name,
				OK:          true,
				CurrentURL:  "http://127.0.0.1:5173/login",
				Title:       "CloudPulse Infrastructure Sentinel",
				VisibleText: "Login Username Password",
			}
			switch actionCalls {
			case 1:
				observation.ScreenshotPath = stageGTestScreenshot(t, policy, filepath.Join(fixture.Root, "login.png"))
			case 2:
				observation.Controls = loginControls(true, false)
			case 3:
				observation.Controls = loginControls(true, true)
			case 4:
				observation.ConsoleErrors = []string{"POST /api/auth/login 500"}
				observation.NetworkIssues = []browserpkg.NetworkIssue{{URL: "http://127.0.0.1:5173/api/auth/login", Status: 500}}
				observation.NetworkEvents = []browserpkg.NetworkEvent{{URL: "http://127.0.0.1:5173/api/auth/login", Method: "POST", Status: 500, ResourceType: "xhr"}}
				observation.ScreenshotPath = stageGTestScreenshot(t, policy, filepath.Join(fixture.Root, "login-500.png"))
			case 5:
				observation.Controls = loginControls(true, false)
			default:
				t.Fatalf("unexpected browser action count after login 500: %d", actionCalls)
			}
			return observation, nil
		}),
	)
	record := runner.StageGForTest(context.Background(), fixture.Run, fixture.Project, fixture.Runtime)
	if record.Status != model.StageFailed || record.ErrorSummary != "frontend E2E findings" {
		t.Fatalf("Stage G login 500 record = %#v, want failed product evidence", record)
	}
	if plannerCalls != 5 || actionCalls != 4 {
		t.Fatalf("Stage G login 500 replay calls planner=%d action=%d, want model finish after login submit", plannerCalls, actionCalls)
	}
	if len(record.Findings) == 0 || record.Findings[0].Title != "Login request returned server error" || !strings.Contains(record.Findings[0].Evidence, "status=500") {
		t.Fatalf("Stage G login 500 missing product error finding: %#v", record.Findings)
	}
	summary := readStageGSummaryForTest(t, fixture.ArtifactRoot)
	if summary.Status != "failed" || len(summary.Findings) == 0 || !strings.Contains(summary.Findings[0].Evidence, "status=500") {
		t.Fatalf("Stage G login 500 summary = %#v", summary)
	}
}

func TestStageGReplayAuthAcceptedStillOnLoginFailsAsAuthTransition(t *testing.T) {
	fixture := newStageGReplayFixture(t, "TASK-20260611-AUTH-STUCK")
	actions := []pipelinepkg.TestBrowserAction{
		{Action: "open_candidate", URLID: "url_1", Reason: "open login"},
		{Action: "fill_input", Selector: "input[type=\"email\"]", Value: "admin@example.com", Reason: "fill email"},
		{Action: "fill_input", Selector: "input[type=\"password\"]", Value: "password", Reason: "fill password"},
		{Action: "click_button", Selector: "button[type=\"submit\"]", Reason: "submit login"},
		{Action: "snapshot", Reason: "observe post-login state"},
	}
	plannerCalls := 0
	actionCalls := 0
	runner := fixture.NewRunner(
		pipelinepkg.WithStageGBrowserPlannerForTest(func(ctx context.Context, sc pipelinepkg.StageContext, promptTemplate, profile, contextText string, candidates []pipelinepkg.TestBrowserURLCandidate, observations []pipelinepkg.TestBrowserObservation, blocked []pipelinepkg.TestBlockedBrowserAction, round int, timeout time.Duration) (string, []pipelinepkg.ArtifactWarning, error) {
			plannerCalls++
			if plannerCalls > len(actions) {
				return "", nil, context.DeadlineExceeded
			}
			content, err := json.Marshal(actions[plannerCalls-1])
			if err != nil {
				t.Fatal(err)
			}
			return string(content), nil, nil
		}),
		pipelinepkg.WithStageGBrowserActionRunnerForTest(func(ctx context.Context, action browserpkg.Action, policy browserpkg.Policy, timeout time.Duration) (pipelinepkg.TestBrowserObservation, error) {
			actionCalls++
			observation := pipelinepkg.TestBrowserObservation{
				Action:      action.Name,
				OK:          true,
				CurrentURL:  "http://127.0.0.1:5173/login",
				Title:       "Sign In",
				VisibleText: "Sign In Email Password",
				Controls:    loginControls(true, true),
			}
			switch actionCalls {
			case 1:
				observation.Controls = loginControls(false, false)
				observation.ScreenshotPath = stageGTestScreenshot(t, policy, filepath.Join(fixture.Root, "login.png"))
			case 2:
				observation.Controls = loginControls(true, false)
			case 3:
				observation.Controls = loginControls(true, true)
			case 4:
				observation.NetworkEvents = []browserpkg.NetworkEvent{{URL: "http://127.0.0.1:5173/api/login", Method: "POST", Status: 200, ResourceType: "xhr"}}
				observation.ScreenshotPath = stageGTestScreenshot(t, policy, filepath.Join(fixture.Root, "login-accepted.png"))
			case 5:
				observation.ScreenshotPath = stageGTestScreenshot(t, policy, filepath.Join(fixture.Root, "still-login.png"))
			default:
				t.Fatalf("unexpected browser action count after auth accepted: %d", actionCalls)
			}
			return observation, nil
		}),
	)
	record := runner.StageGForTest(context.Background(), fixture.Run, fixture.Project, fixture.Runtime)
	if record.Status != model.StageFailed || record.ErrorSummary != "frontend E2E findings" {
		t.Fatalf("Stage G auth-stuck record = %#v, want failed auth transition", record)
	}
	if plannerCalls != 5 || actionCalls != 5 {
		t.Fatalf("Stage G auth-stuck replay calls planner=%d action=%d, want failure after accepted auth follow-up stays on login", plannerCalls, actionCalls)
	}
	if len(record.Findings) == 0 || record.Findings[0].Title != "Authentication response did not reach authenticated browser workflow" || !strings.Contains(record.Findings[0].Evidence, "status=200") {
		t.Fatalf("Stage G auth-stuck missing transition finding: %#v", record.Findings)
	}
	summary := readStageGSummaryForTest(t, fixture.ArtifactRoot)
	if summary.Status != "failed" || len(summary.Findings) == 0 || !strings.Contains(summary.Findings[0].Evidence, "status=200") {
		t.Fatalf("Stage G auth-stuck summary = %#v", summary)
	}
}

func TestStageGUsesModelSubmittedAuthActionBeforeFinish(t *testing.T) {
	fixture := newStageGReplayFixture(t, "TASK-20260612-AUTH-MODEL-SUBMIT")
	actions := []pipelinepkg.TestBrowserAction{
		{Action: "open_candidate", URLID: "url_1", Reason: "open login"},
		{Action: "fill_input", Selector: "input[type=\"email\"]", Value: "admin@example.com", Reason: "fill email"},
		{Action: "fill_input", Selector: "input[type=\"password\"]", Value: "password", Reason: "fill password"},
		{Action: "click_button", Text: "Sign In", Reason: "model submits the observed filled login button"},
		{Action: "finish", Reason: "model concluded from authenticated dashboard evidence", Summary: json.RawMessage(`{"schema_version":"p2r.frontend_e2e.v1","status":"passed","reason":"authenticated dashboard workflow was reached","findings":[]}`)},
	}
	plannerCalls := 0
	actionCalls := 0
	runner := fixture.NewRunner(
		pipelinepkg.WithStageGBrowserPlannerForTest(func(ctx context.Context, sc pipelinepkg.StageContext, promptTemplate, profile, contextText string, candidates []pipelinepkg.TestBrowserURLCandidate, observations []pipelinepkg.TestBrowserObservation, blocked []pipelinepkg.TestBlockedBrowserAction, round int, timeout time.Duration) (string, []pipelinepkg.ArtifactWarning, error) {
			plannerCalls++
			if plannerCalls > len(actions) {
				t.Fatalf("unexpected planner call %d", plannerCalls)
			}
			content, err := json.Marshal(actions[plannerCalls-1])
			if err != nil {
				t.Fatal(err)
			}
			return string(content), nil, nil
		}),
		pipelinepkg.WithStageGBrowserActionRunnerForTest(func(ctx context.Context, action browserpkg.Action, policy browserpkg.Policy, timeout time.Duration) (pipelinepkg.TestBrowserObservation, error) {
			actionCalls++
			observation := pipelinepkg.TestBrowserObservation{
				Action:      action.Name,
				OK:          true,
				CurrentURL:  "http://127.0.0.1:5173/login",
				Title:       "Sign in",
				VisibleText: "Sign in Email Password",
			}
			switch actionCalls {
			case 1:
				observation.Controls = loginControls(false, false)
				observation.ScreenshotPath = stageGTestScreenshot(t, policy, filepath.Join(fixture.Root, "login.png"))
			case 2:
				observation.Controls = loginControls(true, false)
			case 3:
				observation.Controls = loginControls(true, true)
			case 4:
				if action.Name != "click_button" || action.Text != "Sign In" {
					t.Fatalf("expected model-selected submit action, got %#v", action)
				}
				observation.CurrentURL = "http://127.0.0.1:5173/admin/dashboard"
				observation.Title = "Admin Dashboard"
				observation.VisibleText = "Admin Dashboard Projects Users Analytics"
				observation.NetworkEvents = []browserpkg.NetworkEvent{
					{URL: "http://127.0.0.1:5173/api/auth/login", Method: "POST", Status: 200, ResourceType: "xhr"},
					{URL: "http://127.0.0.1:5173/api/projects", Method: "GET", Status: 200, ResourceType: "xhr"},
					{URL: "http://127.0.0.1:5173/api/users", Method: "GET", Status: 200, ResourceType: "xhr"},
				}
				observation.ScreenshotPath = stageGTestScreenshot(t, policy, filepath.Join(fixture.Root, "dashboard.png"))
			default:
				t.Fatalf("unexpected browser action count: %d", actionCalls)
			}
			return observation, nil
		}),
	)
	record := runner.StageGForTest(context.Background(), fixture.Run, fixture.Project, fixture.Runtime)
	if record.Status != model.StageDone || record.ErrorSummary != "" {
		t.Fatalf("Stage G model auth submit record = %#v", record)
	}
	if plannerCalls != 5 || actionCalls != 4 {
		t.Fatalf("model submit should be explicit before finish, planner=%d action=%d", plannerCalls, actionCalls)
	}
	summary := readStageGSummaryForTest(t, fixture.ArtifactRoot)
	if summary.Status != "passed" || !strings.Contains(strings.Join(summary.Notes, "\n"), "business_network_endpoints=2") {
		t.Fatalf("unexpected model auth submit summary: %#v", summary)
	}
}

func TestStageGReplayBlazorAuthSelectorTimeoutFailsAsAuthCoverage(t *testing.T) {
	fixture := newStageGReplayFixture(t, "TASK-20260611-BLAZOR")
	actions := []pipelinepkg.TestBrowserAction{
		{Action: "open_candidate", URLID: "url_1", Reason: "open Blazor app"},
		{Action: "fill_input", Selector: "input[type=\"email\"]", Value: "admin@example.com", Reason: "fill email"},
		{Action: "fill_input", Selector: "input[type=\"password\"]", Value: "password", Reason: "fill password"},
		{Action: "click_button", Text: "Sign In", Reason: "model submits the observed filled login button"},
		{Action: "fill_input", Selector: "form input[type=\"email\"]", Value: "admin@example.com", Reason: "retry username after submit selector failure"},
	}
	plannerCalls := 0
	actionCalls := 0
	runner := fixture.NewRunner(
		pipelinepkg.WithStageGBrowserPlannerForTest(func(ctx context.Context, sc pipelinepkg.StageContext, promptTemplate, profile, contextText string, candidates []pipelinepkg.TestBrowserURLCandidate, observations []pipelinepkg.TestBrowserObservation, blocked []pipelinepkg.TestBlockedBrowserAction, round int, timeout time.Duration) (string, []pipelinepkg.ArtifactWarning, error) {
			plannerCalls++
			if plannerCalls > len(actions) {
				return "", nil, context.DeadlineExceeded
			}
			content, err := json.Marshal(actions[plannerCalls-1])
			if err != nil {
				t.Fatal(err)
			}
			return string(content), nil, nil
		}),
		pipelinepkg.WithStageGBrowserActionRunnerForTest(func(ctx context.Context, action browserpkg.Action, policy browserpkg.Policy, timeout time.Duration) (pipelinepkg.TestBrowserObservation, error) {
			actionCalls++
			observation := pipelinepkg.TestBrowserObservation{
				Action:      action.Name,
				OK:          true,
				CurrentURL:  "http://127.0.0.1:5173/",
				Title:       "ShiftForge Pro",
				VisibleText: "ShiftForge Pro Sign In Email Password",
				NetworkEvents: []browserpkg.NetworkEvent{
					{URL: "http://127.0.0.1:5173/_blazor/negotiate?negotiateVersion=1", Method: "POST", Status: 200, ResourceType: "fetch"},
				},
			}
			switch actionCalls {
			case 1:
				observation.ScreenshotPath = stageGTestScreenshot(t, policy, filepath.Join(fixture.Root, "blazor-home.png"))
			case 2:
				observation.Controls = loginControls(true, false)
			case 3:
				observation.Controls = loginControls(true, true)
			case 4:
				if action.Name != "click_button" || action.Text != "Sign In" {
					t.Fatalf("expected model-selected submit action, got %#v", action)
				}
				observation.OK = false
				observation.Controls = loginControls(true, true)
				observation.Error = `locator.click: Timeout 5000ms exceeded. waiting for getByRole('button', { name: 'Sign In' })`
			case 5:
				if action.Name != "fill_input" {
					t.Fatalf("expected planner fallback after one submit failure, got %#v", action)
				}
				observation.OK = false
				observation.Controls = loginControls(true, true)
				observation.Error = `locator.fill: Timeout 5000ms exceeded. waiting for locator('form input[type="email"]').first()`
			default:
				t.Fatalf("unexpected browser action count after Blazor selector timeout: %d", actionCalls)
			}
			return observation, nil
		}),
	)
	record := runner.StageGForTest(context.Background(), fixture.Run, fixture.Project, fixture.Runtime)
	if record.Status != model.StageFailed || record.ErrorSummary != "frontend E2E findings" {
		t.Fatalf("Stage G Blazor timeout record = %#v, want failed auth coverage", record)
	}
	if plannerCalls != 6 || actionCalls != 5 {
		t.Fatalf("Stage G Blazor timeout replay calls planner=%d action=%d, want model submit failure then fallback", plannerCalls, actionCalls)
	}
	if len(record.Findings) == 0 || record.Findings[0].Title != "Authentication controls prevented browser workflow coverage" || !strings.Contains(record.Findings[0].Evidence, "selector failure(s)") {
		t.Fatalf("Stage G Blazor timeout missing auth selector finding: %#v", record.Findings)
	}
	summary := readStageGSummaryForTest(t, fixture.ArtifactRoot)
	if summary.Status != "failed" || len(summary.Findings) == 0 || !strings.Contains(summary.Findings[0].Evidence, "selector failure(s)") {
		t.Fatalf("Stage G Blazor timeout summary = %#v", summary)
	}
}

func TestStageGPositiveEvidenceOutcomeRejectsServerError(t *testing.T) {
	root := t.TempDir()
	observations := []pipelinepkg.TestBrowserObservation{
		{
			Action:         "open_candidate",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/login",
			VisibleText:    "Sign in Email Password",
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "login.png")),
		},
		{
			Action:         "click_button",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/login",
			VisibleText:    "Sign in Email Password Login failed",
			NetworkIssues:  []browserpkg.NetworkIssue{{URL: "http://127.0.0.1:5173/api/auth/login", Status: 500}},
			NetworkEvents:  []browserpkg.NetworkEvent{{URL: "http://127.0.0.1:5173/api/auth/login", Method: "POST", Status: 500, ResourceType: "xhr"}},
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "failed.png")),
		},
	}
	if summary, ok := pipelinepkg.StageGPositiveEvidenceOutcomeForTest(nil, observations, nil, "planner timeout"); ok {
		t.Fatalf("server error must not pass Stage G: %#v", summary)
	}
	finding, ok := pipelinepkg.StageGPartialProductBlockerFindingForTest(observations, "planner timeout")
	if !ok || !strings.Contains(finding.Evidence, "status=500") {
		t.Fatalf("expected product failure evidence, got ok=%t finding=%#v", ok, finding)
	}
}

func TestStageGAutoOutcomeKeepsRepoSnapshotMutationFinding(t *testing.T) {
	repoPath := t.TempDir()
	target := filepath.Join(repoPath, "src", "app.ts")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := pipelinepkg.SnapshotRepoForTest(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}
	summary := pipelinepkg.TestFrontendE2ESummary{
		SchemaVersion: "p2r.frontend_e2e.v1",
		Status:        "passed",
	}
	record, summary := pipelinepkg.AppendStageGRepoSnapshotFindingsForTest(model.StageRecord{Stage: string(model.StageG)}, summary, repoPath, before)
	if len(record.Findings) != 1 || record.Findings[0].Title != "Stage G modified repository source files" {
		t.Fatalf("expected repo mutation finding, got %#v", record.Findings)
	}
	if len(summary.Findings) != 1 || !strings.Contains(summary.Findings[0].Evidence, "src/app.ts") {
		t.Fatalf("summary missing repo mutation evidence: %#v", summary.Findings)
	}
}

func TestStageGPartialProductBlockerFindingDetectsAuthGateStall(t *testing.T) {
	observations := []pipelinepkg.TestBrowserObservation{
		{
			Action:      "open_candidate",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/login",
			VisibleText: "AvatarForge Studio Sign In Email Password",
			Controls:    loginControls(false, false),
		},
		{
			Action:      "fill_input",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/login",
			VisibleText: "AvatarForge Studio Sign In Email Password",
			Controls:    loginControls(true, false),
		},
		{
			Action:      "fill_input",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/login",
			VisibleText: "AvatarForge Studio Sign In Email Password",
			Controls:    loginControls(true, true),
		},
		{
			Action:      "click_button",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/login",
			VisibleText: "AvatarForge Studio Sign In Email Password",
			Controls:    loginControls(true, true),
		},
		{
			Action:      "collect_network",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/login",
			VisibleText: "AvatarForge Studio Sign In Email Password",
			Controls:    loginControls(true, true),
		},
		{
			Action:      "click_button",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/login",
			VisibleText: "AvatarForge Studio Sign In Email Password",
			Controls:    loginControls(true, true),
		},
	}
	finding, ok := pipelinepkg.StageGPartialProductBlockerFindingForTest(observations, "Stage G stopped after repeated authentication-gate attempts.")
	if !ok {
		t.Fatal("expected auth-gate stall product blocker finding")
	}
	if finding.Title != "Authentication gate prevented browser workflow coverage" || !strings.Contains(finding.Evidence, "2 credentialed submit attempt") {
		t.Fatalf("unexpected finding: %#v", finding)
	}
	if reason := pipelinepkg.StageGObservationStopReasonForTest(observations); !strings.Contains(reason, "authentication-gate") {
		t.Fatalf("expected auth-gate stop reason, got %q", reason)
	}
}

func TestStageGAuthSuccessWaitsForFollowupObservationBeforeNoTransitionFailure(t *testing.T) {
	observations := []pipelinepkg.TestBrowserObservation{
		{
			Action:      "open_candidate",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/login",
			VisibleText: "Sign in Email Password",
			Controls:    loginControls(false, false),
		},
		{
			Action:      "fill_input",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/login",
			VisibleText: "Sign in Email Password",
			Controls:    loginControls(true, true),
		},
		{
			Action:      "click_button",
			OK:          true,
			CurrentURL:  "http://127.0.0.1:5173/login",
			VisibleText: "Sign in Email Password",
			Controls:    loginControls(true, true),
			NetworkEvents: []browserpkg.NetworkEvent{
				{URL: "http://127.0.0.1:5173/api/auth/login", Method: "POST", Status: 200, ResourceType: "xhr"},
			},
		},
	}
	if reason := pipelinepkg.StageGObservationStopReasonForTest(observations); reason != "" {
		t.Fatalf("auth success should wait for follow-up observation, got %q", reason)
	}
	if finding, ok := pipelinepkg.StageGPartialProductBlockerFindingForTest(observations, "planner timeout"); ok {
		t.Fatalf("auth success without follow-up should not be product blocker yet: %#v", finding)
	}
	observations = append(observations, pipelinepkg.TestBrowserObservation{
		Action:      "collect_network",
		OK:          true,
		CurrentURL:  "http://127.0.0.1:5173/login",
		VisibleText: "Sign in Email Password",
		Controls:    loginControls(true, true),
	})
	if reason := pipelinepkg.StageGObservationStopReasonForTest(observations); !strings.Contains(reason, "accepted authentication") {
		t.Fatalf("expected no-transition stop after follow-up observation, got %q", reason)
	}
}

func TestStageGRepeatedStateStallIgnoresAuthGateBeforeSubmitLimit(t *testing.T) {
	observations := []pipelinepkg.TestBrowserObservation{
		{Action: "open_candidate", OK: true, Controls: loginControls(false, false)},
		{Action: "fill_input", OK: true, Controls: loginControls(true, false)},
		{Action: "fill_input", OK: true, Controls: loginControls(true, true)},
		{Action: "fill_input", OK: true, Controls: loginControls(true, true)},
		{Action: "fill_input", OK: true, Controls: loginControls(true, true)},
		{Action: "click_button", OK: true, Controls: loginControls(true, true)},
		{Action: "collect_network", OK: true, Controls: loginControls(true, true)},
	}
	for index := range observations {
		observations[index].CurrentURL = "http://127.0.0.1:5173/login"
		observations[index].VisibleText = "AvatarForge Studio Sign In Email Password"
	}
	if evidence := pipelinepkg.StageGRepeatedStateStallEvidenceForTest(observations); evidence != "" {
		t.Fatalf("auth gate should wait for the dedicated submit stall rule, got %q", evidence)
	}
	if reason := pipelinepkg.StageGObservationStopReasonForTest(observations); reason != "" {
		t.Fatalf("one submit should not stop auth exploration, got %q", reason)
	}
}

func TestStageGAuthGateStallIgnoresFailedSelectorSubmits(t *testing.T) {
	observations := []pipelinepkg.TestBrowserObservation{
		{Action: "open_candidate", OK: true, Controls: loginControls(true, true)},
		{
			Action:   "click_button",
			OK:       false,
			Controls: loginControls(true, true),
			Error:    "locator.click: Timeout 5000ms exceeded. waiting for locator('button[type=submit]').first()",
		},
		{
			Action:   "submit_local_form",
			OK:       false,
			Controls: loginControls(true, true),
			Error:    "locator.press: Target page, context or browser has been closed",
		},
	}
	for index := range observations {
		observations[index].CurrentURL = "http://127.0.0.1:5173/login"
		observations[index].VisibleText = "SketchPad Studio Sign in Username Password"
	}
	if evidence := pipelinepkg.StageGAuthGateStallEvidenceForTest(observations); evidence != "" {
		t.Fatalf("failed selector/tool submit attempts should not count as auth-gate stall, got %q", evidence)
	}
	if reason := pipelinepkg.StageGObservationStopReasonForTest(observations); reason != "" {
		t.Fatalf("failed selector/tool submit attempts should feed react loop, got %q", reason)
	}
}

func TestStageGRepeatedStateStallEvidenceDetectsNoProgressLoop(t *testing.T) {
	observations := []pipelinepkg.TestBrowserObservation{
		{Action: "open_candidate", OK: true},
		{Action: "fill_input", OK: true},
		{Action: "click_button", OK: true},
		{Action: "collect_network", OK: true},
		{Action: "fill_input", OK: true},
		{Action: "click_button", OK: true},
		{Action: "collect_console", OK: true},
	}
	for index := range observations {
		observations[index].CurrentURL = "http://127.0.0.1:5173/editor"
		observations[index].VisibleText = "Editor Empty state"
	}
	evidence := pipelinepkg.StageGRepeatedStateStallEvidenceForTest(observations)
	if !strings.Contains(evidence, "unchanged visible state") {
		t.Fatalf("expected no-progress evidence, got %q", evidence)
	}
}

func TestStageGRepeatedStateStallIgnoresLongFormInput(t *testing.T) {
	observations := []pipelinepkg.TestBrowserObservation{
		{Action: "open_candidate", OK: true},
		{Action: "fill_input", OK: true},
		{Action: "fill_input", OK: true},
		{Action: "fill_input", OK: true},
		{Action: "fill_input", OK: true},
		{Action: "fill_input", OK: true},
		{Action: "fill_input", OK: true},
	}
	for index := range observations {
		observations[index].CurrentURL = "http://127.0.0.1:5173/profile/edit"
		observations[index].VisibleText = "Profile edit form Name Title Bio Location Website Phone Email"
	}
	if evidence := pipelinepkg.StageGRepeatedStateStallEvidenceForTest(observations); evidence != "" {
		t.Fatalf("same-page form filling should not be treated as no-progress stall, got %q", evidence)
	}
}

func TestStageGKeyScreenshotSelectionIsCappedAndRepresentative(t *testing.T) {
	root := t.TempDir()
	var observations []pipelinepkg.TestBrowserObservation
	for index := 0; index < 12; index++ {
		observation := pipelinepkg.TestBrowserObservation{
			Action:         "snapshot",
			OK:             true,
			CurrentURL:     fmt.Sprintf("http://127.0.0.1:5173/business/%02d", index),
			VisibleText:    fmt.Sprintf("Business screen %02d", index),
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "runtime", fmt.Sprintf("shot-%02d.png", index))),
		}
		if index == 3 {
			observation.OK = false
			observation.NetworkIssues = []browserpkg.NetworkIssue{{URL: "http://127.0.0.1:5173/api/business/03", Status: 500}}
		}
		if index == 8 {
			observation.NetworkEvents = []browserpkg.NetworkEvent{{URL: "http://127.0.0.1:5173/api/auth/login", Method: "POST", Status: 200}}
		}
		observations = append(observations, observation)
	}
	indexes := pipelinepkg.StageGKeyScreenshotObservationIndexesForTest(observations)
	if len(indexes) != 10 {
		t.Fatalf("selected %d screenshots, want 10: %#v", len(indexes), indexes)
	}
	for _, want := range []int{0, 3, 8, 11} {
		if !slices.Contains(indexes, want) {
			t.Fatalf("selected screenshots should include key index %d: %#v", want, indexes)
		}
	}
}

func TestStageGMaterializesAtMostTenScreenshotArtifacts(t *testing.T) {
	sourceRoot := t.TempDir()
	artifactRoot := t.TempDir()
	var observations []pipelinepkg.TestBrowserObservation
	for index := 0; index < 12; index++ {
		observations = append(observations, pipelinepkg.TestBrowserObservation{
			Action:         "snapshot",
			OK:             true,
			CurrentURL:     fmt.Sprintf("http://127.0.0.1:5173/business/%02d", index),
			VisibleText:    fmt.Sprintf("Business screen %02d", index),
			ScreenshotPath: writeTinyPNG(t, filepath.Join(sourceRoot, fmt.Sprintf("shot-%02d.png", index))),
		})
	}
	summary := pipelinepkg.TestFrontendE2ESummary{
		SchemaVersion: "p2r.frontend_e2e.v1",
		Status:        "passed",
	}
	materialized, materializedObservations, record := pipelinepkg.MaterializeStageGScreenshotArtifactsForTest(artifactRoot, summary, observations)
	if len(record.Findings) != 0 {
		t.Fatalf("unexpected artifact findings: %#v", record.Findings)
	}
	if len(materialized.Screenshots) != 10 {
		t.Fatalf("summary screenshots = %d, want 10: %#v", len(materialized.Screenshots), materialized.Screenshots)
	}
	legacy := filepath.Join(artifactRoot, "frontend_e2e_screenshot.png")
	if materialized.Screenshots[len(materialized.Screenshots)-1] != legacy {
		t.Fatalf("last screenshot should preserve legacy path: %#v", materialized.Screenshots)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("missing legacy screenshot: %v", err)
	}
	dirEntries, err := os.ReadDir(filepath.Join(artifactRoot, "frontend_e2e_screenshots"))
	if err != nil {
		t.Fatal(err)
	}
	if len(dirEntries) != 9 {
		t.Fatalf("expected 9 supplemental screenshots, got %d", len(dirEntries))
	}
	nonEmpty := 0
	for _, observation := range materializedObservations {
		if observation.ScreenshotPath != "" {
			nonEmpty++
			if _, err := os.Stat(observation.ScreenshotPath); err != nil {
				t.Fatalf("materialized observation screenshot missing: %v", err)
			}
		}
	}
	if nonEmpty != 10 {
		t.Fatalf("materialized observation screenshots = %d, want 10", nonEmpty)
	}
}

func TestStageGMaterializesTwoSupportScreenshotsForPassedEvidence(t *testing.T) {
	sourceRoot := t.TempDir()
	artifactRoot := t.TempDir()
	observations := []pipelinepkg.TestBrowserObservation{
		{
			Action:         "click_button",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/admin/dashboard",
			Title:          "Admin Dashboard",
			VisibleText:    "Admin Dashboard User Management Analytics Settings",
			ScreenshotPath: writeTinyPNG(t, filepath.Join(sourceRoot, "dashboard-1.png")),
		},
		{
			Action:         "snapshot",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/admin/dashboard",
			Title:          "Admin Dashboard",
			VisibleText:    "Admin Dashboard User Management Analytics Settings",
			ScreenshotPath: writeTinyPNG(t, filepath.Join(sourceRoot, "dashboard-2.png")),
		},
	}
	summary := pipelinepkg.TestFrontendE2ESummary{
		SchemaVersion: "p2r.frontend_e2e.v1",
		Status:        "passed",
	}
	materialized, materializedObservations, _ := pipelinepkg.MaterializeStageGScreenshotArtifactsForTest(artifactRoot, summary, observations)
	if len(materialized.Screenshots) != 2 {
		t.Fatalf("passed evidence screenshots = %#v, want 2", materialized.Screenshots)
	}
	nonEmpty := 0
	for _, observation := range materializedObservations {
		if observation.ScreenshotPath != "" {
			nonEmpty++
			if _, err := os.Stat(observation.ScreenshotPath); err != nil {
				t.Fatalf("materialized support screenshot missing: %v", err)
			}
		}
	}
	if nonEmpty != 2 {
		t.Fatalf("materialized support observations = %d, want 2", nonEmpty)
	}
}

func TestStageGMaterializesTextEvidenceWithoutFakeScreenshot(t *testing.T) {
	artifactRoot := t.TempDir()
	summary := pipelinepkg.TestFrontendE2ESummary{
		SchemaVersion: "p2r.frontend_e2e.v1",
		Status:        "blocked",
		Reason:        "Stage G was not executed.",
	}
	materialized, _, record := pipelinepkg.MaterializeStageGScreenshotArtifactsForTest(artifactRoot, summary, nil)
	if len(materialized.Screenshots) != 0 {
		t.Fatalf("text fallback must not be reported as screenshot: %#v", materialized.Screenshots)
	}
	if _, err := os.Stat(filepath.Join(artifactRoot, "frontend_e2e_screenshot.png")); !os.IsNotExist(err) {
		t.Fatalf("unexpected legacy screenshot artifact err=%v", err)
	}
	evidencePath := filepath.Join(artifactRoot, "frontend_e2e_evidence_summary.txt")
	if _, err := os.Stat(evidencePath); err != nil {
		t.Fatalf("expected text evidence summary: %v", err)
	}
	if !slices.Contains(record.ArtifactPaths, evidencePath) {
		t.Fatalf("record artifact paths missing evidence summary: %#v", record.ArtifactPaths)
	}
}

func TestStageGMaterializesFilteredScreenshotAsTextEvidenceOnly(t *testing.T) {
	artifactRoot := t.TempDir()
	sourceRoot := t.TempDir()
	emptyPath := filepath.Join(sourceRoot, "empty.png")
	if err := os.WriteFile(emptyPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	textPath := filepath.Join(sourceRoot, "not-png.png")
	if err := os.WriteFile(textPath, []byte("not a png"), 0o644); err != nil {
		t.Fatal(err)
	}
	observations := []pipelinepkg.TestBrowserObservation{
		{
			Action:         "fill_input",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/login",
			VisibleText:    "Login Email Password",
			ScreenshotPath: writeTinyPNG(t, filepath.Join(sourceRoot, "fill.png")),
		},
		{
			Action:         "snapshot",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/login",
			VisibleText:    "Login Email Password",
			ScreenshotPath: filepath.Join(sourceRoot, "missing.png"),
		},
		{
			Action:         "snapshot",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/dashboard",
			VisibleText:    "Dashboard",
			ScreenshotPath: emptyPath,
		},
		{
			Action:         "snapshot",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/reports",
			VisibleText:    "Reports",
			ScreenshotPath: textPath,
		},
	}
	summary := pipelinepkg.TestFrontendE2ESummary{
		SchemaVersion: "p2r.frontend_e2e.v1",
		Status:        "partial",
		Reason:        "planner timeout",
	}
	materialized, materializedObservations, record := pipelinepkg.MaterializeStageGScreenshotArtifactsForTest(artifactRoot, summary, observations)
	if len(materialized.Screenshots) != 0 {
		t.Fatalf("filtered screenshots must not be reported: %#v", materialized.Screenshots)
	}
	for _, observation := range materializedObservations {
		if observation.ScreenshotPath != "" {
			t.Fatalf("filtered observation screenshot should be cleared: %#v", materializedObservations)
		}
	}
	if _, err := os.Stat(filepath.Join(artifactRoot, "frontend_e2e_screenshot.png")); !os.IsNotExist(err) {
		t.Fatalf("unexpected legacy screenshot artifact err=%v", err)
	}
	evidencePath := filepath.Join(artifactRoot, "frontend_e2e_evidence_summary.txt")
	if _, err := os.Stat(evidencePath); err != nil {
		t.Fatalf("expected text evidence summary: %v", err)
	}
	if !slices.Contains(record.ArtifactPaths, evidencePath) {
		t.Fatalf("record artifact paths missing evidence summary: %#v", record.ArtifactPaths)
	}
}

func TestStageGMaterializesFindingEvidenceScreenshot(t *testing.T) {
	sourceRoot := t.TempDir()
	artifactRoot := t.TempDir()
	var observations []pipelinepkg.TestBrowserObservation
	for index := 0; index < 5; index++ {
		observations = append(observations, pipelinepkg.TestBrowserObservation{
			Action:         "snapshot",
			OK:             true,
			CurrentURL:     fmt.Sprintf("http://127.0.0.1:5173/business/%02d", index),
			VisibleText:    fmt.Sprintf("Business screen %02d", index),
			ScreenshotPath: writeTinyPNG(t, filepath.Join(sourceRoot, fmt.Sprintf("shot-%02d.png", index))),
		})
	}
	observations = append(observations, pipelinepkg.TestBrowserObservation{
		Action:         "click_button",
		OK:             true,
		CurrentURL:     "http://127.0.0.1:5173/editor/1",
		VisibleText:    "Editor No pages yet This page is empty",
		ScreenshotPath: writeTinyPNG(t, filepath.Join(sourceRoot, "editor-stuck.png")),
	})
	summary := pipelinepkg.TestFrontendE2ESummary{
		SchemaVersion: "p2r.frontend_e2e.v1",
		Status:        "failed",
	}
	materialized, materializedObservations, _ := pipelinepkg.MaterializeStageGScreenshotArtifactsForTest(artifactRoot, summary, observations)
	if len(materialized.Screenshots) != 6 {
		t.Fatalf("summary screenshots = %d, want 6: %#v", len(materialized.Screenshots), materialized.Screenshots)
	}
	editor := materializedObservations[len(materializedObservations)-1]
	if editor.ScreenshotPath == "" {
		t.Fatalf("expected final finding evidence screenshot to be retained: %#v", materializedObservations)
	}
	if filepath.Base(editor.ScreenshotPath) != "frontend_e2e_screenshot.png" {
		t.Fatalf("final finding screenshot should use legacy path, got %s", editor.ScreenshotPath)
	}
}

func TestStageGMaterializesMinimumSupportScreenshotsForFailedBlocker(t *testing.T) {
	sourceRoot := t.TempDir()
	artifactRoot := t.TempDir()
	loginURL := "http://127.0.0.1:5173/api/auth/login"
	var observations []pipelinepkg.TestBrowserObservation
	for index := 0; index < 5; index++ {
		observation := pipelinepkg.TestBrowserObservation{
			Action:         "snapshot",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/login",
			VisibleText:    "Login CAPTCHA",
			ScreenshotPath: writeTinyPNG(t, filepath.Join(sourceRoot, fmt.Sprintf("captcha-%02d.png", index))),
		}
		if index == 2 {
			observation.Action = "click_button"
			observation.VisibleText = "Invalid or expired CAPTCHA Login CAPTCHA"
			observation.NetworkIssues = []browserpkg.NetworkIssue{{URL: loginURL, Status: 400}}
			observation.NetworkEvents = []browserpkg.NetworkEvent{{URL: loginURL, Method: "POST", Status: 400}}
		}
		observations = append(observations, observation)
	}
	summary := pipelinepkg.TestFrontendE2ESummary{
		SchemaVersion: "p2r.frontend_e2e.v1",
		Status:        "failed",
		Findings: []pipelinepkg.FrontendE2EFinding{{
			Severity: "High",
			Title:    "CAPTCHA blocks login",
		}},
	}
	materialized, materializedObservations, _ := pipelinepkg.MaterializeStageGScreenshotArtifactsForTest(artifactRoot, summary, observations)
	if len(materialized.Screenshots) != 5 {
		t.Fatalf("summary screenshots = %d, want 5: %#v", len(materialized.Screenshots), materialized.Screenshots)
	}
	nonEmpty := 0
	for _, observation := range materializedObservations {
		if observation.ScreenshotPath != "" {
			nonEmpty++
		}
	}
	if nonEmpty != 5 {
		t.Fatalf("materialized observation screenshots = %d, want 5", nonEmpty)
	}
}

func TestStageGKeyScreenshotsSkipFillAndFailedRetries(t *testing.T) {
	root := t.TempDir()
	observations := []pipelinepkg.TestBrowserObservation{
		{
			Action:         "open_candidate",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/login",
			VisibleText:    "Login Username Password",
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "open.png")),
		},
		{
			Action:         "fill_input",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/login",
			VisibleText:    "Login Username Password",
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "fill.png")),
		},
		{
			Action:         "click_button",
			OK:             false,
			CurrentURL:     "http://127.0.0.1:5173/login",
			VisibleText:    "Login Username Password",
			Error:          "selector timed out",
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "failed-click.png")),
		},
		{
			Action:         "click_button",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/dashboard",
			VisibleText:    "Dashboard Admin Users",
			NetworkEvents:  []browserpkg.NetworkEvent{{URL: "http://127.0.0.1:5173/api/auth/login", Method: "POST", Status: 200}},
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "dashboard.png")),
		},
	}
	indexes := pipelinepkg.StageGKeyScreenshotObservationIndexesForTest(observations)
	if !slices.Equal(indexes, []int{0, 3}) {
		t.Fatalf("key screenshot indexes = %#v, want [0 3]", indexes)
	}
}

func TestStageGKeyScreenshotsSkipFailedFormRetries(t *testing.T) {
	root := t.TempDir()
	observations := []pipelinepkg.TestBrowserObservation{
		{
			Action:         "open_candidate",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/login",
			VisibleText:    "Login Username Password",
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "open.png")),
		},
		{
			Action:         "submit_local_form",
			OK:             false,
			CurrentURL:     "http://127.0.0.1:5173/login",
			VisibleText:    "Login Username Password",
			Error:          "locator.press: Timeout 5000ms exceeded",
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "failed-form.png")),
		},
		{
			Action:         "click_button",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/login",
			VisibleText:    "Login invalid credentials",
			NetworkIssues:  []browserpkg.NetworkIssue{{URL: "http://127.0.0.1:5173/api/auth/login", Status: 401}},
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "auth-failure.png")),
		},
	}
	indexes := pipelinepkg.StageGKeyScreenshotObservationIndexesForTest(observations)
	if !slices.Equal(indexes, []int{0, 2}) {
		t.Fatalf("key screenshot indexes = %#v, want [0 2]", indexes)
	}
}

func TestStageGKeyScreenshotsSuppressRecoveredAuthFailure(t *testing.T) {
	root := t.TempDir()
	loginURL := "http://127.0.0.1:5173/api/auth/login"
	observations := []pipelinepkg.TestBrowserObservation{
		{
			Action:         "open_candidate",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/login",
			VisibleText:    "Login Username Password",
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "open.png")),
		},
		{
			Action:         "click_button",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/login",
			VisibleText:    "Login invalid credentials",
			ConsoleErrors:  []string{"Failed to load resource: the server responded with a status of 401 (Unauthorized)"},
			NetworkIssues:  []browserpkg.NetworkIssue{{URL: loginURL, Status: 401}},
			NetworkEvents:  []browserpkg.NetworkEvent{{URL: loginURL, Method: "POST", Status: 401}},
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "auth-failure.png")),
		},
		{
			Action:         "fill_input",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/login",
			VisibleText:    "Login Username Password",
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "password-fill.png")),
		},
		{
			Action:         "click_button",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/dashboard",
			VisibleText:    "Dashboard Projects",
			NetworkEvents:  []browserpkg.NetworkEvent{{URL: loginURL, Method: "POST", Status: 200}},
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "dashboard.png")),
		},
	}
	indexes := pipelinepkg.StageGKeyScreenshotObservationIndexesForTest(observations)
	if !slices.Equal(indexes, []int{0, 3}) {
		t.Fatalf("key screenshot indexes = %#v, want [0 3]", indexes)
	}
}

func TestStageGKeyScreenshotsAllowReadOnlyChangedStates(t *testing.T) {
	root := t.TempDir()
	observations := []pipelinepkg.TestBrowserObservation{
		{
			Action:         "open_candidate",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/dashboard",
			VisibleText:    "Dashboard Overview",
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "open.png")),
		},
		{
			Action:         "collect_network",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/dashboard",
			VisibleText:    "Dashboard Overview",
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "network-same.png")),
		},
		{
			Action:         "wait",
			OK:             true,
			CurrentURL:     "http://127.0.0.1:5173/dashboard",
			VisibleText:    "Dashboard Overview Async jobs loaded",
			ScreenshotPath: writeTinyPNG(t, filepath.Join(root, "wait-changed.png")),
		},
	}
	indexes := pipelinepkg.StageGKeyScreenshotObservationIndexesForTest(observations)
	if !slices.Equal(indexes, []int{0, 2}) {
		t.Fatalf("key screenshot indexes = %#v, want [0 2]", indexes)
	}
}

func TestExtractJSONObjectAcceptsWrappedPlannerOutput(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "plain object",
			raw:  `{"action":"wait","reason":"observe"}`,
			want: `{"action":"wait","reason":"observe"}`,
		},
		{
			name: "fenced object",
			raw:  "```json\n{\"action\":\"wait\",\"reason\":\"observe\"}\n```",
			want: `{"action":"wait","reason":"observe"}`,
		},
		{
			name: "trailing prose",
			raw:  "{\"action\":\"wait\",\"reason\":\"observe\"}\nDone.",
			want: `{"action":"wait","reason":"observe"}`,
		},
		{
			name: "first complete object",
			raw:  "{\"action\":\"wait\",\"reason\":\"observe\"}\n{\"action\":\"finish\",\"reason\":\"done\"}",
			want: `{"action":"wait","reason":"observe"}`,
		},
		{
			name: "braces inside string",
			raw:  "note {not-json}\n{\"action\":\"wait\",\"reason\":\"text with { brace }\"}",
			want: `{"action":"wait","reason":"text with { brace }"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pipelinepkg.ExtractJSONObjectForTest(tc.raw)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("object = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestExtractJSONObjectRejectsMissingObject(t *testing.T) {
	if _, err := pipelinepkg.ExtractJSONObjectForTest("[]"); err == nil {
		t.Fatal("expected missing object error")
	}
}

func TestFrontendE2EObservationFindingsCanSuppressActionFailureFallbacks(t *testing.T) {
	observations := []pipelinepkg.TestBrowserObservation{
		{Action: "click_button", OK: false, Error: "selector timed out"},
	}
	if findings := pipelinepkg.FrontendE2EObservationFindingsForTest(observations, false); len(findings) != 0 {
		t.Fatalf("expected action failure fallback to be suppressed, got %#v", findings)
	}
	findings := pipelinepkg.FrontendE2EObservationFindingsForTest(observations, true)
	if len(findings) != 1 || findings[0].Title != "Browser action failed during frontend E2E" {
		t.Fatalf("expected action failure fallback finding, got %#v", findings)
	}
}

func TestFrontendE2EObservationFindingsSuppressRecoveredAuthFailure(t *testing.T) {
	loginURL := "http://127.0.0.1:5173/api/auth/login"
	observations := []pipelinepkg.TestBrowserObservation{
		{
			Action:        "click_button",
			OK:            true,
			ConsoleErrors: []string{"Failed to load resource: the server responded with a status of 401 (Unauthorized)"},
			NetworkIssues: []browserpkg.NetworkIssue{{URL: loginURL, Status: 401}},
			NetworkEvents: []browserpkg.NetworkEvent{{URL: loginURL, Method: "POST", Status: 401}},
		},
		{
			Action:        "click_button",
			OK:            true,
			NetworkEvents: []browserpkg.NetworkEvent{{URL: loginURL, Method: "POST", Status: 200}},
		},
	}
	if findings := pipelinepkg.FrontendE2EObservationFindingsForTest(observations, false); len(findings) != 0 {
		t.Fatalf("expected recovered auth failure to be suppressed, got %#v", findings)
	}
	observations = observations[:1]
	findings := pipelinepkg.FrontendE2EObservationFindingsForTest(observations, false)
	if len(findings) != 2 {
		t.Fatalf("expected unrecovered auth failure findings, got %#v", findings)
	}
}

func TestStageGPassedSummarySuppressesRecoveredActionFailures(t *testing.T) {
	passed := pipelinepkg.TestFrontendE2ESummary{Status: "passed"}
	if pipelinepkg.IncludeStageGActionFailureFallbackForTest(passed, nil) {
		t.Fatal("passed Stage G summary should not fail the stage for recovered intermediate action failures")
	}
	failed := pipelinepkg.TestFrontendE2ESummary{Status: "failed"}
	if !pipelinepkg.IncludeStageGActionFailureFallbackForTest(failed, nil) {
		t.Fatal("failed Stage G summary without own findings should include action failure fallback evidence")
	}
	withFindings := []model.Finding{{Title: "summary finding"}}
	if pipelinepkg.IncludeStageGActionFailureFallbackForTest(failed, withFindings) {
		t.Fatal("summary findings should suppress duplicate action failure fallback evidence")
	}
}

func TestStageGValidFinishWithFindingsIsStageDone(t *testing.T) {
	record := model.StageRecord{
		Stage:  string(model.StageG),
		Status: model.StageRunning,
		Findings: []model.Finding{{
			Stage:    string(model.StageG),
			Severity: "High",
			Title:    "login workflow failed",
		}},
	}
	if status := pipelinepkg.StageGFinishedStatusForTest(record); status != model.StageDone {
		t.Fatalf("valid Stage G finish with findings status = %s, want done", status)
	}
	record.Status = model.StageFailed
	record.ErrorSummary = "write required artifact frontend_e2e_report.md: permission denied"
	if status := pipelinepkg.StageGFinishedStatusForTest(record); status != model.StageFailed {
		t.Fatalf("artifact write failure status = %s, want failed", status)
	}
}

func TestStageGLogObservationIncludesNetworkEvidence(t *testing.T) {
	log := pipelinepkg.StageGLogObservationForTest(4, pipelinepkg.TestBrowserObservation{
		Action:     "click_button",
		OK:         true,
		CurrentURL: "http://127.0.0.1:5173/login",
		Title:      "FieldOrder Pro",
		NetworkIssues: []browserpkg.NetworkIssue{
			{URL: "http://127.0.0.1:5173/api/auth/login", Status: 401},
		},
		NetworkEvents: []browserpkg.NetworkEvent{
			{URL: "http://127.0.0.1:5173/api/auth/login", Method: "POST", Status: 401, ResourceType: "xhr"},
		},
	})
	for _, want := range []string{
		"network_issues: 1",
		"network_issue: http://127.0.0.1:5173/api/auth/login status=401",
		"network_events: 1",
		"network_event: POST http://127.0.0.1:5173/api/auth/login status=401 type=xhr",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("Stage G log missing %q:\n%s", want, log)
		}
	}
}

func TestStageGUnavailableDoesNotWriteFakeScreenshotArtifact(t *testing.T) {
	root := t.TempDir()
	projectPath := filepath.Join(root, "batch-1", "TASK-20260604-GSHOT")
	if err := os.MkdirAll(filepath.Join(projectPath, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectPath, "metadata.json"), []byte(`{"project_type":"pure_backend"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	artifactRoot := filepath.Join(root, "result", "run-g")
	run := model.RunRecord{RunID: "run-g", TaskID: "TASK-20260604-GSHOT", ArtifactRoot: artifactRoot}
	project := scanner.Project{TaskID: run.TaskID, Batch: "batch-1", Path: projectPath}
	record := pipelinepkg.NewRunner(&runtimeBlockStore{project: project}, config.Default()).StageGForTest(context.Background(), run, project, pipelinepkg.TestRuntimeEvidence{
		ComposeProject: "p2rqa-test",
		Mappings:       map[string][]pipelinepkg.TestPortMapping{},
	})
	if record.Status != model.StageDone {
		t.Fatalf("pure backend Stage G = %#v, want done", record)
	}
	if _, err := os.Stat(filepath.Join(artifactRoot, "frontend_e2e_screenshot.png")); !os.IsNotExist(err) {
		t.Fatalf("unexpected fallback screenshot artifact err=%v", err)
	}
	content, err := os.ReadFile(filepath.Join(artifactRoot, "logs", "G_frontend_e2e.log"))
	if err != nil {
		t.Fatalf("expected Stage G log artifact: %v", err)
	}
	if !strings.Contains(string(content), "Stage G frontend browser E2E") || !strings.Contains(string(content), "No browser frontend URL is expected") {
		t.Fatalf("unexpected Stage G log content: %s", string(content))
	}
}

func TestBrowserActionValidatorRejectsUnsafeActions(t *testing.T) {
	candidates := []pipelinepkg.TestBrowserURLCandidate{{ID: "url_1", URL: "http://127.0.0.1:3000", Origin: "http://127.0.0.1:3000"}}
	cases := []pipelinepkg.TestBrowserAction{
		{Action: "shell", Reason: "run command"},
		{Action: "open_candidate", URL: "https://example.com", URLID: "url_1", Reason: "external"},
		{Action: "snapshot", OutputPath: "/tmp/out.png", Reason: "write"},
		{Action: "delete_storage", Reason: "clear state"},
		{Action: "click_navigation", Text: "Log out", Reason: "leave session"},
		{Action: "click_navigation", Text: "Log-off", Reason: "leave session"},
		{Action: "click_button", Selector: "#logout", Reason: "inspect session exit"},
		{Action: "click_button", Text: "Sign_off", Reason: "inspect session exit"},
		{Action: "submit_local_form", Selector: "form", Reason: "sign out"},
	}
	for _, tc := range cases {
		if blocked := pipelinepkg.ValidateBrowserActionForTest(tc, candidates); blocked == nil {
			t.Fatalf("expected action %#v to be blocked", tc)
		}
	}
	if blocked := pipelinepkg.ValidateBrowserActionForTest(pipelinepkg.TestBrowserAction{Action: "open_candidate", URLID: "url_1", Reason: "open app"}, candidates); blocked != nil {
		t.Fatalf("valid action blocked: %#v", blocked)
	}
	if blocked := pipelinepkg.ValidateBrowserActionForTest(pipelinepkg.TestBrowserAction{Action: "click_navigation", Text: "Logs outage", Reason: "inspect operational logs"}, candidates); blocked != nil {
		t.Fatalf("non-session action blocked: %#v", blocked)
	}
}

func writeTinyPNG(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func stageGTestScreenshot(t *testing.T, policy browserpkg.Policy, fallback string) string {
	t.Helper()
	if policy.DisableScreenshot {
		return ""
	}
	path := strings.TrimSpace(policy.ScreenshotPath)
	if path == "" {
		path = fallback
	}
	return writeTinyPNG(t, path)
}

type stageGReplayFixture struct {
	Root         string
	RepoPath     string
	ArtifactRoot string
	Run          model.RunRecord
	Project      scanner.Project
	Runtime      pipelinepkg.TestRuntimeEvidence
	Cfg          config.Config
}

func newStageGReplayFixture(t *testing.T, taskID string) stageGReplayFixture {
	t.Helper()
	root := t.TempDir()
	projectPath := writePipelinePackage(t, root, "batch-1", taskID)
	controlDir := filepath.Join(root, ".qa-control")
	if _, err := assets.Release(controlDir); err != nil {
		t.Fatal(err)
	}
	artifactRoot := filepath.Join(root, "result", "batch-1", taskID, "run-g")
	cfg := config.Default()
	cfg.ScanPath = root
	cfg.Codex.PromptProfilesDir = filepath.Join(controlDir, "prompt_profiles")
	cfg.Pipeline.StageTimeouts["G"] = 60
	run := model.RunRecord{RunID: "run-g", TaskID: taskID, ArtifactRoot: artifactRoot}
	return stageGReplayFixture{
		Root:         root,
		RepoPath:     filepath.Join(projectPath, "repo"),
		ArtifactRoot: artifactRoot,
		Run:          run,
		Project:      scanner.Project{TaskID: taskID, Batch: "batch-1", Path: projectPath},
		Runtime: pipelinepkg.TestRuntimeEvidence{
			ComposeProject: "p2rqa-test",
			Services:       []string{"web"},
			Mappings: map[string][]pipelinepkg.TestPortMapping{
				"web": {{Service: "web", URL: "0.0.0.0", Host: 5173, Container: 5173, Protocol: "tcp"}},
			},
			Probes: []pipelinepkg.TestProbeResult{{Service: "web", URL: "http://127.0.0.1:5173", OK: true, Status: 200}},
		},
		Cfg: cfg,
	}
}

func (f stageGReplayFixture) NewRunner(opts ...pipelinepkg.RunnerOption) pipelinepkg.Runner {
	return pipelinepkg.NewRunner(&runtimeBlockStore{project: f.Project}, f.Cfg, opts...)
}

func readStageGSummaryForTest(t *testing.T, artifactRoot string) pipelinepkg.TestFrontendE2ESummary {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(artifactRoot, "frontend_e2e_summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var summary pipelinepkg.TestFrontendE2ESummary
	if err := json.Unmarshal(content, &summary); err != nil {
		t.Fatal(err)
	}
	return summary
}

func loginControls(emailValue, passwordValue bool) []browserpkg.ControlSummary {
	return []browserpkg.ControlSummary{
		{Role: "input", Name: "email", Type: "email", HasValue: emailValue},
		{Role: "input", Name: "password", Type: "password", HasValue: passwordValue},
		{Role: "button", Text: "Sign In", Type: "submit"},
	}
}

type runtimeBlockedRunner struct{}

func (runtimeBlockedRunner) LookPath(name string) (string, error) {
	if strings.Contains(strings.ToLower(name), "docker") {
		return "", errors.New("docker missing")
	}
	return name, nil
}

func (runtimeBlockedRunner) Run(ctx context.Context, timeout time.Duration, dir string, env []string, name string, args ...string) executor.Result {
	return executor.Result{Command: strings.Join(append([]string{name}, args...), " "), Stdout: name + " version\n"}
}

func (r runtimeBlockedRunner) RunStreamingWithOutput(ctx context.Context, timeout time.Duration, dir string, env []string, writer io.Writer, onOutput executor.OutputCallback, name string, args ...string) executor.Result {
	return r.Run(ctx, timeout, dir, env, name, args...)
}

type runtimeBlockStore struct {
	project scanner.Project
}

func (s *runtimeBlockStore) GetProject(context.Context, string) (scanner.Project, error) {
	return s.project, nil
}

func (s *runtimeBlockStore) GetRun(context.Context, string) (model.RunRecord, error) {
	return model.RunRecord{}, nil
}

func (s *runtimeBlockStore) ListRunsForTask(context.Context, string) ([]model.RunRecord, error) {
	return nil, nil
}

func (s *runtimeBlockStore) CreateRun(context.Context, model.RunRecord) error {
	return nil
}

func (s *runtimeBlockStore) PutStage(context.Context, string, model.StageRecord) error {
	return nil
}

func (s *runtimeBlockStore) PutStageAndRecordTaskRuntime(context.Context, string, model.StageRecord, string, string, bool, model.ComposeMeta) error {
	return nil
}

func (s *runtimeBlockStore) InsertFindings(context.Context, string, []model.Finding) error {
	return nil
}

func (s *runtimeBlockStore) FinishRun(context.Context, string, string, string, time.Duration) error {
	return nil
}

func TestFrontendE2ESummarySchemaValidation(t *testing.T) {
	valid := []byte(`{"schema_version":"p2r.frontend_e2e.v1","status":"failed","findings":[{"severity":"High","title":"blank page"}]}`)
	if _, err := pipelinepkg.ParseFrontendE2ESummaryForTest(valid); err != nil {
		t.Fatal(err)
	}
	invalid := []byte(`{"schema_version":"p2r.frontend_e2e.v1","status":"failed","findings":[{"severity":"Critical","title":"bad"}]}`)
	if _, err := pipelinepkg.ParseFrontendE2ESummaryForTest(invalid); err == nil {
		t.Fatal("expected invalid severity to fail")
	}
}

func TestRepoSnapshotDetectsSourceChangesAndIgnoresCaches(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "src", "app.js"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := pipelinepkg.SnapshotRepoForTest(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".pytest_cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".pytest_cache", "README.md"), []byte("cache"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "src", "app.js"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := pipelinepkg.SnapshotRepoForTest(repo)
	if err != nil {
		t.Fatal(err)
	}
	diff := pipelinepkg.RepoSnapshotDiffForTest(before, after)
	if len(diff) != 1 || diff[0] != "src/app.js" {
		t.Fatalf("diff = %#v", diff)
	}
}
