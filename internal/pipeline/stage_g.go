package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	browserpkg "github.com/xuanli520/p2r_tui/internal/browser"
	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/frontende2e"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/preflight"
)

const stageGMaxActions = 30
const stageGMaxInvalidActions = 3
const stageGMinBrowserScreenshots = 5
const stageGMaxBrowserScreenshots = 10
const stageGAuthGateSubmitStallLimit = 2
const stageGRepeatedStateStallLimit = 7
const stageGBrowserProfileName = "frontend_e2e_browser.md"
const stageGBrowserActionPromptTemplateName = "frontend_e2e_browser_action_prompt.md"
const stageGLegacyScreenshotName = "frontend_e2e_screenshot.png"
const stageGEvidenceSummaryName = "frontend_e2e_evidence_summary.txt"
const stageGKeyScreenshotDirName = "frontend_e2e_screenshots"

func (r Runner) stageG(ctx context.Context, sc StageContext) model.StageRecord {
	start := time.Now()
	record := startStage(string(model.StageG))
	writer := artifactWriterForStageContext(sc)
	logPath := stageLogPath(sc.Run.ArtifactRoot, string(model.StageG))
	summaryPath := filepath.Join(sc.Run.ArtifactRoot, "frontend_e2e_summary.json")
	reportPath := qaArtifactPath(sc.Run.ArtifactRoot, "frontend_e2e_report.md")
	screenshotPath := qaArtifactPath(sc.Run.ArtifactRoot, stageGLegacyScreenshotName)
	observationsPath := filepath.Join(sc.Run.ArtifactRoot, "frontend_e2e_observations.json")
	record.LogPath = logPath
	record.ArtifactPaths = []string{logPath, summaryPath, reportPath, observationsPath}
	timeout := stageTimeoutForStageContext(sc, r, string(model.StageG), 600)
	stageCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	defer cleanupStageGBrowserRuntime(sc.Run.ArtifactRoot)

	candidates := browserURLCandidates(sc.Runtime)
	bestEffortStageText(&record, writer, writer.RelativePath(logPath), stageGLogHeader(sc, candidates))
	if !sc.Runtime.HasCleanupTarget() {
		reason := "Stage B runtime evidence is missing. Run Stage B successfully before Stage G."
		return r.finishStageGUnavailable(record, writer, start, reason, model.StageBlocked, FrontendE2ESummary{
			SchemaVersion: frontendE2ESchemaVersion,
			Status:        "blocked",
			Reason:        reason,
			URLCandidates: candidates,
			Findings:      []FrontendE2EFinding{},
		}, nil)
	}
	if projectTypeFromMetadata(sc.Project.Path) == config.ProjectTypePureBackend {
		reason := "No browser frontend URL is expected for pure_backend package metadata."
		return r.finishStageGUnavailable(record, writer, start, reason, model.StageDone, FrontendE2ESummary{
			SchemaVersion: frontendE2ESchemaVersion,
			Status:        "not_applicable",
			Reason:        reason,
			URLCandidates: candidates,
			Findings:      []FrontendE2EFinding{},
		}, nil)
	}
	if len(candidates) == 0 {
		reason := "Stage B did not expose any browser URL candidates."
		record.Findings = append(record.Findings, model.Finding{
			Stage:      string(model.StageG),
			Severity:   "High",
			Title:      "No frontend browser URL candidate was available",
			Rule:       "Stage G requires at least one Docker-published HTTP(S) candidate for frontend E2E exploration.",
			Evidence:   reason,
			Impact:     "Browser E2E evidence cannot be collected.",
			MinimumFix: "Expose the frontend service through Docker port mappings and rerun from Stage B.",
			SourcePath: summaryPath,
		})
		return r.finishStageGUnavailable(record, writer, start, reason, model.StageFailed, FrontendE2ESummary{
			SchemaVersion: frontendE2ESchemaVersion,
			Status:        "failed",
			Reason:        reason,
			URLCandidates: candidates,
			Findings:      []FrontendE2EFinding{},
		}, nil)
	}

	repoPath := filepath.Join(sc.Project.Path, "repo")
	before, snapshotErr := snapshotRepo(repoPath)
	if snapshotErr != nil {
		record.Findings = append(record.Findings, model.Finding{
			Stage:      string(model.StageG),
			Severity:   "High",
			Title:      "Stage G repository snapshot failed",
			Rule:       "Stage G must verify that browser exploration does not modify repo/ source files.",
			Evidence:   snapshotErr.Error(),
			Impact:     "Browser exploration can continue, but source mutation cannot be detected for this run.",
			MinimumFix: "Ensure repo/ is readable and rerun Stage G.",
			SourcePath: repoPath,
		})
		before = nil
	}

	profilePath := filepath.Join(r.cfg.Codex.PromptProfilesDir, stageGBrowserProfileName)
	profile, err := os.ReadFile(profilePath)
	if err != nil {
		record.Findings = append(record.Findings, model.Finding{
			Stage:      string(model.StageG),
			Severity:   "High",
			Title:      "Stage G prompt profile missing",
			Rule:       "Stage G requires the frontend browser prompt profile.",
			Evidence:   err.Error(),
			Impact:     "Codex browser planning cannot start.",
			MinimumFix: "Ensure assets were released and rerun Stage G.",
			SourcePath: profilePath,
		})
		return r.finishStageGUnavailable(record, writer, start, "prompt profile unavailable", model.StageFailed, FrontendE2ESummary{
			SchemaVersion: frontendE2ESchemaVersion,
			Status:        "failed",
			Reason:        "prompt profile unavailable",
			URLCandidates: candidates,
			Findings:      []FrontendE2EFinding{},
		}, nil)
	}
	promptTemplatePath := filepath.Join(r.cfg.Codex.PromptProfilesDir, stageGBrowserActionPromptTemplateName)
	promptTemplate, err := os.ReadFile(promptTemplatePath)
	if err != nil {
		record.Findings = append(record.Findings, model.Finding{
			Stage:      string(model.StageG),
			Severity:   "High",
			Title:      "Stage G browser prompt template missing",
			Rule:       "Stage G requires the frontend browser action prompt template.",
			Evidence:   err.Error(),
			Impact:     "Codex browser planning cannot start.",
			MinimumFix: "Ensure prompt profiles were released to .qa-control and rerun Stage G.",
			SourcePath: promptTemplatePath,
		})
		return r.finishStageGUnavailable(record, writer, start, "browser prompt template unavailable", model.StageFailed, FrontendE2ESummary{
			SchemaVersion: frontendE2ESchemaVersion,
			Status:        "failed",
			Reason:        "browser prompt template unavailable",
			URLCandidates: candidates,
			Findings:      []FrontendE2EFinding{},
		}, nil)
	}
	contextText := stageGBrowserContext(sc.Project.Path)
	nodePath := stageGNodePath(sc.Preflight)
	browserPolicy := browserpkg.Policy{
		AllowlistOrigins: browserAllowlistOrigins(candidates),
		ArtifactRoot:     sc.Run.ArtifactRoot,
	}

	explorer := frontende2e.Explorer{
		Ctx:            stageCtx,
		Candidates:     candidates,
		Policy:         browserPolicy,
		Deadline:       time.Now().Add(timeout),
		SummaryPath:    summaryPath,
		ScreenshotPath: writer.RelativePath(screenshotPath),
		LogPath:        logPath,
		Planner: func(ctx context.Context, candidates []BrowserURLCandidate, observations []browserpkg.Observation, blocked []BlockedBrowserAction, round int, timeout time.Duration) (string, []model.ArtifactWarning, error) {
			return r.nextStageGBrowserAction(ctx, sc, string(promptTemplate), string(profile), contextText, candidates, observations, blocked, round, timeout)
		},
		ActionRunner: func(ctx context.Context, action browserpkg.Action, policy browserpkg.Policy, timeout time.Duration) (browserpkg.Observation, error) {
			return r.runStageGBrowserAction(ctx, nodePath, action, policy, timeout)
		},
		Rules: frontende2e.ExplorerPolicy{
			ShouldCaptureActionScreenshot: stageGShouldCaptureActionScreenshot,
			RuntimeScreenshotPath: func(round int, action string) string {
				return stageGRuntimeScreenshotPath(sc.Run.ArtifactRoot, round, action)
			},
			PlannerTimedOut:                  stageGPlannerTimedOut,
			ObservationStopReason:            stageGObservationStopReason,
			FinishSummaryEvidenceBlockReason: stageGFinishSummaryEvidenceBlockReason,
			FinishScreenshotBlockReason:      stageGFinishScreenshotBlockReasonForSummary,
			Summary:                          stageGSummary,
			LogPlannedAction:                 stageGLogPlannedAction,
			LogObservation:                   stageGLogObservation,
			SchemaFailureFinding:             frontendE2ESchemaFailureFinding,
			SummaryFindings:                  frontendE2EFindings,
			IncludeActionFailureFallback:     includeStageGActionFailureFallback,
			ObservationFindings:              frontendE2EObservationFindings,
		},
		Events: frontende2e.ExplorerEvents{
			AppendLog: func(content string) {
				bestEffortStageAppend(&record, writer, writer.RelativePath(logPath), content)
			},
			WriteObservations: func(observations []browserpkg.Observation) {
				bestEffortStageJSON(&record, writer, writer.RelativePath(observationsPath), observations)
			},
			RecordWarnings: func(warnings []model.ArtifactWarning) {
				recordArtifactWarnings(&record, writer, warnings)
			},
			Progress: func(round int, action string, ok bool) {
				appendStreamProgress(sc.Run.RunID, string(model.StageG), fmt.Sprintf("G action %d: %s -> ok=%t", round, action, ok), "p2r", false, sc.Progress)
			},
		},
		Finishers: frontende2e.ExplorerFinishers{
			EvidenceVerdict: func(observations []browserpkg.Observation, blocked []BlockedBrowserAction, reason string) (model.StageRecord, bool) {
				return r.finishStageGEvidenceVerdict(record, writer, start, candidates, observations, blocked, reason, repoPath, before)
			},
			Partial: func(observations []browserpkg.Observation, blocked []BlockedBrowserAction, reason string) model.StageRecord {
				return r.finishStageGPartial(record, writer, start, candidates, observations, blocked, reason, repoPath, before)
			},
			Unavailable: func(reason, status string, summary FrontendE2ESummary, observations []browserpkg.Observation, findings []model.Finding) model.StageRecord {
				record.Findings = append(record.Findings, findings...)
				return r.finishStageGUnavailable(record, writer, start, reason, status, summary, observations)
			},
			AcceptedSummary: func(summary FrontendE2ESummary, observations []browserpkg.Observation, summaryFindings []model.Finding, observationFindings []model.Finding) model.StageRecord {
				record.Findings = append(record.Findings, summaryFindings...)
				record.Findings = append(record.Findings, observationFindings...)
				summary.Findings = append(summary.Findings, frontendE2EFindingsFromModel(observationFindings)...)
				record, summary = appendStageGRepoSnapshotFindings(record, summary, repoPath, before)
				record = r.writeStageGArtifacts(record, writer, summary, observations)
				record, status := stageGStatusForAcceptedSummary(summary, record)
				bestEffortStageAppend(&record, writer, writer.RelativePath(logPath), stageGLogFinish(summary.Status, record.ErrorSummary, len(observations), len(record.Findings)))
				return finishStage(record, status, start)
			},
		},
	}
	return explorer.Run()
}

