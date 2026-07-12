package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/nodes"
)

func runeKey(value string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(value)} }

func TestLocalizeCoversWorkflowVocabulary(t *testing.T) {
	for _, id := range nodes.Order() {
		if got := localizeNode(id); got == id || strings.TrimSpace(got) == "" {
			t.Errorf("node %q not localized: %q", id, got)
		}
	}
	for _, status := range []string{"succeeded", "failed", "canceled", "running", "pending", "waiting", "blocked", "skipped", string(domain.CheckWarn)} {
		if got := localizeStatus(status); got == status || got == "" {
			t.Errorf("status %q not localized: %q", status, got)
		}
	}
	if got := localizeStatus(string(domain.CheckPass)); got != "通过" {
		t.Fatalf("check pass localization=%q", got)
	}
	for _, eventType := range []string{"run_started", "node_started", "node_succeeded", "node_failed", "gate_requested", "run_succeeded", "run_failed", "run_recovered"} {
		if got := localizeEventType(eventType); got == eventType || got == "" {
			t.Errorf("event type %q not localized: %q", eventType, got)
		}
	}
	for field := startFieldMode; field <= startFieldAgentTimeout; field++ {
		if got := localizeField(field); got == "字段" || got == "" {
			t.Errorf("field %v not localized", field)
		}
	}
	if got := padRightDisplay("中文", 8); runewidth.StringWidth(got) != 8 {
		t.Fatalf("CJK padding width=%d: %q", runewidth.StringWidth(got), got)
	}
}

func TestGateApprovalRequiresConfirmation(t *testing.T) {
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{Workspace: t.TempDir(), TaskDir: t.TempDir()})
	m.view = viewGate
	m.activeGate = &domain.GateRequest{RequestID: "r1", GateID: nodes.FinalReview, Checklist: []domain.ChecklistItem{{ID: "ok", Passed: true, Critical: true}}}
	opened, cmd := m.Update(runeKey("a"))
	pending := opened.(model)
	if cmd == nil || pending.confirm == nil || pending.confirm.Action != confirmApprove {
		t.Fatalf("approval did not open confirmation: %+v", pending.confirm)
	}
	if msg := cmd(); func() bool { _, ok := msg.(confirmOpenedMsg); return ok }() == false {
		t.Fatalf("approval command performed work before confirmation: %T", msg)
	}
	confirmed, submit := pending.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if submit == nil || confirmed.(model).confirm != nil {
		t.Fatal("Enter did not confirm approval")
	}
	written, ok := submit().(gateDecisionWrittenMsg)
	if !ok || !written.decision.Approved || written.decision.Action != "approve" {
		t.Fatalf("unexpected confirmed decision: %#v", written)
	}
}

func TestCancelConfirmationRestoresGate(t *testing.T) {
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{})
	m.view = viewGate
	m.activeGate = &domain.GateRequest{RequestID: "r1", GateID: nodes.FinalReview}
	opened, _ := m.Update(runeKey("r"))
	pending := opened.(model)
	canceled, _ := pending.Update(tea.KeyMsg{Type: tea.KeyEsc})
	got := canceled.(model)
	if got.confirm != nil || got.activeGate == nil || got.activeGate.RequestID != "r1" {
		t.Fatalf("cancel did not restore gate: %+v", got)
	}
}

