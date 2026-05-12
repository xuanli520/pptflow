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

func NewRecoveryService(store *db.Store, cfg config.Config) RecoveryService {
	return RecoveryService{store: store, cfg: cfg}
}

func RecoverStaleRuns(ctx context.Context, store *db.Store, cfg config.Config) error {
	return NewRecoveryService(store, cfg).RecoverStaleRuns(ctx)
}

func (s RecoveryService) RecoverStaleRuns(ctx context.Context) error {
	if s.store == nil {
		return nil
	}
	runs, err := s.store.RunningRuns(ctx)
	if err != nil {
		return err
	}
	var recoverErrors []error
	for _, run := range runs {
		if err := ctx.Err(); err != nil {
			recoverErrors = append(recoverErrors, err)
			break
		}
		manifest := readRecoveryManifest(run.ArtifactRoot)
		stale, reason, started := staleRunReason(run, manifest, s.cfg)
		if !stale {
			continue
		}
		if err := s.recoverStaleRun(ctx, run, manifest, started, reason); err != nil {
			recoverErrors = append(recoverErrors, fmt.Errorf("recover stale run %s: %w", run.RunID, err))
		}
	}
	return errors.Join(recoverErrors...)
}

func staleRunReason(run model.RunRecord, manifest recoveryManifest, cfg config.Config) (bool, string, time.Time) {
	started, err := time.Parse(time.RFC3339, strings.TrimSpace(run.StartedAt))
	if err != nil {
		started = time.Now().UTC()
	}
	if strings.TrimSpace(run.ArtifactRoot) == "" || !dirExists(run.ArtifactRoot) {
		return true, "运行产物目录已不存在，历史 running 记录已失联", started
	}
	expected := expectedRunDuration(run, manifest, cfg)
	if time.Since(started) <= expected {
		return false, "", started
	}
	return true, fmt.Sprintf("运行超过预期上限 %s 仍未完成，已按失联运行回收", expected.Round(time.Second)), started
}

func (s RecoveryService) recoverStaleRun(ctx context.Context, run model.RunRecord, manifest recoveryManifest, started time.Time, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "历史 running 记录已失联"
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
	if err := s.store.InsertFindings(ctx, run.RunID, []model.Finding{{
		ID:         "P2R-INFRA-HIGH-STALLED-RUN",
		Stage:      "INFRA",
		Severity:   "High",
		Title:      "p2r run lost while marked running",
		Rule:       "Pipeline runs must reach a terminal status or be recovered as crashed.",
		Evidence:   reason,
		Impact:     "The TUI status was misleading and the task lock could block reruns.",
		MinimumFix: "Inspect crash_summary.json and rerun the affected stages.",
		SourcePath: filepath.Join(run.ArtifactRoot, "crash_summary.json"),
	}}); err != nil {
		recoverErrors = append(recoverErrors, fmt.Errorf("insert stale-run finding: %w", err))
	}
	runtime := RuntimeState{}
	if loaded, err := readRuntimeState(filepath.Join(run.ArtifactRoot, "port_map.json")); err == nil {
		runtime = loaded
	} else if !os.IsNotExist(err) {
		recoverErrors = append(recoverErrors, fmt.Errorf("read recovery runtime state: %w", err))
	}
	if err := runner.crashRun(ctx, run, started, recoveredStages, runtime, manifest.KeepRuntime || s.cfg.Docker.KeepRuntime, false, reason); err != nil {
		recoverErrors = append(recoverErrors, err)
		return errors.Join(recoverErrors...)
	}
	removeTaskLock(s.cfg, run.TaskID)
	return errors.Join(recoverErrors...)
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
