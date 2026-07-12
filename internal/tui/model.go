package tui

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/commandlog"
	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/harborrun"
	"github.com/purplevoid/harbor-factory/internal/harbor/nodes"
	"github.com/purplevoid/harbor-factory/internal/harbor/sanitize"
)

type viewMode int

const (
	viewStart viewMode = iota
	viewOverview
	viewGate
	viewNodeDetail
	viewLogs
	viewDone
)

type startMode int

const (
	startExistingTask startMode = iota
	startGenerateTask
)

type startField int

const (
	startFieldMode startField = iota
	startFieldTaskDir
	startFieldRepoURL
	startFieldCommit
	startFieldWorkspace
	startFieldTaskOutput
	startFieldTestsAnalysis
	startFieldQwenResult
	startFieldOpusResult
	startFieldQwenScreenshot
	startFieldOpusScreenshot
	startFieldQualityCheck
	startFieldQualityAgent
	startFieldSimilarityCheck
	startFieldSimilarityGitHub
	startFieldSimilarityThreshold
	startFieldHistoryDirs
	startFieldTB3Dirs
	startFieldOutput
	startFieldVerifyDocker
	startFieldRunHarbor
	startFieldHarborAgent
	startFieldQwenModel
	startFieldOpusModel
	startFieldQwenHarborBaseURL
	startFieldOpusHarborBaseURL
	startFieldHarborTimeout
	startFieldHarborSetupTimeout
	startFieldHarborPreflight
	startFieldHarborConcurrency
	startFieldHarborAttempts
	startFieldHarborInfraRetries
	startFieldPackage
	startFieldTaskName
	startFieldCodeLang
	startFieldTaskType
	startFieldApplication
	startFieldAHT
	startFieldDescription
	startFieldZeroToOne
	startFieldCodexModel
	startFieldCodexReasoning
	startFieldCodexPath
	startFieldAgentTimeout
)

type model struct {
	ctx    context.Context
	cancel context.CancelFunc
	runner *app.Runner
	opts   app.RunnerOptions

	width  int
	height int
	view   viewMode

	events   []domain.RunnerEvent
	nodes    map[string]domain.RunnerEvent
	summary  domain.RunSummary
	err      error
	notice   string
	done     bool
	readOnly bool

	activeGate         *domain.GateRequest
	gateNotes          string
	gateEditingNote    bool
	editedFiles        map[string]string
	selectedNode       string
	selectedArtifact   int
	selectedLogFile    int
	logFileScroll      int
	logTail            bool
	startMode          startMode
	startField         startField
	startStep          startStep
	selectedStartGroup startGroup

	// UI components and navigation are explicit sub-models. Workflow/domain
	// state above remains the single source of truth.
	router           *pageRouter
	focusMgr         focusManager
	spinner          spinner.Model
	startInputs      map[startField]textinput.Model
	dirtyStartInputs map[startField]bool
	startCollapsed   map[startGroup]bool
	notesInput       textarea.Model
	searchInput      textinput.Model
	detailViewport   viewport.Model
	overviewTable    table.Model
	overviewRowIDs   []string
	confirm          *ConfirmDialog
	toast            toastState
	helpVisible      bool
	searching        bool
	filter           string
	pathSuggestions  []string
	detailScroll     int
}
type fileSnapshot struct {
	exists bool
	size   int64
	hash   string
}

func initialModel(ctx context.Context, cancel context.CancelFunc, opts app.RunnerOptions) model {
	opts.AutoApprove = false
	runner := app.NewRunner(opts)
	return initModelComponents(model{
		ctx:    ctx,
		cancel: cancel,
		runner: runner,
		opts:   opts,
		view:   viewOverview,
		nodes:  map[string]domain.RunnerEvent{},
	})
}

func initialStartModel(ctx context.Context, cancel context.CancelFunc, opts app.RunnerOptions) model {
	opts = applyStartDefaults(opts)
	opts.AutoApprove = false
	if strings.TrimSpace(opts.Workspace) == "" {
		opts.Workspace = defaultWorkspace("")
	}
	if strings.TrimSpace(opts.OutputDir) == "" {
		opts.OutputDir = defaultOutputDir("")
	}
	mode := startExistingTask
	if opts.Generate {
		mode = startGenerateTask
	}
	return initModelComponents(model{
		ctx:        ctx,
		cancel:     cancel,
		opts:       opts,
		view:       viewStart,
		nodes:      map[string]domain.RunnerEvent{},
		startMode:  mode,
		startField: startFieldMode,
	})
}

func initialWorkspaceModel(ctx context.Context, cancel context.CancelFunc, opts app.RunnerOptions) model {
	if loaded, _, err := app.LoadRunnerOptions(defaultWorkspace(opts.Workspace)); err == nil {
		loaded.AutoApprove = false
		opts = loaded
	}
	opts.AutoApprove = false
	summary, events := loadWorkspaceState(defaultWorkspace(opts.Workspace))
	nodes := map[string]domain.RunnerEvent{}
	for _, event := range events {
		if event.NodeID != "" {
			nodes[event.NodeID] = event
		}
	}
	activeGate := activeGateFromSnapshot(summary, events, nodes)
	selected := ""
	for _, id := range nodeOrder() {
		if _, ok := nodes[id]; ok {
			selected = id
			break
		}
	}
	done := !summary.FinishedAt.IsZero() || summary.Status == "succeeded" || summary.Status == "failed"
	view := viewOverview
	if done {
		view = viewDone
	} else if activeGate != nil {
		view = viewGate
	}
	return initModelComponents(model{
		ctx:          ctx,
		cancel:       cancel,
		opts:         opts,
		view:         view,
		events:       events,
		nodes:        nodes,
		summary:      summary,
		done:         done,
		readOnly:     true,
		activeGate:   activeGate,
		selectedNode: selected,
	})
}

func (m model) updateStartKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if m.toggleStartGroup(key) {
		return m, nil
	}
	switch key {
	case "ctrl+q", "ctrl+c":
		m.cancelRun()
		return m, tea.Quit
	case "q":
		if isTextStartField(m.startField) {
			return m, m.updateFocusedStartInput(msg)
		}
		m.cancelRun()
		return m, tea.Quit
	case "tab", "down":
		m.selectNextStartField()
		return m, m.focusStartInput(m.startField)
	case "shift+tab", "up":
		m.selectPrevStartField()
		return m, m.focusStartInput(m.startField)
	case "ctrl+left":
		if m.startStep == startStepAdvanced {
			m.selectAdvancedGroup(-1)
			return m, nil
		}
	case "ctrl+right":
		if m.startStep == startStepAdvanced {
			m.selectAdvancedGroup(1)
			return m, nil
		}
	case "esc":
		if m.startStep == startStepAdvanced {
			m.startStep = startStepBasic
			m.startField = startFieldMode
			m.focusStartInput(m.startField)
			m.err = nil
			return m, nil
		}
	case "left", "right":
		if m.startField == startFieldMode {
			m.toggleStartMode()
			return m, nil
		}
		return m, m.updateFocusedStartInput(msg)
	case " ":
		if m.startField == startFieldMode {
			m.toggleStartMode()
			return m, nil
		}
		if m.toggleStartBool() {
			return m, nil
		}
		return m, m.updateFocusedStartInput(msg)
	case "ctrl+space", "ctrl+@":
		return m, m.completeFocusedPath()
	case "ctrl+u":
		if isTextStartField(m.startField) {
			ti := m.startInputs[m.startField]
			ti.SetValue("")
			m.startInputs[m.startField] = ti
			m.syncStartField(m.startField, "")
		}
		return m, nil
	case "enter":
		if m.startStep == startStepBasic {
			if err := m.validateStartBasic(); err != nil {
				m.err = err
				return m, nil
			}
			m.startStep = startStepAdvanced
			m.selectedStartGroup = startGroupHarbor
			for _, group := range advancedGroups() {
				m.startCollapsed[group] = group != startGroupHarbor
			}
			m.err = nil
			fields := m.activeStartFields()
			if len(fields) > 0 {
				m.startField = fields[0]
				return m, m.focusStartInput(m.startField)
			}
			return m, nil
		}
		return m.launchStartWorkflow()
	default:
		if isTextStartField(m.startField) {
			return m, m.updateFocusedStartInput(msg)
		}
	}
	return m, nil
}

