package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

func initModelComponents(m model) model {
	m.router = newPageRouter(m.view)
	switch m.view {
	case viewHub:
		m.focusMgr = newFocusManager(focusPage)
	case viewStart:
		m.focusMgr = newFocusManager(focusStartField)
	case viewOverview:
		m.focusMgr = newFocusManager(focusOverviewTable)
	default:
		m.focusMgr = newFocusManager(focusPage)
	}
	m.spinner = spinner.New()
	m.spinner.Spinner = spinner.Dot
	m.spinner.Style = defaultTheme.Focused
	m.startInputs = map[startField]textinput.Model{}
	m.dirtyStartInputs = map[startField]bool{}
	m.startCollapsed = map[startGroup]bool{}
	m.startStep = startStepBasic
	m.selectedStartGroup = startGroupHarbor
	for _, group := range advancedGroups() {
		m.startCollapsed[group] = group != startGroupHarbor
	}
	for _, field := range allTextStartFields() {
		ti := textinput.New()
		ti.CharLimit = 4096
		ti.Prompt = ""
		ti.SetValue(startFieldValue(m.opts, field))
		ti.Width = 48
		m.startInputs[field] = ti
	}
	if ti, ok := m.startInputs[m.startField]; ok {
		ti.Focus()
		m.startInputs[m.startField] = ti
	}
	m.notesInput = textarea.New()
	m.notesInput.Placeholder = "输入审查备注（支持中文和多行）"
	m.notesInput.SetWidth(64)
	m.notesInput.SetHeight(5)
	m.notesInput.CharLimit = 10000
	m.searchInput = textinput.New()
	m.searchInput.Prompt = "/ "
	m.searchInput.Placeholder = "输入节点、消息或文件名"
	m.searchInput.CharLimit = 256
	m.searchInput.Width = 42
	m.hubSearch = textinput.New()
	m.hubSearch.Prompt = "/ "
	m.hubSearch.Placeholder = "搜索名称、语言、类型或状态"
	m.hubSearch.CharLimit = 256
	m.hubSearch.Width = 42
	m.detailViewport = viewport.New(40, 10)
	m.gateViewport = viewport.New(40, 10)
	m.overviewTable = table.New(table.WithColumns([]table.Column{{Title: "状态", Width: 8}, {Title: "节点", Width: 26}, {Title: "消息", Width: 48}}), table.WithFocused(true), table.WithHeight(12))
	m.hubTable = table.New(table.WithColumns([]table.Column{{Title: "名称", Width: 24}, {Title: "状态", Width: 10}, {Title: "语言", Width: 8}, {Title: "类型", Width: 10}, {Title: "时间", Width: 16}, {Title: "大小", Width: 9}}), table.WithFocused(true), table.WithHeight(12))
	return m
}

func (m model) cloneForUpdate() model {
	m.router = m.router.Clone()
	m.events = append([]domain.RunnerEvent(nil), m.events...)
	nodes := m.nodes
	m.nodes = make(map[string]domain.RunnerEvent, len(nodes))
	for id, event := range nodes {
		m.nodes[id] = event
	}
	startInputs := m.startInputs
	m.startInputs = make(map[startField]textinput.Model, len(startInputs))
	for field, input := range startInputs {
		m.startInputs[field] = input
	}
	dirtyInputs := m.dirtyStartInputs
	m.dirtyStartInputs = make(map[startField]bool, len(dirtyInputs))
	for field, dirty := range dirtyInputs {
		m.dirtyStartInputs[field] = dirty
	}
	collapsedGroups := m.startCollapsed
	m.startCollapsed = make(map[startGroup]bool, len(collapsedGroups))
	for group, collapsed := range collapsedGroups {
		m.startCollapsed[group] = collapsed
	}
	m.editedFiles = cloneStringMap(m.editedFiles)
	m.pathSuggestions = append([]string(nil), m.pathSuggestions...)
	m.overviewRowIDs = append([]string(nil), m.overviewRowIDs...)
	m.hubItems = append([]store.RunWithTask(nil), m.hubItems...)
	m.hubRowPaths = append([]string(nil), m.hubRowPaths...)
	m.hubScanRoots = append([]string(nil), m.hubScanRoots...)
	m.runtimeOpts.HarborAgentEnv = append([]string(nil), m.runtimeOpts.HarborAgentEnv...)
	m.focusMgr.stack = append([]focusArea(nil), m.focusMgr.stack...)
	if m.confirm != nil {
		confirmation := *m.confirm
		m.confirm = &confirmation
	}
	if m.runConfig != nil {
		clone := *m.runConfig
		m.runConfig = &clone
	}
	if m.resumeOverlay != nil {
		clone := *m.resumeOverlay
		m.resumeOverlay = &clone
	}
	return m
}

func (m *model) refreshComponentSizes() {
	l := layoutFor(m.width, m.height)
	inputWidth := clampInt(l.ContentWidth-30, 16, 72)
	for field, ti := range m.startInputs {
		ti.Width = inputWidth
		m.startInputs[field] = ti
	}
	m.notesInput.SetWidth(clampInt(l.ContentWidth-8, 24, 90))
	m.notesInput.SetHeight(clampInt(l.ContentHeight/3, 3, 9))
	m.searchInput.Width = clampInt(l.ContentWidth-16, 16, 64)
	m.hubSearch.Width = clampInt(l.ContentWidth-16, 16, 64)
	m.detailViewport.Width = maxInt(20, l.MainWidth-4)
	m.detailViewport.Height = maxInt(4, l.ContentHeight/2)
	m.gateViewport.Width = maxInt(20, l.ContentWidth-panelStyle.GetHorizontalFrameSize())
}

func matchesFilter(filter string, values ...string) bool {
	filter = strings.ToLower(strings.TrimSpace(filter))
	if filter == "" {
		return true
	}
	for _, value := range values {
		if strings.Contains(strings.ToLower(value), filter) {
			return true
		}
	}
	return false
}