func TestStartTextInputSupportsCursorAndChinese(t *testing.T) {
	m := initialStartModel(context.Background(), func() {}, app.RunnerOptions{Workspace: t.TempDir()})
	m.startField = startFieldTaskDir
	m.focusStartInput(m.startField)
	updated, _ := m.Update(runeKey("世界"))
	m = updated.(model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m = updated.(model)
	updated, _ = m.Update(runeKey("你"))
	m = updated.(model)
	if m.opts.TaskDir != "世你界" {
		t.Fatalf("cursor insertion failed: %q", m.opts.TaskDir)
	}
}

func TestStartTextFieldsCanTypeQWithoutQuitting(t *testing.T) {
	m := initialStartModel(context.Background(), func() {}, app.RunnerOptions{Workspace: t.TempDir(), TaskDir: t.TempDir()})
	m.startStep = startStepAdvanced
	m.startField = startFieldQwenModel
	m.focusStartInput(m.startField)
	updated, cmd := m.Update(runeKey("q"))
	got := updated.(model)
	if got.opts.QwenModel != "qwen3.7-maxq" || got.view != viewStart {
		t.Fatalf("q was not handled as text input: model=%q cmd=%v", got.opts.QwenModel, cmd)
	}
}

func TestStartWizardSeparatesBasicAndAdvancedSteps(t *testing.T) {
	m := initialStartModel(context.Background(), func() {}, app.RunnerOptions{Workspace: t.TempDir(), TaskDir: t.TempDir()})
	m.width, m.height = 120, 32
	basic := m.View()
	if !strings.Contains(basic, "● 1 基本配置") || !strings.Contains(basic, "Enter 下一步") {
		t.Fatalf("basic step missing wizard navigation: %s", basic)
	}
	for _, hidden := range []string{"Harbor 超时", "质量检查", "打包输出目录", "Codex 模型"} {
		if strings.Contains(basic, hidden) {
			t.Fatalf("basic step leaked advanced field %q: %s", hidden, basic)
		}
	}
	advancedModel, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	advanced := advancedModel.(model)
	if cmd != nil || advanced.startStep != startStepAdvanced || advanced.runner != nil {
		t.Fatalf("Enter should advance without starting: step=%v runner=%v cmd=%v", advanced.startStep, advanced.runner, cmd)
	}
	harbor := advanced.View()
	if !strings.Contains(harbor, "● 2 高级选项") || !strings.Contains(harbor, "Harbor 超时") || strings.Contains(harbor, "相似度阈值") {
		t.Fatalf("Harbor accordion group rendered incorrectly: %s", harbor)
	}
	qualityModel, _ := advanced.Update(tea.KeyMsg{Type: tea.KeyF2})
	quality := qualityModel.(model)
	qualityView := quality.View()
	if quality.selectedStartGroup != startGroupQuality || !strings.Contains(qualityView, "相似度阈值") || strings.Contains(qualityView, "Harbor 超时") {
		t.Fatalf("F2 did not switch accordion group: %s", qualityView)
	}
	startedModel, startCmd := quality.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if startCmd == nil || startedModel.(model).runner == nil || startedModel.(model).view != viewOverview {
		t.Fatal("advanced step Enter did not start workflow")
	}
}

func TestRouterDispatchesToActivePage(t *testing.T) {
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{})
	m.width, m.height = 100, 30
	m.nodes[nodes.CodeEdgeLint] = domain.RunnerEvent{NodeID: nodes.CodeEdgeLint, Status: "succeeded"}
	m.selectedNode = nodes.CodeEdgeLint
	m.bindPages()
	handled, cmd := m.router.Dispatch(tea.KeyMsg{Type: tea.KeyDown})
	if !handled || cmd != nil {
		t.Fatalf("overview page did not handle table navigation: handled=%v cmd=%v", handled, cmd)
	}
	if m.selectedNode == nodes.CodeEdgeLint || m.overviewTable.Cursor() == 0 {
		t.Fatalf("table component state did not advance: node=%q cursor=%d", m.selectedNode, m.overviewTable.Cursor())
	}
	m.setView(viewLogs)
	m.bindPages()
	if m.router.Active() != viewLogs {
		t.Fatalf("router did not track active page: %v", m.router.Active())
	}
}

