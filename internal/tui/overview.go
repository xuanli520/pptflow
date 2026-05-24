package tui

import (
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/xuanli520/p2r_tui/internal/db"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
)

const overviewSearchDebounce = 120 * time.Millisecond

type overviewSortMode int

const (
	sortByTaskID overviewSortMode = iota
	sortByStatus
	sortBySeverity
	sortByLastRun
	sortByVerdict
	sortByCompletionCount
)

type PageState struct {
	current  int
	size     int
	total    int
	autoSize bool
}

type OverviewModel struct {
	search   textinput.Model
	table    table.Model
	sortMode overviewSortMode
	sortAsc  bool
	page     PageState

	items      []overviewItem
	selectedID string

	loading bool
	err     error
	seq     uint64

	searchSeq    uint64
	loadInFlight bool

	cursorIntent overviewCursorIntent
	width        int
	height       int
}

var _ Page = (*OverviewModel)(nil)

type overviewLoadRequestMsg struct {
	seq           uint64
	query         db.ProjectQuery
	cursorIntent  overviewCursorIntent
	silent        bool
	refreshDetail bool
}

type overviewLoadResultMsg struct {
	seq           uint64
	query         db.ProjectQuery
	cursorIntent  overviewCursorIntent
	items         []overviewItem
	total         int
	refreshDetail bool
	err           error
}

type overviewRefreshMsg struct {
	silent        bool
	refreshDetail bool
}

type overviewSearchDebounceMsg struct {
	searchSeq uint64
	text      string
}

type overviewCursorIntent int

const (
	cursorKeep overviewCursorIntent = iota
	cursorFirst
	cursorLast
)

func newOverviewModel() OverviewModel {
	search := textinput.New()
	search.Placeholder = "搜索任务ID、批次、路径、状态、判定或阶段..."
	search.Prompt = "搜索: "
	search.Focus()

	t := table.New(
		table.WithColumns(buildOverviewColumns(120, sortByTaskID, true)),
		table.WithFocused(false),
		table.WithHeight(12),
	)
	t.SetStyles(tableStyles())

	m := OverviewModel{
		search:   search,
		table:    t,
		sortMode: sortByTaskID,
		sortAsc:  true,
		page: PageState{
			current:  1,
			size:     20,
			autoSize: true,
		},
		width:  120,
		height: 12,
	}
	m.refreshTable(cursorFirst)
	return m
}

func (m OverviewModel) Init() tea.Cmd {
	return overviewRefreshCmd(false, true)
}

func (m OverviewModel) Apply(msg tea.Msg) (OverviewModel, tea.Cmd) {
	next, cmd, _ := m.apply(msg)
	return next, cmd
}

func (m *OverviewModel) Update(msg tea.Msg) (bool, tea.Cmd) {
	if m == nil {
		return false, nil
	}
	next, cmd, handled := m.apply(msg)
	*m = next
	return handled, cmd
}

func (m OverviewModel) apply(msg tea.Msg) (OverviewModel, tea.Cmd, bool) {
	switch value := msg.(type) {
	case overviewRefreshMsg:
		return m, m.requestLoad(value.silent, cursorKeep, value.refreshDetail), true
	case overviewSearchDebounceMsg:
		if value.searchSeq != m.searchSeq || value.text != m.search.Value() {
			return m, nil, true
		}
		m.page.current = 1
		return m, m.requestLoad(false, cursorFirst, false), true
	case overviewLoadResultMsg:
		if value.seq != m.seq {
			return m, nil, true
		}
		m.loadInFlight = false
		m.loading = false
		if value.err != nil {
			m.err = value.err
			return m, nil, true
		}
		m.err = nil
		m.page.total = max(0, value.total)
		m.page.current = max(1, m.page.current)
		if m.page.current > m.page.totalPages() {
			m.page.current = m.page.totalPages()
			return m, m.requestLoad(true, cursorKeep, value.refreshDetail), true
		}
		m.items = append([]overviewItem{}, value.items...)
		m.refreshTable(value.cursorIntent)
		return m, nil, true
	case tea.KeyMsg:
		next, cmd := m.updateKey(value)
		return next, cmd, true
	default:
		return m, nil, false
	}
}

