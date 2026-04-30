package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	"github.com/xuanli520/p2r_tui/internal/taskdocs"
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
		{Title: "failed", Width: 6},
		{Title: "blocker", Width: 7},
		{Title: "high", Width: 5},
		{Title: "docs", Width: 5},
		{Title: "cleanup", Width: 10},
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
		m.applyTableColumns()
		m.refreshRows()
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
			if m.tab == 1 && m.selectedStage < m.latestStageCount()-1 {
				m.selectedStage++
			}
		case "a":
			if taskID := m.selectedTaskID(); taskID != "" {
				m.message = fmt.Sprintf("attach docs with: p2r attach %s --file <path>", taskID)
			}
		case "d":
			if taskID := m.selectedTaskID(); taskID != "" {
				m.message = fmt.Sprintf("docs manifest: %s", taskdocs.ManifestPath(m.cfg.ScanPath, taskID))
			}
		case "p":
			if run, ok := m.latestRun(); ok {
				m.message = fmt.Sprintf("preflight: %s", filepath.Join(run.ArtifactRoot, "preflight.json"))
			}
		case "c":
			if run, ok := m.latestRun(); ok {
				m.message = fmt.Sprintf("cleanup: %s", filepath.Join(run.ArtifactRoot, "cleanup_summary.json"))
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
	builder.WriteString("\n" + mutedStyle.Render("Tab: switch panel  Enter: view execution  a: attach hint  d/p/c: docs/preflight/cleanup  m: mode  ↑/↓: ref run  r: rerun  q: quit"))
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
	projectPath := filepathFromProject(m.projects, taskID)
	selfTestPath := pipeline.SelfTestReportPath(projectPath, m.cfg)
	selfTestStatus := "x 未找到，请放置到 " + selfTestPath
	for _, candidate := range pipeline.SelfTestReportCandidates(projectPath, m.cfg) {
		if fileExists(candidate) {
			selfTestStatus = "ok " + candidate
			break
		}
	}
	if err != nil {
		return fmt.Sprintf("Task: %s\nMode: %s\n自测报告: %s\n\nNo run yet. Press r to start a pipeline run.", taskID, m.qaMode, selfTestStatus)
	}
	stages, _ := m.store.Stages(context.Background(), run.RunID)
	stages = normalizeStages(stages)
	if m.selectedStage >= len(stages) {
		m.selectedStage = max(0, len(stages)-1)
	}
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
	builder.WriteString(fmt.Sprintf("Artifacts: %s\n", run.ArtifactRoot))
	builder.WriteString(fmt.Sprintf("Docs: %d  Manifest: %s\n", taskdocs.Count(m.cfg.ScanPath, taskID), taskdocs.ManifestPath(m.cfg.ScanPath, taskID)))
	builder.WriteString(fmt.Sprintf("Preflight: %s\nCleanup: %s\n\n", filepath.Join(run.ArtifactRoot, "preflight.json"), cleanupStatus(run.ArtifactRoot)))
	var selectedLog string
	var selected model.StageRecord
	for index, stage := range stages {
		if stage.Name == "" {
			stage.Name = model.StageDisplayName(stage.Stage)
		}
		prefix := "  "
		if index == m.selectedStage {
			prefix = "> "
			selectedLog = stage.LogPath
			selected = stage
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
	builder.WriteString(stageDetail(selected, findings))
	builder.WriteString("\n")
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
		if m.width > 0 && m.width < 110 {
			rows = append(rows, table.Row{
				project.TaskID,
				empty(project.RunStatus, "-"),
				empty(project.FailedStage, "-"),
				fmt.Sprint(project.Blocking),
				fmt.Sprint(project.High),
				fmt.Sprint(taskdocs.Count(m.cfg.ScanPath, project.TaskID)),
				latestCleanupStatus(m.store, project.TaskID),
			})
		} else {
			rows = append(rows, table.Row{
				project.TaskID,
				project.Batch,
				project.LastRunAt,
				empty(project.RunStatus, "-"),
				empty(project.FailedStage, "-"),
				fmt.Sprint(project.Blocking),
				fmt.Sprint(project.High),
				fmt.Sprint(taskdocs.Count(m.cfg.ScanPath, project.TaskID)),
				latestCleanupStatus(m.store, project.TaskID),
				empty(project.ManualVerdict, "unset"),
			})
		}
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

func (m app) latestRun() (model.RunRecord, bool) {
	taskID := m.selectedTaskID()
	if taskID == "" || m.store == nil {
		return model.RunRecord{}, false
	}
	run, err := m.store.LatestRunForTask(context.Background(), taskID)
	return run, err == nil
}

func (m app) latestStageCount() int {
	run, ok := m.latestRun()
	if !ok {
		return 6
	}
	stages, err := m.store.Stages(context.Background(), run.RunID)
	if err != nil || len(stages) == 0 {
		return 6
	}
	return len(normalizeStages(stages))
}

func (m *app) applyTableColumns() {
	columns := []table.Column{
		{Title: "task_id", Width: 20},
		{Title: "batch", Width: 12},
		{Title: "last_run", Width: 16},
		{Title: "run_status", Width: 18},
		{Title: "failed", Width: 6},
		{Title: "blocker", Width: 7},
		{Title: "high", Width: 5},
		{Title: "docs", Width: 5},
		{Title: "cleanup", Width: 10},
		{Title: "manual", Width: 8},
	}
	if m.width > 0 && m.width < 110 {
		columns = []table.Column{
			{Title: "task_id", Width: 20},
			{Title: "status", Width: 18},
			{Title: "failed", Width: 6},
			{Title: "blk", Width: 4},
			{Title: "hi", Width: 4},
			{Title: "docs", Width: 5},
			{Title: "cleanup", Width: 10},
		}
	}
	m.table.SetColumns(columns)
}

func normalizeStages(stages []model.StageRecord) []model.StageRecord {
	byStage := map[string]model.StageRecord{}
	for _, stage := range stages {
		if stage.Name == "" {
			stage.Name = model.StageDisplayName(stage.Stage)
		}
		byStage[stage.Stage] = stage
	}
	result := make([]model.StageRecord, 0, 6)
	for _, letter := range []string{"A", "B", "C", "D", "E", "F"} {
		if stage, ok := byStage[letter]; ok {
			result = append(result, stage)
			continue
		}
		result = append(result, model.StageRecord{Stage: letter, Name: model.StageDisplayName(letter), Status: model.StageSkipped, ErrorSummary: "No row stored for this run."})
	}
	return result
}

func stageDetail(stage model.StageRecord, findings []model.Finding) string {
	if stage.Stage == "" {
		return ""
	}
	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("Selected stage %s: %s\n", stage.Stage, stage.Name))
	builder.WriteString(fmt.Sprintf("Status: %s  Duration: %dms\n", stage.Status, stage.DurationMS))
	if stage.ErrorSummary != "" {
		builder.WriteString("Reason: " + stage.ErrorSummary + "\n")
	}
	if stage.LogPath != "" {
		builder.WriteString("Log: " + stage.LogPath + "\n")
	}
	if len(stage.ArtifactPaths) > 0 {
		builder.WriteString("Artifacts:\n")
		for _, path := range stage.ArtifactPaths {
			builder.WriteString("  " + path + "\n")
		}
	}
	count := 0
	for _, finding := range findings {
		if finding.Stage != stage.Stage {
			continue
		}
		if count == 0 {
			builder.WriteString("Stage findings:\n")
		}
		builder.WriteString(fmt.Sprintf("  [%s] %s\n", finding.Severity, finding.Title))
		if finding.SourcePath != "" {
			builder.WriteString("    " + finding.SourcePath + "\n")
		}
		count++
		if count >= 5 {
			break
		}
	}
	return builder.String()
}

func latestCleanupStatus(store *db.Store, taskID string) string {
	if store == nil {
		return "-"
	}
	run, err := store.LatestRunForTask(context.Background(), taskID)
	if err != nil {
		return "-"
	}
	return cleanupStatus(run.ArtifactRoot)
}

func cleanupStatus(artifactRoot string) string {
	path := filepath.Join(artifactRoot, "cleanup_summary.json")
	content, err := os.ReadFile(path)
	if err != nil {
		return "none"
	}
	var data map[string]any
	if json.Unmarshal(content, &data) != nil {
		return "unknown"
	}
	if status, ok := data["status"].(string); ok && status != "" {
		return status
	}
	return "unknown"
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
		return []string{"A", "F"}
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