func TestRouterOverlayInterceptsPageKeys(t *testing.T) {
	router := newPageRouter(viewOverview)
	dialog := newConfirmDialog(confirmQuit, "确认", "是否继续？")
	router.PushOverlay(dialog)
	handled, cmd := router.Dispatch(tea.KeyMsg{Type: tea.KeyRight})
	if !handled || cmd != nil || dialog.FocusedYes {
		t.Fatalf("overlay did not intercept and update selection: handled=%v cmd=%v yes=%v", handled, cmd, dialog.FocusedYes)
	}
	if popped := router.PopOverlay(); popped != dialog || router.TopOverlay() != nil {
		t.Fatal("overlay stack did not pop top dialog")
	}
}

func TestModelUpdatesDoNotMutatePreviousValue(t *testing.T) {
	original := initialModel(context.Background(), func() {}, app.RunnerOptions{})
	updated, _ := original.Update(runnerEventMsg(domain.RunnerEvent{NodeID: nodes.CodeEdgeLint, Type: "node_succeeded", Status: "succeeded"}))
	if _, exists := original.nodes[nodes.CodeEdgeLint]; exists {
		t.Fatal("value-model update mutated the previous node map")
	}
	if _, exists := updated.(model).nodes[nodes.CodeEdgeLint]; !exists {
		t.Fatal("updated model lost runner event")
	}
}

func TestFocusManagerRestoresPreviousArea(t *testing.T) {
	focus := newFocusManager(focusOverviewTable)
	focus.Push(focusSearch)
	focus.Push(focusOverlay)
	if focus.Current() != focusOverlay {
		t.Fatalf("focus stack did not push overlay: %v", focus.Current())
	}
	if got := focus.Pop(); got != focusSearch {
		t.Fatalf("focus stack did not restore search: %v", got)
	}
	if got := focus.Pop(); got != focusOverviewTable {
		t.Fatalf("focus stack did not restore overview: %v", got)
	}
}

func TestDetailViewportComponentScrolls(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "phase2", "artifacts", "codeedge_lint", "lint_report.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var content strings.Builder
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&content, "line-%03d\n", i)
	}
	if err := os.WriteFile(path, []byte(content.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{Workspace: workspace, TaskDir: t.TempDir()})
	m.width, m.height, m.view = 100, 30, viewNodeDetail
	m.selectedNode = nodes.CodeEdgeLint
	m.nodes[nodes.CodeEdgeLint] = domain.RunnerEvent{NodeID: nodes.CodeEdgeLint, Status: "succeeded", Path: path}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	scrolled := updated.(model)
	if scrolled.detailScroll == 0 || scrolled.detailViewport.YOffset == 0 {
		t.Fatalf("viewport did not page down: offset=%d component=%d", scrolled.detailScroll, scrolled.detailViewport.YOffset)
	}
	if rendered := scrolled.View(); !strings.Contains(rendered, "↕ 第") {
		t.Fatalf("scrolled viewport missing indicator: %s", rendered)
	}
}

func TestSpinnerComponentAdvancesOnTick(t *testing.T) {
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{})
	before := m.spinner.View()
	updated, cmd := m.Update(m.spinner.Tick())
	got := updated.(model)
	if after := got.spinner.View(); after == before {
		t.Fatalf("spinner frame did not advance: %q", after)
	}
	if cmd == nil {
		t.Fatal("spinner tick did not schedule its next frame")
	}
}

func TestResponsiveColumnsChangeComposition(t *testing.T) {
	wide := joinResponsiveColumns(layoutFor(140, 35), "LEFT", "RIGHT")
	stacked := joinResponsiveColumns(layoutFor(80, 35), "LEFT", "RIGHT")
	wideLines := strings.Split(wide, "\n")
	if len(wideLines) == 0 || !strings.Contains(wideLines[0], "LEFT") || !strings.Contains(wideLines[0], "RIGHT") {
		t.Fatalf("wide layout did not compose columns: %q", wide)
	}
	if strings.Contains(strings.Split(stacked, "\n")[0], "RIGHT") {
		t.Fatalf("stacked layout unexpectedly used columns: %q", stacked)
	}
}

