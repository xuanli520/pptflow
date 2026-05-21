package tui

type taskColumnID int

const (
	taskColumnInspecting taskColumnID = iota
	taskColumnWaiting
	taskColumnCompleted
)

type taskListModel struct {
	state    string
	title    string
	items    []TaskProject
	cursor   int
	scroll   int
	lastSize int
}

func (m *taskListModel) setItems(items []TaskProject) {
	selected := m.selectedID()
	m.items = append([]TaskProject(nil), items...)
	m.cursor = 0
	if selected != "" {
		for index, item := range m.items {
			if item.ID == selected {
				m.cursor = index
				break
			}
		}
	}
	m.clamp()
}

func (m *taskListModel) move(delta int) {
	if len(m.items) == 0 {
		m.cursor = 0
		m.scroll = 0
		return
	}
	next := (m.cursor + delta) % len(m.items)
	if next < 0 {
		next += len(m.items)
	}
	m.cursor = next
	m.clamp()
}

func (m *taskListModel) selected() (TaskProject, bool) {
	if len(m.items) == 0 {
		return TaskProject{}, false
	}
	index := clamp(m.cursor, 0, len(m.items)-1)
	return m.items[index], true
}

func (m *taskListModel) selectedID() string {
	item, ok := m.selected()
	if !ok {
		return ""
	}
	return item.ID
}

func (m *taskListModel) clamp() {
	if len(m.items) == 0 {
		m.cursor = 0
		m.scroll = 0
		return
	}
	m.cursor = clamp(m.cursor, 0, len(m.items)-1)
	if m.lastSize <= 0 {
		m.lastSize = 1
	}
	if m.cursor < m.scroll {
		m.scroll = m.cursor
	}
	if m.cursor >= m.scroll+m.lastSize {
		m.scroll = m.cursor - m.lastSize + 1
	}
	m.scroll = clamp(m.scroll, 0, max(0, len(m.items)-1))
}
