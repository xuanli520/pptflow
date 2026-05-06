package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/db"
	"github.com/xuanli520/p2r_tui/internal/pipeline"
)

const (
	panelOverview = iota
	panelExecution

	staleRunRecoveryInterval = 30 * time.Second
)

type app struct {
	store *db.Store
	cfg   config.Config

	projects      []db.ProjectSummary
	overviewItems []overviewItem
	visibleRows   []overviewItem

	table  table.Model
	search textinput.Model
	detail viewport.Model

	tab   int
	focus focusArea

	selectedTaskIDValue string
	selectedStageKey    string
	selectedRefRunID    string
	stageIndex          int
	refIndex            int

	width  int
	height int

	confirm bool
	message string
	running bool
	qaMode  string

	detailVM       executionViewModel
	detailContent  string
	lastRecoveryAt time.Time
}

type projectsMsg struct {
	projects []db.ProjectSummary
	items    []overviewItem
	err      error
}

type detailMsg struct {
	taskID string
	vm     executionViewModel
	err    error
}

type runMsg struct {
	result pipeline.Result
	err    error
}

type recoveryMsg struct {
	err error
}

type tickMsg time.Time

func Start(store *db.Store, cfg config.Config) error {
	program := tea.NewProgram(newApp(store, cfg), tea.WithAltScreen())
	_, err := program.Run()
	return err
}

func newApp(store *db.Store, cfg config.Config) app {
	search := textinput.New()
	search.Placeholder = "搜索任务ID、批次、状态或阶段..."
	search.Prompt = "搜索: "
	search.Focus()

	t := table.New(
		table.WithColumns(buildOverviewColumns(120)),
		table.WithFocused(false),
		table.WithHeight(12),
	)
	t.SetStyles(tableStyles())

	m := app{
		store:          store,
		cfg:            cfg,
		table:          t,
		search:         search,
		detail:         viewport.New(80, 10),
		tab:            panelOverview,
		focus:          focusSearch,
		qaMode:         "initial",
		message:        "",
		lastRecoveryAt: time.Now(),
	}
	m.setFocus(focusSearch)
	return m
}

func (m app) Init() tea.Cmd {
	return tea.Batch(m.recoverStaleRuns(), m.reload(), m.tick())
}

func (m app) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch value := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = value.Width
		m.height = value.Height
		m.applyLayout()
	case projectsMsg:
		if value.err != nil {
			m.message = value.err.Error()
			break
		}
		m.projects = value.projects
		m.overviewItems = value.items
		m.refreshRows()
		if m.selectedTaskID() != "" {
			cmds = append(cmds, m.reloadDetail())
		}
	case detailMsg:
		if value.taskID != m.selectedTaskID() {
			break
		}
		if value.err != nil {
			m.message = value.err.Error()
			break
		}
		m.detailVM = value.vm
		m.syncStageSelection()
		m.syncRefSelection()
		m.updateDetailContent(false)
	case runMsg:
		m.running = false
		if value.err != nil {
			m.message = value.err.Error()
		} else {
			m.message = fmt.Sprintf("流水线完成 %s（%s）", value.result.Run.RunID, localizeRunStatus(value.result.Run.Status))
		}
		cmds = append(cmds, m.reload())
	case recoveryMsg:
		if value.err != nil {
			m.message = value.err.Error()
		}
	case tickMsg:
		if time.Since(m.lastRecoveryAt) >= staleRunRecoveryInterval {
			m.lastRecoveryAt = time.Time(value)
			cmds = append(cmds, m.recoverStaleRuns())
		}
		cmds = append(cmds, m.reload(), m.tick())
	case tea.KeyMsg:
		next, keyCmds := m.handleKey(value)
		m = next
		cmds = append(cmds, keyCmds...)
	}

	return m, tea.Batch(cmds...)
}

