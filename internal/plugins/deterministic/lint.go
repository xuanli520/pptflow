package deterministic

import (
	"context"
	"fmt"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/lint"
	"github.com/purplevoid/harbor-factory/internal/plugins/pluginutil"
	"github.com/purplevoid/harbor-factory/internal/workflow"
)

const (
	CodeEdgeLintPluginID = "harborfactory.codeedge_lint"
	CodeEdgeLintKind     = "harborfactory.codeedge_lint"
)

type LintFunc func(context.Context, lint.Options) (domain.LintReport, error)

type CodeEdgeLintPlugin struct {
	Run LintFunc
}

func (CodeEdgeLintPlugin) Manifest() workflow.PluginManifest {
	return workflow.PluginManifest{ID: CodeEdgeLintPluginID, Version: "1.0.0", Kinds: []string{CodeEdgeLintKind}}
}

func (CodeEdgeLintPlugin) Validate(spec workflow.NodeSpec) error {
	return pluginutil.RequiredString(spec, "task_dir")
}

func (p CodeEdgeLintPlugin) Execute(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	if req.Store == nil {
		return workflow.NodeResult{}, fmt.Errorf("codeedge_lint artifact store is required")
	}
	run := p.Run
	if run == nil {
		run = lint.Run
	}
	report, runErr := run(ctx, lint.Options{
		TaskDir:          pluginutil.String(req, "task_dir"),
		ZipPath:          pluginutil.String(req, "zip_path"),
		RepoURL:          pluginutil.String(req, "repo_url"),
		Commit:           pluginutil.String(req, "commit"),
		QwenResult:       pluginutil.String(req, "qwen_result"),
		OpusResult:       pluginutil.String(req, "opus_result"),
		QwenScreenshot:   pluginutil.String(req, "qwen_screenshot"),
		OpusScreenshot:   pluginutil.String(req, "opus_screenshot"),
		TestsAnalysis:    pluginutil.String(req, "tests_analysis"),
		StrictSubmission: pluginutil.Bool(req, "strict_submission"),
	})
	ref, storeErr := req.Store.PutJSON(ctx, pluginutil.ArtifactName(req, "phase2/artifacts/"+req.Spec.ID+"/lint_report.json"), "lint_report", req.Spec.ID, report)
	if storeErr != nil {
		return workflow.NodeResult{}, fmt.Errorf("store lint report: %w", storeErr)
	}
	result := workflow.NodeResult{Artifacts: []workflow.ArtifactRef{ref}}
	if runErr != nil {
		return result, fmt.Errorf("run CodeEdge lint: %w", runErr)
	}
	if !report.Passed && !pluginutil.Bool(req, "defer_failure_to_gate") {
		return result, workflow.NewNodeError(workflow.FailurePermanent, false, "CodeEdge lint", fmt.Errorf("report did not pass"))
	}
	return result, nil
}
