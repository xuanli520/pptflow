package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/xuanli520/p2r_tui/internal/config"
	dockermgr "github.com/xuanli520/p2r_tui/internal/docker"
	"github.com/xuanli520/p2r_tui/internal/executor"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
)

type taskRunLock struct {
	path string
	file *os.File
}

type taskRunLockStatus struct {
	Path    string
	Exists  bool
	PID     int
	TaskID  string
	Stale   bool
	ReadErr error
}

type TaskRunLockStatus struct {
	Path    string
	Exists  bool
	PID     int
	TaskID  string
	Stale   bool
	ReadErr error
}

type cleanupOutcome struct {
	Summary            dockermgr.CleanupSummary
	Finding            *model.Finding
	PersistErrors      []error
	RuntimeCleanupDone bool
}

type ExitCleanupSummary struct {
	Runtime []dockermgr.CleanupSummary `json:"runtime,omitempty"`
	GC      dockermgr.GCSummary        `json:"gc"`
}

func StopDockerRuntime(ctx context.Context, exec executor.CommandRunner, cfg config.DockerConfig, meta model.ComposeMeta) dockermgr.CleanupSummary {
	if exec == nil {
		exec = executor.New()
	}
	return dockermgr.CleanupComposeProjectFilesWithEnvFiles(ctx, exec, cfg, meta.ComposeFiles, meta.EnvFiles, meta.Project, meta.WorkDir)
}

func ForceExitCleanup(ctx context.Context, exec executor.CommandRunner, cfg config.Config, metas []model.ComposeMeta) (ExitCleanupSummary, error) {
	if exec == nil {
		exec = executor.New()
	}
	var summary ExitCleanupSummary
	var errs []error
	for _, meta := range metas {
		cleanup := StopDockerRuntime(ctx, exec, cfg.Docker, meta)
		summary.Runtime = append(summary.Runtime, cleanup)
		if cleanup.Status == "failed" {
			errs = append(errs, fmt.Errorf("cleanup %s: %s", meta.Project, cleanupErrorText(cleanup)))
		}
	}
	gc, err := runExitLabelCleanup(ctx, exec, cfg, "force_exit")
	summary.GC = gc
	if err != nil {
		errs = append(errs, err)
	}
	return summary, errors.Join(errs...)
}

func LightExitCleanup(ctx context.Context, exec executor.CommandRunner, cfg config.Config) (ExitCleanupSummary, error) {
	if exec == nil {
		exec = executor.New()
	}
	gc, err := runExitLabelCleanup(ctx, exec, cfg, "light_exit")
	return ExitCleanupSummary{GC: gc}, err
}

func runExitLabelCleanup(ctx context.Context, exec executor.CommandRunner, cfg config.Config, trigger string) (dockermgr.GCSummary, error) {
	dockerCfg := cfg.Docker
	dockerCfg.GC.Enabled = true
	dockerCfg.GC.P2ROnly = true
	dockerCfg.GC.PruneExitedContainers = true
	dockerCfg.GC.PruneNetworks = true
	dockerCfg.GC.PruneVolumes = false
	dockerCfg.GC.PruneImages = false
	dockerCfg.GC.PruneBuilderCache = false
	return dockermgr.RunGC(ctx, dockermgr.GCRunRequest{
		ScanPath: cfg.ScanPath,
		Config:   dockerCfg,
		Exec:     exec,
		Yes:      true,
		Trigger:  trigger,
	})
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

func (r Runner) acquireTaskRunLock(taskID string) (taskRunLock, error) {
	lockDir := filepath.Join(r.cfg.ScanPath, ".qa-control", "locks")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		return taskRunLock{}, err
	}
	path := taskRunLockPath(r.cfg.ScanPath, taskID)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil && os.IsExist(err) && staleTaskRunLock(path) {
		_ = os.Remove(path)
		file, err = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	}
	if err != nil {
		return taskRunLock{}, fmt.Errorf("task %s is already locked for a p2r run or lock cannot be created: %w", taskID, err)
	}
	_, _ = fmt.Fprintf(file, "task_id=%s\npid=%d\ncreated_at=%s\n", taskID, os.Getpid(), time.Now().UTC().Format(time.RFC3339))
	return taskRunLock{path: path, file: file}, nil
}

func taskRunLockPath(scanPath, taskID string) string {
	return filepath.Join(scanPath, ".qa-control", "locks", safeLockName(taskID)+".lock")
}

func TaskRunLockPath(scanPath, taskID string) string {
	return taskRunLockPath(scanPath, taskID)
}