func (m app) View() string {
	var builder strings.Builder
	builder.WriteString(renderHeader(m))
	builder.WriteString("\n")
	if m.message != "" {
		builder.WriteString(messageStyle(m.message).Render(m.message))
		builder.WriteString("\n")
	}
	if m.confirm {
		builder.WriteString(renderConfirm(m))
		builder.WriteString("\n")
	}
	if m.tab == panelOverview {
		builder.WriteString(renderOverview(m))
	} else {
		builder.WriteString(renderExecution(m))
	}
	builder.WriteString("\n")
	builder.WriteString(footerStyle.Render(footerFor(m)))
	return appStyle.Render(builder.String())
}

func (m app) reload() tea.Cmd {
	if m.store == nil {
		return nil
	}
	return func() tea.Msg {
		ctx := context.Background()
		projects, err := m.store.ListProjects(ctx)
		if err != nil {
			return projectsMsg{err: err}
		}
		items := buildOverviewItems(ctx, m.store, m.cfg, projects)
		return projectsMsg{projects: projects, items: items}
	}
}

func (m app) recoverStaleRuns() tea.Cmd {
	if m.store == nil {
		return nil
	}
	return func() tea.Msg {
		return recoveryMsg{err: pipeline.RecoverStaleRuns(context.Background(), m.store, m.cfg)}
	}
}

func (m app) reloadDetail() tea.Cmd {
	taskID := m.selectedTaskID()
	if m.store == nil || taskID == "" {
		return nil
	}
	return func() tea.Msg {
		vm, err := buildExecutionViewModel(context.Background(), m.store, m.cfg, taskID)
		return detailMsg{taskID: taskID, vm: vm, err: err}
	}
}

