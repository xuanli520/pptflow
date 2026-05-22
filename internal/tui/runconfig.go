package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/xuanli520/p2r_tui/internal/pipeline"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/taskdocs"
)

type runConfigFocus int

const (
	runConfigFocusMode runConfigFocus = iota
	runConfigFocusStages
	runConfigFocusFrom
	runConfigFocusKeepRuntime
	runConfigFocusExtraDocs
	runConfigFocusSubmit
	runConfigFocusCancel
)

type runConfigAction int

const (
	runConfigActionPipeline runConfigAction = iota
	runConfigActionInspection
)

type runConfig struct {
	active        bool
	action        runConfigAction
	focus         runConfigFocus
	taskID        string
	mode          string
	refRun        string
	fromStage     string
	stages        map[string]bool
	stageIndex    int
	keepRuntime   bool
	extraDocs     []string
	attachedCount int
	input         textinput.Model
	err           string
}

func newRunConfig(taskID, mode, refRun, selectedStage string, keepRuntime bool, attachedCount int, action runConfigAction, configuredStages map[string][]string) runConfig {
	input := textinput.New()
	input.Placeholder = "/absolute/path/to/doc.md"
	input.Prompt = "路径: "
	input.CharLimit = 4096
	input.Width = 56

	cfg := runConfig{
		active:        true,
		action:        action,
		focus:         runConfigFocusMode,
		taskID:        taskID,
		mode:          empty(mode, "initial"),
		refRun:        refRun,
		keepRuntime:   keepRuntime,
		attachedCount: attachedCount,
		input:         input,
	}
	if stages, ok := configuredStageSet(mode, configuredStages); ok {
		cfg.stages = stages
	} else if mode == "recheck" {
		recheckStages := stageSet(affectedStages(selectedStage))
		if len(recheckStages) > 0 {
			cfg.stages = recheckStages
		}
	}
	cfg.syncInputFocus()
	return cfg
}

func (c *runConfig) syncInputFocus() {
	if c.focus == runConfigFocusExtraDocs {
		c.input.Focus()
		return
	}
	c.input.Blur()
}

func (c runConfig) selectedStage() string {
	return stageLetter(c.stageIndex)
}

func (c runConfig) hasExplicitStages() bool {
	return len(c.stages) > 0
}

func (c runConfig) toRunOptions(plan stagePlan) pipeline.RunOptions {
	opts := pipeline.RunOptions{
		Mode:        c.mode,
		RefRun:      c.refRun,
		From:        plan.fromStage,
		Stages:      append([]string(nil), plan.runStages...),
		KeepRuntime: c.keepRuntime,
	}
	if c.mode == "recheck" {
		opts.ExtraDocs = append([]string(nil), c.extraDocs...)
	}
	return opts
}

func (m app) handleRunConfigKey(msg tea.KeyMsg) (app, []tea.Cmd) {
	key := msg.String()
	var cmds []tea.Cmd
	m.runConfig.err = ""

	switch key {
	case "esc":
		m.runConfig = runConfig{}
		m.message = "已取消重跑"
		return m, cmds
	case "tab":
		m.runConfig.focus = (m.runConfig.focus + 1) % 7
		m.runConfig.syncInputFocus()
		return m, cmds
	case "shift+tab":
		m.runConfig.focus = (m.runConfig.focus + 6) % 7
		m.runConfig.syncInputFocus()
		return m, cmds
	case "up":
		if m.runConfig.focus == runConfigFocusStages {
			m.runConfig.stageIndex = clamp(m.runConfig.stageIndex-1, 0, len(model.AllStages())-1)
		}
		return m, cmds
	case "down":
		if m.runConfig.focus == runConfigFocusStages {
			m.runConfig.stageIndex = clamp(m.runConfig.stageIndex+1, 0, len(model.AllStages())-1)
		}
		return m, cmds
	case " ":
		m.toggleRunConfigFocused()
		return m, cmds
	case "enter":
		if m.runConfig.focus == runConfigFocusExtraDocs && strings.TrimSpace(m.runConfig.input.Value()) != "" {
			m.attachRunConfigDoc()
			return m, cmds
		}
		if m.runConfig.focus == runConfigFocusCancel {
			m.runConfig = runConfig{}
			m.message = "已取消重跑"
			return m, cmds
		}
		cmd := m.submitRunConfig()
		return m, append(cmds, cmd)
	}

	if m.runConfig.focus == runConfigFocusExtraDocs {
		var cmd tea.Cmd
		m.runConfig.input, cmd = m.runConfig.input.Update(msg)
		cmds = append(cmds, cmd)
	}
	return m, cmds
}