func (m model) launchStartWorkflow() (tea.Model, tea.Cmd) {
	if err := m.validateDirtyStartInputs(); err != nil {
		m.err = err
		return m, nil
	}
	opts, err := m.startOptions()
	if err != nil {
		m.err = err
		return m, nil
	}
	m = m.startRunner(opts)
	return m, tea.Batch(m.runWorkflow(), m.waitEvent(), m.refreshWorkspace(), m.spinner.Tick)
}

func (m model) updateGateKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.activeGate == nil {
		m.view = viewOverview
		return m, nil
	}
	if m.readOnly {
		switch msg.String() {
		case "tab":
			if idx, _, ok := m.selectedGateArtifact(); ok {
				m.selectedArtifact = (idx + 1) % len(m.visibleGateArtifacts(m.activeGate))
			}
		case "shift+tab":
			m.selectPrevArtifact()
		case "1":
			m.view = viewOverview
		case "3":
			m.view = viewLogs
		case "q", "ctrl+c":
			m.cancelRun()
			return m, tea.Quit
		case "a", "ctrl+a", "r", "ctrl+r", "v", "ctrl+v", "n", "ctrl+n", "e":
			m.err = fmt.Errorf("工作区快照为只读，当前运行由另一个 Factory 进程持有")
		}
		return m, nil
	}
	if m.gateEditingNote {
		switch msg.String() {
		case "esc", "ctrl+s":
			m.gateEditingNote = false
			m.notesInput.Blur()
			m.gateNotes = m.notesInput.Value()
			m.focusMgr.Pop()
			return m, nil
		default:
			var cmd tea.Cmd
			m.notesInput, cmd = m.notesInput.Update(msg)
			m.gateNotes = m.notesInput.Value()
			return m, cmd
		}
	}
	switch msg.String() {
	case "a", "ctrl+a":
		if blockers := gateBlockingChecklist(m.activeGate.Checklist); len(blockers) > 0 {
			m.err = fmt.Errorf("无法批准：存在未通过的关键检查项：%s", strings.Join(blockers, "；"))
			return m, nil
		}
		gate := m.activeGate
		m.activeGate = nil
		m.err = nil
		dialog := newConfirmDialog(confirmApprove, "确认批准", "批准后工作流将进入下一阶段，是否继续？")
		dialog.Gate = gate
		m.openConfirm(dialog)
		return m, func() tea.Msg { return confirmOpenedMsg{} }
	case "r", "ctrl+r":
		gate := m.activeGate
		m.activeGate = nil
		m.err = nil
		dialog := newConfirmDialog(confirmReject, "确认拒绝", "拒绝将中止或退回当前审查关卡，是否继续？")
		dialog.Gate = gate
		m.openConfirm(dialog)
		return m, func() tea.Msg { return confirmOpenedMsg{} }
	case "v", "ctrl+v":
		if m.activeGate.GateID != nodes.FinalReview && m.activeGate.GateID != nodes.ResultReview {
			m.err = fmt.Errorf("修订/刷新仅在最终审查和结果审查中可用")
			return m, nil
		}
		gate := m.activeGate
		decision := m.makeGateDecision(false)
		decision.Action = "revise"
		m.activeGate = nil
		m.err = nil
		m.view = viewOverview
		return m, m.submitDecision(decision, gate)
	case "n", "ctrl+n":
		m.gateEditingNote = true
		m.notesInput.SetValue(m.gateNotes)
		m.notesInput.Focus()
		m.focusMgr.Push(focusGateNotes)
		return m, m.notesInput.Cursor.BlinkCmd()
	case "tab":
		if idx, _, ok := m.selectedGateArtifact(); ok {
			m.selectedArtifact = (idx + 1) % len(m.visibleGateArtifacts(m.activeGate))
		}
		return m, nil
	case "shift+tab":
		m.selectPrevArtifact()
		return m, nil
	case "e":
		_, artifact, ok := m.selectedGateArtifact()
		if !ok {
			return m, nil
		}
		path, err := m.safeEditableArtifactPath(artifact.Path)
		if err != nil {
			m.err = err
			return m, nil
		}
		dialog := newConfirmDialog(confirmEditArtifact, "确认编辑工件", "将使用外部编辑器打开：\n"+path)
		dialog.Path = path
		m.openConfirm(dialog)
		return m, func() tea.Msg { return confirmOpenedMsg{} }
	case "1":
		m.view = viewOverview
		return m, nil
	case "3":
		m.view = viewLogs
		return m, nil
	case "q", "ctrl+c":
		m.cancelRun()
		return m, tea.Quit
	}
	return m, nil
}

func (m model) updateLogsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		m.cancelRun()
		return m, tea.Quit
	case "tab":
		m.selectNextArtifact()
		return m, nil
	case "shift+tab":
		m.selectPrevArtifact()
		return m, nil
	case "x":
		if m.readOnly {
			m.err = fmt.Errorf("工作区快照为只读，当前运行由另一个 Factory 进程持有")
		} else if m.runner != nil && m.runner.CancelNode(m.selectedNode) {
			m.notice = "已请求取消 " + m.selectedNode + "；其他模型阶段可能继续运行。"
			m.err = nil
		}
		return m, nil
	case "j", "down":
		m.scrollLogFile(1)
		return m, nil
	case "k", "up":
		m.scrollLogFile(-1)
		return m, nil
	case "pgdown":
		m.scrollLogFile(logPageStep(m.height))
		return m, nil
	case "pgup":
		m.scrollLogFile(-logPageStep(m.height))
		return m, nil
	case "g", "home":
		m.logTail = false
		m.logFileScroll = 0
		return m, nil
	case "G", "end":
		m.logTail = true
		m.logFileScroll = 0
		return m, nil
	case "t":
		m.logTail = !m.logTail
		m.logFileScroll = 0
		return m, nil
	case "1":
		m.view = viewOverview
		return m, nil
	case "2", "d":
		if m.activeGate != nil {
			m.view = viewGate
		} else {
			m.view = viewNodeDetail
		}
		return m, nil
	case "3", "l":
		m.view = viewLogs
		return m, nil
	case "4":
		if m.done {
			m.view = viewDone
		} else {
			return m, m.showToast("运行尚未完成", toastWarning)
		}
		return m, nil
	}
	return m, nil
}

func (m model) makeGateDecision(approved bool) domain.GateDecision {
	action := "reject"
	if approved {
		action = "approve"
	}
	decision := domain.GateDecision{
		Action:      action,
		Approved:    approved,
		Notes:       m.gateNotes,
		EditedFiles: m.editedFiles,
		DecidedAt:   time.Now().UTC(),
	}
	if m.activeGate == nil {
		return sanitize.GateDecision(decision)
	}
	decision.RequestID = m.activeGate.RequestID
	decision.GateID = m.activeGate.GateID
	return sanitize.GateDecision(decision)
}

func (m model) cancelRun() {
	if m.cancel != nil {
		m.cancel()
	}
}

func (m model) submitDecision(decision domain.GateDecision, gate *domain.GateRequest) tea.Cmd {
	return func() tea.Msg {
		decision = sanitize.GateDecision(decision)
		if m.runner != nil {
			m.runner.SubmitGateDecision(decision)
			return gateDecisionWrittenMsg{gate: gate, decision: decision}
		}
		path, err := writeWorkspaceGateDecision(defaultWorkspace(m.opts.Workspace), decision)
		return gateDecisionWrittenMsg{path: path, gate: gate, decision: decision, err: err}
	}
}

func writeWorkspaceGateDecision(workspace string, decision domain.GateDecision) (string, error) {
	decision = sanitize.GateDecision(decision)
	phase, ok := reviewGatePhase(decision.GateID)
	if !ok {
		return "", fmt.Errorf("未知审查关卡 ID：%s", redactUI(decision.GateID))
	}
	path := nodes.ReviewDecisionPath(workspace, phase, decision.GateID)
	reviewsRoot := filepath.Join(defaultWorkspace(workspace), phase, "artifacts", "reviews")
	if !pathWithinRoot(path, reviewsRoot) {
		return "", fmt.Errorf("审查决定路径超出工作区")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return path, err
	}
	data, err := json.MarshalIndent(decision, "", "  ")
	if err != nil {
		return path, err
	}
	return path, os.WriteFile(path, append(data, '\n'), 0o600)
}

