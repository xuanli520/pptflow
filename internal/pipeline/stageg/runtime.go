package stageg

import (
	"context"
	"fmt"
	browserpkg "github.com/xuanli520/p2r_tui/internal/browser"
	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/frontende2e"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func stageGPlannerTimedOut(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if ctx.Err() != nil {
		return true
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{"context deadline exceeded", "timed out", "timeout", "deadline exceeded", "deadline"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func stageGPlannerTurnTimeout(cfg config.StageGConfig) time.Duration {
	if cfg.PlannerTurnTimeoutSeconds <= 0 {
		return frontende2e.DefaultPlannerTurnTimeout
	}
	return time.Duration(cfg.PlannerTurnTimeoutSeconds) * time.Second
}

func stageGPlannerTurnTimeoutSeconds(cfg config.StageGConfig) int {
	return int(stageGPlannerTurnTimeout(cfg) / time.Second)
}

func (r Runner) nextStageGBrowserAction(ctx context.Context, sc StageContext, promptTemplate, profile, contextText string, candidates []BrowserURLCandidate, observations []browserpkg.Observation, blocked []BlockedBrowserAction, round int, timeout time.Duration) (string, []ArtifactWarning, error) {
	if r.request.Planner == nil {
		return "", nil, fmt.Errorf("Stage G browser planner is not configured")
	}
	return r.request.Planner(ctx, promptTemplate, profile, contextText, candidates, observations, blocked, round, timeout)
}

func (r Runner) runStageGBrowserAction(ctx context.Context, action browserpkg.Action, policy browserpkg.Policy, timeout time.Duration) (browserpkg.Observation, error) {
	if r.request.ActionRunner == nil {
		return browserpkg.Observation{}, fmt.Errorf("Stage G browser action runner is not configured")
	}
	return r.request.ActionRunner(ctx, action, policy, timeout)
}

func stageGFinishedStatus(record model.StageRecord) string {
	if record.Status == model.StageFailed {
		return model.StageFailed
	}
	return model.StageDone
}

func stageGStatusForAcceptedSummary(summary FrontendE2ESummary, record model.StageRecord) (model.StageRecord, string) {
	switch strings.TrimSpace(summary.Status) {
	case "failed", "partial":
		if record.ErrorSummary == "" {
			record.ErrorSummary = "frontend E2E findings"
		}
		return record, model.StageFailed
	case "blocked":
		if record.ErrorSummary == "" {
			record.ErrorSummary = "frontend E2E blocked"
		}
		return record, model.StageBlocked
	default:
		return record, stageGFinishedStatus(record)
	}
}

func cleanupStageGBrowserRuntime(artifactRoot string) {
	if strings.TrimSpace(artifactRoot) == "" {
		return
	}
	_ = os.RemoveAll(filepath.Join(artifactRoot, "browser_runtime"))
}

func stageGRuntimeScreenshotPath(artifactRoot string, round int, action string) string {
	name := fmt.Sprintf("round_%02d_%s.png", round, stageGScreenshotSafeName(action))
	return filepath.Join(artifactRoot, "browser_runtime", "screenshots", name)
}

func stageGShouldCaptureActionScreenshot(action BrowserAction, observations []browserpkg.Observation) bool {
	actionName := strings.TrimSpace(action.Action)
	switch actionName {
	case "open_candidate", "wait", "snapshot", "collect_console", "collect_network", "click_navigation", "submit_local_form", "go_back":
		return true
	case "click_button":
		if stageGBrowserScreenshotCount(observations) < stageGMinBrowserScreenshots {
			return true
		}
		return stageGActionLooksBusinessCritical(action)
	default:
		return false
	}
}

func stageGActionLooksBusinessCritical(action BrowserAction) bool {
	text := strings.ToLower(strings.Join([]string{action.Text, action.Selector, action.Reason}, " "))
	for _, keyword := range []string{
		"login", "log in", "sign in", "signin", "submit", "admin", "dashboard", "studio", "analytics",
		"catalog", "product", "cart", "order", "checkout", "create", "save", "apply", "publish", "upload",
	} {
		if strings.Contains(text, keyword) {
			return true
		}
	}
	return false
}

func stageGScreenshotSafeName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "action"
	}
	var builder strings.Builder
	lastDash := false
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			builder.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			builder.WriteByte('-')
			lastDash = true
		}
	}
	result := strings.Trim(builder.String(), "-")
	if result == "" {
		return "action"
	}
	if len(result) > 48 {
		result = result[:48]
		result = strings.TrimRight(result, "-")
	}
	return result
}
