package pipeline

import (
	"context"
	"encoding/json"
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
	StageTimeouts map[string]int `json:"stage_timeouts"`
}

func RecoverStaleRuns(ctx context.Context, store *db.Store, cfg config.Config) error {
	if store == nil {
		return nil
	}
	runs, err := store.RunningRuns(ctx)
	if err != nil {
		return err
	}
	for _, run := range runs {
		manifest := readRecoveryManifest(run.ArtifactRoot)
		stale, reason, started := staleRunReason(run, manifest, cfg)
		if !stale {
			continue
		}
		recoverStaleRun(ctx, store, cfg, run, manifest, started, reason)
	}
	return nil
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

func recoverStaleRun(ctx context.Context, store *db.Store, cfg config.Config, run model.RunRecord, manifest recoveryManifest, started time.Time, reason string) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "历史 running 记录已失联"
	}
	runner := Runner{store: store, cfg: cfg}
	stages, _ := store.Stages(ctx, run.RunID)
	selected := selectedStagesForRecovery(run, manifest)
	now := time.Now().UTC()
	byStage := map[string]model.StageRecord{}
	for _, stage := range stages {
		byStage[stage.Stage] = stage
	}
	for _, letter := range []string{"A", "B", "C", "D", "E", "F"} {
		if !selected[letter] {
			continue
		}
		stage, ok := byStage[letter]
		if ok && terminalStageStatus(stage.Status) {
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
		_ = appendText(stage.LogPath, "\n[p2r recovery] "+reason+"\n")
		_ = store.PutStage(ctx, run.RunID, stage)
	}
	_ = store.InsertFindings(ctx, run.RunID, []model.Finding{{
		ID:         "P2R-INFRA-HIGH-STALLED-RUN",
		Stage:      "INFRA",
		Severity:   "High",
		Title:      "p2r run lost while marked running",
		Rule:       "Pipeline runs must reach a terminal status or be recovered as crashed.",
		Evidence:   reason,
		Impact:     "The TUI status was misleading and the task lock could block reruns.",
		MinimumFix: "Inspect crash_summary.json and rerun the affected stages.",
		SourcePath: filepath.Join(run.ArtifactRoot, "crash_summary.json"),
	}})
	runner.markRunCrashed(ctx, run, started, reason)
	removeTaskLock(cfg, run.TaskID)
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
	for _, stage := range []string{"A", "B", "C", "D", "E", "F"} {
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
	path := filepath.Join(cfg.ScanPath, ".qa-control", "locks", safeLockName(taskID)+".lock")
	_ = os.Remove(path)
}
