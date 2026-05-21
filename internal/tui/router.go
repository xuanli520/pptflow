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
	descriptors []pageDescriptor
	pages       map[pageID]Page
	overlays    []Overlay
	active      pageID
}

func newPageRouter() *pageRouter {
	return &pageRouter{
		active: pageTaskBoard,
		pages:  map[pageID]Page{},
		descriptors: []pageDescriptor{
			{id: pageTaskBoard, name: "题目管理"},
			{id: pageOverview, name: "总览", key: "Ctrl+O"},
			{id: pageExecution, name: "执行详情"},
		},
	}
}

func (r *pageRouter) RegisterPage(id pageID, page Page) {
	if r == nil || page == nil {
		return
	}
	if r.pages == nil {
		r.pages = map[pageID]Page{}
	}
	r.pages[id] = page
}

func (r *pageRouter) SwitchTo(id pageID) {
	if r == nil {
		return
	}
	if current := r.pages[r.active]; current != nil && r.active != id {
		current.Blur()
	}
	r.active = id
	if next := r.pages[id]; next != nil {
		next.Focus()
	}
}

func (r *pageRouter) Active() pageID {
	if r == nil {
		return pageTaskBoard
	}
	return r.active
}

func (r *pageRouter) ActivePage() Page {
	if r == nil {
		return nil
	}
	return r.pages[r.active]
}

func (r *pageRouter) PushOverlay(overlay Overlay) tea.Cmd {
	if r == nil || overlay == nil {
		return nil
	}
	r.overlays = append(r.overlays, overlay)
	return overlay.Init()
}

func (r *pageRouter) PopOverlay() tea.Cmd {
	if r == nil || len(r.overlays) == 0 {
		return nil
	}
	last := len(r.overlays) - 1
	overlay := r.overlays[last]
	r.overlays = r.overlays[:last]
	return overlay.Destroy()
}

func (r *pageRouter) TopOverlay() Overlay {
	if r == nil || len(r.overlays) == 0 {
		return nil
	}
	return r.overlays[len(r.overlays)-1]
}

func (r *pageRouter) Dispatch(msg tea.Msg) (bool, tea.Cmd) {
	if r == nil {
		return false, nil
	}
	if overlay := r.TopOverlay(); overlay != nil {
		handled, cmd := overlay.Update(msg)
		if handled {
			return true, cmd
		}
		if _, ok := msg.(tea.KeyMsg); ok && overlay.InterceptsAllKeys() {
			return true, cmd
		}
	}
	if page := r.ActivePage(); page != nil {
		return page.Update(msg)
	}
	return false, nil
}
