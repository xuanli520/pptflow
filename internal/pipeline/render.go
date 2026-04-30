package pipeline

import (
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

const (
	terminalScreenshotMaxLines = 80
	terminalScreenshotMaxCols  = 120
)

func renderTerminalLog(text string, basePath string) ([]string, error) {
	lines := terminalScreenshotLines(text)
	if len(lines) == 0 {
		lines = []string{"(no terminal output)"}
	}
	width := terminalScreenshotMaxCols*7 + 16
	height := len(lines)*13 + 16
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	drawer := font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(color.Black),
		Face: basicfont.Face7x13,
	}
	for i, line := range lines {
		drawer.Dot = fixed.P(8, 8+(i+1)*13)
		drawer.DrawString(asciiForBasicFont(line))
	}
	if err := os.MkdirAll(filepath.Dir(basePath), 0o755); err != nil {
		return nil, err
	}
	file, err := os.Create(basePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if err := png.Encode(file, img); err != nil {
		return nil, err
	}
	return []string{basePath}, nil
}

func terminalScreenshotLines(text string) []string {
	text = stripANSI(text)
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	raw := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(raw) > terminalScreenshotMaxLines {
		raw = raw[len(raw)-terminalScreenshotMaxLines:]
	}
	lines := make([]string, 0, len(raw))
	for _, line := range raw {
		runes := []rune(line)
		if len(runes) > terminalScreenshotMaxCols {
			line = string(runes[:terminalScreenshotMaxCols-1]) + ">"
		}
		lines = append(lines, line)
	}
	return lines
}

func stripANSI(text string) string {
	var builder strings.Builder
	for i := 0; i < len(text); i++ {
		if text[i] != 0x1b {
			builder.WriteByte(text[i])
			continue
		}
		i++
		if i >= len(text) || text[i] != '[' {
			continue
		}
		for i < len(text) {
			ch := text[i]
			if ch >= 0x40 && ch <= 0x7e {
				break
			}
			i++
		}
	}
	return builder.String()
}

func asciiForBasicFont(text string) string {
	var builder strings.Builder
	for len(text) > 0 {
		r, size := utf8.DecodeRuneInString(text)
		if r == utf8.RuneError && size == 1 {
			builder.WriteByte('?')
			text = text[1:]
			continue
		}
		if r >= 32 && r <= 126 {
			builder.WriteRune(r)
		} else if r == '\t' {
			builder.WriteString("    ")
		} else {
			builder.WriteByte('?')
		}
		text = text[size:]
	}
	return builder.String()
}
