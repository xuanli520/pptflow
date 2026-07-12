package verify

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/quality"
	"github.com/purplevoid/harbor-factory/internal/plugins/pluginutil"
	"github.com/purplevoid/harbor-factory/internal/workflow"
)

const (
	QualityCheckPluginID = "harborfactory.quality_check"
	QualityCheckKind     = "harborfactory.quality_check"
)

type QualityCheckPlugin struct{}

func (QualityCheckPlugin) Manifest() workflow.PluginManifest {
	return workflow.PluginManifest{ID: QualityCheckPluginID, Version: "1.0.0", Kinds: []string{QualityCheckKind}}
}

func (QualityCheckPlugin) Validate(spec workflow.NodeSpec) error {
	return pluginutil.RequiredString(spec, "task_dir")
}

func (QualityCheckPlugin) Execute(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	if req.Store == nil {
		return workflow.NodeResult{}, fmt.Errorf("quality_check artifact store is required")
	}
	proposal, err := proposalInput(ctx, req)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	agent := req.Runtimes.Agent
	if !pluginutil.Bool(req, "agent_enabled") {
		agent = nil
	}
	report, runErr := quality.Run(ctx, quality.Options{
		TaskDir:             pluginutil.String(req, "task_dir"),
		Workspace:           req.WorkspaceRoot,
		RepoURL:             pluginutil.String(req, "repo_url"),
		Commit:              pluginutil.String(req, "commit"),
		TestsAnalysisPath:   pluginutil.String(req, "tests_analysis"),
		Proposal:            proposal,
		Agent:               agent,
		Model:               pluginutil.String(req, "model"),
		ReasoningEffort:     pluginutil.String(req, "reasoning_effort"),
		AgentTimeoutSeconds: pluginutil.Int(req, "agent_timeout_seconds"),
	})
	name := pluginutil.ArtifactName(req, "phase2/artifacts/"+req.Spec.ID+"/quality_report.json")
	ref, storeErr := req.Store.PutJSON(ctx, name, "quality_report", req.Spec.ID, report)
	result := workflow.NodeResult{Artifacts: []workflow.ArtifactRef{ref}}
	if storeErr != nil {
		return workflow.NodeResult{}, fmt.Errorf("store quality report: %w", storeErr)
	}
	if runErr != nil {
		return result, fmt.Errorf("run quality check: %w", runErr)
	}
	if !report.OverallPass {
		return result, workflow.NewNodeError(workflow.FailurePermanent, false, "quality check", fmt.Errorf("report did not pass"))
	}
	return result, nil
}

func proposalInput(ctx context.Context, req workflow.NodeRequest) (*domain.TaskProposal, error) {
	for _, ref := range req.Inputs {
		if !strings.EqualFold(strings.TrimSpace(ref.Type), "task_proposal") {
			continue
		}
		reader, _, err := req.Store.Get(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("open task proposal input: %w", err)
		}
		var proposal domain.TaskProposal
		decodeErr := json.NewDecoder(reader).Decode(&proposal)
		closeErr := reader.Close()
		if decodeErr != nil {
			return nil, fmt.Errorf("decode task proposal input: %w", decodeErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close task proposal input: %w", closeErr)
		}
		return &proposal, nil
	}
	return nil, nil
}