func (m OverviewModel) updateKey(msg tea.KeyMsg) (OverviewModel, tea.Cmd) {
	key := msg.String()
	if m.search.Focused() {
		var cmd tea.Cmd
		m.search, cmd = m.search.Update(msg)
		m.searchSeq++
		seq := m.searchSeq
		text := m.search.Value()
		return m, tea.Batch(cmd, tea.Tick(overviewSearchDebounce, func(time.Time) tea.Msg {
			return overviewSearchDebounceMsg{searchSeq: seq, text: text}
		}))
	}
	if !m.table.Focused() {
		return m, nil
	}

	switch key {
	case "s":
		m.cycleSortMode()
		m.page.current = 1
		return m, m.requestLoad(false, cursorFirst, false)
	case "S":
		m.sortAsc = !m.sortAsc
		m.page.current = 1
		return m, m.requestLoad(false, cursorFirst, false)
	case "z":
		m.page.autoSize = false
		m.page.size = nextPageSize(m.page.size)
		m.page.current = 1
		return m, m.requestLoad(false, cursorFirst, false)
	case "pgdown":
		if m.page.current >= m.page.totalPages() {
			return m, nil
		}
		m.page.current++
		return m, m.requestLoad(false, cursorFirst, false)
	case "pgup":
		if m.page.current <= 1 {
			return m, nil
		}
		m.page.current--
		return m, m.requestLoad(false, cursorLast, false)
	case "up":
		if m.table.Cursor() <= 0 {
			if m.page.current <= 1 {
				return m, nil
			}
			m.page.current--
			return m, m.requestLoad(false, cursorLast, false)
		}
	case "down":
		if len(m.items) == 0 || m.table.Cursor() < len(m.items)-1 {
			break
		}
		if m.page.current >= m.page.totalPages() {
			return m, nil
		}
		m.page.current++
		return m, m.requestLoad(false, cursorFirst, false)
	}

	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	m.syncSelectedFromCursor()
	return m, cmd
}

func (m *OverviewModel) confirmSearch() tea.Cmd {
	m.searchSeq++
	m.page.current = 1
	return m.requestLoad(false, cursorFirst, false)
}

func (m *OverviewModel) SetFocus(area focusArea) {
	if area == focusSearch {
		m.search.Focus()
	} else {
		m.search.Blur()
	}
	if area == focusOverviewTable {
		m.table.Focus()
	} else {
		m.table.Blur()
	}
}

func (m *OverviewModel) Focus() {
	if m == nil {
		return
	}
	m.SetFocus(focusOverviewTable)
}

func (m *OverviewModel) Blur() {
	if m == nil {
		return
	}
	m.search.Blur()
	m.table.Blur()
}

func (m *OverviewModel) HandleKey(msg tea.KeyMsg) tea.Cmd {
	if m == nil {
		return nil
	}
	_, cmd := m.Update(msg)
	return cmd
}

func (m *OverviewModel) Destroy() tea.Cmd {
	return nil
}

func (m *OverviewModel) SetSize(width, height int) bool {
	if width <= 0 {
		width = 120
	}
	if height <= 0 {
		height = 12
	}
	m.width = width
	m.height = height
	m.search.Width = max(12, width-8)
	m.table.SetWidth(width)
	m.table.SetHeight(height)

	changedPageSize := false
	if m.page.autoSize {
		next := computePageSize(height+11, true, m.page.size)
		if next != normalizePageSize(m.page.size) {
			m.page.size = next
			m.page.current = 1
			changedPageSize = true
		}
	}
	m.refreshTable(cursorKeep)
	return changedPageSize
}

func (m OverviewModel) SelectedTaskID() string {
	return m.selectedID
}

