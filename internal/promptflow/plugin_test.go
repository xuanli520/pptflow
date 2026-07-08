package promptflow

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/xuanli520/pptflow/internal/workflow"
)

type captureAgent struct {
	request  workflow.AgentTurnRequest
	requests []workflow.AgentTurnRequest
	text     string
	texts    []string
	err      error
}

func (a *captureAgent) Turn(_ context.Context, req workflow.AgentTurnRequest) (workflow.AgentTurnResult, error) {
	a.request = req
	a.requests = append(a.requests, req)
	if a.err != nil {
		return workflow.AgentTurnResult{}, a.err
	}
	text := a.text
	if idx := len(a.requests) - 1; idx >= 0 && idx < len(a.texts) {
		text = a.texts[idx]
	}
	return workflow.AgentTurnResult{Text: text, Model: req.Model}, nil
}

type captureImageRuntime struct {
	requests []workflow.ImageRequest
	write    func(string) error
	err      error
}

func (r *captureImageRuntime) Configured() bool {
	return true
}

func (r *captureImageRuntime) Generate(_ context.Context, req workflow.ImageRequest) (workflow.ImageResult, error) {
	r.requests = append(r.requests, req)
	if r.err != nil {
		return workflow.ImageResult{}, r.err
	}
	if r.write != nil {
		if err := r.write(req.OutputPath); err != nil {
			return workflow.ImageResult{}, err
		}
		return workflow.ImageResult{Path: req.OutputPath, Model: firstNonEmpty(req.Model, "fake-image"), Size: req.Size, Quality: req.Quality, MIME: "image/png"}, nil
	}
	if err := writeSolidPNG(req.OutputPath, color.RGBA{R: 10, G: 20, B: 30, A: 255}, 64, 36); err != nil {
		return workflow.ImageResult{}, err
	}
	return workflow.ImageResult{Path: req.OutputPath, Model: firstNonEmpty(req.Model, "fake-image"), Size: req.Size, Quality: req.Quality, MIME: "image/png"}, nil
}

