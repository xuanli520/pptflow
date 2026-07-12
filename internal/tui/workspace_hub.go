package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

const hubRefreshInterval = 5 * time.Second

type hubPage struct{ pageBase }

func (p *hubPage) Init() tea.Cmd {
	if p.m == nil {
		return nil
	}
	return tea.Batch(p.m.loadHub(true), hubPollCmd())
}

func (p *hubPage) Update(msg tea.Msg) (bool, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok || p.m == nil {
		return false, nil
	}
	return true, p.m.updateHubKey(key)
}

func (p *hubPage) HandleKey(key tea.KeyMsg) tea.Cmd {
	_, cmd := p.Update(key)
	return cmd
}

func (p *hubPage) View(width, height int) string {
	if p.m == nil {
		return ""
	}
	p.m.width, p.m.height = width, height
	return p.m.hubView()
}

func (m model) loadHub(syncFirst bool) tea.Cmd {
	return func() tea.Msg {
		if m.store == nil {
			return hubLoadedMsg{err: fmt.Errorf("workspace store is not available")}
		}
		if syncFirst {
			if err := m.store.SyncFromFilesystem(m.ctx, m.hubScanRoots); err != nil {
				return hubLoadedMsg{err: err}
			}
		}
		items, err := m.store.ListRuns(m.hubSort, m.hubSortAsc, m.hubFilter)
		return hubLoadedMsg{items: items, err: err}
	}
}

func (m model) refreshRunningHub() tea.Cmd {
	return func() tea.Msg {
		if m.store == nil {
			return hubLoadedMsg{err: fmt.Errorf("workspace store is not available")}
		}
		if err := m.store.RefreshRunning(m.ctx); err != nil {
			return hubLoadedMsg{err: err}
		}
		items, err := m.store.ListRuns(m.hubSort, m.hubSortAsc, m.hubFilter)
		return hubLoadedMsg{items: items, err: err}
	}
}

func hubPollCmd() tea.Cmd {
	return tea.Tick(hubRefreshInterval, func(time.Time) tea.Msg { return hubPollMsg{} })
}

func (m *model) applyHubItems(items []store.RunWithTask) {
	selected := m.selectedHubPath()
	m.hubItems = append([]store.RunWithTask(nil), items...)
	m.hubRowPaths = m.hubRowPaths[:0]
	m.hubTotalSize = 0
	rows := make([]table.Row, 0, len(items))
	selectedIndex := 0
	for index, item := range items {
		path := item.Run.WorkspacePath
		m.hubRowPaths = append(m.hubRowPaths, path)
		m.hubTotalSize += item.Run.SizeBytes
		if path == selected {
			selectedIndex = index
		}
		rows = append(rows, table.Row{
			emptyDash(item.Task.TaskName),
			hubStatus(item.Run),
			emptyDash(item.Task.CodeLang),
			emptyDash(item.Task.TaskType),
			hubTime(item.Run),
			formatBytes(item.Run.SizeBytes),
		})
	}
	m.hubTable.SetRows(rows)
	if len(rows) > 0 {
		m.hubTable.SetCursor(clampInt(selectedIndex, 0, len(rows)-1))
	}
	m.hubLastSync = time.Now()
	m.hubLoading = false
}

func (m *model) updateHubKey(msg tea.KeyMsg) tea.Cmd {
	if m.hubSearching {
		return m.updateHubSearch(msg)
	}
	switch msg.String() {
	case "up", "k":
		m.hubTable.MoveUp(1)
	case "down", "j":
		m.hubTable.MoveDown(1)
	case "pgup":
		m.hubTable.MoveUp(maxInt(1, m.hubTable.Height()/2))
	case "pgdown":
		m.hubTable.MoveDown(maxInt(1, m.hubTable.Height()/2))
	case "/":
		m.hubSearching = true
		m.hubSearch.SetValue(m.hubFilter)
		m.hubSearch.Focus()
		m.focusMgr.Push(focusSearch)
		return m.hubSearch.Cursor.BlinkCmd()
	case "s":
		m.hubSort = nextHubSort(m.hubSort)
		m.hubLoading = true
		return m.loadHub(false)
	case "S":
		m.hubSortAsc = !m.hubSortAsc
		m.hubLoading = true
		return m.loadHub(false)
	case "enter":
		return m.openSelectedWorkspace()
	case "ctrl+n":
		return m.openStartFromHubSelection()
	case "ctrl+r":
		return m.openRunConfigForSelected()
	case "f":
		return m.openTaskRepairForSelected()
	case "delete", "backspace":
		return m.confirmDeleteSelectedWorkspace()
	case "tab":
		if m.runner != nil {
			m.setView(viewOverview)
		}
	}
	return nil
}

