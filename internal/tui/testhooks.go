package tui

import (
	"context"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/db"
	"github.com/xuanli520/p2r_tui/internal/pipeline"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/scheduler"
)

// TestHarness exposes narrow state-machine probes for the external tests under
// tests/internal/tui. The package is internal, so these hooks do not form a
// public API for callers outside this module tree.
type TestHarness struct {
	model app
}

type TestKeyResult struct {
	Quit     bool
	CmdCount int
}

type TestExecutionProbe struct {
	StageCount         int
	FirstStageError    string
	DocsManifestExists bool
	CleanupStatus      string
	CleanupText        string
	DetailContent      string
	RefRunIDs          []string
}

type TestStagePlan struct {
	RunStages     []string
	DisplayStages []string
	BlockedReason string
}

type TestOverviewColumn struct {
	Key   string
	Title string
	Width int
}

func NewTestHarness(cfg config.Config) TestHarness {
	m := newApp(nil, cfg)
	m.width = 120
	m.height = 30
	m.applyLayout()
	return TestHarness{model: m}
}

func NewTestHarnessWithStore(store *db.Store, cfg config.Config) TestHarness {
	m := newApp(store, cfg)
	m.width = 120
	m.height = 30
	m.applyLayout()
	return TestHarness{model: m}
}

func (h TestHarness) Press(key string) (TestHarness, TestKeyResult) {
	next, cmds := h.model.handleKey(testKeyMsg(key))
	result := TestKeyResult{CmdCount: len(cmds)}
	if key == "ctrl+c" || key == "ctrl+q" {
		result.Quit = len(cmds) > 0
	}
	return TestHarness{model: next}, result
}

func (h TestHarness) ApplyProjectReloadForTest() (TestHarness, bool) {
	h.model.overview.seq++
	next, cmd := h.model.Update(overviewLoadResultMsg{
		seq:           h.model.overview.seq,
		query:         h.model.overview.projectQuery(),
		cursorIntent:  cursorKeep,
		items:         h.model.overview.items,
		total:         h.model.overview.page.total,
		refreshDetail: true,
	})
	model, ok := next.(app)
	if !ok {
		return h, cmd != nil
	}
	return TestHarness{model: model}, cmd != nil
}

func (h TestHarness) ApplyRunSubmitForTest(jobID string) TestHarness {
	next, _ := h.model.Update(runSubmitMsg{jobID: jobID})
	model, ok := next.(app)
	if !ok {
		return h
	}
	return TestHarness{model: model}
}

func (h TestHarness) ApplyCancelRequestForTest(taskID, jobID string, err error) TestHarness {
	next, _ := h.model.Update(taskCancelRequestMsg{taskID: taskID, jobID: jobID, err: err})
	model, ok := next.(app)
	if !ok {
		return h
	}
	return TestHarness{model: model}
}

func (h TestHarness) ApplySchedulerJobsForTest(jobs []scheduler.JobSnapshot) TestHarness {
	next, _ := h.model.Update(schedulerJobsMsg{jobs: jobs})
	model, ok := next.(app)
	if !ok {
		return h
	}
	return TestHarness{model: model}
}

func (h TestHarness) SetFocus(name string) TestHarness {
	h.model.setFocus(testFocusArea(name))
	return h
}

func (h TestHarness) SetExecutionPanel() TestHarness {
	h.model.tab = panelExecution
	h.model.setFocus(focusStageList)
	return h
}

func (h TestHarness) SetSize(width, height int) TestHarness {
	h.model.width = width
	h.model.height = height
	h.model.applyLayout()
	return h
}

func (h TestHarness) SeedOverview(taskIDs ...string) TestHarness {
	h.model.overview.items = nil
	for _, taskID := range taskIDs {
		item := overviewItem{TaskID: taskID, RunStatus: model.RunCompletedClean}
		item.SearchText = overviewSearchText(item)
		h.model.overview.items = append(h.model.overview.items, item)
	}
	h.model.overview.page.total = len(h.model.overview.items)
	h.model.overview.page.current = 1
	h.model.overview.refreshTable(cursorKeep)
	return h
}

func (h TestHarness) SeedStages(stages []model.StageRecord, selected string) TestHarness {
	h.model.detailVM.Stages = normalizeStageViews(stages)
	h.model.selectedStageKey = selected
	h.model.syncStageSelection()
	return h
}

func (h TestHarness) SeedRefRuns(runIDs ...string) TestHarness {
	h.model.qaMode = "recheck"
	return h.SeedRefRunsForCurrentMode(runIDs...)
}

