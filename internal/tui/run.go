package tui

import (
	"context"
	"errors"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/purplevoid/harbor-factory/internal/app"
)

type teaProgram interface {
	Run() (tea.Model, error)
}

var newTeaProgram = func(model tea.Model, opts ...tea.ProgramOption) teaProgram {
	return tea.NewProgram(model, opts...)
}

type RunOptions struct {
	// Lifecycle enables the V2 Task Hub. When present, all lifecycle reads and
	// plan requests use this injected app-service boundary. No legacy workspace
	// index, scheduler, or direct filesystem mutation path remains.
	Lifecycle TaskHubLifecycleService
}

// ErrLegacyWorkspaceTUIUnavailable enforces the lifecycle hard cutover. The
// former workspace UI could clone directories, resume a filesystem run, and
// mutate live evidence without a durable lifecycle command.
var ErrLegacyWorkspaceTUIUnavailable = errors.New("legacy workspace TUI is unavailable after the lifecycle cutover; use RunWithLifecycle")

func Run(_ context.Context, _ app.RunnerOptions) error {
	return ErrLegacyWorkspaceTUIUnavailable
}

func RunWithOptions(ctx context.Context, opts app.RunnerOptions, runOptions RunOptions) error {
	if runOptions.Lifecycle == nil {
		return ErrLegacyWorkspaceTUIUnavailable
	}
	opts.AutoApprove = false
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	model := initialLifecycleHubModel(runCtx, cancel, opts, runOptions.Lifecycle)
	program := newTeaProgram(model,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
		tea.WithReportFocus(),
	)
	_, err := program.Run()
	return err
}

// RunWithLifecycle opens the service-backed V2 Task Hub without creating a
// legacy filesystem/SQLite workspace hub. The supplied service is normally an
// application-layer adapter around lifecycle services.
func RunWithLifecycle(ctx context.Context, opts app.RunnerOptions, lifecycle TaskHubLifecycleService) error {
	return RunWithOptions(ctx, opts, RunOptions{Lifecycle: lifecycle})
}
