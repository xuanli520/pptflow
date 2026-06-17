package stageg

import (
	"path/filepath"
	"time"

	"github.com/xuanli520/p2r_tui/internal/frontende2e"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
)

func stageName(stage string) string {
	return model.StageDisplayName(stage)
}

func startStage(stage string) model.StageRecord {
	return model.StageRecord{
		Stage:         stage,
		Name:          stageName(stage),
		Status:        model.StageRunning,
		StartedAt:     time.Now().UTC().Format(time.RFC3339),
		ArtifactPaths: []string{},
	}
}

func finishStage(record model.StageRecord, status string, started time.Time) model.StageRecord {
	if record.Status == model.StageFailed && status == model.StageDone {
		status = model.StageFailed
	}
	record.Status = status
	record.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	record.DurationMS = time.Since(started).Milliseconds()
	return record
}

func stageLogPath(artifactRoot, stage string) string {
	return filepath.Join(artifactRoot, "logs", model.StageLogName(stage))
}

func qaArtifactPath(root, name string) string {
	return filepath.Join(root, name)
}

func requiredStageJSON(record model.StageRecord, writer ArtifactWriter, path string, value any) model.StageRecord {
	if err := writer.RequiredJSON(path, value); err != nil {
		return recordArtifactWriteError(record, err, writer.Path(path))
	}
	return record
}

func requiredStageText(record model.StageRecord, writer ArtifactWriter, path, content string) model.StageRecord {
	if err := writer.RequiredText(path, content); err != nil {
		return recordArtifactWriteError(record, err, writer.Path(path))
	}
	return record
}

func bestEffortStageJSON(record *model.StageRecord, writer ArtifactWriter, path string, value any) {
	recordArtifactWarning(record, writer.BestEffortJSON(path, value))
}

func bestEffortStageText(record *model.StageRecord, writer ArtifactWriter, path, content string) {
	recordArtifactWarning(record, writer.BestEffortText(path, content))
}

func bestEffortStageAppend(record *model.StageRecord, writer ArtifactWriter, path, content string) {
	recordArtifactWarning(record, writer.BestEffortAppend(path, content))
}

func recordArtifactWarning(record *model.StageRecord, warning ArtifactWarning) {
	if record == nil || warning.OK() {
		return
	}
	record.ArtifactWarnings = append(record.ArtifactWarnings, warning)
}

func recordArtifactWarnings(record *model.StageRecord, writer ArtifactWriter, warnings []ArtifactWarning) {
	for _, warning := range warnings {
		if warning.Path != "" {
			warning.Path = writer.RelativePath(warning.Path)
		}
		recordArtifactWarning(record, warning)
	}
}

func recordArtifactWriteError(record model.StageRecord, err error, sourcePath string) model.StageRecord {
	if err == nil {
		return record
	}
	record.Status = model.StageFailed
	record.ErrorSummary = err.Error()
	record.Findings = append(record.Findings, model.Finding{
		Stage:      "INFRA",
		Severity:   "High",
		Title:      "Required p2r artifact could not be written",
		Rule:       "p2r stages must persist required evidence artifacts.",
		Evidence:   err.Error(),
		Impact:     "The run evidence is incomplete even if the underlying stage work completed.",
		MinimumFix: "Ensure the run artifact directory is writable and rerun the affected stage.",
		SourcePath: sourcePath,
	})
	return record
}

func frontendE2EFindings(summary FrontendE2ESummary, sourcePath string) []model.Finding {
	return frontende2e.Findings(summary, sourcePath)
}

func frontendE2ESchemaFailureFinding(sourcePath string, err error) model.Finding {
	return frontende2e.SchemaFailureFinding(sourcePath, err)
}

func browserAllowlistOrigins(candidates []BrowserURLCandidate) []string {
	return frontende2e.AllowlistOrigins(candidates)
}
