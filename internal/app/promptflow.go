package app

import (
	"context"

	"github.com/xuanli520/pptflow/internal/executor"
	"github.com/xuanli520/pptflow/internal/promptflow"
	"github.com/xuanli520/pptflow/internal/runtime/codexruntime"
	commandruntime "github.com/xuanli520/pptflow/internal/runtime/command"
	"github.com/xuanli520/pptflow/internal/runtime/image2"
	"github.com/xuanli520/pptflow/internal/workflow"
	"github.com/xuanli520/pptflow/internal/workflow/builtin"
)

type PromptFlowOptions struct {
	Prompt              string
	Model               string
	ImageModel          string
	ImageSize           string
	ImageQuality        string
	ArtifactRoot        string
	WorkspaceRoot       string
	ImageTimeoutSeconds int
	CodexTimeoutSeconds int
	RequireImages       bool
	QualityMode         string
	FallbackPolicy      string
}

func RunPromptFlow(ctx context.Context, opts PromptFlowOptions) (workflow.RunResult, error) {
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
	if err := promptflow.Register(registry); err != nil {
		return workflow.RunResult{}, err
	}

	exec := executor.New()
	engine := workflow.NewEngine(registry, workflow.Runtimes{
		Command: commandruntime.New(exec),
		Agent:   codexruntime.New(exec, "", nil),
		Image:   image2.NewFromEnv(),
	})

	return engine.Run(ctx, workflow.RunRequest{
		Workflow: promptflow.V2Workflow(promptflow.WorkflowOptions{
			Prompt:              opts.Prompt,
			Model:               opts.Model,
			ImageModel:          opts.ImageModel,
			ImageSize:           opts.ImageSize,
			ImageQuality:        opts.ImageQuality,
			ImageTimeoutSeconds: opts.ImageTimeoutSeconds,
			CodexTimeoutSeconds: opts.CodexTimeoutSeconds,
			RequireImages:       opts.RequireImages,
			Profile: promptflow.RunProfile{
				QualityMode:    opts.QualityMode,
				FallbackPolicy: opts.FallbackPolicy,
			},
		}),
		ArtifactRoot:  artifactRoot,
		WorkspaceRoot: workspaceRoot,
		Input: map[string]any{
			"prompt": opts.Prompt,
		},
	})
}
