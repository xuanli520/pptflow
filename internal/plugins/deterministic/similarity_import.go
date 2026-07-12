package deterministic

import (
	"context"
	"fmt"

	"github.com/purplevoid/harbor-factory/internal/harbor/packager"
	"github.com/purplevoid/harbor-factory/internal/plugins/pluginutil"
	"github.com/purplevoid/harbor-factory/internal/workflow"
)

const (
	SimilarityReportImportPluginID = "harborfactory.similarity_report_import"
	SimilarityReportImportKind     = "harborfactory.similarity_report_import"
)

type SimilarityReportImportPlugin struct{}

func (SimilarityReportImportPlugin) Manifest() workflow.PluginManifest {
	return workflow.PluginManifest{ID: SimilarityReportImportPluginID, Version: "1.0.0", Kinds: []string{SimilarityReportImportKind}}
}

func (SimilarityReportImportPlugin) Validate(spec workflow.NodeSpec) error {
	if err := pluginutil.RequiredString(spec, "task_dir"); err != nil {
		return err
	}
	return pluginutil.RequiredString(spec, "report_path")
}

func (SimilarityReportImportPlugin) Execute(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	report, err := packager.ValidateSimilarityReport(pluginutil.String(req, "report_path"), pluginutil.String(req, "task_dir"))
	if err != nil {
		return workflow.NodeResult{}, workflow.NewNodeError(workflow.FailurePermanent, false, "validate reusable similarity report", err)
	}
	ref, err := req.Store.PutJSON(ctx, pluginutil.ArtifactName(req, "phase2/artifacts/similarity_check/similarity_report.json"), "similarity_report", req.Spec.ID, report)
	if err != nil {
		return workflow.NodeResult{}, fmt.Errorf("import reusable similarity report: %w", err)
	}
	if req.Events != nil {
		_ = req.Events.Emit(ctx, workflow.Event{RunID: req.RunID, NodeID: req.Spec.ID, Type: "node_progress", Status: workflow.NodeRunning, Attempt: req.Attempt, Message: "reused existing similarity report"})
	}
	return workflow.NodeResult{Artifacts: []workflow.ArtifactRef{ref}}, nil
}