func stageGPlannerTimedOut(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if ctx.Err() != nil {
		return true
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{"context deadline exceeded", "timed out", "timeout", "deadline exceeded", "deadline"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func (r Runner) nextStageGBrowserAction(ctx context.Context, sc StageContext, promptTemplate, profile, contextText string, candidates []BrowserURLCandidate, observations []browserpkg.Observation, blocked []BlockedBrowserAction, round int, timeout time.Duration) (string, []ArtifactWarning, error) {
	if r.stageGBrowserPlan != nil {
		return r.stageGBrowserPlan(ctx, sc, promptTemplate, profile, contextText, candidates, observations, blocked, round, timeout)
	}
	return r.nextBrowserAction(ctx, sc, promptTemplate, profile, contextText, candidates, observations, blocked, round, timeout)
}

func (r Runner) runStageGBrowserAction(ctx context.Context, nodePath string, action browserpkg.Action, policy browserpkg.Policy, timeout time.Duration) (browserpkg.Observation, error) {
	if r.stageGBrowserAction != nil {
		return r.stageGBrowserAction(ctx, action, policy, timeout)
	}
	runner := browserpkg.NewPlaywrightWrapper(r.exec, nodePath, policy)
	return runner.Run(ctx, action, timeout)
}

func stageGFinishedStatus(record model.StageRecord) string {
	if record.Status == model.StageFailed {
		return model.StageFailed
	}
	return model.StageDone
}

func stageGStatusForAcceptedSummary(summary FrontendE2ESummary, record model.StageRecord) (model.StageRecord, string) {
	switch strings.TrimSpace(summary.Status) {
	case "failed", "partial":
		if record.ErrorSummary == "" {
			record.ErrorSummary = "frontend E2E findings"
		}
		return record, model.StageFailed
	case "blocked":
		if record.ErrorSummary == "" {
			record.ErrorSummary = "frontend E2E blocked"
		}
		return record, model.StageBlocked
	default:
		return record, stageGFinishedStatus(record)
	}
}

func stageGNodePath(result preflight.CheckResult) string {
	for _, check := range result.Checks {
		if check.Name == "node" && strings.TrimSpace(check.Path) != "" && check.Status != "missing" {
			return check.Path
		}
	}
	return ""
}

func cleanupStageGBrowserRuntime(artifactRoot string) {
	if strings.TrimSpace(artifactRoot) == "" {
		return
	}
	_ = os.RemoveAll(filepath.Join(artifactRoot, "browser_runtime"))
}

func stageGRuntimeScreenshotPath(artifactRoot string, round int, action string) string {
	name := fmt.Sprintf("round_%02d_%s.png", round, stageGScreenshotSafeName(action))
	return filepath.Join(artifactRoot, "browser_runtime", "screenshots", name)
}

func stageGShouldCaptureActionScreenshot(action BrowserAction, observations []browserpkg.Observation) bool {
	actionName := strings.TrimSpace(action.Action)
	switch actionName {
	case "open_candidate", "wait", "snapshot", "collect_console", "collect_network", "click_navigation", "submit_local_form", "go_back":
		return true
	case "click_button":
		if stageGBrowserScreenshotCount(observations) < stageGMinBrowserScreenshots {
			return true
		}
		return stageGActionLooksBusinessCritical(action)
	default:
		return false
	}
}

func stageGActionLooksBusinessCritical(action BrowserAction) bool {
	text := strings.ToLower(strings.Join([]string{action.Text, action.Selector, action.Reason}, " "))
	for _, keyword := range []string{
		"login", "log in", "sign in", "signin", "submit", "admin", "dashboard", "studio", "analytics",
		"catalog", "product", "cart", "order", "checkout", "create", "save", "apply", "publish", "upload",
	} {
		if strings.Contains(text, keyword) {
			return true
		}
	}
	return false
}

func stageGScreenshotSafeName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "action"
	}
	var builder strings.Builder
	lastDash := false
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			builder.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			builder.WriteByte('-')
			lastDash = true
		}
	}
	result := strings.Trim(builder.String(), "-")
	if result == "" {
		return "action"
	}
	if len(result) > 48 {
		result = result[:48]
		result = strings.TrimRight(result, "-")
	}
	return result
}

func stageGFinishScreenshotBlockReason(observations []browserpkg.Observation) string {
	count := stageGBrowserScreenshotCount(observations)
	if count >= stageGMinBrowserScreenshots {
		return ""
	}
	return fmt.Sprintf("Stage G requires at least %d browser screenshots before finish; currently captured %d. Continue with snapshot, read-only observation, or non-destructive navigation until distinct key browser states are evidenced.", stageGMinBrowserScreenshots, count)
}

func stageGFinishScreenshotBlockReasonForSummary(summary FrontendE2ESummary, observations []browserpkg.Observation) string {
	reason := stageGFinishScreenshotBlockReason(observations)
	if reason == "" || stageGSummaryCanFinishWithLimitedScreenshots(summary, observations) {
		return ""
	}
	return reason
}

func stageGSummaryCanFinishWithLimitedScreenshots(summary FrontendE2ESummary, observations []browserpkg.Observation) bool {
	status := strings.TrimSpace(summary.Status)
	if status == "passed" && len(summary.Findings) == 0 {
		return stageGDeterministicPositiveEvidenceReady(observations)
	}
	if status != "failed" && status != "blocked" && status != "partial" {
		return false
	}
	if len(summary.Findings) == 0 {
		return false
	}
	if stageGAuthGateStallEvidence(observations) != "" {
		return true
	}
	if stageGBrowserToolErrorEvidence(observations) != "" {
		return true
	}
	if stageGNativeDialogBoundaryEvidence(observations) != "" {
		return true
	}
	for index := range observations {
		if stageGObservationHasProductFailureEvidenceAt(index, observations) {
			return true
		}
	}
	return false
}

func stageGFinishSummaryEvidenceBlockReason(summary FrontendE2ESummary, observations []browserpkg.Observation) string {
	status := strings.TrimSpace(summary.Status)
	if status != "failed" && status != "blocked" && status != "partial" {
		return ""
	}
	if len(summary.Findings) == 0 || stageGObservationsBackNonPassedSummary(observations) {
		return ""
	}
	return fmt.Sprintf("planner %s finish requires observation-backed Stage G evidence", status)
}

func stageGObservationsBackNonPassedSummary(observations []browserpkg.Observation) bool {
	if stageGPostAuthSessionLossEvidence(observations) != "" ||
		stageGAuthAcceptedNoTransitionEvidence(observations) != "" ||
		stageGAuthGateBlockerEvidence(observations) != "" ||
		stageGAuthSelectorBlockerEvidence(observations) != "" ||
		stageGAuthGateStallEvidence(observations) != "" ||
		stageGRepeatedStateStallEvidence(observations) != "" ||
		stageGNativeDialogBoundaryEvidence(observations) != "" ||
		stageGBrowserToolErrorEvidence(observations) != "" {
		return true
	}
	for index, observation := range observations {
		if stageGObservationHasProductFailureEvidenceAt(index, observations) {
			return true
		}
		if observation.CurrentURL != "" && len(strings.TrimSpace(observation.VisibleText)) < 10 {
			return true
		}
	}
	return false
}

func stageGBrowserToolErrorEvidence(observations []browserpkg.Observation) string {
	for index, observation := range observations {
		if observation.OK {
			continue
		}
		if observation.Metadata["p2r_error_kind"] != "browser_action_runner_error" {
			continue
		}
		evidence := strings.TrimSpace(observation.Error)
		if evidence == "" {
			evidence = "browser action runner returned an error"
		}
		return fmt.Sprintf("Browser action %s at observation %d could not be executed by the tool: %s", strings.TrimSpace(observation.Action), index+1, evidence)
	}
	return ""
}

func stageGBrowserToolErrorFinding(observations []browserpkg.Observation, reason string) (model.Finding, bool) {
	evidence := stageGBrowserToolErrorEvidence(observations)
	if evidence == "" {
		return model.Finding{}, false
	}
	return model.Finding{
		Stage:      string(model.StageG),
		Severity:   "High",
		Title:      "Browser tool error prevented frontend E2E coverage",
		Rule:       "Stage G browser action tool failures should be visible to the agent and resolved or concluded from observations.",
		Evidence:   evidence + " " + strings.TrimSpace(reason),
		Impact:     "Stage G could not collect enough browser evidence for the intended frontend workflow.",
		MinimumFix: "Inspect the browser action error, wrapper runtime, and selected action target; rerun Stage G after fixing the tool/runtime issue or improving the planner action.",
		SourcePath: "frontend_e2e_observations.json",
	}, true
}

func stageGNativeDialogBoundaryEvidence(observations []browserpkg.Observation) string {
	for index, observation := range observations {
		if observation.OK {
			continue
		}
		dialogType := strings.TrimSpace(observation.Metadata["p2r_dialog_type"])
		if dialogType != "prompt" && dialogType != "confirm" {
			continue
		}
		if strings.TrimSpace(observation.Metadata["p2r_dialog_action"]) != "dismissed" {
			continue
		}
		reason := strings.TrimSpace(observation.Metadata["p2r_dialog_reason"])
		if reason != "missing_action_value" && reason != "action_cannot_use_dialog_value" {
			continue
		}
		target := strings.TrimSpace(observation.CurrentURL)
		if target == "" {
			target = strings.TrimSpace(observation.Title)
		}
		var parts []string
		parts = append(parts, fmt.Sprintf("Browser action %s at observation %d opened a native %s dialog", strings.TrimSpace(observation.Action), index+1, dialogType))
		if message := strings.TrimSpace(observation.Metadata["p2r_dialog_message"]); message != "" {
			parts = append(parts, fmt.Sprintf("message=%q", message))
		}
		if defaultValue := strings.TrimSpace(observation.Metadata["p2r_dialog_default_value"]); defaultValue != "" {
			parts = append(parts, fmt.Sprintf("default=%q", defaultValue))
		}
		if target != "" {
			parts = append(parts, "at "+target)
		}
		parts = append(parts, "but the model action did not provide an explicit value, so p2r dismissed it and returned the boundary to the planner.")
		return strings.Join(parts, " ")
	}
	return ""
}

func stageGNativeDialogBoundaryFinding(observations []browserpkg.Observation, reason string) (model.Finding, bool) {
	evidence := stageGNativeDialogBoundaryEvidence(observations)
	if evidence == "" {
		return model.Finding{}, false
	}
	return model.Finding{
		Stage:      string(model.StageG),
		Severity:   "High",
		Title:      "Native browser dialog required explicit model input",
		Rule:       "Stage G must expose browser-native prompt and confirm dialogs to the planner instead of choosing dialog values automatically.",
		Evidence:   evidence + " " + strings.TrimSpace(reason),
		Impact:     "The browser workflow could not progress until the planner supplied an explicit dialog value.",
		MinimumFix: "Retry the safe browser action with action.value for the dialog, or replace the native dialog with deterministic in-page controls for E2E coverage.",
		SourcePath: "frontend_e2e_observations.json",
	}, true
}

func stageGBrowserScreenshotCount(observations []browserpkg.Observation) int {
	return len(stageGKeyScreenshotObservationIndexes(observations))
}

func stageGKeyScreenshotObservationIndexes(observations []browserpkg.Observation) []int {
	var eligible []int
	for index, observation := range observations {
		if stageGScreenshotObservationEligible(index, observation, observations) {
			eligible = append(eligible, index)
		}
	}
	if len(eligible) == 0 {
		return nil
	}
	selected := map[int]bool{}
	seenStates := map[string]bool{}
	add := func(index int) {
		if index < 0 || selected[index] {
			return
		}
		state := stageGObservationStateKey(observations[index])
		if state != "" && seenStates[state] {
			return
		}
		selected[index] = true
		if state != "" {
			seenStates[state] = true
		}
	}
	add(eligible[0])
	add(eligible[len(eligible)-1])
	for _, index := range eligible {
		observation := observations[index]
		if !observation.OK {
			add(index)
		}
		if len(observation.PageErrors) > 0 || len(observation.ConsoleErrors) > 0 || len(observation.NetworkIssues) > 0 {
			add(index)
		}
		if stageGObservationHasBusinessNetworkEvidenceAt(index, observations) {
			add(index)
		}
	}
	if len(selected) > stageGMaxBrowserScreenshots {
		return trimStageGScreenshotIndexes(sortedStageGIndexSet(selected))
	}
	for _, index := range eligible {
		if len(selected) >= stageGMaxBrowserScreenshots {
			break
		}
		observation := observations[index]
		if observation.CurrentURL != "" && stageGObservationURLChanged(index, observations) {
			add(index)
		}
	}
	for _, index := range eligible {
		if len(selected) >= stageGMaxBrowserScreenshots {
			break
		}
		add(index)
	}
	return sortedStageGIndexSet(selected)
}

func stageGScreenshotObservationEligible(index int, observation browserpkg.Observation, observations []browserpkg.Observation) bool {
	if path := strings.TrimSpace(observation.ScreenshotPath); path == "" || !stageGScreenshotFileUsable(path) {
		return false
	}
	action := strings.TrimSpace(observation.Action)
	if action == "fill_input" {
		return false
	}
	productFailure := stageGObservationHasProductFailureEvidenceAt(index, observations)
	if !observation.OK && !productFailure {
		return false
	}
	switch action {
	case "open_candidate", "snapshot":
		return strings.TrimSpace(observation.VisibleText) != "" || strings.TrimSpace(observation.CurrentURL) != "" || productFailure
	case "click_navigation", "submit_local_form", "go_back":
		return productFailure || (observation.OK && (stageGObservationURLChanged(index, observations) || stageGMeaningfulObservationStateChanged(index, observations) || stageGObservationHasBusinessNetworkEvidenceAt(index, observations)))
	case "click_button":
		return productFailure || (observation.OK && (stageGObservationURLChanged(index, observations) || stageGMeaningfulObservationStateChanged(index, observations) || stageGObservationHasBusinessNetworkEvidenceAt(index, observations)))
	case "wait", "collect_console", "collect_network":
		return observation.OK && stageGMeaningfulObservationStateChanged(index, observations)
	default:
		return productFailure
	}
}

func stageGMeaningfulObservationStateChanged(index int, observations []browserpkg.Observation) bool {
	if !stageGObservationStateChanged(index, observations) {
		return false
	}
	return !stageGObservationOnlyRecoveredAuthFailure(index, observations)
}

