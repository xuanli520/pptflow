package tui

import (
	"context"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/purplevoid/harbor-factory/internal/app"
)

// RunNewTUIAdapter matches the lifecycleTUIRunner signature used by cmd/tui_v2.go.
func RunNewTUIAdapter(ctx context.Context, services *app.LifecycleServices) error {
	return RunNewTUI(ctx, services)
}

// RunNewTUI launches the replacement task-board TUI. Coexists with the legacy
// TaskHub TUI until the old files are deleted in the Phase 4 cleanup step.
func RunNewTUI(ctx context.Context, services *app.LifecycleServices) error {
	model := newAppModel(ctx, services)
	program := tea.NewProgram(model, tea.WithAltScreen())
	_, err := program.Run()
	return err
}

// appModel is the top-level model for the task-board TUI.
type appModel struct {
	ctx      context.Context
	services *app.LifecycleServices

	board  TaskBoardModel
	input  TaskInputModel
	detail *detailModel

	width  int
	height int
	err    error
}

func newAppModel(ctx context.Context, services *app.LifecycleServices) appModel {
	return appModel{
		ctx:      ctx,
		services: services,
		board:    NewTaskBoardModel(),
		input:    NewTaskInputModel(),
	}
}

func (m appModel) Init() tea.Cmd {
	return tea.Batch(
		m.pollTick(),
		textinput.Blink,
	)
}

func (m appModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Let the input process non-key messages (for cursor blink etc.)
	var inputCmd tea.Cmd
	if _, ok := msg.(tea.KeyMsg); !ok {
		inputCmd, _ = m.input.Update(msg)
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, inputCmd

	case tea.KeyMsg:
		return m.handleKey(msg, inputCmd)

	case TaskSubmitMsg:
		// Submit new task via lifecycle services
		// TODO: call m.services.AuthoringLaunches.Start(ctx, ...)
		return m, inputCmd

	case tickMsg:
		m.pollTasks()
		return m, tea.Batch(inputCmd, m.pollTick())

	case error:
		m.err = msg
		return m, inputCmd
	}

	return m, inputCmd
}

func (m appModel) handleKey(msg tea.KeyMsg, inputCmd tea.Cmd) (tea.Model, tea.Cmd) {
	key := msg.String()

	// If input is focused, let it handle the key
	if m.input.Focused() {
		if cmd, handled := m.input.Update(msg); handled {
			return m, cmd
		}
	}

	// Global keys (work even when input focused)
	switch key {
	case "esc":
		if m.detail != nil {
			m.detail = nil
			return m, nil
		}
		if m.input.Focused() {
			m.input.Hide()
			return m, nil
		}
		return m, inputCmd

	case "q", "ctrl+c":
		if !m.input.Focused() {
			return m, tea.Quit
		}
		return m, inputCmd

	case "n":
		if !m.input.Focused() && m.detail == nil {
			m.input.Show()
			return m, textinput.Blink
		}
		return m, inputCmd

	case "tab":
		m.board.MoveRight()
		return m, inputCmd

	case "shift+tab":
		m.board.MoveLeft()
		return m, inputCmd

	case "up", "k":
		m.board.MoveUp()
		return m, inputCmd

	case "down", "j":
		m.board.MoveDown()
		return m, inputCmd

	case "left", "h":
		m.board.MoveLeft()
		return m, inputCmd

	case "right", "l":
		m.board.MoveRight()
		return m, inputCmd

	case "d":
		if t := m.board.SelectedTask(); t != nil {
			m.detail = newDetailModel(t)
		}
		return m, inputCmd

	case "a":
		if m.detail != nil {
			// approve gate — TODO: call services.Mutations.DecideGate()
			m.detail = nil
		}
		return m, inputCmd

	case "r":
		if m.detail != nil {
			// request changes — TODO
			m.detail = nil
		}
		return m, inputCmd
	}

	return m, inputCmd
}

func (m *appModel) pollTasks() {
	// TODO: query store for tasks by state and update board
	// For now this is a placeholder — the real implementation
	// will query m.services.Store() for task lists.
}

func (m appModel) View() string {
	if m.width == 0 {
		return "loading..."
	}

	if m.detail != nil {
		return appStyle.Render(lipgloss.JoinVertical(lipgloss.Top,
			headerStyle.Width(m.width).Render("Harbor Task Factory"),
			m.detail.View(m.width, m.height-4),
			footerStyle.Render("[a] 通过  [r] 返修  [q] 返回"),
		))
	}

	return appStyle.Render(lipgloss.JoinVertical(lipgloss.Top,
		headerStyle.Width(m.width).Render("Harbor Task Factory"),
		m.board.View(m.width, m.height-5),
		m.input.View(m.width),
		footerStyle.Render("[n] 新任务  [tab] 切换面板  [hjkl/↑↓←→] 导航  [d] 详情  [q] 退出"),
	))
}

type tickMsg struct{}

func (m appModel) pollTick() tea.Cmd {
	return tea.Tick(2*time.Second, func(t time.Time) tea.Msg {
		return tickMsg{}
	})
}

// detailModel renders task detail content for gate review.
type detailModel struct {
	task      *TaskItem
	activeTab int
}

var detailTabs = []string{"instruction.md", "task.toml", "Dockerfile", "solve.sh", "test.sh", "tests_analysis.md"}

func newDetailModel(task *TaskItem) *detailModel {
	return &detailModel{task: task}
}

func (d *detailModel) View(width, height int) string {
	tabs := make([]string, len(detailTabs))
	for i, name := range detailTabs {
		if i == d.activeTab {
			tabs[i] = highlightStyle.Render("[" + name + "]")
		} else {
			tabs[i] = mutedStyle.Render(" " + name + " ")
		}
	}
	tabBar := lipgloss.JoinHorizontal(lipgloss.Top, tabs...)

	body := mutedStyle.Render("(" + d.task.Name + " — 选择文件查看内容)")

	return lipgloss.JoinVertical(lipgloss.Top,
		detailTitleStyle.Width(width).Render(d.task.Name),
		mutedStyle.Render(d.task.RepoURL),
		tabBar,
		lipgloss.NewStyle().Width(width-2).Height(max(1, height-5)).Render(body),
	)
}