func (m *app) toggleRunConfigFocused() {
	switch m.runConfig.focus {
	case runConfigFocusMode:
		if m.runConfig.mode == "recheck" {
			m.runConfig.mode = "initial"
			m.runConfig.refRun = ""
			if stages, ok := configuredStageSet(m.runConfig.mode, m.cfg.Pipeline.DefaultStages); ok {
				m.runConfig.stages = stages
			} else {
				m.runConfig.stages = nil
			}
		} else {
			m.runConfig.mode = "recheck"
			m.syncRefSelection()
			m.runConfig.refRun = m.selectedRefRunCandidate()
			if stages, ok := configuredStageSet(m.runConfig.mode, m.cfg.Pipeline.DefaultStages); ok {
				m.runConfig.stages = stages
			} else if m.runConfig.fromStage == "" {
				m.runConfig.stages = defaultStageSet(m.runConfig.mode, m.rerunStageKey(), m.cfg.Pipeline.StaticOnly, nil)
			} else {
				m.runConfig.stages = nil
			}
		}
	case runConfigFocusStages:
		if m.runConfig.fromStage != "" {
			m.runConfig.err = "使用起始阶段时不能多选阶段"
			return
		}
		if m.runConfig.stages == nil {
			m.runConfig.stages = defaultStageSet(m.runConfig.mode, m.rerunStageKey(), m.cfg.Pipeline.StaticOnly, m.cfg.Pipeline.DefaultStages)
		}
		stage := m.runConfig.selectedStage()
		m.runConfig.stages[stage] = !m.runConfig.stages[stage]
	case runConfigFocusFrom:
		if len(m.runConfig.stages) > 0 {
			m.runConfig.err = "多选阶段时不能使用起始阶段"
			return
		}
		m.runConfig.fromStage = nextFromStage(m.runConfig.fromStage)
	case runConfigFocusKeepRuntime:
		m.runConfig.keepRuntime = !m.runConfig.keepRuntime
	case runConfigFocusSubmit:
		// Enter submits; Space keeps focus stable.
	case runConfigFocusCancel:
		m.runConfig = runConfig{}
		m.message = "已取消重跑"
	}
}

func (m *app) attachRunConfigDoc() {
	path := strings.TrimSpace(m.runConfig.input.Value())
	if path == "" {
		return
	}
	doc, err := attachRunConfigDoc(m.cfg, m.runConfig.taskID, path)
	if err != nil {
		m.runConfig.err = err.Error()
		return
	}
	m.runConfig.attachedCount = taskdocs.Count(m.cfg.ScanPath, m.runConfig.taskID)
	m.runConfig.input.SetValue("")
	m.message = fmt.Sprintf("已托管补充文档: %s", doc.OriginalName)
}

func (m *app) submitRunConfig() tea.Cmd {
	if m.runConfig.taskID == "" {
		m.runConfig.err = "未选择任务"
		return nil
	}
	if m.runConfig.mode == "recheck" && strings.TrimSpace(m.runConfig.refRun) == "" {
		m.syncRefSelection()
		m.runConfig.refRun = m.selectedRefRunCandidate()
	}
	if m.runConfig.mode == "recheck" && m.runConfig.refRun == "" {
		m.runConfig.err = "打回重检模式需要选择一个参考运行"
		return nil
	}
	plan := m.rerunStagePlan()
	if plan.blockedReason != "" {
		m.runConfig.err = plan.blockedReason
		return nil
	}
	opts := m.runConfig.toRunOptions(plan)
	taskID := m.runConfig.taskID
	var cmd tea.Cmd
	if m.runConfig.action == runConfigActionInspection {
		cmd = m.submitInspection(taskID, opts)
	} else {
		cmd = m.submitRun(taskID, opts)
	}
	m.runConfig = runConfig{}
	m.message = "正在提交流水线 job..."
	return cmd
}

func stageSet(stages []string) map[string]bool {
	result := map[string]bool{}
	for _, stage := range stages {
		stage = strings.ToUpper(strings.TrimSpace(stage))
		if stage != "" {
			result[stage] = true
		}
	}
	return result
}

func defaultStageSet(mode, selectedStage string, staticOnly bool, configured map[string][]string) map[string]bool {
	if stages, ok := configuredStageSet(mode, configured); ok {
		return stages
	}
	plan := stagePlanForMode(mode, selectedStage, staticOnly, nil, "")
	if len(plan.displayStages) == 0 {
		return map[string]bool{string(model.StageF): true}
	}
	return stageSet(plan.displayStages)
}

func configuredStageSet(mode string, configured map[string][]string) (map[string]bool, bool) {
	stages := configured[strings.ToLower(strings.TrimSpace(mode))]
	if len(stages) == 0 {
		return nil, false
	}
	return stageSet(stages), true
}

func nextFromStage(current string) string {
	order := append([]string{""}, model.AllStages()...)
	current = strings.ToUpper(strings.TrimSpace(current))
	for i, stage := range order {
		if stage == current {
			return order[(i+1)%len(order)]
		}
	}
	return string(model.StageA)
}
