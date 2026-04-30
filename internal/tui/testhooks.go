package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/db"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
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
	next, cmd := h.model.Update(projectsMsg{
		projects: h.model.projects,
		items:    h.model.overviewItems,
	})
	model, ok := next.(app)
	if !ok {
		return h, cmd != nil
	}
	return TestHarness{model: model}, cmd != nil
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
	h.model.overviewItems = nil
	for _, taskID := range taskIDs {
		item := overviewItem{TaskID: taskID, RunStatus: model.RunCompletedClean}
		item.SearchText = overviewSearchText(item)
		h.model.overviewItems = append(h.model.overviewItems, item)
	}
	h.model.refreshRows()
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

func (h TestHarness) View() string {
	return h.model.View()
}

func (h TestHarness) SearchValue() string {
	return h.model.search.Value()
}

func (h TestHarness) VisibleCount() int {
	return len(h.model.visibleRows)
}

func (h TestHarness) Mode() string {
	return h.model.qaMode
}

func (h TestHarness) Confirm() bool {
	return h.model.confirm
}

func (h TestHarness) Running() bool {
	return h.model.running
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
	h.model.selectedTaskIDValue = taskID
	return h
}

func (h TestHarness) ReplaceOverviewForRefresh(taskIDs ...string) TestHarness {
	h.model.overviewItems = nil
	for _, taskID := range taskIDs {
		item := overviewItem{TaskID: taskID, RunStatus: model.RunCompletedClean}
		item.SearchText = overviewSearchText(item)
		h.model.overviewItems = append(h.model.overviewItems, item)
	}
	h.model.refreshRows()
	return h
}

func OverviewColumnTitlesForTest(width int) []string {
	columns := buildOverviewColumns(width)
	titles := make([]string, 0, len(columns))
	for _, column := range columns {
		titles = append(titles, column.Title)
	}
	return titles
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

func FooterForTest(focus string, confirm bool) string {
	m := newApp(nil, config.Default())
	m.confirm = confirm
	m.setFocus(testFocusArea(focus))
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
		DetailContent:      buildDetailContent(vm, selectedStage, width),
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
	case "ctrl+a":
		return tea.KeyMsg{Type: tea.KeyCtrlA}
	case "ctrl+m":
		return tea.KeyMsg{Type: tea.KeyCtrlM}
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
