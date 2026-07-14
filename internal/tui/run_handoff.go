package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

const taskHubExitHandoffReason = "Task Hub exit requested controlled child-worker handoff"

type taskHubRunHandoffState string

const (
	taskHubRunHandoffReady       taskHubRunHandoffState = "ready"
	taskHubRunHandoffLaunching   taskHubRunHandoffState = "launching"
	taskHubRunHandoffSucceeded   taskHubRunHandoffState = "succeeded"
	taskHubRunHandoffFailed      taskHubRunHandoffState = "failed"
	taskHubRunHandoffNotEligible taskHubRunHandoffState = "not_eligible"
)

// The indirection keeps UUID allocation failures and collision defenses
// deterministic in unit tests while production always uses the global V2
// UUIDv7 allocator.
var newTaskHubRunHandoffOperationID = store.NewUUIDv7

type taskHubExitHandoffItem struct {
	Run      TaskHubRun
	Request  TaskHubRunHandoffRequest
	Selected bool
	State    taskHubRunHandoffState
	Result   TaskHubRunHandoffResult
	Error    string
}

func (item taskHubExitHandoffItem) selectable() bool {
	return item.Run.Handoff.Enabled && item.State != taskHubRunHandoffLaunching && item.State != taskHubRunHandoffSucceeded
}

func (item taskHubExitHandoffItem) Clone() taskHubExitHandoffItem {
	item.Run = item.Run.Clone()
	return item
}

// taskHubExitHandoffOverlay owns only the temporary per-Run exit decisions.
// Durable operation IDs and expected checkpoints are captured when this panel
// opens, so a refresh cannot silently retarget a selected handoff.
type taskHubExitHandoffOverlay struct {
	Items       []taskHubExitHandoffItem
	Selected    int
	ExecutingID string
	Error       string
}

func newTaskHubExitHandoffOverlay(runs []TaskHubRun) (*taskHubExitHandoffOverlay, error) {
	overlay := &taskHubExitHandoffOverlay{}
	seen := make(map[string]struct{}, len(runs))
	actor := ""
	for _, run := range sortTaskHubRuns(runs) {
		if !run.Active && run.QueuePosition <= 0 {
			continue
		}
		if _, duplicate := seen[run.RunID]; duplicate {
			continue
		}
		seen[run.RunID] = struct{}{}
		item := taskHubExitHandoffItem{Run: run.Clone(), State: taskHubRunHandoffNotEligible}
		if !run.Handoff.Enabled {
			overlay.Items = append(overlay.Items, item)
			continue
		}
		if actor == "" {
			resolved, err := resolveTaskHubLocalActor()
			if err != nil {
				return nil, fmt.Errorf("derive Task Hub handoff actor: %w", err)
			}
			actor = resolved
		}
		operationID, err := newTaskHubRunHandoffOperationID()
		if err != nil {
			return nil, fmt.Errorf("allocate Task Hub run-worker handoff operation ID: %w", err)
		}
		if err := store.ValidateUUIDv7(operationID); err != nil {
			return nil, fmt.Errorf("allocate Task Hub run-worker handoff operation ID: %w", err)
		}
		idempotencyKey, err := newTaskHubIdempotencyKey()
		if err != nil {
			return nil, fmt.Errorf("allocate Task Hub run-worker handoff idempotency key: %w", err)
		}
		if err := store.ValidateUUIDv7(idempotencyKey); err != nil {
			return nil, fmt.Errorf("allocate Task Hub run-worker handoff idempotency key: %w", err)
		}
		if operationID == idempotencyKey {
			return nil, fmt.Errorf("allocate Task Hub run-worker handoff identities: operation ID and idempotency key collided")
		}
		item.Selected = true
		item.State = taskHubRunHandoffReady
		item.Request = TaskHubRunHandoffRequest{
			RunID:              run.RunID,
			Expected:           run.Handoff.Expected,
			HandoffOperationID: operationID,
			IdempotencyKey:     idempotencyKey,
			Owner:              taskHubExitHandoffOwner(operationID),
			Actor:              actor,
			Reason:             taskHubExitHandoffReason,
		}
		overlay.Items = append(overlay.Items, item)
	}
	if len(overlay.Items) == 0 {
		return nil, fmt.Errorf("Task Hub exit handoff requires at least one active Run")
	}
	return overlay, nil
}

func taskHubExitHandoffOwner(operationID string) string {
	return "task-hub-child:" + strings.TrimSpace(operationID)
}

func (overlay *taskHubExitHandoffOverlay) Clone() *taskHubExitHandoffOverlay {
	if overlay == nil {
		return nil
	}
	copy := *overlay
	copy.Items = make([]taskHubExitHandoffItem, len(overlay.Items))
	for index, item := range overlay.Items {
		copy.Items[index] = item.Clone()
	}
	return &copy
}

