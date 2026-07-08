package promptflow

import (
	"archive/zip"
	"context"
	"encoding/xml"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

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
	"promptflow.render_pptx_preview",
	"promptflow.visual_qa",
	"promptflow.repair_plan",
	"promptflow.repair_apply",
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
	case "promptflow.render_pptx_preview":
		return p.renderPPTXPreview(ctx, req)
	case "promptflow.visual_qa":
		return p.visualQA(ctx, req)
	case "promptflow.repair_plan":
		return p.repairPlan(ctx, req)
	case "promptflow.repair_apply":
		return p.repairApply(ctx, req)
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
		ReasoningEffort:   stringConfig(req.Spec.Config, "reasoning_effort", "medium"),
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
		ReasoningEffort:   stringConfig(req.Spec.Config, "reasoning_effort", "medium"),
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

	result, err := generateImage(ctx, req, workflow.ImageRequest{
		Model:          stringConfig(req.Spec.Config, "model"),
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
	imageRef, err := req.Store.Register(ctx, workflow.RegisterArtifactRequest{
		Name:     "slide_images/style_ref.png",
		Type:     "image",
		Producer: req.Spec.ID,
		Path:     result.Path,
	})
	if err != nil {
		return workflow.NodeResult{}, err
	}
	ref, err := req.Store.PutJSON(ctx, "slide_image_manifest.json", "slide_image_manifest", req.Spec.ID, manifest)
	return withImageMetrics([]workflow.ArtifactRef{imageRef, ref}, result, err)
}

// generateSlides generates each slide as an image via Image2, maintaining style consistency.
func (Plugin) generateSlides(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
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

		result, err := generateImage(ctx, req, workflow.ImageRequest{
			Model:          stringConfig(req.Spec.Config, "model"),
			Prompt:         prompt,
			Size:           stringConfig(req.Spec.Config, "size", "1536x1024"),
			Quality:        stringConfig(req.Spec.Config, "quality", "high"),
			OutputPath:     outputPath,
			SourceImages:   styleReferenceSource(manifest.StyleRef),
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
		imageRef, err := req.Store.Register(ctx, workflow.RegisterArtifactRequest{
			Name:     fmt.Sprintf("slide_images/slide_%02d.png", slide.SlideNumber),
			Type:     "image",
			Producer: req.Spec.ID,
			Path:     result.Path,
		})
		if err != nil {
			return workflow.NodeResult{}, err
		}
		refs = append(refs, imageRef)
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

	fallback := func(operation string, cause error) (workflow.NodeResult, error) {
		if boolConfig(req.Spec.Config, "allow_image_fallback") {
			analysis := fallbackLayoutAnalysis(manifest)
			ref, putErr := req.Store.PutJSON(ctx, "layout_analysis.json", "layout_analysis", req.Spec.ID, analysis)
			if putErr != nil {
				return workflow.NodeResult{}, fmt.Errorf("%s: %w", operation, cause)
			}
			return withMetrics([]workflow.ArtifactRef{ref}, "local-layout-fallback", nil)
		}
		return workflow.NodeResult{}, fmt.Errorf("%s: %w", operation, cause)
	}

	merged := LayoutAnalysis{
		SlideWidth:  13.333,
		SlideHeight: 7.5,
		Slides:      make([]SlideLayout, 0, len(manifest.Images)),
	}
	allV2 := true
	var combinedResult workflow.AgentTurnResult
	for i, img := range manifest.Images {
		slideManifest := manifest
		slideManifest.Images = []SlideImage{img}
		prompt := buildLayoutAnalysisPrompt(slideManifest, outline)
		logPath, _ := req.Store.Path(fmt.Sprintf("analyze_layout/slide_%02d_codex.log", img.SlideNumber))
		input, err := layoutAnalysisInput(slideManifest)
		if err != nil {
			return workflow.NodeResult{}, err
		}

		result, err := req.Runtimes.Agent.Turn(ctx, workflow.AgentTurnRequest{
			ProjectPath:       req.ArtifactRoot,
			Prompt:            prompt,
			Input:             input,
			Model:             stringConfig(req.Spec.Config, "model"),
			ReasoningEffort:   stringConfig(req.Spec.Config, "reasoning_effort", "low"),
			SandboxMode:       stringConfig(req.Spec.Config, "sandbox_mode", "read-only"),
			SandboxPolicy:     stringConfig(req.Spec.Config, "sandbox_policy", "readOnly"),
			NetworkAccess:     boolConfig(req.Spec.Config, "network_access"),
			WorkspaceRoots:    []string{req.ArtifactRoot},
			TimeoutSeconds:    intConfig(req.Spec.Config, "timeout_seconds", 300),
			MaxOutputBytes:    intConfig(req.Spec.Config, "max_output_bytes", 4<<20),
			CapabilitySummary: "pptflow layout analyzer",
			LogPath:           logPath,
		})
		if err != nil {
			return fallback("analyze layout", fmt.Errorf("slide %d: %w", img.SlideNumber, err))
		}

		analysis, err := parseLayoutAnalysis(result.Text)
		if err != nil {
			return fallback("parse layout analysis", fmt.Errorf("slide %d: %w", img.SlideNumber, err))
		}
		normalizeSingleSlideLayoutAnalysis(&analysis, img)
		if err := validateLayoutAnalysis(analysis, slideManifest); err != nil {
			return fallback("validate layout analysis", fmt.Errorf("slide %d: %w", img.SlideNumber, err))
		}
		if i == 0 {
			merged.SchemaVersion = analysis.SchemaVersion
			merged.SlideWidth = analysis.SlideWidth
			merged.SlideHeight = analysis.SlideHeight
		}
		if !isLayoutAnalysisV2(analysis) {
			allV2 = false
		}
		merged.Slides = append(merged.Slides, analysis.Slides...)
		if combinedResult.Model == "" {
			combinedResult.Model = result.Model
		}
		combinedResult.TokenUsage.Input += result.TokenUsage.Input
		combinedResult.TokenUsage.Output += result.TokenUsage.Output
		combinedResult.TokenUsage.Total += result.TokenUsage.Total
	}
	if allV2 {
		merged.SchemaVersion = "pptflow.layout_analysis.v2"
	}
	normalizeLayoutAnalysis(&merged)
	if err := validateLayoutAnalysis(merged, manifest); err != nil {
		return fallback("validate layout analysis", err)
	}

	ref, err := req.Store.PutJSON(ctx, "layout_analysis.json", "layout_analysis", req.Spec.ID, merged)
	return withAgentMetrics([]workflow.ArtifactRef{ref}, combinedResult, err)
}

// extractResources uses Image2 to generate individual, editable resources from slide images.
func (Plugin) extractResources(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	var analysis LayoutAnalysis
	if _, err := req.Store.ReadJSON(ctx, "layout_analysis.json", &analysis); err != nil {
		return workflow.NodeResult{}, err
	}

	manifest := ExtractedResourceManifest{SchemaVersion: "pptflow.extracted_resources.v1"}
	refs := []workflow.ArtifactRef{}
	tasks := []resourceExtractTask{}

		for _, slide := range analysis.Slides {
		for _, region := range slideRenderableRegions(slide) {
			if !shouldRenderVisualResourceRegion(region) {
				continue
			}
			resourceID := fmt.Sprintf("slide_%02d_%s", slide.SlideNumber, region.ID)
			outputPath, _ := req.Store.Path(fmt.Sprintf("extracted/%s.png", resourceID))
			tasks = append(tasks, resourceExtractTask{
				Index:      len(tasks),
				Slide:      slide,
				Region:     region,
				ResourceID: resourceID,
				OutputPath: outputPath,
			})
		}
	}
	results, err := extractResourcesConcurrently(ctx, req, analysis, tasks)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	for _, result := range results {
		manifest.Resources = append(manifest.Resources, result.Resource)
		ref, err := req.Store.Register(ctx, workflow.RegisterArtifactRequest{
			Name:     fmt.Sprintf("extracted/%s.png", result.Resource.ID),
			Type:     "image",
			Producer: req.Spec.ID,
			Path:     result.Resource.FilePath,
		})
		if err != nil {
			return workflow.NodeResult{}, err
		}
		refs = append(refs, ref)
	}

	manifestRef, err := req.Store.PutJSON(ctx, "extracted_resource_manifest.json", "extracted_resource_manifest", req.Spec.ID, manifest)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	refs = append(refs, manifestRef)
	return workflow.NodeResult{Artifacts: refs, Metrics: workflow.NodeMetrics{Model: "local-crop"}}, nil
}

type resourceExtractTask struct {
	Index      int
	Slide      SlideLayout
	Region     LayoutRegion
	ResourceID string
	OutputPath string
}

type resourceExtractResult struct {
	Index    int
	Resource ExtractedResource
}

func extractResourcesConcurrently(ctx context.Context, req workflow.NodeRequest, analysis LayoutAnalysis, tasks []resourceExtractTask) ([]resourceExtractResult, error) {
	if len(tasks) == 0 {
		return nil, nil
	}
	concurrency := intConfig(req.Spec.Config, "resource_concurrency", 4)
	if concurrency <= 0 {
		concurrency = 4
	}
	if concurrency > len(tasks) {
		concurrency = len(tasks)
	}
	results := make([]resourceExtractResult, len(tasks))
	errs := make(chan error, len(tasks))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, task := range tasks {
		task := task
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			method, err := extractVisualResource(ctx, req, task.Slide, task.Region, analysis, task.OutputPath)
			if err != nil {
				errs <- fmt.Errorf("extract resource %s: %w", task.ResourceID, err)
				return
			}
			hasAlpha := pngHasAlpha(task.OutputPath)
			quality := inspectTransparentResource(task.OutputPath, extractionMatteColor())
			transparentQualityPass := true
			if wantsTransparentResource(task.Region) {
				transparentQualityPass = quality.Pass
			}
			results[task.Index] = resourceExtractResult{
				Index: task.Index,
				Resource: ExtractedResource{
					ID:          task.ResourceID,
					SlideNumber: task.Slide.SlideNumber,
					Type:        task.Region.Type,
					FilePath:    task.OutputPath,
					X:           task.Region.X,
					Y:           task.Region.Y,
					W:           task.Region.W,
					H:           task.Region.H,
					Properties: map[string]any{
						"desc":               task.Region.ImageDesc,
						"crop_hint":          task.Region.CropHint,
						"transparent":        hasAlpha,
						"background_removed": wantsTransparentResource(task.Region) && hasAlpha,
						"alpha_coverage":     quality.AlphaCoverage,
						"checkerboard":       quality.CheckerboardDetected,
						"matte_detected":     quality.MatteDetected,
						"quality_pass":       transparentQualityPass,
						"extraction_method":  method,
						"source_region_id":   task.Region.ID,
						"fit":                imageFit(task.Region),
						"mask_shape":         imageMask(task.Region),
					},
				},
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			return nil, err
		}
	}
	sort.SliceStable(results, func(i, j int) bool {
		return results[i].Index < results[j].Index
	})
	return results, nil
}

func (Plugin) assemblePPTX(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
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

	deckPath := filepath.Join(req.WorkspaceRoot, "output", "deck.pptx")
	if isFullSlideFallbackAnalysis(analysis) && !boolConfig(req.Spec.Config, "allow_image_fallback") {
		return workflow.NodeResult{}, fmt.Errorf("assemble pptx: editable layout analysis unavailable")
	}
	if isFullSlideFallbackAnalysis(analysis) && boolConfig(req.Spec.Config, "allow_image_fallback") {
		if err := buildImageBackedPPTX(deckPath, analysis); err != nil {
			return workflow.NodeResult{}, fmt.Errorf("assemble pptx: %w", err)
		}
	} else if err := buildEditablePPTX(deckPath, analysis, resources, styleSpec); err != nil {
		return workflow.NodeResult{}, fmt.Errorf("assemble pptx: %w", err)
	}
	if err := validatePPTX(deckPath, len(analysis.Slides)); err != nil {
		return workflow.NodeResult{}, err
	}
	ref, err := putRequiredFile(ctx, req.Store, "deck.pptx", "pptx", req.Spec.ID, deckPath)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	return withMetrics([]workflow.ArtifactRef{ref}, "local-pptx", nil)
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

func doubleConfig(config map[string]any, key string, defaultVal float64) float64 {
	if len(config) == 0 {
		return defaultVal
	}
	switch v := config[key].(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	default:
		return defaultVal
	}
}

func boolConfig(config map[string]any, key string) bool {
	if len(config) == 0 {
		return false
	}
	value, _ := config[key].(bool)
	return value
}

func generateImage(ctx context.Context, req workflow.NodeRequest, imageReq workflow.ImageRequest) (workflow.ImageResult, error) {
	if req.Runtimes.Image != nil && req.Runtimes.Image.Configured() {
		return req.Runtimes.Image.Generate(ctx, imageReq)
	}
	if boolConfig(req.Spec.Config, "require_images") || !strings.EqualFold(stringConfig(req.Spec.Config, "fallback_policy", "strict"), "dev_placeholder") {
		return workflow.ImageResult{}, fmt.Errorf("image runtime is required")
	}
	size := strings.TrimSpace(imageReq.Size)
	if size == "" {
		size = "1536x1024"
	}
	if err := writePlaceholderPNG(imageReq.OutputPath, size); err != nil {
		return workflow.ImageResult{}, err
	}
	quality := strings.TrimSpace(imageReq.Quality)
	if quality == "" {
		quality = "placeholder"
	}
	return workflow.ImageResult{Path: imageReq.OutputPath, Model: "local-placeholder", Size: size, Quality: quality, MIME: "image/png"}, nil
}

func writePlaceholderPNG(path, size string) error {
	width, height := imageSize(size)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: color.RGBA{R: 245, G: 246, B: 248, A: 255}}, image.Point{}, draw.Src)
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	encodeErr := png.Encode(file, img)
	closeErr := file.Close()
	if encodeErr != nil {
		return encodeErr
	}
	return closeErr
}

func imageSize(size string) (int, int) {
	width, height := 1536, 1024
	left, right, ok := strings.Cut(strings.ToLower(strings.TrimSpace(size)), "x")
	if !ok {
		return width, height
	}
	parsedWidth, widthErr := strconv.Atoi(strings.TrimSpace(left))
	parsedHeight, heightErr := strconv.Atoi(strings.TrimSpace(right))
	if widthErr == nil && parsedWidth > 0 {
		width = parsedWidth
	}
	if heightErr == nil && parsedHeight > 0 {
		height = parsedHeight
	}
	return width, height
}

func styleReferenceSource(path string) []workflow.ImageSource {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	return []workflow.ImageSource{{Path: path, Role: "style_reference", Detail: "high"}}
}

func layoutAnalysisInput(manifest SlideImageManifest) ([]workflow.AgentInputPart, error) {
	input := make([]workflow.AgentInputPart, 0, len(manifest.Images))
	for _, img := range manifest.Images {
		path := strings.TrimSpace(img.ImagePath)
		if path == "" {
			return nil, fmt.Errorf("slide %d image path is empty", img.SlideNumber)
		}
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("slide %d image is not readable: %w", img.SlideNumber, err)
		}
		input = append(input, workflow.AgentInputPart{Type: "localImage", Path: path})
	}
	return input, nil
}

func normalizeSingleSlideLayoutAnalysis(analysis *LayoutAnalysis, img SlideImage) {
	if analysis == nil || len(analysis.Slides) != 1 {
		return
	}
	analysis.Slides[0].SlideNumber = img.SlideNumber
	analysis.Slides[0].ImagePath = img.ImagePath
}

func validateLayoutAnalysis(analysis LayoutAnalysis, manifest SlideImageManifest) error {
	if len(analysis.Slides) != len(manifest.Images) {
		return fmt.Errorf("layout analysis slide count %d does not match image count %d", len(analysis.Slides), len(manifest.Images))
	}
	imageBySlide := map[int]string{}
	for _, img := range manifest.Images {
		if img.SlideNumber <= 0 {
			return fmt.Errorf("manifest contains invalid slide number %d", img.SlideNumber)
		}
		imageBySlide[img.SlideNumber] = filepath.Clean(img.ImagePath)
	}
	width := analysis.SlideWidth
	if width <= 0 {
		width = 13.333
	}
	height := analysis.SlideHeight
	if height <= 0 {
		height = 7.5
	}
	seenSlides := map[int]bool{}
	for i := range analysis.Slides {
		slide := analysis.Slides[i]
		normalizeSlideLayout(&slide)
		expectedImage, ok := imageBySlide[slide.SlideNumber]
		if !ok {
			return fmt.Errorf("layout analysis contains unknown slide %d", slide.SlideNumber)
		}
		if seenSlides[slide.SlideNumber] {
			return fmt.Errorf("layout analysis contains duplicate slide %d", slide.SlideNumber)
		}
		seenSlides[slide.SlideNumber] = true
		if filepath.Clean(slide.ImagePath) != expectedImage {
			return fmt.Errorf("slide %d image path mismatch", slide.SlideNumber)
		}
		regionIDs := map[string]bool{}
		for _, region := range slideRenderableRegions(slide) {
			if strings.TrimSpace(region.ID) == "" {
				return fmt.Errorf("slide %d contains region with empty id", slide.SlideNumber)
			}
			if regionIDs[region.ID] {
				return fmt.Errorf("slide %d contains duplicate region %s", slide.SlideNumber, region.ID)
			}
			regionIDs[region.ID] = true
			if !validRegionBounds(region, width, height) {
				return fmt.Errorf("slide %d region %s is outside slide bounds", slide.SlideNumber, region.ID)
			}
			if isTextRegion(region.Type) && strings.TrimSpace(regionText(region)) == "" {
				return fmt.Errorf("slide %d text region %s has empty text_content", slide.SlideNumber, region.ID)
			}
			if isLayoutAnalysisV2(analysis) {
				if strings.EqualFold(region.Type, "shape") && (region.Shape == nil || region.Fill == nil || region.Stroke == nil || region.Shadow == nil) {
					return fmt.Errorf("slide %d shape region %s is missing shape/fill/stroke/shadow", slide.SlideNumber, region.ID)
				}
				if strings.EqualFold(region.Type, "line") && region.Stroke == nil {
					return fmt.Errorf("slide %d line region %s is missing stroke", slide.SlideNumber, region.ID)
				}
			}
		}
	}
	return nil
}

func fallbackLayoutAnalysis(manifest SlideImageManifest) LayoutAnalysis {
	analysis := LayoutAnalysis{
		SchemaVersion: "pptflow.layout_analysis.v1",
		SlideWidth:    13.333,
		SlideHeight:   7.5,
		Slides:        make([]SlideLayout, 0, len(manifest.Images)),
	}
	for _, img := range manifest.Images {
		analysis.Slides = append(analysis.Slides, SlideLayout{
			SlideNumber: img.SlideNumber,
			ImagePath:   img.ImagePath,
			Regions: []LayoutRegion{{
				ID:        "full_slide",
				Type:      "image",
				X:         0,
				Y:         0,
				W:         analysis.SlideWidth,
				H:         analysis.SlideHeight,
				ZOrder:    1,
				ImageDesc: "Full slide image",
				CropHint:  "entire slide",
			}},
		})
	}
	return analysis
}

func isVisualResourceRegion(regionType string) bool {
	switch strings.ToLower(strings.TrimSpace(regionType)) {
	case "image", "icon", "chart", "table":
		return true
	default:
		return false
	}
}

func shouldRenderVisualResourceRegion(region LayoutRegion) bool {
	if !isVisualResourceRegion(region.Type) {
		return false
	}
	return !isCompositeVisualRegion(region)
}

func isTextRegion(regionType string) bool {
	switch strings.ToLower(strings.TrimSpace(regionType)) {
	case "text", "title", "body_text":
		return true
	default:
		return false
	}
}

func slideRenderableRegions(slide SlideLayout) []LayoutRegion {
	if len(slide.Elements) > 0 {
		return slide.Elements
	}
	return slide.Regions
}

func isLayoutAnalysisV2(analysis LayoutAnalysis) bool {
	return strings.EqualFold(strings.TrimSpace(analysis.SchemaVersion), "pptflow.layout_analysis.v2")
}

func validRegionBounds(region LayoutRegion, width, height float64) bool {
	if region.X < 0 || region.Y < 0 || region.X+region.W > width || region.Y+region.H > height {
		return false
	}
	if strings.EqualFold(region.Type, "line") {
		return region.W >= 0 && region.H >= 0 && (region.W > 0 || region.H > 0)
	}
	return region.W > 0 && region.H > 0
}

func isFullSlideFallbackAnalysis(analysis LayoutAnalysis) bool {
	if len(analysis.Slides) == 0 {
		return false
	}
	width, height := slideDimensions(analysis)
	for _, slide := range analysis.Slides {
		regions := slideRenderableRegions(slide)
		if len(regions) != 1 {
			return false
		}
		region := regions[0]
		if region.ID != "full_slide" || region.Type != "image" || region.X != 0 || region.Y != 0 {
			return false
		}
		if !near(region.W, width) || !near(region.H, height) {
			return false
		}
	}
	return true
}

func isBackgroundRegion(region LayoutRegion) bool {
	id := strings.ToLower(strings.TrimSpace(region.ID))
	if strings.Contains(id, "background") || strings.Contains(id, "bg") {
		return true
	}
	return region.X == 0 && region.Y == 0 && region.W >= 13 && region.H >= 7
}

func near(a, b float64) bool {
	if a > b {
		return a-b < 0.05
	}
	return b-a < 0.05
}

func cropRegionFromSlide(slidePath string, region LayoutRegion, slideWidth, slideHeight float64, outputPath string) error {
	if slideWidth <= 0 {
		slideWidth = 13.333
	}
	if slideHeight <= 0 {
		slideHeight = 7.5
	}
	file, err := os.Open(slidePath)
	if err != nil {
		return err
	}
	defer file.Close()
	src, err := png.Decode(file)
	if err != nil {
		return err
	}
	bounds := src.Bounds()
	scaleX := float64(bounds.Dx()) / slideWidth
	scaleY := float64(bounds.Dy()) / slideHeight
	rect := image.Rect(
		int(region.X*scaleX+0.5)+bounds.Min.X,
		int(region.Y*scaleY+0.5)+bounds.Min.Y,
		int((region.X+region.W)*scaleX+0.5)+bounds.Min.X,
		int((region.Y+region.H)*scaleY+0.5)+bounds.Min.Y,
	).Intersect(bounds)
	if rect.Empty() {
		return fmt.Errorf("region %s crop is empty", region.ID)
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return err
	}
	dst := image.NewRGBA(image.Rect(0, 0, rect.Dx(), rect.Dy()))
	draw.Draw(dst, dst.Bounds(), src, rect.Min, draw.Src)
	out, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	encodeErr := png.Encode(out, dst)
	closeErr := out.Close()
	if encodeErr != nil {
		return encodeErr
	}
	return closeErr
}

func validatePPTX(path string, expectedSlides int) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("deck.pptx is missing: %w", err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("deck.pptx is empty")
	}
	reader, err := zip.OpenReader(path)
	if err != nil {
		return fmt.Errorf("deck.pptx is not a valid pptx zip: %w", err)
	}
	defer reader.Close()
	required := map[string]bool{
		"[Content_Types].xml":             false,
		"ppt/presentation.xml":            false,
		"ppt/_rels/presentation.xml.rels": false,
	}
	slideCount := 0
	for _, file := range reader.File {
		name := filepath.ToSlash(file.Name)
		if _, ok := required[name]; ok {
			required[name] = true
			if err := validateZipXML(file, rootNameForPPTXPart(name)); err != nil {
				return fmt.Errorf("deck.pptx invalid %s: %w", name, err)
			}
		}
		if strings.HasPrefix(name, "ppt/slides/slide") && strings.HasSuffix(name, ".xml") && isSlideXML(name) {
			if err := validateZipXML(file, "sld"); err != nil {
				return fmt.Errorf("deck.pptx invalid %s: %w", name, err)
			}
			slideCount++
		}
	}
	for name, found := range required {
		if !found {
			return fmt.Errorf("deck.pptx missing %s", name)
		}
	}
	if expectedSlides > 0 && slideCount != expectedSlides {
		return fmt.Errorf("deck.pptx slide count %d does not match expected %d", slideCount, expectedSlides)
	}
	return nil
}

type pptxImageUse struct {
	RelID      string
	Target     string
	ZipName    string
	SourcePath string
}

func buildEditablePPTX(path string, analysis LayoutAnalysis, resources ExtractedResourceManifest, styleSpec StyleSpec) error {
	slides := append([]SlideLayout(nil), analysis.Slides...)
	sort.Slice(slides, func(i, j int) bool { return slides[i].SlideNumber < slides[j].SlideNumber })
	if len(slides) == 0 {
		return fmt.Errorf("layout analysis has no slides")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	zipWriter := zip.NewWriter(file)
	writeErr := writeEditablePPTXPackage(zipWriter, analysis, slides, resources, styleSpec)
	closeZipErr := zipWriter.Close()
	closeFileErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeZipErr != nil {
		return closeZipErr
	}
	return closeFileErr
}

func writeEditablePPTXPackage(zipWriter *zip.Writer, analysis LayoutAnalysis, slides []SlideLayout, resources ExtractedResourceManifest, styleSpec StyleSpec) error {
	if err := writeCommonPPTXParts(zipWriter, len(slides)); err != nil {
		return err
	}
	resourcesByID := map[string]ExtractedResource{}
	for _, resource := range resources.Resources {
		resourcesByID[resource.ID] = resource
	}
	mediaIndex := 1
	for i, slide := range slides {
		index := i + 1
		regions := append([]LayoutRegion(nil), slideRenderableRegions(slide)...)
		fixRegionLayers(regions)
		sort.SliceStable(regions, func(i, j int) bool {
			if regions[i].ZOrder == regions[j].ZOrder {
				return regions[i].ID < regions[j].ID
			}
			return regions[i].ZOrder < regions[j].ZOrder
		})
		imageRelIDs := map[string]string{}
		imageUses := []pptxImageUse{}
		for _, region := range regions {
			if !shouldRenderVisualResourceRegion(region) {
				continue
			}
			resourceID := resourceIDForRegion(slide.SlideNumber, region.ID)
			resource, ok := resourcesByID[resourceID]
			if !ok {
				return fmt.Errorf("missing extracted resource %s", resourceID)
			}
			sourcePath := strings.TrimSpace(resource.FilePath)
			if sourcePath == "" {
				return fmt.Errorf("extracted resource %s has empty file_path", resourceID)
			}
			ext := pptxImageExt(sourcePath)
			relID := fmt.Sprintf("rId%d", len(imageUses)+2)
			target := fmt.Sprintf("../media/image%d.%s", mediaIndex, ext)
			zipName := fmt.Sprintf("ppt/media/image%d.%s", mediaIndex, ext)
			imageRelIDs[resourceID] = relID
			imageUses = append(imageUses, pptxImageUse{RelID: relID, Target: target, ZipName: zipName, SourcePath: sourcePath})
			mediaIndex++
		}
		if err := writeZipText(zipWriter, fmt.Sprintf("ppt/slides/slide%d.xml", index), pptxEditableSlide(slide, regions, imageRelIDs, analysis, styleSpec)); err != nil {
			return err
		}
		if err := writeZipText(zipWriter, fmt.Sprintf("ppt/slides/_rels/slide%d.xml.rels", index), pptxSlideRels(imageUses)); err != nil {
			return err
		}
		for _, imageUse := range imageUses {
			data, err := os.ReadFile(imageUse.SourcePath)
			if err != nil {
				return fmt.Errorf("read %s: %w", imageUse.SourcePath, err)
			}
			if err := writeZipBytes(zipWriter, imageUse.ZipName, data); err != nil {
				return err
			}
		}
	}
	return nil
}

func fixRegionLayers(regions []LayoutRegion) {
	for i := range regions {
		if isBackgroundRegion(regions[i]) && regions[i].ZOrder > 0 {
			regions[i].ZOrder = 0
		}
	}
	for i := range regions {
		if !isPictureLikeOverlapRegion(regions[i]) {
			continue
		}
		for j := range regions {
			if i == j || !isTextLikeRegion(regions[j]) {
				continue
			}
			if boxIntersection(regionBox(regions[i]), regionBox(regions[j])) <= 0 {
				continue
			}
			target := FlexibleInt(int(regions[j].ZOrder) - 1)
			if regions[i].ZOrder >= regions[j].ZOrder || regions[i].ZOrder > target {
				regions[i].ZOrder = target
			}
		}
	}
}

func writeCommonPPTXParts(zipWriter *zip.Writer, slideCount int) error {
	if err := writeZipText(zipWriter, "[Content_Types].xml", pptxContentTypes(slideCount)); err != nil {
		return err
	}
	if err := writeZipText(zipWriter, "_rels/.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="ppt/presentation.xml"/></Relationships>`); err != nil {
		return err
	}
	if err := writeZipText(zipWriter, "ppt/presentation.xml", pptxPresentation(slideCount)); err != nil {
		return err
	}
	if err := writeZipText(zipWriter, "ppt/_rels/presentation.xml.rels", pptxPresentationRels(slideCount)); err != nil {
		return err
	}
	if err := writeZipText(zipWriter, "ppt/slideMasters/slideMaster1.xml", pptxSlideMaster()); err != nil {
		return err
	}
	if err := writeZipText(zipWriter, "ppt/slideMasters/_rels/slideMaster1.xml.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slideLayout" Target="../slideLayouts/slideLayout1.xml"/><Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/theme" Target="../theme/theme1.xml"/></Relationships>`); err != nil {
		return err
	}
	if err := writeZipText(zipWriter, "ppt/slideLayouts/slideLayout1.xml", pptxSlideLayout()); err != nil {
		return err
	}
	if err := writeZipText(zipWriter, "ppt/slideLayouts/_rels/slideLayout1.xml.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slideMaster" Target="../slideMasters/slideMaster1.xml"/></Relationships>`); err != nil {
		return err
	}
	return writeZipText(zipWriter, "ppt/theme/theme1.xml", pptxTheme())
}

func pptxEditableSlide(slide SlideLayout, regions []LayoutRegion, imageRelIDs map[string]string, analysis LayoutAnalysis, styleSpec StyleSpec) string {
	var shapes strings.Builder
	nextID := 2
	for _, region := range regions {
		switch {
		case isTextRegion(region.Type):
			shapes.WriteString(pptxTextShape(nextID, region, styleSpec))
			nextID++
		case shouldRenderVisualResourceRegion(region):
			resourceID := resourceIDForRegion(slide.SlideNumber, region.ID)
			if relID := imageRelIDs[resourceID]; relID != "" {
				shapes.WriteString(pptxPicture(nextID, region, relID))
				nextID++
			}
		case strings.EqualFold(region.Type, "line") || looksLikeLine(region):
			shapes.WriteString(pptxLineShape(nextID, region, styleSpec))
			nextID++
		case strings.EqualFold(region.Type, "shape"):
			shapes.WriteString(pptxShape(nextID, region, styleSpec))
			nextID++
		case strings.EqualFold(region.Type, "decoration") && isBackgroundRegion(region):
			shapes.WriteString(pptxShape(nextID, region, styleSpec))
			nextID++
		}
	}
	width, height := slideDimensions(analysis)
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><p:sld xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"><p:cSld><p:spTree><p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr><p:grpSpPr><a:xfrm><a:off x="0" y="0"/><a:ext cx="%d" cy="%d"/><a:chOff x="0" y="0"/><a:chExt cx="%d" cy="%d"/></a:xfrm></p:grpSpPr>%s</p:spTree></p:cSld><p:clrMapOvr><a:masterClrMapping/></p:clrMapOvr></p:sld>`, emu(width), emu(height), emu(width), emu(height), shapes.String())
}

func pptxTextShape(id int, region LayoutRegion, styleSpec StyleSpec) string {
	text := effectiveTextSpec(region, styleSpec)
	return fmt.Sprintf(`<p:sp><p:nvSpPr><p:cNvPr id="%d" name="%s"/><p:cNvSpPr txBox="1"/><p:nvPr/></p:nvSpPr><p:spPr>%s<a:noFill/><a:ln><a:noFill/></a:ln></p:spPr>%s</p:sp>`, id, xmlAttr(region.ID), pptxTransform(region), pptxShapeTextBody(region, text, styleSpec))
}

func pptxPicture(id int, region LayoutRegion, relID string) string {
	return fmt.Sprintf(`<p:pic><p:nvPicPr><p:cNvPr id="%d" name="%s"/><p:cNvPicPr><a:picLocks noChangeAspect="1"/></p:cNvPicPr><p:nvPr/></p:nvPicPr><p:blipFill><a:blip r:embed="%s"/>%s</p:blipFill><p:spPr>%s<a:prstGeom prst="%s"><a:avLst/></a:prstGeom></p:spPr></p:pic>`, id, xmlAttr(region.ID), xmlAttr(relID), pptxPictureFill(region), pptxTransform(region), xmlAttr(pptxImageMask(region)))
}

func pptxRectShape(id int, region LayoutRegion, styleSpec StyleSpec) string {
	return pptxShapeWithPreset(id, region, styleSpec, "rect")
}

func pptxShape(id int, region LayoutRegion, styleSpec StyleSpec) string {
	switch pptxShapePreset(region) {
	case "line":
		return pptxLineShape(id, region, styleSpec)
	case "ellipse":
		return pptxEllipseShape(id, region, styleSpec)
	case "roundRect":
		return pptxRoundRectShape(id, region, styleSpec)
	default:
		return pptxRectShape(id, region, styleSpec)
	}
}

func pptxLineShape(id int, region LayoutRegion, styleSpec StyleSpec) string {
	return pptxShapeWithPreset(id, region, styleSpec, "line")
}

func pptxEllipseShape(id int, region LayoutRegion, styleSpec StyleSpec) string {
	return pptxShapeWithPreset(id, region, styleSpec, "ellipse")
}

func pptxRoundRectShape(id int, region LayoutRegion, styleSpec StyleSpec) string {
	return pptxShapeWithPreset(id, region, styleSpec, "roundRect")
}

func pptxShapeWithPreset(id int, region LayoutRegion, styleSpec StyleSpec, preset string) string {
	body := pptxShapeTextBody(region, effectiveTextSpec(region, styleSpec), styleSpec)
	return fmt.Sprintf(`<p:sp><p:nvSpPr><p:cNvPr id="%d" name="%s"/><p:cNvSpPr/><p:nvPr/></p:nvSpPr><p:spPr>%s<a:prstGeom prst="%s"><a:avLst/></a:prstGeom>%s%s%s</p:spPr>%s</p:sp>`, id, xmlAttr(region.ID), pptxTransform(region), xmlAttr(preset), pptxFill(region, styleSpec), pptxStroke(region, styleSpec), pptxShadow(region), body)
}

func shapeFillColor(region LayoutRegion, styleSpec StyleSpec) string {
	id := strings.ToLower(strings.TrimSpace(region.ID))
	switch {
	case isBackgroundRegion(region):
		return normalizeHexColor(firstNonEmptyString(styleSpec.ColorScheme.Background, "#FFFFFF"))
	case strings.Contains(id, "bar"), strings.Contains(id, "dot"):
		return normalizeHexColor(firstNonEmptyString(styleSpec.ColorScheme.Accent, styleSpec.ColorScheme.Primary, "#14B8A6"))
	case strings.Contains(id, "divider"), strings.Contains(id, "axis"), strings.Contains(id, "bracket"):
		return "CBD5E1"
	default:
		return "F8FAFC"
	}
}

func pptxShapeTextBody(region LayoutRegion, text TextSpec, styleSpec StyleSpec) string {
	content := strings.TrimSpace(text.Content)
	if content == "" && len(text.Runs) == 0 {
		return `<p:txBody><a:bodyPr/><a:lstStyle/><a:p/></p:txBody>`
	}
	return fmt.Sprintf(`<p:txBody><a:bodyPr wrap="square" rtlCol="0" anchor="%s" lIns="%d" rIns="%d" tIns="%d" bIns="%d"><a:normAutofit fontScale="60000" lnSpcReduction="20000"/></a:bodyPr><a:lstStyle/>%s</p:txBody>`, pptxVerticalAnchor(text.VerticalAlignment), ptEMU(text.MarginLeftPt), ptEMU(text.MarginRightPt), ptEMU(text.MarginTopPt), ptEMU(text.MarginBottomPt), pptxParagraphsFromSpec(text, region, styleSpec))
}

func pptxParagraphsFromSpec(text TextSpec, region LayoutRegion, styleSpec StyleSpec) string {
	fontSize := textFontSize(text, region)
	color := normalizeHexColor(firstNonEmptyString(text.Color, region.FontColorEst, styleSpec.ColorScheme.Foreground, "#1F1F1F"))
	font := firstNonEmptyString(text.FontFamily, preferredFont(styleSpec, region.Type == "title" || region.Role == "title"))
	alignment := firstNonEmptyString(text.Alignment, region.Alignment)
	if len(text.Runs) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, `<a:p><a:pPr algn="%s"/>`, pptxAlignment(alignment))
		wroteRun := false
		for _, run := range text.Runs {
			runText := run.Content
			if runText == "" {
				continue
			}
			wroteRun = true
			runSize := fontSize
			if run.FontSizePt > 0 {
				runSize = int(run.FontSizePt*100 + 0.5)
			}
			runColor := normalizeHexColor(firstNonEmptyString(run.Color, color))
			runFont := firstNonEmptyString(run.FontFamily, font)
			fmt.Fprintf(&b, `<a:r><a:rPr lang="en-US" sz="%d"%s><a:solidFill><a:srgbClr val="%s"/></a:solidFill><a:latin typeface="%s"/></a:rPr><a:t>%s</a:t></a:r>`, runSize, pptxBoldAttr(run.Bold || isBoldWeight(run.FontWeight)), runColor, xmlAttr(runFont), xmlText(runText))
		}
		if !wroteRun && strings.TrimSpace(text.Content) != "" {
			fmt.Fprintf(&b, `<a:r><a:rPr lang="en-US" sz="%d"%s><a:solidFill><a:srgbClr val="%s"/></a:solidFill><a:latin typeface="%s"/></a:rPr><a:t>%s</a:t></a:r>`, fontSize, pptxBoldAttr(isBoldWeight(text.FontWeight)), color, xmlAttr(font), xmlText(text.Content))
		}
		b.WriteString(`</a:p>`)
		return b.String()
	}
	return pptxParagraphs(text.Content, alignment, fontSize/100, color, font, isBoldWeight(text.FontWeight))
}

