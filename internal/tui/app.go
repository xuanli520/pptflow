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
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/scanner"
	"github.com/xuanli520/p2r_tui/internal/scheduler"
)

const (
	panelOverview = iota
	panelExecution
	panelSettings

	staleRunRecoveryInterval      = 30 * time.Second
	persistedStateRefreshInterval = 2 * time.Second
)

type app struct {
	store              appStore
	cfg                config.Config
	scheduler          schedulerClient
	recoverStaleRunsFn func(context.Context) error

	overview     OverviewModel
	detail       viewport.Model
	dockerMirror dockerMirrorPanel
	settings     settingsPanel

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

	confirmCancelTaskID string
	confirmCancelJobID  string
	pendingCancelJobID  string

	detailVM                 executionViewModel
	detailContent            string
	detailFollowTail         bool
	lastRecoveryAt           time.Time
	lastPersistedRefreshAt   time.Time
	lastTerminalRefreshJobID string
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

type taskCancelRequestMsg struct {
	taskID string
	jobID  string
	err    error
}

type schedulerJobsMsg struct {
	jobs []scheduler.JobSnapshot
}

type schedulerNotifyMsg struct{}

type recoveryMsg struct {
	err error
}

type tickMsg time.Time

type overviewStore interface {
	ListProjectsPaginated(context.Context, db.ProjectQuery) ([]db.ProjectSummary, int, error)
}

type executionStore interface {
	GetProject(context.Context, string) (scanner.Project, error)
	LatestRunForTask(context.Context, string) (model.RunRecord, error)
	Stages(context.Context, string) ([]model.StageRecord, error)
	Findings(context.Context, string) ([]model.Finding, error)
	ListRunsForTask(context.Context, string) ([]model.RunRecord, error)
}

type appStore interface {
	overviewStore
	executionStore
}

type schedulerClient interface {
	Submit(string, pipeline.RunOptions) (string, error)
	CancelTask(string) error
	ActiveSnapshot() []scheduler.JobSnapshot
	NotifyCh() <-chan struct{}
	Shutdown(context.Context) error
}

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
	now := time.Now()
	m := app{
		store:                  store,
		cfg:                    cfg,
		overview:               newOverviewModel(),
		detail:                 viewport.New(80, 10),
		dockerMirror:           newDockerMirrorPanel(cfg),
		settings:               newSettingsPanel(),
		tab:                    panelOverview,
		focus:                  focusSearch,
		qaMode:                 "initial",
		message:                "",
		detailFollowTail:       true,
		lastRecoveryAt:         now,
		lastPersistedRefreshAt: now,
	}
	if store != nil {
		m.store = store
		m.scheduler = scheduler.New(store, cfg)
		m.recoverStaleRunsFn = func(ctx context.Context) error {
			return pipeline.RecoverStaleRuns(ctx, store, cfg)
		}
	}
	m.setFocus(focusSearch)
	return m
}

func (m app) Init() tea.Cmd {
	activeJobs := 0
	if m.scheduler != nil {
		activeJobs = len(m.scheduler.ActiveSnapshot())
	}
	return tea.Batch(m.recoverStaleRunsCmd(), m.overview.Init(), m.reloadSchedulerJobs(), m.waitSchedulerNotify(), dockerStartupGCCmd(m.cfg, activeJobs), m.tick())
}

func (m app) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	if handled := m.handleWindowMsg(msg, &cmds); handled {
		return m, tea.Batch(cmds...)
	}
	if handled := m.handleOverviewMsg(msg, &cmds); handled {
		return m, tea.Batch(cmds...)
	}
	if handled := m.handleDetailMsg(msg, &cmds); handled {
		return m, tea.Batch(cmds...)
	}
	if handled := m.handleSchedulerMsg(msg, &cmds); handled {
		return m, tea.Batch(cmds...)
	}
	if handled := m.handleRecoveryMsg(msg, &cmds); handled {
		return m, tea.Batch(cmds...)
	}
	if handled := m.handleDockerMirrorMsg(msg, &cmds); handled {
		return m, tea.Batch(cmds...)
	}

	switch value := msg.(type) {
	case tea.KeyMsg:
		next, keyCmds := m.handleKey(value)
		m = next
		cmds = append(cmds, keyCmds...)
	}

	return m, tea.Batch(cmds...)
}

