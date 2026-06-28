package pptflow

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/xuanli520/pptflow/internal/workflow"
)

var nodeKinds = []string{
	"pptflow.requirements_fixture",
	"pptflow.template_introspect",
	"pptflow.content_plan",
	"pptflow.slide_plan",
	"pptflow.asset_prepare",
	"pptflow.object_graph_build",
	"pptflow.schema_verify",
	"pptflow.pptx_render",
	"pptflow.editability_verify",
	"pptflow.visual_verify",
	"pptflow.repair_plan",
	"pptflow.package",
}

type Plugin struct{}

func Register(registry *workflow.Registry) error {
	return registry.Register(Plugin{})
}

func (Plugin) Manifest() workflow.PluginManifest {
	return workflow.PluginManifest{ID: "pptflow.phase0", Version: "0.1.0", Kinds: nodeKinds}
}

func (Plugin) Validate(spec workflow.NodeSpec) error {
	for _, kind := range nodeKinds {
		if spec.Kind == kind {
			return nil
		}
	}
	return fmt.Errorf("unknown pptflow node kind %s", spec.Kind)
}

func (p Plugin) Execute(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	switch req.Spec.Kind {
	case "pptflow.requirements_fixture":
		return p.requirements(ctx, req)
	case "pptflow.template_introspect":
		return p.template(ctx, req)
	case "pptflow.content_plan":
		return p.contentPlan(ctx, req)
	case "pptflow.slide_plan":
		return p.slidePlan(ctx, req)
	case "pptflow.asset_prepare":
		return p.assetPrepare(ctx, req)
	case "pptflow.object_graph_build":
		return p.objectGraph(ctx, req)
	case "pptflow.schema_verify":
		return p.schemaVerify(ctx, req)
	case "pptflow.pptx_render":
		return p.pptxRender(ctx, req)
	case "pptflow.editability_verify":
		return p.editabilityVerify(ctx, req)
	case "pptflow.visual_verify":
		return p.visualVerify(ctx, req)
	case "pptflow.repair_plan":
		return p.repairPlan(ctx, req)
	case "pptflow.package":
		return p.packageBundle(ctx, req)
	default:
		return workflow.NodeResult{}, fmt.Errorf("unknown pptflow node kind %s", req.Spec.Kind)
	}
}

func (Plugin) requirements(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	requirements := defaultRequirements(stringConfig(req.Spec.Config, "scenario"))
	if path := stringConfig(req.Spec.Config, "fixture_path"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return workflow.NodeResult{}, err
		}
		if err := json.Unmarshal(data, &requirements); err != nil {
			return workflow.NodeResult{}, err
		}
	}
	ref, err := req.Store.PutJSON(ctx, "requirements.json", "requirements", req.Spec.ID, requirements)
	return withMetrics([]workflow.ArtifactRef{ref}, "local-requirements", err)
}

