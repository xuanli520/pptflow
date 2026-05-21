package tui

import (
	"context"
	"errors"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xuanli520/p2r_tui/internal/config"
	dockermgr "github.com/xuanli520/p2r_tui/internal/docker"
	"github.com/xuanli520/p2r_tui/internal/executor"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
)

type dockerHealthStore interface {
	ListTasksWithDockerRunning(context.Context) ([]model.Task, error)
	MarkTaskDockerStopped(context.Context, string) error
}

type schedulerPoller struct {
	store                  dockerHealthStore
	cfg                    config.Config
	exec                   executor.CommandRunner
	lastRecoveryAt         time.Time
	lastPersistedRefreshAt time.Time
	lastDockerHealthAt     time.Time
}

type dockerHealthMsg struct {
	checked int
	stopped []string
	err     error
}

func newSchedulerPoller(store dockerHealthStore, cfg config.Config) *schedulerPoller {
	now := time.Now()
	return &schedulerPoller{store: store, cfg: cfg, exec: executor.New(), lastRecoveryAt: now, lastPersistedRefreshAt: now, lastDockerHealthAt: now}
}

func (p *schedulerPoller) reset(now time.Time) {
	if p == nil {
		return
	}
	p.lastRecoveryAt = now
	p.lastPersistedRefreshAt = now
	p.lastDockerHealthAt = now
}

func (p *schedulerPoller) HandleTick(m app, now time.Time) []tea.Cmd {
	if p == nil {
		return []tea.Cmd{m.reloadSchedulerJobs(), m.tick()}
	}
	var cmds []tea.Cmd
	if now.Sub(p.lastRecoveryAt) >= staleRunRecoveryInterval {
		p.lastRecoveryAt = now
		cmds = append(cmds, m.recoverStaleRunsCmd(), m.recoverOrphanInspectionCmd())
	}
	if now.Sub(p.lastPersistedRefreshAt) >= persistedStateRefreshInterval {
		p.lastPersistedRefreshAt = now
		cmds = append(cmds, m.reloadOverview(), m.taskBoard.Reload())
	}
	if now.Sub(p.lastDockerHealthAt) >= dockerHealthRefreshInterval {
		p.lastDockerHealthAt = now
		cmds = append(cmds, p.RefreshDockerHealthCmd())
	}
	cmds = append(cmds, m.reloadSchedulerJobs(), m.tick())
	return cmds
}

func (p *schedulerPoller) RefreshDockerHealthCmd() tea.Cmd {
	if p == nil || p.store == nil {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		checked, stopped, err := p.refreshDockerHealth(ctx)
		return dockerHealthMsg{checked: checked, stopped: stopped, err: err}
	}
}

func (p *schedulerPoller) refreshDockerHealth(ctx context.Context) (int, []string, error) {
	tasks, err := p.store.ListTasksWithDockerRunning(ctx)
	if err != nil {
		return 0, nil, err
	}
	var stopped []string
	var errs []error
	for _, task := range tasks {
		running, err := dockermgr.IsRunning(ctx, p.exec, task.ComposeMeta.ComposeFiles, task.ComposeMeta.Project, task.ComposeMeta.WorkDir)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", task.ID, err))
			continue
		}
		if running {
			continue
		}
		if err := p.store.MarkTaskDockerStopped(ctx, task.ID); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", task.ID, err))
			continue
		}
		stopped = append(stopped, task.ID)
	}
	return len(tasks), stopped, errors.Join(errs...)
}