func gateDecisionPhase(gateID string) string {
	phase, ok := reviewGatePhase(gateID)
	if !ok {
		return "phase2"
	}
	return phase
}

func reviewGatePhase(gateID string) (string, bool) {
	switch gateID {
	case nodes.TaskReview, nodes.ContentReview:
		return "phase1", true
	case nodes.ResultReview:
		return "phase3", true
	case nodes.FinalReview:
		return "phase2", true
	}
	return "", false
}

func openEditorCmd(path string) tea.Cmd {
	return func() tea.Msg {
		before := snapshotFile(path)
		cmd := safeEditorCommand(path)
		return tea.ExecProcess(cmd, func(err error) tea.Msg {
			return editorDoneMsg{path: path, before: before, after: snapshotFile(path), err: err}
		})()
	}
}

// safeEditorCommand parses the conventional VISUAL/EDITOR command line and
// invokes the executable directly. The path is always one argv element, so a
// crafted filename cannot be interpreted by a shell.
func safeEditorCommand(path string) *exec.Cmd {
	editor := strings.TrimSpace(os.Getenv("VISUAL"))
	if editor == "" {
		editor = strings.TrimSpace(os.Getenv("EDITOR"))
	}
	if editor == "" {
		return exec.Command("vi", path)
	}
	args, err := splitCommandLine(editor)
	if err != nil || len(args) == 0 {
		return exec.Command("vi", path)
	}
	return exec.Command(args[0], append(args[1:], path)...)
}

func splitCommandLine(value string) ([]string, error) {
	var args []string
	var current strings.Builder
	var quote rune
	escaped := false
	flush := func() {
		if current.Len() > 0 {
			args = append(args, current.String())
			current.Reset()
		}
	}
	for _, r := range value {
		if escaped {
			current.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				current.WriteRune(r)
			}
			continue
		}
		switch r {
		case '\'', '"':
			quote = r
		case ' ', '\t', '\n':
			flush()
		default:
			current.WriteRune(r)
		}
	}
	if escaped {
		current.WriteRune('\\')
	}
	if quote != 0 {
		return nil, fmt.Errorf("unclosed quote in editor command")
	}
	flush()
	return args, nil
}

func editorCommand(path string) *exec.Cmd {
	return safeEditorCommand(path)
}

func (m model) runWorkflow() tea.Cmd {
	return func() tea.Msg {
		summary, err := m.runner.Run(m.ctx)
		return runnerDoneMsg{summary: summary, err: err}
	}
}

func (m model) waitEvent() tea.Cmd {
	return func() tea.Msg {
		event, ok := <-m.runner.Events()
		if !ok {
			return nil
		}
		return runnerEventMsg(event)
	}
}

func (m model) refreshWorkspace() tea.Cmd {
	workspace := defaultWorkspace(m.opts.Workspace)
	return tea.Tick(time.Second, func(time.Time) tea.Msg {
		summary, events := loadWorkspaceState(workspace)
		return workspaceRefreshMsg{summary: summary, events: events}
	})
}

func (m *model) applyRunnerEvent(event domain.RunnerEvent) {
	m.events = append(m.events, event)
	if event.NodeID != "" {
		m.nodes[event.NodeID] = event
		if m.selectedNode == "" {
			m.selectedNode = event.NodeID
		}
	}
	if event.Type == "gate_requested" && event.Gate != nil {
		m.activeGate = event.Gate
		m.gateNotes = ""
		m.gateEditingNote = false
		m.editedFiles = map[string]string{}
		m.selectedArtifact = 0
		m.err = nil
		m.view = viewGate
		return
	}
	if m.activeGate != nil && terminalRunnerEvent(event) && sameGateNode(m.activeGate, event.NodeID) {
		m.activeGate = nil
		m.resetGateLocalState()
		if m.view == viewGate {
			m.view = viewOverview
		}
	}
}

func (m *model) applyWorkspaceSnapshot(summary domain.RunSummary, events []domain.RunnerEvent) {
	previousGate := m.activeGate
	m.summary = summary
	m.events = events
	m.nodes = map[string]domain.RunnerEvent{}
	for _, event := range events {
		if event.NodeID != "" {
			m.nodes[event.NodeID] = event
		}
	}
	nextGate := activeGateFromSnapshot(summary, events, m.nodes)
	if !sameGateIdentity(previousGate, nextGate) {
		m.resetGateLocalState()
	}
	m.activeGate = nextGate
	if _, ok := m.nodes[m.selectedNode]; !ok {
		m.selectedNode = firstNodeID(m.nodes)
	}
	m.done = !summary.FinishedAt.IsZero() || summary.Status == "succeeded" || summary.Status == "failed"
	if m.done && m.view != viewNodeDetail && m.view != viewLogs {
		m.view = viewDone
	} else if m.activeGate != nil && m.view != viewNodeDetail && m.view != viewLogs {
		m.view = viewGate
	} else if m.activeGate == nil && m.view == viewGate {
		m.view = viewOverview
	}
}

func (m *model) resetGateLocalState() {
	m.gateNotes = ""
	m.gateEditingNote = false
	m.notesInput.SetValue("")
	m.notesInput.Blur()
	m.editedFiles = nil
	m.selectedArtifact = 0
}

func activeGateFromSnapshot(summary domain.RunSummary, events []domain.RunnerEvent, nodeEvents map[string]domain.RunnerEvent) *domain.GateRequest {
	if isTerminalRunSummary(summary) {
		return nil
	}
	decidedRequests := map[string]bool{}
	decidedLegacyGates := map[string]bool{}
	for _, decision := range summary.GateDecisions {
		if decision.Action == "revise" {
			continue
		}
		if strings.TrimSpace(decision.RequestID) != "" {
			decidedRequests[decision.RequestID] = true
			continue
		}
		if strings.TrimSpace(decision.GateID) != "" {
			decidedLegacyGates[decision.GateID] = true
		}
	}
	var active *domain.GateRequest
	for _, event := range events {
		if event.Type != "gate_requested" || event.Gate == nil {
			continue
		}
		gate := event.Gate
		if strings.TrimSpace(gate.RequestID) != "" && decidedRequests[gate.RequestID] {
			active = nil
			continue
		}
		if strings.TrimSpace(gate.RequestID) == "" && decidedLegacyGates[gate.GateID] {
			active = nil
			continue
		}
		latest, ok := nodeEvents[event.NodeID]
		if !ok {
			active = gate
			continue
		}
		if latest.Type == "gate_requested" || latest.Status == "waiting" || latest.Status == "running" {
			active = gate
			continue
		}
		active = nil
	}
	return active
}

func terminalRunnerEvent(event domain.RunnerEvent) bool {
	switch event.Type {
	case "node_succeeded", "node_failed", "node_canceled", "run_succeeded", "run_failed":
		return true
	}
	return event.Status == "succeeded" || event.Status == "failed" || event.Status == "canceled"
}

func sameGateNode(gate *domain.GateRequest, nodeID string) bool {
	if gate == nil || strings.TrimSpace(nodeID) == "" {
		return false
	}
	return nodeID == gate.NodeID || nodeID == gate.GateID
}

func sameGateIdentity(a, b *domain.GateRequest) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	aRequest := strings.TrimSpace(a.RequestID)
	bRequest := strings.TrimSpace(b.RequestID)
	if aRequest != "" || bRequest != "" {
		return aRequest != "" && aRequest == bRequest
	}
	return strings.TrimSpace(a.GateID) == strings.TrimSpace(b.GateID)
}

func isTerminalRunSummary(summary domain.RunSummary) bool {
	return !summary.FinishedAt.IsZero() || summary.Status == "succeeded" || summary.Status == "failed"
}

