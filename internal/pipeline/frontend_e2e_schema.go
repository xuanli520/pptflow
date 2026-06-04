package pipeline

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
)

const frontendE2ESchemaVersion = "p2r.frontend_e2e.v1"

type FrontendE2ESummary struct {
	SchemaVersion  string                 `json:"schema_version"`
	Status         string                 `json:"status"`
	Reason         string                 `json:"reason,omitempty"`
	URLCandidates  []BrowserURLCandidate  `json:"url_candidates,omitempty"`
	VisitedURLs    []string               `json:"visited_urls,omitempty"`
	Screenshots    []string               `json:"screenshots,omitempty"`
	BlockedActions []BlockedBrowserAction `json:"blocked_actions,omitempty"`
	Findings       []FrontendE2EFinding   `json:"findings"`
	Notes          []string               `json:"notes,omitempty"`
}

type FrontendE2EFinding struct {
	Severity   string `json:"severity"`
	Title      string `json:"title"`
	Rule       string `json:"rule,omitempty"`
	Evidence   string `json:"evidence,omitempty"`
	Impact     string `json:"impact,omitempty"`
	MinimumFix string `json:"minimum_fix,omitempty"`
	Screenshot string `json:"screenshot,omitempty"`
}

func parseFrontendE2ESummary(raw json.RawMessage) (FrontendE2ESummary, error) {
	if len(raw) == 0 {
		return FrontendE2ESummary{}, fmt.Errorf("finish action missing summary")
	}
	var summary FrontendE2ESummary
	if err := json.Unmarshal(raw, &summary); err != nil {
		return FrontendE2ESummary{}, err
	}
	if err := validateFrontendE2ESummary(summary); err != nil {
		return FrontendE2ESummary{}, err
	}
	return summary, nil
}

func validateFrontendE2ESummary(summary FrontendE2ESummary) error {
	if summary.SchemaVersion != frontendE2ESchemaVersion {
		return fmt.Errorf("schema_version must be %s", frontendE2ESchemaVersion)
	}
	switch strings.TrimSpace(summary.Status) {
	case "passed", "failed", "partial", "blocked", "not_applicable":
	default:
		return fmt.Errorf("status must be passed, failed, partial, blocked, or not_applicable")
	}
	for index, finding := range summary.Findings {
		if !validFrontendE2ESeverity(finding.Severity) {
			return fmt.Errorf("finding %d severity must be Blocker, High, Medium, or Low", index+1)
		}
		if strings.TrimSpace(finding.Title) == "" {
			return fmt.Errorf("finding %d title is required", index+1)
		}
	}
	return nil
}

func validFrontendE2ESeverity(severity string) bool {
	switch strings.TrimSpace(severity) {
	case "Blocker", "High", "Medium", "Low":
		return true
	default:
		return false
	}
}

func frontendE2EFindings(summary FrontendE2ESummary, sourcePath string) []model.Finding {
	findings := make([]model.Finding, 0, len(summary.Findings))
	for _, item := range summary.Findings {
		evidence := strings.TrimSpace(item.Evidence)
		if item.Screenshot != "" {
			if evidence != "" {
				evidence += "\n"
			}
			evidence += "screenshot: " + item.Screenshot
		}
		findings = append(findings, model.Finding{
			Stage:      string(model.StageG),
			Severity:   strings.TrimSpace(item.Severity),
			Title:      strings.TrimSpace(item.Title),
			Rule:       strings.TrimSpace(item.Rule),
			Evidence:   evidence,
			Impact:     strings.TrimSpace(item.Impact),
			MinimumFix: strings.TrimSpace(item.MinimumFix),
			SourcePath: sourcePath,
		})
	}
	return findings
}

func frontendE2ESchemaFailureFinding(sourcePath string, err error) model.Finding {
	return model.Finding{
		Stage:      string(model.StageG),
		Severity:   "High",
		Title:      "frontend E2E summary schema invalid",
		Rule:       "Stage G must produce a valid p2r.frontend_e2e.v1 summary.",
		Evidence:   err.Error(),
		Impact:     "p2r cannot reliably persist browser E2E findings.",
		MinimumFix: "Rerun Stage G with a valid finish summary.",
		SourcePath: sourcePath,
	}
}
