package stageg

import (
	browserpkg "github.com/xuanli520/p2r_tui/internal/browser"
	"github.com/xuanli520/p2r_tui/internal/frontende2e"
	"strings"
)

func stageGObservationHasProductFailureEvidenceAt(index int, observations []browserpkg.Observation) bool {
	if index < 0 || index >= len(observations) {
		return false
	}
	observation := observations[index]
	if len(observation.PageErrors) > 0 {
		return true
	}
	if len(observation.ConsoleErrors) > 0 &&
		!stageGConsoleErrorsOnlyRecoveredAuthNoise(index, observations) &&
		!stageGConsoleErrorsOnlyAuthGateNoise(index, observations) {
		return true
	}
	for _, issue := range observation.NetworkIssues {
		if stageGNetworkIssueBlocksEvidence(index, issue, observations) {
			return true
		}
	}
	for _, event := range observation.NetworkEvents {
		if stageGNetworkEventBlocksEvidence(index, event, observations) {
			return true
		}
	}
	return false
}

func stageGNetworkIssueBlocksEvidence(index int, issue browserpkg.NetworkIssue, observations []browserpkg.Observation) bool {
	if stageGNetworkFailureLooksAuthGateNoise(index, issue.URL, issue.Status, observations) {
		return false
	}
	if stageGNetworkFailureLooksPendingAuthRetry(index, issue.URL, issue.Status, observations) {
		return false
	}
	if stageGNetworkIssueRecovered(index, issue, observations) {
		return false
	}
	if stageGNetworkFailureLooksIgnorableNoise(issue.URL, "", issue.Status, issue.Error) {
		return false
	}
	return issue.Status >= 400 || strings.TrimSpace(issue.Error) != ""
}

func stageGNetworkEventBlocksEvidence(index int, event browserpkg.NetworkEvent, observations []browserpkg.Observation) bool {
	if stageGNetworkFailureLooksAuthGateNoise(index, event.URL, event.Status, observations) {
		return false
	}
	if stageGNetworkFailureLooksPendingAuthRetry(index, event.URL, event.Status, observations) {
		return false
	}
	if stageGNetworkEventRecovered(index, event, observations) {
		return false
	}
	if stageGNetworkFailureLooksIgnorableNoise(event.URL, event.ResourceType, event.Status, event.Error) {
		return false
	}
	return event.Status >= 400 || strings.TrimSpace(event.Error) != ""
}

func stageGConsoleErrorsOnlyRecoveredAuthNoise(index int, observations []browserpkg.Observation) bool {
	if index < 0 || index >= len(observations) {
		return false
	}
	observation := observations[index]
	if len(observation.ConsoleErrors) == 0 || len(observation.NetworkIssues) == 0 {
		return false
	}
	for _, issue := range observation.NetworkIssues {
		if !stageGNetworkIssueRecovered(index, issue, observations) {
			return false
		}
	}
	for _, message := range observation.ConsoleErrors {
		if !stageGConsoleErrorLooksLikeAuthNetworkNoise(message) {
			return false
		}
	}
	return true
}

func stageGConsoleErrorsOnlyAuthGateNoise(index int, observations []browserpkg.Observation) bool {
	if index < 0 || index >= len(observations) {
		return false
	}
	observation := observations[index]
	if len(observation.ConsoleErrors) == 0 {
		return false
	}
	hasAuthGateNoise := false
	for _, issue := range observation.NetworkIssues {
		if stageGNetworkFailureLooksAuthGateNoise(index, issue.URL, issue.Status, observations) ||
			stageGNetworkFailureLooksPendingAuthRetry(index, issue.URL, issue.Status, observations) {
			hasAuthGateNoise = true
			break
		}
	}
	if !hasAuthGateNoise {
		for _, event := range observation.NetworkEvents {
			if stageGNetworkFailureLooksAuthGateNoise(index, event.URL, event.Status, observations) ||
				stageGNetworkFailureLooksPendingAuthRetry(index, event.URL, event.Status, observations) {
				hasAuthGateNoise = true
				break
			}
		}
	}
	if !hasAuthGateNoise {
		return false
	}
	for _, message := range observation.ConsoleErrors {
		if !stageGConsoleErrorLooksLikeAuthNetworkNoise(message) {
			return false
		}
	}
	return true
}

func stageGObservationOnlyRecoveredAuthFailure(index int, observations []browserpkg.Observation) bool {
	if index < 0 || index >= len(observations) {
		return false
	}
	if stageGObservationURLChanged(index, observations) {
		return false
	}
	observation := observations[index]
	recovered := false
	for _, issue := range observation.NetworkIssues {
		if stageGNetworkIssueRecovered(index, issue, observations) {
			recovered = true
			continue
		}
		if issue.Status >= 400 || strings.TrimSpace(issue.Error) != "" {
			return false
		}
	}
	for _, event := range observation.NetworkEvents {
		if stageGNetworkEventRecovered(index, event, observations) {
			recovered = true
			continue
		}
		if event.Status >= 400 || strings.TrimSpace(event.Error) != "" {
			return false
		}
	}
	return recovered
}