func (m model) header() string {
	title := titleStyle.Render("Harbor 出题工坊")
	context := redactUI(fmt.Sprintf("工作区=%s 任务=%s 仓库=%s", emptyDash(m.opts.Workspace), emptyDash(m.opts.TaskDir), emptyDash(m.opts.RepoURL)))
	if m.width > 0 {
		context = truncateDisplay(context, maxInt(20, m.width))
	}
	contextLine := subtleStyle.Render(context)
	return lipgloss.JoinVertical(lipgloss.Left, title, contextLine)
}

func checkbox(enabled bool) string {
	if enabled {
		return "[x]"
	}
	return "[ ]"
}

func (m model) uncollapsedStartFields() []startField {
	fields := []startField{startFieldMode}
	if m.startMode == startGenerateTask {
		fields = append(fields, startFieldRepoURL, startFieldCommit, startFieldTaskOutput)
	} else {
		fields = append(fields, startFieldTaskDir, startFieldTestsAnalysis)
	}
	fields = append(fields,
		startFieldWorkspace,
		startFieldVerifyDocker,
		startFieldQualityCheck,
		startFieldQualityAgent,
		startFieldSimilarityCheck,
		startFieldSimilarityGitHub,
		startFieldSimilarityThreshold,
		startFieldHistoryDirs,
		startFieldTB3Dirs,
		startFieldRunHarbor,
		startFieldHarborAgent,
		startFieldQwenModel,
		startFieldOpusModel,
		startFieldQwenHarborBaseURL,
		startFieldOpusHarborBaseURL,
		startFieldHarborTimeout,
		startFieldHarborSetupTimeout,
		startFieldHarborPreflight,
		startFieldHarborConcurrency,
		startFieldHarborAttempts,
		startFieldHarborInfraRetries,
		startFieldQwenResult,
		startFieldOpusResult,
		startFieldQwenScreenshot,
		startFieldOpusScreenshot,
		startFieldPackage,
		startFieldOutput,
		startFieldTaskName,
		startFieldCodeLang,
		startFieldTaskType,
		startFieldApplication,
		startFieldAHT,
		startFieldDescription,
		startFieldZeroToOne,
		startFieldCodexModel,
		startFieldCodexReasoning,
		startFieldCodexPath,
		startFieldAgentTimeout,
	)
	return fields
}

func (m model) activeStartFields() []startField {
	if m.startStep == startStepBasic {
		fields := []startField{startFieldMode}
		if m.startMode == startGenerateTask {
			fields = append(fields, startFieldRepoURL, startFieldCommit, startFieldTaskOutput)
		} else {
			fields = append(fields, startFieldTaskDir)
		}
		return append(fields, startFieldWorkspace)
	}
	if m.startCollapsed[m.selectedStartGroup] {
		return nil
	}
	all := m.uncollapsedStartFields()
	out := make([]startField, 0, len(all))
	for _, field := range all {
		if groupForStartField(field) == m.selectedStartGroup {
			out = append(out, field)
		}
	}
	return out
}

func (m *model) selectNextStartField() {
	fields := m.activeStartFields()
	if len(fields) == 0 {
		return
	}
	current := m.startFieldIndex(fields)
	m.startField = fields[(current+1)%len(fields)]
	m.pathSuggestions = nil
}

func (m *model) selectPrevStartField() {
	fields := m.activeStartFields()
	if len(fields) == 0 {
		return
	}
	current := m.startFieldIndex(fields)
	m.startField = fields[(current-1+len(fields))%len(fields)]
	m.pathSuggestions = nil
}

func (m model) startFieldIndex(fields []startField) int {
	for i, field := range fields {
		if field == m.startField {
			return i
		}
	}
	return 0
}

func (m *model) toggleStartMode() {
	if m.startMode == startGenerateTask {
		m.startMode = startExistingTask
		m.opts.Generate = false
		return
	}
	m.startMode = startGenerateTask
	m.opts.Generate = true
}

func (m *model) toggleStartBool() bool {
	switch m.startField {
	case startFieldVerifyDocker:
		m.opts.VerifyDocker = !m.opts.VerifyDocker
	case startFieldQualityCheck:
		m.opts.QualityCheck = !m.opts.QualityCheck
	case startFieldQualityAgent:
		m.opts.QualityAgent = !m.opts.QualityAgent
	case startFieldSimilarityCheck:
		m.opts.SimilarityCheck = !m.opts.SimilarityCheck
	case startFieldSimilarityGitHub:
		m.opts.SimilarityGitHub = !m.opts.SimilarityGitHub
	case startFieldRunHarbor:
		m.opts.RunHarbor = !m.opts.RunHarbor
	case startFieldHarborPreflight:
		m.opts.HarborPreflight = !m.opts.HarborPreflight
	case startFieldPackage:
		m.opts.Package = !m.opts.Package
	case startFieldZeroToOne:
		m.opts.IsZeroToOne = !m.opts.IsZeroToOne
	default:
		return false
	}
	return true
}

func applyStartDefaults(opts app.RunnerOptions) app.RunnerOptions {
	if opts.SimilarityThreshold == 0 {
		opts.SimilarityThreshold = 0.42
	}
	if strings.TrimSpace(opts.HarborAgent) == "" {
		opts.HarborAgent = "claude-code"
	}
	if strings.TrimSpace(opts.QwenModel) == "" {
		opts.QwenModel = "qwen3.7-max"
	}
	if strings.TrimSpace(opts.OpusModel) == "" {
		opts.OpusModel = "claude-opus-4-8"
	}
	if opts.HarborTimeout == 0 {
		opts.HarborTimeout = 7200
	}
	if opts.HarborSetupTimeout == 0 {
		opts.HarborSetupTimeout = 1200
	}
	if opts.HarborConcurrency == 0 {
		opts.HarborConcurrency = 1
	}
	if opts.HarborAttempts == 0 {
		opts.HarborAttempts = 4
	}
	// The TUI starts with the safe cold-cache preflight enabled. A CLI caller can
	// still explicitly disable it with --harbor-preflight=false.
	if !opts.HarborPreflight {
		opts.HarborPreflight = true
	}
	if opts.HarborInfraRetries == 0 {
		opts.HarborInfraRetries = 1
	}
	if opts.AgentTimeout == 0 {
		opts.AgentTimeout = 600
	}
	return opts
}

func (m *model) setStartFloat(field startField, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		if field == startFieldSimilarityThreshold {
			m.opts.SimilarityThreshold = 0
		}
		m.err = nil
		return
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		m.err = fmt.Errorf("%s格式无效", startFieldName(field))
		return
	}
	if parsed < 0 {
		m.err = fmt.Errorf("%s不能为负数", startFieldName(field))
		return
	}
	if field == startFieldSimilarityThreshold {
		m.opts.SimilarityThreshold = parsed
	}
	m.err = nil
}

func (m *model) setStartInt(field startField, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		switch field {
		case startFieldHarborTimeout:
			m.opts.HarborTimeout = 0
		case startFieldHarborSetupTimeout:
			m.opts.HarborSetupTimeout = 0
		case startFieldHarborConcurrency:
			m.opts.HarborConcurrency = 0
		case startFieldHarborAttempts:
			m.opts.HarborAttempts = 0
		case startFieldHarborInfraRetries:
			m.opts.HarborInfraRetries = 0
		case startFieldAgentTimeout:
			m.opts.AgentTimeout = 0
		}
		m.err = nil
		return
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		m.err = fmt.Errorf("%s格式无效", startFieldName(field))
		return
	}
	if parsed < 0 {
		m.err = fmt.Errorf("%s不能为负数", startFieldName(field))
		return
	}
	switch field {
	case startFieldHarborTimeout:
		m.opts.HarborTimeout = parsed
	case startFieldHarborSetupTimeout:
		m.opts.HarborSetupTimeout = parsed
	case startFieldHarborConcurrency:
		m.opts.HarborConcurrency = parsed
	case startFieldHarborAttempts:
		m.opts.HarborAttempts = parsed
	case startFieldHarborInfraRetries:
		m.opts.HarborInfraRetries = parsed
	case startFieldAgentTimeout:
		m.opts.AgentTimeout = parsed
	}
	m.err = nil
}

func startFieldName(field startField) string {
	return localizeField(field)
}

