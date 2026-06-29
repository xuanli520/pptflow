package promptflow

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/xuanli520/pptflow/internal/workflow"
)

var nodeKinds = []string{
	"promptflow.prompt_optimize",
	"promptflow.outline_generate",
	"promptflow.style_reference",
	"promptflow.generate_slides",
	"promptflow.analyze_layout",
	"promptflow.extract_resources",
	"promptflow.assemble_pptx",
	"promptflow.package",
}

const pluginVersion = "2.0.0"

type Plugin struct{}

func Register(registry *workflow.Registry) error {
	return registry.Register(Plugin{})
}

func (Plugin) Manifest() workflow.PluginManifest {
	return workflow.PluginManifest{ID: "promptflow.v2", Version: pluginVersion, Kinds: nodeKinds}
}

func (Plugin) Validate(spec workflow.NodeSpec) error {
	for _, kind := range nodeKinds {
		if spec.Kind == kind {
			return nil
		}
	}
	return fmt.Errorf("unknown promptflow node kind %s", spec.Kind)
}

func (p Plugin) Execute(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	switch req.Spec.Kind {
	case "promptflow.prompt_optimize":
		return p.promptOptimize(ctx, req)
	case "promptflow.outline_generate":
		return p.outlineGenerate(ctx, req)
	case "promptflow.style_reference":
		return p.styleReference(ctx, req)
	case "promptflow.generate_slides":
		return p.generateSlides(ctx, req)
	case "promptflow.analyze_layout":
		return p.analyzeLayout(ctx, req)
	case "promptflow.extract_resources":
		return p.extractResources(ctx, req)
	case "promptflow.assemble_pptx":
		return p.assemblePPTX(ctx, req)
	case "promptflow.package":
		return p.packageBundle(ctx, req)
	default:
		return workflow.NodeResult{}, fmt.Errorf("unknown promptflow node kind %s", req.Spec.Kind)
	}
}

// promptOptimize takes the user's raw prompt and uses Codex to optimize it into structured requirements.
func (Plugin) promptOptimize(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	if req.Runtimes.Agent == nil {
		return workflow.NodeResult{}, fmt.Errorf("agent runtime is required")
	}
	userPrompt := stringConfig(req.Input, "prompt")
	if userPrompt == "" {
		userPrompt = stringConfig(req.Spec.Config, "prompt")
	}
	if userPrompt == "" {
		return workflow.NodeResult{}, fmt.Errorf("prompt is required")
	}

	systemPrompt := buildPromptOptimizePrompt(userPrompt)
	logPath, _ := req.Store.Path("prompt_optimize/codex.log")

	result, err := req.Runtimes.Agent.Turn(ctx, workflow.AgentTurnRequest{
		ProjectPath:       req.WorkspaceRoot,
		Prompt:            systemPrompt,
		Model:             stringConfig(req.Spec.Config, "model"),
		SandboxMode:       "read-only",
		SandboxPolicy:     "readOnly",
		NetworkAccess:     false,
		TimeoutSeconds:    intConfig(req.Spec.Config, "timeout_seconds", 120),
		MaxOutputBytes:    intConfig(req.Spec.Config, "max_output_bytes", 1<<20),
		CapabilitySummary: "pptflow prompt optimizer",
		LogPath:           logPath,
	})
	if err != nil {
		return workflow.NodeResult{}, fmt.Errorf("prompt optimize: %w", err)
	}

	requirements, err := parseOptimizedRequirements(result.Text)
	if err != nil {
		return workflow.NodeResult{}, fmt.Errorf("parse optimized requirements: %w", err)
	}

	ref, err := req.Store.PutJSON(ctx, "optimized_requirements.json", "optimized_requirements", req.Spec.ID, requirements)
	return withAgentMetrics([]workflow.ArtifactRef{ref}, result, err)
}

// outlineGenerate uses Codex to generate presentation outline and style spec.
func (Plugin) outlineGenerate(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	if req.Runtimes.Agent == nil {
		return workflow.NodeResult{}, fmt.Errorf("agent runtime is required")
	}

	var requirements OptimizedRequirements
	if _, err := req.Store.ReadJSON(ctx, "optimized_requirements.json", &requirements); err != nil {
		return workflow.NodeResult{}, fmt.Errorf("read optimized requirements: %w", err)
	}

	systemPrompt := buildOutlinePrompt(requirements)
	logPath, _ := req.Store.Path("outline_generate/codex.log")

	result, err := req.Runtimes.Agent.Turn(ctx, workflow.AgentTurnRequest{
		ProjectPath:       req.WorkspaceRoot,
		Prompt:            systemPrompt,
		Model:             stringConfig(req.Spec.Config, "model"),
		SandboxMode:       "read-only",
		SandboxPolicy:     "readOnly",
		NetworkAccess:     false,
		TimeoutSeconds:    intConfig(req.Spec.Config, "timeout_seconds", 180),
		MaxOutputBytes:    intConfig(req.Spec.Config, "max_output_bytes", 2<<20),
		CapabilitySummary: "pptflow outline generator",
		LogPath:           logPath,
	})
	if err != nil {
		return workflow.NodeResult{}, fmt.Errorf("outline generate: %w", err)
	}

	outline, styleSpec, err := parseOutlineAndStyle(result.Text)
	if err != nil {
		return workflow.NodeResult{}, fmt.Errorf("parse outline and style: %w", err)
	}

	outlineRef, err := req.Store.PutJSON(ctx, "outline.json", "outline", req.Spec.ID, outline)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	styleRef, err := req.Store.PutJSON(ctx, "style_spec.json", "style_spec", req.Spec.ID, styleSpec)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	return withAgentMetrics([]workflow.ArtifactRef{outlineRef, styleRef}, result, nil)
}

