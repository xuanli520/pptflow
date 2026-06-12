package tui

import (
	"context"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/scheduler"
)

type TaskBoardModel struct {
	query   TaskQueryService
	cols    [3]taskListModel
	focused taskColumnID
	loading bool
	err     error
	now     time.Time
}

type taskBoardLoadMsg struct {
	inspecting []TaskProject
	waiting    []TaskProject
	completed  []TaskProject
	err        error
}

var _ Page = (*TaskBoardModel)(nil)

var handledKeyCmd tea.Cmd = func() tea.Msg { return nil }

func newTaskBoardModel(query TaskQueryService) TaskBoardModel {
	return TaskBoardModel{
		query: query,
		cols: [3]taskListModel{
			{state: model.TaskInspecting, title: "开始质检"},
			{state: model.TaskWaitingManual, title: "待处理"},
			{state: model.TaskCompleted, title: "结束质检"},
		},
		now: time.Now(),
	}
}

func (m TaskBoardModel) Init() tea.Cmd {
	return m.Reload()
}

func (m TaskBoardModel) Reload() tea.Cmd {
	if m.query == nil {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		inspecting, err := m.query.ListByState(ctx, model.TaskInspecting)
		if err != nil {
			return taskBoardLoadMsg{err: err}
		}
		waiting, err := m.query.ListByState(ctx, model.TaskWaitingManual)
		if err != nil {
			return taskBoardLoadMsg{err: err}
		}
		completed, err := m.query.ListByState(ctx, model.TaskCompleted)
		if err != nil {
			return taskBoardLoadMsg{err: err}
		}
		return taskBoardLoadMsg{inspecting: inspecting, waiting: waiting, completed: completed}
	}
}

func (m *TaskBoardModel) Update(msg tea.Msg) (bool, tea.Cmd) {
	next, cmd, handled := m.apply(msg)
	*m = next
	return handled, cmd
}

func (m TaskBoardModel) Apply(msg tea.Msg) (TaskBoardModel, tea.Cmd) {
	next, cmd, _ := m.apply(msg)
	return next, cmd
}

func (m TaskBoardModel) apply(msg tea.Msg) (TaskBoardModel, tea.Cmd, bool) {
	switch value := msg.(type) {
	case taskBoardLoadMsg:
		m.loading = false
		m.err = value.err
		if value.err == nil {
			m.cols[taskColumnInspecting].setItems(value.inspecting)
			m.cols[taskColumnWaiting].setItems(value.waiting)
			m.cols[taskColumnCompleted].setItems(value.completed)
		}
		return m, nil, true
	case tickMsg:
		m.now = time.Time(value)
		return m, nil, true
	default:
		return m, nil, false
	}
}

func (m *TaskBoardModel) Focus() {}

func (m *TaskBoardModel) Blur() {}

func (m *TaskBoardModel) HandleKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "left":
		m.focused = taskColumnID(clamp(int(m.focused)-1, 0, 2))
	case "right":
		m.focused = taskColumnID(clamp(int(m.focused)+1, 0, 2))
	case "up":
		m.cols[m.focused].move(-1)
	case "down":
		m.cols[m.focused].move(1)
	default:
		return nil
	}
	return handledKeyCmd
}

func (m *TaskBoardModel) Destroy() tea.Cmd {
	return nil
}

func (m TaskBoardModel) SelectedTask() (TaskProject, bool) {
	return m.cols[m.focused].selected()
}

func (m TaskBoardModel) StateCount(state string) int {
	for _, col := range m.cols {
		if col.state == state {
			return len(col.items)
		}
	}
	return 0
}

func (m TaskBoardModel) WithJobs(jobs []scheduler.JobSnapshot) TaskBoardModel {
	if len(jobs) == 0 {
		return m
	}
	byTask := map[string]scheduler.JobSnapshot{}
	for _, job := range jobs {
		if job.TaskID == "" {
			continue
		}
		if job.State != scheduler.JobQueued && job.State != scheduler.JobRunning && job.State != scheduler.JobFailed {
			continue
		}
		byTask[job.TaskID] = job
	}
	if len(byTask) == 0 {
		return m
	}
	for colIndex := range m.cols {
		items := append([]TaskProject(nil), m.cols[colIndex].items...)
		for index := range items {
			job, ok := byTask[items[index].ID]
			if !ok {
				continue
			}
			applyJobToTaskProject(&items[index], job)
		}
		m.cols[colIndex].items = items
	}
	return m
}