func stageGObservationHasProductFailureEvidenceAt(index int, observations []browserpkg.Observation) bool {
	if index < 0 || index >= len(observations) {
		return false
	}
	observation := observations[index]
	if len(observation.PageErrors) > 0 {
		return true
	}
	if len(observation.ConsoleErrors) > 0 &&
		!stageGConsoleErrorsOnlyRecoveredAuthNoise(index, observations) &&
		!stageGConsoleErrorsOnlyAuthGateNoise(index, observations) {
		return true
	}
	for _, issue := range observation.NetworkIssues {
		if stageGNetworkIssueBlocksEvidence(index, issue, observations) {
			return true
		}
	}
	for _, event := range observation.NetworkEvents {
		if stageGNetworkEventBlocksEvidence(index, event, observations) {
			return true
		}
	}
	return false
}

func stageGNetworkIssueBlocksEvidence(index int, issue browserpkg.NetworkIssue, observations []browserpkg.Observation) bool {
	if stageGNetworkFailureLooksAuthGateNoise(index, issue.URL, issue.Status, observations) {
		return false
	}
	if stageGNetworkFailureLooksPendingAuthRetry(index, issue.URL, issue.Status, observations) {
		return false
	}
	if stageGNetworkIssueRecovered(index, issue, observations) {
		return false
	}
	if stageGNetworkFailureLooksIgnorableNoise(issue.URL, "", issue.Status, issue.Error) {
		return false
	}
	return issue.Status >= 400 || strings.TrimSpace(issue.Error) != ""
}

func stageGNetworkEventBlocksEvidence(index int, event browserpkg.NetworkEvent, observations []browserpkg.Observation) bool {
	if stageGNetworkFailureLooksAuthGateNoise(index, event.URL, event.Status, observations) {
		return false
	}
	if stageGNetworkFailureLooksPendingAuthRetry(index, event.URL, event.Status, observations) {
		return false
	}
	if stageGNetworkEventRecovered(index, event, observations) {
		return false
	}
	if stageGNetworkFailureLooksIgnorableNoise(event.URL, event.ResourceType, event.Status, event.Error) {
		return false
	}
	return event.Status >= 400 || strings.TrimSpace(event.Error) != ""
}

func stageGConsoleErrorsOnlyRecoveredAuthNoise(index int, observations []browserpkg.Observation) bool {
	if index < 0 || index >= len(observations) {
		return false
	}
	observation := observations[index]
	if len(observation.ConsoleErrors) == 0 || len(observation.NetworkIssues) == 0 {
		return false
	}
	for _, issue := range observation.NetworkIssues {
		if !stageGNetworkIssueRecovered(index, issue, observations) {
			return false
		}
	}
	for _, message := range observation.ConsoleErrors {
		if !stageGConsoleErrorLooksLikeAuthNetworkNoise(message) {
			return false
		}
	}
	return true
}

func stageGConsoleErrorsOnlyAuthGateNoise(index int, observations []browserpkg.Observation) bool {
	if index < 0 || index >= len(observations) {
		return false
	}
	observation := observations[index]
	if len(observation.ConsoleErrors) == 0 {
		return false
	}
	hasAuthGateNoise := false
	for _, issue := range observation.NetworkIssues {
		if stageGNetworkFailureLooksAuthGateNoise(index, issue.URL, issue.Status, observations) ||
			stageGNetworkFailureLooksPendingAuthRetry(index, issue.URL, issue.Status, observations) {
			hasAuthGateNoise = true
			break
		}
	}
	if !hasAuthGateNoise {
		for _, event := range observation.NetworkEvents {
			if stageGNetworkFailureLooksAuthGateNoise(index, event.URL, event.Status, observations) ||
				stageGNetworkFailureLooksPendingAuthRetry(index, event.URL, event.Status, observations) {
				hasAuthGateNoise = true
				break
			}
		}
	}
	if !hasAuthGateNoise {
		return false
	}
	for _, message := range observation.ConsoleErrors {
		if !stageGConsoleErrorLooksLikeAuthNetworkNoise(message) {
			return false
		}
	}
	return true
}

func stageGObservationOnlyRecoveredAuthFailure(index int, observations []browserpkg.Observation) bool {
	if index < 0 || index >= len(observations) {
		return false
	}
	if stageGObservationURLChanged(index, observations) {
		return false
	}
	observation := observations[index]
	recovered := false
	for _, issue := range observation.NetworkIssues {
		if stageGNetworkIssueRecovered(index, issue, observations) {
			recovered = true
			continue
		}
		if issue.Status >= 400 || strings.TrimSpace(issue.Error) != "" {
			return false
		}
	}
	for _, event := range observation.NetworkEvents {
		if stageGNetworkEventRecovered(index, event, observations) {
			recovered = true
			continue
		}
		if event.Status >= 400 || strings.TrimSpace(event.Error) != "" {
			return false
		}
	}
	return recovered
}

func stageGConsoleErrorLooksLikeAuthNetworkNoise(message string) bool {
	message = strings.ToLower(strings.TrimSpace(message))
	if message == "" {
		return false
	}
	return strings.Contains(message, "401") ||
		strings.Contains(message, "403") ||
		strings.Contains(message, "422") ||
		strings.Contains(message, "unauthorized") ||
		strings.Contains(message, "forbidden")
}

func stageGNetworkIssueRecovered(index int, issue browserpkg.NetworkIssue, observations []browserpkg.Observation) bool {
	return stageGRecoverableAuthClientStatus(issue.Status) &&
		stageGNetworkURLLooksAuth(issue.URL) &&
		stageGLaterNetworkSuccess(index, issue.URL, observations)
}

func stageGNetworkEventRecovered(index int, event browserpkg.NetworkEvent, observations []browserpkg.Observation) bool {
	return stageGRecoverableAuthClientStatus(event.Status) &&
		stageGNetworkURLLooksAuth(event.URL) &&
		stageGLaterNetworkSuccess(index, event.URL, observations)
}

func stageGRecoverableAuthClientStatus(status int) bool {
	return frontende2e.RecoverableAuthClientStatus(status)
}

func stageGNetworkFailureLooksAuthGateNoise(index int, raw string, status int, observations []browserpkg.Observation) bool {
	return frontende2e.NetworkFailureLooksAuthGateNoise(index, raw, status, observations)
}

func stageGNetworkFailureLooksPendingAuthRetry(index int, raw string, status int, observations []browserpkg.Observation) bool {
	return frontende2e.NetworkFailureLooksPendingAuthRetry(index, raw, status, observations)
}

func stageGLaterNetworkSuccess(index int, rawURL string, observations []browserpkg.Observation) bool {
	key := stageGNetworkURLKey(rawURL)
	if key == "" {
		return false
	}
	for next := index + 1; next < len(observations); next++ {
		for _, event := range observations[next].NetworkEvents {
			if stageGNetworkURLKey(event.URL) == key && event.Status >= 200 && event.Status < 400 {
				return true
			}
		}
	}
	return false
}

func stageGNetworkURLLooksAuth(raw string) bool {
	return frontende2e.NetworkURLLooksAuth(raw)
}

func stageGNetworkURLLooksLogout(raw string) bool {
	return frontende2e.NetworkURLLooksLogout(raw)
}

func stageGNetworkURLKey(raw string) string {
	return frontende2e.NetworkURLKey(raw)
}

func stageGObservationURLChanged(index int, observations []browserpkg.Observation) bool {
	if index <= 0 || index >= len(observations) {
		return false
	}
	current := strings.TrimSpace(observations[index].CurrentURL)
	if current == "" {
		return false
	}
	for previous := index - 1; previous >= 0; previous-- {
		prev := strings.TrimSpace(observations[previous].CurrentURL)
		if prev != "" {
			return prev != current
		}
	}
	return true
}

func stageGObservationStateChanged(index int, observations []browserpkg.Observation) bool {
	if index <= 0 || index >= len(observations) {
		return false
	}
	current := stageGObservationStateKey(observations[index])
	if current == "" {
		return false
	}
	for previous := index - 1; previous >= 0; previous-- {
		prev := stageGObservationStateKey(observations[previous])
		if prev != "" {
			return prev != current
		}
	}
	return true
}

func stageGObservationStateKey(observation browserpkg.Observation) string {
	parts := []string{
		strings.TrimSpace(observation.CurrentURL),
		strings.TrimSpace(observation.Title),
		stageGCompactStateText(observation.VisibleText, 900),
		stageGControlsStateKey(observation.Controls),
	}
	hasState := false
	for _, part := range parts {
		if part != "" {
			hasState = true
			break
		}
	}
	if !hasState {
		return ""
	}
	return strings.Join(parts, "\x00")
}

func stageGControlsStateKey(controls []browserpkg.ControlSummary) string {
	if len(controls) == 0 {
		return ""
	}
	var parts []string
	for index, control := range controls {
		if index >= 40 {
			break
		}
		text := stageGCompactStateText(strings.Join([]string{control.Role, control.Text, control.Name, control.Placeholder, control.Type}, " "), 160)
		if text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "|")
}

func stageGCompactStateText(value string, limit int) string {
	value = strings.ToLower(strings.Join(strings.Fields(value), " "))
	if limit > 0 && len(value) > limit {
		value = value[:limit]
	}
	return value
}

func stageGObservationHasBusinessNetworkEvidenceAt(index int, observations []browserpkg.Observation) bool {
	if index < 0 || index >= len(observations) {
		return false
	}
	for _, event := range observations[index].NetworkEvents {
		if stageGNetworkEventBlocksEvidence(index, event, observations) {
			return true
		}
		if event.Status >= 400 || event.Error != "" {
			continue
		}
		if stageGNetworkEventLooksSuccessfulBusiness(event) {
			return true
		}
	}
	return false
}

func evenlySampledScreenshotIndex(eligible []int, selected map[int]bool) int {
	if len(eligible) == 0 {
		return -1
	}
	bestIndex := -1
	bestDistance := -1
	for _, candidate := range eligible {
		if selected[candidate] {
			continue
		}
		distance := nearestSelectedDistance(candidate, selected)
		if distance > bestDistance {
			bestDistance = distance
			bestIndex = candidate
		}
	}
	return bestIndex
}

func nearestSelectedDistance(candidate int, selected map[int]bool) int {
	best := -1
	for index := range selected {
		distance := candidate - index
		if distance < 0 {
			distance = -distance
		}
		if best < 0 || distance < best {
			best = distance
		}
	}
	if best < 0 {
		return candidate
	}
	return best
}

func trimStageGScreenshotIndexes(indexes []int) []int {
	keep := map[int]bool{}
	if len(indexes) == 0 {
		return nil
	}
	keep[indexes[0]] = true
	keep[indexes[len(indexes)-1]] = true
	for len(keep) < stageGMaxBrowserScreenshots {
		next := evenlySampledScreenshotIndex(indexes, keep)
		if next < 0 {
			break
		}
		keep[next] = true
	}
	result := make([]int, 0, len(keep))
	for index := range keep {
		result = append(result, index)
	}
	sort.Ints(result)
	return result
}

func sortedStageGIndexSet(indexes map[int]bool) []int {
	result := make([]int, 0, len(indexes))
	for index := range indexes {
		result = append(result, index)
	}
	sort.Ints(result)
	return result
}

func stageGLogHeader(sc StageContext, candidates []BrowserURLCandidate) string {
	var builder strings.Builder
	builder.WriteString("Stage G frontend browser E2E\n")
	builder.WriteString(fmt.Sprintf("run_id: %s\n", sc.Run.RunID))
	builder.WriteString(fmt.Sprintf("task_id: %s\n", sc.Run.TaskID))
	builder.WriteString(fmt.Sprintf("candidate_count: %d\n", len(candidates)))
	for _, candidate := range candidates {
		builder.WriteString(fmt.Sprintf("candidate %s: %s service=%s source=%s probe_ok=%t\n", candidate.ID, candidate.URL, candidate.Service, candidate.Source, candidate.ProbeOK))
	}
	builder.WriteString("\n")
	return builder.String()
}

func stageGLogPlannedAction(round int, action BrowserAction) string {
	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("round %d planned action=%s", round, strings.TrimSpace(action.Action)))
	if text := compactLogValue(action.Text, 120); text != "" {
		builder.WriteString(" text=" + text)
	}
	if selector := compactLogValue(action.Selector, 160); selector != "" {
		builder.WriteString(" selector=" + selector)
	}
	if action.Action == "fill_input" && action.Value != "" {
		builder.WriteString(fmt.Sprintf(" value=[REDACTED len=%d]", len(action.Value)))
	}
	if action.URLID != "" {
		builder.WriteString(" url_id=" + compactLogValue(action.URLID, 80))
	}
	if reason := compactLogValue(action.Reason, 180); reason != "" {
		builder.WriteString(" reason=" + reason)
	}
	builder.WriteString("\n")
	return builder.String()
}

func compactLogValue(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		return ""
	}
	if limit > 0 && len(value) > limit {
		value = value[:limit]
	}
	return fmt.Sprintf("%q", value)
}