// styleReference generates a style reference image (cover/first slide) via Image2.
func (Plugin) styleReference(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	if req.Runtimes.Image == nil || !req.Runtimes.Image.Configured() {
		return workflow.NodeResult{}, fmt.Errorf("image runtime is required")
	}

	var styleSpec StyleSpec
	if _, err := req.Store.ReadJSON(ctx, "style_spec.json", &styleSpec); err != nil {
		return workflow.NodeResult{}, err
	}
	var outline Outline
	if _, err := req.Store.ReadJSON(ctx, "outline.json", &outline); err != nil {
		return workflow.NodeResult{}, err
	}

	outputPath, _ := req.Store.Path("slide_images/style_ref.png")
	prompt := buildStyleRefPrompt(styleSpec, outline)

	result, err := req.Runtimes.Image.Generate(ctx, workflow.ImageRequest{
		Prompt:         prompt,
		Size:           stringConfig(req.Spec.Config, "size", "1536x1024"),
		Quality:        stringConfig(req.Spec.Config, "quality", "high"),
		OutputPath:     outputPath,
		TimeoutSeconds: intConfig(req.Spec.Config, "timeout_seconds", 180),
	})
	if err != nil {
		return workflow.NodeResult{}, fmt.Errorf("generate style reference: %w", err)
	}

	manifest := SlideImageManifest{
		SchemaVersion: "pptflow.slide_image_manifest.v1",
		StyleRef:      result.Path,
	}
	ref, err := req.Store.PutJSON(ctx, "slide_image_manifest.json", "slide_image_manifest", req.Spec.ID, manifest)
	return withImageMetrics([]workflow.ArtifactRef{ref}, result, err)
}

// generateSlides generates each slide as an image via Image2, maintaining style consistency.
func (Plugin) generateSlides(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	if req.Runtimes.Image == nil || !req.Runtimes.Image.Configured() {
		return workflow.NodeResult{}, fmt.Errorf("image runtime is required")
	}

	var outline Outline
	if _, err := req.Store.ReadJSON(ctx, "outline.json", &outline); err != nil {
		return workflow.NodeResult{}, err
	}
	var styleSpec StyleSpec
	if _, err := req.Store.ReadJSON(ctx, "style_spec.json", &styleSpec); err != nil {
		return workflow.NodeResult{}, err
	}
	var manifest SlideImageManifest
	if _, err := req.Store.ReadJSON(ctx, "slide_image_manifest.json", &manifest); err != nil {
		return workflow.NodeResult{}, err
	}

	refs := []workflow.ArtifactRef{}
	for _, slide := range outline.Slides {
		outputPath, _ := req.Store.Path(fmt.Sprintf("slide_images/slide_%02d.png", slide.SlideNumber))
		prompt := buildSlideImagePrompt(styleSpec, outline, slide)

		result, err := req.Runtimes.Image.Generate(ctx, workflow.ImageRequest{
			Prompt:         prompt,
			Size:           stringConfig(req.Spec.Config, "size", "1536x1024"),
			Quality:        stringConfig(req.Spec.Config, "quality", "high"),
			OutputPath:     outputPath,
			TimeoutSeconds: intConfig(req.Spec.Config, "timeout_seconds", 180),
		})
		if err != nil {
			return workflow.NodeResult{}, fmt.Errorf("generate slide %d: %w", slide.SlideNumber, err)
		}

		manifest.Images = append(manifest.Images, SlideImage{
			SlideNumber: slide.SlideNumber,
			ImagePath:   result.Path,
			Prompt:      prompt,
			Model:       result.Model,
			Size:        result.Size,
		})
		refs = append(refs, workflow.ArtifactRef{
			Name: fmt.Sprintf("slide_%02d.png", slide.SlideNumber),
			Type: "image",
			Path: result.Path,
		})
	}

	manifestRef, err := req.Store.PutJSON(ctx, "slide_image_manifest.json", "slide_image_manifest", req.Spec.ID, manifest)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	refs = append(refs, manifestRef)
	return workflow.NodeResult{Artifacts: refs, Metrics: workflow.NodeMetrics{Model: stringConfig(req.Spec.Config, "model", "gpt-image-2")}}, nil
}

