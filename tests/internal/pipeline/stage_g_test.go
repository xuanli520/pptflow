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
		"finish requires at least 5 key browser screenshots",
		"Do not use fill_input retries",
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

func TestStageGUnavailableWritesScreenshotArtifact(t *testing.T) {
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
	if _, err := os.Stat(filepath.Join(artifactRoot, "frontend_e2e_screenshot.png")); err != nil {
		t.Fatalf("expected fallback screenshot artifact: %v", err)
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
	}
	for _, tc := range cases {
		if blocked := pipelinepkg.ValidateBrowserActionForTest(tc, candidates); blocked == nil {
			t.Fatalf("expected action %#v to be blocked", tc)
		}
	}
	if blocked := pipelinepkg.ValidateBrowserActionForTest(pipelinepkg.TestBrowserAction{Action: "open_candidate", URLID: "url_1", Reason: "open app"}, candidates); blocked != nil {
		t.Fatalf("valid action blocked: %#v", blocked)
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