func stageGLogObservation(round int, observation browserpkg.Observation) string {
	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("round %d action=%s ok=%t url=%s title=%s\n", round, observation.Action, observation.OK, observation.CurrentURL, observation.Title))
	if strings.TrimSpace(observation.ScreenshotPath) != "" {
		builder.WriteString("screenshot: " + strings.TrimSpace(observation.ScreenshotPath) + "\n")
	}
	if strings.TrimSpace(observation.Error) != "" {
		builder.WriteString("error: " + strings.TrimSpace(observation.Error) + "\n")
	}
	if len(observation.PageErrors) > 0 {
		builder.WriteString(fmt.Sprintf("page_errors: %d\n", len(observation.PageErrors)))
	}
	if len(observation.ConsoleErrors) > 0 {
		builder.WriteString(fmt.Sprintf("console_errors: %d\n", len(observation.ConsoleErrors)))
	}
	if len(observation.NetworkIssues) > 0 {
		builder.WriteString(fmt.Sprintf("network_issues: %d\n", len(observation.NetworkIssues)))
		for _, issue := range observation.NetworkIssues {
			if issue.Status > 0 {
				builder.WriteString(fmt.Sprintf("- network_issue: %s status=%d\n", issue.URL, issue.Status))
			} else {
				builder.WriteString(fmt.Sprintf("- network_issue: %s %s\n", issue.URL, issue.Error))
			}
		}
	}
	if len(observation.NetworkEvents) > 0 {
		builder.WriteString(fmt.Sprintf("network_events: %d\n", len(observation.NetworkEvents)))
		for _, event := range observation.NetworkEvents {
			builder.WriteString("- network_event: " + stageGNetworkEventText(event) + "\n")
		}
	}
	return builder.String()
}

func stageGNetworkEventText(event browserpkg.NetworkEvent) string {
	var parts []string
	if event.Method != "" {
		parts = append(parts, event.Method)
	}
	if event.URL != "" {
		parts = append(parts, event.URL)
	}
	if event.Status > 0 {
		parts = append(parts, fmt.Sprintf("status=%d", event.Status))
	}
	if event.ResourceType != "" {
		parts = append(parts, "type="+event.ResourceType)
	}
	if event.Error != "" {
		parts = append(parts, "error="+event.Error)
	}
	return strings.Join(parts, " ")
}

func stageGNetworkIssueText(issue browserpkg.NetworkIssue) string {
	var parts []string
	if issue.URL != "" {
		parts = append(parts, issue.URL)
	}
	if issue.Status > 0 {
		parts = append(parts, fmt.Sprintf("status=%d", issue.Status))
	}
	if issue.Error != "" {
		parts = append(parts, "error="+issue.Error)
	}
	return strings.Join(parts, " ")
}

func stageGLogFinish(status, reason string, observationCount, findingCount int) string {
	var builder strings.Builder
	builder.WriteString("\nfinish\n")
	builder.WriteString("status: " + strings.TrimSpace(status) + "\n")
	if strings.TrimSpace(reason) != "" {
		builder.WriteString("reason: " + strings.TrimSpace(reason) + "\n")
	}
	builder.WriteString(fmt.Sprintf("observations: %d\n", observationCount))
	builder.WriteString(fmt.Sprintf("findings: %d\n", findingCount))
	return builder.String()
}

func (r Runner) finishStageGUnavailable(record model.StageRecord, writer ArtifactWriter, start time.Time, reason, status string, summary FrontendE2ESummary, observations []browserpkg.Observation) model.StageRecord {
	if status != model.StageDone && record.ErrorSummary == "" {
		record.ErrorSummary = reason
	}
	summary.Findings = append(summary.Findings, frontendE2EFindingsFromModel(record.Findings)...)
	record = r.writeStageGArtifacts(record, writer, summary, observations)
	bestEffortStageAppend(&record, writer, writer.RelativePath(record.LogPath), stageGLogFinish(summary.Status, reason, len(observations), len(record.Findings)))
	return finishStage(record, status, start)
}

func (r Runner) finishStageGOutcome(record model.StageRecord, writer ArtifactWriter, start time.Time, summary FrontendE2ESummary, observations []browserpkg.Observation, repoPath string, before map[string]string) model.StageRecord {
	observationFindings := frontendE2EObservationFindings(observations, writer.RelativePath(writer.Path(stageGLegacyScreenshotName)), false)
	record.Findings = append(record.Findings, observationFindings...)
	summary.Findings = append(summary.Findings, frontendE2EFindingsFromModel(record.Findings)...)
	record, summary = appendStageGRepoSnapshotFindings(record, summary, repoPath, before)
	record = r.writeStageGArtifacts(record, writer, summary, observations)
	bestEffortStageAppend(&record, writer, writer.RelativePath(record.LogPath), stageGLogFinish(summary.Status, summary.Reason, len(observations), len(record.Findings)))
	return finishStage(record, stageGFinishedStatus(record), start)
}

func (r Runner) finishStageGEvidenceVerdict(record model.StageRecord, writer ArtifactWriter, start time.Time, candidates []BrowserURLCandidate, observations []browserpkg.Observation, blocked []BlockedBrowserAction, reason string, repoPath string, before map[string]string) (model.StageRecord, bool) {
	if outcome, ok := stageGPositiveEvidenceOutcome(candidates, observations, blocked, reason); ok {
		return r.finishStageGOutcome(record, writer, start, outcome, observations, repoPath, before), true
	}
	if finding, ok := stageGPartialProductBlockerFinding(observations, reason); ok {
		record.Findings = append(record.Findings, finding)
		record.ErrorSummary = "frontend E2E findings"
		summary := stageGSummary("failed", finding.Evidence, candidates, observations, blocked)
		summary.Findings = append(summary.Findings, frontendE2EFindingsFromModel(record.Findings)...)
		record, summary = appendStageGRepoSnapshotFindings(record, summary, repoPath, before)
		record = r.writeStageGArtifacts(record, writer, summary, observations)
		bestEffortStageAppend(&record, writer, writer.RelativePath(record.LogPath), stageGLogFinish(summary.Status, reason, len(observations), len(record.Findings)))
		return finishStage(record, model.StageFailed, start), true
	}
	return record, false
}

func appendStageGRepoSnapshotFindings(record model.StageRecord, summary FrontendE2ESummary, repoPath string, before map[string]string) (model.StageRecord, FrontendE2ESummary) {
	if before == nil {
		return record, summary
	}
	after, snapshotErr := snapshotRepo(repoPath)
	if snapshotErr != nil {
		finding := model.Finding{
			Stage:      string(model.StageG),
			Severity:   "High",
			Title:      "Stage G repository post-snapshot failed",
			Rule:       "Stage G must verify that browser exploration does not modify repo/ source files.",
			Evidence:   snapshotErr.Error(),
			Impact:     "Source mutation cannot be ruled out for this run.",
			MinimumFix: "Ensure repo/ is readable and rerun Stage G.",
			SourcePath: repoPath,
		}
		record.Findings = append(record.Findings, finding)
		summary.Findings = append(summary.Findings, frontendE2EFindingFromModel(finding))
		return record, summary
	}
	if changes := repoSnapshotDiff(before, after); len(changes) > 0 {
		finding := repoChangedFinding(changes, repoPath)
		record.Findings = append(record.Findings, finding)
		summary.Findings = append(summary.Findings, frontendE2EFindingFromModel(finding))
	}
	return record, summary
}

func (r Runner) finishStageGPartial(record model.StageRecord, writer ArtifactWriter, start time.Time, candidates []BrowserURLCandidate, observations []browserpkg.Observation, blocked []BlockedBrowserAction, reason string, repoPath string, before map[string]string) model.StageRecord {
	if finished, ok := r.finishStageGEvidenceVerdict(record, writer, start, candidates, observations, blocked, reason, repoPath, before); ok {
		return finished
	}
	if finding, ok := stageGBrowserToolErrorFinding(observations, reason); ok {
		record.Findings = append(record.Findings, finding)
		record.ErrorSummary = "frontend E2E findings"
		summary := stageGSummary("failed", finding.Evidence, candidates, observations, blocked)
		summary.Findings = append(summary.Findings, frontendE2EFindingsFromModel(record.Findings)...)
		record, summary = appendStageGRepoSnapshotFindings(record, summary, repoPath, before)
		record = r.writeStageGArtifacts(record, writer, summary, observations)
		bestEffortStageAppend(&record, writer, writer.RelativePath(record.LogPath), stageGLogFinish(summary.Status, reason, len(observations), len(record.Findings)))
		return finishStage(record, model.StageFailed, start)
	}
	record.Findings = append(record.Findings, model.Finding{
		Stage:      string(model.StageG),
		Severity:   "High",
		Title:      "Stage G browser exploration did not finish",
		Rule:       "Stage G must finish with a valid frontend E2E summary.",
		Evidence:   reason,
		Impact:     "Browser E2E evidence is incomplete.",
		MinimumFix: "Inspect frontend_e2e_observations.json and rerun Stage G.",
	})
	record.ErrorSummary = "frontend E2E incomplete"
	summary := stageGSummary("partial", reason, candidates, observations, blocked)
	summary.Findings = append(summary.Findings, frontendE2EFindingsFromModel(record.Findings)...)
	record, summary = appendStageGRepoSnapshotFindings(record, summary, repoPath, before)
	record = r.writeStageGArtifacts(record, writer, summary, observations)
	bestEffortStageAppend(&record, writer, writer.RelativePath(record.LogPath), stageGLogFinish(summary.Status, reason, len(observations), len(record.Findings)))
	return finishStage(record, model.StageFailed, start)
}

func stageGPositiveEvidenceOutcome(candidates []BrowserURLCandidate, observations []browserpkg.Observation, blocked []BlockedBrowserAction, reason string) (FrontendE2ESummary, bool) {
	profile := stageGEvidenceProfileForObservations(observations)
	if !profile.DeterministicPassReady() {
		return FrontendE2ESummary{}, false
	}
	summary := stageGSummary("passed", reason, candidates, observations, blocked)
	if blockReason := stageGFinishScreenshotBlockReasonForSummary(summary, observations); blockReason != "" {
		return FrontendE2ESummary{}, false
	}
	summary.Notes = append(summary.Notes, stageGEvidenceNote(observations))
	return summary, true
}

type stageGEvidenceProfile struct {
	HasUnrecoveredProductFailure bool
	HasPostAuthSessionLoss       bool
	HasRenderedFrontend          bool
	AuthSuccess                  bool
	AuthenticatedState           bool
	BusinessEndpointCount        int
	BusinessUISignalCount        int
	InteractiveProductUICount    int
	InteractiveProductUIStates   int
	ProductNavigationChangeCount int
	DistinctMeaningfulStates     int
	SupportScreenshotCount       int
	KeyScreenshotCount           int
}

func stageGEvidenceProfileForObservations(observations []browserpkg.Observation) stageGEvidenceProfile {
	evidenceObservations := observations
	authSuccess := false
	if authIndex := stageGCredentialedAuthSuccessIndex(observations); authIndex >= 0 {
		evidenceObservations = observations[authIndex:]
		authSuccess = true
	}
	return stageGEvidenceProfile{
		HasUnrecoveredProductFailure: stageGHasUnrecoveredProductFailure(observations),
		HasPostAuthSessionLoss:       stageGPostAuthSessionLossEvidence(observations) != "",
		HasRenderedFrontend:          stageGHasRenderedFrontendEvidence(evidenceObservations),
		AuthSuccess:                  authSuccess,
		AuthenticatedState:           stageGHasAuthenticatedObservation(evidenceObservations),
		BusinessEndpointCount:        stageGSuccessfulBusinessNetworkEvidenceCount(evidenceObservations),
		BusinessUISignalCount:        stageGAuthenticatedBusinessUISignalCount(evidenceObservations),
		InteractiveProductUICount:    stageGInteractiveProductUICount(evidenceObservations),
		InteractiveProductUIStates:   stageGDistinctInteractiveProductUIStateCount(evidenceObservations),
		ProductNavigationChangeCount: stageGProductNavigationChangeCount(evidenceObservations),
		DistinctMeaningfulStates:     stageGDistinctMeaningfulStateCount(evidenceObservations),
		SupportScreenshotCount:       stageGSupportScreenshotCount(observations),
		KeyScreenshotCount:           stageGBrowserScreenshotCount(observations),
	}
}

func (profile stageGEvidenceProfile) DeterministicPassReady() bool {
	return !profile.HasUnrecoveredProductFailure &&
		!profile.HasPostAuthSessionLoss &&
		profile.HasRenderedFrontend &&
		profile.SupportScreenshotCount >= 2 &&
		profile.CoreBusinessWorkflowReady()
}

func (profile stageGEvidenceProfile) CoreBusinessWorkflowReady() bool {
	hasNetworkWorkflow := profile.BusinessEndpointCount >= 2
	hasStructuredUIWorkflow := profile.BusinessEndpointCount == 0 && profile.InteractiveProductUIStates >= 2 && profile.ProductNavigationChangeCount >= 1
	hasNamedUIWorkflow := profile.BusinessEndpointCount == 0 && profile.BusinessUISignalCount >= 2 && profile.DistinctMeaningfulStates >= 2
	hasAuthWorkflow := profile.AuthSuccess && profile.AuthenticatedState && (hasNetworkWorkflow || hasStructuredUIWorkflow || hasNamedUIWorkflow)
	hasPublicWorkflow := (hasNetworkWorkflow && profile.DistinctMeaningfulStates >= 2) || hasStructuredUIWorkflow || hasNamedUIWorkflow
	return hasAuthWorkflow || hasPublicWorkflow
}

