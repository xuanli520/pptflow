package tui

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/db"
	"github.com/xuanli520/p2r_tui/internal/pipeline"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
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
	qaMode        string
	refRuns       []model.RunRecord
	refIndex      int
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
	return app{store: store, cfg: cfg, table: t, search: search, logs: viewport.New(80, 10), qaMode: "initial"}
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
		m.refreshRefRuns()
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
			m.refreshRefRuns()
		case "shift+tab":
			m.tab = (m.tab + 1) % 2
			m.refreshRefRuns()
		case "esc":
			m.tab = 0
		case "enter":
			if m.tab == 0 {
				m.tab = 1
				m.refreshRefRuns()
			}
		case "left":
			if m.tab == 1 && m.selectedStage > 0 {
				m.selectedStage--
			}
		case "right":
			if m.tab == 1 && m.selectedStage < 5 {
				m.selectedStage++
			}
		case "m":
			if m.qaMode == "recheck" {
				m.qaMode = "initial"
			} else {
				m.qaMode = "recheck"
				m.refreshRefRuns()
			}
		case "up":
			m.refreshRefRuns()
			if m.tab == 1 && m.qaMode == "recheck" && m.refIndex > 0 {
				m.refIndex--
			}
		case "down":
			m.refreshRefRuns()
			if m.tab == 1 && m.qaMode == "recheck" && m.refIndex < len(m.refRuns)-1 {
				m.refIndex++
			}
		case "r":
			m.refreshRefRuns()
			if m.selectedTaskID() != "" && !m.running {
				if m.qaMode == "recheck" && m.selectedRefRun() == "" {
					m.message = "recheck mode requires selecting a ref run"
					break
				}
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
		ref := m.selectedRefRun()
		confirm := fmt.Sprintf("Rerun %s?\nMode: %s\n", m.selectedTaskID(), m.qaMode)
		if m.qaMode == "recheck" {
			confirm += "Ref run: " + ref + "\n"
		}
		confirm += "Stages: " + strings.Join(affectedStages(stageLetter(m.selectedStage)), ", ") + "\ny/n"
		builder.WriteString(errorStyle.Render(confirm) + "\n\n")
	}
	if m.tab == 0 {
		builder.WriteString(m.overview())
	} else {
		builder.WriteString(m.execution())
	}
	builder.WriteString("\n" + mutedStyle.Render("Tab: switch panel  Enter: view execution  m: mode  ↑/↓: ref run  r: rerun  q: quit"))
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
	refRuns := m.refRuns
	if len(refRuns) == 0 {
		refRuns, _ = m.store.ListRunsForTask(context.Background(), taskID)
	}
	selfTestPath := pipeline.SelfTestReportPath(filepathFromProject(m.projects, taskID), m.cfg)
	selfTestStatus := "✗ 未找到，请放置到 " + selfTestPath
	if fileExists(selfTestPath) {
		selfTestStatus = "✓ 已就绪"
	}
	if err != nil {
		return fmt.Sprintf("Task: %s\nMode: %s\n自测报告: %s\n\nNo run yet. Press r to start a pipeline run.", taskID, m.qaMode, selfTestStatus)
	}
	stages, _ := m.store.Stages(context.Background(), run.RunID)
	findings, _ := m.store.Findings(context.Background(), run.RunID)
	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("Task: %s  Run: %s  %s\n", taskID, run.RunID, run.Status))
	builder.WriteString(fmt.Sprintf("Mode: %s", m.qaMode))
	if m.qaMode == "recheck" {
		builder.WriteString(fmt.Sprintf("  Ref run: %s", empty(m.selectedRefRun(), "-")))
	}
	builder.WriteString("\n自测报告: " + selfTestStatus + "\n\n")
	if m.qaMode == "recheck" {
		builder.WriteString("Ref runs:\n")
		for i, item := range refRuns {
			prefix := "  "
			if i == m.refIndex {
				prefix = "> "
			}
			builder.WriteString(fmt.Sprintf("%s%s %s %s\n", prefix, item.RunID, item.Status, item.StartedAt))
		}
		builder.WriteString("\n")
	}
	builder.WriteString(fmt.Sprintf("Artifacts: %s\n\n", run.ArtifactRoot))
	var selectedLog string
	for index, stage := range stages {
		prefix := "  "
		if index == m.selectedStage {
			prefix = "> "
			selectedLog = stage.LogPath
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
	if selectedLog != "" {
		builder.WriteString("\n" + stageLogPreview(selectedLog, m.cfg.TUI.LogMaxLines))
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
		result, err := runner.Run(context.Background(), taskID, pipeline.RunOptions{Stages: affectedStages(stage), Mode: m.qaMode, RefRun: m.selectedRefRun()})
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

func (m *app) refreshRefRuns() {
	taskID := m.selectedTaskID()
	if taskID == "" || m.store == nil {
		m.refRuns = nil
		m.refIndex = 0
		return
	}
	runs, err := m.store.ListRunsForTask(context.Background(), taskID)
	if err != nil {
		m.refRuns = nil
		m.refIndex = 0
		return
	}
	filtered := runs[:0]
	for _, run := range runs {
		if run.Status == model.RunRunning {
			continue
		}
		filtered = append(filtered, run)
	}
	m.refRuns = filtered
	if m.refIndex >= len(m.refRuns) {
		m.refIndex = max(0, len(m.refRuns)-1)
	}
}

func (m app) selectedTaskID() string {
	row := m.table.SelectedRow()
	if len(row) == 0 {
		return ""
	}
	return row[0]
}

func (m app) selectedRefRun() string {
	if m.qaMode != "recheck" || len(m.refRuns) == 0 || m.refIndex < 0 || m.refIndex >= len(m.refRuns) {
		return ""
	}
	return m.refRuns[m.refIndex].RunID
}

func filepathFromProject(projects []db.ProjectSummary, taskID string) string {
	for _, project := range projects {
		if project.TaskID == taskID {
			return project.Path
		}
	}
	return ""
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
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

func stageLogPreview(path string, maxLines int) string {
	content, err := os.ReadFile(path)
	if err != nil {
		return "Log: " + path + "\n" + err.Error() + "\n"
	}
	text := strings.TrimRight(string(content), "\r\n")
	lines := strings.Split(text, "\n")
	if maxLines <= 0 {
		maxLines = 200
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return "Log: " + path + "\n" + strings.Join(lines, "\n")
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
