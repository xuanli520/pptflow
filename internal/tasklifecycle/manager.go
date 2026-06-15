package tasklifecycle

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/db"
	dockermgr "github.com/xuanli520/p2r_tui/internal/docker"
	"github.com/xuanli520/p2r_tui/internal/executor"
	"github.com/xuanli520/p2r_tui/internal/pipeline"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/scheduler"
	"github.com/xuanli520/p2r_tui/internal/taskdocs"
)

const maxTaskIDLength = 64

var taskIDPattern = regexp.MustCompile(`^TASK-\d{8}-[A-F0-9]{6}$`)

type Scheduler interface {
	Submit(context.Context, scheduler.SubmitRequest) (scheduler.SubmitResult, error)
}

type CommandKind string

const (
	CommandSubmitInspection CommandKind = "submit_inspection"
	CommandRetryGitSync     CommandKind = "retry_git_sync"
	CommandStartDocker      CommandKind = "start_docker"
	CommandCompleteManual   CommandKind = "complete_manual"
)

type Command struct {
	Kind        CommandKind
	TaskID      string
	ProjectType string
	Verdict     string
	RunOptions  pipeline.RunOptions
}

type Result struct {
	Kind   CommandKind
	TaskID string
	JobID  string
	FlowID string
}

type Option func(*Manager)

type Manager struct {
	store     *db.Store
	cfg       config.Config
	scheduler Scheduler
	exec      executor.CommandRunner

	mu        sync.Mutex
	taskLocks map[string]*sync.Mutex
}

func NewManager(store *db.Store, cfg config.Config, submitter Scheduler, opts ...Option) *Manager {
	m := &Manager{
		store:     store,
		cfg:       cfg,
		scheduler: submitter,
		exec:      executor.New(),
		taskLocks: map[string]*sync.Mutex{},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(m)
		}
	}
	return m
}

func WithExecutor(exec executor.CommandRunner) Option {
	return func(m *Manager) {
		if exec != nil {
			m.exec = exec
		}
	}
}

func NormalizeTaskID(raw string) (string, error) {
	cleaned := strings.TrimSpace(raw)
	if len(cleaned) > maxTaskIDLength {
		return "", fmt.Errorf("TASK ID exceeds max length")
	}
	if !taskIDPattern.MatchString(cleaned) {
		return "", fmt.Errorf("invalid TASK ID format, expected TASK-YYYYMMDD-XXXXXX")
	}
	return cleaned, nil
}

