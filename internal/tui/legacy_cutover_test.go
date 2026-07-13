package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/purplevoid/harbor-factory/internal/app"
)

type fakeTeaProgram struct {
	model tea.Model
	err   error
}

func (p fakeTeaProgram) Run() (tea.Model, error) {
	return p.model, p.err
}

func TestLegacyWorkspaceTUIEntryPointsAreUnavailable(t *testing.T) {
	if err := Run(context.Background(), app.RunnerOptions{Workspace: t.TempDir()}); !errors.Is(err, ErrLegacyWorkspaceTUIUnavailable) {
		t.Fatalf("Run error = %v, want legacy cutover error", err)
	}
	if err := RunWithOptions(context.Background(), app.RunnerOptions{Workspace: t.TempDir()}, RunOptions{}); !errors.Is(err, ErrLegacyWorkspaceTUIUnavailable) {
		t.Fatalf("RunWithOptions error = %v, want legacy cutover error", err)
	}
}

func TestLegacyWorkspaceTUISourcesAreAbsent(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve TUI package directory")
	}
	for _, name := range []string{"workspace_hub.go", "workspace_resume.go"} {
		_, err := os.Stat(filepath.Join(filepath.Dir(file), name))
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("legacy workspace TUI source %q remains available: %v", name, err)
		}
	}
}

func TestLifecycleHubIgnoresLegacyWorkspaceMutationKeys(t *testing.T) {
	service := &fakeTaskHubLifecycle{snapshot: enabledTaskHubSnapshot()}
	m := initialLifecycleHubModel(context.Background(), func() {}, app.RunnerOptions{}, service)
	m.width, m.height = 100, 30
	m.applyTaskHubSnapshot(service.snapshot)

	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyCtrlR},
		{Type: tea.KeyRunes, Runes: []rune{'f'}},
		{Type: tea.KeyDelete},
		{Type: tea.KeyRunes, Runes: []rune{'r'}},
		{Type: tea.KeyRunes, Runes: []rune{'e'}},
	} {
		updated, command := m.Update(key)
		m = updated.(model)
		if command != nil {
			t.Fatalf("legacy key %q returned a lifecycle command", key.String())
		}
	}
	if m.runner != nil || m.taskHubPlan != nil || service.planCallCount() != 0 {
		t.Fatalf("legacy workspace key reached lifecycle mutation state: runner=%v plan=%+v calls=%d", m.runner, m.taskHubPlan, service.planCallCount())
	}
}
