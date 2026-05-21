package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/xuanli520/p2r_tui/internal/config"
	dockermgr "github.com/xuanli520/p2r_tui/internal/docker"
	"github.com/xuanli520/p2r_tui/internal/executor"
	pipelinepkg "github.com/xuanli520/p2r_tui/internal/pipeline"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
)

func ForceExitCleanup(ctx context.Context, cfg config.Config, tasks []TaskProject) error {
	_, err := forceExitCleanup(ctx, cfg, executor.New(), tasks)
	return err
}

func ForceExitCleanupResult(ctx context.Context, cfg config.Config, tasks []TaskProject) ([]string, error) {
	return forceExitCleanup(ctx, cfg, executor.New(), tasks)
}

func forceExitCleanup(ctx context.Context, cfg config.Config, exec executor.CommandRunner, tasks []TaskProject) ([]string, error) {
	if exec == nil {
		exec = executor.New()
	}
	metas := make([]model.ComposeMeta, 0, len(tasks))
	taskIDs := make([]string, 0, len(tasks))
	for _, task := range tasks {
		if !task.DockerRunning {
			continue
		}
		meta := task.ComposeMeta
		metas = append(metas, meta)
		taskIDs = append(taskIDs, task.ID)
	}
	summary, err := pipelinepkg.ForceExitCleanup(ctx, exec, cfg, metas)
	var stopped []string
	for i, cleanup := range summary.Runtime {
		if cleanup.Status == "failed" {
			continue
		}
		if i < len(taskIDs) && taskIDs[i] != "" {
			stopped = append(stopped, taskIDs[i])
		}
	}
	if err == nil {
		return stopped, nil
	}
	var errs []error
	for i, cleanup := range summary.Runtime {
		if cleanup.Status != "failed" {
			continue
		}
		taskID := cleanup.ComposeProject
		if i < len(taskIDs) && taskIDs[i] != "" {
			taskID = taskIDs[i]
		}
		errs = append(errs, fmt.Errorf("cleanup %s: %s", taskID, cleanupErrorText(cleanup)))
	}
	if gcErr := strings.TrimSpace(summary.GC.Error); gcErr != "" {
		errs = append(errs, fmt.Errorf("docker label cleanup: %s", gcErr))
	}
	if len(errs) == 0 {
		errs = append(errs, err)
	}
	return stopped, errors.Join(errs...)
}

func LightExitCleanup(ctx context.Context, cfg config.Config) error {
	_, err := pipelinepkg.LightExitCleanup(ctx, executor.New(), cfg)
	return err
}

func cleanupErrorText(summary dockermgr.CleanupSummary) string {
	for _, value := range []string{summary.Error, summary.Stderr, summary.Stdout} {
		if text := strings.TrimSpace(value); text != "" {
			return text
		}
	}
	if len(summary.Warnings) > 0 {
		return strings.Join(summary.Warnings, "; ")
	}
	return summary.Status
}
