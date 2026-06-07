package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/db"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
)

type recoveryManifest struct {
	Stage         string         `json:"stage"`
	From          string         `json:"from"`
	Stages        []string       `json:"stages"`
	StaticOnly    bool           `json:"static_only"`
	KeepRuntime   bool           `json:"keep_runtime"`
	StageTimeouts map[string]int `json:"stage_timeouts"`
}

type RecoveryService struct {
	store *db.Store
	cfg   config.Config
}

type RunReference struct {
	RunID  string
	TaskID string
}

type RecoveryResult struct {
	RunIDs  []string
	TaskIDs []string
}

type runRecoveryPlan struct {
	Status                string
	Reason                string
	Started               time.Time
	IncludeStalledFinding bool
}

type normalizedRunReferences struct {
	byRunID  map[string]bool
	byTaskID map[string]bool
}

func NewRecoveryService(store *db.Store, cfg config.Config) RecoveryService {
	return RecoveryService{store: store, cfg: cfg}
}

func RecoverStaleRuns(ctx context.Context, store *db.Store, cfg config.Config) error {
	return NewRecoveryService(store, cfg).RecoverStaleRuns(ctx)
}

func RecoverOrphanedRuns(ctx context.Context, store *db.Store, cfg config.Config) (RecoveryResult, error) {
	return NewRecoveryService(store, cfg).RecoverOrphanedRuns(ctx)
}

func RecoverOrphanedRunForTask(ctx context.Context, store *db.Store, cfg config.Config, taskID string) (RecoveryResult, error) {
	return NewRecoveryService(store, cfg).RecoverOrphanedRunForTask(ctx, taskID)
}

func RecoverInterruptedRuns(ctx context.Context, store *db.Store, cfg config.Config, refs []RunReference, reason string) (RecoveryResult, error) {
	return NewRecoveryService(store, cfg).RecoverInterruptedRuns(ctx, refs, reason)
}

func (s RecoveryService) RecoverStaleRuns(ctx context.Context) error {
	if s.store == nil {
		return nil
	}
	_, err := s.recoverRunningRuns(ctx, func(run model.RunRecord, manifest recoveryManifest) (runRecoveryPlan, bool) {
		stale, reason, started := staleRunReason(run, manifest, s.cfg)
		if !stale {
			return runRecoveryPlan{}, false
		}
		return runRecoveryPlan{
			Status:                model.RunCrashed,
			Reason:                reason,
			Started:               started,
			IncludeStalledFinding: true,
		}, true
	})
	return err
}

func (s RecoveryService) RecoverOrphanedRuns(ctx context.Context) (RecoveryResult, error) {
	return s.recoverOrphanedRuns(ctx, "")
}

func (s RecoveryService) RecoverOrphanedRunForTask(ctx context.Context, taskID string) (RecoveryResult, error) {
	return s.recoverOrphanedRuns(ctx, taskID)
}

func (s RecoveryService) RecoverInterruptedRuns(ctx context.Context, refs []RunReference, reason string) (RecoveryResult, error) {
	references := normalizeRunReferences(refs)
	if len(references.byRunID) == 0 && len(references.byTaskID) == 0 {
		return RecoveryResult{}, nil
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "TUI exited before cancellation could be persisted"
	}
	return s.recoverRunningRuns(ctx, func(run model.RunRecord, _ recoveryManifest) (runRecoveryPlan, bool) {
		if !references.matches(run) {
			return runRecoveryPlan{}, false
		}
		return runRecoveryPlan{
			Status:  model.RunAborted,
			Reason:  reason,
			Started: runStartedAt(run),
		}, true
	})
}

func (s RecoveryService) recoverOrphanedRuns(ctx context.Context, taskID string) (RecoveryResult, error) {
	taskID = strings.TrimSpace(taskID)
	return s.recoverRunningRuns(ctx, func(run model.RunRecord, _ recoveryManifest) (runRecoveryPlan, bool) {
		if taskID != "" && run.TaskID != taskID {
			return runRecoveryPlan{}, false
		}
		orphaned, reason, started := s.orphanedRunReason(run)
		if !orphaned {
			return runRecoveryPlan{}, false
		}
		return runRecoveryPlan{
			Status:                model.RunCrashed,
			Reason:                reason,
			Started:               started,
			IncludeStalledFinding: true,
		}, true
	})
}

func (s RecoveryService) recoverRunningRuns(ctx context.Context, selectPlan func(model.RunRecord, recoveryManifest) (runRecoveryPlan, bool)) (RecoveryResult, error) {
	if s.store == nil {
		return RecoveryResult{}, nil
	}
	runs, err := s.store.RunningRuns(ctx)
	if err != nil {
		return RecoveryResult{}, err
	}
	var result RecoveryResult
	var recoverErrors []error
	for _, run := range runs {
		if err := ctx.Err(); err != nil {
			recoverErrors = append(recoverErrors, err)
			break
		}
		manifest := readRecoveryManifest(run.ArtifactRoot)
		plan, recover := selectPlan(run, manifest)
		if !recover {
			continue
		}
		if err := s.recoverRun(ctx, run, manifest, plan); err != nil {
			recoverErrors = append(recoverErrors, fmt.Errorf("recover run %s: %w", run.RunID, err))
			continue
		}
		result.add(run)
	}
	return result, errors.Join(recoverErrors...)
}

