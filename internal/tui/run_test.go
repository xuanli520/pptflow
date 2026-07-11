package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
)

type fakeTeaProgram struct {
	model tea.Model
	err   error
}

func (p fakeTeaProgram) Run() (tea.Model, error) {
	return p.model, p.err
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

func TestRunKeepsTerminalWorkspaceAsSnapshotEvenWithRunOptions(t *testing.T) {
	workspace := t.TempDir()
	if _, err := app.SaveRunnerOptions(app.RunnerOptions{Workspace: workspace, TaskDir: t.TempDir()}); err != nil {
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
