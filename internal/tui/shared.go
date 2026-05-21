package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/db"
	"github.com/xuanli520/p2r_tui/internal/executor"
	"github.com/xuanli520/p2r_tui/internal/pipeline"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/taskdocs"
)

type TaskProject struct {
	ID               string
	BatchID          string
	GitURL           string
	TaskState        string
	CompletionCount  int
	FrontendURL      string
	DockerRunning    bool
	ComposeMeta      model.ComposeMeta
	LastCompletedAt  string
	EnteredWaitingAt string
	SyncError        string

	LastRunID     string
	LastRun       string
	RunStatus     string
	CurrentStage  string
	CurrentStatus string
	ManualVerdict string
	FailedStage   string
	FailedSummary string
	Blocking      int
	High          int
	DocsCount     int
	Mode          string
	Path          string

	SyncPhase   string
	SyncPercent int
}

type TaskQueryService interface {
	ListByState(context.Context, string) ([]TaskProject, error)
	ListAll(context.Context, db.ProjectQuery) ([]TaskProject, int, error)
	GetByID(context.Context, string) (*TaskProject, error)
	FindWithDockerRunning(context.Context) ([]TaskProject, error)
	FindStaleInspecting(context.Context) ([]TaskProject, error)
}

type TaskActionService interface {
	StartInspection(context.Context, string) error
	ReInspect(context.Context, string) error
	ConfirmComplete(context.Context, string) error
	RetryGitSync(context.Context, string) error
}

type inspectionScheduler interface {
	SubmitInspection(string, string, string, pipeline.RunOptions) (string, error)
}

type dbTaskQueryService struct {
	store *db.Store
	cfg   config.Config
}

func newTaskQueryService(store *db.Store, cfg config.Config) TaskQueryService {
	if store == nil {
		return nil
	}
	return dbTaskQueryService{store: store, cfg: cfg}
}

func (s dbTaskQueryService) ListByState(ctx context.Context, state string) ([]TaskProject, error) {
	tasks, err := s.store.ListTasksByState(ctx, state)
	if err != nil {
		return nil, err
	}
	result := make([]TaskProject, 0, len(tasks))
	for _, task := range tasks {
		project := taskProjectFromTask(s.cfg, task)
		s.enrichTaskProject(ctx, &project)
		result = append(result, project)
	}
	return result, nil
}

func (s dbTaskQueryService) ListAll(ctx context.Context, query db.ProjectQuery) ([]TaskProject, int, error) {
	projects, total, err := s.store.ListProjectsPaginated(ctx, query)
	if err != nil {
		return nil, 0, err
	}
	result := make([]TaskProject, 0, len(projects))
	for _, project := range projects {
		result = append(result, taskProjectFromSummary(s.cfg, project))
	}
	return result, total, nil
}

func (s dbTaskQueryService) GetByID(ctx context.Context, taskID string) (*TaskProject, error) {
	task, err := s.store.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	project := taskProjectFromTask(s.cfg, task)
	return &project, nil
}

func (s dbTaskQueryService) FindWithDockerRunning(ctx context.Context) ([]TaskProject, error) {
	tasks, err := s.store.ListTasksWithDockerRunning(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]TaskProject, 0, len(tasks))
	for _, task := range tasks {
		result = append(result, taskProjectFromTask(s.cfg, task))
	}
	return result, nil
}

func (s dbTaskQueryService) FindStaleInspecting(ctx context.Context) ([]TaskProject, error) {
	tasks, err := s.store.ListTasksByState(ctx, model.TaskInspecting)
	if err != nil {
		return nil, err
	}
	result := make([]TaskProject, 0, len(tasks))
	for _, task := range tasks {
		if strings.TrimSpace(task.CurrentRunID) != "" || strings.TrimSpace(task.SyncError) != "" {
			continue
		}
		result = append(result, taskProjectFromTask(s.cfg, task))
	}
	return result, nil
}

