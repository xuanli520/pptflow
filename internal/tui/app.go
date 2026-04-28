package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/db"
	"github.com/xuanli520/p2r_tui/internal/pipeline"
)

type app struct {
	store         *db.Store
	cfg           config.Config
	projects      []db.ProjectSummary
	table         table.Model
	search        textinput.Model
	logs          viewport.Model
	tab           int
	selectedStage int
	width         int
	height        int
	confirm       bool
	message       string
	running       bool
}

type projectsMsg []db.ProjectSummary

type runMsg struct {
	result pipeline.Result
	err    error
}

var (
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	activeStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("42"))
	errorStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	mutedStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	panelStyle  = lipgloss.NewStyle().Padding(0, 1)
)

func Start(store *db.Store, cfg config.Config) error {
	program := tea.NewProgram(newApp(store, cfg), tea.WithAltScreen())
	_, err := program.Run()
	return err
}

func newApp(store *db.Store, cfg config.Config) app {
	search := textinput.New()
	search.Placeholder = "filter task id, batch, or status"
	search.Prompt = "Search: "
	search.Focus()
	columns := []table.Column{
		{Title: "task_id", Width: 20},
		{Title: "batch", Width: 14},
		{Title: "last_run", Width: 16},
		{Title: "run_status", Width: 18},
		{Title: "bad", Width: 4},
		{Title: "blk", Width: 4},
		{Title: "hi", Width: 4},
		{Title: "manual", Width: 8},
	}
	t := table.New(table.WithColumns(columns), table.WithFocused(true), table.WithHeight(12))
	styles := table.DefaultStyles()
	styles.Selected = styles.Selected.Foreground(lipgloss.Color("229")).Background(lipgloss.Color("57")).Bold(false)
	t.SetStyles(styles)
	return app{store: store, cfg: cfg, table: t, search: search, logs: viewport.New(80, 10)}
}

func (m app) Init() tea.Cmd {
	return m.reload()
}

func (m app) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	switch value := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = value.Width
		m.height = value.Height
		m.table.SetHeight(max(6, value.Height-10))
		m.logs.Width = max(20, value.Width-4)
		m.logs.Height = max(6, value.Height-12)
	case projectsMsg:
		m.projects = []db.ProjectSummary(value)
		m.refreshRows()
	case runMsg:
		m.running = false
		if value.err != nil {
			m.message = value.err.Error()
		} else {
			m.message = fmt.Sprintf("completed %s (%s)", value.result.Run.RunID, value.result.Run.Status)
		}
		cmds = append(cmds, m.reload())
	case tea.KeyMsg:
		key := value.String()
		if m.confirm {
			switch key {
			case "y", "Y":
				m.confirm = false
				m.running = true
				m.message = "running pipeline..."
				cmds = append(cmds, m.runSelected())
			case "n", "N", "esc":
				m.confirm = false
				m.message = "rerun cancelled"
			}
			return m, tea.Batch(cmds...)
		}
		switch key {
		case "ctrl+c", "q":
			return m, tea.Quit
		case "tab":
			m.tab = (m.tab + 1) % 2
		case "shift+tab":
			m.tab = (m.tab + 1) % 2
		case "esc":
			m.tab = 0
		case "enter":
			if m.tab == 0 {
				m.tab = 1
			}
		case "left":
			if m.tab == 1 && m.selectedStage > 0 {
				m.selectedStage--
			}
		case "right":
			if m.tab == 1 && m.selectedStage < 5 {
				m.selectedStage++
			}
		case "r":
			if m.selectedTaskID() != "" && !m.running {
				m.confirm = true
			}
		default:
			if m.tab == 0 {
				var cmd tea.Cmd
				m.search, cmd = m.search.Update(value)
				cmds = append(cmds, cmd)
				m.refreshRows()
			}
		}
	}
	var cmd tea.Cmd
	if m.tab == 0 {
		m.table, cmd = m.table.Update(msg)
	} else {
		m.logs, cmd = m.logs.Update(msg)
	}
	cmds = append(cmds, cmd)
	return m, tea.Batch(cmds...)
}

func (m app) View() string {
	var builder strings.Builder
	builder.WriteString(titleStyle.Render("p2r QA CLI") + "\n")
	builder.WriteString(m.tabs() + "\n\n")
	if m.message != "" {
		builder.WriteString(m.messageStyle().Render(m.message) + "\n\n")
	}
	if m.confirm {
		builder.WriteString(errorStyle.Render("Rerun "+m.selectedTaskID()+" from stage "+stageLetter(m.selectedStage)+"? Affected: "+strings.Join(affectedStages(stageLetter(m.selectedStage)), ", ")+"  y/n") + "\n\n")
	}
	if m.tab == 0 {
		builder.WriteString(m.overview())
	} else {
		builder.WriteString(m.execution())
	}
	builder.WriteString("\n" + mutedStyle.Render("Tab: switch panel  Enter: view execution  r: rerun  q: quit"))
	return panelStyle.Render(builder.String())
}

