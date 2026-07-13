package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
)

func (m model) Init() tea.Cmd {
	if m.view == viewHub {
		m.hubLoading = true
		return tea.Batch(m.loadHub(true), hubPollCmd())
	}
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
		if m.done || m.view == viewStart || m.view == viewHub {
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
		if m.store != nil {
			return m, m.loadHub(true)
		}
		return m, nil
	case workspaceRefreshMsg:
		m.applyWorkspaceSnapshot(msg.summary, msg.events)
		if m.done {
			return m, nil
		}
		return m, m.refreshWorkspace()
	case hubLoadedMsg:
		if msg.err != nil {
			m.hubLoading = false
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.applyHubItems(msg.items)
		return m, nil
	case hubPollMsg:
		if m.view == viewHub {
			m.hubLoading = true
			if m.scheduler != nil {
				tasks := m.scheduler.Snapshot()
				if tasks.Running > 0 || tasks.Queued > 0 {
					return m, tea.Batch(m.loadHub(true), hubPollCmd())
				}
			}
			return m, tea.Batch(m.refreshRunningHub(), hubPollCmd())
		}
		return m, hubPollCmd()
	case hubSearchMsg:
		if m.hubSearching && strings.TrimSpace(m.hubSearch.Value()) == msg.query {
			m.hubFilter = msg.query
			m.hubLoading = true
			return m, m.loadHub(false)
		}
		return m, nil
	case workspaceDeletedMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.notice = "已删除工作区：" + msg.path
		m.hubLoading = true
		return m, tea.Batch(m.showToast("工作区已删除", toastSuccess), m.loadHub(true))
	case clonePreparedMsg:
		if msg.err != nil {
			m.err = msg.err
			if m.runConfig != nil {
				m.runConfig.Loading = false
			}
			if m.taskRepair != nil {
				m.taskRepair.Loading = false
			}
			return m, nil
		}
		if m.taskRepair != nil {
			m.closeTaskRepair()
		} else {
			m.closeRunConfig()
		}
		if msg.background {
			if m.scheduler == nil {
				m.err = fmt.Errorf("并行任务调度器不可用")
				return m, nil
			}
			if _, err := m.scheduler.Submit(msg.opts); err != nil {
				m.err = err
				return m, nil
			}
			m.runner = nil
			m.notice = fmt.Sprintf("已从 %s 创建并加入并行队列。", msg.manifest.SourceWorkspace)
			m.setView(viewHub)
			m.hubLoading = true
			return m, tea.Batch(m.showToast("重跑任务已加入并行队列", toastSuccess), m.loadHub(true))
		}
		m = m.startRunner(msg.opts)
		m.setView(viewOverview)
		m.notice = fmt.Sprintf("已从 %s 创建新工作区。", msg.manifest.SourceWorkspace)
		return m, tea.Batch(m.runWorkflow(), m.waitEvent(), m.refreshWorkspace(), m.spinner.Tick)
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