func TestMouseSelectsAdvancedFormGroup(t *testing.T) {
	m := initialStartModel(context.Background(), func() {}, app.RunnerOptions{Workspace: t.TempDir(), TaskDir: t.TempDir()})
	m.startStep = startStepAdvanced
	m.width, m.height = 120, 35
	m.handleMouse(tea.MouseMsg(tea.MouseEvent{X: 4, Y: 10, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft}))
	if m.selectedStartGroup != startGroupQuality {
		t.Fatalf("mouse did not select quality group: %v", m.selectedStartGroup)
	}
}

func TestMouseOperatesNodeAndGateTargets(t *testing.T) {
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{Workspace: t.TempDir(), TaskDir: t.TempDir()})
	m.width, m.height = 100, 30
	m.nodes[nodes.CodeEdgeLint] = domain.RunnerEvent{NodeID: nodes.CodeEdgeLint, Status: "succeeded"}
	m.handleMouse(tea.MouseMsg(tea.MouseEvent{X: 8, Y: 5, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft}))
	if m.selectedNode != nodes.RepoPrepare {
		t.Fatalf("overview mouse did not select first visible row: %q", m.selectedNode)
	}
	m.view = viewGate
	m.activeGate = &domain.GateRequest{RequestID: "r1", GateID: nodes.FinalReview, Checklist: []domain.ChecklistItem{{ID: "ok", Passed: true}}}
	cmd := m.handleMouse(tea.MouseMsg(tea.MouseEvent{X: 2, Y: 29, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft}))
	if cmd == nil || m.confirm == nil || m.confirm.Action != confirmApprove {
		t.Fatalf("gate mouse did not open approve confirmation: confirm=%+v cmd=%v", m.confirm, cmd)
	}
}

func TestRenderedLayoutsStayWithinTerminalWidth(t *testing.T) {
	for _, width := range []int{60, 80, 100, 140} {
		m := initialStartModel(context.Background(), func() {}, app.RunnerOptions{Workspace: t.TempDir(), TaskDir: t.TempDir()})
		m.width, m.height = width, 32
		for _, line := range strings.Split(m.View(), "\n") {
			if got := lipgloss.Width(line); got > width {
				t.Fatalf("width %d rendered line %d columns: %q", width, got, line)
			}
		}
	}
}

func TestRuntimePagesStayWithinNarrowTerminalWidth(t *testing.T) {
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{Workspace: "ws", TaskDir: "task"})
	m.width, m.height = 60, 24
	m.nodes[nodes.CodeEdgeLint] = domain.RunnerEvent{NodeID: nodes.CodeEdgeLint, Status: "succeeded", Message: "完成"}
	m.selectedNode = nodes.CodeEdgeLint
	m.activeGate = &domain.GateRequest{GateID: nodes.FinalReview, Message: "请审查", Checklist: []domain.ChecklistItem{{Label: "检查", Passed: true}}}
	for _, view := range []viewMode{viewOverview, viewGate, viewNodeDetail, viewLogs, viewDone} {
		m.view = view
		m.done = view == viewDone
		m.summary.Passed = true
		rendered := strings.TrimSuffix(m.View(), "\n")
		pageLines := strings.Split(rendered, "\n")
		if len(pageLines) > m.height {
			t.Fatalf("view %v exceeded height: %d > %d\n%s", view, len(pageLines), m.height, rendered)
		}
		for _, line := range pageLines {
			if got := lipgloss.Width(line); got > m.width {
				t.Fatalf("view %v exceeded width: %d > %d: %q", view, got, m.width, line)
			}
		}
	}
}

