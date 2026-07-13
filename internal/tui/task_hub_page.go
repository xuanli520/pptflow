package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

const taskHubRefreshInterval = 5 * time.Second

// hubPage is intentionally lifecycle-only. The former workspace-index page
// was removed during the V2 hard cutover.
type hubPage struct{ pageBase }

func (p *hubPage) Init() tea.Cmd {
	if p.m == nil || p.m.lifecycle == nil {
		return nil
	}
	p.m.taskHub.Loading = true
	return tea.Batch(p.m.loadTaskHubV2(), taskHubPollCmd())
}

func (p *hubPage) Update(msg tea.Msg) (bool, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok || p.m == nil || p.m.lifecycle == nil {
		return false, nil
	}
	return true, p.m.updateTaskHubV2Key(key)
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
	return p.m.taskHubV2View()
}

func taskHubPollCmd() tea.Cmd {
	return tea.Tick(taskHubRefreshInterval, func(time.Time) tea.Msg { return taskHubPollMsg{} })
}

// returnToHub only returns to the lifecycle-backed Task Hub. It deliberately
// does not reopen a workspace index or filesystem snapshot.
func (m *model) returnToHub() tea.Cmd {
	m.setView(viewHub)
	if m.lifecycle == nil {
		return nil
	}
	m.taskHub.Loading = true
	return m.loadTaskHubV2()
}