func (l taskRunLock) Release() {
	if l.file != nil {
		_ = l.file.Close()
	}
	if l.path != "" {
		_ = os.Remove(l.path)
	}
}

func (r Runner) cleanupStaleRuns(ctx context.Context, runs []model.RunRecord, currentRunID, artifactRoot string) []dockermgr.CleanupSummary {
	var summaries []dockermgr.CleanupSummary
	for _, run := range runs {
		if run.RunID == currentRunID || run.ArtifactRoot == "" {
			continue
		}
		evidence, err := readRuntimeEvidence(filepath.Join(run.ArtifactRoot, "port_map.json"))
		if err != nil || evidence.ComposeProject == "" {
			continue
		}
		summary := dockermgr.CleanupComposeProjectFilesWithEnvFiles(ctx, r.exec, r.cfg.Docker, evidence.ComposeFiles, evidence.EnvFiles, evidence.ComposeProject, evidence.WorkDir)
		summary.Status = "stale_" + summary.Status
		summaries = append(summaries, summary)
	}
	if len(summaries) > 0 {
		_ = writeJSON(filepath.Join(artifactRoot, "pre_run_cleanup_summary.json"), map[string]any{"stale_runs": summaries})
	}
	return summaries
}

func (r Runner) cleanupCurrentRuntime(ctx context.Context, run model.RunRecord, runtime RuntimeState, keepRuntime bool) dockermgr.CleanupSummary {
	var err error
	evidence := runtime
	if !evidence.HasCleanupTarget() {
		evidence, err = readRuntimeState(filepath.Join(run.ArtifactRoot, "port_map.json"))
	}
	if err != nil || !evidence.HasCleanupTarget() {
		summary := dockermgr.CleanupSummary{Status: "not_applicable"}
		if err != nil {
			summary.Warnings = append(summary.Warnings, "runtime evidence unavailable: "+err.Error())
		}
		return writeCleanupSummary(run.ArtifactRoot, summary)
	}
	if keepRuntime {
		args := dockermgr.CleanupComposeArgsFilesWithProjectDirAndEnvFiles(r.cfg.Docker, evidence.ComposeFiles, evidence.EnvFiles, evidence.ComposeProject, evidence.WorkDir)
		summary := dockermgr.CleanupSummary{
			Status:         "kept_by_operator_request",
			ComposeFile:    evidence.ComposeFile,
			ComposeFiles:   evidence.ComposeFiles,
			EnvFiles:       evidence.EnvFiles,
			ComposeProject: evidence.ComposeProject,
			WorkDir:        evidence.WorkDir,
			ManualCommand:  dockermgr.CommandLine("docker", args),
		}
		return writeCleanupSummary(run.ArtifactRoot, summary)
	}
	summary := dockermgr.CleanupComposeProjectFilesWithEnvFiles(ctx, r.exec, r.cfg.Docker, evidence.ComposeFiles, evidence.EnvFiles, evidence.ComposeProject, evidence.WorkDir)
	return writeCleanupSummary(run.ArtifactRoot, summary)
}

func (r Runner) finalizeRuntime(ctx context.Context, run model.RunRecord, stages []model.StageRecord, runtime RuntimeState, keepRuntime bool, reason string) cleanupOutcome {
	outcome := cleanupOutcome{
		Summary:            dockermgr.CleanupSummary{Status: "not_applicable"},
		RuntimeCleanupDone: true,
	}
	if !runtimeStageWasSelected(stages) {
		return outcome
	}
	outcome.Summary = r.cleanupCurrentRuntime(ctx, run, runtime, keepRuntime)
	if err := mergeCleanupIntoManifest(filepath.Join(run.ArtifactRoot, "run_manifest.json"), "cleanup", outcome.Summary); err != nil {
		outcome.PersistErrors = append(outcome.PersistErrors, fmt.Errorf("merge cleanup into run_manifest.json: %w", err))
	}
	if outcome.Summary.Status == "failed" {
		finding := cleanupFinding(outcome.Summary)
		outcome.Finding = &finding
		if r.store != nil {
			if err := r.store.InsertFindings(ctx, run.RunID, []model.Finding{finding}); err != nil {
				outcome.PersistErrors = append(outcome.PersistErrors, fmt.Errorf("insert cleanup finding: %w", err))
			}
		}
	}
	outcome.RuntimeCleanupDone = cleanupDoneForReason(ctx, outcome.Summary, reason)
	return outcome
}