func nextHubSort(current store.SortColumn) store.SortColumn {
	switch current {
	case store.SortByStartedAt:
		return store.SortByTaskName
	case store.SortByTaskName:
		return store.SortByStatus
	case store.SortByStatus:
		return store.SortBySizeBytes
	default:
		return store.SortByStartedAt
	}
}

func (m *model) updateHubSearch(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		m.hubSearching = false
		m.hubSearch.Blur()
		m.focusMgr.Pop()
		return nil
	case "enter":
		m.hubSearching = false
		m.hubSearch.Blur()
		m.focusMgr.Pop()
		m.hubFilter = strings.TrimSpace(m.hubSearch.Value())
		m.hubLoading = true
		return m.loadHub(false)
	}
	var cmd tea.Cmd
	m.hubSearch, cmd = m.hubSearch.Update(msg)
	query := strings.TrimSpace(m.hubSearch.Value())
	return tea.Batch(cmd, tea.Tick(150*time.Millisecond, func(time.Time) tea.Msg { return hubSearchMsg{query: query} }))
}

func (m *model) selectedHubItem() (store.RunWithTask, bool) {
	index := m.hubTable.Cursor()
	if index < 0 || index >= len(m.hubItems) {
		return store.RunWithTask{}, false
	}
	return m.hubItems[index], true
}

func (m *model) selectedHubPath() string {
	item, ok := m.selectedHubItem()
	if !ok {
		return ""
	}
	return item.Run.WorkspacePath
}

func (m *model) openSelectedWorkspace() tea.Cmd {
	item, ok := m.selectedHubItem()
	if !ok {
		return m.showToast("暂无可打开的工作区", toastWarning)
	}
	if m.runner != nil && !m.done {
		if samePath(item.Run.WorkspacePath, m.opts.Workspace) {
			m.setView(viewOverview)
			return nil
		}
		return m.showToast("当前运行尚未结束，不能切换到其他工作区", toastWarning)
	}
	if item.Run.IsResumable && !item.Run.IsActive {
		m.resumeOverlay = NewWorkspaceResumeOverlay(item)
		if m.router != nil {
			m.router.PushOverlay(m.resumeOverlay)
		}
		m.focusMgr.Push(focusOverlay)
		return nil
	}
	m.openWorkspaceSnapshot(item.Run.WorkspacePath)
	return nil
}

func (m *model) openWorkspaceSnapshot(path string) {
	next := initialWorkspaceModel(m.ctx, m.cancel, app.RunnerOptions{Workspace: path})
	*m = m.attachHubContext(next)
}

func (m model) attachHubContext(next model) model {
	runtimeOptions := m.runtimeOpts
	if len(runtimeOptions.HarborAgentEnv) == 0 && runtimeOptions.QwenHarborBaseURL == "" && runtimeOptions.OpusHarborBaseURL == "" {
		runtimeOptions = app.ExtractRuntimeOptions(m.opts)
	}
	next.opts = app.MergeRuntimeOptions(next.opts, runtimeOptions)
	next.runtimeOpts = runtimeOptions
	next.store = m.store
	next.hubRoot = m.hubRoot
	next.hubScanRoots = append([]string(nil), m.hubScanRoots...)
	next.hubItems = append([]store.RunWithTask(nil), m.hubItems...)
	next.hubRowPaths = append([]string(nil), m.hubRowPaths...)
	next.hubTable = m.hubTable
	next.hubSearch = m.hubSearch
	next.hubSort = m.hubSort
	next.hubSortAsc = m.hubSortAsc
	next.hubFilter = m.hubFilter
	next.hubTotalSize = m.hubTotalSize
	next.hubLastSync = m.hubLastSync
	next.width, next.height = m.width, m.height
	next.refreshComponentSizes()
	return next
}

func (m *model) returnToHub() tea.Cmd {
	if m.store == nil {
		return m.showToast("工作区中枢不可用", toastWarning)
	}
	m.setView(viewHub)
	m.hubLoading = true
	return m.loadHub(true)
}

