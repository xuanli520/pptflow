package tui

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/nodes"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

func TestRunWithOptionsDefaultsToTaskHub(t *testing.T) {
	previous := newTeaProgram
	defer func() { newTeaProgram = previous }()
	var captured tea.Model
	newTeaProgram = func(model tea.Model, opts ...tea.ProgramOption) teaProgram {
		captured = model
		return fakeTeaProgram{model: model}
	}
	root := t.TempDir()
	err := RunWithOptions(context.Background(), app.RunnerOptions{Workspace: filepath.Join(root, "workspace")}, RunOptions{WorkspaceRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	got := captured.(model)
	if got.view != viewHub || got.runner != nil || got.store == nil || got.hubRoot != root {
		t.Fatalf("default TUI did not open Task Hub: view=%v runner=%v root=%q", got.view, got.runner, got.hubRoot)
	}
}

func TestHubLoadsAndOpensTerminalWorkspaceThenReturns(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspaces", "finished")
	writeHubWorkspace(t, workspace, domain.RunSummary{
		RunID:      "run-finished",
		Workspace:  workspace,
		Status:     "failed",
		Passed:     false,
		StartedAt:  time.Now().Add(-time.Minute),
		FinishedAt: time.Now(),
	}, app.RunnerOptions{Workspace: workspace, TaskDir: t.TempDir(), TaskName: "finished-task", CodeLang: "go", TaskType: "cli"})

	m, cleanup := testHubModel(t, root)
	defer cleanup()
	m.opts.HarborAgentEnv = []string{"ANTHROPIC_AUTH_TOKEN=${ANTHROPIC_AUTH_TOKEN}"}
	m.opts.QwenHarborBaseURL = "https://qwen.example"
	m.opts.OpusHarborBaseURL = "https://opus.example"
	m.runtimeOpts = app.ExtractRuntimeOptions(m.opts)
	loaded := m.loadHub(true)().(hubLoadedMsg)
	if loaded.err != nil {
		t.Fatal(loaded.err)
	}
	m.applyHubItems(loaded.items)
	if len(m.hubItems) != 1 || !strings.Contains(m.hubView(), "finished-task") {
		t.Fatalf("hub did not render indexed workspace: %s", m.hubView())
	}
	if cmd := m.openSelectedWorkspace(); cmd != nil {
		t.Fatal("opening terminal snapshot should not start an async command")
	}
	if m.view != viewDone || !m.done || !m.readOnly || m.summary.RunID != "run-finished" {
		t.Fatalf("terminal workspace did not open as Done snapshot: %+v", m)
	}
	if len(m.opts.HarborAgentEnv) != 1 || m.opts.QwenHarborBaseURL != "https://qwen.example" || m.opts.OpusHarborBaseURL != "https://opus.example" {
		t.Fatalf("opening historical snapshot dropped runtime credentials: %+v", m.opts)
	}
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	back := updated.(model)
	if back.view != viewHub || cmd == nil {
		t.Fatalf("Esc did not return to Hub: view=%v cmd=%v", back.view, cmd)
	}
}

func TestHubResumeOverlaySupportsResumeAndReadOnly(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspaces", "recoverable")
	writeHubWorkspace(t, workspace, domain.RunSummary{RunID: "old-run", Workspace: workspace, Status: "running"}, app.RunnerOptions{Workspace: workspace, TaskDir: t.TempDir(), TaskName: "recoverable"})
	m, cleanup := testHubModel(t, root)
	defer cleanup()
	loaded := m.loadHub(true)().(hubLoadedMsg)
	if loaded.err != nil {
		t.Fatal(loaded.err)
	}
	m.applyHubItems(loaded.items)
	if !m.hubItems[0].Run.IsResumable {
		t.Fatal("fixture should be resumable")
	}
	m.openSelectedWorkspace()
	if m.resumeOverlay == nil {
		t.Fatal("recoverable workspace did not open resume overlay")
	}
	resumeCmd := m.updateResumeKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if m.runner == nil || m.view != viewOverview || m.readOnly || resumeCmd == nil {
		t.Fatalf("resume choice did not create live runner: runner=%v view=%v readonly=%v", m.runner, m.view, m.readOnly)
	}

	m2, cleanup2 := testHubModel(t, root)
	defer cleanup2()
	loaded = m2.loadHub(true)().(hubLoadedMsg)
	m2.applyHubItems(loaded.items)
	m2.openSelectedWorkspace()
	m2.resumeOverlay.Selected = 2
	m2.updateResumeKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m2.runner != nil || !m2.readOnly || m2.view != viewOverview {
		t.Fatalf("read-only choice mutated workspace: runner=%v readonly=%v view=%v", m2.runner, m2.readOnly, m2.view)
	}
}

func TestHubRerunCloneStartsNewRunnerWithSelectedConfig(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspaces", "source")
	writeHubWorkspace(t, workspace, domain.RunSummary{RunID: "source-run", Workspace: workspace, Status: "failed", FinishedAt: time.Now()}, app.RunnerOptions{Workspace: workspace, TaskDir: t.TempDir(), TaskName: "source"})
	m, cleanup := testHubModel(t, root)
	defer cleanup()
	loaded := m.loadHub(true)().(hubLoadedMsg)
	m.applyHubItems(loaded.items)
	m.openRunConfigForSelected()
	if m.runConfig == nil {
		t.Fatal("rerun did not open config overlay")
	}
	target := filepath.Join(root, "workspaces", "source-retry-test")
	m.runConfig.Target.SetValue(target)
	m.runConfig.AutoApprove = true
	cmd := m.updateRunConfigKey(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("rerun confirmation did not prepare clone command")
	}
	msg := cmd().(clonePreparedMsg)
	if msg.err != nil {
		t.Fatal(msg.err)
	}
	updated, runCmd := m.Update(msg)
	running := updated.(model)
	if running.runner == nil || running.view != viewOverview || running.opts.Workspace != target || !running.opts.AutoApprove || runCmd == nil {
		t.Fatalf("clone did not start configured runner: %+v", running.opts)
	}
	if _, err := os.Stat(filepath.Join(target, "run_options.json")); err != nil {
		t.Fatalf("cloned options missing: %v", err)
	}
}

func TestDoneRerunFromLegacySnapshotCarriesCurrentRuntimeCredentials(t *testing.T) {
	for _, key := range []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_BASE_URL", "QWEN_HARBOR_BASE_URL", "OPUS_HARBOR_BASE_URL"} {
		t.Setenv(key, "")
	}
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "runtime-secret")
	t.Setenv("QWEN_HARBOR_BASE_URL", "https://qwen.current")
	t.Setenv("OPUS_HARBOR_BASE_URL", "https://opus.current")

	root := t.TempDir()
	workspace := filepath.Join(root, "workspaces", "legacy")
	writeHubWorkspace(t, workspace, domain.RunSummary{RunID: "legacy-run", Workspace: workspace, Status: "failed", FinishedAt: time.Now()}, app.RunnerOptions{Workspace: workspace, TaskDir: t.TempDir(), TaskName: "legacy"})
	legacySnapshot := domain.RunnerOptionsSnapshot{
		SchemaVersion: "harbor.runner_options.v1",
		Workspace:     workspace,
		TaskDir:       t.TempDir(),
		TaskName:      "legacy",
		RunHarbor:     true,
		HarborAgent:   "claude-code",
	}
	raw, err := json.Marshal(legacySnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nodes.RunOptionsPath(workspace), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	m, cleanup := testHubModel(t, root)
	defer cleanup()
	loaded := m.loadHub(true)().(hubLoadedMsg)
	if loaded.err != nil {
		t.Fatal(loaded.err)
	}
	m.applyHubItems(loaded.items)
	m.openSelectedWorkspace()
	if m.view != viewDone || len(m.runtimeOpts.HarborAgentEnv) != 1 {
		t.Fatalf("legacy snapshot did not retain runtime context: view=%v runtime=%+v", m.view, m.runtimeOpts)
	}
	m.openRunConfig(m.opts.Workspace, m.opts.TaskName)
	target := filepath.Join(root, "workspaces", "legacy-retry")
	m.runConfig.Target.SetValue(target)
	cmd := m.updateRunConfigKey(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("legacy rerun did not create clone command")
	}
	prepared := cmd().(clonePreparedMsg)
	if prepared.err != nil {
		t.Fatal(prepared.err)
	}
	if len(prepared.opts.HarborAgentEnv) != 1 || prepared.opts.QwenHarborBaseURL != "https://qwen.current" || prepared.opts.OpusHarborBaseURL != "https://opus.current" {
		t.Fatalf("cloned options lost current runtime: %+v", prepared.opts)
	}
	clonedRaw, err := os.ReadFile(nodes.RunOptionsPath(target))
	if err != nil {
		t.Fatal(err)
	}
	clonedText := string(clonedRaw)
	if !strings.Contains(clonedText, "ANTHROPIC_AUTH_TOKEN") || !strings.Contains(clonedText, "https://qwen.current") || !strings.Contains(clonedText, "https://opus.current") {
		t.Fatalf("cloned snapshot lacks runtime references: %s", clonedText)
	}
	if strings.Contains(clonedText, "runtime-secret") {
		t.Fatal("cloned snapshot persisted the credential value")
	}
}

func TestHubDeleteRemovesWorkspaceAndIndex(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspaces", "delete-me")
	writeHubWorkspace(t, workspace, domain.RunSummary{RunID: "delete-run", Workspace: workspace, Status: "failed", FinishedAt: time.Now()}, app.RunnerOptions{Workspace: workspace, TaskDir: t.TempDir(), TaskName: "delete-me"})
	m, cleanup := testHubModel(t, root)
	defer cleanup()
	loaded := m.loadHub(true)().(hubLoadedMsg)
	m.applyHubItems(loaded.items)
	m.confirmDeleteSelectedWorkspace()
	if m.confirm == nil || m.confirm.Action != confirmDeleteWorkspace {
		t.Fatal("delete did not require confirmation")
	}
	cmd := m.updateConfirmKey(tea.KeyMsg{Type: tea.KeyEnter})
	msg := cmd().(workspaceDeletedMsg)
	if msg.err != nil {
		t.Fatal(msg.err)
	}
	updated, _ := m.Update(msg)
	m = updated.(model)
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("workspace still exists after delete: %v", err)
	}
	indexed, err := m.store.GetRunByWorkspace(workspace)
	if err != nil || indexed != nil {
		t.Fatalf("deleted workspace remains indexed: run=%+v err=%v", indexed, err)
	}
}

func TestHubEmptyStateAndSortCycle(t *testing.T) {
	m, cleanup := testHubModel(t, t.TempDir())
	defer cleanup()
	m.width, m.height = 100, 30
	if rendered := m.hubView(); !strings.Contains(rendered, "暂无工作区") {
		t.Fatalf("empty state missing: %s", rendered)
	}
	want := []store.SortColumn{store.SortByTaskName, store.SortByStatus, store.SortBySizeBytes, store.SortByStartedAt}
	current := store.SortByStartedAt
	for _, expected := range want {
		current = nextHubSort(current)
		if current != expected {
			t.Fatalf("sort cycle=%v, want %v", current, expected)
		}
	}
}

func testHubModel(t *testing.T, root string) (model, func()) {
	t.Helper()
	s, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := initialHubModel(ctx, cancel, app.RunnerOptions{}, s, root, []string{root})
	m.width, m.height = 110, 32
	return m, func() {
		cancel()
		s.Close()
	}
}

func writeHubWorkspace(t *testing.T, workspace string, summary domain.RunSummary, opts app.RunnerOptions) {
	t.Helper()
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "state.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := app.SaveRunnerOptions(opts); err != nil {
		t.Fatal(err)
	}
}
