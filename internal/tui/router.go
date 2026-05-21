package tui

import tea "github.com/charmbracelet/bubbletea"

type pageID int

const (
	pageTaskBoard pageID = iota
	pageOverview
	pageExecution
)

type Page interface {
	Init() tea.Cmd
	Update(tea.Msg) (bool, tea.Cmd)
	View(width, height int) string
	Focus()
	Blur()
	HandleKey(tea.KeyMsg) tea.Cmd
	Destroy() tea.Cmd
}

type Overlay interface {
	Init() tea.Cmd
	Update(tea.Msg) (bool, tea.Cmd)
	View(width, height int) string
	ZIndex() int
	InterceptsAllKeys() bool
	Destroy() tea.Cmd
}

type pageDescriptor struct {
	id   pageID
	name string
	key  string
}

type pageRouter struct {
	pages  []pageDescriptor
	active pageID
}

func newPageRouter() *pageRouter {
	return &pageRouter{
		active: pageTaskBoard,
		pages: []pageDescriptor{
			{id: pageTaskBoard, name: "题目管理"},
			{id: pageOverview, name: "总览", key: "Ctrl+O"},
		},
	}
}

func (r *pageRouter) SwitchTo(id pageID) {
	if r == nil {
		return
	}
	r.active = id
}

func (r *pageRouter) Active() pageID {
	if r == nil {
		return pageTaskBoard
	}
	return r.active
}