func (overlay *taskHubExitHandoffOverlay) Init() tea.Cmd                { return nil }
func (overlay *taskHubExitHandoffOverlay) Focus()                       {}
func (overlay *taskHubExitHandoffOverlay) Blur()                        {}
func (overlay *taskHubExitHandoffOverlay) ZIndex() int                  { return 110 }
func (overlay *taskHubExitHandoffOverlay) InterceptsAllKeys() bool      { return true }
func (overlay *taskHubExitHandoffOverlay) HandleKey(tea.KeyMsg) tea.Cmd { return nil }
func (overlay *taskHubExitHandoffOverlay) Update(tea.Msg) (bool, tea.Cmd) {
	return true, nil
}

func (overlay *taskHubExitHandoffOverlay) selectedCount() int {
	if overlay == nil {
		return 0
	}
	count := 0
	for _, item := range overlay.Items {
		if item.Selected {
			count++
		}
	}
	return count
}

func (overlay *taskHubExitHandoffOverlay) pendingSelectedIndex() int {
	if overlay == nil {
		return -1
	}
	for index, item := range overlay.Items {
		if item.Selected && (item.State == taskHubRunHandoffReady || item.State == taskHubRunHandoffFailed) {
			return index
		}
	}
	return -1
}

func (overlay *taskHubExitHandoffOverlay) itemIndexByOperationID(operationID string) int {
	if overlay == nil {
		return -1
	}
	for index, item := range overlay.Items {
		if item.Request.HandoffOperationID == operationID {
			return index
		}
	}
	return -1
}

func (overlay *taskHubExitHandoffOverlay) move(delta int) {
	if overlay == nil || len(overlay.Items) == 0 || overlay.ExecutingID != "" {
		return
	}
	overlay.Selected = (overlay.Selected + delta + len(overlay.Items)) % len(overlay.Items)
}

func (overlay *taskHubExitHandoffOverlay) toggleSelected() {
	if overlay == nil || overlay.ExecutingID != "" || overlay.Selected < 0 || overlay.Selected >= len(overlay.Items) {
		return
	}
	item := &overlay.Items[overlay.Selected]
	if !item.selectable() {
		return
	}
	item.Selected = !item.Selected
	if !item.Selected && item.State == taskHubRunHandoffFailed {
		item.Error = ""
	}
}

func (overlay *taskHubExitHandoffOverlay) View(width, height int) string {
	if overlay == nil {
		return ""
	}
	panelWidth := boundedPanelWidth(width, 42, 92)
	contentWidth := styleContentWidth(panelWidth, panelStyle)
	rows := []string{
		sectionStyle.Render("退出前交接"),
		subtleStyle.Render("为所选 Run 启动受控 child worker；取消勾选的 Run 将不交接。"),
		"",
	}
	for index, item := range overlay.Items {
		cursor := " "
		if index == overlay.Selected {
			cursor = ">"
		}
		mark := "[ ]"
		switch {
		case !item.Run.Handoff.Enabled:
			mark = "[-]"
		case item.Selected:
			mark = "[x]"
		}
		line := fmt.Sprintf("%s %s %s  %s", cursor, mark, shortTaskHubID(item.Run.RunID), taskHubExitHandoffItemState(item))
		rows = append(rows, clipDisplay(line, contentWidth))
		if detail := taskHubExitHandoffItemDetail(item); detail != "" {
			rows = append(rows, subtleStyle.Render(clipDisplay("    "+detail, contentWidth)))
		}
	}
	rows = append(rows, "")
	if overlay.ExecutingID != "" {
		rows = append(rows, warnStyle.Render("正在记录受控 worker handoff；请等待当前 Run 完成。"))
	} else if overlay.selectedCount() == 0 {
		rows = append(rows, warnStyle.Render("未选择任何 Run；按 Enter 将直接退出，不交接 worker。"))
	} else if overlay.Error != "" {
		rows = append(rows, failStyle.Render(clipDisplay(redactSingleLineUI(overlay.Error), contentWidth)))
	}
	rows = append(rows, subtleStyle.Render("[↑↓/j k] 选择  [Space] 勾选/取消  [Enter] 交接并退出  [Esc] 返回"))
	rows = clipOverlayRows(rows, contentWidth)
	rows = fitOverlayRows(rows, height, 2)
	body := lipgloss.JoinVertical(lipgloss.Left, rows...)
	box := panelStyle.Width(panelWidth).Align(lipgloss.Left).Render(body)
	return lipgloss.Place(maxInt(1, width), maxInt(1, height), lipgloss.Center, lipgloss.Center, box)
}

func taskHubExitHandoffItemState(item taskHubExitHandoffItem) string {
	switch item.State {
	case taskHubRunHandoffReady:
		return "待交接"
	case taskHubRunHandoffLaunching:
		return "正在交接"
	case taskHubRunHandoffSucceeded:
		return "已交接"
	case taskHubRunHandoffFailed:
		return "交接失败"
	default:
		return "无需交接"
	}
}

