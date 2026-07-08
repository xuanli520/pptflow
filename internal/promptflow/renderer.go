package promptflow

import (
	"fmt"
	"strconv"
	"strings"
)

func regionText(region LayoutRegion) string {
	if region.Text != nil && strings.TrimSpace(region.Text.Content) != "" {
		return region.Text.Content
	}
	return region.TextContent
}

func effectiveTextSpec(region LayoutRegion, styleSpec StyleSpec) TextSpec {
	text := TextSpec{}
	if region.Text != nil {
		text = *region.Text
	}
	if text.Content == "" {
		text.Content = region.TextContent
	}
	if text.Alignment == "" {
		text.Alignment = region.Alignment
	}
	if text.Alignment == "" && strings.EqualFold(region.Role, "status_pill") {
		text.Alignment = "center"
	}
	if text.VerticalAlignment == "" && shapeHasInlineText(region) {
		text.VerticalAlignment = "middle"
	}
	if text.FontSizePt <= 0 {
		text.FontSizePt = float64(regionFontSize(region))
	}
	if text.Color == "" {
		text.Color = firstNonEmptyString(region.FontColorEst, styleSpec.ColorScheme.Foreground, "#1F1F1F")
	}
	if text.FontFamily == "" {
		text.FontFamily = preferredFont(styleSpec, region.Type == "title" || region.Role == "title")
	}
	return text
}

func shapeHasInlineText(region LayoutRegion) bool {
	return strings.TrimSpace(regionText(region)) != "" || region.Text != nil && len(region.Text.Runs) > 0
}

func textFontSize(text TextSpec, region LayoutRegion) int {
	size := text.FontSizePt
	if size <= 0 {
		size = float64(regionFontSize(region))
	}
	return int(size*100 + 0.5)
}

func isBoldWeight(weight string) bool {
	weight = strings.ToLower(strings.TrimSpace(weight))
	if weight == "bold" || weight == "semibold" || weight == "demibold" {
		return true
	}
	if n, err := strconv.Atoi(weight); err == nil {
		return n >= 600
	}
	return false
}

func pptxBoldAttr(bold bool) string {
	if bold {
		return ` b="1"`
	}
	return ""
}

func pptxVerticalAnchor(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "middle", "center", "centre":
		return "ctr"
	case "bottom":
		return "b"
	default:
		return "t"
	}
}

func ptEMU(points float64) int64 {
	if points <= 0 {
		return 0
	}
	return int64(points*12700 + 0.5)
}

func pptxShapePreset(region LayoutRegion) string {
	if region.Shape != nil {
		switch strings.ToLower(strings.TrimSpace(region.Shape.Kind)) {
		case "round_rect", "roundrect":
			return "roundRect"
		case "ellipse", "circle", "oval":
			return "ellipse"
		case "line":
			return "line"
		case "triangle":
			return "triangle"
		case "rect", "rectangle":
			return "rect"
		}
	}
	if looksLikeLine(region) {
		return "line"
	}
	roleID := strings.ToLower(region.Role + " " + region.ID)
	switch {
	case strings.Contains(roleID, "pill"), strings.Contains(roleID, "card"):
		return "roundRect"
	case strings.Contains(roleID, "badge"), strings.Contains(roleID, "node"), strings.Contains(roleID, "dot"), strings.Contains(roleID, "number"):
		return "ellipse"
	default:
		return "rect"
	}
}

func looksLikeLine(region LayoutRegion) bool {
	if strings.EqualFold(region.Type, "line") {
		return true
	}
	roleID := strings.ToLower(region.Role + " " + region.ID)
	if strings.Contains(roleID, "timeline_axis") || strings.Contains(roleID, "connector") || strings.Contains(roleID, "divider") || strings.Contains(roleID, "tick") || strings.Contains(roleID, "rule") {
		return true
	}
	return (region.W > 0 && region.H > 0 && (region.H <= 0.06 || region.W <= 0.06)) && strings.Contains(roleID, "line")
}

func pptxFill(region LayoutRegion, styleSpec StyleSpec) string {
	if strings.EqualFold(region.Type, "line") || pptxShapePreset(region) == "line" {
		return `<a:noFill/>`
	}
	fill := region.Fill
	if fill == nil {
		color := shapeFillColor(region, styleSpec)
		return fmt.Sprintf(`<a:solidFill><a:srgbClr val="%s"/></a:solidFill>`, color)
	}
	if strings.EqualFold(fill.Type, "none") {
		return `<a:noFill/>`
	}
	color := normalizeHexColor(firstNonEmptyString(fill.Color, shapeFillColor(region, styleSpec)))
	alpha := effectiveAlpha(fill.Alpha, region.Opacity)
	return fmt.Sprintf(`<a:solidFill><a:srgbClr val="%s">%s</a:srgbClr></a:solidFill>`, color, pptxAlpha(alpha))
}