func (m *app) handleWindowMsg(msg tea.Msg, cmds *[]tea.Cmd) bool {
	switch value := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = value.Width
		m.height = value.Height
		m.overview.page.autoSize = true
		if cmd := m.applyLayout(); cmd != nil {
			*cmds = append(*cmds, cmd)
		}
		return true
	default:
		return false
	}
}

func (m *app) handleOverviewMsg(msg tea.Msg, cmds *[]tea.Cmd) bool {
	switch value := msg.(type) {
	case overviewRefreshMsg:
		var cmd tea.Cmd
		m.overview, cmd = m.overview.Update(value)
		*cmds = append(*cmds, cmd)
		return true
	case overviewSearchDebounceMsg:
		var cmd tea.Cmd
		m.overview, cmd = m.overview.Update(value)
		*cmds = append(*cmds, cmd)
		return true
	case overviewLoadRequestMsg:
		*cmds = append(*cmds, m.handleOverviewLoad(value))
		return true
	case overviewLoadResultMsg:
		if value.seq != m.overview.seq {
			return true
		}
		beforeID := m.selectedTaskID()
		beforeKey := m.selectedOverviewDetailKey()
		var cmd tea.Cmd
		m.overview, cmd = m.overview.Update(value)
		*cmds = append(*cmds, cmd)
		if value.err != nil {
			m.message = value.err.Error()
			return true
		}
		afterID := m.selectedTaskID()
		afterKey := m.selectedOverviewDetailKey()
		if afterID == "" {
			m.detailVM = executionViewModel{}
			m.detailContent = ""
			m.detail.SetContent("")
			return true
		}
		if afterID != beforeID || afterKey != beforeKey || value.refreshDetail {
			*cmds = append(*cmds, m.reloadDetail())
		}
		return true
	default:
		return false
	}
}

func (m *app) handleDetailMsg(msg tea.Msg, cmds *[]tea.Cmd) bool {
	switch value := msg.(type) {
	case detailMsg:
		if value.taskID != m.selectedTaskID() {
			return true
		}
		if value.err != nil {
			m.message = value.err.Error()
			return true
		}
		m.detailVM = value.vm
		m.syncStageSelection()
		m.syncRefSelection()
		m.mergeActiveStreamPreview()
		m.updateDetailContent(false)
		return true
	case runMsg:
		if value.err != nil {
			m.message = value.err.Error()
		} else {
			m.message = fmt.Sprintf("流水线完成 %s（%s）", value.result.Run.RunID, localizeRunStatus(value.result.Run.Status))
		}
		*cmds = append(*cmds, m.reload())
		return true
	default:
		return false
	}
}

func (m *app) handleSchedulerMsg(msg tea.Msg, cmds *[]tea.Cmd) bool {
	switch value := msg.(type) {
	case runSubmitMsg:
		if value.err != nil {
			m.message = value.err.Error()
		} else {
			m.message = fmt.Sprintf("已提交 job %s", value.jobID)
			m.pendingJob = value.jobID
			m.runConfig = runConfig{}
		}
		*cmds = append(*cmds, m.reloadSchedulerJobs())
		return true
	case taskCancelRequestMsg:
		if value.err != nil {
			m.message = fmt.Sprintf("终止失败 %s: %s", value.taskID, value.err)
		} else {
			m.pendingCancelJobID = value.jobID
			if m.pendingJob == value.jobID {
				m.pendingJob = ""
			}
			m.message = "已发送终止请求 " + value.taskID
		}
		*cmds = append(*cmds, m.reloadSchedulerJobs(), m.reload())
		return true
	case schedulerJobsMsg:
		beforeChrome := verticalChromeHeight(*m)
		m.activeJobs = value.jobs
		m.updatePendingCancelMessage(value.jobs)
		m.updatePendingJobMessage(value.jobs)
		if m.selectedJobIsTerminal(value.jobs) {
			*cmds = append(*cmds, m.reload())
		}
		streamChanged := m.mergeActiveStreamPreview()
		if verticalChromeHeight(*m) != beforeChrome {
			if cmd := m.applyLayout(); cmd != nil {
				*cmds = append(*cmds, cmd)
			}
		} else if streamChanged {
			m.updateDetailContent(false)
		}
		return true
	case schedulerNotifyMsg:
		*cmds = append(*cmds, m.reloadSchedulerJobs(), m.waitSchedulerNotify())
		return true
	default:
		return false
	}
}