func (m *model) openStartFromHubSelection() tea.Cmd {
	if m.runner != nil && !m.done {
		return m.showToast("请先等待当前运行结束", toastWarning)
	}
	source := ""
	if item, ok := m.selectedHubItem(); ok {
		source = item.Run.WorkspacePath
	}
	m.openStartForm(source)
	return nil
}

func (m *model) openStartForm(sourceWorkspace string) {
	opts := app.MergeRuntimeOptions(m.opts, m.runtimeOpts)
	if sourceWorkspace != "" {
		if loaded, _, err := app.LoadRunnerOptions(sourceWorkspace); err == nil {
			opts = app.MergeRuntimeOptions(loaded, m.runtimeOpts)
		}
	}
	opts.AutoApprove = false
	opts.Workspace = m.nextWorkspacePath(taskLabel(opts, sourceWorkspace), "run")
	next := initialStartModel(m.ctx, m.cancel, opts)
	*m = m.attachHubContext(next)
}

func (m *model) openRunConfigForSelected() tea.Cmd {
	if m.runner != nil && !m.done {
		return m.showToast("请先等待当前运行结束", toastWarning)
	}
	item, ok := m.selectedHubItem()
	if !ok {
		return m.showToast("请先选择工作区", toastWarning)
	}
	return m.openRunConfig(item.Run.WorkspacePath, item.Task.TaskName)
}

func (m *model) openTaskRepairForSelected() tea.Cmd {
	if m.runner != nil && !m.done {
		return m.showToast("请先等待当前运行结束", toastWarning)
	}
	item, ok := m.selectedHubItem()
	if !ok {
		return m.showToast("请先选择需要返修的工作区", toastWarning)
	}
	return m.openTaskRepair(item.Run.WorkspacePath, item.Task.TaskName)
}

func (m *model) openTaskRepair(source, taskName string) tea.Cmd {
	if strings.TrimSpace(source) == "" {
		return m.showToast("返修源工作区不能为空", toastWarning)
	}
	target := m.nextWorkspacePath(taskName, "repair")
	m.taskRepair = NewTaskRepairOverlay(source, target, taskName)
	if m.router != nil {
		m.router.PushOverlay(m.taskRepair)
	}
	m.focusMgr.Push(focusOverlay)
	return m.taskRepair.Target.Cursor.BlinkCmd()
}

func (m *model) openRunConfig(source, taskName string) tea.Cmd {
	target := m.nextWorkspacePath(taskName, "retry")
	m.runConfig = NewRunConfigOverlay(source, target, taskName)
	if m.router != nil {
		m.router.PushOverlay(m.runConfig)
	}
	m.focusMgr.Push(focusOverlay)
	return m.runConfig.Target.Cursor.BlinkCmd()
}

func (m *model) confirmDeleteSelectedWorkspace() tea.Cmd {
	item, ok := m.selectedHubItem()
	if !ok {
		return m.showToast("请先选择工作区", toastWarning)
	}
	if item.Run.IsActive || samePath(item.Run.WorkspacePath, m.opts.Workspace) && m.runner != nil && !m.done {
		return m.showToast("运行中的工作区不能删除", toastWarning)
	}
	if !m.workspaceDeletionAllowed(item.Run.WorkspacePath) {
		return m.showToast("该路径不在可删除的工作区目录内", toastWarning)
	}
	dialog := newConfirmDialog(confirmDeleteWorkspace, "确认删除工作区", "将永久删除工作区及全部工件：\n"+item.Run.WorkspacePath)
	dialog.Path = item.Run.WorkspacePath
	m.openConfirm(dialog)
	return func() tea.Msg { return confirmOpenedMsg{} }
}

func (m *model) workspaceDeletionAllowed(path string) bool {
	if strings.TrimSpace(path) == "" || samePath(path, m.hubRoot) {
		return false
	}
	for _, root := range m.hubScanRoots {
		if pathWithinDirectory(path, root) && !samePath(path, root) {
			return true
		}
	}
	return false
}