func formatFloatInput(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func formatIntInput(value int) string {
	return strconv.Itoa(value)
}

func joinStartList(values []string) string {
	return strings.Join(compactStartList(values), ",")
}

func splitStartList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return compactStartList(strings.Split(value, ","))
}

func rawStartList(value string) []string {
	if value == "" {
		return nil
	}
	return []string{value}
}

func compactStartList(values []string) []string {
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func (m model) startOptions() (app.RunnerOptions, error) {
	opts := m.opts
	opts.AutoApprove = false
	opts.Workspace = strings.TrimSpace(opts.Workspace)
	if opts.Workspace == "" {
		opts.Workspace = defaultWorkspace("")
	}
	opts.OutputDir = strings.TrimSpace(opts.OutputDir)
	if opts.OutputDir == "" {
		opts.OutputDir = defaultOutputDir("")
	}
	opts.TaskOutputDir = strings.TrimSpace(opts.TaskOutputDir)
	opts.TaskDir = strings.TrimSpace(opts.TaskDir)
	opts.RepoURL = strings.TrimSpace(opts.RepoURL)
	opts.Commit = strings.TrimSpace(opts.Commit)
	opts.TestsAnalysis = strings.TrimSpace(opts.TestsAnalysis)
	opts.QwenResult = strings.TrimSpace(opts.QwenResult)
	opts.OpusResult = strings.TrimSpace(opts.OpusResult)
	opts.QwenScreenshot = strings.TrimSpace(opts.QwenScreenshot)
	opts.OpusScreenshot = strings.TrimSpace(opts.OpusScreenshot)
	opts.HarborAgent = strings.TrimSpace(opts.HarborAgent)
	opts.QwenModel = strings.TrimSpace(opts.QwenModel)
	opts.OpusModel = strings.TrimSpace(opts.OpusModel)
	opts.QwenHarborBaseURL = strings.TrimSpace(opts.QwenHarborBaseURL)
	opts.OpusHarborBaseURL = strings.TrimSpace(opts.OpusHarborBaseURL)
	opts.TaskName = strings.TrimSpace(opts.TaskName)
	opts.CodeLang = strings.TrimSpace(opts.CodeLang)
	opts.TaskType = strings.TrimSpace(opts.TaskType)
	opts.Application = strings.TrimSpace(opts.Application)
	opts.AHT = strings.TrimSpace(opts.AHT)
	opts.Description = strings.TrimSpace(opts.Description)
	opts.Model = strings.TrimSpace(opts.Model)
	opts.Reasoning = strings.TrimSpace(opts.Reasoning)
	opts.CodexPath = strings.TrimSpace(opts.CodexPath)
	opts.SimilarityHistoryDirs = splitStartList(joinStartList(opts.SimilarityHistoryDirs))
	opts.SimilarityTB3Dirs = splitStartList(joinStartList(opts.SimilarityTB3Dirs))
	if err := validateStartEvidence(opts, m.startMode); err != nil {
		return app.RunnerOptions{}, err
	}
	if m.startMode == startGenerateTask {
		opts.Generate = true
		opts.TaskDir = ""
		if opts.RepoURL == "" || opts.Commit == "" {
			return app.RunnerOptions{}, fmt.Errorf("从仓库生成需要填写仓库地址和提交哈希")
		}
		return opts, nil
	}
	opts.Generate = false
	if opts.TaskDir == "" {
		return app.RunnerOptions{}, fmt.Errorf("运行已有任务需要填写任务路径")
	}
	return opts, nil
}

func validateStartEvidence(opts app.RunnerOptions, mode startMode) error {
	if !opts.Package {
		return nil
	}
	if mode == startExistingTask && strings.TrimSpace(opts.TestsAnalysis) == "" {
		return fmt.Errorf("打包需要测试分析文件")
	}
	if mode == startExistingTask {
		if err := requireReadableFile("测试分析文件", opts.TestsAnalysis); err != nil {
			return err
		}
	}
	if !opts.SimilarityGitHub && len(opts.SimilarityHistoryDirs) == 0 && len(opts.SimilarityTB3Dirs) == 0 {
		return fmt.Errorf("打包需要 GitHub、历史目录或 TB3 目录中的至少一种相似度来源")
	}
	for _, dir := range opts.SimilarityHistoryDirs {
		if err := requireReadableDir("历史相似度目录", dir); err != nil {
			return err
		}
	}
	for _, dir := range opts.SimilarityTB3Dirs {
		if err := requireReadableDir("TB3 相似度目录", dir); err != nil {
			return err
		}
	}
	if !opts.RunHarbor && (strings.TrimSpace(opts.QwenResult) == "" || strings.TrimSpace(opts.OpusResult) == "") {
		return fmt.Errorf("打包需要运行 Harbor pass@4，或同时提供 Qwen 与 Opus 结果路径")
	}
	if !opts.RunHarbor {
		if err := requireReadableFile("Qwen Harbor 结果", opts.QwenResult); err != nil {
			return err
		}
		if err := requireReadableFile("Opus Harbor 结果", opts.OpusResult); err != nil {
			return err
		}
		if err := validateExplicitOrResultScreenshot("Qwen", opts.QwenScreenshot, opts.QwenResult); err != nil {
			return err
		}
		if err := validateExplicitOrResultScreenshot("Opus", opts.OpusScreenshot, opts.OpusResult); err != nil {
			return err
		}
	}
	return nil
}

func validateExplicitOrResultScreenshot(label, explicit, resultPath string) error {
	if strings.TrimSpace(explicit) != "" {
		return requireReadableFile(label+" pass@4 screenshot", explicit)
	}
	screenshot, ok := resultScreenshotPath(resultPath)
	if !ok {
		switch label {
		case "Qwen":
			return fmt.Errorf("打包需要 Qwen pass@4 截图，或 Qwen 结果中的截图字段")
		case "Opus":
			return fmt.Errorf("打包需要 Opus pass@4 截图，或 Opus 结果中的截图字段")
		default:
			return fmt.Errorf("打包需要 %s pass@4 截图，或结果中的截图字段", label)
		}
	}
	return requireReadableFile(label+" result screenshot", screenshot)
}

func resultScreenshotPath(resultPath string) (string, bool) {
	resultPath = strings.TrimSpace(resultPath)
	if resultPath == "" {
		return "", false
	}
	result, err := harborrun.ParseFile(resultPath)
	if err != nil {
		return "", false
	}
	screenshot := strings.TrimSpace(result.Screenshot)
	if screenshot == "" {
		return "", false
	}
	if !filepath.IsAbs(screenshot) {
		screenshot = filepath.Join(filepath.Dir(resultPath), screenshot)
	}
	return screenshot, true
}

func requireReadableFile(label, path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("%s is required", label)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s不可读取：%w", label, err)
	}
	if info.IsDir() || !info.Mode().IsRegular() {
		return fmt.Errorf("%s必须是普通文件：%s", label, path)
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%s不可读取：%w", label, err)
	}
	return file.Close()
}

func requireReadableDir(label, path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("%s directory is required", label)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s不可读取：%w", label, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s必须是目录：%s", label, path)
	}
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%s不可读取：%w", label, err)
	}
	defer dir.Close()
	if _, err := dir.Readdirnames(1); err != nil && err != io.EOF {
		return fmt.Errorf("%s不可读取：%w", label, err)
	}
	return nil
}

func (m model) startRunner(opts app.RunnerOptions) model {
	opts.AutoApprove = false
	m.opts = opts
	m.runner = app.NewRunner(opts)
	m.view = viewOverview
	m.events = nil
	m.nodes = map[string]domain.RunnerEvent{}
	m.summary = domain.RunSummary{}
	m.err = nil
	m.done = false
	m.readOnly = false
	m.activeGate = nil
	m.gateNotes = ""
	m.gateEditingNote = false
	m.editedFiles = nil
	m.selectedNode = ""
	m.selectedArtifact = 0
	m.selectedLogFile = 0
	m.logFileScroll = 0
	m.logTail = false
	return m
}

func (m model) lastFailureEvent() (domain.RunnerEvent, bool) {
	for _, events := range [][]domain.RunnerEvent{m.summary.Events, m.events} {
		if event, ok := lastFailureInEvents(events); ok {
			return event, true
		}
	}
	for _, event := range m.nodes {
		if failedRunnerEvent(event) {
			return event, true
		}
	}
	return domain.RunnerEvent{}, false
}

