package tui

import (
	"fmt"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
)

func (m model) Init() tea.Cmd {
	if m.view == viewStart {
		return nil
	}
	if m.runner == nil {
		return tea.Batch(m.refreshWorkspace(), m.spinner.Tick)
	}
	return tea.Batch(m.runWorkflow(), m.waitEvent(), m.refreshWorkspace(), m.spinner.Tick)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m = m.cloneForUpdate()
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.refreshComponentSizes()
		return m, nil
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		if m.done || m.view == viewStart {
			return m, nil
		}
		return m, cmd
	case toastExpiredMsg:
		if msg.id == m.toast.ID {
			m.toast.Message = ""
		}
		return m, nil
	case confirmOpenedMsg:
		return m, nil
	case tea.MouseMsg:
		return m, m.handleMouse(msg)
	case tea.FocusMsg:
		m.bindPages()
		if page := m.router.Page(m.view); page != nil {
			page.Focus()
		}
		return m, nil
	case tea.BlurMsg:
		m.bindPages()
		if page := m.router.Page(m.view); page != nil {
			page.Blur()
		}
		return m, nil
	case tea.KeyMsg:
		if handled, cmd := m.handleGlobalKey(msg); handled {
			return m, cmd
		}
		m.bindPages()
		if handled, cmd := m.router.Dispatch(msg); handled {
			return m, cmd
		}
	case runnerEventMsg:
		event := domain.RunnerEvent(msg)
		m.applyRunnerEvent(event)
		return m, m.waitEvent()
	case editorDoneMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		if msg.before.changed(msg.after) {
			if m.editedFiles == nil {
				m.editedFiles = map[string]string{}
			}
			m.editedFiles[msg.path] = editSummary(msg.before, msg.after)
		}
		return m, m.showToast("工件编辑已完成", toastSuccess)
	case gateDecisionWrittenMsg:
		if msg.err != nil {
			m.err = msg.err
			m.notice = ""
			m.activeGate = msg.gate
			m.view = viewGate
			return m, nil
		}
		m.err = nil
		if msg.path != "" {
			m.summary.GateDecisions = mergeGateDecisions(m.summary.GateDecisions, []domain.GateDecision{msg.decision})
			m.notice = fmt.Sprintf("决定已写入 %s。快照模式只写决定文件，需由外部运行器消费。", msg.path)
		} else {
			m.notice = ""
		}
		m.resetGateLocalState()
		return m, m.showToast("审查决定已提交", toastSuccess)
	case runnerDoneMsg:
		m.summary = msg.summary
		m.err = msg.err
		m.done = true
		m.view = viewDone
		return m, nil
	case workspaceRefreshMsg:
		m.applyWorkspaceSnapshot(msg.summary, msg.events)
		if m.done {
			return m, nil
		}
		return m, m.refreshWorkspace()
	}
	return m, nil
}

func (m model) View() string {
	m = m.cloneForUpdate()
	if m.width == 0 {
		return "正在启动 Harbor 出题工坊...\n"
	}
	body := m.currentPage().View(m.width, m.height)
	return renderFrame(m, body)
}
