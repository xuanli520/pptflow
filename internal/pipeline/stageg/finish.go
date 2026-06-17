package stageg

import (
	"fmt"
	browserpkg "github.com/xuanli520/p2r_tui/internal/browser"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"strings"
	"time"
)

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
