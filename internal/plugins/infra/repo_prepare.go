package infra

import (
	"context"
	"fmt"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/repoprep"
	"github.com/purplevoid/harbor-factory/internal/plugins/pluginutil"
	"github.com/purplevoid/harbor-factory/internal/workflow"
)

const (
	RepoPreparePluginID = "harborfactory.repo_prepare"
	RepoPrepareKind     = "harborfactory.repo_prepare"
)

type RepoPrepareFunc func(context.Context, repoprep.Options) (domain.RepoPrepared, error)

type RepoPreparePlugin struct {
	Prepare RepoPrepareFunc
}

func (RepoPreparePlugin) Manifest() workflow.PluginManifest {
	return workflow.PluginManifest{ID: RepoPreparePluginID, Version: "1.0.0", Kinds: []string{RepoPrepareKind}}
}

func (RepoPreparePlugin) Validate(spec workflow.NodeSpec) error {
	if err := pluginutil.RequiredString(spec, "repo_url"); err != nil {
		return err
	}
	return pluginutil.RequiredString(spec, "commit")
}

func (p RepoPreparePlugin) Execute(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	if req.Store == nil {
		return workflow.NodeResult{}, fmt.Errorf("repo_prepare artifact store is required")
	}
	prepare := p.Prepare
	if prepare == nil {
		prepare = repoprep.Prepare
	}
	prepared, err := prepare(ctx, repoprep.Options{
		RepoURL:            pluginutil.String(req, "repo_url"),
		Commit:             pluginutil.String(req, "commit"),
		Workspace:          req.WorkspaceRoot,
		AllowLocal:         pluginutil.Bool(req, "allow_local_repo"),
		MaxNetworkAttempts: pluginutil.Int(req, "max_network_attempts"),
		RetryDelay:         time.Duration(pluginutil.Int(req, "retry_delay_ms")) * time.Millisecond,
	})
	if err != nil {
		return workflow.NodeResult{}, fmt.Errorf("prepare repository: %w", err)
	}
	ref, err := req.Store.PutJSON(ctx, pluginutil.ArtifactName(req, "phase0/repo_prepared.json"), "repo_prepared", req.Spec.ID, prepared)
	if err != nil {
		return workflow.NodeResult{}, fmt.Errorf("store repo_prepare artifact: %w", err)
	}
	return workflow.NodeResult{Artifacts: []workflow.ArtifactRef{ref}}, nil
}