func TestGateViewportScrollsAllContentAndFitsTerminal(t *testing.T) {
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{Workspace: "ws", TaskDir: "task"})
	m.width, m.height = 80, 24
	m.view = viewGate
	checklist := make([]domain.ChecklistItem, 36)
	for i := range checklist {
		checklist[i] = domain.ChecklistItem{ID: fmt.Sprintf("check-%02d", i), Label: fmt.Sprintf("检查项-%02d", i), Passed: true}
	}
	var artifact strings.Builder
	for i := 0; i < 80; i++ {
		fmt.Fprintf(&artifact, "工件行-%02d\n", i)
	}
	m.activeGate = &domain.GateRequest{
		RequestID: "req-scroll",
		GateID:    nodes.TaskReview,
		Message:   "请完整审查所有内容",
		Checklist: checklist,
		Artifacts: []domain.ArtifactPreview{{Name: "task.json", Content: artifact.String()}},
	}

	assertFits := func(label string, current model) string {
		t.Helper()
		rendered := strings.TrimSuffix(current.View(), "\n")
		if lines := len(strings.Split(rendered, "\n")); lines > current.height {
			t.Fatalf("%s gate rendered %d lines above terminal height %d\n%s", label, lines, current.height, rendered)
		}
		return rendered
	}
	initial := assertFits("initial", m)
	if !strings.Contains(initial, "检查项-00") || strings.Contains(initial, "工件行-79") {
		t.Fatalf("initial viewport window is incorrect:\n%s", initial)
	}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	m = updated.(model)
	if m.gateScroll == 0 {
		t.Fatal("PgDown did not advance gate viewport")
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	m = updated.(model)
	if m.gateScroll != 0 {
		t.Fatalf("PgUp did not return one-page scroll to top: %d", m.gateScroll)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnd})
	m = updated.(model)
	bottom := assertFits("bottom", m)
	if m.gateScroll == 0 || !strings.Contains(bottom, "工件行-79") || strings.Contains(bottom, "检查项-00") {
		t.Fatalf("End did not reach gate bottom: offset=%d\n%s", m.gateScroll, bottom)
	}
	if !strings.Contains(bottom, "PgUp/PgDn") {
		t.Fatalf("fixed footer disappeared while scrolling:\n%s", bottom)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyHome})
	m = updated.(model)
	if m.gateScroll != 0 {
		t.Fatalf("Home did not return to top: %d", m.gateScroll)
	}
	updated, _ = m.Update(tea.MouseMsg(tea.MouseEvent{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown}))
	m = updated.(model)
	if m.gateScroll == 0 {
		t.Fatal("mouse wheel did not scroll gate viewport")
	}
}

func TestStartWizardFitsTerminalHeight(t *testing.T) {
	for _, size := range []struct{ width, height int }{{60, 18}, {80, 24}, {120, 32}} {
		m := initialStartModel(context.Background(), func() {}, app.RunnerOptions{Workspace: t.TempDir(), TaskDir: t.TempDir()})
		m.width, m.height = size.width, size.height
		for _, step := range []startStep{startStepBasic, startStepAdvanced} {
			if step == startStepAdvanced {
				updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
				m = updated.(model)
			} else {
				m.startStep = startStepBasic
			}
			rendered := strings.TrimSuffix(m.View(), "\n")
			if lines := len(strings.Split(rendered, "\n")); lines > size.height {
				t.Fatalf("%dx%d step %v rendered %d lines\n%s", size.width, size.height, step, lines, rendered)
			}
		}
	}
}

