package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/nodes"
	"github.com/purplevoid/harbor-factory/internal/harbor/runlock"
)

type fakeTeaProgram struct {
	model tea.Model
	err   error
}

func (p fakeTeaProgram) Run() (tea.Model, error) {
	return p.model, p.err
}

func TestRunEnablesAltScreenMouseAndFocusReporting(t *testing.T) {
	previous := newTeaProgram
	defer func() { newTeaProgram = previous }()
	optionCount := 0
	newTeaProgram = func(model tea.Model, opts ...tea.ProgramOption) teaProgram {
		optionCount = len(opts)
		return fakeTeaProgram{model: model}
	}
	if err := Run(context.Background(), app.RunnerOptions{Workspace: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if optionCount != 3 {
		t.Fatalf("expected alt-screen, mouse and focus options, got %d", optionCount)
	}
}

func TestRunOpensWorkspaceSnapshotWhenNoTaskOrGenerate(t *testing.T) {
	workspace := t.TempDir()
	stateRaw, err := json.Marshal(domain.RunSummary{RunID: "run-1", Workspace: workspace, Status: "running"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "state.json"), stateRaw, 0o644); err != nil {
		t.Fatal(err)
	}

	var captured tea.Model
	previous := newTeaProgram
	newTeaProgram = func(model tea.Model, opts ...tea.ProgramOption) teaProgram {
		captured = model
		return fakeTeaProgram{model: model}
	}
	defer func() { newTeaProgram = previous }()

	if err := Run(context.Background(), app.RunnerOptions{Workspace: workspace}); err != nil {
		t.Fatal(err)
	}
	model, ok := captured.(model)
	if !ok {
		t.Fatalf("captured model has unexpected type %T", captured)
	}
	if model.runner != nil {
		t.Fatal("snapshot mode should not create a live runner")
	}
	if model.summary.RunID != "run-1" || model.summary.Workspace != workspace {
		t.Fatalf("snapshot mode did not load workspace state: %+v", model.summary)
	}
}

func TestRunResumesWorkspaceFromRunOptionsWhenNonTerminal(t *testing.T) {
	workspace := t.TempDir()
	taskDir := t.TempDir()
	if _, err := app.SaveRunnerOptions(app.RunnerOptions{
		Workspace:     workspace,
		TaskDir:       taskDir,
		QualityCheck:  true,
		HarborTimeout: 123,
	}); err != nil {
		t.Fatal(err)
	}
	stateRaw, err := json.Marshal(domain.RunSummary{RunID: "run-1", Workspace: workspace, Status: "running"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "state.json"), stateRaw, 0o644); err != nil {
		t.Fatal(err)
	}

	var captured tea.Model
	previous := newTeaProgram
	newTeaProgram = func(model tea.Model, opts ...tea.ProgramOption) teaProgram {
		captured = model
		return fakeTeaProgram{model: model}
	}
	defer func() { newTeaProgram = previous }()

	if err := Run(context.Background(), app.RunnerOptions{Workspace: workspace}); err != nil {
		t.Fatal(err)
	}
	model, ok := captured.(model)
	if !ok {
		t.Fatalf("captured model has unexpected type %T", captured)
	}
	if model.runner == nil {
		t.Fatal("non-terminal workspace with run_options.json should create a live runner")
	}
	if model.opts.TaskDir != taskDir || !model.opts.QualityCheck || model.opts.HarborTimeout != 123 || model.opts.AutoApprove {
		t.Fatalf("resume did not restore safe options for TUI: %+v", model.opts)
	}
	if model.notice == "" {
		t.Fatal("resume model should explain that run_options.json was used")
	}
}

func TestRunOpensActiveWorkspaceAsReadOnlySnapshot(t *testing.T) {
	workspace := t.TempDir()
	taskDir := t.TempDir()
	if _, err := app.SaveRunnerOptions(app.RunnerOptions{Workspace: workspace, TaskDir: taskDir}); err != nil {
		t.Fatal(err)
	}
	stateRaw, err := json.Marshal(domain.RunSummary{RunID: "run-active", Workspace: workspace, Status: "running"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "state.json"), stateRaw, 0o644); err != nil {
		t.Fatal(err)
	}
	gateEvent := domain.RunnerEvent{
		RunID:  "run-active",
		Type:   "gate_requested",
		NodeID: nodes.FinalReview,
		Status: "waiting",
		Gate: &domain.GateRequest{
			RequestID: "phase2:final_review",
			GateID:    nodes.FinalReview,
			GateName:  "Final Review",
		},
	}
	eventRaw, err := json.Marshal(gateEvent)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "event_log.jsonl"), append(eventRaw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := runlock.Acquire(workspace, runlock.Metadata{RunID: "run-active"})
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	var captured tea.Model
	previous := newTeaProgram
	newTeaProgram = func(model tea.Model, opts ...tea.ProgramOption) teaProgram {
		captured = model
		return fakeTeaProgram{model: model}
	}
	defer func() { newTeaProgram = previous }()

	if err := Run(context.Background(), app.RunnerOptions{Workspace: workspace}); err != nil {
		t.Fatal(err)
	}
	snapshot := captured.(model)
	if snapshot.runner != nil || !snapshot.readOnly || snapshot.summary.RunID != "run-active" || snapshot.activeGate == nil {
		t.Fatalf("active workspace must be a snapshot: runner=%v summary=%+v", snapshot.runner, snapshot.summary)
	}
	if !strings.Contains(snapshot.notice, "只读实时快照") {
		t.Fatalf("active snapshot notice missing: %q", snapshot.notice)
	}
	updated, cmd := snapshot.updateGateKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	readOnly := updated.(model)
	if cmd != nil || readOnly.activeGate == nil || readOnly.err == nil || !strings.Contains(readOnly.err.Error(), "只读") {
		t.Fatalf("read-only snapshot allowed gate mutation: cmd=%v model=%+v", cmd, readOnly)
	}
}

func TestRunKeepsTerminalWorkspaceAsSnapshotEvenWithRunOptions(t *testing.T) {
	workspace := t.TempDir()
	taskDir := t.TempDir()
	if _, err := app.SaveRunnerOptions(app.RunnerOptions{Workspace: workspace, TaskDir: taskDir, RepoURL: "https://github.com/org/repo.git"}); err != nil {
		t.Fatal(err)
	}
	stateRaw, err := json.Marshal(domain.RunSummary{RunID: "run-1", Workspace: workspace, Status: "failed", FinishedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "state.json"), stateRaw, 0o644); err != nil {
		t.Fatal(err)
	}

	var captured tea.Model
	previous := newTeaProgram
	newTeaProgram = func(model tea.Model, opts ...tea.ProgramOption) teaProgram {
		captured = model
		return fakeTeaProgram{model: model}
	}
	defer func() { newTeaProgram = previous }()

	if err := Run(context.Background(), app.RunnerOptions{Workspace: workspace}); err != nil {
		t.Fatal(err)
	}
	model, ok := captured.(model)
	if !ok {
		t.Fatalf("captured model has unexpected type %T", captured)
	}
	if model.runner != nil || !model.done || model.view != viewDone {
		t.Fatalf("terminal workspace should stay in snapshot done view: runner=%v done=%v view=%v", model.runner, model.done, model.view)
	}
	if model.opts.TaskDir != taskDir || model.opts.RepoURL != "https://github.com/org/repo.git" {
		t.Fatalf("terminal snapshot did not hydrate task/repo context: %+v", model.opts)
	}
}

func TestRunOpensStartFormWhenNoTaskGenerateOrSnapshot(t *testing.T) {
	workspace := t.TempDir()
	var captured tea.Model
	previous := newTeaProgram
	newTeaProgram = func(model tea.Model, opts ...tea.ProgramOption) teaProgram {
		captured = model
		return fakeTeaProgram{model: model}
	}
	defer func() { newTeaProgram = previous }()

	if err := Run(context.Background(), app.RunnerOptions{Workspace: workspace}); err != nil {
		t.Fatal(err)
	}
	model, ok := captured.(model)
	if !ok {
		t.Fatalf("captured model has unexpected type %T", captured)
	}
	if model.runner != nil {
		t.Fatal("start form should not create a runner before user starts")
	}
	if model.view != viewStart {
		t.Fatalf("expected start view, got %v", model.view)
	}
}

func TestRunStartsLiveModelWhenTaskProvided(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "state.json"), []byte(`{"run_id":"run-1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var captured tea.Model
	previous := newTeaProgram
	newTeaProgram = func(model tea.Model, opts ...tea.ProgramOption) teaProgram {
		captured = model
		return fakeTeaProgram{model: model}
	}
	defer func() { newTeaProgram = previous }()

	if err := Run(context.Background(), app.RunnerOptions{Workspace: workspace, TaskDir: "task"}); err != nil {
		t.Fatal(err)
	}
	model, ok := captured.(model)
	if !ok {
		t.Fatalf("captured model has unexpected type %T", captured)
	}
	if model.runner == nil {
		t.Fatal("task run should create a live runner even when workspace state exists")
	}
}

func TestBubbleTeaProgramRunHandlesWorkspaceKeyboardInput(t *testing.T) {
	workspace := t.TempDir()
	summary := domain.RunSummary{
		RunID:     "run-1",
		Workspace: workspace,
		Status:    "running",
		Events: []domain.RunnerEvent{{
			Type:   "node_succeeded",
			NodeID: "codeedge_lint",
			Status: "succeeded",
		}},
	}
	stateRaw, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "state.json"), stateRaw, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	initial := initialWorkspaceModel(ctx, cancel, app.RunnerOptions{Workspace: workspace})
	program := tea.NewProgram(
		initial,
		tea.WithContext(ctx),
		tea.WithInput(bytes.NewBuffer(nil)),
		tea.WithOutput(io.Discard),
		tea.WithoutRenderer(),
	)
	type runResult struct {
		model tea.Model
		err   error
	}
	done := make(chan runResult, 1)
	go func() {
		finalModel, err := program.Run()
		done <- runResult{model: finalModel, err: err}
	}()
	time.Sleep(10 * time.Millisecond)
	program.Send(tea.KeyMsg{Type: tea.KeyTab})
	program.Send(tea.Quit())
	result := <-done
	if result.err != nil {
		t.Fatal(result.err)
	}
	final, ok := result.model.(model)
	if !ok {
		t.Fatalf("final model has unexpected type %T", result.model)
	}
	if final.view != viewNodeDetail {
		t.Fatalf("expected tab key to switch to node detail before quit, got view=%v", final.view)
	}
	if final.selectedNode != "codeedge_lint" {
		t.Fatalf("expected workspace node to remain selected, got %q", final.selectedNode)
	}
}
