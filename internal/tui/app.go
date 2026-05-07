package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/db"
	"github.com/xuanli520/p2r_tui/internal/pipeline"
	"github.com/xuanli520/p2r_tui/internal/scheduler"
)

const (
	panelOverview = iota
	panelExecution

	staleRunRecoveryInterval = 30 * time.Second
)

type app struct {
	store     *db.Store
	cfg       config.Config
	scheduler *scheduler.Scheduler

	overview OverviewModel
	detail   viewport.Model

	tab   int
	focus focusArea

	selectedStageKey string
	selectedRefRunID string
	stageIndex       int
	refIndex         int

	width  int
	height int

	message    string
	pendingJob string
	qaMode     string
	runConfig  runConfig
	activeJobs []scheduler.JobSnapshot

	detailVM       executionViewModel
	detailContent  string
	lastRecoveryAt time.Time
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

type runSubmitMsg struct {
	jobID string
	err   error
}

type schedulerJobsMsg struct {
	jobs []scheduler.JobSnapshot
}

type schedulerNotifyMsg struct{}

type recoveryMsg struct {
	err error
}

type tickMsg time.Time

func Start(store *db.Store, cfg config.Config) error {
	m := newApp(store, cfg)
	defer func() {
		if m.scheduler == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = m.scheduler.Shutdown(ctx)
	}()
	program := tea.NewProgram(m, tea.WithAltScreen())
	_, err := program.Run()
	return err
}

func newApp(store *db.Store, cfg config.Config) app {
	m := app{
		store:          store,
		cfg:            cfg,
		overview:       newOverviewModel(),
		detail:         viewport.New(80, 10),
		tab:            panelOverview,
		focus:          focusSearch,
		qaMode:         "initial",
		message:        "",
		lastRecoveryAt: time.Now(),
	}
	if store != nil {
		m.scheduler = scheduler.New(store, cfg)
	}
	m.setFocus(focusSearch)
	return m
}

func (m app) Init() tea.Cmd {
	return tea.Batch(m.recoverStaleRuns(), m.overview.Init(), m.reloadSchedulerJobs(), m.waitSchedulerNotify(), m.tick())
}

func (m app) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch value := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = value.Width
		m.height = value.Height
		m.overview.page.autoSize = true
		if cmd := m.applyLayout(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case overviewRefreshMsg:
		var cmd tea.Cmd
		m.overview, cmd = m.overview.Update(value)
		cmds = append(cmds, cmd)
	case overviewSearchDebounceMsg:
		var cmd tea.Cmd
		m.overview, cmd = m.overview.Update(value)
		cmds = append(cmds, cmd)
	case overviewLoadRequestMsg:
		cmds = append(cmds, m.handleOverviewLoad(value))
	case overviewLoadResultMsg:
		if value.seq != m.overview.seq {
			break
		}
		beforeID := m.selectedTaskID()
		beforeKey := m.selectedOverviewDetailKey()
		var cmd tea.Cmd
		m.overview, cmd = m.overview.Update(value)
		cmds = append(cmds, cmd)
		if value.err != nil {
			m.message = value.err.Error()
			break
		}
		afterID := m.selectedTaskID()
		afterKey := m.selectedOverviewDetailKey()
		if afterID == "" {
			m.detailVM = executionViewModel{}
			m.detailContent = ""
			m.detail.SetContent("")
			break
		}
		if afterID != beforeID || afterKey != beforeKey || value.refreshDetail {
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
		if value.err != nil {
			m.message = value.err.Error()
		} else {
			m.message = fmt.Sprintf("流水线完成 %s（%s）", value.result.Run.RunID, localizeRunStatus(value.result.Run.Status))
		}
		cmds = append(cmds, m.reload())
	case runSubmitMsg:
		if value.err != nil {
			m.message = value.err.Error()
		} else {
			m.message = fmt.Sprintf("已提交 job %s", value.jobID)
			m.pendingJob = value.jobID
			m.runConfig = runConfig{}
		}
		cmds = append(cmds, m.reloadSchedulerJobs())
	case schedulerJobsMsg:
		m.activeJobs = value.jobs
		m.updatePendingJobMessage(value.jobs)
		if cmd := m.applyLayout(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case schedulerNotifyMsg:
		cmds = append(cmds, m.reloadSchedulerJobs(), m.reload(), m.waitSchedulerNotify())
	case recoveryMsg:
		if value.err != nil {
			m.message = value.err.Error()
		}
	case tickMsg:
		if time.Since(m.lastRecoveryAt) >= staleRunRecoveryInterval {
			m.lastRecoveryAt = time.Time(value)
			cmds = append(cmds, m.recoverStaleRuns())
		}
		cmds = append(cmds, m.reload(), m.reloadSchedulerJobs(), m.tick())
	case tea.KeyMsg:
		next, keyCmds := m.handleKey(value)
		m = next
		cmds = append(cmds, keyCmds...)
	}

	return m, tea.Batch(cmds...)
}

func (m *app) updatePendingJobMessage(jobs []scheduler.JobSnapshot) {
	if m.pendingJob == "" {
		return
	}
	job, ok := findJobSnapshot(jobs, m.pendingJob)
	if !ok {
		return
	}
	switch job.State {
	case scheduler.JobRunning:
		if m.message == fmt.Sprintf("已提交 job %s", job.JobID) {
			m.message = fmt.Sprintf("job %s 已开始运行", job.JobID)
		}
	case scheduler.JobFailed:
		reason := strings.TrimSpace(job.Err)
		if reason == "" {
			reason = "未知错误"
		}
		label := "失败"
		if job.RunID == "" {
			label = "启动失败"
		}
		m.message = fmt.Sprintf("job %s %s: %s", job.JobID, label, reason)
		m.pendingJob = ""
	case scheduler.JobDone:
		if m.message == fmt.Sprintf("已提交 job %s", job.JobID) || m.message == fmt.Sprintf("job %s 已开始运行", job.JobID) {
			m.message = fmt.Sprintf("job %s 已完成", job.JobID)
		}
		m.pendingJob = ""
	}
}

func findJobSnapshot(jobs []scheduler.JobSnapshot, jobID string) (scheduler.JobSnapshot, bool) {
	for _, job := range jobs {
		if job.JobID == jobID {
			return job, true
		}
	}
	return scheduler.JobSnapshot{}, false
}

func (m app) View() string {
	var builder strings.Builder
	builder.WriteString(renderHeader(m))
	builder.WriteString("\n")
	if m.message != "" {
		builder.WriteString(messageStyle(m.message).Render(m.message))
		builder.WriteString("\n")
	}
	builder.WriteString(renderPipelineBar(m))
	builder.WriteString("\n")
	if m.runConfig.active {
		builder.WriteString(renderRunConfig(m))
	} else if m.tab == panelOverview {
		builder.WriteString(renderOverview(m))
	} else {
		builder.WriteString(renderExecution(m))
	}
	builder.WriteString("\n")
	builder.WriteString(footerStyle.Render(footerFor(m)))
	return appStyle.Render(builder.String())
}

func (m app) reload() tea.Cmd {
	return overviewRefreshCmd(true, true)
}

func (m app) handleOverviewLoad(req overviewLoadRequestMsg) tea.Cmd {
	return func() tea.Msg {
		if m.store == nil {
			return overviewLoadResultMsg{
				seq:           req.seq,
				query:         req.query,
				cursorIntent:  req.cursorIntent,
				total:         0,
				refreshDetail: req.refreshDetail,
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		projects, total, err := m.store.ListProjectsPaginated(ctx, req.query)
		if err != nil {
			return overviewLoadResultMsg{
				seq:           req.seq,
				query:         req.query,
				cursorIntent:  req.cursorIntent,
				refreshDetail: req.refreshDetail,
				err:           err,
			}
		}
		items := buildOverviewItems(m.cfg, projects)
		return overviewLoadResultMsg{
			seq:           req.seq,
			query:         req.query,
			cursorIntent:  req.cursorIntent,
			items:         items,
			total:         total,
			refreshDetail: req.refreshDetail,
		}
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

func (m app) submitRun(taskID string, opts pipeline.RunOptions) tea.Cmd {
	return func() tea.Msg {
		if m.scheduler == nil {
			return runSubmitMsg{err: fmt.Errorf("scheduler unavailable")}
		}
		jobID, err := m.scheduler.Submit(taskID, opts)
		return runSubmitMsg{jobID: jobID, err: err}
	}
}

func (m app) reloadSchedulerJobs() tea.Cmd {
	if m.scheduler == nil {
		return nil
	}
	return func() tea.Msg {
		return schedulerJobsMsg{jobs: m.scheduler.Snapshot()}
	}
}

func (m app) waitSchedulerNotify() tea.Cmd {
	if m.scheduler == nil || m.scheduler.NotifyCh() == nil {
		return nil
	}
	ch := m.scheduler.NotifyCh()
	return func() tea.Msg {
		<-ch
		return schedulerNotifyMsg{}
	}
}

func (m app) shutdownScheduler() tea.Cmd {
	if m.scheduler == nil {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = m.scheduler.Shutdown(ctx)
		return nil
	}
}

func (m *app) applyLayout() tea.Cmd {
	layout := layoutFor(m.width, max(8, m.height-pipelineBarHeight(*m)), m.tab == panelExecution)
	pageSizeChanged := m.overview.SetSize(layout.contentWidth, layout.overviewTableHeight)
	m.detail.Width = layout.detailWidth
	m.detail.Height = layout.detailHeight
	if m.tab == panelExecution {
		m.detail.Height = max(3, layout.detailHeight-1)
	}
	m.updateDetailContent(false)
	if pageSizeChanged {
		return overviewRefreshCmd(true, false)
	}
	return nil
}

func (m app) selectedTaskID() string {
	return m.overview.SelectedTaskID()
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
	return m.selectedRefRunCandidate()
}

func (m app) selectedRefRunCandidate() string {
	if len(m.detailVM.RefRuns) == 0 {
		return ""
	}
	if m.selectedRefRunID != "" {
		for _, run := range m.detailVM.RefRuns {
			if run.RunID == m.selectedRefRunID {
				return m.selectedRefRunID
			}
		}
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

type overviewDetailKey struct {
	TaskID        string
	LastRunID     string
	RunStatus     string
	ManualVerdict string
	FailedStage   string
	Blocking      int
	High          int
}

func (m app) selectedOverviewDetailKey() overviewDetailKey {
	item, ok := m.overview.SelectedItem()
	if !ok {
		return overviewDetailKey{}
	}
	return overviewDetailKey{
		TaskID:        item.TaskID,
		LastRunID:     item.LastRunID,
		RunStatus:     item.RunStatus,
		ManualVerdict: item.ManualVerdict,
		FailedStage:   item.FailedStage,
		Blocking:      item.Blocking,
		High:          item.High,
	}
}
