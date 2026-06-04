package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	browserpkg "github.com/xuanli520/p2r_tui/internal/browser"
	"github.com/xuanli520/p2r_tui/internal/codex"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
)

func (r Runner) nextBrowserAction(ctx context.Context, sc StageContext, promptTemplate, profile, contextText string, candidates []BrowserURLCandidate, observations []browserpkg.Observation, blocked []BlockedBrowserAction, round int, timeout time.Duration) (string, []ArtifactWarning, error) {
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
	env := browserCodexEnv(sandbox, os.Environ(), r.cfg.Codex.Env, capability.NodePath)
	extraArgs, err := safeCodexExtraArgs(r.cfg.Codex.ExtraArgs)
	if err != nil {
		return "", nil, err
	}
	logPath := filepath.Join(sc.Run.ArtifactRoot, "logs", fmt.Sprintf("G_frontend_e2e_codex_round_%02d.log", round))
	prompt, err := browserActionPrompt(promptTemplate, browserActionPromptDataForStage(sc, profile, contextText, candidates, observations, blocked, round))
	if err != nil {
		return "", nil, err
	}
	result := r.runCodexReviewWithLog(ctx, timeout, reviewPath, logPath, env, prompt, capability, extraArgs, codexDeltaProgress(sc.Run.RunID, string(model.StageG), sc.Progress))
	if result.Result.Err != nil {
		return "", result.ArtifactWarnings, result.Result.Err
	}
	actionJSON, err := extractJSONObject(result.Result.Stdout)
	return actionJSON, result.ArtifactWarnings, err
}

func browserCodexEnv(sandbox codex.Sandbox, base []string, configured map[string]string, nodePath string) []string {
	return sandbox.EnvWithNode(browserCodexBaseEnv(base), configured, nodePath)
}

func browserCodexBaseEnv(base []string) []string {
	filtered := make([]string, 0, len(base))
	for _, item := range base {
		key, _, ok := strings.Cut(item, "=")
		if !ok || !browserCodexBaseEnvAllowed(key) {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

func browserCodexBaseEnvAllowed(key string) bool {
	upper := strings.ToUpper(strings.TrimSpace(key))
	switch upper {
	case "PATH", "PATHEXT", "SYSTEMROOT", "WINDIR", "COMSPEC", "TEMP", "TMP", "TMPDIR", "LANG", "LC_ALL", "TZ", "SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS":
		return true
	default:
		return strings.HasPrefix(upper, "LC_")
	}
}

type browserActionPromptData struct {
	ProjectPath              string
	ArtifactRoot             string
	Round                    int
	URLCandidatesJSON        string
	PreviousObservationsJSON string
	BlockedActionsJSON       string
	Profile                  string
	ProjectContext           string
}

func browserActionPromptDataForStage(sc StageContext, profile, contextText string, candidates []BrowserURLCandidate, observations []browserpkg.Observation, blocked []BlockedBrowserAction, round int) browserActionPromptData {
	candidateJSON, _ := json.MarshalIndent(candidates, "", "  ")
	observationJSON, _ := json.MarshalIndent(observations, "", "  ")
	blockedJSON, _ := json.MarshalIndent(blocked, "", "  ")
	return browserActionPromptData{
		ProjectPath:              sc.Project.Path,
		ArtifactRoot:             sc.Run.ArtifactRoot,
		Round:                    round,
		URLCandidatesJSON:        string(candidateJSON),
		PreviousObservationsJSON: string(observationJSON),
		BlockedActionsJSON:       string(blockedJSON),
		Profile:                  profile,
		ProjectContext:           contextText,
	}
}

func browserActionPrompt(templateText string, data browserActionPromptData) (string, error) {
	parsed, err := template.New(stageGBrowserActionPromptTemplateName).Option("missingkey=error").Parse(templateText)
	if err != nil {
		return "", fmt.Errorf("parse Stage G browser prompt template: %w", err)
	}
	var output bytes.Buffer
	if err := parsed.Execute(&output, data); err != nil {
		return "", fmt.Errorf("render Stage G browser prompt template: %w", err)
	}
	prompt := strings.TrimSpace(output.String())
	if prompt == "" {
		return "", fmt.Errorf("Stage G browser prompt template rendered empty")
	}
	return prompt + "\n", nil
}

func extractJSONObject(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("Codex returned empty action")
	}
	if object, ok := decodeJSONObjectPrefix(raw); ok && strings.TrimSpace(raw[len(object):]) == "" {
		return object, nil
	}
	for start := strings.Index(raw, "{"); start >= 0; {
		if object, ok := decodeJSONObjectPrefix(raw[start:]); ok {
			return object, nil
		}
		next := strings.Index(raw[start+1:], "{")
		if next < 0 {
			break
		}
		start += next + 1
	}
	if strings.Contains(raw, "{") {
		return "", fmt.Errorf("Codex response JSON object is invalid")
	}
	return "", fmt.Errorf("Codex response did not contain a JSON object")
}

func decodeJSONObjectPrefix(raw string) (string, bool) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		return "", false
	}
	end := int(decoder.InputOffset())
	if end <= 0 || end > len(raw) {
		return "", false
	}
	return strings.TrimSpace(raw[:end]), true
}
