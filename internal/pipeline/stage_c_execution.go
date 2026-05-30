package pipeline

import (
	"strings"

	"github.com/xuanli520/p2r_tui/internal/config"
)

const (
	stageCExecutionAuto     = "auto"
	stageCExecutionHost     = "host"
	stageCExecutionIsolated = "isolated"
)

type stageCExecutionDecision struct {
	Requested string
	Selected  string
	Reason    string
	Usage     runTestsRuntimeUsage
}

func selectStageCExecution(cfg config.StageCConfig, repoPath string) stageCExecutionDecision {
	requested := strings.ToLower(strings.TrimSpace(cfg.Execution))
	if requested == "" {
		requested = stageCExecutionAuto
	}
	usage := inspectRunTestsRuntime(repoPath)
	switch requested {
	case stageCExecutionHost:
		return stageCExecutionDecision{
			Requested: requested,
			Selected:  stageCExecutionHost,
			Reason:    "pipeline.stage_c.execution=host",
			Usage:     usage,
		}
	case stageCExecutionIsolated:
		return stageCExecutionDecision{
			Requested: requested,
			Selected:  stageCExecutionIsolated,
			Reason:    "pipeline.stage_c.execution=isolated",
			Usage:     usage,
		}
	}
	if usage.UsesDocker {
		return stageCExecutionDecision{
			Requested: requested,
			Selected:  stageCExecutionHost,
			Reason:    "auto selected host because repo/run_tests.sh invokes Docker or Docker Compose",
			Usage:     usage,
		}
	}
	if usage.ReferencesRuntimePorts && strings.TrimSpace(cfg.RunnerImage) != "" {
		return stageCExecutionDecision{
			Requested: requested,
			Selected:  stageCExecutionIsolated,
			Reason:    "auto selected isolated because repo/run_tests.sh references runtime service endpoints",
			Usage:     usage,
		}
	}
	if usage.ReferencesRuntimePorts {
		return stageCExecutionDecision{
			Requested: requested,
			Selected:  stageCExecutionHost,
			Reason:    "auto selected host because isolated runner_image is not configured",
			Usage:     usage,
		}
	}
	return stageCExecutionDecision{
		Requested: requested,
		Selected:  stageCExecutionHost,
		Reason:    "auto selected host because repo/run_tests.sh appears to run local tests",
		Usage:     usage,
	}
}

func stageCNeedsRuntimePortRewrite(cfg config.StageCConfig, repoPath string) bool {
	decision := selectStageCExecution(cfg, repoPath)
	return decision.Selected == stageCExecutionIsolated || decision.Usage.StartsDockerRuntime
}

func (d stageCExecutionDecision) Summary() map[string]any {
	return map[string]any{
		"requested":                   d.Requested,
		"selected":                    d.Selected,
		"reason":                      d.Reason,
		"uses_docker":                 d.Usage.UsesDocker,
		"starts_docker_runtime":       d.Usage.StartsDockerRuntime,
		"uses_docker_compose":         d.Usage.Compose.Uses,
		"starts_docker_compose_stack": d.Usage.Compose.StartsStack,
		"explicit_compose_project":    d.Usage.Compose.ExplicitProject,
		"references_runtime_ports":    d.Usage.ReferencesRuntimePorts,
		"runtime_endpoint_hints":      append([]string{}, d.Usage.RuntimeEndpointHints...),
	}
}
