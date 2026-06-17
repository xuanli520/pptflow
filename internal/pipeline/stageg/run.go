package stageg

import (
	"context"

	browserpkg "github.com/xuanli520/p2r_tui/internal/browser"
	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/frontende2e"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"os"
	"path/filepath"
	"time"
)

func (r Runner) stageG(ctx context.Context, sc StageContext) model.StageRecord {
	start := time.Now()
	record := startStage(string(model.StageG))
	writer := sc.Writer
	logPath := stageLogPath(sc.Run.ArtifactRoot, string(model.StageG))
	summaryPath := filepath.Join(sc.Run.ArtifactRoot, "frontend_e2e_summary.json")
	reportPath := qaArtifactPath(sc.Run.ArtifactRoot, "frontend_e2e_report.md")
	screenshotPath := qaArtifactPath(sc.Run.ArtifactRoot, stageGLegacyScreenshotName)
	observationsPath := filepath.Join(sc.Run.ArtifactRoot, "frontend_e2e_observations.json")
	record.LogPath = logPath
	record.ArtifactPaths = []string{logPath, summaryPath, reportPath, observationsPath}
	timeout := sc.Timeout
	stageCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	defer cleanupStageGBrowserRuntime(sc.Run.ArtifactRoot)

	candidates := sc.Candidates
	bestEffortStageText(&record, writer, writer.RelativePath(logPath), stageGLogHeader(sc, candidates))
	if !sc.HasCleanupTarget {
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

	profilePath := filepath.Join(sc.PromptProfilesDir, stageGBrowserProfileName)
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
	promptTemplatePath := filepath.Join(sc.PromptProfilesDir, stageGBrowserActionPromptTemplateName)
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
	browserPolicy := browserpkg.Policy{
		AllowlistOrigins: browserAllowlistOrigins(candidates),
		ArtifactRoot:     sc.Run.ArtifactRoot,
	}

	explorer := frontende2e.Explorer{
		Ctx:                stageCtx,
		Candidates:         candidates,
		Policy:             browserPolicy,
		Deadline:           time.Now().Add(timeout),
		PlannerTurnTimeout: sc.PlannerTurnTimeout,
		SummaryPath:        summaryPath,
		ScreenshotPath:     writer.RelativePath(screenshotPath),
		LogPath:            logPath,
		Planner: func(ctx context.Context, candidates []BrowserURLCandidate, observations []browserpkg.Observation, blocked []BlockedBrowserAction, round int, timeout time.Duration) (string, []model.ArtifactWarning, error) {
			return r.nextStageGBrowserAction(ctx, sc, string(promptTemplate), string(profile), contextText, candidates, observations, blocked, round, timeout)
		},
		ActionRunner: func(ctx context.Context, action browserpkg.Action, policy browserpkg.Policy, timeout time.Duration) (browserpkg.Observation, error) {
			return r.runStageGBrowserAction(ctx, action, policy, timeout)
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
				if sc.Progress != nil {
					sc.Progress(round, action, ok)
				}
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
