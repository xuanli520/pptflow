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
		model.notice = "无法验证工作区所有权；已作为只读快照打开。"
	} else if active {
		model = initialWorkspaceModel(runCtx, cancel, opts)
		model.notice = "工作区由另一个 Factory 进程持有；已作为只读实时快照打开。"
	} else if shouldResumeWorkspace(opts) {
		resumeOpts, _, err := app.LoadRunnerOptions(defaultWorkspace(opts.Workspace))
		if err == nil {
			resumeOpts.AutoApprove = false
			model = initialModel(runCtx, cancel, resumeOpts)
			model.notice = "正在从 run_options.json 恢复工作区。"
		} else {
			model = initialWorkspaceModel(runCtx, cancel, opts)
			model.err = err
		}
	} else if shouldOpenWorkspaceSnapshot(opts) {
		model = initialWorkspaceModel(runCtx, cancel, opts)
	} else if opts.Generate || opts.TaskDir != "" {
		model = initialModel(runCtx, cancel, opts)
	}
	program := newTeaProgram(model,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
		tea.WithReportFocus(),
	)
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