func (s dbTaskQueryService) enrichTaskProject(ctx context.Context, project *TaskProject) {
	if project == nil || strings.TrimSpace(project.ID) == "" {
		return
	}
	run, err := s.store.LatestRunForTask(ctx, project.ID)
	if err != nil {
		return
	}
	project.LastRunID = run.RunID
	project.LastRun = run.FinishedAt
	if project.LastRun == "" {
		project.LastRun = run.StartedAt
	}
	project.RunStatus = run.Status
	project.Mode = runMode(run)
	stages, err := s.store.Stages(ctx, run.RunID)
	if err != nil {
		return
	}
	for _, stage := range stages {
		switch stage.Status {
		case model.StageRunning:
			if project.CurrentStage == "" {
				project.CurrentStage = stage.Stage
				project.CurrentStatus = stage.Status
			}
		case model.StageFailed, model.StageBlocked:
			if project.FailedStage == "" {
				project.FailedStage = stage.Stage
				project.FailedSummary = stage.ErrorSummary
				if project.CurrentStage == "" {
					project.CurrentStage = stage.Stage
					project.CurrentStatus = stage.Status
				}
			}
		}
	}
}

type dbTaskActionService struct {
	store     *db.Store
	cfg       config.Config
	scheduler inspectionScheduler
	exec      executor.CommandRunner
}

func newTaskActionService(store *db.Store, cfg config.Config, scheduler schedulerClient) TaskActionService {
	if store == nil {
		return nil
	}
	action := dbTaskActionService{store: store, cfg: cfg, exec: executor.New()}
	if submitter, ok := scheduler.(inspectionScheduler); ok {
		action.scheduler = submitter
	}
	return action
}

func (s dbTaskActionService) StartInspection(ctx context.Context, taskID string) error {
	taskID, err := ValidateTaskID(taskID)
	if err != nil {
		return err
	}
	gitURL, err := taskGitURL(s.cfg.Git.BaseURL, taskID)
	if err != nil {
		return err
	}
	task, err := s.store.CreateTaskWithBatch(ctx, taskID, gitURL, s.cfg.ScanPath)
	if err != nil {
		return err
	}
	return s.submitInspection(ctx, task)
}

func (s dbTaskActionService) ReInspect(ctx context.Context, taskID string) error {
	taskID, err := ValidateTaskID(taskID)
	if err != nil {
		return err
	}
	task, err := s.store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task.State != model.TaskCompleted {
		return fmt.Errorf("task %s is not completed", taskID)
	}
	if strings.TrimSpace(task.CurrentRunID) != "" {
		return fmt.Errorf("task %s already has an active run", taskID)
	}
	return s.submitInspection(ctx, task)
}

func (s dbTaskActionService) RetryGitSync(ctx context.Context, taskID string) error {
	taskID, err := ValidateTaskID(taskID)
	if err != nil {
		return err
	}
	task, err := s.store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task.State != model.TaskInspecting {
		if task.State != model.TaskCompleted {
			return fmt.Errorf("task %s is not inspecting or completed", taskID)
		}
		if strings.TrimSpace(task.CurrentRunID) != "" {
			return fmt.Errorf("task %s already has an active run", taskID)
		}
	}
	if strings.TrimSpace(task.SyncError) == "" {
		return fmt.Errorf("task %s has no git sync error to retry", taskID)
	}
	return s.submitInspection(ctx, task)
}

func (s dbTaskActionService) ConfirmComplete(ctx context.Context, taskID string) error {
	taskID, err := ValidateTaskID(taskID)
	if err != nil {
		return err
	}
	task, err := s.store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task.DockerRunning {
		if err := writeCleanupCheckpoint(s.cfg.ScanPath, task); err != nil {
			return err
		}
		defer removeCleanupCheckpoint(s.cfg.ScanPath)
		if dockerDaemonAvailable(ctx, s.exec) {
			summary := pipeline.StopDockerRuntime(ctx, s.exec, s.cfg.Docker, task.ComposeMeta)
			if summary.Status == "failed" {
				return fmt.Errorf("docker cleanup failed for %s: %s", taskID, cleanupErrorText(summary))
			}
		}
	}
	_, err = s.store.CompleteTask(ctx, taskID)
	return err
}