func (m *Manager) Execute(ctx context.Context, cmd Command) (Result, error) {
	if m == nil || m.store == nil {
		return Result{}, errors.New("task lifecycle manager unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	taskID, err := NormalizeTaskID(cmd.TaskID)
	if err != nil {
		return Result{}, err
	}
	cmd.TaskID = taskID
	return m.withTaskLock(taskID, func() (Result, error) {
		switch cmd.Kind {
		case CommandSubmitInspection:
			return m.submitInspectionCommand(ctx, cmd)
		case CommandRetryGitSync:
			return m.retryGitSync(ctx, cmd)
		case CommandStartDocker:
			return m.startDocker(ctx, taskID)
		case CommandCompleteManual:
			return m.completeManual(ctx, taskID, cmd.Verdict)
		default:
			return Result{}, fmt.Errorf("unknown lifecycle command %q", cmd.Kind)
		}
	})
}

func (m *Manager) withTaskLock(taskID string, fn func() (Result, error)) (Result, error) {
	m.mu.Lock()
	lock := m.taskLocks[taskID]
	if lock == nil {
		lock = &sync.Mutex{}
		m.taskLocks[taskID] = lock
	}
	m.mu.Unlock()

	lock.Lock()
	defer lock.Unlock()
	return fn()
}

func (m *Manager) submitInspectionCommand(ctx context.Context, cmd Command) (Result, error) {
	task, err := m.store.GetTask(ctx, cmd.TaskID)
	if err == nil {
		return m.submitExistingInspection(ctx, task, cmd.ProjectType, cmd.RunOptions)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Result{}, err
	}
	return m.startInspection(ctx, cmd.TaskID, cmd.ProjectType, cmd.RunOptions)
}

func (m *Manager) startInspection(ctx context.Context, taskID, projectType string, opts pipeline.RunOptions) (Result, error) {
	if err := m.ensureInspectingCapacity(ctx); err != nil {
		return Result{}, err
	}
	gitURL, err := taskGitURLForProjectType(m.cfg.Git, projectType, taskID)
	if err != nil {
		return Result{}, err
	}
	task, err := m.store.CreateTaskWithBatch(ctx, taskID, gitURL, m.cfg.ScanPath)
	if err != nil {
		return Result{}, err
	}
	return m.submitInspection(ctx, task, opts)
}

func (m *Manager) submitExistingInspection(ctx context.Context, task model.Task, projectType string, opts pipeline.RunOptions) (Result, error) {
	retryGitSync := canRetryGitSyncWithProjectType(task)
	if task.State == model.TaskWaitingManual {
		return Result{}, fmt.Errorf("task %s is waiting for manual verdict", task.ID)
	}
	if strings.TrimSpace(task.CurrentRunID) != "" {
		return Result{}, fmt.Errorf("task %s already has an active run", task.ID)
	}
	if task.State == model.TaskInspecting && !retryGitSync {
		return Result{}, fmt.Errorf("task %s is not ready for reinspection", task.ID)
	}
	if task.State == model.TaskCompleted || !retryGitSync {
		if err := m.ensureInspectingCapacity(ctx); err != nil {
			return Result{}, err
		}
	}
	task, err := m.applySelectedProjectType(ctx, task, projectType)
	if err != nil {
		return Result{}, err
	}
	return m.submitInspection(ctx, task, opts)
}

func (m *Manager) retryGitSync(ctx context.Context, cmd Command) (Result, error) {
	task, err := m.store.GetTask(ctx, cmd.TaskID)
	if err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(task.SyncError) == "" {
		return Result{}, fmt.Errorf("task %s has no git sync error to retry", task.ID)
	}
	if strings.TrimSpace(task.CurrentRunID) != "" {
		return Result{}, fmt.Errorf("task %s already has an active run", task.ID)
	}
	switch task.State {
	case model.TaskInspecting:
	case model.TaskCompleted:
		if err := m.ensureInspectingCapacity(ctx); err != nil {
			return Result{}, err
		}
	default:
		return Result{}, fmt.Errorf("task %s is not inspecting or completed", task.ID)
	}
	return m.submitInspection(ctx, task, pipeline.RunOptions{})
}

func (m *Manager) submitInspection(ctx context.Context, task model.Task, opts pipeline.RunOptions) (Result, error) {
	opts.DeferRuntimeCleanup = true
	if inspectionModeIsInitial(opts.Mode) && taskdocs.AvailableCount(m.cfg.ScanPath, task.ID) < 1 {
		return Result{}, errors.New(taskdocs.InitialInspectionDocsRequiredMessage)
	}
	if m.scheduler == nil {
		return Result{}, fmt.Errorf("scheduler unavailable")
	}
	result, err := m.scheduler.Submit(ctx, scheduler.SubmitRequest{
		TaskID:  task.ID,
		Flow:    scheduler.FlowGitSyncThenPipeline,
		BatchID: task.BatchID,
		GitURL:  task.GitURL,
		Opts:    opts,
	})
	if err != nil {
		return Result{}, err
	}
	return Result{Kind: CommandSubmitInspection, TaskID: task.ID, JobID: result.JobID, FlowID: result.FlowID}, nil
}

func (m *Manager) ensureInspectingCapacity(ctx context.Context) error {
	count, err := m.store.CountTasksByState(ctx, model.TaskInspecting)
	if err != nil {
		return err
	}
	if count >= db.ActiveTaskStateLimit {
		return db.ErrInspectingTaskLimit
	}
	return nil
}

func (m *Manager) applySelectedProjectType(ctx context.Context, task model.Task, projectType string) (model.Task, error) {
	if strings.TrimSpace(projectType) == "" {
		return task, nil
	}
	gitURL, err := taskGitURLForProjectType(m.cfg.Git, projectType, task.ID)
	if err != nil {
		return model.Task{}, err
	}
	if strings.TrimSpace(task.GitURL) == gitURL {
		return task, nil
	}
	return m.store.UpdateTaskGitURL(ctx, task.ID, gitURL)
}

func (m *Manager) completeManual(ctx context.Context, taskID string, verdict string) (Result, error) {
	verdict = NormalizeManualVerdict(verdict)
	if verdict == "" || verdict == model.ManualUnset {
		return Result{}, fmt.Errorf("task %s requires manual verdict before completion", taskID)
	}
	task, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		return Result{}, err
	}
	if task.DockerRunning {
		if err := writeCleanupCheckpoint(m.cfg.ScanPath, task); err != nil {
			return Result{}, err
		}
		defer removeCleanupCheckpoint(m.cfg.ScanPath)
		if dockerDaemonAvailable(ctx, m.exec) {
			summary := pipeline.StopDockerRuntime(ctx, m.exec, m.cfg.Docker, task.ComposeMeta)
			if summary.Status == "failed" {
				return Result{}, fmt.Errorf("docker cleanup failed for %s: %s", taskID, cleanupErrorText(summary))
			}
		}
	}
	if _, err := m.store.CompleteTaskWithVerdict(ctx, taskID, verdict); err != nil {
		return Result{}, err
	}
	return Result{Kind: CommandCompleteManual, TaskID: taskID}, nil
}

func (m *Manager) startDocker(ctx context.Context, taskID string) (Result, error) {
	task, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		return Result{}, err
	}
	if task.State != model.TaskWaitingManual {
		return Result{}, fmt.Errorf("task %s is not waiting manual", taskID)
	}
	if task.DockerRunning {
		return Result{Kind: CommandStartDocker, TaskID: taskID}, nil
	}
	if strings.TrimSpace(task.ComposeMeta.Project) == "" || len(task.ComposeMeta.ComposeFiles) == 0 {
		return Result{}, fmt.Errorf("task %s has no Docker runtime metadata", taskID)
	}
	args := dockermgr.ComposeCommandArgsWithProjectDirAndEnvFiles(task.ComposeMeta.ComposeFiles, task.ComposeMeta.WorkDir, task.ComposeMeta.Project, task.ComposeMeta.EnvFiles, "up", "-d")
	result := m.exec.Run(ctx, 5*time.Minute, task.ComposeMeta.WorkDir, nil, "docker", args...)
	if result.Err != nil {
		return Result{}, fmt.Errorf("docker start failed for %s: %s", taskID, taskActionResultText(result))
	}
	meta := task.ComposeMeta
	if err := m.store.RecordWaitingManualTaskRuntime(ctx, taskID, task.FrontendURL, true, meta); err != nil {
		m.stopRuntimeBestEffort(ctx, meta)
		return Result{}, err
	}
	ports, frontendURL, err := inspectTaskRuntimePorts(ctx, m.exec, meta)
	if err != nil {
		m.stopRuntimeBestEffort(ctx, meta)
		return Result{}, err
	}
	meta.Ports = ports
	if err := m.store.RecordWaitingManualTaskRuntime(ctx, taskID, frontendURL, true, meta); err != nil {
		m.stopRuntimeBestEffort(ctx, meta)
		return Result{}, err
	}
	return Result{Kind: CommandStartDocker, TaskID: taskID}, nil
}

