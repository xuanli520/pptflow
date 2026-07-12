package tui

import tea "github.com/charmbracelet/bubbletea"

type pageBase struct{ m *model }

func (p *pageBase) apply(updated tea.Model) {
	if next, ok := updated.(model); ok && p.m != nil {
		*p.m = next
	}
}

func (p *pageBase) Init() tea.Cmd                { return nil }
func (p *pageBase) Focus()                       {}
func (p *pageBase) Blur()                        {}
func (p *pageBase) HandleKey(tea.KeyMsg) tea.Cmd { return nil }
