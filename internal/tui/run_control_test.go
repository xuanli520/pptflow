package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
)

func TestCtrlXOpensRunControlWithoutCanceling(t *testing.T) {
	cancelCalls := 0
	m := initialModel(context.Background(), func() { cancelCalls++ }, app.RunnerOptions{Workspace: t.TempDir(), TaskDir: t.TempDir()})
	m.width, m.height = 100, 30
	m.summary.RunID = "run-123"

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlX})
	got := updated.(model)
	if cmd != nil {
		t.Fatalf("Ctrl+X returned a command before an explicit control operation: %v", cmd)
	}
	if got.runControl == nil || got.runControl.RunID != "run-123" {
		t.Fatalf("Ctrl+X did not open a Run-targeted control overlay: %+v", got.runControl)
	}
	if cancelCalls != 0 {
		t.Fatalf("opening run control canceled a shared context %d times", cancelCalls)
	}
	if rendered := got.View(); !strings.Contains(rendered, "返回并保持运行") {
		t.Fatalf("run control did not render the safe default:\n%s", rendered)
	}

	closed, closeCmd := got.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if closeCmd != nil || closed.(model).runControl != nil {
		t.Fatalf("default run control action did not return safely: control=%+v cmd=%v", closed.(model).runControl, closeCmd)
	}
	if cancelCalls != 0 {
		t.Fatalf("returning from run control canceled a shared context %d times", cancelCalls)
	}
}

func TestRunControlPreviewRendersEveryFrozenPlanConsequence(t *testing.T) {
	preview := TaskHubPlanPreview{
		Title:               "暂停运行影响预览",
		Summary:             "确认后创建 durable control operation。",
		Reason:              "Run 正在运行",
		RevisionImpact:      "TaskRevision 不变。",
		ExecutionScope:      []string{"checkpoint", "quota settlement"},
		InvalidatedEvidence: []string{"active stage output"},
		ReusedEvidence:      []string{"completed quality evidence"},
		BudgetImpact:        "释放未使用 reservation",
		ExternalEffects:     []string{"请求 runtime graceful checkpoint"},
	}
	overlay := &RunControlOverlay{RunID: "run-1", State: "运行中", Preview: &preview}
	rendered := overlay.View(100, 60)
	for _, want := range []string{
		"原因：", "Run 正在运行", "Task 版本影响：", "TaskRevision 不变。",
		"将执行：", "checkpoint", "将失效：", "active stage output", "将复用：", "completed quality evidence",
		"预算变化：", "释放未使用 reservation", "外部副作用：", "请求 runtime graceful checkpoint",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("Run Control plan preview omitted %q:\n%s", want, rendered)
		}
	}
}

func TestRunControlUsesMostRecentEventRunIDBeforeSummaryProjection(t *testing.T) {
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{Workspace: t.TempDir(), TaskDir: t.TempDir()})
	m.events = []domain.RunnerEvent{{RunID: "run-old"}, {RunID: "run-current"}}
	m.openRunControl()
	if m.runControl == nil || m.runControl.RunID != "run-current" {
		t.Fatalf("run control did not target the latest known run: %+v", m.runControl)
	}
}

func TestCtrlXOpensRunControlBeforeTextInput(t *testing.T) {
	start := initialStartModel(context.Background(), func() {}, app.RunnerOptions{Workspace: t.TempDir(), TaskDir: t.TempDir()})
	start.startField = startFieldTaskName
	start.focusStartInput(start.startField)
	before := start.startInputs[start.startField].Value()
	updated, _ := start.Update(tea.KeyMsg{Type: tea.KeyCtrlX})
	got := updated.(model)
	if got.runControl == nil || got.startInputs[start.startField].Value() != before {
		t.Fatalf("Ctrl+X was consumed by a start-form input: control=%+v value=%q", got.runControl, got.startInputs[start.startField].Value())
	}

}

func TestPlainXNeverOpensOrCancelsRunControl(t *testing.T) {
	for _, view := range []viewMode{viewHub, viewStart, viewOverview, viewGate, viewNodeDetail, viewLogs, viewDone} {
		t.Run(fmt.Sprintf("view-%d", view), func(t *testing.T) {
			cancelCalls := 0
			m := initialModel(context.Background(), func() { cancelCalls++ }, app.RunnerOptions{Workspace: t.TempDir(), TaskDir: t.TempDir()})
			m.view = view
			m.done = view == viewDone
			m.width, m.height = 100, 30
			if view == viewGate {
				m.activeGate = &domain.GateRequest{GateID: "gate"}
			}

			updated, cmd := m.Update(runeKey("x"))
			got := updated.(model)
			if cmd != nil || got.runControl != nil || cancelCalls != 0 {
				t.Fatalf("plain x mutated run control in view %v: cmd=%v control=%+v cancelCalls=%d", view, cmd, got.runControl, cancelCalls)
			}
		})
	}
}

func TestQuitConfirmationDoesNotCancelSharedContext(t *testing.T) {
	cancelCalls := 0
	m := initialModel(context.Background(), func() { cancelCalls++ }, app.RunnerOptions{Workspace: t.TempDir(), TaskDir: t.TempDir()})

	updated, openedCmd := m.Update(runeKey("q"))
	pending := updated.(model)
	if pending.confirm == nil || pending.confirm.Action != confirmQuit || pending.confirm.FocusedYes {
		t.Fatalf("active run did not get a safe exit confirmation: %+v", pending.confirm)
	}
	if openedCmd == nil {
		t.Fatal("exit confirmation did not return its open event")
	}
	if cancelCalls != 0 {
		t.Fatalf("opening exit confirmation canceled a shared context %d times", cancelCalls)
	}

	pending.confirm.FocusedYes = true
	_, quitCmd := pending.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if quitCmd == nil {
		t.Fatal("confirmed safe detach did not request TUI exit")
	}
	message := quitCmd()
	if _, ok := message.(tea.QuitMsg); !ok {
		t.Fatalf("confirmed safe detach returned %T, want tea.QuitMsg", message)
	}
	if cancelCalls != 0 {
		t.Fatalf("TUI exit canceled a shared context %d times", cancelCalls)
	}
}

func TestMouseRunControlReturnDoesNotReachUnderlyingPage(t *testing.T) {
	cancelCalls := 0
	m := initialModel(context.Background(), func() { cancelCalls++ }, app.RunnerOptions{Workspace: t.TempDir(), TaskDir: t.TempDir()})
	m.width, m.height = 100, 30
	m.syncOverviewTable()
	m.overviewTable.SetCursor(3)
	m.openRunControl()

	clickRenderedMarker(t, &m, "  返回并保持运行  ")
	if m.runControl != nil || cancelCalls != 0 {
		t.Fatalf("mouse return did not close safely: control=%+v cancelCalls=%d", m.runControl, cancelCalls)
	}
	if got := m.overviewTable.Cursor(); got != 3 {
		t.Fatalf("run control mouse action reached the underlying page: cursor=%d", got)
	}
}
