package tasklifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	dockermgr "github.com/xuanli520/p2r_tui/internal/docker"
	"github.com/xuanli520/p2r_tui/internal/pipeline"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/scheduler"
)

type DiagnosticCode string

const (
	DiagnosticInspectingWithoutRun DiagnosticCode = "INSPECTING_WITHOUT_RUN"
	DiagnosticInspectingWithoutJob DiagnosticCode = "INSPECTING_WITHOUT_JOB"
	DiagnosticRunStuckRunning      DiagnosticCode = "RUN_STUCK_RUNNING"
	DiagnosticRunWithoutStages     DiagnosticCode = "RUN_WITHOUT_STAGES"
	DiagnosticInspectingDoubleRun  DiagnosticCode = "INSPECTING_DOUBLE_RUN"
	DiagnosticOrphanedLockFile     DiagnosticCode = "ORPHANED_LOCK_FILE"
	DiagnosticDockerLeakedDone     DiagnosticCode = "DOCKER_LEAKED_COMPLETED"
	DiagnosticDockerLeakedInspect  DiagnosticCode = "DOCKER_LEAKED_INSPECTING"
	DiagnosticStaleWaitingManual   DiagnosticCode = "STALE_WAITING_MANUAL"
	DiagnosticPreRunFailureOnly    DiagnosticCode = "PRE_RUN_FAILURE_ONLY"
	DiagnosticDockerStatusUnknown  DiagnosticCode = "DOCKER_STATUS_UNKNOWN"
)

type DiagnosticSeverity string

const (
	SeverityCritical DiagnosticSeverity = "critical"
	SeverityHigh     DiagnosticSeverity = "high"
	SeverityWarning  DiagnosticSeverity = "warning"
	SeverityInfo     DiagnosticSeverity = "info"
)

type DiagnosticFixPolicy string

const (
	FixNoFix            DiagnosticFixPolicy = "NoFix"
	FixStopLeakedDocker DiagnosticFixPolicy = "StopLeakedDocker"
	FixTerminalReset    DiagnosticFixPolicy = "TerminalReset"
)

type DiagnosticIssue struct {
	Code     DiagnosticCode      `json:"code"`
	Severity DiagnosticSeverity  `json:"severity"`
	Message  string              `json:"message"`
	Detail   string              `json:"detail,omitempty"`
	Policy   DiagnosticFixPolicy `json:"policy"`
}

type DockerSnapshot struct {
	Checked bool   `json:"checked"`
	Running bool   `json:"running"`
	Error   string `json:"error,omitempty"`
}

