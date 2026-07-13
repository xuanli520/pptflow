package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// RunControlOverlay is a read/preview surface for durable run control. It
// never turns a keypress into a process-context cancellation or a persistent
// ControlOperation; mutation inputs and confirmation remain outside this TUI
// overlay.
type RunControlOverlay struct {
	RunID                  string
	TaskID                 string
	RevisionID             string
	Workspace              string
	State                  string
	Stage                  string
	StageAttemptID         string
	StageExecutionState    string
	ControlStatus          string
	CheckpointSequence     uint64
	ExecutionEpoch         int
	GracePeriod            time.Duration
	OperationID            string
	OperationAction        TaskHubRunControlAction
	CheckpointID           string
	QuotaSettlementID      string
	RuntimeReceiptCount    int
	ExternalOutcomeUnknown bool
	FailureReason          string
	Expected               TaskHubControlCheckpoint
	Actions                []TaskHubRunControlActionState
	SelectedAction         TaskHubRunControlAction
	Preview                *TaskHubPlanPreview
}

func newLifecycleRunControlOverlay(run TaskHubRun) *RunControlOverlay {
	state := strings.TrimSpace(run.ExecutionState)
	if state == "" {
		state = "未知"
	}
	control := run.Control.Clone()
	return &RunControlOverlay{
		RunID:                  strings.TrimSpace(run.RunID),
		TaskID:                 strings.TrimSpace(run.TaskID),
		RevisionID:             strings.TrimSpace(run.RevisionID),
		State:                  state,
		Stage:                  strings.TrimSpace(run.Stage),
		StageAttemptID:         strings.TrimSpace(control.StageAttemptID),
		StageExecutionState:    strings.TrimSpace(control.StageExecutionState),
		ControlStatus:          strings.TrimSpace(run.ControlStatus),
		CheckpointSequence:     control.CheckpointSequence,
		ExecutionEpoch:         control.ExecutionEpoch,
		GracePeriod:            control.GracePeriod,
		OperationID:            strings.TrimSpace(control.OperationID),
		OperationAction:        control.OperationAction,
		CheckpointID:           strings.TrimSpace(control.CheckpointID),
		QuotaSettlementID:      strings.TrimSpace(control.QuotaSettlementID),
		RuntimeReceiptCount:    control.RuntimeReceiptCount,
		ExternalOutcomeUnknown: control.ExternalOutcomeUnknown,
		FailureReason:          strings.TrimSpace(control.FailureReason),
		Expected:               control.Expected,
		Actions:                append([]TaskHubRunControlActionState(nil), control.Actions...),
	}
}

func newRunControlOverlay(runID, workspace string, done, readOnly bool) *RunControlOverlay {
	state := "运行中"
	switch {
	case done:
		state = "已结束"
	case readOnly:
		state = "只读快照"
	}
	return &RunControlOverlay{
		RunID:     strings.TrimSpace(runID),
		Workspace: strings.TrimSpace(workspace),
		State:     state,
	}
}

// Clone keeps Bubble Tea value updates from sharing a mutable preview or
// capability slice with an earlier model state.
func (o *RunControlOverlay) Clone() *RunControlOverlay {
	if o == nil {
		return nil
	}
	clone := *o
	clone.Actions = append([]TaskHubRunControlActionState(nil), o.Actions...)
	if o.Preview != nil {
		preview := o.Preview.Clone()
		clone.Preview = &preview
	}
	return &clone
}

func (o *RunControlOverlay) actionState(action TaskHubRunControlAction) TaskHubRunControlActionState {
	if o == nil {
		return TaskHubRunControlActionState{Action: action, DisabledReason: "运行控制视图不可用"}
	}
	for _, state := range o.Actions {
		if state.Action == action {
			return state
		}
	}
	return TaskHubRunControlActionState{Action: action, DisabledReason: "当前 Run 未声明此控制操作可用"}
}

func (o *RunControlOverlay) selectAction(action TaskHubRunControlAction) {
	if o == nil || !taskHubRunControlActionKnown(action) {
		return
	}
	if o.SelectedAction != action {
		o.Preview = nil
	}
	o.SelectedAction = action
}

func (o *RunControlOverlay) selectReturn() {
	if o == nil {
		return
	}
	if o.SelectedAction != "" {
		o.Preview = nil
	}
	o.SelectedAction = ""
}

func (o *RunControlOverlay) selectedIsReturn() bool {
	return o == nil || o.SelectedAction == ""
}

func (o *RunControlOverlay) lifecycleControlAvailable() bool {
	return o != nil && (o.TaskID != "" || len(o.Actions) > 0)
}

func (o *RunControlOverlay) Init() tea.Cmd                { return nil }
func (o *RunControlOverlay) Focus()                       {}
func (o *RunControlOverlay) Blur()                        {}
func (o *RunControlOverlay) ZIndex() int                  { return 100 }
func (o *RunControlOverlay) InterceptsAllKeys() bool      { return true }
func (o *RunControlOverlay) HandleKey(tea.KeyMsg) tea.Cmd { return nil }
func (o *RunControlOverlay) Update(tea.Msg) (bool, tea.Cmd) {
	return true, nil
}

