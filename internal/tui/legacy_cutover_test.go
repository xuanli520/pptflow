package tui

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

type fakeTeaProgram struct {
	model tea.Model
	err   error
}

func (program fakeTeaProgram) Run() (tea.Model, error) {
	return program.model, program.err
}

func runeKey(value string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(value)}
}

func clickRenderedMarker(t *testing.T, model *model, marker string) tea.Cmd {
	t.Helper()
	targets := newRenderedFrame(model.View()).targets(marker, func() tea.Cmd { return nil })
	if len(targets) == 0 {
		t.Fatalf("rendered marker %q is not clickable in:\n%s", marker, model.View())
	}
	target := targets[0]
	return model.handleMouse(tea.MouseMsg(tea.MouseEvent{
		X: target.x, Y: target.y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
	}))
}

func assertRenderedWidth(t *testing.T, name, rendered string, width int) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSuffix(rendered, "\n"), "\n") {
		if got := ansi.StringWidth(line); got > width {
			t.Fatalf("%s exceeded %d columns with width %d: %q", name, width, got, line)
		}
	}
}

func TestLifecycleHubCannotReachLegacyWorkspacePages(t *testing.T) {
	service := &fakeTaskHubLifecycle{snapshot: enabledTaskHubSnapshot()}
	hub := initialLifecycleHubModel(context.Background(), func() {}, service)
	hub.width, hub.height = 100, 30
	hub.applyTaskHubSnapshot(service.snapshot)

	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyCtrlO}, runeKey("2"), {Type: tea.KeyCtrlD}, runeKey("4"), {Type: tea.KeyCtrlL}, runeKey("5"),
	} {
		updated, command := hub.Update(key)
		hub = updated.(model)
		if command != nil {
			t.Fatalf("legacy navigation key %q returned a command", key.String())
		}
		if hub.view != viewHub || hub.lifecycle == nil {
			t.Fatalf("legacy navigation key %q escaped lifecycle hub: view=%v lifecycle=%v", key.String(), hub.view, hub.lifecycle != nil)
		}
	}
}

func TestLegacyWorkspaceTUISourcesAreAbsent(t *testing.T) {
	_, file, _, okay := runtime.Caller(0)
	if !okay {
		t.Fatal("resolve TUI package directory")
	}
	for _, name := range []string{
		"artifact.go", "checklist.go", "localize.go", "node_events.go",
		"page_detail.go", "page_done.go", "page_gate.go", "page_logs.go", "page_overview.go", "page_start.go",
		"start_form.go", "statusbar.go", "workspace_hub.go", "workspace_resume.go",
	} {
		_, err := os.Stat(filepath.Join(filepath.Dir(file), name))
		if !os.IsNotExist(err) {
			t.Fatalf("legacy workspace TUI source %q remains available: %v", name, err)
		}
	}
}
