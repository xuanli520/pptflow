package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Field rendering is shared by every screen. A field is one label/value row
// whose value is truncated to the remaining column budget, so a long digest or
// repository URL can never widen a pane past its window.

// renderDetailSection frames one titled group of fields.
func renderDetailSection(title, content string, outerWidth int) string {
	innerWidth := max(1, outerWidth-4)
	return detailSectionStyle.Width(innerWidth).Render(lipgloss.JoinVertical(lipgloss.Left,
		detailSectionTitleStyle.Render(title),
		content,
	))
}

// detailFields joins pre-rendered rows. It takes no width: each row was already
// budgeted by detailField, and an unused width parameter previously invited
// callers to believe this function enforced one.
func detailFields(fields ...string) string {
	return strings.Join(fields, "\n")
}

// detailField renders one label/value row within the given total width.
func detailField(label, value string, width int) string {
	available := max(1, width-detailLabelColumns-detailFieldGutter)
	return detailLabelStyle.Width(detailLabelColumns).Render(label) +
		detailValueStyle.Render(truncateMiddleDisplay(value, available))
}
