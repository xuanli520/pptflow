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

const stageFReportSplitMarker = "<!-- p2r:report-split -->"

var (
	stageFReport2HeadingBoundary = regexp.MustCompile(`(?im)^#{1,3}\s+.*Report\s+2\b.*$`)
	stageFReport2LineBoundary    = regexp.MustCompile(`(?im)^.*Report\s+2\b.*$`)
)

type stageFSplitKind string

const (
	stageFSplitMarker  stageFSplitKind = "marker"
	stageFSplitHeading stageFSplitKind = "heading"
	stageFSplitLine    stageFSplitKind = "line"
	stageFSplitNone    stageFSplitKind = "none"
)

type stageFSplitResult struct {
	report1 string
	report2 string
	kind    stageFSplitKind
}

func splitStageFCodexReport(report string) stageFSplitResult {
	if idx := strings.Index(report, stageFReportSplitMarker); idx >= 0 {
		return stageFSplitResult{
			strings.TrimSpace(report[:idx]),
			strings.TrimSpace(report[idx+len(stageFReportSplitMarker):]),
			stageFSplitMarker,
		}
	}
	if loc := stageFReport2HeadingBoundary.FindStringIndex(report); loc != nil {
		return stageFSplitResult{
			strings.TrimSpace(report[:loc[0]]),
			strings.TrimSpace(report[loc[0]:]),
			stageFSplitHeading,
		}
	}
	if loc := stageFReport2LineBoundary.FindStringIndex(report); loc != nil {
		return stageFSplitResult{
			strings.TrimSpace(report[:loc[0]]),
			strings.TrimSpace(report[loc[0]:]),
			stageFSplitLine,
		}
	}
	return stageFSplitResult{strings.TrimSpace(report), "", stageFSplitNone}
}

func validateStageFSplit(split stageFSplitResult, report string) []model.Finding {
	var findings []model.Finding
	if split.kind == stageFSplitNone || strings.TrimSpace(split.report2) == "" {
		title := "Stage F report split boundary not found"
		evidence := "No split marker, heading-level 'Report 2', or body-level 'Report 2' line was detected."
		if split.kind != stageFSplitNone && strings.TrimSpace(split.report2) == "" {
			title = "Stage F report2 is empty after split"
			evidence = "The split boundary was found (" + string(split.kind) + ") but report2 is empty or whitespace only."
		}
		findings = append(findings, model.Finding{
			Stage:      string(model.StageF),
			Severity:   "Medium",
			Title:      title,
			Rule:       "Codex output must separate Report 1 and Report 2 with a heading or the p2r:report-split marker.",
			Evidence:   evidence,
			Impact:     "Both Stage F output artifacts receive identical content. Manual split is required.",
			MinimumFix: "Rerun Stage F. If the issue persists, manually split the report into the two required artifacts.",
		})
		return findings
	}
	if split.kind == stageFSplitLine {
		findings = append(findings, model.Finding{
			Stage:      string(model.StageF),
			Severity:   "Low",
			Title:      "Stage F split used weak boundary (body text)",
			Rule:       "The preferred split boundary is the p2r:report-split marker or an H2 '## Report 2' heading.",
			Evidence:   "The split was made at a body line containing 'Report 2' rather than a heading or marker. The split content may not be at the intended boundary.",
			Impact:     "The two reports may be incorrectly separated. Human review of the split point is recommended.",
			MinimumFix: "If artifacts are incorrectly split, rerun Stage F. The prompt now requires explicit heading formatting.",
		})
	}
	if duplicate := splitContentSimilarity(split.report1, split.report2); duplicate {
		findings = append(findings, model.Finding{
			Stage:      string(model.StageF),
			Severity:   "Medium",
			Title:      "Stage F split produced overlapping content",
			Rule:       "Report 1 and Report 2 must be distinct documents covering different topics.",
			Evidence:   "The two split segments share substantial content, suggesting the split boundary was not placed correctly.",
			Impact:     "The two required QA artifacts are not properly separated.",
			MinimumFix: "Rerun Stage F. Review the Codex output to ensure proper Report 1 / Report 2 structure.",
		})
	}
	if !containsIssueStatusKeywords(split.report2) {
		findings = append(findings, model.Finding{
			Stage:      string(model.StageF),
			Severity:   "Low",
			Title:      "Stage F report2 may lack issue verification content",
			Rule:       "Report 2 must list each issue as Resolved, Partially Resolved, Unresolved, or Cannot Confirm.",
			Evidence:   "The expected issue-status keywords (Resolved, Unresolved, Cannot Confirm) are absent from report2.",
			Impact:     "The issue verification report may be incomplete or empty.",
			MinimumFix: "Review report2 manually. Rerun Stage F if it is missing required issue status entries.",
		})
	}
	return findings
}

func containsIssueStatusKeywords(report2 string) bool {
	for _, keyword := range []string{"Resolved", "Unresolved", "Cannot Confirm"} {
		if strings.Contains(report2, keyword) {
			return true
		}
	}
	return false
}

func splitContentSimilarity(a, b string) bool {
	na := len(a)
	nb := len(b)
	if na == 0 || nb == 0 {
		return false
	}
	if na < 128 && nb < 128 {
		return false
	}
	const step = 64
	return checkDirection(a, b, step) || checkDirection(b, a, step)
}

func checkDirection(sample, target string, step int) bool {
	shorter := len(sample)
	if len(target) < shorter {
		shorter = len(target)
	}
	same := 0
	blocks := 0
	for i := 0; i+step <= shorter; i += step {
		blocks++
		if strings.Contains(target, sample[i:i+step]) {
			same++
		}
	}
	if blocks == 0 {
		return false
	}
	return float64(same)/float64(blocks) > 0.8
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
