package promptflow

import "github.com/xuanli520/pptflow/internal/workflow"

type WorkflowOptions struct {
	Prompt              string
	Model               string
	ImageModel          string
	ImageSize           string
	ImageQuality        string
	ImageTimeoutSeconds int
	CodexTimeoutSeconds int
	RequireImages       bool
}

func V2Workflow(opts WorkflowOptions) workflow.WorkflowDefinition {
	model := opts.Model
	if model == "" {
		model = "gpt-5.5"
	}
	imageModel := opts.ImageModel
	if imageModel == "" {
		imageModel = "gpt-image-2"
	}
	imageSize := opts.ImageSize
	if imageSize == "" {
		imageSize = "1536x1024"
	}
	imageQuality := opts.ImageQuality
	if imageQuality == "" {
		imageQuality = "high"
	}
	imageTimeout := opts.ImageTimeoutSeconds
	if imageTimeout <= 0 {
		imageTimeout = 300
	}
	codexTimeout := opts.CodexTimeoutSeconds
	if codexTimeout <= 0 {
		codexTimeout = 300
	}

	agentConfig := func(timeout int, maxBytes int) map[string]any {
		return map[string]any{
			"model":             model,
			"timeout_seconds":   timeout,
			"max_output_bytes":  maxBytes,
			"sandbox_mode":      "read-only",
			"sandbox_policy":    "readOnly",
			"network_access":    false,
		}
	}

	imageConfig := func(timeout int) map[string]any {
		return map[string]any{
			"model":            imageModel,
			"size":             imageSize,
			"quality":          imageQuality,
			"timeout_seconds":  timeout,
			"require_images":   opts.RequireImages,
		}
	}

	return workflow.WorkflowDefinition{
		ID:     "promptflow-v2",
		Name:   "PPTflow V2 — Image2-first AI pipeline",
		Policy: workflow.Policy{MaxNodes: 32},
		Nodes: []workflow.NodeSpec{
			{
				ID: "prompt_optimize", Kind: "promptflow.prompt_optimize", Name: "Prompt Optimization (Codex)",
				Config: agentConfig(180, 1<<20),
				Policy: workflow.NodePolicy{TimeoutSeconds: 240, MaxAttempts: 2},
			},
			{
				ID: "outline_generate", Kind: "promptflow.outline_generate", Name: "Outline & Style Generation (Codex)",
				DependsOn: []string{"prompt_optimize"},
				Config:    agentConfig(360, 2<<20),
				Policy:    workflow.NodePolicy{TimeoutSeconds: 420, MaxAttempts: 2},
			},
			{
				ID: "style_reference", Kind: "promptflow.style_reference", Name: "Style Reference Image (Image2)",
				DependsOn: []string{"outline_generate"},
				Config:    imageConfig(imageTimeout),
				Policy:    workflow.NodePolicy{TimeoutSeconds: imageTimeout + 60, MaxAttempts: 2},
			},
			{
				ID: "generate_slides", Kind: "promptflow.generate_slides", Name: "Generate Slide Images (Image2)",
				DependsOn: []string{"style_reference"},
				Config:    imageConfig(imageTimeout),
				Policy:    workflow.NodePolicy{TimeoutSeconds: imageTimeout*15 + 300, MaxAttempts: 1},
			},
			{
				ID: "analyze_layout", Kind: "promptflow.analyze_layout", Name: "Analyze Slide Layouts (Codex)",
				DependsOn: []string{"generate_slides"},
				Config:    agentConfig(600, 4<<20),
				Policy:    workflow.NodePolicy{TimeoutSeconds: 720, MaxAttempts: 2},
			},
			{
				ID: "extract_resources", Kind: "promptflow.extract_resources", Name: "Extract Resources (Image2)",
				DependsOn: []string{"analyze_layout"},
				Config:    imageConfig(imageTimeout),
				Policy:    workflow.NodePolicy{TimeoutSeconds: imageTimeout*10 + 120, MaxAttempts: 2},
			},
			{
				ID: "assemble_pptx", Kind: "promptflow.assemble_pptx", Name: "Assemble PPTX (Codex workspace-write)",
				DependsOn: []string{"extract_resources"},
				Config: mergeMaps(agentConfig(600, 3<<20), map[string]any{
					"sandbox_mode":   "workspace-write",
					"sandbox_policy": "readWrite",
				}),
				Policy: workflow.NodePolicy{TimeoutSeconds: 900, MaxAttempts: 3},
			},
			{
				ID: "package_bundle", Kind: "promptflow.package", Name: "Package Delivery Bundle",
				DependsOn: []string{"assemble_pptx"},
				Policy:    workflow.NodePolicy{TimeoutSeconds: 30, MaxAttempts: 1},
			},
		},
	}
}

func mergeMaps(base map[string]any, override map[string]any) map[string]any {
	result := make(map[string]any, len(base)+len(override))
	for k, v := range base {
		result[k] = v
	}
	for k, v := range override {
		result[k] = v
	}
	return result
}
