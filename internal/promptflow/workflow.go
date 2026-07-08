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
	Profile             RunProfile
}

type RunProfile struct {
	QualityMode    string
	FallbackPolicy string
	QAThresholds   QAThresholds
}

type QAThresholds struct {
	VisibleTextEditability float64
	GeometricEditability   float64
	ShapeStyleCoverage     float64
	TransparentCoverage    float64
}

func V2Workflow(opts WorkflowOptions) workflow.WorkflowDefinition {
	profile := normalizeRunProfile(opts.Profile)
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
		if codexTimeout > 0 {
			timeout = codexTimeout
		}
		return map[string]any{
			"model":            model,
			"reasoning_effort": "medium",
			"timeout_seconds":  timeout,
			"max_output_bytes": maxBytes,
			"fallback_policy":  profile.FallbackPolicy,
			"quality_mode":     profile.QualityMode,
			"sandbox_mode":     "read-only",
			"sandbox_policy":   "readOnly",
			"network_access":   false,
		}
	}

	imageConfig := func(timeout int) map[string]any {
		return map[string]any{
			"model":           imageModel,
			"size":            imageSize,
			"quality":         imageQuality,
			"timeout_seconds": timeout,
			"require_images":  opts.RequireImages,
			"fallback_policy": profile.FallbackPolicy,
		}
	}

	return workflow.WorkflowDefinition{
		ID:     "promptflow-v2",
		Name:   "PPTflow V2 — Image2-first AI pipeline",
		Policy: workflow.Policy{MaxNodes: 48},
		Nodes: []workflow.NodeSpec{
			{
				ID: "prompt_optimize", Kind: "promptflow.prompt_optimize", Name: "Prompt Optimization (Codex)",
				Config: agentConfig(180, 1<<20),
				Policy: workflow.NodePolicy{TimeoutSeconds: codexTimeout + 60, MaxAttempts: 2},
			},
			{
				ID: "outline_generate", Kind: "promptflow.outline_generate", Name: "Outline & Style Generation (Codex)",
				DependsOn: []string{"prompt_optimize"},
				Config:    agentConfig(360, 2<<20),
				Policy:    workflow.NodePolicy{TimeoutSeconds: codexTimeout + 60, MaxAttempts: 2},
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
				Config: mergeMaps(agentConfig(600, 4<<20), map[string]any{
					"reasoning_effort": "low",
					"sandbox_mode":     "workspace-write",
					"sandbox_policy":   "workspaceWrite",
				}),
				Policy: workflow.NodePolicy{TimeoutSeconds: codexTimeout + 60, MaxAttempts: 2},
			},
			{
				ID: "extract_resources", Kind: "promptflow.extract_resources", Name: "Extract Resources (Image2)",
				DependsOn: []string{"analyze_layout"},
				Config:    imageConfig(imageTimeout),
				Policy:    workflow.NodePolicy{TimeoutSeconds: imageTimeout*10 + 120, MaxAttempts: 2},
			},
			{
				ID: "assemble_pptx", Kind: "promptflow.assemble_pptx", Name: "Assemble PPTX (local OOXML)",
				DependsOn: []string{"extract_resources"},
				Config:    map[string]any{"fallback_policy": profile.FallbackPolicy},
				Policy:    workflow.NodePolicy{TimeoutSeconds: 120, MaxAttempts: 1},
			},
			{
				ID: "render_pptx_preview", Kind: "promptflow.render_pptx_preview", Name: "Render PPTX Preview",
				DependsOn: []string{"assemble_pptx"},
				Config:    map[string]any{"fallback_policy": profile.FallbackPolicy},
				Policy:    workflow.NodePolicy{TimeoutSeconds: 180, MaxAttempts: 1},
			},
			{
				ID: "visual_qa", Kind: "promptflow.visual_qa", Name: "Visual QA",
				DependsOn: []string{"render_pptx_preview"},
				Config: map[string]any{
					"fallback_policy":               profile.FallbackPolicy,
					"visible_text_editability":      profile.QAThresholds.VisibleTextEditability,
					"geometric_editability":         profile.QAThresholds.GeometricEditability,
					"shape_style_coverage":          profile.QAThresholds.ShapeStyleCoverage,
					"transparent_resource_coverage": profile.QAThresholds.TransparentCoverage,
				},
				Policy: workflow.NodePolicy{TimeoutSeconds: 60, MaxAttempts: 1},
			},
			{
				ID: "repair_plan", Kind: "promptflow.repair_plan", Name: "Repair Plan",
				DependsOn: []string{"visual_qa"},
				Policy:    workflow.NodePolicy{TimeoutSeconds: 30, MaxAttempts: 1},
			},
			{
				ID: "repair_apply", Kind: "promptflow.repair_apply", Name: "Repair Apply",
				DependsOn: []string{"repair_plan"},
				Config:    map[string]any{"fallback_policy": profile.FallbackPolicy},
				Policy:    workflow.NodePolicy{TimeoutSeconds: 30, MaxAttempts: 1},
			},
			{
				ID: "package_bundle", Kind: "promptflow.package", Name: "Package Delivery Bundle",
				DependsOn: []string{"repair_apply"},
				Policy:    workflow.NodePolicy{TimeoutSeconds: 30, MaxAttempts: 1},
			},
		},
	}
}

func normalizeRunProfile(profile RunProfile) RunProfile {
	if profile.QualityMode == "" {
		profile.QualityMode = "production"
	}
	if profile.FallbackPolicy == "" {
		profile.FallbackPolicy = "strict"
	}
	if profile.QAThresholds.VisibleTextEditability <= 0 {
		profile.QAThresholds.VisibleTextEditability = 0.95
	}
	if profile.QAThresholds.GeometricEditability <= 0 {
		profile.QAThresholds.GeometricEditability = 0.90
	}
	if profile.QAThresholds.ShapeStyleCoverage <= 0 {
		profile.QAThresholds.ShapeStyleCoverage = 1.0
	}
	if profile.QAThresholds.TransparentCoverage <= 0 {
		profile.QAThresholds.TransparentCoverage = 1.0
	}
	return profile
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