func pptxParagraphs(text, alignment string, fontSize int, color, font string, bold bool) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) == 0 {
		lines = []string{""}
	}
	var b strings.Builder
	for _, line := range lines {
		fmt.Fprintf(&b, `<a:p><a:pPr algn="%s"/><a:r><a:rPr lang="en-US" sz="%d"%s><a:solidFill><a:srgbClr val="%s"/></a:solidFill><a:latin typeface="%s"/></a:rPr><a:t>%s</a:t></a:r></a:p>`, pptxAlignment(alignment), fontSize*100, pptxBoldAttr(bold), color, xmlAttr(font), xmlText(line))
	}
	return b.String()
}

func pptxSlideRels(imageUses []pptxImageUse) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slideLayout" Target="../slideLayouts/slideLayout1.xml"/>`)
	for _, imageUse := range imageUses {
		fmt.Fprintf(&b, `<Relationship Id="%s" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/image" Target="%s"/>`, xmlAttr(imageUse.RelID), xmlAttr(imageUse.Target))
	}
	b.WriteString(`</Relationships>`)
	return b.String()
}

func pptxTransform(region LayoutRegion) string {
	return fmt.Sprintf(`<a:xfrm><a:off x="%d" y="%d"/><a:ext cx="%d" cy="%d"/></a:xfrm>`, emu(region.X), emu(region.Y), emu(region.W), emu(region.H))
}

