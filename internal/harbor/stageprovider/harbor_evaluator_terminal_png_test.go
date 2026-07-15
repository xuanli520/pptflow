package stageprovider

import (
	"bytes"
	"errors"
	"image"
	"image/png"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestRenderHarborEvaluatorTerminalPNGIsValidBoundedAndDeterministic(t *testing.T) {
	text := strings.Join([]string{
		"Harbor 0.18.0 evaluator completed",
		"qwen pass@4: 1.000",
		"ANTHROPIC_AUTH_TOKEN=<redacted>",
	}, "\n")
	digest := workflowkit.SHA256Fingerprint([]byte(text))
	first, err := RenderHarborEvaluatorTerminalPNG(text, digest)
	if err != nil {
		t.Fatalf("render terminal PNG: %v", err)
	}
	second, err := RenderHarborEvaluatorTerminalPNG(text, digest)
	if err != nil {
		t.Fatalf("rerender terminal PNG: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("identical redacted terminal evidence rendered different PNG bytes")
	}
	decoded, format, err := image.Decode(bytes.NewReader(first))
	if err != nil {
		t.Fatalf("decode terminal PNG: %v", err)
	}
	if format != "png" {
		t.Fatalf("decoded terminal evidence format = %q, want png", format)
	}
	if _, err := png.Decode(bytes.NewReader(first)); err != nil {
		t.Fatalf("strict PNG decoder rejected renderer output: %v", err)
	}
	bounds := decoded.Bounds()
	if bounds.Dx() < harborEvaluatorTerminalPaddingX*2+harborEvaluatorTerminalMinColumns*harborEvaluatorTerminalGlyphAdvance ||
		bounds.Dx() > harborEvaluatorTerminalPaddingX*2+harborEvaluatorTerminalMaxColumns*harborEvaluatorTerminalGlyphAdvance ||
		bounds.Dy() <= harborEvaluatorTerminalPaddingY*2+harborEvaluatorTerminalHeaderLines*harborEvaluatorTerminalLineAdvance ||
		bounds.Dy() > harborEvaluatorTerminalPaddingY*2+harborEvaluatorTerminalHeaderLines*harborEvaluatorTerminalLineAdvance+harborEvaluatorTerminalDividerGap+harborEvaluatorTerminalMaxLines*harborEvaluatorTerminalLineAdvance {
		t.Fatalf("terminal PNG bounds = %v, outside renderer limits", bounds)
	}
	if !terminalPNGContainsNonBackgroundPixel(decoded) {
		t.Fatal("terminal PNG did not contain any rendered evidence pixels")
	}

	changedText := strings.Replace(text, "1.000", "0.000", 1)
	changed, err := RenderHarborEvaluatorTerminalPNG(changedText, workflowkit.SHA256Fingerprint([]byte(changedText)))
	if err != nil {
		t.Fatalf("render changed terminal PNG: %v", err)
	}
	if bytes.Equal(first, changed) {
		t.Fatal("different source text/digest rendered the same evidence PNG")
	}
}

func TestRenderHarborEvaluatorTerminalPNGRejectsUnsafeOrNoncanonicalInput(t *testing.T) {
	for name, text := range map[string]string{
		"empty":             "",
		"trailing newline":  "completed\n",
		"CRLF":              "completed\r\nnext",
		"tab":               "completed\tnext",
		"unicode":           "completed \u4e2d",
		"raw secret":        "ANTHROPIC_AUTH_TOKEN=raw-secret-value",
		"too many lines":    strings.Repeat("x\n", harborEvaluatorTerminalMaxLines) + "x",
		"too many columns":  strings.Repeat("x", harborEvaluatorTerminalMaxColumns+1),
		"control character": "completed\x1b[31m",
	} {
		t.Run(name, func(t *testing.T) {
			digest := workflowkit.SHA256Fingerprint([]byte(text))
			_, err := RenderHarborEvaluatorTerminalPNG(text, digest)
			if !errors.Is(err, ErrInvalidHarborEvaluatorTerminalPNGInput) {
				t.Fatalf("render %s error = %v, want invalid terminal PNG input", name, err)
			}
			if strings.Contains(err.Error(), "raw-secret-value") {
				t.Fatalf("renderer error leaked raw secret: %v", err)
			}
		})
	}

	text := "safe completed terminal transcript"
	wrongDigest := workflowkit.SHA256Fingerprint([]byte("another transcript"))
	if _, err := RenderHarborEvaluatorTerminalPNG(text, wrongDigest); !errors.Is(err, ErrInvalidHarborEvaluatorTerminalPNGInput) {
		t.Fatalf("source digest mismatch error = %v, want invalid terminal PNG input", err)
	}
}

func TestHarborEvaluatorTerminalPNGBitmapFontCoversPrintableASCII(t *testing.T) {
	if got, want := len(harborEvaluatorTerminalGlyphs), 0x7e-0x20+1; got != want {
		t.Fatalf("fixed terminal font glyph count = %d, want %d printable ASCII glyphs", got, want)
	}
	for character := byte(0x20); character <= 0x7e; character++ {
		glyph, present := harborEvaluatorTerminalGlyphs[character]
		if !present {
			t.Fatalf("fixed terminal font omits printable ASCII byte 0x%02x", character)
		}
		if character != ' ' {
			nonempty := false
			for _, row := range glyph {
				nonempty = nonempty || row != 0
			}
			if !nonempty {
				t.Fatalf("fixed terminal font has an empty printable glyph 0x%02x", character)
			}
		}
	}
}

func terminalPNGContainsNonBackgroundPixel(picture image.Image) bool {
	for y := picture.Bounds().Min.Y; y < picture.Bounds().Max.Y; y++ {
		for x := picture.Bounds().Min.X; x < picture.Bounds().Max.X; x++ {
			red, green, blue, alpha := picture.At(x, y).RGBA()
			if red != uint32(harborEvaluatorTerminalBackground.R)*0x101 ||
				green != uint32(harborEvaluatorTerminalBackground.G)*0x101 ||
				blue != uint32(harborEvaluatorTerminalBackground.B)*0x101 ||
				alpha != uint32(harborEvaluatorTerminalBackground.A)*0x101 {
				return true
			}
		}
	}
	return false
}
