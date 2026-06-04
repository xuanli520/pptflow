package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	browserpkg "github.com/xuanli520/p2r_tui/internal/browser"
	"github.com/xuanli520/p2r_tui/internal/codex"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
)

func (r Runner) nextBrowserAction(ctx context.Context, sc StageContext, profile, contextText string, candidates []BrowserURLCandidate, observations []browserpkg.Observation, blocked []BlockedBrowserAction, round int, timeout time.Duration) (string, []ArtifactWarning, error) {
	capability := codex.DetectCLI(ctx, r.exec, "")
	if err := codex.ValidateAppServerCapability(capability); err != nil {
		return "", nil, err
	}
	reviewPath := codexReviewPath(sc.Run, sc.Project.Path)
	sandbox, err := codex.NewSandbox(reviewPath, sc.Run.ArtifactRoot, string(model.StageG))
	if err != nil {
		return "", nil, err
	}
	defer os.RemoveAll(sandbox.Home)
	env := sandbox.EnvWithNode(os.Environ(), r.cfg.Codex.Env, capability.NodePath)
	extraArgs, err := safeCodexExtraArgs(r.cfg.Codex.ExtraArgs)
	if err != nil {
		return "", nil, err
	}
	logPath := filepath.Join(sc.Run.ArtifactRoot, "logs", fmt.Sprintf("G_frontend_e2e_codex_round_%02d.log", round))
	prompt := browserActionPrompt(sc, profile, contextText, candidates, observations, blocked, round)
	result := r.runCodexReviewWithLog(ctx, timeout, reviewPath, logPath, env, prompt, capability, extraArgs, codexDeltaProgress(sc.Run.RunID, string(model.StageG), sc.Progress))
	if result.Result.Err != nil {
		return "", result.ArtifactWarnings, result.Result.Err
	}
	actionJSON, err := extractJSONObject(result.Result.Stdout)
	return actionJSON, result.ArtifactWarnings, err
}

func browserActionPrompt(sc StageContext, profile, contextText string, candidates []BrowserURLCandidate, observations []browserpkg.Observation, blocked []BlockedBrowserAction, round int) string {
	candidateJSON, _ := json.MarshalIndent(candidates, "", "  ")
	observationJSON, _ := json.MarshalIndent(observations, "", "  ")
	blockedJSON, _ := json.MarshalIndent(blocked, "", "  ")
	return fmt.Sprintf(`Run p2r stage G as a browser E2E planner.

Project path: %s
Artifact root: %s
Round: %d

Hard boundaries:
- Return exactly one JSON object and no prose.
- Do not ask to run shell commands.
- Do not ask to call Playwright, browser tools, or external URLs directly.
- Do not include arbitrary URLs. Use only url_id from the candidate list.
- Do not include output paths.
- Allowed actions: open_candidate, wait, snapshot, collect_console, collect_network, click_navigation, click_button, fill_input, submit_local_form, go_back, finish.
- Destructive actions are forbidden.
- Use finish only when you can provide a valid p2r.frontend_e2e.v1 summary.

Action JSON examples:
{"action":"open_candidate","url_id":"url_1","reason":"inspect the primary frontend candidate"}
{"action":"click_button","selector":"button[type=submit]","reason":"submit a local form without destructive intent"}
{"action":"finish","reason":"enough evidence collected","summary":{"schema_version":"p2r.frontend_e2e.v1","status":"passed","findings":[]}}

Finish summary schema:
{
  "schema_version": "p2r.frontend_e2e.v1",
  "status": "passed|failed|partial|blocked|not_applicable",
  "reason": "short conclusion",
  "visited_urls": [],
  "screenshots": [],
  "findings": [
    {
      "severity": "Blocker|High|Medium|Low",
      "title": "confirmed issue",
      "rule": "expected browser behavior",
      "evidence": "specific observation",
      "impact": "user-visible impact",
      "minimum_fix": "smallest fix",
      "screenshot": "optional artifact path"
    }
  ]
}

URL candidates:
%s

Previous observations:
%s

Blocked actions:
%s

Profile:
%s

Project context:
%s
`, sc.Project.Path, sc.Run.ArtifactRoot, round, string(candidateJSON), string(observationJSON), string(blockedJSON), profile, contextText)
}

func extractJSONObject(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("Codex returned empty action")
	}
	var value any
	if json.Unmarshal([]byte(raw), &value) == nil {
		return raw, nil
	}
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start < 0 || end <= start {
		return "", fmt.Errorf("Codex response did not contain a JSON object")
	}
	candidate := raw[start : end+1]
	if json.Unmarshal([]byte(candidate), &value) != nil {
		return "", fmt.Errorf("Codex response JSON object is invalid")
	}
	return candidate, nil
}
