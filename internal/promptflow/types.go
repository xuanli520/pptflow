package promptflow

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// OptimizedRequirements is the output of the prompt optimization phase.
// Codex interviews the user and produces this structured requirements document.
type OptimizedRequirements struct {
	SchemaVersion string `json:"schema_version"` // "pptflow.optimized_requirements.v1"

	Scenario   string `json:"scenario"`    // e.g. "roadshow", "performance_review", "business_plan"
	Topic      string `json:"topic"`       // presentation topic
	Audience   string `json:"audience"`    // target audience description
	Purpose    string `json:"purpose"`     // what the user wants to achieve
	Tone       string `json:"tone"`        // e.g. "premium, concise, product-led"
	SlideCount int    `json:"slide_count"` // estimated page count
	Locale     string `json:"locale"`      // e.g. "zh-CN", "en-US"

	KeyMessages  []string      `json:"key_messages"` // 3-5 key takeaways
	BrandContext *BrandContext `json:"brand_context,omitempty"`
}

type BrandContext struct {
	Colors         map[string]string `json:"colors,omitempty"` // brand color palette
	FontPreference string            `json:"font_preference,omitempty"`
	LogoRequired   bool              `json:"logo_required,omitempty"`
}

// Outline is the presentation outline — per-slide content plan.
type Outline struct {
	SchemaVersion string         `json:"schema_version"` // "pptflow.outline.v1"
	Topic         string         `json:"topic"`
	Thesis        string         `json:"thesis"`    // one-sentence core message
	StoryArc      string         `json:"story_arc"` // narrative structure
	Slides        []OutlineSlide `json:"slides"`
}

type OutlineSlide struct {
	SlideNumber   int      `json:"slide_number"`
	ActionTitle   string   `json:"action_title"` // conclusion-type title
	Role          string   `json:"role"`         // cover, agenda, content, evidence, summary
	KeyPoints     []string `json:"key_points"`   // 2-4 bullet points
	VisualDesc    string   `json:"visual_desc"`  // what the visual should show
	LayoutHint    string   `json:"layout_hint"`  // e.g. "hero_image_left", "three_column", "big_number", "chart_focus"
	HasChart      bool     `json:"has_chart"`
	HasTable      bool     `json:"has_table"`
	HasComparison bool     `json:"has_comparison"`
}

// StyleSpec defines the unified visual style for Image2 to maintain consistency.
type StyleSpec struct {
	SchemaVersion string `json:"schema_version"` // "pptflow.style_spec.v1"

	StylePack       string         `json:"style_pack"`  // e.g. "premium_business_dark", "clean_tech_light"
	VisualMood      string         `json:"visual_mood"` // e.g. "professional, trustworthy, innovative"
	ColorScheme     ColorScheme    `json:"color_scheme"`
	Typography      TypographySpec `json:"typography"`
	LayoutDensity   string         `json:"layout_density"`   // "spacious", "balanced", "dense"
	DecorationLevel string         `json:"decoration_level"` // "minimal", "moderate", "rich"

	// StylePromptSuffix is appended to every Image2 prompt to enforce consistency
	StylePromptSuffix string `json:"style_prompt_suffix"`
}

type ColorScheme struct {
	Primary    string `json:"primary"`
	Secondary  string `json:"secondary"`
	Background string `json:"background"`
	Foreground string `json:"foreground"`
	Accent     string `json:"accent"`
	Muted      string `json:"muted"`
}

type TypographySpec struct {
	HeadingStyle string      `json:"heading_style"` // e.g. "bold sans-serif", "elegant serif"
	BodyStyle    string      `json:"body_style"`
	PreferFonts  StringSlice `json:"prefer_fonts"` // e.g. ["Aptos", "Microsoft YaHei"] or "Aptos, Microsoft YaHei"
}

// StringSlice unmarshals either a JSON string or a JSON array of strings.
type StringSlice []string

func (s *StringSlice) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		*s = nil
		return nil
	}
	// Try as array first
	var arr []string
	if json.Unmarshal(data, &arr) == nil {
		*s = arr
		return nil
	}
	// Try as single string
	var str string
	if json.Unmarshal(data, &str) == nil {
		if str == "" {
			*s = nil
		} else {
			*s = []string{str}
		}
		return nil
	}
	return fmt.Errorf("StringSlice: expected string or array of strings, got %s", string(data))
}