func stageGDeterministicPositiveEvidenceReady(observations []browserpkg.Observation) bool {
	return stageGEvidenceProfileForObservations(observations).DeterministicPassReady()
}

func stageGSupportScreenshotCount(observations []browserpkg.Observation) int {
	count := 0
	for index := range observations {
		if stageGScreenshotObservationCanSupportSummary(index, observations) {
			count++
		}
	}
	return count
}

func stageGHasUnrecoveredProductFailure(observations []browserpkg.Observation) bool {
	for index := range observations {
		if stageGObservationHasProductFailureEvidenceAt(index, observations) {
			return true
		}
	}
	return false
}

func stageGHasRenderedFrontendEvidence(observations []browserpkg.Observation) bool {
	for _, observation := range observations {
		if !observation.OK {
			continue
		}
		if strings.TrimSpace(observation.CurrentURL) == "" {
			continue
		}
		if len(strings.TrimSpace(observation.VisibleText)) >= 20 {
			return true
		}
	}
	return false
}

func stageGHasAuthenticatedObservation(observations []browserpkg.Observation) bool {
	for _, observation := range observations {
		if (stageGObservationLooksAuthenticated(observation) || stageGObservationLooksInteractiveProductUI(observation)) && !stageGObservationLooksAuthGate(observation) {
			return true
		}
	}
	return false
}

func stageGHasSuccessfulBusinessNetworkEvidence(observations []browserpkg.Observation) bool {
	return stageGSuccessfulBusinessNetworkEvidenceCount(observations) > 0
}

func stageGSuccessfulBusinessNetworkEvidenceCount(observations []browserpkg.Observation) int {
	seen := map[string]bool{}
	for _, observation := range observations {
		for _, event := range observation.NetworkEvents {
			if stageGNetworkEventLooksSuccessfulBusiness(event) {
				key := stageGNetworkURLKey(event.URL)
				if key == "" {
					key = strings.TrimSpace(event.URL)
				}
				if key != "" {
					seen[key] = true
				}
			}
		}
	}
	return len(seen)
}

func stageGNetworkEventLooksSuccessfulBusiness(event browserpkg.NetworkEvent) bool {
	if event.Status < 200 || event.Status >= 400 || strings.TrimSpace(event.Error) != "" {
		return false
	}
	if stageGNetworkURLLooksAuth(event.URL) || stageGNetworkURLLooksLogout(event.URL) {
		return false
	}
	if stageGNetworkURLLooksFrameworkNoise(event.URL) {
		return false
	}
	resourceType := strings.ToLower(strings.TrimSpace(event.ResourceType))
	if resourceType == "image" || resourceType == "font" || resourceType == "stylesheet" || resourceType == "script" || resourceType == "websocket" {
		return false
	}
	parsed, err := url.Parse(strings.TrimSpace(event.URL))
	if err != nil {
		return false
	}
	path := strings.ToLower(parsed.Path)
	if path == "" || path == "/" {
		return false
	}
	for _, marker := range []string{"/api/", "/graphql", "/dashboard", "/admin", "/project", "/projects", "/user", "/users", "/audit", "/analytics", "/settings", "/module", "/modules", "/order", "/orders", "/product", "/products"} {
		if strings.Contains(path, marker) {
			return true
		}
	}
	method := strings.ToUpper(strings.TrimSpace(event.Method))
	return method == "POST" || method == "PUT" || method == "PATCH" || method == "DELETE"
}

func stageGNetworkFailureLooksIgnorableNoise(raw, resourceType string, status int, message string) bool {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" {
		return false
	}
	parsed, err := url.Parse(raw)
	target := raw
	if err == nil {
		target = strings.ToLower(parsed.Path)
	}
	resourceType = strings.ToLower(strings.TrimSpace(resourceType))
	if resourceType == "websocket" || resourceType == "eventsource" {
		return stageGNetworkURLLooksFrameworkNoise(raw)
	}
	for _, prefix := range []string{"/@vite", "/__vite", "/sockjs-node", "/socket.io", "/webpack"} {
		if strings.HasPrefix(target, prefix) {
			return true
		}
	}
	for _, marker := range []string{"hot-update", "hmr", "livereload", "__webpack", "vite_ping"} {
		if strings.Contains(target, marker) {
			return true
		}
	}
	if status >= 500 {
		return false
	}
	if strings.HasPrefix(target, "/_blazor") && (strings.Contains(target, "negotiate") || resourceType == "fetch" || strings.TrimSpace(message) != "" || status == 0) {
		return true
	}
	for _, suffix := range []string{".map", ".png", ".jpg", ".jpeg", ".svg", ".ico", ".woff", ".woff2", ".ttf"} {
		if strings.HasSuffix(target, suffix) {
			return true
		}
	}
	return false
}

func stageGNetworkURLLooksFrameworkNoise(raw string) bool {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" {
		return false
	}
	parsed, err := url.Parse(raw)
	target := raw
	if err == nil {
		target = strings.ToLower(parsed.Path)
	}
	for _, prefix := range []string{
		"/_blazor",
		"/_framework",
		"/_next/",
		"/@vite",
		"/__vite",
		"/sockjs-node",
		"/socket.io",
		"/webpack",
		"/vite",
	} {
		if strings.HasPrefix(target, prefix) {
			return true
		}
	}
	for _, marker := range []string{"hot-update", "hmr", "livereload", "__webpack", "vite_ping"} {
		if strings.Contains(target, marker) {
			return true
		}
	}
	for _, suffix := range []string{".js", ".css", ".map", ".png", ".jpg", ".jpeg", ".svg", ".ico", ".woff", ".woff2", ".ttf"} {
		if strings.HasSuffix(target, suffix) {
			return true
		}
	}
	return false
}

func stageGAuthenticatedBusinessUISignalCount(observations []browserpkg.Observation) int {
	signals := map[string]bool{}
	for _, observation := range observations {
		if !observation.OK || stageGObservationLooksAuthGate(observation) || stageGObservationHasPasswordControl(observation) {
			continue
		}
		stageGAddBusinessUISignals(signals, observation.CurrentURL)
		stageGAddBusinessUISignals(signals, observation.Title)
		stageGAddBusinessUISignals(signals, observation.VisibleText)
		for _, control := range observation.Controls {
			stageGAddBusinessUISignals(signals, strings.Join([]string{control.Role, control.Text, control.Name, control.Placeholder, control.Type}, " "))
		}
	}
	return len(signals)
}

func stageGInteractiveProductUICount(observations []browserpkg.Observation) int {
	count := 0
	for _, observation := range observations {
		if stageGObservationLooksInteractiveProductUI(observation) {
			count++
		}
	}
	return count
}

func stageGDistinctInteractiveProductUIStateCount(observations []browserpkg.Observation) int {
	seen := map[string]bool{}
	for _, observation := range observations {
		if !stageGObservationLooksInteractiveProductUI(observation) {
			continue
		}
		key := stageGObservationStateKey(observation)
		if key != "" {
			seen[key] = true
		}
	}
	return len(seen)
}

func stageGProductNavigationChangeCount(observations []browserpkg.Observation) int {
	changes := 0
	previous := ""
	for _, observation := range observations {
		if !stageGObservationLooksInteractiveProductUI(observation) {
			continue
		}
		current := stageGObservationURLPathKey(observation.CurrentURL)
		if current == "" {
			continue
		}
		if previous != "" && current != previous {
			changes++
		}
		previous = current
	}
	return changes
}

func stageGObservationURLPathKey(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return strings.ToLower(raw)
	}
	path := strings.TrimSpace(parsed.Path)
	if path == "" {
		path = "/"
	}
	return strings.ToLower(path)
}

func stageGObservationLooksInteractiveProductUI(observation browserpkg.Observation) bool {
	if !observation.OK || stageGObservationLooksAuthGate(observation) || stageGObservationLooksMarketingPage(observation) {
		return false
	}
	if stageGObservationHasPasswordControl(observation) {
		return false
	}
	text := strings.TrimSpace(observation.VisibleText)
	if len(text) < 60 {
		return false
	}
	links, buttons, inputs := stageGControlCounts(observation)
	controlCount := links + buttons + inputs
	if controlCount < 4 {
		return false
	}
	signals := 0
	if links >= 2 {
		signals++
	}
	if buttons >= 2 {
		signals++
	}
	if inputs >= 1 {
		signals++
	}
	if controlCount >= 6 {
		signals++
	}
	if stageGTextLooksDataOrWorkflow(text) {
		signals++
	}
	return signals >= 3
}

func stageGControlCounts(observation browserpkg.Observation) (links, buttons, inputs int) {
	for _, control := range observation.Controls {
		role := strings.ToLower(strings.TrimSpace(control.Role))
		target := strings.ToLower(strings.Join([]string{control.Role, control.Text, control.Name, control.Placeholder, control.Type}, " "))
		switch role {
		case "link":
			links++
		case "button":
			buttons++
		default:
			if strings.Contains(target, "password") || strings.Contains(target, "passwd") {
				continue
			}
			inputs++
		}
	}
	return links, buttons, inputs
}

func stageGTextLooksDataOrWorkflow(value string) bool {
	value = strings.ToLower(strings.Join(strings.Fields(value), " "))
	matches := 0
	for _, marker := range []string{"search", "filter", "status", "priority", "owner", "assigned", "recent", "activity", "queue", "review", "date", "total", "items", "calendar", "export"} {
		if strings.Contains(value, marker) {
			matches++
		}
	}
	return matches >= 3
}

func stageGObservationLooksMarketingPage(observation browserpkg.Observation) bool {
	urlValue := strings.ToLower(strings.TrimSpace(observation.CurrentURL))
	text := strings.ToLower(strings.Join(strings.Fields(observation.VisibleText), " "))
	if strings.Contains(urlValue, "/pricing") || strings.Contains(urlValue, "/contact") || strings.Contains(urlValue, "/about") || strings.Contains(urlValue, "/docs") {
		return true
	}
	matches := 0
	for _, marker := range []string{"pricing", "contact sales", "get started", "book a demo", "features", "testimonials", "hero", "learn more"} {
		if strings.Contains(text, marker) {
			matches++
		}
	}
	links, buttons, inputs := stageGControlCounts(observation)
	return matches >= 2 && inputs == 0 && links+buttons < 5
}

func stageGAddBusinessUISignals(signals map[string]bool, value string) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return
	}
	markers := map[string][]string{
		"dashboard":  {"dashboard", "/dashboard"},
		"admin":      {"admin", "/admin"},
		"analytics":  {"analytics", "metrics"},
		"users":      {"user management", "users", "/users"},
		"projects":   {"projects", "/projects"},
		"settings":   {"settings", "/settings"},
		"modules":    {"modules", "/modules"},
		"orders":     {"orders", "/orders"},
		"products":   {"products", "/products", "catalog"},
		"reports":    {"reports", "/reports"},
		"audit":      {"audit", "activity log"},
		"management": {"management"},
		"create":     {"create", "new "},
		"edit":       {"edit", "update"},
		"save":       {"save", "publish"},
	}
	for signal, candidates := range markers {
		for _, marker := range candidates {
			if strings.Contains(value, marker) {
				signals[signal] = true
				break
			}
		}
	}
}

func stageGDistinctMeaningfulStateCount(observations []browserpkg.Observation) int {
	seen := map[string]bool{}
	for _, observation := range observations {
		if !observation.OK || stageGObservationLooksAuthGate(observation) {
			continue
		}
		key := stageGObservationStateKey(observation)
		if key != "" {
			seen[key] = true
		}
	}
	return len(seen)
}

func stageGEvidenceNote(observations []browserpkg.Observation) string {
	profile := stageGEvidenceProfileForObservations(observations)
	return fmt.Sprintf("Deterministic Stage G evidence: auth_success=%t authenticated_state=%t business_network_endpoints=%d business_ui_signals=%d interactive_product_ui=%d interactive_product_states=%d product_navigation_changes=%d support_screenshots=%d key_screenshots=%d distinct_states=%d.", profile.AuthSuccess, profile.AuthenticatedState, profile.BusinessEndpointCount, profile.BusinessUISignalCount, profile.InteractiveProductUICount, profile.InteractiveProductUIStates, profile.ProductNavigationChangeCount, profile.SupportScreenshotCount, profile.KeyScreenshotCount, profile.DistinctMeaningfulStates)
}