func emu(inches float64) int64 {
	if inches <= 0 {
		return 0
	}
	return int64(inches*914400 + 0.5)
}

func slideDimensions(analysis LayoutAnalysis) (float64, float64) {
	width := analysis.SlideWidth
	if width <= 0 {
		width = 13.333
	}
	height := analysis.SlideHeight
	if height <= 0 {
		height = 7.5
	}
	return width, height
}

func resourceIDForRegion(slideNumber int, regionID string) string {
	return fmt.Sprintf("slide_%02d_%s", slideNumber, regionID)
}

func pptxImageExt(path string) string {
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
	switch ext {
	case "jpg", "jpeg":
		return ext
	default:
		return "png"
	}
}

func regionFontSize(region LayoutRegion) int {
	if size := int(region.FontSizeEst); size > 0 {
		return size
	}
	if region.Type == "title" {
		return 34
	}
	return 18
}

func preferredFont(styleSpec StyleSpec, heading bool) string {
	for _, font := range styleSpec.Typography.PreferFonts {
		font = strings.TrimSpace(font)
		if font != "" {
			return font
		}
	}
	if heading {
		return "Aptos Display"
	}
	return "Aptos"
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func pptxAlignment(alignment string) string {
	switch strings.ToLower(strings.TrimSpace(alignment)) {
	case "center", "centre":
		return "ctr"
	case "right":
		return "r"
	default:
		return "l"
	}
}

func normalizeHexColor(color string) string {
	color = strings.TrimPrefix(strings.TrimSpace(color), "#")
	if len(color) == 3 {
		color = string([]byte{color[0], color[0], color[1], color[1], color[2], color[2]})
	}
	if len(color) != 6 {
		return "1F1F1F"
	}
	if _, err := strconv.ParseUint(color, 16, 32); err != nil {
		return "1F1F1F"
	}
	return strings.ToUpper(color)
}

func xmlText(text string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(text))
	return b.String()
}

