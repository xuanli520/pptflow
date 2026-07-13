package tui

import (
	"context"
	"strconv"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/nodes"
)

func clickRenderedMarker(t *testing.T, m *model, marker string) tea.Cmd {
	t.Helper()
	targets := newRenderedFrame(m.View()).targets(marker, func() tea.Cmd { return nil })
	if len(targets) == 0 {
		t.Fatalf("rendered marker %q is not clickable in:\n%s", marker, m.View())
	}
	target := targets[0]
	return m.handleMouse(tea.MouseMsg(tea.MouseEvent{
		X: target.x, Y: target.y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
	}))
}

func TestStartTextInputOwnsPrintableNavigationRunes(t *testing.T) {
	m := initialStartModel(context.Background(), func() {}, app.RunnerOptions{Workspace: t.TempDir(), TaskDir: t.TempDir()})
	m.width, m.height = 120, 35
	m.startStep = startStepAdvanced
	m.selectAdvancedGroupByNumber(1)
	m.startField = startFieldHarborConcurrency
	input := m.startInputs[m.startField]
	input.SetValue("")
	m.startInputs[m.startField] = input
	m.dirtyStartInputs[m.startField] = true
	m.focusStartInput(m.startField)
	wantGroup := m.selectedStartGroup

	updated, _ := m.Update(runeKey("1234?"))
	got := updated.(model)
	if got.selectedStartGroup != wantGroup {
		t.Fatalf("printable input switched group: got=%v want=%v", got.selectedStartGroup, wantGroup)
	}
	if value := got.startInputs[startFieldHarborConcurrency].Value(); value != "1234?" {
		t.Fatalf("printable runes did not reach focused input: %q", value)
	}
	if got.helpVisible {
		t.Fatal("question mark opened help while a text input was focused")
	}
}

func TestStartGroupUsesFunctionKeysOnly(t *testing.T) {
	m := initialStartModel(context.Background(), func() {}, app.RunnerOptions{Workspace: t.TempDir(), TaskDir: t.TempDir()})
	m.startStep = startStepAdvanced
	m.selectAdvancedGroupByNumber(1)
	m.startField = startFieldRunHarbor

	plain, _ := m.Update(runeKey("2"))
	if got := plain.(model).selectedStartGroup; got != startGroupHarbor {
		t.Fatalf("plain digit switched advanced group: %v", got)
	}
	function, _ := plain.(model).Update(tea.KeyMsg{Type: tea.KeyF2})
	if got := function.(model).selectedStartGroup; got != startGroupQuality {
		t.Fatalf("F2 did not switch advanced group: %v", got)
	}
}

func TestHubSearchConsumesGlobalShortcutCharacters(t *testing.T) {
	m := initialLifecycleHubModel(context.Background(), func() {}, app.RunnerOptions{}, &fakeTaskHubLifecycle{})
	m.hubSearching = true
	m.hubSearch.Focus()

	updated, _ := m.Update(runeKey("q1234"))
	got := updated.(model)
	if got.view != viewHub || !got.hubSearching {
		t.Fatalf("hub search leaked into global navigation: view=%v searching=%v", got.view, got.hubSearching)
	}
	if value := got.hubSearch.Value(); value != "q1234" {
		t.Fatalf("hub search lost shortcut characters: %q", value)
	}
}

func TestMouseClickFocusesAndTogglesVisibleStartFields(t *testing.T) {
	m := initialStartModel(context.Background(), func() {}, app.RunnerOptions{Workspace: t.TempDir(), TaskDir: t.TempDir()})
	m.width, m.height = 120, 35
	clickRenderedMarker(t, &m, localizeField(startFieldWorkspace))
	if m.startField != startFieldWorkspace || !m.startInputs[startFieldWorkspace].Focused() {
		t.Fatalf("text field click did not focus input: field=%v focused=%v", m.startField, m.startInputs[startFieldWorkspace].Focused())
	}

	m.startStep = startStepAdvanced
	m.selectAdvancedGroupByNumber(1)
	before := m.opts.HarborPreflight
	clickRenderedMarker(t, &m, localizeField(startFieldHarborPreflight))
	if m.startField != startFieldHarborPreflight || m.opts.HarborPreflight == before {
		t.Fatalf("checkbox click did not focus and toggle: field=%v value=%v", m.startField, m.opts.HarborPreflight)
	}
}

