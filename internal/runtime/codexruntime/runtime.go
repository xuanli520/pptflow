// Package codexruntime adapts the Codex App Server to the general agent
// conversation port.  It owns only process/session mechanics; callers own
// operation policy, provider selection, prompt contracts, and credentials.
package codexruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/purplevoid/harbor-factory/internal/agent"
	"github.com/purplevoid/harbor-factory/internal/codex"
	"github.com/purplevoid/harbor-factory/internal/codex/appserver"
	"github.com/purplevoid/harbor-factory/internal/executor"
)

// Runtime is a concrete implementation of agent.Runtime for an explicitly
// configured Codex App Server executable.  It deliberately does not discover
// an executable, copy an ambient CODEX_HOME, or select a model/provider.
// Those facts must be supplied by the composition that owns the operation.
type Runtime struct {
	exec        executor.CommandRunner
	commandPath string
	env         map[string]string
}

// New constructs a runtime from explicit process inputs.  OpenConversation
// verifies that commandPath is an absolute executable path and that env
// contains an explicit, existing CODEX_HOME.  The supplied environment is
// copied so later caller mutation cannot alter a live runtime.
func New(exec executor.CommandRunner, commandPath string, env map[string]string) Runtime {
	if exec == nil {
		exec = executor.New()
	}
	return Runtime{exec: exec, commandPath: strings.TrimSpace(commandPath), env: copyEnv(env)}
}

type conversation struct {
	session   appserver.Session
	defaults  agent.ConversationRequest
	model     string
	closeOnce sync.Once
	closeErr  error
}