func (m app) tick() tea.Cmd {
	interval := time.Duration(m.cfg.TUI.RefreshIntervalMS) * time.Millisecond
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	return tea.Tick(interval, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m app) runSelected() tea.Cmd {
	taskID := m.selectedTaskID()
	refRun := m.selectedRefRun()
	mode := m.qaMode
	plan := m.rerunStagePlan()
	return func() tea.Msg {
		runner := pipeline.NewRunner(m.store, m.cfg)
		result, err := runner.Run(context.Background(), taskID, pipeline.RunOptions{
			Stages: plan.runStages,
			Mode:   mode,
			RefRun: refRun,
		})
		return runMsg{result: result, err: err}
	}
}

func (m *app) applyLayout() {
	layout := layoutFor(m.width, m.height, m.tab == panelExecution)
	m.search.Width = max(12, layout.contentWidth-8)
	m.table.SetWidth(layout.contentWidth)
	m.table.SetHeight(layout.overviewTableHeight)
	m.detail.Width = layout.detailWidth
	m.detail.Height = layout.detailHeight
	if m.tab == panelExecution {
		m.detail.Height = max(3, layout.detailHeight-1)
	}
	m.refreshRows()
	m.updateDetailContent(false)
}

func (m *app) refreshRows() {
	specs := overviewColumnSpecs(m.width)
	columns := make([]table.Column, 0, len(specs))
	for _, spec := range specs {
		columns = append(columns, table.Column{Title: spec.Title, Width: spec.Width})
	}
	m.table.SetRows(nil)
	m.table.SetColumns(columns)

	filter := strings.ToLower(strings.TrimSpace(m.search.Value()))
	rows := make([]table.Row, 0, len(m.overviewItems))
	visible := make([]overviewItem, 0, len(m.overviewItems))
	for _, item := range m.overviewItems {
		if filter != "" && !strings.Contains(strings.ToLower(item.SearchText), filter) {
			continue
		}
		rows = append(rows, overviewDisplayRow(item, specs))
		visible = append(visible, item)
	}

	previous := m.selectedTaskIDValue
	m.visibleRows = visible
	m.table.SetRows(rows)
	switch {
	case len(visible) == 0:
		m.selectedTaskIDValue = ""
		m.detailVM = executionViewModel{}
	case previous != "":
		if index := overviewIndex(visible, previous); index >= 0 {
			m.table.SetCursor(index)
			m.selectedTaskIDValue = previous
		} else {
			m.table.SetCursor(0)
			m.selectedTaskIDValue = visible[0].TaskID
			if filter != "" {
				m.message = "当前选择已被过滤，已切换到第一条结果"
			}
		}
	default:
		m.table.SetCursor(0)
		m.selectedTaskIDValue = visible[0].TaskID
	}
}

func (m *app) syncSelectedTaskFromCursor() bool {
	if len(m.visibleRows) == 0 {
		changed := m.selectedTaskIDValue != ""
		m.selectedTaskIDValue = ""
		return changed
	}
	index := clamp(m.table.Cursor(), 0, len(m.visibleRows)-1)
	next := m.visibleRows[index].TaskID
	changed := next != m.selectedTaskIDValue
	m.selectedTaskIDValue = next
	return changed
}

func (m app) selectedTaskID() string {
	return m.selectedTaskIDValue
}

func (m app) selectedStage() stageView {
	if len(m.detailVM.Stages) == 0 {
		return stageView{}
	}
	index := clamp(m.stageIndex, 0, len(m.detailVM.Stages)-1)
	return m.detailVM.Stages[index]
}

func (m app) selectedRefRun() string {
	if m.qaMode != "recheck" {
		return ""
	}
	if m.selectedRefRunID != "" {
		return m.selectedRefRunID
	}
	if len(m.detailVM.RefRuns) == 0 {
		return ""
	}
	index := clamp(m.refIndex, 0, len(m.detailVM.RefRuns)-1)
	return m.detailVM.RefRuns[index].RunID
}

func (m *app) syncStageSelection() {
	if len(m.detailVM.Stages) == 0 {
		m.stageIndex = 0
		m.selectedStageKey = ""
		return
	}
	if m.selectedStageKey != "" {
		for index, stage := range m.detailVM.Stages {
			if stage.Stage == m.selectedStageKey {
				m.stageIndex = index
				return
			}
		}
	}
	m.stageIndex = preferredStageIndex(m.detailVM.Stages)
	m.selectedStageKey = m.detailVM.Stages[m.stageIndex].Stage
}

func (m *app) syncRefSelection() {
	if len(m.detailVM.RefRuns) == 0 {
		m.refIndex = 0
		m.selectedRefRunID = ""
		return
	}
	if m.selectedRefRunID != "" {
		for index, run := range m.detailVM.RefRuns {
			if run.RunID == m.selectedRefRunID {
				m.refIndex = index
				return
			}
		}
	}
	m.refIndex = clamp(m.refIndex, 0, len(m.detailVM.RefRuns)-1)
	m.selectedRefRunID = m.detailVM.RefRuns[m.refIndex].RunID
}

func (m *app) moveStage(delta int) {
	if len(m.detailVM.Stages) == 0 {
		return
	}
	m.stageIndex = clamp(m.stageIndex+delta, 0, len(m.detailVM.Stages)-1)
	m.selectedStageKey = m.detailVM.Stages[m.stageIndex].Stage
	m.updateDetailContent(true)
}

func (m *app) moveRef(delta int) {
	if len(m.detailVM.RefRuns) == 0 {
		return
	}
	m.refIndex = clamp(m.refIndex+delta, 0, len(m.detailVM.RefRuns)-1)
	m.selectedRefRunID = m.detailVM.RefRuns[m.refIndex].RunID
}

func (m *app) updateDetailContent(resetScroll bool) {
	width := m.detail.Width
	if width <= 0 {
		width = max(40, m.width/2)
	}
	content := buildDetailContent(m.detailVM, m.selectedStageKey, width)
	if content == m.detailContent {
		return
	}
	m.detailContent = content
	m.detail.SetContent(content)
	if resetScroll {
		m.detail.GotoTop()
	}
}

func overviewIndex(rows []overviewItem, taskID string) int {
	for index, row := range rows {
		if row.TaskID == taskID {
			return index
		}
	}
	return -1
}