func TestAllDestructiveActionsUseConfirmation(t *testing.T) {
	taskDir := t.TempDir()
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{Workspace: t.TempDir(), TaskDir: taskDir})
	m.width, m.height = 100, 30
	cancelModel, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlX})
	if got := cancelModel.(model).confirm; got == nil || got.Action != confirmCancelRun {
		t.Fatalf("cancel run did not require confirmation: %+v", got)
	}
	quitModel, _ := m.Update(runeKey("q"))
	if got := quitModel.(model).confirm; got == nil || got.Action != confirmQuit {
		t.Fatalf("quit did not require confirmation: %+v", got)
	}
	instruction := filepath.Join(taskDir, "instruction.md")
	if err := os.WriteFile(instruction, []byte("task"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.view = viewNodeDetail
	m.selectedNode = nodes.InstructionGen
	m.nodes[nodes.InstructionGen] = domain.RunnerEvent{NodeID: nodes.InstructionGen, Status: "succeeded", Artifacts: []domain.ArtifactPreview{{Name: "instruction.md", Path: instruction}}}
	artifact, ok := m.selectedNodeArtifact()
	if !ok {
		t.Fatal("test setup did not expose node artifact")
	}
	if _, err := m.safeEditableArtifactPath(artifact.Path); err != nil {
		t.Fatalf("test setup artifact is not editable: %v", err)
	}
	editModel, editCmd := m.Update(runeKey("e"))
	edited := editModel.(model)
	if got := edited.confirm; got == nil || got.Action != confirmEditArtifact {
		t.Fatalf("artifact edit did not require confirmation: confirm=%+v cmd=%v err=%v toast=%q view=%v router=%v", got, editCmd, edited.err, edited.toast.Message, edited.view, edited.router.Active())
	}
}

func TestKnownUIChromeContainsNoLegacyEnglishLabels(t *testing.T) {
	m := initialStartModel(context.Background(), func() {}, app.RunnerOptions{Workspace: t.TempDir(), TaskDir: t.TempDir()})
	m.width, m.height = 120, 32
	views := []string{m.View()}
	runtime := initialModel(context.Background(), func() {}, app.RunnerOptions{Workspace: t.TempDir(), TaskDir: t.TempDir()})
	runtime.width, runtime.height = 120, 32
	runtime.nodes[nodes.CodeEdgeLint] = domain.RunnerEvent{NodeID: nodes.CodeEdgeLint, Status: "succeeded"}
	runtime.selectedNode = nodes.CodeEdgeLint
	views = append(views, runtime.overview(), runtime.nodeDetailView(), runtime.logsView())
	runtime.activeGate = &domain.GateRequest{GateID: nodes.FinalReview, GateName: "Final Review"}
	views = append(views, runtime.gateView())
	runtime.done = true
	runtime.summary.Passed = true
	views = append(views, runtime.doneView())
	rendered := strings.Join(views, "\n")
	for _, forbidden := range []string{"Start Workflow", "Run existing task", "Workspace", "Docker verify", "Cancel model", "Overview", "Node Detail", "Logs", "Done", "Checklist", "Artifact", "Read-only snapshot"} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("UI chrome still contains legacy English label %q: %s", forbidden, rendered)
		}
	}
}

func TestStartViewSnapshots(t *testing.T) {
	basic := initialStartModel(context.Background(), func() {}, app.RunnerOptions{Workspace: ".harbor-factory/workspace", TaskDir: "task"})
	basic.width, basic.height = 80, 24
	assertViewContract(t, "basic", basic.View(), basic.width, basic.height, "启动工作流", "● 1 基本配置", "运行已有任务", "任务路径", "Enter 下一步")

	advancedModel, _ := basic.Update(tea.KeyMsg{Type: tea.KeyEnter})
	advanced := advancedModel.(model)
	advanced.width, advanced.height = 120, 32
	assertViewContract(t, "advanced", advanced.View(), advanced.width, advanced.height, "● 2 高级选项", "Harbor 配置", "Qwen 模型", "Harbor 预检", "Enter 启动")
}

func assertViewContract(t *testing.T, name, rendered string, width, height int, required ...string) {
	t.Helper()
	for _, text := range required {
		if !strings.Contains(rendered, text) {
			t.Errorf("%s view missing %q", name, text)
		}
	}
	lines := strings.Split(strings.TrimSuffix(rendered, "\n"), "\n")
	if len(lines) > height {
		t.Fatalf("%s view exceeded height: %d > %d", name, len(lines), height)
	}
	for _, line := range lines {
		if got := lipgloss.Width(line); got > width {
			t.Fatalf("%s view exceeded width: %d > %d: %q", name, got, width, line)
		}
	}
}

