package stageprovider

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"strings"
	"unicode/utf8"

	"github.com/purplevoid/harbor-factory/internal/redact"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	// HarborEvaluatorTerminalPNGRendererID, Version, and SchemaVersion identify
	// this in-process deterministic renderer. It has no host-font, locale, or
	// timestamp dependency, so its renderer identity can safely be frozen by an
	// evaluator operation catalog/lock.
	HarborEvaluatorTerminalPNGRendererID            = "harbor-terminal-png"
	HarborEvaluatorTerminalPNGRendererVersion       = "1"
	HarborEvaluatorTerminalPNGRendererSchemaVersion = "image/png"

	harborEvaluatorTerminalMaxBytes   = 12 * 1024
	harborEvaluatorTerminalMaxLines   = 96
	harborEvaluatorTerminalMaxColumns = 120
	harborEvaluatorTerminalMinColumns = 80

	harborEvaluatorTerminalGlyphWidth   = 5
	harborEvaluatorTerminalGlyphHeight  = 7
	harborEvaluatorTerminalGlyphScale   = 2
	harborEvaluatorTerminalGlyphAdvance = (harborEvaluatorTerminalGlyphWidth + 1) * harborEvaluatorTerminalGlyphScale
	harborEvaluatorTerminalLineAdvance  = (harborEvaluatorTerminalGlyphHeight + 2) * harborEvaluatorTerminalGlyphScale
	harborEvaluatorTerminalPaddingX     = 24
	harborEvaluatorTerminalPaddingY     = 20
	harborEvaluatorTerminalHeaderLines  = 2
	harborEvaluatorTerminalDividerGap   = 12
)

// ErrInvalidHarborEvaluatorTerminalPNGInput identifies terminal evidence that
// cannot safely be rendered. Errors intentionally never include input text:
// callers may be handling process output containing a credential.
var ErrInvalidHarborEvaluatorTerminalPNGInput = errors.New("stage provider: invalid Harbor evaluator terminal PNG input")

var (
	harborEvaluatorTerminalBackground = color.RGBA{R: 17, G: 22, B: 25, A: 255}
	harborEvaluatorTerminalHeader     = color.RGBA{R: 37, G: 54, B: 63, A: 255}
	harborEvaluatorTerminalText       = color.RGBA{R: 226, G: 237, B: 232, A: 255}
	harborEvaluatorTerminalMetadata   = color.RGBA{R: 159, G: 207, B: 191, A: 255}
	harborEvaluatorTerminalDivider    = color.RGBA{R: 79, G: 113, B: 112, A: 255}
)

// RenderHarborEvaluatorTerminalPNG turns one already-redacted, canonical
// terminal transcript into the sole PNG evidence image for an evaluator run.
//
// The renderer accepts only printable ASCII plus LF. In particular, it rejects
// CRLF, tabs, ANSI escape sequences, Unicode, trailing newlines, and text that
// the repository redactor would still alter. sourceDigest must be the exact
// SHA-256 fingerprint of the accepted transcript. The resulting pixels display
// that digest and are generated with an embedded bitmap font, making repeated
// calls byte-for-byte deterministic for the same input.
func RenderHarborEvaluatorTerminalPNG(text string, sourceDigest workflowkit.Fingerprint) ([]byte, error) {
	lines, columns, err := validateHarborEvaluatorTerminalText(text, sourceDigest)
	if err != nil {
		return nil, err
	}

	headerSource := "source: " + string(sourceDigest)
	columns = max(columns, len("Harbor evaluator terminal evidence"), len(headerSource), harborEvaluatorTerminalMinColumns)
	width := harborEvaluatorTerminalPaddingX*2 + columns*harborEvaluatorTerminalGlyphAdvance
	height := harborEvaluatorTerminalPaddingY*2 + harborEvaluatorTerminalHeaderLines*harborEvaluatorTerminalLineAdvance + harborEvaluatorTerminalDividerGap + len(lines)*harborEvaluatorTerminalLineAdvance
	canvas := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(harborEvaluatorTerminalBackground), image.Point{}, draw.Src)

	x := harborEvaluatorTerminalPaddingX
	y := harborEvaluatorTerminalPaddingY
	drawHarborEvaluatorTerminalText(canvas, x, y, "Harbor evaluator terminal evidence", harborEvaluatorTerminalText)
	y += harborEvaluatorTerminalLineAdvance
	drawHarborEvaluatorTerminalText(canvas, x, y, headerSource, harborEvaluatorTerminalMetadata)
	y += harborEvaluatorTerminalLineAdvance + harborEvaluatorTerminalDividerGap/2
	draw.Draw(canvas, image.Rect(x, y, width-x, y+2), image.NewUniform(harborEvaluatorTerminalDivider), image.Point{}, draw.Src)
	y += harborEvaluatorTerminalDividerGap / 2
	for _, line := range lines {
		drawHarborEvaluatorTerminalText(canvas, x, y, line, harborEvaluatorTerminalText)
		y += harborEvaluatorTerminalLineAdvance
	}

	var encoded bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.BestCompression}
	if err := encoder.Encode(&encoded, canvas); err != nil {
		return nil, fmt.Errorf("render Harbor evaluator terminal PNG: %w", err)
	}
	return encoded.Bytes(), nil
}