func (m app) tabs() string {
	overview := "[项目总览]"
	execution := "[执行]"
	if m.tab == 0 {
		overview = activeStyle.Render(overview)
	} else {
		execution = activeStyle.Render(execution)
	}
	return overview + "  " + execution
}

func (m app) overview() string {
	return m.search.View() + "\n\n" + m.table.View()
}

func (m app) execution() string {
	taskID := m.selectedTaskID()
	if taskID == "" {
		return mutedStyle.Render("No indexed project selected. Run `p2r scan --path <projects-qa>` first.")
	}
	run, err := m.store.LatestRunForTask(context.Background(), taskID)
	if err != nil {
		return fmt.Sprintf("Task: %s\n\nNo run yet. Press r to start a pipeline run.", taskID)
	}
	stages, _ := m.store.Stages(context.Background(), run.RunID)
	findings, _ := m.store.Findings(context.Background(), run.RunID)
	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("Task: %s  Run: %s  %s\n\n", taskID, run.RunID, run.Status))
	for index, stage := range stages {
		prefix := "  "
		if index == m.selectedStage {
			prefix = "> "
		}
		builder.WriteString(fmt.Sprintf("%s[%s] %-34s %-10s %6dms", prefix, stage.Stage, stage.Name, stage.Status, stage.DurationMS))
		if stage.ErrorSummary != "" {
			builder.WriteString("  " + stage.ErrorSummary)
		}
		builder.WriteString("\n")
	}
	blocker, high, medium := 0, 0, 0
	for _, finding := range findings {
		switch finding.Severity {
		case "Blocker":
			blocker++
		case "High":
			high++
		case "Medium":
			medium++
		}
	}
	builder.WriteString(fmt.Sprintf("\nFindings: Blocker %d | High %d | Medium %d\n\n", blocker, high, medium))
	for i, finding := range findings {
		if i >= 8 {
			break
		}
		builder.WriteString(fmt.Sprintf("[%s] %s: %s\n", finding.Stage, finding.Severity, finding.Title))
	}
	m.logs.SetContent(builder.String())
	return m.logs.View()
}

func (m app) reload() tea.Cmd {
	return func() tea.Msg {
		projects, err := m.store.ListProjects(context.Background())
		if err != nil {
			return runMsg{err: err}
		}
		return projectsMsg(projects)
	}
}

func (m app) runSelected() tea.Cmd {
	taskID := m.selectedTaskID()
	stage := stageLetter(m.selectedStage)
	return func() tea.Msg {
		runner := pipeline.NewRunner(m.store, m.cfg)
		result, err := runner.Run(context.Background(), taskID, pipeline.RunOptions{Stages: affectedStages(stage)})
		return runMsg{result: result, err: err}
	}
}

func (m *app) refreshRows() {
	filter := strings.ToLower(strings.TrimSpace(m.search.Value()))
	rows := make([]table.Row, 0, len(m.projects))
	for _, project := range m.projects {
		needle := strings.ToLower(project.TaskID + " " + project.Batch + " " + project.RunStatus)
		if filter != "" && !strings.Contains(needle, filter) {
			continue
		}
		rows = append(rows, table.Row{
			project.TaskID,
			project.Batch,
			project.LastRunAt,
			empty(project.RunStatus, "-"),
			empty(project.FailedStage, "-"),
			fmt.Sprint(project.Blocking),
			fmt.Sprint(project.High),
			empty(project.ManualVerdict, "unset"),
		})
	}
	m.table.SetRows(rows)
}

func (m app) selectedTaskID() string {
	row := m.table.SelectedRow()
	if len(row) == 0 {
		return ""
	}
	return row[0]
}

func (m app) messageStyle() lipgloss.Style {
	if strings.Contains(strings.ToLower(m.message), "error") || strings.Contains(strings.ToLower(m.message), "failed") {
		return errorStyle
	}
	return mutedStyle
}

func stageLetter(index int) string {
	stages := []string{"A", "B", "C", "D", "E", "F"}
	if index < 0 || index >= len(stages) {
		return "A"
	}
	return stages[index]
}

func affectedStages(stage string) []string {
	switch stage {
	case "A":
		return []string{"A", "B", "C", "F"}
	case "B":
		return []string{"B", "C", "F"}
	case "C":
		return []string{"C", "F"}
	case "D", "E":
		return []string{stage, "F"}
	default:
		return []string{stage}
	}
}

func empty(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
