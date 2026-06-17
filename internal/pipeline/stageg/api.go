package stageg

import (
	"time"

	browserpkg "github.com/xuanli520/p2r_tui/internal/browser"
	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/frontende2e"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
)

func PlannerTurnTimeout(cfg config.StageGConfig) time.Duration {
	if cfg.PlannerTurnTimeoutSeconds <= 0 {
		return frontende2e.DefaultPlannerTurnTimeout
	}
	return time.Duration(cfg.PlannerTurnTimeoutSeconds) * time.Second
}

func PlannerTurnTimeoutSeconds(cfg config.StageGConfig) int {
	return int(PlannerTurnTimeout(cfg) / time.Second)
}

func ValidateBrowserAction(action BrowserAction, candidates []BrowserURLCandidate, raw string) frontende2e.BrowserActionValidation {
	return frontende2e.ValidateBrowserAction(action, candidates, raw)
}

func ParseSummary(raw []byte) (FrontendE2ESummary, error) {
	return frontende2e.ParseSummary(raw)
}

func BrowserScreenshotCount(observations []browserpkg.Observation) int {
	return stageGBrowserScreenshotCount(observations)
}

func BrowserContext(projectPath string) string {
	return stageGBrowserContext(projectPath)
}

func ObservationFindings(observations []browserpkg.Observation, screenshot string, includeActionFailures bool) []model.Finding {
	return frontendE2EObservationFindings(observations, screenshot, includeActionFailures)
}

func FindingsFromModel(findings []model.Finding) []FrontendE2EFinding {
	return frontendE2EFindingsFromModel(findings)
}

func LogObservation(round int, observation browserpkg.Observation) string {
	return stageGLogObservation(round, observation)
}

func FinishedStatus(record model.StageRecord) string {
	return stageGFinishedStatus(record)
}

func FinishScreenshotBlockReason(observations []browserpkg.Observation) string {
	return stageGFinishScreenshotBlockReason(observations)
}

func FinishScreenshotBlockReasonForSummary(summary FrontendE2ESummary, observations []browserpkg.Observation) string {
	return stageGFinishScreenshotBlockReasonForSummary(summary, observations)
}

func PartialProductBlockerFinding(observations []browserpkg.Observation, reason string) (model.Finding, bool) {
	return stageGPartialProductBlockerFinding(observations, reason)
}

func NativeDialogBoundaryEvidence(observations []browserpkg.Observation) string {
	return stageGNativeDialogBoundaryEvidence(observations)
}

func PositiveEvidenceOutcome(candidates []BrowserURLCandidate, observations []browserpkg.Observation, blocked []BlockedBrowserAction, reason string) (FrontendE2ESummary, bool) {
	return stageGPositiveEvidenceOutcome(candidates, observations, blocked, reason)
}

func AppendRepoSnapshotFindings(record model.StageRecord, summary FrontendE2ESummary, repoPath string, before map[string]string) (model.StageRecord, FrontendE2ESummary) {
	return appendStageGRepoSnapshotFindings(record, summary, repoPath, before)
}

func ObservationStopReason(observations []browserpkg.Observation) string {
	return stageGObservationStopReason(observations)
}

func AuthGateStallEvidence(observations []browserpkg.Observation) string {
	return stageGAuthGateStallEvidence(observations)
}

func RepeatedStateStallEvidence(observations []browserpkg.Observation) string {
	return stageGRepeatedStateStallEvidence(observations)
}

func KeyScreenshotObservationIndexes(observations []browserpkg.Observation) []int {
	return stageGKeyScreenshotObservationIndexes(observations)
}

func MaterializeScreenshotArtifacts(record model.StageRecord, writer ArtifactWriter, summary FrontendE2ESummary, observations []browserpkg.Observation) (model.StageRecord, FrontendE2ESummary, []browserpkg.Observation) {
	return materializeStageGScreenshotArtifacts(record, writer, summary, observations)
}

func IncludeActionFailureFallback(summary FrontendE2ESummary, summaryFindings []model.Finding) bool {
	return includeStageGActionFailureFallback(summary, summaryFindings)
}

func SnapshotRepo(repoPath string) (map[string]string, error) {
	return snapshotRepo(repoPath)
}

func RepoSnapshotDiff(before, after map[string]string) []string {
	return repoSnapshotDiff(repoSnapshot(before), repoSnapshot(after))
}
