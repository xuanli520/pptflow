package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/workflow"
)

type CommandPlugin struct{}

func (CommandPlugin) Manifest() workflow.PluginManifest {
	return workflow.PluginManifest{ID: "builtin.command", Version: "0.1.0", Kinds: []string{"command"}}
}

func (CommandPlugin) Validate(spec workflow.NodeSpec) error {
	if strings.TrimSpace(stringConfig(spec.Config, "command")) == "" {
		return fmt.Errorf("command node %s missing command", spec.ID)
	}
	return nil
}

func (CommandPlugin) Execute(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	if req.Runtimes.Command == nil {
		return workflow.NodeResult{}, fmt.Errorf("command runtime is not configured")
	}
	args := stringSliceConfig(req.Spec.Config, "args")
	result, err := req.Runtimes.Command.Run(ctx, workflow.CommandRequest{
		Command:        stringConfig(req.Spec.Config, "command"),
		Args:           args,
		Dir:            stringConfig(req.Spec.Config, "dir"),
		TimeoutSeconds: intConfig(req.Spec.Config, "timeout_seconds"),
	})
	ref, putErr := req.Store.PutJSON(ctx, req.Spec.ID+"/command_result.json", "command_result", req.Spec.ID, result)
	if putErr != nil {
		return workflow.NodeResult{}, putErr
	}
	return workflow.NodeResult{Artifacts: []workflow.ArtifactRef{ref}}, err
}

func stringConfig(config map[string]any, key string) string {
	value, _ := config[key].(string)
	return strings.TrimSpace(value)
}

func intConfig(config map[string]any, key string) int {
	switch value := config[key].(type) {
	case int:
		return value
	case float64:
		return int(value)
	default:
		return 0
	}
}

func stringSliceConfig(config map[string]any, key string) []string {
	raw, ok := config[key].([]string)
	if ok {
		return append([]string(nil), raw...)
	}
	values, _ := config[key].([]any)
	result := make([]string, 0, len(values))
	for _, value := range values {
		if text, ok := value.(string); ok {
			result = append(result, text)
		}
	}
	return result
}
