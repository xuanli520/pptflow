package tui

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/db"
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

func isTaskNotFound(err error) bool {
	return errors.Is(err, sql.ErrNoRows)
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
		runs, err := s.store.ListRunsForTask(ctx, task.ID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if len(runs) > 0 {
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
	project.ManualVerdict = run.ManualVerdict
	project.Mode = runMode(run)
	stages, err := s.store.Stages(ctx, run.RunID)
	if err != nil {
		return
	}
	for _, stage := range stages {
		if stage.Status == model.StageRunning {
			if project.CurrentStage == "" {
				project.CurrentStage = stage.Stage
				project.CurrentStatus = stage.Status
			}
		}
	}
	stage, summary, status := primaryFailedStage(stages)
	if stage != "" {
		project.FailedStage = stage
		project.FailedSummary = summary
		if project.CurrentStage == "" {
			project.CurrentStage = stage
			project.CurrentStatus = status
		}
	}
}

func primaryFailedStage(stages []model.StageRecord) (string, string, string) {
	bestIndex := -1
	bestPriority := 0
	for index, stage := range stages {
		priority := failedStagePriority(stage)
		if priority == 0 {
			continue
		}
		if bestIndex < 0 || priority < bestPriority {
			bestIndex = index
			bestPriority = priority
		}
	}
	if bestIndex < 0 {
		return "", "", ""
	}
	stage := stages[bestIndex]
	return stage.Stage, stage.ErrorSummary, stage.Status
}

func failedStagePriority(stage model.StageRecord) int {
	switch stage.Status {
	case model.StageFailed:
		switch stage.Stage {
		case string(model.StageB):
			return 1
		case string(model.StageG):
			return 2
		case string(model.StageC):
			return 3
		default:
			return 10 + stageOrderRank(stage.Stage)
		}
	case model.StageBlocked:
		switch stage.Stage {
		case string(model.StageB):
			return 30
		case string(model.StageG):
			return 31
		case string(model.StageC):
			return 32
		default:
			return 40 + stageOrderRank(stage.Stage)
		}
	default:
		return 0
	}
}

func stageOrderRank(stage string) int {
	for index, item := range model.AllStages() {
		if item == stage {
			return index
		}
	}
	return 99
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
		DocsCount:        taskdocs.AvailableCount(cfg.ScanPath, task.ID),
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
		DocsCount:        taskdocs.AvailableCount(cfg.ScanPath, project.TaskID),
		Path:             project.Path,
		Mode:             "initial",
	}
}