func (m *app) handleRecoveryMsg(msg tea.Msg, cmds *[]tea.Cmd) bool {
	switch value := msg.(type) {
	case recoveryMsg:
		if value.err != nil {
			m.message = value.err.Error()
		}
		return true
	case tickMsg:
		now := time.Time(value)
		if now.Sub(m.lastRecoveryAt) >= staleRunRecoveryInterval {
			m.lastRecoveryAt = now
			*cmds = append(*cmds, m.recoverStaleRunsCmd())
		}
		if now.Sub(m.lastPersistedRefreshAt) >= persistedStateRefreshInterval {
			m.lastPersistedRefreshAt = now
			*cmds = append(*cmds, m.reloadOverview())
		}
		*cmds = append(*cmds, m.reloadSchedulerJobs(), m.tick())
		return true
	default:
		return false
	}
}

func (m *app) updatePendingJobMessage(jobs []scheduler.JobSnapshot) {
	if m.pendingJob == "" {
		return
	}
	if m.pendingJob == m.pendingCancelJobID {
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
	case scheduler.JobCancelled:
		m.message = fmt.Sprintf("job %s 已终止", job.JobID)
		m.pendingJob = ""
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

func (m *app) updatePendingCancelMessage(jobs []scheduler.JobSnapshot) {
	if m.pendingCancelJobID == "" {
		return
	}
	job, ok := findJobSnapshot(jobs, m.pendingCancelJobID)
	if !ok {
		return
	}
	switch job.State {
	case scheduler.JobQueued, scheduler.JobRunning:
		if job.CancelRequested {
			m.message = "正在终止 " + job.TaskID + " 的运行"
		}
	case scheduler.JobCancelled:
		m.message = "已终止 " + job.TaskID + " 的运行"
		m.pendingCancelJobID = ""
	case scheduler.JobFailed:
		if job.CancelRequested {
			m.message = "已终止 " + job.TaskID + " 的运行"
		} else {
			reason := strings.TrimSpace(job.Err)
			if reason == "" {
				reason = "未知错误"
			}
			m.message = "终止后 job 失败: " + reason
		}
		m.pendingCancelJobID = ""
	case scheduler.JobDone:
		m.message = "终止请求已发送，但 job 已完成"
		m.pendingCancelJobID = ""
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

func (m *app) selectedJobIsTerminal(jobs []scheduler.JobSnapshot) bool {
	taskID := m.selectedTaskID()
	if taskID == "" {
		return false
	}
	for _, job := range jobs {
		if job.TaskID == taskID && (job.State == scheduler.JobQueued || job.State == scheduler.JobRunning) {
			return false
		}
	}
	for _, job := range jobs {
		if job.TaskID != taskID || job.JobID == "" {
			continue
		}
		switch job.State {
		case scheduler.JobDone, scheduler.JobCancelled, scheduler.JobFailed:
			if m.lastTerminalRefreshJobID == job.JobID {
				return false
			}
			m.lastTerminalRefreshJobID = job.JobID
			return true
		}
	}
	return false
}

func (m app) View() string {
	var builder strings.Builder
	builder.WriteString(renderHeader(m))
	builder.WriteString("\n")
	if m.message != "" {
		builder.WriteString(messageStyle(m.message).Render(m.message))
		builder.WriteString("\n")
	}
	if m.confirmCancelTaskID != "" {
		prompt := fmt.Sprintf("确认终止 %s 的 %s？(y/n)", m.confirmCancelTaskID, m.confirmCancelJobID)
		builder.WriteString(errorStyle.Render(truncateDisplay(prompt, max(8, m.width-2))))
		builder.WriteString("\n")
	}
	builder.WriteString(renderPipelineBar(m))
	builder.WriteString("\n")
	if m.runConfig.active {
		builder.WriteString(renderRunConfig(m))
	} else if m.tab == panelOverview {
		builder.WriteString(renderOverview(m))
	} else if m.tab == panelSettings {
		builder.WriteString(renderSettings(m))
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

func (m app) reloadOverview() tea.Cmd {
	return overviewRefreshCmd(true, false)
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

func (m app) recoverStaleRunsCmd() tea.Cmd {
	if m.recoverStaleRunsFn == nil {
		return nil
	}
	return func() tea.Msg {
		return recoveryMsg{err: m.recoverStaleRunsFn(context.Background())}
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

func cancelTaskCmd(s schedulerClient, taskID, jobID string) tea.Cmd {
	return func() tea.Msg {
		if s == nil {
			return taskCancelRequestMsg{taskID: taskID, jobID: jobID, err: fmt.Errorf("scheduler unavailable")}
		}
		err := s.CancelTask(taskID)
		return taskCancelRequestMsg{taskID: taskID, jobID: jobID, err: err}
	}
}

func (m app) reloadSchedulerJobs() tea.Cmd {
	if m.scheduler == nil {
		return nil
	}
	return func() tea.Msg {
		return schedulerJobsMsg{jobs: m.scheduler.ActiveSnapshot()}
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
	layout := layoutFor(m.width, max(8, m.height-verticalChromeHeight(*m)), m.tab == panelExecution)
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

func (m app) activeJobForTask(taskID string) (scheduler.JobSnapshot, bool) {
	for _, job := range m.activeJobs {
		if job.TaskID != taskID {
			continue
		}
		if job.State == scheduler.JobQueued || job.State == scheduler.JobRunning {
			return job, true
		}
	}
	return scheduler.JobSnapshot{}, false
}

func (m app) streamJobForTask(taskID string) (scheduler.JobSnapshot, bool) {
	currentRunID := strings.TrimSpace(m.detailVM.Run.RunID)
	matchesCurrentRun := func(job scheduler.JobSnapshot) bool {
		return currentRunID == "" || job.RunID == "" || job.RunID == currentRunID
	}
	if m.pendingJob != "" {
		for _, job := range m.activeJobs {
			if job.JobID == m.pendingJob && job.TaskID == taskID {
				return job, true
			}
		}
	}
	if job, ok := m.activeJobForTask(taskID); ok && matchesCurrentRun(job) {
		return job, true
	}
	for index := len(m.activeJobs) - 1; index >= 0; index-- {
		job := m.activeJobs[index]
		if job.TaskID == taskID && len(job.StreamByStage) > 0 && matchesCurrentRun(job) {
			return job, true
		}
	}
	return scheduler.JobSnapshot{}, false
}

func (m *app) mergeActiveStreamPreview() bool {
	if m.detailVM.StreamByStage == nil {
		m.detailVM.StreamByStage = map[string]pipeline.StreamUpdate{}
	}
	changed := false
	job, ok := m.streamJobForTask(m.selectedTaskID())
	if ok {
		if m.mergeActiveStagePreview(job.Stages) {
			changed = true
		}
	}
	for stage := range m.detailVM.StreamByStage {
		sv := stageForKey(m.detailVM.Stages, stage)
		if sv.Status != model.StageRunning {
			delete(m.detailVM.StreamByStage, stage)
			changed = true
		}
	}
	if ok && len(job.StreamByStage) > 0 {
		for stage, update := range job.StreamByStage {
			sv := stageForKey(m.detailVM.Stages, stage)
			if sv.Status != model.StageRunning {
				continue
			}
			if _, exists := m.detailVM.StreamByStage[stage]; !exists && stage == m.selectedStageKey {
				m.detailFollowTail = true
			}
			if !streamUpdateEqual(m.detailVM.StreamByStage[stage], update) {
				m.detailVM.StreamByStage[stage] = update
				changed = true
			}
		}
	}
	return changed
}

func (m *app) mergeActiveStagePreview(stages []model.StageRecord) bool {
	if len(stages) == 0 {
		return false
	}
	changed := false
	if len(m.detailVM.Stages) == 0 {
		m.detailVM.Stages = normalizeStageViews(stages)
		m.syncStageSelection()
		return true
	}
	byStage := map[string]model.StageRecord{}
	for _, stage := range stages {
		if stage.Stage != "" {
			byStage[stage.Stage] = stage
		}
	}
	for index := range m.detailVM.Stages {
		next, ok := byStage[m.detailVM.Stages[index].Stage]
		if !ok {
			continue
		}
		if next.Name == "" {
			next.Name = m.detailVM.Stages[index].Name
		}
		if stageRecordEqual(m.detailVM.Stages[index].StageRecord, next) {
			continue
		}
		m.detailVM.Stages[index] = stageView{
			StageRecord: next,
			DisplayName: localizeStageName(next.Stage, next.Name),
		}
		changed = true
	}
	if changed {
		m.syncStageSelection()
	}
	return changed
}

func stageRecordEqual(left, right model.StageRecord) bool {
	return left.Stage == right.Stage &&
		left.Name == right.Name &&
		left.Status == right.Status &&
		left.StartedAt == right.StartedAt &&
		left.FinishedAt == right.FinishedAt &&
		left.DurationMS == right.DurationMS &&
		left.LogPath == right.LogPath &&
		left.ErrorSummary == right.ErrorSummary &&
		stringSlicesEqual(left.BlockedBy, right.BlockedBy) &&
		stringSlicesEqual(left.ArtifactPaths, right.ArtifactPaths) &&
		artifactWarningsEqual(left.ArtifactWarnings, right.ArtifactWarnings)
}

func stringSlicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func artifactWarningsEqual(left, right []model.ArtifactWarning) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func streamUpdateEqual(left, right pipeline.StreamUpdate) bool {
	if left.Stage != right.Stage ||
		left.Mode != right.Mode ||
		left.ItemID != right.ItemID ||
		left.Text != right.Text ||
		left.Delta != right.Delta ||
		left.Source != right.Source ||
		left.Done != right.Done ||
		left.Truncated != right.Truncated ||
		len(left.Lines) != len(right.Lines) {
		return false
	}
	for index := range left.Lines {
		if left.Lines[index] != right.Lines[index] {
			return false
		}
	}
	return true
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
	m.detailFollowTail = true
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
	content := buildDetailContent(m.detailVM, m.selectedStageKey, width, m.detail.Height)
	if content == m.detailContent {
		if resetScroll {
			m.detail.GotoTop()
		}
		if m.detailFollowTail && m.currentStageHasRunningStream() {
			m.detail.GotoTop()
		}
		return
	}
	m.detailContent = content
	m.detail.SetContent(content)
	if resetScroll {
		m.detail.GotoTop()
	}
	if m.detailFollowTail && m.currentStageHasRunningStream() {
		m.detail.GotoTop()
	}
}

func (m app) currentStageHasRunningStream() bool {
	stage := m.selectedStage()
	if stage.Status != model.StageRunning {
		return false
	}
	stream, ok := m.detailVM.StreamByStage[stage.Stage]
	return ok && stream.Stage != ""
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