func validateHarborEvaluatorTerminalText(text string, sourceDigest workflowkit.Fingerprint) ([]string, int, error) {
	if err := sourceDigest.Validate(); err != nil {
		return nil, 0, fmt.Errorf("%w: source digest is not canonical", ErrInvalidHarborEvaluatorTerminalPNGInput)
	}
	if text == "" || len(text) > harborEvaluatorTerminalMaxBytes || !utf8.ValidString(text) || strings.HasSuffix(text, "\n") {
		return nil, 0, fmt.Errorf("%w: terminal text is empty, oversized, invalid UTF-8, or has a trailing newline", ErrInvalidHarborEvaluatorTerminalPNGInput)
	}
	if redact.Text(text) != text {
		return nil, 0, fmt.Errorf("%w: terminal text is not fully redacted", ErrInvalidHarborEvaluatorTerminalPNGInput)
	}
	if workflowkit.SHA256Fingerprint([]byte(text)) != sourceDigest {
		return nil, 0, fmt.Errorf("%w: source digest does not match terminal text", ErrInvalidHarborEvaluatorTerminalPNGInput)
	}

	lines := strings.Split(text, "\n")
	if len(lines) > harborEvaluatorTerminalMaxLines {
		return nil, 0, fmt.Errorf("%w: terminal text has too many lines", ErrInvalidHarborEvaluatorTerminalPNGInput)
	}
	columns := 0
	for _, line := range lines {
		if len(line) > harborEvaluatorTerminalMaxColumns {
			return nil, 0, fmt.Errorf("%w: terminal text line is too wide", ErrInvalidHarborEvaluatorTerminalPNGInput)
		}
		columns = max(columns, len(line))
		for index := 0; index < len(line); index++ {
			if line[index] < 0x20 || line[index] > 0x7e {
				return nil, 0, fmt.Errorf("%w: terminal text contains a non-printable or non-ASCII byte", ErrInvalidHarborEvaluatorTerminalPNGInput)
			}
		}
	}
	return append([]string(nil), lines...), columns, nil
}

func drawHarborEvaluatorTerminalText(canvas *image.RGBA, x, y int, text string, foreground color.RGBA) {
	for index := 0; index < len(text); index++ {
		glyph, present := harborEvaluatorTerminalGlyphs[text[index]]
		if !present {
			glyph = harborEvaluatorTerminalGlyphs['?']
		}
		for row, mask := range glyph {
			for column := 0; column < harborEvaluatorTerminalGlyphWidth; column++ {
				if mask&(1<<uint(harborEvaluatorTerminalGlyphWidth-1-column)) == 0 {
					continue
				}
				left := x + column*harborEvaluatorTerminalGlyphScale
				top := y + row*harborEvaluatorTerminalGlyphScale
				draw.Draw(canvas, image.Rect(left, top, left+harborEvaluatorTerminalGlyphScale, top+harborEvaluatorTerminalGlyphScale), image.NewUniform(foreground), image.Point{}, draw.Src)
			}
		}
		x += harborEvaluatorTerminalGlyphAdvance
	}
}

