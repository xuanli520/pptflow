package tui

import (
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/purplevoid/harbor-factory/internal/app"
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
	Slug         string
	Name         string
	RepoURL      string
	CommitSHA    string
	State        TaskState
	RunID        string
	CurrentStage string
	RunStatus    string
	Lifecycle    string
	Review       *app.TaskBoardReview
	OpenReviews  int
	Runs         []TaskRunItem
}

// TaskRunItem is the terminal-facing copy of one durable Run record. It is
// populated only from the task-board application projection.
type TaskRunItem struct {
	ID                    string
	ParentRunID           string
	Status                string
	CurrentStage          string
	FailureStage          string
	FailureClass          string
	FailureReason         string
	FailureCode           string
	FailureSummary        string
	FailureJobID          string
	FailureArtifactID     string
	FailureRecordedAt     *time.Time
	FailureRecoveryAction app.TaskBoardFailureRecoveryAction
	CanRedrive            bool
	CreatedAt             time.Time
	StartedAt             *time.Time
	FinishedAt            *time.Time
	LogPath               string
	HasLog                bool
	CanRetry              bool
	RetryReason           string
	RetryStrategy         app.TaskBoardRetryStrategy
}

func truncate(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
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
	if maxLen <= 0 {
		return ""
	}
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	front := (maxLen - 3) / 2
	back := maxLen - 3 - front
	return s[:front] + "..." + s[len(s)-back:]
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
		if item.Review != nil {
			lines = append(lines, statusRunningStyle.Render("等待审核"))
		} else if item.OpenReviews > 1 {
			lines = append(lines, failStyleV2.Render("多个审核待处理"))
		} else if item.RunStatus == "" {
			lines = append(lines, mutedStyle.Render("等待启动"))
		} else {
			lines = append(lines, mutedStyle.Render(item.RunStatus))
		}
		lines = append(lines, mutedStyle.Render(truncateMiddle(item.RepoURL, width-4)))
		lines = append(lines, mutedStyle.Render("sha:"+shortSHA(item.CommitSHA)))

	case TaskRunning:
		stage := item.CurrentStage
		if stage == "" {
			stage = item.RunStatus
		} else {
			stage = displayStageName(stage)
		}
		stageLine := statusRunningStyle.Render("●") + " " + stage
		lines = append(lines, truncate(stageLine, width-4))
		lines = append(lines, mutedStyle.Render(truncateMiddle(item.RepoURL, width-4)))
		lines = append(lines, mutedStyle.Render("sha:"+shortSHA(item.CommitSHA)))

	case TaskCompleted:
		lines = append(lines, mutedStyle.Render(item.RunStatus))
		lines = append(lines, mutedStyle.Render(truncateMiddle(item.RepoURL, width-4)))
		lines = append(lines, mutedStyle.Render("sha:"+shortSHA(item.CommitSHA)))
	}

	// Ensure minimum card height
	for len(lines) < 4 {
		if selected {
			lines = append(lines, highlightStyle.Render(strings.Repeat("─", max(0, width-6))))
		} else {
			lines = append(lines, mutedStyle.Render(strings.Repeat("─", max(0, width-6))))
		}
	}

	return style.Width(width).Render(lipgloss.JoinVertical(lipgloss.Left, lines...))
}