func (m OverviewModel) SelectedItem() (overviewItem, bool) {
	return m.ItemByTaskID(m.selectedID)
}

func (m OverviewModel) ItemByTaskID(taskID string) (overviewItem, bool) {
	for _, item := range m.items {
		if item.TaskID == taskID {
			return item, true
		}
	}
	return overviewItem{}, false
}

func (m *OverviewModel) View(width, height int) string {
	if m == nil {
		return ""
	}
	if width > 0 || height > 0 {
		m.SetSize(width, height)
	}
	return m.Render()
}

func (m OverviewModel) Render() string {
	var builder strings.Builder
	builder.WriteString(m.search.View())
	builder.WriteString("\n\n")
	if len(m.items) == 0 {
		message := "未选择已索引的项目\n请先执行 `p2r scan --path <projects-qa>`"
		if strings.TrimSpace(m.search.Value()) != "" {
			message = "没有匹配的项目"
		}
		if m.loading {
			message = "加载中..."
		}
		builder.WriteString(mutedStyle.Render(message))
	} else {
		builder.WriteString(m.table.View())
	}
	builder.WriteString("\n")
	builder.WriteString(mutedStyle.Render(m.paginationBar()))
	return builder.String()
}

func (m *OverviewModel) requestLoad(silent bool, intent overviewCursorIntent, refreshDetail bool) tea.Cmd {
	if silent && m.loadInFlight {
		return nil
	}
	m.seq++
	m.loading = !silent
	m.loadInFlight = true
	m.cursorIntent = intent
	req := overviewLoadRequestMsg{
		seq:           m.seq,
		query:         m.projectQuery(),
		cursorIntent:  intent,
		silent:        silent,
		refreshDetail: refreshDetail,
	}
	return overviewLoadCmd(req)
}

func (m OverviewModel) projectQuery() db.ProjectQuery {
	return db.ProjectQuery{
		Sort:   m.dbSort(),
		Asc:    m.sortAsc,
		Search: overviewSearchFromText(m.search.Value()),
		Limit:  normalizePageSize(m.page.size),
		Offset: m.page.offset(),
	}
}

func (m OverviewModel) dbSort() db.ProjectSort {
	switch m.sortMode {
	case sortByStatus:
		return db.ProjectSortStatus
	case sortBySeverity:
		return db.ProjectSortSeverity
	case sortByLastRun:
		return db.ProjectSortLastRun
	case sortByVerdict:
		return db.ProjectSortVerdict
	case sortByCompletionCount:
		return db.ProjectSortCompletionCount
	default:
		return db.ProjectSortTaskID
	}
}

func (m *OverviewModel) cycleSortMode() {
	m.sortMode = (m.sortMode + 1) % 6
	switch m.sortMode {
	case sortByTaskID:
		m.sortAsc = true
	default:
		m.sortAsc = false
	}
	m.refreshTable(cursorKeep)
}

func (m *OverviewModel) refreshTable(intent overviewCursorIntent) {
	specs := overviewColumnSpecs(m.width, m.sortMode, m.sortAsc)
	m.table.SetRows(nil)
	m.table.SetColumns(columnsFromSpecs(specs))
	rows := make([]table.Row, 0, len(m.items))
	for _, item := range m.items {
		rows = append(rows, overviewDisplayRow(item, specs))
	}
	m.table.SetRows(rows)

	if len(m.items) == 0 {
		m.selectedID = ""
		m.table.SetCursor(0)
		return
	}
	switch intent {
	case cursorLast:
		m.table.SetCursor(len(m.items) - 1)
		m.selectedID = m.items[len(m.items)-1].TaskID
	case cursorFirst:
		m.table.SetCursor(0)
		m.selectedID = m.items[0].TaskID
	default:
		if m.selectedID != "" {
			if index := overviewIndex(m.items, m.selectedID); index >= 0 {
				m.table.SetCursor(index)
				return
			}
		}
		m.table.SetCursor(0)
		m.selectedID = m.items[0].TaskID
	}
	m.syncSelectedFromCursor()
}

