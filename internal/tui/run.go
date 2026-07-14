package tui

import (
	"context"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
)

type teaProgram interface {
	Run() (tea.Model, error)
}

var newTeaProgram = func(model tea.Model, opts ...tea.ProgramOption) teaProgram {
	return tea.NewProgram(model, opts...)
}

// RunWithLifecycle opens the service-backed V2 Task Hub. The package exposes
// no workspace Runner or scheduler entry point after the hard cutover.
func RunWithLifecycle(ctx context.Context, lifecycle TaskHubLifecycleService) error {
	if lifecycle == nil {
		return fmt.Errorf("Task Hub lifecycle service is required")
	}
	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	model := initialLifecycleHubModel(runContext, cancel, lifecycle)
	program := newTeaProgram(model,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
		tea.WithReportFocus(),
	)
	_, err := program.Run()
	return err
}