func TestGateNotesTextareaSupportsChineseMultiline(t *testing.T) {
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{})
	m.view = viewGate
	m.activeGate = &domain.GateRequest{RequestID: "r1", GateID: nodes.FinalReview}
	updated, _ := m.Update(runeKey("n"))
	m = updated.(model)
	updated, _ = m.Update(runeKey("第一行"))
	m = updated.(model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	updated, _ = m.Update(runeKey("第二行"))
	m = updated.(model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(model)
	if m.gateEditingNote || !strings.Contains(m.gateNotes, "第一行\n第二行") {
		t.Fatalf("multiline notes failed: %q", m.gateNotes)
	}
}

func TestResponsiveLayoutBreakpoints(t *testing.T) {
	cases := []struct {
		width int
		want  layoutMode
	}{{60, layoutMinimal}, {80, layoutStacked}, {100, layoutMedium}, {140, layoutWide}}
	for _, tc := range cases {
		if got := layoutFor(tc.width, 30); got.Mode != tc.want || got.ContentWidth < 24 || got.ContentHeight < 6 {
			t.Errorf("layout %d: %+v", tc.width, got)
		}
	}
}

func TestSearchFiltersOverview(t *testing.T) {
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{})
	m.width = 120
	m.height = 30
	m.nodes = map[string]domain.RunnerEvent{nodes.CodeEdgeLint: {NodeID: nodes.CodeEdgeLint, Status: "succeeded", Message: "lint passed"}, nodes.Package: {NodeID: nodes.Package, Status: "pending", Message: "waiting"}}
	m.filter = "lint"
	rendered := m.overview()
	if !strings.Contains(rendered, "codeedge_lint") || strings.Contains(rendered, "package") {
		t.Fatalf("overview filter failed: %s", rendered)
	}
}

func TestSearchInputAppliesFilterThroughKeyboard(t *testing.T) {
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{})
	m.width, m.height = 100, 30
	m.nodes[nodes.CodeEdgeLint] = domain.RunnerEvent{NodeID: nodes.CodeEdgeLint, Status: "succeeded", Message: "lint passed"}
	opened, _ := m.Update(runeKey("/"))
	m = opened.(model)
	if !m.searching || m.focusMgr.Current() != focusSearch {
		t.Fatal("slash did not focus search input")
	}
	typed, _ := m.Update(runeKey("lint"))
	m = typed.(model)
	applied, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = applied.(model)
	if m.searching || m.filter != "lint" || !strings.Contains(m.View(), "筛选：lint") {
		t.Fatalf("search was not applied: searching=%v filter=%q", m.searching, m.filter)
	}
}

func TestSearchFiltersGateChecklistAndArtifacts(t *testing.T) {
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{})
	m.width, m.height, m.view = 120, 32, viewGate
	m.filter = "target"
	m.activeGate = &domain.GateRequest{
		GateID:    nodes.FinalReview,
		Checklist: []domain.ChecklistItem{{ID: "target-check", Label: "目标检查", Passed: true}, {ID: "other", Label: "其他检查", Passed: true}},
		Artifacts: []domain.ArtifactPreview{{Name: "target.json", Content: "target body"}, {Name: "other.json", Content: "other body"}},
	}
	rendered := m.gateView()
	if !strings.Contains(rendered, "目标检查") || !strings.Contains(rendered, "target.json") || strings.Contains(rendered, "其他检查") || strings.Contains(rendered, "other.json") {
		t.Fatalf("gate filter failed: %s", rendered)
	}
}

func TestPathCompletionHandlesCJK(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "中文目录")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	completed, matches := completePath(filepath.Join(root, "中"))
	if len(matches) != 1 || completed != dir+string(filepath.Separator) {
		t.Fatalf("path completion: completed=%q matches=%v", completed, matches)
	}
}