func stageGConsoleErrorLooksLikeAuthNetworkNoise(message string) bool {
	message = strings.ToLower(strings.TrimSpace(message))
	if message == "" {
		return false
	}
	return strings.Contains(message, "401") ||
		strings.Contains(message, "403") ||
		strings.Contains(message, "422") ||
		strings.Contains(message, "unauthorized") ||
		strings.Contains(message, "forbidden")
}

func stageGNetworkIssueRecovered(index int, issue browserpkg.NetworkIssue, observations []browserpkg.Observation) bool {
	return stageGRecoverableAuthClientStatus(issue.Status) &&
		stageGNetworkURLLooksAuth(issue.URL) &&
		stageGLaterNetworkSuccess(index, issue.URL, observations)
}

func stageGNetworkEventRecovered(index int, event browserpkg.NetworkEvent, observations []browserpkg.Observation) bool {
	return stageGRecoverableAuthClientStatus(event.Status) &&
		stageGNetworkURLLooksAuth(event.URL) &&
		stageGLaterNetworkSuccess(index, event.URL, observations)
}

func stageGRecoverableAuthClientStatus(status int) bool {
	return frontende2e.RecoverableAuthClientStatus(status)
}

func stageGNetworkFailureLooksAuthGateNoise(index int, raw string, status int, observations []browserpkg.Observation) bool {
	return frontende2e.NetworkFailureLooksAuthGateNoise(index, raw, status, observations)
}

func stageGNetworkFailureLooksPendingAuthRetry(index int, raw string, status int, observations []browserpkg.Observation) bool {
	return frontende2e.NetworkFailureLooksPendingAuthRetry(index, raw, status, observations)
}

func stageGLaterNetworkSuccess(index int, rawURL string, observations []browserpkg.Observation) bool {
	key := stageGNetworkURLKey(rawURL)
	if key == "" {
		return false
	}
	for next := index + 1; next < len(observations); next++ {
		for _, event := range observations[next].NetworkEvents {
			if stageGNetworkURLKey(event.URL) == key && event.Status >= 200 && event.Status < 400 {
				return true
			}
		}
	}
	return false
}

func stageGNetworkURLLooksAuth(raw string) bool {
	return frontende2e.NetworkURLLooksAuth(raw)
}

func stageGNetworkURLLooksLogout(raw string) bool {
	return frontende2e.NetworkURLLooksLogout(raw)
}

func stageGNetworkURLKey(raw string) string {
	return frontende2e.NetworkURLKey(raw)
}

func stageGObservationURLChanged(index int, observations []browserpkg.Observation) bool {
	if index <= 0 || index >= len(observations) {
		return false
	}
	current := strings.TrimSpace(observations[index].CurrentURL)
	if current == "" {
		return false
	}
	for previous := index - 1; previous >= 0; previous-- {
		prev := strings.TrimSpace(observations[previous].CurrentURL)
		if prev != "" {
			return prev != current
		}
	}
	return true
}

func stageGObservationStateChanged(index int, observations []browserpkg.Observation) bool {
	if index <= 0 || index >= len(observations) {
		return false
	}
	current := stageGObservationStateKey(observations[index])
	if current == "" {
		return false
	}
	for previous := index - 1; previous >= 0; previous-- {
		prev := stageGObservationStateKey(observations[previous])
		if prev != "" {
			return prev != current
		}
	}
	return true
}

func stageGObservationStateKey(observation browserpkg.Observation) string {
	parts := []string{
		strings.TrimSpace(observation.CurrentURL),
		strings.TrimSpace(observation.Title),
		stageGCompactStateText(observation.VisibleText, 900),
		stageGControlsStateKey(observation.Controls),
	}
	hasState := false
	for _, part := range parts {
		if part != "" {
			hasState = true
			break
		}
	}
	if !hasState {
		return ""
	}
	return strings.Join(parts, "\x00")
}

func stageGControlsStateKey(controls []browserpkg.ControlSummary) string {
	if len(controls) == 0 {
		return ""
	}
	var parts []string
	for index, control := range controls {
		if index >= 40 {
			break
		}
		text := stageGCompactStateText(strings.Join([]string{control.Role, control.Text, control.Name, control.Placeholder, control.Type}, " "), 160)
		if text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "|")
}

func stageGCompactStateText(value string, limit int) string {
	value = strings.ToLower(strings.Join(strings.Fields(value), " "))
	if limit > 0 && len(value) > limit {
		value = value[:limit]
	}
	return value
}

func stageGObservationHasBusinessNetworkEvidenceAt(index int, observations []browserpkg.Observation) bool {
	if index < 0 || index >= len(observations) {
		return false
	}
	for _, event := range observations[index].NetworkEvents {
		if stageGNetworkEventBlocksEvidence(index, event, observations) {
			return true
		}
		if event.Status >= 400 || event.Error != "" {
			continue
		}
		if stageGNetworkEventLooksSuccessfulBusiness(event) {
			return true
		}
	}
	return false
}
