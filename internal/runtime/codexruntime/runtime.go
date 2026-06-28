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
	sandbox, err := codex.NewSandbox(projectPath, filepath.Dir(logPath), "pptflow")
	if err == nil {
		env = sandbox.Env(env, r.env)
	} else {
		env = mergeEnv(env, r.env)
	}
	session := appserver.New(envKeys(env))
	if err := session.Start(ctx, appserver.Request{
		Timeout:           timeout,
		ProjectPath:       projectPath,
		LogPath:           logPath,
		Env:               env,
		Prompt:            req.Prompt,
		CommandPath:       capability.Path,
		CapabilitySummary: capability.Version,
		HasAppServer:      capability.HasAppServer,
		Model:             req.Model,
		SandboxMode:       sandboxMode,
		SandboxPolicy:     sandboxPolicy,
		NetworkAccess:     req.NetworkAccess,
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
