package promptflow

import (
	"encoding/json"
	"fmt"
	"image/color"
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
5. How many slides are appropriate? If the user gives an explicit slide count or upper limit, preserve it exactly. Otherwise use 8-15 for a focused presentation.
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

The outline must contain exactly requirements.slide_count slides. Do not add agenda, appendix, divider, or closing slides beyond that count.

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

	return fmt.Sprintf(`You are analyzing presentation slide images to extract their exact editable layout for PPTX reproduction.

Slide dimensions: 16:9 (13.333 x 7.5 inches)

For each slide image, identify the semantically important visible elements needed to recreate the slide as editable PowerPoint XML objects, not a full-slide screenshot.

The actual slide images are attached to this turn as localImage inputs in the same order as this list:
%s

Do not run shell commands or inspect files with tools. Use the attached image directly and return JSON only.

Element rules:
- The slide_number and image_path fields must exactly match the listed Slide N entry for each attached image.
- Use type "text" for standalone editable text.
- Use type "shape" for rectangles, rounded cards, pills, ellipses, badges, icon backgrounds, and decorative containers. Every shape must include shape, fill, stroke, and shadow objects.
- Use type "line" for thin rules, dividers, timeline axes, ticks, connectors, and brackets. Do not fake lines with very thin rectangles.
- Use type "image" or "icon" only for standalone visual assets that cannot be recreated with editable PPT shapes/lines/text. Do not output every tiny decorative glyph as an icon; convert simple glyphs, dots, arrows, dividers, badges, route nodes, and timeline markers into shape/line/text elements.
- Do not classify a row/column of workflow icons, metric icon columns, sparklines, number badges, label pills, or status chips as one image/icon. Recreate those as native shapes, lines, and text.
- Containers/cards/panels are background shapes only. Do not put body copy into the container shape when the same text should be a child text element.
- Image/icon regions must never cover any text box. Put their z_order below nearby text, or split the layout into separate non-overlapping regions.
- Every image/icon region must include a concrete non-empty image_desc that names the exact visual subject. Never leave image_desc empty or generic.
- Keep the full slide compact: target 20-35 elements per slide, never more than 40. Merge repeated decorative shapes into one larger container when possible.
- Keep image/icon asset count small: normally no more than 4 per slide. Prefer one background/photo/illustration asset plus at most three essential transparent icons.
- Ignore purely decorative micro-icons, tiny texture marks, background dots, and redundant separators unless they carry text or clarify a workflow.
- Transparent icons/logos/illustrations must set image.asset_kind to "transparent_icon", "logo", or "illustration" and image.background_strategy to "remove". Background photos, maps, and scene crops should use image.background_strategy "keep".
- Transcribe all visible text, including text inside status pills, badges, numbered dots, timeline nodes, captions, metadata, and labels.
- Use text.runs only when one text box contains mixed emphasis such as "Owner:" bold and the value regular. Every run must include its exact content; if you cannot split the content, omit runs and put the full text in text.content.
- Rounded cards must be shape.kind "round_rect" with corner_radius and shadow.
- Empty timeline nodes must be ellipse shapes with white fill and colored stroke. Filled nodes must be ellipse shapes with solid fill.
- Timeline axis, tick, connector, milestone label, and node elements for the same timeline must share one group_id.

All coordinates in inches, origin at top-left. Be precise with positions and sizes.

Output ONLY valid JSON (no markdown):

{
  "schema_version": "pptflow.layout_analysis.v2",
  "slide_width": 13.333,
  "slide_height": 7.5,
  "slides": [
    {
      "slide_number": 1,
      "image_path": "...",
      "elements": [
        {
          "id": "title_1",
          "type": "text",
          "role": "title",
          "x": 0.8, "y": 0.5, "w": 11.7, "h": 0.9,
          "z_order": 1,
          "opacity": 1,
          "text": {
            "content": "...",
            "role": "title",
            "font_family": "Aptos Display",
            "font_size_pt": 36,
            "font_weight": "700",
            "color": "#111827",
            "alignment": "left",
            "vertical_alignment": "top",
            "margin_left_pt": 0,
            "margin_right_pt": 0,
            "margin_top_pt": 0,
            "margin_bottom_pt": 0,
            "runs": []
          }
        },
        {
          "id": "status_pill_1",
          "type": "shape",
          "role": "status_pill",
          "x": 9.8, "y": 0.55, "w": 1.7, "h": 0.42,
          "z_order": 2,
          "shape": { "kind": "round_rect", "corner_radius": 0.2 },
          "fill": { "type": "solid", "color": "#2563EB", "alpha": 1 },
          "stroke": { "type": "none", "color": "#2563EB", "alpha": 0, "width_pt": 0, "dash": "solid" },
          "shadow": { "enabled": false },
          "text": { "content": "In Progress", "font_size_pt": 11, "font_weight": "700", "color": "#FFFFFF", "alignment": "center", "vertical_alignment": "middle" }
        },
        {
          "id": "timeline_axis",
          "type": "line",
          "role": "timeline_axis",
          "group_id": "timeline_main",
          "x": 1.1, "y": 5.2, "w": 10.8, "h": 0,
          "z_order": 3,
          "stroke": { "type": "solid", "color": "#CBD5E1", "alpha": 1, "width_pt": 1.5, "dash": "solid", "cap": "round" }
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

func buildExtractPrompt(region LayoutRegion, matte color.RGBA) string {
	description := extractionDescription(region)
	parts := []string{
		"Extract the specified visual element from the attached source slide as a standalone PNG suitable for PowerPoint.",
		"Use the source slide as the authority. Do not invent a new 3D object, decorative background, scene, label, or extra text.",
		fmt.Sprintf("Region bbox in slide inches: x=%.3f y=%.3f w=%.3f h=%.3f.", region.X, region.Y, region.W, region.H),
		fmt.Sprintf("Asset type: %s. Role: %s.", firstNonEmptyString(imageAssetKind(region), region.Type), firstNonEmptyString(region.Role, region.ID)),
		fmt.Sprintf("Description: %s", description),
		fmt.Sprintf("Background: place the extracted element on one flat solid matte color %s only.", matteHex(matte)),
		"No checkerboard, no transparency grid, no tiled grid, no texture, no gradient, no shadow cast onto the matte background.",
		"Keep edges clean so the solid matte can be removed by color keying after generation.",
	}
	if region.CropHint != "" {
		parts = append(parts, fmt.Sprintf("Cropping: %s", region.CropHint))
	}
	parts = append(parts,
		"No text labels, no watermarks, no slide backgrounds. Return only the isolated visual element on the matte background.",
		"Style: match the attached source slide exactly.",
	)
	return strings.Join(parts, "\n")
}

func extractionDescription(region LayoutRegion) string {
	values := []string{
		strings.TrimSpace(region.ImageDesc),
		strings.TrimSpace(region.CropHint),
		strings.TrimSpace(region.Role),
		strings.TrimSpace(region.ID),
	}
	if region.Image != nil {
		values = append(values, strings.TrimSpace(region.Image.AssetKind), strings.TrimSpace(region.Image.CropHint))
	}
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return "standalone visual element"
}

func imageAssetKind(region LayoutRegion) string {
	if region.Image == nil {
		return ""
	}
	return strings.TrimSpace(region.Image.AssetKind)
}