// analyzeLayout uses Codex to analyze each slide image and describe its visual layout.
func (Plugin) analyzeLayout(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	if req.Runtimes.Agent == nil {
		return workflow.NodeResult{}, fmt.Errorf("agent runtime is required")
	}

	var manifest SlideImageManifest
	if _, err := req.Store.ReadJSON(ctx, "slide_image_manifest.json", &manifest); err != nil {
		return workflow.NodeResult{}, err
	}
	var outline Outline
	if _, err := req.Store.ReadJSON(ctx, "outline.json", &outline); err != nil {
		return workflow.NodeResult{}, err
	}

	prompt := buildLayoutAnalysisPrompt(manifest, outline)
	logPath, _ := req.Store.Path("analyze_layout/codex.log")

	result, err := req.Runtimes.Agent.Turn(ctx, workflow.AgentTurnRequest{
		ProjectPath:       req.WorkspaceRoot,
		Prompt:            prompt,
		Model:             stringConfig(req.Spec.Config, "model"),
		SandboxMode:       "read-only",
		SandboxPolicy:     "readOnly",
		NetworkAccess:     false,
		TimeoutSeconds:    intConfig(req.Spec.Config, "timeout_seconds", 300),
		MaxOutputBytes:    intConfig(req.Spec.Config, "max_output_bytes", 4<<20),
		CapabilitySummary: "pptflow layout analyzer",
		LogPath:           logPath,
	})
	if err != nil {
		return workflow.NodeResult{}, fmt.Errorf("analyze layout: %w", err)
	}

	analysis, err := parseLayoutAnalysis(result.Text)
	if err != nil {
		return workflow.NodeResult{}, fmt.Errorf("parse layout analysis: %w", err)
	}

	ref, err := req.Store.PutJSON(ctx, "layout_analysis.json", "layout_analysis", req.Spec.ID, analysis)
	return withAgentMetrics([]workflow.ArtifactRef{ref}, result, err)
}

// extractResources uses Image2 to generate individual, editable resources from slide images.
func (Plugin) extractResources(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	if req.Runtimes.Image == nil || !req.Runtimes.Image.Configured() {
		return workflow.NodeResult{}, fmt.Errorf("image runtime is required")
	}

	var analysis LayoutAnalysis
	if _, err := req.Store.ReadJSON(ctx, "layout_analysis.json", &analysis); err != nil {
		return workflow.NodeResult{}, err
	}

	manifest := ExtractedResourceManifest{SchemaVersion: "pptflow.extracted_resources.v1"}
	refs := []workflow.ArtifactRef{}

	for _, slide := range analysis.Slides {
		for _, region := range slide.Regions {
			if region.Type != "image" {
				continue
			}
			resourceID := fmt.Sprintf("slide_%02d_%s", slide.SlideNumber, region.ID)
			outputPath, _ := req.Store.Path(fmt.Sprintf("extracted/%s.png", resourceID))

			extractPrompt := buildExtractPrompt(region)
			result, err := req.Runtimes.Image.Generate(ctx, workflow.ImageRequest{
				Prompt:         extractPrompt,
				Size:           "1024x1024",
				Quality:        "high",
				OutputPath:     outputPath,
				TimeoutSeconds: intConfig(req.Spec.Config, "timeout_seconds", 180),
			})
			if err != nil {
				return workflow.NodeResult{}, fmt.Errorf("extract resource %s: %w", resourceID, err)
			}

			manifest.Resources = append(manifest.Resources, ExtractedResource{
				ID:         resourceID,
				SlideNumber: slide.SlideNumber,
				Type:       "image",
				FilePath:   result.Path,
				X:          region.X,
				Y:          region.Y,
				W:          region.W,
				H:          region.H,
				Properties: map[string]any{
					"desc":        region.ImageDesc,
					"crop_hint":   region.CropHint,
				},
			})
			refs = append(refs, workflow.ArtifactRef{
				Name: resourceID + ".png",
				Type: "image",
				Path: result.Path,
			})
		}
	}

	manifestRef, err := req.Store.PutJSON(ctx, "extracted_resource_manifest.json", "extracted_resource_manifest", req.Spec.ID, manifest)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	refs = append(refs, manifestRef)
	return workflow.NodeResult{Artifacts: refs, Metrics: workflow.NodeMetrics{Model: "gpt-image-2"}}, nil
}

