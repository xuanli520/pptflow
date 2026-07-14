package tui

import "github.com/charmbracelet/bubbles/textinput"

func initModelComponents(m model) model {
	m.router = newPageRouter(viewHub)
	m.focusMgr = newFocusManager(focusPage)
	m.hubSearch = textinput.New()
	m.hubSearch.Prompt = "/ "
	m.hubSearch.Placeholder = "搜索名称、语言、类型或状态"
	m.hubSearch.CharLimit = 256
	m.hubSearch.Width = 42
	return m
}

func (m model) cloneForUpdate() model {
	m.router = m.router.Clone()
	m.taskHub = m.taskHub.Clone()
	if m.taskHubPlan != nil {
		preview := m.taskHubPlan.Clone()
		m.taskHubPlan = &preview
	}
	if m.taskHubPlanCommand != nil {
		command := *m.taskHubPlanCommand
		command.Target = m.taskHubPlanCommand.Target
		m.taskHubPlanCommand = &command
	}
	if m.taskHubDetail != nil {
		m.taskHubDetail = m.taskHubDetail.Clone()
	}
	if m.taskHubMutation != nil {
		m.taskHubMutation = m.taskHubMutation.Clone()
	}
	if m.runControl != nil {
		m.runControl = m.runControl.Clone()
	}
	if m.exitHandoff != nil {
		m.exitHandoff = m.exitHandoff.Clone()
	}
	m.focusMgr.stack = append([]focusArea(nil), m.focusMgr.stack...)
	return m
}

func (m *model) refreshComponentSizes() {
	m.hubSearch.Width = clampInt(contentWidth(m.width)-16, 16, 64)
}