func (Plugin) template(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	name := "default-16x9"
	if path := stringConfig(req.Spec.Config, "template_path"); path != "" {
		name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	profile := TemplateProfile{
		Name:        name,
		SlideWidth:  12192000,
		SlideHeight: 6858000,
		ThemeColors: map[string]string{
			"background": "FFFFFF",
			"foreground": "14213D",
			"accent":     "2A9D8F",
			"muted":      "E9ECEF",
		},
		Fonts:        []string{"Aptos", "Microsoft YaHei"},
		Layouts:      []string{"cover", "agenda", "content", "table", "chart", "process", "summary"},
		Placeholders: []string{"title", "subtitle", "body", "media"},
	}
	ref, err := req.Store.PutJSON(ctx, "template_profile.json", "template_profile", req.Spec.ID, profile)
	return withMetrics([]workflow.ArtifactRef{ref}, "local-template-introspector", err)
}

func (Plugin) contentPlan(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	var requirements Requirements
	if _, err := req.Store.ReadJSON(ctx, "requirements.json", &requirements); err != nil {
		return workflow.NodeResult{}, err
	}
	plan := ContentPlan{Scenario: requirements.Scenario, Narrative: requirements.Topic}
	for _, title := range phase0SectionTitles(requirements.Scenario) {
		plan.Sections = append(plan.Sections, ContentSection{
			Title: title,
			Points: []string{
				requirements.Topic + " - " + title,
				"Audience: " + requirements.Audience,
				"Tone: " + requirements.Tone,
			},
		})
	}
	ref, err := req.Store.PutJSON(ctx, "content_plan.json", "content_plan", req.Spec.ID, plan)
	return withMetrics([]workflow.ArtifactRef{ref}, stringConfig(req.Spec.Config, "model"), err)
}

func (Plugin) slidePlan(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	var plan ContentPlan
	if _, err := req.Store.ReadJSON(ctx, "content_plan.json", &plan); err != nil {
		return workflow.NodeResult{}, err
	}
	layouts := []struct {
		layout  string
		objects []string
	}{
		{"cover", []string{"text_box", "picture"}},
		{"agenda", []string{"text_box", "shape"}},
		{"content", []string{"text_box", "picture"}},
		{"table", []string{"text_box", "table"}},
		{"chart", []string{"text_box", "chart"}},
		{"process", []string{"text_box", "shape", "connector"}},
		{"content", []string{"text_box", "picture"}},
		{"table", []string{"text_box", "table"}},
		{"chart", []string{"text_box", "chart"}},
		{"summary", []string{"text_box", "shape"}},
	}
	slidePlan := SlidePlan{}
	for i, layout := range layouts {
		title := plan.Sections[i%len(plan.Sections)].Title
		slidePlan.Slides = append(slidePlan.Slides, SlideIntent{
			ID:      fmt.Sprintf("slide-%02d", i+1),
			Title:   title,
			Layout:  layout.layout,
			Objects: layout.objects,
		})
	}
	ref, err := req.Store.PutJSON(ctx, "slide_plan.json", "slide_plan", req.Spec.ID, slidePlan)
	return withMetrics([]workflow.ArtifactRef{ref}, stringConfig(req.Spec.Config, "model"), err)
}

func (Plugin) objectGraph(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	var slidePlan SlidePlan
	var template TemplateProfile
	var assets AssetsManifest
	if _, err := req.Store.ReadJSON(ctx, "slide_plan.json", &slidePlan); err != nil {
		return workflow.NodeResult{}, err
	}
	if _, err := req.Store.ReadJSON(ctx, "template_profile.json", &template); err != nil {
		return workflow.NodeResult{}, err
	}
	_, _ = req.Store.ReadJSON(ctx, "assets_manifest.json", &assets)
	graph := ObjectGraph{SchemaVersion: "pptflow.object_graph.v1", Template: template, Assets: assets}
	for index, intent := range slidePlan.Slides {
		graph.Slides = append(graph.Slides, buildSlide(index, intent, assets))
	}
	ref, err := req.Store.PutJSON(ctx, "ppt_object_graph.json", "ppt_object_graph", req.Spec.ID, graph)
	return withMetrics([]workflow.ArtifactRef{ref}, stringConfig(req.Spec.Config, "model"), err)
}

func (Plugin) assetPrepare(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	var slidePlan SlidePlan
	if _, err := req.Store.ReadJSON(ctx, "slide_plan.json", &slidePlan); err != nil {
		return workflow.NodeResult{}, err
	}
	manifest := AssetsManifest{}
	refs := []workflow.ArtifactRef{}
	for _, slide := range slidePlan.Slides {
		if !contains(slide.Objects, "picture") {
			continue
		}
		id := slide.ID + "-image"
		prompt := imagePromptForSlide(slide)
		outputPath, err := req.Store.Path("assets/images/" + id + ".png")
		if err != nil {
			return workflow.NodeResult{}, err
		}
		asset := ImageAsset{ID: id, Path: outputPath, Prompt: prompt, Model: stringConfig(req.Spec.Config, "model"), Size: stringConfig(req.Spec.Config, "size"), Source: "placeholder"}
		if req.Runtimes.Image != nil && req.Runtimes.Image.Configured() {
			result, err := req.Runtimes.Image.Generate(ctx, workflow.ImageRequest{
				Prompt:         prompt,
				Size:           stringConfig(req.Spec.Config, "size"),
				Quality:        stringConfig(req.Spec.Config, "quality"),
				OutputPath:     outputPath,
				TimeoutSeconds: 180,
			})
			if err == nil {
				asset.Path = result.Path
				asset.Model = result.Model
				asset.Size = result.Size
				asset.Source = "image2"
			} else {
				return workflow.NodeResult{}, fmt.Errorf("generate image asset %s: %w", id, err)
			}
		} else if err := writePlaceholderPNG(outputPath); err != nil {
			return workflow.NodeResult{}, err
		}
		ref, err := putFileArtifact(ctx, req.Store, "assets_manifest_files/"+id+".png", "image", req.Spec.ID, asset.Path)
		if err != nil {
			return workflow.NodeResult{}, err
		}
		asset.Path = ref.Path
		refs = append(refs, ref)
		manifest.Images = append(manifest.Images, asset)
	}
	ref, err := req.Store.PutJSON(ctx, "assets_manifest.json", "assets_manifest", req.Spec.ID, manifest)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	refs = append(refs, ref)
	return workflow.NodeResult{Artifacts: refs, Metrics: workflow.NodeMetrics{Model: stringConfig(req.Spec.Config, "model")}}, nil
}

func (Plugin) schemaVerify(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	var graph ObjectGraph
	if _, err := req.Store.ReadJSON(ctx, "ppt_object_graph.json", &graph); err != nil {
		return workflow.NodeResult{}, err
	}
	report := validateObjectGraph(graph)
	ref, err := req.Store.PutJSON(ctx, "schema_report.json", "schema_report", req.Spec.ID, report)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	if !report.OK {
		return workflow.NodeResult{Artifacts: []workflow.ArtifactRef{ref}, Metrics: workflow.NodeMetrics{FailureType: "schema"}}, errors.New(strings.Join(report.Errors, "; "))
	}
	return workflow.NodeResult{Artifacts: []workflow.ArtifactRef{ref}}, nil
}

func (Plugin) pptxRender(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	var graph ObjectGraph
	if _, err := req.Store.ReadJSON(ctx, "ppt_object_graph.json", &graph); err != nil {
		return workflow.NodeResult{}, err
	}
	data, err := RenderPPTX(graph)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	ref, err := req.Store.Put(ctx, workflow.PutArtifactRequest{
		Name:     "deck.pptx",
		Type:     "pptx",
		Producer: req.Spec.ID,
		Content:  bytes.NewReader(data),
	})
	return withMetrics([]workflow.ArtifactRef{ref}, "deterministic-openxml-renderer", err)
}

func (Plugin) editabilityVerify(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	var graph ObjectGraph
	if _, err := req.Store.ReadJSON(ctx, "ppt_object_graph.json", &graph); err != nil {
		return workflow.NodeResult{}, err
	}
	report := editabilityReport(graph)
	ref, err := req.Store.PutJSON(ctx, "editability_report.json", "editability_report", req.Spec.ID, report)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	if !report.OK {
		return workflow.NodeResult{Artifacts: []workflow.ArtifactRef{ref}, Metrics: workflow.NodeMetrics{FailureType: "editability"}}, errors.New(strings.Join(report.Errors, "; "))
	}
	return workflow.NodeResult{Artifacts: []workflow.ArtifactRef{ref}}, nil
}

func (Plugin) visualVerify(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	var graph ObjectGraph
	if _, err := req.Store.ReadJSON(ctx, "ppt_object_graph.json", &graph); err != nil {
		return workflow.NodeResult{}, err
	}
	report := visualReport(graph)
	ref, err := req.Store.PutJSON(ctx, "visual_report.json", "visual_report", req.Spec.ID, report)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	if !report.OK {
		return workflow.NodeResult{Artifacts: []workflow.ArtifactRef{ref}, Metrics: workflow.NodeMetrics{FailureType: "visual"}}, errors.New(strings.Join(report.Errors, "; "))
	}
	return workflow.NodeResult{Artifacts: []workflow.ArtifactRef{ref}}, nil
}

func (Plugin) repairPlan(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	var edit EditabilityReport
	var visual VisualReport
	_, _ = req.Store.ReadJSON(ctx, "editability_report.json", &edit)
	_, _ = req.Store.ReadJSON(ctx, "visual_report.json", &visual)
	plan := RepairPlan{}
	if !edit.OK {
		plan.Required = true
		plan.Actions = append(plan.Actions, edit.Errors...)
	}
	if !visual.OK {
		plan.Required = true
		plan.Actions = append(plan.Actions, visual.Errors...)
	}
	if len(plan.Actions) == 0 {
		plan.Actions = []string{"no repair required"}
	}
	ref, err := req.Store.PutJSON(ctx, "repair_plan.json", "repair_plan", req.Spec.ID, plan)
	return withMetrics([]workflow.ArtifactRef{ref}, stringConfig(req.Spec.Config, "model"), err)
}

func (Plugin) packageBundle(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	artifacts, err := req.Store.List(ctx, "")
	if err != nil {
		return workflow.NodeResult{}, err
	}
	ref, err := req.Store.PutJSON(ctx, "delivery_bundle.json", "delivery_bundle", req.Spec.ID, map[string]any{
		"artifacts": artifacts,
		"status":    "phase0_initialized",
	})
	return withMetrics([]workflow.ArtifactRef{ref}, "local-packager", err)
}

func defaultRequirements(scenario string) Requirements {
	switch strings.TrimSpace(scenario) {
	case "business_plan":
		return Requirements{Scenario: "business_plan", Topic: "智能会议助手商业计划书", Audience: "投资人", Tone: "专业、可信、增长导向", SlideCount: 10, MustHave: []string{"market", "product", "business model", "financials"}}
	case "roadshow":
		return Requirements{Scenario: "roadshow", Topic: "AI 硬件新品发布路演", Audience: "渠道伙伴与媒体", Tone: "简洁、有冲击力、产品导向", SlideCount: 10, MustHave: []string{"launch story", "features", "ecosystem", "call to action"}}
	default:
		return Requirements{Scenario: "performance_review", Topic: "年度述职汇报", Audience: "管理层", Tone: "清晰、务实、结果导向", SlideCount: 10, MustHave: []string{"results", "metrics", "learnings", "next plan"}}
	}
}

func phase0SectionTitles(scenario string) []string {
	switch strings.TrimSpace(scenario) {
	case "business_plan":
		return []string{"机会判断", "市场规模", "产品方案", "商业模式", "增长路径", "财务预测"}
	case "roadshow":
		return []string{"发布主题", "用户痛点", "核心卖点", "场景演示", "生态合作", "上市计划"}
	default:
		return []string{"年度目标", "关键成果", "核心指标", "项目复盘", "问题改进", "下一阶段计划"}
	}
}

func imagePromptForSlide(slide SlideIntent) string {
	return strings.Join([]string{
		"Use case: productivity-visual",
		"Asset type: editable PowerPoint picture object",
		"Primary request: create a polished business presentation visual for slide " + slide.Title,
		"Style/medium: clean premium business illustration, suitable for executive decks",
		"Composition/framing: landscape 16:9, central subject with generous margins, no text",
		"Lighting/mood: bright, professional, confident",
		"Constraints: no watermark, no logo, no readable text, no UI screenshots",
	}, "\n")
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func imageIDForSlide(slideID string, assets AssetsManifest) string {
	target := slideID + "-image"
	for _, asset := range assets.Images {
		if asset.ID == target {
			return asset.ID
		}
	}
	if len(assets.Images) > 0 {
		return assets.Images[0].ID
	}
	return "placeholder"
}

func writePlaceholderPNG(path string) error {
	data, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAFgwJ/lr9mogAAAABJRU5ErkJggg==")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func putFileArtifact(ctx context.Context, store workflow.ArtifactStore, name, artifactType, producer, path string) (workflow.ArtifactRef, error) {
	file, err := os.Open(path)
	if err != nil {
		return workflow.ArtifactRef{}, err
	}
	defer file.Close()
	return store.Put(ctx, workflow.PutArtifactRequest{Name: name, Type: artifactType, Producer: producer, Content: file})
}

func buildSlide(index int, intent SlideIntent, assets AssetsManifest) Slide {
	slide := Slide{ID: intent.ID, Title: intent.Title, Layout: intent.Layout}
	slide.Objects = append(slide.Objects, PPTObject{ID: intent.ID + "-title", Type: "text_box", Text: intent.Title, X: 0.7, Y: 0.35, W: 11.8, H: 0.55, Style: map[string]string{"font_size": "28", "fill": "FFFFFF"}})
	for i, objectType := range intent.Objects {
		switch objectType {
		case "text_box":
			slide.Objects = append(slide.Objects, PPTObject{ID: fmt.Sprintf("%s-text-%d", intent.ID, i), Type: "text_box", Text: intent.Title + "：核心信息与证据链", X: 0.9, Y: 1.35, W: 5.6, H: 1.5})
		case "picture":
			slide.Objects = append(slide.Objects, PPTObject{ID: fmt.Sprintf("%s-picture-%d", intent.ID, i), Type: "picture", X: 7.0, Y: 1.25, W: 4.9, H: 2.8, Image: imageIDForSlide(intent.ID, assets)})
		case "table":
			slide.Objects = append(slide.Objects, PPTObject{ID: fmt.Sprintf("%s-table-%d", intent.ID, i), Type: "table", X: 0.9, Y: 1.5, W: 10.8, H: 3.4, Rows: [][]string{{"指标", "当前", "目标"}, {"效率", "72%", "90%"}, {"成本", "-18%", "-25%"}, {"满意度", "8.1", "9.0"}}})
		case "chart":
			slide.Objects = append(slide.Objects, PPTObject{ID: fmt.Sprintf("%s-chart-%d", intent.ID, i), Type: "chart", X: 1.1, Y: 1.45, W: 10.2, H: 3.6, Series: []ChartSeries{{Name: "增长", Labels: []string{"Q1", "Q2", "Q3", "Q4"}, Values: []float64{24, 38, 57, 81}}}})
		case "shape":
			for step := 0; step < 3; step++ {
				slide.Objects = append(slide.Objects, PPTObject{ID: fmt.Sprintf("%s-shape-%d-%d", intent.ID, i, step), Type: "shape", Text: fmt.Sprintf("Step %d", step+1), Shape: "roundRect", X: 1.1 + float64(step)*3.5, Y: 2.0 + float64(index%2)*0.2, W: 2.4, H: 0.9})
			}
		case "connector":
			slide.Objects = append(slide.Objects, PPTObject{ID: fmt.Sprintf("%s-connector-%d", intent.ID, i), Type: "connector", X: 3.5, Y: 2.45, W: 3.1, H: 0.0})
			slide.Objects = append(slide.Objects, PPTObject{ID: fmt.Sprintf("%s-connector-%d-b", intent.ID, i), Type: "connector", X: 7.0, Y: 2.45, W: 3.1, H: 0.0})
		}
	}
	return slide
}

func validateObjectGraph(graph ObjectGraph) SchemaReport {
	report := SchemaReport{OK: true, SlideCount: len(graph.Slides)}
	if graph.SchemaVersion != "pptflow.object_graph.v1" {
		report.OK = false
		report.Errors = append(report.Errors, "unsupported schema version")
	}
	if len(graph.Slides) < 10 || len(graph.Slides) > 15 {
		report.OK = false
		report.Errors = append(report.Errors, "slide count must be 10-15")
	}
	for _, slide := range graph.Slides {
		if len(slide.Objects) == 0 {
			report.OK = false
			report.Errors = append(report.Errors, slide.ID+" has no objects")
		}
		for _, object := range slide.Objects {
			if object.ID == "" || object.Type == "" {
				report.OK = false
				report.Errors = append(report.Errors, slide.ID+" has object without id or type")
			}
			if object.W < 0 || object.H < 0 {
				report.OK = false
				report.Errors = append(report.Errors, object.ID+" has invalid size")
			}
		}
	}
	return report
}

func editabilityReport(graph ObjectGraph) EditabilityReport {
	report := EditabilityReport{OK: true, SlideCount: len(graph.Slides), Counts: map[string]int{}}
	for _, slide := range graph.Slides {
		for _, object := range slide.Objects {
			report.Counts[object.Type]++
		}
	}
	for _, required := range []string{"text_box", "table", "chart", "shape", "connector", "picture"} {
		if report.Counts[required] == 0 {
			report.OK = false
			report.Errors = append(report.Errors, required+" object missing")
		}
	}
	if report.SlideCount < 10 || report.SlideCount > 15 {
		report.OK = false
		report.Errors = append(report.Errors, "slide count must be 10-15")
	}
	return report
}

func visualReport(graph ObjectGraph) VisualReport {
	report := VisualReport{OK: true}
	for _, slide := range graph.Slides {
		if strings.TrimSpace(slide.Title) == "" {
			report.OK = false
			report.Errors = append(report.Errors, slide.ID+" title is empty")
		}
		if len(slide.Objects) < 2 {
			report.Warnings = append(report.Warnings, slide.ID+" has low object density")
		}
		for _, object := range slide.Objects {
			if object.X+object.W > 13.4 || object.Y+object.H > 7.5 {
				report.OK = false
				report.Errors = append(report.Errors, object.ID+" overflows 16:9 canvas")
			}
		}
	}
	return report
}

func withMetrics(refs []workflow.ArtifactRef, model string, err error) (workflow.NodeResult, error) {
	return workflow.NodeResult{
		Artifacts: refs,
		Metrics: workflow.NodeMetrics{
			Model:      strings.TrimSpace(model),
			TokenUsage: workflow.TokenUsage{Input: 0, Output: 0, Total: 0},
		},
	}, err
}

func stringConfig(config map[string]any, key string) string {
	if len(config) == 0 {
		return ""
	}
	value, _ := config[key].(string)
	return strings.TrimSpace(value)
}
