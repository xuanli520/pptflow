package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

const taskCardMinHeight = 5

// TaskBoardModel holds the three-column task board state.
type TaskBoardModel struct {
	pending   []TaskItem
	running   []TaskItem
	completed []TaskItem
	cursor    int // 0=pending, 1=running, 2=completed
	selected  int // selected index within current column
	scroll    [3]int
}

func NewTaskBoardModel() TaskBoardModel {
	return TaskBoardModel{}
}

func (m *TaskBoardModel) SetTasks(pending, running, completed []TaskItem) {
	m.pending = pending
	m.running = running
	m.completed = completed
	// Clamp cursor and selection
	if m.selected >= m.currentColLen() {
		m.selected = max(0, m.currentColLen()-1)
	}
}

func (m *TaskBoardModel) currentCol() []TaskItem {
	switch m.cursor {
	case 1:
		return m.running
	case 2:
		return m.completed
	default:
		return m.pending
	}
}

func (m *TaskBoardModel) currentColLen() int {
	return len(m.currentCol())
}

func (m *TaskBoardModel) MoveUp() {
	if m.selected > 0 {
		m.selected--
	}
}

func (m *TaskBoardModel) MoveDown() {
	if m.selected < m.currentColLen()-1 {
		m.selected++
	}
}

func (m *TaskBoardModel) MoveLeft() {
	if m.cursor > 0 {
		m.cursor--
		m.selected = 0
	}
}

func (m *TaskBoardModel) MoveRight() {
	if m.cursor < 2 {
		m.cursor++
		m.selected = 0
	}
}

func (m *TaskBoardModel) SelectedTask() *TaskItem {
	col := m.currentCol()
	if m.selected >= 0 && m.selected < len(col) {
		return &col[m.selected]
	}
	return nil
}

// View renders the three-column task board. Single column if width < 80.
func (m *TaskBoardModel) View(width, height int) string {
	if width < 80 {
		return m.viewSingle(width, height)
	}
	return m.viewColumns(width, height)
}

func (m *TaskBoardModel) viewColumns(width, height int) string {
	colWidth := max(24, (width-4)/3)
	bodyHeight := max(1, height-1)

	pendingTitle := formatColTitle("待处理", len(m.pending), m.cursor == 0)
	runningTitle := formatColTitle("运行中", len(m.running), m.cursor == 1)
	completedTitle := formatColTitle("已完成", len(m.completed), m.cursor == 2)

	pendingCol := renderColumn(m.pending, colWidth, bodyHeight, m.cursor == 0, m.selected, m.scroll[0])
	runningCol := renderColumn(m.running, colWidth, bodyHeight, m.cursor == 1, m.selected, m.scroll[1])
	completedCol := renderColumn(m.completed, colWidth, bodyHeight, m.cursor == 2, m.selected, m.scroll[2])

	separator := mutedStyle.Render("│")

	return lipgloss.JoinVertical(lipgloss.Top,
		lipgloss.JoinHorizontal(lipgloss.Top,
			pendingTitle,
			separator,
			runningTitle,
			separator,
			completedTitle,
		),
		lipgloss.JoinHorizontal(lipgloss.Top,
			lipgloss.NewStyle().Width(colWidth).Render(pendingCol),
			separator,
			lipgloss.NewStyle().Width(colWidth).Render(runningCol),
			separator,
			lipgloss.NewStyle().Width(colWidth).Render(completedCol),
		),
	)
}

func (m *TaskBoardModel) viewSingle(width, height int) string {
	titles := []string{
		formatColTitle("待处理", len(m.pending), m.cursor == 0),
		formatColTitle("运行中", len(m.running), m.cursor == 1),
		formatColTitle("已完成", len(m.completed), m.cursor == 2),
	}
	bodyHeight := max(1, height-2)
	var col []TaskItem
	var sel int
	switch m.cursor {
	case 1:
		col = m.running
		sel = m.selected
	case 2:
		col = m.completed
		sel = m.selected
	default:
		col = m.pending
		sel = m.selected
	}
	return lipgloss.JoinVertical(lipgloss.Top,
		lipgloss.JoinHorizontal(lipgloss.Top, titles...),
		renderColumn(col, width, bodyHeight, true, sel, m.scroll[m.cursor]),
	)
}

func formatColTitle(title string, count int, active bool) string {
	text := "── " + title + " (" + strings.ReplaceAll(strings.ReplaceAll(
		strings.ReplaceAll(strings.ReplaceAll(
			string(rune(count+'0')), "0", "0"), "1", "1"), "2", "2"), "3", "3") + ") ──"
	if active {
		return columnTitleStyle.Render(text)
	}
	return columnTitleMutedStyle.Render(text)
}

func renderColumn(items []TaskItem, width, bodyHeight int, active bool, selected int, scroll int) string {
	if len(items) == 0 {
		return mutedStyle.Render(strings.Repeat("\n", bodyHeight) + "  暂无题目")
	}

	// Calculate visible range
	cardsPerScreen := bodyHeight / taskCardMinHeight
	if cardsPerScreen < 1 {
		cardsPerScreen = 1
	}

	start := scroll
	if selected < start {
		start = selected
	}
	if selected >= start+cardsPerScreen {
		start = selected - cardsPerScreen + 1
	}
	if start > len(items)-cardsPerScreen {
		start = max(0, len(items)-cardsPerScreen)
	}

	var lines []string
	for i := start; i < len(items) && len(lines) < bodyHeight; i++ {
		card := renderTaskCard(items[i], width-2, active && i == selected)
		lines = append(lines, card)
	}

	// Pad to body height
	for len(lines) < bodyHeight {
		lines = append(lines, "")
	}

	return strings.Join(lines, "\n")
}
