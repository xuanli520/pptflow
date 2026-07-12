package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
)

type layoutMode int

const (
	layoutMinimal layoutMode = iota
	layoutStacked
	layoutMedium
	layoutWide
)

type appLayout struct {
	Mode                        layoutMode
	ContentWidth, ContentHeight int
	SidebarWidth, MainWidth     int
}

func layoutFor(width, height int) appLayout {
	contentW := maxInt(24, width-2)
	contentH := maxInt(6, height-7)
	l := appLayout{ContentWidth: contentW, ContentHeight: contentH, MainWidth: contentW}
	switch {
	case width >= 120:
		l.Mode = layoutWide
		l.SidebarWidth = minInt(36, contentW/3)
		l.MainWidth = contentW - l.SidebarWidth - 1
	case width >= 90:
		l.Mode = layoutMedium
		l.SidebarWidth = minInt(28, contentW/3)
		l.MainWidth = contentW - l.SidebarWidth - 1
	case width >= 72:
		l.Mode = layoutStacked
	default:
		l.Mode = layoutMinimal
	}
	return l
}

func padRightDisplay(s string, width int) string {
	w := runewidth.StringWidth(s)
	if w >= width {
		return s
	}
	return s + strings.Repeat(" ", width-w)
}

func truncateDisplay(s string, width int) string {
	if width <= 0 {
		return ""
	}
	return ansi.Truncate(s, width, "…")
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
func clampInt(v, low, high int) int {
	if v < low {
		return low
	}
	if v > high {
		return high
	}
	return v
}
