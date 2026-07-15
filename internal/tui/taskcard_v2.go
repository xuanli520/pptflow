package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// TaskState represents which column a task belongs to.
type TaskState string

const (
	TaskPending   TaskState = "pending"
	TaskRunning   TaskState = "running"
	TaskCompleted TaskState = "completed"
)

// TaskItem holds the data for one task card in the board.
type TaskItem struct {
	ID           string
	Name         string
	RepoURL      string
	CommitSHA   string
	State        TaskState
	CurrentStage string
	CodexLine    string  // last line of Codex streaming output
	Elapsed      time.Duration
	QwenPass     int
	QwenTotal    int
	OpusPass     int
	OpusTotal    int
	AvgTurns     float64
	PackagePath  string
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-1] + "..."
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func truncateMiddle(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	half := (maxLen - 3) / 2
	return s[:half] + "..." + s[len(s)-half:]
}

// renderTaskCard renders a single task card. Width is the available column width.
func renderTaskCard(item TaskItem, width int, selected bool) string {
	style := cardStyle
	if selected {
		style = cardSelectedStyle
	}

	var lines []string

	// Title line
	title := cardTitleStyle.Render(truncateMiddle(item.Name, width-4))
	lines = append(lines, title)

	switch item.State {
	case TaskPending:
		lines = append(lines, mutedStyle.Render(truncateMiddle(item.RepoURL, width-4)))
		lines = append(lines, mutedStyle.Render("sha:"+shortSHA(item.CommitSHA)))
		if item.Elapsed > 0 {
			lines = append(lines, mutedStyle.Render(fmtDuration(item.Elapsed)))
		}

	case TaskRunning:
		// Current stage with running indicator
		stageLine := statusRunningStyle.Render("●") + " " + item.CurrentStage
		lines = append(lines, truncate(stageLine, width-4))
		// Codex streaming last line
		if item.CodexLine != "" {
			lines = append(lines, labelStyle.Render(truncate(item.CodexLine, width-4)))
		}
		if item.Elapsed > 0 {
			lines = append(lines, mutedStyle.Render(fmtDuration(item.Elapsed)))
		}

	case TaskCompleted:
		qwenLine := fmtQwenResult(item)
		opusLine := fmtOpusResult(item)
		lines = append(lines, truncate(qwenLine, width-4))
		lines = append(lines, truncate(opusLine, width-4))
		if item.AvgTurns > 0 {
			lines = append(lines, mutedStyle.Render(fmt.Sprintf("avg turns: %.1f", item.AvgTurns)))
		}
		if item.PackagePath != "" {
			lines = append(lines, mutedStyle.Render(truncateMiddle(item.PackagePath, width-4)))
		}
	}

	// Ensure minimum card height
	for len(lines) < 4 {
		if selected {
			lines = append(lines, highlightStyle.Render(strings.Repeat("─", width-6)))
		} else {
			lines = append(lines, mutedStyle.Render(strings.Repeat("─", width-6)))
		}
	}

	return style.Width(width).Render(lipgloss.JoinVertical(lipgloss.Left, lines...))
}

func fmtDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}

func fmtQwenResult(item TaskItem) string {
	result := passStyleV2.Render("✓") + " "
	if item.QwenPass > 0 {
		result += passStyleV2.Render(fmt.Sprintf("Qwen %d/%d", item.QwenPass, item.QwenTotal))
	} else {
		result += failStyleV2.Render(fmt.Sprintf("Qwen %d/%d", item.QwenPass, item.QwenTotal))
	}
	return result
}

func fmtOpusResult(item TaskItem) string {
	result := "  Opus "
	if item.OpusPass >= 3 {
		result += passStyleV2.Render(fmt.Sprintf("%d/%d", item.OpusPass, item.OpusTotal))
	} else {
		result += fmt.Sprintf("%d/%d", item.OpusPass, item.OpusTotal)
	}
	return result
}
