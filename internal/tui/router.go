package tui

import (
	"sort"

	tea "github.com/charmbracelet/bubbletea"
)

// Page is the common contract for full-screen content. Pages are intentionally
// small value objects; workflow state remains owned by model and is passed in
// when a page is rendered, preventing duplicated sources of truth.
type Page interface {
	Init() tea.Cmd
	Update(tea.Msg) (bool, tea.Cmd)
	View(width, height int) string
	Focus()
	Blur()
	HandleKey(tea.KeyMsg) tea.Cmd
}

type Overlay interface {
	Page
	ZIndex() int
	InterceptsAllKeys() bool
}

type pageRouter struct {
	active   viewMode
	pages    map[viewMode]Page
	overlays []Overlay
}

func newPageRouter(active viewMode) *pageRouter {
	return &pageRouter{active: active, pages: map[viewMode]Page{}}
}

func (r *pageRouter) Clone() *pageRouter {
	if r == nil {
		return nil
	}
	clone := &pageRouter{active: r.active, pages: map[viewMode]Page{}}
	for id, page := range r.pages {
		clone.pages[id] = page
	}
	clone.overlays = append([]Overlay(nil), r.overlays...)
	return clone
}

func (r *pageRouter) Register(id viewMode, page Page) {
	if r == nil || page == nil {
		return
	}
	if r.pages == nil {
		r.pages = map[viewMode]Page{}
	}
	r.pages[id] = page
}

func (r *pageRouter) Page(id viewMode) Page {
	if r == nil {
		return nil
	}
	return r.pages[id]
}

func (r *pageRouter) SwitchTo(id viewMode) {
	if r != nil {
		if r.active == id {
			return
		}
		if current := r.Page(r.active); current != nil {
			current.Blur()
		}
		r.active = id
		if next := r.Page(id); next != nil {
			next.Focus()
		}
	}
}

func (r *pageRouter) Active() viewMode {
	if r == nil {
		return viewHub
	}
	return r.active
}

func (r *pageRouter) PushOverlay(overlay Overlay) {
	if r == nil || overlay == nil {
		return
	}
	r.overlays = append(r.overlays, overlay)
	sort.SliceStable(r.overlays, func(i, j int) bool { return r.overlays[i].ZIndex() < r.overlays[j].ZIndex() })
	overlay.Focus()
}

func (r *pageRouter) PopOverlay() Overlay {
	if r == nil || len(r.overlays) == 0 {
		return nil
	}
	idx := len(r.overlays) - 1
	overlay := r.overlays[idx]
	r.overlays = r.overlays[:idx]
	overlay.Blur()
	return overlay
}

func (r *pageRouter) TopOverlay() Overlay {
	if r == nil || len(r.overlays) == 0 {
		return nil
	}
	return r.overlays[len(r.overlays)-1]
}

func (r *pageRouter) Dispatch(msg tea.Msg) (bool, tea.Cmd) {
	if overlay := r.TopOverlay(); overlay != nil {
		handled, cmd := overlay.Update(msg)
		if handled || overlay.InterceptsAllKeys() {
			return true, cmd
		}
	}
	page := r.Page(r.active)
	if page == nil {
		return false, nil
	}
	return page.Update(msg)
}
