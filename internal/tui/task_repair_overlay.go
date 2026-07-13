package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/purplevoid/harbor-factory/internal/app"
)

type TaskRepairOverlay struct {
	SourceWorkspace string
	TaskName        string
	Target          textinput.Model
	Feedback        textarea.Model
	Field           int
	Loading         bool
}

func NewTaskRepairOverlay(source, target, taskName string) *TaskRepairOverlay {
	targetInput := textinput.New()
	targetInput.Prompt = ""
	targetInput.CharLimit = 4096
	targetInput.Width = 60
	targetInput.SetValue(target)
	targetInput.Focus()
	feedback := textarea.New()
	feedback.Placeholder = "粘贴题目方机审结果、人工审核意见，或写明希望 Codex 如何返修"
	feedback.CharLimit = 20000
	feedback.SetWidth(72)
	feedback.SetHeight(8)
	feedback.Blur()
	return &TaskRepairOverlay{SourceWorkspace: source, TaskName: taskName, Target: targetInput, Feedback: feedback}
}

func (o *TaskRepairOverlay) Init() tea.Cmd                  { return nil }
func (o *TaskRepairOverlay) Update(tea.Msg) (bool, tea.Cmd) { return true, nil }
func (o *TaskRepairOverlay) Focus()                         {}
func (o *TaskRepairOverlay) Blur()                          { o.Target.Blur(); o.Feedback.Blur() }
func (o *TaskRepairOverlay) HandleKey(tea.KeyMsg) tea.Cmd   { return nil }
func (o *TaskRepairOverlay) ZIndex() int                    { return 55 }
func (o *TaskRepairOverlay) InterceptsAllKeys() bool        { return true }

func (o *TaskRepairOverlay) View(width, height int) string {
	boxWidth := boundedPanelWidth(width, 52, 92)
	lineWidth := styleContentWidth(boxWidth, panelStyle)
	feedbackHeight := 8
	if height > 0 && height < 20 {
		feedbackHeight = maxInt(1, height-9)
	}
	o.Feedback.SetWidth(lineWidth)
	o.Feedback.SetHeight(feedbackHeight)
	rows := []string{
		sectionStyle.Render(clipDisplay("外部审查题目返修", lineWidth)), "",
		clipDisplay("源工作区: "+redactSingleLineUI(o.SourceWorkspace), lineWidth),
		inputFieldLine(o.Field == 0, "目标工作区", o.Target, lineWidth), "",
		fieldLine(o.Field == 1, "机审 / 人工审核反馈", "", lineWidth),
		o.Feedback.View(), "",
		subtleStyle.Render(clipDisplay("Tab 切换字段  Ctrl+S 创建返修运行  Esc 取消", lineWidth)),
	}
	if o.Loading {
		rows = append(rows, subtleStyle.Render(clipDisplay("正在创建返修工作区...", lineWidth)))
	}
	rows = flattenRenderedLines(rows)
	selectedRow := 3
	if o.Field == 1 {
		selectedRow = 5
	}
	rows = fitOverlayRows(rows, height, selectedRow)
	box := panelStyle.Width(boxWidth).Render(strings.Join(rows, "\n"))
	return lipgloss.Place(maxInt(1, width), maxInt(1, height), lipgloss.Center, lipgloss.Center, box)
}

func (m *model) updateTaskRepairKey(msg tea.KeyMsg) tea.Cmd {
	o := m.taskRepair
	if o == nil || o.Loading {
		return nil
	}
	switch msg.String() {
	case "esc":
		m.closeTaskRepair()
		return nil
	case "tab", "shift+tab":
		o.Field = 1 - o.Field
		if o.Field == 0 {
			o.Feedback.Blur()
			o.Target.Focus()
			return o.Target.Cursor.BlinkCmd()
		}
		o.Target.Blur()
		o.Feedback.Focus()
		return o.Feedback.Cursor.BlinkCmd()
	case "ctrl+s":
		target := strings.TrimSpace(o.Target.Value())
		feedback := strings.TrimSpace(o.Feedback.Value())
		if target == "" {
			m.err = fmt.Errorf("目标工作区不能为空")
			return nil
		}
		if feedback == "" {
			m.err = fmt.Errorf("请填写题目方机审、人工审核意见或返修指导")
			return nil
		}
		o.Loading = true
		config := app.CloneWorkspaceOptions{SourceWorkspace: o.SourceWorkspace, TargetWorkspace: target, RuntimeOptions: m.runtimeOpts}
		return func() tea.Msg {
			opts, manifest, err := app.CloneRunnerOptions(config)
			if err == nil {
				opts.RepairGuidance = feedback
				opts.RepairSource = "external_review"
				opts.AutoApprove = false
			}
			return clonePreparedMsg{opts: opts, manifest: manifest, err: err}
		}
	}
	var cmd tea.Cmd
	if o.Field == 0 {
		o.Target, cmd = o.Target.Update(msg)
	} else {
		o.Feedback, cmd = o.Feedback.Update(msg)
	}
	return cmd
}

func (m *model) closeTaskRepair() {
	m.taskRepair = nil
	if m.router != nil {
		m.router.PopOverlay()
	}
	m.focusMgr.Pop()
}