func dockerDaemonAvailable(ctx context.Context, exec executor.CommandRunner) bool {
	if exec == nil {
		exec = executor.New()
	}
	infoCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	result := exec.Run(infoCtx, 3*time.Second, "", nil, "docker", "info")
	return result.Err == nil
}

func writeCleanupCheckpoint(scanPath string, task model.Task) error {
	path := cleanupCheckpointPath(scanPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	payload := map[string]any{
		"task_id":      task.ID,
		"compose_meta": task.ComposeMeta,
		"created_at":   time.Now().UTC().Format(time.RFC3339),
	}
	content, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, content, 0o644)
}

func removeCleanupCheckpoint(scanPath string) {
	_ = os.Remove(cleanupCheckpointPath(scanPath))
}

func cleanupCheckpointPath(scanPath string) string {
	return filepath.Join(scanPath, ".qa-control", "cleanup_checkpoint.json")
}

func (s dbTaskActionService) submitInspection(ctx context.Context, task model.Task) error {
	var err error
	if s.scheduler == nil {
		err = fmt.Errorf("scheduler unavailable")
	} else {
		opts := pipeline.RunOptions{DeferRuntimeCleanup: true}
		_, err = s.scheduler.SubmitInspection(task.ID, task.BatchID, task.GitURL, opts)
	}
	if err != nil {
		if recordErr := s.store.RecordTaskGitError(ctx, task.ID, err); recordErr != nil {
			return fmt.Errorf("%w; additionally failed to record git sync error: %v", err, recordErr)
		}
	}
	return err
}

func taskProjectFromTask(cfg config.Config, task model.Task) TaskProject {
	return TaskProject{
		ID:               task.ID,
		BatchID:          task.BatchID,
		GitURL:           task.GitURL,
		TaskState:        task.State,
		CompletionCount:  task.CompletionCount,
		FrontendURL:      task.FrontendURL,
		DockerRunning:    task.DockerRunning,
		ComposeMeta:      task.ComposeMeta,
		LastCompletedAt:  task.LastCompletedAt,
		EnteredWaitingAt: task.EnteredWaitingAt,
		SyncError:        task.SyncError,
		Path:             task.RepoPath,
		DocsCount:        taskdocs.Count(cfg.ScanPath, task.ID),
		Mode:             "initial",
	}
}

func taskProjectFromSummary(cfg config.Config, project db.ProjectSummary) TaskProject {
	return TaskProject{
		ID:               project.TaskID,
		BatchID:          project.Batch,
		TaskState:        project.TaskState,
		CompletionCount:  project.CompletionCount,
		FrontendURL:      project.FrontendURL,
		DockerRunning:    project.DockerRunning,
		LastCompletedAt:  project.LastCompletedAt,
		EnteredWaitingAt: project.EnteredWaitingAt,
		SyncError:        project.SyncError,
		LastRunID:        project.LastRunID,
		LastRun:          project.LastRunAt,
		RunStatus:        project.RunStatus,
		ManualVerdict:    project.ManualVerdict,
		FailedStage:      project.FailedStage,
		Blocking:         project.Blocking,
		High:             project.High,
		DocsCount:        taskdocs.Count(cfg.ScanPath, project.TaskID),
		Path:             project.Path,
		Mode:             "initial",
	}
}

func taskGitURL(base string, taskID string) (string, error) {
	base = strings.TrimSpace(base)
	if base == "" {
		return "", fmt.Errorf("git.base_url is empty")
	}
	joined, err := url.JoinPath(base, taskID)
	if err != nil {
		return "", err
	}
	return joined, nil
}