type FlexibleInt int

func (i *FlexibleInt) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		*i = 0
		return nil
	}
	var num int
	if json.Unmarshal(data, &num) == nil {
		*i = FlexibleInt(num)
		return nil
	}
	var floatNum float64
	if json.Unmarshal(data, &floatNum) == nil {
		*i = FlexibleInt(floatNum + 0.5)
		return nil
	}
	var text string
	if json.Unmarshal(data, &text) != nil {
		return fmt.Errorf("FlexibleInt: expected number or string, got %s", string(data))
	}
	text = strings.TrimSpace(text)
	if text == "" {
		*i = 0
		return nil
	}
	if num, err := strconv.Atoi(text); err == nil {
		*i = FlexibleInt(num)
		return nil
	}
	if floatNum, err := strconv.ParseFloat(text, 64); err == nil {
		*i = FlexibleInt(floatNum + 0.5)
		return nil
	}
	for start := 0; start < len(text); start++ {
		if text[start] < '0' || text[start] > '9' {
			continue
		}
		end := start + 1
		for end < len(text) && text[end] >= '0' && text[end] <= '9' {
			end++
		}
		num, err := strconv.Atoi(text[start:end])
		if err != nil {
			return err
		}
		*i = FlexibleInt(num)
		return nil
	}
	*i = 0
	return nil
}

// SlideImage records a generated slide image.
type SlideImage struct {
	SlideNumber int    `json:"slide_number"`
	ImagePath   string `json:"image_path"`
	Prompt      string `json:"prompt"`
	Model       string `json:"model"`
	Size        string `json:"size"`
}

type SlideImageManifest struct {
	SchemaVersion string       `json:"schema_version"`
	StyleRef      string       `json:"style_ref"` // path to style reference image
	Images        []SlideImage `json:"images"`
}

// LayoutAnalysis is Codex's analysis of a slide image's visual layout.
type LayoutAnalysis struct {
	SchemaVersion string        `json:"schema_version"`
	SlideWidth    float64       `json:"slide_width"`  // in inches (16:9 = 13.333)
	SlideHeight   float64       `json:"slide_height"` // in inches (16:9 = 7.5)
	Slides        []SlideLayout `json:"slides"`
}

type SlideLayout struct {
	SlideNumber int             `json:"slide_number"`
	ImagePath   string          `json:"image_path"`
	Regions     []LayoutRegion  `json:"regions"`
	Elements    []LayoutElement `json:"elements,omitempty"`
}

type LayoutRegion struct {
	ID     string      `json:"id"`
	Type   string      `json:"type"` // "title", "body_text", "image", "chart", "table", "decoration", "shape"
	X      float64     `json:"x"`    // inches from left
	Y      float64     `json:"y"`    // inches from top
	W      float64     `json:"w"`    // width in inches
	H      float64     `json:"h"`    // height in inches
	ZOrder FlexibleInt `json:"z_order"`

	// Text regions
	TextContent  string      `json:"text_content,omitempty"`
	FontSizeEst  FlexibleInt `json:"font_size_est,omitempty"`
	FontColorEst string      `json:"font_color_est,omitempty"`
	Alignment    string      `json:"alignment,omitempty"` // "left", "center", "right"

	// Image regions
	ImageDesc     string `json:"image_desc,omitempty"`     // description for Image2 regeneration
	CropHint      string `json:"crop_hint,omitempty"`      // cropping instruction
	HasBackground bool   `json:"has_background,omitempty"` // needs background removal

	// Chart regions
	ChartType     string `json:"chart_type,omitempty"`
	ChartDataHint string `json:"chart_data_hint,omitempty"`

	// Table regions
	TableRowsHint FlexibleInt `json:"table_rows_hint,omitempty"`
	TableColsHint FlexibleInt `json:"table_cols_hint,omitempty"`

	Role     string      `json:"role,omitempty"`
	GroupID  string      `json:"group_id,omitempty"`
	ParentID string      `json:"parent_id,omitempty"`
	Opacity  float64     `json:"opacity,omitempty"`
	Shape    *ShapeSpec  `json:"shape,omitempty"`
	Fill     *FillSpec   `json:"fill,omitempty"`
	Stroke   *StrokeSpec `json:"stroke,omitempty"`
	Shadow   *ShadowSpec `json:"shadow,omitempty"`
	Text     *TextSpec   `json:"text,omitempty"`
	Image    *ImageSpec  `json:"image,omitempty"`
}