func lastFailureInEvents(events []domain.RunnerEvent) (domain.RunnerEvent, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		if failedNodeEvent(events[i]) {
			return events[i], true
		}
	}
	for i := len(events) - 1; i >= 0; i-- {
		if failedRunnerEvent(events[i]) {
			return events[i], true
		}
	}
	return domain.RunnerEvent{}, false
}

func failedRunnerEvent(event domain.RunnerEvent) bool {
	return event.Type == "node_failed" || event.Type == "run_failed" || event.Status == "failed"
}

func failedNodeEvent(event domain.RunnerEvent) bool {
	return strings.TrimSpace(event.NodeID) != "" && (event.Type == "node_failed" || event.Status == "failed")
}

func statusIcon(status string) string {
	switch status {
	case "succeeded", string(domain.CheckPass):
		return passStyle.Render("✓")
	case "failed", string(domain.CheckFail):
		return failStyle.Render("✗")
	case "canceled":
		return failStyle.Render("⊗")
	case string(domain.CheckWarn):
		return warnStyle.Render("⚠")
	case "running":
		return defaultTheme.Focused.Render("◌")
	case "waiting", "gate_requested":
		return warnStyle.Render("⚷")
	case "blocked":
		return warnStyle.Render("⊘")
	case "skipped":
		return subtleStyle.Render("–")
	default:
		return subtleStyle.Render("○")
	}
}

func emptyDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func contentWidth(width int) int {
	if width < 20 {
		return 20
	}
	return width - 4
}

func (m *model) selectNextNode() {
	order := nodeOrder()
	current := 0
	for i, id := range order {
		if id == m.selectedNode {
			current = i
			break
		}
	}
	for i := 1; i <= len(order); i++ {
		next := order[(current+i)%len(order)]
		if event, ok := m.nodes[next]; ok && matchesFilter(m.filter, next, localizeNode(next), event.Message, event.Status) {
			m.selectedNode = next
			m.selectedArtifact = 0
			return
		}
	}
}

func (m *model) selectPrevNode() {
	order := nodeOrder()
	current := 0
	for i, id := range order {
		if id == m.selectedNode {
			current = i
			break
		}
	}
	for i := 1; i <= len(order); i++ {
		next := order[(current-i+len(order))%len(order)]
		if event, ok := m.nodes[next]; ok && matchesFilter(m.filter, next, localizeNode(next), event.Message, event.Status) {
			m.selectedNode = next
			m.selectedArtifact = 0
			return
		}
	}
}

func (m *model) selectNextArtifact() {
	switch m.view {
	case viewLogs:
		files := m.visibleLogArtifacts()
		if len(files) > 0 {
			m.selectedLogFile = (m.selectedLogFile + 1) % len(files)
			m.logFileScroll = 0
			m.logTail = false
		}
	case viewNodeDetail:
		artifacts := m.nodeArtifacts(m.selectedNode)
		if len(artifacts) > 0 {
			m.selectedArtifact = (m.selectedArtifact + 1) % len(artifacts)
			m.detailScroll = 0
		}
	}
}

func (m *model) selectPrevArtifact() {
	switch m.view {
	case viewLogs:
		files := m.visibleLogArtifacts()
		if len(files) > 0 {
			m.selectedLogFile = (m.selectedLogFile - 1 + len(files)) % len(files)
			m.logFileScroll = 0
			m.logTail = false
		}
	case viewNodeDetail:
		artifacts := m.nodeArtifacts(m.selectedNode)
		if len(artifacts) > 0 {
			m.selectedArtifact = (m.selectedArtifact - 1 + len(artifacts)) % len(artifacts)
			m.detailScroll = 0
		}
	case viewGate:
		artifacts := m.visibleGateArtifacts(m.activeGate)
		if len(artifacts) > 0 {
			m.selectedArtifact = (m.selectedArtifact - 1 + len(artifacts)) % len(artifacts)
		}
	}
}

func (m *model) scrollLogFile(delta int) {
	artifact, ok := m.selectedLogArtifact()
	if !ok {
		m.logFileScroll = 0
		m.logTail = false
		return
	}
	content := m.logArtifactContent(artifact)
	maxOffset := maxLineOffset(content, logPreviewLines(m.height))
	if m.logTail {
		m.logFileScroll = maxOffset
		m.logTail = false
	}
	m.logFileScroll += delta
	if m.logFileScroll < 0 {
		m.logFileScroll = 0
	}
	if m.logFileScroll > maxOffset {
		m.logFileScroll = maxOffset
	}
}

func (m model) selectedNodeArtifact() (domain.ArtifactPreview, bool) {
	artifacts := m.nodeArtifacts(m.selectedNode)
	if len(artifacts) == 0 {
		return domain.ArtifactPreview{}, false
	}
	idx := m.selectedArtifact
	if idx < 0 || idx >= len(artifacts) {
		idx = 0
	}
	return artifacts[idx], true
}

func (m model) selectedGateArtifact() (int, domain.ArtifactPreview, bool) {
	if m.activeGate == nil {
		return 0, domain.ArtifactPreview{}, false
	}
	artifacts := m.visibleGateArtifacts(m.activeGate)
	if len(artifacts) == 0 {
		return 0, domain.ArtifactPreview{}, false
	}
	idx := m.selectedArtifact
	if idx < 0 || idx >= len(artifacts) {
		idx = 0
	}
	return idx, artifacts[idx], true
}

func (m model) visibleGateArtifacts(gate *domain.GateRequest) []domain.ArtifactPreview {
	if gate == nil {
		return nil
	}
	if strings.TrimSpace(m.filter) == "" {
		return gate.Artifacts
	}
	artifacts := make([]domain.ArtifactPreview, 0, len(gate.Artifacts))
	for _, artifact := range gate.Artifacts {
		if matchesFilter(m.filter, artifact.Name, artifact.Path) {
			artifacts = append(artifacts, artifact)
		}
	}
	return artifacts
}

func (m model) selectedLogArtifact() (domain.ArtifactPreview, bool) {
	files := m.visibleLogArtifacts()
	if len(files) == 0 {
		return domain.ArtifactPreview{}, false
	}
	idx := m.selectedLogFile
	if idx < 0 || idx >= len(files) {
		idx = 0
	}
	return files[idx], true
}

func (m model) visibleLogArtifacts() []domain.ArtifactPreview {
	files := m.logArtifacts()
	if strings.TrimSpace(m.filter) == "" {
		return files
	}
	visible := make([]domain.ArtifactPreview, 0, len(files))
	for _, artifact := range files {
		if matchesFilter(m.filter, artifact.Name, artifact.Path) {
			visible = append(visible, artifact)
		}
	}
	return visible
}

func (m model) nodeArtifacts(nodeID string) []domain.ArtifactPreview {
	seen := map[string]bool{}
	var paths []string
	add := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" || seen[path] {
			return
		}
		safePath, ok := m.safeArtifactPath(path)
		if !ok {
			return
		}
		if seen[safePath] {
			return
		}
		seen[safePath] = true
		paths = append(paths, safePath)
	}
	if event, ok := m.nodes[nodeID]; ok {
		for _, artifact := range event.Artifacts {
			add(artifact.Path)
		}
		for _, artifact := range event.Logs {
			add(artifact.Path)
		}
		add(event.Path)
		if info, err := os.Stat(event.Path); err == nil && info.IsDir() {
			for _, name := range []string{"trial_result.json", "command_run.json", "stdout.txt", "stderr.txt", "lint_report.json", "verify_report.json", "quality_report.json", "similarity_report.json"} {
				add(filepath.Join(event.Path, name))
			}
		}
	}
	workspace := defaultWorkspace(m.opts.Workspace)
	for _, path := range knownNodeArtifactPaths(workspace, nodeID) {
		add(path)
	}
	artifacts := make([]domain.ArtifactPreview, 0, len(paths))
	for _, path := range paths {
		artifacts = append(artifacts, fileArtifact(path))
	}
	return artifacts
}

