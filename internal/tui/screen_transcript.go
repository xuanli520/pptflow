package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/purplevoid/harbor-factory/internal/app"
)

// agentTranscriptModel presents retained model output without turning it into a
// workflow input. It is read-only and operates solely on the task-board
// projection the TUI already loaded; it never reads an artifact itself.
type agentTranscriptModel struct {
	taskName    string
	transcripts []app.TaskBoardAgentTranscript
	selected    int
	pane        *scrollPane
}

func newAgentTranscriptModel(task *TaskItem) *agentTranscriptModel {
	name := "Agent 回合"
	transcripts := make([]app.TaskBoardAgentTranscript, 0)
	if task != nil {
		if task.Name != "" {
			name = task.Name
		}
		for _, run := range task.Runs {
			if run.ID == task.RunID {
				transcripts = append(transcripts, run.AgentTurnTranscripts...)
				break
			}
		}
		if len(transcripts) == 0 && len(task.Runs) > 0 {
			transcripts = append(transcripts, task.Runs[0].AgentTurnTranscripts...)
		}
	}
	return &agentTranscriptModel{taskName: name, transcripts: transcripts, pane: newScrollPane()}
}

func (m *agentTranscriptModel) current() *app.TaskBoardAgentTranscript {
	if m == nil || m.selected < 0 || m.selected >= len(m.transcripts) {
		return nil
	}
	return &m.transcripts[m.selected]
}

// MovePrevious and MoveNext change which turn is shown. Scroll position resets
// because an offset carried from a longer response would start the next turn
// somewhere in its middle.
func (m *agentTranscriptModel) MovePrevious() {
	if m != nil && m.selected > 0 {
		m.selected--
		m.pane.GoToStart()
	}
}

func (m *agentTranscriptModel) MoveNext() {
	if m != nil && m.selected+1 < len(m.transcripts) {
		m.selected++
		m.pane.GoToStart()
	}
}

func (m *agentTranscriptModel) MoveUp() {
	if m != nil {
		m.pane.MoveUp()
	}
}

func (m *agentTranscriptModel) MoveDown() {
	if m != nil {
		m.pane.MoveDown()
	}
}

func (m *agentTranscriptModel) PageUp() {
	if m != nil {
		m.pane.PageUp()
	}
}

func (m *agentTranscriptModel) PageDown() {
	if m != nil {
		m.pane.PageDown()
	}
}

// bodyContent renders the selected turn's metadata followed by its response.
func (m *agentTranscriptModel) bodyContent(width int) string {
	transcript := m.current()
	if transcript == nil {
		return mutedStyle.Render("未保留 Agent 回合")
	}
	fields := []string{
		detailField("阶段", displayStageName(transcript.StageKey), width),
		detailField("回合", fmt.Sprintf("%d", transcript.Turn), width),
		detailField("模型", transcript.ModelID, width),
		detailField("提交", transcript.SubmissionStatus, width),
		detailField("响应", fmt.Sprintf("%d bytes · %s", transcript.ResponseBytes, transcript.ResponseSHA256), width),
		detailField("创建时间", formatDetailTime(&transcript.CreatedAt), width),
		detailField("到期时间", formatDetailTime(&transcript.ExpiresAt), width),
	}
	if transcript.ProtocolRejectionCode != "" {
		fields = append(fields, detailField("协议拒绝", transcript.ProtocolRejectionCode, width))
	}
	if transcript.FailureCode != "" {
		fields = append(fields, detailField("失败码", transcript.FailureCode, width))
	}
	fields = append(fields, detailField("工具提交", fmt.Sprintf("%d", transcript.SubmissionCount), width))

	body := ""
	switch {
	case transcript.ExpiredAt != nil:
		body = mutedStyle.Render("原文已按保留规则清除")
	case strings.TrimSpace(transcript.ResponseText) == "":
		body = mutedStyle.Render("此回合没有返回文本")
	default:
		body = wrapDisplay(transcript.ResponseText, width)
	}
	return strings.Join([]string{
		detailFields(fields...),
		"",
		detailSectionTitleStyle.Render("模型响应"),
		body,
	}, "\n")
}

func (m *agentTranscriptModel) View(width, bodyRows int) string {
	contentWidth := max(24, width)
	position := fmt.Sprintf("%d / %d", m.selected+1, len(m.transcripts))
	if len(m.transcripts) == 0 {
		position = "0 / 0"
	}
	header := lipgloss.JoinVertical(lipgloss.Left,
		detailBreadcrumbStyle.Render("题目管理 / 任务详情 / Agent 回合"),
		detailTitleStyle.Width(max(1, contentWidth-2)).Render(
			truncateDisplay(m.taskName+" · Agent 回合 "+position, max(1, contentWidth-4)),
		),
	)
	paneRows := max(1, bodyRows-lipgloss.Height(header)-framedPaneRows)
	lineWidth := max(1, contentWidth-4)

	m.pane.Resize(lineWidth, paneRows)
	m.pane.SetContent(m.bodyContent(lineWidth), lineWidth)
	body := inputStyle.Width(lineWidth).Height(paneRows).Render(m.pane.View())
	return lipgloss.JoinVertical(lipgloss.Left, header, body)
}