func stageGPartialProductBlockerFinding(observations []browserpkg.Observation, reason string) (model.Finding, bool) {
	if evidence := stageGPostAuthSessionLossEvidence(observations); evidence != "" {
		return model.Finding{
			Stage:      string(model.StageG),
			Severity:   "High",
			Title:      "Authenticated browser session was lost during Stage G",
			Rule:       "Stage G must preserve an authenticated browser session while exploring the documented product workflow.",
			Evidence:   evidence + " " + strings.TrimSpace(reason),
			Impact:     "Stage G cannot trust later browser evidence because the workflow returned to an unauthenticated state.",
			MinimumFix: "Avoid logout/session-reset actions during E2E exploration and persist browser session state across Stage G browser actions.",
			SourcePath: "frontend_e2e_observations.json",
		}, true
	}
	if evidence := stageGAuthAcceptedNoTransitionEvidence(observations); evidence != "" {
		return model.Finding{
			Stage:      string(model.StageG),
			Severity:   "High",
			Title:      "Authentication response did not reach authenticated browser workflow",
			Rule:       "A successful credentialed authentication response should establish an authenticated browser session and route to the documented product workflow.",
			Evidence:   evidence + " " + strings.TrimSpace(reason),
			Impact:     "Stage G cannot verify the authenticated product workflow even though the auth request returned success.",
			MinimumFix: "Persist the browser session after login and redirect/render the authenticated workflow after successful credentials.",
			SourcePath: "frontend_e2e_observations.json",
		}, true
	}
	if evidence := stageGAuthGateBlockerEvidence(observations); evidence != "" {
		return model.Finding{
			Stage:      string(model.StageG),
			Severity:   "High",
			Title:      "Authentication gate prevented browser workflow coverage",
			Rule:       "README-provided local credentials or safe registration paths should allow Stage G to reach authenticated browser workflows, or the app should expose deterministic test authentication.",
			Evidence:   evidence + " " + strings.TrimSpace(reason),
			Impact:     "Stage G cannot verify the authenticated product workflow from the browser UI.",
			MinimumFix: "Provide deterministic E2E credentials or a test-mode CAPTCHA bypass, and ensure the registration/login controls are reachable with stable selectors.",
			SourcePath: "frontend_e2e_observations.json",
		}, true
	}
	if evidence := stageGAuthSelectorBlockerEvidence(observations); evidence != "" {
		return model.Finding{
			Stage:      string(model.StageG),
			Severity:   "High",
			Title:      "Authentication controls prevented browser workflow coverage",
			Rule:       "README-provided local credentials or safe registration paths should be usable through stable browser controls.",
			Evidence:   evidence + " " + strings.TrimSpace(reason),
			Impact:     "Stage G cannot verify the authenticated product workflow from the browser UI.",
			MinimumFix: "Expose stable login/register selectors or provide deterministic E2E authentication controls.",
			SourcePath: "frontend_e2e_observations.json",
		}, true
	}
	if evidence := stageGAuthGateStallEvidence(observations); evidence != "" {
		return model.Finding{
			Stage:      string(model.StageG),
			Severity:   "High",
			Title:      "Authentication gate prevented browser workflow coverage",
			Rule:       "README-provided local credentials or safe registration paths should allow Stage G to reach authenticated browser workflows, or the app should expose deterministic test authentication.",
			Evidence:   evidence + " " + strings.TrimSpace(reason),
			Impact:     "Stage G cannot verify the authenticated product workflow from the browser UI.",
			MinimumFix: "Provide deterministic E2E credentials or a test-mode auth bypass, and ensure successful login redirects to the documented product workflow.",
			SourcePath: "frontend_e2e_observations.json",
		}, true
	}
	if finding, ok := stageGNativeDialogBoundaryFinding(observations, reason); ok {
		return finding, true
	}
	for index := range observations {
		if stageGObservationHasProductFailureEvidenceAt(index, observations) {
			return model.Finding{
				Stage:      string(model.StageG),
				Severity:   "High",
				Title:      "Frontend workflow could not progress after product error",
				Rule:       "Browser workflows should either reach the documented business state or expose a clear product failure.",
				Evidence:   stageGPartialFailureEvidence(index, observations, reason),
				Impact:     "Stage G could not complete the documented frontend workflow.",
				MinimumFix: "Fix the observed frontend/API failure and rerun Stage G.",
				SourcePath: "frontend_e2e_observations.json",
			}, true
		}
	}
	return model.Finding{}, false
}

func stageGAuthSelectorBlockerEvidence(observations []browserpkg.Observation) string {
	if stageGObservedAuthRecovery(observations) {
		return ""
	}
	credentialEvidence := false
	selectorFailures := 0
	var firstFailure browserpkg.Observation
	for _, observation := range observations {
		if stageGObservationHasCredentialEvidence(observation) {
			credentialEvidence = true
		}
		if observation.OK || !stageGObservationLooksAuthSelectorFailure(observation) {
			continue
		}
		if !credentialEvidence && !stageGObservationLooksAuthInputAction(observation) {
			continue
		}
		if selectorFailures == 0 {
			firstFailure = observation
		}
		selectorFailures++
	}
	if selectorFailures < 2 {
		return ""
	}
	target := strings.TrimSpace(firstFailure.CurrentURL)
	if target == "" {
		target = "the authentication UI"
	}
	return fmt.Sprintf("Credentialed authentication control failed on %s after %d selector failure(s): %s", target, selectorFailures, strings.TrimSpace(firstFailure.Error))
}

func stageGObservationLooksAuthSelectorFailure(observation browserpkg.Observation) bool {
	errorText := strings.ToLower(strings.TrimSpace(observation.Error))
	if errorText == "" || !strings.Contains(errorText, "timeout") {
		return false
	}
	target := strings.ToLower(strings.Join([]string{observation.Action, observation.Error, observation.VisibleText, observation.CurrentURL}, " "))
	for _, marker := range []string{"email", "password", "username", "login", "sign in", "signin", "register", "auth", "form input"} {
		if strings.Contains(target, marker) {
			return true
		}
	}
	return false
}

func stageGObservationLooksAuthInputAction(observation browserpkg.Observation) bool {
	target := strings.ToLower(strings.Join([]string{observation.Action, observation.Error, observation.VisibleText, observation.CurrentURL}, " "))
	return strings.Contains(target, "fill_input") &&
		(strings.Contains(target, "email") || strings.Contains(target, "password") || strings.Contains(target, "username") || strings.Contains(target, "login"))
}

func stageGObservationStopReason(observations []browserpkg.Observation) string {
	if evidence := stageGPostAuthSessionLossEvidence(observations); evidence != "" {
		return "Stage G stopped after authenticated session was lost. " + evidence
	}
	if evidence := stageGAuthAcceptedNoTransitionEvidence(observations); evidence != "" {
		return "Stage G stopped after accepted authentication did not reach a product workflow. " + evidence
	}
	if stageGAuthGateStallEvidence(observations) != "" {
		return "Stage G stopped after repeated authentication-gate attempts did not reach a product workflow."
	}
	if evidence := stageGRepeatedStateStallEvidence(observations); evidence != "" {
		return "Stage G stopped after repeated unchanged browser observations. " + evidence
	}
	return ""
}

func stageGAuthGateBlockerEvidence(observations []browserpkg.Observation) string {
	authFailure := ""
	authSuccess := false
	stayedOnAuthGate := false
	registerSelectorFailures := 0
	captchaBoundary := false
	for _, observation := range observations {
		text := strings.ToLower(observation.VisibleText)
		urlValue := strings.ToLower(observation.CurrentURL)
		if strings.Contains(text, "captcha") || strings.Contains(text, "sign in") || strings.Contains(text, "register") || strings.Contains(urlValue, "/login") {
			stayedOnAuthGate = true
		}
		if !observation.OK && strings.Contains(strings.ToLower(observation.Error), "timeout") {
			target := strings.ToLower(strings.Join([]string{observation.Action, observation.Error, observation.VisibleText}, " "))
			if strings.Contains(target, "register") || strings.Contains(target, "captcha") || strings.Contains(target, "sign in") || strings.Contains(target, "login") {
				registerSelectorFailures++
			}
		}
		for _, issue := range observation.NetworkIssues {
			if stageGNetworkURLLooksAuth(issue.URL) && stageGRecoverableAuthClientStatus(issue.Status) {
				authFailure = fmt.Sprintf("%s status=%d", issue.URL, issue.Status)
				if stageGObservationAuthFailureLooksCaptchaBoundary(observation, issue.URL, issue.Status) {
					captchaBoundary = true
				}
			}
		}
		for _, event := range observation.NetworkEvents {
			if stageGNetworkURLLooksAuth(event.URL) && !stageGNetworkURLLooksLogout(event.URL) && event.Status >= 200 && event.Status < 400 && strings.ToUpper(strings.TrimSpace(event.Method)) == "POST" {
				authSuccess = true
			}
			if stageGNetworkURLLooksAuth(event.URL) && stageGRecoverableAuthClientStatus(event.Status) {
				authFailure = fmt.Sprintf("%s status=%d", event.URL, event.Status)
				if stageGObservationAuthFailureLooksCaptchaBoundary(observation, event.URL, event.Status) {
					captchaBoundary = true
				}
			}
		}
	}
	if authFailure == "" || authSuccess {
		return ""
	}
	credentialedFailures := stageGCredentialedAuthClientFailureCount(observations)
	if !captchaBoundary && credentialedFailures < stageGAuthGateSubmitStallLimit {
		return ""
	}
	evidence := "Observed authentication gate failure: " + authFailure + "."
	if stayedOnAuthGate {
		evidence += " Browser remained on login/register/CAPTCHA UI."
	}
	if credentialedFailures > 0 {
		evidence += fmt.Sprintf(" Credentialed auth failure attempts: %d.", credentialedFailures)
	}
	if registerSelectorFailures > 0 {
		evidence += fmt.Sprintf(" Repeated auth/register selector attempts failed %d time(s).", registerSelectorFailures)
	}
	return evidence
}

func stageGCredentialedAuthClientFailureCount(observations []browserpkg.Observation) int {
	return frontende2e.CredentialedAuthClientFailureCount(observations)
}

func stageGObservationAuthClientFailure(observation browserpkg.Observation) (string, int, bool) {
	return frontende2e.ObservationAuthClientFailure(observation)
}

func stageGObservationAuthFailureLooksCaptchaBoundary(observation browserpkg.Observation, raw string, status int) bool {
	return frontende2e.ObservationAuthFailureLooksCaptchaBoundary(observation, raw, status)
}

func stageGPostAuthSessionLossEvidence(observations []browserpkg.Observation) string {
	establishedAt := -1
	establishedBy := ""
	authenticatedStateSeen := false
	for index, observation := range observations {
		if establishedAt >= 0 {
			if stageGObservationHasSessionEndingError(observation) {
				return fmt.Sprintf("After %s at observation %d, action %s attempted a session-ending browser operation: %s.", establishedBy, establishedAt+1, strings.TrimSpace(observation.Action), strings.TrimSpace(observation.Error))
			}
			for _, event := range observation.NetworkEvents {
				if stageGNetworkURLLooksLogout(event.URL) && (event.Status >= 200 && event.Status < 400 || strings.TrimSpace(event.Error) != "") {
					return fmt.Sprintf("After %s at observation %d, action %s triggered session-ending request %s.", establishedBy, establishedAt+1, strings.TrimSpace(observation.Action), stageGNetworkEventText(event))
				}
			}
			for _, issue := range observation.NetworkIssues {
				if stageGNetworkURLLooksLogout(issue.URL) && (issue.Status >= 200 || strings.TrimSpace(issue.Error) != "") {
					return fmt.Sprintf("After %s at observation %d, action %s hit session-ending request %s.", establishedBy, establishedAt+1, strings.TrimSpace(observation.Action), stageGNetworkIssueText(issue))
				}
			}
			if authenticatedStateSeen && stageGObservationLooksAuthGate(observation) {
				target := strings.TrimSpace(observation.CurrentURL)
				if target == "" {
					target = "login/register UI"
				}
				return fmt.Sprintf("After %s at observation %d, browser returned to %s during action %s.", establishedBy, establishedAt+1, target, strings.TrimSpace(observation.Action))
			}
			if stageGObservationLooksAuthenticated(observation) && !stageGObservationLooksAuthGate(observation) {
				authenticatedStateSeen = true
				establishedBy = "authenticated browser state"
			}
			continue
		}
		if stageGObservationLooksAuthenticated(observation) && !stageGObservationLooksAuthGate(observation) {
			establishedAt = index
			establishedBy = "authenticated browser state"
			authenticatedStateSeen = true
			continue
		}
		for _, event := range observation.NetworkEvents {
			if strings.ToUpper(strings.TrimSpace(event.Method)) == "POST" &&
				event.Status >= 200 && event.Status < 400 &&
				stageGNetworkURLLooksAuth(event.URL) &&
				!stageGNetworkURLLooksLogout(event.URL) {
				establishedAt = index
				establishedBy = "successful credentialed authentication"
				break
			}
		}
	}
	return ""
}

func stageGObservationHasSessionEndingError(observation browserpkg.Observation) bool {
	errorText := strings.ToLower(strings.TrimSpace(observation.Error))
	return strings.Contains(errorText, "session-ending browser target blocked") ||
		strings.Contains(errorText, "session-ending browser request blocked")
}