func (m model) logArtifacts() []domain.ArtifactPreview {
	workspace := defaultWorkspace(m.opts.Workspace)
	seen := map[string]bool{}
	var paths []string
	add := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" {
			return
		}
		safePath, ok := m.safeArtifactPath(path)
		if !ok || seen[safePath] {
			return
		}
		seen[safePath] = true
		paths = append(paths, safePath)
	}

	eventLogPath := filepath.Join(workspace, "event_log.jsonl")
	add(eventLogPath)
	add(filepath.Join(workspace, "state.json"))
	for _, event := range m.events {
		for _, artifact := range event.Logs {
			add(artifact.Path)
		}
	}
	for _, root := range logSearchRoots(workspace) {
		walkLogArtifacts(root, add)
	}

	sort.Strings(paths)
	var artifacts []domain.ArtifactPreview
	for _, path := range paths {
		name := relativeArtifactName(workspace, path)
		artifact := fileArtifactWithName(path, name)
		if sameFilePath(path, eventLogPath) && m.summary.RunID != "" && len(m.events) > 0 {
			artifact.Content = marshalEventPreview(m.events)
		}
		artifacts = append(artifacts, artifact)
	}
	return artifacts
}

func knownNodeArtifactPaths(workspace, nodeID string) []string {
	return nodes.ArtifactPaths(workspace, nodeID)
}

func logSearchRoots(workspace string) []string {
	return []string{
		filepath.Join(workspace, "phase0", "command_logs"),
		filepath.Join(workspace, "phase1", "artifacts"),
		filepath.Join(workspace, "phase2", "artifacts"),
		filepath.Join(workspace, "phase3", "artifacts", nodes.HarborRunQwen),
		filepath.Join(workspace, "phase3", "artifacts", nodes.HarborRunOpus),
	}
}

func walkLogArtifacts(root string, add func(string)) {
	if strings.TrimSpace(root) == "" {
		return
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return
	}
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if isLogArtifactPath(path) {
			add(path)
		}
		return nil
	})
}

func isLogArtifactPath(path string) bool {
	base := filepath.Base(path)
	switch base {
	case "agent.log", "command_run.json", "stdout.txt", "stderr.txt":
		return true
	}
	if strings.HasSuffix(base, "_command_run.json") {
		return true
	}
	return strings.HasSuffix(base, ".json") && hasPathSegment(path, "command_logs")
}

func hasPathSegment(path, segment string) bool {
	for _, part := range strings.Split(filepath.Clean(path), string(filepath.Separator)) {
		if part == segment {
			return true
		}
	}
	return false
}

func fileArtifact(path string) domain.ArtifactPreview {
	return fileArtifactWithName(path, filepath.Base(path))
}

func fileArtifactWithName(path, name string) domain.ArtifactPreview {
	if strings.TrimSpace(name) == "" {
		name = filepath.Base(path)
	}
	return domain.ArtifactPreview{
		Name:    name,
		Path:    path,
		Content: readPreview(path, 20000),
	}
}

func relativeArtifactName(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return filepath.Base(path)
	}
	return rel
}

func sameFilePath(a, b string) bool {
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return a == b
	}
	return absA == absB
}

func (m model) artifactContent(artifact domain.ArtifactPreview) string {
	if path, ok := m.safeArtifactPath(artifact.Path); ok {
		return readPreview(path, 20000)
	}
	return redactUI(artifact.Content)
}

func (m model) logArtifactContent(artifact domain.ArtifactPreview) string {
	if path, ok := m.safeArtifactPath(artifact.Path); ok {
		eventLogPath := filepath.Join(defaultWorkspace(m.opts.Workspace), "event_log.jsonl")
		if sameFilePath(path, eventLogPath) && strings.TrimSpace(artifact.Content) != "" {
			return artifact.Content
		}
		if m.logTail {
			return readPreviewTail(path, 64000)
		}
		return readPreview(path, 64000)
	}
	return redactUI(artifact.Content)
}

func (m model) logContentOffset(content string, maxLines int) int {
	maxOffset := maxLineOffset(content, maxLines)
	if m.logTail {
		return maxOffset
	}
	if m.logFileScroll < 0 {
		return 0
	}
	if m.logFileScroll > maxOffset {
		return maxOffset
	}
	return m.logFileScroll
}

func readPreview(path string, maxBytes int64) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.IsDir() {
		return ""
	}
	if maxBytes <= 0 {
		maxBytes = 20000
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return ""
	}
	truncated := int64(len(data)) > maxBytes
	if truncated {
		data = data[:maxBytes]
	}
	for len(data) > 0 && !utf8.Valid(data) {
		data = data[:len(data)-1]
	}
	content := string(data)
	if truncated {
		content += "\n... 内容已截断 ..."
	}
	return redactUI(content)
}

func readPreviewTail(path string, maxBytes int64) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.IsDir() {
		return ""
	}
	if maxBytes <= 0 {
		maxBytes = 20000
	}
	size := info.Size()
	start := int64(0)
	truncated := false
	if size > maxBytes {
		start = size - maxBytes
		truncated = true
	}
	readLen := size - start
	if readLen <= 0 {
		return ""
	}
	data := make([]byte, int(readLen))
	n, _ := file.ReadAt(data, start)
	data = data[:n]
	if truncated {
		for len(data) > 0 && !utf8.RuneStart(data[0]) {
			data = data[1:]
		}
	}
	content := string(data)
	content = strings.ToValidUTF8(content, "�")
	if truncated {
		content = "... 已从开头截断 ...\n" + content
	}
	return redactUI(content)
}

func isReadableFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func (m model) safeArtifactPath(path string) (string, bool) {
	path = strings.TrimSpace(path)
	if path == "" || !isReadableFile(path) {
		return "", false
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	evaluatedPath, err := filepath.EvalSymlinks(absPath)
	if err != nil || !isReadableFile(evaluatedPath) {
		return "", false
	}
	for _, root := range m.artifactRoots() {
		if pathWithinRoot(evaluatedPath, root) {
			return evaluatedPath, true
		}
	}
	return "", false
}

func (m model) safeEditableArtifactPath(path string) (string, error) {
	safePath, ok := m.safeArtifactPath(path)
	if !ok {
		return "", unsafeArtifactPathError(path)
	}
	if m.editableWorkspaceArtifact(safePath) || m.editableTaskFile(safePath) {
		return safePath, nil
	}
	return "", nonEditableArtifactPathError(path)
}

func (m model) editableWorkspaceArtifact(path string) bool {
	workspace := defaultWorkspace(m.opts.Workspace)
	for _, candidate := range editableWorkspaceArtifactPaths(workspace) {
		if sameFilePath(path, candidate) {
			return true
		}
	}
	return false
}

func editableWorkspaceArtifactPaths(workspace string) []string {
	var paths []string
	for _, gateID := range []string{nodes.TaskReview, nodes.ContentReview, nodes.FinalReview, nodes.ResultReview} {
		paths = append(paths, nodes.ReviewDecisionPath(workspace, gateDecisionPhase(gateID), gateID))
	}
	return paths
}

func (m model) editableTaskFile(path string) bool {
	for _, root := range []string{m.opts.TaskDir, m.opts.TaskOutputDir} {
		if editableTaskFileUnderRoot(root, path) {
			return true
		}
	}
	return editableGeneratedTaskFile(defaultWorkspace(m.opts.Workspace), path)
}

func editableTaskFileUnderRoot(root, path string) bool {
	rel, ok := relativePathWithinRoot(path, root)
	if !ok {
		return false
	}
	return editableTaskRelPath(rel)
}

func editableGeneratedTaskFile(workspace, path string) bool {
	rel, ok := relativePathWithinRoot(path, filepath.Join(workspace, "phase2", "task"))
	if !ok {
		return false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) < 2 || strings.TrimSpace(parts[0]) == "" {
		return false
	}
	return editableTaskRelPath(strings.Join(parts[1:], "/"))
}

func editableTaskRelPath(rel string) bool {
	switch filepath.ToSlash(rel) {
	case "instruction.md",
		"task.toml",
		"tests_analysis.md",
		"environment/Dockerfile",
		"environment/docker-compose.yaml",
		"solution/solve.sh",
		"tests/test.sh":
		return true
	}
	return false
}

func (m model) artifactRoots() []string {
	candidates := []string{
		defaultWorkspace(m.opts.Workspace),
		m.opts.TaskDir,
		m.opts.TaskOutputDir,
		defaultOutputDir(m.opts.OutputDir),
	}
	roots := make([]string, 0, len(candidates))
	seen := map[string]bool{}
	for _, root := range candidates {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		absRoot, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		if evaluatedRoot, err := filepath.EvalSymlinks(absRoot); err == nil {
			absRoot = evaluatedRoot
		}
		if seen[absRoot] {
			continue
		}
		seen[absRoot] = true
		roots = append(roots, absRoot)
	}
	return roots
}

func pathWithinRoot(path, root string) bool {
	_, ok := relativePathWithinRoot(path, root)
	return ok
}

func relativePathWithinRoot(path, root string) (string, bool) {
	path = strings.TrimSpace(path)
	root = strings.TrimSpace(root)
	if path == "" || root == "" {
		return "", false
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", false
	}
	if evaluatedPath, err := filepath.EvalSymlinks(absPath); err == nil {
		absPath = evaluatedPath
	}
	if evaluatedRoot, err := filepath.EvalSymlinks(absRoot); err == nil {
		absRoot = evaluatedRoot
	}
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil {
		return "", false
	}
	if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return rel, true
	}
	return "", false
}

