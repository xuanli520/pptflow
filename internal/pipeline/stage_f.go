package pipeline

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
)

func (r Runner) stageF(run model.RunRecord, prior map[string]model.StageRecord) model.StageRecord {
	start := time.Now()
	record := startStage("F")
	logPath := filepath.Join(run.ArtifactRoot, "logs", "F_repair.log")
	summaryPath := filepath.Join(run.ArtifactRoot, "repair_summary.json")
	reportPath := filepath.Join(run.ArtifactRoot, "3_标注员AI报告问题的修复报告.md")
	shortPath := filepath.Join(run.ArtifactRoot, "short_comment.txt")
	record.LogPath = logPath
	record.ArtifactPaths = append(record.ArtifactPaths, summaryPath, reportPath, shortPath)
	var findings []model.Finding
	stageStatuses := map[string]string{}
	for _, stage := range []string{"A", "B", "C", "D", "E"} {
		if item, ok := prior[stage]; ok {
			stageStatuses[stage] = item.Status
			findings = append(findings, item.Findings...)
		}
	}
	sortFindings(findings)
	summary := map[string]any{
		"run_id":         run.RunID,
		"stage_statuses": stageStatuses,
		"findings":       findings,
		"highest_risk":   highestRisk(findings),
	}
	_ = writeJSON(summaryPath, summary)
	report := repairMarkdown(stageStatuses, findings)
	_ = writeText(reportPath, report)
	_ = writeText(logPath, report)
	_ = writeText(shortPath, shortComment(stageStatuses, findings))
	record.Findings = findings
	return finishStage(record, model.StageDone, start)
}

func repairMarkdown(stageStatuses map[string]string, findings []model.Finding) string {
	var builder strings.Builder
	builder.WriteString("# Repair Summary\n\n")
	builder.WriteString("## Stage Statuses\n\n")
	for _, stage := range []string{"A", "B", "C", "D", "E"} {
		builder.WriteString(fmt.Sprintf("- %s: %s\n", stage, stageStatuses[stage]))
	}
	builder.WriteString("\n## Priority Findings\n\n")
	if len(findings) == 0 {
		builder.WriteString("No Blocker/High findings were recorded.\n")
		return builder.String()
	}
	for _, finding := range findings {
		builder.WriteString(fmt.Sprintf("- %s %s: %s\n", finding.Severity, finding.ID, finding.Title))
		if finding.Rule != "" {
			builder.WriteString("  Rule: " + finding.Rule + "\n")
		}
		if finding.Evidence != "" {
			builder.WriteString("  Evidence: " + finding.Evidence + "\n")
		}
		if finding.Impact != "" {
			builder.WriteString("  Impact: " + finding.Impact + "\n")
		}
		if finding.DoneCriteria != "" {
			builder.WriteString("  Done criteria: " + finding.DoneCriteria + "\n")
		}
		if finding.MinimumFix != "" {
			builder.WriteString("  Minimum fix: " + finding.MinimumFix + "\n")
		}
	}
	return builder.String()
}

func shortComment(stageStatuses map[string]string, findings []model.Finding) string {
	runtime := fmt.Sprintf("1.<Runtime conclusion: B=%s, C=%s. Runtime conclusions are based only on collected B/C artifacts or explicit missing evidence.>", stageStatuses["B"], stageStatuses["C"])
	blocker, high := countSeverity(findings, "Blocker"), countSeverity(findings, "High")
	match := fmt.Sprintf("2.<Acceptance match conclusion: Static acceptance requires human review; recorded Blocker=%d, High=%d from pipeline findings.>", blocker, high)
	risk := "3.<Highest risk: No Blocker/High finding recorded.>"
	if len(findings) > 0 {
		top := highestRisk(findings)
		detail := firstNonEmpty(top.Rule, top.Evidence, top.Impact)
		if detail == "" {
			detail = "see finding evidence"
		}
		risk = fmt.Sprintf("3.<Highest risk: %s %s - %s>", top.ID, top.Title, detail)
	}
	return runtime + "\n" + match + "\n" + risk + "\n<[ ] PASS  [ ] REWORK  [ ] FAIL>\n"
}