func pptxStroke(region LayoutRegion, styleSpec StyleSpec) string {
	stroke := region.Stroke
	if stroke == nil {
		if looksLikeLine(region) {
			color := normalizeHexColor(firstNonEmptyString(styleSpec.ColorScheme.Muted, styleSpec.ColorScheme.Accent, "#CBD5E1"))
			return fmt.Sprintf(`<a:ln w="%d" cap="rnd"><a:solidFill><a:srgbClr val="%s"/></a:solidFill><a:prstDash val="solid"/></a:ln>`, ptEMU(1.5), color)
		}
		return `<a:ln><a:noFill/></a:ln>`
	}
	if strings.EqualFold(stroke.Type, "none") {
		return `<a:ln><a:noFill/></a:ln>`
	}
	width := stroke.WidthPt
	if width <= 0 {
		width = 1
	}
	capAttr := ""
	if strings.EqualFold(stroke.Cap, "round") {
		capAttr = ` cap="rnd"`
	}
	color := normalizeHexColor(firstNonEmptyString(stroke.Color, styleSpec.ColorScheme.Muted, "#CBD5E1"))
	alpha := effectiveAlpha(stroke.Alpha, region.Opacity)
	return fmt.Sprintf(`<a:ln w="%d"%s><a:solidFill><a:srgbClr val="%s">%s</a:srgbClr></a:solidFill><a:prstDash val="%s"/></a:ln>`, ptEMU(width), capAttr, color, pptxAlpha(alpha), pptxDash(stroke.Dash))
}

func pptxShadow(region LayoutRegion) string {
	if region.Shadow == nil || !region.Shadow.Enabled {
		return ""
	}
	shadow := region.Shadow
	color := normalizeHexColor(firstNonEmptyString(shadow.Color, "#000000"))
	alpha := shadow.Alpha
	if alpha <= 0 {
		alpha = 0.18
	}
	return fmt.Sprintf(`<a:effectLst><a:outerShdw blurRad="%d" dist="%d" dir="%d" algn="tl"><a:srgbClr val="%s">%s</a:srgbClr></a:outerShdw></a:effectLst>`, ptEMU(shadow.BlurPt), ptEMU(shadow.DistancePt), int(shadow.Angle*60000+0.5), color, pptxAlpha(alpha))
}

func pptxAlpha(alpha float64) string {
	if alpha <= 0 {
		return ""
	}
	if alpha > 1 {
		alpha = alpha / 100
	}
	if alpha >= 1 {
		return ""
	}
	return fmt.Sprintf(`<a:alpha val="%d"/>`, int(alpha*100000+0.5))
}

func effectiveAlpha(values ...float64) float64 {
	alpha := 1.0
	for _, value := range values {
		if value <= 0 {
			continue
		}
		if value > 1 {
			value = value / 100
		}
		alpha *= value
	}
	return alpha
}

func pptxDash(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "dash", "dashed":
		return "dash"
	case "dot", "dotted":
		return "dot"
	default:
		return "solid"
	}
}

func pptxPictureFill(region LayoutRegion) string {
	srcRect := pptxSourceRect(region)
	return srcRect + `<a:stretch><a:fillRect/></a:stretch>`
}

func pptxSourceRect(region LayoutRegion) string {
	if region.Image == nil {
		return ""
	}
	crop := strings.TrimSpace(region.Image.CropHint)
	if crop == "" {
		return ""
	}
	values := map[string]int{}
	for _, part := range strings.FieldsFunc(crop, func(r rune) bool {
		return r == ',' || r == ';' || r == ' '
	}) {
		key, raw, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(raw), "%"))
		if err != nil || n < 0 {
			continue
		}
		values[strings.ToLower(strings.TrimSpace(key))] = n * 1000
	}
	if len(values) == 0 {
		return ""
	}
	return fmt.Sprintf(`<a:srcRect l="%d" t="%d" r="%d" b="%d"/>`, values["l"], values["t"], values["r"], values["b"])
}

func pptxImageMask(region LayoutRegion) string {
	if region.Image == nil {
		return "rect"
	}
	switch strings.ToLower(strings.TrimSpace(region.Image.MaskShape)) {
	case "round_rect", "roundrect":
		return "roundRect"
	case "ellipse", "circle", "oval":
		return "ellipse"
	default:
		return "rect"
	}
}