func (m TaskBoardModel) View(width, height int) string {
	if m.err != nil {
		return errorStyle.Render(m.err.Error())
	}
	if m.query == nil {
		return mutedStyle.Render("未配置题目服务")
	}
	if width <= 110 {
		return m.viewSingle(width, height)
	}
	return m.viewColumns(width, height)
}

func applyJobToTaskProject(task *TaskProject, job scheduler.JobSnapshot) {
	if task == nil {
		return
	}
	if job.SyncProgress.Phase != "" {
		task.SyncPhase = job.SyncProgress.Phase
		task.SyncPercent = job.SyncProgress.Percent
		if job.SyncProgress.Phase == "failed" && task.SyncError == "" {
			task.SyncError = job.SyncProgress.Message
		}
	}
	if job.State == scheduler.JobQueued || job.State == scheduler.JobRunning {
		task.RunStatus = model.RunRunning
	}
	if job.CurrentStage != "" && job.CurrentStage != "Git" {
		task.CurrentStage = job.CurrentStage
		task.CurrentStatus = model.StageRunning
	}
	for _, stage := range job.Stages {
		if stage.Status == model.StageRunning {
			task.CurrentStage = stage.Stage
			task.CurrentStatus = stage.Status
			return
		}
	}
	stage, summary, status := primaryFailedStage(job.Stages)
	if stage != "" {
		task.FailedStage = stage
		task.FailedSummary = summary
		task.CurrentStage = stage
		task.CurrentStatus = status
	}
}

func (m TaskBoardModel) viewColumns(width, height int) string {
	colWidth := max(24, (width-2)/3)
	if colWidth*3+2 > width {
		colWidth = max(1, (width-2)/3)
	}
	available := max(4, height-2)
	views := make([]string, 0, 3)
	for index := range m.cols {
		views = append(views, renderTaskColumn(m.cols[index], taskColumnID(index) == m.focused, colWidth, available, m.now))
	}
	separator := mutedStyle.Render("│")
	return lipgloss.JoinHorizontal(lipgloss.Top, views[0], separator, views[1], separator, views[2])
}

func (m TaskBoardModel) viewSingle(width, height int) string {
	var tabs []string
	for index, col := range m.cols {
		label := col.title
		if taskColumnID(index) == m.focused {
			label = activeStyle.Render("[" + label + "]")
		} else {
			label = mutedStyle.Render("[" + label + "]")
		}
		tabs = append(tabs, label)
	}
	col := m.cols[m.focused]
	return strings.Join(tabs, " ") + "\n" + renderTaskColumn(col, true, width, max(4, height-3), m.now)
}

func (m *TaskBoardModel) prepareLayout(width, height int) {
	if m == nil {
		return
	}
	if width <= 110 {
		m.cols[m.focused].setVisibleSize(taskColumnBodyHeight(width, max(4, height-3)) / taskCardMinLineCount)
		return
	}
	for index := range m.cols {
		m.cols[index].setVisibleSize(taskColumnBodyHeight(width, height) / taskCardMinLineCount)
	}
}

func taskColumnBodyHeight(width, height int) int {
	contentHeight := max(1, height-panelStyle.GetVerticalFrameSize())
	return max(0, contentHeight-1)
}

func renderTaskColumn(col taskListModel, focused bool, width, height int, now time.Time) string {
	contentWidth := max(1, width-panelStyle.GetHorizontalFrameSize())
	title := truncateDisplay("─── "+col.title+" ("+itoa(len(col.items))+") ───", max(8, contentWidth))
	if focused {
		title = activeStyle.Render(title)
	} else {
		title = mutedStyle.Render(title)
	}
	bodyHeight := taskColumnBodyHeight(width, height)
	lines := []string{title}
	if bodyHeight == 0 {
		return renderPanel(width, height, strings.Join(lines, "\n"))
	}
	if len(col.items) == 0 {
		lines = append(lines, mutedStyle.Render("暂无题目"))
	} else {
		lines = append(lines, visibleTaskCardLines(&col, focused, max(12, contentWidth), bodyHeight, now)...)
	}
	return renderPanel(width, height, strings.Join(lines, "\n"))
}

