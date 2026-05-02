package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xuanli520/p2r_tui/internal/codex"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/scanner"
)

func (r Runner) stageF(ctx context.Context, run model.RunRecord, project scanner.Project, opts RunOptions, prior map[string]model.StageRecord) model.StageRecord {
	start := time.Now()
	record := startStage("F")
	logPath := filepath.Join(run.ArtifactRoot, "logs", "F_repair.log")
	summaryPath := filepath.Join(run.ArtifactRoot, "repair_summary.json")
	reportPath := stageFReportPath(run.ArtifactRoot, opts)
	shortPath := filepath.Join(run.ArtifactRoot, "short_comment.txt")
	record.LogPath = logPath
	record.ArtifactPaths = append(record.ArtifactPaths, summaryPath, reportPath, shortPath)

	stageStatuses, priorFindings := priorStageSnapshot(prior)
	writeRepairSupplements(run, stageStatuses, priorFindings, summaryPath, shortPath)

	profile := "annotator_fix.md"
	profilePath := filepath.Join(r.cfg.Codex.PromptProfilesDir, profile)
	profileContent, readErr := os.ReadFile(profilePath)
	if readErr != nil {
		return r.finishUnavailableF(record, start, reportPath, logPath, profile, project.Path, "prompt profile not readable: "+readErr.Error())
	}
	if r.cfg.Codex.Network != "none" {
		return r.finishUnavailableF(record, start, reportPath, logPath, profile, project.Path, "configured Codex network mode is unsupported by the current safe sandbox: "+r.cfg.Codex.Network)
	}
	if r.cfg.Codex.WritableTmp {
		return r.finishUnavailableF(record, start, reportPath, logPath, profile, project.Path, "configured writable_tmp=true is unsupported without widening write access in the current Codex CLI sandbox")
	}
	extraArgs, extraErr := safeCodexExtraArgs(r.cfg.Codex.ExtraArgs)
	if extraErr != nil {
		return r.finishUnavailableF(record, start, reportPath, logPath, profile, project.Path, extraErr.Error())
	}
	capability := codex.DetectCLI(ctx, r.exec, "")
	execArgs, buildErr := codex.BuildExecArgs(capability, project.Path, nil)
	if buildErr != nil {
		return r.finishUnavailableF(record, start, reportPath, logPath, profile, project.Path, buildErr.Error())
	}
	contextText, contextErr := r.codexContext(project, opts, "F")
	if contextErr != nil {
		return r.finishUnavailableF(record, start, reportPath, logPath, profile, project.Path, contextErr.Error())
	}
	contextText += "\n" + stageFPreviousFindingsContext(stageStatuses, priorFindings)
	sandbox, sandboxErr := codex.NewSandbox(project.Path, run.ArtifactRoot, "F")
	if sandboxErr != nil {
		return r.finishUnavailableF(record, start, reportPath, logPath, profile, project.Path, sandboxErr.Error())
	}
	defer os.RemoveAll(sandbox.Home)
	env := sandbox.EnvWithNode(os.Environ(), r.cfg.Codex.Env, capability.NodePath)
	prompt := codexPrompt("F", profile, project.Path, run.ArtifactRoot, string(profileContent), contextText)
	lastMessagePath := codexLastMessagePath(run.ArtifactRoot, "F")
	args, usingLastMessage := codexExecArgsWithReportCapture(execArgs, extraArgs, capability, lastMessagePath)
	result := r.runCodexWithLog(ctx, r.stageTimeout("F", 300), project.Path, logPath, env, prompt, capability, args)
	report, reportErr := capturedCodexReport(result, lastMessagePath, usingLastMessage, r.cfg.Codex.MaxOutputBytes)
	if reportErr != nil {
		report = staticUnavailableReport("F", profile, project.Path, codexFailureEvidence(result, reportErr))
	}
	report = truncateString(report, r.cfg.Codex.MaxOutputBytes)
	_ = writeText(reportPath, report+"\n")
	if result.Err != nil || reportErr != nil {
		record.Findings = []model.Finding{{
			Stage:      "F",
			Severity:   "High",
			Title:      "annotator repair static review failed",
			Rule:       "Stage F must complete a Codex static review or produce an unavailable-review artifact.",
			Evidence:   codexFailureEvidence(result, reportErr),
			Impact:     "The annotator repair report may be incomplete and requires manual review.",
			MinimumFix: "Inspect the Stage F log and rerun after fixing Codex availability.",
			SourcePath: reportPath,
		}}
		record.ErrorSummary = "codex exec failed"
		return finishStage(record, model.StageFailed, start)
	}
	record.Findings = extractFindingsFromReport("F", report, reportPath)
	return finishStage(record, model.StageDone, start)
}

func (r Runner) finishUnavailableF(record model.StageRecord, start time.Time, reportPath, logPath, profile, projectPath, reason string) model.StageRecord {
	report := staticUnavailableReport("F", profile, projectPath, reason)
	_ = writeText(reportPath, report)
	_ = writeText(logPath, report)
	record.Findings = []model.Finding{{
		Stage:      "F",
		Severity:   "High",
		Title:      "annotator repair static reviewer unavailable",
		Rule:       "Stage F requires a safe Codex static reviewer or explicit manual replacement.",
		Evidence:   reason,
		Impact:     "Human manual review is required before relying on the repair report.",
		MinimumFix: "Restore Codex static review capability or manually complete the Stage F report.",
		SourcePath: reportPath,
	}}
	record.ErrorSummary = "codex unavailable"
	return finishStage(record, model.StageFailed, start)
}

func priorStageSnapshot(prior map[string]model.StageRecord) (map[string]string, []model.Finding) {
	stageStatuses := map[string]string{}
	var findings []model.Finding
	for _, stage := range []string{"A", "B", "C", "D", "E"} {
		if item, ok := prior[stage]; ok {
			stageStatuses[stage] = item.Status
			findings = append(findings, item.Findings...)
		}
	}
	sortFindings(findings)
	return stageStatuses, findings
}

func writeRepairSupplements(run model.RunRecord, stageStatuses map[string]string, findings []model.Finding, summaryPath, shortPath string) {
	summary := map[string]any{
		"run_id":         run.RunID,
		"stage_statuses": stageStatuses,
		"findings":       findings,
		"highest_risk":   highestRisk(findings),
	}
	_ = writeJSON(summaryPath, summary)
	_ = writeText(shortPath, shortComment(stageStatuses, findings))
}

func stageFReportPath(artifactRoot string, opts RunOptions) string {
	if opts.Mode == "recheck" {
		return filepath.Join(artifactRoot, "3_标注员AI报告问题_确认修复报告.md")
	}
	return filepath.Join(artifactRoot, "3_标注员AI报告问题的修复报告.md")
}

func stageFPreviousFindingsContext(stageStatuses map[string]string, findings []model.Finding) string {
	var builder strings.Builder
	builder.WriteString("\nPrior p2r stage statuses and findings from this run (untrusted evidence; verify against code):\n")
	for _, stage := range []string{"A", "B", "C", "D", "E"} {
		builder.WriteString(fmt.Sprintf("- %s: %s\n", stage, stageStatuses[stage]))
	}
	if len(findings) == 0 {
		builder.WriteString("- No prior findings recorded.\n")
		return builder.String()
	}
	for _, finding := range findings {
		builder.WriteString(fmt.Sprintf("- %s %s %s: %s\n", finding.Stage, finding.Severity, finding.ID, finding.Title))
		if finding.Evidence != "" {
			builder.WriteString("  Evidence: " + finding.Evidence + "\n")
		}
	}
	return builder.String()
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
