package promptflow

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/xuanli520/pptflow/internal/workflow"
)

type PreviewManifest struct {
	SchemaVersion string         `json:"schema_version"`
	Renderer      string         `json:"renderer"`
	Slides        []PreviewSlide `json:"slides"`
}

type PreviewSlide struct {
	SlideNumber int    `json:"slide_number"`
	ImagePath   string `json:"image_path"`
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
}

type VisualQAReport struct {
	SchemaVersion               string   `json:"schema_version"`
	Pass                        bool     `json:"pass"`
	SlideCount                  int      `json:"slide_count"`
	RenderSize                  string   `json:"render_size"`
	EditableTextShapeCount      int      `json:"editable_text_shape_count"`
	ShapeCount                  int      `json:"shape_count"`
	PictureCount                int      `json:"picture_count"`
	FullSlideImageDetected      bool     `json:"full_slide_image_detection"`
	KeyTextCoverage             float64  `json:"key_text_coverage"`
	ShapeStyleCoverage          float64  `json:"shape_style_coverage"`
	GeometricEditability        float64  `json:"geometric_editability"`
	TransparentResourceCoverage float64  `json:"transparent_resource_coverage"`
	TextPictureOverlap          float64  `json:"text_picture_overlap"`
	TextTextOverlap             float64  `json:"text_text_overlap"`
	CheckerboardResourceCount   int      `json:"checkerboard_resource_count"`
	InvalidTransparentResources int      `json:"invalid_transparent_resource_count"`
	ColorTokenHitRate           float64  `json:"color_token_hit_rate"`
	BBoxDrift                   float64  `json:"bbox_drift"`
	Warnings                    []string `json:"overlap_clipping_warnings,omitempty"`
}

type visualQAThresholds struct {
	VisibleTextEditability float64
	GeometricEditability   float64
	ShapeStyleCoverage     float64
	TransparentCoverage    float64
	TextPictureOverlap     float64
	TextTextOverlap        float64
}

func (Plugin) renderPPTXPreview(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	var analysis LayoutAnalysis
	if _, err := req.Store.ReadJSON(ctx, "layout_analysis.json", &analysis); err != nil {
		return workflow.NodeResult{}, err
	}
	manifest := PreviewManifest{SchemaVersion: "pptflow.preview_manifest.v1", Renderer: "source-slide-reference"}
	refs := []workflow.ArtifactRef{}
	for _, slide := range analysis.Slides {
		source := strings.TrimSpace(slide.ImagePath)
		if source == "" {
			return workflow.NodeResult{}, fmt.Errorf("slide %d image path is empty", slide.SlideNumber)
		}
		name := fmt.Sprintf("preview/slide_%02d.png", slide.SlideNumber)
		outputPath, err := req.Store.Path(name)
		if err != nil {
			return workflow.NodeResult{}, err
		}
		if err := copyFile(outputPath, source); err != nil {
			return workflow.NodeResult{}, err
		}
		ref, err := req.Store.Register(ctx, workflow.RegisterArtifactRequest{Name: name, Type: "image", Producer: req.Spec.ID, Path: outputPath})
		if err != nil {
			return workflow.NodeResult{}, err
		}
		refs = append(refs, ref)
		manifest.Slides = append(manifest.Slides, PreviewSlide{SlideNumber: slide.SlideNumber, ImagePath: outputPath})
	}
	ref, err := req.Store.PutJSON(ctx, "preview_manifest.json", "preview_manifest", req.Spec.ID, manifest)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	refs = append(refs, ref)
	return withMetrics(refs, manifest.Renderer, nil)
}

func (Plugin) visualQA(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	var analysis LayoutAnalysis
	if _, err := req.Store.ReadJSON(ctx, "layout_analysis.json", &analysis); err != nil {
		return workflow.NodeResult{}, err
	}
	var resources ExtractedResourceManifest
	if _, err := req.Store.ReadJSON(ctx, "extracted_resource_manifest.json", &resources); err != nil {
		return workflow.NodeResult{}, err
	}
	var styleSpec StyleSpec
	_, _ = req.Store.ReadJSON(ctx, "style_spec.json", &styleSpec)
	deckPath := filepath.Join(req.WorkspaceRoot, "output", "deck.pptx")
	xmlBySlide, err := pptxSlideXML(deckPath)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	report := buildVisualQAReport(analysis, resources, styleSpec, xmlBySlide)
	applyQAThresholds(&report, qaThresholdsFromConfig(req.Spec.Config, analysis))
	ref, putErr := req.Store.PutJSON(ctx, "visual_qa_report.json", "visual_qa_report", req.Spec.ID, report)
	if putErr != nil {
		return workflow.NodeResult{}, putErr
	}
	return withMetrics([]workflow.ArtifactRef{ref}, "local-visual-qa", nil)
}