func unsafeArtifactPathError(path string) error {
	return fmt.Errorf("工件路径超出允许的 TUI 根目录：%s", redactUI(path))
}

func nonEditableArtifactPathError(path string) error {
	return fmt.Errorf("工件路径不是可编辑的 Harbor 工件：%s", redactUI(path))
}

func defaultWorkspace(workspace string) string {
	if strings.TrimSpace(workspace) == "" {
		return filepath.Join(".harbor-factory", "workspace")
	}
	return workspace
}

func defaultOutputDir(outputDir string) string {
	if strings.TrimSpace(outputDir) == "" {
		return filepath.Join(".harbor-factory", "output")
	}
	return outputDir
}

func loadWorkspaceState(workspace string) (domain.RunSummary, []domain.RunnerEvent) {
	var summary domain.RunSummary
	statePath := filepath.Join(workspace, "state.json")
	if raw, err := os.ReadFile(statePath); err == nil {
		_ = json.Unmarshal(raw, &summary)
	}
	events := readEventLog(filepath.Join(workspace, "event_log.jsonl"), summary.RunID)
	if len(events) == 0 && len(summary.Events) > 0 {
		events = summary.Events
	}
	if summary.Workspace == "" {
		summary.Workspace = workspace
	}
	summary.GateDecisions = mergeGateDecisions(summary.GateDecisions, loadWorkspaceGateDecisions(workspace))
	return summary, events
}

func loadWorkspaceGateDecisions(workspace string) []domain.GateDecision {
	var decisions []domain.GateDecision
	for _, gateID := range []string{nodes.TaskReview, nodes.ContentReview, nodes.FinalReview, nodes.ResultReview} {
		path := nodes.ReviewDecisionPath(workspace, gateDecisionPhase(gateID), gateID)
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var decision domain.GateDecision
		if err := json.Unmarshal(raw, &decision); err != nil {
			continue
		}
		decision.GateID = gateID
		decisions = append(decisions, sanitize.GateDecision(decision))
	}
	return decisions
}

func mergeGateDecisions(existing, loaded []domain.GateDecision) []domain.GateDecision {
	if len(loaded) == 0 {
		return existing
	}
	out := append([]domain.GateDecision(nil), existing...)
	seen := map[string]bool{}
	for _, decision := range out {
		if key := gateDecisionKey(decision); key != "" {
			seen[key] = true
		}
	}
	for _, decision := range loaded {
		key := gateDecisionKey(decision)
		if key != "" && seen[key] {
			continue
		}
		out = append(out, decision)
		if key != "" {
			seen[key] = true
		}
	}
	return out
}

func gateDecisionKey(decision domain.GateDecision) string {
	if strings.TrimSpace(decision.RequestID) != "" {
		return "request:" + strings.TrimSpace(decision.RequestID)
	}
	if strings.TrimSpace(decision.GateID) != "" {
		return "gate:" + strings.TrimSpace(decision.GateID)
	}
	return ""
}

func readEventLog(path, runID string) []domain.RunnerEvent {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var events []domain.RunnerEvent
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var event domain.RunnerEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		if runID != "" && event.RunID != "" && event.RunID != runID {
			continue
		}
		events = append(events, event)
	}
	return events
}

func marshalEventPreview(events []domain.RunnerEvent) string {
	var lines []string
	for _, event := range events {
		data, err := json.Marshal(event)
		if err != nil {
			continue
		}
		lines = append(lines, redactUI(string(data)))
	}
	return strings.Join(lines, "\n")
}

func redactUI(text string) string {
	return commandlog.RedactText(text)
}

func detailPreviewLines(height int) int {
	if height <= 0 {
		return 18
	}
	n := height - 18
	if n < 8 {
		return 8
	}
	if n > 40 {
		return 40
	}
	return n
}

func logPreviewLines(height int) int {
	if height <= 0 {
		return 10
	}
	n := height - 24
	if n < 6 {
		return 6
	}
	if n > 30 {
		return 30
	}
	return n
}

func logPageStep(height int) int {
	step := logPreviewLines(height) - 1
	if step < 1 {
		return 1
	}
	return step
}

func trimLines(content string, maxLines int) string {
	return trimLinesFrom(content, maxLines, 0)
}

func trimLinesFrom(content string, maxLines, offset int) string {
	if strings.TrimSpace(content) == "" {
		return subtleStyle.Render("（内容为空或不可用）")
	}
	if maxLines <= 0 {
		maxLines = 1
	}
	lines := strings.Split(content, "\n")
	maxOffset := maxLineOffset(content, maxLines)
	if offset < 0 {
		offset = 0
	}
	if offset > maxOffset {
		offset = maxOffset
	}
	end := offset + maxLines
	if end > len(lines) {
		end = len(lines)
	}
	out := make([]string, 0, maxLines+2)
	if offset > 0 {
		out = append(out, subtleStyle.Render(fmt.Sprintf("... 上方还有 %d 行 ...", offset)))
	}
	out = append(out, lines[offset:end]...)
	if end < len(lines) {
		out = append(out, subtleStyle.Render(fmt.Sprintf("... 下方还有 %d 行 ...", len(lines)-end)))
	}
	return strings.Join(out, "\n")
}

func maxLineOffset(content string, maxLines int) int {
	if strings.TrimSpace(content) == "" {
		return 0
	}
	if maxLines <= 0 {
		maxLines = 1
	}
	lines := strings.Split(content, "\n")
	if len(lines) <= maxLines {
		return 0
	}
	return len(lines) - maxLines
}

func snapshotFile(path string) fileSnapshot {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fileSnapshot{}
	}
	sum := sha256.Sum256(raw)
	return fileSnapshot{exists: true, size: int64(len(raw)), hash: fmt.Sprintf("%x", sum[:8])}
}

func (s fileSnapshot) changed(other fileSnapshot) bool {
	return s.exists != other.exists || s.size != other.size || s.hash != other.hash
}

func editSummary(before, after fileSnapshot) string {
	return fmt.Sprintf("size %d->%d sha256 %s->%s", before.size, after.size, emptyHash(before.hash), emptyHash(after.hash))
}

func emptyHash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func redactStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[redactUI(key)] = redactUI(value)
	}
	return out
}

func gateBlockingChecklist(checklist []domain.ChecklistItem) []string {
	var blockers []string
	for _, item := range checklist {
		if item.Critical && !item.Passed {
			label := strings.TrimSpace(item.Label)
			if label == "" {
				label = strings.TrimSpace(item.ID)
			}
			if label == "" {
				label = "检查项"
			}
			blockers = append(blockers, redactUI(label))
		}
	}
	return blockers
}

func firstNodeID(nodes map[string]domain.RunnerEvent) string {
	for _, id := range nodeOrder() {
		if _, ok := nodes[id]; ok {
			return id
		}
	}
	return ""
}

func nodeOrder() []string {
	return nodes.Order()
}