func xmlAttr(text string) string {
	replacer := strings.NewReplacer("&", "&amp;", `"`, "&quot;", "<", "&lt;", ">", "&gt;")
	return replacer.Replace(text)
}

func buildImageBackedPPTX(path string, analysis LayoutAnalysis) error {
	slides := append([]SlideLayout(nil), analysis.Slides...)
	sort.Slice(slides, func(i, j int) bool { return slides[i].SlideNumber < slides[j].SlideNumber })
	if len(slides) == 0 {
		return fmt.Errorf("layout analysis has no slides")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	zipWriter := zip.NewWriter(file)
	writeErr := writePPTXPackage(zipWriter, slides)
	closeZipErr := zipWriter.Close()
	closeFileErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeZipErr != nil {
		return closeZipErr
	}
	return closeFileErr
}

func writePPTXPackage(zipWriter *zip.Writer, slides []SlideLayout) error {
	if err := writeZipText(zipWriter, "[Content_Types].xml", pptxContentTypes(len(slides))); err != nil {
		return err
	}
	if err := writeZipText(zipWriter, "_rels/.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="ppt/presentation.xml"/></Relationships>`); err != nil {
		return err
	}
	if err := writeZipText(zipWriter, "ppt/presentation.xml", pptxPresentation(len(slides))); err != nil {
		return err
	}
	if err := writeZipText(zipWriter, "ppt/_rels/presentation.xml.rels", pptxPresentationRels(len(slides))); err != nil {
		return err
	}
	if err := writeZipText(zipWriter, "ppt/slideMasters/slideMaster1.xml", pptxSlideMaster()); err != nil {
		return err
	}
	if err := writeZipText(zipWriter, "ppt/slideMasters/_rels/slideMaster1.xml.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slideLayout" Target="../slideLayouts/slideLayout1.xml"/><Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/theme" Target="../theme/theme1.xml"/></Relationships>`); err != nil {
		return err
	}
	if err := writeZipText(zipWriter, "ppt/slideLayouts/slideLayout1.xml", pptxSlideLayout()); err != nil {
		return err
	}
	if err := writeZipText(zipWriter, "ppt/slideLayouts/_rels/slideLayout1.xml.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slideMaster" Target="../slideMasters/slideMaster1.xml"/></Relationships>`); err != nil {
		return err
	}
	if err := writeZipText(zipWriter, "ppt/theme/theme1.xml", pptxTheme()); err != nil {
		return err
	}
	for i, slide := range slides {
		index := i + 1
		imagePath := strings.TrimSpace(slide.ImagePath)
		if imagePath == "" {
			return fmt.Errorf("slide %d image path is empty", slide.SlideNumber)
		}
		if err := writeZipText(zipWriter, fmt.Sprintf("ppt/slides/slide%d.xml", index), pptxSlide()); err != nil {
			return err
		}
		if err := writeZipText(zipWriter, fmt.Sprintf("ppt/slides/_rels/slide%d.xml.rels", index), fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slideLayout" Target="../slideLayouts/slideLayout1.xml"/><Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/image" Target="../media/image%d.png"/></Relationships>`, index)); err != nil {
			return err
		}
		data, err := os.ReadFile(imagePath)
		if err != nil {
			return fmt.Errorf("read slide %d image: %w", slide.SlideNumber, err)
		}
		if err := writeZipBytes(zipWriter, fmt.Sprintf("ppt/media/image%d.png", index), data); err != nil {
			return err
		}
	}
	return nil
}