func (m *OverviewModel) syncSelectedFromCursor() bool {
	if len(m.items) == 0 {
		changed := m.selectedID != ""
		m.selectedID = ""
		return changed
	}
	index := clamp(m.table.Cursor(), 0, len(m.items)-1)
	next := m.items[index].TaskID
	changed := next != m.selectedID
	m.selectedID = next
	return changed
}

func (m OverviewModel) paginationBar() string {
	page := m.page.currentPage()
	totalPages := m.page.totalPages()
	start := m.page.rangeStart()
	end := m.page.rangeEnd()
	sortLabel := m.sortLabel()
	size := normalizePageSize(m.page.size)
	if m.width < 72 {
		return "PgUp  " + itoa(page) + "/" + itoa(totalPages) + " " + itoa(start) + "-" + itoa(end) + "/" + itoa(m.page.total) + "  PgDn  " + sortLabel + " " + itoa(size)
	}
	return "上页 PgUp  第 " + itoa(page) + "/" + itoa(totalPages) + " 页  " + itoa(start) + "-" + itoa(end) + "/" + itoa(m.page.total) + "  下页 PgDn  排序: " + sortLabel + "  条数: " + itoa(size)
}

func (m OverviewModel) sortLabel() string {
	label := "任务ID"
	switch m.sortMode {
	case sortByStatus:
		label = "状态"
	case sortBySeverity:
		label = "严重"
	case sortByLastRun:
		label = "最近运行"
	case sortByVerdict:
		label = "判定"
	case sortByCompletionCount:
		label = "完成"
	}
	if m.sortAsc {
		return label + "↑"
	}
	return label + "↓"
}

func (ps PageState) totalPages() int {
	size := normalizePageSize(ps.size)
	if ps.total <= 0 {
		return 1
	}
	return (ps.total + size - 1) / size
}

func (ps PageState) currentPage() int {
	return clamp(ps.current, 1, ps.totalPages())
}

func (ps PageState) offset() int {
	return (ps.currentPage() - 1) * normalizePageSize(ps.size)
}

func (ps PageState) rangeStart() int {
	if ps.total <= 0 {
		return 0
	}
	return ps.offset() + 1
}

func (ps PageState) rangeEnd() int {
	if ps.total <= 0 {
		return 0
	}
	return min(ps.offset()+normalizePageSize(ps.size), ps.total)
}

func normalizePageSize(size int) int {
	switch size {
	case 10, 20, 40, 50:
		return size
	default:
		return 20
	}
}

func computePageSize(termHeight int, auto bool, current int) int {
	if !auto {
		return normalizePageSize(current)
	}
	available := termHeight - 11
	switch {
	case available >= 50:
		return 50
	case available >= 40:
		return 40
	case available >= 20:
		return 20
	default:
		return 10
	}
}

func nextPageSize(size int) int {
	switch normalizePageSize(size) {
	case 10:
		return 20
	case 20:
		return 40
	case 40:
		return 50
	default:
		return 10
	}
}

func overviewSearchFromText(value string) db.ProjectSearch {
	rawTerms := strings.Fields(strings.TrimSpace(value))
	terms := make([]db.ProjectSearchTerm, 0, len(rawTerms))
	for _, raw := range rawTerms {
		term := db.ProjectSearchTerm{Text: raw}
		term.Statuses = append(term.Statuses, statusMatchesForSearch(raw)...)
		term.TaskStates = append(term.TaskStates, taskStateMatchesForSearch(raw)...)
		term.Verdicts = append(term.Verdicts, verdictMatchesForSearch(raw)...)
		term.FailedStages = append(term.FailedStages, stageMatchesForSearch(raw)...)
		term.Statuses = uniqueStrings(term.Statuses)
		term.TaskStates = uniqueStrings(term.TaskStates)
		term.Verdicts = uniqueStrings(term.Verdicts)
		term.FailedStages = uniqueStrings(term.FailedStages)
		terms = append(terms, term)
	}
	return db.ProjectSearch{Terms: terms}
}