func stageGAuthGateStallEvidence(observations []browserpkg.Observation) string {
	if stageGObservedAuthRecovery(observations) || len(observations) == 0 {
		return ""
	}
	last := observations[len(observations)-1]
	if !stageGObservationLooksAuthGate(last) {
		return ""
	}
	submits := 0
	authGateObservations := 0
	credentialEvidence := false
	for _, observation := range observations {
		if !stageGObservationLooksAuthGate(observation) {
			continue
		}
		authGateObservations++
		if stageGObservationHasCredentialEvidence(observation) {
			credentialEvidence = true
		}
		if stageGObservationLooksInputAttempt(observation) {
			credentialEvidence = true
		}
		if stageGObservationLooksCompletedAuthSubmitAttempt(observation) {
			submits++
		}
	}
	if submits < stageGAuthGateSubmitStallLimit || !credentialEvidence {
		return ""
	}
	target := strings.TrimSpace(last.CurrentURL)
	if target == "" {
		target = "the login/register UI"
	}
	return fmt.Sprintf("Browser remained on %s after %d credentialed submit attempt(s) across %d auth-gate observation(s), with no successful auth transition observed.", target, submits, authGateObservations)
}

func stageGRepeatedStateStallEvidence(observations []browserpkg.Observation) string {
	if len(observations) < stageGRepeatedStateStallLimit {
		return ""
	}
	last := observations[len(observations)-1]
	lastKey := stageGObservationStateKey(last)
	if lastKey == "" {
		return ""
	}
	if stageGObservationLooksAuthGate(last) {
		return ""
	}
	sameState := 0
	activeActions := 0
	for index := len(observations) - 1; index >= 0; index-- {
		if stageGObservationStateKey(observations[index]) != lastKey {
			break
		}
		sameState++
		if stageGObservationCountsAsProgressAttempt(observations[index]) {
			activeActions++
		}
	}
	if sameState < stageGRepeatedStateStallLimit || activeActions < 2 {
		return ""
	}
	target := strings.TrimSpace(last.CurrentURL)
	if target == "" {
		target = strings.TrimSpace(last.Title)
	}
	if target == "" {
		target = "the same browser state"
	}
	return fmt.Sprintf("Browser stayed at %s with unchanged visible state for %d consecutive observation(s) after %d progress attempt(s).", target, sameState, activeActions)
}

func stageGObservedAuthSuccess(observations []browserpkg.Observation) bool {
	return stageGCredentialedAuthSuccessIndex(observations) >= 0
}

func stageGObservedAuthRecovery(observations []browserpkg.Observation) bool {
	if stageGObservedAuthNetworkSuccess(observations) {
		return true
	}
	return stageGObservedCredentialedAuthTransition(observations)
}

func stageGObservedAuthNetworkSuccess(observations []browserpkg.Observation) bool {
	for _, observation := range observations {
		for _, event := range observation.NetworkEvents {
			if strings.ToUpper(strings.TrimSpace(event.Method)) == "POST" &&
				event.Status >= 200 && event.Status < 400 &&
				stageGNetworkURLLooksAuth(event.URL) &&
				!stageGNetworkURLLooksLogout(event.URL) {
				return true
			}
		}
	}
	return false
}

func stageGCredentialedAuthSuccessIndex(observations []browserpkg.Observation) int {
	if index, _, ok := stageGCredentialedAuthNetworkSuccess(observations); ok {
		return index
	}
	return stageGCredentialedAuthTransitionIndex(observations)
}

func stageGObservedCredentialedAuthTransition(observations []browserpkg.Observation) bool {
	return stageGCredentialedAuthTransitionIndex(observations) >= 0
}

func stageGCredentialedAuthTransitionIndex(observations []browserpkg.Observation) int {
	credentialed := false
	submitted := false
	for index, observation := range observations {
		if credentialed && stageGObservationLooksSubmitAttempt(observation) {
			submitted = true
		}
		if stageGObservationLooksAuthGate(observation) {
			if stageGObservationHasCredentialEvidence(observation) {
				credentialed = true
			}
			continue
		}
		if submitted && stageGObservationLooksAuthenticated(observation) {
			return index
		}
	}
	return -1
}

func stageGAuthAcceptedNoTransitionEvidence(observations []browserpkg.Observation) string {
	successIndex, event, ok := stageGCredentialedAuthNetworkSuccess(observations)
	if !ok {
		return ""
	}
	if successIndex >= len(observations)-1 {
		return ""
	}
	for index := successIndex + 1; index < len(observations); index++ {
		if stageGObservationLooksAuthenticated(observations[index]) && !stageGObservationLooksAuthGate(observations[index]) {
			return ""
		}
	}
	last := observations[len(observations)-1]
	if !stageGObservationLooksAuthGate(last) {
		return ""
	}
	target := strings.TrimSpace(last.CurrentURL)
	if target == "" {
		target = "the login/register UI"
	}
	return fmt.Sprintf("Credentialed authentication request succeeded (%s status=%d), but browser remained on %s for %d later observation(s) with no authenticated product UI.", event.URL, event.Status, target, len(observations)-successIndex-1)
}

func stageGCredentialedAuthNetworkSuccess(observations []browserpkg.Observation) (int, browserpkg.NetworkEvent, bool) {
	credentialed := false
	submitted := false
	for index, observation := range observations {
		if stageGObservationLooksAuthGate(observation) && stageGObservationHasCredentialEvidence(observation) {
			credentialed = true
		}
		if credentialed && stageGObservationLooksSubmitAttempt(observation) {
			submitted = true
		}
		authenticatedTransition := stageGObservationLooksAuthenticated(observation) && !stageGObservationLooksAuthGate(observation)
		for _, event := range observation.NetworkEvents {
			if (submitted || authenticatedTransition) &&
				strings.ToUpper(strings.TrimSpace(event.Method)) == "POST" &&
				event.Status >= 200 && event.Status < 400 &&
				stageGNetworkURLLooksAuth(event.URL) &&
				!stageGNetworkURLLooksLogout(event.URL) {
				return index, event, true
			}
		}
	}
	return -1, browserpkg.NetworkEvent{}, false
}

func stageGObservationLooksAuthGate(observation browserpkg.Observation) bool {
	return frontende2e.ObservationLooksAuthGate(observation)
}

func stageGObservationLooksAuthenticated(observation browserpkg.Observation) bool {
	return frontende2e.ObservationLooksAuthenticated(observation)
}

func stageGObservationHasCredentialEvidence(observation browserpkg.Observation) bool {
	return frontende2e.ObservationHasCredentialEvidence(observation)
}

func stageGObservationHasPasswordControl(observation browserpkg.Observation) bool {
	return frontende2e.ObservationHasPasswordControl(observation)
}

func stageGObservationLooksInputAttempt(observation browserpkg.Observation) bool {
	return frontende2e.ObservationLooksInputAttempt(observation)
}

func stageGObservationLooksSubmitAttempt(observation browserpkg.Observation) bool {
	return frontende2e.ObservationLooksSubmitAttempt(observation)
}

func stageGObservationLooksCompletedAuthSubmitAttempt(observation browserpkg.Observation) bool {
	if !stageGObservationLooksSubmitAttempt(observation) {
		return false
	}
	if observation.OK {
		return true
	}
	_, _, ok := stageGObservationAuthClientFailure(observation)
	return ok
}

func stageGObservationCountsAsProgressAttempt(observation browserpkg.Observation) bool {
	switch strings.TrimSpace(observation.Action) {
	case "open_candidate", "click_button", "submit_local_form", "click_navigation", "go_back":
		return true
	default:
		return false
	}
}

func stageGPartialFailureEvidence(index int, observations []browserpkg.Observation, reason string) string {
	var parts []string
	if index < 0 || index >= len(observations) {
		return strings.TrimSpace(reason)
	}
	observation := observations[index]
	if observation.CurrentURL != "" {
		parts = append(parts, "URL: "+observation.CurrentURL)
	}
	for _, issue := range observation.NetworkIssues {
		if stageGNetworkIssueBlocksEvidence(index, issue, observations) && issue.Status >= 400 {
			parts = append(parts, fmt.Sprintf("%s status=%d", issue.URL, issue.Status))
		}
	}
	for _, event := range observation.NetworkEvents {
		if stageGNetworkEventBlocksEvidence(index, event, observations) && event.Status >= 400 {
			parts = append(parts, fmt.Sprintf("%s status=%d", event.URL, event.Status))
		}
	}
	if strings.TrimSpace(observation.Error) != "" {
		parts = append(parts, strings.TrimSpace(observation.Error))
	}
	if strings.TrimSpace(reason) != "" {
		parts = append(parts, strings.TrimSpace(reason))
	}
	if len(parts) == 0 {
		return fmt.Sprintf("Observation %d showed a product failure.", index+1)
	}
	return strings.Join(parts, " ")
}

func (r Runner) writeStageGArtifacts(record model.StageRecord, writer ArtifactWriter, summary FrontendE2ESummary, observations []browserpkg.Observation) model.StageRecord {
	if observations == nil {
		observations = []browserpkg.Observation{}
	}
	record, summary, observations = materializeStageGScreenshotArtifacts(record, writer, summary, observations)
	record = requiredStageJSON(record, writer, "frontend_e2e_summary.json", summary)
	record = requiredStageText(record, writer, "frontend_e2e_report.md", frontendE2EReport(summary, observations))
	record = requiredStageJSON(record, writer, "frontend_e2e_observations.json", observations)
	return record
}

func materializeStageGScreenshotArtifacts(record model.StageRecord, writer ArtifactWriter, summary FrontendE2ESummary, observations []browserpkg.Observation) (model.StageRecord, FrontendE2ESummary, []browserpkg.Observation) {
	selected := stageGKeyScreenshotObservationIndexes(observations)
	selected = stageGEnsureFindingEvidenceScreenshot(summary, observations, selected)
	selected = stageGEnsureMinimumSupportScreenshots(summary, observations, selected)
	selectedSet := map[int]bool{}
	for _, index := range selected {
		selectedSet[index] = true
	}
	var screenshots []string
	for selectedIndex, observationIndex := range selected {
		source := strings.TrimSpace(observations[observationIndex].ScreenshotPath)
		dest := stageGFinalScreenshotPath(writer, selectedIndex, len(selected), observations[observationIndex])
		if err := copyStageGScreenshot(source, dest); err != nil {
			record = recordArtifactWriteError(record, err, dest)
			observations[observationIndex].ScreenshotPath = ""
			continue
		}
		observations[observationIndex].ScreenshotPath = dest
		screenshots = append(screenshots, dest)
		record = ensureArtifactPath(record, dest)
	}
	for index := range observations {
		if !selectedSet[index] {
			observations[index].ScreenshotPath = ""
		}
	}
	if len(screenshots) == 0 {
		evidencePath := writer.Path(stageGEvidenceSummaryName)
		if err := os.MkdirAll(filepath.Dir(evidencePath), 0o755); err != nil {
			record = recordArtifactWriteError(record, err, evidencePath)
		} else if err := os.WriteFile(evidencePath, []byte(stageGScreenshotFallbackText(summary, observations)), 0o644); err != nil {
			record = recordArtifactWriteError(record, err, evidencePath)
		} else {
			record = ensureArtifactPath(record, evidencePath)
		}
	}
	summary.Screenshots = screenshots
	return record, summary, observations
}

func stageGEnsureFindingEvidenceScreenshot(summary FrontendE2ESummary, observations []browserpkg.Observation, selected []int) []int {
	if !stageGSummaryNeedsFailureEvidenceScreenshot(summary) {
		return selected
	}
	selectedSet := map[int]bool{}
	for _, index := range selected {
		selectedSet[index] = true
	}
	for index := len(observations) - 1; index >= 0; index-- {
		if selectedSet[index] || !stageGScreenshotObservationCanSupportFinding(index, observations) {
			continue
		}
		return stageGAppendRequiredScreenshotIndex(selected, index)
	}
	return selected
}

func stageGEnsureMinimumSupportScreenshots(summary FrontendE2ESummary, observations []browserpkg.Observation, selected []int) []int {
	minimum := stageGMinimumSupportScreenshotCount(summary)
	if minimum == 0 || len(selected) >= minimum {
		return selected
	}
	selectedSet := map[int]bool{}
	for _, index := range selected {
		selectedSet[index] = true
	}
	for index := range observations {
		if len(selected) >= minimum {
			break
		}
		if selectedSet[index] || !stageGScreenshotObservationCanSupportSummary(index, observations) {
			continue
		}
		selected = append(selected, index)
		selectedSet[index] = true
	}
	sort.Ints(selected)
	return selected
}

func stageGMinimumSupportScreenshotCount(summary FrontendE2ESummary) int {
	if stageGSummaryNeedsFailureEvidenceScreenshot(summary) {
		return stageGMinBrowserScreenshots
	}
	if strings.TrimSpace(summary.Status) == "passed" {
		return 2
	}
	return 0
}

func stageGSummaryNeedsFailureEvidenceScreenshot(summary FrontendE2ESummary) bool {
	status := strings.TrimSpace(summary.Status)
	return status == "failed" || status == "partial" || len(summary.Findings) > 0
}