func (h TestHarness) SeedRefRunsForCurrentMode(runIDs ...string) TestHarness {
	h.model.detailVM.RefRuns = nil
	for _, runID := range runIDs {
		h.model.detailVM.RefRuns = append(h.model.detailVM.RefRuns, model.RunRecord{RunID: runID})
	}
	h.model.syncRefSelection()
	return h
}

func (h TestHarness) SetDetailContent(width, height int, content string) TestHarness {
	h.model.detail.Width = width
	h.model.detail.Height = height
	h.model.detail.SetContent(content)
	return h
}

func (h TestHarness) SeedExecutionDetail(taskID string) TestHarness {
	h = h.SeedOverview(taskID)
	h.model.tab = panelExecution
	h.model.detailVM = executionViewModel{
		TaskID:       taskID,
		HasRun:       true,
		Run:          model.RunRecord{RunID: "run-1", TaskID: taskID, Status: model.RunCompletedWithFindings},
		ArtifactRoot: "/tmp/artifacts",
		Stages: normalizeStageViews([]model.StageRecord{
			{Stage: "A", Status: model.StageDone},
			{Stage: "B", Status: model.StageFailed, ErrorSummary: "docker compose up failed"},
		}),
		DocsSummary: docsSummary{ManifestPath: "/tmp/manifest.json"},
	}
	h.model.syncStageSelection()
	h.model.updateDetailContent(true)
	h.model.setFocus(focusStageList)
	return h
}

func (h TestHarness) SeedExecutionRun(taskID, runID string, stages []model.StageRecord, selected string) TestHarness {
	h = h.SeedOverview(taskID)
	h.model.tab = panelExecution
	h.model.detailVM = executionViewModel{
		TaskID:                taskID,
		HasRun:                true,
		Run:                   model.RunRecord{RunID: runID, TaskID: taskID, Status: model.RunRunning},
		ArtifactRoot:          "/tmp/artifacts",
		Stages:                normalizeStageViews(stages),
		DocsSummary:           docsSummary{ManifestPath: "/tmp/manifest.json"},
		LogTailByStage:        map[string]string{},
		GuidanceEventsByStage: map[string][]string{},
		StreamByStage:         map[string]pipeline.StreamUpdate{},
	}
	h.model.selectedStageKey = selected
	h.model.syncStageSelection()
	h.model.updateDetailContent(true)
	h.model.setFocus(focusStageList)
	return h
}

func (h TestHarness) View() string {
	return h.model.View()
}

func (h TestHarness) SearchValue() string {
	return h.model.overview.search.Value()
}

func (h TestHarness) VisibleCount() int {
	return len(h.model.overview.items)
}

func (h TestHarness) OverviewSeq() uint64 {
	return h.model.overview.seq
}

func (h TestHarness) SearchSeq() uint64 {
	return h.model.overview.searchSeq
}

func (h TestHarness) PageCurrent() int {
	return h.model.overview.page.current
}

func (h TestHarness) PageSize() int {
	return normalizePageSize(h.model.overview.page.size)
}

func (h TestHarness) SortName() string {
	switch h.model.overview.sortMode {
	case sortByStatus:
		return "status"
	case sortBySeverity:
		return "severity"
	case sortByLastRun:
		return "last_run"
	case sortByVerdict:
		return "verdict"
	default:
		return "task_id"
	}
}

func (h TestHarness) SortAsc() bool {
	return h.model.overview.sortAsc
}

func (h TestHarness) SetOverviewPage(current, size, total int) TestHarness {
	h.model.overview.page.current = current
	h.model.overview.page.size = size
	h.model.overview.page.total = total
	h.model.overview.refreshTable(cursorKeep)
	return h
}

func (h TestHarness) SetOverviewCursor(index int) TestHarness {
	h.model.overview.table.SetCursor(index)
	h.model.overview.syncSelectedFromCursor()
	return h
}

func (h TestHarness) ApplySearchDebounceForTest(searchSeq uint64, text string) (TestHarness, bool) {
	next, cmd := h.model.Update(overviewSearchDebounceMsg{searchSeq: searchSeq, text: text})
	model, ok := next.(app)
	if !ok {
		return h, cmd != nil
	}
	return TestHarness{model: model}, cmd != nil
}

func (h TestHarness) ApplyOverviewRefreshForTest() (TestHarness, bool) {
	next, cmd := h.model.Update(overviewRefreshMsg{silent: true})
	model, ok := next.(app)
	if !ok {
		return h, cmd != nil
	}
	return TestHarness{model: model}, cmd != nil
}