type RunFailureFile struct {
	Path      string `json:"path"`
	Phase     string `json:"phase,omitempty"`
	Error     string `json:"error,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
}

type TaskDiagnosticsSnapshot struct {
	Task         model.Task                     `json:"task"`
	Runs         []model.RunRecord              `json:"runs"`
	StagesForRun map[string][]model.StageRecord `json:"stages_for_run,omitempty"`
	Lock         pipeline.TaskRunLockStatus     `json:"lock"`
	Docker       DockerSnapshot                 `json:"docker"`
	ActiveJobs   []scheduler.JobSnapshot        `json:"active_jobs,omitempty"`
	Events       []model.TaskEvent              `json:"events,omitempty"`
	FailureFiles []RunFailureFile               `json:"failure_files,omitempty"`
	Issues       []DiagnosticIssue              `json:"issues,omitempty"`
	CollectedAt  time.Time                      `json:"collected_at"`
}

type DiagnosticsRepairResult struct {
	TaskID       string              `json:"task_id"`
	Policy       DiagnosticFixPolicy `json:"policy"`
	FixedIssues  []DiagnosticCode    `json:"fixed_issues,omitempty"`
	LogPath      string              `json:"log_path,omitempty"`
	CleanupError string              `json:"cleanup_error,omitempty"`
}

type activeJobSnapshotter interface {
	ActiveSnapshot() []scheduler.JobSnapshot
}

func (m *Manager) DiagnoseTask(ctx context.Context, taskID string, activeJobs []scheduler.JobSnapshot) (TaskDiagnosticsSnapshot, error) {
	if m == nil || m.store == nil {
		return TaskDiagnosticsSnapshot{}, errors.New("task lifecycle manager unavailable")
	}
	taskID, err := NormalizeTaskID(taskID)
	if err != nil {
		return TaskDiagnosticsSnapshot{}, err
	}
	var snapshot TaskDiagnosticsSnapshot
	_, err = m.withTaskLock(taskID, func() (Result, error) {
		var err error
		snapshot, err = m.collectDiagnostics(ctx, taskID, m.currentActiveJobs(activeJobs))
		return Result{}, err
	})
	return snapshot, err
}

func (m *Manager) RepairTaskDiagnostics(ctx context.Context, snapshot TaskDiagnosticsSnapshot) (DiagnosticsRepairResult, error) {
	if m == nil || m.store == nil {
		return DiagnosticsRepairResult{}, errors.New("task lifecycle manager unavailable")
	}
	taskID, err := NormalizeTaskID(snapshot.Task.ID)
	if err != nil {
		return DiagnosticsRepairResult{}, err
	}
	var result DiagnosticsRepairResult
	_, err = m.withTaskLock(taskID, func() (Result, error) {
		refreshed, err := m.collectDiagnostics(ctx, taskID, m.currentActiveJobs(snapshot.ActiveJobs))
		if err != nil {
			return Result{}, err
		}
		result, err = m.repairDiagnostics(ctx, refreshed)
		return Result{}, err
	})
	return result, err
}

func (m *Manager) collectDiagnostics(ctx context.Context, taskID string, activeJobs []scheduler.JobSnapshot) (TaskDiagnosticsSnapshot, error) {
	task, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		return TaskDiagnosticsSnapshot{}, err
	}
	runs, err := m.store.ListRunsForTask(ctx, taskID)
	if err != nil {
		return TaskDiagnosticsSnapshot{}, err
	}
	stages := make(map[string][]model.StageRecord, len(runs))
	for _, run := range runs {
		records, err := m.store.Stages(ctx, run.RunID)
		if err != nil {
			return TaskDiagnosticsSnapshot{}, fmt.Errorf("load stages for %s: %w", run.RunID, err)
		}
		stages[run.RunID] = records
	}
	events, err := m.store.TaskEvents(ctx, taskID, 50)
	if err != nil {
		return TaskDiagnosticsSnapshot{}, err
	}
	snapshot := TaskDiagnosticsSnapshot{
		Task:         task,
		Runs:         runs,
		StagesForRun: stages,
		Lock:         pipeline.TaskRunLockStatusForTask(m.cfg.ScanPath, taskID),
		ActiveJobs:   activeJobsForTask(activeJobs, taskID),
		Events:       events,
		FailureFiles: readRunFailureFiles(m.cfg.ScanPath, taskID),
		CollectedAt:  time.Now().UTC(),
	}
	snapshot.Docker = m.collectDockerSnapshot(ctx, task)
	snapshot.Issues = m.evaluateDiagnostics(snapshot)
	return snapshot, nil
}

func (m *Manager) collectDockerSnapshot(ctx context.Context, task model.Task) DockerSnapshot {
	if strings.TrimSpace(task.ComposeMeta.Project) == "" || len(task.ComposeMeta.ComposeFiles) == 0 {
		return DockerSnapshot{}
	}
	result := DockerSnapshot{Checked: true}
	running, err := dockermgr.IsRunningWithEnvFiles(ctx, m.exec, task.ComposeMeta.ComposeFiles, task.ComposeMeta.EnvFiles, task.ComposeMeta.Project, task.ComposeMeta.WorkDir)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.Running = running
	return result
}

func (m *Manager) evaluateDiagnostics(snapshot TaskDiagnosticsSnapshot) []DiagnosticIssue {
	var issues []DiagnosticIssue
	task := snapshot.Task
	runningRuns := filterRunsByStatus(snapshot.Runs, model.RunRunning)
	active := hasActiveJob(snapshot.ActiveJobs)
	if task.State == model.TaskInspecting && strings.TrimSpace(task.CurrentRunID) == "" && len(runningRuns) == 0 && !active && strings.TrimSpace(task.SyncError) == "" {
		issues = append(issues, issue(DiagnosticInspectingWithoutRun, SeverityCritical, "题目处于 inspecting，但没有 run、active job 或 Git 错误", "", FixTerminalReset))
	}
	if task.State == model.TaskInspecting && strings.TrimSpace(task.CurrentRunID) != "" && !active {
		issues = append(issues, issue(DiagnosticInspectingWithoutJob, SeverityCritical, "题目指向 active run，但当前进程没有对应 job", task.CurrentRunID, FixTerminalReset))
	}
	if len(runningRuns) > 1 {
		issues = append(issues, issue(DiagnosticInspectingDoubleRun, SeverityCritical, "同一题目存在多个 running run", fmt.Sprintf("%d running runs", len(runningRuns)), FixTerminalReset))
	}
	stuckAfter := m.stuckRunThreshold()
	for _, run := range runningRuns {
		started, ok := parseRFC3339(run.StartedAt)
		if ok && time.Since(started) > stuckAfter {
			issues = append(issues, issue(DiagnosticRunStuckRunning, SeverityCritical, "running run 超过预期上限仍未完成", run.RunID, fixForInactiveRun(active)))
		}
		if len(snapshot.StagesForRun[run.RunID]) == 0 && ok && time.Since(started) > 2*time.Minute && !active {
			issues = append(issues, issue(DiagnosticRunWithoutStages, SeverityHigh, "running run 没有任何 stage 记录", run.RunID, FixTerminalReset))
		}
	}
	if snapshot.Lock.ReadErr != nil && !os.IsNotExist(snapshot.Lock.ReadErr) {
		issues = append(issues, issue(DiagnosticOrphanedLockFile, SeverityWarning, "读取运行锁失败", snapshot.Lock.ReadErr.Error(), FixNoFix))
	} else if snapshot.Lock.Exists {
		if snapshot.Lock.Stale || (snapshot.Lock.TaskID != "" && snapshot.Lock.TaskID != task.ID) || snapshot.Lock.PID <= 0 {
			issues = append(issues, issue(DiagnosticOrphanedLockFile, SeverityHigh, "发现失效或不匹配的运行锁", snapshot.Lock.Path, FixTerminalReset))
		}
	}
	if task.State == model.TaskCompleted && (task.DockerRunning || snapshot.Docker.Running) {
		issues = append(issues, issue(DiagnosticDockerLeakedDone, SeverityHigh, "completed 任务仍有 Docker runtime 记录或实际运行", "", FixStopLeakedDocker))
	}
	if task.State == model.TaskInspecting && !active && (task.DockerRunning || snapshot.Docker.Running) {
		issues = append(issues, issue(DiagnosticDockerLeakedInspect, SeverityHigh, "inspecting 异常任务仍有 Docker runtime", "", FixTerminalReset))
	}
	if snapshot.Docker.Checked && snapshot.Docker.Error != "" {
		issues = append(issues, issue(DiagnosticDockerStatusUnknown, SeverityWarning, "Docker 状态采集失败", snapshot.Docker.Error, FixNoFix))
	}
	if task.State == model.TaskWaitingManual {
		if entered, ok := parseRFC3339(task.EnteredWaitingAt); ok && time.Since(entered) > 24*time.Hour {
			issues = append(issues, issue(DiagnosticStaleWaitingManual, SeverityInfo, "waiting_manual 已超过 24 小时", task.EnteredWaitingAt, FixNoFix))
		}
	}
	if len(snapshot.FailureFiles) > 0 && len(snapshot.Runs) == 0 {
		policy := FixNoFix
		if task.State == model.TaskInspecting && !active {
			policy = FixTerminalReset
		}
		issues = append(issues, issue(DiagnosticPreRunFailureOnly, SeverityWarning, "存在 CreateRun 前失败记录，但没有 DB run", snapshot.FailureFiles[0].Path, policy))
	}
	sort.SliceStable(issues, func(i, j int) bool {
		return severityRank(issues[i].Severity) > severityRank(issues[j].Severity)
	})
	return issues
}

func (m *Manager) stuckRunThreshold() time.Duration {
	total := 0
	for _, seconds := range m.cfg.Pipeline.StageTimeouts {
		if seconds > 0 {
			total += seconds
		}
	}
	if total <= 0 {
		return 2 * time.Hour
	}
	return time.Duration(total)*time.Second + 10*time.Minute
}

func (m *Manager) repairDiagnostics(ctx context.Context, snapshot TaskDiagnosticsSnapshot) (DiagnosticsRepairResult, error) {
	policy := dominantFixPolicy(snapshot.Issues)
	result := DiagnosticsRepairResult{TaskID: snapshot.Task.ID, Policy: policy, FixedIssues: issueCodesForPolicy(snapshot.Issues, policy)}
	switch policy {
	case FixTerminalReset:
		cleanupErr := m.stopRuntimeForDiagnostics(ctx, snapshot.Task)
		if cleanupErr != nil {
			result.CleanupError = cleanupErr.Error()
		}
		if snapshot.Lock.Exists && (snapshot.Lock.Stale || snapshot.Lock.PID <= 0 || snapshot.Lock.TaskID != snapshot.Task.ID) {
			if err := pipeline.RemoveTaskRunLock(m.cfg.ScanPath, snapshot.Task.ID); err != nil {
				return result, err
			}
		}
		if _, err := m.store.TerminalResetTaskForRerun(ctx, snapshot.Task.ID, diagnosticResetReason(snapshot)); err != nil {
			return result, err
		}
	case FixStopLeakedDocker:
		if err := m.stopRuntimeForDiagnostics(ctx, snapshot.Task); err != nil {
			return result, err
		}
		if err := m.store.MarkTaskDockerStopped(ctx, snapshot.Task.ID); err != nil {
			return result, err
		}
		if err := m.store.RecordTaskEvent(ctx, model.TaskEvent{
			TaskID:  snapshot.Task.ID,
			Kind:    "diagnostic_stop_leaked_docker",
			Message: "stopped leaked Docker runtime",
			Source:  "task_diagnostics",
		}); err != nil {
			return result, err
		}
	default:
		result.LogPath = writeDiagnosticLog(m.cfg.ScanPath, snapshot, result)
		return result, nil
	}
	result.LogPath = writeDiagnosticLog(m.cfg.ScanPath, snapshot, result)
	return result, nil
}

func (m *Manager) stopRuntimeForDiagnostics(ctx context.Context, task model.Task) error {
	if strings.TrimSpace(task.ComposeMeta.Project) == "" || len(task.ComposeMeta.ComposeFiles) == 0 {
		return nil
	}
	summary := pipeline.StopDockerRuntime(ctx, m.exec, m.cfg.Docker, task.ComposeMeta)
	if summary.Status == "failed" {
		return fmt.Errorf("docker cleanup failed: %s", cleanupErrorText(summary))
	}
	return nil
}

func activeJobsForTask(jobs []scheduler.JobSnapshot, taskID string) []scheduler.JobSnapshot {
	var result []scheduler.JobSnapshot
	for _, job := range jobs {
		if job.TaskID == taskID {
			result = append(result, job)
		}
	}
	return result
}

func (m *Manager) currentActiveJobs(fallback []scheduler.JobSnapshot) []scheduler.JobSnapshot {
	if m != nil && m.scheduler != nil {
		if source, ok := m.scheduler.(activeJobSnapshotter); ok {
			return source.ActiveSnapshot()
		}
	}
	return append([]scheduler.JobSnapshot(nil), fallback...)
}

func hasActiveJob(jobs []scheduler.JobSnapshot) bool {
	for _, job := range jobs {
		if job.State == scheduler.JobQueued || job.State == scheduler.JobRunning {
			return true
		}
	}
	return false
}

func filterRunsByStatus(runs []model.RunRecord, status string) []model.RunRecord {
	var result []model.RunRecord
	for _, run := range runs {
		if run.Status == status {
			result = append(result, run)
		}
	}
	return result
}

func fixForInactiveRun(active bool) DiagnosticFixPolicy {
	if active {
		return FixNoFix
	}
	return FixTerminalReset
}

func issue(code DiagnosticCode, severity DiagnosticSeverity, message string, detail string, policy DiagnosticFixPolicy) DiagnosticIssue {
	return DiagnosticIssue{Code: code, Severity: severity, Message: message, Detail: detail, Policy: policy}
}

func severityRank(severity DiagnosticSeverity) int {
	switch severity {
	case SeverityCritical:
		return 4
	case SeverityHigh:
		return 3
	case SeverityWarning:
		return 2
	case SeverityInfo:
		return 1
	default:
		return 0
	}
}

func dominantFixPolicy(issues []DiagnosticIssue) DiagnosticFixPolicy {
	for _, item := range issues {
		if item.Policy == FixTerminalReset {
			return FixTerminalReset
		}
	}
	for _, item := range issues {
		if item.Policy == FixStopLeakedDocker {
			return FixStopLeakedDocker
		}
	}
	return FixNoFix
}

func issueCodesForPolicy(issues []DiagnosticIssue, policy DiagnosticFixPolicy) []DiagnosticCode {
	var codes []DiagnosticCode
	for _, item := range issues {
		if item.Policy == policy {
			codes = append(codes, item.Code)
		}
	}
	return codes
}

func diagnosticResetReason(snapshot TaskDiagnosticsSnapshot) string {
	if len(snapshot.Issues) == 0 {
		return "diagnostic terminal reset"
	}
	parts := make([]string, 0, len(snapshot.Issues))
	for _, item := range snapshot.Issues {
		if item.Policy == FixTerminalReset {
			parts = append(parts, string(item.Code))
		}
	}
	if len(parts) == 0 {
		parts = append(parts, string(snapshot.Issues[0].Code))
	}
	return "diagnostic terminal reset: " + strings.Join(parts, ", ")
}

func readRunFailureFiles(scanPath, taskID string) []RunFailureFile {
	dir := filepath.Join(scanPath, ".qa-control", "run-failures")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	prefix := diagnosticSafeName(taskID)
	var files []RunFailureFile
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		record := RunFailureFile{Path: path}
		if content, err := os.ReadFile(path); err == nil {
			var data struct {
				Phase     string `json:"phase"`
				Error     string `json:"error"`
				CreatedAt string `json:"created_at"`
			}
			if json.Unmarshal(content, &data) == nil {
				record.Phase = data.Phase
				record.Error = data.Error
				record.CreatedAt = data.CreatedAt
			}
		}
		files = append(files, record)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path > files[j].Path })
	return files
}

func writeDiagnosticLog(scanPath string, snapshot TaskDiagnosticsSnapshot, result DiagnosticsRepairResult) string {
	dir := filepath.Join(scanPath, ".qa-control", "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	now := time.Now().UTC()
	path := filepath.Join(dir, fmt.Sprintf("diagnostic_%s_%s.log", diagnosticSafeName(snapshot.Task.ID), now.Format("20060102T150405Z")))
	payload := map[string]any{
		"snapshot": snapshot,
		"repair":   result,
		"written":  now.Format(time.RFC3339),
	}
	content, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return ""
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return ""
	}
	return path
}

func diagnosticSafeName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' {
			builder.WriteRune(r)
		} else {
			builder.WriteByte('_')
		}
	}
	if builder.Len() == 0 {
		return "task"
	}
	return strings.Trim(builder.String(), "._-")
}

func parseRFC3339(value string) (time.Time, bool) {
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
	return parsed, err == nil
}
