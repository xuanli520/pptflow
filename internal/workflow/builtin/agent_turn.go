package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/xuanli520/pptflow/internal/workflow"
)

type AgentTurnPlugin struct{}

func (AgentTurnPlugin) Manifest() workflow.PluginManifest {
	return workflow.PluginManifest{ID: "builtin.agent_turn", Version: "0.1.0", Kinds: []string{"agent_turn"}}
}

func (AgentTurnPlugin) Validate(spec workflow.NodeSpec) error {
	if strings.TrimSpace(stringConfig(spec.Config, "prompt")) == "" {
		return fmt.Errorf("agent_turn node %s missing prompt", spec.ID)
	}
	return nil
}

func (AgentTurnPlugin) Execute(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	if req.Runtimes.Agent == nil {
		return workflow.NodeResult{}, fmt.Errorf("agent runtime is not configured")
	}
	logPath, err := req.Store.Path(req.Spec.ID + "/codex.log")
	if err != nil {
		return workflow.NodeResult{}, err
	}
	result, err := req.Runtimes.Agent.Turn(ctx, workflow.AgentTurnRequest{
		ProjectPath:       req.WorkspaceRoot,
		Prompt:            stringConfig(req.Spec.Config, "prompt"),
		Model:             stringConfig(req.Spec.Config, "model"),
		ReasoningEffort:   stringConfig(req.Spec.Config, "reasoning_effort"),
		SandboxMode:       stringConfig(req.Spec.Config, "sandbox_mode"),
		SandboxPolicy:     stringConfig(req.Spec.Config, "sandbox_policy"),
		NetworkAccess:     boolConfig(req.Spec.Config, "network_access"),
		TimeoutSeconds:    intConfig(req.Spec.Config, "timeout_seconds"),
		MaxOutputBytes:    intConfig(req.Spec.Config, "max_output_bytes"),
		CapabilitySummary: "pptflow agent turn",
		LogPath:           logPath,
	})
	if err != nil {
		return workflow.NodeResult{}, err
	}
	output, putErr := req.Store.PutText(ctx, req.Spec.ID+"/agent_output.txt", "agent_output", req.Spec.ID, result.Text)
	if putErr != nil {
		return workflow.NodeResult{}, putErr
	}
	return workflow.NodeResult{
		Artifacts: []workflow.ArtifactRef{output},
		Metrics: workflow.NodeMetrics{
			Model:      result.Model,
			TokenUsage: result.TokenUsage,
		},
	}, nil
}

func boolConfig(config map[string]any, key string) bool {
	value, _ := config[key].(bool)
	return value
}
