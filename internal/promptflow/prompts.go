package promptflow

import (
	"encoding/json"
	"fmt"
	"strings"
)

func buildPromptOptimizePrompt(userPrompt string) string {
	return fmt.Sprintf(`You are a presentation design consultant. A user wants to create a presentation and has given you this brief:

"%s"

Your job: Transform this brief into a structured presentation requirements document.

Ask yourself:
1. What scenario is this for? (roadshow, performance_review, business_plan, product_launch, education, other)
2. Who is the audience? What do they care about?
3. What is the core purpose — what should the audience think/feel/do after?
4. What tone fits best? (premium, concise, innovative, trustworthy, energetic, academic, etc.)
5. How many slides are appropriate? (typically 8-15 for a focused presentation)
6. What are the 3-5 key messages that MUST land?
7. What language/locale?

Output ONLY valid JSON in this exact format (no markdown, no explanation):

{
  "schema_version": "pptflow.optimized_requirements.v1",
  "scenario": "...",
  "topic": "...",
  "audience": "...",
  "purpose": "...",
  "tone": "...",
  "slide_count": 10,
  "locale": "zh-CN",
  "key_messages": ["message1", "message2", "message3"],
  "brand_context": {
    "colors": {},
    "font_preference": "",
    "logo_required": false
  }
}

If the user's brief is vague, make reasonable inferences and note them. But DO fill every field with your best judgment — do not leave fields empty or "TBD".`, userPrompt)
}

func buildOutlinePrompt(requirements OptimizedRequirements) string {
	reqJSON, _ := json.MarshalIndent(requirements, "", "  ")
	return fmt.Sprintf(`You are a presentation architect. Given these optimized requirements, generate a complete presentation outline and visual style specification.

Requirements:
%s

## Part 1: Outline
Generate a slide-by-slide outline. Each slide must have:
- slide_number: 1-based
- action_title: A conclusion-type title (not just a topic word — express the key insight)
- role: One of "cover", "agenda", "content", "evidence", "summary"
- key_points: 2-4 bullet points that support the action title
- visual_desc: What the visual on this slide should show (be specific — describe composition, mood, focal point)
- layout_hint: One of "hero_image_full", "hero_image_split", "three_column", "big_number", "chart_focus", "comparison_matrix", "timeline", "summary_points"
- has_chart: true if this slide benefits from a chart
- has_table: true if this slide benefits from a comparison table
- has_comparison: true if this slide compares options

## Part 2: Style Specification
Define a unified visual style:
- style_pack: A descriptive name for this visual style
- visual_mood: The emotional quality (e.g. "professional trustworthy innovative")
- color_scheme: { primary, secondary, background, foreground, accent, muted } — use hex colors
- typography: { heading_style, body_style, prefer_fonts }
- layout_density: "spacious", "balanced", or "dense"
- decoration_level: "minimal", "moderate", or "rich"
- style_prompt_suffix: A paragraph (in English) that will be appended to EVERY image generation prompt to maintain visual consistency. Describe the overall aesthetic, color use, composition style, lighting, and mood. This is critical for Image2 consistency.

Output ONLY valid JSON in this format (no markdown, no explanation):

{
  "outline": {
    "schema_version": "pptflow.outline.v1",
    "topic": "...",
    "thesis": "...",
    "story_arc": "...",
    "slides": [ ... ]
  },
  "style_spec": {
    "schema_version": "pptflow.style_spec.v1",
    "style_pack": "...",
    "visual_mood": "...",
    "color_scheme": { ... },
    "typography": { ... },
    "layout_density": "...",
    "decoration_level": "...",
    "style_prompt_suffix": "..."
  }
}`, string(reqJSON))
}

func buildStyleRefPrompt(styleSpec StyleSpec, outline Outline) string {
	coverSlide := outline.Slides[0]
	return fmt.Sprintf(`Presentation cover slide design.

Style: %s
Mood: %s
Color scheme: primary=%s, background=%s, accent=%s
Title: %s
Topic: %s

Composition: Full-slide cover design with clear title hierarchy. Professional presentation quality. No body text clutter.

%s

IMPORTANT: No watermarks, no logos, no QR codes, no readable small text beyond the title. The image must look like a professionally designed presentation slide.`,
		styleSpec.StylePack, styleSpec.VisualMood,
		styleSpec.ColorScheme.Primary, styleSpec.ColorScheme.Background, styleSpec.ColorScheme.Accent,
		coverSlide.ActionTitle, outline.Topic,
		styleSpec.StylePromptSuffix)
}

func buildSlideImagePrompt(styleSpec StyleSpec, outline Outline, slide OutlineSlide) string {
	visualDesc := slide.VisualDesc
	if visualDesc == "" {
		visualDesc = slide.ActionTitle
	}

	return fmt.Sprintf(`Presentation slide design — page %d of %d.

Title: %s
Key points: %s
Visual focus: %s
Layout: %s

Style: %s
Mood: %s
Color scheme: primary=%s, background=%s, accent=%s
Density: %s

Design this as a complete, beautiful presentation slide. The layout should be clean and professional.
Use the layout hint to determine composition: %s.

%s

IMPORTANT: No watermarks, no logos, no QR codes, no page numbers. The image must look like a professionally designed presentation slide ready for a big screen.`,
		slide.SlideNumber, len(outline.Slides),
		slide.ActionTitle,
		strings.Join(slide.KeyPoints, "; "),
		visualDesc,
		slide.LayoutHint,
		styleSpec.StylePack, styleSpec.VisualMood,
		styleSpec.ColorScheme.Primary, styleSpec.ColorScheme.Background, styleSpec.ColorScheme.Accent,
		styleSpec.LayoutDensity,
		slide.LayoutHint,
		styleSpec.StylePromptSuffix)
}

