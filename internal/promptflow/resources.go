package promptflow

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/xuanli520/pptflow/internal/workflow"
)

func extractVisualResource(ctx context.Context, req workflow.NodeRequest, slide SlideLayout, region LayoutRegion, analysis LayoutAnalysis, outputPath string) (string, error) {
	matte := extractionMatteColor()
	if !boolConfig(req.Spec.Config, "disable_source_image_extraction") && wantsTransparentResource(region) && shouldUseImage2SourceExtraction(region) && req.Runtimes.Image != nil && req.Runtimes.Image.Configured() {
		result, err := req.Runtimes.Image.Generate(ctx, workflow.ImageRequest{
			Model:          stringConfig(req.Spec.Config, "model"),
			Prompt:         buildExtractPrompt(region, matte),
			Size:           stringConfig(req.Spec.Config, "size", "1024x1024"),
			Quality:        stringConfig(req.Spec.Config, "quality", "high"),
			OutputPath:     outputPath,
			SourceImages:   []workflow.ImageSource{{Path: slide.ImagePath, Role: "source_slide", Detail: "high"}},
			TimeoutSeconds: intConfig(req.Spec.Config, "timeout_seconds", 180),
		})
		if err == nil && strings.TrimSpace(result.Path) != "" {
			quality := inspectTransparentResource(result.Path, matte)
			if quality.CheckerboardDetected {
				return fallbackExtractVisualResource(slide, region, analysis, outputPath)
			}
			if !quality.HasAlpha && quality.MatteDetected {
				_ = removeMatteBackground(result.Path, matte)
				quality = inspectTransparentResource(result.Path, matte)
			}
			if !quality.HasAlpha {
				_ = removeNearWhiteBackground(result.Path)
				quality = inspectTransparentResource(result.Path, matte)
			}
			if quality.Pass {
				return "image2-source-extraction", nil
			}
		}
	}
	return fallbackExtractVisualResource(slide, region, analysis, outputPath)
}

func fallbackExtractVisualResource(slide SlideLayout, region LayoutRegion, analysis LayoutAnalysis, outputPath string) (string, error) {
	if err := cropRegionFromSlide(slide.ImagePath, region, analysis.SlideWidth, analysis.SlideHeight, outputPath); err != nil {
		return "", err
	}
	if wantsTransparentResource(region) {
		if err := removeNearWhiteBackground(outputPath); err != nil {
			return "", err
		}
		return "local-crop-background-remove", nil
	}
	return "local-crop", nil
}

func shouldUseImage2SourceExtraction(region LayoutRegion) bool {
	if isCompositeVisualRegion(region) {
		return false
	}
	return hasExtractionSemantics(region)
}

func hasExtractionSemantics(region LayoutRegion) bool {
	if strings.TrimSpace(region.ImageDesc) != "" || strings.TrimSpace(region.CropHint) != "" {
		return true
	}
	if region.Image != nil {
		if strings.TrimSpace(region.Image.AssetKind) != "" || strings.TrimSpace(region.Image.CropHint) != "" {
			return true
		}
	}
	roleID := strings.ToLower(strings.TrimSpace(region.Role + " " + region.ID))
	if roleID == "" {
		return false
	}
	generic := map[string]bool{
		"image": true, "icon": true, "visual": true, "graphic": true,
		"shape": true, "asset": true, "picture": true,
	}
	return !generic[roleID]
}

func isCompositeVisualRegion(region LayoutRegion) bool {
	roleID := strings.ToLower(strings.TrimSpace(region.ID + " " + region.Role + " " + region.ImageDesc))
	needles := []string{
		"step_icon_row", "icon_row", "metric_icon_column", "icon_column",
		"metric_sparklines", "sparkline", "workflow", "process_row",
		"timeline", "number_badge", "label_pill", "status_pill",
	}
	for _, needle := range needles {
		if strings.Contains(roleID, needle) {
			return true
		}
	}
	if strings.EqualFold(region.Type, "icon") && region.W > 0 && region.H > 0 {
		ratio := region.W / region.H
		return ratio >= 2.5 || ratio <= 0.35
	}
	return false
}

