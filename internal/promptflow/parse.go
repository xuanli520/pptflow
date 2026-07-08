package promptflow

import (
	"encoding/json"
	"fmt"
	"strings"
)

// extractJSON extracts the first JSON object from text that may contain markdown fences.
func extractJSON(text string) string {
	text = strings.TrimSpace(text)

	// Strip markdown code fences
	for {
		if strings.HasPrefix(text, "```") {
			idx := strings.Index(text, "\n")
			if idx < 0 {
				break
			}
			text = strings.TrimSpace(text[idx+1:])
		} else {
			break
		}
	}
	if strings.HasSuffix(text, "```") {
		text = strings.TrimSpace(strings.TrimSuffix(text, "```"))
	}

	// Find first { and matching }
	start := strings.Index(text, "{")
	if start < 0 {
		return text
	}
	depth := 0
	for i := start; i < len(text); i++ {
		switch text[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return text[start : i+1]
			}
		}
	}
	return text[start:]
}

func parseOptimizedRequirements(text string) (OptimizedRequirements, error) {
	jsonStr := extractJSON(text)
	var req OptimizedRequirements
	if err := json.Unmarshal([]byte(jsonStr), &req); err != nil {
		return OptimizedRequirements{}, fmt.Errorf("invalid JSON: %w\nraw text: %.200s", err, text)
	}
	if req.SchemaVersion == "" {
		req.SchemaVersion = "pptflow.optimized_requirements.v1"
	}
	if req.SlideCount <= 0 {
		req.SlideCount = 10
	}
	if req.Locale == "" {
		req.Locale = "zh-CN"
	}
	if len(req.KeyMessages) == 0 {
		req.KeyMessages = []string{req.Purpose}
	}
	return req, nil
}

type outlineStyleWrapper struct {
	Outline   Outline   `json:"outline"`
	StyleSpec StyleSpec `json:"style_spec"`
}

func parseOutlineAndStyle(text string) (Outline, StyleSpec, error) {
	jsonStr := extractJSON(text)
	var wrapper outlineStyleWrapper
	if err := json.Unmarshal([]byte(jsonStr), &wrapper); err != nil {
		return Outline{}, StyleSpec{}, fmt.Errorf("invalid JSON: %w\nraw text: %.200s", err, text)
	}
	outline := wrapper.Outline
	style := wrapper.StyleSpec

	if outline.SchemaVersion == "" {
		outline.SchemaVersion = "pptflow.outline.v1"
	}
	if style.SchemaVersion == "" {
		style.SchemaVersion = "pptflow.style_spec.v1"
	}
	if len(outline.Slides) == 0 {
		return Outline{}, StyleSpec{}, fmt.Errorf("outline has no slides")
	}
	if style.StylePromptSuffix == "" {
		style.StylePromptSuffix = fmt.Sprintf("Professional presentation design. %s style. %s mood. Clean layout, high contrast, suitable for large screen projection.",
			style.StylePack, style.VisualMood)
	}
	return outline, style, nil
}

func parseLayoutAnalysis(text string) (LayoutAnalysis, error) {
	jsonStr := extractJSON(text)
	var analysis LayoutAnalysis
	if err := json.Unmarshal([]byte(jsonStr), &analysis); err != nil {
		return LayoutAnalysis{}, fmt.Errorf("invalid JSON: %w\nraw text: %.200s", err, text)
	}
	if analysis.SchemaVersion == "" {
		analysis.SchemaVersion = "pptflow.layout_analysis.v1"
	}
	if analysis.SlideWidth <= 0 {
		analysis.SlideWidth = 13.333
	}
	if analysis.SlideHeight <= 0 {
		analysis.SlideHeight = 7.5
	}
	if len(analysis.Slides) == 0 {
		return LayoutAnalysis{}, fmt.Errorf("layout analysis has no slides")
	}
	normalizeLayoutAnalysis(&analysis)
	return analysis, nil
}

func normalizeLayoutAnalysis(analysis *LayoutAnalysis) {
	if analysis == nil {
		return
	}
	width := analysis.SlideWidth
	if width <= 0 {
		width = 13.333
	}
	height := analysis.SlideHeight
	if height <= 0 {
		height = 7.5
	}
	for i := range analysis.Slides {
		normalizeSlideLayout(&analysis.Slides[i])
		clampSlideLayout(&analysis.Slides[i], width, height)
	}
}

func normalizeSlideLayout(slide *SlideLayout) {
	if slide == nil {
		return
	}
	if len(slide.Regions) == 0 && len(slide.Elements) > 0 {
		slide.Regions = append([]LayoutRegion(nil), slide.Elements...)
	}
	if len(slide.Elements) == 0 && len(slide.Regions) > 0 {
		slide.Elements = append([]LayoutElement(nil), slide.Regions...)
	}
	for i := range slide.Regions {
		normalizeLayoutRegion(&slide.Regions[i])
	}
	for i := range slide.Elements {
		normalizeLayoutRegion(&slide.Elements[i])
	}
}

func clampSlideLayout(slide *SlideLayout, width, height float64) {
	for i := range slide.Regions {
		clampLayoutRegion(&slide.Regions[i], width, height)
	}
	for i := range slide.Elements {
		clampLayoutRegion(&slide.Elements[i], width, height)
	}
}

func clampLayoutRegion(region *LayoutRegion, width, height float64) {
	if region == nil {
		return
	}
	if region.X < 0 {
		region.W += region.X
		region.X = 0
	}
	if region.Y < 0 {
		region.H += region.Y
		region.Y = 0
	}
	if region.X > width {
		region.X = width
		region.W = 0
	}
	if region.Y > height {
		region.Y = height
		region.H = 0
	}
	if region.X+region.W > width {
		region.W = width - region.X
	}
	if region.Y+region.H > height {
		region.H = height - region.Y
	}
	if region.W < 0 {
		region.W = 0
	}
	if region.H < 0 {
		region.H = 0
	}
}

func normalizeLayoutRegion(region *LayoutRegion) {
	if region == nil {
		return
	}
	if region.Text != nil {
		if region.TextContent == "" {
			region.TextContent = region.Text.Content
		}
		if region.FontSizeEst == 0 && region.Text.FontSizePt > 0 {
			region.FontSizeEst = FlexibleInt(region.Text.FontSizePt + 0.5)
		}
		if region.FontColorEst == "" {
			region.FontColorEst = region.Text.Color
		}
		if region.Alignment == "" {
			region.Alignment = region.Text.Alignment
		}
	}
	if region.Image != nil {
		if region.CropHint == "" {
			region.CropHint = region.Image.CropHint
		}
		if strings.EqualFold(region.Image.BackgroundStrategy, "remove") {
			region.HasBackground = true
		}
	}
	if region.Role == "" && region.Text != nil {
		region.Role = region.Text.Role
	}
}
