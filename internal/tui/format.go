package tui

import (
	"github.com/charmbracelet/x/ansi"
)

// ellipsis is the marker appended by every truncation helper. Its display width
// is accounted for in the caller's budget so a truncated string never exceeds
// the width it was given.
const ellipsis = "..."

// displayWidth reports the terminal cell width of a string. It is the single
// authority for width in this package: byte length and rune count both
// misreport CJK and emoji, which previously produced misaligned columns.
func displayWidth(value string) int {
	return ansi.StringWidthWc(value)
}

// truncateDisplay shortens value to at most limit terminal cells, appending an
// ellipsis when content was removed. It is grapheme- and width-aware, so it
// never splits a multi-byte rune and never returns invalid UTF-8.
func truncateDisplay(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if displayWidth(value) <= limit {
		return value
	}
	if limit <= len(ellipsis) {
		return ansi.TruncateWc(value, limit, "")
	}
	return ansi.TruncateWc(value, limit, ellipsis)
}

// truncateMiddleDisplay keeps both ends of value and elides its middle. It is
// used for identifiers and paths where the head and tail carry the meaning.
func truncateMiddleDisplay(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	width := displayWidth(value)
	if width <= limit {
		return value
	}
	if limit <= len(ellipsis) {
		return ansi.TruncateWc(value, limit, "")
	}
	remaining := limit - len(ellipsis)
	front := remaining / 2
	back := remaining - front
	head := ansi.TruncateWc(value, front, "")
	tail := ansi.TruncateLeftWc(value, width-back, "")
	return head + ellipsis + tail
}

// wrapDisplay reflows value to the given cell width, stripping any escape
// sequences first so styling in the source can never corrupt the layout.
func wrapDisplay(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	return ansi.WrapWc(ansi.Strip(value), limit, "")
}