type LayoutElement = LayoutRegion

type ShapeSpec struct {
	Kind         string  `json:"kind,omitempty"`
	CornerRadius float64 `json:"corner_radius,omitempty"`
}

type FillSpec struct {
	Type  string  `json:"type,omitempty"`
	Color string  `json:"color,omitempty"`
	Alpha float64 `json:"alpha,omitempty"`
}

type StrokeSpec struct {
	Type    string  `json:"type,omitempty"`
	Color   string  `json:"color,omitempty"`
	Alpha   float64 `json:"alpha,omitempty"`
	WidthPt float64 `json:"width_pt,omitempty"`
	Dash    string  `json:"dash,omitempty"`
	Cap     string  `json:"cap,omitempty"`
}

type ShadowSpec struct {
	Enabled    bool    `json:"enabled,omitempty"`
	Color      string  `json:"color,omitempty"`
	Alpha      float64 `json:"alpha,omitempty"`
	BlurPt     float64 `json:"blur_pt,omitempty"`
	DistancePt float64 `json:"distance_pt,omitempty"`
	Angle      float64 `json:"angle,omitempty"`
}

type TextSpec struct {
	Content           string        `json:"content,omitempty"`
	Role              string        `json:"role,omitempty"`
	FontFamily        string        `json:"font_family,omitempty"`
	FontSizePt        float64       `json:"font_size_pt,omitempty"`
	FontWeight        string        `json:"font_weight,omitempty"`
	Color             string        `json:"color,omitempty"`
	Alignment         string        `json:"alignment,omitempty"`
	VerticalAlignment string        `json:"vertical_alignment,omitempty"`
	MarginLeftPt      float64       `json:"margin_left_pt,omitempty"`
	MarginRightPt     float64       `json:"margin_right_pt,omitempty"`
	MarginTopPt       float64       `json:"margin_top_pt,omitempty"`
	MarginBottomPt    float64       `json:"margin_bottom_pt,omitempty"`
	Runs              []TextRunSpec `json:"runs,omitempty"`
}

type TextRunSpec struct {
	Content    string  `json:"content,omitempty"`
	FontFamily string  `json:"font_family,omitempty"`
	FontSizePt float64 `json:"font_size_pt,omitempty"`
	FontWeight string  `json:"font_weight,omitempty"`
	Color      string  `json:"color,omitempty"`
	Bold       bool    `json:"bold,omitempty"`
}

type ImageSpec struct {
	AssetKind          string `json:"asset_kind,omitempty"`
	BackgroundStrategy string `json:"background_strategy,omitempty"`
	HasAlpha           bool   `json:"has_alpha,omitempty"`
	CropHint           string `json:"crop_hint,omitempty"`
	Fit                string `json:"fit,omitempty"`
	MaskShape          string `json:"mask_shape,omitempty"`
}

// ExtractedResource is an individual resource extracted from a slide image.
type ExtractedResource struct {
	ID          string         `json:"id"`
	SlideNumber int            `json:"slide_number"`
	Type        string         `json:"type"`      // "text", "image", "chart", "table", "shape"
	FilePath    string         `json:"file_path"` // path to the resource file
	X           float64        `json:"x"`
	Y           float64        `json:"y"`
	W           float64        `json:"w"`
	H           float64        `json:"h"`
	Properties  map[string]any `json:"properties,omitempty"`
}

type ExtractedResourceManifest struct {
	SchemaVersion string              `json:"schema_version"`
	Resources     []ExtractedResource `json:"resources"`
}

// AssemblyResult is the final output.
type AssemblyResult struct {
	PPTXPath    string `json:"pptx_path"`
	SlideCount  int    `json:"slide_count"`
	ObjectCount int    `json:"object_count"`
}
