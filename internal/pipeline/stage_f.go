package pipeline

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/scanner"
)

func (r Runner) stageF(ctx context.Context, run model.RunRecord, project scanner.Project, opts RunOptions, prior map[string]model.StageRecord, progress func(RunProgress)) model.StageRecord {
	sc := StageContext{
		Run:      run,
		Project:  project,
		Options:  opts,
		Prior:    prior,
		Progress: progress,
		Writer:   NewArtifactWriter(run.ArtifactRoot),
		Timeout:  r.stageTimeout,
	}
	return CodexReviewStage{runner: r, spec: stageFCodexReviewSpec()}.Execute(ctx, sc).Record
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

func writeRepairSupplements(record *model.StageRecord, writer ArtifactWriter, run model.RunRecord, stageStatuses map[string]string, findings []model.Finding, summaryPath string) {
	summary := map[string]any{
		"run_id":         run.RunID,
		"stage_statuses": stageStatuses,
		"findings":       findings,
		"highest_risk":   highestRisk(findings),
	}
	bestEffortStageJSON(record, writer, writer.RelativePath(summaryPath), summary)
}

func stageFReportPath(artifactRoot string, opts RunOptions) string {
	if opts.Mode == "recheck" {
		return qaArtifactPath(artifactRoot, "prompt_requirements_verification.md")
	}
	return qaArtifactPath(artifactRoot, "operator_prompt_requirements_verification.md")
}

func stageFIssueReportPath(artifactRoot string, opts RunOptions) string {
	if opts.Mode == "recheck" {
		return qaArtifactPath(artifactRoot, "codex_report_issues_verification.md")
	}
	return qaArtifactPath(artifactRoot, "operator_codex_report_issues_verification.md")
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

var stageFReport2Boundary = regexp.MustCompile(`(?im)^.*Report\s+2\b.*$`)

func splitStageFCodexReport(report string) (string, string) {
	loc := stageFReport2Boundary.FindStringIndex(report)
	if loc == nil {
		return report, ""
	}
	return strings.TrimSpace(report[:loc[0]]), strings.TrimSpace(report[loc[0]:])
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
