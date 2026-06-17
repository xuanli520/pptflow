package stageg

import (
	"encoding/json"
	"fmt"
	browserpkg "github.com/xuanli520/p2r_tui/internal/browser"
	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/frontende2e"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"os"
	"path/filepath"
	"strings"
)

func stageGSummary(status, reason string, candidates []BrowserURLCandidate, observations []browserpkg.Observation, blocked []BlockedBrowserAction) FrontendE2ESummary {
	var visited []string
	var screenshots []string
	for _, observation := range observations {
		if observation.CurrentURL != "" {
			visited = append(visited, observation.CurrentURL)
		}
		if observation.ScreenshotPath != "" {
			screenshots = append(screenshots, observation.ScreenshotPath)
		}
	}
	return FrontendE2ESummary{
		SchemaVersion:  frontendE2ESchemaVersion,
		Status:         status,
		Reason:         reason,
		URLCandidates:  candidates,
		VisitedURLs:    visited,
		Screenshots:    screenshots,
		BlockedActions: blocked,
		Findings:       []FrontendE2EFinding{},
	}
}

func frontendE2EObservationFindings(observations []browserpkg.Observation, screenshot string, includeActionFailures bool) []model.Finding {
	var findings []model.Finding
	blankRecorded := false
	for index, observation := range observations {
		if !observation.OK && includeActionFailures {
			evidence := strings.TrimSpace(observation.Error)
			if evidence == "" {
				evidence = "browser action did not complete successfully"
			}
			findings = append(findings, model.Finding{
				Stage:      string(model.StageG),
				Severity:   "High",
				Title:      "Browser action failed during frontend E2E",
				Rule:       "Every validated browser action must either complete or produce a failing Stage G finding.",
				Evidence:   evidence,
				Impact:     "The browser exploration could not verify the intended user workflow.",
				MinimumFix: "Inspect frontend_e2e_observations.json and fix the page or action target.",
				SourcePath: screenshot,
			})
		}
		if observation.CurrentURL != "" && len(strings.TrimSpace(observation.VisibleText)) < 10 && !blankRecorded {
			blankRecorded = true
			findings = append(findings, model.Finding{
				Stage:      string(model.StageG),
				Severity:   "High",
				Title:      "Frontend page appears blank",
				Rule:       "A browser-visible frontend should render meaningful visible content.",
				Evidence:   "URL: " + observation.CurrentURL,
				Impact:     "Users may see a blank or non-rendered application shell.",
				MinimumFix: "Fix frontend boot/render errors and rerun Stage G.",
				SourcePath: screenshot,
			})
		}
		if len(observation.PageErrors) > 0 {
			findings = append(findings, model.Finding{
				Stage:      string(model.StageG),
				Severity:   "High",
				Title:      "Frontend runtime page error",
				Rule:       "Browser pages must not throw uncaught runtime errors during E2E exploration.",
				Evidence:   strings.Join(observation.PageErrors, "\n"),
				Impact:     "Core frontend workflows may be broken for users.",
				MinimumFix: "Fix the reported runtime exception and rerun Stage G.",
				SourcePath: screenshot,
			})
		}
		if len(observation.ConsoleErrors) > 0 &&
			!stageGConsoleErrorsOnlyRecoveredAuthNoise(index, observations) &&
			!stageGConsoleErrorsOnlyAuthGateNoise(index, observations) {
			findings = append(findings, model.Finding{
				Stage:      string(model.StageG),
				Severity:   "Medium",
				Title:      "Frontend console errors were observed",
				Rule:       "Browser E2E should not produce console errors during normal page load or interaction.",
				Evidence:   strings.Join(observation.ConsoleErrors, "\n"),
				Impact:     "Frontend behavior may be degraded or brittle.",
				MinimumFix: "Fix the console error source and rerun Stage G.",
				SourcePath: screenshot,
			})
		}
		for _, issue := range observation.NetworkIssues {
			if !stageGNetworkIssueBlocksEvidence(index, issue, observations) {
				continue
			}
			if issue.Status >= 500 {
				findings = append(findings, model.Finding{
					Stage:      string(model.StageG),
					Severity:   "High",
					Title:      "Frontend request returned server error",
					Rule:       "Browser E2E should not observe 5xx responses for app resources or APIs.",
					Evidence:   fmt.Sprintf("%s status=%d", issue.URL, issue.Status),
					Impact:     "A user-visible workflow or required resource may fail.",
					MinimumFix: "Fix the failing endpoint or resource and rerun Stage G.",
					SourcePath: screenshot,
				})
			} else if issue.Status >= 400 {
				findings = append(findings, model.Finding{
					Stage:      string(model.StageG),
					Severity:   "Medium",
					Title:      "Frontend request returned client error",
					Rule:       "Browser E2E should not observe unexpected 4xx responses for app resources or APIs.",
					Evidence:   fmt.Sprintf("%s status=%d", issue.URL, issue.Status),
					Impact:     "A page route, asset, or API call may be misconfigured.",
					MinimumFix: "Fix the failing request and rerun Stage G.",
					SourcePath: screenshot,
				})
			}
		}
	}
	return findings
}