func (r Runtime) OpenConversation(ctx context.Context, req agent.ConversationRequest) (agent.Conversation, error) {
	configuredEnv := copyEnv(r.env)
	if err := validateExplicitCodexEnvironment(configuredEnv); err != nil {
		return nil, err
	}
	// Probe the approved executable with the same sanitized environment that
	// will be used for the App Server itself.  This keeps version/help probing
	// from consulting an ambient home, credential, endpoint, or PATH mutation.
	env := codex.SanitizeEnvironment(os.Environ(), configuredEnv)
	capability := codex.InspectCLIWithEnvironment(ctx, r.exec, r.commandPath, env)
	if err := codex.ValidateControlledAppServerCapability(capability); err != nil {
		return nil, err
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		return nil, fmt.Errorf("codex agent runtime requires an explicit model")
	}
	projectPath, err := absoluteConfiguredPath(req.ProjectPath, "project path")
	if err != nil {
		return nil, err
	}
	logPath := strings.TrimSpace(req.LogPath)
	if logPath == "" {
		logPath = filepath.Join(projectPath, ".codex-app-server.log")
	} else if logPath, err = filepath.Abs(logPath); err != nil {
		return nil, fmt.Errorf("resolve Codex log path: %w", err)
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

	// The base environment is intentionally sanitized.  In particular, an
	// ambient CODEX_HOME, API key, or provider endpoint cannot silently become
	// part of this operation; only explicitly supplied values survive.
	session := appserver.New(envKeys(env))
	if err := session.Start(ctx, appserver.Request{
		ClientName:        "codex-agent-runtime",
		ClientVersion:     "1",
		ProjectPath:       projectPath,
		LogPath:           logPath,
		Env:               env,
		CommandPath:       capability.Path,
		CapabilitySummary: capability.Version,
		HasAppServer:      capability.HasAppServer,
		Model:             model,
		ReasoningEffort:   req.ReasoningEffort,
		SandboxMode:       sandboxMode,
		SandboxPolicy:     sandboxPolicy,
		NetworkAccess:     req.NetworkAccess,
		WorkspaceRoots:    workspaceRoots(projectPath, req.WorkspaceRoots),
		MaxOutputBytes:    maxOutputBytes,
		DynamicTools:      appServerDynamicTools(req.DynamicTools),
	}); err != nil {
		_ = session.Close()
		return nil, err
	}
	defaults := req
	defaults.ProjectPath = projectPath
	defaults.Model = model
	defaults.LogPath = logPath
	defaults.SandboxMode = sandboxMode
	defaults.SandboxPolicy = sandboxPolicy
	defaults.MaxOutputBytes = maxOutputBytes
	defaults.WorkspaceRoots = workspaceRoots(projectPath, req.WorkspaceRoots)
	return &conversation{session: session, defaults: defaults, model: model}, nil
}

func (c *conversation) Turn(ctx context.Context, req agent.TurnRequest) (agent.TurnResult, error) {
	return c.turn(ctx, req, nil)
}

// TurnStream performs one Codex turn and forwards its best-effort App Server
// updates through the generic Agent streaming capability.
func (c *conversation) TurnStream(ctx context.Context, req agent.TurnRequest, onUpdate agent.TurnUpdateHandler) (agent.TurnResult, error) {
	return c.turn(ctx, req, onUpdate)
}

func (c *conversation) turn(ctx context.Context, req agent.TurnRequest, onUpdate agent.TurnUpdateHandler) (agent.TurnResult, error) {
	if c == nil || c.session == nil {
		return agent.TurnResult{}, fmt.Errorf("codex conversation is not open")
	}
	if model := strings.TrimSpace(req.Model); model != "" && model != c.model {
		return agent.TurnResult{}, fmt.Errorf("codex conversation model cannot change from %q to %q", c.model, model)
	}
	timeoutSeconds := req.TimeoutSeconds
	if timeoutSeconds <= 0 {
		timeoutSeconds = c.defaults.TimeoutSeconds
	}
	timeout := time.Duration(timeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	maxOutputBytes := req.MaxOutputBytes
	if maxOutputBytes <= 0 {
		maxOutputBytes = c.defaults.MaxOutputBytes
	}
	logPath := strings.TrimSpace(req.LogPath)
	if logPath == "" {
		logPath = c.defaults.LogPath
	}
	turnRequest := appserver.TurnRequest{
		Timeout:        timeout,
		Prompt:         req.Prompt,
		Input:          appServerInput(req.Input),
		LogPath:        logPath,
		MaxOutputBytes: maxOutputBytes,
		OutputSchema:   append(json.RawMessage(nil), req.OutputSchema...),
	}
	if onUpdate != nil {
		turnRequest.OnDelta = func(update appserver.Update) {
			onUpdate(agent.TurnUpdate{
				TurnID:    update.TurnID,
				ItemID:    update.ItemID,
				Delta:     update.Delta,
				Text:      update.Text,
				Done:      update.Done,
				Truncated: update.Truncated,
			})
		}
	}
	result, err := c.session.Turn(ctx, turnRequest)
	if err != nil {
		return agent.TurnResult{}, err
	}
	warnings := make([]string, 0, len(result.Warnings))
	for _, warning := range result.Warnings {
		if !warning.OK() {
			warnings = append(warnings, warning.Error)
		}
	}
	return agent.TurnResult{Text: result.Result.Stdout, Model: c.model, Warnings: warnings}, nil
}

func appServerDynamicTools(tools []agent.DynamicTool) []appserver.DynamicTool {
	if len(tools) == 0 {
		return nil
	}
	result := make([]appserver.DynamicTool, 0, len(tools))
	for _, tool := range tools {
		result = append(result, appserver.DynamicTool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: append(json.RawMessage(nil), tool.InputSchema...),
			Handler:     appserver.DynamicToolHandler(tool.Handler),
		})
	}
	return result
}

// Steer forwards live caller guidance to the active Codex App Server turn.
// The App Server enforces that a turn is active before accepting it.
func (c *conversation) Steer(ctx context.Context, guidance string) error {
	if c == nil || c.session == nil {
		return fmt.Errorf("codex conversation is not open")
	}
	return c.session.SendGuidance(ctx, guidance)
}

func (c *conversation) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		if c.session != nil {
			c.closeErr = c.session.Close()
		}
	})
	return c.closeErr
}

func appServerInput(input []agent.InputPart) []appserver.InputPart {
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

func validateExplicitCodexEnvironment(env map[string]string) error {
	home, ok := configuredEnvValue(env, "CODEX_HOME")
	if !ok || strings.TrimSpace(home) == "" {
		return fmt.Errorf("codex agent runtime requires an explicit CODEX_HOME in its configured environment")
	}
	if !filepath.IsAbs(home) {
		return fmt.Errorf("codex agent runtime CODEX_HOME must be an absolute path")
	}
	info, err := os.Stat(home)
	if err != nil {
		return fmt.Errorf("stat configured CODEX_HOME: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("configured CODEX_HOME is not a directory: %s", home)
	}
	return nil
}

func configuredEnvValue(env map[string]string, key string) (string, bool) {
	value, ok := env[key]
	return strings.TrimSpace(value), ok
}

func absoluteConfiguredPath(value, label string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("codex agent runtime requires an explicit %s", label)
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", label, err)
	}
	return filepath.Clean(abs), nil
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

var (
	_ agent.Runtime               = Runtime{}
	_ agent.Conversation          = (*conversation)(nil)
	_ agent.StreamingConversation = (*conversation)(nil)
	_ agent.SteerableConversation = (*conversation)(nil)
)