func statusMatchesForSearch(term string) []string {
	switch {
	case strings.Contains(term, "运行中"):
		return []string{model.RunRunning}
	case strings.Contains(term, "崩溃"):
		return []string{model.RunCrashed}
	case strings.Contains(term, "有发现"):
		return []string{model.RunCompletedWithFindings}
	case strings.Contains(term, "已中止") || strings.Contains(term, "中止"):
		return []string{model.RunAborted}
	case strings.Contains(term, "不通过"):
		return nil
	case strings.Contains(term, "通过"):
		return []string{model.RunCompletedClean}
	default:
		return nil
	}
}

func taskStateMatchesForSearch(term string) []string {
	switch {
	case strings.Contains(term, "开始质检") || strings.Contains(term, "质检中") || strings.Contains(term, "同步中"):
		return []string{model.TaskInspecting}
	case strings.Contains(term, "待处理") || strings.Contains(term, "等待人工") || strings.Contains(term, "等待手动"):
		return []string{model.TaskWaitingManual}
	case strings.Contains(term, "结束质检") || strings.Contains(term, "已完成"):
		return []string{model.TaskCompleted}
	default:
		return nil
	}
}

func verdictMatchesForSearch(term string) []string {
	switch {
	case strings.Contains(term, "不通过"):
		return []string{model.ManualFail}
	case strings.Contains(term, "未判定"):
		return []string{model.ManualUnset}
	case strings.Contains(term, "返工"):
		return []string{model.ManualRework}
	case strings.Contains(term, "通过"):
		return []string{model.ManualPass}
	default:
		return nil
	}
}

func stageMatchesForSearch(term string) []string {
	trimmed := strings.TrimSpace(term)
	upper := strings.ToUpper(trimmed)
	if isStageLetter(upper) {
		return []string{upper}
	}
	if strings.HasPrefix(trimmed, "阶段") {
		stage := strings.ToUpper(strings.TrimSpace(strings.TrimPrefix(trimmed, "阶段")))
		if isStageLetter(stage) {
			return []string{stage}
		}
	}

	lower := strings.ToLower(trimmed)
	var stages []string
	if strings.Contains(trimmed, "结构") {
		stages = append(stages, string(model.StageA))
	}
	if strings.Contains(lower, "docker") {
		stages = append(stages, string(model.StageB))
	}
	if strings.Contains(trimmed, "测试") {
		stages = append(stages, string(model.StageC), string(model.StageD))
	}
	if strings.Contains(trimmed, "验收") {
		stages = append(stages, string(model.StageE))
	}
	if strings.Contains(trimmed, "修复") || strings.Contains(trimmed, "标注") {
		stages = append(stages, string(model.StageF))
	}
	return uniqueStrings(stages)
}

func isStageLetter(value string) bool {
	return model.IsStageID(value)
}

func uniqueStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	result := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func overviewLoadCmd(req overviewLoadRequestMsg) tea.Cmd {
	return func() tea.Msg { return req }
}

func overviewRefreshCmd(silent bool, refreshDetail bool) tea.Cmd {
	return func() tea.Msg {
		return overviewRefreshMsg{silent: silent, refreshDetail: refreshDetail}
	}
}

func columnsFromSpecs(specs []overviewColumnSpec) []table.Column {
	columns := make([]table.Column, 0, len(specs))
	for _, spec := range specs {
		columns = append(columns, table.Column{Title: spec.Title, Width: spec.Width})
	}
	return columns
}

func overviewIndex(rows []overviewItem, taskID string) int {
	for index, row := range rows {
		if row.TaskID == taskID {
			return index
		}
	}
	return -1
}

func itoa(value int) string {
	return strconv.Itoa(value)
}