func (h TestHarness) ApplyOverviewResultForTest(seq uint64, total int, taskIDs ...string) (TestHarness, bool) {
	items := make([]overviewItem, 0, len(taskIDs))
	for _, taskID := range taskIDs {
		item := overviewItem{TaskID: taskID, RunStatus: model.RunCompletedClean}
		item.SearchText = overviewSearchText(item)
		items = append(items, item)
	}
	next, cmd := h.model.Update(overviewLoadResultMsg{
		seq:          seq,
		query:        h.model.overview.projectQuery(),
		cursorIntent: cursorKeep,
		items:        items,
		total:        total,
	})
	model, ok := next.(app)
	if !ok {
		return h, cmd != nil
	}
	return TestHarness{model: model}, cmd != nil
}

func (h TestHarness) Mode() string {
	return h.model.qaMode
}

func (h TestHarness) Confirm() bool {
	return h.model.runConfig.active
}

func (h TestHarness) CancelConfirm() bool {
	return h.model.confirmCancelTaskID != ""
}

func (h TestHarness) Running() bool {
	if h.model.message == "正在提交流水线 job..." {
		return true
	}
	for _, job := range activeJobSnapshots(h.model.activeJobs) {
		if job.State.String() == "running" || job.State.String() == "queued" {
			return true
		}
	}
	return false
}

func (h TestHarness) Message() string {
	return h.model.message
}

func (h TestHarness) SelectedTaskID() string {
	return h.model.selectedTaskID()
}

func (h TestHarness) SelectedStageKey() string {
	return h.model.selectedStageKey
}

func (h TestHarness) SelectedRefRun() string {
	return h.model.selectedRefRun()
}

func (h TestHarness) DetailYOffset() int {
	return h.model.detail.YOffset
}

func (h TestHarness) DetailFollowTail() bool {
	return h.model.detailFollowTail
}

func (h TestHarness) TabName() string {
	if h.model.tab == panelExecution {
		return "execution"
	}
	return "overview"
}

func (h TestHarness) FocusName() string {
	return h.model.focus.String()
}

func (h TestHarness) SetSelectedTaskForRefresh(taskID string) TestHarness {
	h.model.overview.selectedID = taskID
	return h
}

func (h TestHarness) ReplaceOverviewForRefresh(taskIDs ...string) TestHarness {
	h.model.overview.items = nil
	for _, taskID := range taskIDs {
		item := overviewItem{TaskID: taskID, RunStatus: model.RunCompletedClean}
		item.SearchText = overviewSearchText(item)
		h.model.overview.items = append(h.model.overview.items, item)
	}
	h.model.overview.page.total = len(h.model.overview.items)
	h.model.overview.refreshTable(cursorKeep)
	return h
}

func OverviewColumnTitlesForTest(width int) []string {
	columns := buildOverviewColumns(width, sortByTaskID, true)
	titles := make([]string, 0, len(columns))
	for _, column := range columns {
		titles = append(titles, strings.TrimRight(column.Title, "↑↓"))
	}
	return titles
}

func OverviewColumnsForTest(width int) []TestOverviewColumn {
	specs := overviewColumnSpecs(width, sortByTaskID, true)
	columns := make([]TestOverviewColumn, 0, len(specs))
	for _, spec := range specs {
		columns = append(columns, TestOverviewColumn{Key: spec.Key, Title: spec.Title, Width: spec.Width})
	}
	return columns
}

func OverviewRowForTest(lastRun string, width int) []string {
	specs := overviewColumnSpecs(width, sortByTaskID, true)
	row := overviewDisplayRow(overviewItem{
		TaskID:        "TASK-1",
		Batch:         "batch-1",
		RunStatus:     model.RunCompletedClean,
		ManualVerdict: model.ManualUnset,
		LastRun:       lastRun,
		Mode:          "initial",
	}, specs)
	return []string(row)
}

func ShortTimeForTest(value string) string {
	return shortTime(value)
}

func LocalizeRunStatusForTest(status string) string {
	return localizeRunStatus(status)
}

func LocalizeStageStatusForTest(status string) string {
	return localizeStageStatus(status)
}

func LocalizeManualVerdictForTest(verdict string) string {
	return localizeManualVerdict(verdict)
}

func LocalizeSeverityForTest(severity string) string {
	return localizeSeverity(severity)
}

func LocalizeStageNameForTest(stage, name string) string {
	return localizeStageName(stage, name)
}

func LocalizeCleanupStatusForTest(status string) string {
	return localizeCleanupStatus(status)
}

