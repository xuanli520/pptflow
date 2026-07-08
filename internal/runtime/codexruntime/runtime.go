package codexruntime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xuanli520/pptflow/internal/codex"
	"github.com/xuanli520/pptflow/internal/codex/appserver"
	"github.com/xuanli520/pptflow/internal/executor"
	"github.com/xuanli520/pptflow/internal/workflow"
)

type Runtime struct {
	exec          executor.CommandRunner
	preferredPath string
	env           map[string]string
}

func New(exec executor.CommandRunner, preferredPath string, env map[string]string) Runtime {
	if exec == nil {
		exec = executor.New()
	}
	return Runtime{exec: exec, preferredPath: strings.TrimSpace(preferredPath), env: copyEnv(env)}
}

func (r Runtime) Turn(ctx context.Context, req workflow.AgentTurnRequest) (workflow.AgentTurnResult, error) {
	capability := codex.DetectCLI(ctx, r.exec, r.preferredPath)
	if err := codex.ValidateAppServerCapability(capability); err != nil {
		return workflow.AgentTurnResult{}, err
	}
	projectPath := strings.TrimSpace(req.ProjectPath)
	if projectPath == "" {
		projectPath = "."
	}
	logPath := strings.TrimSpace(req.LogPath)
	if logPath == "" {
		logPath = filepath.Join(projectPath, ".pptflow-codex.log")
	}
	timeout := time.Duration(req.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	maxOutputBytes := req.MaxOutputBytes
	if maxOutputBytes <= 0 {
		maxOutputBytes = 3 << 20
	}
	sandboxMode := strings.TrimSpace(req.SandboxMode)
	if sandboxMode == "" {
		sandboxMode = "read-only"
	}
	sandboxPolicy := strings.TrimSpace(req.SandboxPolicy)
	if sandboxPolicy == "" {
		sandboxPolicy = "readOnly"
	}
	env := os.Environ()
	if capability.NodePath != "" {
		env = codex.WithNodeOnPATH(env, capability.NodePath)
	}
	configuredEnv := copyEnv(r.env)
	cleanupCodexHome := ""
	sandbox, err := codex.NewSandbox(projectPath, filepath.Dir(logPath), fmt.Sprintf("pptflow-%d", time.Now().UnixNano()))
	if err != nil {
		return workflow.AgentTurnResult{}, err
	}
	if !hasConfiguredEnv(configuredEnv, "CODEX_HOME") {
		if err := prepareAutomationCodexHome(sandbox.Home, projectPath); err != nil {
			return workflow.AgentTurnResult{}, err
		}
		if configuredEnv == nil {
			configuredEnv = map[string]string{}
		}
		configuredEnv["CODEX_HOME"] = sandbox.Home
		cleanupCodexHome = sandbox.Home
	}
	env = sandbox.Env(env, configuredEnv)
	if cleanupCodexHome != "" {
		defer func() { _ = os.RemoveAll(cleanupCodexHome) }()
	}
	session := appserver.New(envKeys(env))
	if err := session.Start(ctx, appserver.Request{
		Timeout:           timeout,
		ProjectPath:       projectPath,
		LogPath:           logPath,
		Env:               env,
		Prompt:            req.Prompt,
		Input:             appServerInput(req.Input),
		CommandPath:       capability.Path,
		CapabilitySummary: capability.Version,
		HasAppServer:      capability.HasAppServer,
		Model:             req.Model,
		ReasoningEffort:   req.ReasoningEffort,
		SandboxMode:       sandboxMode,
		SandboxPolicy:     sandboxPolicy,
		NetworkAccess:     req.NetworkAccess,
		WorkspaceRoots:    workspaceRoots(projectPath, req.WorkspaceRoots),
		MaxOutputBytes:    maxOutputBytes,
	}); err != nil {
		result, _ := session.Wait(context.Background())
		if result.Result.Stderr != "" {
			return workflow.AgentTurnResult{}, fmt.Errorf("%w: %s", err, result.Result.Stderr)
		}
		return workflow.AgentTurnResult{}, err
	}
	result, err := session.Wait(ctx)
	if err != nil {
		return workflow.AgentTurnResult{}, err
	}
	warnings := make([]string, 0, len(result.Warnings))
	for _, warning := range result.Warnings {
		if !warning.OK() {
			warnings = append(warnings, warning.Error)
		}
	}
	return workflow.AgentTurnResult{Text: result.Result.Stdout, Model: req.Model, Warnings: warnings}, nil
}

func appServerInput(input []workflow.AgentInputPart) []appserver.InputPart {
	if len(input) == 0 {
		return nil
	}
	result := make([]appserver.InputPart, 0, len(input))
	for _, part := range input {
		result = append(result, appserver.InputPart{
			Type:   part.Type,
			Text:   part.Text,
			URL:    part.URL,
			Path:   part.Path,
			Detail: part.Detail,
		})
	}
	return result
}

func workspaceRoots(projectPath string, roots []string) []string {
	seen := map[string]bool{}
	result := []string{}
	add := func(root string) {
		root = strings.TrimSpace(root)
		if root == "" {
			return
		}
		abs, err := filepath.Abs(root)
		if err != nil {
			return
		}
		clean := filepath.Clean(abs)
		if !seen[clean] {
			seen[clean] = true
			result = append(result, clean)
		}
	}
	add(projectPath)
	for _, root := range roots {
		add(root)
	}
	return result
}

func copyEnv(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func hasConfiguredEnv(env map[string]string, key string) bool {
	for item := range env {
		if strings.EqualFold(item, key) {
			return true
		}
	}
	return false
}

func prepareAutomationCodexHome(home, projectPath string) error {
	if err := os.MkdirAll(home, 0o755); err != nil {
		return err
	}
	source := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if source == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		source = filepath.Join(userHome, ".codex")
	}
	if err := writeAutomationCodexConfig(source, home, projectPath); err != nil {
		return err
	}
	if err := copyCodexHomeFile(source, home, "auth.json"); err != nil {
		return err
	}
	return nil
}

func writeAutomationCodexConfig(sourceHome, targetHome, projectPath string) error {
	sourcePath := filepath.Join(sourceHome, "config.toml")
	data, _ := os.ReadFile(sourcePath)
	source := string(data)
	provider := topLevelConfigString(source, "model_provider")
	model := topLevelConfigString(source, "model")
	if provider == "" {
		provider = "custom"
	}
	if model == "" {
		model = "gpt-5.5"
	}
	providerBlock := extractTomlTableBlock(source, "model_providers."+provider)
	if strings.TrimSpace(providerBlock) == "" && provider == "custom" {
		providerBlock = `[model_providers.custom]
name = "custom"
wire_api = "responses"
requires_openai_auth = true
base_url = "https://new-api.metalics.cn/v1"
`
	}
	config := strings.Join([]string{
		fmt.Sprintf("model_provider = %q", provider),
		fmt.Sprintf("model = %q", model),
		"disable_response_storage = true",
		"",
		strings.TrimSpace(providerBlock),
		"",
		fmt.Sprintf("[projects.%q]", filepath.Clean(projectPath)),
		`trust_level = "trusted"`,
		"",
	}, "\n")
	return os.WriteFile(filepath.Join(targetHome, "config.toml"), []byte(config), 0o600)
}

func topLevelConfigString(config, key string) string {
	for _, line := range strings.Split(config, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}
		left, right, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(left) != key {
			continue
		}
		value := strings.TrimSpace(strings.SplitN(right, "#", 2)[0])
		value = strings.Trim(value, `"'`)
		return strings.TrimSpace(value)
	}
	return ""
}

func extractTomlTableBlock(config, table string) string {
	header := "[" + table + "]"
	lines := strings.Split(config, "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == header {
			start = i
			break
		}
	}
	if start < 0 {
		return ""
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			end = i
			break
		}
	}
	return strings.TrimSpace(strings.Join(lines[start:end], "\n"))
}

func copyCodexHomeFile(sourceHome, targetHome, name string) error {
	sourcePath := filepath.Join(sourceHome, name)
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return os.WriteFile(filepath.Join(targetHome, name), data, 0o600)
}

func mergeEnv(base []string, configured map[string]string) []string {
	if len(configured) == 0 {
		return base
	}
	result := append([]string{}, base...)
	for key, value := range configured {
		result = append(result, key+"="+value)
	}
	return result
}

func envKeys(env []string) []string {
	keys := make([]string, 0, len(env))
	for _, item := range env {
		key, _, ok := strings.Cut(item, "=")
		if ok {
			keys = append(keys, key)
		}
	}
	return keys
}