func taskHubExitHandoffItemDetail(item taskHubExitHandoffItem) string {
	if item.Error != "" {
		return item.Error
	}
	if !item.Run.Handoff.Enabled {
		return item.Run.Handoff.DisabledReason
	}
	if item.Result.Summary != "" {
		return item.Result.Summary
	}
	return taskHubRunDisplayState(item.Run)
}

func (m *model) requestTaskHubExitHandoff() tea.Cmd {
	if m.exitHandoff != nil {
		return nil
	}
	overlay, err := newTaskHubExitHandoffOverlay(m.taskHub.Snapshot.Runs)
	if err != nil {
		m.err = err
		return m.showToast("无法打开退出交接："+err.Error(), toastError)
	}
	m.exitHandoff = overlay
	if m.router != nil {
		m.router.PushOverlay(overlay)
	}
	m.focusMgr.Push(focusOverlay)
	return nil
}

func (m *model) closeTaskHubExitHandoff() {
	if m.exitHandoff == nil {
		return
	}
	m.exitHandoff = nil
	if m.router != nil {
		m.router.PopOverlay()
	}
	m.focusMgr.Pop()
}

func (m *model) updateTaskHubExitHandoffKey(msg tea.KeyMsg) tea.Cmd {
	overlay := m.exitHandoff
	if overlay == nil {
		return nil
	}
	switch msg.String() {
	case "up", "k":
		overlay.move(-1)
		return nil
	case "down", "j":
		overlay.move(1)
		return nil
	case " ", "space":
		overlay.toggleSelected()
		return nil
	case "esc", "q", "ctrl+q", "ctrl+c":
		if overlay.ExecutingID == "" {
			m.closeTaskHubExitHandoff()
		}
		return nil
	case "enter":
		return m.executeNextTaskHubExitHandoff()
	default:
		return nil
	}
}

func (m *model) executeNextTaskHubExitHandoff() tea.Cmd {
	overlay := m.exitHandoff
	if overlay == nil || overlay.ExecutingID != "" {
		return nil
	}
	if overlay.selectedCount() == 0 {
		return tea.Quit
	}
	executor, supported := m.lifecycle.(TaskHubRunHandoffExecutor)
	if !supported || executor == nil {
		overlay.Error = "当前生命周期服务未提供受控 child-worker handoff 接口"
		return m.showToast(overlay.Error, toastError)
	}
	index := overlay.pendingSelectedIndex()
	if index < 0 {
		return tea.Quit
	}
	item := &overlay.Items[index]
	item.State = taskHubRunHandoffLaunching
	item.Error = ""
	overlay.Error = ""
	overlay.ExecutingID = item.Request.HandoffOperationID
	request := item.Request
	return func() tea.Msg {
		result, err := executor.ExecuteTaskHubRunHandoff(m.ctx, request)
		return taskHubRunHandoffExecutedMsg{operationID: request.HandoffOperationID, result: result, err: err}
	}
}

func (m *model) applyTaskHubExitHandoffResult(message taskHubRunHandoffExecutedMsg) tea.Cmd {
	overlay := m.exitHandoff
	if overlay == nil || overlay.ExecutingID != message.operationID {
		return nil
	}
	index := overlay.itemIndexByOperationID(message.operationID)
	if index < 0 {
		return nil
	}
	overlay.ExecutingID = ""
	item := &overlay.Items[index]
	if message.err != nil {
		item.State = taskHubRunHandoffFailed
		item.Error = message.err.Error()
		overlay.Error = "交接未完成；可重放原操作确认其状态，或取消该 Run 后直接退出。"
		m.err = message.err
		return m.showToast("Run 交接失败；未退出 Task Hub", toastError)
	}
	if message.result.RunID != "" && message.result.RunID != item.Run.RunID {
		item.State = taskHubRunHandoffFailed
		item.Error = "交接回执的 Run ID 与已选择 Run 不一致"
		overlay.Error = item.Error
		return m.showToast("Run 交接回执不一致；未退出 Task Hub", toastError)
	}
	if message.result.OperationID != "" && message.result.OperationID != item.Request.HandoffOperationID {
		item.State = taskHubRunHandoffFailed
		item.Error = "交接回执的操作 ID 与已冻结操作不一致"
		overlay.Error = item.Error
		return m.showToast("Run 交接回执不一致；未退出 Task Hub", toastError)
	}
	item.State = taskHubRunHandoffSucceeded
	item.Result = message.result
	item.Error = ""
	m.err = nil
	if overlay.pendingSelectedIndex() >= 0 {
		return m.executeNextTaskHubExitHandoff()
	}
	return tea.Quit
}
