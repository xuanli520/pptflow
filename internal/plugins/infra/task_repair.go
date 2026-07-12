package infra

import (
	"context"
	"fmt"

	"github.com/purplevoid/harbor-factory/internal/harbor/repair"
	"github.com/purplevoid/harbor-factory/internal/plugins/pluginutil"
	"github.com/purplevoid/harbor-factory/internal/workflow"
)

const (
	TaskRepairPluginID = "harborfactory.task_repair"
	TaskRepairKind     = "harborfactory.task_repair"
)

type TaskRepairPlugin struct {
	Run func(context.Context, repair.Options) (repair.Report, error)
}

func (TaskRepairPlugin) Manifest() workflow.PluginManifest {
	return workflow.PluginManifest{ID: TaskRepairPluginID, Version: "1.0.0", Kinds: []string{TaskRepairKind}}
}

func (TaskRepairPlugin) Validate(spec workflow.NodeSpec) error {
	if err := pluginutil.RequiredString(spec, "task_dir"); err != nil {
		return err
	}
	return pluginutil.RequiredString(spec, "guidance")
}

func (p TaskRepairPlugin) Execute(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	if req.Store == nil {
		return workflow.NodeResult{}, fmt.Errorf("task_repair artifact store is required")
	}
	if req.Runtimes.Agent == nil {
		return workflow.NodeResult{}, fmt.Errorf("task_repair agent runtime is required")
	}
	run := p.Run
	if run == nil {
		run = repair.Run
	}
	logName := pluginutil.String(req, "log_artifact_name")
	if logName == "" {
		logName = "phase2/artifacts/task_repair/external/repair-agent.log"
	}
	logPath, err := req.Store.Path(logName)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	report, err := run(ctx, repair.Options{
		TaskDir: pluginutil.String(req, "task_dir"), Guidance: pluginutil.String(req, "guidance"),
		Source: pluginutil.String(req, "source"), Round: max(req.Revision+1, 1), Agent: req.Runtimes.Agent,
		Model: pluginutil.String(req, "model"), ReasoningEffort: pluginutil.String(req, "reasoning_effort"),
		TimeoutSeconds: pluginutil.Int(req, "agent_timeout_seconds"), LogPath: logPath,
	})
	if err != nil {
		return workflow.NodeResult{}, err
	}
	ref, err := req.Store.PutJSON(ctx, pluginutil.ArtifactName(req, "phase2/artifacts/task_repair/external/repair.json"), "task_repair_report", req.Spec.ID, report)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	return workflow.NodeResult{Artifacts: []workflow.ArtifactRef{ref}}, nil
}
