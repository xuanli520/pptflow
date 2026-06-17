package stageg

import (
	"path/filepath"

	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
)

func MaterializeSkipped(run model.RunRecord, record model.StageRecord, writer ArtifactWriter) model.StageRecord {
	logPath := filepath.Join(run.ArtifactRoot, "logs", "G_frontend_e2e.log")
	summaryPath := filepath.Join(run.ArtifactRoot, "frontend_e2e_summary.json")
	reportPath := qaArtifactPath(run.ArtifactRoot, "frontend_e2e_report.md")
	reason := record.ErrorSummary
	if reason == "" {
		reason = "Stage G was not executed."
	}
	if err := writer.RequiredText("logs/G_frontend_e2e.log", reason); err != nil {
		record = recordArtifactWriteError(record, err, logPath)
	}
	summary := FrontendE2ESummary{
		SchemaVersion: frontendE2ESchemaVersion,
		Status:        "blocked",
		Reason:        reason,
		Findings:      frontendE2EFindingsFromModel(record.Findings),
	}
	if err := writer.RequiredJSON("frontend_e2e_summary.json", summary); err != nil {
		record = recordArtifactWriteError(record, err, summaryPath)
	}
	if err := writer.RequiredText("frontend_e2e_report.md", "# Browser Frontend E2E\n\n"+reason+"\n"); err != nil {
		record = recordArtifactWriteError(record, err, reportPath)
	}
	record.LogPath = logPath
	record.ArtifactPaths = []string{logPath, summaryPath, reportPath}
	return record
}