func cleanupDoneForReason(ctx context.Context, summary dockermgr.CleanupSummary, reason string) bool {
	if summary.Status != "failed" {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(reason), "normal") && ctx.Err() == nil {
		return true
	}
	return false
}

func cleanupPersistErrorStrings(outcome cleanupOutcome) []string {
	result := make([]string, 0, len(outcome.PersistErrors))
	for _, err := range outcome.PersistErrors {
		if err != nil {
			result = append(result, err.Error())
		}
	}
	return result
}

func cleanupPersistError(outcome cleanupOutcome) error {
	if len(outcome.PersistErrors) == 0 {
		return nil
	}
	return errors.Join(outcome.PersistErrors...)
}

func writeCleanupSummary(artifactRoot string, summary dockermgr.CleanupSummary) dockermgr.CleanupSummary {
	if err := NewArtifactWriter(artifactRoot).RequiredJSON("cleanup_summary.json", summary); err != nil {
		if summary.Status != "failed" {
			summary.Status = "failed"
		}
		if strings.TrimSpace(summary.Error) == "" {
			summary.Error = err.Error()
		}
		summary.Warnings = append(summary.Warnings, "cleanup summary artifact write failed: "+err.Error())
	}
	return summary
}

func runtimeCleanupPoint(stage string, stages []model.StageRecord) bool {
	return false
}

func runtimeStageWasSelected(stages []model.StageRecord) bool {
	for _, item := range stages {
		if model.IsRuntimeStage(item.Stage) && item.Status != model.StageSkipped {
			return true
		}
	}
	return false
}

func safeLockName(taskID string) string {
	value := strings.ToLower(taskID)
	var builder strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' {
			builder.WriteRune(r)
		} else {
			builder.WriteByte('_')
		}
	}
	if builder.Len() == 0 {
		builder.WriteString("task")
	}
	sum := sha256Text(taskID)
	name := builder.String()
	if len(name) > 48 {
		name = name[:48]
	}
	return strings.Trim(name, "._-") + "-" + sum[:8]
}

func staleTaskRunLock(path string) bool {
	return readTaskRunLockStatus(path).Stale
}

func taskRunLockStatusForTask(scanPath, taskID string) taskRunLockStatus {
	return readTaskRunLockStatus(taskRunLockPath(scanPath, taskID))
}

func TaskRunLockStatusForTask(scanPath, taskID string) TaskRunLockStatus {
	return exportTaskRunLockStatus(taskRunLockStatusForTask(scanPath, taskID))
}

func RemoveTaskRunLock(scanPath, taskID string) error {
	err := os.Remove(taskRunLockPath(scanPath, taskID))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func exportTaskRunLockStatus(status taskRunLockStatus) TaskRunLockStatus {
	return TaskRunLockStatus{
		Path:    status.Path,
		Exists:  status.Exists,
		PID:     status.PID,
		TaskID:  status.TaskID,
		Stale:   status.Stale,
		ReadErr: status.ReadErr,
	}
}

func readTaskRunLockStatus(path string) taskRunLockStatus {
	status := taskRunLockStatus{Path: path}
	content, err := os.ReadFile(path)
	if err != nil {
		status.ReadErr = err
		return status
	}
	status.Exists = true
	values := map[string]string{}
	for _, line := range strings.Split(string(content), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	status.TaskID = values["task_id"]
	pid, err := strconv.Atoi(values["pid"])
	if err != nil || pid <= 0 {
		return status
	}
	status.PID = pid
	status.Stale = !processAlive(pid)
	return status
}

func mergeCleanupIntoManifest(path string, key string, value any) error {
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var data map[string]any
	if err := json.Unmarshal(content, &data); err != nil {
		return err
	}
	data[key] = value
	return writeJSON(path, data)
}

func cleanupFinding(summary dockermgr.CleanupSummary) model.Finding {
	evidence := strings.TrimSpace(summary.Error)
	if evidence == "" {
		evidence = strings.TrimSpace(summary.Stderr)
	}
	if evidence == "" {
		evidence = "cleanup command failed"
	}
	return model.Finding{
		ID:         "P2R-INFRA-HIGH-001",
		Stage:      "INFRA",
		Severity:   "High",
		Title:      "Docker cleanup failed",
		Rule:       "p2r-managed Docker resources must be cleaned after runtime stages.",
		Evidence:   evidence,
		Impact:     "Repeated runs may hit stale containers, networks, volumes, or port conflicts.",
		MinimumFix: "Run the cleanup command recorded in cleanup_summary.json and rerun the affected task.",
	}
}
