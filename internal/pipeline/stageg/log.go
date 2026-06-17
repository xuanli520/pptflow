package stageg

import (
	"fmt"
	browserpkg "github.com/xuanli520/p2r_tui/internal/browser"
	"strings"
)

func stageGLogHeader(sc StageContext, candidates []BrowserURLCandidate) string {
	var builder strings.Builder
	builder.WriteString("Stage G frontend browser E2E\n")
	builder.WriteString(fmt.Sprintf("run_id: %s\n", sc.Run.RunID))
	builder.WriteString(fmt.Sprintf("task_id: %s\n", sc.Run.TaskID))
	builder.WriteString(fmt.Sprintf("candidate_count: %d\n", len(candidates)))
	for _, candidate := range candidates {
		builder.WriteString(fmt.Sprintf("candidate %s: %s service=%s source=%s probe_ok=%t\n", candidate.ID, candidate.URL, candidate.Service, candidate.Source, candidate.ProbeOK))
	}
	builder.WriteString("\n")
	return builder.String()
}

func stageGLogPlannedAction(round int, action BrowserAction) string {
	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("round %d planned action=%s", round, strings.TrimSpace(action.Action)))
	if text := compactLogValue(action.Text, 120); text != "" {
		builder.WriteString(" text=" + text)
	}
	if selector := compactLogValue(action.Selector, 160); selector != "" {
		builder.WriteString(" selector=" + selector)
	}
	if action.Action == "fill_input" && action.Value != "" {
		builder.WriteString(fmt.Sprintf(" value=[REDACTED len=%d]", len(action.Value)))
	}
	if action.URLID != "" {
		builder.WriteString(" url_id=" + compactLogValue(action.URLID, 80))
	}
	if reason := compactLogValue(action.Reason, 180); reason != "" {
		builder.WriteString(" reason=" + reason)
	}
	builder.WriteString("\n")
	return builder.String()
}

func compactLogValue(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		return ""
	}
	if limit > 0 && len(value) > limit {
		value = value[:limit]
	}
	return fmt.Sprintf("%q", value)
}

func stageGLogObservation(round int, observation browserpkg.Observation) string {
	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("round %d action=%s ok=%t url=%s title=%s\n", round, observation.Action, observation.OK, observation.CurrentURL, observation.Title))
	if strings.TrimSpace(observation.ScreenshotPath) != "" {
		builder.WriteString("screenshot: " + strings.TrimSpace(observation.ScreenshotPath) + "\n")
	}
	if strings.TrimSpace(observation.Error) != "" {
		builder.WriteString("error: " + strings.TrimSpace(observation.Error) + "\n")
	}
	if len(observation.PageErrors) > 0 {
		builder.WriteString(fmt.Sprintf("page_errors: %d\n", len(observation.PageErrors)))
	}
	if len(observation.ConsoleErrors) > 0 {
		builder.WriteString(fmt.Sprintf("console_errors: %d\n", len(observation.ConsoleErrors)))
	}
	if len(observation.NetworkIssues) > 0 {
		builder.WriteString(fmt.Sprintf("network_issues: %d\n", len(observation.NetworkIssues)))
		for _, issue := range observation.NetworkIssues {
			if issue.Status > 0 {
				builder.WriteString(fmt.Sprintf("- network_issue: %s status=%d\n", issue.URL, issue.Status))
			} else {
				builder.WriteString(fmt.Sprintf("- network_issue: %s %s\n", issue.URL, issue.Error))
			}
		}
	}
	if len(observation.NetworkEvents) > 0 {
		builder.WriteString(fmt.Sprintf("network_events: %d\n", len(observation.NetworkEvents)))
		for _, event := range observation.NetworkEvents {
			builder.WriteString("- network_event: " + stageGNetworkEventText(event) + "\n")
		}
	}
	return builder.String()
}

func stageGNetworkEventText(event browserpkg.NetworkEvent) string {
	var parts []string
	if event.Method != "" {
		parts = append(parts, event.Method)
	}
	if event.URL != "" {
		parts = append(parts, event.URL)
	}
	if event.Status > 0 {
		parts = append(parts, fmt.Sprintf("status=%d", event.Status))
	}
	if event.ResourceType != "" {
		parts = append(parts, "type="+event.ResourceType)
	}
	if event.Error != "" {
		parts = append(parts, "error="+event.Error)
	}
	return strings.Join(parts, " ")
}

func stageGNetworkIssueText(issue browserpkg.NetworkIssue) string {
	var parts []string
	if issue.URL != "" {
		parts = append(parts, issue.URL)
	}
	if issue.Status > 0 {
		parts = append(parts, fmt.Sprintf("status=%d", issue.Status))
	}
	if issue.Error != "" {
		parts = append(parts, "error="+issue.Error)
	}
	return strings.Join(parts, " ")
}

func stageGLogFinish(status, reason string, observationCount, findingCount int) string {
	var builder strings.Builder
	builder.WriteString("\nfinish\n")
	builder.WriteString("status: " + strings.TrimSpace(status) + "\n")
	if strings.TrimSpace(reason) != "" {
		builder.WriteString("reason: " + strings.TrimSpace(reason) + "\n")
	}
	builder.WriteString(fmt.Sprintf("observations: %d\n", observationCount))
	builder.WriteString(fmt.Sprintf("findings: %d\n", findingCount))
	return builder.String()
}