func (r *RecoveryResult) add(run model.RunRecord) {
	if r == nil {
		return
	}
	r.RunIDs = append(r.RunIDs, run.RunID)
	r.TaskIDs = append(r.TaskIDs, run.TaskID)
}

func (r RecoveryResult) Count() int {
	return len(r.RunIDs)
}

func normalizeRunReferences(refs []RunReference) normalizedRunReferences {
	normalized := normalizedRunReferences{
		byRunID:  map[string]bool{},
		byTaskID: map[string]bool{},
	}
	for _, ref := range refs {
		if runID := strings.TrimSpace(ref.RunID); runID != "" {
			normalized.byRunID[runID] = true
		}
		if taskID := strings.TrimSpace(ref.TaskID); taskID != "" {
			normalized.byTaskID[taskID] = true
		}
	}
	return normalized
}

func (r normalizedRunReferences) matches(run model.RunRecord) bool {
	return r.byRunID[strings.TrimSpace(run.RunID)] || r.byTaskID[strings.TrimSpace(run.TaskID)]
}

func runStartedAt(run model.RunRecord) time.Time {
	started, err := time.Parse(time.RFC3339, strings.TrimSpace(run.StartedAt))
	if err != nil {
		return time.Now().UTC()
	}
	return started
}

func staleRunReason(run model.RunRecord, manifest recoveryManifest, cfg config.Config) (bool, string, time.Time) {
	started := runStartedAt(run)
	if strings.TrimSpace(run.ArtifactRoot) == "" || !dirExists(run.ArtifactRoot) {
		return true, "运行产物目录已不存在，历史 running 记录已失联", started
	}
	expected := expectedRunDuration(run, manifest, cfg)
	if time.Since(started) <= expected {
		return false, "", started
	}
	return true, fmt.Sprintf("运行超过预期上限 %s 仍未完成，已按失联运行回收", expected.Round(time.Second)), started
}

func (s RecoveryService) orphanedRunReason(run model.RunRecord) (bool, string, time.Time) {
	started := runStartedAt(run)
	status := taskRunLockStatusForTask(s.cfg.ScanPath, run.TaskID)
	if status.ReadErr != nil {
		if os.IsNotExist(status.ReadErr) {
			return true, "运行锁不存在，running 记录已失联", started
		}
		return false, "", started
	}
	if !status.Exists {
		return true, "运行锁不存在，running 记录已失联", started
	}
	if status.TaskID != "" && status.TaskID != run.TaskID {
		return true, fmt.Sprintf("运行锁属于任务 %s，running 记录已失联", status.TaskID), started
	}
	if status.PID <= 0 {
		return true, "运行锁缺少有效 owner pid，running 记录已失联", started
	}
	if status.Stale {
		return true, fmt.Sprintf("运行锁 owner pid %d 已退出，running 记录已失联", status.PID), started
	}
	return false, "", started
}