func writeZipText(zipWriter *zip.Writer, name, content string) error {
	return writeZipBytes(zipWriter, name, []byte(content))
}

func writeZipBytes(zipWriter *zip.Writer, name string, data []byte) error {
	writer, err := zipWriter.Create(name)
	if err != nil {
		return err
	}
	_, err = writer.Write(data)
	return err
}

func pptxContentTypes(slideCount int) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Default Extension="png" ContentType="image/png"/><Default Extension="jpg" ContentType="image/jpeg"/><Default Extension="jpeg" ContentType="image/jpeg"/><Override PartName="/ppt/presentation.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"/><Override PartName="/ppt/slideMasters/slideMaster1.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slideMaster+xml"/><Override PartName="/ppt/slideLayouts/slideLayout1.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slideLayout+xml"/><Override PartName="/ppt/theme/theme1.xml" ContentType="application/vnd.openxmlformats-officedocument.theme+xml"/>`)
	for i := 1; i <= slideCount; i++ {
		fmt.Fprintf(&b, `<Override PartName="/ppt/slides/slide%d.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slide+xml"/>`, i)
	}
	b.WriteString(`</Types>`)
	return b.String()
}

func pptxPresentation(slideCount int) string {
	var ids strings.Builder
	for i := 1; i <= slideCount; i++ {
		fmt.Fprintf(&ids, `<p:sldId id="%d" r:id="rId%d"/>`, 255+i, i+1)
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><p:presentation xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"><p:sldMasterIdLst><p:sldMasterId id="2147483648" r:id="rId1"/></p:sldMasterIdLst><p:sldIdLst>%s</p:sldIdLst><p:sldSz cx="12192000" cy="6858000" type="wide"/><p:notesSz cx="6858000" cy="9144000"/></p:presentation>`, ids.String())
}

