package tui

type focusTarget int

const (
	focusTargetPage focusTarget = iota
	focusTargetInputBox
	focusTargetOverlay
)

type focusManager struct {
	stack   []focusTarget
	current focusTarget
}

func newFocusManager() focusManager {
	return focusManager{current: focusTargetPage}
}

func (m *focusManager) Push(target focusTarget) {
	if m == nil {
		return
	}
	if m.current != target {
		m.stack = append(m.stack, m.current)
	}
	m.current = target
}

func (m *focusManager) Pop() focusTarget {
	if m == nil {
		return focusTargetPage
	}
	if len(m.stack) == 0 {
		m.current = focusTargetPage
		return m.current
	}
	last := len(m.stack) - 1
	m.current = m.stack[last]
	m.stack = m.stack[:last]
	return m.current
}

func (m focusManager) Current() focusTarget {
	return m.current
}
