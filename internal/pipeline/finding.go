package pipeline

import (
	"fmt"
	"sort"
	"strings"

	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
)

func assignFindingIDs(stage string, findings []model.Finding) []model.Finding {
	counts := map[string]int{}
	for i := range findings {
		findings[i].Stage = stage
		short := severityShort(findings[i].Severity)
		counts[short]++
		findings[i].ID = fmt.Sprintf("P2R-%s-%s-%03d", stage, short, counts[short])
	}
	return findings
}

func assignMissingFindingIDs(stage string, findings []model.Finding) []model.Finding {
	counts := map[string]int{}
	for _, finding := range findings {
		if finding.Stage == stage && finding.ID != "" {
			short := severityShort(finding.Severity)
			counts[short]++
		}
	}
	for i := range findings {
		if findings[i].Stage == "" {
			findings[i].Stage = stage
		}
		if findings[i].ID != "" {
			continue
		}
		short := severityShort(findings[i].Severity)
		counts[short]++
		findings[i].ID = fmt.Sprintf("P2R-%s-%s-%03d", findings[i].Stage, short, counts[short])
	}
	return findings
}

func severityShort(severity string) string {
	switch strings.ToLower(severity) {
	case "blocker":
		return "BLK"
	case "high":
		return "HIGH"
	case "medium":
		return "MED"
	default:
		return "LOW"
	}
}

func extractFindingsFromReport(stage, report, sourcePath string) []model.Finding {
	var findings []model.Finding
	seen := map[string]bool{}
	for index, line := range strings.Split(report, "\n") {
		title := strings.TrimSpace(strings.TrimLeft(line, "#-*0123456789.> \t"))
		if title == "" {
			continue
		}
		lower := strings.ToLower(title)
		if strings.Contains(lower, "no blocker") || strings.Contains(lower, "no high") || strings.Contains(lower, "no issue") {
			continue
		}
		severity := ""
		switch {
		case strings.Contains(lower, "blocker") || strings.Contains(lower, "critical"):
			severity = "Blocker"
		case strings.Contains(lower, "high"):
			severity = "High"
		case strings.Contains(lower, "medium"):
			severity = "Medium"
		case strings.Contains(lower, "low"):
			severity = "Low"
		}
		if severity == "" {
			continue
		}
		key := severity + title
		if seen[key] {
			continue
		}
		seen[key] = true
		findings = append(findings, model.Finding{
			Stage:      stage,
			Severity:   severity,
			Title:      title,
			Rule:       "Static review report finding",
			Evidence:   fmt.Sprintf("%s:%d", sourcePath, index+1),
			Impact:     "See the static review report for full context.",
			MinimumFix: "Review the cited report item and repair the delivery package.",
			SourcePath: sourcePath,
		})
	}
	return findings
}

func countSeverity(findings []model.Finding, severity string) int {
	count := 0
	for _, finding := range findings {
		if finding.Severity == severity {
			count++
		}
	}
	return count
}

func highestRisk(findings []model.Finding) model.Finding {
	if len(findings) == 0 {
		return model.Finding{}
	}
	sortFindings(findings)
	return findings[0]
}

func sortFindings(findings []model.Finding) {
	sort.SliceStable(findings, func(i, j int) bool {
		ri := severityRank(findings[i].Severity)
		rj := severityRank(findings[j].Severity)
		if ri != rj {
			return ri < rj
		}
		return findings[i].ID < findings[j].ID
	})
}

func severityRank(severity string) int {
	switch strings.ToLower(severity) {
	case "blocker":
		return 0
	case "high":
		return 1
	case "medium":
		return 2
	default:
		return 3
	}
}
