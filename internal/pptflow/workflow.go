package pptflow

import "github.com/xuanli520/pptflow/internal/workflow"

func Phase0Workflow(scenario, fixturePath, templatePath string) workflow.WorkflowDefinition {
	assetPrepare := node("asset_prepare", "pptflow.asset_prepare", []string{"slide_plan"}, map[string]any{"model": "gpt-image-2", "size": "1536x1024", "quality": "high"})
	assetPrepare.Policy.TimeoutSeconds = 600
	return workflow.WorkflowDefinition{
		ID:     "pptflow-phase0",
		Name:   "PPTflow Phase 0 local pipeline",
		Policy: workflow.Policy{MaxNodes: 24},
		Nodes: []workflow.NodeSpec{
			node("requirements", "pptflow.requirements_fixture", nil, map[string]any{"scenario": scenario, "fixture_path": fixturePath}),
			node("template", "pptflow.template_introspect", []string{"requirements"}, map[string]any{"template_path": templatePath}),
			node("content_plan", "pptflow.content_plan", []string{"requirements"}, map[string]any{"model": "local-content-planner"}),
			node("slide_plan", "pptflow.slide_plan", []string{"content_plan", "template"}, map[string]any{"model": "local-layout-planner"}),
			assetPrepare,
			node("object_graph", "pptflow.object_graph_build", []string{"slide_plan", "template", "asset_prepare"}, map[string]any{"model": "local-object-graph"}),
			node("schema_verify", "pptflow.schema_verify", []string{"object_graph"}, nil),
			node("pptx_render", "pptflow.pptx_render", []string{"schema_verify"}, nil),
			node("editability_verify", "pptflow.editability_verify", []string{"pptx_render"}, nil),
			node("visual_verify", "pptflow.visual_verify", []string{"pptx_render"}, nil),
			node("repair_plan", "pptflow.repair_plan", []string{"editability_verify", "visual_verify"}, map[string]any{"model": "local-repair-planner"}),
			node("package", "pptflow.package", []string{"repair_plan"}, nil),
		},
	}
}

func node(id, kind string, deps []string, config map[string]any) workflow.NodeSpec {
	return workflow.NodeSpec{
		ID:        id,
		Kind:      kind,
		Name:      kind,
		DependsOn: deps,
		Config:    config,
		Policy:    workflow.NodePolicy{TimeoutSeconds: 60, MaxAttempts: 1},
	}
}