func (m *model) nextWorkspacePath(label, suffix string) string {
	base := slug(label)
	if base == "" {
		base = "task"
	}
	root := filepath.Join(m.hubRoot, "workspaces")
	_ = os.MkdirAll(root, 0o700)
	for index := 1; ; index++ {
		candidate := filepath.Join(root, fmt.Sprintf("%s-%s-%d", base, suffix, index))
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
}

func (m model) hubView() string {
	layout := layoutFor(m.width, m.height)
	contentWidth := maxInt(40, layout.ContentWidth)
	columns := hubColumns(contentWidth)
	m.hubTable.SetColumns(columns)
	m.hubTable.SetHeight(clampInt(layout.ContentHeight-7, 4, 20))
	lines := []string{sectionStyle.Render("工作区管理")}
	if m.hubSearching || m.hubFilter != "" {
		search := m.hubSearch.View()
		if !m.hubSearching {
			search = "/ " + emptyDash(m.hubFilter)
		}
		lines = append(lines, "搜索: "+search)
	}
	if len(m.hubItems) == 0 && !m.hubLoading {
		lines = append(lines, "", subtleStyle.Render("暂无工作区，按 Ctrl+N 创建第一个运行。"), "")
	} else {
		lines = append(lines, m.hubTable.View())
	}
	summary := fmt.Sprintf("共 %d 个工作区  磁盘占用 %s", len(m.hubItems), formatBytes(m.hubTotalSize))
	if m.hubLoading {
		summary += "  正在刷新..."
	}
	lines = append(lines, subtleStyle.Render(summary))
	return panelStyle.Width(contentWidth).Render(strings.Join(lines, "\n"))
}

func hubColumns(width int) []table.Column {
	usable := maxInt(48, width-8)
	name := clampInt(usable*28/100, 12, 30)
	status := clampInt(usable*14/100, 8, 12)
	lang := clampInt(usable*10/100, 6, 10)
	taskType := clampInt(usable*12/100, 6, 12)
	when := clampInt(usable*21/100, 12, 18)
	size := maxInt(7, usable-name-status-lang-taskType-when-5)
	return []table.Column{{Title: "名称", Width: name}, {Title: "状态", Width: status}, {Title: "语言", Width: lang}, {Title: "类型", Width: taskType}, {Title: "时间", Width: when}, {Title: "大小", Width: size}}
}

func hubStatus(run store.Run) string {
	if run.IsActive {
		return activeIconStyle.Render("◌ 运行中")
	}
	if run.IsResumable {
		return resumableIconStyle.Render("⚷ 可恢复")
	}
	switch strings.ToLower(run.Status) {
	case "succeeded":
		return passStyle.Render("✓ 成功")
	case "failed":
		return failStyle.Render("✗ 失败")
	case "running":
		return activeIconStyle.Render("◌ 运行中")
	default:
		return emptyDash(run.Status)
	}
}

func hubTime(run store.Run) string {
	value := run.StartedAt
	if !run.FinishedAt.IsZero() {
		value = run.FinishedAt
	}
	if value.IsZero() {
		return "-"
	}
	return value.Local().Format("01-02 15:04")
}

func formatBytes(size int64) string {
	if size < 1024 {
		return fmt.Sprintf("%dB", size)
	}
	units := []string{"K", "M", "G", "T"}
	value := float64(size)
	unit := "B"
	for _, candidate := range units {
		value /= 1024
		unit = candidate
		if value < 1024 {
			break
		}
	}
	return fmt.Sprintf("%.1f%s", value, unit)
}

func taskLabel(opts app.RunnerOptions, workspace string) string {
	if strings.TrimSpace(opts.TaskName) != "" {
		return opts.TaskName
	}
	if strings.TrimSpace(opts.TaskDir) != "" {
		return filepath.Base(opts.TaskDir)
	}
	return filepath.Base(workspace)
}

var nonSlug = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func slug(value string) string {
	value = strings.Trim(nonSlug.ReplaceAllString(strings.TrimSpace(value), "-"), "-._")
	if len(value) > 48 {
		value = value[:48]
	}
	return strings.ToLower(value)
}

func samePath(left, right string) bool {
	leftAbs, leftErr := filepath.Abs(left)
	rightAbs, rightErr := filepath.Abs(right)
	return leftErr == nil && rightErr == nil && filepath.Clean(leftAbs) == filepath.Clean(rightAbs)
}

func pathWithinDirectory(path, root string) bool {
	pathAbs, pathErr := filepath.Abs(path)
	rootAbs, rootErr := filepath.Abs(root)
	if pathErr != nil || rootErr != nil {
		return false
	}
	rel, err := filepath.Rel(rootAbs, pathAbs)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