func buildLayoutAnalysisPrompt(manifest SlideImageManifest, outline Outline) string {
	imageDescs := make([]string, 0, len(manifest.Images))
	for _, img := range manifest.Images {
		slide := findOutlineSlide(outline, img.SlideNumber)
		title := ""
		if slide != nil {
			title = slide.ActionTitle
		}
		imageDescs = append(imageDescs, fmt.Sprintf("Slide %d: title=%q, image=%s", img.SlideNumber, title, img.ImagePath))
	}

	return fmt.Sprintf(`You are analyzing presentation slide images to extract their exact visual layout for PPTX reproduction.

Slide dimensions: 16:9 (13.333 x 7.5 inches)

For each slide image, identify ALL visible regions:

Slide images to analyze:
%s

For each slide, output regions like:
- "title" regions: The main heading text area. Include text_content, font_size_est, font_color_est, alignment.
- "body_text" regions: Paragraph or bullet text areas. Include text_content and estimated font size.
- "image" regions: Photos, illustrations, icons. Include image_desc (a detailed description for Image2 to regenerate a standalone version), crop_hint, has_background (true if needs background removal).
- "chart" regions: Data visualizations. Include chart_type (bar/line/pie/area), chart_data_hint.
- "table" regions: Data tables. Include table_rows_hint, table_cols_hint.
- "shape" regions: Decorative shapes, containers, colored blocks.
- "decoration" regions: Background patterns, gradients, subtle decorative elements.

All coordinates in inches, origin at top-left. Be precise with positions and sizes.

Output ONLY valid JSON (no markdown):

{
  "schema_version": "pptflow.layout_analysis.v1",
  "slide_width": 13.333,
  "slide_height": 7.5,
  "slides": [
    {
      "slide_number": 1,
      "image_path": "...",
      "regions": [
        {
          "id": "title",
          "type": "title",
          "x": 0.8, "y": 0.5, "w": 11.7, "h": 0.9,
          "z_order": 1,
          "text_content": "...",
          "font_size_est": 36,
          "font_color_est": "#FFFFFF",
          "alignment": "left"
        }
      ]
    }
  ]
}`, strings.Join(imageDescs, "\n"))
}

func findOutlineSlide(outline Outline, num int) *OutlineSlide {
	for i := range outline.Slides {
		if outline.Slides[i].SlideNumber == num {
			return &outline.Slides[i]
		}
	}
	return nil
}

func buildExtractPrompt(region LayoutRegion) string {
	parts := []string{
		"Extract this visual element as a standalone image suitable for PowerPoint.",
		"Remove the background — make it transparent (PNG with alpha channel).",
		"The element should be centered and clean, ready to place on a slide.",
		fmt.Sprintf("Description: %s", region.ImageDesc),
	}
	if region.CropHint != "" {
		parts = append(parts, fmt.Sprintf("Cropping: %s", region.CropHint))
	}
	parts = append(parts,
		"No text labels, no watermarks, no slide backgrounds — just the isolated visual element.",
		"Style: Clean, professional, with soft shadows if appropriate for depth.",
	)
	return strings.Join(parts, "\n")
}

func buildAssemblyPrompt(analysisJSON, resourcesJSON, styleJSON, workspaceRoot string) string {
	return fmt.Sprintf(`You are a PowerPoint automation expert. You have:
1. A layout analysis describing exactly where each element should be on each slide
2. Extracted image resources (PNG files with transparent backgrounds)
3. A style specification

Your job: Write and execute a Python script (using python-pptx) that generates deck.pptx at %s/output/deck.pptx

## Layout Analysis (slide layouts with exact coordinates)
%s

## Extracted Resources (image files available)
%s

## Style Specification
%s

## Instructions for the Python script:

1. Create a 16:9 presentation (13.333 x 7.5 inches)
2. For EACH slide in the layout analysis:
   a. Set the slide background color from the style spec
   b. Add text boxes for all "title" and "body_text" regions at their exact positions
      - Use the correct font size, color, and alignment
      - For body text, format bullet points properly
   c. Add pictures for all "image" regions at their exact positions
      - Reference the extracted resource PNG files
      - Maintain aspect ratio, fit within the region bounds
   d. Add shapes for "shape" and "decoration" regions
      - Use python-pptx shapes (rectangles, rounded rects, etc.)
      - Match the fill colors and borders
3. Save as %s/output/deck.pptx
4. Print "DONE: deck.pptx created with N slides"

## Important:
- All text must be REAL PowerPoint text boxes (editable)
- All images must be REAL PowerPoint picture objects (movable, scalable)
- Charts should be added as images if no chart data is available, or created with python-pptx chart objects if data is provided
- Every coordinate from the layout analysis must be respected
- Fonts: Use the preferred fonts from the style spec

Write the Python script to a file, then execute it with: python generate.py`,
		workspaceRoot, analysisJSON, resourcesJSON, styleJSON, workspaceRoot)
}