func TestAnalyzeLayoutSendsLocalImages(t *testing.T) {
	store, err := workflow.NewFileArtifactStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	img1 := writeTestPNG(t, store, "slide_images/slide_01.png", color.RGBA{R: 255, A: 255})
	img2 := writeTestPNG(t, store, "slide_images/slide_02.png", color.RGBA{G: 255, A: 255})
	outline := Outline{Slides: []OutlineSlide{{SlideNumber: 1, ActionTitle: "One"}, {SlideNumber: 2, ActionTitle: "Two"}}}
	manifest := SlideImageManifest{Images: []SlideImage{{SlideNumber: 1, ImagePath: img1}, {SlideNumber: 2, ImagePath: img2}}}
	if _, err := store.PutJSON(context.Background(), "outline.json", "outline", "test", outline); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutJSON(context.Background(), "slide_image_manifest.json", "slide_image_manifest", "test", manifest); err != nil {
		t.Fatal(err)
	}
	analysis1 := LayoutAnalysis{
		SchemaVersion: "pptflow.layout_analysis.v1",
		SlideWidth:    13.333,
		SlideHeight:   7.5,
		Slides: []SlideLayout{
			{SlideNumber: 1, ImagePath: img1, Regions: []LayoutRegion{{ID: "title", Type: "title", X: 1, Y: 1, W: 3, H: 1, TextContent: "One"}}},
		},
	}
	analysis2 := LayoutAnalysis{
		SchemaVersion: "pptflow.layout_analysis.v1",
		SlideWidth:    13.333,
		SlideHeight:   7.5,
		Slides: []SlideLayout{
			{SlideNumber: 1, ImagePath: img2, Regions: []LayoutRegion{{ID: "title", Type: "title", X: 1, Y: 1, W: 3, H: 1, TextContent: "Two"}}},
		},
	}
	analysisJSON1, err := json.Marshal(analysis1)
	if err != nil {
		t.Fatal(err)
	}
	analysisJSON2, err := json.Marshal(analysis2)
	if err != nil {
		t.Fatal(err)
	}
	agent := &captureAgent{texts: []string{string(analysisJSON1), string(analysisJSON2)}}
	_, err = (Plugin{}).analyzeLayout(context.Background(), workflow.NodeRequest{
		Spec:          workflow.NodeSpec{ID: "analyze_layout", Kind: "promptflow.analyze_layout"},
		ArtifactRoot:  store.Root(),
		WorkspaceRoot: workspace,
		Store:         store,
		Runtimes:      workflow.Runtimes{Agent: agent},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(agent.requests) != 2 {
		t.Fatalf("requests = %d", len(agent.requests))
	}
	for i, request := range agent.requests {
		if len(request.Input) != 1 {
			t.Fatalf("request %d input length = %d", i, len(request.Input))
		}
		part := request.Input[0]
		if part.Type != "localImage" || part.Path == "" {
			t.Fatalf("request %d input = %+v", i, part)
		}
	}
	if len(agent.request.WorkspaceRoots) != 1 || agent.request.WorkspaceRoots[0] != store.Root() {
		t.Fatalf("workspace roots = %#v", agent.request.WorkspaceRoots)
	}
	var merged LayoutAnalysis
	if _, err := store.ReadJSON(context.Background(), "layout_analysis.json", &merged); err != nil {
		t.Fatal(err)
	}
	if len(merged.Slides) != 2 {
		t.Fatalf("merged slides = %d", len(merged.Slides))
	}
	if merged.Slides[1].SlideNumber != 2 {
		t.Fatalf("merged slide 2 number = %d", merged.Slides[1].SlideNumber)
	}
}

func TestValidateLayoutAnalysisRejectsBadOutput(t *testing.T) {
	manifest := SlideImageManifest{Images: []SlideImage{{SlideNumber: 1, ImagePath: `C:\slides\slide_01.png`}}}
	cases := []LayoutAnalysis{
		{SlideWidth: 13.333, SlideHeight: 7.5},
		{SlideWidth: 13.333, SlideHeight: 7.5, Slides: []SlideLayout{{SlideNumber: 1, ImagePath: `C:\slides\other.png`}}},
		{SlideWidth: 13.333, SlideHeight: 7.5, Slides: []SlideLayout{{SlideNumber: 1, ImagePath: `C:\slides\slide_01.png`, Regions: []LayoutRegion{{ID: "r", Type: "title", X: 1, Y: 1, W: 1, H: 1}}}}},
		{SlideWidth: 13.333, SlideHeight: 7.5, Slides: []SlideLayout{{SlideNumber: 1, ImagePath: `C:\slides\slide_01.png`, Regions: []LayoutRegion{{ID: "r", Type: "shape", X: 12, Y: 1, W: 3, H: 1}}}}},
	}
	for i, analysis := range cases {
		if err := validateLayoutAnalysis(analysis, manifest); err == nil {
			t.Fatalf("case %d expected error", i)
		}
	}
}

func TestValidateLayoutAnalysisAcceptsV2LineWithZeroHeight(t *testing.T) {
	manifest := SlideImageManifest{Images: []SlideImage{{SlideNumber: 1, ImagePath: `C:\slides\slide_01.png`}}}
	analysis := LayoutAnalysis{
		SchemaVersion: "pptflow.layout_analysis.v2",
		SlideWidth:    13.333,
		SlideHeight:   7.5,
		Slides: []SlideLayout{{
			SlideNumber: 1,
			ImagePath:   `C:\slides\slide_01.png`,
			Elements: []LayoutElement{{
				ID:     "timeline_axis",
				Type:   "line",
				X:      1,
				Y:      3,
				W:      8,
				H:      0,
				Stroke: &StrokeSpec{Type: "solid", Color: "#CBD5E1", WidthPt: 1.5},
			}},
		}},
	}
	if err := validateLayoutAnalysis(analysis, manifest); err != nil {
		t.Fatal(err)
	}
}

func TestParseLayoutAnalysisClampsOutOfBoundsRegions(t *testing.T) {
	analysis, err := parseLayoutAnalysis(`{
		"schema_version": "pptflow.layout_analysis.v2",
		"slide_width": 13.333,
		"slide_height": 7.5,
		"slides": [{
			"slide_number": 1,
			"image_path": "slide.png",
			"elements": [{
				"id": "route_line_1",
				"type": "line",
				"x": 12.9,
				"y": 7.4,
				"w": 1.0,
				"h": 0,
				"stroke": { "type": "solid", "color": "#CBD5E1", "width_pt": 1.5 }
			}]
		}]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	region := analysis.Slides[0].Elements[0]
	if region.X+region.W > analysis.SlideWidth || region.Y+region.H > analysis.SlideHeight {
		t.Fatalf("region was not clamped: %+v", region)
	}
	if err := validateLayoutAnalysis(analysis, SlideImageManifest{Images: []SlideImage{{SlideNumber: 1, ImagePath: "slide.png"}}}); err != nil {
		t.Fatal(err)
	}
}

func TestParseLayoutAnalysisAcceptsFlexibleInts(t *testing.T) {
	analysis, err := parseLayoutAnalysis(`{
		"schema_version": "pptflow.layout_analysis.v1",
		"slide_width": 13.333,
		"slide_height": 7.5,
		"slides": [{
			"slide_number": 1,
			"image_path": "slide.png",
			"regions": [{
				"id": "title",
				"type": "title",
				"x": 1,
				"y": 1,
				"w": 3,
				"h": 1,
				"z_order": "1",
				"text_content": "Title",
				"font_size_est": 10.5,
				"table_rows_hint": "3 rows"
			}]
		}]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	region := analysis.Slides[0].Regions[0]
	if region.ZOrder != 1 || region.FontSizeEst != 11 || region.TableRowsHint != 3 {
		t.Fatalf("region = %+v", region)
	}
}

func TestAnalyzeLayoutFailsWithoutImageFallback(t *testing.T) {
	store, err := workflow.NewFileArtifactStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	img := writeTestPNG(t, store, "slide_images/slide_01.png", color.RGBA{R: 255, A: 255})
	if _, err := store.PutJSON(context.Background(), "outline.json", "outline", "test", Outline{Slides: []OutlineSlide{{SlideNumber: 1, ActionTitle: "One"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutJSON(context.Background(), "slide_image_manifest.json", "slide_image_manifest", "test", SlideImageManifest{Images: []SlideImage{{SlideNumber: 1, ImagePath: img}}}); err != nil {
		t.Fatal(err)
	}
	_, err = (Plugin{}).analyzeLayout(context.Background(), workflow.NodeRequest{
		Spec:          workflow.NodeSpec{ID: "analyze_layout", Kind: "promptflow.analyze_layout"},
		ArtifactRoot:  store.Root(),
		WorkspaceRoot: t.TempDir(),
		Store:         store,
		Runtimes:      workflow.Runtimes{Agent: &captureAgent{err: errors.New("timeout")}},
	})
	if err == nil {
		t.Fatal("expected analyze layout error")
	}
	var analysis LayoutAnalysis
	if _, err := store.ReadJSON(context.Background(), "layout_analysis.json", &analysis); err == nil {
		t.Fatalf("unexpected fallback analysis = %+v", analysis)
	}
}

func TestGenerateSlidesPassesStyleReference(t *testing.T) {
	store, err := workflow.NewFileArtifactStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	styleRef := writeTestPNG(t, store, "slide_images/style_ref.png", color.RGBA{R: 255, A: 255})
	if _, err := store.PutJSON(context.Background(), "outline.json", "outline", "test", Outline{Slides: []OutlineSlide{{SlideNumber: 1, ActionTitle: "Title"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutJSON(context.Background(), "style_spec.json", "style_spec", "test", StyleSpec{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutJSON(context.Background(), "slide_image_manifest.json", "slide_image_manifest", "test", SlideImageManifest{StyleRef: styleRef}); err != nil {
		t.Fatal(err)
	}
	imageRuntime := &captureImageRuntime{}
	_, err = (Plugin{}).generateSlides(context.Background(), workflow.NodeRequest{
		Spec:  workflow.NodeSpec{ID: "generate_slides", Kind: "promptflow.generate_slides", Config: map[string]any{"model": "custom-image"}},
		Store: store,
		Runtimes: workflow.Runtimes{
			Image: imageRuntime,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(imageRuntime.requests) != 1 {
		t.Fatalf("requests = %d", len(imageRuntime.requests))
	}
	req := imageRuntime.requests[0]
	if req.Model != "custom-image" {
		t.Fatalf("model = %s", req.Model)
	}
	if len(req.SourceImages) != 1 || req.SourceImages[0].Path != styleRef || req.SourceImages[0].Role != "style_reference" {
		t.Fatalf("source images = %+v", req.SourceImages)
	}
	refs, err := store.List(context.Background(), "slide_images")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) == 0 {
		t.Fatal("expected registered slide images")
	}
}

func TestAssemblePPTXCreatesDeck(t *testing.T) {
	store, err := workflow.NewFileArtifactStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	imagePath := writeTestPNG(t, store, "slide_images/slide_01.png", color.RGBA{R: 255, A: 255})
	resourcePath := writeTestPNG(t, store, "extracted/slide_01_hero.png", color.RGBA{B: 255, A: 255})
	analysis := LayoutAnalysis{
		SlideWidth:  13.333,
		SlideHeight: 7.5,
		Slides: []SlideLayout{{
			SlideNumber: 1,
			ImagePath:   imagePath,
			Regions: []LayoutRegion{
				{ID: "bg", Type: "shape", X: 0, Y: 0, W: 13.333, H: 7.5, ZOrder: 1},
				{ID: "title", Type: "title", X: 0.7, Y: 0.5, W: 7, H: 0.8, ZOrder: 2, TextContent: "Editable Title", FontSizeEst: 32, FontColorEst: "#123456"},
				{ID: "hero", Type: "image", X: 8, Y: 1, W: 3, H: 2, ZOrder: 3},
			},
		}},
	}
	if _, err := store.PutJSON(context.Background(), "layout_analysis.json", "layout_analysis", "test", analysis); err != nil {
		t.Fatal(err)
	}
	resources := ExtractedResourceManifest{Resources: []ExtractedResource{{ID: "slide_01_hero", SlideNumber: 1, Type: "image", FilePath: resourcePath, X: 8, Y: 1, W: 3, H: 2}}}
	if _, err := store.PutJSON(context.Background(), "extracted_resource_manifest.json", "extracted_resource_manifest", "test", resources); err != nil {
		t.Fatal(err)
	}
	styleSpec := StyleSpec{ColorScheme: ColorScheme{Foreground: "#111111", Muted: "#EFEFEF"}, Typography: TypographySpec{PreferFonts: []string{"Aptos"}}}
	if _, err := store.PutJSON(context.Background(), "style_spec.json", "style_spec", "test", styleSpec); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	result, err := (Plugin{}).assemblePPTX(context.Background(), workflow.NodeRequest{
		Spec:          workflow.NodeSpec{ID: "assemble_pptx", Kind: "promptflow.assemble_pptx"},
		ArtifactRoot:  store.Root(),
		WorkspaceRoot: workspace,
		Store:         store,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Artifacts) != 1 || result.Artifacts[0].Name != "deck.pptx" {
		t.Fatalf("artifacts = %+v", result.Artifacts)
	}
	if err := validatePPTX(filepath.Join(workspace, "output", "deck.pptx"), 1); err != nil {
		t.Fatal(err)
	}
	deckPath := filepath.Join(workspace, "output", "deck.pptx")
	slideXML := readZipText(t, deckPath, "ppt/slides/slide1.xml")
	for _, want := range []string{"<p:sp>", "<a:t>Editable Title</a:t>", "<p:pic>"} {
		if !strings.Contains(slideXML, want) {
			t.Fatalf("slide xml missing %s: %s", want, slideXML)
		}
	}
	if strings.Contains(slideXML, `name="Slide image"`) {
		t.Fatalf("deck used full-slide image xml: %s", slideXML)
	}
	relsXML := readZipText(t, deckPath, "ppt/slides/_rels/slide1.xml.rels")
	if !strings.Contains(relsXML, `Target="../media/image1.png"`) {
		t.Fatalf("slide rels = %s", relsXML)
	}
}

func TestPPTXShapeRendererWritesEllipseRoundRectLine(t *testing.T) {
	xml := pptxEditableSlide(SlideLayout{SlideNumber: 1}, []LayoutRegion{
		{ID: "status", Type: "shape", Role: "status_pill", X: 1, Y: 1, W: 2, H: 0.4, Shape: &ShapeSpec{Kind: "round_rect"}, Fill: &FillSpec{Type: "solid", Color: "#2563EB"}, Stroke: &StrokeSpec{Type: "none"}, Shadow: &ShadowSpec{}},
		{ID: "badge", Type: "shape", Role: "badge", X: 4, Y: 1, W: 0.4, H: 0.4, Shape: &ShapeSpec{Kind: "ellipse"}, Fill: &FillSpec{Type: "solid", Color: "#FFFFFF"}, Stroke: &StrokeSpec{Type: "solid", Color: "#2563EB", WidthPt: 1}, Shadow: &ShadowSpec{}},
		{ID: "timeline_axis", Type: "line", Role: "timeline_axis", X: 1, Y: 3, W: 6, H: 0, Stroke: &StrokeSpec{Type: "solid", Color: "#CBD5E1", WidthPt: 1.5}},
	}, nil, LayoutAnalysis{SlideWidth: 13.333, SlideHeight: 7.5}, StyleSpec{})
	for _, want := range []string{`prst="roundRect"`, `prst="ellipse"`, `prst="line"`} {
		if !strings.Contains(xml, want) {
			t.Fatalf("slide xml missing %s: %s", want, xml)
		}
	}
}

func TestPPTXShapeRendererWritesFillStrokeDashShadow(t *testing.T) {
	xml := pptxShape(2, LayoutRegion{
		ID:     "card",
		Type:   "shape",
		X:      1,
		Y:      1,
		W:      3,
		H:      2,
		Shape:  &ShapeSpec{Kind: "round_rect"},
		Fill:   &FillSpec{Type: "solid", Color: "#2563EB", Alpha: 0.8},
		Stroke: &StrokeSpec{Type: "solid", Color: "#111827", Alpha: 0.6, WidthPt: 2, Dash: "dash"},
		Shadow: &ShadowSpec{Enabled: true, Color: "#000000", Alpha: 0.2, BlurPt: 8, DistancePt: 2, Angle: 45},
	}, StyleSpec{})
	for _, want := range []string{`<a:srgbClr val="2563EB">`, `<a:alpha val="80000"/>`, `<a:srgbClr val="111827">`, `<a:prstDash val="dash"/>`, `<a:outerShdw`} {
		if !strings.Contains(xml, want) {
			t.Fatalf("shape xml missing %s: %s", want, xml)
		}
	}
}

func TestPPTXShapeRendererWritesShapeText(t *testing.T) {
	xml := pptxShape(2, LayoutRegion{
		ID:     "status",
		Type:   "shape",
		Role:   "status_pill",
		X:      1,
		Y:      1,
		W:      2,
		H:      0.4,
		Shape:  &ShapeSpec{Kind: "round_rect"},
		Fill:   &FillSpec{Type: "solid", Color: "#2563EB"},
		Stroke: &StrokeSpec{Type: "none"},
		Shadow: &ShadowSpec{},
		Text:   &TextSpec{Content: "In Progress", FontSizePt: 11, FontWeight: "700", Color: "#FFFFFF", Alignment: "center", VerticalAlignment: "middle"},
	}, StyleSpec{})
	for _, want := range []string{`prst="roundRect"`, `anchor="ctr"`, `<a:t>In Progress</a:t>`, ` b="1"`} {
		if !strings.Contains(xml, want) {
			t.Fatalf("shape xml missing %s: %s", want, xml)
		}
	}
}

func TestPPTXTextShapeWritesRunsBoldColorAndSpacing(t *testing.T) {
	xml := pptxTextShape(2, LayoutRegion{
		ID:   "metadata",
		Type: "text",
		X:    1,
		Y:    1,
		W:    4,
		H:    1,
		Text: &TextSpec{
			Alignment:     "left",
			FontFamily:    "Aptos",
			FontSizePt:    12,
			Color:         "#334155",
			MarginLeftPt:  4,
			MarginRightPt: 4,
			Runs: []TextRunSpec{
				{Content: "Owner:", Bold: true, Color: "#111827"},
				{Content: " PMO ", Color: "#334155"},
				{Content: "Due:", FontWeight: "700", Color: "#111827"},
				{Content: " Friday", Color: "#334155"},
			},
		},
	}, StyleSpec{})
	for _, want := range []string{`lIns="50800"`, `<a:t>Owner:</a:t>`, `<a:t>Due:</a:t>`, ` b="1"`, `val="111827"`, `val="334155"`} {
		if !strings.Contains(xml, want) {
			t.Fatalf("text xml missing %s: %s", want, xml)
		}
	}
}

func TestPPTXTextShapeFallsBackWhenRunsHaveNoContent(t *testing.T) {
	xml := pptxTextShape(2, LayoutRegion{
		ID:   "body",
		Type: "text",
		X:    1,
		Y:    1,
		W:    4,
		H:    1,
		Text: &TextSpec{
			Content:    "完整中文正文",
			FontSizePt: 16,
			Runs:       []TextRunSpec{{FontSizePt: 18, FontWeight: "700"}},
		},
	}, StyleSpec{})
	if !strings.Contains(xml, `<a:t>完整中文正文</a:t>`) {
		t.Fatalf("text xml missing fallback content: %s", xml)
	}
}

func TestPPTXPictureUsesFitModeWithoutDistortion(t *testing.T) {
	xml := pptxPicture(2, LayoutRegion{
		ID:   "icon",
		Type: "image",
		X:    1,
		Y:    1,
		W:    1,
		H:    1,
		Image: &ImageSpec{
			Fit:       "cover",
			CropHint:  "l=10 t=5 r=10 b=5",
			MaskShape: "ellipse",
		},
	}, "rId2")
	for _, want := range []string{`noChangeAspect="1"`, `<a:srcRect l="10000" t="5000" r="10000" b="5000"/>`, `prst="ellipse"`} {
		if !strings.Contains(xml, want) {
			t.Fatalf("picture xml missing %s: %s", want, xml)
		}
	}
}

func TestAssemblePPTXRejectsFullSlideFallback(t *testing.T) {
	store, err := workflow.NewFileArtifactStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	imagePath := writeTestPNG(t, store, "slide_images/slide_01.png", color.RGBA{R: 255, A: 255})
	analysis := fallbackLayoutAnalysis(SlideImageManifest{Images: []SlideImage{{SlideNumber: 1, ImagePath: imagePath}}})
	if _, err := store.PutJSON(context.Background(), "layout_analysis.json", "layout_analysis", "test", analysis); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutJSON(context.Background(), "extracted_resource_manifest.json", "extracted_resource_manifest", "test", ExtractedResourceManifest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutJSON(context.Background(), "style_spec.json", "style_spec", "test", StyleSpec{}); err != nil {
		t.Fatal(err)
	}
	_, err = (Plugin{}).assemblePPTX(context.Background(), workflow.NodeRequest{
		Spec:          workflow.NodeSpec{ID: "assemble_pptx", Kind: "promptflow.assemble_pptx"},
		ArtifactRoot:  store.Root(),
		WorkspaceRoot: t.TempDir(),
		Store:         store,
	})
	if err == nil || !strings.Contains(err.Error(), "editable layout analysis unavailable") {
		t.Fatalf("err = %v", err)
	}
}

func TestAssemblePPTXRequiresExtractedImageResource(t *testing.T) {
	store, err := workflow.NewFileArtifactStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	imagePath := writeTestPNG(t, store, "slide_images/slide_01.png", color.RGBA{R: 255, A: 255})
	analysis := LayoutAnalysis{Slides: []SlideLayout{{SlideNumber: 1, ImagePath: imagePath, Regions: []LayoutRegion{{ID: "hero", Type: "image", X: 1, Y: 1, W: 2, H: 2}}}}}
	if _, err := store.PutJSON(context.Background(), "layout_analysis.json", "layout_analysis", "test", analysis); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutJSON(context.Background(), "extracted_resource_manifest.json", "extracted_resource_manifest", "test", ExtractedResourceManifest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutJSON(context.Background(), "style_spec.json", "style_spec", "test", StyleSpec{}); err != nil {
		t.Fatal(err)
	}
	_, err = (Plugin{}).assemblePPTX(context.Background(), workflow.NodeRequest{
		Spec:          workflow.NodeSpec{ID: "assemble_pptx", Kind: "promptflow.assemble_pptx"},
		ArtifactRoot:  store.Root(),
		WorkspaceRoot: t.TempDir(),
		Store:         store,
	})
	if err == nil || !strings.Contains(err.Error(), "missing extracted resource slide_01_hero") {
		t.Fatalf("err = %v", err)
	}
}

func TestCropRegionFromSlideWritesPNG(t *testing.T) {
	store, err := workflow.NewFileArtifactStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := writeTestPNG(t, store, "slide_images/slide_01.png", color.RGBA{B: 255, A: 255})
	output := filepath.Join(t.TempDir(), "crop.png")
	if err := cropRegionFromSlide(source, LayoutRegion{ID: "hero", X: 0, Y: 0, W: 6.666, H: 3.75}, 13.333, 7.5, output); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
}

func TestExtractResourcesCropsImagesAndRegistersManifest(t *testing.T) {
	store, err := workflow.NewFileArtifactStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := writeTestPNG(t, store, "slide_images/slide_01.png", color.RGBA{B: 255, A: 255})
	analysis := LayoutAnalysis{
		SlideWidth:  13.333,
		SlideHeight: 7.5,
		Slides: []SlideLayout{{
			SlideNumber: 1,
			ImagePath:   source,
			Regions: []LayoutRegion{
				{ID: "hero", Type: "image", X: 0, Y: 0, W: 3, H: 3, ImageDesc: "hero"},
				{ID: "title", Type: "title", X: 1, Y: 1, W: 2, H: 1, TextContent: "Title"},
			},
		}},
	}
	if _, err := store.PutJSON(context.Background(), "layout_analysis.json", "layout_analysis", "test", analysis); err != nil {
		t.Fatal(err)
	}
	result, err := (Plugin{}).extractResources(context.Background(), workflow.NodeRequest{
		Spec:  workflow.NodeSpec{ID: "extract_resources", Kind: "promptflow.extract_resources"},
		Store: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Metrics.Model != "local-crop" {
		t.Fatalf("model = %s", result.Metrics.Model)
	}
	var manifest ExtractedResourceManifest
	if _, err := store.ReadJSON(context.Background(), "extracted_resource_manifest.json", &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Resources) != 1 || manifest.Resources[0].ID != "slide_01_hero" {
		t.Fatalf("manifest = %+v", manifest)
	}
	refs, err := store.List(context.Background(), "extracted")
	if err != nil {
		t.Fatal(err)
	}
	foundImage := false
	for _, ref := range refs {
		if ref.Name == "extracted/slide_01_hero.png" && ref.Type == "image" {
			foundImage = true
		}
	}
	if !foundImage {
		t.Fatalf("refs = %+v", refs)
	}
}

func TestExtractResourcesUsesImage2SourceExtractionByDefault(t *testing.T) {
	store, err := workflow.NewFileArtifactStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := writeTestPNG(t, store, "slide_images/slide_01.png", color.RGBA{R: 255, G: 255, B: 255, A: 255})
	analysis := LayoutAnalysis{
		SlideWidth:  13.333,
		SlideHeight: 7.5,
		Slides: []SlideLayout{{
			SlideNumber: 1,
			ImagePath:   source,
			Regions: []LayoutRegion{{
				ID:            "icon",
				Type:          "icon",
				X:             0,
				Y:             0,
				W:             1,
				H:             1,
				HasBackground: true,
				Image:         &ImageSpec{AssetKind: "transparent_icon", BackgroundStrategy: "remove"},
			}},
		}},
	}
	if _, err := store.PutJSON(context.Background(), "layout_analysis.json", "layout_analysis", "test", analysis); err != nil {
		t.Fatal(err)
	}
	imageRuntime := &captureImageRuntime{write: writeTransparentFixture}
	_, err = (Plugin{}).extractResources(context.Background(), workflow.NodeRequest{
		Spec:     workflow.NodeSpec{ID: "extract_resources", Kind: "promptflow.extract_resources"},
		Store:    store,
		Runtimes: workflow.Runtimes{Image: imageRuntime},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(imageRuntime.requests) != 1 {
		t.Fatalf("source extraction requests = %d", len(imageRuntime.requests))
	}
	var manifest ExtractedResourceManifest
	if _, err := store.ReadJSON(context.Background(), "extracted_resource_manifest.json", &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Resources) != 1 || manifest.Resources[0].Properties["extraction_method"] != "image2-source-extraction" {
		t.Fatalf("manifest = %+v", manifest)
	}
}

func TestExtractPromptUsesSolidMatte(t *testing.T) {
	prompt := buildExtractPrompt(LayoutRegion{
		ID:        "icon",
		Type:      "icon",
		Role:      "service_icon",
		X:         1,
		Y:         2,
		W:         0.5,
		H:         0.5,
		ImageDesc: "blue research service icon",
		Image:     &ImageSpec{AssetKind: "transparent_icon", BackgroundStrategy: "remove"},
	}, extractionMatteColor())
	for _, want := range []string{"#00FF00", "No checkerboard", "Region bbox", "blue research service icon"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q: %s", want, prompt)
		}
	}
	if strings.Contains(prompt, "make it transparent") {
		t.Fatalf("prompt still requests transparent background: %s", prompt)
	}
}

func TestTransparentResourceQualityRejectsCheckerboard(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checker.png")
	if err := writeCheckerboardPNG(path); err != nil {
		t.Fatal(err)
	}
	quality := inspectTransparentResource(path, extractionMatteColor())
	if !quality.CheckerboardDetected || quality.Pass {
		t.Fatalf("quality = %+v", quality)
	}
}

func TestVisualQARejectsTextOverlaps(t *testing.T) {
	resourcePath := filepath.Join(t.TempDir(), "icon.png")
	if err := writeTransparentFixture(resourcePath); err != nil {
		t.Fatal(err)
	}
	analysis := LayoutAnalysis{
		SchemaVersion: "pptflow.layout_analysis.v2",
		SlideWidth:    13.333,
		SlideHeight:   7.5,
		Slides: []SlideLayout{{
			SlideNumber: 1,
			Regions: []LayoutRegion{
				{ID: "title", Type: "title", X: 1, Y: 1, W: 3, H: 0.6, TextContent: "Title"},
				{ID: "icon", Type: "icon", X: 1.2, Y: 1.05, W: 1, H: 0.5, Image: &ImageSpec{AssetKind: "transparent_icon", BackgroundStrategy: "remove"}},
				{ID: "duplicate", Type: "text", X: 1.1, Y: 1.05, W: 2.5, H: 0.5, Text: &TextSpec{Content: "Title"}},
			},
		}},
	}
	resources := ExtractedResourceManifest{Resources: []ExtractedResource{{ID: "slide_01_icon", SlideNumber: 1, Type: "icon", FilePath: resourcePath}}}
	report := buildVisualQAReport(analysis, resources, StyleSpec{}, map[int]string{1: "<a:t>Title</a:t>"})
	applyQAThresholds(&report, qaThresholdsFromConfig(nil, analysis))
	if report.Pass || report.TextPictureOverlap <= 0.12 || report.TextTextOverlap <= 0.08 {
		t.Fatalf("report = %+v", report)
	}
}

func TestFixRegionLayersPlacesPictureBehindText(t *testing.T) {
	regions := []LayoutRegion{
		{ID: "hero", Type: "image", X: 1, Y: 1, W: 3, H: 2, ZOrder: 5},
		{ID: "title", Type: "title", X: 1.5, Y: 1.2, W: 2, H: 0.5, ZOrder: 1, TextContent: "Title"},
	}
	fixRegionLayers(regions)
	if regions[0].ZOrder >= regions[1].ZOrder {
		t.Fatalf("regions = %+v", regions)
	}
}

func TestValidatePPTXChecksSlideCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deck.pptx")
	makePPTX(t, path, 2)
	if err := validatePPTX(path, 2); err != nil {
		t.Fatal(err)
	}
	if err := validatePPTX(path, 1); err == nil {
		t.Fatal("expected slide count mismatch")
	}
	invalid := filepath.Join(t.TempDir(), "invalid.pptx")
	makeInvalidPPTX(t, invalid)
	if err := validatePPTX(invalid, 1); err == nil {
		t.Fatal("expected invalid pptx error")
	}
}

func TestPackageBundleIncludesRegisteredArtifacts(t *testing.T) {
	store, err := workflow.NewFileArtifactStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	imagePath := writeTestPNG(t, store, "slide_images/slide_01.png", color.RGBA{A: 255})
	if _, err := store.Register(context.Background(), workflow.RegisterArtifactRequest{Name: "slide_images/slide_01.png", Type: "image", Producer: "generate_slides", Path: imagePath}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutText(context.Background(), "deck.pptx", "pptx", "assemble_pptx", "pptx bytes"); err != nil {
		t.Fatal(err)
	}
	if _, err := (Plugin{}).packageBundle(context.Background(), workflow.NodeRequest{
		Spec:  workflow.NodeSpec{ID: "package_bundle", Kind: "promptflow.package"},
		Store: store,
	}); err != nil {
		t.Fatal(err)
	}
	var bundle struct {
		Artifacts []workflow.ArtifactRef `json:"artifacts"`
		Status    string                 `json:"status"`
	}
	if _, err := store.ReadJSON(context.Background(), "delivery_bundle.json", &bundle); err != nil {
		t.Fatal(err)
	}
	if bundle.Status != "completed" {
		t.Fatalf("status = %s", bundle.Status)
	}
	names := map[string]bool{}
	for _, artifact := range bundle.Artifacts {
		names[artifact.Name] = true
	}
	for _, name := range []string{"slide_images/slide_01.png", "deck.pptx"} {
		if !names[name] {
			t.Fatalf("missing %s in bundle: %+v", name, bundle.Artifacts)
		}
	}
}

func TestGenerateImageUsesPlaceholderUnlessImagesRequired(t *testing.T) {
	store, err := workflow.NewFileArtifactStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "placeholder.png")
	result, err := generateImage(context.Background(), workflow.NodeRequest{Store: store, Spec: workflow.NodeSpec{Config: map[string]any{"fallback_policy": "dev_placeholder"}}}, workflow.ImageRequest{OutputPath: output, Size: "32x24"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Model != "local-placeholder" {
		t.Fatalf("model = %s", result.Model)
	}
	if _, err := os.Stat(output); err != nil {
		t.Fatal(err)
	}
	_, err = generateImage(context.Background(), workflow.NodeRequest{Store: store, Spec: workflow.NodeSpec{Config: map[string]any{"fallback_policy": "strict"}}}, workflow.ImageRequest{OutputPath: filepath.Join(t.TempDir(), "required.png")})
	if err == nil {
		t.Fatal("expected image runtime error")
	}
}

func writeTestPNG(t *testing.T, store *workflow.FileArtifactStore, name string, c color.Color) string {
	t.Helper()
	path, err := store.Path(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeSolidPNG(path, c, 160, 90); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeSolidPNG(path string, c color.Color, width, height int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, c)
		}
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := png.Encode(file, img); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func writeTransparentFixture(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 16; y < 48; y++ {
		for x := 16; x < 48; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 20, G: 80, B: 220, A: 255})
		}
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := png.Encode(file, img); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func writeCheckerboardPNG(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			if (x/8+y/8)%2 == 0 {
				img.SetRGBA(x, y, color.RGBA{R: 224, G: 224, B: 224, A: 255})
			} else {
				img.SetRGBA(x, y, color.RGBA{R: 176, G: 176, B: 176, A: 255})
			}
		}
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := png.Encode(file, img); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func readZipText(t *testing.T, path, name string) string {
	t.Helper()
	reader, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for _, file := range reader.File {
		if file.Name != name {
			continue
		}
		rc, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(rc)
		closeErr := rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
		return string(data)
	}
	t.Fatalf("missing %s in %s", name, path)
	return ""
}

func makePPTX(t *testing.T, path string, slides int) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zipWriter := zip.NewWriter(file)
	parts := map[string]string{
		"[Content_Types].xml":             `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"></Types>`,
		"ppt/presentation.xml":            `<p:presentation xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"></p:presentation>`,
		"ppt/_rels/presentation.xml.rels": `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"></Relationships>`,
	}
	for name, content := range parts {
		w, err := zipWriter.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= slides; i++ {
		w, err := zipWriter.Create("ppt/slides/slide" + strconv.Itoa(i) + ".xml")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(`<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"></p:sld>`)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func makeInvalidPPTX(t *testing.T, path string) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zipWriter := zip.NewWriter(file)
	for _, name := range []string{"[Content_Types].xml", "ppt/presentation.xml", "ppt/_rels/presentation.xml.rels", "ppt/slides/slide1.xml"} {
		w, err := zipWriter.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("<xml/>")); err != nil {
			t.Fatal(err)
		}
	}
	if err := zipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