func pptxPresentationRels(slideCount int) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slideMaster" Target="slideMasters/slideMaster1.xml"/>`)
	for i := 1; i <= slideCount; i++ {
		fmt.Fprintf(&b, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slide" Target="slides/slide%d.xml"/>`, i+1, i)
	}
	b.WriteString(`</Relationships>`)
	return b.String()
}

func pptxSlide() string {
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><p:sld xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"><p:cSld><p:spTree><p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr><p:grpSpPr><a:xfrm><a:off x="0" y="0"/><a:ext cx="0" cy="0"/><a:chOff x="0" y="0"/><a:chExt cx="0" cy="0"/></a:xfrm></p:grpSpPr><p:pic><p:nvPicPr><p:cNvPr id="2" name="Slide image"/><p:cNvPicPr><a:picLocks noChangeAspect="1"/></p:cNvPicPr><p:nvPr/></p:nvPicPr><p:blipFill><a:blip r:embed="rId2"/><a:stretch><a:fillRect/></a:stretch></p:blipFill><p:spPr><a:xfrm><a:off x="0" y="0"/><a:ext cx="12192000" cy="6858000"/></a:xfrm><a:prstGeom prst="rect"><a:avLst/></a:prstGeom></p:spPr></p:pic></p:spTree></p:cSld><p:clrMapOvr><a:masterClrMapping/></p:clrMapOvr></p:sld>`
}