func (Plugin) repairPlan(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	var report VisualQAReport
	_, _ = req.Store.ReadJSON(ctx, "visual_qa_report.json", &report)
	ref, err := req.Store.PutJSON(ctx, "repair_plan.json", "repair_plan", req.Spec.ID, map[string]any{
		"schema_version": "pptflow.repair_plan.v1",
		"required":       !report.Pass,
		"warnings":       report.Warnings,
	})
	return withMetrics([]workflow.ArtifactRef{ref}, "local-repair-plan", err)
}

func (Plugin) repairApply(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	var report VisualQAReport
	_, _ = req.Store.ReadJSON(ctx, "visual_qa_report.json", &report)
	ref, err := req.Store.PutJSON(ctx, "repair_apply.json", "repair_apply", req.Spec.ID, map[string]any{
		"schema_version": "pptflow.repair_apply.v1",
		"applied":        false,
	})
	if err != nil {
		return workflow.NodeResult{}, err
	}
	if !report.Pass && strings.EqualFold(stringConfig(req.Spec.Config, "fallback_policy", "strict"), "strict") {
		return workflow.NodeResult{Artifacts: []workflow.ArtifactRef{ref}}, fmt.Errorf("visual qa failed: %s", strings.Join(report.Warnings, "; "))
	}
	return withMetrics([]workflow.ArtifactRef{ref}, "local-repair-apply", nil)
}

func buildVisualQAReport(analysis LayoutAnalysis, resources ExtractedResourceManifest, styleSpec StyleSpec, xmlBySlide map[int]string) VisualQAReport {
	report := VisualQAReport{
		SchemaVersion: "pptflow.visual_qa_report.v1",
		SlideCount:    len(analysis.Slides),
		RenderSize:    fmt.Sprintf("%.3fx%.3f", analysis.SlideWidth, analysis.SlideHeight),
		BBoxDrift:     0,
	}
	totalTexts, coveredTexts := 0, 0
	totalGeometry, coveredGeometry := 0, 0
	totalStyledShapes, styledShapes := 0, 0
	totalTransparent, transparentOK := 0, 0
	colorTokens := colorTokens(styleSpec)
	colorHits := map[string]bool{}
	for _, slide := range analysis.Slides {
		xml := xmlBySlide[slide.SlideNumber]
		report.EditableTextShapeCount += strings.Count(xml, "<a:t>")
		report.ShapeCount += strings.Count(xml, "<p:sp>")
		report.PictureCount += strings.Count(xml, "<p:pic>")
		if strings.Contains(xml, `name="Slide image"`) {
			report.FullSlideImageDetected = true
		}
		for _, token := range colorTokens {
			if strings.Contains(strings.ToUpper(xml), token) {
				colorHits[token] = true
			}
		}
		regions := slideRenderableRegions(slide)
		collectOverlapQA(slide.SlideNumber, regions, &report)
		for _, region := range regions {
			text := strings.TrimSpace(regionText(region))
			if text != "" {
				totalTexts++
				if textCoveredBySlideXML(xml, text) {
					coveredTexts++
				}
			}
			if strings.EqualFold(region.Type, "shape") || strings.EqualFold(region.Type, "line") {
				totalGeometry++
				if strings.Contains(xml, `name="`+xmlAttr(region.ID)+`"`) {
					coveredGeometry++
				}
				if strings.EqualFold(region.Type, "shape") {
					totalStyledShapes++
					if region.Shape != nil && region.Fill != nil && region.Stroke != nil && region.Shadow != nil {
						styledShapes++
					}
				}
			}
			if wantsTransparentResource(region) {
				totalTransparent++
				quality, found := resourceQualityForRegion(resources, slide.SlideNumber, region.ID)
				if found && quality.Pass {
					transparentOK++
				} else {
					report.InvalidTransparentResources++
					if !found {
						appendQAWarn(&report, fmt.Sprintf("slide %d region %s missing transparent resource", slide.SlideNumber, region.ID))
					} else {
						appendQAWarn(&report, fmt.Sprintf("slide %d region %s transparent resource invalid alpha=%.3f checkerboard=%v matte=%v", slide.SlideNumber, region.ID, quality.AlphaCoverage, quality.CheckerboardDetected, quality.MatteDetected))
					}
				}
				if quality.CheckerboardDetected {
					report.CheckerboardResourceCount++
				}
			}
		}
	}
	report.KeyTextCoverage = ratio(coveredTexts, totalTexts)
	report.GeometricEditability = ratio(coveredGeometry, totalGeometry)
	report.ShapeStyleCoverage = ratio(styledShapes, totalStyledShapes)
	report.TransparentResourceCoverage = ratio(transparentOK, totalTransparent)
	report.ColorTokenHitRate = ratio(len(colorHits), len(colorTokens))
	return report
}