func (m *Manager) stopRuntimeBestEffort(ctx context.Context, meta model.ComposeMeta) {
	_ = pipeline.StopDockerRuntime(ctx, m.exec, m.cfg.Docker, meta)
}

func NormalizeManualVerdict(verdict string) string {
	switch strings.TrimSpace(verdict) {
	case model.ManualPass:
		return model.ManualPass
	case model.ManualRework:
		return model.ManualRework
	case model.ManualFail:
		return model.ManualFail
	default:
		return ""
	}
}

func canRetryGitSyncWithProjectType(task model.Task) bool {
	return task.State == model.TaskInspecting &&
		strings.TrimSpace(task.CurrentRunID) == "" &&
		strings.TrimSpace(task.SyncError) != ""
}

func inspectionModeIsInitial(mode string) bool {
	mode = strings.TrimSpace(strings.ToLower(mode))
	return mode == "" || mode == "initial"
}

func taskGitURLForProjectType(git config.GitConfig, projectType string, taskID string) (string, error) {
	base, err := config.GitBaseURLForProjectType(git, projectType)
	if err != nil {
		return "", err
	}
	return taskGitURL(base, taskID)
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

func inspectTaskRuntimePorts(ctx context.Context, exec executor.CommandRunner, meta model.ComposeMeta) ([]model.ServicePort, string, error) {
	args := dockermgr.ComposeCommandArgsWithProjectDirAndEnvFiles(meta.ComposeFiles, meta.WorkDir, meta.Project, meta.EnvFiles, "ps", "--format", "json")
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

func cleanupErrorText(summary dockermgr.CleanupSummary) string {
	if strings.TrimSpace(summary.Error) != "" {
		return summary.Error
	}
	return summary.Status
}