func pptxSlideMaster() string {
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><p:sldMaster xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"><p:cSld><p:spTree><p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr><p:grpSpPr><a:xfrm><a:off x="0" y="0"/><a:ext cx="0" cy="0"/><a:chOff x="0" y="0"/><a:chExt cx="0" cy="0"/></a:xfrm></p:grpSpPr></p:spTree></p:cSld><p:clrMap bg1="lt1" tx1="dk1" bg2="lt2" tx2="dk2" accent1="accent1" accent2="accent2" accent3="accent3" accent4="accent4" accent5="accent5" accent6="accent6" hlink="hlink" folHlink="folHlink"/><p:sldLayoutIdLst><p:sldLayoutId id="2147483649" r:id="rId1"/></p:sldLayoutIdLst></p:sldMaster>`
}

func pptxSlideLayout() string {
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><p:sldLayout xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" type="blank" preserve="1"><p:cSld name="Blank"><p:spTree><p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr><p:grpSpPr><a:xfrm><a:off x="0" y="0"/><a:ext cx="0" cy="0"/><a:chOff x="0" y="0"/><a:chExt cx="0" cy="0"/></a:xfrm></p:grpSpPr></p:spTree></p:cSld><p:clrMapOvr><a:masterClrMapping/></p:clrMapOvr></p:sldLayout>`
}

