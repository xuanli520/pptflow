package app

import (
	"context"

	"github.com/xuanli520/pptflow/internal/executor"
	"github.com/xuanli520/pptflow/internal/pptflow"
	"github.com/xuanli520/pptflow/internal/runtime/codexruntime"
	commandruntime "github.com/xuanli520/pptflow/internal/runtime/command"
	"github.com/xuanli520/pptflow/internal/runtime/image2"
	"github.com/xuanli520/pptflow/internal/workflow"
	"github.com/xuanli520/pptflow/internal/workflow/builtin"
)

type Phase0Options struct {
	Scenario      string
	FixturePath   string
	TemplatePath  string
	ArtifactRoot  string
	WorkspaceRoot string
}

func RunPhase0(ctx context.Context, opts Phase0Options) (workflow.RunResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	artifactRoot := opts.ArtifactRoot
	if artifactRoot == "" {
		artifactRoot = "artifacts"
	}
	workspaceRoot := opts.WorkspaceRoot
	if workspaceRoot == "" {
		workspaceRoot = "workspace"
	}
	registry := workflow.NewRegistry()
	if err := registry.Register(builtin.CommandPlugin{}); err != nil {
		return workflow.RunResult{}, err
	}
	if err := registry.Register(builtin.AgentTurnPlugin{}); err != nil {
		return workflow.RunResult{}, err
	}
	if err := pptflow.Register(registry); err != nil {
		return workflow.RunResult{}, err
	}
	exec := executor.New()
	engine := workflow.NewEngine(registry, workflow.Runtimes{
		Command: commandruntime.New(exec),
		Agent:   codexruntime.New(exec, "", nil),
		Image:   image2.NewFromEnv(),
	})
	return engine.Run(ctx, workflow.RunRequest{
		Workflow:      pptflow.Phase0Workflow(opts.Scenario, opts.FixturePath, opts.TemplatePath),
		ArtifactRoot:  artifactRoot,
		WorkspaceRoot: workspaceRoot,
	})
}