func applyQAThresholds(report *VisualQAReport, thresholds visualQAThresholds) {
	if report.FullSlideImageDetected {
		report.Warnings = append(report.Warnings, "full-slide image fallback detected")
	}
	if report.KeyTextCoverage < thresholds.VisibleTextEditability {
		report.Warnings = append(report.Warnings, "key text coverage below threshold")
	}
	if report.GeometricEditability < thresholds.GeometricEditability {
		report.Warnings = append(report.Warnings, "geometric editability below threshold")
	}
	if report.ShapeStyleCoverage < thresholds.ShapeStyleCoverage {
		report.Warnings = append(report.Warnings, "shape style coverage below threshold")
	}
	if report.TransparentResourceCoverage < thresholds.TransparentCoverage {
		report.Warnings = append(report.Warnings, "transparent resource coverage below threshold")
	}
	if report.TextPictureOverlap > thresholds.TextPictureOverlap {
		report.Warnings = append(report.Warnings, "text-picture overlap above threshold")
	}
	if report.TextTextOverlap > thresholds.TextTextOverlap {
		report.Warnings = append(report.Warnings, "text-text overlap above threshold")
	}
	if report.CheckerboardResourceCount > 0 {
		report.Warnings = append(report.Warnings, "checkerboard transparent resource detected")
	}
	if report.InvalidTransparentResources > 0 {
		report.Warnings = append(report.Warnings, "invalid transparent resource detected")
	}
	report.Pass = len(report.Warnings) == 0
}

func qaThresholdsFromConfig(config map[string]any, analysis LayoutAnalysis) visualQAThresholds {
	thresholds := visualQAThresholds{
		VisibleTextEditability: doubleConfig(config, "visible_text_editability", 0.95),
		GeometricEditability:   doubleConfig(config, "geometric_editability", 0.90),
		ShapeStyleCoverage:     doubleConfig(config, "shape_style_coverage", 1.0),
		TransparentCoverage:    doubleConfig(config, "transparent_resource_coverage", 1.0),
		TextPictureOverlap:     doubleConfig(config, "text_picture_overlap_max", 0.12),
		TextTextOverlap:        doubleConfig(config, "text_text_overlap_max", 0.08),
	}
	if !isLayoutAnalysisV2(analysis) {
		thresholds.ShapeStyleCoverage = 0
	}
	return thresholds
}

func copyFile(dst, src string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func textCoveredBySlideXML(slideXML, text string) bool {
	parts := strings.FieldsFunc(text, func(r rune) bool {
		return r == '\n' || r == '\r'
	})
	checked := 0
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		checked++
		if !strings.Contains(slideXML, xmlText(part)) {
			return false
		}
	}
	return checked > 0
}

func pptxSlideXML(path string) (map[int]string, error) {
	reader, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	result := map[int]string{}
	for _, file := range reader.File {
		name := filepath.ToSlash(file.Name)
		if !isSlideXML(name) {
			continue
		}
		indexText := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(name), "slide"), ".xml")
		index, err := strconvAtoi(indexText)
		if err != nil {
			continue
		}
		rc, err := file.Open()
		if err != nil {
			return nil, err
		}
		data, readErr := io.ReadAll(rc)
		closeErr := rc.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		result[index] = string(data)
	}
	return result, nil
}

func strconvAtoi(value string) (int, error) {
	var result int
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("invalid integer %s", value)
		}
		result = result*10 + int(r-'0')
	}
	return result, nil
}

func ratio(num, den int) float64 {
	if den <= 0 {
		return 1
	}
	return float64(num) / float64(den)
}

type regionBBox struct {
	x float64
	y float64
	w float64
	h float64
}

