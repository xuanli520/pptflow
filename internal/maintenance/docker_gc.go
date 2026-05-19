package maintenance

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xuanli520/p2r_tui/internal/config"
	dockermgr "github.com/xuanli520/p2r_tui/internal/docker"
	"github.com/xuanli520/p2r_tui/internal/executor"
)

type DockerMaintenanceState struct {
	LastCheckedAt string `json:"last_checked_at,omitempty"`
	LastSuccessAt string `json:"last_success_at,omitempty"`
	LastStatus    string `json:"last_status,omitempty"`
}

func DockerMaintenanceStatePath(scanPath string) string {
	return filepath.Join(scanPath, ".qa-control", "docker_maintenance_state.json")
}

func TryRunOnTUIStart(ctx context.Context, cfg config.Config, exec executor.CommandRunner, activeJobs int) (dockermgr.GCSummary, error) {
	if !cfg.Docker.GC.RunOnTUIStart {
		return runTUIStartGC(ctx, cfg, exec, "docker.gc.run_on_tui_start=false")
	}
	if activeJobs > 0 {
		return runTUIStartGC(ctx, cfg, exec, "scheduler has active jobs")
	}
	if hasTaskRunLocks(cfg.ScanPath) {
		return runTUIStartGC(ctx, cfg, exec, "task run lock exists")
	}
	if !gcIntervalElapsed(cfg) {
		return runTUIStartGC(ctx, cfg, exec, "docker.gc.interval has not elapsed")
	}
	summary, err := dockermgr.RunGC(ctx, dockermgr.GCRunRequest{ScanPath: cfg.ScanPath, Config: cfg.Docker, Exec: exec, Yes: true, Trigger: "tui_start"})
	recordState(cfg.ScanPath, summary)
	return summary, err
}

func runTUIStartGC(ctx context.Context, cfg config.Config, exec executor.CommandRunner, skipReason string) (dockermgr.GCSummary, error) {
	summary, err := dockermgr.RunGC(ctx, dockermgr.GCRunRequest{ScanPath: cfg.ScanPath, Config: cfg.Docker, Exec: exec, DryRun: true, Trigger: "tui_start", SkipReason: skipReason})
	recordState(cfg.ScanPath, summary)
	return summary, err
}

func TryRunBeforeCLIRun(ctx context.Context, cfg config.Config, exec executor.CommandRunner) (dockermgr.GCSummary, error) {
	if !cfg.Docker.GC.RunBeforeCLIRun {
		return dockermgr.GCSummary{Skipped: true, SkipReason: "docker.gc.run_before_cli_run=false"}, nil
	}
	if hasTaskRunLocks(cfg.ScanPath) {
		return dockermgr.RunGC(ctx, dockermgr.GCRunRequest{ScanPath: cfg.ScanPath, Config: cfg.Docker, Exec: exec, DryRun: true, Trigger: "cli_run", SkipReason: "task run lock exists"})
	}
	summary, err := dockermgr.RunGC(ctx, dockermgr.GCRunRequest{ScanPath: cfg.ScanPath, Config: cfg.Docker, Exec: exec, Yes: true, Trigger: "cli_run"})
	recordState(cfg.ScanPath, summary)
	return summary, err
}

func Status(cfg config.Config) (DockerMaintenanceState, dockermgr.GCSummary) {
	state := readState(cfg.ScanPath)
	var summary dockermgr.GCSummary
	content, err := os.ReadFile(dockermgr.DockerGCSummaryPath(cfg.ScanPath))
	if err == nil {
		_ = json.Unmarshal(content, &summary)
	}
	return state, summary
}

func gcIntervalElapsed(cfg config.Config) bool {
	interval, err := time.ParseDuration(cfg.Docker.GC.Interval)
	if err != nil {
		return true
	}
	state := readState(cfg.ScanPath)
	lastAt := state.LastSuccessAt
	if lastAt == "" {
		lastAt = state.LastCheckedAt
	}
	if lastAt == "" {
		return true
	}
	last, err := time.Parse(time.RFC3339, lastAt)
	if err != nil {
		return true
	}
	return time.Since(last) >= interval
}

func readState(scanPath string) DockerMaintenanceState {
	content, err := os.ReadFile(DockerMaintenanceStatePath(scanPath))
	if err != nil {
		return DockerMaintenanceState{}
	}
	var state DockerMaintenanceState
	_ = json.Unmarshal(content, &state)
	return state
}

func recordState(scanPath string, summary dockermgr.GCSummary) {
	state := DockerMaintenanceState{
		LastCheckedAt: time.Now().UTC().Format(time.RFC3339),
		LastStatus:    "skipped",
	}
	if summary.OK && !summary.Skipped {
		state.LastSuccessAt = state.LastCheckedAt
		state.LastStatus = "ok"
	} else if !summary.OK {
		state.LastStatus = "failed"
	}
	path := DockerMaintenanceStatePath(scanPath)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	content, _ := json.MarshalIndent(state, "", "  ")
	_ = os.WriteFile(path, append(content, '\n'), 0o644)
}

func hasTaskRunLocks(scanPath string) bool {
	lockDir := filepath.Join(scanPath, ".qa-control", "locks")
	entries, err := os.ReadDir(lockDir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || name == "docker-maintenance.lock" || !strings.HasSuffix(name, ".lock") {
			continue
		}
		return true
	}
	return false
}