func LocalizeSummaryForTest(summary string) string {
	return localizeSummary(summary)
}

func StageLogPreviewForTest(path string, maxLines int) string {
	return stageLogPreview(path, maxLines)
}

func ExecutionLayoutModeForTest(width, height int) string {
	switch layoutFor(width, height, true).mode {
	case layoutWide:
		return "wide"
	case layoutMedium:
		return "medium"
	case layoutStacked:
		return "stacked"
	case layoutMinimal:
		return "minimal"
	default:
		return "unknown"
	}
}

func TruncateDisplayForTest(value string, width int) string {
	return truncateDisplay(value, width)
}

func StagePlanForTest(mode, stage string, staticOnly bool) TestStagePlan {
	plan := stagePlanForMode(mode, stage, staticOnly, nil, "")
	result := TestStagePlan{
		BlockedReason: plan.blockedReason,
	}
	if plan.runStages != nil {
		result.RunStages = append([]string{}, plan.runStages...)
	}
	if plan.displayStages != nil {
		result.DisplayStages = append([]string{}, plan.displayStages...)
	}
	return result
}

func StagePlanWithOptionsForTest(mode, stage string, staticOnly bool, selected []string, fromStage string) TestStagePlan {
	var selectedMap map[string]bool
	if len(selected) > 0 {
		selectedMap = map[string]bool{}
		for _, stage := range selected {
			selectedMap[stage] = true
		}
	}
	plan := stagePlanForMode(mode, stage, staticOnly, selectedMap, fromStage)
	result := TestStagePlan{
		BlockedReason: plan.blockedReason,
	}
	if plan.runStages != nil {
		result.RunStages = append([]string{}, plan.runStages...)
	}
	if plan.displayStages != nil {
		result.DisplayStages = append([]string{}, plan.displayStages...)
	}
	return result
}

func FooterForTest(focus string, confirm bool) string {
	m := newApp(nil, config.Default())
	if confirm {
		m.runConfig = newRunConfig("TASK-1", "initial", "", "A", false, 0)
	}
	m.setFocus(testFocusArea(focus))
	return footerFor(m)
}

func CancelFooterForTest() string {
	m := newApp(nil, config.Default())
	m.confirmCancelTaskID = "TASK-1"
	m.confirmCancelJobID = "job-1"
	return footerFor(m)
}

func BuildExecutionProbeForTest(ctx context.Context, store *db.Store, cfg config.Config, taskID, selectedStage string, width int) (TestExecutionProbe, error) {
	vm, err := buildExecutionViewModel(ctx, store, cfg, taskID)
	if err != nil {
		return TestExecutionProbe{}, err
	}
	probe := TestExecutionProbe{
		StageCount:         len(vm.Stages),
		DocsManifestExists: vm.DocsSummary.ManifestExists,
		CleanupStatus:      vm.CleanupStatus,
		CleanupText:        vm.CleanupText,
		DetailContent:      buildDetailContent(vm, selectedStage, width, 24),
	}
	for _, run := range vm.RefRuns {
		probe.RefRunIDs = append(probe.RefRunIDs, run.RunID)
	}
	if len(vm.Stages) > 0 {
		probe.FirstStageError = vm.Stages[0].ErrorSummary
	}
	return probe, nil
}

func testKeyMsg(key string) tea.KeyMsg {
	switch key {
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	case "ctrl+q":
		return tea.KeyMsg{Type: tea.KeyCtrlQ}
	case "ctrl+r":
		return tea.KeyMsg{Type: tea.KeyCtrlR}
	case "ctrl+x":
		return tea.KeyMsg{Type: tea.KeyCtrlX}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "shift+tab":
		return tea.KeyMsg{Type: tea.KeyShiftTab}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "left":
		return tea.KeyMsg{Type: tea.KeyLeft}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	case "pgup":
		return tea.KeyMsg{Type: tea.KeyPgUp}
	case "pgdown":
		return tea.KeyMsg{Type: tea.KeyPgDown}
	case "home":
		return tea.KeyMsg{Type: tea.KeyHome}
	case "end":
		return tea.KeyMsg{Type: tea.KeyEnd}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	}
}

func testFocusArea(name string) focusArea {
	switch name {
	case "search":
		return focusSearch
	case "overview-table":
		return focusOverviewTable
	case "stage-list":
		return focusStageList
	case "ref-run-list":
		return focusRefRunList
	case "detail-viewport":
		return focusDetailViewport
	default:
		return focusSearch
	}
}