func wantsTransparentResource(region LayoutRegion) bool {
	if region.Image != nil {
		assetKind := strings.ToLower(strings.TrimSpace(region.Image.AssetKind))
		if assetKind == "transparent_icon" || assetKind == "logo" || assetKind == "illustration" {
			return true
		}
		if strings.EqualFold(region.Image.BackgroundStrategy, "remove") && (strings.EqualFold(region.Type, "icon") || strings.Contains(strings.ToLower(region.Role), "icon") || strings.Contains(strings.ToLower(region.Role), "logo")) {
			return true
		}
	}
	idRole := strings.ToLower(region.ID + " " + region.Role)
	return region.HasBackground && (strings.EqualFold(region.Type, "icon") || strings.Contains(idRole, "icon") || strings.Contains(idRole, "logo"))
}

func imageFit(region LayoutRegion) string {
	if region.Image != nil && strings.TrimSpace(region.Image.Fit) != "" {
		return region.Image.Fit
	}
	return "stretch"
}

func imageMask(region LayoutRegion) string {
	if region.Image != nil {
		return region.Image.MaskShape
	}
	return ""
}

type transparentResourceQuality struct {
	HasAlpha             bool
	AlphaCoverage        float64
	CheckerboardDetected bool
	MatteDetected        bool
	Pass                 bool
}

func extractionMatteColor() color.RGBA {
	return color.RGBA{R: 0, G: 255, B: 0, A: 255}
}

func matteHex(c color.RGBA) string {
	return fmt.Sprintf("#%02X%02X%02X", c.R, c.G, c.B)
}

func removeNearWhiteBackground(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	src, err := png.Decode(file)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	bounds := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(dst, dst.Bounds(), src, bounds.Min, draw.Src)
	for y := 0; y < dst.Bounds().Dy(); y++ {
		for x := 0; x < dst.Bounds().Dx(); x++ {
			r, g, b, a := dst.At(x, y).RGBA()
			if a == 0 {
				continue
			}
			r8, g8, b8 := uint8(r>>8), uint8(g>>8), uint8(b>>8)
			if r8 >= 245 && g8 >= 245 && b8 >= 245 {
				dst.SetRGBA(x, y, color.RGBA{R: r8, G: g8, B: b8, A: 0})
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	encodeErr := png.Encode(out, dst)
	closeErr = out.Close()
	if encodeErr != nil {
		return encodeErr
	}
	return closeErr
}

func removeMatteBackground(path string, matte color.RGBA) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	src, err := png.Decode(file)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	bounds := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(dst, dst.Bounds(), src, bounds.Min, draw.Src)
	for y := 0; y < dst.Bounds().Dy(); y++ {
		for x := 0; x < dst.Bounds().Dx(); x++ {
			p := dst.RGBAAt(x, y)
			if p.A == 0 {
				continue
			}
			distance := colorDistance(p, matte)
			switch {
			case distance <= 55:
				p.A = 0
			case distance <= 120:
				p.A = uint8(float64(p.A) * ((distance - 55) / 65))
			default:
				continue
			}
			dst.SetRGBA(x, y, p)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	encodeErr := png.Encode(out, dst)
	closeErr = out.Close()
	if encodeErr != nil {
		return encodeErr
	}
	return closeErr
}

func pngHasAlpha(path string) bool {
	return inspectTransparentResource(path, extractionMatteColor()).HasAlpha
}

func inspectTransparentResource(path string, matte color.RGBA) transparentResourceQuality {
	file, err := os.Open(path)
	if err != nil {
		return transparentResourceQuality{}
	}
	img, err := png.Decode(file)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return transparentResourceQuality{}
	}
	quality := inspectTransparentImage(img, matte)
	quality.CheckerboardDetected = pngImageHasCheckerboard(img)
	quality.Pass = quality.HasAlpha && quality.AlphaCoverage >= 0.01 && !quality.CheckerboardDetected && !quality.MatteDetected
	return quality
}

func inspectTransparentImage(img image.Image, matte color.RGBA) transparentResourceQuality {
	bounds := img.Bounds()
	total := 0
	alphaPixels := 0
	mattePixels := 0
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, a := img.At(x, y).RGBA()
			total++
			if a < 0xf000 {
				alphaPixels++
				continue
			}
			p := color.RGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: uint8(a >> 8)}
			if colorDistance(p, matte) <= 45 {
				mattePixels++
			}
		}
	}
	quality := transparentResourceQuality{}
	if total == 0 {
		return quality
	}
	quality.HasAlpha = alphaPixels > 0
	quality.AlphaCoverage = float64(alphaPixels) / float64(total)
	quality.MatteDetected = float64(mattePixels)/float64(total) >= 0.03
	return quality
}

