package evidence

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
)

const (
	canvasWidth  = 1200
	canvasHeight = 720
)

var (
	background = color.RGBA{R: 15, G: 19, B: 24, A: 255}
	panel      = color.RGBA{R: 28, G: 34, B: 42, A: 255}
	panelAlt   = color.RGBA{R: 35, G: 43, B: 52, A: 255}
	text       = color.RGBA{R: 233, G: 238, B: 244, A: 255}
	muted      = color.RGBA{R: 156, G: 168, B: 181, A: 255}
	accent     = color.RGBA{R: 50, G: 184, B: 198, A: 255}
	passed     = color.RGBA{R: 65, G: 190, B: 120, A: 255}
	failed     = color.RGBA{R: 225, G: 92, B: 92, A: 255}
)

// RenderPassAt4PNG creates a deterministic, human-readable evidence image from
// the canonical Harbor result. It intentionally uses only the Go standard
// library so evidence generation also works in headless CI environments.
func RenderPassAt4PNG(slot string, result domain.TrialResult) ([]byte, error) {
	return RenderPassAt4PNGWithStatus(slot, result, RenderStatus{Verified: summaryCoherent(result), RawResultSHA256: result.RawResultSHA256})
}

type RenderStatus struct {
	Verified           bool
	RawResultSHA256    string
	CommandEvidenceSHA string
}

// RenderPassAt4PNGWithStatus keeps authority for the completion decision in
// the caller, which owns the canonical Harbor validation and command outcome.
func RenderPassAt4PNGWithStatus(slot string, result domain.TrialResult, status RenderStatus) ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, canvasWidth, canvasHeight))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: background}, image.Point{}, draw.Src)

	drawRect(img, 0, 0, canvasWidth, 8, accent)
	drawText(img, 48, 38, 5, accent, "HARBOR PASS@4 EVIDENCE")
	drawText(img, 49, 92, 2, muted, "AUTOMATIC HEADLESS RUN SUMMARY")
	drawTextRight(img, 1152, 43, 3, text, cleanText(slot, 30))

	statusLabel := "VERIFIED"
	statusColor := passed
	if !status.Verified {
		statusLabel = "INCOMPLETE"
		statusColor = failed
	}
	drawRect(img, 48, 132, 1104, 104, panel)
	drawText(img, 72, 154, 2, muted, "MODEL")
	drawFittedText(img, 72, 188, 700, 4, text, displayValue(result.Model, "UNKNOWN"))
	drawTextRight(img, 1128, 154, 2, muted, "STATUS")
	drawTextRight(img, 1128, 188, 3, statusColor, statusLabel)

	drawMetric(img, 48, 260, 250, "TRIALS", fmt.Sprintf("%d / %d", result.Trials, domain.RequiredTrialCount), accent)
	drawMetric(img, 316, 260, 250, "PASSED", fmt.Sprintf("%d / %d", result.PassCount, domain.RequiredTrialCount), passed)
	drawMetric(img, 584, 260, 250, "PASS@4", fmt.Sprintf("%.2f", result.PassAt4), accent)
	drawMetric(img, 852, 260, 300, "AVERAGE TURNS", fmt.Sprintf("%.1f", result.AverageTurns), accent)

	drawText(img, 48, 386, 3, text, "TRIAL DETAILS")
	for i := 0; i < domain.RequiredTrialCount; i++ {
		x := 48 + i*276
		run, ok := trialByNumber(result.Runs, i+1)
		drawRect(img, x, 426, 258, 130, panelAlt)
		drawText(img, x+18, 446, 2, muted, fmt.Sprintf("TRIAL %d", i+1))
		if !ok {
			drawText(img, x+18, 488, 3, failed, "MISSING")
			drawText(img, x+18, 528, 2, muted, "TURNS --")
			continue
		}
		outcome := "FAIL"
		outcomeColor := failed
		if run.Passed {
			outcome = "PASS"
			outcomeColor = passed
		}
		drawText(img, x+18, 488, 3, outcomeColor, outcome)
		drawText(img, x+18, 528, 2, text, fmt.Sprintf("TURNS %d", run.Turns))
	}

	digest := strings.TrimSpace(result.TaskDigest)
	drawText(img, 48, 574, 2, muted, "TASK DIGEST  "+displayDigest(digest))
	drawText(img, 48, 606, 2, muted, "RAW SHA256   "+displayDigest(firstNonEmpty(status.RawResultSHA256, result.RawResultSHA256)))
	drawText(img, 48, 638, 2, muted, "COMMAND SHA   "+displayDigest(status.CommandEvidenceSHA))
	createdAt := result.CreatedAt.UTC()
	if createdAt.IsZero() {
		createdAt = time.Unix(0, 0).UTC()
	}
	drawText(img, 48, 678, 1, muted, "AGENT "+displayValue(result.Agent, "NOT RECORDED"))
	drawTextRight(img, 1152, 678, 1, muted, "CREATED UTC "+createdAt.Format("2006-01-02 15:04:05"))

	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, fmt.Errorf("encode pass@4 evidence PNG: %w", err)
	}
	return out.Bytes(), nil
}