// harborEvaluatorTerminalGlyphs is a fixed 5x7 ASCII bitmap font. Keeping it
// in source avoids dependence on a host font file, fontconfig, locale, or text
// shaping. Every printable ASCII byte has one fixed glyph; values are row bit
// masks with the most-significant of the low five bits drawn on the left.
var harborEvaluatorTerminalGlyphs = map[byte][harborEvaluatorTerminalGlyphHeight]uint8{
	' ':  {0, 0, 0, 0, 0, 0, 0},
	'!':  {4, 4, 4, 4, 4, 0, 4},
	'"':  {10, 10, 0, 0, 0, 0, 0},
	'#':  {10, 31, 10, 10, 31, 10, 10},
	'$':  {4, 15, 20, 14, 5, 30, 4},
	'%':  {25, 26, 4, 8, 22, 19, 0},
	'&':  {12, 18, 20, 8, 21, 18, 13},
	'\'': {4, 4, 8, 0, 0, 0, 0},
	'(':  {2, 4, 8, 8, 8, 4, 2},
	')':  {8, 4, 2, 2, 2, 4, 8},
	'*':  {0, 4, 21, 14, 21, 4, 0},
	'+':  {0, 4, 4, 31, 4, 4, 0},
	',':  {0, 0, 0, 0, 4, 4, 8},
	'-':  {0, 0, 0, 31, 0, 0, 0},
	'.':  {0, 0, 0, 0, 0, 12, 12},
	'/':  {1, 2, 4, 8, 16, 0, 0},
	'0':  {14, 17, 19, 21, 25, 17, 14},
	'1':  {4, 12, 4, 4, 4, 4, 14},
	'2':  {14, 17, 1, 2, 4, 8, 31},
	'3':  {30, 1, 1, 14, 1, 1, 30},
	'4':  {2, 6, 10, 18, 31, 2, 2},
	'5':  {31, 16, 16, 30, 1, 1, 30},
	'6':  {14, 16, 16, 30, 17, 17, 14},
	'7':  {31, 1, 2, 4, 8, 8, 8},
	'8':  {14, 17, 17, 14, 17, 17, 14},
	'9':  {14, 17, 17, 15, 1, 1, 14},
	':':  {0, 12, 12, 0, 12, 12, 0},
	';':  {0, 12, 12, 0, 12, 4, 8},
	'<':  {2, 4, 8, 16, 8, 4, 2},
	'=':  {0, 0, 31, 0, 31, 0, 0},
	'>':  {8, 4, 2, 1, 2, 4, 8},
	'?':  {14, 17, 1, 2, 4, 0, 4},
	'@':  {14, 17, 23, 21, 23, 16, 14},
	'A':  {14, 17, 17, 31, 17, 17, 17},
	'B':  {30, 17, 17, 30, 17, 17, 30},
	'C':  {14, 17, 16, 16, 16, 17, 14},
	'D':  {30, 17, 17, 17, 17, 17, 30},
	'E':  {31, 16, 16, 30, 16, 16, 31},
	'F':  {31, 16, 16, 30, 16, 16, 16},
	'G':  {14, 17, 16, 23, 17, 17, 15},
	'H':  {17, 17, 17, 31, 17, 17, 17},
	'I':  {14, 4, 4, 4, 4, 4, 14},
	'J':  {7, 2, 2, 2, 18, 18, 12},
	'K':  {17, 18, 20, 24, 20, 18, 17},
	'L':  {16, 16, 16, 16, 16, 16, 31},
	'M':  {17, 27, 21, 21, 17, 17, 17},
	'N':  {17, 25, 21, 19, 17, 17, 17},
	'O':  {14, 17, 17, 17, 17, 17, 14},
	'P':  {30, 17, 17, 30, 16, 16, 16},
	'Q':  {14, 17, 17, 17, 21, 18, 13},
	'R':  {30, 17, 17, 30, 20, 18, 17},
	'S':  {15, 16, 16, 14, 1, 1, 30},
	'T':  {31, 4, 4, 4, 4, 4, 4},
	'U':  {17, 17, 17, 17, 17, 17, 14},
	'V':  {17, 17, 17, 17, 17, 10, 4},
	'W':  {17, 17, 17, 21, 21, 21, 10},
	'X':  {17, 17, 10, 4, 10, 17, 17},
	'Y':  {17, 17, 10, 4, 4, 4, 4},
	'Z':  {31, 1, 2, 4, 8, 16, 31},
	'[':  {14, 8, 8, 8, 8, 8, 14},
	'\\': {16, 8, 4, 2, 1, 0, 0},
	']':  {14, 2, 2, 2, 2, 2, 14},
	'^':  {4, 10, 17, 0, 0, 0, 0},
	'_':  {0, 0, 0, 0, 0, 0, 31},
	'`':  {8, 4, 0, 0, 0, 0, 0},
	'a':  {0, 0, 14, 1, 15, 17, 15},
	'b':  {16, 16, 22, 25, 17, 17, 30},
	'c':  {0, 0, 14, 17, 16, 17, 14},
	'd':  {1, 1, 13, 19, 17, 17, 15},
	'e':  {0, 0, 14, 17, 31, 16, 14},
	'f':  {6, 9, 8, 28, 8, 8, 8},
	'g':  {0, 0, 15, 17, 15, 1, 14},
	'h':  {16, 16, 22, 25, 17, 17, 17},
	'i':  {4, 0, 12, 4, 4, 4, 14},
	'j':  {2, 0, 6, 2, 2, 18, 12},
	'k':  {16, 16, 18, 20, 24, 20, 18},
	'l':  {12, 4, 4, 4, 4, 4, 14},
	'm':  {0, 0, 26, 21, 21, 21, 21},
	'n':  {0, 0, 22, 25, 17, 17, 17},
	'o':  {0, 0, 14, 17, 17, 17, 14},
	'p':  {0, 0, 30, 17, 30, 16, 16},
	'q':  {0, 0, 13, 19, 15, 1, 1},
	'r':  {0, 0, 22, 25, 16, 16, 16},
	's':  {0, 0, 15, 16, 14, 1, 30},
	't':  {8, 8, 28, 8, 8, 9, 6},
	'u':  {0, 0, 17, 17, 17, 19, 13},
	'v':  {0, 0, 17, 17, 17, 10, 4},
	'w':  {0, 0, 17, 17, 21, 21, 10},
	'x':  {0, 0, 17, 10, 4, 10, 17},
	'y':  {0, 0, 17, 17, 15, 1, 14},
	'z':  {0, 0, 31, 2, 4, 8, 31},
	'{':  {2, 4, 4, 8, 4, 4, 2},
	'|':  {4, 4, 4, 4, 4, 4, 4},
	'}':  {8, 4, 4, 2, 4, 4, 8},
	'~':  {0, 0, 9, 22, 0, 0, 0},
}
