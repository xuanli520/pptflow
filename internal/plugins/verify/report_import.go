package verify

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/packager"
	"github.com/purplevoid/harbor-factory/internal/plugins/pluginutil"
	"github.com/purplevoid/harbor-factory/internal/workflow"
)

const (
	VerifyReportImportPluginID = "harborfactory.verify_report_import"
	VerifyReportImportKind     = "harborfactory.verify_report_import"
)

type VerifyReportImportPlugin struct{}

func (VerifyReportImportPlugin) Manifest() workflow.PluginManifest {
	return workflow.PluginManifest{ID: VerifyReportImportPluginID, Version: "1.0.0", Kinds: []string{VerifyReportImportKind}}
}

func (VerifyReportImportPlugin) Validate(spec workflow.NodeSpec) error {
	if err := pluginutil.RequiredString(spec, "task_dir"); err != nil {
		return err
	}
	return pluginutil.RequiredString(spec, "report_path")
}

func (VerifyReportImportPlugin) Execute(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	path := pluginutil.String(req, "report_path")
	if err := packager.ValidateVerifyReport(path, pluginutil.String(req, "task_dir")); err != nil {
		return workflow.NodeResult{}, workflow.NewNodeError(workflow.FailurePermanent, false, "validate reusable verification report", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	var report domain.VerifyReport
	if err := json.Unmarshal(raw, &report); err != nil {
		return workflow.NodeResult{}, fmt.Errorf("decode reusable verification report: %w", err)
	}
	ref, err := req.Store.PutJSON(ctx, pluginutil.ArtifactName(req, "phase2/artifacts/verify/verify_report.json"), "verify_report", req.Spec.ID, report)
	if err != nil {
		return workflow.NodeResult{}, fmt.Errorf("import reusable verification report: %w", err)
	}
	if req.Events != nil {
		_ = req.Events.Emit(ctx, workflow.Event{RunID: req.RunID, NodeID: req.Spec.ID, Type: "node_progress", Status: workflow.NodeRunning, Attempt: req.Attempt, Message: "reused existing Docker/oracle verification report"})
	}
	return workflow.NodeResult{Artifacts: []workflow.ArtifactRef{ref}}, nil
}