func stageGScreenshotObservationCanSupportFinding(index int, observations []browserpkg.Observation) bool {
	return stageGScreenshotObservationCanSupportSummary(index, observations)
}

func stageGScreenshotObservationCanSupportSummary(index int, observations []browserpkg.Observation) bool {
	if index < 0 || index >= len(observations) {
		return false
	}
	observation := observations[index]
	if path := strings.TrimSpace(observation.ScreenshotPath); path == "" || !stageGScreenshotFileUsable(path) {
		return false
	}
	if strings.TrimSpace(observation.Action) == "fill_input" {
		return false
	}
	if !observation.OK && !stageGObservationHasProductFailureEvidenceAt(index, observations) {
		return false
	}
	if stageGObservationOnlyRecoveredAuthFailure(index, observations) {
		return false
	}
	return strings.TrimSpace(observation.VisibleText) != "" || strings.TrimSpace(observation.CurrentURL) != ""
}

func stageGAppendRequiredScreenshotIndex(selected []int, required int) []int {
	for _, index := range selected {
		if index == required {
			return selected
		}
	}
	selected = append(selected, required)
	sort.Ints(selected)
	if len(selected) <= stageGMaxBrowserScreenshots {
		return selected
	}
	for index := len(selected) - 1; index >= 0; index-- {
		if selected[index] == required || index == 0 {
			continue
		}
		return append(selected[:index], selected[index+1:]...)
	}
	return selected[:stageGMaxBrowserScreenshots]
}

func stageGFinalScreenshotPath(writer ArtifactWriter, selectedIndex, selectedCount int, observation browserpkg.Observation) string {
	if selectedIndex == selectedCount-1 {
		return writer.Path(stageGLegacyScreenshotName)
	}
	name := fmt.Sprintf("%02d_%s.png", selectedIndex+1, stageGScreenshotSafeName(observation.Action))
	return writer.Path(filepath.Join(stageGKeyScreenshotDirName, name))
}

func copyStageGScreenshot(source, dest string) error {
	source = strings.TrimSpace(source)
	if source == "" {
		return fmt.Errorf("source screenshot path is empty")
	}
	if !stageGScreenshotFileUsable(source) {
		return fmt.Errorf("source screenshot is not a valid PNG: %s", source)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	content, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return os.WriteFile(dest, content, 0o644)
}

func stageGScreenshotFileUsable(path string) bool {
	content, err := os.ReadFile(strings.TrimSpace(path))
	if err != nil || len(content) < 8 {
		return false
	}
	return content[0] == 0x89 &&
		content[1] == 'P' &&
		content[2] == 'N' &&
		content[3] == 'G' &&
		content[4] == '\r' &&
		content[5] == '\n' &&
		content[6] == 0x1a &&
		content[7] == '\n'
}

func stageGScreenshotFallbackText(summary FrontendE2ESummary, observations []browserpkg.Observation) string {
	var builder strings.Builder
	builder.WriteString("Stage G browser frontend E2E\n")
	builder.WriteString("status: " + summary.Status + "\n")
	if summary.Reason != "" {
		builder.WriteString("reason: " + summary.Reason + "\n")
	}
	if len(summary.Findings) > 0 {
		builder.WriteString("findings:\n")
		for _, finding := range summary.Findings {
			builder.WriteString("- " + finding.Severity + ": " + finding.Title + "\n")
		}
	}
	if len(observations) > 0 {
		builder.WriteString("observations:\n")
		for _, observation := range observations {
			builder.WriteString(fmt.Sprintf("- %s ok=%t url=%s\n", observation.Action, observation.OK, observation.CurrentURL))
		}
	}
	return builder.String()
}

func ensureArtifactPath(record model.StageRecord, path string) model.StageRecord {
	path = strings.TrimSpace(path)
	if path == "" {
		return record
	}
	for _, existing := range record.ArtifactPaths {
		if existing == path {
			return record
		}
	}
	record.ArtifactPaths = append(record.ArtifactPaths, path)
	return record
}

func stageGSummary(status, reason string, candidates []BrowserURLCandidate, observations []browserpkg.Observation, blocked []BlockedBrowserAction) FrontendE2ESummary {
	var visited []string
	var screenshots []string
	for _, observation := range observations {
		if observation.CurrentURL != "" {
			visited = append(visited, observation.CurrentURL)
		}
		if observation.ScreenshotPath != "" {
			screenshots = append(screenshots, observation.ScreenshotPath)
		}
	}
	return FrontendE2ESummary{
		SchemaVersion:  frontendE2ESchemaVersion,
		Status:         status,
		Reason:         reason,
		URLCandidates:  candidates,
		VisitedURLs:    visited,
		Screenshots:    screenshots,
		BlockedActions: blocked,
		Findings:       []FrontendE2EFinding{},
	}
}

func frontendE2EObservationFindings(observations []browserpkg.Observation, screenshot string, includeActionFailures bool) []model.Finding {
	var findings []model.Finding
	blankRecorded := false
	for index, observation := range observations {
		if !observation.OK && includeActionFailures {
			evidence := strings.TrimSpace(observation.Error)
			if evidence == "" {
				evidence = "browser action did not complete successfully"
			}
			findings = append(findings, model.Finding{
				Stage:      string(model.StageG),
				Severity:   "High",
				Title:      "Browser action failed during frontend E2E",
				Rule:       "Every validated browser action must either complete or produce a failing Stage G finding.",
				Evidence:   evidence,
				Impact:     "The browser exploration could not verify the intended user workflow.",
				MinimumFix: "Inspect frontend_e2e_observations.json and fix the page or action target.",
				SourcePath: screenshot,
			})
		}
		if observation.CurrentURL != "" && len(strings.TrimSpace(observation.VisibleText)) < 10 && !blankRecorded {
			blankRecorded = true
			findings = append(findings, model.Finding{
				Stage:      string(model.StageG),
				Severity:   "High",
				Title:      "Frontend page appears blank",
				Rule:       "A browser-visible frontend should render meaningful visible content.",
				Evidence:   "URL: " + observation.CurrentURL,
				Impact:     "Users may see a blank or non-rendered application shell.",
				MinimumFix: "Fix frontend boot/render errors and rerun Stage G.",
				SourcePath: screenshot,
			})
		}
		if len(observation.PageErrors) > 0 {
			findings = append(findings, model.Finding{
				Stage:      string(model.StageG),
				Severity:   "High",
				Title:      "Frontend runtime page error",
				Rule:       "Browser pages must not throw uncaught runtime errors during E2E exploration.",
				Evidence:   strings.Join(observation.PageErrors, "\n"),
				Impact:     "Core frontend workflows may be broken for users.",
				MinimumFix: "Fix the reported runtime exception and rerun Stage G.",
				SourcePath: screenshot,
			})
		}
		if len(observation.ConsoleErrors) > 0 &&
			!stageGConsoleErrorsOnlyRecoveredAuthNoise(index, observations) &&
			!stageGConsoleErrorsOnlyAuthGateNoise(index, observations) {
			findings = append(findings, model.Finding{
				Stage:      string(model.StageG),
				Severity:   "Medium",
				Title:      "Frontend console errors were observed",
				Rule:       "Browser E2E should not produce console errors during normal page load or interaction.",
				Evidence:   strings.Join(observation.ConsoleErrors, "\n"),
				Impact:     "Frontend behavior may be degraded or brittle.",
				MinimumFix: "Fix the console error source and rerun Stage G.",
				SourcePath: screenshot,
			})
		}
		for _, issue := range observation.NetworkIssues {
			if !stageGNetworkIssueBlocksEvidence(index, issue, observations) {
				continue
			}
			if issue.Status >= 500 {
				findings = append(findings, model.Finding{
					Stage:      string(model.StageG),
					Severity:   "High",
					Title:      "Frontend request returned server error",
					Rule:       "Browser E2E should not observe 5xx responses for app resources or APIs.",
					Evidence:   fmt.Sprintf("%s status=%d", issue.URL, issue.Status),
					Impact:     "A user-visible workflow or required resource may fail.",
					MinimumFix: "Fix the failing endpoint or resource and rerun Stage G.",
					SourcePath: screenshot,
				})
			} else if issue.Status >= 400 {
				findings = append(findings, model.Finding{
					Stage:      string(model.StageG),
					Severity:   "Medium",
					Title:      "Frontend request returned client error",
					Rule:       "Browser E2E should not observe unexpected 4xx responses for app resources or APIs.",
					Evidence:   fmt.Sprintf("%s status=%d", issue.URL, issue.Status),
					Impact:     "A page route, asset, or API call may be misconfigured.",
					MinimumFix: "Fix the failing request and rerun Stage G.",
					SourcePath: screenshot,
				})
			}
		}
	}
	return findings
}

func includeStageGActionFailureFallback(summary FrontendE2ESummary, summaryFindings []model.Finding) bool {
	if len(summaryFindings) > 0 {
		return false
	}
	switch strings.TrimSpace(summary.Status) {
	case "passed", "not_applicable":
		return false
	default:
		return true
	}
}

func frontendE2EFindingsFromModel(findings []model.Finding) []FrontendE2EFinding {
	result := make([]FrontendE2EFinding, 0, len(findings))
	for _, finding := range findings {
		result = append(result, frontendE2EFindingFromModel(finding))
	}
	return result
}

func frontendE2EFindingFromModel(finding model.Finding) FrontendE2EFinding {
	return FrontendE2EFinding{
		Severity:   strings.TrimSpace(finding.Severity),
		Title:      strings.TrimSpace(finding.Title),
		Rule:       strings.TrimSpace(finding.Rule),
		Evidence:   strings.TrimSpace(finding.Evidence),
		Impact:     strings.TrimSpace(finding.Impact),
		MinimumFix: strings.TrimSpace(finding.MinimumFix),
		Screenshot: strings.TrimSpace(finding.SourcePath),
	}
}

func frontendE2EReport(summary FrontendE2ESummary, observations []browserpkg.Observation) string {
	var builder strings.Builder
	builder.WriteString("# Browser Frontend E2E\n\n")
	builder.WriteString("Status: " + summary.Status + "\n")
	if summary.Reason != "" {
		builder.WriteString("Reason: " + summary.Reason + "\n")
	}
	builder.WriteString("\n## URL Candidates\n")
	for _, candidate := range summary.URLCandidates {
		builder.WriteString(fmt.Sprintf("- %s %s service=%s probe_ok=%t\n", candidate.ID, candidate.URL, candidate.Service, candidate.ProbeOK))
	}
	builder.WriteString("\n## Findings\n")
	if len(summary.Findings) == 0 {
		builder.WriteString("- none\n")
	} else {
		for _, finding := range summary.Findings {
			builder.WriteString(fmt.Sprintf("- %s: %s\n", finding.Severity, finding.Title))
		}
	}
	if len(summary.BlockedActions) > 0 {
		builder.WriteString("\n## Blocked Actions\n")
		for _, action := range summary.BlockedActions {
			builder.WriteString(fmt.Sprintf("- %s: %s\n", action.Action, action.Reason))
		}
	}
	if len(summary.Screenshots) > 0 {
		builder.WriteString("\n## Screenshots\n")
		for index, screenshot := range summary.Screenshots {
			builder.WriteString(fmt.Sprintf("- %02d %s\n", index+1, screenshot))
		}
	}
	if len(observations) > 0 {
		builder.WriteString("\n## Observations\n")
		for _, observation := range observations {
			builder.WriteString(fmt.Sprintf("- %s ok=%t url=%s title=%s\n", observation.Action, observation.OK, observation.CurrentURL, observation.Title))
			if observation.ScreenshotPath != "" {
				builder.WriteString(fmt.Sprintf("  screenshot: %s\n", observation.ScreenshotPath))
			}
			for _, err := range observation.PageErrors {
				builder.WriteString(fmt.Sprintf("  page_error: %s\n", err))
			}
			for _, err := range observation.ConsoleErrors {
				builder.WriteString(fmt.Sprintf("  console_error: %s\n", err))
			}
			for _, issue := range observation.NetworkIssues {
				if issue.Status > 0 {
					builder.WriteString(fmt.Sprintf("  network_issue: %s status=%d\n", issue.URL, issue.Status))
				} else {
					builder.WriteString(fmt.Sprintf("  network_issue: %s %s\n", issue.URL, issue.Error))
				}
			}
			for _, event := range observation.NetworkEvents {
				builder.WriteString("  network_event: " + stageGNetworkEventText(event) + "\n")
			}
			for _, blocked := range observation.BlockedRequests {
				builder.WriteString(fmt.Sprintf("  blocked_request: %s\n", blocked.URL))
			}
		}
	}
	return builder.String()
}

func stageGBrowserContext(projectPath string) string {
	return frontende2e.BrowserContext(projectPath)
}

func projectTypeFromMetadata(projectPath string) string {
	content, err := os.ReadFile(filepath.Join(projectPath, "metadata.json"))
	if err != nil {
		return ""
	}
	var data map[string]any
	if json.Unmarshal(content, &data) != nil {
		return ""
	}
	return config.NormalizeProjectType(fmt.Sprint(data["project_type"]))
}