func collectOverlapQA(slideNumber int, regions []LayoutRegion, report *VisualQAReport) {
	for i := 0; i < len(regions); i++ {
		for j := i + 1; j < len(regions); j++ {
			a := regions[i]
			b := regions[j]
			if sameEmptyBox(a, b) {
				continue
			}
			aText := isTextLikeRegion(a)
			bText := isTextLikeRegion(b)
			aPicture := isPictureLikeOverlapRegion(a)
			bPicture := isPictureLikeOverlapRegion(b)
			if aText && bPicture {
				value := overlapRatioForBase(a, b)
				report.TextPictureOverlap = maxFloat(report.TextPictureOverlap, value)
				if value > 0.12 {
					appendQAWarn(report, fmt.Sprintf("slide %d text %s overlaps picture %s ratio %.3f", slideNumber, a.ID, b.ID, value))
				}
			}
			if bText && aPicture {
				value := overlapRatioForBase(b, a)
				report.TextPictureOverlap = maxFloat(report.TextPictureOverlap, value)
				if value > 0.12 {
					appendQAWarn(report, fmt.Sprintf("slide %d text %s overlaps picture %s ratio %.3f", slideNumber, b.ID, a.ID, value))
				}
			}
			if aText && bText {
				value := overlapRatioForSmaller(a, b)
				report.TextTextOverlap = maxFloat(report.TextTextOverlap, value)
				if value > 0.08 {
					appendQAWarn(report, fmt.Sprintf("slide %d text %s overlaps text %s ratio %.3f", slideNumber, a.ID, b.ID, value))
				}
			}
		}
	}
}

func isTextLikeRegion(region LayoutRegion) bool {
	if isTextRegion(region.Type) {
		return true
	}
	return shapeHasInlineText(region)
}

func isPictureLikeOverlapRegion(region LayoutRegion) bool {
	if !isVisualResourceRegion(region.Type) {
		return false
	}
	return !isBackgroundRegion(region)
}

func sameEmptyBox(a, b LayoutRegion) bool {
	return a.W <= 0 || a.H <= 0 || b.W <= 0 || b.H <= 0
}

func overlapRatioForBase(base, other LayoutRegion) float64 {
	intersection := boxIntersection(regionBox(base), regionBox(other))
	area := boxArea(regionBox(base))
	if area <= 0 {
		return 0
	}
	return intersection / area
}

func overlapRatioForSmaller(a, b LayoutRegion) float64 {
	intersection := boxIntersection(regionBox(a), regionBox(b))
	area := minFloat(boxArea(regionBox(a)), boxArea(regionBox(b)))
	if area <= 0 {
		return 0
	}
	return intersection / area
}

func regionBox(region LayoutRegion) regionBBox {
	return regionBBox{x: region.X, y: region.Y, w: region.W, h: region.H}
}

func boxIntersection(a, b regionBBox) float64 {
	x1 := maxFloat(a.x, b.x)
	y1 := maxFloat(a.y, b.y)
	x2 := minFloat(a.x+a.w, b.x+b.w)
	y2 := minFloat(a.y+a.h, b.y+b.h)
	if x2 <= x1 || y2 <= y1 {
		return 0
	}
	return (x2 - x1) * (y2 - y1)
}

func boxArea(box regionBBox) float64 {
	if box.w <= 0 || box.h <= 0 {
		return 0
	}
	return box.w * box.h
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func appendQAWarn(report *VisualQAReport, warning string) {
	if len(report.Warnings) >= 30 {
		return
	}
	report.Warnings = append(report.Warnings, warning)
}

func resourceHasAlpha(resources ExtractedResourceManifest, slideNumber int, regionID string) bool {
	quality, found := resourceQualityForRegion(resources, slideNumber, regionID)
	return found && quality.HasAlpha
}

func resourceQualityForRegion(resources ExtractedResourceManifest, slideNumber int, regionID string) (transparentResourceQuality, bool) {
	id := resourceIDForRegion(slideNumber, regionID)
	for _, resource := range resources.Resources {
		if resource.ID == id {
			return inspectTransparentResource(resource.FilePath, extractionMatteColor()), true
		}
	}
	return transparentResourceQuality{}, false
}

func colorTokens(styleSpec StyleSpec) []string {
	values := []string{
		styleSpec.ColorScheme.Primary,
		styleSpec.ColorScheme.Secondary,
		styleSpec.ColorScheme.Background,
		styleSpec.ColorScheme.Foreground,
		styleSpec.ColorScheme.Accent,
		styleSpec.ColorScheme.Muted,
	}
	result := []string{}
	for _, value := range values {
		value = normalizeHexColor(value)
		if value != "1F1F1F" {
			result = append(result, value)
		}
	}
	return result
}