func TestMouseUsesExactGateActionTarget(t *testing.T) {
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{Workspace: t.TempDir(), TaskDir: t.TempDir()})
	m.width, m.height, m.view = 100, 30, viewGate
	m.activeGate = &domain.GateRequest{RequestID: "r1", GateID: nodes.FinalReview, Checklist: []domain.ChecklistItem{{ID: "ok", Passed: true}}}

	m.handleMouse(tea.MouseMsg(tea.MouseEvent{X: 1, Y: m.height - 1, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft}))
	if m.confirm != nil {
		t.Fatal("blank footer area triggered a gate action")
	}
	clickRenderedMarker(t, &m, "[Ctrl+A/a 批准]")
	if m.confirm == nil || m.confirm.Action != confirmApprove {
		t.Fatalf("approve target did not open confirmation: %+v", m.confirm)
	}
}

func TestMouseConfirmationButtonsExecuteTheirChoice(t *testing.T) {
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{})
	m.width, m.height = 80, 24
	m.openConfirm(newConfirmDialog(confirmQuit, "确认", "确认退出"))
	clickRenderedMarker(t, &m, "  否  ")
	if m.confirm != nil {
		t.Fatal("clicking no did not close confirmation")
	}
	if strings.Contains(m.notice, "取消") {
		t.Fatalf("clicking no executed destructive action: %q", m.notice)
	}
}

func TestOverlayBlocksMouseWheelFromUnderlyingPage(t *testing.T) {
	m := initialModel(context.Background(), func() {}, app.RunnerOptions{})
	m.width, m.height, m.view = 100, 30, viewOverview
	m.syncOverviewTable()
	m.overviewTable.SetCursor(4)
	m.syncSelectedOverviewRow()
	m.openConfirm(newConfirmDialog(confirmQuit, "确认", "确认退出"))

	m.handleMouse(tea.MouseMsg(tea.MouseEvent{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown}))
	if got := m.overviewTable.Cursor(); got != 4 {
		t.Fatalf("overlay let wheel event reach underlying table: cursor=%d", got)
	}
}

func TestHarborConcurrencyDefaultsAndRejectsAboveDomainMaximum(t *testing.T) {
	opts := applyStartDefaults(app.RunnerOptions{})
	if opts.HarborConcurrency != domain.DefaultHarborConcurrency {
		t.Fatalf("default concurrency=%d want=%d", opts.HarborConcurrency, domain.DefaultHarborConcurrency)
	}
	m := initialStartModel(context.Background(), func() {}, app.RunnerOptions{})
	passMaximum := minInt(domain.MaxHarborConcurrency, domain.RequiredTrialCount)
	aboveMax := strconv.Itoa(passMaximum + 1)
	m.setStartInt(startFieldHarborConcurrency, aboveMax)
	if m.err == nil || !strings.Contains(m.err.Error(), strconv.Itoa(passMaximum)) {
		t.Fatalf("missing max concurrency validation: %v", m.err)
	}
	m.dirtyStartInputs[startFieldHarborConcurrency] = true
	input := m.startInputs[startFieldHarborConcurrency]
	input.SetValue(aboveMax)
	m.startInputs[startFieldHarborConcurrency] = input
	if err := m.validateDirtyStartInputs(); err == nil || !strings.Contains(err.Error(), strconv.Itoa(passMaximum)) {
		t.Fatalf("submission validation did not enforce max concurrency: %v", err)
	}

	m.setStartInt(startFieldHarborAttempts, "3")
	if m.err == nil || !strings.Contains(m.err.Error(), strconv.Itoa(domain.RequiredTrialCount)) {
		t.Fatalf("missing fixed pass@4 attempts validation: %v", m.err)
	}
}