func TestPathCompletionHandlesCommaSeparatedDirectoryLists(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "第二目录")
	for _, dir := range []string{first, second} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	m := initialStartModel(context.Background(), func() {}, app.RunnerOptions{Workspace: t.TempDir(), TaskDir: t.TempDir()})
	m.startStep = startStepAdvanced
	m.selectedStartGroup = startGroupQuality
	m.startCollapsed[startGroupHarbor] = true
	m.startCollapsed[startGroupQuality] = false
	m.startField = startFieldHistoryDirs
	input := m.startInputs[startFieldHistoryDirs]
	input.SetValue(first + "," + filepath.Join(root, "第"))
	m.startInputs[startFieldHistoryDirs] = input
	m.completeFocusedPath()
	if got := m.startInputs[startFieldHistoryDirs].Value(); got != first+","+second+string(filepath.Separator) {
		t.Fatalf("list path completion failed: %q", got)
	}
}

func TestReadPreviewDoesNotSplitUTF8(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cjk.txt")
	if err := os.WriteFile(path, []byte("中文中文中文"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := readPreview(path, 5)
	if strings.ContainsRune(got, '�') || !strings.Contains(got, "内容已截断") {
		t.Fatalf("invalid UTF-8 preview: %q", got)
	}
}

func TestSafeEditorCommandDoesNotUseShell(t *testing.T) {
	t.Setenv("VISUAL", `code --wait --reuse-window`)
	t.Setenv("EDITOR", "")
	path := "/tmp/a file; touch PWNED.md"
	cmd := safeEditorCommand(path)
	if filepath.Base(cmd.Path) != "code" || len(cmd.Args) != 4 || cmd.Args[len(cmd.Args)-1] != path {
		t.Fatalf("unsafe/incorrect editor command: path=%q args=%#v", cmd.Path, cmd.Args)
	}
}

func TestFooterAndHelpAreContextAware(t *testing.T) {
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{})
	m.width = 100
	m.view = viewLogs
	if footer := m.footer(); !strings.Contains(footer, "PgUp/PgDn") || strings.Contains(footer, "批准") {
		t.Fatalf("logs footer not contextual: %s", footer)
	}
	updated, _ := m.Update(runeKey("?"))
	shown := updated.(model)
	if !shown.helpVisible || !strings.Contains(shown.View(), "快捷键帮助") {
		t.Fatal("help overlay not shown")
	}
}

func TestFooterHidesUnavailableActions(t *testing.T) {
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{})
	m.readOnly = true
	if footer := m.footer(); strings.Contains(footer, "取消运行") || strings.Contains(footer, "编辑") {
		t.Fatalf("read-only footer exposed mutation: %s", footer)
	}
	m.readOnly = false
	m.view = viewGate
	m.activeGate = &domain.GateRequest{GateID: nodes.TaskReview}
	if footer := m.footer(); strings.Contains(footer, "修订") || strings.Contains(footer, "刷新") {
		t.Fatalf("task review footer exposed unavailable revise action: %s", footer)
	}
	m.activeGate.GateID = nodes.FinalReview
	if footer := m.footer(); !strings.Contains(footer, "Codex指导返修") || !strings.Contains(footer, "Codex自动循环") || !strings.Contains(footer, "人工编辑后重跑") {
		t.Fatalf("final review footer hid repair actions: %s", footer)
	}
}

func TestToastExpirationDoesNotClearNewerMessage(t *testing.T) {
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{})
	m.showToast("第一条", toastInfo)
	firstID := m.toast.ID
	m.showToast("第二条", toastSuccess)
	newer, _ := m.Update(toastExpiredMsg{id: firstID})
	m = newer.(model)
	if m.toast.Message != "第二条" {
		t.Fatalf("older expiration cleared newer toast: %+v", m.toast)
	}
	expired, _ := m.Update(toastExpiredMsg{id: m.toast.ID})
	if expired.(model).toast.Message != "" {
		t.Fatal("current toast did not expire")
	}
}