func pngImageHasCheckerboard(img image.Image) bool {
	bounds := img.Bounds()
	if bounds.Dx() < 16 || bounds.Dy() < 16 {
		return false
	}
	total := 0
	neutral := 0
	buckets := map[int]int{}
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, a := img.At(x, y).RGBA()
			if a < 0xf000 {
				continue
			}
			total++
			r8, g8, b8 := int(r>>8), int(g>>8), int(b>>8)
			if !isNeutralCheckerCandidate(r8, g8, b8) {
				continue
			}
			neutral++
			brightness := (r8 + g8 + b8) / 3
			buckets[brightness/16]++
		}
	}
	if total == 0 || float64(neutral)/float64(total) < 0.18 {
		return false
	}
	firstBucket, secondBucket := topTwoBuckets(buckets)
	if firstBucket.count == 0 || secondBucket.count == 0 || absInt(firstBucket.key-secondBucket.key)*16 < 24 {
		return false
	}
	transition, comparable := checkerboardTransitions(img, 8)
	if comparable == 0 {
		return false
	}
	return float64(transition)/float64(comparable) >= 0.22
}

type bucketCount struct {
	key   int
	count int
}

func topTwoBuckets(buckets map[int]int) (bucketCount, bucketCount) {
	first := bucketCount{}
	second := bucketCount{}
	for key, count := range buckets {
		item := bucketCount{key: key, count: count}
		if item.count > first.count {
			second = first
			first = item
			continue
		}
		if item.count > second.count {
			second = item
		}
	}
	return first, second
}

func checkerboardTransitions(img image.Image, stride int) (int, int) {
	bounds := img.Bounds()
	transition := 0
	comparable := 0
	for y := bounds.Min.Y; y < bounds.Max.Y-stride; y += stride {
		for x := bounds.Min.X; x < bounds.Max.X-stride; x += stride {
			base, ok := neutralBrightness(img, x, y)
			if !ok {
				continue
			}
			for _, point := range [][2]int{{x + stride, y}, {x, y + stride}} {
				next, ok := neutralBrightness(img, point[0], point[1])
				if !ok {
					continue
				}
				comparable++
				if absInt(base-next) >= 24 {
					transition++
				}
			}
		}
	}
	return transition, comparable
}

func neutralBrightness(img image.Image, x, y int) (int, bool) {
	r, g, b, a := img.At(x, y).RGBA()
	if a < 0xf000 {
		return 0, false
	}
	r8, g8, b8 := int(r>>8), int(g>>8), int(b>>8)
	if !isNeutralCheckerCandidate(r8, g8, b8) {
		return 0, false
	}
	return (r8 + g8 + b8) / 3, true
}

func isNeutralCheckerCandidate(r, g, b int) bool {
	maxValue := maxInt(r, maxInt(g, b))
	minValue := minInt(r, minInt(g, b))
	brightness := (r + g + b) / 3
	return maxValue-minValue <= 18 && brightness >= 145 && brightness <= 250
}

func colorDistance(a, b color.RGBA) float64 {
	dr := float64(int(a.R) - int(b.R))
	dg := float64(int(a.G) - int(b.G))
	db := float64(int(a.B) - int(b.B))
	return math.Sqrt(dr*dr + dg*dg + db*db)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