// assemblePPTX uses Codex (workspace-write) to assemble the final editable PPTX from extracted resources.
func (Plugin) assemblePPTX(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	if req.Runtimes.Agent == nil {
		return workflow.NodeResult{}, fmt.Errorf("agent runtime is required")
	}

	var analysis LayoutAnalysis
	if _, err := req.Store.ReadJSON(ctx, "layout_analysis.json", &analysis); err != nil {
		return workflow.NodeResult{}, err
	}
	var resources ExtractedResourceManifest
	if _, err := req.Store.ReadJSON(ctx, "extracted_resource_manifest.json", &resources); err != nil {
		return workflow.NodeResult{}, err
	}
	var styleSpec StyleSpec
	if _, err := req.Store.ReadJSON(ctx, "style_spec.json", &styleSpec); err != nil {
		return workflow.NodeResult{}, err
	}

	analysisJSON, _ := json.Marshal(analysis)
	resourcesJSON, _ := json.Marshal(resources)
	styleJSON, _ := json.Marshal(styleSpec)

	prompt := buildAssemblyPrompt(string(analysisJSON), string(resourcesJSON), string(styleJSON), req.WorkspaceRoot)
	logPath, _ := req.Store.Path("assemble_pptx/codex.log")

	result, err := req.Runtimes.Agent.Turn(ctx, workflow.AgentTurnRequest{
		ProjectPath:       req.WorkspaceRoot,
		Prompt:            prompt,
		Model:             stringConfig(req.Spec.Config, "model"),
		SandboxMode:       "workspace-write",
		SandboxPolicy:     "readWrite",
		NetworkAccess:     false,
		TimeoutSeconds:    intConfig(req.Spec.Config, "timeout_seconds", 600),
		MaxOutputBytes:    intConfig(req.Spec.Config, "max_output_bytes", 3<<20),
		CapabilitySummary: "pptflow PPTX assembler",
		LogPath:           logPath,
	})
	if err != nil {
		return workflow.NodeResult{}, fmt.Errorf("assemble pptx: %w", err)
	}

	_ = result.Text // Codex wrote deck.pptx to workspace

	deckPath := req.WorkspaceRoot + "/output/deck.pptx"
	ref, err := putFileIfExists(ctx, req.Store, "deck.pptx", "pptx", req.Spec.ID, deckPath)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	return withAgentMetrics([]workflow.ArtifactRef{ref}, result, nil)
}

// packageBundle collects all artifacts into a delivery bundle.
func (Plugin) packageBundle(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	artifacts, err := req.Store.List(ctx, "")
	if err != nil {
		return workflow.NodeResult{}, err
	}
	ref, err := req.Store.PutJSON(ctx, "delivery_bundle.json", "delivery_bundle", req.Spec.ID, map[string]any{
		"artifacts": artifacts,
		"status":    "completed",
	})
	return withMetrics([]workflow.ArtifactRef{ref}, "promptflow-packager", err)
}

// --- helpers ---

func withAgentMetrics(refs []workflow.ArtifactRef, result workflow.AgentTurnResult, err error) (workflow.NodeResult, error) {
	return workflow.NodeResult{
		Artifacts: refs,
		Metrics: workflow.NodeMetrics{
			Model:      result.Model,
			TokenUsage: result.TokenUsage,
		},
	}, err
}

func withImageMetrics(refs []workflow.ArtifactRef, result workflow.ImageResult, err error) (workflow.NodeResult, error) {
	return workflow.NodeResult{
		Artifacts: refs,
		Metrics: workflow.NodeMetrics{
			Model: result.Model,
		},
	}, err
}

func withMetrics(refs []workflow.ArtifactRef, model string, err error) (workflow.NodeResult, error) {
	return workflow.NodeResult{
		Artifacts: refs,
		Metrics: workflow.NodeMetrics{
			Model: strings.TrimSpace(model),
		},
	}, err
}

func stringConfig(config map[string]any, key string, defaults ...string) string {
	if len(config) == 0 {
		if len(defaults) > 0 {
			return defaults[0]
		}
		return ""
	}
	value, _ := config[key].(string)
	value = strings.TrimSpace(value)
	if value == "" && len(defaults) > 0 {
		return defaults[0]
	}
	return value
}

func intConfig(config map[string]any, key string, defaultVal int) int {
	if len(config) == 0 {
		return defaultVal
	}
	switch v := config[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	default:
		return defaultVal
	}
}

func putFileIfExists(ctx context.Context, store workflow.ArtifactStore, name, artifactType, producer, path string) (workflow.ArtifactRef, error) {
	f, err := os.Open(path)
	if err != nil {
		return workflow.ArtifactRef{Name: name, Type: artifactType, Path: path}, nil
	}
	defer f.Close()
	return store.Put(ctx, workflow.PutArtifactRequest{
		Name:     name,
		Type:     artifactType,
		Producer: producer,
		Content:  f,
	})
}
