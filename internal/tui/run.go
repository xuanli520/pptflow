package tui

import (
	"context"
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/nodes"
	"github.com/purplevoid/harbor-factory/internal/harbor/runlock"
)

type teaProgram interface {
	Run() (tea.Model, error)
}

var newTeaProgram = func(model tea.Model, opts ...tea.ProgramOption) teaProgram {
	return tea.NewProgram(model, opts...)
}

func Run(ctx context.Context, opts app.RunnerOptions) error {
	opts.AutoApprove = false
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	model := initialStartModel(runCtx, cancel, opts)
	active, activeErr := workspaceRunActive(opts)
	if activeErr != nil {
		model = initialWorkspaceModel(runCtx, cancel, opts)
		model.err = activeErr
		model.notice = "Workspace ownership could not be verified; opened as a read-only snapshot."
	} else if active {
		model = initialWorkspaceModel(runCtx, cancel, opts)
		model.notice = "Workspace is owned by an active Factory process; opened as a read-only live snapshot."
	} else if shouldResumeWorkspace(opts) {
		resumeOpts, _, err := app.LoadRunnerOptions(defaultWorkspace(opts.Workspace))
		if err == nil {
			resumeOpts.AutoApprove = false
			model = initialModel(runCtx, cancel, resumeOpts)
			model.notice = "Resuming workspace from run_options.json."
		} else {
			model = initialWorkspaceModel(runCtx, cancel, opts)
			model.err = err
		}
	} else if shouldOpenWorkspaceSnapshot(opts) {
		model = initialWorkspaceModel(runCtx, cancel, opts)
	} else if opts.Generate || opts.TaskDir != "" {
		model = initialModel(runCtx, cancel, opts)
	}
	program := newTeaProgram(model, tea.WithAltScreen())
	_, err := program.Run()
	return err
}

func workspaceRunActive(opts app.RunnerOptions) (bool, error) {
	if opts.Generate || opts.TaskDir != "" {
		return false, nil
	}
	return runlock.IsActive(defaultWorkspace(opts.Workspace))
}

func shouldResumeWorkspace(opts app.RunnerOptions) bool {
	if opts.Generate || opts.TaskDir != "" {
		return false
	}
	workspace := defaultWorkspace(opts.Workspace)
	if _, err := os.Stat(nodes.RunOptionsPath(workspace)); err != nil {
		return false
	}
	summary, _ := loadWorkspaceState(workspace)
	return !isTerminalRunSummary(summary)
}

func shouldOpenWorkspaceSnapshot(opts app.RunnerOptions) bool {
	if opts.Generate || opts.TaskDir != "" {
		return false
	}
	workspace := opts.Workspace
	if workspace == "" {
		workspace = filepath.Join(".harbor-factory", "workspace")
	}
	_, err := os.Stat(filepath.Join(workspace, "state.json"))
	return err == nil
}
