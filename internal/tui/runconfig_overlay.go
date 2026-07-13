package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/purplevoid/harbor-factory/internal/app"
)

type RunConfigOverlay struct {
	SourceWorkspace string
	TaskName        string
	Target          textinput.Model
	Field           int
	ReuseDocker     bool
	ReuseQuality    bool
	ReuseSimilarity bool
	ReuseHarbor     bool
	AutoApprove     bool
	Background      bool
	Loading         bool
}

func NewRunConfigOverlay(source, target, taskName string) *RunConfigOverlay {
	input := textinput.New()
	input.Prompt = ""
	input.CharLimit = 4096
	input.Width = 48
	input.SetValue(target)
	input.Focus()
	return &RunConfigOverlay{SourceWorkspace: source, TaskName: taskName, Target: input}
}

func (o *RunConfigOverlay) Init() tea.Cmd                  { return nil }
func (o *RunConfigOverlay) Update(tea.Msg) (bool, tea.Cmd) { return true, nil }
func (o *RunConfigOverlay) Focus()                         { o.Target.Focus() }
func (o *RunConfigOverlay) Blur()                          { o.Target.Blur() }
func (o *RunConfigOverlay) HandleKey(tea.KeyMsg) tea.Cmd   { return nil }
func (o *RunConfigOverlay) ZIndex() int                    { return 50 }
func (o *RunConfigOverlay) InterceptsAllKeys() bool        { return true }

func (o *RunConfigOverlay) View(width, height int) string {
	if o == nil {
		return ""
	}
	boxWidth := boundedPanelWidth(width, 46, 82)
	lineWidth := styleContentWidth(boxWidth, panelStyle)
	rows := []string{
		sectionStyle.Render("重跑配置"),
		"",
		clipDisplay("源工作区: "+redactSingleLineUI(o.SourceWorkspace), lineWidth),
		inputFieldLine(o.Field == 0, "目标工作区", o.Target, lineWidth),
		"",
		fieldLine(o.Field == 1, "复用 Docker 验证结果", checkbox(o.ReuseDocker), lineWidth),
		fieldLine(o.Field == 2, "复用质量检查结果", checkbox(o.ReuseQuality), lineWidth),
		fieldLine(o.Field == 3, "复用相似度检查结果", checkbox(o.ReuseSimilarity), lineWidth),
		fieldLine(o.Field == 4, "复用 Harbor 运行结果", checkbox(o.ReuseHarbor), lineWidth),
		fieldLine(o.Field == 5, "自动批准全部审查关卡", checkbox(o.AutoApprove), lineWidth),
		fieldLine(o.Field == 6, "后台并行运行", checkbox(o.Background), lineWidth),
		"",
		subtleStyle.Render(clipDisplay("Tab/↑↓ 切换  Space 开关  Enter 开始重跑  Esc 取消", lineWidth)),
	}
	if o.Loading {
		rows = append(rows, subtleStyle.Render(clipDisplay("正在准备新工作区...", lineWidth)))
	}
	rows = clipOverlayRows(rows, lineWidth)
	selectedRow := 3
	if o.Field > 0 {
		selectedRow = 4 + o.Field
	}
	rows = fitOverlayRows(rows, height, selectedRow)
	box := panelStyle.Width(boxWidth).Render(strings.Join(rows, "\n"))
	return lipgloss.Place(maxInt(1, width), maxInt(1, height), lipgloss.Center, lipgloss.Center, box)
}

func fieldLine(selected bool, label, value string, lineWidth int) string {
	prefix := "  "
	if selected {
		prefix = selectedStyle.Render("> ")
	}
	labelWidth := clampInt(lineWidth/2, 1, 28)
	valueWidth := maxInt(0, lineWidth-2-labelWidth-1)
	return clipDisplay(prefix+padRightDisplay(label, labelWidth)+" "+clipDisplay(value, valueWidth), lineWidth)
}

func inputFieldLine(selected bool, label string, input textinput.Model, lineWidth int) string {
	labelWidth := clampInt(lineWidth/2, 1, 28)
	valueWidth := maxInt(0, lineWidth-2-labelWidth-1)
	value := boxedTextInput(input, valueWidth)
	if sanitized := redactSingleLineUI(input.Value()); sanitized != input.Value() {
		value = boxedText(sanitized, valueWidth)
	}
	return fieldLine(selected, label, value, lineWidth)
}

func (m *model) updateRunConfigKey(msg tea.KeyMsg) tea.Cmd {
	o := m.runConfig
	if o == nil || o.Loading {
		return nil
	}
	switch msg.String() {
	case "esc":
		m.closeRunConfig()
		return nil
	case "tab", "down":
		o.Field = (o.Field + 1) % 7
	case "shift+tab", "up":
		o.Field = (o.Field + 6) % 7
	case " ":
		if o.Field == 0 {
			var cmd tea.Cmd
			o.Target, cmd = o.Target.Update(msg)
			return cmd
		}
		o.toggle()
	case "enter":
		target := strings.TrimSpace(o.Target.Value())
		if target == "" {
			m.err = fmt.Errorf("目标工作区不能为空")
			return nil
		}
		o.Loading = true
		config := app.CloneWorkspaceOptions{
			SourceWorkspace:  o.SourceWorkspace,
			TargetWorkspace:  target,
			ReuseDocker:      o.ReuseDocker,
			ReuseQuality:     o.ReuseQuality,
			ReuseSimilarity:  o.ReuseSimilarity,
			ReuseHarbor:      o.ReuseHarbor,
			AutoApproveGates: o.AutoApprove,
			RuntimeOptions:   m.runtimeOpts,
		}
		return func() tea.Msg {
			opts, manifest, err := app.CloneRunnerOptions(config)
			return clonePreparedMsg{opts: opts, manifest: manifest, background: o.Background, err: err}
		}
	default:
		if o.Field == 0 {
			var cmd tea.Cmd
			o.Target, cmd = o.Target.Update(msg)
			return cmd
		}
	}
	if o.Field == 0 {
		o.Target.Focus()
	} else {
		o.Target.Blur()
	}
	return nil
}

func (o *RunConfigOverlay) toggle() {
	switch o.Field {
	case 1:
		o.ReuseDocker = !o.ReuseDocker
	case 2:
		o.ReuseQuality = !o.ReuseQuality
	case 3:
		o.ReuseSimilarity = !o.ReuseSimilarity
	case 4:
		o.ReuseHarbor = !o.ReuseHarbor
	case 5:
		o.AutoApprove = !o.AutoApprove
	case 6:
		o.Background = !o.Background
	}
}

func (m *model) closeRunConfig() {
	m.runConfig = nil
	if m.router != nil {
		m.router.PopOverlay()
	}
	m.focusMgr.Pop()
}
