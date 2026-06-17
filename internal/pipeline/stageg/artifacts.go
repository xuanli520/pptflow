package stageg

import (
	"fmt"
	browserpkg "github.com/xuanli520/p2r_tui/internal/browser"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func stageGBrowserScreenshotCount(observations []browserpkg.Observation) int {
	return len(stageGKeyScreenshotObservationIndexes(observations))
}

func stageGKeyScreenshotObservationIndexes(observations []browserpkg.Observation) []int {
	var eligible []int
	for index, observation := range observations {
		if stageGScreenshotObservationEligible(index, observation, observations) {
			eligible = append(eligible, index)
		}
	}
	if len(eligible) == 0 {
		return nil
	}
	selected := map[int]bool{}
	seenStates := map[string]bool{}
	add := func(index int) {
		if index < 0 || selected[index] {
			return
		}
		state := stageGObservationStateKey(observations[index])
		if state != "" && seenStates[state] {
			return
		}
		selected[index] = true
		if state != "" {
			seenStates[state] = true
		}
	}
	add(eligible[0])
	add(eligible[len(eligible)-1])
	for _, index := range eligible {
		observation := observations[index]
		if !observation.OK {
			add(index)
		}
		if len(observation.PageErrors) > 0 || len(observation.ConsoleErrors) > 0 || len(observation.NetworkIssues) > 0 {
			add(index)
		}
		if stageGObservationHasBusinessNetworkEvidenceAt(index, observations) {
			add(index)
		}
	}
	if len(selected) > stageGMaxBrowserScreenshots {
		return trimStageGScreenshotIndexes(sortedStageGIndexSet(selected))
	}
	for _, index := range eligible {
		if len(selected) >= stageGMaxBrowserScreenshots {
			break
		}
		observation := observations[index]
		if observation.CurrentURL != "" && stageGObservationURLChanged(index, observations) {
			add(index)
		}
	}
	for _, index := range eligible {
		if len(selected) >= stageGMaxBrowserScreenshots {
			break
		}
		add(index)
	}
	return sortedStageGIndexSet(selected)
}

func stageGScreenshotObservationEligible(index int, observation browserpkg.Observation, observations []browserpkg.Observation) bool {
	if path := strings.TrimSpace(observation.ScreenshotPath); path == "" || !stageGScreenshotFileUsable(path) {
		return false
	}
	action := strings.TrimSpace(observation.Action)
	if action == "fill_input" {
		return false
	}
	productFailure := stageGObservationHasProductFailureEvidenceAt(index, observations)
	if !observation.OK && !productFailure {
		return false
	}
	switch action {
	case "open_candidate", "snapshot":
		return strings.TrimSpace(observation.VisibleText) != "" || strings.TrimSpace(observation.CurrentURL) != "" || productFailure
	case "click_navigation", "submit_local_form", "go_back":
		return productFailure || (observation.OK && (stageGObservationURLChanged(index, observations) || stageGMeaningfulObservationStateChanged(index, observations) || stageGObservationHasBusinessNetworkEvidenceAt(index, observations)))
	case "click_button":
		return productFailure || (observation.OK && (stageGObservationURLChanged(index, observations) || stageGMeaningfulObservationStateChanged(index, observations) || stageGObservationHasBusinessNetworkEvidenceAt(index, observations)))
	case "wait", "collect_console", "collect_network":
		return observation.OK && stageGMeaningfulObservationStateChanged(index, observations)
	default:
		return productFailure
	}
}

func stageGMeaningfulObservationStateChanged(index int, observations []browserpkg.Observation) bool {
	if !stageGObservationStateChanged(index, observations) {
		return false
	}
	return !stageGObservationOnlyRecoveredAuthFailure(index, observations)
}

func evenlySampledScreenshotIndex(eligible []int, selected map[int]bool) int {
	if len(eligible) == 0 {
		return -1
	}
	bestIndex := -1
	bestDistance := -1
	for _, candidate := range eligible {
		if selected[candidate] {
			continue
		}
		distance := nearestSelectedDistance(candidate, selected)
		if distance > bestDistance {
			bestDistance = distance
			bestIndex = candidate
		}
	}
	return bestIndex
}

func nearestSelectedDistance(candidate int, selected map[int]bool) int {
	best := -1
	for index := range selected {
		distance := candidate - index
		if distance < 0 {
			distance = -distance
		}
		if best < 0 || distance < best {
			best = distance
		}
	}
	if best < 0 {
		return candidate
	}
	return best
}

func trimStageGScreenshotIndexes(indexes []int) []int {
	keep := map[int]bool{}
	if len(indexes) == 0 {
		return nil
	}
	keep[indexes[0]] = true
	keep[indexes[len(indexes)-1]] = true
	for len(keep) < stageGMaxBrowserScreenshots {
		next := evenlySampledScreenshotIndex(indexes, keep)
		if next < 0 {
			break
		}
		keep[next] = true
	}
	result := make([]int, 0, len(keep))
	for index := range keep {
		result = append(result, index)
	}
	sort.Ints(result)
	return result
}

func sortedStageGIndexSet(indexes map[int]bool) []int {
	result := make([]int, 0, len(indexes))
	for index := range indexes {
		result = append(result, index)
	}
	sort.Ints(result)
	return result
}

func (r Runner) writeStageGArtifacts(record model.StageRecord, writer ArtifactWriter, summary FrontendE2ESummary, observations []browserpkg.Observation) model.StageRecord {
	if observations == nil {
		observations = []browserpkg.Observation{}
	}
	record, summary, observations = materializeStageGScreenshotArtifacts(record, writer, summary, observations)
	record = requiredStageJSON(record, writer, "frontend_e2e_summary.json", summary)
	record = requiredStageText(record, writer, "frontend_e2e_report.md", frontendE2EReport(summary, observations))
	record = requiredStageJSON(record, writer, "frontend_e2e_observations.json", observations)
	return record
}

func materializeStageGScreenshotArtifacts(record model.StageRecord, writer ArtifactWriter, summary FrontendE2ESummary, observations []browserpkg.Observation) (model.StageRecord, FrontendE2ESummary, []browserpkg.Observation) {
	selected := stageGKeyScreenshotObservationIndexes(observations)
	selected = stageGEnsureFindingEvidenceScreenshot(summary, observations, selected)
	selected = stageGEnsureMinimumSupportScreenshots(summary, observations, selected)
	selectedSet := map[int]bool{}
	for _, index := range selected {
		selectedSet[index] = true
	}
	var screenshots []string
	for selectedIndex, observationIndex := range selected {
		source := strings.TrimSpace(observations[observationIndex].ScreenshotPath)
		dest := stageGFinalScreenshotPath(writer, selectedIndex, len(selected), observations[observationIndex])
		if err := copyStageGScreenshot(source, dest); err != nil {
			record = recordArtifactWriteError(record, err, dest)
			observations[observationIndex].ScreenshotPath = ""
			continue
		}
		observations[observationIndex].ScreenshotPath = dest
		screenshots = append(screenshots, dest)
		record = ensureArtifactPath(record, dest)
	}
	for index := range observations {
		if !selectedSet[index] {
			observations[index].ScreenshotPath = ""
		}
	}
	if len(screenshots) == 0 {
		evidencePath := writer.Path(stageGEvidenceSummaryName)
		if err := os.MkdirAll(filepath.Dir(evidencePath), 0o755); err != nil {
			record = recordArtifactWriteError(record, err, evidencePath)
		} else if err := os.WriteFile(evidencePath, []byte(stageGScreenshotFallbackText(summary, observations)), 0o644); err != nil {
			record = recordArtifactWriteError(record, err, evidencePath)
		} else {
			record = ensureArtifactPath(record, evidencePath)
		}
	}
	summary.Screenshots = screenshots
	return record, summary, observations
}

func stageGEnsureFindingEvidenceScreenshot(summary FrontendE2ESummary, observations []browserpkg.Observation, selected []int) []int {
	if !stageGSummaryNeedsFailureEvidenceScreenshot(summary) {
		return selected
	}
	selectedSet := map[int]bool{}
	for _, index := range selected {
		selectedSet[index] = true
	}
	for index := len(observations) - 1; index >= 0; index-- {
		if selectedSet[index] || !stageGScreenshotObservationCanSupportFinding(index, observations) {
			continue
		}
		return stageGAppendRequiredScreenshotIndex(selected, index)
	}
	return selected
}

func stageGEnsureMinimumSupportScreenshots(summary FrontendE2ESummary, observations []browserpkg.Observation, selected []int) []int {
	minimum := stageGMinimumSupportScreenshotCount(summary)
	if minimum == 0 || len(selected) >= minimum {
		return selected
	}
	selectedSet := map[int]bool{}
	for _, index := range selected {
		selectedSet[index] = true
	}
	for index := range observations {
		if len(selected) >= minimum {
			break
		}
		if selectedSet[index] || !stageGScreenshotObservationCanSupportSummary(index, observations) {
			continue
		}
		selected = append(selected, index)
		selectedSet[index] = true
	}
	sort.Ints(selected)
	return selected
}

func stageGMinimumSupportScreenshotCount(summary FrontendE2ESummary) int {
	if stageGSummaryNeedsFailureEvidenceScreenshot(summary) {
		return stageGMinBrowserScreenshots
	}
	if strings.TrimSpace(summary.Status) == "passed" {
		return 2
	}
	return 0
}

func stageGSummaryNeedsFailureEvidenceScreenshot(summary FrontendE2ESummary) bool {
	status := strings.TrimSpace(summary.Status)
	return status == "failed" || status == "partial" || len(summary.Findings) > 0
}

func stageGScreenshotObservationCanSupportFinding(index int, observations []browserpkg.Observation) bool {
	return stageGScreenshotObservationCanSupportSummary(index, observations)
}

func stageGScreenshotObservationCanSupportSummary(index int, observations []browserpkg.Observation) bool {
	if index < 0 || index >= len(observations) {
		return false
	}
	observation := observations[index]
	if path := strings.TrimSpace(observation.ScreenshotPath); path == "" || !stageGScreenshotFileUsable(path) {
		return false
	}
	if strings.TrimSpace(observation.Action) == "fill_input" {
		return false
	}
	if !observation.OK && !stageGObservationHasProductFailureEvidenceAt(index, observations) {
		return false
	}
	if stageGObservationOnlyRecoveredAuthFailure(index, observations) {
		return false
	}
	return strings.TrimSpace(observation.VisibleText) != "" || strings.TrimSpace(observation.CurrentURL) != ""
}

func stageGAppendRequiredScreenshotIndex(selected []int, required int) []int {
	for _, index := range selected {
		if index == required {
			return selected
		}
	}
	selected = append(selected, required)
	sort.Ints(selected)
	if len(selected) <= stageGMaxBrowserScreenshots {
		return selected
	}
	for index := len(selected) - 1; index >= 0; index-- {
		if selected[index] == required || index == 0 {
			continue
		}
		return append(selected[:index], selected[index+1:]...)
	}
	return selected[:stageGMaxBrowserScreenshots]
}

func stageGFinalScreenshotPath(writer ArtifactWriter, selectedIndex, selectedCount int, observation browserpkg.Observation) string {
	if selectedIndex == selectedCount-1 {
		return writer.Path(stageGLegacyScreenshotName)
	}
	name := fmt.Sprintf("%02d_%s.png", selectedIndex+1, stageGScreenshotSafeName(observation.Action))
	return writer.Path(filepath.Join(stageGKeyScreenshotDirName, name))
}

func copyStageGScreenshot(source, dest string) error {
	source = strings.TrimSpace(source)
	if source == "" {
		return fmt.Errorf("source screenshot path is empty")
	}
	if !stageGScreenshotFileUsable(source) {
		return fmt.Errorf("source screenshot is not a valid PNG: %s", source)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	content, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return os.WriteFile(dest, content, 0o644)
}

func stageGScreenshotFileUsable(path string) bool {
	content, err := os.ReadFile(strings.TrimSpace(path))
	if err != nil || len(content) < 8 {
		return false
	}
	return content[0] == 0x89 &&
		content[1] == 'P' &&
		content[2] == 'N' &&
		content[3] == 'G' &&
		content[4] == '\r' &&
		content[5] == '\n' &&
		content[6] == 0x1a &&
		content[7] == '\n'
}

func stageGScreenshotFallbackText(summary FrontendE2ESummary, observations []browserpkg.Observation) string {
	var builder strings.Builder
	builder.WriteString("Stage G browser frontend E2E\n")
	builder.WriteString("status: " + summary.Status + "\n")
	if summary.Reason != "" {
		builder.WriteString("reason: " + summary.Reason + "\n")
	}
	if len(summary.Findings) > 0 {
		builder.WriteString("findings:\n")
		for _, finding := range summary.Findings {
			builder.WriteString("- " + finding.Severity + ": " + finding.Title + "\n")
		}
	}
	if len(observations) > 0 {
		builder.WriteString("observations:\n")
		for _, observation := range observations {
			builder.WriteString(fmt.Sprintf("- %s ok=%t url=%s\n", observation.Action, observation.OK, observation.CurrentURL))
		}
	}
	return builder.String()
}

func ensureArtifactPath(record model.StageRecord, path string) model.StageRecord {
	path = strings.TrimSpace(path)
	if path == "" {
		return record
	}
	for _, existing := range record.ArtifactPaths {
		if existing == path {
			return record
		}
	}
	record.ArtifactPaths = append(record.ArtifactPaths, path)
	return record
}