func drawMetric(img *image.RGBA, x, y, width int, label, value string, valueColor color.Color) {
	drawRect(img, x, y, width, 98, panel)
	drawText(img, x+18, y+18, 2, muted, label)
	drawText(img, x+18, y+54, 3, valueColor, value)
}

func trialByNumber(runs []domain.TrialRun, number int) (domain.TrialRun, bool) {
	for _, run := range runs {
		if run.Trial == number {
			return run, true
		}
	}
	return domain.TrialRun{}, false
}

func displayValue(value, fallback string) string {
	value = cleanText(value, 64)
	if value == "" {
		return fallback
	}
	return value
}

func displayDigest(value string) string {
	value = strings.TrimPrefix(strings.TrimSpace(value), "sha256:")
	return displayValue(value, "NOT RECORDED")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func drawRect(img *image.RGBA, x, y, width, height int, c color.Color) {
	draw.Draw(img, image.Rect(x, y, x+width, y+height), &image.Uniform{C: c}, image.Point{}, draw.Src)
}

func drawTextRight(img *image.RGBA, right, y, scale int, c color.Color, value string) {
	value = cleanText(value, 64)
	drawText(img, right-textWidth(value, scale), y, scale, c, value)
}

func drawFittedText(img *image.RGBA, x, y, maxWidth, preferredScale int, c color.Color, value string) {
	value = cleanText(value, 128)
	scale := preferredScale
	for scale > 1 && textWidth(value, scale) > maxWidth {
		scale--
	}
	if textWidth(value, scale) > maxWidth {
		maxChars := maxWidth / (6 * scale)
		if maxChars <= 3 {
			value = strings.Repeat(".", max(0, maxChars))
		} else {
			value = value[:maxChars-3] + "..."
		}
	}
	drawText(img, x, y, scale, c, value)
}

func textWidth(value string, scale int) int {
	if value == "" {
		return 0
	}
	return len([]rune(value))*6*scale - scale
}

func drawText(img *image.RGBA, x, y, scale int, c color.Color, value string) {
	value = cleanText(value, 256)
	for _, r := range value {
		glyph, ok := glyphs[r]
		if !ok {
			glyph = glyphs['?']
		}
		for row, bits := range glyph {
			for col := 0; col < 5; col++ {
				if bits&(1<<(4-col)) == 0 {
					continue
				}
				drawRect(img, x+col*scale, y+row*scale, scale, scale, c)
			}
		}
		x += 6 * scale
	}
}

func cleanText(value string, limit int) string {
	value = strings.TrimSpace(strings.ToUpper(value))
	var out strings.Builder
	for _, r := range value {
		if out.Len() >= limit {
			break
		}
		if r < 32 || r > 126 {
			r = '?'
		}
		out.WriteRune(r)
	}
	return out.String()
}

func summaryCoherent(result domain.TrialResult) bool {
	if result.Trials != domain.RequiredTrialCount || len(result.Runs) != domain.RequiredTrialCount {
		return false
	}
	seen := make(map[int]bool, domain.RequiredTrialCount)
	passes := 0
	totalTurns := 0
	for _, run := range result.Runs {
		if run.Trial < 1 || run.Trial > domain.RequiredTrialCount || seen[run.Trial] || run.Turns < 0 {
			return false
		}
		if run.Passed && strings.TrimSpace(run.FailureReason) != "" {
			return false
		}
		seen[run.Trial] = true
		totalTurns += run.Turns
		if run.Passed {
			passes++
		}
	}
	return result.PassCount == passes && math.Abs(result.PassAt4-float64(passes)/float64(domain.RequiredTrialCount)) <= 1e-9 &&
		math.Abs(result.AverageTurns-float64(totalTurns)/float64(domain.RequiredTrialCount)) <= 0.01
}

var glyphs = map[rune][7]byte{
	' ': {},
	'A': {14, 17, 17, 31, 17, 17, 17}, 'B': {30, 17, 17, 30, 17, 17, 30},
	'C': {14, 17, 16, 16, 16, 17, 14}, 'D': {30, 17, 17, 17, 17, 17, 30},
	'E': {31, 16, 16, 30, 16, 16, 31}, 'F': {31, 16, 16, 30, 16, 16, 16},
	'G': {14, 17, 16, 23, 17, 17, 15}, 'H': {17, 17, 17, 31, 17, 17, 17},
	'I': {31, 4, 4, 4, 4, 4, 31}, 'J': {7, 2, 2, 2, 18, 18, 12},
	'K': {17, 18, 20, 24, 20, 18, 17}, 'L': {16, 16, 16, 16, 16, 16, 31},
	'M': {17, 27, 21, 21, 17, 17, 17}, 'N': {17, 25, 21, 19, 17, 17, 17},
	'O': {14, 17, 17, 17, 17, 17, 14}, 'P': {30, 17, 17, 30, 16, 16, 16},
	'Q': {14, 17, 17, 17, 21, 18, 13}, 'R': {30, 17, 17, 30, 20, 18, 17},
	'S': {15, 16, 16, 14, 1, 1, 30}, 'T': {31, 4, 4, 4, 4, 4, 4},
	'U': {17, 17, 17, 17, 17, 17, 14}, 'V': {17, 17, 17, 17, 17, 10, 4},
	'W': {17, 17, 17, 21, 21, 21, 10}, 'X': {17, 17, 10, 4, 10, 17, 17},
	'Y': {17, 17, 10, 4, 4, 4, 4}, 'Z': {31, 1, 2, 4, 8, 16, 31},
	'0': {14, 17, 19, 21, 25, 17, 14}, '1': {4, 12, 4, 4, 4, 4, 14},
	'2': {14, 17, 1, 2, 4, 8, 31}, '3': {30, 1, 1, 14, 1, 1, 30},
	'4': {2, 6, 10, 18, 31, 2, 2}, '5': {31, 16, 16, 30, 1, 1, 30},
	'6': {14, 16, 16, 30, 17, 17, 14}, '7': {31, 1, 2, 4, 8, 8, 8},
	'8': {14, 17, 17, 14, 17, 17, 14}, '9': {14, 17, 17, 15, 1, 1, 14},
	'.': {0, 0, 0, 0, 0, 12, 12}, ':': {0, 12, 12, 0, 12, 12, 0},
	'-': {0, 0, 0, 31, 0, 0, 0}, '_': {0, 0, 0, 0, 0, 0, 31},
	'/': {1, 2, 2, 4, 8, 8, 16}, '@': {14, 17, 23, 21, 23, 16, 14},
	'#': {10, 31, 10, 10, 31, 10, 0}, '%': {25, 25, 2, 4, 8, 19, 19},
	'?': {14, 17, 1, 2, 4, 0, 4},
}