func visibleTaskCardLines(col *taskListModel, focused bool, width, budget int, now time.Time) []string {
	if col == nil || len(col.items) == 0 || budget <= 0 {
		return nil
	}
	laneCount := taskColumnLaneCount(width)
	col.setVisibleSize((budget / taskCardMinLineCount) * laneCount)
	start := clamp(col.scroll, 0, len(col.items)-1)
	if col.cursor < start {
		start = col.cursor
	}
	lines, count, includesCursor := taskCardWindowLines(col.items, start, col.cursor, focused, width, budget, now)
	if laneCount == 2 {
		lines, count, includesCursor = taskCardGridLines(col.items, start, col.cursor, focused, width, budget, now)
	}
	if !includesCursor {
		start = col.cursor
		lines, count, _ = taskCardWindowLines(col.items, start, col.cursor, focused, width, budget, now)
		if laneCount == 2 {
			lines, count, _ = taskCardGridLines(col.items, start, col.cursor, focused, width, budget, now)
		}
	}
	col.scroll = start
	col.lastSize = max(1, count)
	return lines
}

func taskColumnLaneCount(width int) int {
	if width >= 36 {
		return 2
	}
	return 1
}

func taskCardGridLines(items []TaskProject, start, cursor int, focused bool, width, budget int, now time.Time) ([]string, int, bool) {
	gap := " "
	laneWidth := max(12, (width-lipgloss.Width(gap))/2)
	left, leftCount, leftCursor := taskCardWindowLines(items, start, cursor, focused, laneWidth, budget, now)
	rightStart := start + leftCount
	right, rightCount, rightCursor := taskCardWindowLines(items, rightStart, cursor, focused, laneWidth, budget, now)
	return joinTaskCardLanes(left, right, laneWidth, gap, budget), leftCount + rightCount, leftCursor || rightCursor
}

func joinTaskCardLanes(left, right []string, laneWidth int, gap string, budget int) []string {
	height := max(len(left), len(right))
	height = min(height, budget)
	lines := make([]string, 0, height)
	for index := 0; index < height; index++ {
		leftLine := ""
		if index < len(left) {
			leftLine = left[index]
		}
		rightLine := ""
		if index < len(right) {
			rightLine = right[index]
		}
		lines = append(lines, fitTaskLaneLine(leftLine, laneWidth)+gap+fitTaskLaneLine(rightLine, laneWidth))
	}
	return lines
}

func fitTaskLaneLine(line string, width int) string {
	if lipgloss.Width(line) > width {
		line = truncateDisplay(line, width)
	}
	if pad := width - lipgloss.Width(line); pad > 0 {
		line += strings.Repeat(" ", pad)
	}
	return line
}

func taskCardWindowLines(items []TaskProject, start, cursor int, focused bool, width, budget int, now time.Time) ([]string, int, bool) {
	var lines []string
	count := 0
	includesCursor := false
	for index := start; index < len(items); index++ {
		isSelected := focused && index == cursor
		card := renderTaskCard(items[index], width, now, isSelected)
		cardLines := strings.Split(card, "\n")
		needed := len(cardLines)
		if isSelected {
			needed += 3
		}
		remaining := budget - len(lines)
		if remaining <= 0 {
			break
		}
		if needed > remaining {
			if count == 0 {
				remainingLines := remaining
				if isSelected && remainingLines >= 2 {
					lines = append(lines, "")
					lines = append(lines, renderSelectedIndicator(width))
					remainingLines -= 2
				}
				if remainingLines > 0 {
					lines = append(lines, cardLines[:remainingLines]...)
				}
				count++
				if index == cursor {
					includesCursor = true
				}
			}
			break
		}
		if isSelected {
			lines = append(lines, "")
			lines = append(lines, renderSelectedIndicator(width))
		}
		lines = append(lines, cardLines...)
		if isSelected {
			lines = append(lines, "")
		}
		count++
		if index == cursor {
			includesCursor = true
		}
	}
	return lines, count, includesCursor
}