func pptxTheme() string {
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><a:theme xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" name="pptflow"><a:themeElements><a:clrScheme name="pptflow"><a:dk1><a:sysClr val="windowText" lastClr="000000"/></a:dk1><a:lt1><a:sysClr val="window" lastClr="FFFFFF"/></a:lt1><a:dk2><a:srgbClr val="1F1F1F"/></a:dk2><a:lt2><a:srgbClr val="F7F7F7"/></a:lt2><a:accent1><a:srgbClr val="4472C4"/></a:accent1><a:accent2><a:srgbClr val="ED7D31"/></a:accent2><a:accent3><a:srgbClr val="A5A5A5"/></a:accent3><a:accent4><a:srgbClr val="FFC000"/></a:accent4><a:accent5><a:srgbClr val="5B9BD5"/></a:accent5><a:accent6><a:srgbClr val="70AD47"/></a:accent6><a:hlink><a:srgbClr val="0563C1"/></a:hlink><a:folHlink><a:srgbClr val="954F72"/></a:folHlink></a:clrScheme><a:fontScheme name="pptflow"><a:majorFont><a:latin typeface="Aptos Display"/></a:majorFont><a:minorFont><a:latin typeface="Aptos"/></a:minorFont></a:fontScheme><a:fmtScheme name="pptflow"><a:fillStyleLst><a:solidFill><a:schemeClr val="phClr"/></a:solidFill></a:fillStyleLst><a:lnStyleLst><a:ln w="9525"><a:solidFill><a:schemeClr val="phClr"/></a:solidFill></a:ln></a:lnStyleLst><a:effectStyleLst><a:effectStyle><a:effectLst/></a:effectStyle></a:effectStyleLst><a:bgFillStyleLst><a:solidFill><a:schemeClr val="phClr"/></a:solidFill></a:bgFillStyleLst></a:fmtScheme></a:themeElements></a:theme>`
}

func validateZipXML(file *zip.File, rootLocalName string) error {
	reader, err := file.Open()
	if err != nil {
		return err
	}
	defer reader.Close()
	var root struct {
		XMLName xml.Name
	}
	if err := xml.NewDecoder(reader).Decode(&root); err != nil {
		return err
	}
	if rootLocalName != "" && root.XMLName.Local != rootLocalName {
		return fmt.Errorf("root element is %s, expected %s", root.XMLName.Local, rootLocalName)
	}
	return nil
}

func rootNameForPPTXPart(name string) string {
	switch filepath.ToSlash(name) {
	case "[Content_Types].xml":
		return "Types"
	case "ppt/presentation.xml":
		return "presentation"
	case "ppt/_rels/presentation.xml.rels":
		return "Relationships"
	default:
		return ""
	}
}

func isSlideXML(name string) bool {
	base := filepath.Base(name)
	if !strings.HasPrefix(base, "slide") || !strings.HasSuffix(base, ".xml") {
		return false
	}
	number := strings.TrimSuffix(strings.TrimPrefix(base, "slide"), ".xml")
	_, err := strconv.Atoi(number)
	return err == nil
}

func putRequiredFile(ctx context.Context, store workflow.ArtifactStore, name, artifactType, producer, path string) (workflow.ArtifactRef, error) {
	f, err := os.Open(path)
	if err != nil {
		return workflow.ArtifactRef{}, err
	}
	defer f.Close()
	return store.Put(ctx, workflow.PutArtifactRequest{
		Name:     name,
		Type:     artifactType,
		Producer: producer,
		Content:  f,
	})
}
