package tui

type focusArea int

const (
	focusPage focusArea = iota
	focusSearch
	focusOverlay
)

type focusManager struct {
	stack   []focusArea
	current focusArea
}

func newFocusManager(initial focusArea) focusManager { return focusManager{current: initial} }
func (f *focusManager) Current() focusArea           { return f.current }
func (f *focusManager) SetCurrent(area focusArea)    { f.current = area }
func (f *focusManager) Push(area focusArea) {
	f.stack = append(f.stack, f.current)
	f.current = area
}
func (f *focusManager) Pop() focusArea {
	if len(f.stack) == 0 {
		return f.current
	}
	idx := len(f.stack) - 1
	f.current = f.stack[idx]
	f.stack = f.stack[:idx]
	return f.current
}
