package deterministic

import (
	"context"
	"fmt"
	"net/http"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/similarity"
	"github.com/purplevoid/harbor-factory/internal/plugins/pluginutil"
	"github.com/purplevoid/harbor-factory/internal/workflow"
)

const (
	SimilarityPluginID = "harborfactory.similarity_check"
	SimilarityKind     = "harborfactory.similarity_check"
)

type SimilarityFunc func(context.Context, similarity.Options) (domain.SimilarityReport, error)

type SimilarityPlugin struct {
	Run        SimilarityFunc
	HTTPClient *http.Client
}

func (SimilarityPlugin) Manifest() workflow.PluginManifest {
	return workflow.PluginManifest{ID: SimilarityPluginID, Version: "1.0.0", Kinds: []string{SimilarityKind}}
}

func (SimilarityPlugin) Validate(spec workflow.NodeSpec) error {
	if err := pluginutil.RequiredString(spec, "task_dir"); err != nil {
		return err
	}
	if enabled, _ := spec.Config["enable_github"].(bool); enabled {
		return pluginutil.RequiredString(spec, "repo_url")
	}
	history, historyOK := spec.Config["history_dirs"]
	tb3, tb3OK := spec.Config["tb3_dirs"]
	if (!historyOK || lenStringConfig(history) == 0) && (!tb3OK || lenStringConfig(tb3) == 0) {
		return fmt.Errorf("%s node %s requires at least one GitHub, history, or TB3 similarity source", spec.Kind, spec.ID)
	}
	return nil
}

func (p SimilarityPlugin) Execute(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	if req.Store == nil {
		return workflow.NodeResult{}, fmt.Errorf("similarity_check artifact store is required")
	}
	run := p.Run
	if run == nil {
		run = similarity.Run
	}
	report, runErr := run(ctx, similarity.Options{
		TaskDir:           pluginutil.String(req, "task_dir"),
		RepoURL:           pluginutil.String(req, "repo_url"),
		TestsAnalysisPath: pluginutil.String(req, "tests_analysis"),
		HistoryDirs:       pluginutil.Strings(req, "history_dirs"),
		TB3Dirs:           pluginutil.Strings(req, "tb3_dirs"),
		EnableGitHub:      pluginutil.Bool(req, "enable_github"),
		GitHubToken:       pluginutil.String(req, "github_token"),
		GitHubBaseURL:     pluginutil.String(req, "github_base_url"),
		HTTPClient:        p.HTTPClient,
		Threshold:         pluginutil.Float(req, "threshold"),
		StrictSources:     true,
	})
	ref, storeErr := req.Store.PutJSON(ctx, pluginutil.ArtifactName(req, "phase2/artifacts/"+req.Spec.ID+"/similarity_report.json"), "similarity_report", req.Spec.ID, report)
	if storeErr != nil {
		return workflow.NodeResult{}, fmt.Errorf("store similarity report: %w", storeErr)
	}
	result := workflow.NodeResult{Artifacts: []workflow.ArtifactRef{ref}}
	if runErr != nil {
		return result, fmt.Errorf("run similarity check: %w", runErr)
	}
	if !report.OverallPass || len(report.SuccessfulSources) == 0 {
		return result, workflow.NewNodeError(workflow.FailurePermanent, false, "similarity check", fmt.Errorf("report failed or scanned no source successfully"))
	}
	return result, nil
}

func lenStringConfig(value any) int {
	switch typed := value.(type) {
	case []string:
		return len(typed)
	case []any:
		return len(typed)
	case string:
		if typed != "" {
			return 1
		}
	}
	return 0
}