func (s RecoveryService) recoverRun(ctx context.Context, run model.RunRecord, manifest recoveryManifest, plan runRecoveryPlan) error {
	reason := strings.TrimSpace(plan.Reason)
	if reason == "" {
		reason = "running 记录已失联"
	}
	started := plan.Started
	if started.IsZero() {
		started = runStartedAt(run)
	}
	status := strings.TrimSpace(plan.Status)
	if status == "" {
		status = model.RunCrashed
	}
	var recoverErrors []error
	runner := NewRunner(s.store, s.cfg)
	stages, err := s.store.Stages(ctx, run.RunID)
	if err != nil {
		return err
	}
	selected := selectedStagesForRecovery(run, manifest)
	now := time.Now().UTC()
	byStage := map[string]model.StageRecord{}
	for _, stage := range stages {
		byStage[stage.Stage] = stage
	}
	recoveredStages := make([]model.StageRecord, 0, len(stages))
	for _, letter := range model.AllStages() {
		if !selected[letter] {
			continue
		}
		stage, ok := byStage[letter]
		if ok && terminalStageStatus(stage.Status) {
			recoveredStages = append(recoveredStages, stage)
			continue
		}
		if !ok {
			stage = model.StageRecord{
				Stage:         letter,
				Name:          stageName(letter),
				StartedAt:     run.StartedAt,
				ArtifactPaths: []string{},
			}
		}
		stage.Status = model.StageFailed
		stage.FinishedAt = now.Format(time.RFC3339)
		stage.ErrorSummary = reason
		if stage.LogPath == "" {
			stage.LogPath = stageLogPath(run.ArtifactRoot, letter)
		}
		if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(stage.StartedAt)); err == nil {
			stage.DurationMS = now.Sub(parsed).Milliseconds()
		} else {
			stage.DurationMS = now.Sub(started).Milliseconds()
		}
		if err := appendText(stage.LogPath, "\n[p2r recovery] "+reason+"\n"); err != nil {
			recoverErrors = append(recoverErrors, fmt.Errorf("append recovery log for stage %s: %w", letter, err))
		}
		if err := s.store.PutStage(ctx, run.RunID, stage); err != nil {
			recoverErrors = append(recoverErrors, fmt.Errorf("put recovered stage %s: %w", letter, err))
		}
		recoveredStages = append(recoveredStages, stage)
	}
	if err := runner.writeStageStatus(run.RunID, run.ArtifactRoot, recoveredStages); err != nil {
		recoverErrors = append(recoverErrors, fmt.Errorf("write recovered stage_status.json: %w", err))
	}
	if plan.IncludeStalledFinding {
		if err := s.store.InsertFindings(ctx, run.RunID, []model.Finding{stalledRunFinding(run, status, reason)}); err != nil {
			recoverErrors = append(recoverErrors, fmt.Errorf("insert stale-run finding: %w", err))
		}
	}
	runtime := RuntimeState{}
	if loaded, err := readRuntimeState(filepath.Join(run.ArtifactRoot, "port_map.json")); err == nil {
		runtime = loaded
	} else if !os.IsNotExist(err) {
		recoverErrors = append(recoverErrors, fmt.Errorf("read recovery runtime state: %w", err))
	}
	if err := runner.finishRecoveredRun(ctx, run, started, recoveredStages, runtime, manifest.KeepRuntime || s.cfg.Docker.KeepRuntime, status, reason); err != nil {
		recoverErrors = append(recoverErrors, err)
		return errors.Join(recoverErrors...)
	}
	removeTaskLock(s.cfg, run.TaskID)
	return errors.Join(recoverErrors...)
}

func (r Runner) finishRecoveredRun(ctx context.Context, run model.RunRecord, started time.Time, stages []model.StageRecord, runtime RuntimeState, keepRuntime bool, status string, reason string) error {
	switch status {
	case model.RunAborted:
		return r.abortRun(ctx, run, started, stages, runtime, keepRuntime, false, reason)
	default:
		return r.crashRun(ctx, run, started, stages, runtime, keepRuntime, false, reason)
	}
}

func stalledRunFinding(run model.RunRecord, status string, reason string) model.Finding {
	summary := "crash_summary.json"
	if status == model.RunAborted {
		summary = "abort_summary.json"
	}
	return model.Finding{
		ID:         "P2R-INFRA-HIGH-STALLED-RUN",
		Stage:      "INFRA",
		Severity:   "High",
		Title:      "p2r run lost while marked running",
		Rule:       "Pipeline runs must reach a terminal status or be recovered.",
		Evidence:   reason,
		Impact:     "The TUI status was misleading and the task lock could block reruns.",
		MinimumFix: "Inspect " + summary + " and rerun the affected stages.",
		SourcePath: filepath.Join(run.ArtifactRoot, summary),
	}
}

func readRecoveryManifest(artifactRoot string) recoveryManifest {
	var manifest recoveryManifest
	content, err := os.ReadFile(filepath.Join(artifactRoot, "run_manifest.json"))
	if err != nil {
		return manifest
	}
	_ = json.Unmarshal(content, &manifest)
	return manifest
}

func selectedStagesForRecovery(run model.RunRecord, manifest recoveryManifest) map[string]bool {
	return selectedStages(RunOptions{
		Stage:      manifest.Stage,
		From:       manifest.From,
		Stages:     manifest.Stages,
		StaticOnly: manifest.StaticOnly || run.StaticOnly,
	}, manifest.StaticOnly || run.StaticOnly)
}

func expectedRunDuration(run model.RunRecord, manifest recoveryManifest, cfg config.Config) time.Duration {
	selected := selectedStagesForRecovery(run, manifest)
	var total time.Duration
	for _, stage := range model.AllStages() {
		if !selected[stage] {
			continue
		}
		seconds := manifest.StageTimeouts[stage]
		if seconds <= 0 {
			seconds = cfg.Pipeline.StageTimeouts[stage]
		}
		if seconds <= 0 {
			seconds = 300
		}
		total += time.Duration(seconds) * time.Second
	}
	if total <= 0 {
		total = 30 * time.Minute
	}
	grace := total / 5
	if grace < 2*time.Minute {
		grace = 2 * time.Minute
	}
	if grace > 10*time.Minute {
		grace = 10 * time.Minute
	}
	return total + grace
}

func terminalStageStatus(status string) bool {
	switch strings.TrimSpace(status) {
	case model.StageDone, model.StageFailed, model.StageBlocked, model.StageSkipped:
		return true
	default:
		return false
	}
}

func removeTaskLock(cfg config.Config, taskID string) {
	_ = os.Remove(taskRunLockPath(cfg.ScanPath, taskID))
}