func includeStageGActionFailureFallback(summary FrontendE2ESummary, summaryFindings []model.Finding) bool {
	if len(summaryFindings) > 0 {
		return false
	}
	switch strings.TrimSpace(summary.Status) {
	case "passed", "not_applicable":
		return false
	default:
		return true
	}
}

func frontendE2EFindingsFromModel(findings []model.Finding) []FrontendE2EFinding {
	result := make([]FrontendE2EFinding, 0, len(findings))
	for _, finding := range findings {
		result = append(result, frontendE2EFindingFromModel(finding))
	}
	return result
}

func frontendE2EFindingFromModel(finding model.Finding) FrontendE2EFinding {
	return FrontendE2EFinding{
		Severity:   strings.TrimSpace(finding.Severity),
		Title:      strings.TrimSpace(finding.Title),
		Rule:       strings.TrimSpace(finding.Rule),
		Evidence:   strings.TrimSpace(finding.Evidence),
		Impact:     strings.TrimSpace(finding.Impact),
		MinimumFix: strings.TrimSpace(finding.MinimumFix),
		Screenshot: strings.TrimSpace(finding.SourcePath),
	}
}

func frontendE2EReport(summary FrontendE2ESummary, observations []browserpkg.Observation) string {
	var builder strings.Builder
	builder.WriteString("# Browser Frontend E2E\n\n")
	builder.WriteString("Status: " + summary.Status + "\n")
	if summary.Reason != "" {
		builder.WriteString("Reason: " + summary.Reason + "\n")
	}
	builder.WriteString("\n## URL Candidates\n")
	for _, candidate := range summary.URLCandidates {
		builder.WriteString(fmt.Sprintf("- %s %s service=%s probe_ok=%t\n", candidate.ID, candidate.URL, candidate.Service, candidate.ProbeOK))
	}
	builder.WriteString("\n## Findings\n")
	if len(summary.Findings) == 0 {
		builder.WriteString("- none\n")
	} else {
		for _, finding := range summary.Findings {
			builder.WriteString(fmt.Sprintf("- %s: %s\n", finding.Severity, finding.Title))
		}
	}
	if len(summary.BlockedActions) > 0 {
		builder.WriteString("\n## Blocked Actions\n")
		for _, action := range summary.BlockedActions {
			builder.WriteString(fmt.Sprintf("- %s: %s\n", action.Action, action.Reason))
		}
	}
	if len(summary.Screenshots) > 0 {
		builder.WriteString("\n## Screenshots\n")
		for index, screenshot := range summary.Screenshots {
			builder.WriteString(fmt.Sprintf("- %02d %s\n", index+1, screenshot))
		}
	}
	if len(observations) > 0 {
		builder.WriteString("\n## Observations\n")
		for _, observation := range observations {
			builder.WriteString(fmt.Sprintf("- %s ok=%t url=%s title=%s\n", observation.Action, observation.OK, observation.CurrentURL, observation.Title))
			if observation.ScreenshotPath != "" {
				builder.WriteString(fmt.Sprintf("  screenshot: %s\n", observation.ScreenshotPath))
			}
			for _, err := range observation.PageErrors {
				builder.WriteString(fmt.Sprintf("  page_error: %s\n", err))
			}
			for _, err := range observation.ConsoleErrors {
				builder.WriteString(fmt.Sprintf("  console_error: %s\n", err))
			}
			for _, issue := range observation.NetworkIssues {
				if issue.Status > 0 {
					builder.WriteString(fmt.Sprintf("  network_issue: %s status=%d\n", issue.URL, issue.Status))
				} else {
					builder.WriteString(fmt.Sprintf("  network_issue: %s %s\n", issue.URL, issue.Error))
				}
			}
			for _, event := range observation.NetworkEvents {
				builder.WriteString("  network_event: " + stageGNetworkEventText(event) + "\n")
			}
			for _, blocked := range observation.BlockedRequests {
				builder.WriteString(fmt.Sprintf("  blocked_request: %s\n", blocked.URL))
			}
		}
	}
	return builder.String()
}

func stageGBrowserContext(projectPath string) string {
	return frontende2e.BrowserContext(projectPath)
}

func projectTypeFromMetadata(projectPath string) string {
	content, err := os.ReadFile(filepath.Join(projectPath, "metadata.json"))
	if err != nil {
		return ""
	}
	var data map[string]any
	if json.Unmarshal(content, &data) != nil {
		return ""
	}
	return config.NormalizeProjectType(fmt.Sprint(data["project_type"]))
}
