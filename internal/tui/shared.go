package tui

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/db"
	dockermgr "github.com/xuanli520/p2r_tui/internal/docker"
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
	StartInspection(context.Context, string, pipeline.RunOptions) error
	SubmitInspection(context.Context, string, pipeline.RunOptions) error
	ReInspect(context.Context, string, pipeline.RunOptions) error
	StartDocker(context.Context, string) error
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

func (s dbTaskActionService) StartInspection(ctx context.Context, taskID string, opts pipeline.RunOptions) error {
	taskID, err := ValidateTaskID(taskID)
	if err != nil {
		return err
	}
	if err := s.ensureInspectingCapacity(ctx); err != nil {
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
	return s.submitInspection(ctx, task, opts)
}

func (s dbTaskActionService) SubmitInspection(ctx context.Context, taskID string, opts pipeline.RunOptions) error {
	taskID, err := ValidateTaskID(taskID)
	if err != nil {
		return err
	}
	task, err := s.store.GetTask(ctx, taskID)
	if err == nil {
		if !canOpenInspectionRunConfig(task.State) {
			return fmt.Errorf("task %s is not ready for reinspection", taskID)
		}
		if strings.TrimSpace(task.CurrentRunID) != "" {
			return fmt.Errorf("task %s already has an active run", taskID)
		}
		if err := s.ensureInspectingCapacity(ctx); err != nil {
			return err
		}
		return s.submitInspection(ctx, task, opts)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return s.StartInspection(ctx, taskID, opts)
}

func (s dbTaskActionService) ReInspect(ctx context.Context, taskID string, opts pipeline.RunOptions) error {
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
	if err := s.ensureInspectingCapacity(ctx); err != nil {
		return err
	}
	return s.submitInspection(ctx, task, opts)
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
		if err := s.ensureInspectingCapacity(ctx); err != nil {
			return err
		}
	}
	if strings.TrimSpace(task.SyncError) == "" {
		return fmt.Errorf("task %s has no git sync error to retry", taskID)
	}
	return s.submitInspection(ctx, task, pipeline.RunOptions{})
}

func (s dbTaskActionService) ensureInspectingCapacity(ctx context.Context) error {
	count, err := s.store.CountTasksByState(ctx, model.TaskInspecting)
	if err != nil {
		return err
	}
	if count >= db.ActiveTaskStateLimit {
		return db.ErrInspectingTaskLimit
	}
	return nil
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

func (s dbTaskActionService) StartDocker(ctx context.Context, taskID string) error {
	taskID, err := ValidateTaskID(taskID)
	if err != nil {
		return err
	}
	task, err := s.store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task.State != model.TaskWaitingManual {
		return fmt.Errorf("task %s is not waiting manual", taskID)
	}
	if task.DockerRunning {
		return nil
	}
	if strings.TrimSpace(task.ComposeMeta.Project) == "" || len(task.ComposeMeta.ComposeFiles) == 0 {
		return fmt.Errorf("task %s has no Docker runtime metadata", taskID)
	}
	if s.exec == nil {
		s.exec = executor.New()
	}
	args := dockermgr.ComposeCommandArgsWithProjectDir(task.ComposeMeta.ComposeFiles, task.ComposeMeta.WorkDir, task.ComposeMeta.Project, "up", "-d")
	result := s.exec.Run(ctx, 5*time.Minute, task.ComposeMeta.WorkDir, nil, "docker", args...)
	if result.Err != nil {
		return fmt.Errorf("docker start failed for %s: %s", taskID, taskActionResultText(result))
	}
	meta := task.ComposeMeta
	if err := s.store.RecordTaskRuntime(ctx, taskID, task.FrontendURL, true, meta); err != nil {
		return err
	}
	ports, frontendURL, err := inspectTaskRuntimePorts(ctx, s.exec, meta)
	if err != nil {
		return err
	}
	meta.Ports = ports
	return s.store.RecordTaskRuntime(ctx, taskID, frontendURL, true, meta)
}

func inspectTaskRuntimePorts(ctx context.Context, exec executor.CommandRunner, meta model.ComposeMeta) ([]model.ServicePort, string, error) {
	args := dockermgr.ComposeCommandArgsWithProjectDir(meta.ComposeFiles, meta.WorkDir, meta.Project, "ps", "--format", "json")
	result := exec.Run(ctx, 30*time.Second, meta.WorkDir, nil, "docker", args...)
	if result.Err != nil {
		return nil, "", fmt.Errorf("docker port inspection failed: %s", taskActionResultText(result))
	}
	mappings, services := dockermgr.ParseComposePS(result.Stdout)
	ports := servicePortsFromMappings(mappings, services)
	frontendURL := ""
	if len(ports) > 0 {
		frontendURL = ports[0].URL
	}
	return ports, frontendURL, nil
}

func servicePortsFromMappings(mappings map[string][]dockermgr.PortMapping, services []string) []model.ServicePort {
	names := append([]string(nil), services...)
	seenServices := map[string]bool{}
	for _, name := range names {
		seenServices[name] = true
	}
	var extra []string
	for name := range mappings {
		if !seenServices[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	names = append(names, extra...)
	seenPorts := map[string]bool{}
	var ports []model.ServicePort
	for _, service := range names {
		for _, mapping := range mappings[service] {
			port := servicePortFromMapping(mapping)
			key := port.Service + "|" + port.URL
			if port.URL == "" || seenPorts[key] {
				continue
			}
			seenPorts[key] = true
			ports = append(ports, port)
		}
	}
	return ports
}

func servicePortFromMapping(mapping dockermgr.PortMapping) model.ServicePort {
	return model.ServicePort{
		Service:   mapping.Service,
		URL:       servicePortURL(mapping.URL, mapping.Host, mapping.Container),
		Host:      mapping.Host,
		Container: mapping.Container,
		Protocol:  mapping.Protocol,
	}
}

func servicePortURL(rawHost string, hostPort, containerPort int) string {
	if hostPort == 0 {
		return ""
	}
	scheme := "http"
	if containerPort == 443 || hostPort == 443 {
		scheme = "https"
	}
	host := normalizeTaskPortHost(rawHost)
	return fmt.Sprintf("%s://%s:%d", scheme, host, hostPort)
}

func normalizeTaskPortHost(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "http://")
	raw = strings.TrimPrefix(raw, "https://")
	raw = strings.Trim(raw, "[]")
	if raw == "" || raw == "0.0.0.0" || raw == "::" || raw == "127.0.0.1" {
		return "localhost"
	}
	if strings.Contains(raw, ":") {
		host, _, ok := strings.Cut(raw, ":")
		if ok {
			return normalizeTaskPortHost(host)
		}
	}
	return raw
}

func taskActionResultText(result executor.Result) string {
	for _, value := range []string{result.Stderr, result.Stdout} {
		if text := strings.TrimSpace(value); text != "" {
			return text
		}
	}
	if result.Err != nil {
		return result.Err.Error()
	}
	return result.Command
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

func (s dbTaskActionService) submitInspection(ctx context.Context, task model.Task, opts pipeline.RunOptions) error {
	var err error
	opts.DeferRuntimeCleanup = true
	if s.scheduler == nil {
		err = fmt.Errorf("scheduler unavailable")
	} else {
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
