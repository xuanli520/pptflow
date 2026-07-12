package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/nodes"
	"github.com/purplevoid/harbor-factory/internal/harbor/runlock"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
)

type teaProgram interface {
	Run() (tea.Model, error)
}

var newTeaProgram = func(model tea.Model, opts ...tea.ProgramOption) teaProgram {
	return tea.NewProgram(model, opts...)
}

type RunOptions struct {
	WorkspaceRoot     string
	WorkspaceExplicit bool
	Rescan            bool
}

func Run(ctx context.Context, opts app.RunnerOptions) error {
	return RunWithOptions(ctx, opts, RunOptions{
		WorkspaceRoot:     defaultHubRoot(opts.Workspace),
		WorkspaceExplicit: strings.TrimSpace(opts.Workspace) != "",
	})
}

func RunWithOptions(ctx context.Context, opts app.RunnerOptions, runOptions RunOptions) error {
	opts.AutoApprove = false
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	root := strings.TrimSpace(runOptions.WorkspaceRoot)
	if root == "" {
		root = ".harbor-factory"
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve workspace root: %w", err)
	}
	if runOptions.Rescan {
		if err := store.ResetDatabase(root); err != nil {
			return err
		}
	}
	dataStore, err := store.Open(root)
	if err != nil {
		// The DB is only an index. A corrupt database can be discarded and
		// rebuilt from workspace files without losing authoritative data.
		if resetErr := store.ResetDatabase(root); resetErr != nil {
			return fmt.Errorf("open workspace store: %w (reset failed: %v)", err, resetErr)
		}
		dataStore, err = store.Open(root)
		if err != nil {
			return fmt.Errorf("rebuild workspace store: %w", err)
		}
	}
	defer dataStore.Close()

	scanRoots := []string{root}
	if runOptions.WorkspaceExplicit && strings.TrimSpace(opts.Workspace) != "" && !pathWithinDirectory(opts.Workspace, root) {
		scanRoots = append(scanRoots, opts.Workspace)
	}
	hub := initialHubModel(runCtx, cancel, opts, dataStore, root, scanRoots)
	model := hub
	if opts.Generate || opts.TaskDir != "" {
		model = hub.attachHubContext(initialModel(runCtx, cancel, opts))
	} else if runOptions.WorkspaceExplicit {
		model = hub.attachHubContext(initialStartModel(runCtx, cancel, opts))
		active, activeErr := workspaceRunActive(opts)
		if activeErr != nil {
			model = hub.attachHubContext(initialWorkspaceModel(runCtx, cancel, opts))
			model.err = activeErr
			model.notice = "无法验证工作区所有权；已作为只读快照打开。"
		} else if active {
			model = hub.attachHubContext(initialWorkspaceModel(runCtx, cancel, opts))
			model.notice = "工作区由另一个 Factory 进程持有；已作为只读实时快照打开。"
		} else if shouldResumeWorkspace(opts) {
			resumeOpts, _, loadErr := app.LoadRunnerOptions(defaultWorkspace(opts.Workspace))
			if loadErr == nil {
				resumeOpts = app.MergeRuntimeOptions(resumeOpts, opts)
				resumeOpts.AutoApprove = false
				model = hub.attachHubContext(initialModel(runCtx, cancel, resumeOpts))
				model.notice = "正在从 run_options.json 恢复工作区。"
			} else {
				model = hub.attachHubContext(initialWorkspaceModel(runCtx, cancel, opts))
				model.err = loadErr
			}
		} else if shouldOpenWorkspaceSnapshot(opts) {
			model = hub.attachHubContext(initialWorkspaceModel(runCtx, cancel, opts))
		}
	}
	program := newTeaProgram(model,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
		tea.WithReportFocus(),
	)
	_, err = program.Run()
	return err
}

func defaultHubRoot(workspace string) string {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		return ".harbor-factory"
	}
	clean := filepath.Clean(workspace)
	parent := filepath.Dir(clean)
	if filepath.Base(clean) == "workspace" {
		return parent
	}
	if filepath.Base(parent) == "workspaces" {
		return filepath.Dir(parent)
	}
	return clean
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