func (o *RunControlOverlay) View(width, height int) string {
	if o == nil {
		return ""
	}
	runID := redactSingleLineUI(o.RunID)
	if runID == "" {
		runID = "尚未分配"
	}
	rows := []string{
		sectionStyle.Render("运行控制"),
		"",
		"目标 Run：" + runID,
		"状态：" + o.State,
	}
	if workspace := redactSingleLineUI(o.Workspace); workspace != "" {
		rows = append(rows, "工作区："+workspace)
	}
	if o.Stage != "" {
		stage := o.Stage
		if o.StageExecutionState != "" {
			stage += " / " + o.StageExecutionState
		}
		rows = append(rows, "当前阶段："+stage)
	}
	if o.CheckpointSequence > 0 {
		rows = append(rows, "控制 checkpoint：序列 "+fmt.Sprintf("%d", o.CheckpointSequence)+" / epoch "+fmt.Sprintf("%d", o.ExecutionEpoch))
	}
	if o.GracePeriod > 0 {
		rows = append(rows, "冻结 grace period："+o.GracePeriod.String())
	}
	if o.ControlStatus != "" {
		rows = append(rows, "控制状态："+o.ControlStatus)
	}
	if o.OperationID != "" {
		operation := "最近操作：" + emptyDash(string(o.OperationAction))
		if o.ControlStatus != "" {
			operation += " / " + o.ControlStatus
		}
		rows = append(rows, operation)
	}
	if o.CheckpointID != "" {
		rows = append(rows, "最近 checkpoint："+o.CheckpointID)
	}
	if o.RuntimeReceiptCount > 0 {
		rows = append(rows, fmt.Sprintf("runtime ack：%d 条 receipt", o.RuntimeReceiptCount))
	}
	if o.QuotaSettlementID != "" {
		rows = append(rows, "quota settlement："+o.QuotaSettlementID)
	}
	if o.ExternalOutcomeUnknown {
		rows = append(rows, warnStyle.Render("未决外部副作用：需要 reconcile"))
	} else if o.OperationID != "" {
		rows = append(rows, "未决外部副作用：无")
	}
	if o.FailureReason != "" {
		rows = append(rows, failStyle.Render("控制失败："+o.FailureReason))
	}

	rows = append(rows, "")
	selectedRow := len(rows)
	rows = append(rows, o.choiceLine("  返回并保持运行  ", "", ""))
	if o.lifecycleControlAvailable() {
		for _, item := range []struct {
			action TaskHubRunControlAction
			label  string
		}{
			{TaskHubRunControlPause, "[P] 暂停运行"},
			{TaskHubRunControlCancelStage, "[K] 取消选中阶段"},
			{TaskHubRunControlTerminate, "[S] 终止本次运行"},
		} {
			state := o.actionState(item.action)
			reason := ""
			if !state.Enabled {
				reason = state.DisabledReason
				if reason == "" {
					reason = "生命周期服务未提供可提交条件"
				}
			}
			if o.SelectedAction == item.action {
				selectedRow = len(rows)
			}
			rows = append(rows, o.choiceLine(item.label, item.action, reason))
		}
	}
	if o.Preview != nil {
		preview := o.Preview
		rows = append(rows, "", sectionStyle.Render("影响预览"), preview.Title, preview.Summary)
		rows = append(rows, taskHubPlanExplanationRows(*preview, styleContentWidth(boundedPanelWidth(width, 38, 72), panelStyle))...)
	}
	rows = append(rows, "")
	if o.lifecycleControlAvailable() {
		rows = append(rows, subtleStyle.Render("[P/K/S] 选择  [Enter] 查看影响预览  [Esc] 返回"))
	} else {
		rows = append(rows, subtleStyle.Render("[Enter/Esc] 返回"))
	}
	rows = clipOverlayRows(rows, styleContentWidth(boundedPanelWidth(width, 38, 72), panelStyle))
	rows = fitOverlayRows(rows, height, selectedRow)
	body := lipgloss.JoinVertical(lipgloss.Center, rows...)
	box := panelStyle.Width(boundedPanelWidth(width, 38, 72)).Align(lipgloss.Center).Render(body)
	return lipgloss.Place(maxInt(1, width), maxInt(1, height), lipgloss.Center, lipgloss.Center, box)
}

func (o *RunControlOverlay) choiceLine(label string, action TaskHubRunControlAction, reason string) string {
	line := label
	if reason != "" {
		line += "（" + reason + "）"
	}
	if (action == "" && o.selectedIsReturn()) || (action != "" && o.SelectedAction == action) {
		return selectedStyle.Render(line)
	}
	return line
}
